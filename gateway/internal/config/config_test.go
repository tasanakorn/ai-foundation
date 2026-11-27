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

func TestAnthropicDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
admin: { listen: "off" }
database: { url: "postgres://x" }
providers:
  - { name: anthropic, type: anthropic, openai_format: compat, workspace_id: wrkspc_1 }
  - { name: proxy, type: anthropic, openai_format: compat, base_url: "http://127.0.0.1:9000/v1/" }
`))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Providers[0]; p.BaseURL != anthropicBaseURL || p.WorkspaceID != "wrkspc_1" || p.OpenAIFormat != OpenAIFormatCompat {
		t.Fatalf("anthropic defaults: %+v", p)
	}
	if p := cfg.Providers[1]; p.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("custom base_url: %q", p.BaseURL)
	}
}

func TestFormatsAndAnthropicServer(t *testing.T) {
	cfg, err := Parse([]byte(`
admin: { listen: "off" }
database: { url: "postgres://x" }
providers:
  - { name: o, base_url: "http://x" }
  - { name: a, type: anthropic, openai_format: compat }
models:
  - { name: om, provider: o }
  - { name: am, provider: a }
  - { name: an, provider: a, formats: [anthropic] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models[0].Formats; len(got) != 1 || got[0] != FormatOpenAI {
		t.Fatalf("openai model formats: %v", got)
	}
	if got := cfg.Models[1].Formats; len(got) != 2 || got[1] != FormatAnthropic {
		t.Fatalf("anthropic model formats: %v", got)
	}
	if got := cfg.Models[2].Formats; len(got) != 1 || got[0] != FormatAnthropic {
		t.Fatalf("explicit formats: %v", got)
	}
	a := cfg.Server.Anthropic
	if a.Prefix() != "/anthropic" || a.ListenEnabled() || a.Listen != "off" {
		t.Fatalf("anthropic server defaults: prefix %q listen %q", a.Prefix(), a.Listen)
	}
	if f := cfg.NATS.Hooks.PreRequest.Formats; len(f) != 1 || f[0] != FormatOpenAI {
		t.Fatalf("hook formats default: %v", f)
	}

	cfg, err = Parse([]byte(`{admin: {listen: "off"}, database: {url: "postgres://x"},
		server: {anthropic: {path_prefix: "", listen: "127.0.0.1:8082"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Server.Anthropic; a.Prefix() != "" || !a.ListenEnabled() {
		t.Fatalf("prefix off / listener on: %+v", a)
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
		"no database url":            `providers: []`,
		"catalog mode":               db + "catalog: {mode: git}",
		"catalog watch":              db + "catalog: {watch_interval: 1ms}",
		"catalog secret key":         db + "catalog: {secret_keys: [c2hvcnQ=]}",
		"api_key and accounts":       db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, api_key: k, accounts: [{name: a}]}]",
		"account name":               db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, accounts: [{name: Plan A}]}]",
		"account twice":              db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, accounts: [{name: a}, {name: a}]}]",
		"weight with priority":       db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, accounts: [{name: a, weight: 2}]}]",
		"weight too big":             db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, policy: round_robin, accounts: [{name: a, weight: 101}]}]",
		"all drained":                db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, policy: round_robin, accounts: [{name: a, weight: 0}]}]",
		"sticky with priority":       db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, sticky: key}]",
		"bad policy":                 db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x, policy: random}]",
		"sqlite driver":              "database: {url: \"postgres://x\", driver: sqlite}",
		"old dsn":                    "database: {url: \"postgres://x\", dsn: aif.db}",
		"pool sizes":                 "database: {url: \"postgres://x\", max_conns: 2, min_conns: 3}",
		"short master key":           db + "admin: {master_key: short}",
		"cookie_secure":              db + "admin: {cookie_secure: sometimes}",
		"session_ttl":                db + "admin: {session_ttl: 1s}",
		"bootstrap password":         db + "admin: {bootstrap: {email: a@b.c, password: short}}",
		"bootstrap email":            db + "admin: {bootstrap: {email: nope, password: long-enough-password}}",
		"unknown provider":           db + "admin: {listen: \"off\"}\nmodels: [{name: m, provider: x}]",
		"slash in name":              db + "admin: {listen: \"off\"}\nproviders: [{name: a/b, base_url: http://x}]",
		"duplicate model":            db + "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x}]\nmodels: [{name: m, provider: p}, {name: m, provider: p}]",
		"wildcard subject":           db + "admin: {listen: \"off\"}\nnats: {usage: {subject_prefix: \"aif.*\"}}",
		"anthropic no openai_format": db + "admin: {listen: \"off\"}\nproviders: [{name: a, type: anthropic}]",
		"anthropic native (later)":   db + "admin: {listen: \"off\"}\nproviders: [{name: a, type: anthropic, openai_format: native}]",
		"anthropic bad format":       db + "admin: {listen: \"off\"}\nproviders: [{name: a, type: anthropic, openai_format: magic}]",
		"openai_format on openai":    db + "admin: {listen: \"off\"}\nproviders: [{name: o, base_url: http://x, openai_format: compat}]",
		"workspace_id on openai":     db + "admin: {listen: \"off\"}\nproviders: [{name: o, base_url: http://x, workspace_id: w}]",
		"empty token":                db + "admin: {listen: \"off\"}\nnats: {hooks: {pre_request: {subject: \"aif..hook\"}}}",
		"prefix without slash":       db + "admin: {listen: \"off\"}\nserver: {anthropic: {path_prefix: anthropic}}",
		"prefix trailing slash":      db + "admin: {listen: \"off\"}\nserver: {anthropic: {path_prefix: /anthropic/}}",
		"prefix clashes with /v1":    db + "admin: {listen: \"off\"}\nserver: {anthropic: {path_prefix: /v1}}",
		"anthropic listen":           db + "admin: {listen: \"off\"}\nserver: {anthropic: {listen: nope}}",
		"unknown model format":       db + "admin: {listen: \"off\"}\nproviders: [{name: o, base_url: http://x}]\nmodels: [{name: m, provider: o, formats: [gemini]}]",
		"anthropic format on openai": db + "admin: {listen: \"off\"}\nproviders: [{name: o, base_url: http://x}]\nmodels: [{name: m, provider: o, formats: [anthropic]}]",
		"unknown hook format":        db + "admin: {listen: \"off\"}\nnats: {hooks: {pre_request: {formats: [grpc]}}}",
		"bad trusted proxy":          db + "server: {trusted_proxies: [10.0.0.0/33]}",
		"trusted proxy hostname":     db + "server: {trusted_proxies: [ingress.local]}",
		"bad client ip header":       db + "server: {client_ip_header: \"X Forwarded\"}",
		"metrics listen":             db + "metrics: {listen: nope}",
		"dev master key":             db + "admin: {master_key: dev-master-key-0000}",
		"dev bootstrap password":     db + "admin: {bootstrap: {email: a@b.c, password: dev-admin-password}}",
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

func TestProductionSettings(t *testing.T) {
	cfg, err := Parse([]byte(`database: {url: "postgres://x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ClientIPHeader != "X-Forwarded-For" || len(cfg.Server.TrustedProxies) != 0 || cfg.Metrics.Enabled() || cfg.DevMode {
		t.Fatalf("defaults: %+v %+v %v", cfg.Server, cfg.Metrics, cfg.DevMode)
	}
	cfg, err = Parse([]byte(`
database: {url: "postgres://x"}
server: {trusted_proxies: [10.0.0.0/8, 192.0.2.7], client_ip_header: CF-Connecting-IP}
metrics: {listen: ":9090"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ClientIPHeader != "CF-Connecting-IP" || len(cfg.Server.TrustedProxies) != 2 || !cfg.Metrics.Enabled() {
		t.Fatalf("parsed: %+v %+v", cfg.Server, cfg.Metrics)
	}
	// dev_mode allows the known dev secrets.
	if _, err := Parse([]byte(`
dev_mode: true
database: {url: "postgres://x"}
admin: {master_key: dev-master-key-0000, bootstrap: {email: a@b.c, password: dev-admin-password}}
`)); err != nil {
		t.Fatalf("dev_mode: %v", err)
	}
}

func TestCatalogSettingsAndKeyRefs(t *testing.T) {
	t.Setenv("AIF_TEST_KEY", "sk-from-env")
	cfg, err := Parse([]byte(`
database: {url: "postgres://x"}
providers:
  - {name: a, base_url: http://a, api_key: "${AIF_TEST_KEY}"}
  - {name: b, base_url: http://b, api_key: "env:B_KEY"}
  - {name: c, base_url: http://c, api_key: literal}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalog.Mode != CatalogDatabase || cfg.Catalog.WatchInterval != 5*time.Second {
		t.Fatalf("catalog defaults: %+v", cfg.Catalog)
	}
	acct := func(i int) Account { return cfg.Providers[i].Accounts[0] }
	if a := acct(0); a.Name != DefaultAccount || a.KeyRef != "env:AIF_TEST_KEY" || a.APIKey != "sk-from-env" {
		t.Errorf("from ${}: %+v", a)
	}
	if a := acct(1); a.KeyRef != "env:B_KEY" || a.APIKey != "" {
		t.Errorf("written reference: %+v", a)
	}
	if a := acct(2); a.KeyRef != "" || a.APIKey != "literal" {
		t.Errorf("literal: %+v", a)
	}
	if p := cfg.Providers[0]; p.APIKey != "" || p.KeyRef != "" || p.Policy != PolicyPriority || p.Sticky != StickyNone || p.Cooldown != 30*time.Second {
		t.Errorf("provider defaults: %+v", p)
	}
}
