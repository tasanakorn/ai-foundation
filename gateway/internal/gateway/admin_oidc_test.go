package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/config"
)

// oidc runs a browser through start -> IdP -> callback and returns the final page.
func (b *browser) oidc(path string) (int, string) {
	b.e.t.Helper()
	code, body, _ := b.do("GET", path, "")
	return code, body
}

// oidcLogin signs in through the IdP called name and fails the test if it does not work.
func (b *browser) oidcLogin(name string) {
	b.e.t.Helper()
	if code, body := b.oidc("/admin/v1/auth/oidc/" + name + "/start"); code != 200 || !strings.Contains(body, "Signed in.") {
		b.e.t.Fatalf("oidc login via %s: %d %s", name, code, body)
	}
}

// createOIDCUser adds an account (no password) with the master key and allows its first link.
func (e *env) createOIDCUser(email, role string) string {
	e.t.Helper()
	code, body := e.do(e.admin.URL, "POST", "/admin/v1/users", masterKey, `{"email":"`+email+`","role":"`+role+`"}`)
	if code != 201 {
		e.t.Fatalf("create %s: %d %s", email, code, body)
	}
	id := userID(e.t, body)
	if code, body := e.do(e.admin.URL, "POST", "/admin/v1/users/"+id+"/identities/allow", masterKey, ""); code != 200 || !strings.Contains(body, "link_allowed_until") {
		e.t.Fatalf("allow link %s: %d %s", email, code, body)
	}
	return id
}

func TestAdminOIDCLoginAndLinks(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")))

	code, body := e.do(e.admin.URL, "GET", "/admin/v1/auth/methods", "", "")
	if code != 200 || !strings.Contains(body, `"password":true`) || !strings.Contains(body, `{"name":"sso","title":"Fake sso"}`) {
		t.Fatalf("methods: %d %s", code, body)
	}

	// Without an account, or before a link is allowed, the login is refused.
	idp.as(idpUser{Sub: "u1", Email: "Ann@Example.com", Verified: true})
	b := e.browser()
	if code, body := b.oidc("/admin/v1/auth/oidc/sso/start"); code != 403 || !strings.Contains(body, "no admin account is linked") {
		t.Fatalf("no account: %d %s", code, body)
	}
	code, body = e.do(e.admin.URL, "POST", "/admin/v1/users", masterKey, `{"email":"ann@example.com","role":"admin"}`)
	if code != 201 || !strings.Contains(body, `"local":false`) {
		t.Fatalf("create: %d %s", code, body)
	}
	annID := userID(t, body)
	if code, _ := b.oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
		t.Fatalf("no allowance: %d", code)
	}

	// An email the IdP has not verified does not use the allowance.
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/users/"+annID+"/identities/allow", masterKey, ""); code != 200 {
		t.Fatal("allow")
	}
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: false})
	if code, _ := b.oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
		t.Fatalf("unverified email: %d", code)
	}

	// With the allowance and a verified email, the first login links the account.
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})
	b.oidcLogin("sso")
	var session *http.Cookie
	for _, c := range b.c.Jar.Cookies(mustURL(t, e.admin.URL)) {
		if c.Name == admin.SessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie after oidc login")
	}
	code, body, _ = b.do("GET", "/admin/v1/auth/me", "")
	if code != 200 || !strings.Contains(body, "ann@example.com") || !strings.Contains(body, `"local":false`) {
		t.Fatalf("me: %d %s", code, body)
	}
	if code, _, _ := b.do("POST", "/admin/v1/keys", `{"name":"by-ann"}`); code != 201 {
		t.Fatalf("admin via oidc creates key: %d", code)
	}
	code, body, _ = b.do("GET", "/admin/v1/users/"+annID, "")
	if code != 200 || strings.Contains(body, "link_allowed_until") {
		t.Fatalf("allowance used up: %d %s", code, body)
	}

	// Later logins match by the link, not the email: a changed email still signs in,
	// and another IdP user with Ann's email does not.
	idp.as(idpUser{Sub: "u1", Email: "ann.new@example.com", Verified: true})
	e.browser().oidcLogin("sso")
	idp.as(idpUser{Sub: "intruder", Email: "ann@example.com", Verified: true})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
		t.Fatalf("same email, other subject: %d", code)
	}

	// Own links: list, and link a second IdP user from the signed-in session.
	code, body, _ = b.do("GET", "/admin/v1/auth/identities", "")
	if code != 200 || strings.Count(body, `"subject"`) != 1 || !strings.Contains(body, `"subject":"u1"`) {
		t.Fatalf("my links: %d %s", code, body)
	}
	idp.as(idpUser{Sub: "u1-second", Email: "other@example.com", Verified: false})
	if code, body := b.oidc("/admin/v1/auth/oidc/sso/start?link=1"); code != 200 || !strings.Contains(body, "Login linked.") {
		t.Fatalf("link from session: %d %s", code, body)
	}
	e.browser().oidcLogin("sso") // u1-second now signs in as Ann
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/sso/start?link=1"); code != 401 {
		t.Fatalf("link without session: %d", code)
	}
	// The same IdP user cannot be linked to a second account.
	e.createOIDCUser("bob@example.com", "viewer")
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true})
	bob := e.browser()
	bob.oidcLogin("sso")
	idp.as(idpUser{Sub: "u1", Email: "x@example.com", Verified: true})
	if code, body := bob.oidc("/admin/v1/auth/oidc/sso/start?link=1"); code != 403 || !strings.Contains(body, "already linked") {
		t.Fatalf("link conflict: %d %s", code, body)
	}

	// Remove a link: the admin API for any user, and a user for their own.
	var links struct {
		Data []struct{ ID, Subject string }
	}
	_, body = e.do(e.admin.URL, "GET", "/admin/v1/users/"+annID+"/identities", masterKey, "")
	_ = json.Unmarshal([]byte(body), &links)
	if len(links.Data) != 2 {
		t.Fatalf("ann's links: %s", body)
	}
	if code, _, _ := bob.do("DELETE", "/admin/v1/auth/identities/"+links.Data[0].ID, ""); code != 404 {
		t.Fatalf("bob removes ann's link: %d", code)
	}
	if code, _ := e.do(e.admin.URL, "DELETE", "/admin/v1/users/"+annID+"/identities/"+links.Data[1].ID, masterKey, ""); code != 204 {
		t.Fatalf("admin removes link: %d", code)
	}
	if code, _, _ := b.do("DELETE", "/admin/v1/auth/identities/"+links.Data[0].ID, ""); code != 204 {
		t.Fatalf("remove own link: %d", code)
	}
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
		t.Fatalf("login after link removed: %d", code)
	}

	// A disabled account cannot sign in, linked or not.
	bobID := ""
	_, body = e.do(e.admin.URL, "GET", "/admin/v1/users", masterKey, "")
	var users struct{ Data []struct{ ID, Email string } }
	_ = json.Unmarshal([]byte(body), &users)
	for _, u := range users.Data {
		if u.Email == "bob@example.com" {
			bobID = u.ID
		}
	}
	if code, _ := e.do(e.admin.URL, "PATCH", "/admin/v1/users/"+bobID, masterKey, `{"disabled":true}`); code != 200 {
		t.Fatal("disable bob")
	}
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true})
	if code, body := e.browser().oidc("/admin/v1/auth/oidc/sso/start"); code != 403 || !strings.Contains(body, "disabled") {
		t.Fatalf("disabled: %d %s", code, body)
	}
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/users/"+bobID+"/identities/allow", masterKey, ""); code != 400 {
		t.Fatalf("allow link for disabled account: %d", code)
	}

	// Unknown provider.
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/nope/start"); code != 404 {
		t.Fatalf("unknown provider: %d", code)
	}
}

func TestAdminOIDCAllowanceExpires(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")))
	id := e.createOIDCUser("ann@example.com", "admin")
	past := time.Now().Add(-time.Minute)
	if err := e.st.AllowAdminLink(context.Background(), id, &past); err != nil {
		t.Fatal(err)
	}
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
		t.Fatalf("expired allowance: %d", code)
	}
}

func TestAdminOIDCLinkByEmailAndGroups(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	open := idp.provider("open")
	open.LinkByEmail = true
	staff := idp.provider("staff")
	staff.AllowedGroups = []string{"aif-admins"}
	e := newEnv(t, withIDP(open, staff))

	// link_by_email: no admin step needed, but the email must be verified.
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/users", masterKey, `{"email":"ann@example.com"}`); code != 201 {
		t.Fatal("create ann")
	}
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: false})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/open/start"); code != 403 {
		t.Fatalf("link_by_email, unverified: %d", code)
	}
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})
	e.browser().oidcLogin("open")

	// allowed_groups: a linked account outside the group is refused, inside it works.
	e.createOIDCUser("bob@example.com", "viewer")
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true, Groups: []string{"end-users"}})
	if code, body := e.browser().oidc("/admin/v1/auth/oidc/staff/start"); code != 403 || !strings.Contains(body, "not in a group") {
		t.Fatalf("not in group: %d %s", code, body)
	}
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/staff/start"); code != 403 {
		t.Fatalf("no groups claim: %d", code)
	}
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true, Groups: []string{"end-users", "aif-admins"}})
	e.browser().oidcLogin("staff")

	// Claims only in userinfo (not in the ID token) are read from there.
	idp.claimsInUserinfo = true
	e.browser().oidcLogin("staff")
	idp.as(idpUser{Sub: "bob", Email: "bob@example.com", Verified: true, Groups: []string{"end-users"}})
	if code, _ := e.browser().oidc("/admin/v1/auth/oidc/staff/start"); code != 403 {
		t.Fatalf("userinfo groups checked: %d", code)
	}
}

func TestAdminOIDCBadTokensAndReplay(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")))
	e.createOIDCUser("ann@example.com", "admin")
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})

	for name, knob := range map[string]*bool{"audience": &idp.badAud, "nonce": &idp.badNonce, "signature": &idp.badKey} {
		*knob = true
		if code, _ := e.browser().oidc("/admin/v1/auth/oidc/sso/start"); code != 403 {
			t.Errorf("bad %s: %d", name, code)
		}
		*knob = false
	}

	// Stop at the callback, to replay it and to change it.
	b := e.browser()
	b.c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.Contains(req.URL.Path, "/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	_, _, resp := b.do("GET", "/admin/v1/auth/oidc/sso/start", "")
	callback := resp.Header.Get("Location")
	if !strings.HasPrefix(callback, e.admin.URL+"/admin/v1/auth/oidc/sso/callback?") {
		t.Fatalf("callback: %q", callback)
	}
	var flow *http.Cookie
	for _, c := range resp.Request.Response.Cookies() {
		if c.Name == admin.FlowCookie {
			flow = c
		}
	}
	if flow == nil || !flow.HttpOnly || flow.SameSite != http.SameSiteLaxMode || flow.Path != "/admin/v1/auth/oidc" {
		t.Fatalf("flow cookie: %+v", flow)
	}
	get := func(u string, cookie *http.Cookie) int {
		req, _ := http.NewRequest("GET", u, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	wrongState := strings.Replace(callback, "state=", "state=x", 1)
	if code := get(callback, nil); code != 403 {
		t.Fatalf("no flow cookie: %d", code)
	}
	if code := get(wrongState, flow); code != 403 {
		t.Fatalf("wrong state: %d", code)
	}
	if code := get(callback, flow); code != 403 { // the wrong-state try used the flow up
		t.Fatalf("flow after a failed try: %d", code)
	}

	// A good callback works once, also on another node (the flow is in Postgres).
	_, _, resp = b.do("GET", "/admin/v1/auth/oidc/sso/start", "")
	callback = resp.Header.Get("Location")
	for _, c := range resp.Request.Response.Cookies() {
		if c.Name == admin.FlowCookie {
			flow = c
		}
	}
	p := e.peer(func(c *config.Config) { c.Admin.PublicURL = e.admin.URL }) // one public URL, as behind a load balancer
	onPeer := strings.Replace(callback, e.admin.URL, p.admin.URL, 1)
	if code := get(onPeer, flow); code != 200 {
		t.Fatalf("callback on the other node: %d", code)
	}
	if code := get(onPeer, flow); code != 403 {
		t.Fatalf("replayed callback: %d", code)
	}

	// The IdP reporting an error ends at the sign-in page.
	u, _ := url.Parse(e.admin.URL + "/admin/v1/auth/oidc/sso/callback?error=access_denied")
	if code := get(u.String(), flow); code != 403 {
		t.Fatalf("idp error: %d", code)
	}
}

func TestAdminOIDCSessionTTL(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")), func(c *config.Config) { c.Admin.OIDC.SessionTTL = time.Hour })
	e.createOIDCUser("ann@example.com", "admin")
	idp.as(idpUser{Sub: "u1", Email: "ann@example.com", Verified: true})
	b := e.browser()
	b.oidcLogin("sso")
	for _, c := range b.c.Jar.Cookies(mustURL(t, e.admin.URL)) {
		if c.Name != admin.SessionCookie {
			continue
		}
		sum := sha256.Sum256([]byte(c.Value))
		sess, err := e.st.GetAdminSession(context.Background(), hex.EncodeToString(sum[:]))
		if err != nil {
			t.Fatal(err)
		}
		if left := time.Until(sess.ExpiresAt); left > time.Hour || left < 59*time.Minute {
			t.Fatalf("oidc session lasts %v, want 1h", left)
		}
		return
	}
	t.Fatal("no session cookie")
}
