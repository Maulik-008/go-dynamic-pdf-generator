# Plan: render-engines Phase 1 (HTML + Markdown fidelity path)

Implementation plan for `docs/planning/SPEC-render-engines.md`.

## Components and dependencies

1. **Go module scaffold** — `go.mod` (`github.com/Maulik-zuru/great-pdf-generator`), directory
   layout per the platform spec. No dependencies; everything else needs this first.
2. **`options.go`** — `RenderOptions` type (v1 subset: format/orientation/margins, header/footer
   template strings, wait strategy). No dependencies.
3. **`chromium.go`** — `Renderer.RenderHTML`: single-shot HTML → PDF via chromedp against the
   pre-installed headless_shell binary. Depends on (2).
4. **`pool.go`** — wraps (3)'s browser allocation in a warm, bounded pool (checkout/checkin over a
   channel, per the research doc's rod-`BrowserPool`-style pattern). Depends on (3).
5. **`markdown.go`** — goldmark Markdown → HTML, then delegates to (3)/(4). Depends on (3).
6. **Minimal HTTP wiring** (`cmd/api` + a thin handler) — proves the module works end-to-end over
   HTTP, not just as a Go API. Depends on (4), (5). *Deliberately minimal* — full request
   validation, error envelope, and the customization catalog are `conversion-api`'s and
   `customization-layer`'s modules, not this slice's job.

## Order (sequential where dependent, parallel where not)

Sequential: 1 → 2 → 3 → (4 and 5 can happen in parallel, both only need 3) → 6.

## Risks and mitigations

- **Headless Chromium behaves differently under `--no-sandbox` in this container vs. a real
  deployment target.** Mitigation: keep the exact launch-flag set in one place (`chromium.go`),
  documented, so it's a one-line change when deployment environment specifics are known.
- **Pool reuse could leak state (cookies/JS globals) across renders if given a shared browser
  *context*, not just a shared browser *process*.** Mitigation: per the research doc's finding —
  reuse the process, allocate a fresh CDP browser context per render. Tested explicitly (two
  renders in a row must not see each other's page state).
- **Zombie processes on `Close()`/crash**, the #1 Puppeteer failure mode from research. Mitigation:
  `context.Context` cancellation drives teardown (not a bare `os.Process.Kill()` call with no
  verification); tested explicitly per the Success Criteria.

## Verification checkpoints

- After (3): a single render produces valid PDF bytes — checkpoint before building the pool on top
  of an unverified renderer.
- After (4): pool reuse and concurrency tests pass with `-race` — checkpoint before wiring HTTP.
- After (6): a real `curl` against the running server returns a valid PDF for both an HTML and a
  Markdown body — the actual "does this work end to end" proof.

Task breakdown: `tasks/todo.md`.
