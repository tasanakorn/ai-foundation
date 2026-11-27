// Package catalog turns the routing catalog (providers, accounts, models, targets) into
// an immutable snapshot per node, swapped atomically (spec-0006 sections 4 and 5, D36, D39).
//
// The database holds the catalog (store.Catalog). Build checks it, resolves key
// references and makes the providers and the router. A Holder serves the current
// snapshot to requests through an atomic pointer: no lock on the request path.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

// DefaultAccount is the account name of a provider written with a single api_key.
const DefaultAccount = config.DefaultAccount

// Snapshot is one immutable catalog: never changed after Build.
type Snapshot struct {
	Revision int64          // 0 = the empty catalog of a database never written
	Catalog  *store.Catalog // as stored (keys are references)
	Router   *router.Router
	LoadedAt time.Time
}

// Holder holds the snapshot this node serves. The zero value is not usable; use NewHolder.
type Holder struct {
	cur atomic.Pointer[Snapshot]
}

func NewHolder(s *Snapshot) *Holder {
	h := &Holder{}
	h.cur.Store(s)
	return h
}

// Snapshot returns the current snapshot.
func (h *Holder) Snapshot() *Snapshot { return h.cur.Load() }

// Router returns the current router. It implements router.Source.
func (h *Holder) Router() *router.Router { return h.cur.Load().Router }

// Revision is the revision this node serves.
func (h *Holder) Revision() int64 { return h.cur.Load().Revision }

// Offer swaps in s when its revision is higher than the current one (spec-0006 section 5.3.3).
// It reports whether s is now served. Requests that already hold the old router keep it.
func (h *Holder) Offer(s *Snapshot) bool {
	for {
		old := h.cur.Load()
		if s.Revision <= old.Revision {
			return false
		}
		if h.cur.CompareAndSwap(old, s) {
			return true
		}
	}
}

// FromFile turns the catalog part of the file into the stored form. A provider key
// must be a reference: ${NAME} in the file becomes env:NAME, and env:NAME or file:/path
// stay as written. A key value written in the file is refused, because keys are never
// stored in the database (spec-0006 D38).
func FromFile(cfg *config.Config) (*store.Catalog, error) {
	return FromCatalogFile(&config.CatalogOnly{Passthrough: cfg.Passthrough, Providers: cfg.Providers, Models: cfg.Models})
}

// FromCatalogFile is FromFile for a file that holds only the catalog (import).
func FromCatalogFile(cfg *config.CatalogOnly) (*store.Catalog, error) {
	// Normalize copies: Parse already did, but a hand-built or changed config may not be.
	providers := append([]config.Provider(nil), cfg.Providers...)
	models := append([]config.Model(nil), cfg.Models...)
	config.NormalizeCatalog(providers, models)
	c := &store.Catalog{Passthrough: cfg.Passthrough, Providers: []store.CatalogProvider{}, Models: []store.CatalogModel{}}
	for _, p := range providers {
		headers := copyMap(p.Headers)
		if headers == nil {
			headers = map[string]string{}
		}
		sp := store.CatalogProvider{Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, Headers: headers,
			WorkspaceID: p.WorkspaceID, OpenAIFormat: p.OpenAIFormat, Accounts: []store.CatalogAccount{},
			Policy: p.Policy, Sticky: p.Sticky, MaxAttempts: p.MaxAttempts, Cooldown: p.Cooldown}
		for _, a := range p.Accounts {
			if a.KeyRef == "" && a.APIKey != "" {
				return nil, &InvalidError{fmt.Errorf("provider %q account %q: api_key is a value written in the file; put the key in an environment variable and write ${NAME}, or use env:NAME or file:/path (keys are never stored in the database)", p.Name, a.Name)}
			}
			sp.Accounts = append(sp.Accounts, store.CatalogAccount{Name: a.Name, KeyRef: a.KeyRef, Weight: a.Weight,
				BaseURL: a.BaseURL, Headers: copyMap(a.Headers), WorkspaceID: a.WorkspaceID, Credit: toStoreCredit(a.Credit)})
		}
		c.Providers = append(c.Providers, sp)
	}
	for _, m := range models {
		sm := store.CatalogModel{Name: m.Name, Formats: append([]string{}, m.Formats...), Targets: []store.CatalogTarget{},
			Policy: m.Policy, Sticky: m.Sticky, MaxAttempts: m.MaxAttempts}
		for _, t := range m.Targets {
			st := store.CatalogTarget{Provider: t.Provider, Upstream: t.Upstream, Weight: t.Weight, Formats: append([]string{}, t.Formats...)}
			if t.Price != nil {
				pr := store.Price(*t.Price)
				st.Price = &pr
			}
			sm.Targets = append(sm.Targets, st)
		}
		c.Models = append(c.Models, sm)
	}
	return c, Validate(c)
}

// SameContent reports whether a and b hold the same catalog, ignoring revision and
// who changed it last. Nil and empty lists and maps count as equal.
func SameContent(a, b *store.Catalog) bool {
	ja, _ := json.Marshal(canon(a))
	jb, _ := json.Marshal(canon(b))
	return bytes.Equal(ja, jb) && bytes.Equal(ciphers(a), ciphers(b))
}

// ciphers joins the encrypted keys, which JSON leaves out.
func ciphers(c *store.Catalog) []byte {
	var out []byte
	for _, p := range c.Providers {
		for _, a := range p.Accounts {
			out = append(append(out, a.KeyCipher...), 0)
		}
	}
	return out
}

func canon(c *store.Catalog) store.Catalog {
	out := store.Catalog{Passthrough: c.Passthrough, Providers: []store.CatalogProvider{}, Models: []store.CatalogModel{}}
	for _, p := range c.Providers {
		if p.Headers == nil {
			p.Headers = map[string]string{}
		}
		accounts := []store.CatalogAccount{}
		for _, a := range p.Accounts {
			if len(a.Headers) == 0 {
				a.Headers = nil
			}
			accounts = append(accounts, a)
		}
		p.Accounts = accounts
		out.Providers = append(out.Providers, p)
	}
	for _, m := range c.Models {
		if m.Formats == nil {
			m.Formats = []string{}
		}
		targets := []store.CatalogTarget{}
		for _, t := range m.Targets {
			if t.Formats == nil {
				t.Formats = []string{}
			}
			targets = append(targets, t)
		}
		m.Targets = targets
		out.Models = append(out.Models, m)
	}
	return out
}

// Describe is a short summary for logs and the change history.
func Describe(c *store.Catalog) string {
	return fmt.Sprintf("%d providers, %d models", len(c.Providers), len(c.Models))
}

// Env is what building a snapshot needs besides the catalog.
type Env struct {
	Client   *http.Client
	Keyring  *Keyring           // decrypts encrypted keys; nil = references only
	Observer provider.Observer  // optional: upstream attempt metrics
	Log      *slog.Logger       // optional: failed upstream attempts
	Sync     provider.StateSync // optional: shares account cooldowns with the other nodes
}

// Load reads the catalog from the database and builds a snapshot.
func Load(ctx context.Context, st store.Store, env Env) (*Snapshot, error) {
	c, err := st.GetCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return Build(c, env)
}

// Build checks c, resolves its keys (references, or encrypted values with kr) and makes
// the providers and the router. It fails, and the caller keeps its last good snapshot,
// when c is invalid or a key does not resolve (spec-0006 section 5.5). kr may be nil
// when the catalog has no encrypted keys.
func Build(c *store.Catalog, env Env) (*Snapshot, error) {
	providers, models, err := toConfig(c, env.Keyring, true)
	if err != nil {
		return nil, err
	}
	built, err := provider.Build(providers, provider.BuildOptions{Client: env.Client, Observer: env.Observer, Log: env.Log, Sync: env.Sync})
	if err != nil {
		return nil, err
	}
	return &Snapshot{Revision: c.Revision, Catalog: c, Router: router.New(models, built, c.Passthrough), LoadedAt: time.Now()}, nil
}

// InheritState moves account state (cooldowns, round-robin positions) from prev into s,
// by provider and account name (spec-0006 section 5.6). Call it before s is served.
func (s *Snapshot) InheritState(prev *Snapshot) {
	if prev == nil || prev.Router == nil || s.Router == nil {
		return
	}
	s.Router.InheritState(prev.Router)
	for _, name := range s.Router.Providers() {
		np, _ := s.Router.Provider(name)
		op, ok := prev.Router.Provider(name)
		if !ok {
			continue
		}
		if a, ok := provider.AsPool(np); ok {
			if b, ok := provider.AsPool(op); ok {
				a.Inherit(b)
			}
		}
	}
}

// InvalidError is a catalog that fails validation. Nothing was written or swapped.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return "invalid catalog: " + e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// Validate checks the whole catalog with the same rules as the file, without resolving keys.
func Validate(c *store.Catalog) error {
	_, _, err := toConfig(c, nil, false)
	return err
}

// Update changes the database catalog in one transaction: change edits the current
// catalog, then the whole result is validated; an invalid catalog is never committed
// (D39). ifRevision is the revision the change is based on (store.AnyRevision: none).
func Update(ctx context.Context, st store.Store, ifRevision int64, actor, summary string, change func(*store.Catalog) error) (*store.Catalog, error) {
	return st.UpdateCatalog(ctx, ifRevision, actor, summary, func(c *store.Catalog) error {
		if err := change(c); err != nil {
			return err
		}
		return Validate(c)
	})
}

// Replace writes next as the whole catalog (seed, file sync, import).
func Replace(ctx context.Context, st store.Store, ifRevision int64, actor, summary string, next *store.Catalog) (*store.Catalog, error) {
	return Update(ctx, st, ifRevision, actor, summary, func(c *store.Catalog) error {
		c.Passthrough, c.Providers, c.Models = next.Passthrough, next.Providers, next.Models
		return nil
	})
}

// toConfig converts c to the config types, applies defaults and validates. With resolve,
// keys are read (references) or decrypted with kr; without, keys stay empty (validation only).
func toConfig(c *store.Catalog, kr *Keyring, resolve bool) ([]config.Provider, []config.Model, error) {
	invalid := func(format string, a ...any) error { return &InvalidError{fmt.Errorf(format, a...)} }
	providers := make([]config.Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		if len(p.Accounts) == 0 {
			return nil, nil, invalid("provider %q: needs at least one account", p.Name)
		}
		cp := config.Provider{Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, Headers: copyMap(p.Headers),
			WorkspaceID: p.WorkspaceID, OpenAIFormat: p.OpenAIFormat,
			Policy: p.Policy, Sticky: p.Sticky, MaxAttempts: p.MaxAttempts, Cooldown: p.Cooldown}
		for _, a := range p.Accounts {
			if err := checkAccountKey(a); err != nil {
				return nil, nil, invalid("provider %q account %q: %v", p.Name, a.Name, err)
			}
			ca := config.Account{Name: a.Name, Weight: a.Weight, BaseURL: a.BaseURL, Headers: copyMap(a.Headers), WorkspaceID: a.WorkspaceID}
			if a.Credit != nil {
				ca.Credit = &config.Credit{Type: a.Credit.Type, USD: a.Credit.USD, CycleDay: a.Credit.CycleDay, Timezone: a.Credit.Timezone}
			}
			if resolve {
				key, err := ResolveKey(a.KeyRef)
				if a.KeyCipher != nil {
					key, err = kr.open(a)
				}
				if err != nil {
					return nil, nil, fmt.Errorf("provider %q account %q: %w", p.Name, a.Name, err)
				}
				ca.APIKey = key
			}
			cp.Accounts = append(cp.Accounts, ca)
		}
		providers = append(providers, cp)
	}
	models := make([]config.Model, 0, len(c.Models))
	for _, m := range c.Models {
		if len(m.Targets) == 0 {
			return nil, nil, invalid("model %q: needs at least one target", m.Name)
		}
		cm := config.Model{Name: m.Name, Formats: append([]string(nil), m.Formats...), Policy: m.Policy, Sticky: m.Sticky, MaxAttempts: m.MaxAttempts}
		for _, t := range m.Targets {
			ct := config.Target{Provider: t.Provider, Upstream: t.Upstream, Weight: t.Weight, Formats: append([]string(nil), t.Formats...)}
			if t.Price != nil {
				pr := config.Price(*t.Price)
				ct.Price = &pr
			}
			cm.Targets = append(cm.Targets, ct)
		}
		models = append(models, cm)
	}
	config.NormalizeCatalog(providers, models)
	if err := config.ValidateCatalog(providers, models); err != nil {
		return nil, nil, &InvalidError{err}
	}
	return providers, models, nil
}

func toStoreCredit(c *config.Credit) *store.Credit {
	if c == nil {
		return nil
	}
	return &store.Credit{Type: c.Type, USD: c.USD, CycleDay: c.CycleDay, Timezone: c.Timezone}
}

// checkAccountKey checks that an account holds its key in exactly one allowed form.
func checkAccountKey(a store.CatalogAccount) error {
	if a.KeyCipher == nil {
		if a.KeyID != "" || a.KeyHint != "" {
			return errors.New("key_id or key_hint without an encrypted key")
		}
		return CheckKeyRef(a.KeyRef)
	}
	switch {
	case a.KeyRef != "":
		return errors.New("a key reference and an encrypted key: use one")
	case a.KeyID == "":
		return errors.New("encrypted key without its secret key ID")
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckKeyRef checks the form of a key reference: "" (no key), "env:NAME" or "file:/abs/path".
// A key value itself is refused, so a secret never lands in the database by mistake.
func CheckKeyRef(ref string) error {
	switch {
	case ref == "":
		return nil
	case strings.HasPrefix(ref, "env:"):
		if !envName.MatchString(ref[4:]) {
			return errors.New("key reference env:NAME needs a valid variable name")
		}
		return nil
	case strings.HasPrefix(ref, "file:"):
		if !strings.HasPrefix(ref[5:], "/") {
			return errors.New("key reference file:PATH needs an absolute path")
		}
		return nil
	}
	return errors.New("key must be a reference (env:NAME or file:/path), not a key value")
}

// ResolveKey reads the key a reference points to. A file is read on every call, so a
// rotated Kubernetes Secret takes effect at the next reload (spec-0006 section 4.4.4).
// Errors name the reference, never a key value.
func ResolveKey(ref string) (string, error) {
	if err := CheckKeyRef(ref); err != nil {
		return "", err
	}
	switch {
	case ref == "":
		return "", nil
	case strings.HasPrefix(ref, "env:"):
		v := os.Getenv(ref[4:])
		if v == "" {
			return "", fmt.Errorf("key reference %s: variable is not set", ref)
		}
		return v, nil
	default:
		b, err := os.ReadFile(ref[5:])
		if err != nil {
			return "", fmt.Errorf("key reference %s: %w", ref, errors.Unwrap(err))
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("key reference %s: file is empty", ref)
		}
		return v, nil
	}
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
