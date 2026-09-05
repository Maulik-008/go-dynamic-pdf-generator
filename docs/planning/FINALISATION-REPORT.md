# Finalisation Report: Strict Test Pass, Heavy Load, Chaos Testing, Requirements Cross-Check

Written in response to an explicit request for a strict, no-illusions pass over the whole
platform: every requirement cross-checked against real test evidence, heavy/extreme load testing
actually performed (not just described), and an honest verdict — not a self-congratulatory "yes".
Every number below comes from a real run against real Chromium (`chrome-headless-shell`) and real
WeasyPrint, executed during this pass, on this repository's current commit. Nothing here is
projected or assumed.

## Verdict

**The platform is production-ready for the scope it actually claims (v1: HTML + Markdown → PDF via
Chromium, plus the separate opt-in WeasyPrint path), with the operational hardening from the
previous pass (crash recovery, hang detection, health probes, logging, multi-replica scaling) now
also proven under real concurrent load and real chaos, not just at rest.** One real correctness bug
was found by this pass's chaos testing and is now fixed and regression-tested (see below). Every
capability explicitly listed as v1 scope is implemented, tested, and — for anything with a
speed/reliability claim — measured. Everything *not* in v1 scope (auth, billing, storage/webhooks,
sandbox mode, idempotency keys, URL/image input, the fast path, golden-file visual regression) is
still genuinely missing, exactly as the specs already say — this pass does not change that, and does
not paper over it.

## What this pass added

1. **112 total test functions** across the module suite (100 before this pass, +12 new: 9 strict
   edge-case tests, 2 heavy/chaos load tests, 1 direct regression test for the bug found below),
   all passing with `-race`, both with and without real engines wired in.
2. **A strict edge-case suite** (`internal/api/strict_test.go`, 9 new tests) covering gaps the
   existing suite had never exercised: the exact `REQUEST_TOO_LARGE` byte boundary, unknown-route
   (404) and wrong-method (405) routing, `Content-Length` correctness, every error path setting
   `Content-Type: application/json`, concurrent cross-talk across all three conversion routes at
   once, and graceful shutdown composed with an in-flight request (readiness fails immediately,
   the already-admitted request still completes).
3. **A heavy/extreme load + chaos suite** (`internal/api/extreme_load_test.go`, 2 new tests):
   sustained multi-wave overload against the heavy 3-page fixture (not a single burst), and —
   the one genuinely new kind of test in this codebase — killing every Chromium process
   **while real concurrent HTTP traffic is actively flowing**, not against an idle pool. Every
   existing recovery test in `internal/renderengines` proves the pool repairs itself for the
   *next* caller; this proves it holds up for callers already in flight when the failure happens.
4. **One real bug found and fixed** by that chaos test (see "Bug found" below), plus a direct
   unit-level regression test for it.
5. **A full re-run of the existing heavy-document load test and its orchestration follow-up**,
   refreshing `docs/research/load-test-results.md` with today's numbers on this environment.
6. **This document** — a full cross-check of every Success Criteria line in `CAPABILITY-MAP.md`,
   `PLATFORM-SPEC.md`, and all five `SPEC-*.md` files against concrete test evidence.

## Bug found and fixed this pass: in-flight render misclassified as 422 on Chromium death

**How it was found**: not by reasoning — by running `TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled`,
which fires continuous real HTTP traffic and hard-kills every Chromium process mid-stream. Four
requests that were already executing when the kill landed came back `422 RENDER_ERROR` instead of
either succeeding or a `503 ENGINE_UNAVAILABLE`.

**Root cause**: `Pool.RenderHTML`/`RenderMarkdown`/`MeasureHTMLHeight` returned whatever raw error
`chromedp.Run` produced when the browser died mid-call. The existing `ErrInstanceUnavailable`
classification only ever ran at the *next* `acquire()` call, for the *next* request — never
retroactively for the request that was running when the instance died. `writeRenderError` then had
no way to tell "the browser just died under you" apart from "your HTML is broken", and fell through
to the generic `422` — telling a caller with a perfectly valid document not to bother retrying, when
retrying against the pool's self-healed replacement was exactly correct.

**Fix**: `classifyRenderErr` (`internal/renderengines/pool.go`) checks `Renderer.alive()`
(`browserCtx.Err()`) after every render call returns an error, and reclassifies as
`ErrInstanceUnavailable` if the instance is now dead — never by inspecting the error's type or text,
for the same reason the original crash-recovery design already establishes: `chromedp.Run` returns a
plain `context.Canceled` on browser death, indistinguishable by inspection from the caller's own
request being cancelled. Checking `alive()` correctly separates the two, since a caller cancelling
their own request never touches the instance's `browserCtx`.

**Verified fixed**, twice:
- Unit level: `TestPool_InFlightRenderReclassifiesAsInstanceUnavailableWhenKilledMidRender`
  (`internal/renderengines/recovery_test.go`) — kills Chromium mid-render (using the same
  never-resolving-fonts-promise technique as the existing timeout test) and asserts the error wraps
  `ErrInstanceUnavailable`, returned in ~1.4s, not the render's own 8s timeout.
- Live, under load: re-running the chaos test after the fix — **255/257 requests succeeded, 2
  correctly reported `503 ENGINE_UNAVAILABLE`, zero `422`s, zero transport errors, zero unexpected
  outcomes.**

## Real load & chaos test results (this run, this environment: 4 vCPU, 15GB RAM)

### Heavy-document load test (refreshed today — `docs/research/load-test-results.md`)

Pool: 3 Chromium instances × 4 concurrent renders each (12 total capacity). Fixture: the existing
3-page heavy report (embedded charts + 50-row table, 21,597 bytes HTML).

| Concurrency | Requests | Failed | Throughput | p50 | p90 | p99 |
|---|---|---|---|---|---|---|
| 1 | 8 | 0 | 12.34/s | 79ms | 86ms | 86ms |
| 2 | 16 | 0 | 20.47/s | 96ms | 106ms | 107ms |
| 4 | 32 | 0 | 23.98/s | 157ms | 187ms | 219ms |
| 8 | 64 | 0 | 25.62/s | 293ms | 365ms | 418ms |
| 16 | 128 | 0 | 25.48/s | 552ms | 832ms | 956ms |

Every level, every percentile, stayed inside the platform's own speed budget (p50 < 600ms, p99 <
2.5s) — the closest this repo has come to a clean pass on this criterion (see the corrected
`PLATFORM-SPEC.md` checkbox). Peak Chromium RSS during the sweep: 2,256MB (idle baseline: 1,052MB
for 3 warm instances).

Orchestration backpressure re-check, same pool, 3x admitted capacity (72 concurrent requests against
24 admitted slots): **24 succeeded, 48 rejected in 42–93ms each, zero other failures** — immediate
admission-control rejection, not delayed-then-failed requests.

### Sustained multi-wave overload (new — `TestExtremeLoad_SustainedOverloadWaves`)

Same heavy fixture, 5 consecutive waves at 4x admitted capacity (96 concurrent requests per wave,
24 admitted each time), pool sized 3×4:

| Wave | Succeeded | Rejected (503) | Other failures | Chromium RSS after | Goroutines |
|---|---|---|---|---|---|
| 1 | 24 | 72 | 0 | 1,077.8 MB | 45 |
| 2 | 24 | 72 | 0 | 1,084.5 MB | 45 |
| 3 | 24 | 72 | 0 | 1,082.9 MB | 45 |
| 4 | 24 | 72 | 0 | 1,084.3 MB | 45 |
| 5 | 24 | 72 | 0 | 1,083.1 MB | 45 |

Perfectly flat: memory varies by under 7MB (0.6%) across five full overload waves, goroutine count
is identical every time. No slow leak under repeated punishment — the deterministic backpressure
(exactly 24 admitted, 72 rejected, every single wave) is itself evidence the admission gate isn't
degrading either. After `Close()`: **zero surviving Chromium processes, goroutine count back within
20 of baseline** — both asserted, not just logged.

### Chaos under real concurrent load (new — `TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled`)

Pool: 2 instances × 2 concurrency. 4 goroutines firing continuous requests for ~8 seconds; every
running Chromium process (7 of them) hard-killed 1 second in, load continues for 6 more seconds.

- **257 total requests, 255 succeeded (99.2%), 2 correctly reported 503 ENGINE_UNAVAILABLE, 0
  transport errors, 0 unexpected outcomes.**
- `Pool.Stats().Restarts >= 1` confirmed, `Alive == Size` confirmed after recovery.
- Final-second success rate: 100% (well above the 80% bar the test enforces) — sustained recovery
  under load, not a lucky single request.
- Zero surviving Chromium processes after `Close()`, confirmed by polling, not assumed.

## Requirements cross-check matrix

Every Success Criteria line from `CAPABILITY-MAP.md`, `PLATFORM-SPEC.md`, and all five
`SPEC-*.md` files, mapped to concrete evidence. `✅ done` = implemented and tested; `📐 measured` =
a speed/reliability claim backed by a real run; `⛔ deferred` = explicitly out of v1 scope by
design, not a gap; `⚠️ open` = a real, acknowledged gap.

### `render-engines`

| Criterion | Status | Evidence |
|---|---|---|
| HTML/Markdown render to valid PDF | ✅ | `TestRenderHTML_ProducesValidPDF`, `TestRenderMarkdown_ProducesValidPDF` |
| Pool reuses processes, no cold start per request | ✅ | `TestPool_ReusesProcessesAcrossRenders` |
| Concurrent calls safe | ✅ | `TestPool_ConcurrentRendersAreSafe`, full suite run with `-race` |
| `Close()` leaves no orphans | ✅ | `TestPool_ClosesWithoutOrphanedProcesses`, plus leak checks in both new load tests |
| Context cancellation clean, incl. mid-render | ✅ | `TestRenderHTML_TimeoutDuringActiveRenderAbortsCleanly` |
| Relative-URL (`base href`) behavior tested | ✅ | `TestRenderHTML_BaseHrefResolvesRelativeURLs` |
| Crash recovery (dead instance detected + replaced) | ✅ | `TestPool_RecoversFromKilledInstance`, restart cost ~90ms measured |
| Restart storms prevented | ✅ | `TestPool_RestartCooldownPreventsStorm` |
| Hang detection (frozen, not dead) | ✅ | `TestPool_DetectsAndRecoversFromHungInstance` |
| Recovery holds under real concurrent load, not just idle | ✅ **(new this pass)** | `TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled` — 99.2% success through a live kill |
| In-flight failure correctly classified, not blamed on the document | ✅ **(bug found + fixed this pass)** | `TestPool_InFlightRenderReclassifiesAsInstanceUnavailableWhenKilledMidRender`, live chaos re-run |
| Proactive recycling after N renders | ⛔ deferred, measured | `docs/research/soak-test-results.md`: +5.8MB / 400 renders (1.6%), decided against — 3 explicit falsification conditions recorded |
| Golden-file visual regression (CJK/RTL/emoji) | ⚠️ open | Not started — see PLATFORM-SPEC.md |

### `job-orchestration`

| Criterion | Status | Evidence |
|---|---|---|
| Hard concurrency cap enforced | ✅ | `TestPool_EnforcesConcurrencyCap` (live in-flight counter, not timing) |
| Queue-full rejected immediately | ✅ | `TestPool_QueueFullRejectsImmediately` |
| Context cancellation while queued | ✅ | `TestSubmit_ContextCancelledWhileQueuedReturnsPromptly` |
| `QueueDepth`/`QueueCapacity` accurate | ✅ | `TestPool_QueueDepthAndCapacityReportLiveState` |
| `InFlight`/`Capacity` as the real saturation signal | ✅ | `TestPool_InFlightIsTheSaturationSignal` |
| Wired into both Chromium and WeasyPrint paths, isolated | ✅ | separate `jobPool`/`staticJobPool`, `TestHandleHTML_QueueFullReturns503WithRetryAfter` |
| Backpressure holds under sustained repeated overload, not just one burst | 📐 **(new this pass)** | `TestExtremeLoad_SustainedOverloadWaves` — identical 24/72 split, 5 waves running |
| Live server backpressure (real curl, not test harness) | ✅ | recorded in `SPEC-job-orchestration.md` |

### `customization-layer` (v1 + v2)

| Criterion | Status | Evidence |
|---|---|---|
| Merge: simple/nested fields, `{{range}}` | ✅ | `TestMerge_SimpleField`, `TestMerge_NestedField`, `TestMerge_RangeOverArray` |
| Missing field names itself in the error | ✅ | `TestMerge_MissingFieldErrorsNamingField` |
| Malformed template → clear parse error, no panic | ✅ | `TestMerge_MalformedTemplateSyntaxErrors` |
| Payload HTML/script escaped (HTML **and** Markdown template) | ✅ | `TestMerge_EscapesHTMLInPayloadValue_HTMLTemplate`/`_MarkdownTemplate` |
| Header/footer height mismatch rejected before render | ✅ | `TestValidateHeaderFooterHeight_RejectsHeaderTallerThanMargin`/`_RejectsFooterTallerThanMargin` |
| `MeasureHTMLHeight` reflects real content, not viewport stub | ✅ | `TestMeasureHTMLHeight_ReflectsContentSize` (quirks-mode bug found + fixed during original TDD) |
| Wired into HTTP (options schema) | ⛔ deferred | Needs `conversion-api`'s full options schema — not built (out of scope, recorded) |
| Watermark, custom fonts, PDF/A | ⛔ deferred | Not started, recorded as future slices |

### `conversion-api`

| Criterion | Status | Evidence |
|---|---|---|
| Unified `{content, payload?}` envelope on every route | ✅ | `TestHandleHTML_Success`, `TestHandleMarkdown_Success`, `TestHandleHTMLLite_Success` |
| Consistent JSON error envelope, every code | ✅ | `TestWriteJSONError_AlwaysSetsJSONContentType` **(new this pass)** across 4 distinct error paths |
| `INVALID_REQUEST` (empty/malformed/missing content) | ✅ | `TestHandleHTML_EmptyBodyRejected`, `_MissingContentRejected`, `_MalformedJSONRejected` |
| `REQUEST_TOO_LARGE` exact byte boundary | ✅ **(new this pass — previously untested)** | `TestDecodeConversionRequest_BodyOneByteOverLimitRejected`/`_BodyExactlyAtLimitIsNotTooLarge`, `TestHandleHTML_RequestTooLargeReturns413` |
| `TEMPLATE_ERROR` / `RENDER_ERROR` / `QUEUE_FULL` distinguished | ✅ | existing suite + `TestHandleHTML_MergeErrorSurfacesAs422` |
| `ENGINE_UNAVAILABLE` distinct from `RENDER_ERROR`, incl. in-flight death | ✅ | `TestHandleHTML_EngineUnavailableIsNot422`, chaos test (this pass) |
| Unknown route / wrong method behave correctly | ✅ **(new this pass — previously untested)** | `TestRoutes_UnknownPathReturns404`, `TestRoutes_WrongMethodReturns405` |
| `Content-Length` matches actual body | ✅ **(new this pass)** | `TestHandleHTML_ContentLengthHeaderMatchesBody` |
| Concurrent, distinct requests across all 3 routes don't cross-talk | ✅ **(new this pass)** | `TestConcurrentMixedRoutes_NoCrossTalk`, 60 concurrent goroutines under `-race` |
| Old `-template` routes fully removed | ✅ | confirmed no remaining references |
| `Idempotency-Key` | ⛔ deferred, reasoned | needs `storage-and-delivery`'s persistence — building a half-mechanism was explicitly rejected |

### Health, logging, shutdown, scaling (from the previous hardening pass, re-verified here)

| Criterion | Status | Evidence |
|---|---|---|
| `/livez` stays up regardless of Chromium health | ✅ | `TestLivez_StaysUpWhileDependenciesAreDown` |
| `/readyz` fails on zero live instances, not on saturation | ✅ | `TestReadyz_FailsWhenNoInstanceIsAlive`, `TestReadyz_DoesNotFailWhenMerelySaturated` |
| Shutdown: readiness fails immediately, in-flight work still finishes | ✅ **(composed together for the first time this pass)** | `TestGracefulShutdown_InFlightRequestCompletesWhileReadyzReports503` |
| Structured JSON logging, request IDs, level-appropriate severity | ✅ | `internal/observability` suite, 13 tests |
| Multi-replica: stateless, one replica's death doesn't affect another | ✅ | live-verified (previous pass), documented in `docs/guides/04-deployment.md` |
| Deployment artifacts (Dockerfile/compose/systemd) | ⚠️ open, explicitly | reviewed, never built/started — no Docker daemon or systemd PID 1 in this environment; stated plainly in the guide itself |

### Whole-suite hygiene (cross-cutting, this pass)

| Check | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `gofmt -l .` | empty (no formatting diffs) |
| `go test ./... -race` (no real engines) | all packages pass |
| `go test ./... -race` (`CHROMIUM_PATH` + `WEASYPRINT_PATH` set) | all packages pass, every real-engine integration test runs |
| Goroutine leak check after sustained overload | pass — within 20 of baseline after `Close()` |
| Process leak check after sustained overload | pass — zero `headless_shell` processes survive `Close()` |
| Process leak check after chaos (kill + recovery under load) | pass — zero surviving processes after `Close()` |

## Genuinely out of scope — not gaps in what was tested, gaps in what was built

These are unchanged by this pass and were never claimed as done. Listed here so "strict cross-check"
means something — a report that only lists passes and omits what's missing isn't a cross-check:

- **`auth-and-tenancy`** — no API keys, no rate limiting, no usage metering. Not started at all.
- **`storage-and-delivery`** — no async jobs, no webhooks, no object storage, no job-status endpoint.
- **Sandbox/test mode** — blocked on the still-deferred watermark slice in `customization-layer`.
- **`Idempotency-Key` enforcement** — deferred with reasoning (needs a persisted side effect to
  deduplicate against, which doesn't exist yet).
- **Fast path** (image → PDF, merge/split/watermark/encrypt) — not started; a different, browser-free
  engine phase, sequenced after v1 in the roadmap.
- **URL → PDF** — not started; needs SSRF guarding, explicitly deferred.
- **Golden-file visual regression (CJK/RTL/emoji/mixed-language)** — not started. This is the one
  gap inside `render-engines`' own stated Testing Strategy that remains genuinely open.
- **CI wiring** — no `.github/workflows` or equivalent exists in this repository. Every test in this
  report was run manually, on this session's environment. Nothing here runs automatically on a push.
- **Docker/systemd deployment artifacts** — reviewed, never built or started (no Docker daemon,
  systemd isn't PID 1, in this development environment). This is stated plainly in
  `docs/guides/04-deployment.md` and unchanged by this pass.
- **No authentication or rate limiting at all** — this service must not be exposed directly to the
  public internet. Repeated here because it's the single most important operational caveat and
  belongs in a finalisation report, not just a deployment guide's fine print.

## How to reproduce every result in this report

```bash
export CHROMIUM_PATH=/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell
export WEASYPRINT_PATH=/usr/local/bin/weasyprint

# Hygiene
go build ./... && go vet ./... && gofmt -l .

# Full suite, both without and with real engines
go test ./... -race

# Heavy-document load test + orchestration backpressure follow-up
LOADTEST=1 go test ./internal/api/... -run TestLoadHeavyDocument$ -v -timeout 280s
LOADTEST=1 go test ./internal/api/... -run TestLoadHeavyDocument_WithOrchestration -v

# New: sustained multi-wave overload with leak checks
LOADTEST=1 EXTREME_POOL_SIZE=3 EXTREME_MAX_CONCURRENCY=4 EXTREME_WAVES=5 EXTREME_OVERLOAD_FACTOR=4 \
  go test ./internal/api/... -run TestExtremeLoad_SustainedOverloadWaves -v -timeout 280s

# New: chaos under real concurrent load
LOADTEST=1 CHAOS_POOL_SIZE=2 CHAOS_MAX_CONCURRENCY=2 \
  go test ./internal/api/... -race -run TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled -v -timeout 60s

# Memory-growth soak test (already run in the previous pass; not re-run here — see
# docs/research/soak-test-results.md; it takes several minutes and its conclusion is unchanged)
SOAK=1 go test ./internal/api/... -run TestSoakMemoryGrowth -v -timeout 600s
```
