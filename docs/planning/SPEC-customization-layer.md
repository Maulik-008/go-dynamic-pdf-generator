# Spec: customization-layer (v1 — data-bound templates; v2 — header/footer height validation)

Module spec under `CAPABILITY-MAP.md`. `customization-layer`'s full responsibility per the
capability map is broad — page setup, header/footer, watermark, custom fonts, wait strategy,
data-bound templates — and per `PLATFORM-SPEC.md`'s customization catalog spans seven groups (Page,
Header/footer, Visual, Behavior, Document, Data-driven templates, Manipulation). Following the same
incremental-delivery discipline already used for every prior module in this repo (`render-engines`
shipped HTML+Markdown before its fast path; `lightweight-render` shipped without process pooling
before optimizing), **this spec scopes v1 to one slice: data-driven templates (template + payload
merge)** — catalog group 6, and the single feature most repeatedly named across the platform
research as the concrete answer to "my way, any kind of customisation": *"a merge-variable system
(an HTML/Markdown template plus a JSON payload produces the rendered document)... this is what turns
'my way' into something that scales past one-off HTML strings"* (`PLATFORM-SPEC.md`).

**v2 adds the follow-up slice originally deferred here: header/footer auto-height validation** — see
its own section below. **Still deferred, not dropped**: watermark, custom fonts, PDF/A, and the
manipulation group (merge/split/encrypt — that's the fast path, a different `render-engines` phase
entirely, not this module). Page/wait-strategy/behavior options already exist as
`renderengines.RenderOptions` from Phase 1 and aren't revisited here.

## Objective

Given a template string (HTML or Markdown, containing Go template actions like `{{.customerName}}`)
and a payload (a JSON object decoded to `map[string]any`), produce the final document string —
unchanged input to the existing `renderengines.RenderHTML`/`RenderMarkdown`, which this module sits
in front of and never modifies. This is what lets a customer submit one template once and render it
against many different payloads (invoices, certificates, reports), instead of building a new HTML
string per document on their own side.

**Explicitly not goals**: a full templating language (Liquid, Handlebars) — Go's stdlib
`html/template` is deliberately used instead (see Tech Stack); template *storage/management*
(that's the deferred `template-studio` Tier 2/3 decision in `PLATFORM-SPEC.md`, gated on real usage
signal); validating the payload against a schema beyond what template execution itself requires.

## Tech Stack

Go's standard library `html/template` — **not** a Liquid implementation (the pattern PDFMonkey and
the research doc's competitor survey use) or any third-party template engine. Chosen deliberately
per this project's own dependency discipline (`code-review-and-quality`'s "prefer standard library
... over new dependencies; every dependency is a liability") and, more importantly, for a real
security property: `html/template` contextually HTML-escapes every merged value by default, where
`text/template` (or Liquid) would not. This matters beyond markup correctness — `render-engines`
executes the merged result in a real Chromium instance, so an unescaped payload value containing
`<script>` would be a genuine HTML/JS injection vector into the rendered PDF (not merely cosmetic),
particularly for payload data that ultimately originates from *someone else's* end user (e.g. a
customer name field on a form). The syntax differs slightly from Liquid's `{{ field }}` — Go
template actions require a leading dot, `{{.field}}` — a real, visible difference worth noting since
the research explicitly cites Liquid as "the reference pattern"; adopting Liquid-compatible syntax
via a third-party package is a possible future enhancement if real customer demand justifies the new
dependency, not built speculatively now.

Confirmed relevant to this choice: `internal/renderengines/markdown.go` calls `goldmark.Convert`
with default settings, which (per CommonMark) passes raw HTML through into the output verbatim — so
an unescaped `<script>` in a *Markdown* template's merged payload would execute too, not just in the
HTML path. `html/template`'s escaping is applied uniformly to both template kinds for this reason,
not just the HTML one.

## Project Structure

```
internal/customization/
  merge.go       → Merge(tmpl string, payload map[string]any) (string, error)
  merge_test.go  → unit tests (no Chromium needed — pure text processing)
```

Wired into `internal/api` as two new routes, `POST /v1/pdf/html-template` and
`POST /v1/pdf/markdown-template` — separate from the existing raw-body `/v1/pdf/html` and
`/v1/pdf/markdown` routes (those keep accepting a raw document body unchanged; a template+payload
request has two logical fields, so it needs a JSON body, the first departure from this project's
raw-body convention). Each new route: decode JSON → `customization.Merge` → existing
`s.renderer.RenderHTML`/`RenderMarkdown` via the same `s.jobPool` admission gate the existing routes
already use — this module produces a string, then hands off to code that already exists and is
already tested; no new render path.

## API Shape

```go
package customization

// Merge fills tmpl with values from payload (already JSON-decoded) using
// Go's html/template with Option("missingkey=error") — a missing payload
// field is a template execution error naming the exact field, matching
// PLATFORM-SPEC.md's "name the exact missing template variable" requirement
// (Go's own error text does this; verified by a test asserting the field
// name appears in the error, not re-implemented as a separate typed
// accessor — conversion-api's future field-scoped JSON error envelope is
// this module's designed consumer, once it exists).
func Merge(tmpl string, payload map[string]any) (string, error)
```

Request JSON shape for the new endpoints (raw minimal shape, matching this project's existing
deliberately-minimal API — the full request/response envelope is `conversion-api`'s job, not this
slice's):

```json
{
  "template": "<html><body>Hello {{.customerName}}</body></html>",
  "payload": { "customerName": "Ada" }
}
```

- `400` — malformed JSON body, empty body, or missing `template` (`payload` is optional, defaults
  to an empty object — a template with no `{{}}` actions is valid with no payload at all).
- `422` — template parse or execute failure (bad syntax, missing field) — distinct from `400`
  because the JSON itself is well-formed; the template+payload combination is invalid. Matches the
  existing convention where a downstream render failure also maps to `422`.
- `422` — a downstream render failure (existing behavior, unchanged).
- `503` + `Retry-After` — the orchestration pool is saturated (existing behavior, unchanged).

## Testing Strategy

- Pure unit tests for `Merge` (no Chromium/subprocess dependency — this is text processing) covering:
  successful merge with simple and nested (`{{.customer.name}}`) fields, a `{{range}}` loop over a
  payload array (the invoice line-item use case explicitly cited in research), a missing field
  producing an error that names the field, malformed template syntax producing a parse error, and
  **the security property this design depends on**: a payload value containing `<script>` or
  `"><img src=x onerror=...>` must appear HTML-escaped in the merged output, for both an HTML
  template and (per the goldmark raw-HTML-passthrough finding above) a Markdown template.
- One real end-to-end test per new route, through the real HTTP handler with a fake renderer
  (matching `internal/api/handlers_test.go`'s existing pattern), plus one live-Chromium integration
  test (skipped without `CHROMIUM_PATH`, matching the existing skip convention) proving a merged,
  real-rendered PDF actually contains the merged value and not the raw `{{.field}}` placeholder.

## Boundaries

- **Always**: HTML-escape payload values by construction (via `html/template`, not manual escaping
  callers could forget) for both template kinds; keep this module producing a plain string handed
  to the *existing*, unmodified `RenderHTML`/`RenderMarkdown` — never a parallel render path.
- **Ask first**: adopting a non-stdlib template engine (Liquid-compatible or otherwise) to match
  competitors' syntax; allowing a payload field to inject trusted/unescaped raw HTML (no such
  override exists in v1 — see Tech Stack).
- **Never**: execute a template against payload data without HTML-escaping; silently substitute a
  missing field with an empty/placeholder value instead of erroring (Go's default `html/template`
  behavior without `missingkey=error` is exactly this footgun — explicitly overridden).

## Success Criteria

- [x] `Merge` succeeds for simple fields, nested fields, and `{{range}}` over an array payload
      (`TestMerge_SimpleField`, `TestMerge_NestedField`, `TestMerge_RangeOverArray`)
- [x] A missing payload field produces an error whose text names the exact field
      (`TestMerge_MissingFieldErrorsNamingField`; confirmed live via curl —
      `map has no entry for key "missing"`)
- [x] Malformed template syntax produces a clear parse error, not a panic
      (`TestMerge_MalformedTemplateSyntaxErrors`)
- [x] A payload value containing HTML/script markup is escaped in the merged output — verified for
      both an HTML template and a Markdown template through the real `goldmark.Convert` dependency
      (`TestMerge_EscapesHTMLInPayloadValue_HTMLTemplate`,
      `TestMerge_EscapesHTMLInPayloadValue_MarkdownTemplate`)
- [x] `POST /v1/pdf/html-template` and `POST /v1/pdf/markdown-template` work end-to-end against a
      fake renderer (`TestHandleHTMLTemplate_Success`, `TestHandleMarkdownTemplate_Success` — assert
      the *merged* string, not the raw template, reaches the renderer) and the real Chromium pool
      (`TestEndToEnd_RealPool_Template`); live curl-verified for both routes plus the 400 (missing
      template) and 422 (missing field) error paths
- [x] Existing `/v1/pdf/html`, `/v1/pdf/markdown`, and `/v1/pdf/html-lite` routes are completely
      unaffected — verified both by their unchanged existing tests passing and live curl against a
      running server alongside the new routes
- [x] `go test ./... -race` green, with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH` set

---

# v2: header/footer auto-height validation

## Objective

`renderengines.RenderOptions` already accepts `HeaderTemplate`/`FooterTemplate` (Chromium's own
margin-box HTML snippets, rendered via `page.PrintToPDF`'s `headerTemplate`/`footerTemplate`
params) with zero validation — a header or footer tall enough to exceed its configured margin
(`MarginTop`/`MarginBottom`) silently overlaps body content. This is the exact, chronic bug class
named in the platform research (Puppeteer issues #10024, #13738, #4132/#4266-style overlap
failures) and a hard "Never" boundary in `PLATFORM-SPEC.md`: *"silently swallow a header/footer
height mismatch."* This slice adds the fast-fail: measure the template's real rendered height and
reject before an actual PDF render if it won't fit.

## Design

Adds `renderengines.Renderer.MeasureHTMLHeight` (and `Pool.MeasureHTMLHeight`, same
acquire/release-wrapping pattern as `RenderHTML`/`RenderMarkdown`) — a new, narrow capability on
`render-engines`, anticipated by its own spec's Boundaries ("Ask first: changing the exported
Renderer/RenderOptions API shape once customization-layer starts depending on it" — this is exactly
that moment, done deliberately rather than as scope creep):

```go
// MeasureHTMLHeight renders htmlFragment (treated as body content, not a
// full document) in a plain page at the given viewport width and returns
// its rendered height in CSS pixels (document.body.scrollHeight).
func (r *Renderer) MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error)
func (p *Pool) MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error)
```

**A real bug found and fixed during TDD, not a theoretical one**: the first version injected
`htmlFragment` directly via `page.SetDocumentContent` with no wrapper. A test asserting that a
400px-tall div measures taller than a 20px-tall one failed — both measured as exactly the viewport
height (1000px) regardless of content. Root-caused via a direct diagnostic (not guessed): the
injected fragment has no `<!DOCTYPE html>`, so the document loads in quirks mode
(`document.compatMode == "BackCompat"`), and in quirks mode `body` stretches to fill the viewport,
so `scrollHeight` reports the viewport size instead of the content's actual height. Confirmed the
fix by the same diagnostic: wrapping the fragment in a minimal `<!DOCTYPE html>` document forces
standards mode (`compatMode == "CSS1Compat"`), after which a 400px div correctly measures ~400px.
`MeasureHTMLHeight` now always wraps its input this way — not optional formatting, a correctness
requirement.

`internal/customization` adds the validation logic on top, against a small interface so unit tests
don't need real Chromium:

```go
type HeightMeasurer interface {
    MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error)
}

type HeightMismatchError struct {
    Region         string // "header" or "footer"
    MeasuredInches float64
    MarginInches   float64
}

// ValidateHeaderFooterHeight measures opts.HeaderTemplate/opts.FooterTemplate
// (when opts.DisplayHeaderFooter is set) against opts.MarginTop/MarginBottom
// and returns a *HeightMismatchError if either would overflow its margin box.
func ValidateHeaderFooterHeight(ctx context.Context, m HeightMeasurer, opts renderengines.RenderOptions) error
```

Measurement happens at `opts.PaperWidth` converted to CSS pixels (96px/in) — a documented
approximation of Chromium's internal print-margin-box layout, not a byte-for-byte replica: Chromium
lays out `headerTemplate`/`footerTemplate` through its own internal print pass, which isn't exposed
for direct introspection over CDP. Rendering the same content in a normal, standards-mode page load
at the configured paper width is the same workaround the wider Puppeteer/Chromium community uses
for this exact problem, for lack of a direct API.

## Deliberately not wired into HTTP yet

Unlike the v1 template-merge slice (which got two new routes because it introduced a genuinely new
request shape), this validation is **not** wired into any HTTP route in this slice. The current
minimal API's existing routes all call `renderengines.DefaultRenderOptions()` unconditionally —
`HeaderTemplate`, `FooterTemplate`, `MarginTop`/`MarginBottom`, and every other `RenderOptions`
field beyond the document body itself are already unexposed via HTTP today, for every caller, not
just this one. Adding HTTP exposure for only these two fields while every other `RenderOptions`
field stays hardcoded would be a partial, premature options schema — that schema is explicitly
`conversion-api`'s job per `CAPABILITY-MAP.md`. `ValidateHeaderFooterHeight` is proven as a real,
tested Go capability now (unit tests against a fake, integration test against real Chromium);
`conversion-api` calls it once the real options schema exists.

## Testing Strategy

- `renderengines`: a real-Chromium test proving `MeasureHTMLHeight` reflects actual content size
  (a taller div measures taller, within a sane ballpark) and a context-cancellation test, matching
  this package's existing testing conventions exactly.
- `customization`: unit tests against `fakeHeightMeasurer` (deterministic, no Chromium) covering:
  no-op when `DisplayHeaderFooter` is false, no-op when templates are empty, pass when content fits
  its margin, reject with the correct named region when either the header or the footer overflows,
  header checked before footer, and the underlying measurement error propagating. One real
  integration test against the actual `renderengines.Pool` proving a genuinely tall header (200px
  against a 0.4in/~38px margin) is rejected and a genuinely short one (10px) is accepted.

## Boundaries

- **Always**: measure before rendering (fail fast, per the Accuracy pillar), name the exact region
  (header vs. footer) and the numbers involved in the error.
- **Ask first**: wiring this into any HTTP route ahead of `conversion-api`'s real options schema
  (see above); changing the viewport-width approximation to something more precise if Chromium ever
  exposes direct introspection of its print margin-box layout.
- **Never**: silently allow a header/footer taller than its margin through to an actual render.

## Success Criteria

- [x] `MeasureHTMLHeight` reflects real content size, not a stub or the viewport size — proven by a
      test that initially failed for a real reason (quirks-mode viewport-stretch bug) and passed
      once fixed (`TestMeasureHTMLHeight_ReflectsContentSize`,
      `TestPool_MeasureHTMLHeight`)
- [x] A header/footer taller than its configured margin is rejected with a specific,
      region-and-numbers-naming error, not silently rendered overlapping
      (`TestValidateHeaderFooterHeight_RejectsHeaderTallerThanMargin`,
      `TestValidateHeaderFooterHeight_RejectsFooterTallerThanMargin`)
- [x] A header/footer that fits its margin passes with no error
      (`TestValidateHeaderFooterHeight_PassesWhenHeaderFitsMargin`)
- [x] Proven against the real Chromium pipeline, not just a fake
      (`TestValidateHeaderFooterHeight_WithRealRenderEnginesPool`)
- [x] `go test ./... -race` green, with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH` set

## Open Questions

None blocking v1 or v2 as scoped above. Still deferred (not open questions on *these* slices):
watermark, custom fonts, PDF/A, and wiring header/footer options (and the rest of `RenderOptions`)
into HTTP once `conversion-api`'s real options schema exists.
