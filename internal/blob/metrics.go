package blob

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counters for data-plane operations, keyed by op + label (backend or scheme) +
// result. They are process-wide and safe for concurrent use.
var (
	metricsMu sync.Mutex
	counters  = map[metricKey]*int64{}
)

type metricKey struct{ op, label, result string }

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

// WritePrometheus renders the counters in the Prometheus text exposition format.
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
}

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
