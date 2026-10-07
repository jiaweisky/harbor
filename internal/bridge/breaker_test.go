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

func TestBreakerOpensAfterThreshold(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 2, Cooldown: time.Hour, MaxCooldown: time.Hour})
	if !b.Allow() {
		t.Fatal("should start closed")
	}
	b.Failure()
	if !b.Allow() {
		t.Fatal("one failure should not open it")
	}
	b.Failure()
	if b.Allow() {
		t.Fatal("threshold failures should open it")
	}
}

func TestBreakerResolveClearsProbe(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Millisecond, MaxCooldown: 10 * time.Millisecond})
	b.Failure() // opens
	time.Sleep(5 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe")
	}
	b.Resolve() // probe ended without an agent failure
	if !b.Allow() {
		t.Fatal("resolve should close the breaker")
	}
}

func TestBreakerState(t *testing.T) {
	var disabled *breaker
	if got := disabled.State(); got != "closed" {
		t.Fatalf("disabled breaker state = %q, want closed", got)
	}

	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Hour, MaxCooldown: time.Hour})
	if got := b.State(); got != "closed" {
		t.Fatalf("state = %q, want closed", got)
	}
	b.Failure()
	if got := b.State(); got != "open" {
		t.Fatalf("state = %q, want open", got)
	}
}

func TestBreakerStateHalfOpen(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Millisecond, MaxCooldown: time.Millisecond})
	b.Failure()
	if got := b.State(); got != "open" {
		t.Fatalf("state = %q, want open", got)
	}
	time.Sleep(5 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected a half-open probe")
	}
	if got := b.State(); got != "half-open" {
		t.Fatalf("state = %q, want half-open", got)
	}
}

func TestBreakerHalfOpenProbe(t *testing.T) {
	b := newBreaker(&config.BreakerConfig{Threshold: 1, Cooldown: time.Millisecond, MaxCooldown: 10 * time.Millisecond})
	b.Failure() // opens
	if b.Allow() {
		t.Fatal("should be open before cooldown")
	}
	time.Sleep(5 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("should allow a probe after cooldown")
	}
	if b.Allow() {
		t.Fatal("only one probe should be allowed")
	}
	b.Success()
	if !b.Allow() {
		t.Fatal("success should close the breaker")
	}
}
