package catalog

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// Actors written to the change history for changes the gateway makes itself.
const (
	ActorSeed = "seed"
	ActorFile = "file"
)

// Options configure a Manager.
type Options struct {
	Store  store.Store
	Client *http.Client
	Log    *slog.Logger
	Mode   string         // config.CatalogDatabase or config.CatalogFile
	File   *store.Catalog // the file's catalog (FromFile)
	// Path is the config file. File mode watches it every WatchInterval; "" = no watch.
	Path          string
	WatchInterval time.Duration
	// PollInterval is how often Run checks the database revision; 0 = no poll.
	PollInterval time.Duration
	// Publish tells the other nodes about a new revision (bus.PublishCatalogChanged). Optional.
	Publish  func(ctx context.Context, rev int64) error
	Observer Observer // optional
	Keyring  *Keyring // decrypts encrypted provider keys; nil = references only
}

// Observer counts reloads (metrics.Gateway).
type Observer interface {
	CatalogReload(result string) // ok or error
}

// Manager keeps this node's snapshot up to date (spec-0006 sections 3, 5 and 6).
type Manager struct {
	st     store.Store
	client *http.Client
	log    *slog.Logger
	mode   string
	path   string
	watch  time.Duration
	poll   time.Duration
	pub    func(ctx context.Context, rev int64) error
	obs    Observer
	kr     *Keyring
	holder *Holder

	reload  sync.Mutex    // one reload at a time
	trigger chan struct{} // SIGHUP
	wake    chan string   // reload from the database: event or reconnect

	// file watch state, used only by the Run goroutine
	lastMod  time.Time
	lastSize int64
	lastSum  [32]byte
	lastErr  string
}

// Start seeds (database mode) or syncs (file mode) the database catalog, then loads the
// first snapshot. It fails when no snapshot can be built: a node never serves without
// a catalog (spec-0006 section 6).
func Start(ctx context.Context, o Options) (*Manager, error) {
	m := &Manager{st: o.Store, client: o.Client, log: o.Log, mode: o.Mode, path: o.Path, watch: o.WatchInterval,
		poll: o.PollInterval, pub: o.Publish, obs: o.Observer, kr: o.Keyring, trigger: make(chan struct{}, 1), wake: make(chan string, 1)}
	switch o.Mode {
	case config.CatalogFile:
		if _, err := m.syncFile(ctx, o.File); err != nil {
			return nil, fmt.Errorf("catalog file mode: %w", err)
		}
	default:
		if err := m.seed(ctx, o.File); err != nil {
			return nil, fmt.Errorf("catalog seed: %w", err)
		}
	}
	snap, err := Load(ctx, m.st, m.client, m.kr)
	if err != nil {
		return nil, err
	}
	m.holder = NewHolder(snap)
	if snap.Catalog.Empty() {
		m.log.Warn("the catalog is empty: no providers and no models", "mode", m.mode)
	}
	m.log.Info("catalog loaded", "mode", m.mode, "revision", snap.Revision, "catalog", Describe(snap.Catalog))
	return m, nil
}

// Holder is the snapshot source for requests.
func (m *Manager) Holder() *Holder { return m.holder }

// seed writes the file's catalog when the database has never had one (revision 0).
// Several nodes may start at once: the revision check lets exactly one of them write.
func (m *Manager) seed(ctx context.Context, file *store.Catalog) error {
	cur, err := m.st.GetCatalog(ctx)
	if err != nil {
		return err
	}
	if cur.Revision > 0 {
		if !file.Empty() && !SameContent(cur, file) {
			m.log.Info("catalog.mode is database: the catalog in the file is ignored, the database has its own", "revision", cur.Revision)
		}
		return nil
	}
	if file.Empty() {
		return nil
	}
	c, err := Replace(ctx, m.st, 0, ActorSeed, "seeded from the file: "+Describe(file), file)
	if errors.Is(err, store.ErrStale) {
		return nil // another node seeded first
	}
	if err != nil {
		return err
	}
	m.log.Info("seeded the database catalog from the file", "revision", c.Revision, "catalog", Describe(c))
	m.publish(ctx, c.Revision)
	return nil
}

// syncFile makes the database catalog equal to the file's. It writes only when the
// content differs, so restarts and every node of a cluster do not add revisions.
func (m *Manager) syncFile(ctx context.Context, want *store.Catalog) (bool, error) {
	for range 5 {
		cur, err := m.st.GetCatalog(ctx)
		if err != nil {
			return false, err
		}
		if SameContent(cur, want) {
			return false, nil
		}
		c, err := Replace(ctx, m.st, cur.Revision, ActorFile, "synced from the file: "+Describe(want), want)
		if errors.Is(err, store.ErrStale) {
			continue // another node wrote first; compare again
		}
		if err != nil {
			return false, err
		}
		m.log.Info("wrote the file's catalog to the database", "revision", c.Revision, "catalog", Describe(c))
		m.publish(ctx, c.Revision)
		return true, nil
	}
	return false, errors.New("the catalog kept changing while syncing; will try again")
}

// publish tells the other nodes. A lost event costs them at most one poll interval.
func (m *Manager) publish(ctx context.Context, rev int64) {
	if m.pub == nil {
		return
	}
	if err := m.pub(ctx, rev); err != nil {
		m.log.Warn("catalog_changed event not published; other nodes reload at their next poll", "revision", rev, "err", err)
	}
}

// Committed is for writers outside the manager (admin API, import): it tells the other
// nodes about revision rev and reloads this node now.
func (m *Manager) Committed(ctx context.Context, rev int64) error {
	m.publish(ctx, rev)
	return m.Reload(ctx, "local")
}

// Changed handles a catalog_changed event (or a reconnect, rev 0). It never blocks:
// Run does the reload.
func (m *Manager) Changed(rev int64, source string) {
	if rev > 0 && rev <= m.holder.Revision() {
		return // already served (often our own event)
	}
	select {
	case m.wake <- source:
	default: // a reload is pending; it reads the latest revision anyway
	}
}

// Reload loads the database catalog when its revision is higher than the one served.
// On failure the node keeps its last good snapshot (spec-0006 section 5.5).
func (m *Manager) Reload(ctx context.Context, source string) error {
	m.reload.Lock()
	defer m.reload.Unlock()
	start := time.Now()
	old := m.holder.Revision()
	rev, err := m.st.CatalogRevision(ctx)
	if err != nil {
		m.log.Error("catalog reload failed; keeping the last good catalog", "source", source, "revision", old, "err", err)
		return err
	}
	if rev <= old {
		return nil
	}
	snap, err := Load(ctx, m.st, m.client, m.kr)
	if err != nil {
		m.observe("error")
		m.log.Error("catalog reload failed; keeping the last good catalog", "source", source, "revision", old, "new_revision", rev, "err", err)
		return err
	}
	if m.holder.Offer(snap) {
		m.observe("ok")
		m.log.Info("catalog reloaded", "source", source, "old_revision", old, "revision", snap.Revision,
			"catalog", Describe(snap.Catalog), "duration_ms", time.Since(start).Milliseconds())
	}
	return nil
}

func (m *Manager) observe(result string) {
	if m.obs != nil {
		m.obs.CatalogReload(result)
	}
}

// Trigger asks Run for a reload now (SIGHUP). File mode reads the file again;
// database mode reloads from the database.
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default: // one is already pending
	}
}

// Run watches for changes until ctx ends: the file in file mode (every WatchInterval),
// the database revision (every PollInterval), events and reconnects (Changed), and
// Trigger in both modes.
func (m *Manager) Run(ctx context.Context) {
	var tick, poll <-chan time.Time
	if m.mode == config.CatalogFile && m.path != "" {
		t := time.NewTicker(m.watch)
		defer t.Stop()
		tick = t.C
	}
	if m.poll > 0 {
		t := time.NewTicker(m.poll)
		defer t.Stop()
		poll = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			m.checkFile(ctx, false)
		case <-poll:
			_ = m.Reload(ctx, "poll")
		case source := <-m.wake:
			_ = m.Reload(ctx, source)
		case <-m.trigger:
			if m.mode == config.CatalogFile && m.path != "" {
				m.checkFile(ctx, true)
			} else {
				_ = m.Reload(ctx, "sighup")
			}
		}
	}
}

// checkFile reads the file when its time or size changed (or force), and applies its
// catalog when the content changed. A file that fails to parse or validate is logged
// and ignored; the last good catalog stays.
func (m *Manager) checkFile(ctx context.Context, force bool) {
	source := "file"
	if force {
		source = "sighup"
	}
	fi, err := os.Stat(m.path)
	if err != nil {
		m.fileError("cannot read the config file", err)
		return
	}
	if !force && fi.ModTime().Equal(m.lastMod) && fi.Size() == m.lastSize {
		return
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		m.fileError("cannot read the config file", err)
		return
	}
	m.lastMod, m.lastSize = fi.ModTime(), fi.Size()
	sum := sha256.Sum256(raw)
	if !force && sum == m.lastSum {
		return
	}
	cfg, err := config.Parse(raw)
	var want *store.Catalog
	if err == nil {
		want, err = FromFile(cfg)
	}
	if err != nil { // retried only when the file changes again
		m.fileError("the config file is invalid and was not applied; keeping the last good catalog", err)
		return
	}
	if _, err := m.syncFile(ctx, want); err != nil {
		m.lastMod = time.Time{} // database problem: try again at the next check
		m.fileError("the config file was not applied; keeping the last good catalog", err)
		return
	}
	m.lastSum, m.lastErr = sum, ""
	_ = m.Reload(ctx, source)
}

// fileError logs a file problem once, not on every check.
func (m *Manager) fileError(msg string, err error) {
	if e := err.Error(); e != m.lastErr {
		m.lastErr = e
		m.log.Error(msg, "path", m.path, "revision", m.holder.Revision(), "err", err)
	}
}
