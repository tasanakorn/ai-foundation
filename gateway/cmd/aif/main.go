// Command aif runs the ai-foundation LLM gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // credit time zones work in a minimal image without zoneinfo files

	"ai-foundation/gateway/internal/app"
	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store/postgres"
)

var version = "dev"

const usage = `Usage:
  aif [serve] [-config aif.yaml]          run the gateway
  aif admin create-user -email E [-name N] [-role admin|viewer]
                                          add an account that signs in through OIDC (no password)
  aif admin list-users
  aif admin hash-password                 print an argon2id hash for AIF_ADMIN_PASSWORD_HASH
  aif catalog export [-out FILE]          write the database catalog as YAML (key references kept)
  aif catalog import -file FILE [-if-revision N]
                                          replace the catalog (database mode), in one transaction
  aif catalog set-key -provider P [-account A]
                                          store an encrypted provider key (needs catalog.secret_keys)
  aif catalog history [-limit N]          list catalog revisions
  aif credit add -provider P [-account A] -usd N [-note T] [-at DATE]
                                          record a top-up (or a negative correction) of a prepaid account
  aif credit list [-provider P [-account A]]
                                          list the prepaid ledger
  aif version

hash-password reads the password, and set-key the provider key, from the terminal
(hidden) or from stdin (first line).
The config file is -config, else $AIF_CONFIG, else aif.yaml.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	log := newLogger()
	var err error
	switch cmd {
	case "version":
		fmt.Println(version)
		return
	case "serve":
		fs := flag.NewFlagSet("aif serve", flag.ExitOnError)
		cfgPath := fs.String("config", envOr("AIF_CONFIG", "aif.yaml"), "path to config file")
		_ = fs.Parse(args)
		err = run(*cfgPath, log)
	case "admin":
		err = adminCommand(args)
	case "catalog":
		err = catalogCommand(args)
	case "credit":
		err = creditCommand(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func openStore(ctx context.Context, cfg *config.Config) (*postgres.Store, error) {
	st, err := postgres.Open(ctx, cfg.Database.URL, postgres.Options{MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns})
	if err != nil {
		return nil, fmt.Errorf("open database (is PostgreSQL running? for local dev: make services-up): %w", err)
	}
	return st, nil
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	nb, err := bus.Connect(ctx, cfg.NATS, log) // required; fails fast when NATS is down
	if err != nil {
		return err
	}
	app.Version = version
	if cfg.DevMode {
		log.Warn("dev_mode is on: the public dev secrets are allowed. Never use dev_mode in production")
	}
	a, err := app.New(ctx, cfg, st, nb, log)
	if err != nil {
		nb.Close(context.Background())
		return err
	}

	servers := []*http.Server{{Addr: cfg.Server.Listen, Handler: a.Public, ReadHeaderTimeout: 10 * time.Second}}
	if an := cfg.Server.Anthropic; an.ListenEnabled() {
		servers = append(servers, &http.Server{Addr: an.Listen, Handler: a.Anthropic, ReadHeaderTimeout: 10 * time.Second})
	}
	if cfg.Metrics.Enabled() {
		if !config.IsLoopback(cfg.Metrics.Listen) {
			log.Info("metrics endpoint is reachable from the network; do not route it through a public ingress", "addr", cfg.Metrics.Listen)
		}
		servers = append(servers, &http.Server{Addr: cfg.Metrics.Listen, Handler: a.Metrics.Handler(), ReadHeaderTimeout: 10 * time.Second})
	}
	if len(cfg.Server.TrustedProxies) > 0 {
		log.Info("client IP from trusted proxies", "header", cfg.Server.ClientIPHeader, "trusted_proxies", cfg.Server.TrustedProxies)
	}
	if a.Admin != nil {
		if !config.IsLoopback(cfg.Admin.Listen) {
			log.Warn("admin API is reachable from the network; use TLS in front and strong passwords", "addr", cfg.Admin.Listen)
		}
		servers = append(servers, &http.Server{Addr: cfg.Admin.Listen, Handler: a.Admin, ReadHeaderTimeout: 10 * time.Second})
	}

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("listening", "addr", s.Addr)
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP) // reload the catalog (spec-0006 section 5.4)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			log.Info("SIGHUP: reloading the catalog")
			a.Catalogs.Trigger()
		}
	}()
	log.Info("aif started", "version", version, "node", nb.NodeID, "providers", a.Catalog.Router().Providers(), "models", len(a.Catalog.Router().Models()))

	select {
	case <-ctx.Done():
	case err = <-errCh:
	}

	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutCtx)
	}
	if cerr := a.Close(shutCtx); cerr != nil {
		log.Warn("usage flush incomplete", "err", cerr)
	}
	if cerr := nb.Close(shutCtx); cerr != nil {
		log.Warn("nats close incomplete", "err", cerr)
	}
	return err
}

func newLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if os.Getenv("AIF_LOG_FORMAT") == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
