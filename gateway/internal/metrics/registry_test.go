package metrics

import (
	"strconv"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/store"
)

func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTextFormat(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Line one\nline two.", "a", "b")
	c.Inc("1", `q"u\o`+"\n")
	c.Add(2.5, "1", `q"u\o`+"\n")
	c.Add(-1, "1", `q"u\o`+"\n") // ignored: counters only go up
	h := r.Histogram("d_seconds", "Latency.", []float64{1, 0.5}, "m")
	h.Observe(0.2, "x")
	h.Observe(0.7, "x")
	h.Observe(9, "x")
	r.GaugeFunc("g", "A gauge.", func() float64 { return 3 })
	r.Gauge("plain", "No labels.").Set(4)
	want := `# HELP x_total Line one\nline two.
# TYPE x_total counter
x_total{a="1",b="q\"u\\o\n"} 3.5
# HELP d_seconds Latency.
# TYPE d_seconds histogram
d_seconds_bucket{m="x",le="0.5"} 1
d_seconds_bucket{m="x",le="1"} 2
d_seconds_bucket{m="x",le="+Inf"} 3
d_seconds_sum{m="x"} 9.9
d_seconds_count{m="x"} 3
# HELP g A gauge.
# TYPE g gauge
g 3
# HELP plain No labels.
# TYPE plain gauge
plain 4
`
	if got := scrape(t, r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCardinalityCap(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("m_total", "Capped.", "model", "code")
	for i := range MaxSeries + 50 {
		c.Inc("model-"+strconv.Itoa(i), "200")
	}
	c.Inc("model-0", "200") // an existing series still counts normally
	out := scrape(t, r)
	if n := strings.Count(out, "\nm_total{"); n != MaxSeries+1 {
		t.Fatalf("%d series, want %d", n, MaxSeries+1)
	}
	if !strings.Contains(out, `m_total{model="_other",code="_other"} 50`) || !strings.Contains(out, `m_total{model="model-0",code="200"} 2`) {
		t.Fatal("overflow series missing or existing series wrong")
	}
}

func TestDuplicateAndLabelCount(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("a_total", "A.", "x")
	mustPanic(t, func() { r.Counter("a_total", "again") })
	mustPanic(t, func() { c.Inc("1", "2") })
}

func TestNilGatewayIsNoop(t *testing.T) {
	var g *Gateway
	g.Usage(store.UsageRecord{})
	g.SinkError("store")
	g.HookDecision("openai", "allow")
	if h := g.Wrap("public", nil); h != nil {
		t.Fatal("nil gateway must return the handler unchanged")
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	f()
}
