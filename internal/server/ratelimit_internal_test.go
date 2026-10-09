package server

import (
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/config"
)

// TestRateLimiterRefill verifies the token bucket limits bursts and refills over
// time at the configured rate.
func TestRateLimiterRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := newRateLimiter(&config.RateLimitConfig{PerMinute: 60, Burst: 2}, false)
	l.now = func() time.Time { return now }

	if !l.allow("k") || !l.allow("k") {
		t.Fatal("the first two requests should be allowed")
	}
	if l.allow("k") {
		t.Fatal("the third request should be limited")
	}
	now = now.Add(time.Second) // 60/min -> one token per second
	if !l.allow("k") {
		t.Fatal("a token should refill after one second")
	}
	if l.allow("k") {
		t.Fatal("only one token refilled")
	}
}

// TestRateLimiterDisabled verifies a nil limiter allows everything.
func TestRateLimiterDisabled(t *testing.T) {
	l := newRateLimiter(nil, false)
	if l != nil {
		t.Fatal("nil config should disable the limiter")
	}
	if !l.allow("k") {
		t.Fatal("a nil limiter must allow requests")
	}
}
