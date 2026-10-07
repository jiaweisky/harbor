package bridge

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// RequireAuthOnExposedBind refuses to bind a non-loopback address that has no
// authentication configured, unless the operator explicitly opts out with
// allowInsecure. Any non-"none" auth mode is accepted; the check is about
// having *some* auth, not about a specific scheme.
func RequireAuthOnExposedBind(listen, mode string, allowInsecure bool) error {
	if isLoopback(listen) || allowInsecure || mode != "none" {
		return nil
	}
	return fmt.Errorf("refusing to bind %q with auth.mode=none; set server.auth.mode or server.allow_insecure", listen)
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	// Empty host (":8080") binds all interfaces -> treat as non-loopback.
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// BearerMiddleware enforces "Authorization: Bearer <token>" against a token set.
func BearerMiddleware(tokens []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !matchToken(got, tokens) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware answers preflight and sets CORS headers for allowed origins.
func CORSMiddleware(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func matchToken(got string, tokens []string) bool {
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
			return true
		}
	}
	return false
}

func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
