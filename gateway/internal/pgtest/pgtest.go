// Package pgtest gives tests a real PostgreSQL. Test-only.
//
// With AIF_TEST_PG_URL set (e.g. the compose Postgres), tests use that server.
// Otherwise Main starts an embedded PostgreSQL for the test binary: the server binary is
// downloaded once into gateway/.cache/embedded-postgres, so no Docker is needed.
// Every test gets its own empty database, dropped when the test ends.
package pgtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
)

const EnvURL = "AIF_TEST_PG_URL"

const version = embeddedpostgres.V18

var baseURL string

// Main runs the tests of one package with a Postgres server. Use it from TestMain.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if u := os.Getenv(EnvURL); u != "" {
		baseURL = u
		return m.Run()
	}
	stop, err := startEmbedded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgtest: start embedded postgres:", err)
		fmt.Fprintln(os.Stderr, "pgtest: set "+EnvURL+" to use another server (e.g. make services-up, then AIF_TEST_PG_URL=$DATABASE_URL)")
		return 1
	}
	defer stop()
	return m.Run()
}

func startEmbedded() (func(), error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	runtimeDir, err := os.MkdirTemp("", "aif-pgtest-")
	if err != nil {
		return nil, err
	}
	// Binaries are unpacked once into the cache and shared. The lock stops two test
	// binaries (go test runs packages in parallel) from unpacking at the same time.
	unlock, err := lockCache()
	if err != nil {
		return nil, err
	}
	defer unlock()
	var logs bytes.Buffer
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(version).
		BinariesPath(filepath.Join(cacheDir(), "bin-"+string(version))).
		Port(uint32(port)).
		Username("aif").Password("aif").Database("postgres").
		RuntimePath(runtimeDir).
		CachePath(cacheDir()).
		StartTimeout(60 * time.Second).
		StartParameters(map[string]string{"fsync": "off", "synchronous_commit": "off", "full_page_writes": "off", "max_connections": "300"}).
		Logger(&logs))
	if err := pg.Start(); err != nil {
		os.RemoveAll(runtimeDir)
		return nil, fmt.Errorf("%w\n%s", err, logs.String())
	}
	baseURL = fmt.Sprintf("postgres://aif:aif@127.0.0.1:%d/postgres?sslmode=disable", port)
	return func() {
		_ = pg.Stop()
		os.RemoveAll(runtimeDir)
	}, nil
}

// URL creates a fresh database for this test and returns its URL.
// Call it only in tests of a package whose TestMain calls Main.
func URL(t testing.TB) string {
	t.Helper()
	if baseURL == "" {
		t.Fatal("pgtest: no server; call pgtest.Main(m) from TestMain")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "aif_test_" + hex.EncodeToString(b[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, baseURL)
		if err != nil {
			t.Logf("pgtest: drop %s: %v", name, err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("pgtest: drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("pgtest: %s: %v", EnvURL, err)
	}
	u.Path = "/" + name
	return u.String()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func lockCache() (func(), error) {
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(cacheDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// cacheDir is gateway/.cache/embedded-postgres (gitignored), so the download stays in the repo.
func cacheDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", ".cache", "embedded-postgres")
}
