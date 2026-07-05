// Package collector turns a config spec into something that samples one
// numeric value from the outside world. Keeping every signal a float64
// time series is what keeps the rest of stallwatch simple.
package collector

import (
	"context"
	"fmt"

	"github.com/Albaraazain/stallwatch/internal/config"
)

type Collector interface {
	Collect(ctx context.Context) (float64, error)
}

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
