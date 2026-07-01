// Package metrics is a tiny, dependency-free metrics registry that exposes
// counters and gauges in the Prometheus text exposition format. It exists so
// operators of a Castlet install can scrape /metrics without pulling in
// prometheus/client_golang — in the spirit of the rest of the codebase, which
// prefers the standard library (e.g. blob/s3 hand-rolls SigV4).
//
// It supports exactly what Castlet needs: monotonic counters (with optional
// label pairs), settable gauges, and gauges whose value is computed on scrape
// (GaugeFunc, used for queue depth). Values are stored as float64 bits behind
// sync/atomic so hot-path Inc/Add calls never take the registry lock; the lock
// only guards the family/series maps while a new series is created.
//
// A Registry is safe for concurrent use.
package metrics

import (
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Type is the exposition metric type.
type Type int

const (
	// Counter is a monotonically increasing value.
	Counter Type = iota
	// Gauge is a value that can go up or down.
	Gauge
)

func (t Type) String() string {
	if t == Gauge {
		return "gauge"
	}
	return "counter"
}

// Registry holds metric families and renders them in the Prometheus text
// exposition format.
type Registry struct {
	mu    sync.Mutex
	fams  map[string]*family
	order []string // family names in registration order, for stable output
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{fams: make(map[string]*family)}
}

type family struct {
	name   string
	help   string
	typ    Type
	fn     func() float64     // non-nil => computed gauge, no labeled series
	series map[string]*sample // by canonical label key
	order  []string           // series keys in first-seen order
}

type sample struct {
	labels string // pre-rendered `{k="v",...}` (empty when there are no labels)
	bits   atomic.Uint64
}

func (s *sample) add(delta float64) {
	for {
		old := s.bits.Load()
		next := math.Float64frombits(old) + delta
		if s.bits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

func (s *sample) set(v float64) { s.bits.Store(math.Float64bits(v)) }
func (s *sample) get() float64  { return math.Float64frombits(s.bits.Load()) }

// Register declares a family's help text and type up front. It is optional:
// Inc/Add/Set auto-create a family (Counter for Inc/Add, Gauge for Set) with no
// help text. Registering ahead of time yields nicer # HELP/# TYPE headers and
// pins the family's type. Re-registering an existing family updates its help.
func (r *Registry) Register(name string, typ Type, help string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.familyLocked(name, typ)
	f.help = help
}

// GaugeFunc registers a gauge whose value is computed at scrape time. It is used
// for values sourced from elsewhere (e.g. pending job count from the store).
// Re-registering replaces the function.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.familyLocked(name, Gauge)
	f.help = help
	f.fn = fn
}

// Inc adds 1 to the counter series identified by name and the label key/value
// pairs (labelKV must be even-length: k1, v1, k2, v2, ...).
func (r *Registry) Inc(name string, labelKV ...string) { r.Add(name, 1, labelKV...) }

// Add adds delta to the counter series identified by name and labels.
func (r *Registry) Add(name string, delta float64, labelKV ...string) {
	r.sample(name, Counter, labelKV).add(delta)
}

// Set sets the gauge series identified by name and labels to v.
func (r *Registry) Set(name string, v float64, labelKV ...string) {
	r.sample(name, Gauge, labelKV).set(v)
}

// sample resolves (creating if needed) the series for a family+labels. The map
// mutations are done under the lock; the returned sample's value is updated
// lock-free by the caller via atomics.
func (r *Registry) sample(name string, typ Type, labelKV []string) *sample {
	key := renderLabels(labelKV)
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.familyLocked(name, typ)
	s := f.series[key]
	if s == nil {
		s = &sample{labels: key}
		f.series[key] = s
		f.order = append(f.order, key)
	}
	return s
}

// familyLocked returns the family, creating it with typ when absent. Caller
// holds r.mu.
func (r *Registry) familyLocked(name string, typ Type) *family {
	f := r.fams[name]
	if f == nil {
		f = &family{name: name, typ: typ, series: make(map[string]*sample)}
		r.fams[name] = f
		r.order = append(r.order, name)
	}
	return f
}

// ServeHTTP renders the registry as a Prometheus scrape response, so a Registry
// can be mounted directly as the /metrics handler.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = r.WriteTo(w)
}

// WriteTo renders every family in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	// Snapshot the families and their series so scrape rendering happens without
	// holding the lock (GaugeFunc callbacks may be slow / take other locks).
	type line struct {
		labels string
		val    float64
	}
	type fam struct {
		name  string
		help  string
		typ   Type
		fn    func() float64
		lines []line
	}
	snap := make([]fam, 0, len(r.order))
	for _, name := range r.order {
		f := r.fams[name]
		fs := fam{name: f.name, help: f.help, typ: f.typ, fn: f.fn}
		for _, key := range f.order {
			s := f.series[key]
			fs.lines = append(fs.lines, line{labels: s.labels, val: s.get()})
		}
		snap = append(snap, fs)
	}
	r.mu.Unlock()

	cw := &countingWriter{w: w}
	var buf strings.Builder
	for _, f := range snap {
		buf.Reset()
		if f.help != "" {
			buf.WriteString("# HELP ")
			buf.WriteString(f.name)
			buf.WriteByte(' ')
			buf.WriteString(escapeHelp(f.help))
			buf.WriteByte('\n')
		}
		buf.WriteString("# TYPE ")
		buf.WriteString(f.name)
		buf.WriteByte(' ')
		buf.WriteString(f.typ.String())
		buf.WriteByte('\n')
		if f.fn != nil {
			writeLine(&buf, f.name, "", f.fn())
		}
		for _, ln := range f.lines {
			writeLine(&buf, f.name, ln.labels, ln.val)
		}
		if _, err := io.WriteString(cw, buf.String()); err != nil {
			return cw.n, err
		}
	}
	return cw.n, nil
}

func writeLine(buf *strings.Builder, name, labels string, v float64) {
	buf.WriteString(name)
	buf.WriteString(labels)
	buf.WriteByte(' ')
	buf.WriteString(formatFloat(v))
	buf.WriteByte('\n')
}

// renderLabels turns k,v pairs into a canonical `{k="v",...}` string with keys
// sorted so the same label set always maps to the same series. An odd trailing
// key without a value is dropped. Empty input yields "".
func renderLabels(kv []string) string {
	n := len(kv) / 2
	if n == 0 {
		return ""
	}
	pairs := make([][2]string, 0, n)
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, [2]string{kv[i], kv[i+1]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p[0])
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(p[1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes a label value per the exposition format: backslash,
// double-quote, and newline.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\"", `\"`, "\n", `\n`)
	return r.Replace(s)
}

// escapeHelp escapes a HELP string: backslash and newline (quotes are allowed).
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	r := strings.NewReplacer("\\", `\\`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(v float64) string {
	// Integral values render without a decimal point for readable counters.
	if v == math.Trunc(v) && !math.IsInf(v, 0) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
