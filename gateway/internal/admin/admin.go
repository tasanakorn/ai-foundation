// Package admin is the local admin REST API. It is the main integration point
// for systems built on top (portals, billing, tenant managers) in any language.
package admin

import (
	"crypto/subtle"
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

	"ai-foundation/gateway/internal/httpx"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

type Handler struct {
	keys      *keys.Service
	st        store.Store
	router    *router.Router
	masterKey string
	uiDir     string
	log       *slog.Logger
}

type Options struct {
	Keys      *keys.Service
	Store     store.Store
	Router    *router.Router
	MasterKey string
	UIDir     string
	Log       *slog.Logger
}

func New(o Options) *Handler {
	return &Handler{keys: o.Keys, st: o.Store, router: o.Router, masterKey: o.MasterKey, uiDir: o.UIDir, log: o.Log}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	api := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, h.auth(fn)) }
	api("GET /admin/v1/keys", h.listKeys)
	api("POST /admin/v1/keys", h.createKey)
	api("GET /admin/v1/keys/{id}", h.getKey)
	api("DELETE /admin/v1/keys/{id}", h.revokeKey)
	api("GET /admin/v1/models", h.listModels)
	api("GET /admin/v1/usage", h.listUsage)
	api("GET /admin/v1/usage/summary", h.summarizeUsage)
	if h.uiDir != "" {
		mux.Handle("GET /", spa(h.uiDir))
	}
	return mux
}

func (h *Handler) auth(next http.HandlerFunc) http.Handler {
	want := []byte(h.masterKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(httpx.BearerToken(r))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_master_key", "invalid or missing master key")
			return
		}
		next(w, r)
	})
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
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
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data":        h.router.Models(),
		"providers":   h.router.Providers(),
		"passthrough": h.router.Passthrough(),
	})
}

// ---- usage ----

func usageFilter(r *http.Request) (store.UsageFilter, error) {
	q := r.URL.Query()
	f := store.UsageFilter{
		KeyID: q.Get("key_id"), TenantID: q.Get("tenant_id"), AppID: q.Get("app_id"),
		EndUser: q.Get("end_user"), Model: q.Get("model"),
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

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found")
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
