package catalog_test

import (
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/provider"
)

const targetsYAML = `
database: { url: "postgres://unused" }
providers:
  - { name: a, base_url: http://a/v1 }
  - { name: b, base_url: http://b/v1 }
  - { name: claude, type: anthropic, openai_format: compat }
models:
  - name: coder
    policy: round_robin
    max_attempts: 2
    targets:
      - { provider: a, upstream: coder-a, weight: 3, price: { input: 1, output: 2 } }
      - { provider: b, upstream: coder-b, weight: 1 }
  - name: chat
    policy: round_robin
    sticky: session
    targets:
      - { provider: a, upstream: chat-a }
      - { provider: claude, upstream: chat-c }
`

func TestTargetSelection(t *testing.T) {
	c, err := catalog.FromFile(parse(t, targetsYAML))
	if err != nil {
		t.Fatal(err)
	}
	s, err := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	route, _ := s.Router.Resolve("coder")
	got := map[string]int{}
	for range 1000 {
		got[route.Plan("openai", provider.Identity{})[0].Upstream]++
	}
	if got["coder-a"] != 750 || got["coder-b"] != 250 {
		t.Fatalf("3:1 targets over 1000 requests: %v", got)
	}

	// Sticky by session: one conversation stays on one target; the Anthropic format
	// only ever plans the Anthropic-type target.
	chat, _ := s.Router.Resolve("chat")
	id := provider.Identity{KeyID: "k", Session: "conv-1"}
	first := chat.Plan("openai", id)[0].Upstream
	for range 20 {
		if chat.Plan("openai", id)[0].Upstream != first {
			t.Fatal("sticky session moved")
		}
	}
	if p := chat.Plan("anthropic", id); len(p) != 1 || p[0].Upstream != "chat-c" {
		t.Fatalf("anthropic plan: %+v", p)
	}

	// A reload keeps the round-robin position of the alias.
	route.Plan("openai", provider.Identity{}) // position: one pick into the next cycle
	next, _ := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()})
	next.InheritState(s)
	r2, _ := next.Router.Resolve("coder")
	fresh, _ := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()})
	r3, _ := fresh.Router.Resolve("coder")
	var inherited, restarted []string
	for range 4 {
		inherited = append(inherited, r2.Plan("openai", provider.Identity{})[0].Upstream)
		restarted = append(restarted, r3.Plan("openai", provider.Identity{})[0].Upstream)
	}
	if strings.Join(inherited, ",") == strings.Join(restarted, ",") {
		t.Fatalf("position not carried over: %v", inherited)
	}
}

func TestExportImportTargets(t *testing.T) {
	src, err := catalog.FromFile(parse(t, targetsYAML))
	if err != nil {
		t.Fatal(err)
	}
	out, err := catalog.Export(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"targets:", "upstream: coder-a", "weight: 3", "sticky: session", "max_attempts: 2"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export misses %q:\n%s", want, out)
		}
	}
	back, err := catalog.ParseImport(out)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if !catalog.SameContent(src, back) {
		t.Fatalf("round trip changed the catalog:\n%s", out)
	}
}
