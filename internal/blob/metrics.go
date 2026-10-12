package blob

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// durationBounds are the operation-duration histogram buckets, in seconds.
var durationBounds = [...]float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30}

// Counters and duration histograms for data-plane operations, keyed by op +
// label (backend or scheme) + result. They are process-wide and concurrency
// safe.
var (
	metricsMu sync.Mutex
	counters  = map[metricKey]*int64{}
	durations = map[string]*durMetric{}
)

type metricKey struct{ op, label, result string }

type durMetric struct {
	buckets [len(durationBounds)]atomic.Int64
	sumMS   atomic.Int64
	count   atomic.Int64
}

// count increments the counter for one operation outcome.
func count(op, label, result string) {
	metricsMu.Lock()
	k := metricKey{op, label, result}
	c, ok := counters[k]
	if !ok {
		c = new(int64)
		counters[k] = c
	}
	metricsMu.Unlock()
	atomic.AddInt64(c, 1)
}

// observe records one operation duration.
func observe(op string, d time.Duration) {
	metricsMu.Lock()
	m, ok := durations[op]
	if !ok {
		m = &durMetric{}
		durations[op] = m
	}
	metricsMu.Unlock()
	m.sumMS.Add(d.Milliseconds())
	m.count.Add(1)
	sec := d.Seconds()
	for i, bound := range durationBounds {
		if sec <= bound {
			m.buckets[i].Add(1)
			return
		}
	}
}

// record counts, times, and logs one data-plane operation.
func record(op, label string, err error, start time.Time, kv ...any) {
	d := time.Since(start)
	result := resultLabel(err)
	count(op, label, result)
	observe(op, d)
	args := append([]any{"op", op, "label", label, "result", result, "duration", d}, kv...)
	if err != nil {
		args = append(args, "err", err)
	}
	slog.Debug("blob operation", args...)
}

// resultLabel classifies an operation outcome.
func resultLabel(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}

// Counter is one exported sample.
type Counter struct {
	Op     string
	Label  string
	Result string
	Value  int64
}

// Snapshot returns the current counters, sorted for stable output.
func Snapshot() []Counter {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	out := make([]Counter, 0, len(counters))
	for k, c := range counters {
		out = append(out, Counter{k.op, k.label, k.result, atomic.LoadInt64(c)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Op != out[j].Op {
			return out[i].Op < out[j].Op
		}
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Result < out[j].Result
	})
	return out
}

// WritePrometheus renders the counters and operation-duration histograms in the
// Prometheus text exposition format.
func WritePrometheus(w io.Writer) {
	families := []struct {
		name, help, op, labelName string
	}{
		{"harbor_blob_get_total", "Store reads.", "get", "backend"},
		{"harbor_blob_put_total", "Store writes to the process's own ingest.", "put", "backend"},
		{"harbor_blob_reserve_total", "Broker upload-slot reservations.", "reserve", "backend"},
		{"harbor_blob_upload_total", "Sender uploads to a consumer target.", "upload", "scheme"},
	}
	snaps := Snapshot()
	for _, f := range families {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", f.name, f.help, f.name)
		for _, c := range snaps {
			if c.Op != f.op {
				continue
			}
			fmt.Fprintf(w, "%s{%s=%s,result=%s} %d\n", f.name, f.labelName, promLabel(c.Label), promLabel(c.Result), c.Value)
		}
	}
	writeDurations(w)
}

// writeDurations renders the per-op duration histograms.
func writeDurations(w io.Writer) {
	metricsMu.Lock()
	ops := make([]string, 0, len(durations))
	for op := range durations {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	metrics := make([]*durMetric, len(ops))
	for i, op := range ops {
		metrics[i] = durations[op]
	}
	metricsMu.Unlock()

	fmt.Fprintf(w, "# HELP harbor_blob_operation_duration_seconds Data-plane operation duration.\n"+
		"# TYPE harbor_blob_operation_duration_seconds histogram\n")
	for i, op := range ops {
		m := metrics[i]
		var cumulative int64
		for j, bound := range durationBounds {
			cumulative += m.buckets[j].Load()
			fmt.Fprintf(w, "harbor_blob_operation_duration_seconds_bucket{op=%s,le=%s} %d\n",
				promLabel(op), promLabel(formatFloat(bound)), cumulative)
		}
		fmt.Fprintf(w, "harbor_blob_operation_duration_seconds_bucket{op=%s,le=\"+Inf\"} %d\n", promLabel(op), m.count.Load())
		fmt.Fprintf(w, "harbor_blob_operation_duration_seconds_sum{op=%s} %g\n", promLabel(op), float64(m.sumMS.Load())/1000)
		fmt.Fprintf(w, "harbor_blob_operation_duration_seconds_count{op=%s} %d\n", promLabel(op), m.count.Load())
	}
}

// formatFloat renders a bucket bound compactly.
func formatFloat(v float64) string { return fmt.Sprintf("%g", v) }

// promLabel quotes a Prometheus label value.
func promLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}

// schemeOf extracts the URL scheme for the upload counter label.
func schemeOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i > 0 {
		return rawURL[:i]
	}
	return "unknown"
}

// redactURL strips userinfo and query from a URL so a presigned URL's credential
// never reaches a log line.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
