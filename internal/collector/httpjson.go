package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Albaraazain/stallwatch/internal/config"
)

const maxBodyBytes = 1 << 20 // a metrics endpoint should not need more than 1 MiB

// httpJSON GETs a URL and extracts a numeric value at a dot path, e.g.
// "pending.count" or "queues.0.depth" (numeric segments index arrays).
type httpJSON struct {
	url     string
	path    []string
	headers map[string]string
	client  *http.Client
}

func newHTTPJSON(spec config.CollectorSpec) *httpJSON {
	var path []string
	if spec.Path != "" {
		path = strings.Split(spec.Path, ".")
	}
	return &httpJSON{url: spec.URL, path: path, headers: spec.Headers, client: &http.Client{}}
}

func (c *httpJSON) Collect(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, fmt.Errorf("GET %s: status %d", c.url, resp.StatusCode)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, fmt.Errorf("GET %s: %w", c.url, err)
	}
	val, err := walk(doc, c.path)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", c.url, err)
	}
	return toFloat(val, strings.Join(c.path, "."))
}

func walk(doc any, path []string) (any, error) {
	cur := doc
	for i, seg := range path {
		at := strings.Join(path[:i+1], ".")
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, fmt.Errorf("path %q: key %q not found", at, seg)
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return nil, fmt.Errorf("path %q: %q indexes an array but is not a number", at, seg)
			}
			if idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("path %q: index %d out of range (len %d)", at, idx, len(node))
			}
			cur = node[idx]
		default:
			return nil, fmt.Errorf("path %q: cannot descend into %T", at, cur)
		}
	}
	return cur, nil
}

func toFloat(v any, path string) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, fmt.Errorf("value %q at %q is not numeric", n, path)
		}
		return f, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("value at %q is %T, want a number", path, v)
	}
}
