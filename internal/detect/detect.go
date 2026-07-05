// Package detect evaluates expectation rules against a sample window.
// It is pure — no I/O, no clocks — which makes every edge case
// table-testable.
package detect

import (
	"fmt"
	"time"

	"github.com/Albaraazain/stallwatch/internal/config"
	"github.com/Albaraazain/stallwatch/internal/store"
)

type Status int

const (
	Warmup Status = iota
	OK
	Breach
)

func (s Status) String() string {
	switch s {
	case Warmup:
		return "warmup"
	case OK:
		return "ok"
	case Breach:
		return "breach"
	default:
		return "unknown"
	}
}

type Kind string

const (
	KindStall  Kind = "stall"
	KindBounds Kind = "bounds"
)

type Result struct {
	Status Status
	Kind   Kind // set when Status is Breach
	Reason string
}

// Evaluate checks an expectation against a sample window. The window must be
// sorted oldest-first and must extend further back than the expectation's
// `over` duration (the engine fetches `over` plus one interval of slack), so
// that a baseline sample at least `over` old can exist.
//
// Semantics:
//   - Bounds (min/max) apply to the latest sample and fire immediately,
//     even during warmup.
//   - A stall rule (increase_by/over) compares the latest sample against the
//     newest sample that is at least `over` older. If no sample is that old
//     yet, the signal is still warming up. Anchoring on "at least `over` old"
//     (rather than the window edge) is what makes the rule immune to sample
//     jitter — a naive span check would sit in warmup forever.
//   - A negative delta means the counter reset (deploy, truncation). A reset
//     is evidence of activity, not a stall, so it evaluates as OK.
func Evaluate(exp config.Expectation, window []store.Sample) Result {
	if len(window) == 0 {
		return Result{Status: Warmup, Reason: "no samples yet"}
	}
	latest := window[len(window)-1]

	if exp.Min != nil && latest.Value < *exp.Min {
		return Result{Status: Breach, Kind: KindBounds,
			Reason: fmt.Sprintf("value %g below min %g", latest.Value, *exp.Min)}
	}
	if exp.Max != nil && latest.Value > *exp.Max {
		return Result{Status: Breach, Kind: KindBounds,
			Reason: fmt.Sprintf("value %g above max %g", latest.Value, *exp.Max)}
	}
	if exp.IncreaseBy == nil {
		return Result{Status: OK, Reason: fmt.Sprintf("value %g within bounds", latest.Value)}
	}

	over := exp.Over.Std()
	baseline, ok := baselineSample(window, latest.TS.Add(-over))
	if !ok {
		span := latest.TS.Sub(window[0].TS)
		return Result{Status: Warmup,
			Reason: fmt.Sprintf("collecting baseline (%s of %s)", span.Round(time.Second), over)}
	}
	delta := latest.Value - baseline.Value
	if delta < 0 {
		return Result{Status: OK,
			Reason: fmt.Sprintf("counter reset detected (%g -> %g)", baseline.Value, latest.Value)}
	}
	if delta < *exp.IncreaseBy {
		return Result{Status: Breach, Kind: KindStall,
			Reason: fmt.Sprintf("increased by %g over %s, expected >= %g (last value %g)",
				delta, over, *exp.IncreaseBy, latest.Value)}
	}
	return Result{Status: OK, Reason: fmt.Sprintf("increased by %g over %s", delta, over)}
}

// baselineSample returns the newest sample with TS <= cutoff, i.e. the most
// recent value that is old enough to serve as the stall baseline.
func baselineSample(window []store.Sample, cutoff time.Time) (store.Sample, bool) {
	for i := len(window) - 1; i >= 0; i-- {
		if !window[i].TS.After(cutoff) {
			return window[i], true
		}
	}
	return store.Sample{}, false
}
