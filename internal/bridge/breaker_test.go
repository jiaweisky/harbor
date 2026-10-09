package bridge

import (
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

func TestBreakerDisabled(t *testing.T) {
	if newBreaker(nil) != nil {
		t.Fatal("nil config should disable the breaker")
	}
	var b *breaker
	if !b.Allow() {
		t.Fatal("a nil breaker should allow calls")
	}
}

// TestBreakerTripsOnFailureRatio verifies the breaker trips once the window is
// full and the failure ratio is reached, even when failures are not consecutive.
func TestBreakerTripsOnFailureRatio(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 4, FailureRatio: 0.5, Cooldown: time.Hour, MaxCooldown: time.Hour})
	b.Success()
	b.Failure()
	b.Success()
	if b.State() != stateNameClosed {
		t.Fatalf("state = %s, want closed before the window fills", b.State())
	}
	b.Failure() // 2 failures out of the last 4 -> ratio 0.5
	if b.State() != stateNameOpen {
		t.Fatalf("state = %s, want open at the failure ratio", b.State())
	}
}

// TestBreakerStaleSuccessIgnoredWhileOpen verifies a call that began before the
// trip cannot close the breaker when it later succeeds.
func TestBreakerStaleSuccessIgnoredWhileOpen(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Hour, MaxCooldown: time.Hour})
	b.Failure() // opens
	b.Success()
	if b.State() != stateNameOpen {
		t.Fatalf("state = %s, want open (stale success ignored)", b.State())
	}
	if b.Allow() {
		t.Fatal("breaker should still reject calls")
	}
}

// TestBreakerStaleSuccessAfterCooldownIgnored verifies the same even after the
// cooldown has elapsed: only an admitted probe, not a stale success, changes
// state.
func TestBreakerStaleSuccessAfterCooldownIgnored(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: 40 * time.Millisecond, MaxCooldown: time.Second})
	b.Failure() // opens
	time.Sleep(60 * time.Millisecond)
	b.Success() // a call admitted before the trip, completing after the cooldown
	if b.State() != stateNameOpen {
		t.Fatalf("state = %s, want open (stale success after cooldown ignored)", b.State())
	}
}

// TestBreakerResolveReleasesProbe verifies Resolve frees a half-open probe slot
// without closing the breaker.
func TestBreakerResolveReleasesProbe(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: 100 * time.Millisecond, MaxCooldown: time.Second})
	b.Failure() // opens
	time.Sleep(150 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe after the cooldown")
	}
	b.Resolve() // probe ended without an agent failure: frees the slot, does not close
	if b.State() != stateNameHalfOpen {
		t.Fatalf("state = %s, want half-open", b.State())
	}
	if !b.Allow() {
		t.Fatal("resolve should free the probe slot for another probe")
	}
}

// TestBreakerHalfOpenIgnoresOutcomeWithoutProbe verifies an outcome reported
// while no probe is outstanding (for example after Resolve) is ignored.
func TestBreakerHalfOpenIgnoresOutcomeWithoutProbe(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: 100 * time.Millisecond, MaxCooldown: time.Second})
	b.Failure() // opens
	time.Sleep(150 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe")
	}
	b.Resolve() // frees the probe slot
	b.Success() // no probe outstanding: must not close the breaker
	if b.State() != stateNameHalfOpen {
		t.Fatalf("state = %s, want half-open", b.State())
	}
}

// TestBreakerHalfOpenNeedsConsecutiveSuccesses verifies more than one probe must
// succeed before the breaker closes.
func TestBreakerHalfOpenNeedsConsecutiveSuccesses(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{
		Threshold: 1, HalfOpenSuccesses: 2, Cooldown: 100 * time.Millisecond, MaxCooldown: time.Second,
	})
	b.Failure() // opens
	time.Sleep(150 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe")
	}
	b.Success()
	if b.State() != stateNameHalfOpen {
		t.Fatalf("state = %s, want half-open after one success", b.State())
	}
	if !b.Allow() {
		t.Fatal("expected a second probe")
	}
	b.Success()
	if b.State() != stateNameClosed {
		t.Fatalf("state = %s, want closed after two successes", b.State())
	}
}

// TestBreakerHalfOpenDoesNotWedge verifies an unanswered half-open probe cannot
// keep the breaker half-open forever.
func TestBreakerHalfOpenDoesNotWedge(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: 40 * time.Millisecond, MaxCooldown: time.Second})
	b.Failure() // opens
	time.Sleep(60 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a probe after the cooldown")
	}
	// The probe is never answered; the half-open phase expires and reopens.
	time.Sleep(60 * time.Millisecond)
	if b.Allow() {
		t.Fatal("an unanswered probe must not keep the breaker half-open forever")
	}
	if b.State() != stateNameOpen {
		t.Fatalf("state = %s, want open after the probe deadline", b.State())
	}
	// It becomes recruitable again after the (doubled) cooldown.
	time.Sleep(100 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("breaker should recover after the backoff")
	}
}

func TestBreakerState(t *testing.T) {
	var disabled *breaker
	if got := disabled.State(); got != stateNameClosed {
		t.Fatalf("disabled breaker state = %q, want closed", got)
	}

	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Hour, MaxCooldown: time.Hour})
	if got := b.State(); got != stateNameClosed {
		t.Fatalf("state = %q, want closed", got)
	}
	b.Failure()
	if got := b.State(); got != stateNameOpen {
		t.Fatalf("state = %q, want open", got)
	}
}

func TestBreakerStateHalfOpen(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: 100 * time.Millisecond, MaxCooldown: time.Second})
	b.Failure()
	if got := b.State(); got != stateNameOpen {
		t.Fatalf("state = %q, want open", got)
	}
	time.Sleep(150 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe")
	}
	if got := b.State(); got != stateNameHalfOpen {
		t.Fatalf("state = %q, want half-open", got)
	}
}
