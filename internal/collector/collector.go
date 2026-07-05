// Package collector turns a config spec into something that samples one
// numeric value from the outside world. Keeping every signal a float64
// time series is what keeps the rest of stallwatch simple.
package collector

import (
	"context"
	"fmt"

	"github.com/Albaraazain/stallwatch/internal/config"
)

// Collector samples one numeric value. Implementations must honor ctx's
// deadline — a hung probe must not outlive it.
type Collector interface {
	Collect(ctx context.Context) (float64, error)
}

// New builds the collector described by spec.
func New(spec config.CollectorSpec) (Collector, error) {
	switch spec.Type {
	case "http_json":
		return newHTTPJSON(spec), nil
	case "exec":
		return &execCollector{cmd: spec.Cmd}, nil
	default:
		return nil, fmt.Errorf("unknown collector type %q", spec.Type)
	}
}
