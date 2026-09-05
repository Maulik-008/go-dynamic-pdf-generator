# Tasks: render-engines Phase 1

- [x] Task: Scaffold Go module and directory layout
  - Acceptance: `go build ./...` succeeds on an empty scaffold
  - Verify: `go build ./...`
  - Files: `go.mod`, `internal/renderengines/` (empty package files)

- [x] Task: Implement `RenderOptions` (v1 subset)
  - Acceptance: struct compiles, sensible zero-value defaults (A4, no header/footer, network-idle wait)
  - Verify: `go build ./...`
  - Files: `internal/renderengines/options.go`

- [x] Task: Implement `Renderer.RenderHTML` (single-shot, no pool yet)
  - Acceptance: renders a simple HTML string to bytes starting with `%PDF-`
  - Verify: `go test ./internal/renderengines/... -run TestRenderHTML -v`
  - Files: `internal/renderengines/chromium.go`, `chromium_test.go`
  - Note: first implementation derived each call's context from the raw
    allocator, which turned out to spawn a brand-new browser process per
    call (verified via a process-count diagnostic — 1 → 6 → 11 processes
    across two renders). Fixed by starting one persistent browser-attached
    context in `NewRenderer` and deriving every call from that instead.

- [x] Task: Implement warm pool wrapper
  - Acceptance: two sequential renders reuse one browser process; concurrent renders (`-race`) all
    succeed; `Close()` leaves no orphaned processes; a cancelled context aborts cleanly
  - Verify: `go test ./internal/renderengines/... -run TestPool -race -v`
  - Files: `internal/renderengines/pool.go`, `pool_test.go`
  - Note: implemented as N warm Chromium instances (real process-level
    parallelism, not just tabs), each internally bounded to
    MaxConcurrencyPerInstance concurrent tabs (default 6, Gotenberg's own
    number) — the concrete shape of the platform spec's "~8-12 instances x
    ~6 concurrent" v1 scale target.

- [x] Task: Implement Markdown → PDF path
  - Acceptance: a Markdown string renders to valid PDF bytes via the same pool
  - Verify: `go test ./internal/renderengines/... -run TestRenderMarkdown -v`
  - Files: `internal/renderengines/markdown.go`, `markdown_test.go`

- [x] Task: Wire minimal HTTP handler
  - Acceptance: `POST /v1/pdf/html` and `/v1/pdf/markdown` return a valid PDF for a real request
  - Verify: run `go run ./cmd/api` and `curl` both endpoints, confirm `%PDF-` response bytes
  - Files: `cmd/api/main.go`, `internal/api/handlers.go`, `internal/api/handlers_test.go`
  - Verified live: real server run, `curl` against both endpoints confirmed
    via `file` as valid 1-page PDFs (12KB/17KB, ~50ms each), SIGTERM
    shutdown confirmed via `pgrep` to leave zero orphaned Chromium
    processes.

- [x] Task: Full suite + commit
  - Acceptance: `go test ./... -race` green, working tree committed and pushed
  - Verify: `go test ./... -race`
  - Files: —

All Phase 1 tasks complete: HTML→PDF and Markdown→PDF work end-to-end through
a warm, multi-instance Chromium pool, over a real (if minimal) HTTP API.
Remaining render-engines success criteria from SPEC-render-engines.md not yet
covered: the golden-file visual regression suite and the CJK/RTL/emoji
fixture set (customization-layer's scope per the module spec) — next slice.

- [x] Task: `/review` pass (code-review-and-quality, five axes) + fix all findings
  - Acceptance: 4 Important + 6 Suggestion findings addressed; 3 FYIs required no action
  - Verify: `go test ./... -race` green (16/16, up from 14), `gofmt -l .` and `go vet ./...` clean,
    live curl re-verification after the fixes
  - Files: `internal/renderengines/{chromium,pool}.go`, `internal/renderengines/chromium_test.go`,
    `internal/api/handlers.go`, `cmd/api/main.go`, `docs/planning/SPEC-render-engines.md`
  - Notable: two of the new tests initially failed on first write, for real reasons worth recording
    — `document.baseURI`/`.src` resolution against "about:blank" behaves differently than assumed
    (raw literal, not an "about:blank"-prefixed string), and a local httptest.Server used to
    simulate a hung image load got blocked outright by Chromium's Private Network Access policy
    (about:blank-origin fetches to private addresses are treated as cross-origin-to-private and
    rejected, not hung) — switched to a deterministic never-resolving-Promise override of
    `document.fonts` instead, with zero network dependency.
  - Deferred, not dropped: automatic recovery from a crashed pool instance — recorded explicitly in
    SPEC-render-engines.md's Success Criteria as job-orchestration's job, not silently skipped.

- [x] Task: heavy-document load test (performance-optimization skill's measure-first workflow)
  - Acceptance: a real, opt-in, repeatable load test against a heavy 2-3 page document (embedded
    images + a large data table) through the actual HTTP path, at multiple concurrency levels, with
    latency percentiles + throughput + memory sampled and written up with a data-driven conclusion
  - Verify: `LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument -v
    -timeout 300s` (skips cleanly when LOADTEST is unset, confirmed in the normal suite run)
  - Files: `internal/api/heavy_content_test.go`, `internal/api/loadtest_test.go`,
    `docs/research/load-test-results.md`, `docs/planning/PLATFORM-SPEC.md` (Scale Target + Success
    Criteria updated with the findings)
  - Two real findings, not guesses: (1) throughput plateaus at ~26-27 heavy-renders/sec on this
    4-core test box regardless of pool size (3 vs 4 instances) — the bottleneck is CPU cores, not
    pool slot count, so `PoolConfig.Size` should track `runtime.NumCPU()`, not a fixed target;
    (2) the pool has no bounded queue/backpressure (confirmed by the earlier code review, now
    quantified) — latency degrades gracefully under overload but never rejects, which
    job-orchestration's queue design needs to fix. Zero failures across 1,550 total requests in
    both configs; zero orphaned processes after either run.

- [x] Task: lightweight-render — a separate, opt-in Chromium-free path (WeasyPrint)
  - Acceptance: a genuinely separate module and endpoint (`POST /v1/pdf/html-lite`), never touching
    or silently substituting for the existing Chromium path (`/v1/pdf/html`); real tests proving its
    defining property (no JS execution); measured, not assumed, comparison against Chromium on the
    same heavy fixture
  - Verify: `WEASYPRINT_PATH=weasyprint go test ./internal/lightrender/... ./internal/api/... -race
    -v`; live server verified both with and without `WEASYPRINT_PATH` set
  - Files: `docs/planning/SPEC-lightweight-renderer.md`, `docs/planning/CAPABILITY-MAP.md` (new
    `lightweight-render` module row), `internal/lightrender/{renderer,renderer_test}.go`,
    `internal/api/handlers.go` (new route + `staticPdfRenderer` interface, deliberately separate
    from `pdfRenderer`), `internal/api/handlers_test.go`, `cmd/api/main.go` (optional wiring, guarding
    against Go's nil-pointer-in-interface trap when `WEASYPRINT_PATH` is unset)
  - Reframing that made this safe to build: the platform research
    (`docs/research/resource-optimization-research.md` §3) recommended *against* a Chromium-free path
    specifically because silently auto-detecting "no JS" and routing to a different engine risks a
    customer-invisible accuracy regression. This is not that — it's a distinctly-named, explicitly
    opt-in endpoint the caller must deliberately choose, with the trade-off documented up front.
  - Real numbers, not assumptions, on the same heavy 3-page fixture used for the Chromium load test:
    Chromium 91ms/244,195 bytes vs. WeasyPrint 1,043ms/25,357 bytes, both agreeing on page count for
    this specific (table-heavy, no-flexbox) document — confirms this path trades real speed for a
    different resource profile, exactly as documented, not a hidden win.
  - A test-writing lesson worth recording: the first version of the no-JS-execution test compared
    full PDF byte-equality and failed for a reason that had nothing to do with JavaScript —
    DEFLATE-compressed PDF streams cascade large byte differences from a single incidental metadata
    difference (e.g. an embedded creation timestamp). Verified this via a direct diff before
    concluding anything, then fixed the test to render uncompressed and substring-search for a
    distinctive marker instead — a more robust methodology, not just a workaround for one failure.

- [x] Task: job-orchestration — bounded queue + backpressure gating both render engines
  - Acceptance: a hard concurrency cap and a bounded queue in front of both `renderengines.Pool`
    (Chromium) and `lightrender.Renderer` (WeasyPrint) via separate, independently-sized pools;
    requests beyond capacity rejected immediately with 503 + Retry-After instead of piling up
    unboundedly; the heavy-document load test re-run to prove it
  - Verify: `go test ./... -race` (green with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH` set);
    `LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument_WithOrchestration -v`;
    live curl verification of a real 503 + Retry-After under a saturated single-worker server
  - Files: `docs/planning/SPEC-job-orchestration.md`, `internal/orchestration/{pool,pool_test}.go`,
    `internal/api/handlers.go` (`jobPool`/`staticJobPool` fields, `submit` helper,
    `writeRenderError`), `internal/api/handlers_test.go` (new `TestHandleHTML_QueueFullReturns503WithRetryAfter`),
    `internal/api/loadtest_test.go` (new `TestLoadHeavyDocument_WithOrchestration`),
    `cmd/api/main.go` (wiring + env-configurable `JOB_QUEUE_CAPACITY`/`WEASYPRINT_WORKERS`/
    `WEASYPRINT_QUEUE_CAPACITY`), `docs/planning/PLATFORM-SPEC.md` (Scale Target implementation
    note), `docs/research/load-test-results.md` (appended "Follow-up" section)
  - A real bug found and fixed during TDD, not just a clean build on the first try: the first
    design used a single channel (buffer size = `QueueCapacity`) as both the admission gate and the
    worker-dispatch queue. This raced at pool startup (an unbuffered `QueueCapacity: 0` channel
    needs an already-scheduled worker for a non-blocking send to succeed — the very first `Submit`
    after `NewPool` could be spuriously rejected before the worker goroutines finished starting) and
    silently over-admitted at steady state (a job leaving the buffer once a worker picked it up
    freed capacity for a new admission even though the running job still logically occupied a
    slot) — caught by `go test -race -count=10` and by a queue-full test that initially failed for
    the wrong reason. Fixed by separating the *admission* decision (a semaphore sized
    `Workers+QueueCapacity`) from the *dispatch* mechanism (a FIFO channel to the fixed worker
    pool), so channel length only ever reports genuine queue depth.
  - Real, measured proof the backpressure fix works, not just unit tests: same pool config as the
    original heavy-document load test (3 Chromium instances x 4 concurrency = 12 real capacity),
    now gated by an orchestration pool (Workers=12, QueueCapacity=12, admits 24 at once), fired at
    3x that (72 concurrent requests): 24 succeeded, 48 rejected in 42-58ms each, zero other
    failures — versus the original run's documented "no rejections, ever, just growing latency."
  - Deliberately out of scope, per the module spec: async/job-status execution (needs
    `storage-and-delivery`, not this module) and swapping the in-process channel for a distributed
    queue (anticipated by `PLATFORM-SPEC.md`'s Scale Target section as a later, deliberate change
    behind the same interface shape).

- [x] Task: `/review` pass on job-orchestration + fix all findings
  - Acceptance: two Important findings and two Suggestions from the five-axis review addressed
  - Verify: `go test ./... -race` green (with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH`);
    `go test ./internal/orchestration/... -race -count=10` clean after the sleep→poll test changes
  - Files: `internal/lightrender/renderer.go` (+test), `docs/planning/SPEC-job-orchestration.md`,
    `internal/api/handlers.go`, `internal/orchestration/pool_test.go`
  - Important #1 (real bug, not theoretical): `lightrender.Renderer.RenderHTML` had no self-imposed
    timeout — it relied entirely on the caller's `ctx`, unlike `renderengines.RenderHTML` which
    always wraps every render in `context.WithTimeout(taskCtx, opts.Timeout)` regardless of the
    caller. Combined with job-orchestration giving WeasyPrint its first-ever *shared, small* worker
    pool, a single pathologically slow render (a known failure class for CSS layout engines) would
    permanently pin one of only `runtime.NumCPU()` worker slots forever — a handful of such requests
    zeroes out `/v1/pdf/html-lite` capacity until process restart, a failure mode that didn't exist
    before this pool existed (each call used to get its own disposable subprocess). Fixed by adding
    `RenderOptions.Timeout` (defaulting to 30s, matching renderengines) and wrapping `ctx` with
    `context.WithTimeout` inside `RenderHTML` — verified live: a 1ns timeout with a `context.Background()`
    caller ctx (no deadline at all) still fails in ~0ms rather than hanging
    (`TestRenderHTML_OptsTimeoutBoundsRenderIndependentOfCtx`).
  - Important #2 (doc/code mismatch): `SPEC-job-orchestration.md`'s "Design" section still showed
    the original single-channel pseudocode that TDD had already found buggy and replaced — a future
    reader following the spec's own reference code would have reintroduced the fixed race. Rewrote
    the Design section to describe the actual two-channel (admission semaphore + dispatch channel)
    implementation, and added a caveat (also cross-linked from Success Criteria) that `QueueDepth()`
    is an approximation, not an exact count, when fronting `renderengines.Pool`'s round-robin
    dispatch (a job orchestration considers "running" can itself still be queued one layer down).
  - Suggestion: named the previously-hardcoded `Retry-After: "1"` literal as
    `queueFullRetryAfterSeconds` in `internal/api/handlers.go` for clarity — still a deliberate v1
    fixed value, not derived from queue depth.
  - Suggestion: replaced fixed `time.Sleep(shortWait)`-then-assert "settle" patterns in
    `internal/orchestration/pool_test.go` with a `waitUntil` poll-with-timeout helper for the three
    positive ("this should eventually become true") assertions — the genuine negative assertion
    ("job2 must NOT have started within a bounded window") was left as a fixed wait, since a negative
    claim can't be usefully polled away. Confirmed clean across `-race -count=10` after the change.

- [x] Task: customization-layer v1 — data-bound templates (template + payload merge)
  - Acceptance: an HTML or Markdown template with Go-template placeholders (`{{.field}}`), merged
    against a JSON payload, feeding the existing `renderengines.RenderHTML`/`RenderMarkdown`
    unchanged; payload values HTML-escaped by construction; a missing field errors naming the exact
    field; two new HTTP routes wired end-to-end through the existing Chromium admission pool
  - Verify: `go test ./... -race` (green with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH`); live
    curl against a running server — invoice-style template with a `{{range}}` loop and an injection
    attempt (200, valid PDF), missing field (422, error names the field), missing template (400),
    existing `/v1/pdf/html`/`/v1/pdf/markdown`/`/v1/pdf/html-lite` routes unaffected; clean SIGTERM
    shutdown, zero orphaned processes
  - Files: `docs/planning/SPEC-customization-layer.md`, `internal/customization/{merge,merge_test}.go`,
    `internal/api/handlers.go` (new `templateRequest` type, `renderTemplate` helper, two new routes),
    `internal/api/handlers_test.go`
  - Deliberately scoped down from customization-layer's full capability-map responsibility (page
    setup/header-footer/watermark/fonts/PDF-A), matching the same incremental-delivery discipline as
    every prior module (render-engines: HTML+Markdown before its fast path; lightweight-render: no
    pooling before optimizing) — this slice is data-driven templates only, the single most
    repeatedly-named "turns 'my way' into something that scales" feature in the platform research.
    Header/footer auto-height validation, watermark, custom fonts, and PDF/A are explicitly deferred,
    not dropped — see the spec's own scope note.
  - A deliberate, documented technology choice worth recording: Go's stdlib `html/template`, not a
    Liquid implementation (the pattern the research cites as competitors' reference, e.g. PDFMonkey).
    Chosen for zero new dependency (this project's consistent practice — go.mod is still just
    chromedp+goldmark) and, more importantly, a real security property: `html/template` contextually
    HTML-escapes every merged value by default, where `text/template`/Liquid would not — and since
    `render-engines` executes the merged result in a real Chromium instance, an unescaped payload
    value would be a genuine HTML/script injection vector, not a cosmetic bug. Verified this actually
    matters for the Markdown path too (not just HTML): confirmed via `internal/renderengines/markdown.go`
    that `goldmark.Convert` runs with default settings, which pass raw HTML through verbatim per
    CommonMark — an unescaped `<script>` in a merged Markdown template would have executed just the
    same. Wrote a real end-to-end test through the actual `goldmark.Convert` dependency (not just
    asserting on `Merge`'s own output) to confirm the escape survives the round-trip, rather than
    assuming it from the CommonMark entity-decoding theory alone — it passed on the first run,
    confirming the theory rather than replacing the need to check it.
  - Also verified Go's own `missingkey=error` template option produces an error that already names
    the exact missing field (satisfying PLATFORM-SPEC.md's "name the exact missing template
    variable" requirement) without needing a separate typed field-name parser — confirmed both by a
    unit test and live: `map has no entry for key "missing"`.

- [x] Task: customization-layer v2 — header/footer auto-height validation
  - Acceptance: a header/footer template taller than its configured margin is rejected with a
    specific, region-naming error before an actual PDF render, not silently rendered overlapping —
    the named regression class from real Puppeteer issues (#10024, #13738, #4132/#4266)
  - Verify: `go test ./... -race` (green with and without `CHROMIUM_PATH`); real-Chromium
    integration test proving both rejection (200px header vs 0.4in margin) and acceptance (10px
    header vs 0.4in margin)
  - Files: `internal/renderengines/chromium.go` (+test) — new `Renderer.MeasureHTMLHeight`;
    `internal/renderengines/pool.go` (+test) — `Pool.MeasureHTMLHeight`;
    `internal/customization/{heightvalidation,heightvalidation_test}.go` — `HeightMeasurer`,
    `ValidateHeaderFooterHeight`, `HeightMismatchError`; `docs/planning/SPEC-customization-layer.md`
    (new v2 section)
  - A real bug found and fixed during TDD, not theoretical: the first version of
    `MeasureHTMLHeight` injected the header/footer HTML fragment directly with no wrapper document.
    A test asserting a 400px-tall div measures taller than a 20px-tall one failed — both measured
    as exactly the viewport height (1000px) regardless of actual content. Root-caused with a direct
    diagnostic (not guessed): a fragment with no `<!DOCTYPE html>` loads in quirks mode
    (`document.compatMode == "BackCompat"`), and in quirks mode `body` stretches to fill the
    viewport, so `scrollHeight` reports the viewport size instead of the content's height. Confirmed
    the fix with the same diagnostic: wrapping the fragment in a minimal `<!DOCTYPE html>` document
    forces standards mode (`compatMode == "CSS1Compat"`), after which the 400px div correctly
    measures ~400px. Every subsequent test passed once this was fixed.
  - Also re-confirmed the previously-known flaky `TestPool_ClosesWithoutOrphanedProcesses`
    ("before=38, during=38" process-count noise) reappeared once during this work; re-ran it in
    isolation (5/5 clean) and as part of the full package (clean) before concluding it's the same
    environmental noise already root-caused earlier in this project, not a regression from this
    change — did not skip or silence it.
  - Deliberately not wired into any HTTP route this slice: the current minimal API's existing
    routes all call `renderengines.DefaultRenderOptions()` unconditionally, so `HeaderTemplate`/
    `FooterTemplate`/margins are already unexposed via HTTP for every caller today, not just this
    one — adding HTTP exposure for only these two fields would be a partial, premature options
    schema, which is explicitly `conversion-api`'s job. `ValidateHeaderFooterHeight` is proven as a
    real, tested Go capability now; `conversion-api` calls it once the real options schema exists.

- [x] Task: conversion-api v1 — unified request envelope, structured errors, request validation
  - Acceptance: every conversion endpoint (`/v1/pdf/html`, `/v1/pdf/markdown`, `/v1/pdf/html-lite`)
    accepts one JSON shape (`{"content", "payload"?}`) and returns one JSON error envelope
    (`{"error": {"code", "message"}}`) on every failure mode; `/v1/pdf/html-template` and
    `/v1/pdf/markdown-template` removed, folded into the main routes via the optional `payload`
    field
  - Verify: `go test ./... -race` (green with and without `CHROMIUM_PATH`/`WEASYPRINT_PATH`); live
    curl — success on html/html-lite (both with payload), `INVALID_REQUEST` (missing content,
    malformed JSON), `TEMPLATE_ERROR` (missing payload field), `QUEUE_FULL` (with `Retry-After: 1`),
    `healthz`; clean SIGTERM shutdown
  - Files: `docs/planning/SPEC-conversion-api.md`, `internal/api/handlers.go` (full rewrite:
    `conversionRequest`, `errorEnvelope`/`errorBody`, `writeJSONError`, `decodeConversionRequest`,
    `mergeIfNeeded`, `writePDF`, `handleConversion`), `internal/api/handlers_test.go` (rewritten for
    the JSON envelope, `-template`-specific tests folded into the main ones with a `payload` field),
    `internal/api/loadtest_test.go` (request construction moved from raw-body to the JSON envelope
    via a new `conversionRequestBody` helper — the load test's actual measurement is unaffected)
  - A deliberate, documented consolidation, not scope creep: `/v1/pdf/html-template` and
    `/v1/pdf/markdown-template` (added one slice ago) are removed outright rather than kept
    alongside the unified routes — nothing outside this repo depends on the old shape yet
    (pre-launch), and keeping two request shapes for the same capability would itself violate the
    "one mental model, not five" requirement this module exists to satisfy. Confirmed via `grep`
    that no reference to the old routes/types remains anywhere in the codebase.
  - A deliberate scope boundary, reasoned through and recorded rather than silently skipped:
    `Idempotency-Key` enforcement is *not* implemented this slice, even though `PLATFORM-SPEC.md`
    lists it as a v1 requirement. Every conversion endpoint today is a pure, side-effect-free
    compute operation — no database row, no metered quota, no async job — so a retry has nothing to
    duplicate yet, and real idempotency-key handling (atomic claim, payload-hash guard, replay
    decision) needs a persistent store to claim against, which is `storage-and-delivery`'s
    dependency, not built yet. Accepting the header now and doing nothing real with it would be
    worse than not building it — it would tell a client retrying is safe when the guarantee doesn't
    exist. Same reasoning applied to sandbox/test mode (needs `customization-layer`'s still-deferred
    watermark) and async job-id/webhook dispatch (needs `storage-and-delivery`).
  - Live-verified a real, if minor, operational detail worth recording: SIGTERM shutdown briefly
    showed `<defunct>` (zombie) Chromium processes via `pgrep` immediately after the signal, fully
    reaped within 2 seconds. Investigated rather than assumed clean — zombies are already-dead
    processes awaiting parent reaping, not a resource leak; re-checked after a short wait to confirm
    they cleared rather than accepting the first, alarming-looking result at face value.
