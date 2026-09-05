# Spec: lightweight-render (WeasyPrint path — separate from render-engines)

A **new, separate, explicitly opt-in** rendering path, alongside `render-engines` (Chromium), not a
replacement or an automatic substitution for it. Directly follows from
`docs/research/resource-optimization-research.md` §3, which researched and initially recommended
*against* building a "Chromium-free path" — specifically because silently auto-detecting "no JS" and
substituting a different engine risks a customer-invisible accuracy regression. This spec builds the
same underlying capability differently: as a distinct, separately-named endpoint the caller must
deliberately choose, with the accuracy trade-off documented up front, not auto-selected behind their
back. That reframing is what makes this safe to build now.

## Objective

Render static HTML/CSS (no JavaScript execution, no dynamic content) to PDF via WeasyPrint, a
mature, pure-Python CSS Paged Media engine, run as a subprocess. For customers who know their
documents are simple/static and want a different resource profile than the Chromium path — never as
a silent default, and never marketed as more accurate or faster than the Chromium path, because it
is neither.

**Explicitly not goals**: replacing `/v1/pdf/html` (render-engines/Chromium), automatic
content-complexity detection/routing between engines, or matching Chromium's rendering output
pixel-for-pixel. Customers choose this path knowing its trade-offs, documented below.

## The trade-off, stated plainly (from `resource-optimization-research.md`)

- **No JavaScript execution at all.** Any document relying on client-side rendering, dynamic content,
  or JS-computed layout will render incorrectly or incompletely — this is a hard limitation, not a
  bug to fix.
- **Real, confirmed CSS divergences from Chromium** on flexbox page-breaking and nested flex-direction
  layouts (Kozea/WeasyPrint issues #2076, #2222 — cited in the research doc), and partial CSS Grid
  support. **The failure mode is silent**: the render succeeds, a table row just splits at the wrong
  page boundary. This is why this path is opt-in, not a default.
- **Slower per-render, not faster**: ~500ms measured directly in this environment for a trivial
  document (Python + WeasyPrint import startup dominates), matching the research's cited ~629ms for a
  comparable document — call it 5-10x slower than the Chromium path's warm-pool renders. This path's
  value proposition is a different resource profile (lower baseline memory per render, no browser
  process tree), not speed.
- **Memory is not bounded the way "lightweight" implies**: it scales with document complexity — the
  research doc cites a real case of 5,000+ row tables consuming 1.4GB+. Fine for genuinely simple
  documents; not a blank check for arbitrarily large ones.

## Tech Stack

- WeasyPrint (Python, pip-installed), invoked via its own CLI (`weasyprint - -`, reading HTML from
  stdin, writing PDF to stdout) — confirmed working via `os/exec` subprocess, no cgo/FFI needed.
- No warm pool for v1 — each render spawns a fresh subprocess. This is a real, known cost (~500ms
  overhead per render) accepted for this first slice; pooling long-lived Python workers (the pattern
  the research doc found real projects use, e.g. `weasyprint-service`) is a documented future
  optimization once this path has real usage to justify the added complexity, not built speculatively
  now — matches `incremental-implementation`'s discipline.

## Project Structure

```
internal/lightrender/
  renderer.go       → Renderer: HTML string + options → PDF bytes via WeasyPrint subprocess
  renderer_test.go  → real subprocess tests (skipped if WEASYPRINT_PATH unset), including a test
                       that proves JS is NOT executed (the defining property of this path)
```

Wired into `internal/api` as a new, separate route — not modifying the existing `/v1/pdf/html`
handler or its `pdfRenderer` interface at all.

## API Shape

- `POST /v1/pdf/html-lite` — separate endpoint, separate name, so it can never be reached by a
  client expecting the Chromium path's behavior by accident. Same minimal request/response shape as
  the existing endpoints (raw HTML body in, PDF bytes out) for consistency, but a distinct route.
- Response includes no special marker distinguishing it from the Chromium path's output today (v1
  scope, matching the existing minimal API's lack of a full envelope) — noted as a gap: once
  `conversion-api`'s real response envelope exists, this path's identity should be explicit in it
  (e.g. an `engine: "weasyprint"` field), so a customer can always tell which engine actually
  rendered their document.

## Testing Strategy

- Real subprocess, real WeasyPrint — no mocking, matching `render-engines`' own testing philosophy.
- Skip cleanly (like `CHROMIUM_PATH`) when `WEASYPRINT_PATH` isn't configured, so the normal test
  suite doesn't require WeasyPrint to be installed.
- Explicit test proving JS is never executed — this is the one property this path *must* get right,
  since it's the defining trade-off customers are told about.
- Explicit test demonstrating the documented accuracy limitation is real, not just cited — mirrors
  how `SPEC-render-engines.md` proved its own base-href limitation with a real test rather than just
  a comment.

## Boundaries

- **Always**: keep this fully separate from `render-engines` — no shared code path that could make
  a future change to one silently affect the other's behavior. Document the accuracy trade-off
  wherever this path is described (API docs, code comments) — never let it look like a drop-in
  Chromium substitute.
- **Ask first**: any change that would make this path the *default* for any existing endpoint, or
  any automatic engine-selection logic between this and `render-engines` — both are explicitly ruled
  out by the research this spec is built on.
- **Never**: silently route a `/v1/pdf/html` request to this engine; claim this path is faster or
  more accurate than the Chromium path in any documentation.

## Success Criteria

- [x] `POST /v1/pdf/html-lite` renders valid PDF bytes for real static HTML input
- [x] A real test proves embedded JavaScript is never executed
      (`TestRenderHTML_DoesNotExecuteJavaScript`) — first attempt used byte-equality between a
      script-bearing and script-free render, which failed for a reason unrelated to JS execution
      (DEFLATE-compressed streams cascade byte differences from incidental metadata like an embedded
      timestamp); fixed by rendering uncompressed and substring-searching for a distinctive marker
      the script would have injected, confirmed absent.
- [~] **Adjusted, not delivered as originally scoped**: demonstrating the cited flexbox/CSS-divergence
      GitHub issues exactly would need their precise repro HTML, which wasn't in hand — reproducing an
      approximation risked a low-confidence, potentially flaky test for uncertain payoff. Substituted
      the no-JS-execution proof as the primary, cleanly-verified trade-off demonstration instead; the
      flexbox divergence risk remains real and cited (research doc) but isn't independently re-proven
      here.
- [x] The existing `/v1/pdf/html` (Chromium) path and its tests are completely unaffected — verified
      live: server started with `WEASYPRINT_PATH` unset, `/v1/pdf/html` unaffected, `/v1/pdf/html-lite`
      returns 503; restarted with both configured, both work independently.
- [x] **Measured, not assumed** — same heavy 3-page fixture as the Chromium load test
      (`docs/research/load-test-results.md`), single request, live server:

  | Engine | Latency | Output size | Pages |
  |---|---|---|---|
  | Chromium (`/v1/pdf/html`) | 91ms | 244,195 bytes | 3 |
  | WeasyPrint (`/v1/pdf/html-lite`) | 1,043ms (~11.5x slower) | 25,357 bytes (~10x smaller) | 3 |

  Both agree on page count for this specific document (a table-heavy fixture with no flexbox — the
  one case this test exercises, not a general claim of parity). Confirms the spec's own honest framing:
  this path is not faster, but does produce meaningfully smaller output here, most likely due to
  different image re-encoding/compression than Chromium's PDF image embedding — worth understanding
  further if output size becomes a driving concern, but not investigated further in this slice.
