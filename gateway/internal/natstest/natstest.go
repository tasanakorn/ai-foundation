// Package natstest runs an in-process NATS server (with JetStream) for tests.
// Only test files import it, so the server code never lands in the aif binary.
package natstest

import (
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"ai-foundation/gateway/internal/config"
)

// Run starts a server on a random port and returns its client URL.
func Run(t testing.TB) string { return run(t, true) }

// RunWithoutJetStream starts a server without JetStream.
func RunWithoutJetStream(t testing.TB) string { return run(t, false) }

func run(t testing.TB, jetStream bool) string {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: jetStream, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// Config is the default nats config section pointed at url.
func Config(t testing.TB, url string) config.NATS {
	t.Helper()
	cfg, err := config.Parse([]byte("admin: { listen: off }\ndatabase: { url: \"postgres://unused\" }\nnats: { url: " + url + " }\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.NATS
}
