package detect

import (
	"strings"
	"testing"
	"time"

	"github.com/Albaraazain/stallwatch/internal/config"
	"github.com/Albaraazain/stallwatch/internal/store"
)

func f(v float64) *float64 { return &v }

// win builds a window from (offset-minutes, value) pairs, oldest first.
func win(pairs ...float64) []store.Sample {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var out []store.Sample
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, store.Sample{
			TS:    base.Add(time.Duration(pairs[i]) * time.Minute),
			Value: pairs[i+1],
		})
	}
	return out
}

func TestEvaluate(t *testing.T) {
	stall := config.Expectation{IncreaseBy: f(5), Over: config.Duration(time.Hour)}

	tests := []struct {
		name       string
		exp        config.Expectation
		window     []store.Sample
		wantStatus Status
		wantKind   Kind
		wantReason string // substring
	}{
		{
			name:       "empty window warms up",
			exp:        stall,
			window:     nil,
			wantStatus: Warmup,
			wantReason: "no samples",
		},
		{
			name:       "no sample old enough warms up",
			exp:        stall,
			window:     win(0, 100, 30, 110),
			wantStatus: Warmup,
			wantReason: "baseline",
		},
		{
			name: "jittered samples do not stick in warmup",
			exp:  stall,
			// Oldest sample is 70m old: no sample sits exactly at the 60m
			// cutoff, but the 70m-old one qualifies as baseline.
			window:     win(0, 100, 35, 100, 70, 100),
			wantStatus: Breach,
			wantKind:   KindStall,
		},
		{
			name:       "flat counter stalls",
			exp:        stall,
			window:     win(0, 100, 30, 100, 60, 100),
			wantStatus: Breach,
			wantKind:   KindStall,
			wantReason: "increased by 0",
		},
		{
			name:       "insufficient increase stalls",
			exp:        stall,
			window:     win(0, 100, 60, 103),
			wantStatus: Breach,
			wantKind:   KindStall,
			wantReason: "expected >= 5",
		},
		{
			name:       "healthy increase is ok",
			exp:        stall,
			window:     win(0, 100, 30, 104, 60, 110),
			wantStatus: OK,
			wantReason: "increased by 10",
		},
		{
			name: "baseline is newest sample old enough",
			exp:  stall,
			// The 100->104 movement is older than the window's edge; only
			// the last hour (104 -> 106) counts, and it's not enough.
			window:     win(0, 100, 5, 104, 65, 106),
			wantStatus: Breach,
			wantKind:   KindStall,
			wantReason: "increased by 2",
		},
		{
			name:       "counter reset is activity not stall",
			exp:        stall,
			window:     win(0, 5000, 60, 12),
			wantStatus: OK,
			wantReason: "reset",
		},
		{
			name:       "below min breaches",
			exp:        config.Expectation{Min: f(10)},
			window:     win(0, 3),
			wantStatus: Breach,
			wantKind:   KindBounds,
			wantReason: "below min",
		},
		{
			name:       "above max breaches",
			exp:        config.Expectation{Max: f(500)},
			window:     win(0, 900),
			wantStatus: Breach,
			wantKind:   KindBounds,
			wantReason: "above max",
		},
		{
			name:       "within bounds is ok",
			exp:        config.Expectation{Min: f(1), Max: f(500)},
			window:     win(0, 42),
			wantStatus: OK,
		},
		{
			name:       "bounds fire even during stall warmup",
			exp:        config.Expectation{IncreaseBy: f(5), Over: config.Duration(time.Hour), Max: f(500)},
			window:     win(0, 900),
			wantStatus: Breach,
			wantKind:   KindBounds,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.exp, tt.window)
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s (reason: %s)", got.Status, tt.wantStatus, got.Reason)
			}
			if tt.wantStatus == Breach && got.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("reason %q does not contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}
