// Package metering turns upstream usage into UsageRecords and writes them off the hot path.
package metering

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/store"
)

// Usage is the OpenAI "usage" object, plus OpenRouter's optional "cost" (USD).
type Usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	Cost *float64 `json:"cost"`
}

// Apply copies upstream usage into rec and sets the cost.
// Provider-reported cost wins; otherwise the configured price is used.
func Apply(rec *store.UsageRecord, u *Usage, price *config.Price) {
	rec.PromptTokens = u.PromptTokens
	rec.CompletionTokens = u.CompletionTokens
	rec.TotalTokens = u.TotalTokens
	if rec.TotalTokens == 0 {
		rec.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.PromptTokensDetails != nil {
		rec.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		rec.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.Cost != nil {
		rec.CostUSD = *u.Cost
	} else {
		rec.CostUSD = Cost(price, rec.PromptTokens, rec.CachedTokens, rec.CompletionTokens)
	}
}

// Estimate fills tokens with a rough 4-bytes-per-token guess when the upstream sent no usage.
func Estimate(rec *store.UsageRecord, promptBytes, completionBytes int, price *config.Price) {
	rec.PromptTokens = int64(promptBytes+3) / 4
	rec.CompletionTokens = int64(completionBytes+3) / 4
	rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	rec.Estimated = true
	rec.CostUSD = Cost(price, rec.PromptTokens, 0, rec.CompletionTokens)
}

// Cost in USD. Prices are per 1M tokens.
func Cost(p *config.Price, prompt, cached, completion int64) float64 {
	if p == nil {
		return 0
	}
	cachedPrice := p.CachedInput
	if cachedPrice == 0 {
		cachedPrice = p.Input
	}
	return (float64(prompt-cached)*p.Input + float64(cached)*cachedPrice + float64(completion)*p.Output) / 1e6
}

// Sink receives batches of usage records. Store, webhook, queue, ... are all sinks.
type Sink interface {
	Name() string
	Write(ctx context.Context, recs []store.UsageRecord) error
}

// StoreSink writes usage into the gateway database.
type StoreSink struct{ Store store.Store }

func (s StoreSink) Name() string { return "store" }
func (s StoreSink) Write(ctx context.Context, recs []store.UsageRecord) error {
	return s.Store.InsertUsage(ctx, recs)
}

// Recorder buffers records in memory and flushes them to every sink in batches.
// Record never blocks the request; when the buffer is full the record is dropped and counted.
type Recorder struct {
	ch       chan store.UsageRecord
	sinks    []Sink
	batch    int
	interval time.Duration
	log      *slog.Logger
	done     chan struct{}
	dropped  atomic.Int64
	mu       sync.RWMutex
	closed   bool
}

func NewRecorder(cfg config.Metering, log *slog.Logger, sinks ...Sink) *Recorder {
	r := &Recorder{
		ch:       make(chan store.UsageRecord, cfg.BufferSize),
		sinks:    sinks,
		batch:    cfg.BatchSize,
		interval: cfg.FlushInterval,
		log:      log,
		done:     make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *Recorder) Record(rec store.UsageRecord) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.dropped.Add(1)
		return
	}
	select {
	case r.ch <- rec:
	default:
		if n := r.dropped.Add(1); n == 1 || n%1000 == 0 {
			r.log.Warn("usage buffer full, dropping records", "dropped_total", n)
		}
	}
}

func (r *Recorder) Dropped() int64 { return r.dropped.Load() }

// Close stops intake and flushes what is buffered.
func (r *Recorder) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.ch)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	buf := make([]store.UsageRecord, 0, r.batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		for _, s := range r.sinks {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.Write(ctx, buf); err != nil {
				r.log.Error("usage sink write failed", "sink", s.Name(), "records", len(buf), "err", err)
			}
			cancel()
		}
		buf = buf[:0]
	}
	for {
		select {
		case rec, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, rec)
			if len(buf) >= r.batch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}
