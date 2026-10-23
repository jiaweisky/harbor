package bridge

import (
	"sync"
	"time"

	"github.com/jiaweisky/harbor/internal/core"
)

// breakerStates.
const (
	stateClosed = iota
	stateOpen
	stateHalfOpen
)

// Breaker state names reported by State.
const (
	stateNameClosed   = "closed"
	stateNameOpen     = "open"
	stateNameHalfOpen = "half-open"
)

// breaker is a per-agent circuit breaker over a sliding window of the most
// recent call outcomes. It trips when the window is full and the failure ratio
// reaches failureRatio, fast-fails while open, and after the cooldown admits
// half-open probes that close it once enough of them succeed in a row. A
// half-open phase is time-bounded, so a probe that is never reported back cannot
// wedge the breaker. A nil or zero-threshold breaker is disabled.
//
// The breaker cannot associate an outcome with the specific Allow that admitted
// the call, so a call admitted before a trip that finishes during a half-open
// window may be counted as a probe; the effect is a premature close that
// self-corrects on the next window.
type breaker struct {
	window       int           // recent outcomes tracked
	failureRatio float64       // trip when failures/window >= failureRatio
	halfOpenNeed int           // consecutive probe successes needed to close
	cooldown     time.Duration // open duration before a probe (also the probe deadline)
	maxCooldown  time.Duration // backoff cap

	mu             sync.Mutex
	state          int
	outcomes       []bool // ring of the last window outcomes; true = failure
	next           int
	failures       int
	openedAt       time.Time
	curCooldown    time.Duration
	halfOpenUntil  time.Time
	probeInFlight  int
	probeSuccesses int
}

// newBreaker builds a breaker, applying sensible defaults for zero fields.
func newBreaker(cfg *core.Breaker) *breaker {
	if cfg == nil || cfg.Threshold <= 0 {
		return nil
	}
	b := &breaker{
		window:       cfg.Threshold,
		failureRatio: cfg.FailureRatio,
		halfOpenNeed: cfg.HalfOpenSuccesses,
		cooldown:     cfg.Cooldown,
		maxCooldown:  cfg.MaxCooldown,
	}
	switch {
	case b.failureRatio <= 0:
		b.failureRatio = 0.5
	case b.failureRatio > 1:
		b.failureRatio = 1
	}
	if b.halfOpenNeed <= 0 {
		b.halfOpenNeed = 1
	}
	if b.cooldown <= 0 {
		b.cooldown = 30 * time.Second
	}
	if b.maxCooldown <= 0 {
		b.maxCooldown = 10 * time.Minute
	}
	if b.maxCooldown < b.cooldown {
		b.maxCooldown = b.cooldown
	}
	return b
}

// Allow reports whether a call may proceed. While open it fast-fails; after the
// cooldown it promotes to half-open and admits a single probe at a time.
func (b *breaker) Allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.expireProbeLocked(now)
	b.promoteLocked(now)
	switch b.state {
	case stateOpen:
		return false
	case stateHalfOpen:
		if b.probeInFlight > 0 {
			return false
		}
		b.probeInFlight = 1
		return true
	default:
		return true
	}
}

// State reports the breaker's current state without side effects. A nil breaker
// (disabled) reports closed.
func (b *breaker) State() string {
	if b == nil {
		return stateNameClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateOpen:
		return stateNameOpen
	case stateHalfOpen:
		return stateNameHalfOpen
	default:
		return stateNameClosed
	}
}

// Success records a call that the agent completed. In half-open it counts toward
// closing the breaker; while open it is ignored (a call that began before the
// trip must not close it).
func (b *breaker) Success() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireProbeLocked(time.Now())
	switch b.state {
	case stateHalfOpen:
		if b.probeInFlight == 0 {
			return // not the admitted probe; ignore a stale outcome
		}
		b.releaseProbeLocked()
		b.probeSuccesses++
		if b.probeSuccesses >= b.halfOpenNeed {
			b.closeLocked()
		}
	case stateClosed:
		b.recordLocked(false)
	}
}

// Failure records a call the agent failed. In half-open it re-opens the breaker
// with a longer cooldown; while closed it feeds the sliding window.
func (b *breaker) Failure() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireProbeLocked(time.Now())
	switch b.state {
	case stateHalfOpen:
		if b.probeInFlight == 0 {
			return // not the admitted probe; ignore a stale outcome
		}
		b.releaseProbeLocked()
		b.openLocked(time.Now())
	case stateClosed:
		b.recordLocked(true)
	}
}

// Resolve releases a half-open probe that ended without an agent failure, such
// as a configuration error or a cancellation, so another probe can run. It never
// closes the breaker.
func (b *breaker) Resolve() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stateHalfOpen {
		b.releaseProbeLocked()
	}
}

// expireProbeLocked reverts a half-open phase whose probe was never reported
// back, so the breaker cannot stay wedged in half-open. It is the only
// time-based transition applied to an outcome report.
func (b *breaker) expireProbeLocked(now time.Time) {
	if b.state == stateHalfOpen && now.After(b.halfOpenUntil) {
		b.openLocked(now)
	}
}

// promoteLocked moves an open breaker to half-open once the cooldown elapses.
// Only Allow calls it, so a late report from a call admitted before the trip
// cannot promote the breaker and then count as a probe.
func (b *breaker) promoteLocked(now time.Time) {
	if b.state == stateOpen && now.Sub(b.openedAt) >= b.curCooldown {
		b.enterHalfOpenLocked(now)
	}
}

// recordLocked adds one outcome to the sliding window and trips the breaker when
// the window is full and the failure ratio is reached.
func (b *breaker) recordLocked(failure bool) {
	if len(b.outcomes) < b.window {
		b.outcomes = append(b.outcomes, failure)
	} else {
		if b.outcomes[b.next] {
			b.failures--
		}
		b.outcomes[b.next] = failure
		b.next = (b.next + 1) % b.window
	}
	if failure {
		b.failures++
	}
	if len(b.outcomes) >= b.window && float64(b.failures)/float64(len(b.outcomes)) >= b.failureRatio {
		b.openLocked(time.Now())
	}
}

// closeLocked returns the breaker to closed and clears the window and backoff.
func (b *breaker) closeLocked() {
	b.state = stateClosed
	b.outcomes = b.outcomes[:0]
	b.next = 0
	b.failures = 0
	b.curCooldown = 0
	b.releaseProbeLocked()
	b.probeSuccesses = 0
}

// openLocked fast-fails, clears the window, and grows the cooldown (doubling up
// to maxCooldown).
func (b *breaker) openLocked(now time.Time) {
	b.state = stateOpen
	b.openedAt = now
	b.outcomes = b.outcomes[:0]
	b.next = 0
	b.failures = 0
	b.releaseProbeLocked()
	b.probeSuccesses = 0
	switch {
	case b.curCooldown == 0:
		b.curCooldown = b.cooldown
	case b.curCooldown*2 > b.maxCooldown:
		b.curCooldown = b.maxCooldown
	default:
		b.curCooldown *= 2
	}
}

// enterHalfOpenLocked opens a single-probe window ending at the probe deadline.
func (b *breaker) enterHalfOpenLocked(now time.Time) {
	b.state = stateHalfOpen
	b.probeInFlight = 0
	b.probeSuccesses = 0
	b.halfOpenUntil = now.Add(b.cooldown)
}

// releaseProbeLocked frees the half-open probe slot if one is held.
func (b *breaker) releaseProbeLocked() {
	if b.probeInFlight > 0 {
		b.probeInFlight--
	}
}
