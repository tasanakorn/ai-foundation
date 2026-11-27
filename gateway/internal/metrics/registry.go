// Package metrics is a small Prometheus text-format registry (spec-0003 section 4, D22).
// It has no dependencies: counters, gauges and histograms with labels, and a cap on label sets.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// MaxSeries is the most label sets one family keeps. Later sets fold into one
// series whose label values are all OtherValue.
const MaxSeries = 1000

// OtherValue is the label value of the overflow series.
const OtherValue = "_other"

// Registry holds metric families in registration order.
type Registry struct {
	mu       sync.Mutex
	families []*family
}

func NewRegistry() *Registry { return &Registry{} }

type kind string

const (
	kindCounter   kind = "counter"
	kindGauge     kind = "gauge"
	kindHistogram kind = "histogram"
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64      // histogram only, ascending, without +Inf
	fn         func() float64 // gauge or counter read at scrape time

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	value  float64  // counter or gauge
	counts []uint64 // histogram: one per bucket (not cumulative)
	sum    float64
	count  uint64
}

// Vec is a family with labels. The number of values passed must match the labels.
type Vec struct{ f *family }

func (r *Registry) add(f *family) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.families {
		if g.name == f.name {
			panic("metrics: duplicate family " + f.name)
		}
	}
	f.series = map[string]*series{}
	r.families = append(r.families, f)
	return f
}

// Counter adds a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *Vec {
	return &Vec{r.add(&family{name: name, help: help, kind: kindCounter, labels: labels})}
}

// Gauge adds a gauge family that is set, not read at scrape time.
func (r *Registry) Gauge(name, help string, labels ...string) *Vec {
	return &Vec{r.add(&family{name: name, help: help, kind: kindGauge, labels: labels})}
}

// Histogram adds a histogram family with the given ascending upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Vec {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Vec{r.add(&family{name: name, help: help, kind: kindHistogram, labels: labels, buckets: b})}
}

// GaugeFunc adds an unlabeled gauge read at scrape time.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: kindGauge, fn: fn})
}

// CounterFunc adds an unlabeled counter read at scrape time (fn must never go down).
func (r *Registry) CounterFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: kindCounter, fn: fn})
}

// get returns the series for values, creating it (or the overflow series). Caller holds f.mu.
func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	if s, ok := f.series[key]; ok {
		return s
	}
	if len(f.series) >= MaxSeries {
		other := make([]string, len(values))
		for i := range other {
			other[i] = OtherValue
		}
		values, key = other, strings.Join(other, "\xff")
		if s, ok := f.series[key]; ok {
			return s
		}
	}
	s := &series{values: append([]string(nil), values...)}
	if f.kind == kindHistogram {
		s.counts = make([]uint64, len(f.buckets))
	}
	f.series[key] = s
	return s
}

// Add adds delta to a counter (delta must not be negative) or a gauge.
func (v *Vec) Add(delta float64, values ...string) {
	if v.f.kind == kindCounter && (delta < 0 || math.IsNaN(delta)) {
		return
	}
	v.f.mu.Lock()
	v.f.get(values).value += delta
	v.f.mu.Unlock()
}

// Inc adds 1.
func (v *Vec) Inc(values ...string) { v.Add(1, values...) }

// Set sets a gauge.
func (v *Vec) Set(x float64, values ...string) {
	v.f.mu.Lock()
	v.f.get(values).value = x
	v.f.mu.Unlock()
}

// Observe records one histogram sample.
func (v *Vec) Observe(x float64, values ...string) {
	if math.IsNaN(x) {
		return
	}
	v.f.mu.Lock()
	s := v.f.get(values)
	for i, ub := range v.f.buckets {
		if x <= ub {
			s.counts[i]++
			break
		}
	}
	s.sum += x
	s.count++
	v.f.mu.Unlock()
}

// Write writes every family in the Prometheus text exposition format 0.0.4.
func (r *Registry) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	for _, f := range fams {
		f.write(bw)
	}
	return bw.Flush()
}

func (f *family) write(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	if f.fn != nil {
		fmt.Fprintf(w, "%s %s\n", f.name, num(f.fn()))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.series))
	for k := range f.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := f.series[k]
		if f.kind != kindHistogram {
			fmt.Fprintf(w, "%s%s %s\n", f.name, labelText(f.labels, s.values, "", ""), num(s.value))
			continue
		}
		var cum uint64
		for i, ub := range f.buckets {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelText(f.labels, s.values, "le", num(ub)), cum)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelText(f.labels, s.values, "le", "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelText(f.labels, s.values, "", ""), num(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelText(f.labels, s.values, "", ""), s.count)
	}
}

func labelText(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(values[i]))
		b.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraName)
		b.WriteString(`="`)
		b.WriteString(extraValue)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func num(x float64) string {
	switch {
	case math.IsInf(x, 1):
		return "+Inf"
	case math.IsInf(x, -1):
		return "-Inf"
	case math.IsNaN(x):
		return "NaN"
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}

// Handler serves GET /metrics.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.Write(w)
	})
	return mux
}
