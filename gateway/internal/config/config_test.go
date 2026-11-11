package config

import (
	"strings"
	"testing"
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
providers:
  - { name: openrouter, type: openrouter }
models:
  - { name: m, provider: openrouter }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].BaseURL != openRouterBaseURL || cfg.Models[0].Upstream != "m" || cfg.Server.Listen != ":8080" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("AIF_MASTER_KEY", "0123456789abcdef0")
	if _, err := Load("../../aif.example.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"master key":       `providers: []`,
		"unknown provider": "admin: {listen: \"off\"}\nmodels: [{name: m, provider: x}]",
		"slash in name":    "admin: {listen: \"off\"}\nproviders: [{name: a/b, base_url: http://x}]",
		"duplicate model":  "admin: {listen: \"off\"}\nproviders: [{name: p, base_url: http://x}]\nmodels: [{name: m, provider: p}, {name: m, provider: p}]",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if testing.Verbose() {
			t.Logf("%s: %v", name, strings.TrimSpace(err.Error()))
		}
	}
}
