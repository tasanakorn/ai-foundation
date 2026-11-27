package bus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"ai-foundation/gateway/internal/provider"
)

// stateTTL is how long a bucket entry lives. Rests are at most an hour (spec-0004
// section 5.3), so a day keeps the bucket small without losing a live rest.
const stateTTL = 24 * time.Hour

// AccountSync shares provider account state between nodes through a JetStream KV
// bucket (spec-0004 section 4.4, D40). It implements provider.StateSync.
type AccountSync struct {
	b   *Bus
	kv  jetstream.KeyValue
	out chan provider.AccountEvent
}

// AccountSync opens the shared account state bucket, and creates it when missing.
// It fails when JetStream is not available; the caller then keeps per-node state.
func (b *Bus) AccountSync(ctx context.Context) (*AccountSync, error) {
	js, err := jetstream.New(b.nc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bucket := b.cfg.State.Bucket
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, History: 1, TTL: stateTTL,
			Description: "ai-foundation provider account state (spec-0004 section 4.4)"})
		if errors.Is(err, jetstream.ErrBucketExists) { // another node made it first
			kv, err = js.KeyValue(ctx, bucket)
		}
		if err == nil {
			b.log.Info("created jetstream kv bucket", "bucket", bucket)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("kv bucket %s: %w", bucket, err)
	}
	s := &AccountSync{b: b, kv: kv, out: make(chan provider.AccountEvent, 256)}
	go s.send()
	return s, nil
}

// Publish queues ev for the bucket. It never blocks: when the queue is full the event
// is dropped, and the other nodes find the rest themselves, as without sharing.
func (s *AccountSync) Publish(ev provider.AccountEvent) {
	ev.Node = s.b.NodeID
	select {
	case s.out <- ev:
	default:
		s.b.log.Warn("account state not shared: queue full", "provider", ev.Provider, "account", ev.Account)
	}
}

// send writes queued events one at a time, so the bucket keeps them in order.
func (s *AccountSync) send() {
	for {
		select {
		case ev := <-s.out:
			data, _ := json.Marshal(ev)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := s.kv.Put(ctx, StateKey(ev.Provider, ev.Account), data)
			cancel()
			if err != nil {
				s.b.log.Warn("account state not shared", "provider", ev.Provider, "account", ev.Account, "err", err)
			}
		case <-s.b.closed:
			return
		}
	}
}

// Watch calls apply for every entry in the bucket (the current ones first) and every
// later change made by another node, until ctx ends. apply runs on one goroutine.
func (s *AccountSync) Watch(ctx context.Context, apply func(provider.AccountEvent)) error {
	w, err := s.kv.WatchAll(ctx)
	if err != nil {
		return err
	}
	go func() {
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.Updates():
				if !ok {
					return
				}
				if e == nil || e.Operation() != jetstream.KeyValuePut { // nil: the current entries are done
					continue
				}
				var ev provider.AccountEvent
				if err := json.Unmarshal(e.Value(), &ev); err != nil || ev.Provider == "" || ev.Account == "" {
					s.b.log.Warn("bad account state entry", "key", e.Key(), "err", err)
					continue
				}
				if ev.Node != s.b.NodeID {
					apply(ev)
				}
			}
		}
	}()
	return nil
}

var plainKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// StateKey is the bucket key of one account: <provider>.<account>. A provider name
// with other characters is written as b64_<base64url>. Account names are always plain.
func StateKey(providerName, account string) string {
	if !plainKey.MatchString(providerName) {
		providerName = "b64_" + base64.RawURLEncoding.EncodeToString([]byte(providerName))
	}
	return providerName + "." + account
}
