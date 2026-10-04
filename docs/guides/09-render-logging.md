# 09 — Render logging (`pdf_render`) and cross-service reconciliation

Every PDF request to **this service** and to **pdf-service-saas (Node/Puppeteer)** writes one
structured JSON record named `pdf_render`. The two use the same field names and the same phase
names, so the records can be joined and compared. This is how we decide, with data, whether the
two services produce equivalent results and which one is faster.

> The field and phase names are a contract between the two repos. Node side:
> `pdf-service-saas/saas-pdf-backend/utils/helpers/pdfTrace.js` and
> `middlewares/requestTrace.js`. Go side: `internal/observability/trace.go` and
> `middleware.go`. Change one side only together with the other.

## Where the records go

| Service | Always | Dedicated file (on by default, no env needed) |
|---|---|---|
| Go | stdout (journald / `docker logs`) | `logs/pdf-render.jsonl` under the working directory; rotates at 50 MB, keeps 10 files |
| Node | — | `logs/pdf-render.jsonl` under the working directory; rotates at 50 MB, keeps 10 files |

**Go.** The defaults live in code (`observability.DefaultRenderLogConfig`), not in `.env`. The path
is relative, so it resolves against the process's working directory:

- **Development** — the repo root, so the file is `logs/pdf-render.jsonl` inside the repo. `logs/`
  is tracked only for its own `.gitignore` (`*` / `!.gitignore`), so the log files are never committed.
- **systemd** — the unit sets `WorkingDirectory=/var/lib/go-dynamic-pdf-generator` (the service's
  own writable state directory; the server has the binary only, no repo, and the rest of the
  filesystem is read-only), giving `/var/lib/go-dynamic-pdf-generator/logs/pdf-render.jsonl`. **An
  already-installed unit must be updated once** (copy the new unit, `systemctl daemon-reload`,
  restart) — until then the working directory is `/`, the file cannot be opened, and the service
  logs one `render log file unavailable` warning and carries on with stdout only.
- **Docker** — the image's `WORKDIR` is `/var/lib/go-dynamic-pdf-generator`, owned by the service
  user; `docker-compose.yml` keeps `logs/` in a named volume.

The file is a convenience copy: every record is also on stdout, so an unwritable location is a
warning, never a startup failure. The service logs the resolved absolute path at startup
(`render log file enabled`). Rotation is built in; no logrotate needed.

Optional override (not needed normally): `RENDER_LOG_FILE=/some/path.jsonl` moves the file, and
`RENDER_LOG_FILE=off` disables it.

Node's records are written only to its dedicated file (plus the console outside production), not to
`combined.log`; its `logs/` directory is already gitignored.

Both emit `time` (RFC 3339), `level`, `msg`. A request with no render activity (`/health`, `/livez`,
a request rejected by API-key auth) produces no `pdf_render` record.

## Request correlation

Both services honour an inbound `X-Request-Id` (`^[A-Za-z0-9_-]{1,64}$`, otherwise a fresh id is
generated), echo it in the response, and log it as `request_id`. If saas-backend sends one id to
both services, that id is the exact join key. Without it, join on `content_sha256` (below).

## Record shape

```json
{
  "time": "2026-10-04T10:23:17.524Z", "level": "INFO", "msg": "pdf_render",
  "engine": "node-puppeteer",            // or "go-chromedp"
  "request_id": "ab26ddd0…", "route": "POST /v1/pdf/html",
  "status": 200, "outcome": "ok",        // ok | error | aborted (Node only: client hung up)
  "content_bytes": 5840, "content_sha256": "5e37408cea02d41b",
  "options": { "paper_in": "8.5x11", "landscape": false, "fit_to_page": true,
               "embed_images": false, "overlay": false, "overlay_pages": "" },
  "images":  { "found": 3, "embedded": 2, "failed": 1, "skipped": 0, "bytes": 1234 },
  "output_bytes": 80148, "page_count": 1,
  "fit_scale": 0.655, "fit_overflow": false,
  "timings_ms": { "total": 667, "request_parse": 0.4, "queue_wait": 578.7, "render": 81.7, "…": 0 }
}
```

- **`content_sha256`** is the first 16 hex chars of SHA-256 over the raw `content` string exactly as
  the caller sent it (UTF-8, before image embedding or merging). The same document has the same
  fingerprint on both services — this is the reconciliation key. Only a hash and a length are
  logged, never the document.
- `images` appears only when `embedImages` ran. `found` counts distinct fetchable URLs.
- `page_count` is counted on the engine's own output *before* any overlay. It is omitted when it
  cannot be determined.
- `fit_scale` / `fit_overflow` appear only when `fitToPage` was requested.
- `overlay_reserved_px` appears only when a last-page overlay needed room reserved (see below). A
  value here explains why a document got one more page than it would without the overlay.
- `error_code` / `error_message` (≤200 chars) appear when `outcome` is not `ok`. Compare
  `error_code`, not the message: messages produced by Node's schema validator are worded
  differently from Go's.
- Requests rejected before options are parsed (Node schema failures) carry fewer `options` fields.

## Phases (`timings_ms`, milliseconds, 0.1 resolution)

`total` is the whole request, from just before the body is read to when the response is written.
Phases accumulate if they repeat. The overlay fragment render records the same phases prefixed with
`overlay_` (e.g. `overlay_print_pdf`).

| Phase | Meaning | Go | Node |
|---|---|---|---|
| `request_parse` | read body, parse, validate options | ✓ | ✓ |
| `embed_images` | fetch + inline remote `<img>` | ✓ | ✓ |
| `queue_wait` | waiting for a render slot | admission queue only | cluster queue **plus page creation** |
| `render` | engine time once running | ✓ (includes `acquire`, `tab_open`, …) | ✓ |
| `set_content` | inject the HTML | ✓ | ✓ |
| `wait_images` | wait for `<img>` to load | ✓ | ✓ |
| `viewport` | set viewport | fit-to-page only | always |
| `measure` | measure natural height | fit-to-page only | fit-to-page only |
| `print_pdf` | Chromium `printToPDF` | ✓ | ✓ |
| `overlay` | whole overlay step (incl. its render) | ✓ | ✓ |
| `reserve_overlay` | measure the last-page overlay and keep room for it | ~6 ms | ~60 ms |
| `overlay_stamp` | composite the fragment onto pages | pdfcpu | pdf-lib |
| `acquire` | pick a healthy Chromium instance (incl. health probe) | ✓ | — |
| `tab_open` | open a tab on the warm browser | ✓ | — (inside `queue_wait`) |
| `navigate`, `wait_ready`, `wait_fonts` | CDP steps | ✓ | — |

**Comparing like with like:** the two services slice the pre-render work differently. Compare
`total` and `render` for the headline; for the pre-render cost compare Node's `queue_wait` with Go's
`queue_wait + acquire + tab_open`. `output_bytes` differs between the services for the same input
and is not a correctness signal — judge equivalence by `page_count`, `fit_scale` and the rendered
PDF itself.

## Queries (jq)

Latency percentiles per engine (successful renders only):

```bash
jq -s 'map(select(.msg=="pdf_render" and .outcome=="ok")) | group_by(.engine)[]
  | {engine: .[0].engine, n: length,
     p50: (map(.timings_ms.total) | sort | .[length/2|floor]),
     p95: (map(.timings_ms.total) | sort | .[(length*0.95)|floor])}' \
  node-pdf-render.jsonl go-pdf-render.jsonl
```

Same document on both engines (matched by content hash) — pages and time side by side:

```bash
jq -s 'map(select(.msg=="pdf_render" and .outcome=="ok")) | group_by(.content_sha256)[]
  | select(map(.engine) | unique | length == 2)
  | {sha: .[0].content_sha256,
     pages: (map({(.engine): .page_count}) | add),
     total_ms: (map({(.engine): .timings_ms.total}) | add)}' \
  node-pdf-render.jsonl go-pdf-render.jsonl
```

Documents where the engines disagree on page count (the main equivalence alarm):

```bash
jq -s 'map(select(.msg=="pdf_render" and .outcome=="ok" and .page_count != null))
  | group_by(.content_sha256)[]
  | select((map(.engine) | unique | length == 2) and (map(.page_count) | unique | length > 1))
  | {sha: .[0].content_sha256, pages: (map({(.engine): .page_count}) | add)}' \
  node-pdf-render.jsonl go-pdf-render.jsonl
```

Slowest requests and where their time went:

```bash
jq -s 'map(select(.msg=="pdf_render")) | sort_by(-.timings_ms.total) | .[:10][]
  | {engine, request_id, timings_ms}' go-pdf-render.jsonl
```

On the Go host without a file, extract from the journal:
`journalctl -u go-dynamic-pdf-generator -o cat | jq -c 'select(.msg=="pdf_render")'`.

## What to conclude from it

- **Equivalence:** same `page_count`, same `fit_scale` (Node and Go currently differ on
  `fitToPage` — see the parity audit), same `error_code`, for the same `content_sha256`.
- **Speed:** compare `total` and `render` percentiles per engine over a representative window, not
  single requests. Look at `wait_images`: a value near 10 000 ms on Node is its 10 s image-wait
  fallback firing (a broken image that finished loading before the wait started).
- **Outliers:** the phase that dominates `total` names the cause.

## Last-page overlay never covers content

The disclaimer is stamped onto the finished PDF, and its box is taller than the 10 mm bottom print
margin, so on a nearly-full last page it used to sit on top of real text. Both services now measure
the overlay's bottom-anchored height inside the render tab (`reserve_overlay`) and append an empty
spacer so the last content cannot end underneath it. If the content would reach into that zone, the
spacer spills onto a new page and the disclaimer lands there instead — the document gains one page
and `overlay_reserved_px` is logged.

- Applies only to overlays that target the last page (`pages` omitted or `"last"`). An overlay on
  `first`, `all` or a numbered page cannot be guarded by a trailing spacer and is unchanged.
- Only elements touching the bottom edge of the overlay's page count, and whitespace the body
  already ends with (bottom padding/border/margin) is credited, so a template that already leaves
  room at the bottom reserves little or nothing.
- With `fitToPage`, the reserved zone is part of the measured height, so content is scaled to fit
  *with* the disclaimer instead of being covered by it.
- Best effort: if the measurement fails, the document renders exactly as before.
- Chromium never paints body content into the margin strip where the page number sits (tested with
  fixed/absolute/transformed/overflowing elements), so the page number cannot be overlapped by body
  content; this guard is only about the disclaimer box.
