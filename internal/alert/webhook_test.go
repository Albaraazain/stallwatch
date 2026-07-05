package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Albaraazain/stallwatch/internal/config"
)

func TestWebhookSend(t *testing.T) {
	var gotBody map[string]string
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Internal-Token")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("payload is not JSON: %v", err)
		}
	}))
	defer srv.Close()

	w := NewWebhook(config.Sink{
		Name:    "ops",
		Type:    "webhook",
		URL:     srv.URL,
		Headers: map[string]string{"X-Internal-Token": "tok"},
		App:     "stallwatch",
	})
	err := w.Send(context.Background(), Event{
		Severity:    "critical",
		Title:       "queue: stalled",
		Body:        "increased by 0 over 1h",
		Fingerprint: "stallwatch:breach:queue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "tok" {
		t.Errorf("header not sent, got %q", gotToken)
	}
	want := map[string]string{
		"app":         "stallwatch",
		"severity":    "critical",
		"title":       "queue: stalled",
		"body":        "increased by 0 over 1h",
		"fingerprint": "stallwatch:breach:queue",
	}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("payload[%q] = %q, want %q", k, gotBody[k], v)
		}
	}
}

func TestWebhookNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	w := NewWebhook(config.Sink{Name: "ops", Type: "webhook", URL: srv.URL})
	err := w.Send(context.Background(), Event{Severity: "info", Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("err = %v, want status 403", err)
	}
}
