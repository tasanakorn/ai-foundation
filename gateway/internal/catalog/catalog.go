// Package catalog turns the routing catalog (providers, accounts, models, targets) into
// an immutable snapshot per node, swapped atomically (spec-0006 sections 4 and 5, D36, D39).
//
// The database holds the catalog (store.Catalog). Build checks it, resolves key
// references and makes the providers and the router. A Holder serves the current
// snapshot to requests through an atomic pointer: no lock on the request path.
package catalog

import (
	"context"
	"errors"
	"fmt"
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
const DefaultAccount = "default"

// Snapshot is one immutable catalog: never changed after Build.
type Snapshot struct {
	Revision int64          // 0 = built from the file, not from the database
	Catalog  *store.Catalog // as stored (keys are references); nil when built from the file
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

// FromConfig builds a snapshot from the catalog part of the file (revision 0).
func FromConfig(cfg *config.Config, client *http.Client) (*Snapshot, error) {
	providers, err := provider.Build(cfg.Providers, client)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Router: router.New(cfg.Models, providers, cfg.Passthrough), LoadedAt: time.Now()}, nil
}

// Load reads the catalog from the database and builds a snapshot.
func Load(ctx context.Context, st store.Store, client *http.Client) (*Snapshot, error) {
	c, err := st.GetCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return Build(c, client)
}

// Build checks c, resolves its key references and makes the providers and the router.
// It fails, and the caller keeps its last good snapshot, when c is invalid or a key
// reference does not resolve (spec-0006 section 5.5).
func Build(c *store.Catalog, client *http.Client) (*Snapshot, error) {
	providers, models, err := toConfig(c, true)
	if err != nil {
		return nil, err
	}
	built, err := provider.Build(providers, client)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Revision: c.Revision, Catalog: c, Router: router.New(models, built, c.Passthrough), LoadedAt: time.Now()}, nil
}

// InvalidError is a catalog that fails validation. Nothing was written or swapped.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return "invalid catalog: " + e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// Validate checks the whole catalog with the same rules as the file, without resolving keys.
func Validate(c *store.Catalog) error {
	_, _, err := toConfig(c, false)
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

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// toConfig converts c to the config types, applies defaults and validates. With resolve,
// key references become key values; without, keys stay empty (validation only).
func toConfig(c *store.Catalog, resolve bool) ([]config.Provider, []config.Model, error) {
	invalid := func(format string, a ...any) error { return &InvalidError{fmt.Errorf(format, a...)} }
	providers := make([]config.Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		// Several accounts and their policy arrive with spec-0004 (task-0020).
		if len(p.Accounts) != 1 {
			return nil, nil, invalid("provider %q: has %d accounts; exactly one is supported until spec-0004", p.Name, len(p.Accounts))
		}
		a := p.Accounts[0]
		if !accountName.MatchString(a.Name) {
			return nil, nil, invalid("provider %q: account name %q: use a-z, 0-9 and '-'", p.Name, a.Name)
		}
		if err := CheckKeyRef(a.KeyRef); err != nil {
			return nil, nil, invalid("provider %q account %q: %v", p.Name, a.Name, err)
		}
		cp := config.Provider{Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, Headers: copyMap(p.Headers),
			WorkspaceID: p.WorkspaceID, OpenAIFormat: p.OpenAIFormat}
		if resolve {
			key, err := ResolveKey(a.KeyRef)
			if err != nil {
				return nil, nil, fmt.Errorf("provider %q account %q: %w", p.Name, a.Name, err)
			}
			cp.APIKey = key
		}
		providers = append(providers, cp)
	}
	models := make([]config.Model, 0, len(c.Models))
	for _, m := range c.Models {
		// Several targets per alias arrive with spec-0005 (task-0021).
		if len(m.Targets) != 1 {
			return nil, nil, invalid("model %q: has %d targets; exactly one is supported until spec-0005", m.Name, len(m.Targets))
		}
		t := m.Targets[0]
		cm := config.Model{Name: m.Name, Provider: t.Provider, Upstream: t.Upstream, Formats: append([]string(nil), m.Formats...)}
		if t.Price != nil {
			pr := config.Price(*t.Price)
			cm.Price = &pr
		}
		models = append(models, cm)
	}
	config.NormalizeCatalog(providers, models)
	if err := config.ValidateCatalog(providers, models); err != nil {
		return nil, nil, &InvalidError{err}
	}
	return providers, models, nil
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
