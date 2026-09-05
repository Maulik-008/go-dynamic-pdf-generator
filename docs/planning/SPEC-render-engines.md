# Spec: render-engines (Phase 1 — HTML + Markdown fidelity path)

Module spec under `CAPABILITY-MAP.md`. Scoped per `PLATFORM-SPEC.md`'s Round 1 decision: v1 is
HTML → PDF and Markdown → PDF only, via the Chromium fidelity path. The fast path (image/PDF
manipulation via pdfcpu/fpdf) is a separate later slice of this same module — not in this spec.

## Objective

A Go package that renders an HTML string (or Markdown compiled to HTML) to PDF bytes, driving a
**pre-warmed pool of headless Chromium instances** over the DevTools Protocol — no Node, no
per-request browser launch. This is the module everything else in the platform depends on; nothing
downstream (`customization-layer`, `job-orchestration`, `conversion-api`) can be built or tested
meaningfully until this exists and is proven correct under load.

Success looks like: a caller gets back valid, correctly-rendered PDF bytes from an HTML or Markdown
string, fast (warm pool, no cold-start tax per request), without leaking Chromium processes or
memory across requests — the concrete answer to the Puppeteer failure modes documented in
`docs/research/go-pdf-generation-research.md` Part 4.

## Tech Stack

- Go 1.26 (toolchain requirement pulled in by `chromedp@latest`, not a deliberate pin — confirm this
  is acceptable for CI/Docker base images before it's a surprise at deploy time; Go's automatic
  toolchain download handles local builds transparently, verified working in this environment, but
  a from-scratch CI runner or Docker build needs network access to fetch it on first build unless
  the base image bakes it in).
- `github.com/chromedp/chromedp` — CDP driver, browser lifecycle via `context.Context`.
- `github.com/yuin/goldmark` — Markdown → HTML (CommonMark-compliant, per research doc Part 5.3).
- Chromium binary: **chrome-headless-shell** (leaner/faster-starting variant per performance
  research), pre-installed in this environment at
  `/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell`. Production
  deployment will vendor the same binary into the Docker image (Gotenberg-style); the path is
  read from an env var (`CHROMIUM_PATH`) with that location as the local-dev default, not
  hardcoded, so CI/prod can point elsewhere.
- `go test` with real Chromium (not mocked) — this module's whole value proposition is correctness
  against a real rendering engine, so tests render real HTML and inspect real PDF bytes.

## Project Structure (this module)

```
internal/renderengines/
  chromium.go        → Renderer: HTML string + options → PDF bytes, single conversion
  chromium_test.go
  pool.go            → Pool: warm, bounded set of browser instances; checkout/checkin
  pool_test.go
  markdown.go        → Markdown → HTML compilation step (goldmark), feeds into chromium.go
  markdown_test.go
  options.go         → RenderOptions type (page size, margins, header/footer, wait strategy — v1
                       subset only; full customization catalog is customization-layer's job, this
                       module just needs enough to be independently useful and testable)
```

## Code Style

Idiomatic Go, `gofmt`/`golangci-lint` defaults. Errors wrapped with context (`fmt.Errorf("...: %w",
err)`), never swallowed. Exported API kept minimal and explicit:

```go
type Renderer struct { /* unexported: pool, config */ }

func NewRenderer(cfg Config) (*Renderer, error)
func (r *Renderer) RenderHTML(ctx context.Context, html string, opts RenderOptions) ([]byte, error)
func (r *Renderer) RenderMarkdown(ctx context.Context, markdown string, opts RenderOptions) ([]byte, error)
func (r *Renderer) Close() error
```

`context.Context` carries per-call timeout/cancellation end-to-end (research doc Part 5.5) —
every render call must accept and honor one.

## Testing Strategy

- **Real Chromium, not a mock** — this module's whole job is correctness against the actual
  rendering engine; a mock would test nothing that matters.
- Unit tests in `internal/renderengines/*_test.go`, run with `go test -race ./internal/renderengines/...`.
- v1 test set (kept small and specific, expanded over time — not the full CJK/RTL/emoji golden-file
  suite yet, that's `customization-layer`'s scope once it exists):
  - Simple HTML renders to valid PDF bytes (starts with `%PDF-`, parses via `pdfcpu` or a minimal
    header check).
  - Markdown renders to valid PDF bytes via the same path.
  - Pool reuse: two sequential renders on the same `Renderer` do not spawn two browser processes
    (assert process count, or assert render 2 is meaningfully faster than a cold-start baseline).
  - Concurrent renders (goroutines calling `RenderHTML` at once) all succeed and don't corrupt each
    other's output.
  - `Close()` leaves no orphaned Chromium processes (assert no child processes survive).
  - A context timeout during render returns an error and doesn't hang.

## Boundaries

- **Always**: honor the passed `context.Context` for cancellation/timeout; return wrapped errors,
  never panic on malformed input HTML; run each Chromium instance in its own properly isolated
  container/pod, never shared across tenants — Chromium runs with `--no-sandbox` here (required in
  most container environments), so the container boundary is the only real isolation layer once
  this executes arbitrary customer-supplied HTML/JS.
- **Ask first**: changing the exported `Renderer`/`RenderOptions` API shape once `customization-layer`
  starts depending on it; adding a second Chromium driver library alongside chromedp.
- **Never**: launch a browser per request (mandatory warm pool, per the speed budget in
  `PLATFORM-SPEC.md`); silently ignore a render error and return partial/empty PDF bytes; run
  without `--no-sandbox`'s container isolation compensated for at the deployment layer.

## Success Criteria

- [x] `RenderHTML` and `RenderMarkdown` produce valid PDF bytes for real test inputs
- [x] Pool reuses browser processes across calls (no per-request cold start)
- [x] Concurrent calls are safe (tested with `-race`)
- [x] `Close()` leaves no orphaned processes
- [x] A cancelled/expired context aborts cleanly, no hang, no leak — including mid-render, not just
      before the call starts (`TestRenderHTML_TimeoutDuringActiveRenderAbortsCleanly`)
- [x] All tests pass in this environment against the real pre-installed Chromium binary
- [x] Relative-resource-URL behavior is documented and tested, not just silently present
      (`TestRenderHTML_BaseHrefResolvesRelativeURLs`)

## Crash recovery (delivered — previously the top deferred gap)

The gap recorded here originally — *"a dead Chromium process keeps receiving ~1-in-N round-robin
requests forever, each failing, until the whole `Pool` is recreated"* — is now **fixed**. See
`docs/research/crash-recovery-research.md` for the full research behind the design.

- [x] A dead instance is **detected**, via `Renderer.alive()` (`browserCtx.Err() != nil`). Verified
      empirically before designing anything: hard-killing a live `headless_shell` makes chromedp
      cancel that instance's browser context (`browserCtx.Done()` fires; `allocCtx` stays nil), so
      death is a free, event-driven signal — no polling loop and no CDP round trip needed. The
      research independently traced the same behavior through chromedp's own source
      (`conn.Read` error → `Browser.LostConnection` → `ExecAllocator` cancels the browser context).
- [x] A dead instance is **replaced in place** (`Pool.restart`), measured at ~90ms to rebuild.
- [x] Concurrent requests hitting the same dead instance produce **exactly one** replacement, not
      one per request (`restartMu` + re-check under the lock) —
      `TestPool_ConcurrentRequestsRestartInstanceOnlyOnce`.
- [x] Dead instances are **skipped at dispatch**: `acquire` tries the next instance rather than
      failing, bounded by pool size so a fully broken pool fails fast instead of looping.
- [x] **Restart storms are prevented** by a per-instance cooldown. This is evidence-based, not
      hygiene: a failed `NewRenderer` was measured at ~1ms, so an uncooled retry-on-demand would let
      every request fork another attempt — thousands per second — whenever Chromium is genuinely
      unstartable. `TestPool_RestartCooldownPreventsStorm`.
- [x] `Pool.Stats()` exposes `Size`/`Alive`/`Restarts`/`RestartAttempts`, both for the health
      endpoints and as the diagnostic signal that matters most: `RestartAttempts` climbing while
      `Restarts` stays flat means Chromium cannot start at all (bad path, OOM, full disk).

**A trap worth recording**: `chromedp.Run` returns a plain `context.Canceled` when the browser dies,
which is *indistinguishable* from the caller cancelling their own request (since `RenderHTML` wires
the caller's context into the task context). Instance death must therefore never be classified from
the returned error — only from `browserCtx.Err()`. The implementation does exactly this.

## Hang detection (delivered)

A *hung* browser is a different fault from a dead one and needs a different detector. Verified by
freezing a live instance with `SIGSTOP` — process present, websocket open, message loop stopped:
`alive()` reported `true` throughout, so the pool considered the instance perfectly healthy while
every render on it hung until its own 30s timeout, forever, with no self-repair.

- [x] Active probe (`Renderer.probe`) issuing a real `Browser.getVersion` CDP round trip, ~1ms
      against a healthy browser. Two details are load-bearing: it runs on the **existing** browser
      context (allocating a fresh tab to ask a health question could itself hang on the very
      browser being diagnosed), and it issues a **real CDP command** — a no-op chromedp action is
      not a probe, measured returning `nil` against a genuinely dead browser because it never talks
      to the browser at all.
- [x] Successful probes cached (`HealthProbeInterval`, 2s) so a healthy instance does not pay a
      round trip per render — asserted by `TestPool_HealthyProbeIsCached` (1 probe across 10
      renders). Failures are never cached, so recovery is visible on the next dispatch.
- [x] Hysteresis (`HealthProbeFailureThreshold`, 2 consecutive failures) so a single blown probe
      under CPU pressure does not rebuild a merely-busy browser — rebuilding across the pool during
      a load spike is how a busy service becomes a restarting one
      (`TestPool_HysteresisToleratesASingleProbeFailure`).
- [x] End-to-end recovery proven against a genuinely frozen instance
      (`TestPool_DetectsAndRecoversFromHungInstance`): the render that would previously have hung
      for 30s now succeeds in ~2.4s because the instance is rebuilt underneath it.
- [x] `PoolStats.HangsDetected` / `ProbesPerformed` surfaced through `/readyz`, reported separately
      from crashes because the causes differ — crashes point at memory or browser bugs, hangs at
      CPU starvation or a wedged message loop.

**A bug this found, which reasoning alone did not**: `restart()`'s double-check (guarding against
two goroutines rebuilding the same slot) originally re-tested `alive()`. That is the *dead*
detector, and a hung browser passes it — so `restart()` short-circuited and handed the caller back
the exact wedged instance it had just diagnosed, skipping the rebuild entirely. The check now
compares **identity** against the Renderer the caller found faulty, which distinguishes "another
goroutine already replaced it" from "it is still the broken one". The hang test caught this; the
dead-instance tests could not have, because on that path `alive()` is false and the short-circuit
never fires.

**A second bug, found only under real concurrent load, not at rest**: every crash/hang test above
kills or freezes Chromium against an otherwise-idle pool, then issues a *new* render to prove
recovery — none had a render actually *in flight* at the moment of death. A chaos test added during
the strict-test/finalisation pass (`internal/api/extreme_load_test.go`,
`TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled`) runs continuous real HTTP traffic
and kills every Chromium process mid-stream. That surfaced a real gap: a request already executing
when the browser died got whatever raw error `chromedp.Run` happened to return — never
`ErrInstanceUnavailable`, since that classification previously only ever ran at the *next*
`acquire()` call — which the HTTP layer reported as a plain `422 RENDER_ERROR`, wrongly telling the
caller their valid document was broken. Fixed by `classifyRenderErr` (`pool.go`): after a render
call returns, check `Renderer.alive()` and reclassify as `ErrInstanceUnavailable` if the instance is
now dead — same principle as the first bug above, classify from `browserCtx.Err()`, never from the
error itself, which is what correctly leaves a caller's own context cancellation unaffected.
Regression-tested directly at the unit level too
(`TestPool_InFlightRenderReclassifiesAsInstanceUnavailableWhenKilledMidRender`), and confirmed live:
re-running the same chaos scenario after the fix showed 255/257 requests succeeding, 2 correctly
reported as `503 ENGINE_UNAVAILABLE`, zero `422`s and zero transport errors.

## Proactive recycling after N renders — measured, and deliberately NOT implemented

The research recommended recycling to bound memory growth, citing Gotenberg's `restart-after=100`
while recording that **no published source justifies that number**. Rather than adopt it, this was
measured: `internal/api/soak_test.go` (`SOAK=1`) renders the heavy 3-page load-test fixture 400
times against one warm instance, sampling RSS. Full data in `docs/research/soak-test-results.md`.

**Result: +5.8 MB across 400 renders — 1.6% of a 356 MB baseline, ~0.015 MB/render.** The curve does
not climb; it oscillates between ~360 and ~364 MB and goes *down* at 150, 225, 325 and 400 renders.
A genuine leak is monotonic; a series that repeatedly returns to where it started is noise and
ordinary allocator behaviour. Render latency stayed flat (76-104ms) with no upward drift.

So the feature would cost and buy nothing here: `restart-after=100` would have destroyed and rebuilt
a healthy browser **four times** during that run, each costing ~90ms plus warm state, to reclaim
memory that was not being lost — and it would additionally need drain machinery (acquiring every
outstanding concurrency token before swapping) that the reactive path legitimately does not, since a
dead instance's in-flight renders already fail immediately rather than being interrupted mid-work.

This is not a claim that Gotenberg's default is wrong for Gotenberg — different build, workloads and
process model, and it must be safe across all of them. The narrow point is that the number was never
justified by a published measurement, and this service's own measurement says it is unnecessary.
`soak-test-results.md` states the three conditions that would reverse the decision (a different
document profile, a multi-hour horizon, or observed growth in production).
