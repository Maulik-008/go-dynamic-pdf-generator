# Spec: High-Performance PDF Generation Platform

Companion to `CAPABILITY-MAP.md` (the Phase 0 module breakdown) and
`docs/research/go-pdf-generation-research.md` (the underlying technical research this spec applies).
Written per `spec-driven-development`. **This is a draft for review** — see Assumptions and Open
Questions before we move to per-module specs.

## Assumptions I'm making

1. This is a multi-tenant, API-first **platform** (like cloudlayer.io), not an internal tool — based
   on the original framing ("create one software", explicit cloudlayer.io reference) and today's
   "platform where user can generate" phrasing.
2. The primary interface is a REST/JSON HTTP API, matching every competitor researched plus
   Gotenberg. A visual template designer/dashboard (cloudlayer.io has one) is a later phase, not v1.
3. Self-hostable-first (Docker/Kubernetes), consistent with the Go/Gotenberg architecture already
   chosen — a hosted/managed version is a business decision on top of the same artifact, not a
   different build.
4. **Speed is the top-ranked requirement when it trades off against something — but never at the
   cost of silently wrong output.** Accuracy is a gate (must pass), not a dial to trade away for
   more speed.
5. "Any kind of customisation" means full, real control over standard PDF/print concerns (page
   setup, headers/footers, watermarks, custom CSS/fonts, page ranges, merge/split, PDF/A,
   encryption, data-bound templates) — **not** a new proprietary layout language. The first research
   doc already ruled out reimplementing CSS layout from scratch; this spec doesn't relitigate that.
6. ~~No target scale had been given~~ — resolved below in "Scale Target (v1)": a concrete starting
   number, chosen deliberately small and cheap, with the extensibility mechanism built in from day
   one rather than promised for later.

→ Correct any of these now, or I'll proceed with them into the per-module specs.

## Decisions confirmed (Round 1 review)

1. **No billing in v1.** `auth-and-tenancy` is API keys + rate limiting only. Usage is still
   metered/counted per key from day one (cheap now, avoids a backfill problem if billing is ever
   added later) — but nothing is charged against it.
2. **V1 scope narrows to HTML → PDF and Markdown → PDF.** URL → PDF, Image → PDF, and PDF
   manipulation (merge/split/watermark/encrypt) all move to Phase 2+ — see Roadmap below. This
   supersedes the original research doc's Part 8 roadmap, which sequenced the browser-free fast
   path first specifically to avoid Chromium complexity early; the business priority is the two
   formats actually wanted first, so `render-engines` takes on the Chromium warm-pool build now
   rather than later.
3. **Speed budgets confirmed as v1 targets.** An automated performance test suite that validates
   them is explicitly in v1 scope (not "later, maybe") — see Testing Strategy and Success Criteria
   below — with an ongoing tuning cadence understood to continue past v1, not a one-time gate.
4. **Scale target and extensibility approach** — chosen below (see "Scale Target (v1)"), designed
   so the queue/concurrency layer can be swapped or grown without a redesign.
5. **Visual template designer** — options elaborated below with a recommendation; tier choice
   tracked as the one still-open decision.

## Objective

Build a Go-native platform that converts **HTML, Markdown, URLs, and images** into PDFs — plus
manipulates existing PDFs (merge/split/watermark/encrypt) — through one consistent API, competing
directly with cloudlayer.io on the four axes the business owner named: **speed, accuracy, ease of
use, and customization depth.** The user is a developer integrating PDF generation into their own
product (invoices, reports, exported web pages) — the same buyer cloudlayer.io/DocRaptor/PDFShift
sell to.

**V1 scope is HTML → PDF and Markdown → PDF.** Both share one rendering path (Markdown compiles to
HTML via `goldmark`, then both go through the same Chromium fidelity path), so this is really one
engine build, not two. URL → PDF, Image → PDF, and PDF manipulation are real, planned capabilities
— just sequenced after v1, per the Roadmap below.

The founding bet, from the first research doc: driving Chromium from Go instead of Node removes
Puppeteer's orchestration-layer fragility (zombie processes, event-loop contention, a second V8
runtime's overhead stacked on Chromium's own) without pretending Chromium itself gets lighter. Speed
comes from architecture and pooling discipline, not from a magic faster engine.

## Non-Functional Requirements — the four pillars, reframed as testable criteria

*(Per spec-driven-development: vague requirements get reframed into concrete conditions before
anything is built. Numbers below are proposed targets to validate with real load tests, not facts —
today's research found no vendor publishes a rigorous, reproducible throughput benchmark for this
category, so we set our own bar rather than borrow an unverified one.)*

### Speed

Two paths, two budgets, because they have fundamentally different cost structures:

| Path | Operations | Target p50 | Target p99 |
|---|---|---|---|
| **Fast path** (no browser — direct construction) | Image → PDF, merge, split, watermark, encrypt, rotate | < 50ms | < 200ms |
| **Fidelity path** (Chromium-driven, warm pool) | HTML/Markdown/URL → PDF, single-page business document (< 500KB rendered HTML, no long custom wait) | < 600ms | < 2.5s |

Anchoring logic from research: cold Chromium launch costs 300–800ms per instance — this is the
single biggest lever, which is why a **warm pool is mandatory, not an optimization we might add
later.** Gotenberg's own default (~6 concurrent conversions per Chromium instance, 512Mi–1GB floor)
is a reasonable starting capacity-planning unit per node, to be tuned against our own load tests.

Additional speed success criteria:
- Under sustained load, queue wait stays bounded and visible (autoscale on **queue depth**, not
  raw CPU%) — degrade via backpressure/429, never via silent unbounded queueing.
- A page requiring a long custom `waitFor` (heavy client-side JS app) is explicitly out of the
  fidelity-path budget above by nature, not a target we control — this must be a documented,
  honest scoping statement in the public docs, not a promise we can't keep.

### Accuracy

Defined as: **matches real Chromium rendering** (the fidelity path's actual engine) and is
regression-tested automatically, not asserted by eye.

- **Golden-file visual regression is part of CI, not optional** — rasterize each output PDF page,
  diff against a checked-in reference PNG per template, fail the build above a pixel-delta
  threshold. (Go-side equivalent of the `pdf-visual-diff` pattern found in research.)
- **Wait-for-fonts + network-idle is the default, not opt-in** — the single most-cited root cause
  of "fonts don't render / text is missing" bugs across every engine researched is not waiting for
  web fonts before the print snapshot. We default to correct and let callers opt out for speed if
  they know their document has no web fonts.
- **Header/footer margin-box height mismatch is defended against, not just documented** — this
  showed up repeatedly across Puppeteer's own issue tracker (footer overlapping body content,
  stray whitespace above/below header regions) as a chronic, unresolved category-wide bug. Our
  fast-fail: auto-measure the rendered header/footer template height at request time and reject a
  margin that doesn't fit, with a specific error, instead of silently overlapping content.
- CJK, RTL (Arabic/Hebrew), emoji, and mixed-language text are **first-class fixtures in the golden
  suite from day one**, not edge cases added later — research found these are exactly where
  non-browser engines and misconfigured font stacks quietly fail.

### Ease of use

- One required field per conversion call (`html`, `url`, `markdown`, or `image`); everything else
  defaults to something that "just works" (A4/Letter default, no header/footer, scale 1,
  wait-for-fonts + network-idle on).
- **Sync endpoint for small/quick jobs, async job-id + webhook for anything larger** — mirrors
  cloudlayer's v1/v2 split and DocRaptor's `async: true` + `callback_url` pattern, which is the
  category norm, not an unusual choice.
- **Idempotency keys on every mutating request** (Stripe-style: same key, same result, side effects
  happen once) so retries are always safe.
- **Structured, field-scoped errors** — name the exact missing template variable, the exact
  CSS/HTML problem, the exact margin/header-height mismatch. Never a bare `400 Bad Request`.
- A **free sandbox/test mode** (like PDFShift's `sandbox: true` / DocRaptor's `test: true`) that
  returns real, watermarked output, so a developer can validate integration before spending quota.
- **One consistent request/response envelope across every conversion type** — html, markdown, url,
  image, and manipulation endpoints all shaped the same way. One mental model, not five.

### Customisation ("my way, any kind of customisation")

Concrete options schema (see the full catalog table below), organized in five groups:

1. **Page** — format/size (A4/Letter/custom mm), orientation, margins, scale.
2. **Header/footer** — HTML snippet templates with a token vocabulary (page number, total pages,
   date, title, url), auto-height-validated against the margin.
3. **Visual** — print-background toggle, watermark (text or image, position, opacity), custom CSS
   injection, custom font embedding.
4. **Behavior** — wait strategy (network-idle default, selector-wait, fixed delay, custom JS
   expression), page ranges, timeout.
5. **Document** — PDF/A output, tagged/accessible PDF, encryption/password, bookmarks/outline.
6. **Data-driven templates** — a merge-variable system (an HTML/Markdown template plus a JSON
   payload produces the rendered document), matching the pattern PDFMonkey built its whole product
   around (Liquid templates over a payload object). This is what turns "my way" into something that
   scales past one-off HTML strings.
7. **Manipulation** — merge, split, rotate, watermark-existing, encrypt on already-produced PDFs,
   no browser involved (pdfcpu, fast path).

Explicit non-goal, stated plainly so it doesn't get re-litigated later: **we are not inventing a new
page-layout language.** Customization means full control over real CSS/HTML plus the PDF-specific
primitives above — reimplementing CSS layout was already ruled out in the first research doc (Part
2) as out of reasonable scope, and every competitor researched made the same call.

### Reference: customization parameter catalog (sourced, not invented)

| Concern | Param shape (Chromium/CDP-family — our fidelity path) | Notes from research |
|---|---|---|
| Page size | `format` (A4/Letter) or `width`/`height` | CDP: `paperWidth`/`paperHeight` in inches |
| Orientation | `landscape: bool` | — |
| Margins | `margin: {top,right,bottom,left}` | — |
| Header/footer | `headerTemplate`/`footerTemplate` (HTML snippets), built-in tokens: `date`, `title`, `url`, `pageNumber`, `totalPages` | Auto-height-validate — see Accuracy above |
| Background graphics | `printBackground: bool` | — |
| Wait strategy | network-idle (default), selector-wait, fixed delay, custom JS expression, `waitForFonts` | Default on — see Accuracy above |
| Page ranges | `"1-5,8"` style | — |
| Scale | `0.1–2.0` | — |
| PDF/A / tagged PDF | boolean flags | Experimental even in Chromium itself — treat as best-effort in v1 |
| Encryption/password | password + permission flags | Not exposed by bare Puppeteer; PDFShift/DocRaptor-tier feature |
| Watermark | text/image, offset, opacity | Fast-path (pdfcpu) or fidelity-path (CSS) |
| Sandbox/test mode | `sandbox: true` → free, watermarked output | Category-standard DX pattern |
| Data-driven templates | template + JSON payload | PDFMonkey's Liquid-over-payload is the reference pattern |

## Scale Target (v1)

No traffic/customer-count number was available to anchor this to, so here's a concrete starting
point, chosen deliberately small and cheap to run, with the extensibility the business asked for
built in from day one rather than retrofitted:

- **v1 target**: a single well-provisioned node (e.g. 8 vCPU / 16GB) running a warm pool of
  ~8–12 Chromium instances, each handling Gotenberg's own default of ~6 concurrent conversions —
  roughly **50–70 concurrent fidelity-path conversions** before the queue starts absorbing excess
  load instead of failing. That's a deliberately conservative starting ceiling, not a hard limit —
  see below.
- **Update from real load testing** (`docs/research/load-test-results.md`): pool size should be
  sized to available CPU cores, not to a fixed instance-count target in isolation — a 4-core test box
  showed adding a 4th Chromium instance (beyond ~1 per core) bought no additional throughput and
  slightly *increased* contention. Read "~8-12 Chromium instances" above as implying a node with a
  correspondingly larger core count (roughly 8-16 vCPUs), and size `PoolConfig.Size` operationally to
  `runtime.NumCPU() - 1` as a starting heuristic, confirmed on the real target node.
- **Extensibility mechanism, not a promise on faith**: `job-orchestration` defines its queue behind
  a small internal interface (`Enqueue`, `Dequeue`, `Depth`) from the very first implementation.
  V1's implementation is an in-process Go channel — zero extra infrastructure, fastest to build and
  operate. When real traffic outgrows one node, the *only* change is swapping that interface's
  implementation for a Redis- or NATS-backed queue so multiple API/render nodes can share one queue
  — no change to `conversion-api`, `customization-layer`, or anything above the interface. This is
  the concrete answer to "extendable/changeable": the seam is designed in now, the multi-node
  backend is deferred until it's actually needed.
- **Autoscaling posture**: even at v1's single-node scale, size the node so queue depth (not raw
  CPU%) is the signal that would trigger scaling — this makes the later move to Kubernetes+KEDA
  (Part 5 of the research doc) a config change against an already-correct metric, not a rearchitect.
- **Implementation note (`job-orchestration` built)**: the delivered shape is a generic
  `orchestration.Pool`/`Submit[T]` (see `docs/planning/SPEC-job-orchestration.md`), not the literal
  `Enqueue`/`Dequeue`/`Depth` method names sketched above when this section was first written — same
  intent (an in-process channel-backed queue behind a small interface, swappable later for a
  distributed backend), different concrete shape once it was actually built. `QueueDepth()` is the
  autoscaling signal this bullet calls for. Confirmed with a real re-run of the heavy-document load
  test at 3x admitted capacity: 48/72 requests rejected immediately (503, 42-58ms) instead of the
  previously-documented unbounded latency growth with zero rejections — see the "Follow-up" section
  of `docs/research/load-test-results.md`.

## Visual Template Designer — Options for Decision

cloudlayer.io ships a visual template designer; whether we build one, and how much of one, is a
real product decision with a wide range of possible answers. Laid out as tiers, cheapest/fastest
first, each one a strict superset of the one before it:

| Tier | What it is | What it needs | Value delivered | Rough effort |
|---|---|---|---|---|
| **0 — None** | API + written docs only, no UI at all | Nothing beyond the API itself | Matches Gotenberg's own scope; zero extra surface to maintain | None |
| **1 — Preview/test console** *(recommended starting point)* | A single page: paste an HTML/Markdown template + a JSON payload, click render, see the resulting PDF instantly. No save, no accounts, no persistence — a debugging tool, not a CMS. | A thin frontend + the existing `sandbox: true` code path already in this spec (Ease-of-use pillar) — genuinely just a UI wrapper around a capability we're building anyway | Directly serves the "ease of use" pillar: a developer can see their template render correctly *before* writing integration code, without any new backend module | Small — days, not weeks; no new capability-map module needed |
| **2 — Template manager** | Save, list, name, and version templates through the UI; edit HTML/CSS/payload schema and preview inline | Everything in Tier 1, plus persistent storage for named templates (schema + ownership per API key) and basic CRUD endpoints | Lets a non-developer (or a developer without redeploying code) manage templates that data changes but structure doesn't (invoices, certificates) | Medium — a real, if small, new module (`template-studio`), new storage, new auth-scoped endpoints |
| **3 — Full WYSIWYG designer** | Drag-and-drop visual block editor, like cloudlayer.io's own — build a template visually, bind data fields by clicking, no HTML/CSS required from the end user | Everything in Tier 2, plus a substantial frontend product (a real design-tool UI, not a form), a block→HTML/CSS compiler, and ongoing UX investment | Opens the product to non-technical end users directly, not just the developers who integrate the API — a genuinely different buyer | Large — this is closer to a second product built on top of the API than an extension of it |

**Decided: Tier 1**, as part of v1 — it's nearly free (reuses the sandbox path we're building
anyway) and directly proves out the "ease of use" pillar with something a developer can actually
click through, rather than just reading docs. Tier 2/3 are deferred until there's a real usage
signal that customers want to manage templates without redeploying code — Tier 3 in particular is
a different product surface (a design tool, not an API), worth a dedicated decision once the core
API has real users, not something to commit engineering time to speculatively now.

## Tech Stack

Go 1.22+. `chromedp` for the fidelity path (CDP over websocket, no Node anywhere). `pdfcpu` +
`go-pdf/fpdf` for the fast/manipulation path (pure Go, no subprocess). `goldmark` for Markdown → HTML.
Job orchestration starts as an in-process goroutine worker pool + channel queue (single-node); a
Redis- or NATS-backed queue is a deferred decision (see Open Questions) for when multi-node
horizontal scaling is actually needed. S3-compatible object storage for async job output. Docker for
packaging, Kubernetes + KEDA (queue-depth-based autoscaling) for orchestration at scale.
Prometheus/OpenTelemetry for observability. This doesn't relitigate the engine choice — that's
Part 5 of the first research doc; this spec applies it.

## Architecture

```
                 ┌────────────────────────┐
  HTTP/JSON ───► │   conversion-api        │◄── auth-and-tenancy (API keys, rate limits)
                 │  request validation ·   │
                 │  envelope · error model │
                 └───────────┬─────────────┘
                             │
                 ┌───────────▼─────────────┐
                 │   customization-layer     │
                 │  options schema · header/ │
                 │  footer height-check ·    │
                 │  template + payload merge │
                 └───────────┬─────────────┘
                             │
                 ┌───────────▼─────────────┐
                 │   job-orchestration        │
                 │  sync vs async · queue ·   │
                 │  concurrency caps ·        │
                 │  backpressure              │
                 └──┬──────────────────┬────┘
                    │                  │
        ┌───────────▼───────┐   ┌──────▼────────────┐
        │  render-engines     │   │  render-engines     │
        │  fast path          │   │  fidelity path       │
        │  (pdfcpu/fpdf,      │   │  (chromedp + warm    │
        │  no browser)        │   │  Chromium pool)      │
        └─────────────────────┘   └──────────────────────┘
                    │                        │
                    └───────────┬────────────┘
                                │
                    ┌───────────▼─────────────┐
                    │  storage-and-delivery      │
                    │  object storage · webhooks │
                    │  · job status · TTL         │
                    └────────────────────────────┘

  observability (metrics/logs/tracing) instruments every box above, from day one.
```

## Roadmap (v1 priority: HTML + Markdown first)

Supersedes the general phased roadmap in `docs/research/go-pdf-generation-research.md` Part 8 for
*this* plan — that doc's Phase 0 (fast path first, to avoid Chromium early) is now sequenced later,
per the Round 1 decision above.

1. **HTML → PDF core** — `chromedp` fidelity path, mandatory warm Chromium pool, basic
   customization (page setup, header/footer with auto-height validation, wait strategy defaulting
   to fonts+network-idle). This is the first thing that has to exist and be right.
2. **Markdown → PDF** — `goldmark` → HTML, reusing the exact same renderer from step 1. Small
   incremental addition once HTML→PDF is solid, not a separate engine.
3. **`job-orchestration` + `customization-layer` depth** — sync/async dispatch, the pluggable
   queue interface (Scale Target above), backpressure, data-bound templates (payload merge),
   watermark, PDF/A. Needed to make steps 1–2 production-grade rather than a demo.
4. **`conversion-api` hardening + `auth-and-tenancy`** — consistent envelope, idempotency keys,
   field-scoped errors, sandbox mode (**and Tier 1 template preview console — nearly free once
   sandbox mode exists**), API keys + rate limiting + usage metering (unbilled).
5. **Automated performance test suite** — validates the speed budgets from this spec against real
   hardware; not optional, explicitly in scope per the Round 1 decision, feeding an ongoing tuning
   cadence rather than a one-time gate. Good moment to also run `/constraints` to turn these
   numbers into an enforced `CONSTRAINTS.md`.
6. **`storage-and-delivery`** — webhooks, async job status, object storage, TTL cleanup.
7. **URL → PDF** — adds SSRF-guarded URL fetching onto the *same* fidelity path built in step 1;
   small incremental addition, not a new engine.
8. **Image → PDF + PDF manipulation (fast path)** — `pdfcpu`/`fpdf`, no browser. This was originally
   proposed as the very first phase (zero browser dependency, fastest to something real); it's
   still low-effort and high-value, just no longer first, per the Round 1 priority decision.
9. **Template designer Tier 2/3** — only if usage signal after step 4–8 justifies it, per the
   Visual Template Designer section above.

## API Shape

- `POST /v1/pdf/html`, `/v1/pdf/markdown`, `/v1/pdf/url`, `/v1/pdf/image`, `/v1/pdf/merge`,
  `/v1/pdf/manipulate` — one route per conversion "shape", same request/response envelope.
- Every route accepts an optional `async: true` (or exceeds a size/time threshold and is forced
  async) → returns `{job_id, status_url}`; otherwise streams the PDF back directly.
- `GET /v1/jobs/{id}` for polling; `webhook_url` on the request for push notification.
- `Idempotency-Key` header honored on all mutating requests.
- Errors: `{error: {code, message, field, request_id, docs_url}}` — never a bare status code with
  no body.
- `sandbox: true` on any request → real, watermarked output, no quota consumed.

## Commands *(proposed — no code exists yet, this is the intended shape)*

```
Build: go build ./...
Test:  go test ./... -race -cover
Lint:  golangci-lint run
Dev:   go run ./cmd/api
```

## Project Structure *(proposed)*

```
cmd/api/              → main entrypoint, HTTP server wiring
internal/renderengines/→ fast path (pdfcpu/fpdf) + fidelity path (chromedp) implementations
internal/customization/→ options schema, template+payload merge, header/footer height validation
internal/orchestration/→ worker pool, queue, sync/async dispatch, backpressure
internal/api/          → HTTP handlers, request validation, error envelope
internal/auth/         → API keys, rate limiting
internal/storage/      → object storage client, webhook delivery, job status
internal/observability/→ metrics, structured logging, tracing helpers
testdata/golden/       → per-template reference PNGs for visual regression tests
docs/                  → this spec, the research doc, ADRs
```

## Code Style

Standard Go conventions (`gofmt`, `golangci-lint` defaults) — no house style invented beyond that.
Example of the shape handlers should take (illustrative, not final):

```go
func (s *Server) HandleHTMLToPDF(w http.ResponseWriter, r *http.Request) {
    req, err := decodeConversionRequest[HTMLRequest](r)
    if err != nil {
        writeError(w, http.StatusBadRequest, err) // field-scoped, per Ease-of-use above
        return
    }
    job := s.orchestrator.Submit(r.Context(), req.ToRenderJob())
    if req.Async {
        writeJobAccepted(w, job)
        return
    }
    streamPDFResult(w, job) // stream, don't buffer the whole file — per performance research
}
```

## Testing Strategy

- Unit tests per module (`internal/...`), `go test -race` in CI.
- **Golden-file visual regression suite** (`testdata/golden/`) is a required, first-class test tier —
  not optional, not "add later" — covering the CJK/RTL/emoji/mixed-language fixture set from day one.
- **Load/performance testing is in v1 scope, not deferred** (Round 1 decision): an automated
  suite validates the speed budgets above against real hardware before they're published as
  external commitments, and gets re-run as an ongoing tuning cadence afterward — not a one-time
  gate. `performance-optimization`'s measure-first approach applies directly here; once numbers
  are validated, `/constraints` is a good way to turn them into an enforced `CONSTRAINTS.md`.
- Header/footer height-validation logic gets explicit unit tests for the overlap bug class found in
  research (Puppeteer #10024, #13738, #4132/#4266-style failures) — this is a named regression class,
  not a generic "test more" instruction.

## Boundaries

- **Always:** run `go test -race`, validate all external input (HTML/URL/template payloads) before
  it reaches Chromium, honor `Idempotency-Key`, stream PDF responses rather than buffer in memory.
- **Ask first:** adding a new external dependency (LibreOffice, a message queue, a new cloud
  service), any change to the public API's request/response shape, choosing the multi-node queue
  backend (Redis vs. NATS vs. other).
- **Never:** fetch a user-supplied URL without SSRF guards (deny private/link-local ranges), accept
  unbounded queue growth instead of backpressure, silently swallow a header/footer height mismatch,
  skip the golden-file suite to ship faster.

## Success Criteria

- [x] v1 ships HTML → PDF and Markdown → PDF only; URL → PDF, Image → PDF, and PDF manipulation are
      explicitly out of v1 scope (tracked in the Roadmap, not silently dropped) — held since Phase 1,
      still true through `conversion-api`
- [x] Fidelity-path operations meet the p50/p99 targets above under load test, on a warm pool —
      **re-confirmed** with a fresh run (`docs/research/load-test-results.md`, regenerated during the
      strict-test/finalisation pass, see `docs/planning/FINALISATION-REPORT.md`): on this 4-core box,
      p50/p99 stayed within budget (<600ms / <2.5s) at every tested concurrency level, 1 through 16
      (worst case: p50 552ms, p99 956ms at concurrency=16), zero failures. This uses the same heavy
      3-page fixture as before (embedded images + a large table) — heavier than the budget's own
      "<500KB, single-page" scope, so this is a stricter proxy in the fixture's favor, not a literal
      match to the budget's defined document size; still the closest real measurement this repo has.
      One box, one moment in time — re-validate on real deployment hardware before treating these
      numbers as an external SLA, per this file's own framing.
- [x] **An automated performance test suite exists** (`internal/api/loadtest_test.go`,
      opt-in via `LOADTEST=1`) **and has validated the speed budgets against real hardware**
      once — see `docs/research/load-test-results.md` for methodology, results, and the two
      concrete findings it produced (pool sizing should track CPU cores; the queue needed a real
      bounded-backpressure ceiling — now fixed, see the file's "Follow-up" section). Extended by
      `internal/api/extreme_load_test.go` (sustained multi-wave overload with leak checks, and a
      real Chromium kill injected mid-flight under continuous concurrent HTTP traffic) — see
      `docs/planning/FINALISATION-REPORT.md`. Still not wired into CI or a scheduled job — that
      half remains open.
- [ ] Golden-file visual regression suite runs in CI and covers CJK/RTL/emoji fixtures — not started
- [x] Header/footer height mismatches are rejected at request time with a specific error, not
      silently rendered overlapping — `customization-layer` v2 (`ValidateHeaderFooterHeight`),
      not yet wired into any HTTP route (see that spec's own boundary note — needs this module's
      own options-schema exposure, deliberately deferred)
- [x] A single request/response envelope is used consistently across every conversion endpoint —
      `conversion-api` v1
- [ ] Sandbox/test mode is available and free, and doubles as the Tier 1 template preview console —
      not started; blocked on `customization-layer`'s still-deferred watermark slice
- [ ] Every mutating endpoint honors `Idempotency-Key` — deliberately deferred, reasoned through in
      `SPEC-conversion-api.md` (no persisted/metered side effect exists yet to deduplicate against;
      needs `storage-and-delivery`)
- [ ] Per-API-key usage is metered/counted from v1, even though nothing is billed yet —
      `auth-and-tenancy` not started at all
- [x] The queue sits behind an interface such that swapping the v1 in-process implementation for a
      distributed backend requires no change above `job-orchestration` — `internal/api` only calls
      `orchestration.Submit`/`Pool.QueueDepth`/`QueueCapacity`, so a distributed reimplementation
      stays inside that package
- [ ] Fast-path operations (image/manipulation, once built in Phase 8) meet their own p50/p99
      targets — fast path not started at all
      targets under load test

## Open Questions

None outstanding. All Round 1 decisions (scale target, billing, v1 scope narrowing to
HTML+Markdown, speed-budget confirmation, and the template designer tier — **Tier 1, confirmed**)
are resolved above and in `CAPABILITY-MAP.md`. Next step per the Roadmap: `SPEC-render-engines.md`
for Phase 1 (HTML → PDF core), scoped to the Chromium fidelity path only.
