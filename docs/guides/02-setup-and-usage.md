# How to Setup and Use This Project — Step by Step

This guide is written in simple steps. Follow it in order, and you should have the PDF service
running on your machine without any confusion.

## 1. What you need before starting (Prerequisites)

| Requirement | Why you need it | Notes |
|---|---|---|
| **Go 1.26 or newer** | This whole project is written in Go | Go's toolchain auto-downloads the exact patch version if needed |
| **A Chromium/Chrome binary** (headless-capable) | Used for the main HTML/Markdown → PDF path | Any recent Chrome, Chromium, or `chrome-headless-shell` build works. If you already have Puppeteer/Playwright installed in some other project on your machine, you likely already have one — just find its path. Otherwise install `chromium` (Linux) or download Chrome normally. |
| **Python 3 + WeasyPrint** *(optional)* | Only needed if you want the lighter, no-browser PDF path | Install with `pip install weasyprint`. WeasyPrint also needs a few system libraries (Pango, cairo, etc.) — check WeasyPrint's own official installation instructions for your operating system if `pip install` alone doesn't work. |

You do **not** need Node.js, Docker, or a database to simply run and try this project locally. Go
and a Chromium binary are enough to get the core feature working.

## 2. Get the code and build it

```bash
git clone <your-repo-url>
cd great-pdf-generator
go build ./...
```

If this finishes with no errors, your code compiles correctly. Good sign.

## 3. Set up the required settings (Environment Variables)

This project is configured using environment variables — simple `KEY=value` settings you set
before running the server. Here is the full, honest list:

| Variable | Required? | Default if not set | What it controls |
|---|---|---|---|
| `CHROMIUM_PATH` | **Yes** | *(server refuses to start without it)* | Full path to your Chromium/Chrome/`chrome-headless-shell` binary |
| `API_KEYS` | **Yes** *(unless `AUTH_DISABLED=true`)* | *(server refuses to start)* | Comma-separated API keys. Callers send one via `X-API-Key` or `Authorization: Bearer`. Health probes are exempt |
| `AUTH_DISABLED` | No | `false` | Set to `true` to run with no key check — local development only |
| `PDF_DEFAULT_PAPER` | No | `A4` | Deployment-wide default paper size (`Letter`, `Legal`, `A4`, …); a request's `options` overrides it |
| `PDF_DEFAULT_MARGIN_TOP` / `_RIGHT` / `_BOTTOM` / `_LEFT` | No | `0.4in` | Deployment-wide default margins (`0`, `10mm`, `0.5in`, …) |
| `EMBED_IMAGES_ENABLED` | No | `false` | Allow `options.embedImages` (fetch + inline remote `<img>` server-side) |
| `PORT` | No | `8080` | Which port the HTTP server listens on |
| `POOL_SIZE` | No | `2` | How many Chromium browser processes stay warm and ready |
| `MAX_CONCURRENCY_PER_INSTANCE` | No | `6` | How many PDF jobs one Chromium instance handles at the same time |
| `JOB_QUEUE_CAPACITY` | No | (same as `POOL_SIZE × MAX_CONCURRENCY_PER_INSTANCE`) | How many extra requests are allowed to wait in line before the server says "too busy, try later" |
| `WEASYPRINT_PATH` | No | *(feature turned off if not set)* | Full path to your `weasyprint` executable — set this only if you want the lighter, no-browser PDF path |
| `WEASYPRINT_WORKERS` | No | Number of CPU cores on your machine | Same idea as `MAX_CONCURRENCY_PER_INSTANCE`, but for the WeasyPrint path |
| `WEASYPRINT_QUEUE_CAPACITY` | No | Same as `WEASYPRINT_WORKERS` | Same idea as `JOB_QUEUE_CAPACITY`, but for the WeasyPrint path |
| `LOG_LEVEL` | No | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. Health probes only appear at `debug` |
| `SHUTDOWN_GRACE` | No | `5s` | How long readiness reports failure before the listener closes, so load balancers stop sending traffic before in-flight work drains |

**A simple rule of thumb for `POOL_SIZE`**: set it close to the number of CPU cores your machine
actually has. Adding more Chromium instances than you have cores does not make things faster — our
own real-world testing on this exact project proved that (see
`docs/research/load-test-results.md` if you're curious about the numbers).

## 4. Run the server

```bash
export CHROMIUM_PATH=/path/to/your/chromium-or-chrome-binary
# Optional, only if you want the lighter WeasyPrint path too:
export WEASYPRINT_PATH=/path/to/weasyprint

go run ./cmd/api
```

If everything is set up correctly, you should see something like:

```
listening on :8080 (pool size=2, max concurrency/instance=6, job queue workers=12 capacity=12)
```

That means your PDF service is now running and ready to accept requests on `http://localhost:8080`.

## 5. Quickly check that it's actually working

Open another terminal and run:

```bash
curl -s -w "\nstatus=%{http_code}\n" http://localhost:8080/readyz
```

You should get `status=200` plus a JSON body reporting live Chromium instances
and queue state. There are two probes, and the difference matters when you wire
up monitoring:

- **`/livez`** — "is this process wedged?" Deliberately does not depend on
  Chromium, because a restart is the wrong response to a pool that is already
  repairing itself.
- **`/readyz`** — "should this instance get traffic?" Reports 503 if no Chromium
  instance is alive, or while the service is shutting down. `/healthz` is kept
  as an alias of this for backwards compatibility.

For deploying with auto-restart, health probes, and multiple replicas, see
`04-deployment.md`.

Now try generating a real PDF:

```bash
curl -X POST http://localhost:8080/v1/pdf/html \
  -H "Content-Type: application/json" \
  -H "X-API-Key: <one of your API_KEYS>" \
  -d '{"content":"<html><body><h1>Hello, my first PDF!</h1></body></html>"}' \
  --output test.pdf

file test.pdf
```

(Without the key you get `401 {"error":{"code":"UNAUTHORIZED",...}}`. For local
development you can instead start the server with `AUTH_DISABLED=true`.)

If `file test.pdf` says something like `PDF document, version 1.4`, congratulations — it worked.

(See the third document, `03-integration-guide-nodejs.md`, for the full details on exactly what to
send and what you get back — request shape, error shape, and a proper Node.js example.)

## 6. Running the test suite (optional, for developers)

```bash
# Fast tests only (no real browser needed)
go test ./... -race

# Full tests, including real-Chromium and real-WeasyPrint checks
export CHROMIUM_PATH=/path/to/chromium
export WEASYPRINT_PATH=/path/to/weasyprint
go test ./... -race
```

There's also a heavier, opt-in load test that measures real speed under load — it's slow on
purpose, so it doesn't run by default:

```bash
export LOADTEST=1
go test ./internal/api/... -run TestLoadHeavyDocument -v -timeout 300s
```

## 7. Shutting down cleanly

Just press `Ctrl+C` in the terminal running the server, or send it a normal stop signal
(`SIGTERM`). The server is written to shut down gracefully — it finishes any in-flight requests and
closes every Chromium process properly before exiting, so you won't be left with leftover browser
processes eating memory in the background.

---

## Pros of this project

- **You own it completely** — no monthly subscription, no per-PDF pricing from a third party, no
  vendor lock-in.
- **Real browser accuracy** — since it uses actual Chromium, your PDF looks exactly like your HTML
  would look in a real browser tab. Modern CSS, web fonts, flexbox/grid — all supported properly.
- **Fast, warm-pool design** — the browser is kept running and reused between requests, so you
  don't pay a slow "cold start" cost on every single PDF.
- **Handles load safely** — built-in queue management means the system won't silently crash or hang
  under heavy traffic; it tells you clearly when it's at capacity.
- **Built-in template + data support** — one design, many outputs, safely (automatic protection
  against bad/malicious data sneaking into your PDF).
- **Catches a common, annoying bug automatically** — header/footer overlap is checked before
  printing, not left as a silent visual bug for your customers to notice.
- **One clean, consistent way to use every feature** — same request shape, same error shape,
  everywhere.
- **Simple to run** — just Go + a Chromium binary. No database, no message queue, no complicated
  infrastructure needed to get started.

## Cons / current limitations (be aware before using in production)

- **API key auth is basic** — one or more shared static keys (`API_KEYS`), checked in constant
  time on every conversion endpoint. There are no per-key scopes, no per-key rate limits, and no
  usage metering yet (see `docs/planning/SPEC-auth-and-tenancy.md`). Still keep it on a
  private/internal network; do not expose it directly to the public internet.
- **No usage tracking or per-key rate limiting** — a valid key can generate as many PDFs as it
  wants, as fast as it wants, up to the server's own global queue limit.
- **No background/async jobs** — every request waits for the PDF to finish generating right there
  and then. There's no "submit now, get notified later" mode for very large batches yet.
- **No file storage built in** — you get the PDF bytes back immediately; if you want to keep a copy
  saved somewhere (cloud storage, database, etc.), that's your application's job to do, not this
  service's.
- **Needs Chromium installed and running** — this is a real resource cost. A Chromium process uses
  real CPU and memory (roughly a few hundred MB per warm instance, more under heavy documents), so
  size your server accordingly.
- **Single-server design for now** — the request queue currently lives inside one running process.
  If you need multiple servers sharing one queue (for very high scale), that's a planned future
  upgrade, not available today.
- **No watermarking, custom font embedding, or PDF/A output yet.**
- **No visual/no-code template builder** — templates are plain HTML/Markdown text with
  `{{.fieldName}}` placeholders, written by a developer, not built in a drag-and-drop UI.

## When should you use this project?

**Good fit if:**
- You want full control over your PDF generation, without depending on (or paying) a third-party
  API.
- Your PDFs come from HTML/Markdown/CSS that you already control (invoices, reports, certificates,
  tickets, letters, etc.).
- You're comfortable running this as an internal service inside your own infrastructure, behind
  your own security layer.

**Maybe wait, or add more first, if:**
- You need this to be a public-facing, pay-per-use API with logins and billing today — that layer
  isn't built yet (though it's on the roadmap).
- You need guaranteed delivery via webhooks/async jobs for very large PDF batches — also not built
  yet.
