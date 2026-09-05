# Capability Map: High-Performance PDF Generation Platform

Per `spec-driven-development`'s Phase 0 scope check: this platform bundles several independently
testable capabilities (each has its own consumer and could ship/verify on its own), so it gets a
capability map before any module spec is written. **This map is the review gate** — once it's
approved, each module gets its own `SPEC-<module-id>.md` in dependency order.

| Module id | Responsibility | Depends on |
|---|---|---|
| `render-engines` | The actual conversion engines: a **fidelity path** (Chromium-driven rendering — **v1 scope: HTML and Markdown input only**) and a **fast path** (direct PDF construction for images and PDF manipulation — no browser, **deferred past v1**, see Roadmap in `PLATFORM-SPEC.md`). Owns browser/process pool lifecycle. | — |
| `customization-layer` | The options schema and template system applied on top of the engines: page setup, headers/footers, watermarks, custom CSS/fonts, wait strategy, merge-variable templating (data-bound documents). | `render-engines` |
| `job-orchestration` | Worker pool, queue, concurrency limits, sync vs. async execution, backpressure, autoscaling signal (queue depth). | `render-engines` |
| `conversion-api` | The public HTTP surface: one endpoint per conversion type, request validation, consistent request/response envelope, error model. | `customization-layer`, `job-orchestration` |
| `auth-and-tenancy` | API keys, per-key rate limiting, usage metering (billing-ready, whether or not billing ships in v1). | `conversion-api` |
| `storage-and-delivery` | Object storage for async job output, webhooks, TTL cleanup, job-status endpoint. | `job-orchestration`, `conversion-api` |
| `observability` | Metrics (RED: rate/errors/duration), structured logging, tracing, health/readiness probes. Threaded through every module from day one — specified here, implemented incrementally alongside each module rather than bolted on last. | `render-engines`, `job-orchestration`, `conversion-api` |
| `lightweight-render` | A **separate, explicitly opt-in** static HTML/CSS→PDF path (WeasyPrint subprocess, no JS execution) — not a replacement for `render-engines`, not auto-routed to. Own endpoint (`/v1/pdf/html-lite`), own accuracy trade-offs documented in `SPEC-lightweight-renderer.md`, own tests. Exists specifically because the platform-wide research doc ruled out *silent* content-based engine routing as unsafe, but a customer-chosen separate path sidesteps that risk. | — (deliberately independent of `render-engines`) |

**Build order:** `render-engines` (HTML + Markdown fidelity path only) → (`customization-layer`,
`job-orchestration` in parallel) → `conversion-api` → (`auth-and-tenancy`, `storage-and-delivery`
in parallel) → `observability` hardening pass → `render-engines` fast path (image/manipulation,
now Phase 5, see `PLATFORM-SPEC.md` Roadmap) → URL input on the fidelity path.

**Revision note (Round 1 review):** this supersedes the original build order, which followed
`docs/research/go-pdf-generation-research.md` Part 8 and started with the fast path (image→PDF +
pdfcpu manipulation) specifically *because* it needs zero browser dependency and was the fastest
path to something real. The business priority is HTML and Markdown first — those are the formats
actually wanted first — so `render-engines` now takes on Chromium-pooling complexity immediately
instead of deferring it. The fast path is still valuable (it's near-zero marginal cost once
`pdfcpu`/`fpdf` are wired in) but no longer gates anything else, so it moves later without
blocking the modules that depend on `render-engines`.

## Why this shape, not a different split

- `render-engines` is isolated from everything else because it's the one module where "speed" and
  "accuracy" are in direct tension (fast path trades CSS fidelity for zero browser overhead;
  fidelity path pays Chromium's cost for full rendering correctness) — it needs to be gotten right
  and load-tested on its own before anything is built on top of it.
- `customization-layer` is split out from `conversion-api` because "any kind of customisation" is
  the platform's stated differentiator, not an API detail — it deserves its own success criteria
  and its own test suite (the golden-file visual-regression suite lives here), independent of HTTP
  concerns.
- `auth-and-tenancy` and `storage-and-delivery` are separable from the core `conversion-api` because
  a single-tenant / synchronous-only deployment is a legitimate, testable subset of the platform
  (this is exactly Gotenberg's scope) — splitting them keeps that smaller product real and shippable
  on its own rather than gated behind billing/multi-tenancy work.

## Resolved (Round 1 review)

1. **No billing planned.** `auth-and-tenancy` stays "API keys + rate limiting" for v1. Recommend
   still recording per-key usage counters (request count, bytes, render time) from day one even
   though nothing is billed on them — cheap to add now, and avoids a data-backfill problem if
   billing ever does get added later. This needs no new module; it's a field on data
   `auth-and-tenancy` already owns.
2. **Visual template designer UI** — **decided: Tier 1**, a preview/test console (paste
   template+payload, render instantly, no save/persistence). Reuses the sandbox-mode code path
   already in `conversion-api`'s v1 scope, so it needs no new capability-map module. Tier 2/3 (a
   real `template-studio` module) stay deferred until real usage signal justifies them — see
   `PLATFORM-SPEC.md`.
3. **Table shape** — confirmed fine as-is, no merge/split/reorder needed.
