// Package config loads and validates the stallwatch YAML configuration.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var validSeverities = map[string]bool{
	"critical": true,
	"error":    true,
	"warn":     true,
	"info":     true,
}

// Duration wraps time.Duration so YAML values like "90s" or "1h" parse;
// yaml.v3 has no native support for duration strings.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"90s\" or \"1h\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Defaults Defaults `yaml:"defaults"`
	Alerts   []Sink   `yaml:"alerts"`
	Signals  []Signal `yaml:"signals"`
}

type Defaults struct {
	Interval  Duration `yaml:"interval"`
	Retention Duration `yaml:"retention"`
	FailAfter int      `yaml:"fail_after"`
}

// Sink is an alert destination: a JSON POST carrying
// {app, severity, title, body, fingerprint}.
type Sink struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	App     string            `yaml:"app"`
}

// Signal is one progress metric to watch: how to collect it, how often,
// and what to expect of it.
type Signal struct {
	Name      string        `yaml:"name"`
	Collect   CollectorSpec `yaml:"collect"`
	Interval  Duration      `yaml:"interval"`
	Expect    Expectation   `yaml:"expect"`
	Severity  string        `yaml:"severity"`
	Alert     string        `yaml:"alert"`
	FailAfter int           `yaml:"fail_after"`
}

type CollectorSpec struct {
	Type    string            `yaml:"type"`
	URL     string            `yaml:"url"`
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
	Cmd     []string          `yaml:"cmd"`
}

// Expectation declares what a healthy signal looks like. increase_by/over
// detects stalls in counters that must keep moving; min/max bound gauges.
type Expectation struct {
	IncreaseBy *float64 `yaml:"increase_by"`
	Over       Duration `yaml:"over"`
	Min        *float64 `yaml:"min"`
	Max        *float64 `yaml:"max"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	// Env expansion walks the parsed node tree rather than the raw bytes so
	// that only values are expanded — a comment mentioning ${SOME_VAR} must
	// not trip the strict unset-variable check.
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind == 0 {
		return nil, fmt.Errorf("config is empty")
	}
	if err := expandNode(&doc); err != nil {
		return nil, err
	}
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(expanded)))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// expandNode expands env references in string scalars. A plain (unquoted)
// scalar that was only stringy because of the ${...} syntax gets its tag
// cleared so the expanded value re-resolves — `max: ${LIMIT}` becomes a
// number; a quoted "${TOKEN}" stays a string.
func expandNode(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.Contains(n.Value, "$") {
		expanded, err := expandEnv(n.Value)
		if err != nil {
			return err
		}
		if expanded != n.Value {
			n.Value = expanded
			if n.Style == 0 {
				n.Tag = ""
			}
		}
	}
	for _, child := range n.Content {
		if err := expandNode(child); err != nil {
			return err
		}
	}
	return nil
}

// expandEnv substitutes ${VAR} / $VAR references and fails loudly on any
// unset variable, so a missing token never silently becomes "".
func expandEnv(s string) (string, error) {
	var missing []string
	out := os.Expand(s, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (c *Config) applyDefaults() {
	if c.Defaults.Interval <= 0 {
		c.Defaults.Interval = Duration(time.Minute)
	}
	if c.Defaults.Retention <= 0 {
		c.Defaults.Retention = Duration(7 * 24 * time.Hour)
	}
	if c.Defaults.FailAfter <= 0 {
		c.Defaults.FailAfter = 3
	}
	for i := range c.Signals {
		s := &c.Signals[i]
		if s.Interval <= 0 {
			s.Interval = c.Defaults.Interval
		}
		if s.Severity == "" {
			s.Severity = "error"
		}
		if s.FailAfter <= 0 {
			s.FailAfter = c.Defaults.FailAfter
		}
		// Retention must always cover the widest stall window, including the
		// one-interval slack the engine adds when fetching baselines.
		if s.Expect.Over > 0 && c.Defaults.Retention < 2*(s.Expect.Over+s.Interval) {
			c.Defaults.Retention = 2 * (s.Expect.Over + s.Interval)
		}
	}
	for i := range c.Alerts {
		if c.Alerts[i].App == "" {
			c.Alerts[i].App = "stallwatch"
		}
	}
}

func (c *Config) validate() error {
	if len(c.Signals) == 0 {
		return fmt.Errorf("config declares no signals")
	}
	sinkNames := map[string]bool{}
	for i, a := range c.Alerts {
		if a.Name == "" {
			return fmt.Errorf("alerts[%d]: name is required", i)
		}
		if sinkNames[a.Name] {
			return fmt.Errorf("alerts[%d]: duplicate sink name %q", i, a.Name)
		}
		sinkNames[a.Name] = true
		if a.Type != "webhook" {
			return fmt.Errorf("alert %q: unknown type %q (want webhook)", a.Name, a.Type)
		}
		if a.URL == "" {
			return fmt.Errorf("alert %q: url is required", a.Name)
		}
	}
	sigNames := map[string]bool{}
	for i, s := range c.Signals {
		if s.Name == "" {
			return fmt.Errorf("signals[%d]: name is required", i)
		}
		if sigNames[s.Name] {
			return fmt.Errorf("signals[%d]: duplicate signal name %q", i, s.Name)
		}
		sigNames[s.Name] = true
		if !validSeverities[s.Severity] {
			return fmt.Errorf("signal %q: invalid severity %q (want critical|error|warn|info)", s.Name, s.Severity)
		}
		if s.Alert != "" && !sinkNames[s.Alert] {
			return fmt.Errorf("signal %q: alert references unknown sink %q", s.Name, s.Alert)
		}
		if err := s.Collect.validate(); err != nil {
			return fmt.Errorf("signal %q: %w", s.Name, err)
		}
		if err := s.Expect.validate(); err != nil {
			return fmt.Errorf("signal %q: %w", s.Name, err)
		}
	}
	return nil
}

func (s CollectorSpec) validate() error {
	switch s.Type {
	case "http_json":
		if s.URL == "" {
			return fmt.Errorf("http_json collector: url is required")
		}
	case "exec":
		if len(s.Cmd) == 0 {
			return fmt.Errorf("exec collector: cmd is required")
		}
	case "":
		return fmt.Errorf("collector type is required")
	default:
		return fmt.Errorf("unknown collector type %q (want http_json|exec)", s.Type)
	}
	return nil
}

func (e Expectation) validate() error {
	if e.IncreaseBy == nil && e.Min == nil && e.Max == nil {
		return fmt.Errorf("expect: at least one of increase_by, min, max is required")
	}
	if e.IncreaseBy != nil && e.Over <= 0 {
		return fmt.Errorf("expect: increase_by requires over > 0")
	}
	if e.IncreaseBy == nil && e.Over > 0 {
		return fmt.Errorf("expect: over is only meaningful with increase_by")
	}
	if e.Min != nil && e.Max != nil && *e.Min > *e.Max {
		return fmt.Errorf("expect: min (%v) > max (%v)", *e.Min, *e.Max)
	}
	return nil
}
