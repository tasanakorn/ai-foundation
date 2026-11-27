package gateway_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
)

// idpUser is who the fake IdP signs in as on the next /authorize.
type idpUser struct {
	Sub      string
	Email    string
	Verified bool
	Groups   []string
}

// fakeIDP is a small OIDC provider: discovery, authorize (no login page: it signs in as
// next), token (checks PKCE), userinfo and JWKS. Knobs break one thing at a time.
type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu    sync.Mutex
	next  idpUser
	codes map[string]idpCode

	// knobs
	badAud, badNonce, badKey bool
	claimsInUserinfo         bool // email and groups only from userinfo, not in the ID token
}

type idpCode struct {
	user                            idpUser
	nonce, challenge, redirect, cid string
}

const idpClientID, idpSecret = "aif-admin", "idp-client-secret"

func newFakeIDP(t *testing.T) *fakeIDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{t: t, key: key, kid: "k1", codes: map[string]idpCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", f.userinfo)
	mux.HandleFunc("GET /jwks", f.jwks)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) URL() string { return f.srv.URL }

func (f *fakeIDP) as(u idpUser) {
	f.mu.Lock()
	f.next = u
	f.mu.Unlock()
}

// provider is an admin.oidc.providers entry for this IdP.
func (f *fakeIDP) provider(name string) config.OIDCProvider {
	return config.OIDCProvider{Name: name, Title: "Fake " + name, Issuer: f.srv.URL, ClientID: idpClientID,
		ClientSecret: idpSecret, Scopes: []string{"openid", "email", "profile", "groups"}, GroupsClaim: "groups"}
}

// withIDP adds providers to the gateway config.
func withIDP(ps ...config.OIDCProvider) func(*config.Config) {
	return func(c *config.Config) { c.Admin.OIDC.Providers = append(c.Admin.OIDC.Providers, ps...) }
}

func (f *fakeIDP) discovery(w http.ResponseWriter, r *http.Request) {
	u := f.srv.URL
	writeJSON(w, map[string]any{"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
		"userinfo_endpoint": u + "/userinfo", "jwks_uri": u + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}})
}

func (f *fakeIDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("nonce") == "" || q.Get("state") == "" || !strings.Contains(q.Get("scope"), "openid") {
		f.t.Errorf("authorize: bad request %v", q)
		http.Error(w, "bad request", 400)
		return
	}
	f.mu.Lock()
	code := "code-" + randString()
	f.codes[code] = idpCode{user: f.next, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirect: q.Get("redirect_uri"), cid: q.Get("client_id")}
	f.mu.Unlock()
	http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
}

func (f *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	f.mu.Lock()
	c, found := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code")) // one use
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case id != idpClientID || secret != idpSecret:
		http.Error(w, `{"error":"invalid_client"}`, 401)
		return
	case !found || c.cid != id || c.redirect != r.PostForm.Get("redirect_uri"):
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge:
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`, 400)
		return
	}
	claims := map[string]any{"iss": f.srv.URL, "sub": c.user.Sub, "aud": idpClientID, "nonce": c.nonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
	if !f.claimsInUserinfo {
		claims["email"], claims["email_verified"], claims["groups"] = c.user.Email, c.user.Verified, c.user.Groups
	}
	if f.badAud {
		claims["aud"] = "someone-else"
	}
	if f.badNonce {
		claims["nonce"] = "other-nonce"
	}
	access := "at-" + randString()
	f.mu.Lock()
	f.codes[access] = c // userinfo looks it up
	f.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": f.sign(claims)})
}

func (f *fakeIDP) userinfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	c, ok := f.codes[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no", 401)
		return
	}
	writeJSON(w, map[string]any{"sub": c.user.Sub, "email": c.user.Email, "email_verified": c.user.Verified, "groups": c.user.Groups})
}

func (f *fakeIDP) jwks(w http.ResponseWriter, r *http.Request) {
	pub := f.key.PublicKey
	writeJSON(w, map[string]any{"keys": []map[string]any{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": f.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}})
}

func (f *fakeIDP) sign(claims map[string]any) string {
	key := f.key
	if f.badKey {
		key, _ = rsa.GenerateKey(rand.Reader, 2048)
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": f.kid})
	b, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randString() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
