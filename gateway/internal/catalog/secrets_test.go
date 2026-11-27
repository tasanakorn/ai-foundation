package catalog_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/pgtest"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
)

func newSecret(t *testing.T) string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func keyring(t *testing.T, keys ...string) *catalog.Keyring {
	t.Helper()
	kr, err := catalog.ParseKeyring(keys)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

const secretValue = "sk-ant-api03-very-secret-value-WXYZ"

func TestKeyringRotation(t *testing.T) {
	if _, err := catalog.ParseKeyring([]string{"c2hvcnQ="}); err == nil {
		t.Fatal("short key accepted")
	}
	oldKey, newKey := newSecret(t), newSecret(t)
	old := keyring(t, oldKey)
	a := store.CatalogAccount{Name: "default", KeyRef: "env:WAS_A_REFERENCE"}
	if err := old.SetKey(&a, secretValue); err != nil {
		t.Fatal(err)
	}
	if a.KeyRef != "" || a.KeyID == "" || a.KeyHint != "WXYZ" || strings.Contains(string(a.KeyCipher), secretValue) {
		t.Fatalf("sealed account: ref %q id %q hint %q", a.KeyRef, a.KeyID, a.KeyHint)
	}

	srv, seen := keyEcho(t)
	cat := func(acc store.CatalogAccount) *store.Catalog {
		return &store.Catalog{
			Providers: []store.CatalogProvider{{Name: "p", Type: "openai", BaseURL: srv.URL, Accounts: []store.CatalogAccount{acc}}},
			Models:    []store.CatalogModel{{Name: "m", Targets: []store.CatalogTarget{{Provider: "p", Upstream: "m"}}}},
		}
	}
	use := func(kr *catalog.Keyring, acc store.CatalogAccount) error {
		s, err := catalog.Build(cat(acc), provider.NewHTTPClient(), kr)
		if err != nil {
			return err
		}
		r, _ := s.Router.Resolve("m")
		resp, err := r.Provider.Do(context.Background(), "/chat/completions", []byte(`{}`))
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	// Rotation: the new key goes first; values sealed with the old key still open.
	rotated := keyring(t, newKey, oldKey)
	if err := use(rotated, a); err != nil || *seen != "Bearer "+secretValue {
		t.Fatalf("old value with the rotated keyring: %v, upstream saw %q", err, *seen)
	}
	b := store.CatalogAccount{Name: "default"}
	rotated.SetKey(&b, secretValue)
	if b.KeyID == a.KeyID {
		t.Fatal("new writes did not use the first key")
	}

	// Errors name the key ID, never the value.
	for name, tc := range map[string]struct {
		kr  *catalog.Keyring
		acc store.CatalogAccount
	}{
		"no keyring":  {nil, a},
		"key removed": {keyring(t, newKey), a},
		"tampered value": {old, func() store.CatalogAccount {
			c := a
			c.KeyCipher = append([]byte(nil), a.KeyCipher...)
			c.KeyCipher[20] ^= 1
			return c
		}()},
	} {
		err := use(tc.kr, tc.acc)
		if err == nil || strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), "WXYZ") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// keyEcho is an upstream that records the Authorization header it gets.
func keyEcho(t *testing.T) (*httptest.Server, *string) {
	seen := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestEncryptedKeyForms(t *testing.T) {
	kr := keyring(t, newSecret(t))
	for name, edit := range map[string]func(a *store.CatalogAccount){
		"reference and cipher": func(a *store.CatalogAccount) { kr.SetKey(a, secretValue); a.KeyRef = "env:X" },
		"cipher without id":    func(a *store.CatalogAccount) { kr.SetKey(a, secretValue); a.KeyID = "" },
		"id without cipher":    func(a *store.CatalogAccount) { a.KeyID = "k1234" },
	} {
		c := valid()
		edit(&c.Providers[0].Accounts[0])
		if err := catalog.Validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Encrypted keys survive the database and never appear in JSON (what an API would return).
func TestEncryptedKeyStoredNotShown(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "k")
	st := openStore(t, pgtest.URL(t))
	kr := keyring(t, newSecret(t))
	ctx := context.Background()
	c := valid()
	if err := kr.SetKey(&c.Providers[0].Accounts[0], secretValue); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Replace(ctx, st, 0, "admin", "", c); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Load(ctx, st, provider.NewHTTPClient(), kr); err != nil {
		t.Fatalf("load with an encrypted key: %v", err)
	}
	got, _ := st.GetCatalog(ctx)
	acc := got.Providers[0].Accounts[0]
	if len(acc.KeyCipher) == 0 || acc.KeyHint != "WXYZ" {
		t.Fatalf("stored account: %+v", acc)
	}
	out, _ := json.Marshal(got)
	if strings.Contains(string(out), secretValue) || strings.Contains(string(out), base64.StdEncoding.EncodeToString(acc.KeyCipher)) {
		t.Fatalf("JSON shows the key: %s", out)
	}
	if !catalog.SameContent(got, got) || catalog.SameContent(got, c) == false {
		t.Fatal("SameContent with ciphers")
	}
	other := valid()
	kr.SetKey(&other.Providers[0].Accounts[0], secretValue) // new nonce: another ciphertext
	if catalog.SameContent(got, other) {
		t.Fatal("different ciphertexts compared equal")
	}
}
