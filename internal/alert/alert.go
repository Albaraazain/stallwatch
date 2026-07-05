// Package alert delivers signal state transitions to external sinks.
package alert

import "context"

type Event struct {
	Severity    string
	Title       string
	Body        string
	Fingerprint string
}

type Sink interface {
	Name() string
	Send(ctx context.Context, e Event) error
}
