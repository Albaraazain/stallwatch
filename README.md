# stallwatch

[![CI](https://github.com/Albaraazain/stallwatch/actions/workflows/ci.yml/badge.svg)](https://github.com/Albaraazain/stallwatch/actions/workflows/ci.yml)

**Uptime monitors tell you a service is alive. stallwatch tells you it's actually doing work.**

There's a class of production failure that every mainstream monitoring tool misses:
the job queue that retries forever, the worker that wedges, the pipeline that
silently stops writing rows. The containers are up. The health endpoint is green.
Throughput is zero. Nobody finds out for days.

stallwatch was born from exactly that outage: a generation queue spent days
retrying against exhausted upstream providers while every dashboard showed
healthy. Liveness monitoring answered "is it up?" — nobody was asking
"is it *moving*?"

stallwatch is a single static binary that watches **progress signals** — numbers
that must keep moving — and alerts when they stall.

```yaml
signals:
  - name: mcq-generation-progress
    collect:
      type: exec
      cmd: ["psql", "-tAc", "SELECT count(*) FROM generated_questions"]
    interval: 5m
    expect:
      increase_by: 1   # this counter must grow...
      over: 1h         # ...at least this often, or someone gets paged
    severity: critical
```

## How it's different

| | checks | catches a wedged-but-alive worker? |
|---|---|---|
| Uptime Kuma & friends | liveness (ping, HTTP 200) | no — the process is up |
| healthchecks.io / Dead Man's Snitch | push: your job must ping | no — a retry loop *thinks* it's working and keeps running |
| Prometheus + Alertmanager | anything, if you build exporters + PromQL + the stack | yes, at the cost of an ecosystem |
| **stallwatch** | **pull: observes throughput from outside** | **yes — one binary, one YAML file, zero app changes** |

The key design choice is **pull-based observation**: stallwatch measures progress
from the outside (a row count, a queue depth, a metric endpoint), so it needs no
cooperation from the thing it watches. A stuck process can't forget to report
that it's stuck.

## How it works

```
YAML config          engine (1 goroutine per signal)         alerting
───────────          ───────────────────────────────         ────────
signals ──────▶  collect ──▶ SQLite window ──▶ evaluate ──▶ transition?
                 (http_json,   (persists        (stall /      │
                  exec)         restarts)        bounds)      ▼
                                                        webhook (JSON)
```

- **Collect**: every signal is sampled on its own interval as one `float64`.
  Two collectors cover almost everything:
  - `http_json` — GET a URL, extract a value at a dot path (`pending.count`,
    `queues.0.depth`)
  - `exec` — run a command, parse numeric stdout: `psql`, `redis-cli`,
    `docker exec`, `ssh anything` all just work
- **Store**: samples land in an embedded SQLite database, so stall baselines
  survive restarts — stallwatch isn't blind for an hour after a deploy.
- **Evaluate**: `increase_by`/`over` detects stalls in counters that must keep
  moving; `min`/`max` bound gauges. Counter resets (deploys, truncation) are
  recognized as activity, not stalls.
- **Alert on transitions only**: one alert when a signal enters breach, one
  when it recovers. Never a page per tick. Collector failures themselves alert
  after N consecutive misses — a monitor whose probes silently fail is the very
  disease it treats.

## Quick start

```bash
go install github.com/Albaraazain/stallwatch/cmd/stallwatch@latest
# or: make build  (CGO-free static binary, ~10 MB)

cp stallwatch.example.yaml stallwatch.yaml   # declare your signals
stallwatch -check                            # validate config
stallwatch                                   # run
```

Deploying to a server? See [docs/production-deployment.md](docs/production-deployment.md)
for the systemd pattern and real-world signal recipes.

## CLI

```
stallwatch [flags]
```

| flag | default | description |
|---|---|---|
| `-config` | `stallwatch.yaml` | path to the config file |
| `-db` | `stallwatch.db` | path to the SQLite sample database |
| `-check` | | validate the config and exit (use in CI / before restarts) |
| `-debug` | | log every evaluation, not just alerts |
| `-version` | | print version and exit |

Logs go to stderr in `slog` text format. SIGINT/SIGTERM shut down gracefully.

## Configuration reference

### Signal

| field | required | description |
|---|---|---|
| `name` | yes | unique signal name, used in alerts and storage |
| `collect` | yes | collector spec (see below) |
| `expect` | yes | at least one of `increase_by`+`over`, `min`, `max` |
| `interval` | no | sampling interval (default from `defaults.interval`, 60s) |
| `severity` | no | `critical` \| `error` \| `warn` \| `info` (default `error`) |
| `alert` | no | route to one named sink (default: all sinks) |
| `fail_after` | no | consecutive collector failures before alerting (default 3) |

### Defaults

| field | default | description |
|---|---|---|
| `defaults.interval` | `60s` | collection interval for signals that don't set their own |
| `defaults.retention` | `168h` | sample retention; auto-raised if a stall window needs more |
| `defaults.fail_after` | `3` | consecutive collector failures before alerting |

### Collectors

**`http_json`** — GET a URL, extract a number at a dot path.

```yaml
collect:
  type: http_json
  url: http://localhost:8080/metrics
  path: queues.0.depth        # keys descend objects; numeric segments index arrays
  headers: {Authorization: "Bearer ${METRICS_TOKEN}"}
```

Requires a 2xx response; bodies are capped at 1 MiB. The value at the path may
be a JSON number, a numeric string (`"3.5"`), or a bool (`true` → 1). An empty
`path` means the whole body is the number.

**`exec`** — run a command, parse its stdout as a number.

```yaml
collect:
  type: exec
  cmd: ["psql", "-tAc", "SELECT count(*) FROM jobs_done"]
```

`cmd` is an argv list executed directly — no shell, so no quoting surprises;
wrap in `["sh", "-c", "..."]` when you want pipes. Stdout is trimmed and must
parse as a number. On failure the first line of stderr lands in the error.
Probes run in their own process group and the whole group is killed on
timeout, so an `ssh` to a hung host can't wedge a collector slot or leak
processes.

### Expectations

| rule | meaning |
|---|---|
| `increase_by: N` + `over: D` | value must grow by ≥ N over any trailing window of D — the stall detector |
| `min: N` / `max: N` | latest value must stay within bounds — fires immediately, even during warmup |

Detection semantics worth knowing:

- **Warmup**: a stall rule needs a baseline sample at least `over` old before
  it can fire. Baselines live in SQLite, so warmup survives restarts.
- **Baseline anchoring**: the delta is measured against the newest sample that
  is at least `over` old — not the window edge — so jittered sampling can
  never wedge a rule in permanent warmup.
- **Counter resets**: a negative delta (deploy, truncation) counts as
  activity, not a stall.
- Every collection times out (30s) and runs under a concurrency cap (8).

### Alert sinks

`type: webhook` POSTs JSON:

```json
{
  "app": "stallwatch",
  "severity": "critical",
  "title": "queue-progress: stalled",
  "body": "increased by 0 over 1h0m0s, expected >= 1 (last value 4312)",
  "fingerprint": "stallwatch:breach:queue-progress"
}
```

The fingerprint is stable per signal+condition so receivers can deduplicate.
Headers support `${ENV_VAR}` expansion — referencing an unset variable is a
startup error, never a silent empty string (and comments are never expanded).

Delivery semantics: alerts fire on **state transitions only** (breach entered,
breach recovered, collector failing, collector recovered). A delivery refused
by a sink is parked and retried next tick — newest event wins. Alerts raised
during shutdown still deliver, bounded by a 10s timeout.

## Design notes

- **Lock-free by construction**: each signal's state is owned exclusively by
  its own goroutine; there is no shared mutable state and therefore no locking
  to get wrong. A semaphore bounds concurrent collections; a context deadline
  bounds each one.
- **Strict config**: unknown YAML fields are rejected, so a typo like `colect:`
  is a startup error instead of a signal that silently never runs.
- **Pure-Go SQLite** (`modernc.org/sqlite`): keeps `CGO_ENABLED=0`
  cross-compilation honest — one `GOOS=linux make` away from any server.

## Development

```bash
make test   # go test -race ./...
make lint   # gofmt + go vet
make linux  # cross-compile static linux/amd64 binary
```

## Roadmap

- `postgres` native collector (direct DSN, no psql binary needed)
- `docker_logs` collector: pattern match rate over `docker logs --since`
- rate-of-change expectations (EWMA baseline: "alert when throughput drops 80% below normal")
- `/healthz` self-endpoint + Prometheus `/metrics` export
- status TUI (`stallwatch top`)
