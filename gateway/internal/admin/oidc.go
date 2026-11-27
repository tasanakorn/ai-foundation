package admin

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/httpx"
)

// FlowCookie ties an OIDC sign-in to the browser that started it (spec-0001 section 6.3).
// SameSite=Lax: the IdP sends the browser back from another site, and a Strict cookie is
// not sent on that redirect. Its path keeps it to the OIDC endpoints.
const FlowCookie = "aif_oidc"

const flowPath = "/admin/v1/auth/oidc"

func (h *Handler) methods(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"password": h.auth.PasswordLogin(), "oidc": h.oidc.Providers(),
		"auto_redirect": h.oidc.AutoRedirect()})
}

// oidcStart sends the browser to the IdP. With ?link=1 a signed-in user links that IdP.
func (h *Handler) oidcStart(w http.ResponseWriter, r *http.Request) {
	linkUser := ""
	if r.URL.Query().Get("link") == "1" {
		p, err := h.identify(r)
		if err != nil || p.User == nil {
			h.authPage(w, http.StatusUnauthorized, "Sign in first, then link a login from your account page.", loginError("Sign in first, then link a login from your account page."))
			return
		}
		linkUser = p.User.ID
	}
	to, flow, err := h.oidc.Start(r.Context(), r.PathValue("name"), linkUser)
	var ue *auth.UnavailableError
	switch {
	case errors.Is(err, auth.ErrUnknownProvider):
		h.authPage(w, http.StatusNotFound, "Unknown identity provider.", loginError("Unknown identity provider."))
		return
	case errors.As(err, &ue):
		h.authPage(w, http.StatusServiceUnavailable, ue.Error(), loginError(ue.Error()))
		return
	case err != nil:
		h.log.Error("oidc start failed", "err", err)
		h.authPage(w, http.StatusInternalServerError, "Sign-in could not start.", loginError("Sign-in could not start."))
		return
	}
	h.setFlowCookie(w, r, flow, time.Now().Add(10*time.Minute))
	http.Redirect(w, r, to, http.StatusFound)
}

// oidcCallback is where the IdP sends the browser back. On success it sets the session
// cookie and moves the browser to the UI with a same-origin page, so that request
// carries the SameSite=Strict session cookie.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flow := ""
	if c, err := r.Cookie(FlowCookie); err == nil {
		flow = c.Value
	}
	h.setFlowCookie(w, r, "", time.Unix(0, 0))
	name, ip := r.PathValue("name"), h.clientIP.IP(r)
	var res *auth.CallbackResult
	var err error
	if e := q.Get("error"); e != "" {
		err = &auth.RefusedError{Msg: "the identity provider stopped the sign-in", Reason: e + ": " + q.Get("error_description")}
		if flow != "" { // use the flow up, so it cannot be replayed
			_, _ = h.oidc.Callback(r.Context(), name, flow, "", "", "", ip)
		}
	} else {
		res, err = h.oidc.Callback(r.Context(), name, flow, q.Get("state"), q.Get("code"), r.UserAgent(), ip)
	}
	var re *auth.RefusedError
	var ue *auth.UnavailableError
	switch {
	case errors.As(err, &re):
		h.log.Warn("oidc sign-in refused", "provider", name, "ip", ip, "reason", re.Reason)
		h.authPage(w, http.StatusForbidden, re.Msg, loginError(re.Msg))
		return
	case errors.As(err, &ue):
		h.authPage(w, http.StatusServiceUnavailable, ue.Error(), loginError(ue.Error()))
		return
	case errors.Is(err, auth.ErrUnknownProvider):
		h.authPage(w, http.StatusNotFound, "Unknown identity provider.", loginError("Unknown identity provider."))
		return
	case err != nil:
		h.log.Error("oidc callback failed", "provider", name, "err", err)
		h.authPage(w, http.StatusInternalServerError, "Sign-in failed.", loginError("Sign-in failed."))
		return
	}
	if res.Linked {
		h.authPage(w, http.StatusOK, "Login linked.", "/users?linked=1")
		return
	}
	h.setCookie(w, r, res.Token, time.Now().Add(h.oidcTTL))
	h.authPage(w, http.StatusOK, "Signed in.", "/")
}

// loginError is the sign-in page with an error. The page never redirects to the IdP while it
// shows one, so a failed sign-in cannot loop (spec-0001 section 6.4).
func loginError(msg string) string { return "/login?error=" + url.QueryEscape(msg) }

var authPageTmpl = template.Must(template.New("p").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<meta http-equiv="refresh" content="0;url={{.To}}"><title>aif sign-in</title></head>
<body><p>{{.Msg}}</p><p><a href="{{.To}}">Continue</a></p></body></html>`))

// authPage is a small same-origin page that moves the browser to "to" (a local path).
func (h *Handler) authPage(w http.ResponseWriter, status int, msg, to string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = authPageTmpl.Execute(w, struct{ Msg, To string }{msg, to})
}

func (h *Handler) setFlowCookie(w http.ResponseWriter, r *http.Request, value string, expires time.Time) {
	c := &http.Cookie{Name: FlowCookie, Value: value, Path: flowPath, Expires: expires,
		HttpOnly: true, Secure: h.secure(r), SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// ---- links ----

func (h *Handler) myLinks(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.User == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	h.writeLinks(w, r, p.User.ID)
}

func (h *Handler) deleteMyLink(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if p.User == nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "the master key has no links")
		return
	}
	h.removeLink(w, r, p.User.ID, r.PathValue("id"))
}

func (h *Handler) userLinks(w http.ResponseWriter, r *http.Request) {
	h.writeLinks(w, r, r.PathValue("id"))
}

func (h *Handler) deleteUserLink(w http.ResponseWriter, r *http.Request) {
	h.removeLink(w, r, r.PathValue("id"), r.PathValue("identity_id"))
}

func (h *Handler) allowLink(w http.ResponseWriter, r *http.Request) {
	u, err := h.auth.AllowLink(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("one-time oidc link allowed", "user", u.ID, "until", u.LinkAllowedUntil, "by", actor(r))
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) writeLinks(w http.ResponseWriter, r *http.Request, userID string) {
	links, err := h.auth.ListLinks(r.Context(), userID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": links})
}

func (h *Handler) removeLink(w http.ResponseWriter, r *http.Request, userID, identityID string) {
	if err := h.auth.DeleteLink(r.Context(), userID, identityID); err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("oidc link removed", "user", userID, "identity", identityID, "by", actor(r))
	w.WriteHeader(http.StatusNoContent)
}
