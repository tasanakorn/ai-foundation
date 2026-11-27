// Package admin is the admin REST API. It is the main integration point
// for systems built on top (portals, billing, tenant managers) in any language.
//
// Callers sign in through OIDC (oidc.go) or with the local account (session cookie,
// see auth.go), or send the optional master key as a bearer token (scripts, emergency access).
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/clientip"
	"ai-foundation/gateway/internal/httpx"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

type Handler struct {
	keys         *keys.Service
	auth         *auth.Service
	oidc         *auth.OIDC // nil when admin.oidc has no providers
	oidcTTL      time.Duration
	st           store.Store
	routes       router.Source
	public       http.Handler
	masterKey    string
	cookieSecure string
	uiDir        string
	clientIP     *clientip.Resolver
	nodeID       string
	log          *slog.Logger
}

type Options struct {
	Keys         *keys.Service
	Auth         *auth.Service
	OIDC         *auth.OIDC    // optional
	OIDCTTL      time.Duration // session lifetime after an OIDC sign-in
	Store        store.Store
	Router       router.Source
	Public       http.Handler // the /v1 handler, for the playground
	MasterKey    string       // optional; "" turns bearer master-key access off
	CookieSecure string       // auto, always, never
	UIDir        string
	ClientIP     *clientip.Resolver // client IP behind trusted proxies; nil = TCP peer
	NodeID       string             // shown with per-node state (GET /admin/v1/providers)
	Log          *slog.Logger
}

func New(o Options) *Handler {
	return &Handler{keys: o.Keys, auth: o.Auth, oidc: o.OIDC, oidcTTL: o.OIDCTTL, st: o.Store, routes: o.Router, public: o.Public, masterKey: o.MasterKey,
		cookieSecure: o.CookieSecure, uiDir: o.UIDir, clientIP: o.ClientIP, nodeID: o.NodeID, log: o.Log}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Sign-in endpoints. methods, login and the OIDC redirects have no auth; the others
	// need a signed-in caller.
	mux.HandleFunc("GET /admin/v1/auth/methods", h.methods)
	mux.HandleFunc("GET /admin/v1/auth/oidc/{name}/start", h.oidcStart)
	mux.HandleFunc("GET /admin/v1/auth/oidc/{name}/callback", h.oidcCallback)
	mux.Handle("POST /admin/v1/auth/login", h.sameOrigin(http.HandlerFunc(h.login)))
	mux.Handle("POST /admin/v1/auth/logout", h.sameOrigin(http.HandlerFunc(h.logout)))
	api := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, h.authenticate(fn, false)) }
	self := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, h.authenticate(fn, true)) } // any role
	self("GET /admin/v1/auth/me", h.me)
	self("POST /admin/v1/auth/password", h.changePassword)
	self("GET /admin/v1/auth/identities", h.myLinks)
	self("DELETE /admin/v1/auth/identities/{id}", h.deleteMyLink)

	api("GET /admin/v1/users", h.listUsers)
	api("POST /admin/v1/users", h.createUser)
	api("GET /admin/v1/users/{id}", h.getUser)
	api("PATCH /admin/v1/users/{id}", h.updateUser)
	api("DELETE /admin/v1/users/{id}", h.deleteUser)
	api("GET /admin/v1/users/{id}/identities", h.userLinks)
	api("DELETE /admin/v1/users/{id}/identities/{identity_id}", h.deleteUserLink)
	api("POST /admin/v1/users/{id}/identities/allow", h.allowLink)

	// Playground: any role; it changes no admin state and needs a real API key.
	self("POST /admin/v1/playground/chat/completions", h.playground)
	self("POST /admin/v1/playground/completions", h.playground)
	self("POST /admin/v1/playground/embeddings", h.playground)
	self("GET /admin/v1/playground/models", h.playground)

	api("GET /admin/v1/keys", h.listKeys)
	api("POST /admin/v1/keys", h.createKey)
	api("GET /admin/v1/keys/{id}", h.getKey)
	api("DELETE /admin/v1/keys/{id}", h.revokeKey)
	api("GET /admin/v1/models", h.listModels)
	api("GET /admin/v1/providers", h.listProviders)
	api("GET /admin/v1/credit-entries", h.listCreditEntries)
	api("POST /admin/v1/credit-entries", h.addCreditEntry)
	api("GET /admin/v1/usage", h.listUsage)
	api("GET /admin/v1/usage/summary", h.summarizeUsage)
	if h.uiDir != "" {
		mux.Handle("GET /", spa(h.uiDir))
	}
	return mux
}

// ---- keys ----

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := h.st.ListKeys(r.Context(), store.KeyFilter{
		TenantID:       q.Get("tenant_id"),
		AppID:          q.Get("app_id"),
		IncludeRevoked: q.Get("include_revoked") == "true",
		Limit:          atoi(q.Get("limit")),
		Offset:         atoi(q.Get("offset")),
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var in keys.CreateInput
	if !decode(w, r, &in) {
		return
	}
	secret, k, err := h.keys.Create(r.Context(), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The secret is shown once. Only its hash is stored.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		*store.Key
		Secret string `json:"secret"`
	}{k, secret})
}

func (h *Handler) getKey(w http.ResponseWriter, r *http.Request) {
	k, err := h.st.GetKey(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, k)
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := h.keys.Revoke(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- models ----

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	rt := h.routes.Router()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data":        rt.Models(),
		"providers":   rt.Providers(),
		"passthrough": rt.Passthrough(),
	})
}

// listProviders shows each provider with its policy and accounts, as this node sees
// them (cooldowns are shared by all nodes, spec-0004 section 4.4). Never a key. An account with a
// credit also gets its spend, from the usage table and ledger of all nodes (task-0010).
func (h *Handler) listProviders(w http.ResponseWriter, r *http.Request) {
	rt := h.routes.Router()
	out := []provider.PoolState{}
	for _, name := range rt.Providers() {
		p, _ := rt.Provider(name)
		if pool, ok := provider.AsPool(p); ok {
			out = append(out, pool.State())
		}
	}
	if err := catalog.FillCredits(r.Context(), h.st, out); err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"node": h.nodeID, "data": out})
}

// ---- prepaid ledger (task-0010) ----

func (h *Handler) listCreditEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if (q.Get("provider") == "") != (q.Get("account") == "") {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_filter", "give both provider and account, or neither")
		return
	}
	out, err := h.st.ListCreditEntries(r.Context(), q.Get("provider"), q.Get("account"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) addCreditEntry(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string    `json:"provider"`
		Account  string    `json:"account"`
		USD      float64   `json:"usd"`
		Note     string    `json:"note"`
		At       time.Time `json:"at"`
	}
	if !decode(w, r, &in) {
		return
	}
	e := &store.CreditEntry{Provider: in.Provider, Account: in.Account, USD: in.USD, Note: in.Note, At: in.At, Actor: actor(r)}
	if err := catalog.AddCreditEntry(r.Context(), h.st, e); err != nil {
		if errors.Is(err, catalog.ErrBadCreditEntry) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_credit_entry", err.Error())
			return
		}
		h.fail(w, err)
		return
	}
	h.log.Info("credit entry added", "provider", e.Provider, "account", e.Account, "usd", e.USD, "actor", e.Actor)
	httpx.WriteJSON(w, http.StatusCreated, e)
}

// ---- usage ----

func usageFilter(r *http.Request) (store.UsageFilter, error) {
	q := r.URL.Query()
	f := store.UsageFilter{
		ID: q.Get("id"), KeyID: q.Get("key_id"), TenantID: q.Get("tenant_id"), AppID: q.Get("app_id"),
		EndUser: q.Get("end_user"), Model: q.Get("model"), Provider: q.Get("provider"), Account: q.Get("account"),
		Limit: atoi(q.Get("limit")), Offset: atoi(q.Get("offset")),
	}
	var err error
	if f.From, err = parseTime(q.Get("from")); err != nil {
		return f, errors.New("from: " + err.Error())
	}
	if f.To, err = parseTime(q.Get("to")); err != nil {
		return f, errors.New("to: " + err.Error())
	}
	return f, nil
}

func (h *Handler) listUsage(w http.ResponseWriter, r *http.Request) {
	f, err := usageFilter(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_filter", err.Error())
		return
	}
	out, err := h.st.ListUsage(r.Context(), f)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) summarizeUsage(w http.ResponseWriter, r *http.Request) {
	f, err := usageFilter(r)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_filter", err.Error())
		return
	}
	group := r.URL.Query().Get("group_by")
	if group == "" {
		group = "none"
	}
	if !slices.Contains(store.GroupBy, group) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_group_by",
			"group_by must be one of: "+strings.Join(store.GroupBy, ", "))
		return
	}
	out, err := h.st.SummarizeUsage(r.Context(), f, group)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"group_by": group, "data": out})
}

// ---- helpers ----

// decode reads a JSON body (max 1 MiB, no unknown fields). On error it writes a 400 and returns false.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return false
	}
	return true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
		return
	}
	var ie *auth.InputError
	if errors.As(err, &ie) {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", ie.Msg)
		return
	}
	h.log.Error("admin request failed", "err", err)
	httpx.WriteError(w, http.StatusInternalServerError, "server_error", "internal_error", "internal error")
}

// parseTime accepts RFC 3339 or a plain date (UTC midnight).
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.DateOnly, s)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// spa serves a static single-page-app build. Real files (assets) are served as is;
// every other path gets the app shell, and the client-side router takes over.
func spa(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	shell := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if st, err := os.Stat(p); err == nil && !st.IsDir() && filepath.Base(p) != "index.html" {
			fs.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, shell)
	})
}
