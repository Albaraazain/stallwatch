package config

import (
	"strings"
	"testing"
	"time"
)

const fullConfig = `
defaults:
  interval: 30s
  retention: 48h
  fail_after: 5

alerts:
  - name: ops
    type: webhook
    url: http://notifier:4000/notify
    headers:
      X-Internal-Token: ${TEST_NOTIFIER_TOKEN}

signals:
  - name: queue-progress
    collect:
      type: exec
      cmd: ["psql", "-tAc", "SELECT count(*) FROM jobs_done"]
    interval: 5m
    expect:
      increase_by: 10
      over: 1h
    severity: critical
    alert: ops

  - name: queue-depth
    collect:
      type: http_json
      url: http://localhost:8080/metrics
      path: pending.count
    expect:
      max: 500
`

func TestParseFullConfig(t *testing.T) {
	t.Setenv("TEST_NOTIFIER_TOKEN", "secret-token")
	cfg, err := Parse([]byte(fullConfig))
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.Alerts[0].Headers["X-Internal-Token"]; got != "secret-token" {
		t.Errorf("env expansion: got %q", got)
	}
	if cfg.Alerts[0].App != "stallwatch" {
		t.Errorf("sink app default: got %q", cfg.Alerts[0].App)
	}

	prog := cfg.Signals[0]
	if prog.Interval.Std() != 5*time.Minute {
		t.Errorf("interval: got %s", prog.Interval.Std())
	}
	if prog.Expect.Over.Std() != time.Hour || *prog.Expect.IncreaseBy != 10 {
		t.Errorf("expect: got %+v", prog.Expect)
	}
	if prog.FailAfter != 5 {
		t.Errorf("fail_after default from defaults: got %d", prog.FailAfter)
	}

	depth := cfg.Signals[1]
	if depth.Interval.Std() != 30*time.Second {
		t.Errorf("default interval: got %s", depth.Interval.Std())
	}
	if depth.Severity != "error" {
		t.Errorf("default severity: got %q", depth.Severity)
	}
}

func TestCommentsAreNotExpanded(t *testing.T) {
	cfg, err := Parse([]byte(`
# Values support ${TOTALLY_UNSET_DOC_VAR} expansion — this comment must not
# trip the strict unset-variable check.
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}}
`))
	if err != nil {
		t.Fatalf("comment mentioning an unset var must parse: %v", err)
	}
	if len(cfg.Signals) != 1 {
		t.Fatalf("signals: %+v", cfg.Signals)
	}
}

func TestEnvExpansionInNumericField(t *testing.T) {
	t.Setenv("TEST_MAX_DEPTH", "500")
	cfg, err := Parse([]byte(`
signals:
  - name: s
    collect: {type: exec, cmd: ["true"]}
    expect:
      max: ${TEST_MAX_DEPTH}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := *cfg.Signals[0].Expect.Max; got != 500 {
		t.Fatalf("max = %g, want 500", got)
	}
}

func TestRetentionCoversStallWindows(t *testing.T) {
	cfg, err := Parse([]byte(`
signals:
  - name: slow-counter
    collect: {type: exec, cmd: ["true"]}
    interval: 1h
    expect: {increase_by: 1, over: 200h}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := 2 * (200*time.Hour + time.Hour)
	if cfg.Defaults.Retention.Std() != want {
		t.Errorf("retention auto-raise: got %s, want %s", cfg.Defaults.Retention.Std(), want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "no signals",
			yaml:    `defaults: {interval: 60s}`,
			wantErr: "no signals",
		},
		{
			name: "unknown field is rejected",
			yaml: `
signals:
  - name: s
    colect: {type: exec, cmd: ["true"]}
    expect: {min: 1}
`,
			wantErr: "field colect not found",
		},
		{
			name: "duplicate signal name",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}}
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}}
`,
			wantErr: "duplicate signal",
		},
		{
			name: "expect requires a rule",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {}}
`,
			wantErr: "at least one of",
		},
		{
			name: "increase_by requires over",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {increase_by: 1}}
`,
			wantErr: "requires over",
		},
		{
			name: "over without increase_by",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1, over: 1h}}
`,
			wantErr: "only meaningful",
		},
		{
			name: "min greater than max",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 10, max: 5}}
`,
			wantErr: "min (10) > max (5)",
		},
		{
			name: "unknown collector type",
			yaml: `
signals:
  - {name: s, collect: {type: carrier-pigeon}, expect: {min: 1}}
`,
			wantErr: "unknown collector type",
		},
		{
			name: "invalid severity",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}, severity: mild}
`,
			wantErr: "invalid severity",
		},
		{
			name: "unknown alert sink reference",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}, alert: nowhere}
`,
			wantErr: "unknown sink",
		},
		{
			name: "webhook requires url",
			yaml: `
alerts:
  - {name: ops, type: webhook}
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}}
`,
			wantErr: "url is required",
		},
		{
			name: "unset env var fails loudly",
			yaml: `
alerts:
  - {name: ops, type: webhook, url: "${STALLWATCH_TEST_UNSET_VAR}"}
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}}
`,
			wantErr: "STALLWATCH_TEST_UNSET_VAR",
		},
		{
			name: "bad duration",
			yaml: `
signals:
  - {name: s, collect: {type: exec, cmd: ["true"]}, expect: {min: 1}, interval: fortnight}
`,
			wantErr: "invalid duration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}
