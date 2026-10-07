package bridge

import (
	"sync"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
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

// breaker is a per-agent failure circuit breaker. It opens after a number of
// consecutive failures, rejects calls while open, and probes once after a
// cooldown. A nil or zero-threshold breaker is disabled.
type breaker struct {
	threshold   int
	cooldown    time.Duration
	maxCooldown time.Duration

	mu          sync.Mutex
	state       int
	failures    int
	openedAt    time.Time
	curCooldown time.Duration
	probing     bool
}

// newBreaker builds a breaker, applying sensible defaults for zero fields.
func newBreaker(cfg *config.BreakerConfig) *breaker {
	if cfg == nil || cfg.Threshold <= 0 {
		return nil
	}
	b := &breaker{threshold: cfg.Threshold, cooldown: cfg.Cooldown, maxCooldown: cfg.MaxCooldown}
	if b.cooldown <= 0 {
		b.cooldown = 30 * time.Second
	}
	if b.maxCooldown <= 0 {
		b.maxCooldown = 10 * time.Minute
	}
	return b
}

// Allow reports whether a call may proceed. While open it triggers a single
// half-open probe once the cooldown has elapsed.
func (b *breaker) Allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateOpen:
		if time.Since(b.openedAt) < b.curCooldown {
			return false
		}
		b.state = stateHalfOpen
		b.probing = true
		return true
	case stateHalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
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

// Success closes the breaker.
func (b *breaker) Success() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = stateClosed
	b.failures = 0
	b.probing = false
	b.curCooldown = 0
}

// Failure records a failed call, opening or backing off the breaker.
func (b *breaker) Failure() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stateHalfOpen {
		b.open()
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.open()
	}
}

// Resolve clears a half-open probe when a call ends without an agent failure,
// such as a configuration error or a cancellation. It is a no-op when closed.
func (b *breaker) Resolve() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == stateHalfOpen {
		b.state = stateClosed
		b.probing = false
		b.curCooldown = 0
	}
}

func (b *breaker) open() {
	b.state = stateOpen
	b.openedAt = time.Now()
	b.probing = false
	b.failures = 0

	switch {
	case b.curCooldown == 0:
		b.curCooldown = b.cooldown
	case b.curCooldown*2 > b.maxCooldown:
		b.curCooldown = b.maxCooldown
	default:
		b.curCooldown *= 2
	}
}
