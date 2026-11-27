package catalog

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// The export uses the config file's field names, so it can be imported again or
// pasted into aif.yaml for file mode.
type exportFile struct {
	Passthrough bool             `yaml:"passthrough"`
	Providers   []exportProvider `yaml:"providers"`
	Models      []exportModel    `yaml:"models"`
}

type exportProvider struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	BaseURL      string            `yaml:"base_url,omitempty"`
	APIKey       string            `yaml:"api_key,omitempty"`
	Headers      map[string]string `yaml:"headers,omitempty"`
	WorkspaceID  string            `yaml:"workspace_id,omitempty"`
	OpenAIFormat string            `yaml:"openai_format,omitempty"`
}

type exportModel struct {
	Name     string       `yaml:"name"`
	Provider string       `yaml:"provider"`
	Upstream string       `yaml:"upstream,omitempty"`
	Price    *exportPrice `yaml:"price,omitempty"`
	Formats  []string     `yaml:"formats,omitempty,flow"`
}

type exportPrice struct {
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CachedInput float64 `yaml:"cached_input,omitempty"`
	CacheWrite  float64 `yaml:"cache_write,omitempty"`
}

// Export writes the catalog as YAML in the config file format. Key references stay
// references; encrypted keys are left out (they only work with this database's
// secret keys), and a comment lists them.
func Export(c *store.Catalog) ([]byte, error) {
	out := exportFile{Passthrough: c.Passthrough, Providers: []exportProvider{}, Models: []exportModel{}}
	var encrypted []string
	for _, p := range c.Providers {
		if len(p.Accounts) != 1 {
			return nil, fmt.Errorf("provider %q: export of %d accounts needs spec-0004", p.Name, len(p.Accounts))
		}
		a := p.Accounts[0]
		if a.KeyCipher != nil {
			encrypted = append(encrypted, p.Name)
		}
		out.Providers = append(out.Providers, exportProvider{Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, APIKey: a.KeyRef,
			Headers: p.Headers, WorkspaceID: p.WorkspaceID, OpenAIFormat: p.OpenAIFormat})
	}
	for _, m := range c.Models {
		if len(m.Targets) != 1 {
			return nil, fmt.Errorf("model %q: export of %d targets needs spec-0005", m.Name, len(m.Targets))
		}
		t := m.Targets[0]
		em := exportModel{Name: m.Name, Provider: t.Provider, Upstream: t.Upstream, Formats: m.Formats}
		if t.Price != nil {
			em.Price = &exportPrice{Input: t.Price.Input, Output: t.Price.Output, CachedInput: t.Price.CachedInput, CacheWrite: t.Price.CacheWrite}
		}
		out.Models = append(out.Models, em)
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# aif catalog export: revision %d, changed %s by %s\n", c.Revision, c.UpdatedAt.Format(time.RFC3339), orDash(c.UpdatedBy))
	if len(encrypted) > 0 {
		sort.Strings(encrypted)
		fmt.Fprintf(&buf, "# Encrypted keys are not exported (providers: %v). Import keeps them for accounts with the same name.\n", encrypted)
	}
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// ParseImport reads a catalog file for import (config file format; other sections ignored).
func ParseImport(raw []byte) (*store.Catalog, error) {
	cf, err := config.ParseCatalog(raw)
	if err != nil {
		return nil, &InvalidError{err}
	}
	return FromCatalogFile(cf)
}

// KeepEncryptedKeys copies encrypted keys from cur into next for accounts (same
// provider and account name) that next gives no key, so export then import keeps
// them. It returns the providers whose key it kept.
func KeepEncryptedKeys(cur, next *store.Catalog) []string {
	old := map[string]store.CatalogAccount{}
	for _, p := range cur.Providers {
		for _, a := range p.Accounts {
			if a.KeyCipher != nil {
				old[p.Name+"/"+a.Name] = a
			}
		}
	}
	var kept []string
	for i := range next.Providers {
		p := &next.Providers[i]
		for j := range p.Accounts {
			a := &p.Accounts[j]
			if o, ok := old[p.Name+"/"+a.Name]; ok && a.KeyRef == "" && a.KeyCipher == nil {
				a.KeyCipher, a.KeyID, a.KeyHint = o.KeyCipher, o.KeyID, o.KeyHint
				kept = append(kept, p.Name)
			}
		}
	}
	return kept
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
