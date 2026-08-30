// Package telemetry exposes metrics in the Prometheus text exposition format.
//
// Hand-rolled rather than using the official client library. The reasoning:
// this process exports a few dozen series, all of which it already computes for
// its own API; the client library's value is its registry, its collector
// abstraction and its histogram implementation, and MIRROR has its own
// histogram (in internal/state) whose buckets must match the ones the
// simulation reports. Bridging the two would mean maintaining a translation
// layer between two histogram representations, which is more code and more
// risk than emitting the format directly. The format itself is a stable,
// documented, one-page specification.
//
// If this service ever needs exemplars, native histograms or the OpenMetrics
// content negotiation, that trade flips and the library should be adopted.
package telemetry

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds counters and latency summaries for the HTTP layer plus
// whatever the caller writes in directly at scrape time.
type Registry struct {
	mu sync.Mutex
	// apiLatency is a per-route bucketed histogram in milliseconds.
	apiLatency map[string]*hist
	apiStatus  map[string]int64
	started    time.Time
}

// latencyBounds are the upper edges, in milliseconds. Chosen around the
// behaviour that matters here: a read of live state should be sub-millisecond
// because it is a lock acquisition and a struct walk, and anything past 100ms
// means it queued behind a tick.
var latencyBounds = []float64{0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

type hist struct {
	counts []int64
	sum    float64
	count  int64
}

func newHist() *hist { return &hist{counts: make([]int64, len(latencyBounds)+1)} }

func (h *hist) observe(ms float64) {
	h.sum += ms
	h.count++
	for i, b := range latencyBounds {
		if ms <= b {
			h.counts[i]++
			return
		}
	}
	h.counts[len(latencyBounds)]++
}

func NewRegistry() *Registry {
	return &Registry{
		apiLatency: make(map[string]*hist),
		apiStatus:  make(map[string]int64),
		started:    time.Now(),
	}
}

// ObserveAPI records one handled request.
//
// The path is normalised to its route template before use as a label. Using
// the raw path would create one time series per simulation id, which is the
// canonical way to melt a Prometheus instance -- cardinality is a resource,
// and it is spent here deliberately.
func (r *Registry) ObserveAPI(path string, status int, d time.Duration) {
	route := normaliseRoute(path)
	r.mu.Lock()
	h, ok := r.apiLatency[route]
	if !ok {
		h = newHist()
		r.apiLatency[route] = h
	}
	h.observe(float64(d.Microseconds()) / 1000)
	r.apiStatus[route+"|"+strconv.Itoa(status/100)+"xx"]++
	r.mu.Unlock()
}

func normaliseRoute(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range parts {
		if strings.HasPrefix(seg, "sim-") || strings.HasPrefix(seg, "scn-") {
			parts[i] = "{id}"
		}
	}
	return "/" + strings.Join(parts, "/")
}

// Writer accumulates exposition-format output.
type Writer struct {
	b strings.Builder
}

func NewWriter() *Writer { return &Writer{} }

func (w *Writer) Help(name, help, typ string) {
	fmt.Fprintf(&w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// Gauge writes a single sample. Labels must alternate key, value.
func (w *Writer) Gauge(name string, v float64, labels ...string) {
	w.sample(name, v, labels...)
}

func (w *Writer) Counter(name string, v float64, labels ...string) {
	w.sample(name, v, labels...)
}

func (w *Writer) sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	if len(labels) >= 2 {
		w.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.b.WriteByte(',')
			}
			w.b.WriteString(labels[i])
			w.b.WriteString(`="`)
			w.b.WriteString(escapeLabel(labels[i+1]))
			w.b.WriteByte('"')
		}
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	w.b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	w.b.WriteByte('\n')
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func (w *Writer) String() string { return w.b.String() }

// WriteAPI emits the HTTP-layer series.
func (r *Registry) WriteAPI(w *Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w.Help("mirror_process_uptime_seconds", "Seconds since the process started", "gauge")
	w.Gauge("mirror_process_uptime_seconds", time.Since(r.started).Seconds())

	routes := make([]string, 0, len(r.apiLatency))
	for k := range r.apiLatency {
		routes = append(routes, k)
	}
	sort.Strings(routes)

	w.Help("mirror_api_request_duration_ms", "API handler latency in milliseconds", "histogram")
	for _, route := range routes {
		h := r.apiLatency[route]
		var cum int64
		for i, b := range latencyBounds {
			cum += h.counts[i]
			w.sample("mirror_api_request_duration_ms_bucket", float64(cum),
				"route", route, "le", strconv.FormatFloat(b, 'g', -1, 64))
		}
		cum += h.counts[len(latencyBounds)]
		w.sample("mirror_api_request_duration_ms_bucket", float64(cum), "route", route, "le", "+Inf")
		w.sample("mirror_api_request_duration_ms_sum", h.sum, "route", route)
		w.sample("mirror_api_request_duration_ms_count", float64(h.count), "route", route)
	}

	keys := make([]string, 0, len(r.apiStatus))
	for k := range r.apiStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.Help("mirror_api_requests_total", "API requests by route and status class", "counter")
	for _, k := range keys {
		i := strings.LastIndexByte(k, '|')
		w.Counter("mirror_api_requests_total", float64(r.apiStatus[k]), "route", k[:i], "status", k[i+1:])
	}
}
