package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/store"
)

const publishRetries = 3

// UsageSink publishes each usage record to <prefix>.<tenant>.<app>.
// With JetStream, every publish waits for the server ack and carries
// Nats-Msg-Id = request ID, so retries never count a request twice.
type UsageSink struct {
	b       *Bus
	dropped atomic.Int64
}

func (b *Bus) UsageSink() *UsageSink { return &UsageSink{b: b} }

var _ metering.Sink = (*UsageSink)(nil)

func (s *UsageSink) Name() string { return "nats" }

// Dropped counts records that were not published after all retries.
func (s *UsageSink) Dropped() int64 { return s.dropped.Load() }

func (s *UsageSink) Write(ctx context.Context, recs []store.UsageRecord) error {
	ctx, cancel := withDefaultTimeout(ctx, 10*time.Second)
	defer cancel()
	pending := recs
	backoff := 100 * time.Millisecond
	for attempt := 0; ; attempt++ {
		pending = s.publish(ctx, pending)
		if len(pending) == 0 {
			return nil
		}
		if attempt == publishRetries || !sleep(ctx, backoff) {
			s.dropped.Add(int64(len(pending)))
			return fmt.Errorf("%d of %d usage records not published to nats (dropped total %d)",
				len(pending), len(recs), s.dropped.Load())
		}
		backoff *= 4
	}
}

// publish sends recs and returns the ones that failed.
func (s *UsageSink) publish(ctx context.Context, recs []store.UsageRecord) []store.UsageRecord {
	var failed []store.UsageRecord
	if s.b.js == nil {
		for _, r := range recs {
			data, _ := json.Marshal(r)
			if s.b.nc.Publish(UsageSubject(s.b.cfg.Usage.SubjectPrefix, r), data) != nil {
				failed = append(failed, r)
			}
		}
		if s.b.nc.FlushWithContext(ctx) != nil {
			return recs // unknown which made it; core NATS has no dedup, so resend all
		}
		return failed
	}

	type inflight struct {
		rec store.UsageRecord
		ack jetstream.PubAckFuture
	}
	futures := make([]inflight, 0, len(recs))
	for _, r := range recs {
		data, _ := json.Marshal(r)
		msg := &nats.Msg{Subject: UsageSubject(s.b.cfg.Usage.SubjectPrefix, r), Data: data}
		f, err := s.b.js.PublishMsgAsync(msg, jetstream.WithMsgID(r.ID))
		if err != nil {
			failed = append(failed, r)
			continue
		}
		futures = append(futures, inflight{r, f})
	}
	for _, f := range futures {
		select {
		case <-f.ack.Ok():
		case <-f.ack.Err():
			failed = append(failed, f.rec)
		case <-ctx.Done():
			failed = append(failed, f.rec)
		}
	}
	return failed
}

// UsageSubject is <prefix>.<tenant>.<app>. Empty labels become "_".
// Characters outside [A-Za-z0-9_-] are %-escaped, so a label is always one subject token.
func UsageSubject(prefix string, r store.UsageRecord) string {
	return prefix + "." + Token(r.TenantID) + "." + Token(r.AppID)
}

func Token(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// withDefaultTimeout adds a deadline when ctx has none (nats.go needs one for Flush and Request).
func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
