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
	"syscall"
	"time"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/gateway"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store/sqlite"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println(version)
			return
		case "serve":
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("aif serve", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("AIF_CONFIG", "aif.yaml"), "path to config file")
	_ = fs.Parse(args)

	log := newLogger()
	if err := run(*cfgPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, log *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := sqlite.Open(cfg.Database.DSN)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	providers, err := provider.Build(cfg.Providers, provider.NewHTTPClient())
	if err != nil {
		return err
	}
	rt := router.New(cfg.Models, providers, cfg.Passthrough)
	ks := keys.NewService(st)
	rec := metering.NewRecorder(cfg.Metering, log, metering.StoreSink{Store: st})

	gw := gateway.New(gateway.Options{
		Router: rt, Keys: ks, Recorder: rec, Log: log,
		MaxBodyBytes: cfg.Server.MaxBodyBytes, UpstreamTimeout: cfg.Server.UpstreamTimeout,
	})
	servers := []*http.Server{{Addr: cfg.Server.Listen, Handler: gw.Routes(), ReadHeaderTimeout: 10 * time.Second}}
	if cfg.Admin.Enabled() {
		ad := admin.New(admin.Options{Keys: ks, Store: st, Router: rt, MasterKey: cfg.Admin.MasterKey, UIDir: cfg.Admin.UIDir, Log: log})
		servers = append(servers, &http.Server{Addr: cfg.Admin.Listen, Handler: ad.Routes(), ReadHeaderTimeout: 10 * time.Second})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("listening", "addr", s.Addr)
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	log.Info("aif started", "version", version, "providers", rt.Providers(), "models", len(rt.Models()))

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
	if cerr := rec.Close(shutCtx); cerr != nil {
		log.Warn("usage flush incomplete", "err", cerr)
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
