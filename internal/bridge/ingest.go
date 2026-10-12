package bridge

import (
	"net/http"
	"path"
	"strings"

	"github.com/jiaweisky/harbor/internal/blob"
)

// IngestMount is the path segment under an agent's base path that accepts file
// uploads for the agent's own ingest endpoint.
const IngestMount = "storage/in/"

// IngestHandler accepts a PUT and stores its body through the data plane, so an
// agent can advertise its own endpoint as an upload target. The response carries
// the resulting reference in the X-Harbor-Reference header.
func IngestHandler(store blob.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if store == nil {
			http.Error(w, "storage disabled", http.StatusNotFound)
			return
		}
		name := path.Base(strings.TrimRight(r.URL.Path, "/"))
		uri, err := store.Put(r.Context(), name, r.Header.Get("Content-Type"), r.Body, r.ContentLength)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("X-Harbor-Reference", uri)
		w.WriteHeader(http.StatusNoContent)
	})
}
