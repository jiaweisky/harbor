package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func TestVersionInterceptor(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		wantOK bool
	}{
		{"match", http.Header{"A2A-Version": {"1.0"}}, true},
		{"mismatch", http.Header{"A2A-Version": {"0.3"}}, false},
		{"empty", http.Header{"A2A-Version": {""}}, false},
		{"missing", http.Header{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, callCtx := a2asrv.NewCallContext(
				context.Background(), a2asrv.NewServiceParams(tc.header),
			)

			_, _, err := (versionInterceptor{}).Before(ctx, callCtx, &a2asrv.Request{})
			if tc.wantOK && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}

			if !tc.wantOK && !errors.Is(err, a2a.ErrVersionNotSupported) {
				t.Fatalf("error = %v, want ErrVersionNotSupported", err)
			}
		})
	}
}

func TestA2AVersionQuery(t *testing.T) {
	cases := []struct {
		name   string
		target string
		header string
		want   string
	}{
		{"query only", "/?A2A-Version=1.0", "", "1.0"},
		{"case insensitive", "/?a2a-version=1.0", "", "1.0"},
		{"header wins", "/?A2A-Version=0.3", "1.0", "1.0"},
		{"absent", "/", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.header != "" {
				req.Header.Set(a2a.SvcParamVersion, tc.header)
			}

			var got string
			a2aVersionQuery(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(a2a.SvcParamVersion)
			})).ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Fatalf("A2A-Version = %q, want %q", got, tc.want)
			}
		})
	}
}
