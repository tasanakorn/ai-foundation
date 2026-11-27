package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"time"

	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/httpx"
	"ai-foundation/gateway/internal/store"
)

// SessionCookie holds the browser session token. HttpOnly (scripts cannot read it),
// SameSite=Strict, Secure on HTTPS.
const SessionCookie = "aif_session"

// Principal is who is calling the admin API.
type Principal struct {
	User        *store.AdminUser // nil for the master key
	Role        string
	Method      string // "session" or "master_key"
	sessionHash string
}

type ctxKey struct{}

func principal(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxKey{}).(*Principal)
	return p
}

// authenticate accepts the master key (bearer) or a session cookie, then checks the role:
// viewers may only read. anyRole endpoints act only on the caller's own account.
func (h *Handler) authenticate(next http.HandlerFunc, anyRole bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.identify(r)
		if err != nil {
			if !errors.Is(err, auth.ErrNoSession) {
				h.fail(w, err)
				return
			}
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "unauthenticated", "sign in, or send a valid master key")
			return
		}
		if p.Method == "session" && !h.checkOrigin(w, r) {
			return
		}
		if !anyRole && p.Role != store.RoleAdmin && !safeMethod(r.Method) {
			httpx.WriteError(w, http.StatusForbidden, "invalid_request_error", "forbidden", "your role ("+p.Role+") can only read")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

func (h *Handler) identify(r *http.Request) (*Principal, error) {
	if bearer := httpx.BearerToken(r); bearer != "" {
		if h.masterKey != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(h.masterKey)) == 1 {
			return &Principal{Role: store.RoleAdmin, Method: "master_key"}, nil
		}
		return nil, auth.ErrNoSession // a wrong bearer never falls back to the cookie
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, auth.ErrNoSession
	}
	u, sess, err := h.auth.Session(r.Context(), c.Value)
	if err != nil {
		return nil, err
	}
	return &Principal{User: u, Role: u.Role, Method: "session", sessionHash: sess.Hash}, nil
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin guards cookie endpoints that have no session yet (login, logout).
func (h *Handler) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.checkOrigin(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

// checkOrigin blocks cross-site requests that change state (CSRF). Browsers send
// Origin on such requests; it must match the host the request was sent to.
// SameSite=Strict on the cookie is the first line; this is the second.
func (h *Handler) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if safeMethod(r.Method) {
		return true
	}
	ok := true
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		ok = err == nil && u.Host == r.Host
	} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		ok = site == "same-origin" || site == "none"
	}
	if !ok {
		httpx.WriteError(w, http.StatusForbidden, "invalid_request_error", "cross_origin", "cross-origin request blocked")
	}
	return ok
}

// ---- sign in / out ----

type userView struct {
	User   *store.AdminUser `json:"user"`
	Role   string           `json:"role"`
	Method string           `json:"method"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	token, u, err := h.auth.Login(r.Context(), in.Email, in.Password, r.UserAgent(), h.clientIP.IP(r))
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_credentials", err.Error())
		return
	case errors.Is(err, auth.ErrTooManyAttempts):
		w.Header().Set("Retry-After", "900")
		httpx.WriteError(w, http.StatusTooManyRequests, "invalid_request_error", "too_many_attempts", err.Error())
		return
	case err != nil:
		h.fail(w, err)
		return
	}
	h.setCookie(w, r, token, time.Now().Add(h.auth.TTL()))
	h.log.Info("admin sign-in", "user", u.ID, "email", u.Email, "ip", h.clientIP.IP(r))
	httpx.WriteJSON(w, http.StatusOK, userView{User: u, Role: u.Role, Method: "session"})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		if err := h.auth.Logout(r.Context(), c.Value); err != nil {
			h.fail(w, err)
			return
		}
	}
	h.setCookie(w, r, "", time.Unix(0, 0))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	httpx.WriteJSON(w, http.StatusOK, userView{User: p.User, Role: p.Role, Method: p.Method})
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.User == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "the master key has no password; sign in with an account")
		return
	}
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := h.auth.ChangePassword(r.Context(), p.User, in.CurrentPassword, in.NewPassword, p.sessionHash); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	secure := h.cookieSecure == "always" ||
		(h.cookieSecure == "auto" && (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"))
	c := &http.Cookie{Name: SessionCookie, Value: value, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// ---- accounts ----

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.st.ListAdminUsers(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": users})
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := h.st.GetAdminUser(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var in auth.CreateUserInput
	if !decode(w, r, &in) {
		return
	}
	u, err := h.auth.CreateUser(r.Context(), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("admin account created", "user", u.ID, "email", u.Email, "role", u.Role, "by", actor(r))
	httpx.WriteJSON(w, http.StatusCreated, u)
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	var in auth.UpdateUserInput
	if !decode(w, r, &in) {
		return
	}
	u, err := h.auth.UpdateUser(r.Context(), r.PathValue("id"), actorID(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("admin account updated", "user", u.ID, "by", actor(r))
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.DeleteUser(r.Context(), r.PathValue("id"), actorID(r)); err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("admin account deleted", "user", r.PathValue("id"), "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}

func actorID(r *http.Request) string {
	if p := principal(r); p != nil && p.User != nil {
		return p.User.ID
	}
	return ""
}

func actor(r *http.Request) string {
	if id := actorID(r); id != "" {
		return id
	}
	return "master_key"
}
