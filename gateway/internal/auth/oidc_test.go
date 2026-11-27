package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
)

// An IdP that is down at startup is tried again on a later sign-in, at most every 30s.
func TestDiscoveryRetry(t *testing.T) {
	var up atomic.Bool
	var calls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !up.Load() {
			http.Error(w, "down", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"issuer":"`+srv.URL+`","authorization_endpoint":"`+srv.URL+`/a","token_endpoint":"`+srv.URL+`/t","jwks_uri":"`+srv.URL+`/k"}`)
	}))
	defer srv.Close()

	now := time.Now()
	a := config.Admin{PublicURL: "http://127.0.0.1:1", OIDC: config.AdminOIDC{SessionTTL: time.Hour,
		Providers: []config.OIDCProvider{{Name: "sso", Title: "SSO", Issuer: srv.URL, ClientID: "c"}}}}
	o := NewOIDC(context.Background(), nil, nil, a, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	o.now = func() time.Time { return now }
	if calls.Load() != 1 {
		t.Fatalf("discovery at startup: %d calls", calls.Load())
	}
	if len(o.Providers()) != 1 {
		t.Fatal("the button stays while the IdP is down")
	}

	up.Store(true)
	var ue *UnavailableError
	if _, _, err := o.lookup(context.Background(), "sso"); !errors.As(err, &ue) || calls.Load() != 1 {
		t.Fatalf("within 30s: %v, %d calls", err, calls.Load())
	}
	now = now.Add(31 * time.Second)
	if _, op, err := o.lookup(context.Background(), "sso"); err != nil || op == nil || calls.Load() != 2 {
		t.Fatalf("after 30s: %v, %d calls", err, calls.Load())
	}
	if _, _, err := o.lookup(context.Background(), "sso"); err != nil || calls.Load() != 2 {
		t.Fatalf("cached: %v, %d calls", err, calls.Load())
	}
	if _, _, err := o.lookup(context.Background(), "other"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown: %v", err)
	}
}
