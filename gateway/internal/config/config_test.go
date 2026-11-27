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
	if cfg.Providers[0].BaseURL != openRouterBaseURL || cfg.Models[0].Targets[0].Upstream != "m" || cfg.Server.Listen != ":8080" ||
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

// oidc is an admin section with a good public_url and the given providers.
func oidc(ps string) string {
	return "admin: {public_url: https://admin.example.com, oidc: {providers: [" + ps + "]}}"
}

func TestOIDCConfig(t *testing.T) {
	const db = "database: {url: \"postgres://x\"}\n"
	c, err := Parse([]byte(db + `admin:
  public_url: https://admin.example.com/
  bootstrap: {email: a@b.c, password_hash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"}
  oidc:
    auto_redirect: sso
    providers:
      - {name: sso, issuer: "https://idp.example.com/", client_id: c, allowed_groups: [aif-admins]}
      - {name: local-idp, title: Local, issuer: "http://localhost:8443", client_id: c}
`))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Admin
	p := a.OIDC.Providers[0]
	if a.PublicURL != "https://admin.example.com" || a.PasswordLoginOn() || a.OIDC.AutoRedirect != "sso" || a.OIDC.SessionTTL != a.SessionTTL ||
		p.Title != "sso" || p.GroupsClaim != "groups" || len(p.Scopes) != 3 || p.Issuer != "https://idp.example.com/" ||
		a.OIDC.Providers[1].Title != "Local" || c.NATS.Audit.SubjectPrefix != "aif.audit.v1" {
		t.Fatalf("defaults: %+v %+v", a, p)
	}
}

// A hash from the environment survives ${VAR} expansion and YAML (it has $ and commas).
func TestPasswordHashFromEnv(t *testing.T) {
	const hash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo+/"
	t.Setenv("AIF_TEST_HASH", hash)
	c, err := Parse([]byte("database: {url: \"postgres://x\"}\nadmin:\n  bootstrap:\n    email: a@b.c\n    password_hash: ${AIF_TEST_HASH:-}\n"))
	if err != nil || c.Admin.Bootstrap.PasswordHash != hash {
		t.Fatalf("%v %q", err, c.Admin.Bootstrap.PasswordHash)
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
		"password and hash":          db + "admin: {bootstrap: {email: a@b.c, password: long-enough-password, password_hash: \"$argon2id$v=19$m=1,t=1,p=1$a$b\"}}",
		"hash not argon2id":          db + "admin: {bootstrap: {email: a@b.c, password_hash: \"$2a$10$abc\"}}",
		"oidc without public_url":    db + "admin: {oidc: {providers: [{name: sso, issuer: https://idp.example.com, client_id: c}]}}",
		"oidc http public_url":       db + "admin: {public_url: http://admin.example.com, oidc: {providers: [{name: sso, issuer: https://idp.example.com, client_id: c}]}}",
		"oidc bad name":              db + oidc("{name: Big SSO, issuer: https://idp.example.com, client_id: c}"),
		"oidc no client_id":          db + oidc("{name: sso, issuer: https://idp.example.com}"),
		"auto_redirect unknown":      db + "admin: {public_url: https://a.example.com, oidc: {auto_redirect: other, providers: [{name: sso, issuer: https://i.example.com, client_id: c}]}}",
		"auto_redirect no provider":  db + "admin: {oidc: {auto_redirect: sso}}",
		"oidc http issuer":           db + oidc("{name: sso, issuer: http://idp.example.com, client_id: c}"),
		"oidc name twice":            db + oidc("{name: sso, issuer: https://i.example.com, client_id: c}, {name: sso, issuer: https://j.example.com, client_id: c}"),
		"oidc session_ttl":           db + "admin: {public_url: https://a.example.com, oidc: {session_ttl: 1s}}",
		"audit subject":              db + "nats: {audit: {subject_prefix: \"aif.>\"}}",
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

func TestModelTargets(t *testing.T) {
	const base = "database: {url: \"postgres://x\"}\nadmin: {listen: \"off\"}\nproviders: [{name: a, type: anthropic, openai_format: compat}, {name: o, base_url: http://o}]\n"
	cfg, err := Parse([]byte(base + `models:
  - name: mixed
    policy: round_robin
    targets:
      - {provider: a, upstream: claude-x, weight: 3}
      - {provider: o, upstream: vendor/x}
  - {name: short, provider: a, formats: [anthropic]}
`))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Models[0]
	if len(m.Formats) != 2 || m.Formats[0] != FormatOpenAI || m.Targets[1].Formats[0] != FormatOpenAI || len(m.Targets[1].Formats) != 1 {
		t.Fatalf("mixed: formats %v, targets %+v", m.Formats, m.Targets)
	}
	if s := cfg.Models[1]; s.Provider != "" || len(s.Targets) != 1 || s.Targets[0].Upstream != "short" || s.Targets[0].Formats[0] != FormatAnthropic {
		t.Fatalf("short form: %+v", s)
	}
	for name, models := range map[string]string{
		"both forms":        "[{name: m, provider: o, targets: [{provider: o}]}]",
		"unknown target":    "[{name: m, targets: [{provider: nope}]}]",
		"target twice":      "[{name: m, targets: [{provider: o, upstream: x}, {provider: o, upstream: x}]}]",
		"weight priority":   "[{name: m, targets: [{provider: o, weight: 2}]}]",
		"anthropic on o":    "[{name: m, targets: [{provider: o, formats: [anthropic]}]}]",
		"format not served": "[{name: m, formats: [anthropic], targets: [{provider: o}]}]",
		"sticky priority":   "[{name: m, sticky: key, targets: [{provider: o}]}]",
		"all drained":       "[{name: m, policy: round_robin, targets: [{provider: o, weight: 0}]}]",
	} {
		if _, err := Parse([]byte(base + "models: " + models + "\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Credit per account (task-0010): one type per account; the provider's credit is
// each account's default; cycle_day and timezone have defaults; bad values are refused.
func TestAccountCredit(t *testing.T) {
	const base = "database: {url: \"postgres://x\"}\nadmin: {listen: \"off\"}\n"
	cfg, err := Parse([]byte(base + `providers:
  - name: p
    base_url: http://x
    credit: {type: monthly, usd: 100, cycle_day: 5, timezone: Asia/Bangkok}
    accounts:
      - {name: a}
      - {name: b, credit: {type: prepaid}}
      - {name: c, credit: {type: payg, usd: 300}}
  - {name: q, base_url: http://y, credit: {type: monthly, usd: 20}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers[0]
	if p.Credit != nil || *p.Accounts[0].Credit != (Credit{CreditMonthly, 100, 5, "Asia/Bangkok"}) ||
		*p.Accounts[1].Credit != (Credit{Type: CreditPrepaid}) || *p.Accounts[2].Credit != (Credit{CreditPayg, 300, 1, "UTC"}) {
		t.Fatalf("p: provider %+v, a %+v, b %+v, c %+v", p.Credit, p.Accounts[0].Credit, p.Accounts[1].Credit, p.Accounts[2].Credit)
	}
	if q := cfg.Providers[1]; *q.Accounts[0].Credit != (Credit{CreditMonthly, 20, 1, "UTC"}) {
		t.Fatalf("q (api_key form): %+v", q.Accounts[0].Credit)
	}
	for name, credit := range map[string]string{
		"no type":          "{usd: 5}",
		"unknown type":     "{type: weekly, usd: 5}",
		"zero":             "{type: monthly, usd: 0}",
		"below 0":          "{type: payg, usd: -5}",
		"day below 1":      "{type: monthly, usd: 5, cycle_day: -1}",
		"day 32":           "{type: monthly, usd: 5, cycle_day: 32}",
		"bad timezone":     "{type: monthly, usd: 5, timezone: Mars/Base}",
		"local timezone":   "{type: monthly, usd: 5, timezone: Local}",
		"prepaid with usd": "{type: prepaid, usd: 50}",
		"prepaid with day": "{type: prepaid, cycle_day: 3}",
	} {
		if _, err := Parse([]byte(base + "providers: [{name: p, base_url: http://x, credit: " + credit + "}]\n")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCreditCycle(t *testing.T) {
	bkk, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string, loc *time.Location) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		cycleDay        int
		tz              string
		loc             *time.Location
		now, start, end string
	}{
		{1, "UTC", time.UTC, "2026-11-27 10:00", "2026-11-01 00:00", "2026-12-01 00:00"},
		{5, "", time.UTC, "2026-11-27 10:00", "2026-11-05 00:00", "2026-12-05 00:00"},
		{5, "UTC", time.UTC, "2026-11-04 23:59", "2026-10-05 00:00", "2026-11-05 00:00"},
		{5, "UTC", time.UTC, "2026-11-05 00:00", "2026-11-05 00:00", "2026-12-05 00:00"},
		{28, "UTC", time.UTC, "2027-01-03 00:00", "2026-12-28 00:00", "2027-01-28 00:00"},
		{31, "UTC", time.UTC, "2026-02-15 00:00", "2026-01-31 00:00", "2026-02-28 00:00"}, // February has no 31st
		{31, "UTC", time.UTC, "2026-02-28 00:00", "2026-02-28 00:00", "2026-03-31 00:00"},
		{30, "UTC", time.UTC, "2028-02-29 12:00", "2028-02-29 00:00", "2028-03-30 00:00"}, // leap year
		// 2026-11-04 20:00 UTC is already the 5th in Bangkok (UTC+7).
		{5, "Asia/Bangkok", bkk, "2026-11-05 03:00", "2026-11-05 00:00", "2026-12-05 00:00"},
		// New York changes to summer time on 2026-03-08: the cycle is 23 hours shorter than 31 days.
		{1, "America/New_York", ny, "2026-03-15 12:00", "2026-03-01 00:00", "2026-04-01 00:00"},
	} {
		now := at(c.now, c.loc)
		start, end := Credit{Type: CreditMonthly, USD: 1, CycleDay: c.cycleDay, Timezone: c.tz}.Cycle(now.UTC())
		if !start.Equal(at(c.start, c.loc)) || !end.Equal(at(c.end, c.loc)) {
			t.Errorf("day %d %s at %s: got %s to %s, want %s to %s", c.cycleDay, c.tz, c.now, start, end, c.start, c.end)
		}
	}
}
