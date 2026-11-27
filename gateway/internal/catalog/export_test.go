package catalog_test

import (
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
)

func TestExportImportRoundTrip(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	src, err := catalog.FromFile(parse(t, `
database: { url: "postgres://unused" }
passthrough: true
providers:
  - { name: local, base_url: http://local/v1, headers: { X-Title: demo } }
  - { name: keyed, type: openrouter, api_key: "${AIF_CATALOG_TEST_KEY}" }
  - { name: claude, type: anthropic, openai_format: compat, api_key: file:/run/secrets/claude, workspace_id: wrk }
models:
  - { name: small, provider: local, price: { input: 1, output: 2, cached_input: 0.5 } }
  - { name: big, provider: keyed, upstream: vendor/big }
  - { name: sonnet, provider: claude, formats: [anthropic] }
`))
	if err != nil {
		t.Fatal(err)
	}
	src.Revision = 4
	out, err := catalog.Export(src)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"# aif catalog export: revision 4", "api_key: env:AIF_CATALOG_TEST_KEY", "api_key: file:/run/secrets/claude", "formats: [anthropic]"} {
		if !strings.Contains(text, want) {
			t.Errorf("export misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "sk-test") {
		t.Fatal("export holds a key value")
	}
	back, err := catalog.ParseImport(out)
	if err != nil {
		t.Fatalf("import of the export: %v\n%s", err, text)
	}
	if !catalog.SameContent(src, back) {
		t.Fatalf("round trip changed the catalog:\n%s", text)
	}
}

func TestImportKeepsEncryptedKeys(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	kr := keyring(t, newSecret(t))
	cur, err := catalog.FromFile(parse(t, fileYAML("database", "  - { name: small, provider: local }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := kr.SetKey(&cur.Providers[0].Accounts[0], secretValue); err != nil {
		t.Fatal(err)
	}
	out, err := catalog.Export(cur)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Encrypted keys are not exported (providers: [local])") || strings.Contains(string(out), "WXYZ") {
		t.Fatalf("export of an encrypted key:\n%s", out)
	}
	next, err := catalog.ParseImport(out)
	if err != nil {
		t.Fatal(err)
	}
	if kept := catalog.KeepEncryptedKeys(cur, next); len(kept) != 1 || kept[0] != "local" {
		t.Fatalf("kept %v", kept)
	}
	if !catalog.SameContent(cur, next) {
		t.Fatal("export and import with a kept key changed the catalog")
	}
	// An import that gives the account a reference replaces the encrypted key.
	next, _ = catalog.ParseImport([]byte(strings.Replace(string(out), "name: local\n", "name: local\n    api_key: env:NEW_KEY\n", 1)))
	if kept := catalog.KeepEncryptedKeys(cur, next); len(kept) != 0 || next.Providers[0].Accounts[0].KeyRef != "env:NEW_KEY" {
		t.Fatalf("reference in the import: kept %v, account %+v", kept, next.Providers[0].Accounts[0])
	}
}

func TestImportRefusesKeyValues(t *testing.T) {
	_, err := catalog.ParseImport([]byte("providers:\n  - { name: x, base_url: http://x/v1, api_key: sk-live }\n"))
	if err == nil || !strings.Contains(err.Error(), "never stored in the database") {
		t.Fatalf("got %v", err)
	}
	_, err = catalog.ParseImport([]byte("models:\n  - { name: m, provider: missing }\n"))
	if err == nil {
		t.Fatal("invalid catalog imported")
	}
}
