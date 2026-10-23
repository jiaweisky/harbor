package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		token  string
		ok     bool
	}{
		{"Bearer secret", "secret", true},
		{"bearer secret", "secret", true},
		{"Basic secret", "", false},
		{"Bearer ", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		token, ok := BearerToken(tc.header)
		if token != tc.token || ok != tc.ok {
			t.Errorf("BearerToken(%q) = (%q,%v), want (%q,%v)", tc.header, token, ok, tc.token, tc.ok)
		}
	}
}

func TestBearerMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := BearerMiddleware([]string{"secret"}, next)

	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Errorf("missing token = %d, want 401", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusNoContent {
		t.Errorf("valid token = %d, want 204", authorized.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := CORSMiddleware([]string{"https://ops.example.com"}, next)

	preflight := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://ops.example.com")
	h.ServeHTTP(preflight, req)
	if preflight.Code != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", preflight.Code)
	}
	if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Errorf("allow-origin = %q", got)
	}
}
