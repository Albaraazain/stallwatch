package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Albaraazain/stallwatch/internal/config"
)

func TestHTTPJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"pending":{"count":42},"queues":[{"depth":"3.5"}],"healthy":true}`))
	}))
	defer srv.Close()

	spec := func(path string) config.CollectorSpec {
		return config.CollectorSpec{
			Type: "http_json", URL: srv.URL, Path: path,
			Headers: map[string]string{"X-Token": "tok"},
		}
	}

	tests := []struct {
		name    string
		path    string
		want    float64
		wantErr string
	}{
		{name: "nested key", path: "pending.count", want: 42},
		{name: "array index and numeric string", path: "queues.0.depth", want: 3.5},
		{name: "bool maps to 1", path: "healthy", want: 1},
		{name: "missing key", path: "pending.missing", wantErr: "not found"},
		{name: "non-numeric value", path: "pending", wantErr: "want a number"},
		{name: "index out of range", path: "queues.9.depth", wantErr: "out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(spec(tt.path))
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Collect(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %g, want %g", got, tt.want)
			}
		})
	}
}

func TestHTTPJSONWholeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`7`))
	}))
	defer srv.Close()

	c, _ := New(config.CollectorSpec{Type: "http_json", URL: srv.URL})
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("got %g, want 7", got)
	}
}

func TestHTTPJSONBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New(config.CollectorSpec{Type: "http_json", URL: srv.URL})
	if _, err := c.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v, want status 500", err)
	}
}

func TestExec(t *testing.T) {
	tests := []struct {
		name    string
		cmd     []string
		want    float64
		wantErr string
	}{
		{name: "numeric stdout", cmd: []string{"sh", "-c", "echo '  42 '"}, want: 42},
		{name: "float stdout", cmd: []string{"sh", "-c", "echo 3.14"}, want: 3.14},
		{name: "non-numeric stdout", cmd: []string{"sh", "-c", "echo pending"}, wantErr: "not numeric"},
		{name: "failure surfaces stderr", cmd: []string{"sh", "-c", "echo boom >&2; exit 3"}, wantErr: "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(config.CollectorSpec{Type: "exec", Cmd: tt.cmd})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Collect(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %g, want %g", got, tt.want)
			}
		})
	}
}

func TestExecRespectsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c, _ := New(config.CollectorSpec{Type: "exec", Cmd: []string{"sh", "-c", "sleep 5; echo 1"}})
	start := time.Now()
	if _, err := c.Collect(ctx); err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("collect did not respect context timeout, took %s", elapsed)
	}
}
