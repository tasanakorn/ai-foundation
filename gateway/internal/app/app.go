// Package app wires the gateway parts together. cmd/aif and the end-to-end tests
// both use it, so tests run the same wiring as production.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/auth"
	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/clientip"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/gateway"
	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/metrics"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

// Version is the build version shown in metrics; cmd/aif sets it.
var Version = "dev"

type App struct {
	Catalog  *catalog.Holder  // the catalog snapshot this node serves (spec-0006)
	Catalogs *catalog.Manager // keeps Catalog up to date
	Keys     *keys.Service
	Auth     *auth.Service
	OIDC     *auth.OIDC // nil without admin.oidc providers
	Recorder *metering.Recorder
	Bus      *bus.Bus
	Metrics  *metrics.Gateway // served on metrics.listen when that is on

	Public    http.Handler // OpenAI API (/v1, /openai/v1) and Anthropic API (/anthropic)
	Anthropic http.Handler // Anthropic API at the root, for server.anthropic.listen
	Admin     http.Handler // nil when admin.listen is off

	stop context.CancelFunc // ends the background work started by New
}

// New builds one gateway node. The caller owns st and nb and closes them after Close.
// It also makes the local admin account match admin.bootstrap (D42).
func New(ctx context.Context, cfg *config.Config, st store.Store, nb *bus.Bus, log *slog.Logger) (*App, error) {
	audit := func(method, userID, email, ip string) {
		if err := nb.PublishBreakGlass(bus.BreakGlass{Method: method, UserID: userID, Email: email, IP: ip}); err != nil {
			log.Warn("breakglass event not published", "err", err)
		}
	}
	a := &App{Keys: keys.NewService(st), Bus: nb, Auth: auth.NewService(st, auth.Options{
		SessionTTL: cfg.Admin.SessionTTL, LocalEmail: cfg.Admin.Bootstrap.Email, PasswordLogin: cfg.Admin.PasswordLoginOn(),
		Audit: audit, Log: log,
	})}

	// Key revocation reaches every node through NATS.
	if err := nb.OnKeyRevoked(a.Keys.Forget); err != nil {
		return nil, fmt.Errorf("subscribe to control events: %w", err)
	}
	nb.OnReconnect(a.Keys.ForgetAll) // events may have been missed while disconnected
	a.Keys.OnRevoke(func(ctx context.Context, id string) {
		if err := nb.PublishKeyRevoked(ctx, id); err != nil {
			log.Warn("key_revoked event not published; other nodes drop the key within 30s", "key", id, "err", err)
		}
	})

	dbCheck := func() error {
		if p, ok := st.(interface{ Ping(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if p.Ping(ctx) != nil {
				return errors.New("database unreachable") // public endpoint: no connection details
			}
		}
		return nil
	}
	a.Metrics = metrics.New(Version, metrics.Checks{NATS: nb.Status, Database: dbCheck,
		Dropped: func() int64 { return a.Recorder.Dropped() },
		Catalog: func() (int64, time.Time) {
			s := a.Catalog.Snapshot()
			return s.Revision, s.LoadedAt
		},
		Accounts: func() []metrics.AccountState { return accountStates(a.Catalog.Router()) }})

	// The catalog: seed or sync, then load (spec-0006 section 6).
	file, err := catalog.FromFile(cfg)
	if err != nil {
		return nil, err
	}
	kr, err := catalog.ParseKeyring(cfg.Catalog.SecretKeys)
	if err != nil {
		return nil, fmt.Errorf("catalog.secret_keys: %w", err)
	}
	// Account cooldowns are shared by all nodes through a NATS KV bucket (spec-0004 D40).
	sync, err := nb.AccountSync(ctx)
	if err != nil {
		log.Warn("provider account state is per node: no shared bucket", "err", err)
		sync = nil
	}
	env := catalog.Env{Client: provider.NewHTTPClient(), Keyring: kr, Observer: a.Metrics, Log: log}
	if sync != nil {
		env.Sync = sync
	}
	cm, err := catalog.Start(ctx, catalog.Options{Env: env, Store: st, Log: log,
		Mode: cfg.Catalog.Mode, File: file, Path: cfg.Path, WatchInterval: cfg.Catalog.WatchInterval,
		PollInterval: cfg.Catalog.PollInterval, Publish: nb.PublishCatalogChanged, Observer: a.Metrics})
	if err != nil {
		return nil, err
	}
	a.Catalog, a.Catalogs = cm.Holder(), cm
	if err := nb.OnCatalogChanged(func(rev int64) { cm.Changed(rev, "event") }); err != nil {
		return nil, fmt.Errorf("subscribe to control events: %w", err)
	}
	nb.OnReconnect(func() { cm.Changed(0, "reconnect") })
	cm.Changed(0, "start") // a change between the first load and the subscription

	runCtx, stop := context.WithCancel(context.Background())
	a.stop = stop
	if sync != nil {
		if err := sync.Watch(runCtx, func(ev provider.AccountEvent) { applyAccountState(a.Catalog.Router(), ev) }); err != nil {
			stop()
			return nil, fmt.Errorf("watch account state: %w", err)
		}
		nb.OnReconnect(func() { // our rests may not have reached the bucket while away
			for _, ev := range restingAccounts(a.Catalog.Router()) {
				sync.Publish(ev)
			}
		})
	}

	a.Recorder = metering.NewRecorder(cfg.Metering, log, a.Metrics, metering.StoreSink{Store: st}, nb.UsageSink())

	var preHook hook.Hook
	if h := cfg.NATS.Hooks.PreRequest; h.Enabled() {
		preHook = hook.Guard(nb.PreRequestHook(h.Subject), hook.Policy{Timeout: h.Timeout, FailOpen: h.FailOpen, Models: h.Models, Formats: h.Formats}, log)
		log.Info("pre-request hook on", "subject", h.Subject, "timeout", h.Timeout, "fail_open", h.FailOpen, "formats", h.Formats)
	}

	ready := func() error {
		if err := nb.Status(); err != nil {
			return err
		}
		return dbCheck()
	}
	gw := gateway.New(gateway.Options{
		Router: a.Catalog, Keys: a.Keys, Recorder: a.Recorder, Hook: preHook, Ready: ready, Log: log,
		MaxBodyBytes: cfg.Server.MaxBodyBytes, UpstreamTimeout: cfg.Server.UpstreamTimeout,
		AnthropicPrefix: cfg.Server.Anthropic.Prefix(), Metrics: a.Metrics,
	})
	a.Public, a.Anthropic = a.Metrics.Wrap("public", gw.Routes()), a.Metrics.Wrap("anthropic", gw.AnthropicRoutes())
	if cfg.Admin.Enabled() {
		if err := a.bootstrapAdmin(ctx, cfg.Admin, log); err != nil {
			stop()
			return nil, err
		}
		ips, err := clientip.New(cfg.Server.TrustedProxies, cfg.Server.ClientIPHeader)
		if err != nil {
			stop()
			return nil, err
		}
		a.OIDC = auth.NewOIDC(ctx, a.Auth, st, cfg.Admin, &http.Client{Timeout: 15 * time.Second}, log)
		a.Admin = admin.New(admin.Options{Keys: a.Keys, Auth: a.Auth, OIDC: a.OIDC, OIDCTTL: cfg.Admin.OIDC.SessionTTL, Store: st, Router: a.Catalog, Public: a.Public,
			MasterKey: cfg.Admin.MasterKey, CookieSecure: cfg.Admin.CookieSecure, UIDir: cfg.Admin.UIDir,
			ClientIP: ips, NodeID: nb.NodeID, Log: log}).Routes()
	}
	go cm.Run(runCtx)
	return a, nil
}

// applyAccountState gives one account's state from another node to its pool.
func applyAccountState(rt *router.Router, ev provider.AccountEvent) {
	if p, ok := rt.Provider(ev.Provider); ok {
		if pool, ok := provider.AsPool(p); ok {
			pool.Apply(ev)
		}
	}
}

// restingAccounts lists every account that rests now, on this node.
func restingAccounts(rt *router.Router) []provider.AccountEvent {
	var out []provider.AccountEvent
	for _, name := range rt.Providers() {
		p, _ := rt.Provider(name)
		if pool, ok := provider.AsPool(p); ok {
			out = append(out, pool.Resting()...)
		}
	}
	return out
}

// accountStates lists every provider account of the current snapshot and whether it rests.
func accountStates(rt *router.Router) []metrics.AccountState {
	var out []metrics.AccountState
	for _, name := range rt.Providers() {
		p, _ := rt.Provider(name)
		pool, ok := provider.AsPool(p)
		if !ok {
			continue
		}
		for _, a := range pool.State().Accounts {
			out = append(out, metrics.AccountState{Provider: name, Account: a.Name, Cooldown: a.State == "cooldown"})
		}
	}
	return out
}

// Close stops the background work and flushes buffered usage to every sink.
// Call it after the HTTP servers stop.
func (a *App) Close(ctx context.Context) error {
	a.stop()
	return a.Recorder.Close(ctx)
}

func (a *App) bootstrapAdmin(ctx context.Context, c config.Admin, log *slog.Logger) error {
	created, err := a.Auth.SyncLocal(ctx, c.Bootstrap)
	if err != nil {
		return fmt.Errorf("admin.bootstrap: %w", err)
	}
	if created {
		log.Info("created the local admin account from admin.bootstrap", "email", c.Bootstrap.Email)
	}
	switch {
	case c.Bootstrap.Email != "" && len(c.OIDC.Providers) > 0:
		log.Info("admin.oidc is set, so password sign-in is off: the local account cannot sign in until admin.oidc is removed")
	case c.Bootstrap.Email != "" && !c.PasswordLoginOn():
		log.Info("admin.password_login is off: the local account cannot sign in with its password")
	}
	if (c.Bootstrap.Email == "" || !c.PasswordLoginOn()) && len(c.OIDC.Providers) == 0 && c.MasterKey == "" {
		log.Warn("no way to sign in to the admin API: set admin.bootstrap, admin.oidc or admin.master_key")
	}
	return nil
}
