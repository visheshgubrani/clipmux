// Package metrics keeps the server's counters, gauges and histograms and
// serves them in the Prometheus text exposition format (version 0.0.4).
// It is small on purpose: the format is stable, and a client library would
// bring a global registry and dependencies for what this covers.
package metrics

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metric families. The zero value is not usable; use New.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// New returns an empty registry.
func New() *Registry { return &Registry{families: map[string]*family{}} }

type family struct {
	name, help, typ string
	labels          []string
	buckets         []float64 // histograms only
	mu              sync.Mutex
	series          map[string]*series // by joined label values
	collect         func() []Sample    // scrape-time families only
}

type series struct {
	values []string
	value  float64   // counter or gauge
	counts []float64 // histogram: per bucket, not cumulative
	sum    float64
	count  float64
}

// Sample is one series of a scrape-time family: its label values, in the
// order the family declared them, and its value.
type Sample struct {
	Labels []string
	Value  float64
}

func (r *Registry) add(f *family) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[f.name]; dup {
		panic("metrics: " + f.name + " registered twice")
	}
	f.series = map[string]*series{}
	r.families[f.name] = f
	return f
}

// Counter is a family of counters, one per set of label values.
type Counter struct{ f *family }

// Counter registers a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.add(&family{name: name, help: help, typ: "counter", labels: labels})}
}

// Add adds v (which must not be negative) to the series with these values.
func (c *Counter) Add(v float64, values ...string) {
	if v < 0 {
		return
	}
	c.f.update(values, func(s *series) { s.value += v })
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Gauge is a family of gauges.
type Gauge struct{ f *family }

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	return &Gauge{r.add(&family{name: name, help: help, typ: "gauge", labels: labels})}
}

// Set sets the series with these values.
func (g *Gauge) Set(v float64, values ...string) { g.f.update(values, func(s *series) { s.value = v }) }

// Add moves the series by v, which may be negative.
func (g *Gauge) Add(v float64, values ...string) {
	g.f.update(values, func(s *series) { s.value += v })
}

// Histogram is a family of histograms.
type Histogram struct{ f *family }

// Histogram registers a histogram family with these upper bounds, in
// increasing order; +Inf is implied.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	return &Histogram{r.add(&family{name: name, help: help, typ: "histogram", labels: labels, buckets: slices.Clone(buckets)})}
}

// Observe records v in the series with these values.
func (h *Histogram) Observe(v float64, values ...string) {
	h.f.update(values, func(s *series) {
		if s.counts == nil {
			s.counts = make([]float64, len(h.f.buckets))
		}
		for i, b := range h.f.buckets {
			if v <= b {
				s.counts[i]++
				break
			}
		}
		s.sum += v
		s.count++
	})
}

// Func registers a family whose series are read at scrape time, such as
// counts from Postgres. typ is "gauge" or "counter".
func (r *Registry) Func(name, help, typ string, labels []string, collect func() []Sample) {
	r.add(&family{name: name, help: help, typ: typ, labels: labels, collect: collect})
}

func (f *family) update(values []string, fn func(*series)) {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.series[key]
	if s == nil {
		s = &series{values: slices.Clone(values)}
		f.series[key] = s
	}
	fn(s)
}

// Handler serves every family in the text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(r.Expose())
	})
}

// Expose renders every family, sorted by name, each series sorted by its
// label values.
func (r *Registry) Expose() []byte {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	slices.SortFunc(fams, func(a, b *family) int { return strings.Compare(a.name, b.name) })

	var b bytes.Buffer
	for _, f := range fams {
		f.write(&b)
	}
	return b.Bytes()
}

func (f *family) write(b *bytes.Buffer) {
	var list []*series
	if f.collect != nil {
		for _, s := range f.collect() {
			if len(s.Labels) == len(f.labels) {
				list = append(list, &series{values: s.Labels, value: s.Value})
			}
		}
	} else {
		f.mu.Lock()
		for _, s := range f.series {
			c := *s
			c.counts = slices.Clone(s.counts)
			list = append(list, &c)
		}
		f.mu.Unlock()
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.typ)
	slices.SortFunc(list, func(x, y *series) int { return slices.Compare(x.values, y.values) })
	for _, s := range list {
		if f.typ != "histogram" {
			fmt.Fprintf(b, "%s%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), number(s.value))
			continue
		}
		var cum float64
		for i, bound := range f.buckets {
			if s.counts != nil {
				cum += s.counts[i]
			}
			fmt.Fprintf(b, "%s_bucket%s %s\n", f.name, labelSet(f.labels, s.values, "le", number(bound)), number(cum))
		}
		fmt.Fprintf(b, "%s_bucket%s %s\n", f.name, labelSet(f.labels, s.values, "le", "+Inf"), number(s.count))
		fmt.Fprintf(b, "%s_sum%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), number(s.sum))
		fmt.Fprintf(b, "%s_count%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), number(s.count))
	}
}

func labelSet(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeValue(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func number(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var (
	valueEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeValue(s string) string { return valueEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
