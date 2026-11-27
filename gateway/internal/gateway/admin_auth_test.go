package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
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
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")))
	a := e.browser()
	a.mustLogin(bootEmail, bootPassword)
	code, me, _ := a.do("GET", "/admin/v1/auth/me", "")
	var self struct{ User struct{ ID string } }
	_ = json.Unmarshal([]byte(me), &self)
	if code != 200 || self.User.ID == "" || !strings.Contains(me, `"local":true`) {
		t.Fatalf("me: %d %s", code, me)
	}

	for name, body := range map[string]string{
		"password":      `{"email":"x@example.com","password":"long-enough-password"}`,
		"bad email":     `{"email":"not-an-email"}`,
		"bad role":      `{"email":"x@example.com","role":"root"}`,
		"local account": `{"email":"BOOT@example.com"}`,
		"unknown field": `{"email":"x@example.com","admin":true}`,
	} {
		if code, out, _ := a.do("POST", "/admin/v1/users", body); code != 400 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	code, body, _ := a.do("POST", "/admin/v1/users", `{"email":"Viewer@Example.com","name":"V","role":"viewer"}`)
	if code != 201 || !strings.Contains(body, `"email":"viewer@example.com"`) {
		t.Fatalf("create viewer: %d %s", code, body)
	}
	viewerID := userID(t, body)
	if code, _, _ := a.do("POST", "/admin/v1/users/"+viewerID+"/identities/allow", ""); code != 200 {
		t.Fatal("allow viewer link")
	}
	if code := e.browser().login("viewer@example.com", "anything-long-enough"); code != 401 {
		t.Fatalf("viewer password login: %d", code)
	}
	if code, _, _ := a.do("PATCH", "/admin/v1/users/"+viewerID, `{"password":"long-enough-password"}`); code != 400 {
		t.Fatalf("set a password: %d", code)
	}

	idp.as(idpUser{Sub: "viewer", Email: "viewer@example.com", Verified: true})
	v := e.browser()
	v.oidcLogin("sso")
	for _, path := range []string{"/admin/v1/keys", "/admin/v1/usage/summary", "/admin/v1/models", "/admin/v1/users", "/admin/v1/auth/identities"} {
		if code, _, _ := v.do("GET", path, ""); code != 200 {
			t.Errorf("viewer GET %s: %d", path, code)
		}
	}
	if code, _, _ := v.do("POST", "/admin/v1/keys", `{"name":"x"}`); code != 403 {
		t.Fatalf("viewer create key: %d", code)
	}
	if code, _, _ := v.do("POST", "/admin/v1/users", `{"email":"y@example.com"}`); code != 403 {
		t.Fatalf("viewer create user: %d", code)
	}
	if code, _, _ := v.do("POST", "/admin/v1/users/"+viewerID+"/identities/allow", ""); code != 403 {
		t.Fatalf("viewer allows a link: %d", code)
	}

	// The local account is set in config: no role change, disable, delete or link here.
	for name, req := range map[string][2]string{
		"demote":  {"PATCH", `{"role":"viewer"}`},
		"disable": {"PATCH", `{"disabled":true}`},
		"delete":  {"DELETE", ""},
	} {
		if code, _ := e.do(e.admin.URL, req[0], "/admin/v1/users/"+self.User.ID, masterKey, req[1]); code != 400 {
			t.Errorf("%s the local account: %d", name, code)
		}
	}
	if code, _ := e.do(e.admin.URL, "PATCH", "/admin/v1/users/"+self.User.ID, masterKey, `{"name":"Boot 2"}`); code != 200 {
		t.Fatalf("rename the local account: %d", code)
	}
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/users/"+self.User.ID+"/identities/allow", masterKey, ""); code != 400 {
		t.Fatalf("allow a link for the local account: %d", code)
	}

	// Disabling ends the viewer's sessions at once.
	if code, out, _ := a.do("PATCH", "/admin/v1/users/"+viewerID, `{"disabled":true,"name":"V2"}`); code != 200 || !strings.Contains(out, `"disabled_at"`) {
		t.Fatalf("disable viewer: %d %s", code, out)
	}
	if code, _, _ := v.do("GET", "/admin/v1/keys", ""); code != 401 {
		t.Fatalf("disabled viewer session: %d", code)
	}
	if code, _, _ := a.do("PATCH", "/admin/v1/users/"+viewerID, `{"disabled":false,"role":"admin"}`); code != 200 {
		t.Fatalf("enable + promote: %d", code)
	}
	v.oidcLogin("sso")
	if code, _, _ := v.do("POST", "/admin/v1/keys", `{"name":"x"}`); code != 201 {
		t.Fatalf("promoted user create key: %d", code)
	}
	// Never lock everyone out: the promoted user is not the last admin, so it may be
	// demoted, but not by itself into a state with no admin left.
	if code, _, _ := v.do("PATCH", "/admin/v1/users/"+viewerID, `{"disabled":true}`); code != 400 {
		t.Fatalf("disable self: %d", code)
	}
	if code, _, _ := v.do("DELETE", "/admin/v1/users/"+viewerID, ""); code != 400 {
		t.Fatalf("delete self: %d", code)
	}
	if code, _, _ := a.do("DELETE", "/admin/v1/users/"+viewerID, ""); code != 204 {
		t.Fatalf("delete user: %d", code)
	}
	if code, _, _ := v.do("GET", "/admin/v1/keys", ""); code != 401 {
		t.Fatalf("deleted user's session: %d", code)
	}
	if code, _, _ := a.do("GET", "/admin/v1/users/"+viewerID, ""); code != 404 {
		t.Fatalf("get deleted user: %d", code)
	}
}

func TestAdminLocalAccountFromConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	one, two := e.browser(), e.browser()
	one.mustLogin(bootEmail, bootPassword)
	if code, out, _ := one.do("POST", "/admin/v1/auth/password", `{"current_password":"`+bootPassword+`","new_password":"new-password-123"}`); code != 403 || !strings.Contains(out, "set in config") {
		t.Fatalf("password change: %d %s", code, out)
	}

	// A restart with the same password keeps sessions; a new password ends them.
	e.peer()
	if code, _, _ := one.do("GET", "/admin/v1/auth/me", ""); code != 200 {
		t.Fatalf("session after restart with the same password: %d", code)
	}
	p := e.peer(func(c *config.Config) { c.Admin.Bootstrap.Password = "changed-password-1" })
	if code, _, _ := one.do("GET", "/admin/v1/auth/me", ""); code != 401 {
		t.Fatalf("session after password change: %d", code)
	}
	two = p.browser()
	if code := two.login(bootEmail, bootPassword); code != 401 {
		t.Fatalf("old password: %d", code)
	}
	two.mustLogin(bootEmail, "changed-password-1")

	// A hash in config (production) works the same.
	hash, err := auth.HashPassword("hashed-password-1")
	if err != nil {
		t.Fatal(err)
	}
	p = e.peer(func(c *config.Config) { c.Admin.Bootstrap.Password, c.Admin.Bootstrap.PasswordHash = "", hash })
	if code, _, _ := two.do("GET", "/admin/v1/auth/me", ""); code != 401 {
		t.Fatalf("session after the hash changed: %d", code)
	}
	two = p.browser()
	two.mustLogin(bootEmail, "hashed-password-1")

	// An old password of another account (from before D42) is ignored.
	old, _ := auth.HashPassword("old-password-123")
	now := time.Now().UTC()
	if err := e.st.CreateAdminUser(context.Background(), &store.AdminUser{ID: "usr_old", Email: "old@example.com", Role: store.RoleAdmin,
		PasswordHash: old, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if code := p.browser().login("old@example.com", "old-password-123"); code != 401 {
		t.Fatalf("old password of another account: %d", code)
	}
	code, body, _ := two.do("GET", "/admin/v1/users", "")
	if code != 200 || strings.Count(body, `"local":true`) != 1 || strings.Count(body, `"email"`) != 2 {
		t.Fatalf("users: %d %s", code, body)
	}
}

func TestAdminBreakGlassAudit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	nc, err := nats.Connect(e.natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	sub, err := nc.SubscribeSync("aif.audit.v1.breakglass")
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	e.browser().mustLogin(bootEmail, bootPassword)
	for range 3 { // the master key sends one event per minute, not one per call
		e.do(e.admin.URL, "GET", "/admin/v1/keys", masterKey, "")
	}
	var got []map[string]any
	for {
		m, err := sub.NextMsg(300 * time.Millisecond)
		if err != nil {
			break
		}
		var ev map[string]any
		_ = json.Unmarshal(m.Data, &ev)
		got = append(got, ev)
	}
	if len(got) != 2 || got[0]["method"] != "password" || got[0]["email"] != bootEmail || got[0]["type"] != "breakglass" ||
		got[1]["method"] != "master_key" || got[0]["node"] == "" || got[0]["ip"] == "" {
		t.Fatalf("events: %v", got)
	}
}

func TestAdminPasswordLoginOff(t *testing.T) {
	t.Parallel()
	off := false
	e := newEnv(t, func(c *config.Config) { c.Admin.PasswordLogin = &off })
	b := e.browser()
	if code := b.login(bootEmail, bootPassword); code != 403 {
		t.Fatalf("password login off: %d", code)
	}
	code, body := e.do(e.admin.URL, "GET", "/admin/v1/auth/methods", "", "")
	if code != 200 || !strings.Contains(body, `"password":false`) || !strings.Contains(body, `"oidc":[]`) {
		t.Fatalf("methods: %d %s", code, body)
	}
	if code, _ := e.do(e.admin.URL, "GET", "/admin/v1/keys", masterKey, ""); code != 200 {
		t.Fatalf("master key: %d", code)
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
