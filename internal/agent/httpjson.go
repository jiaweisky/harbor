package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PostJSON marshals body as JSON and POSTs it to url, decoding a 2xx JSON
// response into out when out is non-nil. Entries in header are added to the
// request. label prefixes error messages (for example "codebuddy server").
func PostJSON(ctx context.Context, client *http.Client, url, label string, header http.Header, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}

	req.Header.Set("content-type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s: %s", label, resp.Status, strings.TrimSpace(string(data)))
	}

	if out != nil {
		return json.Unmarshal(data, out)
	}

	return nil
}
