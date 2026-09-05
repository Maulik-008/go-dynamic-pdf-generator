# Spec: job-orchestration

## Objective

Bound the number of renders the platform will run or admit at once, and reject fast and cheaply
once that bound is exceeded, instead of letting requests pile up indefinitely. This directly closes
a gap the heavy-document load test found and quantified
(`docs/research/load-test-results.md`): neither `renderengines.Pool` nor `lightrender.Renderer`
reject anything — under overload, latency degrades gracefully but every request is eventually
served (or times out on the client side), which means an unbounded number of goroutines and queued
Chromium/WeasyPrint work can accumulate with no signal to the caller that the system is saturated.

Per the capability map: "Worker pool, queue, concurrency limits, sync vs. async execution,
backpressure, autoscaling signal (queue depth)." This slice covers concurrency limits, sync
backpressure (reject over capacity), and the queue-depth signal. Async/job-status execution is
explicitly out of scope here — it depends on `storage-and-delivery` (job persistence, webhooks) per
the capability map's own dependency graph, and isn't needed to fix the problem this phase exists to
fix.

## Why a new module, not a change inside `render-engines`/`lightrender`

`renderengines.Pool` already does its own internal concurrency bookkeeping (round-robin dispatch,
per-instance tab semaphores) — that's engine-level resource management and stays there. This module
is a layer *in front of* both render paths that answers a different question: "should this request
even be admitted right now?" Keeping it separate means:

- `lightrender.Renderer` gets its first-ever concurrency bound without touching its own code at all
  — today it spawns one WeasyPrint subprocess per call with zero limit, a real, separate gap this
  module also closes as a side effect of being engine-agnostic.
- The queue-full/backpressure policy can be tuned (or swapped for a distributed queue later, per
  `PLATFORM-SPEC.md`'s Scale Target section) without touching either render engine's code.
- `PLATFORM-SPEC.md`'s Scale Target already commits to exactly this shape: *"job-orchestration
  defines its queue behind a small internal interface... V1's implementation is an in-process Go
  channel."*

## Tech Stack

Stdlib only — a buffered Go channel as the bounded queue, a fixed goroutine pool as the workers,
Go 1.26 generics so one `Pool` type works for both `[]byte`-returning render engines without an
`any`-typed, type-asserting API. No new dependency.

## Design

**This section describes the actual shipped implementation** (`internal/orchestration/pool.go`) —
an earlier draft of this section sketched a simpler single-channel design that TDD found to be
buggy (see "Success Criteria" below for the two concrete failure modes it had); that draft is
intentionally not reproduced here so a future reader can't mistake it for the real design.

```go
package orchestration

type PoolConfig struct {
    Workers       int // concurrent job goroutines; < 1 treated as 1
    QueueCapacity int // jobs allowed to wait beyond Workers before rejecting; < 0 treated as 0
}

var ErrQueueFull = errors.New("orchestration: queue is full, try again later")

type Pool struct {
    admission chan struct{} // counting semaphore, capacity Workers+QueueCapacity
    jobs      chan func()   // FIFO handoff to Workers goroutines, same capacity
    queueCap  int
    // ...
}

func NewPool(cfg PoolConfig) *Pool

// Submit admits fn for execution if the pool has not reached
// Workers+QueueCapacity jobs already admitted, blocks until fn completes or
// ctx is done, and returns ErrQueueFull immediately (without blocking) once
// that many jobs are already admitted.
func Submit[T any](ctx context.Context, p *Pool, fn func(context.Context) (T, error)) (T, error)

func (p *Pool) QueueDepth() int    // len(p.jobs): admitted jobs waiting for a free worker
func (p *Pool) QueueCapacity() int // the configured QueueCapacity

// Close stops accepting new work signal and waits for every already-admitted
// job to finish. Precondition: no concurrent Submit calls in flight when
// Close is called from a caller's perspective for a *new* request; mirrors
// renderengines.Renderer.Close/Pool.Close's own documented precondition, so
// cmd/api's existing shutdown ordering (HTTP server stops accepting new
// requests and drains in-flight ones first, via httpServer.Shutdown, before
// any pool's Close runs) already satisfies it.
func (p *Pool) Close()
```

Two separate mechanisms do the work, deliberately not one channel: **admission** is a counting
semaphore (capacity `Workers+QueueCapacity`) gating the *total* number of jobs `Submit` will accept
at once (running plus queued) — a single non-blocking send, `select { case p.admission <- struct{}{}:
default: return zero, ErrQueueFull }` — this is the actual backpressure boundary. **`jobs`** is the
FIFO handoff to a fixed pool of `Workers` goroutines, gating how many *run concurrently*; its buffer
is sized to the same `Workers+QueueCapacity`, so a send to it can never block once admission has
already succeeded. `QueueCapacity: 0` remains a legitimate, meaningful config (no waiting line at
all: admit only if a worker is immediately free), not a sentinel for "unset."

Why two mechanisms instead of sizing one channel to `QueueCapacity` and admitting via a non-blocking
send directly on it (the simpler design this section used to describe): a job leaving that channel's
buffer the instant a worker picks it up — while the job is still running — would let a new admission
in past the intended total, and an unbuffered (`QueueCapacity: 0`) channel needs an already-scheduled
worker for a non-blocking send to succeed at all, so the very first `Submit` after `NewPool` could be
spuriously rejected before the worker goroutines finished starting. Both were real bugs caught by
`go test -race` during TDD, not theoretical — see "Success Criteria" below.

Each `Submit` call gets its own 1-buffered result channel so the worker goroutine can always
deliver its result and exit even if the caller's context is cancelled first and nobody's listening
by the time `fn` finishes — no goroutine leak, no result-channel send blocking forever.

**Caveat on `QueueDepth()`'s accuracy** (found during review, not fixed in code — see rationale
below): `QueueDepth()` counts a job as "running" (out of the queue) the instant a `Pool` worker
goroutine dequeues it. When this `Pool` fronts `renderengines.Pool` (Chromium), that worker can then
itself block inside `renderengines.Pool.acquire()`, which dispatches strictly round-robin to one
specific Chromium instance and only ever waits on *that* instance's semaphore — it never falls back
to an idle sibling instance. So under an unlucky arrival pattern, a "running" job here can actually
still be waiting one layer down, meaning `QueueDepth()` can transiently under-report the real
backlog and the achievable concurrency can transiently be less than `Workers`. This is a property of
`renderengines.Pool`'s existing, already-tested dispatch algorithm (unchanged by this module) doing
exactly what it was built to do, not a bug in either module — and the load test in the Success
Criteria below (24/24 admitted requests succeeding) didn't observe any practical degradation from
it. Treat `QueueDepth()` as a close approximation for autoscaling purposes, not an exact concurrent-
render count.

## Wiring into `internal/api`

Two separate `*orchestration.Pool` instances, sized to each engine's real cost profile — not one
shared pool, since Chromium and WeasyPrint have very different concurrency characteristics and
sharing a single admission gate between them would let one engine's saturation reject the other
engine's requests for no reason:

- **Chromium** (`renderengines.Pool`): `Workers = PoolConfig.Size * MaxConcurrencyPerInstance` —
  the pool's own real total concurrent-render capacity (already measured and documented in
  `load-test-results.md`), so this layer's admission limit matches what the engine can actually do
  concurrently rather than introducing a second, differently-tuned bottleneck underneath it.
- **WeasyPrint** (`lightrender.Renderer`): `Workers` defaults to `runtime.NumCPU()` — WeasyPrint has
  no concurrency bound today at all, so this is a new, first-ever cap, sized conservatively to
  available cores per the resource-optimization research's own CPU-bound finding.
- `QueueCapacity` for both defaults to `Workers` (configurable via env) — allow up to roughly double
  capacity in flight (running + briefly queued) before rejecting, a starting heuristic consistent
  with the "extendable/changeable" scale-target framing in `PLATFORM-SPEC.md`, not a permanent
  constant.

`Server` gains two `*orchestration.Pool` fields (nil-able, same pattern as `staticRenderer` nil
meaning "not configured"). A `nil` job pool means "no admission gate, call the render function
directly" — kept specifically so the many existing handler unit tests that use `fakeRenderer` /
`fakeStaticRenderer` don't need an orchestration pool wired in just to exercise handler logic;
production wiring in `cmd/api/main.go` always supplies a real pool for both engines.

`orchestration.ErrQueueFull` is translated to HTTP `503 Service Unavailable` with a `Retry-After`
header (a small fixed value, e.g. `1` second) — distinct from the existing `422` used for genuine
render failures, so a client can tell "the system is busy, retry" apart from "your document failed
to render."

## Project Structure

```
internal/orchestration/
  pool.go       → Pool, PoolConfig, Submit, ErrQueueFull, QueueDepth/QueueCapacity, Close
  pool_test.go  → correctness under normal load, deterministic queue-full rejection, concurrency-cap
                  enforcement (measured via a live-in-flight counter, not inferred from timing),
                  ctx-cancellation-while-queued aborts cleanly with no goroutine leak, an
                  integration test wrapping the real renderengines.Pool
```

## Testing Strategy

- Real concurrency, not mocked scheduling: use a counter of currently-executing jobs (guarded by
  `atomic`) plus a controllable blocking gate (a channel each fake job blocks on until told to
  proceed) to prove the concurrency cap is actually enforced — never more than `Workers` jobs run
  at once — deterministically, not by racing on sleep durations.
- Deterministic queue-full test: fill `Workers + QueueCapacity` slots with jobs blocked on a gate,
  then assert the very next `Submit` returns `ErrQueueFull` immediately (bounded by a short
  deadline, not by hoping it's fast).
- `-race` throughout, matching every other module's testing philosophy in this codebase.
- One integration test wired to a real `renderengines.Pool` (skipped without `CHROMIUM_PATH`,
  matching the existing skip pattern), proving the wrapper works against the real engine, not just
  fakes.
- Handler-level test in `internal/api`: a tiny orchestration pool (`Workers=1, QueueCapacity=0`)
  wired into `Server`, two concurrent requests where the render function blocks until released,
  asserting the second gets `503` with `Retry-After` while the first is still in flight.

## Boundaries

- **Always**: keep this a thin layer in front of both render engines — no engine-specific logic
  inside `internal/orchestration` itself (it only knows `func(context.Context) (T, error)`).
- **Ask first**: swapping the in-process channel for a distributed queue (Redis/NATS-backed) —
  `PLATFORM-SPEC.md`'s Scale Target already anticipates this as a later, deliberate change behind
  the same interface shape, not something to do speculatively now.
- **Never**: let one engine's saturation reject the other engine's requests (separate pools, not a
  shared one); let `Submit` block forever with no path to `ErrQueueFull` or `ctx.Done()`.

## Success Criteria

- [x] `Submit` enforces a hard concurrency cap (`Workers`), proven by a test with a live in-flight
      counter, not inferred from latency (`TestPool_EnforcesConcurrencyCap`).
- [x] Requests beyond `Workers + QueueCapacity` are rejected immediately via `ErrQueueFull`, proven
      deterministically (fill every slot with blocked jobs, then assert immediate rejection —
      `TestPool_QueueFullRejectsImmediately`). Caught and fixed a real bug during TDD: a
      single-channel design (buffer = `QueueCapacity` only) both raced at startup (an unbuffered
      `QueueCapacity: 0` channel needs an already-scheduled worker for a non-blocking send to
      succeed, so the very first `Submit` after `NewPool` could be spuriously rejected) and
      over-admitted at steady state (a job leaving the buffer once picked up by a worker freed
      capacity for another admission even though the running job still occupied a "slot"). Fixed by
      separating the admission decision (a semaphore sized `Workers+QueueCapacity`) from the
      worker-dispatch mechanism (a FIFO channel), so channel-length only ever reports true queue
      depth and admission is never subject to goroutine-scheduling timing.
- [x] A cancelled `ctx` while a job is queued (not yet running) returns promptly without leaking the
      queued job's goroutine (`TestSubmit_ContextCancelledWhileQueuedReturnsPromptly`).
- [x] `QueueDepth()`/`QueueCapacity()` report accurate live numbers within this module's own
      bookkeeping — the "autoscaling signal" the capability map calls for
      (`TestPool_QueueDepthAndCapacityReportLiveState`). See the Design section's caveat above for
      the one case this doesn't cover: a "running" job can itself still be queued one layer down
      inside `renderengines.Pool`'s round-robin dispatch.
- [x] Wired into both `/v1/pdf/html` + `/v1/pdf/markdown` (Chromium) and `/v1/pdf/html-lite`
      (WeasyPrint) via separate pools; a saturated pool on one path never affects the other
      (`internal/api/handlers.go`'s `jobPool`/`staticJobPool` fields, `cmd/api/main.go`'s wiring).
- [x] `ErrQueueFull` surfaces as HTTP `503` + `Retry-After`, distinct from the existing `422`
      render-failure path — verified both by a handler test
      (`TestHandleHTML_QueueFullReturns503WithRetryAfter`) and live: a real server with
      `POOL_SIZE=1 MAX_CONCURRENCY_PER_INSTANCE=1 JOB_QUEUE_CAPACITY=0`, two concurrent real
      requests, second returned `503`/`Retry-After: 1` in 0.5ms while the first completed normally.
- [x] The heavy-document load test is re-run against the new, orchestration-gated server
      (`TestLoadHeavyDocument_WithOrchestration`) and shows deterministic fast rejections under
      overload instead of the previously-documented unbounded latency growth with zero rejections —
      closing the loop on the finding that motivated this module. Real result at 3x admitted
      capacity (workers=12, queue=12, 72 concurrent requests): 24 succeeded, 48 rejected in
      42-58ms each, zero other failures. See `docs/research/load-test-results.md`'s "Follow-up"
      section.
- [x] `go test ./... -race` green (both without `CHROMIUM_PATH`/`WEASYPRINT_PATH`, and with both set
      so every real-engine integration test runs too); existing render-engines and lightrender test
      suites completely unaffected.
