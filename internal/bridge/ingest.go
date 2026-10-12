package bridge

import (
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/jiaweisky/harbor/internal/blob"
)

// IngestMount is the path segment under an agent's base path that accepts file
// uploads for the agent's own ingest endpoint.
const IngestMount = "storage/in/"

// IngestHandler backs an agent's own ingest endpoint: a PUT stores the body
// through the data plane (so an agent can advertise this endpoint as an upload
// target), and a GET streams a stored object back, honoring Range requests.
func IngestHandler(store blob.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			http.Error(w, "storage disabled", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			ingestPut(store, w, r)
		case http.MethodGet, http.MethodHead:
			ingestGet(store, w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// ingestPut stores the request body and reports the resulting reference.
func ingestPut(store blob.Store, w http.ResponseWriter, r *http.Request) {
	name := path.Base(strings.TrimRight(r.URL.Path, "/"))
	uri, err := store.Put(r.Context(), name, r.Header.Get("Content-Type"), r.Body, r.ContentLength)
	if err != nil {
		http.Error(w, "store failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("X-Harbor-Reference", uri)
	w.WriteHeader(http.StatusNoContent)
}

// ingestGet streams a stored object back. The path after the ingest mount is the
// store-relative key; the store maps it to the object and serves it with Range.
func ingestGet(store blob.Store, w http.ResponseWriter, r *http.Request) {
	resolver, ok := store.(blob.KeyResolver)
	if !ok {
		http.Error(w, "storage does not serve reads", http.StatusNotFound)
		return
	}
	key := ingestKey(r.URL.Path)
	uri, ok := resolver.Reference(key)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := blob.ServeContent(w, r, store, uri, path.Base(key)); err != nil {
		if errors.Is(err, blob.ErrUnsupportedURI) || errors.Is(err, blob.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "read failed", http.StatusBadGateway)
	}
}

// ingestKey returns the store-relative key in a request path under the ingest
// mount.
func ingestKey(urlPath string) string {
	if i := strings.Index(urlPath, IngestMount); i >= 0 {
		return urlPath[i+len(IngestMount):]
	}
	return strings.TrimPrefix(urlPath, "/")
}
