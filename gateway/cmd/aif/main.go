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

	"ai-foundation/gateway/internal/app"
	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/config"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nb, err := bus.Connect(ctx, cfg.NATS, log) // required; fails fast when NATS is down
	if err != nil {
		return err
	}
	a, err := app.New(cfg, st, nb, log)
	if err != nil {
		nb.Close(context.Background())
		return err
	}

	servers := []*http.Server{{Addr: cfg.Server.Listen, Handler: a.Public, ReadHeaderTimeout: 10 * time.Second}}
	if a.Admin != nil {
		if !config.IsLoopback(cfg.Admin.Listen) {
			log.Warn("admin API is reachable from the network; use a strong master key and TLS", "addr", cfg.Admin.Listen)
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
	log.Info("aif started", "version", version, "node", nb.NodeID, "providers", a.Router.Providers(), "models", len(a.Router.Models()))

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
