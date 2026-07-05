package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for i, v := range []float64{10, 20, 30} {
		if err := st.Append("a", base.Add(time.Duration(i)*time.Minute), v); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Append("b", base, 99); err != nil {
		t.Fatal(err)
	}

	window, err := st.Window("a", base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 2 {
		t.Fatalf("window len = %d, want 2 (filtered by from and signal)", len(window))
	}
	if window[0].Value != 20 || window[1].Value != 30 {
		t.Fatalf("window not ordered oldest-first: %+v", window)
	}
	if !window[0].TS.Equal(base.Add(time.Minute)) {
		t.Fatalf("timestamp round trip: got %s", window[0].TS)
	}
}

func TestStorePrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.Append("a", base, 1)
	st.Append("a", base.Add(time.Hour), 2)
	if err := st.Prune(base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	window, err := st.Window("a", base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 1 || window[0].Value != 2 {
		t.Fatalf("after prune: %+v", window)
	}
}

// Persistence across reopen is a feature, not an accident: stall baselines
// must survive restarts so stallwatch isn't blind for `over` after a deploy.
func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st.Append("a", base, 123)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	window, err := st2.Window("a", base.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(window) != 1 || window[0].Value != 123 {
		t.Fatalf("after reopen: %+v", window)
	}
}
