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
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/gateway"
	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

type App struct {
	Router   *router.Router
	Keys     *keys.Service
	Auth     *auth.Service
	Recorder *metering.Recorder
	Bus      *bus.Bus

	Public http.Handler // OpenAI-compatible API
	Admin  http.Handler // nil when admin.listen is off
}

// New builds one gateway node. The caller owns st and nb and closes them after Close.
// It also creates the first admin account from admin.bootstrap when none exists.
func New(ctx context.Context, cfg *config.Config, st store.Store, nb *bus.Bus, log *slog.Logger) (*App, error) {
	providers, err := provider.Build(cfg.Providers, provider.NewHTTPClient())
	if err != nil {
		return nil, err
	}
	a := &App{Router: router.New(cfg.Models, providers, cfg.Passthrough), Keys: keys.NewService(st),
		Auth: auth.NewService(st, cfg.Admin.SessionTTL, log), Bus: nb}

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

	a.Recorder = metering.NewRecorder(cfg.Metering, log, metering.StoreSink{Store: st}, nb.UsageSink())

	var preHook hook.Hook
	if h := cfg.NATS.Hooks.PreRequest; h.Enabled() {
		preHook = hook.Guard(nb.PreRequestHook(h.Subject), hook.Policy{Timeout: h.Timeout, FailOpen: h.FailOpen, Models: h.Models}, log)
		log.Info("pre-request hook on", "subject", h.Subject, "timeout", h.Timeout, "fail_open", h.FailOpen)
	}

	ready := func() error {
		if err := nb.Status(); err != nil {
			return err
		}
		if p, ok := st.(interface{ Ping(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if p.Ping(ctx) != nil {
				return errors.New("database unreachable") // public endpoint: no connection details
			}
		}
		return nil
	}
	a.Public = gateway.New(gateway.Options{
		Router: a.Router, Keys: a.Keys, Recorder: a.Recorder, Hook: preHook, Ready: ready, Log: log,
		MaxBodyBytes: cfg.Server.MaxBodyBytes, UpstreamTimeout: cfg.Server.UpstreamTimeout,
	}).Routes()
	if cfg.Admin.Enabled() {
		if err := a.bootstrapAdmin(ctx, cfg.Admin, log); err != nil {
			return nil, err
		}
		a.Admin = admin.New(admin.Options{Keys: a.Keys, Auth: a.Auth, Store: st, Router: a.Router, Public: a.Public,
			MasterKey: cfg.Admin.MasterKey, CookieSecure: cfg.Admin.CookieSecure, UIDir: cfg.Admin.UIDir, Log: log}).Routes()
	}
	return a, nil
}

// Close flushes buffered usage to every sink. Call it after the HTTP servers stop.
func (a *App) Close(ctx context.Context) error { return a.Recorder.Close(ctx) }

func (a *App) bootstrapAdmin(ctx context.Context, c config.Admin, log *slog.Logger) error {
	created, err := a.Auth.Bootstrap(ctx, c.Bootstrap)
	if err != nil {
		return fmt.Errorf("admin.bootstrap: %w", err)
	}
	if created {
		log.Info("created the first admin account from admin.bootstrap; change its password after sign-in", "email", c.Bootstrap.Email)
		return nil
	}
	has, err := a.Auth.HasUsers(ctx)
	if err != nil {
		return err
	}
	if !has && c.MasterKey == "" {
		log.Warn("no admin account and no master key: nobody can sign in. Set admin.bootstrap or run `aif admin create-user`")
	}
	return nil
}
