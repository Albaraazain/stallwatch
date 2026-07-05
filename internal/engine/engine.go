// Package engine schedules collectors, evaluates expectations, and emits
// alerts on state transitions.
//
// Concurrency model: one goroutine per signal, and each signal's state is
// owned exclusively by its goroutine — there is no shared mutable state and
// therefore no locking. A semaphore bounds how many collections run at once.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Albaraazain/stallwatch/internal/alert"
	"github.com/Albaraazain/stallwatch/internal/collector"
	"github.com/Albaraazain/stallwatch/internal/config"
	"github.com/Albaraazain/stallwatch/internal/detect"
	"github.com/Albaraazain/stallwatch/internal/store"
)

const (
	collectTimeout = 30 * time.Second
	deliverTimeout = 10 * time.Second
	maxConcurrent  = 8
	pruneEvery     = time.Hour
)

type Engine struct {
	cfg        *config.Config
	store      *store.Store
	sinks      []alert.Sink
	collectors map[string]collector.Collector
	log        *slog.Logger
	sem        chan struct{}
	now        func() time.Time // injectable for tests
}

// signalState is owned exclusively by its signal's goroutine.
type signalState struct {
	status      detect.Status
	consecFails int
	failing     bool
	// pending holds the newest event that failed to deliver to every sink;
	// it is retried each tick until it lands. One failed POST must not
	// silently lose a page.
	pending *alert.Event
}

func New(cfg *config.Config, st *store.Store, sinks []alert.Sink, log *slog.Logger) (*Engine, error) {
	collectors := make(map[string]collector.Collector, len(cfg.Signals))
	for _, sig := range cfg.Signals {
		c, err := collector.New(sig.Collect)
		if err != nil {
			return nil, fmt.Errorf("signal %q: %w", sig.Name, err)
		}
		collectors[sig.Name] = c
	}
	return &Engine{
		cfg:        cfg,
		store:      st,
		sinks:      sinks,
		collectors: collectors,
		log:        log,
		sem:        make(chan struct{}, maxConcurrent),
		now:        time.Now,
	}, nil
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range e.cfg.Signals {
		sig := e.cfg.Signals[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.loop(ctx, sig)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.pruneLoop(ctx)
	}()
	wg.Wait()
}

func (e *Engine) loop(ctx context.Context, sig config.Signal) {
	st := &signalState{status: detect.Warmup}
	interval := sig.Interval.Std()
	// Spread the first round of collections so N signals don't fire at once.
	jitter := rand.N(min(interval, 10*time.Second))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		e.tick(ctx, sig, st)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick performs one collect → store → evaluate → alert cycle.
func (e *Engine) tick(ctx context.Context, sig config.Signal, st *signalState) {
	if st.pending != nil {
		e.deliver(ctx, sig, st, *st.pending)
	}
	value, err := e.collect(ctx, sig)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		e.handleCollectError(ctx, sig, st, err)
		return
	}
	if st.failing {
		st.failing = false
		e.deliver(ctx, sig, st, alert.Event{
			Severity:    "info",
			Title:       fmt.Sprintf("%s: collector recovered", sig.Name),
			Body:        fmt.Sprintf("collector for %q is reachable again (value %g)", sig.Name, value),
			Fingerprint: "stallwatch:collect:" + sig.Name,
		})
	}
	st.consecFails = 0

	now := e.now()
	if err := e.store.Append(sig.Name, now, value); err != nil {
		e.log.Error("store append failed", "signal", sig.Name, "err", err)
		return
	}
	window, err := e.store.Window(sig.Name, now.Add(-e.windowSpan(sig)))
	if err != nil {
		e.log.Error("store window failed", "signal", sig.Name, "err", err)
		return
	}

	res := detect.Evaluate(sig.Expect, window)
	prev := st.status
	st.status = res.Status
	e.log.Debug("evaluated",
		"signal", sig.Name, "value", value, "status", res.Status.String(), "reason", res.Reason)

	switch {
	case prev != detect.Breach && res.Status == detect.Breach:
		title := sig.Name + ": out of bounds"
		if res.Kind == detect.KindStall {
			title = sig.Name + ": stalled"
		}
		e.deliver(ctx, sig, st, alert.Event{
			Severity:    sig.Severity,
			Title:       title,
			Body:        res.Reason,
			Fingerprint: "stallwatch:breach:" + sig.Name,
		})
	case prev == detect.Breach && res.Status == detect.OK:
		e.deliver(ctx, sig, st, alert.Event{
			Severity:    "info",
			Title:       sig.Name + ": recovered",
			Body:        res.Reason,
			Fingerprint: "stallwatch:recovered:" + sig.Name,
		})
	}
}

// deliver emits an event and, if any sink refuses it, parks it for retry on
// the next tick. Newest event wins: if a fresher transition happens while one
// is parked, the fresher one reflects current reality and replaces it.
// A retry re-sends to every targeted sink — a sink that already accepted the
// event may see it twice, which the stable fingerprint lets receivers dedupe;
// a duplicate page beats a lost one.
func (e *Engine) deliver(ctx context.Context, sig config.Signal, st *signalState, ev alert.Event) {
	if e.emit(ctx, sig, ev) {
		st.pending = nil
		return
	}
	st.pending = &ev
}

func (e *Engine) collect(ctx context.Context, sig config.Signal) (float64, error) {
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	cctx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()
	return e.collectors[sig.Name].Collect(cctx)
}

// handleCollectError alerts once a collector fails fail_after times in a row:
// a monitor whose probes silently fail is the very disease it treats.
func (e *Engine) handleCollectError(ctx context.Context, sig config.Signal, st *signalState, err error) {
	st.consecFails++
	e.log.Warn("collect failed", "signal", sig.Name, "attempt", st.consecFails, "err", err)
	if st.failing || st.consecFails < sig.FailAfter {
		return
	}
	st.failing = true
	e.deliver(ctx, sig, st, alert.Event{
		Severity:    "error",
		Title:       fmt.Sprintf("%s: collector failing", sig.Name),
		Body:        fmt.Sprintf("%d consecutive collection failures; latest: %v", st.consecFails, err),
		Fingerprint: "stallwatch:collect:" + sig.Name,
	})
}

// emit sends an event to the signal's sinks and reports whether every
// targeted sink accepted it.
func (e *Engine) emit(ctx context.Context, sig config.Signal, ev alert.Event) bool {
	e.log.Info("alert",
		"signal", sig.Name, "severity", ev.Severity, "title", ev.Title, "body", ev.Body)
	ok := true
	for _, s := range e.sinks {
		if sig.Alert != "" && s.Name() != sig.Alert {
			continue
		}
		// WithoutCancel: an alert raised moments before shutdown should
		// still deliver; the timeout keeps shutdown bounded regardless.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliverTimeout)
		if err := s.Send(sctx, ev); err != nil {
			e.log.Error("alert delivery failed", "sink", s.Name(), "signal", sig.Name, "err", err)
			ok = false
		}
		cancel()
	}
	return ok
}

// windowSpan is how far back Evaluate needs to look for this signal. Stall
// rules need one interval of slack beyond `over` so a baseline sample at
// least `over` old is guaranteed to be inside the window.
func (e *Engine) windowSpan(sig config.Signal) time.Duration {
	if sig.Expect.IncreaseBy != nil {
		return sig.Expect.Over.Std() + sig.Interval.Std()
	}
	// Bounds-only rules only need the latest sample; a few intervals of
	// context is plenty.
	return 3 * sig.Interval.Std()
}

func (e *Engine) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.store.Prune(e.now().Add(-e.cfg.Defaults.Retention.Std())); err != nil {
				e.log.Error("prune failed", "err", err)
			}
		}
	}
}
