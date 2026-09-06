# Spec: conversion-api (v1 — unified envelope, structured errors, request validation)

Module spec under `CAPABILITY-MAP.md`. Responsibility: *"The public HTTP surface: one endpoint per
conversion type, request validation, consistent request/response envelope, error model."* Every
prior module's spec in this repo has explicitly deferred this: *"Deliberately minimal... full
request validation, the error envelope... belong to the conversion-api... module, not this slice"*
(`SPEC-render-engines.md`, `SPEC-lightweight-renderer.md`, `SPEC-job-orchestration.md` all say some
version of this). This is that module — it replaces the ad-hoc raw-body/mixed-JSON routes built
incrementally across those prior slices with the real, consistent surface, per
`PLATFORM-SPEC.md`'s explicit requirement: *"One consistent request/response envelope across every
conversion type — html, markdown, url, image, and manipulation endpoints all shaped the same way.
One mental model, not five."*

## What changes, concretely

Before this module: `/v1/pdf/html` and `/v1/pdf/markdown` took a raw document body;
`/v1/pdf/html-template`/`/v1/pdf/markdown-template` took a JSON body (`{template, payload}`);
`/v1/pdf/html-lite` took a raw body with no payload-merge option at all — four different request
shapes for what is, from a caller's perspective, one operation (render a document) with an optional
feature (payload merge) and a choice of engine. This is exactly the "one mental model, not five"
problem the module exists to fix.

**After**: every conversion endpoint takes the same JSON shape, `{"content": "...", "payload":
{...}?}` — `payload` is optional; when present, `content` is treated as a template and merged via
the existing `customization.Merge` before rendering, exactly like the (now removed)
`-template` routes did. `/v1/pdf/html-template` and `/v1/pdf/markdown-template` are removed, not
kept alongside the unified routes — this is a deliberate consolidation, not scope creep: nothing
outside this repo depends on the old shape yet (pre-launch, no external customers), and maintaining
two request shapes for the same capability would itself violate the "one mental model" requirement
this module exists to satisfy.

Every error response (validation, template-merge failure, render failure, queue-full) becomes a
consistent JSON envelope instead of a plain-text `http.Error` body, per api-and-interface-design's
"pick one error strategy and use it everywhere" principle:

```json
{"error": {"code": "TEMPLATE_ERROR", "message": "customization: execute template: ..."}}
```

## Objective

Give every conversion endpoint (`/v1/pdf/html`, `/v1/pdf/markdown`, `/v1/pdf/html-lite`) the same
request shape, the same structured error shape, and real request validation at the boundary —
turning the accumulated-by-necessity minimal routes from prior slices into the platform's real,
stable public surface.

**Explicitly not goals for this slice** (deferred, with reasoning, not silently dropped — see
Boundaries): async job-id + webhook dispatch (needs `storage-and-delivery`, which doesn't exist
yet); sandbox/test mode (its defining property, per `PLATFORM-SPEC.md`, is *watermarked* free
output — watermarking is still deferred in `customization-layer`, so sandbox mode has nothing real
to deliver yet); `Idempotency-Key` enforcement (see reasoning below — this becomes a meaningful
requirement once a metered/billed side effect exists to deduplicate, which is `auth-and-tenancy`'s
job, not yet built); URL/image input and PDF manipulation (still explicitly out of v1 scope per
every prior spec).

### Why `Idempotency-Key` is deferred, not just forgotten

`PLATFORM-SPEC.md` lists `Idempotency-Key` as a v1 requirement, and `api-and-interface-design`'s own
checklist calls for it on state-changing endpoints. But every conversion endpoint today is a pure,
side-effect-free compute operation: no database row is created, no quota is deducted (no
`auth-and-tenancy` yet), no async job is enqueued (no `storage-and-delivery` yet) — a retried
request just re-renders and returns equivalent bytes, with no double-effect to guard against.
Real idempotency-key handling (per `api-and-interface-design`: claim the key atomically against a
store, guard the payload hash, decide the in-flight-duplicate response) needs a persistent store to
claim against — that's `storage-and-delivery`'s dependency, not this module's. Building a
half-mechanism now (accept the header, do nothing real with it) would be worse than not building it:
it would tell a client retrying is safe when the guarantee doesn't actually exist yet. Honored once
a metered or persisted side effect exists to deduplicate against.

## Tech Stack

No new dependency — `encoding/json` (stdlib) for the envelope, reusing every existing engine call
(`renderengines.Pool`, `lightrender.Renderer`, `customization.Merge`, `orchestration.Pool`)
unchanged. This module is purely an HTTP-boundary reshaping, not a new render capability.

## Project Structure

```
internal/api/
  handlers.go       → conversionRequest, errorEnvelope, writeJSONError, decodeConversionRequest,
                       mergeIfNeeded, writePDF, handleHTML/handleMarkdown/handleHTMLLite
  handlers_test.go  → rewritten for the JSON envelope (was raw-body/mixed-JSON before)
  loadtest_test.go  → request construction updated to the JSON envelope; behavior unchanged
```

## API Shape

Request (identical shape on every conversion endpoint):

```json
{
  "content": "<html>...{{.customerName}}...</html>",
  "payload": { "customerName": "Ada" }
}
```

- `content` (string, required) — the document body (HTML, Markdown, or WeasyPrint-targeted HTML,
  depending on which endpoint), or a template if `payload` is present.
- `payload` (object, optional) — when present, `content` is merged via `customization.Merge` before
  rendering (see `SPEC-customization-layer.md`); when absent, `content` is rendered as-is.

Success response: raw PDF bytes, `Content-Type: application/pdf` — **not** JSON-wrapped. This is a
deliberate exception to "consistent envelope," matching the category convention already cited in
`docs/research/go-pdf-generation-research.md` (DocRaptor, PDFShift, cloudlayer.io all return raw
PDF bytes on synchronous success, reserving JSON for errors and async job status). Base64-wrapping
a binary PDF in JSON would only make the common case worse for no real consistency benefit.

Error response, every failure mode, every endpoint:

```json
{"error": {"code": "TEMPLATE_ERROR", "message": "..."}}
```

| Code | Status | When |
|---|---|---|
| `INVALID_REQUEST` | 400 | malformed JSON, empty body, missing `content` |
| `REQUEST_TOO_LARGE` | 413 | body exceeds `maxBodyBytes` |
| `TEMPLATE_ERROR` | 422 | `customization.Merge` failed (bad syntax, missing payload field) |
| `RENDER_ERROR` | 422 | the underlying render engine failed |
| `QUEUE_FULL` | 503 | `orchestration.ErrQueueFull` (existing behavior; `Retry-After` header kept) |
| `NOT_CONFIGURED` | 503 | `/v1/pdf/html-lite` called with no `WEASYPRINT_PATH` configured (existing behavior) |

## Testing Strategy

- Rewrite every existing `internal/api/handlers_test.go` test for the new JSON request/error shape
  — same behaviors as before (success, empty body, render error, queue-full, missing static
  renderer), now asserting the JSON error envelope's `code` field instead of a plain-text body.
- New tests specific to this module: malformed JSON body → 400 `INVALID_REQUEST`; missing `content`
  → 400 `INVALID_REQUEST`; a `payload` present with a bad template → 422 `TEMPLATE_ERROR`; the same
  request shape and merge behavior verified on `/v1/pdf/html-lite` too (payload merge is
  engine-agnostic, so WeasyPrint gets it for free, not as a separate feature).
- Live-Chromium and live-WeasyPrint end-to-end tests updated to the new request shape, proving the
  real wiring still works end-to-end, not just against fakes.
- `internal/api/loadtest_test.go`'s request construction updated to the JSON envelope — the load
  test's actual measurement (latency/throughput/backpressure) is unaffected, only how the request is
  built.

## Boundaries

- **Always**: every error response uses the JSON envelope, never a plain-text `http.Error` body;
  validate `content` is present before any rendering work starts; keep the Chromium and WeasyPrint
  render paths' own separation intact (payload merge is now shared plumbing at the HTTP boundary,
  but the actual render call still goes through the same distinct, never-shared engine methods).
- **Ask first**: JSON-wrapping the successful PDF response (binary-in-JSON) instead of raw bytes;
  building partial `Idempotency-Key` persistence ahead of `storage-and-delivery`; adding sandbox mode
  ahead of `customization-layer`'s watermark slice.
- **Never**: return a plain-text or HTML error body from a conversion endpoint; accept a request
  with no `content` and attempt to render anyway; remove the JSON error envelope for one endpoint
  while keeping it for others (the entire point is one consistent shape).

## Success Criteria

- [x] Every conversion endpoint (`/v1/pdf/html`, `/v1/pdf/markdown`, `/v1/pdf/html-lite`) accepts the
      same `{"content", "payload"?}` JSON shape (`decodeConversionRequest`, shared across all three)
- [x] Every error response across every endpoint uses the same `{"error": {"code", "message"}}`
      envelope, with the status/code mapping in the API Shape table above (`writeJSONError`) —
      verified by test for every code (`INVALID_REQUEST`, `TEMPLATE_ERROR`, `RENDER_ERROR`,
      `QUEUE_FULL`, `NOT_CONFIGURED`) and live via curl
- [x] Payload merge works identically on the WeasyPrint path as on the Chromium path
      (`TestHandleHTMLLite_WithPayload_Success`, live curl) — proving it's genuinely
      engine-agnostic plumbing, not a Chromium-only feature
- [x] `/v1/pdf/html-template` and `/v1/pdf/markdown-template` are removed, their capability fully
      absorbed into the unified `content`+`payload` shape on the main routes (confirmed no
      remaining references anywhere in the codebase)
- [x] `go test ./... -race` green, with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH` set
- [x] Live curl verification: success (html, html-lite, both with payload), each error code in the
      table (`INVALID_REQUEST` x2, `TEMPLATE_ERROR`, `QUEUE_FULL` with `Retry-After: 1`), `healthz`
      — all through the new envelope; clean SIGTERM shutdown (zombie Chromium processes observed
      transiently, reaped within 2s — not a leak)

## Open Questions

None blocking v1 as scoped above. Deferred, with reasoning recorded above (not silently dropped):
`Idempotency-Key` enforcement (needs `storage-and-delivery`'s persistence), sandbox/test mode (needs
`customization-layer`'s watermark slice), async job-id + webhook dispatch (needs
`storage-and-delivery`), API-key auth and rate limiting (`auth-and-tenancy`, a separate module).

---

## v2 — options envelope (Synectus Medico integration)

Added so `pdf-service-saas` (Node/Puppeteer) can be replaced by this service without a rendering
regression. v1 deliberately hard-coded `renderengines.DefaultRenderOptions()` for every request
(A4, 0.4in margins, no header/footer) and said the real options schema was this module's job "once
it exists." This is that schema.

### Request shape

`options` is a third optional key alongside `content` and `payload`. Every field is optional;
an absent field falls back to the **deployment default** (`PDF_DEFAULT_*` env → `WithRenderDefaults`),
which itself falls back to `DefaultRenderOptions`. A request sending only `{content}` is byte-for-byte
unaffected.

```jsonc
{
  "content": "...", "payload": { }?,
  "options": {
    "paperSize": "Letter|Legal|Tabloid|A3|A4|A5",   // OR width+height, not both
    "width": "8.5in", "height": "11in",              // number = inches, or "10mm"/"1cm"/"72pt"/"96px"
    "landscape": false,
    "margin": { "top": "0", "right": "0", "bottom": "10mm", "left": "0" },
    "scale": 1.0,                                     // 0.1–2.0
    "printBackground": true,
    "displayHeaderFooter": false,
    "headerTemplate": "<div>…</div>", "footerTemplate": "<div>…</div>",
    "waitForFonts": true, "waitForImages": true,
    "timeoutMs": 30000,                               // clamped to [1000, 120000]
    "embedImages": false,                             // /v1/pdf/html only; requires EMBED_IMAGES_ENABLED
    "fitToPage": false,                               // /v1/pdf/html only
    "overlay": {                                      // /v1/pdf/html only
      "html": "<div style=…>…{{totalPages}}…</div>", //   fragment rendered on a transparent page
      "pages": "last"                                 //   "last"(default)|"first"|"all"|"3"|"3-5"|"2,4"
    }
  }
}
```

### `fitToPage`

Single-page fit: the content's natural height is measured (in the same tab, at a layout viewport
equal to the print page width) and, if it exceeds the printable page height implied by paper +
margins, Chromium's own `PrintToPDF` scale is set to shrink it — clamped at a `MinFitScale` (0.6)
readability floor. On the floor, one page is still produced and overflow is flagged. Ports
`pdf-service-saas`'s `fitToPage`; the scale actually applied travels back in response **headers**
(`X-Fit-Scale`, `X-Fit-Overflow`) since the success body is raw PDF bytes, not JSON.

`renderengines` gains `RenderHTMLFitted` / `Pool.RenderHTMLFitted` (additive — `RenderHTML` and
its tests are untouched) returning `FittedRender{PDF, Scale, Overflowed}`.

### `embedImages`

Ports `pdf-service-saas`'s `embedImagesAsBase64`: remote `<img src>` are fetched server-side and
inlined as `data:` URIs before rendering, so a slow/expired signed URL can't stall or fail the
render. It is doubly gated — the request must ask (`options.embedImages`) **and** the deployment
must allow it (`EMBED_IMAGES_ENABLED=true`) — because fetching URLs named in a submitted document
is SSRF surface. `internal/assets` refuses any URL that resolves to a loopback/private/link-local
address unless `EMBED_IMAGES_ALLOW_PRIVATE=true`, supports a host allow-list, caps per-image and
total bytes, and never fails the render for an image that can't be fetched (the URL is left as-is).

For Synectus Medico specifically this is optional: every image in its report/intake HTML is
already an absolute signed-S3 URL or a `data:` URI, which Chromium fetches directly — `embedImages`
is a latency/resilience nicety, not a correctness requirement.

### `overlay`

A rendered HTML fragment stamped onto **specific pages** of the finished PDF — the job Chromium's
own `headerTemplate`/`footerTemplate` can't do, because those repeat on *every* page. The concrete
driver is Synectus Medico's report disclaimer: a bordered box that must appear on the **last page
only**. `pdf-service-saas` did this with a second Puppeteer render plus a `pdf-lib`
`page.drawPage` overlay; after the switch to this service that overlay had been living on in
`saas-backend` (`utils/helpers/pdf-lib/addDisclaimerFooter.js`). `options.overlay` moves it here so
the caller stops post-processing render output.

Two fields:

- **`html`** (required) — the fragment. It is rendered on its own page the exact size and
  orientation of the main document, with **zero margins, no Chromium header/footer, native scale,
  backgrounds on**. The fragment positions itself with its own CSS (typically
  `position:fixed; bottom:0`); every area it doesn't paint stays transparent, so only its visible
  box composites onto the target page — same result as `pdf-lib`'s `drawPage` at `{x:0, y:0,
  width, height}`. The literal token **`{{totalPages}}`** in `html` is replaced with the main
  document's final page count before the fragment renders (a single documented substring swap, not
  a template engine — any other `{{…}}` is left untouched).
- **`pages`** (optional, default `"last"`) — which 1-based pages to stamp: `"last"`, `"first"`,
  `"all"`, or an explicit list of single pages / ascending ranges (`"3"`, `"3-5"`, `"2,4"`,
  `"1,3-4"`). Deliberately narrower than pdfcpu's own selection grammar (no `l`, `even`, `odd`,
  negation, open-ended ranges) so callers get one predictable shape and this service owns every
  error message. A page past the end of the document is a `422 RENDER_ERROR` — the real page count
  isn't known until the main render completes, so it can't be a `400`.

Mechanics: `internal/overlay` counts the main PDF's pages (**pdfcpu**, pure Go — the first, narrow
use of the direct-construction "fast path" `CAPABILITY-MAP.md` assigns to `render-engines`, brought
in ahead of the full Phase 8 image/manipulation slice), substitutes the token, renders the fragment
through the **same admission-gated Chromium path** as any other request (so a fragment render is
subject to the same backpressure — an overlay request therefore takes an admission slot twice, in
sequence, and can get a `503 QUEUE_FULL` on the fragment render even after the main render
succeeded), then stamps it with `pdfcpu.AddWatermarks` (`onTop`). Page count and existing page
content are preserved — the stamp is drawn over, never inserted. Full text/image watermarking (all
pages, opacity, rotation, positioning grammar) stays the separate deferred `customization-layer`
slice; this is one composited overlay.

### Error code additions

| Code | Status | When |
|---|---|---|
| `INVALID_REQUEST` | 400 | unknown `paperSize`, `scale` out of range, `width`/`height` not paired, a length that doesn't parse, `fitToPage`/`embedImages`/`overlay` on a non-HTML route, `embedImages` when the server hasn't enabled it, `overlay` with an empty `html` or an unparseable `pages` selector |
| `RENDER_ERROR` | 422 | (existing) — now also: `overlay.pages` names a page past the end of the rendered document (only knowable after the main render), or the fragment render / stamp step fails |

(No new top-level code — an unusable options object is a caller-side mistake, same class as a
malformed body; an overlay that can't be applied to the produced PDF is the same class as any other
post-decode render failure.)

### Route support matrix

| Option group | `/v1/pdf/html` | `/v1/pdf/markdown` | `/v1/pdf/html-lite` |
|---|---|---|---|
| paper / margin / scale / header-footer / wait / timeout | ✅ | ✅ | timeout only (WeasyPrint has no page-setup concept) |
| `fitToPage` | ✅ | ✗ 400 | ✗ 400 |
| `embedImages` | ✅ (if enabled) | ✗ 400 | ✗ 400 |
| `overlay` | ✅ | ✗ 400 | ✗ 400 |

`fitToPage` and `embedImages` are HTML-only for a structural reason: there is no DOM to measure,
and no `<img>` tag to rewrite, before goldmark runs on the markdown path. `overlay` is HTML-only
because its fragment is itself a Chromium render (so its CSS behaves exactly like the main
document's), and neither the markdown nor the WeasyPrint path has an equivalent post-render
composition step.
