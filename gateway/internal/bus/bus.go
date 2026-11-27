// Package bus connects the gateway to NATS, its required message bus.
//
// Everything an extension needs goes through NATS, so an extension in any language
// only needs a NATS client:
//
//	<usage.subject_prefix>.<tenant>.<app>   one JSON UsageRecord per request (JetStream stream)
//	<control.subject_prefix>.key_revoked      {"type","id","node","at"}; every node drops the key
//	<control.subject_prefix>.catalog_changed  {"type","revision","node","at"}; every node reloads the catalog
//	<audit.subject_prefix>.breakglass         {"type","method","user_id","email","ip","node","at"}; the local account or master key was used
//	<hooks.pre_request.subject>             request/reply; see package hook
//	KV bucket <state.bucket>                provider account state, shared by all nodes; see AccountSync
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/id"
)

type Bus struct {
	NodeID string

	nc  *nats.Conn
	js  jetstream.JetStream // nil when usage.jetstream is false
	cfg config.NATS
	log *slog.Logger

	closed      chan struct{}
	mu          sync.Mutex
	onReconnect []func()
}

// Connect dials NATS and, when JetStream is on, makes sure the usage stream exists.
// It fails fast: the gateway does not start without NATS. After that, the client
// reconnects forever on its own.
func Connect(ctx context.Context, cfg config.NATS, log *slog.Logger) (*Bus, error) {
	b := &Bus{NodeID: id.New("node_"), cfg: cfg, log: log, closed: make(chan struct{})}
	opts := []nats.Option{
		nats.Name("aif " + b.NodeID),
		nats.Timeout(cfg.ConnectTimeout),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil { // nil = our own Close/Drain
				log.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", redact(nc.ConnectedUrl()))
			b.mu.Lock()
			fns := append([]func(){}, b.onReconnect...)
			b.mu.Unlock()
			for _, fn := range fns {
				fn()
			}
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subj := ""
			if sub != nil {
				subj = sub.Subject
			}
			log.Error("nats async error", "subject", subj, "err", err)
		}),
		nats.ClosedHandler(func(*nats.Conn) { close(b.closed) }),
	}
	if cfg.Creds != "" {
		opts = append(opts, nats.UserCredentials(cfg.Creds))
	}
	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS at %s: %w (NATS is required; for local dev run `make services-up`)", redact(cfg.URL), err)
	}
	b.nc = nc

	if cfg.Usage.UseJetStream() {
		js, err := jetstream.New(nc)
		if err == nil {
			err = b.ensureStream(ctx, js)
		}
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("nats jetstream: %w (start nats-server with --jetstream, or set nats.usage.jetstream: false)", err)
		}
		b.js = js
	}
	log.Info("nats connected", "url", redact(nc.ConnectedUrl()), "node", b.NodeID,
		"jetstream", b.js != nil, "usage_subject", cfg.Usage.SubjectPrefix+".<tenant>.<app>")
	return b, nil
}

func (b *Bus) ensureStream(ctx context.Context, js jetstream.JetStream) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	subjects := b.cfg.Usage.SubjectPrefix + ".>"
	st, err := js.Stream(ctx, b.cfg.Usage.Stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		_, err = js.CreateStream(ctx, jetstream.StreamConfig{
			Name:        b.cfg.Usage.Stream,
			Description: "ai-foundation usage records",
			Subjects:    []string{subjects},
			Storage:     jetstream.FileStorage,
			MaxAge:      b.cfg.Usage.MaxAge,
			Duplicates:  2 * time.Minute, // dedup window for Nats-Msg-Id = request ID
		})
		if err == nil {
			b.log.Info("created jetstream stream", "stream", b.cfg.Usage.Stream, "subjects", subjects)
		}
		return err
	}
	if err != nil {
		return err
	}
	// The stream exists (another node or an operator made it). Leave its settings alone.
	for _, s := range st.CachedInfo().Config.Subjects {
		if s == subjects || s == ">" {
			return nil
		}
	}
	b.log.Warn("jetstream stream does not list the usage subjects; publishes may fail",
		"stream", b.cfg.Usage.Stream, "want", subjects, "has", st.CachedInfo().Config.Subjects)
	return nil
}

// Conn is the raw connection, for tests and future extension points.
func (b *Bus) Conn() *nats.Conn { return b.nc }

// Status is nil while connected.
func (b *Bus) Status() error {
	if b.nc.IsConnected() {
		return nil
	}
	return fmt.Errorf("nats %s", b.nc.Status())
}

// OnReconnect runs fn after each reconnect (events may have been missed while away).
func (b *Bus) OnReconnect(fn func()) {
	b.mu.Lock()
	b.onReconnect = append(b.onReconnect, fn)
	b.mu.Unlock()
}

// Close drains subscriptions and pending publishes, then closes the connection.
func (b *Bus) Close(ctx context.Context) error {
	if err := b.nc.Drain(); err != nil {
		b.nc.Close()
		return err
	}
	select {
	case <-b.closed:
		return nil
	case <-ctx.Done():
		b.nc.Close()
		return ctx.Err()
	}
}

// ---- control events ----

const EventKeyRevoked = "key_revoked"

type ControlEvent struct {
	Type string    `json:"type"`
	ID   string    `json:"id"`
	Node string    `json:"node"`
	At   time.Time `json:"at"`
}

// BreakGlass is the payload of <audit.subject_prefix>.breakglass (spec-0001 section 6.1):
// someone used the local account's password or the master key.
type BreakGlass struct {
	Type   string    `json:"type"`   // always "breakglass"
	Method string    `json:"method"` // "password" or "master_key"
	UserID string    `json:"user_id,omitempty"`
	Email  string    `json:"email,omitempty"`
	IP     string    `json:"ip"`
	Node   string    `json:"node"`
	At     time.Time `json:"at"`
}

// PublishBreakGlass sends a break-glass event. Core pub/sub: a listener that is down
// misses it, and the warning log line stays the record.
func (b *Bus) PublishBreakGlass(ev BreakGlass) error {
	ev.Type, ev.Node, ev.At = "breakglass", b.NodeID, time.Now().UTC()
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return b.nc.Publish(b.cfg.Audit.SubjectPrefix+".breakglass", data)
}

func (b *Bus) controlSubject(event string) string { return b.cfg.Control.SubjectPrefix + "." + event }

// PublishKeyRevoked tells every node to drop key id from its auth cache.
func (b *Bus) PublishKeyRevoked(ctx context.Context, keyID string) error {
	data, _ := json.Marshal(ControlEvent{Type: EventKeyRevoked, ID: keyID, Node: b.NodeID, At: time.Now().UTC()})
	if err := b.nc.Publish(b.controlSubject(EventKeyRevoked), data); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return b.nc.FlushWithContext(ctx)
}

const EventCatalogChanged = "catalog_changed"

// CatalogEvent says the catalog in the database reached Revision (spec-0006 section 5.3).
type CatalogEvent struct {
	Type     string    `json:"type"`
	Revision int64     `json:"revision"`
	Node     string    `json:"node"`
	At       time.Time `json:"at"`
}

// PublishCatalogChanged tells every node that the catalog is at revision rev.
func (b *Bus) PublishCatalogChanged(ctx context.Context, rev int64) error {
	data, _ := json.Marshal(CatalogEvent{Type: EventCatalogChanged, Revision: rev, Node: b.NodeID, At: time.Now().UTC()})
	if err := b.nc.Publish(b.controlSubject(EventCatalogChanged), data); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return b.nc.FlushWithContext(ctx)
}

// OnCatalogChanged calls fn for every catalog_changed event, from any node (this one included).
// fn runs on the NATS goroutine, so it must not block.
func (b *Bus) OnCatalogChanged(fn func(rev int64)) error {
	_, err := b.nc.Subscribe(b.controlSubject(EventCatalogChanged), func(m *nats.Msg) {
		var ev CatalogEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil || ev.Revision <= 0 {
			b.log.Warn("bad control event", "subject", m.Subject, "err", err)
			return
		}
		fn(ev.Revision)
	})
	if err != nil {
		return err
	}
	return b.nc.Flush()
}

// OnKeyRevoked calls fn for every key_revoked event, from any node (this one included).
func (b *Bus) OnKeyRevoked(fn func(keyID string)) error {
	_, err := b.nc.Subscribe(b.controlSubject(EventKeyRevoked), func(m *nats.Msg) {
		var ev ControlEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil || ev.ID == "" {
			b.log.Warn("bad control event", "subject", m.Subject, "err", err)
			return
		}
		fn(ev.ID)
	})
	if err != nil {
		return err
	}
	return b.nc.Flush() // the subscription is live on the server before we serve traffic
}

func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	return u.Redacted()
}
