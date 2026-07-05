package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Albaraazain/stallwatch/internal/alert"
	"github.com/Albaraazain/stallwatch/internal/config"
	"github.com/Albaraazain/stallwatch/internal/store"
)

type response struct {
	v   float64
	err error
}

// scriptedCollector returns queued responses; the test owns the sequencing.
type scriptedCollector struct {
	responses []response
	i         int
}

func (c *scriptedCollector) Collect(context.Context) (float64, error) {
	if c.i >= len(c.responses) {
		return 0, errors.New("scripted collector exhausted")
	}
	r := c.responses[c.i]
	c.i++
	return r.v, r.err
}

type recordingSink struct {
	events []alert.Event
}

func (s *recordingSink) Name() string { return "rec" }
func (s *recordingSink) Send(_ context.Context, e alert.Event) error {
	s.events = append(s.events, e)
	return nil
}

func f(v float64) *float64 { return &v }

func newTestEngine(t *testing.T, sig config.Signal, responses []response) (*Engine, *recordingSink, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Defaults: config.Defaults{Retention: config.Duration(24 * time.Hour)},
		Signals:  []config.Signal{sig},
	}
	sink := &recordingSink{}
	eng, err := New(cfg, st, []alert.Sink{sink}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	eng.collectors[sig.Name] = &scriptedCollector{responses: responses}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	eng.now = func() time.Time { return now }
	return eng, sink, &now
}

func stallSignal() config.Signal {
	return config.Signal{
		Name:      "queue-progress",
		Collect:   config.CollectorSpec{Type: "exec", Cmd: []string{"true"}},
		Interval:  config.Duration(5 * time.Minute),
		Expect:    config.Expectation{IncreaseBy: f(1), Over: config.Duration(time.Hour)},
		Severity:  "critical",
		FailAfter: 2,
	}
}

func TestTickStallAndRecovery(t *testing.T) {
	eng, sink, now := newTestEngine(t, stallSignal(), []response{
		{v: 100}, // t0: first sample, warming up
		{v: 100}, // t0+1h: flat for an hour → stall
		{v: 105}, // t0+2h: moving again → recovery
	})
	sig := eng.cfg.Signals[0]
	st := &signalState{}
	ctx := context.Background()

	eng.tick(ctx, sig, st)
	if len(sink.events) != 0 {
		t.Fatalf("warmup should not alert, got %+v", sink.events)
	}

	*now = now.Add(time.Hour)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 1 {
		t.Fatalf("flat counter should alert once, got %d events", len(sink.events))
	}
	breach := sink.events[0]
	if breach.Severity != "critical" || !strings.Contains(breach.Title, "stalled") {
		t.Fatalf("breach event: %+v", breach)
	}

	// Still breached next tick: no repeat alert (transition-only alerting).
	// Rewind the scripted collector to replay the flat value.
	eng.collectors[sig.Name] = &scriptedCollector{responses: []response{{v: 100}, {v: 105}}}
	*now = now.Add(5 * time.Minute)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 1 {
		t.Fatalf("ongoing breach must not re-alert, got %d events", len(sink.events))
	}

	*now = now.Add(time.Hour)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 2 {
		t.Fatalf("recovery should alert, got %d events", len(sink.events))
	}
	rec := sink.events[1]
	if rec.Severity != "info" || !strings.Contains(rec.Title, "recovered") {
		t.Fatalf("recovery event: %+v", rec)
	}
}

func TestTickCollectorFailureAlerts(t *testing.T) {
	boom := errors.New("connection refused")
	eng, sink, now := newTestEngine(t, stallSignal(), []response{
		{err: boom}, // strike one: below fail_after, no alert
		{err: boom}, // strike two: alert
		{err: boom}, // still failing: no repeat alert
		{v: 100},    // back: recovery alert
	})
	sig := eng.cfg.Signals[0]
	st := &signalState{}
	ctx := context.Background()

	eng.tick(ctx, sig, st)
	if len(sink.events) != 0 {
		t.Fatalf("single failure should not alert, got %+v", sink.events)
	}

	*now = now.Add(5 * time.Minute)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 1 || !strings.Contains(sink.events[0].Title, "collector failing") {
		t.Fatalf("after fail_after failures: %+v", sink.events)
	}

	*now = now.Add(5 * time.Minute)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 1 {
		t.Fatalf("ongoing failure must not re-alert, got %d events", len(sink.events))
	}

	*now = now.Add(5 * time.Minute)
	eng.tick(ctx, sig, st)
	if len(sink.events) != 2 || !strings.Contains(sink.events[1].Title, "collector recovered") {
		t.Fatalf("collector recovery: %+v", sink.events)
	}
	if st.consecFails != 0 {
		t.Fatalf("consecFails not reset: %d", st.consecFails)
	}
}

// flakySink refuses the first n sends, then accepts.
type flakySink struct {
	recordingSink
	failures int
}

func (s *flakySink) Send(ctx context.Context, e alert.Event) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("notifier unreachable")
	}
	return s.recordingSink.Send(ctx, e)
}

func TestUndeliveredAlertRetriesNextTick(t *testing.T) {
	sig := stallSignal()
	sig.Expect = config.Expectation{Max: f(10)} // breaches immediately

	eng, _, now := newTestEngine(t, sig, []response{{v: 999}, {v: 999}})
	sink := &flakySink{failures: 1}
	eng.sinks = []alert.Sink{sink}
	st := &signalState{}
	ctx := context.Background()

	eng.tick(ctx, sig, st) // breach fires, delivery fails
	if len(sink.events) != 0 {
		t.Fatalf("delivery should have failed, got %+v", sink.events)
	}
	if st.pending == nil {
		t.Fatal("undelivered event was not parked for retry")
	}

	*now = now.Add(5 * time.Minute)
	eng.tick(ctx, sig, st) // still breached (no new transition); parked event retried
	if len(sink.events) != 1 || !strings.Contains(sink.events[0].Title, "out of bounds") {
		t.Fatalf("parked event not delivered on retry: %+v", sink.events)
	}
	if st.pending != nil {
		t.Fatal("pending not cleared after successful delivery")
	}
}

func TestSinkRouting(t *testing.T) {
	sig := stallSignal()
	sig.Expect = config.Expectation{Max: f(10)} // bounds breach fires immediately
	sig.Alert = "other"                         // routed away from our sink

	eng, sink, _ := newTestEngine(t, sig, []response{{v: 999}})
	st := &signalState{}
	eng.tick(context.Background(), eng.cfg.Signals[0], st)

	if st.status.String() != "breach" {
		t.Fatalf("expected breach state, got %s", st.status)
	}
	if len(sink.events) != 0 {
		t.Fatalf("event should have been routed to sink %q, got %+v", sig.Alert, sink.events)
	}
}
