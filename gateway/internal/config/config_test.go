package config

import (
	"strings"
	"testing"
	"time"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("AIF_TEST_A", "alpha")
	got := ExpandEnv("a=${AIF_TEST_A} b=${AIF_TEST_MISSING:-dflt} c=${AIF_TEST_MISSING} d=$KEEP")
	if want := "a=alpha b=dflt c= d=$KEEP"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDefaultsAndOpenRouter(t *testing.T) {
	cfg, err := Parse([]byte(`
admin: { listen: "off" }
database: { url: "postgres://x" }
providers:
  - { name: openrouter, type: openrouter }
models:
  - { name: m, provider: openrouter }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].BaseURL != openRouterBaseURL || cfg.Models[0].Upstream != "m" || cfg.Server.Listen != ":8080" ||
		cfg.Database.MaxConns != 10 || cfg.Admin.SessionTTL != 12*time.Hour || cfg.Admin.CookieSecure != "auto" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestNATSDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{admin: { listen: "off" }, database: { url: "postgres://x" }}`))
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.NATS
	if n.URL != "nats://127.0.0.1:4222" || n.Usage.SubjectPrefix != "aif.usage.v1" || n.Usage.Stream != "AIF_USAGE" ||
		!n.Usage.UseJetStream() || n.Control.SubjectPrefix != "aif.control.v1" || n.Hooks.PreRequest.Enabled() ||
		n.Hooks.PreRequest.Timeout != 200*time.Millisecond {
		t.Fatalf("nats defaults: %+v", n)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("AIF_MASTER_KEY", "0123456789abcdef0")
	t.Setenv("DATABASE_URL", "postgres://aif@127.0.0.1/aif")
	for _, f := range []string{"../../aif.example.yaml", "../../aif.dev.yaml"} {
		if _, err := Load(f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

func TestValidate(t *testing.T) {
	const db = "database: {url: \"postgres://x\"}\n"
	if _, err := Parse([]byte(db)); err != nil {
		t.Fatalf("minimal config must load: %v", err)
	}
	cases := map[string]string{
		"no database url":    `providers: []`,
		"sqlite driver":      "database: {url: \"postgres://x\", driver: sqlite}",
		"old dsn":            "database: {url: \"postgres://x\", dsn: aif.db}",
		"pool sizes":         "database: {url: \"postgres://x\", max_conns: 2, min_conns: 3}",
		"short master key":   db + "admin: {master_key: short}",
		"cookie_secure":      db + "admin: {cookie_secure: sometimes}",
		"session_ttl":        db + "admin: {session_ttl: 1s}",
		"bootstrap password": db + "admin: {bootstrap: {email: a@b.c, password: short}}",
		"bootstrap email":    db + "admin: {bootstrap: {email: nope, password: long-enough-password}}",
		"unknown provider":   db + "admin: {listen: \"off\"}\nmodels: [{name: m, provider: x}]",
		"slash in name":      db + "admin: {listen: \"off\"}\nproviders: [{name: a/b, base_url: http://x}]",
		"duplicate model":    db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x}]\nmodels: [{name: m, provider: p}, {name: m, provider: p}]",
		"wildcard subject":   db + "admin: {listen: \"off\"}\nnats: {usage: {subject_prefix: \"aif.*\"}}",
		"empty token":        db + "admin: {listen: \"off\"}\nnats: {hooks: {pre_request: {subject: \"aif..hook\"}}}",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, strings.TrimSpace(err.Error()))
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8081": true, "localhost:8081": true, "[::1]:8081": true,
		"0.0.0.0:8081": false, ":8081": false, "[::]:8081": false, "10.0.0.5:8081": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
