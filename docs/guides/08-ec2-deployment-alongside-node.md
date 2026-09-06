# Deploying `go-dynamic-pdf-generator` to EC2 (alongside the existing Node PDF service)

This is a step-by-step runbook for putting the Go PDF service on the **same EC2
box** that already runs the Node PDF service (`saas-pdf-backend`, pm2, port
1345) and the main backend, as a **new, independent service** with its **own
systemd unit**, its **own nginx server block**, and its **own subdomain**.
Nothing about the existing Node service changes until the final cutover step,
which is a one-line env change in `saas-backend` and is reversible.

Why this deploy matters: the disclaimer / "sticky footer" on reports is now
produced by this service via `options.overlay` (see
`docs/planning/SPEC-conversion-api.md`). Until `saas-backend` points at a build
that has that feature, report PDFs render with no footer.

---

## 0. Decide three values before you start

| Placeholder | What it is | Example |
|---|---|---|
| `<EC2_HOST>` | the box's SSH target | `ubuntu@ec2-…compute.amazonaws.com` |
| `<PDF_SUBDOMAIN>` | new DNS name for this service | `api-go-pdf.synectus.com` |
| `<API_KEY>` | a long random secret the backend will send as `X-API-Key` | `openssl rand -hex 32` |

- **Port:** this service listens on **`127.0.0.1:8080`** (localhost only). The
  Node service keeps `1345`. nginx is the only thing that talks to `:8080`.
- **Supervisor:** **systemd** (this repo ships the unit). The Node service uses
  pm2 because it's a Node app; a compiled Go binary belongs under systemd. If
  you strongly prefer pm2 for operational consistency, see Appendix B.
- **Install location:** `~/go-dynamic-pdf-generator` for the deploy user
  (`ubuntu`), mirroring `~/saas-pdf-backend`.

Add the DNS record now so it has time to propagate:

```
<PDF_SUBDOMAIN>.  A  <EC2 public IP>
```

---

## 0.5 If the box is 1 GB RAM — read this first

The current instance is **~1 GB RAM / ~30 GB disk**, already running the main
`saas-backend` **and** the Node PDF service (which itself launches Chromium).
That changes the plan:

**Disk is fine.** The whole footprint — `chrome-headless-shell` (~200 MB),
fonts (~150 MB), the Go binary (~15 MB), logs — is well under 30 GB. Nothing
below is disk-constrained.

**RAM is the hard limit, and it does not fit "alongside".** One Chromium
instance rendering a heavy multi-page report needs **~500–800 MB by itself**.
You cannot run *two* Chromium-based PDF services on 1 GB — the second render
will OOM-kill something (often `sshd`). So on this box:

1. **This is a migration, not an addition.** Deploy the Go service, cut
   `saas-backend` over (§3), then **immediately stop the Node PDF service**
   (`pm2 stop saas-pdf-backend && pm2 save`) to free its Chromium. Keep it
   *stopped, not deleted* for a few days for rollback. Follow
   `05-small-vm-production-and-migration-guide.md` for the retire-and-cleanup
   mechanics — this guide's "alongside" framing doesn't apply here.

2. **Add swap — mandatory on 1 GB.** Chromium's memory is bursty; swap turns
   "OOM-kill a random process" into "this one render is slow for a few
   seconds".

   ```bash
   sudo fallocate -l 3G /swapfile && sudo chmod 600 /swapfile
   sudo mkswap /swapfile && sudo swapon /swapfile
   echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
   sudo sysctl -w vm.swappiness=10          # only swap under real pressure
   echo 'vm.swappiness=10' | sudo tee /etc/sysctl.d/99-swappiness.conf
   ```

3. **Build the binary in CI, not on the box.** `go build` of this project
   spikes to ~400–600 MB and pulls a ~500 MB toolchain + build cache. On 1 GB
   that competes with everything else. Use `.github/workflows/deploy.yml`
   (which builds on a GitHub runner and `scp`s the result) — **skip step 1a
   and 1d below entirely**. For a one-off first deploy without CI, build on
   your laptop and `scp`:

   ```bash
   # on your machine, from the repo root:
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
     go build -trimpath -ldflags="-s -w" -o pdfsvc-linux ./cmd/api
   scp pdfsvc-linux <EC2_HOST>:/tmp/pdfsvc
   # on the box:
   sudo install -m 0755 /tmp/pdfsvc /usr/local/bin/pdfsvc
   ```
   (Use `GOARCH=arm64` for a Graviton instance.)

4. **Use `chrome-headless-shell`, not the `chromium` package.** It idles at
   ~120–180 MB vs ~350 MB for full Chromium, and has no snap confinement to
   fight. Install it per `05-small-vm-production-and-migration-guide.md` §3.4
   (the `.deb` shared libs + the standalone binary), not via
   `apt install chromium-browser` as step 1b suggests.

5. **The 1 GB config** for `/etc/go-dynamic-pdf-generator.env`:

   ```ini
   CHROMIUM_PATH=/opt/chrome-headless-shell/chrome-headless-shell
   PORT=8080
   LOG_LEVEL=info
   SHUTDOWN_GRACE=3s

   # One render at a time. MAX_CONCURRENCY_PER_INSTANCE=1 is honored as-is
   # (0 or unset would default to 6). A small queue absorbs a short burst;
   # anything past it gets 503 QUEUE_FULL, which the backend already retries.
   POOL_SIZE=1
   MAX_CONCURRENCY_PER_INSTANCE=1
   JOB_QUEUE_CAPACITY=6

   PDF_DEFAULT_PAPER=Letter
   PDF_DEFAULT_MARGIN_TOP=0
   PDF_DEFAULT_MARGIN_RIGHT=0
   PDF_DEFAULT_MARGIN_BOTTOM=10mm
   PDF_DEFAULT_MARGIN_LEFT=0
   PDF_DEFAULT_PRINT_BACKGROUND=true

   API_KEYS=<API_KEY>
   ```

   And in the systemd unit (`/etc/systemd/system/go-dynamic-pdf-generator.service`),
   lower the ceiling so systemd kills *this service* on a leak rather than the
   kernel picking `sshd`:

   ```ini
   MemoryMax=780M
   MemoryHigh=680M        # soft throttle before the hard cap
   ```

   `MemoryHigh` makes the kernel reclaim/throttle this cgroup as it approaches
   the limit instead of hard-OOMing at it — with swap present, a render near
   the ceiling slows down instead of dying.

6. **Expected throughput:** heavy 3-page reports at roughly **2–4 per minute,
   strictly one at a time**. That's fine for interactive "click generate, wait
   a few seconds" report creation. It is **not** enough for bulk/batch
   generation — if you need that, resize the box.

**The honest recommendation:** bump the instance to **2 GB** (`t3.micro` →
`t3.small`, a small monthly delta). 1 GB is below this repo's smallest
documented config, and a Chromium renderer on it is permanently one big report
away from an OOM. Everything above makes 1 GB *survivable*; 2 GB makes it
*comfortable* and lets you keep `POOL_SIZE=1, MAX_CONCURRENCY=2` for real
concurrency. The rest of this guide assumes you've either resized or applied
all six points above.

---

## 1. One-time host setup

SSH in: `ssh <EC2_HOST>`

### 1a. Install Go 1.26

Ubuntu's apt is too old for `go 1.26` (required by `go.mod`). Install the
official tarball:

```bash
GO_VERSION=1.26.0
curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tgz
rm /tmp/go.tgz

# Put it on PATH for interactive shells and for the deploy workflow.
echo 'export PATH=$PATH:/usr/local/go/bin' | sudo tee /etc/profile.d/go.sh
export PATH=$PATH:/usr/local/go/bin
go version    # expect: go version go1.26.0 linux/amd64
```

> On Graviton / ARM instances use `go${GO_VERSION}.linux-arm64.tar.gz`.

### 1b. Install Chromium and fonts

This is the real new dependency. Without fonts, text renders as blank boxes —
the classic "the PDF is empty" bug.

```bash
sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  chromium-browser \
  fonts-dejavu-core fonts-liberation fonts-noto-cjk \
  ca-certificates

# Find the actual binary path — it varies by Ubuntu release / snap:
which chromium-browser || which chromium || ls /usr/lib/chromium*/chromium /snap/bin/chromium 2>/dev/null
```

Note the path it prints — you need it for `CHROMIUM_PATH` below. Common values:
`/usr/bin/chromium-browser`, `/usr/bin/chromium`, `/snap/bin/chromium`.

> **Snap Chromium caveat:** if `which chromium` resolves to `/snap/bin/chromium`,
> the snap's confinement can block the service (which runs under a locked-down
> systemd sandbox) from executing it. Prefer the `.deb`:
> `sudo apt-get install -y chromium` on 22.04+, or add the
> `xtradeb/apps` / `savoury1/chromium` PPA. If you must use the snap, test
> `sudo -u pdfsvc /snap/bin/chromium --headless --version` before trusting it.

Quick sanity check that it runs headless at all:

```bash
"<CHROMIUM_PATH>" --headless --no-sandbox --disable-gpu --dump-dom about:blank >/dev/null && echo "chromium OK"
```

### 1c. Clone the repo

```bash
cd ~
git clone https://github.com/Maulik-008/go-dynamic-pdf-generator.git
cd go-dynamic-pdf-generator
git checkout main
```

### 1d. Build the binary

```bash
cd ~/go-dynamic-pdf-generator
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/pdfsvc ./cmd/api
sudo install -m 0755 /tmp/pdfsvc /usr/local/bin/pdfsvc
/usr/local/bin/pdfsvc -healthcheck || true   # prints a connection error — fine, service isn't up yet
```

### 1e. Create the service user and directories

```bash
sudo useradd --system --home /var/lib/go-dynamic-pdf-generator --create-home pdfsvc || true
```

### 1f. Install the systemd unit and env file

```bash
cd ~/go-dynamic-pdf-generator
sudo cp deploy/systemd/go-dynamic-pdf-generator.service /etc/systemd/system/
sudo cp deploy/systemd/go-dynamic-pdf-generator.env     /etc/go-dynamic-pdf-generator.env
sudo chmod 640 /etc/go-dynamic-pdf-generator.env
sudo chown root:pdfsvc /etc/go-dynamic-pdf-generator.env
```

Now edit `/etc/go-dynamic-pdf-generator.env` — the two lines that matter:

```bash
sudo nano /etc/go-dynamic-pdf-generator.env
```

```ini
CHROMIUM_PATH=<CHROMIUM_PATH from step 1b>
API_KEYS=<API_KEY from step 0>
```

Leave the `PDF_DEFAULT_*` block as shipped — it already mirrors the Node
service (`Letter`, 0 margins, 10mm bottom, print backgrounds) so requests that
send no options render identically. Tune `POOL_SIZE` to `(cores - 1)` if the
box has more or fewer than 4 cores.

### 1g. Start it

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now go-dynamic-pdf-generator
sudo systemctl status go-dynamic-pdf-generator --no-pager
```

Local smoke test (before nginx):

```bash
curl -s -w ' [%{http_code}]\n' http://127.0.0.1:8080/livez
curl -s -w ' [%{http_code}]\n' http://127.0.0.1:8080/readyz

KEY='<API_KEY>'
curl -s -o /tmp/out.pdf -w 'status=%{http_code}\n' -X POST \
  -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"<html><body><h1>deploy check 中文</h1></body></html>"}' \
  http://127.0.0.1:8080/v1/pdf/html
file /tmp/out.pdf     # expect: PDF document
```

Open `/tmp/out.pdf` (scp it down) and confirm the CJK characters are **not**
empty boxes — that verifies fonts.

---

## 2. nginx: new server block + TLS

```bash
cd ~/go-dynamic-pdf-generator
sudo cp deploy/nginx/go-dynamic-pdf-generator.conf \
  /etc/nginx/sites-available/go-dynamic-pdf-generator

# set your subdomain
sudo sed -i 's/<PDF_SUBDOMAIN>/api-go-pdf.synectus.com/' \
  /etc/nginx/sites-available/go-dynamic-pdf-generator

sudo ln -s /etc/nginx/sites-available/go-dynamic-pdf-generator /etc/nginx/sites-enabled/
sudo nginx -t
sudo systemctl reload nginx
```

Verify plain HTTP works through nginx:

```bash
curl -s -w ' [%{http_code}]\n' http://<PDF_SUBDOMAIN>/livez
```

Then issue the certificate (certbot rewrites the file to add the 443 block and
an HTTP→HTTPS redirect):

```bash
sudo certbot --nginx -d <PDF_SUBDOMAIN>
sudo nginx -t && sudo systemctl reload nginx
curl -s -w ' [%{http_code}]\n' https://<PDF_SUBDOMAIN>/livez
```

`certbot renew` is already on a timer from the existing setup; nothing to add.

### AWS security group

The box already allows 80/443 inbound for the other services, so there's
nothing to open. **Do not** open 8080 — it must stay localhost-only.

---

## 3. Cut `saas-backend` over to the new service

Edit `saas-backend/.env` on the box (wherever the main backend reads its env):

```ini
# was: PDF_SERVICE_URL=http://api-pdf.synectus.com   (Node)
PDF_SERVICE_URL=https://<PDF_SUBDOMAIN>
PDF_SERVICE_API_KEY=<API_KEY>
```

Restart the backend the way it's normally restarted (e.g. `pm2 restart
saas-backend`).

`services/pdfServiceClient.js` already speaks this service's contract
(`/v1/pdf/html`, `X-API-Key`, the `options` envelope, and `options.overlay` for
the disclaimer). No code change is needed for the cutover — only the two env
lines.

---

## 4. Post-cutover smoke checks (do all of these)

```bash
KEY='<API_KEY>'
BASE='https://<PDF_SUBDOMAIN>'

# 1. Health
curl -s -w ' [%{http_code}]\n' $BASE/livez
curl -s $BASE/readyz | jq .        # status:"ok", chromium.alive == chromium.size

# 2. Plain render
curl -s -o /tmp/a.pdf -w 'status=%{http_code}\n' -X POST -H "X-API-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"content":"<h1>hello</h1>","options":{"paperSize":"Letter"}}' $BASE/v1/pdf/html
file /tmp/a.pdf

# 3. THE OVERLAY / DISCLAIMER PATH — this is why you did this deploy.
#    A bad page selector must return 400. An OLD build returns 200 (ignores it).
curl -s -w ' <- %{http_code}\n' -X POST -H "X-API-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"content":"<h1>x</h1>","options":{"overlay":{"html":"<div>d</div>","pages":"nonsense"}}}' \
  $BASE/v1/pdf/html
#    EXPECT: {"error":{"code":"INVALID_REQUEST", ...}}  <- 400

#    And a real 2-page doc with a last-page overlay:
curl -s -o /tmp/ov.pdf -X POST -H "X-API-Key: $KEY" -H 'Content-Type: application/json' -d '{
  "content":"<html><body><p style=\"height:1100px\">one</p><p>two</p></body></html>",
  "options":{"paperSize":"Letter","overlay":{"pages":"last",
    "html":"<div style=\"position:fixed;bottom:0;left:0;width:100%;border-top:1px solid #000;font-size:9px\">Disclaimer — page {{totalPages}} of {{totalPages}}</div>"}}
}' $BASE/v1/pdf/html
#    scp /tmp/ov.pdf down: it must be 2 pages, with the box on page 2 ONLY,
#    and "{{totalPages}}" rendered as "2".

# 4. Real report through the app
#    Generate a report PDF from the frontend for a template that has a
#    "Sticky Footer Content" set. The disclaimer box must now appear on the
#    last page.
```

If check 3 fails (returns 200 for the bad selector, or the box is missing),
the running binary predates the overlay feature — rebuild:
`cd ~/go-dynamic-pdf-generator && git pull && go build ... && sudo systemctl restart go-dynamic-pdf-generator`.

---

## 5. Redeploying on future changes

### Manual

```bash
cd ~/go-dynamic-pdf-generator
git fetch --prune origin && git reset --hard origin/main
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/pdfsvc.new ./cmd/api
sudo install -m 0755 /tmp/pdfsvc.new /usr/local/bin/pdfsvc && rm /tmp/pdfsvc.new
sudo systemctl restart go-dynamic-pdf-generator
curl -fsS http://127.0.0.1:8080/readyz && echo ' OK'
```

The restart is graceful: readiness fails first, in-flight renders drain, then
the process exits and systemd starts the new binary. A request in flight during
the restart may get a `503` — the backend already retries `QUEUE_FULL` /
`ENGINE_UNAVAILABLE`.

### Automated (GitHub Actions)

`.github/workflows/deploy.yml` in this repo does the above on push to `main`.
Add three repo secrets: `EC2_SSH_KEY`, `EC2_HOST`, `EC2_USER`. It builds to a
temp path and only swaps the binary on a successful build, then waits for
`/readyz` and fails the run if the service doesn't come up.

---

## 6. Rollback

The cutover is one env var. To revert to the Node service:

```ini
# saas-backend/.env
PDF_SERVICE_URL=http://api-pdf.synectus.com
PDF_SERVICE_API_KEY=<the Node service key>
```

…and restart the backend. The Go service can keep running; nothing depends on
it once the backend points elsewhere. (You lose the last-page disclaimer again
until you cut back.)

To stop the Go service entirely:

```bash
sudo systemctl disable --now go-dynamic-pdf-generator
sudo rm /etc/nginx/sites-enabled/go-dynamic-pdf-generator
sudo systemctl reload nginx
```

---

## 7. Operations

| Task | Command |
|---|---|
| Logs (live) | `sudo journalctl -u go-dynamic-pdf-generator -f` |
| Logs (since boot) | `sudo journalctl -u go-dynamic-pdf-generator -b` |
| Restart | `sudo systemctl restart go-dynamic-pdf-generator` |
| Status + last logs | `sudo systemctl status go-dynamic-pdf-generator` |
| Health JSON | `curl -s localhost:8080/readyz \| jq .` |
| Correlate a failed request | grep the `X-Request-Id` the caller got in journald |

Logs are structured JSON, one line per request, at `LOG_LEVEL=info` (set
`debug` in `/etc/go-dynamic-pdf-generator.env` + restart when diagnosing). A
`503` logs at `warn` and is usually a designed response (load shedding /
draining), not a fault. The field to alert on is `restart_attempts` climbing
while `restarts` stays flat — that means Chromium can't start (bad path, OOM,
full disk) and needs a human. See `docs/guides/04-deployment.md` §4 and
`docs/guides/06-ec2-monitoring-alarms-best-practices.md`.

### Resource footprint on a shared box

Chromium is memory-hungry: budget **~800 MB per pool instance** under heavy
load (not the ~350 MB idle figure). With `POOL_SIZE=3` that's ~2.4 GB plus the
Go process. `MemoryMax=3G` in the unit is the ceiling; make sure the box has
that much headroom over the Node service and the backend, or lower `POOL_SIZE`.
Measured throughput on 4 cores: ~26 heavy 3-page renders/sec.

---

## 8. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `systemctl status` shows `activating (auto-restart)` looping, then `failed` | Bad `CHROMIUM_PATH`, or Chromium can't run. Check `journalctl -u go-dynamic-pdf-generator`. Test `sudo -u pdfsvc <CHROMIUM_PATH> --headless --no-sandbox --dump-dom about:blank`. |
| Service starts but `/readyz` says `chromium.alive: 0` | Chromium is present but crashing on launch — often the snap confinement issue (§1b) or missing shared libs. Switch to the `.deb` Chromium. |
| PDFs render but **text is blank boxes** | Fonts not installed for the `pdfsvc` user's environment. Re-run the `fonts-*` apt install; `fc-list \| head`. |
| `502 Bad Gateway` from nginx | Service isn't listening on `127.0.0.1:8080`. `sudo systemctl status …`, `ss -ltnp \| grep 8080`. |
| `413 Request Entity Too Large` | Raise `client_max_body_size` in the nginx block (default here is 12m). |
| Report PDF has **no disclaimer / sticky footer** | The running binary predates `options.overlay`. Confirm with smoke check #3 (bad selector should be `400`, not `200`). Rebuild from `main` and restart. |
| `504 Gateway Timeout` on big reports | Raise `proxy_read_timeout` / `proxy_send_timeout` in the nginx block above the service's render timeout. |
| Backend logs `ECONNREFUSED` / TLS errors to the PDF service | `PDF_SERVICE_URL` wrong, or certbot not run yet. `curl https://<PDF_SUBDOMAIN>/livez` from the box. |
| `401 UNAUTHORIZED` from the service | `API_KEYS` in `/etc/go-dynamic-pdf-generator.env` doesn't match `PDF_SERVICE_API_KEY` in the backend. |

---

## Appendix A — Docker instead of a native binary

If the box already runs Docker and you'd rather containerise:

```bash
cd ~/go-dynamic-pdf-generator
API_KEYS='<API_KEY>' docker compose up -d --build
```

`docker-compose.yml` ships `restart: unless-stopped`, `shm_size: 1gb`, a
compose-level healthcheck, and memory limits. Remove the `ports: "8080:8080"`
mapping and bind to `127.0.0.1:8080:8080` so only nginx can reach it. Everything
else in this guide (nginx, TLS, cutover, smoke checks) is unchanged. See
`docs/guides/04-deployment.md` §2 for the container specifics and the crash /
auto-restart checks.

## Appendix B — pm2 instead of systemd

Works, but pm2 is a Node process manager and won't kill the forked Chromium
child processes cleanly on restart (systemd's `KillMode=control-group` does).
If you accept that:

```bash
cd ~/go-dynamic-pdf-generator
go build -trimpath -ldflags="-s -w" -o ./pdfsvc ./cmd/api

# pm2 doesn't read .env files; pass env inline or via an ecosystem file.
CHROMIUM_PATH=<path> API_KEYS='<API_KEY>' PORT=8080 \
  PDF_DEFAULT_PAPER=Letter PDF_DEFAULT_MARGIN_BOTTOM=10mm PDF_DEFAULT_PRINT_BACKGROUND=true \
  pm2 start ./pdfsvc --name go-dynamic-pdf-generator --time
pm2 save
```

Redeploy = `git pull && go build … && pm2 restart go-dynamic-pdf-generator`.
Prefer systemd unless you have a strong reason.
