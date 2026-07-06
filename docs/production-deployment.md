# Production deployment

stallwatch is a single static binary — the deployment is a file copy, a
config, and a systemd unit. This guide is the pattern used for the first
production deployment (a multi-app Docker host), genericized.

## 1. Build and ship

```bash
make linux                      # CGO-free linux/amd64 binary, ~10 MB
ssh root@yourhost 'mkdir -p /opt/stallwatch'
scp stallwatch-linux-amd64 root@yourhost:/opt/stallwatch/stallwatch
```

No runtime, no libraries, no container required. If the host runs your
workloads in Docker, running stallwatch *on the host* (not in a container) is
deliberate: `exec` probes get `docker exec`, host files, and local sockets for
free, without mounting `docker.sock` into anything.

## 2. Config

`/opt/stallwatch/stallwatch.yaml` — see the recipes below. Validate before
every restart:

```bash
stallwatch -check -config /opt/stallwatch/stallwatch.yaml
```

`-check` fails on unknown fields, invalid rules, and unset `${ENV_VAR}`
references, so a bad config never replaces a running monitor.

## 3. systemd unit

`/etc/systemd/system/stallwatch.service`:

```ini
[Unit]
Description=stallwatch - progress-aware monitoring
After=network-online.target docker.service
Wants=network-online.target

[Service]
# Source webhook tokens from the receiver's own env file where possible:
# one source of truth means a token rotation never needs a stallwatch change.
EnvironmentFile=/opt/notifier/.env
WorkingDirectory=/opt/stallwatch
ExecStart=/opt/stallwatch/stallwatch -config /opt/stallwatch/stallwatch.yaml -db /opt/stallwatch/stallwatch.db
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now stallwatch
journalctl -u stallwatch -f
```

## 4. Who watches the watcher

stallwatch can't alert about its own death. Close the loop with a systemd
`OnFailure=` hook that posts to your alert receiver when the service enters
failed state.

There's a trap here: with `Restart=always` and `RestartSec=5`, a crash-looping
service **never trips systemd's default start-rate limit** (5 starts in 10s),
so it never reaches failed state and `OnFailure=` never fires — it just loops
silently forever. Widen the window so crash-loops actually trip it:

`/etc/systemd/system/stallwatch.service.d/watchdog.conf`:

```ini
[Unit]
OnFailure=stallwatch-notify-failure.service
StartLimitIntervalSec=300
StartLimitBurst=5
```

`/etc/systemd/system/stallwatch-notify-failure.service`:

```ini
[Unit]
Description=Alert notifier when stallwatch dies

[Service]
Type=oneshot
EnvironmentFile=/opt/notifier/.env
ExecStart=/usr/bin/curl -fsS -X POST http://127.0.0.1:4000/notify -H "Content-Type: application/json" -H "X-Internal-Token: ${INTERNAL_TOKEN}" -d '{"app":"stallwatch","severity":"critical","title":"stallwatch service died","body":"systemd gave up restarting stallwatch (failed state). No progress monitoring until this is fixed.","fingerprint":"stallwatch:service-died"}'
```

Trade-off, stated plainly: after 5 failures in 5 minutes the service stays
down until a human intervenes — but you *know*, which beats an invisible
crash loop. Test the hook once with `systemctl start
stallwatch-notify-failure` (use a harmless payload first) before trusting it.

## 5. Prove the alert chain before trusting it

Add a temporary signal that breaches immediately, watch it arrive at your
alert receiver, then remove it and restart. A monitor whose delivery path was
never exercised is decoration:

```yaml
  - name: deploy-smoke
    collect: {type: exec, cmd: ["echo", "999"]}
    interval: 60s
    expect: {max: 10}
    severity: info
```

## Signal recipes

### Job queue stall (the founding use case)

A task sitting unfinished for 30+ minutes means the queue is wedged — true at
any traffic level, unlike "counter must grow hourly", which false-alarms
during quiet hours. Returns 0 when the queue is empty:

```yaml
  - name: myapp-queue-oldest-task-age
    collect:
      type: exec
      cmd: ["docker", "exec", "myapp-db", "psql", "-U", "postgres", "-d", "myapp", "-tAc",
            "SELECT COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at)))::bigint, 0) FROM tasks WHERE status IN ('pending','running')"]
    interval: 2m
    expect: {max: 1800}
    severity: critical
```

### Credential / token refresh heartbeat

Watch seconds-until-expiry of a token that a background refresher should keep
fresh. If the refresh chain dies, the value decays toward zero and breaches
long before workloads start failing:

```yaml
  - name: oauth-token-seconds-to-expiry
    collect:
      type: exec
      cmd: ["python3", "-c",
            "import json,time; d=json.load(open('/root/.credentials.json')); print(int(d['oauth']['expiresAt'])//1000-int(time.time()))"]
    interval: 5m
    expect: {min: 1800}
    severity: critical
```

Pick the `min` so healthy steady-state clears it with margin (e.g. refresher
triggers at 2h remaining → alert at 30m remaining).

### Disk usage

```yaml
  - name: root-disk-used-percent
    collect:
      type: exec
      cmd: ["sh", "-c", "df --output=pcent / | tail -1 | tr -dc 0-9"]
    interval: 10m
    expect: {max: 90}
    severity: error
```

### Throughput floor via an app metric

```yaml
  - name: api-jobs-completed
    collect:
      type: http_json
      url: http://localhost:8080/metrics
      path: jobs.completed_total
    interval: 1m
    expect:
      increase_by: 1
      over: 1h
    severity: error
```

Only use `increase_by` for counters that genuinely always move; for anything
traffic-shaped, prefer queue-age or backlog-depth bounds.

## Operational notes

- **Webhook receiver**: any endpoint accepting
  `{app, severity, title, body, fingerprint}` JSON works. Fingerprints are
  stable per signal+condition — deduplicate on them if your receiver may see
  retries.
- **The sample DB** (`stallwatch.db`) persists baselines across restarts;
  keep it on persistent storage so stall rules don't re-warm after deploys.
- **Collector failures alert too** (after `fail_after` consecutive misses) —
  a database container being down surfaces as "collector failing" even before
  the queue signal can say anything.
- **Adding signals**: edit the YAML, run `-check`, `systemctl restart
  stallwatch`. Existing samples are kept; new signals warm up from their
  first sample.
