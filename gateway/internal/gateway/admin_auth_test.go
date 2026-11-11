package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/config"
)

// browser is one admin UI user: a cookie jar, and Origin set like a real browser does.
type browser struct {
	e      *env
	c      *http.Client
	origin string
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{Jar: jar}, origin: e.admin.URL}
}

func (b *browser) do(method, path, body string) (int, string, *http.Response) {
	b.e.t.Helper()
	req, _ := http.NewRequest(method, b.e.admin.URL+path, strings.NewReader(body))
	if method != "GET" && b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), resp
}

func (b *browser) login(email, password string) int {
	b.e.t.Helper()
	code, _, _ := b.do("POST", "/admin/v1/auth/login", `{"email":"`+email+`","password":"`+password+`"}`)
	return code
}

func (b *browser) mustLogin(email, password string) {
	b.e.t.Helper()
	if code := b.login(email, password); code != 200 {
		b.e.t.Fatalf("login %s: %d", email, code)
	}
}

func userID(t *testing.T, body string) string {
	t.Helper()
	var u struct{ ID string }
	if err := json.Unmarshal([]byte(body), &u); err != nil || u.ID == "" {
		t.Fatalf("no user id in %s", body)
	}
	return u.ID
}

func TestAdminLoginFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.browser()

	if code, _, _ := b.do("GET", "/admin/v1/keys", ""); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	if code := b.login(bootEmail, "wrong-password-123"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code := b.login("nobody@example.com", bootPassword); code != 401 {
		t.Fatalf("unknown email: %d", code)
	}

	code, body, resp := b.do("POST", "/admin/v1/auth/login", `{"email":"  BOOT@Example.com ","password":"`+bootPassword+`"}`)
	if code != 200 || !strings.Contains(body, `"role":"admin"`) || strings.Contains(body, "password") {
		t.Fatalf("login: %d %s", code, body)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure || !strings.HasPrefix(cookie.Value, "aifs_") {
		t.Fatalf("session cookie: %+v", cookie)
	}

	code, body, _ = b.do("GET", "/admin/v1/auth/me", "")
	if code != 200 || !strings.Contains(body, bootEmail) || !strings.Contains(body, `"method":"session"`) || !strings.Contains(body, `"last_login_at"`) {
		t.Fatalf("me: %d %s", code, body)
	}
	if code, _, _ := b.do("POST", "/admin/v1/keys", `{"name":"from-ui"}`); code != 201 {
		t.Fatalf("create key with session: %d", code)
	}

	// The master key still works for scripts, and reports itself.
	code, body = e.do(e.admin.URL, "GET", "/admin/v1/auth/me", masterKey, "")
	if code != 200 || !strings.Contains(body, `"method":"master_key"`) || !strings.Contains(body, `"user":null`) {
		t.Fatalf("me with master key: %d %s", code, body)
	}
	// A wrong bearer never falls back to the cookie.
	req, _ := http.NewRequest("GET", e.admin.URL+"/admin/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, _ := b.c.Do(req); resp.StatusCode != 401 {
		t.Fatalf("wrong bearer + valid cookie: %d", resp.StatusCode)
	}

	if code, _, _ := b.do("POST", "/admin/v1/auth/logout", ""); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ := b.do("GET", "/admin/v1/auth/me", ""); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

func TestAdminRolesAndAccounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.browser()
	a.mustLogin(bootEmail, bootPassword)
	code, me, _ := a.do("GET", "/admin/v1/auth/me", "")
	var self struct{ User struct{ ID string } }
	_ = json.Unmarshal([]byte(me), &self)
	if code != 200 || self.User.ID == "" {
		t.Fatalf("me: %d %s", code, me)
	}

	for name, body := range map[string]string{
		"short password": `{"email":"x@example.com","password":"short"}`,
		"bad email":      `{"email":"not-an-email","password":"long-enough-password"}`,
		"bad role":       `{"email":"x@example.com","password":"long-enough-password","role":"root"}`,
		"duplicate":      `{"email":"BOOT@example.com","password":"long-enough-password"}`,
		"unknown field":  `{"email":"x@example.com","password":"long-enough-password","admin":true}`,
	} {
		if code, out, _ := a.do("POST", "/admin/v1/users", body); code != 400 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	code, body, _ := a.do("POST", "/admin/v1/users", `{"email":"Viewer@Example.com","name":"V","role":"viewer","password":"viewer-password-1"}`)
	if code != 201 || !strings.Contains(body, `"email":"viewer@example.com"`) {
		t.Fatalf("create viewer: %d %s", code, body)
	}
	viewerID := userID(t, body)

	v := e.browser()
	v.mustLogin("viewer@example.com", "viewer-password-1")
	for _, path := range []string{"/admin/v1/keys", "/admin/v1/usage/summary", "/admin/v1/models", "/admin/v1/users"} {
		if code, _, _ := v.do("GET", path, ""); code != 200 {
			t.Errorf("viewer GET %s: %d", path, code)
		}
	}
	if code, _, _ := v.do("POST", "/admin/v1/keys", `{"name":"x"}`); code != 403 {
		t.Fatalf("viewer create key: %d", code)
	}
	if code, _, _ := v.do("POST", "/admin/v1/users", `{"email":"y@example.com","password":"long-enough-password"}`); code != 403 {
		t.Fatalf("viewer create user: %d", code)
	}
	// Any role may change its own password.
	if code, out, _ := v.do("POST", "/admin/v1/auth/password", `{"current_password":"viewer-password-1","new_password":"viewer-password-2"}`); code != 204 {
		t.Fatalf("viewer own password: %d %s", code, out)
	}

	// Never lock everyone out.
	if code, out, _ := a.do("PATCH", "/admin/v1/users/"+self.User.ID, `{"role":"viewer"}`); code != 400 || !strings.Contains(out, "last active admin") {
		t.Fatalf("demote last admin: %d %s", code, out)
	}
	if code, _, _ := a.do("PATCH", "/admin/v1/users/"+self.User.ID, `{"disabled":true}`); code != 400 {
		t.Fatalf("disable self: %d", code)
	}
	if code, _, _ := a.do("DELETE", "/admin/v1/users/"+self.User.ID, ""); code != 400 {
		t.Fatalf("delete self: %d", code)
	}
	if code, _ := e.do(e.admin.URL, "DELETE", "/admin/v1/users/"+self.User.ID, masterKey, ""); code != 400 {
		t.Fatalf("master key deletes last admin: %d", code)
	}

	// Disabling ends the viewer's sessions at once.
	if code, out, _ := a.do("PATCH", "/admin/v1/users/"+viewerID, `{"disabled":true,"name":"V2"}`); code != 200 || !strings.Contains(out, `"disabled_at"`) {
		t.Fatalf("disable viewer: %d %s", code, out)
	}
	if code, _, _ := v.do("GET", "/admin/v1/keys", ""); code != 401 {
		t.Fatalf("disabled viewer session: %d", code)
	}
	if code := v.login("viewer@example.com", "viewer-password-2"); code != 401 {
		t.Fatalf("disabled viewer login: %d", code)
	}
	if code, _, _ := a.do("PATCH", "/admin/v1/users/"+viewerID, `{"disabled":false,"role":"admin"}`); code != 200 {
		t.Fatalf("enable + promote: %d", code)
	}
	v.mustLogin("viewer@example.com", "viewer-password-2")
	if code, _, _ := v.do("POST", "/admin/v1/keys", `{"name":"x"}`); code != 201 {
		t.Fatalf("promoted user create key: %d", code)
	}
	// Now there are two admins: the first may be demoted, then deleted.
	if code, _, _ := v.do("PATCH", "/admin/v1/users/"+self.User.ID, `{"role":"viewer"}`); code != 200 {
		t.Fatalf("demote with another admin: %d", code)
	}
	if code, _, _ := v.do("DELETE", "/admin/v1/users/"+self.User.ID, ""); code != 204 {
		t.Fatalf("delete user: %d", code)
	}
	if code, _, _ := a.do("GET", "/admin/v1/keys", ""); code != 401 {
		t.Fatalf("deleted user's session: %d", code)
	}
	if code, _, _ := v.do("GET", "/admin/v1/users/"+self.User.ID, ""); code != 404 {
		t.Fatalf("get deleted user: %d", code)
	}
}

func TestAdminPasswordChangeEndsOtherSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	one, two := e.browser(), e.browser()
	one.mustLogin(bootEmail, bootPassword)
	two.mustLogin(bootEmail, bootPassword)

	if code, _, _ := one.do("POST", "/admin/v1/auth/password", `{"current_password":"nope-nope-nope","new_password":"new-password-123"}`); code != 400 {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, _, _ := one.do("POST", "/admin/v1/auth/password", `{"current_password":"`+bootPassword+`","new_password":"short"}`); code != 400 {
		t.Fatalf("short new password: %d", code)
	}
	if code, _, _ := one.do("POST", "/admin/v1/auth/password", `{"current_password":"`+bootPassword+`","new_password":"new-password-123"}`); code != 204 {
		t.Fatalf("change password: %d", code)
	}
	if code, _, _ := one.do("GET", "/admin/v1/auth/me", ""); code != 200 {
		t.Fatalf("current session kept: %d", code)
	}
	if code, _, _ := two.do("GET", "/admin/v1/auth/me", ""); code != 401 {
		t.Fatalf("other session ended: %d", code)
	}
	if code := two.login(bootEmail, bootPassword); code != 401 {
		t.Fatalf("old password: %d", code)
	}
	two.mustLogin(bootEmail, "new-password-123")
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/auth/password", masterKey, `{"current_password":"x","new_password":"y"}`); code != 400 {
		t.Fatalf("master key has no password: %d", code)
	}
}

func TestAdminCrossOriginBlocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.browser()
	b.origin = "https://evil.example"
	if code := b.login(bootEmail, bootPassword); code != 403 {
		t.Fatalf("cross-origin login: %d", code)
	}
	b.origin = e.admin.URL
	b.mustLogin(bootEmail, bootPassword)
	b.origin = "https://evil.example"
	if code, _, _ := b.do("POST", "/admin/v1/keys", `{"name":"x"}`); code != 403 {
		t.Fatalf("cross-origin write with cookie: %d", code)
	}
	if code, _, _ := b.do("GET", "/admin/v1/keys", ""); code != 200 {
		t.Fatalf("reads are not blocked: %d", code)
	}
	// Scripts with the master key send no Origin and no cookie; they are not affected.
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/keys", masterKey, `{"name":"x"}`, "Origin", "https://evil.example"); code != 201 {
		t.Fatalf("master key with foreign origin: %d", code)
	}
}

func TestAdminLoginRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.browser()
	for i := range 10 {
		if code := b.login(bootEmail, "wrong-password-123"); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code := b.login(bootEmail, bootPassword); code != 429 {
		t.Fatalf("after 10 failures: %d", code)
	}
}

func TestAdminMasterKeyOptionalAndBootstrapOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *config.Config) { c.Admin.MasterKey = "" })
	if code, _ := e.do(e.admin.URL, "GET", "/admin/v1/keys", masterKey, ""); code != 401 {
		t.Fatalf("master key off: %d", code)
	}
	b := e.browser()
	b.mustLogin(bootEmail, bootPassword)

	// A second node with the same bootstrap config does not add another account.
	p := e.peer()
	pb := p.browser()
	pb.mustLogin(bootEmail, bootPassword)
	code, body, _ := pb.do("GET", "/admin/v1/users", "")
	if code != 200 || strings.Count(body, `"email"`) != 1 {
		t.Fatalf("users after second start: %d %s", code, body)
	}
	// The session from node 1 works on node 2 (shared database).
	req, _ := http.NewRequest("GET", p.admin.URL+"/admin/v1/auth/me", nil)
	for _, c := range b.c.Jar.Cookies(mustURL(t, e.admin.URL)) {
		req.AddCookie(c)
	}
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("session on other node: %v %v", resp, err)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
