# 10 — Deploy runbook: Go PDF service + saas-backend

Step-by-step for shipping a change to production. Written from the real server (EC2, Ubuntu,
systemd + pm2 + nginx), not from the generic guides. Follow it top to bottom; each step says what a
good result looks like, so you know when to stop and look.

For the one-time server setup see [08-ec2-deployment-alongside-node.md](08-ec2-deployment-alongside-node.md).
For reading the render logs see [09-render-logging.md](09-render-logging.md).

---

## 0. How production is laid out

```
saas-backend (pm2: saas-backend-pdf)  ──HTTP + X-API-Key──▶  nginx :443  ──▶  Go service :8080
                                                                               (systemd, user pdfsvc)
```

| Piece | Where / how it runs |
|---|---|
| Go service | systemd unit `go-dynamic-pdf-generator`, binary `/usr/local/bin/pdfsvc`, listens on `:8080` |
| Go config | `/etc/go-dynamic-pdf-generator.env` (secrets: `API_KEYS`, `CHROMIUM_PATH`, pool sizes). **Never overwrite this file** |
| Unit drop-ins | `/etc/systemd/system/go-dynamic-pdf-generator.service.d/` — `chromium.conf`, `workingdir.conf` |
| Go state + render log | `/var/lib/go-dynamic-pdf-generator/logs/pdf-render.jsonl` (owned by `pdfsvc`; use `sudo`) |
| Chromium for Go | `/opt/chrome-headless-shell/chrome-headless-shell` |
| saas-backend | pm2 app `saas-backend-pdf` (other pm2 apps on the box: `saas-backend-cron`, `next_level_clinic_live`) |
| Which PDF service saas-backend calls | `PDF_SERVICE_URL` in saas-backend's `.env` |

As observed on 2026-10-04: ~1.9 GB RAM, ~14 GB disk (about 89% used), Go pool size 1 with 2
concurrent renders, `MemoryMax` about 1.3 GB. This is a small box — see §7.

**Fill in once and keep here:**

- Go repo checkout on the server: `______________________` (it is **not** `~/go-dynamic-pdf-generator`
  on this box; find it with `sudo find / -maxdepth 4 -type d -name 'go-dynamic-pdf-generator' 2>/dev/null`)
- saas-backend checkout: `______________________` (get it with
  `pm2 describe saas-backend-pdf | grep -E "exec cwd|script path"`)

---

## 1. Before you start (2 minutes)

```bash
# Enough disk? Builds and logs need room. Aim for > 2 GB free.
df -h / | tail -1

# Is the service healthy right now? (so you know a problem is yours, not pre-existing)
curl -s -m 3 -w ' [HTTP %{http_code}]\n' localhost:8080/readyz

# Note what is running, for rollback
cd <GO_CHECKOUT>        && git log -1 --format='Go now at: %h %s'
cd <SAAS_BACKEND_CHECKOUT> && git log -1 --format='saas-backend now at: %h %s'
```

Deploy at a quiet time if you can. The restart is graceful (readiness fails, in-flight renders
finish), but a request that lands mid-restart may get a `503`, which saas-backend already retries.

**Order:** deploy the **Go service first**, then **saas-backend**. A new Go build accepts everything
the old saas-backend sends, so this order is always safe; the reverse can briefly send new options to
an old service.

---

## 2. Deploy the Go service

```bash
cd <GO_CHECKOUT>
git fetch --prune origin
git pull            # or: git reset --hard origin/main   (discards local edits on the server)
git log -1 --format='deploying: %h %s'

# Build to a temp path first — a failed build never replaces the working binary.
export PATH=$PATH:/usr/local/go/bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/pdfsvc.new ./cmd/api

# Keep the current binary so you can roll back in seconds.
sudo cp /usr/local/bin/pdfsvc /usr/local/bin/pdfsvc.prev

# Swap and restart.
sudo install -m 0755 /tmp/pdfsvc.new /usr/local/bin/pdfsvc && rm /tmp/pdfsvc.new
sudo systemctl restart go-dynamic-pdf-generator

# Wait for healthy.
for i in $(seq 1 30); do curl -fsS -o /dev/null localhost:8080/readyz && { echo "healthy after ${i}s"; break; }; sleep 1; done
```

**Good result:** `healthy after Ns`, and

```bash
sudo journalctl -u go-dynamic-pdf-generator -n 20 --no-pager
```

shows `render log file enabled`, `api-key auth enabled`, `listening on :8080`.

- **Build gets killed / out of memory** (small box): build on your laptop or in CI for linux/amd64
  (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o pdfsvc-linux ./cmd/api`),
  `scp` it to `/tmp/pdfsvc.new`, then run the swap/restart lines above.
- **Go build cache eats disk:** run `go clean -cache` after a successful deploy.

### If the change touched the systemd unit or env

The deploy above only swaps the binary. The unit and env file are **not** updated automatically.

- **Unit changed** (`deploy/systemd/go-dynamic-pdf-generator.service`): do not blindly overwrite the
  installed one — this box has drop-ins and may have local tweaks. Prefer a drop-in:
  ```bash
  sudo mkdir -p /etc/systemd/system/go-dynamic-pdf-generator.service.d
  printf '[Service]\n<the changed setting>\n' | sudo tee /etc/systemd/system/go-dynamic-pdf-generator.service.d/<name>.conf
  sudo systemctl daemon-reload && sudo systemctl restart go-dynamic-pdf-generator
  ```
  See what systemd really uses with `systemctl cat go-dynamic-pdf-generator`.
- **A new setting needed in the env file:** edit it in place, never copy the repo's sample over it:
  `sudo nano /etc/go-dynamic-pdf-generator.env`, then restart. Check the key names without printing
  secrets: `sudo sed -E 's/=.*/=<hidden>/' /etc/go-dynamic-pdf-generator.env | grep -vE '^\s*(#|$)'`.

---

## 3. Deploy saas-backend (`saas-backend-pdf`)

```bash
cd <SAAS_BACKEND_CHECKOUT>
git pull
git log -1 --format='deploying: %h %s'

# Only if package.json / the lockfile changed — use the same tool the repo uses (pnpm or npm):
#   pnpm install      (or: npm ci)

pm2 restart saas-backend-pdf --update-env   # --update-env picks up .env changes
pm2 logs saas-backend-pdf --lines 30 --nostream
pm2 list
```

**Good result:** status `online`, no stack traces in the logs, and the `↺` restart counter does
not keep climbing (a counter that goes up every few seconds means a crash loop — read the logs).

- **Other pm2 apps may run the same checkout.** Check each with
  `pm2 describe <name> | grep "exec cwd"`. If `saas-backend-cron` or another app runs from the same
  folder and uses the changed code, restart it too, otherwise it keeps the old code in memory.
- You only need `pm2 save` if you added or removed an app from pm2, not for a normal restart.
- Changed `PDF_SERVICE_URL` / `PDF_SERVICE_API_KEY`? That is a `.env` change: edit the file, then
  `pm2 restart saas-backend-pdf --update-env`.

---

## 4. Verify (do all of these)

**1. Health**
```bash
curl -s localhost:8080/readyz | jq .         # status ok, chromium alive == size
```

**2. A render works and is being logged**

Generate a report from the app, then:
```bash
sudo journalctl -u go-dynamic-pdf-generator -n 5 --no-pager | grep pdf_render | tail -1 | jq .
sudo tail -1 /var/lib/go-dynamic-pdf-generator/logs/pdf-render.jsonl | jq .
```
Look for `"status":200`, a sensible `page_count`, and `timings_ms.total`.

**3. Input validation still behaves** (reads the key into a variable without printing it)
```bash
KEY=$(sudo sed -n 's/^API_KEYS=//p' /etc/go-dynamic-pdf-generator.env | cut -d, -f1)
curl -s -o /dev/null -w 'bad overlay selector -> HTTP %{http_code} (expect 400)\n' \
  -X POST localhost:8080/v1/pdf/html -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
  -d '{"content":"<p>x</p>","options":{"overlay":{"html":"<b>x</b>","pages":"abc"}}}'
curl -s -o /dev/null -w 'no key -> HTTP %{http_code} (expect 401)\n' -X POST localhost:8080/v1/pdf/html \
  -H 'Content-Type: application/json' -d '{"content":"<p>x</p>"}'
unset KEY
```

**4. Look at the PDF from the real app** (the part tests can't judge)

- Page numbers are the same small size and position on every page, including the last.
- On a report with a disclaimer, the disclaimer box never sits on top of text. If the last page was
  nearly full it may now sit on its own extra page — that is intended.
- Fonts look right. If text looks different from before, check the font is installed:
  `fc-list | grep -i carlito` (if empty: `sudo apt install fonts-crosextra-carlito`, then restart Go).

---

## 5. Roll back

**Go service** (needs the `pdfsvc.prev` you saved in §2):
```bash
sudo install -m 0755 /usr/local/bin/pdfsvc.prev /usr/local/bin/pdfsvc
sudo systemctl restart go-dynamic-pdf-generator
curl -fsS localhost:8080/readyz && echo ' OK'
```

**saas-backend:**
```bash
cd <SAAS_BACKEND_CHECKOUT>
git checkout <previous-commit-from-§1>     # or: git revert <bad-commit> and pull
pm2 restart saas-backend-pdf --update-env
```

**Switch saas-backend back to the Node PDF service** (if it is still deployed): set `PDF_SERVICE_URL`
(and the matching `PDF_SERVICE_API_KEY`) in saas-backend's `.env`, then
`pm2 restart saas-backend-pdf --update-env`.

---

## 6. Everyday operations

| Task | Command |
|---|---|
| Live Go logs | `sudo journalctl -u go-dynamic-pdf-generator -f` |
| Only render records | `sudo journalctl -u go-dynamic-pdf-generator -o cat \| jq -c 'select(.msg=="pdf_render")'` |
| Slowest 10 renders | `sudo jq -s 'map(select(.msg=="pdf_render")) \| sort_by(-.timings_ms.total) \| .[:10][] \| {request_id,total:.timings_ms.total,page_count}' /var/lib/go-dynamic-pdf-generator/logs/pdf-render.jsonl` |
| Service status | `systemctl status go-dynamic-pdf-generator --no-pager` |
| Restart Go | `sudo systemctl restart go-dynamic-pdf-generator` |
| Current unit incl. drop-ins | `systemctl cat go-dynamic-pdf-generator` |
| pm2 status / logs | `pm2 list` / `pm2 logs saas-backend-pdf --lines 50 --nostream` |

---

## 7. Keep an eye on: disk and memory

This box is small, and a full disk is the most likely cause of a confusing failure (Chromium cannot
start, logs cannot be written).

```bash
df -h / | tail -1                 # keep > 1 GB free
sudo du -sh /var/lib/go-dynamic-pdf-generator/logs        # render log, capped at ~550 MB (50 MB x 11 files)
sudo journalctl --disk-usage      # systemd journal
sudo journalctl --vacuum-size=200M   # shrink it if it is large
free -m                           # "available" should not sit near zero
```

If disk stays tight, lower the render-log cap (`MaxBytes` / `MaxFiles` in
`internal/observability/renderlog.go`) or turn the file off with `RENDER_LOG_FILE=off` in the env
file — records still go to journald either way.

---

## 8. Troubleshooting

| Symptom | Check |
|---|---|
| `go build` fails | Read the error; `git log -1` to confirm you pulled what you meant. The old binary is untouched, nothing is down. |
| Service won't start after restart | `sudo journalctl -u go-dynamic-pdf-generator -n 50 --no-pager`. Common: bad `CHROMIUM_PATH`, full disk, missing/typo'd env value. Roll back (§5). |
| `start request repeated too quickly` | Five failures in five minutes stops auto-restart. Fix the cause, then `sudo systemctl reset-failed go-dynamic-pdf-generator && sudo systemctl restart go-dynamic-pdf-generator`. |
| Logs show `render log file unavailable` | Service is fine (records still go to journald). The working directory is not writable — confirm `workingdir.conf` exists and `systemctl cat go-dynamic-pdf-generator` shows `WorkingDirectory=/var/lib/go-dynamic-pdf-generator`, then `daemon-reload` + restart. |
| App gets `401` from the PDF service | `PDF_SERVICE_API_KEY` in saas-backend's `.env` must equal one of `API_KEYS` in `/etc/go-dynamic-pdf-generator.env`. |
| App gets `503` `QUEUE_FULL` / `ENGINE_UNAVAILABLE` | Load spike or Chromium restarting. saas-backend retries these; if constant, check `curl localhost:8080/readyz` (`restart_attempts` climbing while `restarts` stays flat = Chromium cannot start). |
| nginx `502` / `504` | `curl localhost:8080/readyz` on the box. If that is fine, check the nginx `proxy_read_timeout` (should be 120s, above Go's 45s render + 55s write timeouts). |
| PDF looks different after deploy | Did you deploy **both** repos? Page-number footer and font changes are in saas-backend; overlay/footer-guard and logging are in Go. Some PDFs (calendar, patient documents) are rendered locally by saas-backend with its own code and do not go through Go at all. |
| pm2 app keeps restarting (`↺` rising) | `pm2 logs saas-backend-pdf --lines 100 --nostream` — usually a missing env var or a dependency that needs installing. |

---

## 9. About the automated deploy (GitHub Actions)

`.github/workflows/deploy.yml` runs on push to `main` and does the build + install + restart from
§2. Two things to know:

- It assumes the repo is at `~/go-dynamic-pdf-generator` on the server. On this box it is not, so
  until the path in the workflow (or the checkout location) is fixed it fails with
  `repo not found — run one-time host setup first`. Manual deploys (this document) are unaffected.
- It never updates the systemd unit or the env file. Anything in §2's "unit changed / env changed"
  section is always a manual step, even when the binary is deployed automatically.
