# Deploying This Service for Production

This guide covers running the service reliably: auto-restart, health probes,
logging, and removing the single point of failure by running more than one copy.

## Read this first: what is verified and what is not

This repository is careful about the difference between "tested" and "written".

| Artifact | Status |
|---|---|
| The Go service itself (rendering, crash recovery, health, logging) | **Verified** — automated tests plus live runs against real Chromium |
| `Dockerfile`, `docker-compose.yml` | **Not verified** — no Docker daemon was available where they were written. Reviewed, never built |
| `deploy/systemd/*` | **Not verified** — systemd is not PID 1 in that environment. Reviewed, never started |

Build and smoke-test the container and systemd artifacts before trusting them.
The checks at the end of this guide are exactly what to run.

## 1. The gaps this closes (and the one it doesn't)

Earlier the service had five production gaps. Four are now closed **in the
service itself**:

| Gap | Status |
|---|---|
| A crashed Chromium instance kept failing forever | **Fixed** — instances self-heal; verified by killing every browser under a live server and watching the next request succeed in 118ms |
| `/healthz` always returned 200 and checked nothing | **Fixed** — `/livez` + `/readyz` report real state |
| No request-level logging | **Fixed** — one structured JSON record per request, with a request ID |
| No auto-restart if the process dies | **Fixed by deployment config** — the artifacts here |
| Single point of failure | **Fixed by running 2+ replicas** — see §5 |

Still genuinely missing, and unchanged by this guide: **there is no
authentication or rate limiting.** Do not expose this service directly to the
public internet. Keep it on a private network reachable only from your own
backend.

## 2. Option A — Docker (recommended)

```bash
docker compose up -d --build
docker compose logs -f
```

What the compose file gives you:

- `restart: unless-stopped` — the auto-restart the service cannot do for itself.
- `shm_size: 1gb` — Chromium stores shared memory in `/dev/shm`, and Docker's
  64MB default is a well-known cause of renderer crashes on larger documents.
- A `healthcheck` that runs the binary's own `-healthcheck` flag. The image
  ships no curl or wget on purpose, so the binary probes itself instead. This
  flag is real and tested: it exits 0 when the service answers and 1 when it
  does not.
- Memory limits sized from the **measured loaded** figure (~800MB per Chromium
  instance under sustained heavy load), not the ~350MB idle figure.

**Why the healthcheck probes `/livez` and not `/readyz`:** readiness
deliberately reports failure while the service is draining, and while a
Chromium pool is repairing itself. Restarting the container on either of those
would destroy recovery that was already in progress.

## 3. Option B — systemd (no containers)

```bash
go build -o pdfsvc ./cmd/api
sudo install -m755 pdfsvc /usr/local/bin/pdfsvc
sudo useradd --system --home /var/lib/great-pdf-generator --create-home pdfsvc
sudo cp deploy/systemd/great-pdf-generator.env /etc/great-pdf-generator.env
sudo cp deploy/systemd/great-pdf-generator.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now great-pdf-generator
systemctl status great-pdf-generator
```

Two settings there are load-bearing and worth understanding rather than copying
blindly:

- **`KillMode=control-group`** — the service forks Chromium child processes.
  Killing only the main process would strand them, each holding hundreds of MB.
- **`StartLimitBurst=5` / `StartLimitIntervalSec=300`** — if the service is
  genuinely broken (wrong `CHROMIUM_PATH`, missing fonts) it will fail on every
  start. Without a limit systemd would restart it forever; with one, it stops
  after 5 failures in 5 minutes and shows as `failed`, which someone can
  actually see.
- **`TimeoutStopSec=60`** must stay above `SHUTDOWN_GRACE` plus your longest
  render (30s default), or systemd SIGKILLs mid-drain and orphans browsers.

## 4. Health endpoints

| Endpoint | Answers | Use it for |
|---|---|---|
| `GET /livez` | "Is this process wedged?" | Restart decisions (Docker/systemd/K8s liveness) |
| `GET /readyz` | "Should this instance get traffic?" | Load balancer / K8s readiness |
| `GET /healthz` | Same as `/readyz` | Backwards compatibility |

`/readyz` returns real state:

```json
{
  "status": "ok",
  "chromium": {"size": 2, "alive": 2, "restarts": 0, "restart_attempts": 0, "hangs_detected": 0},
  "queue": {"in_flight": 0, "depth": 0, "capacity": 24, "workers": 12},
  "lightweight_render": {"configured": true}
}
```

`hangs_detected` counts instances rebuilt because they stopped answering while
still *looking* alive — a browser frozen or wedged rather than crashed. It is
reported separately from `restarts` because the causes differ: crashes point at
memory or browser bugs, hangs at CPU starvation. Both are detected and repaired
automatically; a steadily climbing count means something upstream is wrong.

Clients may also see `503 ENGINE_UNAVAILABLE` (with `Retry-After`) when no
instance is currently healthy. That is deliberately not a `422`: the caller's
document may be perfectly valid, the pool repairs itself within seconds, and
retrying is the correct behaviour.

**The field to alert on** is `restart_attempts` climbing while `restarts` stays
flat. That means Chromium is being asked to start and cannot — a bad path, an
out-of-memory host, or a full disk. No amount of retrying fixes it, so it needs
a human.

`in_flight` versus `capacity` is your saturation signal. Deliberately **not**
`depth`: queue depth reads 0 both when the service is idle and when every worker
is busy with a momentarily empty queue, so alerting on depth alone would report
"all clear" at peak load.

Two deliberate behaviours worth knowing before you wire up alerts:

- **Readiness does not fail just because the service is busy.** A saturated
  replica reporting not-ready would be pulled from the load balancer, pushing its
  traffic onto its peers and saturating them next — the classic cascading
  failure. Overload is handled per-request instead, with `503 QUEUE_FULL` and a
  `Retry-After` header.
- **Readiness fails as soon as shutdown begins,** while liveness stays healthy,
  so balancers drain the instance without anything killing it mid-drain.

## 5. Removing the single point of failure

The service holds no state between requests — **verified**, by running two
copies side by side, hard-killing one with `SIGKILL`, and confirming the other
kept serving PDFs without interruption.

So you can run as many replicas as you like behind any load balancer today:

```nginx
upstream pdfsvc {
    server 10.0.0.11:8080 max_fails=3 fail_timeout=10s;
    server 10.0.0.12:8080 max_fails=3 fail_timeout=10s;
}

server {
    listen 80;
    location / {
        proxy_pass http://pdfsvc;
        proxy_read_timeout 60s;   # must exceed your longest render
    }
}
```

Or with compose: `docker compose up -d --scale pdfsvc=3` (remove the fixed
host `ports:` mapping first, and put a load balancer in front).

**The one real caveat, stated plainly:** each replica has its *own* admission
queue. Backpressure is therefore per-replica, not global — three replicas each
admitting 24 in-flight requests will admit 72 in total, not 24. That is fine for
capacity planning as long as you size from the per-replica number and multiply.
A genuinely shared queue would need the distributed backend that
`PLATFORM-SPEC.md` deliberately defers.

## 6. Logging

Logs are JSON on stdout, one record per request:

```json
{"time":"...","level":"INFO","msg":"http_request","request_id":"1a1a34be...",
 "method":"POST","path":"/v1/pdf/html","route":"POST /v1/pdf/html",
 "status":200,"bytes":5595,"duration_ms":41}
```

- `LOG_LEVEL` accepts `debug`, `info` (default), `warn`, `error`.
- The `request_id` is returned to the caller as an `X-Request-Id` header, so a
  user reporting "this request failed" hands you the exact key to grep for.
- Health probes log at `debug`, so a probe every 10s does not bury real traffic
  (that would otherwise be ~8,600 records per replica per day saying nothing
  happened). Set `LOG_LEVEL=debug` when a probe itself is under suspicion.
- A `503` logs at `warn`, not `error`. In this service a 503 is always a
  designed response — shedding load, draining, or a dependency not configured —
  and logging those beside genuine faults is how `ERROR` becomes noise people
  learn to ignore.

## 7. Smoke checks after deploying

Run these against a freshly deployed instance. They are the same checks used to
verify the service during development.

```bash
BASE=http://localhost:8080

# 1. Both probes answer
curl -s -w " [%{http_code}]\n" $BASE/livez
curl -s -w " [%{http_code}]\n" $BASE/readyz

# 2. A real PDF comes back
curl -s -o out.pdf -w "status=%{http_code}\n" -X POST \
  -H "Content-Type: application/json" \
  -d '{"content":"<html><body><h1>deploy check</h1></body></html>"}' \
  $BASE/v1/pdf/html
file out.pdf     # expect: PDF document

# 3. Fonts are actually installed (blank text is the classic container bug)
curl -s -o cjk.pdf -X POST -H "Content-Type: application/json" \
  -d '{"content":"<html><body><p>English 中文 日本語 한국어</p></body></html>"}' \
  $BASE/v1/pdf/html
#    Open cjk.pdf and confirm the CJK characters are not empty boxes.

# 4. Crash recovery works in YOUR environment
#    Kill the browsers, then confirm the next request still succeeds.
docker compose exec pdfsvc pkill -9 chromium || true
curl -s -o heal.pdf -w "after kill: status=%{http_code}\n" -X POST \
  -H "Content-Type: application/json" -d '{"content":"<p>healed</p>"}' \
  $BASE/v1/pdf/html
curl -s $BASE/readyz    # expect restarts >= 1

# 5. Auto-restart works
#    Kill the service process itself; the supervisor must bring it back.
docker compose kill pdfsvc && sleep 15 && curl -s -w " [%{http_code}]\n" $BASE/livez
```

Check 4 and 5 are the ones people skip and later regret — they are the two
failure modes this whole guide exists to handle, and they take a minute to
confirm.

## 8. Sizing

| Setting | Guidance |
|---|---|
| `POOL_SIZE` | About one Chromium instance per CPU core; start at `cores - 1`. **Measured**: adding instances beyond core count gave no extra throughput and slightly increased contention |
| Memory | Budget ~800MB per instance for heavy documents (measured under load), not the ~350MB idle figure |
| `MAX_CONCURRENCY_PER_INSTANCE` | 6 by default, matching Gotenberg's own default |
| `JOB_QUEUE_CAPACITY` | Defaults to the pool's concurrent-render capacity. Higher tolerates burstier traffic; lower rejects sooner |

Measured throughput on a 4-core box: **~26-27 heavy 3-page renders/second**
with zero failures. See `docs/research/load-test-results.md` for the methodology
and the raw numbers.
