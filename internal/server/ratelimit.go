package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jiaweisky/harbor/internal/bridge"
	"github.com/jiaweisky/harbor/internal/config"
)

// rateLimiter is a per-(caller, agent) token bucket. A nil limiter allows
// everything, so the middleware is free when rate limiting is disabled.
type rateLimiter struct {
	mu          sync.Mutex
	perMin      float64
	burst       float64
	buckets     map[string]*bucket
	now         func() time.Time
	authEnabled bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter from config, or nil when disabled.
// authEnabled reports whether bearer authentication is enforced ahead of the
// limiter; when it is not, an unverified token must not identify a caller.
func newRateLimiter(cfg *config.RateLimitConfig, authEnabled bool) *rateLimiter {
	if cfg == nil || cfg.PerMinute <= 0 {
		return nil
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = cfg.PerMinute
	}
	return &rateLimiter{
		perMin:      float64(cfg.PerMinute),
		burst:       float64(burst),
		buckets:     map[string]*bucket{},
		now:         time.Now,
		authEnabled: authEnabled,
	}
}

// allow reports whether a request for key may proceed, refilling the caller's
// bucket by the elapsed time since the last request.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * (l.perMin / 60)
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// middleware rate-limits requests to one agent, keyed by the caller's bearer
// token (or client IP when unauthenticated).
func (l *rateLimiter) middleware(agentID string, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(agentID + "\x00" + l.callerKey(r)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerKey identifies the caller for a rate-limit bucket. When bearer auth is
// enforced ahead of the limiter the token has already been validated, so its
// hash identifies the caller; otherwise the token is unverified and the client
// address is used, so rotating a header cannot mint fresh buckets.
func (l *rateLimiter) callerKey(r *http.Request) string {
	if l.authEnabled {
		if tok, ok := bridge.BearerToken(r.Header.Get("Authorization")); ok {
			return tokenKey(tok)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// tokenKey hashes a bearer token so the limiter never stores the raw secret.
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
