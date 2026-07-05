package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Albaraazain/stallwatch/internal/config"
)

// Webhook POSTs events as JSON: {app, severity, title, body, fingerprint}.
// The fingerprint lets receivers deduplicate repeats of the same condition.
type Webhook struct {
	name    string
	url     string
	headers map[string]string
	app     string
	client  *http.Client
}

func NewWebhook(spec config.Sink) *Webhook {
	return &Webhook{
		name:    spec.Name,
		url:     spec.URL,
		headers: spec.Headers,
		app:     spec.App,
		client:  &http.Client{},
	}
}

func (w *Webhook) Name() string { return w.name }

func (w *Webhook) Send(ctx context.Context, e Event) error {
	payload, err := json.Marshal(map[string]string{
		"app":         w.app,
		"severity":    e.Severity,
		"title":       e.Title,
		"body":        e.Body,
		"fingerprint": e.Fingerprint,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s: status %d", w.name, resp.StatusCode)
	}
	return nil
}
