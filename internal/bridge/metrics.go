package bridge

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/jiaweisky/harbor/internal/agent"
)

// durationBucketBounds are the task-duration histogram buckets, in seconds.
var durationBucketBounds = [...]float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// Task outcomes recorded as metrics and audit events.
const (
	outcomeCompleted = "completed"
	outcomeFailed    = "failed"
	outcomeCanceled  = "canceled"
	outcomeRejected  = "rejected"
)

// agentMetrics holds per-agent runtime counters. It is safe for concurrent use.
type agentMetrics struct {
	submitted atomic.Int64
	active    atomic.Int64
	completed atomic.Int64
	failed    atomic.Int64
	canceled  atomic.Int64
	rejected  atomic.Int64

	// Task-duration histogram (seconds): per-bucket counts plus sum/count.
	durBuckets [len(durationBucketBounds)]atomic.Int64
	durSumMS   atomic.Int64
	durCount   atomic.Int64

	// Token/cost totals reported by the CLI.
	tokensIn  atomic.Int64
	tokensOut atomic.Int64
	costMicro atomic.Int64
}

// observeDuration records a finished task's duration in the histogram.
func (m *agentMetrics) observeDuration(d time.Duration) {
	m.durSumMS.Add(d.Milliseconds())
	m.durCount.Add(1)
	sec := d.Seconds()
	for i, bound := range durationBucketBounds {
		if sec <= bound {
			m.durBuckets[i].Add(1)
			return
		}
	}
}

// addUsage accumulates token/cost usage reported by the CLI.
func (m *agentMetrics) addUsage(u *agent.Usage) {
	if u == nil {
		return
	}
	m.tokensIn.Add(int64(u.InputTokens))
	m.tokensOut.Add(int64(u.OutputTokens))
	if u.CostUSD > 0 {
		m.costMicro.Add(int64(u.CostUSD * 1e6))
	}
}

// record increments the counter matching an outcome.
func (m *agentMetrics) record(outcome string) {
	switch outcome {
	case outcomeCompleted:
		m.completed.Add(1)
	case outcomeFailed:
		m.failed.Add(1)
	case outcomeCanceled:
		m.canceled.Add(1)
	case outcomeRejected:
		m.rejected.Add(1)
	}
}

// MetricsSnapshot is a point-in-time view of one agent's counters and health.
type MetricsSnapshot struct {
	Name         string
	Submitted    int64
	Active       int64
	Completed    int64
	Failed       int64
	Canceled     int64
	Rejected     int64
	BreakerState string

	// DurationBuckets holds per-bucket counts (non-cumulative) aligned with
	// durationBucketBounds; DurationSumSeconds and DurationCount are the
	// histogram aggregate.
	DurationBuckets    []int64
	DurationSumSeconds float64
	DurationCount      int64

	// Token/cost totals.
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
}

// terminalOutcome maps a terminal status event to its outcome name.
func terminalOutcome(ev a2a.Event) (string, bool) {
	su, ok := ev.(*a2a.TaskStatusUpdateEvent)
	if !ok {
		return "", false
	}
	switch su.Status.State {
	case a2a.TaskStateCompleted:
		return outcomeCompleted, true
	case a2a.TaskStateFailed:
		return outcomeFailed, true
	case a2a.TaskStateCanceled:
		return outcomeCanceled, true
	case a2a.TaskStateRejected:
		return outcomeRejected, true
	}
	return "", false
}

// WritePrometheus renders snapshots in the Prometheus text exposition format.
func WritePrometheus(w io.Writer, snaps []MetricsSnapshot) {
	sorted := append([]MetricsSnapshot(nil), snaps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	writeGauge := func(name, help string, value func(MetricsSnapshot) int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, s := range sorted {
			fmt.Fprintf(w, "%s{agent=%s} %d\n", name, promLabel(s.Name), value(s))
		}
	}

	fmt.Fprintf(w, "# HELP harbor_tasks_submitted_total Tasks submitted.\n# TYPE harbor_tasks_submitted_total counter\n")
	for _, s := range sorted {
		fmt.Fprintf(w, "harbor_tasks_submitted_total{agent=%s} %d\n", promLabel(s.Name), s.Submitted)
	}

	fmt.Fprintf(w, "# HELP harbor_tasks_total Tasks finished, by outcome.\n# TYPE harbor_tasks_total counter\n")
	for _, s := range sorted {
		outcomes := []struct {
			name  string
			count int64
		}{
			{outcomeCompleted, s.Completed},
			{outcomeFailed, s.Failed},
			{outcomeCanceled, s.Canceled},
			{outcomeRejected, s.Rejected},
		}
		for _, o := range outcomes {
			fmt.Fprintf(w, "harbor_tasks_total{agent=%s,outcome=%s} %d\n", promLabel(s.Name), promLabel(o.name), o.count)
		}
	}

	writeGauge("harbor_tasks_active", "In-flight tasks.", func(s MetricsSnapshot) int64 { return s.Active })
	writeGauge("harbor_breaker_state", "Circuit breaker state (0=closed, 1=half-open, 2=open).",
		func(s MetricsSnapshot) int64 { return breakerGauge(s.BreakerState) })

	writeDurationHistogram(w, sorted)
	writeUsageCounters(w, sorted)
}

// writeDurationHistogram renders the task-duration histogram. Bucket counts are
// stored non-cumulative and summed here into the cumulative form Prometheus
// expects.
func writeDurationHistogram(w io.Writer, snaps []MetricsSnapshot) {
	fmt.Fprintf(w, "# HELP harbor_task_duration_seconds Task duration from submission to terminal.\n"+
		"# TYPE harbor_task_duration_seconds histogram\n")
	for _, s := range snaps {
		var cumulative int64
		for i, bound := range durationBucketBounds {
			cumulative += s.DurationBuckets[i]
			fmt.Fprintf(w, "harbor_task_duration_seconds_bucket{agent=%s,le=%s} %d\n",
				promLabel(s.Name), promLabel(strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
		}
		fmt.Fprintf(w, "harbor_task_duration_seconds_bucket{agent=%s,le=\"+Inf\"} %d\n", promLabel(s.Name), s.DurationCount)
		fmt.Fprintf(w, "harbor_task_duration_seconds_sum{agent=%s} %g\n", promLabel(s.Name), s.DurationSumSeconds)
		fmt.Fprintf(w, "harbor_task_duration_seconds_count{agent=%s} %d\n", promLabel(s.Name), s.DurationCount)
	}
}

// writeUsageCounters renders the token and cost totals reported by the CLI.
func writeUsageCounters(w io.Writer, snaps []MetricsSnapshot) {
	fmt.Fprintf(w, "# HELP harbor_task_tokens_total Tokens reported by the CLI, by direction.\n"+
		"# TYPE harbor_task_tokens_total counter\n")
	for _, s := range snaps {
		fmt.Fprintf(w, "harbor_task_tokens_total{agent=%s,direction=\"input\"} %d\n", promLabel(s.Name), s.TokensIn)
		fmt.Fprintf(w, "harbor_task_tokens_total{agent=%s,direction=\"output\"} %d\n", promLabel(s.Name), s.TokensOut)
	}

	fmt.Fprintf(w, "# HELP harbor_task_cost_usd_total Reported task cost in USD.\n"+
		"# TYPE harbor_task_cost_usd_total counter\n")
	for _, s := range snaps {
		fmt.Fprintf(w, "harbor_task_cost_usd_total{agent=%s} %g\n", promLabel(s.Name), s.CostUSD)
	}
}

// breakerGauge maps a breaker state name to its numeric gauge value.
func breakerGauge(state string) int64 {
	switch state {
	case stateNameHalfOpen:
		return 1
	case stateNameOpen:
		return 2
	default:
		return 0
	}
}

// promLabel quotes a Prometheus label value. Unlike %q it emits only the escapes
// the exposition format defines.
func promLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}
