package metrics

import (
	"net/http"
	"runtime"
	"strconv"
	"time"

	"ai-foundation/gateway/internal/store"
)

// DurationBuckets are the request latency buckets in seconds (spec-0003 section 4.6).
var DurationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

// Checks are read at scrape time. Any of them may be nil.
type Checks struct {
	NATS     func() error
	Database func() error
	Dropped  func() int64 // usage records dropped by the recorder
	// Catalog is the revision this node serves and when that snapshot was loaded.
	Catalog func() (revision int64, loadedAt time.Time)
}

// Gateway is the gateway's metric set (spec-0003 section 4.4). A nil *Gateway is a no-op,
// so callers never check whether metrics are on.
type Gateway struct {
	reg       *Registry
	responses *Vec
	requests  *Vec
	duration  *Vec
	tokens    *Vec
	cost      *Vec
	hooks     *Vec
	sinkErrs  *Vec
	reloads   *Vec
}

// New builds the metric set. The endpoint is served only when metrics.listen is on.
func New(version string, c Checks) *Gateway {
	r := NewRegistry()
	g := &Gateway{reg: r}
	info := r.Gauge("aif_build_info", "Build information; always 1.", "version")
	info.Set(1, version)
	g.responses = r.Counter("aif_http_responses_total", "HTTP responses of the public listeners, metered or not.", "listener", "code")
	g.requests = r.Counter("aif_requests_total", "Metered requests (one per usage record).", "format", "endpoint", "model", "status")
	g.duration = r.Histogram("aif_request_duration_seconds", "Metered request latency, streams until the last byte.", DurationBuckets, "format", "model")
	g.tokens = r.Counter("aif_tokens_total", "Tokens by kind; prompt includes cached and cache_write (OpenAI meaning).", "format", "model", "kind")
	g.cost = r.Counter("aif_cost_usd_total", "Cost in USD from the configured prices.", "format", "model")
	g.hooks = r.Counter("aif_hook_decisions_total", "Pre-request hook decisions.", "format", "decision")
	g.sinkErrs = r.Counter("aif_usage_sink_errors_total", "Failed usage batch writes.", "sink")
	r.CounterFunc("aif_usage_dropped_total", "Usage records dropped (buffer full or shutting down).", func() float64 {
		if c.Dropped == nil {
			return 0
		}
		return float64(c.Dropped())
	})
	g.reloads = r.Counter("aif_catalog_reloads_total", "Catalog reloads that found a new revision, by result (ok, error).", "result")
	r.GaugeFunc("aif_catalog_revision", "Catalog revision this node serves.", func() float64 {
		if c.Catalog == nil {
			return 0
		}
		rev, _ := c.Catalog()
		return float64(rev)
	})
	r.GaugeFunc("aif_catalog_last_reload_seconds", "Unix time when the served catalog was loaded.", func() float64 {
		if c.Catalog == nil {
			return 0
		}
		_, at := c.Catalog()
		return float64(at.Unix())
	})
	r.GaugeFunc("aif_nats_up", "1 when the NATS connection is up.", up(c.NATS))
	r.GaugeFunc("aif_database_up", "1 when the database answers a ping.", up(c.Database))
	r.GaugeFunc("go_goroutines", "Number of goroutines.", func() float64 { return float64(runtime.NumGoroutine()) })
	r.GaugeFunc("go_memstats_heap_alloc_bytes", "Heap bytes allocated and in use.", func() float64 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc)
	})
	start := float64(time.Now().Unix())
	r.GaugeFunc("process_start_time_seconds", "Start time of the process, Unix seconds.", func() float64 { return start })
	return g
}

func up(check func() error) func() float64 {
	return func() float64 {
		if check == nil || check() == nil {
			return 1
		}
		return 0
	}
}

// Handler serves GET /metrics.
func (g *Gateway) Handler() http.Handler { return g.reg.Handler() }

// Registry is the underlying registry (tests, extra families).
func (g *Gateway) Registry() *Registry { return g.reg }

// Usage counts one usage record. It implements metering.Observer.
func (g *Gateway) Usage(rec store.UsageRecord) {
	if g == nil {
		return
	}
	f, m := rec.Format, rec.Model
	g.requests.Inc(f, rec.Endpoint, m, strconv.Itoa(rec.StatusCode))
	g.duration.Observe(float64(rec.LatencyMS)/1000, f, m)
	for kind, n := range map[string]int64{"prompt": rec.PromptTokens, "completion": rec.CompletionTokens,
		"cached": rec.CachedTokens, "cache_write": rec.CacheWriteTokens} {
		if n > 0 {
			g.tokens.Add(float64(n), f, m, kind)
		}
	}
	if rec.CostUSD > 0 {
		g.cost.Add(rec.CostUSD, f, m)
	}
}

// SinkError counts one failed batch write. It implements metering.Observer.
func (g *Gateway) SinkError(sink string) {
	if g == nil {
		return
	}
	g.sinkErrs.Inc(sink)
}

// CatalogReload counts one catalog reload: ok or error. It implements catalog.Observer.
func (g *Gateway) CatalogReload(result string) {
	if g == nil {
		return
	}
	g.reloads.Inc(result)
}

// HookDecision counts one pre-request hook result: allow, deny, modify or unavailable.
func (g *Gateway) HookDecision(format, decision string) {
	if g == nil {
		return
	}
	g.hooks.Inc(format, decision)
}

// Wrap counts the status code of every response of h under the listener name.
// The wrapped writer keeps http.Flusher, so streams still flush per event.
func (g *Gateway) Wrap(listener string, h http.Handler) http.Handler {
	if g == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		code := sw.code
		if code == 0 {
			code = http.StatusOK // nothing written: net/http sends 200
		}
		g.responses.Inc(listener, strconv.Itoa(code))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 && code >= 200 { // 1xx are informational; the final code comes later
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (deadlines, hijack).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
