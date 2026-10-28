package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// versionInterceptor rejects requests that do not target the only A2A protocol
// version Harbor supports.
type versionInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

// Before implements a2asrv.CallInterceptor.
func (versionInterceptor) Before(
	ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request,
) (context.Context, any, error) {
	versions, ok := callCtx.ServiceParams().Get(a2a.SvcParamVersion)
	if !ok || len(versions) != 1 || strings.TrimSpace(versions[0]) != string(a2a.Version) {
		return ctx, nil, a2a.ErrVersionNotSupported
	}

	return ctx, nil, nil
}

// a2aVersionQuery lets a client pass A2A-Version as a query parameter instead of
// a header, as the A2A specification permits. The SDK transport reads service
// parameters only from headers, so copy the value across before it runs.
func a2aVersionQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(a2a.SvcParamVersion) != "" {
			next.ServeHTTP(w, r)
			return
		}

		for key, values := range r.URL.Query() {
			if strings.EqualFold(key, a2a.SvcParamVersion) && len(values) > 0 {
				r.Header.Set(a2a.SvcParamVersion, values[0])
				break
			}
		}

		next.ServeHTTP(w, r)
	})
}
