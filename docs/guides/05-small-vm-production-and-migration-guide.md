# Running This Service in Production on a Small VM (and Retiring the Old Node.js Service)

This guide is for the specific situation many people migrating from a Node.js + Puppeteer PDF
service will recognize: a single, modestly-sized VM (think 2 vCPU, 2GB RAM class — a `t3.small` or
similar), already running a Node.js/Chromium-based PDF service in production, that needs to be
replaced by this Go service with as little risk and downtime as possible.

It combines four things into one place, in the order you'll actually do them:

1. How to configure this service so it runs well within a small VM's real limits.
2. What to do with the old Node.js service — how to retire it safely, and how to clear out the
   disk/cache clutter it (and the OS itself) tends to leave behind.
3. How to actually use this service once it's running (the API, from a caller's point of view).
4. How to deploy and operate it at a production level — security, monitoring, and the safe
   cutover sequence from old to new.

Every number in this guide (memory per Chromium instance, throughput, etc.) comes from real
measurements recorded in this repo — `docs/research/load-test-results.md` and
`docs/planning/FINALISATION-REPORT.md` — not guesses. Treat the specific figures as a starting
point to re-verify on your own box with the commands given, not as guaranteed constants.

---

## 0. At a glance

| Your situation | What this guide tells you to do |
|---|---|
| VM has ~1GB RAM (and shares it with other services) | `POOL_SIZE=1`, `MAX_CONCURRENCY_PER_INSTANCE=1`, **3GB swap**, `MemoryMax=780M`, build off-box. You cannot also run the old Node/Chromium PDF service — this must be a straight replacement. See §3.1 and `08-ec2-deployment-alongside-node.md` §0.5. Resizing to 2GB is strongly recommended. |
| VM has ~2GB RAM | `POOL_SIZE=1`, `MAX_CONCURRENCY_PER_INSTANCE=2` — see §3 |
| VM has 4GB+ RAM | `POOL_SIZE=2`, can raise concurrency — see §3 |
| Old Node.js/Puppeteer service still running | Don't stop it yet — validate the new service side-by-side first (§2, §6) |
| Need to free disk space | §2.3 |
| Calling server lives elsewhere | Its integration code needs updating too, not just the VM — §5, §6 |
| This service will be reachable from the internet | Read §4 before you open any port |

---

## 1. Know your box before configuring anything

Don't skip this — every tuning number below is a function of what you actually have, not a fixed
recipe. Run this once and keep the output handy:

```bash
echo "--- CPU ---"; nproc; lscpu | grep -E "Model name|CPU\(s\)"
echo "--- Memory ---"; free -h
echo "--- Disk ---"; df -h /
echo "--- What's currently listening ---"; sudo ss -tlnp
echo "--- OS ---"; lsb_release -a 2>/dev/null || cat /etc/os-release
```

A 2 vCPU / 2GB RAM VM (the common small-instance shape) is what the rest of this guide assumes by
default — it will call out where a bigger box changes the answer.

---

## 2. Retiring the old Node.js/Puppeteer service

**Do not stop it yet if it's still serving real traffic.** The right order is: prove the new
service works (§6.1), switch the caller over (§6.3), *then* stop and clean up the old one. This
section covers the mechanics of that stop-and-cleanup step, to do once you've actually cut over.

### 2.1 Find out how it's actually running

It could be managed a few different ways — check in this order:

```bash
pm2 list                                                          # PM2
sudo systemctl list-units --type=service --state=running | grep -iE 'node|pdf|puppet'  # systemd
sudo docker ps                                                    # Docker
ps aux | grep -iE 'node|puppeteer|chrome' | grep -v grep          # plain `node server.js` / nohup
```

### 2.2 Stop it (once you've cut traffic over)

Match whichever of the above found it:

```bash
# PM2
pm2 stop <app-name>
pm2 save                       # so it doesn't come back on the next reboot
pm2 delete <app-name>          # once you're confident, a few days later

# systemd
sudo systemctl stop <unit-name>
sudo systemctl disable <unit-name>

# Docker
sudo docker stop <container-name>
sudo docker rm <container-name>          # once confident
sudo docker image rm <image-name>        # reclaim the image's disk too

# Plain process
kill <pid>                     # find pid via `ps aux | grep node`
```

**Stop it, don't delete anything, for the first few days.** Keep the code/container/PM2 entry
around so you can restart it in seconds if the new service turns out to have a problem you missed
in testing. Only remove things permanently once you've watched real production traffic succeed on
the new service for a real stretch of time (a day or two of normal usage, not just an hour).

### 2.3 Clear out storage and cache — what's actually safe to remove

A Node.js + Puppeteer setup accumulates disk clutter in fairly predictable places. Check sizes
before deleting anything:

```bash
# See what's actually using disk, biggest first
sudo du -sh /var/log/* 2>/dev/null | sort -rh | head -10
sudo du -sh /home/*/.pm2 /home/*/.npm /home/*/.cache 2>/dev/null | sort -rh
sudo du -sh /var/lib/docker 2>/dev/null
sudo du -sh /tmp/* 2>/dev/null | sort -rh | head -10
```

Safe, common cleanup targets:

| What | Command | Why it's safe |
|---|---|---|
| npm's download cache | `npm cache clean --force` | Just re-downloads packages if ever needed again; nothing depends on it being present |
| PM2's own logs | `pm2 flush` (clears logs, keeps the process running) | PM2 logs grow unbounded by default and are rarely needed once you've read them |
| Puppeteer's leftover Chromium temp profiles | `sudo find /tmp -maxdepth 1 -iname "puppeteer_dev_chrome_profile-*" -mtime +1 -exec rm -rf {} \;` | Puppeteer creates a fresh temp profile directory per browser launch; orphaned ones from crashed runs are pure waste — this is a well-documented Puppeteer/Node pain point, and one of the concrete failure modes this whole migration exists to fix |
| Docker, if it was used and won't be again | `sudo docker system prune -a --volumes` | **Careful**: this deletes *all* unused images/containers/volumes, not just the old PDF service's. Only run this if Docker isn't used for anything else on this box. |
| APT's downloaded package cache | `sudo apt-get clean` | Just deletes cached `.deb` files already installed; nothing breaks |
| Unused old kernels/packages | `sudo apt autoremove -y` | Standard, safe Ubuntu housekeeping |
| systemd journal logs beyond what you need | `sudo journalctl --vacuum-time=7d` | Keeps a week, drops older logs |

**The single most valuable thing to check**: if the old service's own crash/restart behavior was
the actual problem you're migrating away from (per the earlier diagnosis — a server that "gets full
somehow, maybe cache issue"), it was very likely exactly this: Puppeteer launching a Chromium
process per request (or per crash-restart) without ever cleaning up the previous one's temp
profile directory or, in the worst case, the zombie Chromium process itself. Confirm this
diagnosis before you move on, with:

```bash
ps aux | grep -c '[c]hrome'    # a large, ever-growing number here (not 1-2) confirms the theory
```

If that number is high, this migration's own crash recovery (self-healing pool, no leaked
processes — verified directly in `docs/planning/FINALISATION-REPORT.md`) is the structural fix, not
just a one-time cleanup.

---

## 3. Configuration for a small VM

### 3.1 The sizing math

The two settings that matter most, and the reasoning behind them:

- **`POOL_SIZE`** — how many Chromium browser processes stay warm. Real measurement on a 4-core
  box: throughput plateaus once `POOL_SIZE` reaches the core count — more instances than cores adds
  contention, not speed (`docs/research/load-test-results.md`). On a 2-core box, that means
  `POOL_SIZE=1` (leave the second core for the Go process, OS, and networking), not `POOL_SIZE=2`.
- **Memory per instance** — measured at ~350MB idle, up to **~800MB under a heavy multi-page
  document** (`docs/planning/FINALISATION-REPORT.md`). This is the number that actually constrains
  a small VM, not CPU.

| Your VM's total RAM | `POOL_SIZE` | `MAX_CONCURRENCY_PER_INSTANCE` | Rough headroom left for OS |
|---|---|---|---|
| ~1GB (dedicated to this service) | `1` | `1` | ~200–300MB + **3GB swap required** |
| ~2GB | `1` | `2` | ~1.1–1.3GB |
| ~4GB | `2` | `4` | ~1.5–2GB |
| ~8GB | `3` | `6` (default) | ~4GB+ |
| 16GB+ | `runtime.NumCPU() - 1` | `6` (default) | plenty |

**At 1GB** the box has room for *one* Chromium and nothing else Chromium-based
— so it must be the only PDF service on it (retire the Node one), it must have
swap (a heavy render briefly needs ~500–800MB, more than the box has free), and
`MAX_CONCURRENCY_PER_INSTANCE` must be `1` so two heavy renders never overlap.
`MemoryMax=780M` + `MemoryHigh=680M` in the unit make systemd throttle/kill
this cgroup on a leak instead of the kernel OOM-killer hitting `sshd`.
Throughput is ~2–4 heavy reports/minute, serialized — fine for interactive use,
not for batch. Prefer resizing to 2GB.

Don't set `MAX_CONCURRENCY_PER_INSTANCE` high on a small VM even though it doesn't change
`POOL_SIZE` — each concurrent render opens another browser tab, which costs its own memory on top
of the base ~350MB, so a high concurrency setting on a single instance can still exhaust RAM even
with `POOL_SIZE=1`.

### 3.2 The full environment file

```bash
sudo tee /etc/go-dynamic-pdf-generator.env >/dev/null <<'EOF'
# --- required ---------------------------------------------------------------
CHROMIUM_PATH=/opt/chrome-headless-shell/chrome-headless-shell

# --- server ------------------------------------------------------------------
PORT=8080
LOG_LEVEL=info
SHUTDOWN_GRACE=5s

# --- render pool: tuned for a ~2GB RAM VM (see the table in §3.1) -----------
POOL_SIZE=1
MAX_CONCURRENCY_PER_INSTANCE=2

# Leave JOB_QUEUE_CAPACITY unset: it defaults to the pool's own real
# concurrent-render capacity (POOL_SIZE x MAX_CONCURRENCY_PER_INSTANCE),
# which is already the right starting point.

# --- lightweight (WeasyPrint) path -------------------------------------------
# Leave commented out unless you specifically need it. It's a separate
# Python subprocess per render with no pooling (SPEC-lightweight-renderer.md)
# — real, extra memory and CPU cost you don't want on a box this size unless
# you have a genuine use case for it.
# WEASYPRINT_PATH=/usr/bin/weasyprint
EOF
```

### 3.3 The systemd unit, with a memory ceiling sized to this box

```bash
sudo tee /etc/systemd/system/go-dynamic-pdf-generator.service >/dev/null <<'EOF'
[Unit]
Description=go-dynamic-pdf-generator (HTML/Markdown to PDF service)
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=pdfsvc
Group=pdfsvc
EnvironmentFile=/etc/go-dynamic-pdf-generator.env
ExecStart=/usr/local/bin/pdfsvc

# Auto-restart (this is the process-supervision answer to "who restarts it
# if it crashes" — the old Node service likely had no equivalent).
Restart=always
RestartSec=5s
StartLimitIntervalSec=300
StartLimitBurst=5

# Graceful shutdown: must exceed SHUTDOWN_GRACE plus the longest render
# (30s default), or systemd SIGKILLs mid-drain and orphans Chromium.
KillSignal=SIGTERM
TimeoutStopSec=60
KillMode=control-group

# Hardening — real, not box-ticking: this process runs a sandbox-disabled
# Chromium executing arbitrary submitted HTML/JS, so these restrictions are
# the actual isolation boundary.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
StateDirectory=go-dynamic-pdf-generator
Environment=HOME=/var/lib/go-dynamic-pdf-generator

# Sized for a ~2GB-RAM VM: leaves ~500-600MB headroom for the OS/sshd, so
# systemd's cgroup kills *this service* on a real leak rather than the
# kernel's global OOM-killer picking a random process (sshd included).
# Recalculate for your own box: total RAM minus ~500MB OS headroom.
MemoryMax=1400M

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now go-dynamic-pdf-generator
systemctl status go-dynamic-pdf-generator --no-pager
```

### 3.4 Installing Chromium itself (the lean way, no snap)

Ubuntu's `chromium` apt package is a snap wrapper and commonly fails under systemd (no desktop
session/DBus). Use the standalone `chrome-headless-shell` binary instead — the same one this
project's own tests run against:

```bash
sudo apt update
sudo apt install -y jq unzip \
  ca-certificates fonts-liberation libasound2 libatk-bridge2.0-0 libatk1.0-0 \
  libcairo2 libcups2 libdbus-1-3 libexpat1 libfontconfig1 libgbm1 libglib2.0-0 \
  libgtk-3-0 libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 \
  libxdamage1 libxext6 libxfixes3 libxrandr2 libxkbcommon0 xdg-utils

CHS_URL=$(curl -s https://googlechromelabs.github.io/chrome-for-testing/known-good-versions-with-downloads.json \
  | jq -r '[.versions[] | select(.channel=="Stable")] | last | .downloads["chrome-headless-shell"][] | select(.platform=="linux64") | .url')
curl -L -o /tmp/chs.zip "$CHS_URL"
sudo mkdir -p /opt/chrome-headless-shell
sudo unzip -j /tmp/chs.zip -d /opt/chrome-headless-shell
sudo chmod +x /opt/chrome-headless-shell/chrome-headless-shell
/opt/chrome-headless-shell/chrome-headless-shell --version
```

If `apt install` complains about one specific package name not existing, drop just that line and
continue — a few package names differ by Ubuntu version and most on this list are optional
convenience libraries, not hard requirements.

### 3.5 Getting the Go binary onto the server

You don't need to install the Go toolchain on a small, disk-constrained VM. Cross-compile it
wherever you have Go installed (or ask for a pre-built one):

```bash
# On a machine with Go installed, from the repo root:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o pdfsvc ./cmd/api
```

Then copy it up and install it:

```bash
scp -i <your-ssh-key.pem> pdfsvc ubuntu@<VM-IP>:/tmp/pdfsvc
```

```bash
# On the VM:
sudo install -m755 /tmp/pdfsvc /usr/local/bin/pdfsvc
sudo useradd --system --home /var/lib/go-dynamic-pdf-generator --create-home pdfsvc
```

To ship an update later, repeat: rebuild, `scp`, `sudo install -m755 ...`, then
`sudo systemctl restart go-dynamic-pdf-generator`.

---

## 4. Security — read this before opening any port

This service has **no built-in authentication or rate limiting** (a deliberate v1 scoping
decision, not an oversight — see `docs/planning/PLATFORM-SPEC.md`). That means:

- **Never** open its port to `0.0.0.0/0` in your cloud provider's firewall/security group.
- If the calling application lives on the **same server**, don't expose the port publicly at all —
  call `http://localhost:8080` directly; nothing external needs to reach it.
- If the calling application lives on a **different server**, restrict the inbound rule to that
  specific server's IP address (or its security group, if both are in the same private network).
- If you genuinely need broader access later, put a reverse proxy (nginx) in front with TLS and at
  minimum an API-key check or an IP allowlist — don't rely on this service alone for that.

---

## 5. How to use the service

Every conversion endpoint takes the same request shape and returns the same kind of response —
this is deliberate (`docs/planning/SPEC-conversion-api.md`), so you only need to learn it once.

### 5.1 Endpoints

| Endpoint | What it does |
|---|---|
| `POST /v1/pdf/html` | Renders an HTML string to PDF (full Chromium fidelity — CSS, JS, fonts, images) |
| `POST /v1/pdf/markdown` | Renders a Markdown string to PDF (compiled to HTML first, same rendering path) |
| `POST /v1/pdf/html-lite` | Renders static HTML to PDF via WeasyPrint — no JavaScript execution, only if `WEASYPRINT_PATH` is configured |
| `GET /livez` | "Is the process alive?" — for restart decisions |
| `GET /readyz` | "Should this instance get traffic?" — for load-balancer/monitoring checks |

### 5.2 Request and response shape

Request body (identical on every conversion endpoint):

```json
{
  "content": "<html><body><h1>Invoice #1042</h1></body></html>",
  "payload": { "customerName": "Ada Lovelace" }
}
```

- `content` (required) — your final, already-built HTML (or Markdown). If you already generate the
  final HTML on your own server (e.g. via Handlebars, as many Node.js setups do), just send that
  finished HTML here unchanged — you do **not** need to rewrite your templates into this service's
  own merge syntax to get started.
- `payload` (optional) — only needed if you want *this service* to do the variable substitution
  itself, using Go's `{{.fieldName}}` syntax (not Handlebars' `{{fieldName}}` — different syntax,
  a real migration step if you choose to use it, entirely optional).

Success: the raw PDF bytes come back directly, `Content-Type: application/pdf` — not wrapped in
JSON. Stream it straight to your response or storage upload, same as you'd handle Puppeteer's
output today.

Failure: a JSON error envelope, always the same shape:

```json
{"error": {"code": "RENDER_ERROR", "message": "..."}}
```

| Code | HTTP status | Meaning | What your caller should do |
|---|---|---|---|
| `INVALID_REQUEST` | 400 | Malformed JSON, empty body, or missing `content` | Fix the request — don't retry as-is |
| `REQUEST_TOO_LARGE` | 413 | Body over 10MB | Reduce document size |
| `TEMPLATE_ERROR` | 422 | `payload` merge failed (bad syntax, missing field) | Fix the template/payload |
| `RENDER_ERROR` | 422 | The document itself failed to render | Your input HTML has a real problem — don't blindly retry |
| `QUEUE_FULL` | 503 | Server is at capacity right now | **Retry after the `Retry-After` header's value** — this is a "try again shortly," not a permanent failure |
| `ENGINE_UNAVAILABLE` | 503 | No healthy Chromium instance right now (self-healing in progress) | **Retry after `Retry-After`** — same as above, the pool repairs itself within seconds |
| `NOT_CONFIGURED` | 503 | `/v1/pdf/html-lite` called but WeasyPrint isn't set up | Configure `WEASYPRINT_PATH`, or don't call this route |

The distinction between `RENDER_ERROR` (don't retry, your document is broken) and
`QUEUE_FULL`/`ENGINE_UNAVAILABLE` (do retry, it's a transient server-side condition) is the one
thing worth building real retry logic around — it didn't exist as a concept in a typical bespoke
Puppeteer wrapper, and getting it right avoids both pointless retries and giving up too early.

### 5.3 A minimal Node.js caller

```javascript
async function renderPdf(html) {
  const res = await fetch("http://<VM-IP>:8080/v1/pdf/html", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ content: html }),
  });

  if (res.status === 200) {
    return Buffer.from(await res.arrayBuffer()); // the PDF bytes
  }

  const { error } = await res.json();
  if (res.status === 503) {
    // QUEUE_FULL or ENGINE_UNAVAILABLE — safe, expected to retry
    const retryAfter = Number(res.headers.get("Retry-After") || "1");
    await new Promise((r) => setTimeout(r, retryAfter * 1000));
    return renderPdf(html); // simple retry; add a max-attempts guard in real code
  }
  throw new Error(`PDF generation failed: ${error.code} - ${error.message}`);
}
```

A fuller version of this, including idempotency/backoff patterns, is in
`docs/guides/03-integration-guide-nodejs.md`.

### 5.4 Quick manual test

```bash
curl -s -o test.pdf -X POST \
  -H "Content-Type: application/json" \
  -d '{"content":"<html><body><h1>Hello</h1></body></html>"}' \
  http://localhost:8080/v1/pdf/html
file test.pdf   # expect: PDF document
```

---

## 6. Production cutover sequence

Putting §2 through §5 together, in the order that keeps the old service as a safety net until
you're sure the new one works:

### 6.1 Validate side-by-side (old service still fully running, zero risk)

1. Deploy this service on a **port the old one isn't using** — check with `sudo ss -tlnp` first.
2. Take real HTML your current system generates (the actual final HTML you send to Puppeteer
   today, not a synthetic test case) and send it to the new service's `/v1/pdf/html`.
3. Compare the resulting PDF against what Puppeteer produces for the same input: page count,
   fonts, header/footer position, images, any non-Latin text you use. Check a few real templates,
   not just one.

### 6.2 Update the calling server's integration code

Since the request/response shape is different from whatever custom endpoint the old Node service
exposed, the caller needs new code (§5.3), not just a new URL. Deploy this behind a flag/config
value if you can, so switching back is a config change, not a redeploy.

### 6.3 Switch traffic

Flip the calling server's config to point at the new service. This is your (brief, accepted)
downtime window — a normal deploy, not an emergency. Watch closely:

```bash
journalctl -u go-dynamic-pdf-generator -f
curl -s localhost:8080/readyz
```

If anything looks wrong, flip the flag back — the old service is still running, untouched, so
rollback is immediate.

### 6.4 Retire the old service

Once you've watched real traffic succeed for a day or two (not just the first hour), go back to
§2.2 and §2.3 to stop it and reclaim its disk/memory footprint.

---

## 7. Ongoing operations

- **Watch this, not raw CPU%**: `curl localhost:8080/readyz` — `restart_attempts` climbing while
  `restarts` stays flat means Chromium can't start at all (bad path, out of memory, full disk), not
  a transient issue that will resolve itself.
- **Logs**: structured JSON on stdout, one line per request, viewable via
  `journalctl -u go-dynamic-pdf-generator -f`. Set `LOG_LEVEL=debug` only when actively diagnosing
  something — health probes are chatty at that level.
- **When to upgrade the VM, not just retune config**: if you see swap usage climbing under real
  traffic and response times degrading, that's this box's real ceiling, not a settings problem —
  move to a bigger instance (4GB+ RAM) rather than continuing to tune `POOL_SIZE`/concurrency
  downward.
- **Updating the binary**: rebuild, `scp` it up, `sudo install -m755 /tmp/pdfsvc /usr/local/bin/pdfsvc`,
  `sudo systemctl restart go-dynamic-pdf-generator`. The graceful shutdown built into the service means
  in-flight renders finish before the restart takes effect.
- **Recurring disk hygiene** (not just a one-time migration step): `sudo journalctl --vacuum-time=7d`
  and `sudo apt-get clean` are safe to run periodically, e.g. via a monthly cron job, on any small VM.
