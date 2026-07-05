// Package alert delivers signal state transitions to external sinks.
package alert

import "context"

// Event is one alert: a state transition worth telling a human about.
// Fingerprint is stable per signal+condition so receivers can deduplicate.
type Event struct {
	Severity    string
	Title       string
	Body        string
	Fingerprint string
}

// Sink is an alert destination. Send must be safe to call concurrently and
// should honor ctx's deadline.
type Sink interface {
	Name() string
	Send(ctx context.Context, e Event) error
}
