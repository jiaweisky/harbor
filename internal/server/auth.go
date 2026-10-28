package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerMiddleware enforces "Authorization: Bearer <token>" against a token set.
func BearerMiddleware(tokens []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := BearerToken(r.Header.Get("Authorization"))
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

// BearerToken extracts the token from an "Authorization: Bearer <token>"
// header value. It reports false when the scheme is missing or the token empty.
func BearerToken(header string) (string, bool) {
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
