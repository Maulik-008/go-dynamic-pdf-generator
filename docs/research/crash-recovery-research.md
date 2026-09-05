# Research: Crash Recovery for the Warm Chromium Pool

**Date**: 2026-08-30
**Scope**: How to detect and recover from a dead/hung Chromium instance inside
`internal/renderengines.Pool`, without recreating the whole pool.
**Status**: Research + recommendation only. No implementation.

## The problem, restated against the current code

`Pool.acquire` (`internal/renderengines/pool.go:106`) picks a slot by
`p.next.Add(1) % len(p.instances)` and never consults the health of the slot it picked.
`poolSlot` holds a `*Renderer` that is written once at construction and never replaced.
`Renderer` (`internal/renderengines/chromium.go:48`) holds an `allocCtx`/`browserCtx` pair
created once in `NewRenderer` and torn down once in `Close`.

Consequence: if instance *i* dies, `~1-in-N` requests keep being routed to it, each failing,
until the process restarts. `docs/planning/SPEC-render-engines.md:112-117` already records this
as the known deferred gap.

Two facts from `docs/research/load-test-results.md` shape the recommendation:

- Pool sizing is CPU-bound (~1 saturating instance per core), so N is small — 3-4 on the test
  box, 8-16 on a production node. Losing one instance is a 25-33% capacity loss, not a rounding
  error. Recovery matters.
- Memory is ~349 MB/instance idle, ~750-790 MB/instance under sustained heavy-document load.
  That is the cost of a spare instance and the reason unbounded memory growth is worth bounding.

---

## Q1 — What Gotenberg actually does

Gotenberg's recovery is not in the Chromium module; it is a generic `ProcessSupervisor` in
`pkg/gotenberg/supervisor.go`, parameterised for Chromium in `pkg/modules/chromium/chromium.go`.
All of the following is read directly from `main` branch source (fetched 2026-08-30), not from
the docs site (`gotenberg.dev` is blocked by this environment's egress proxy, so flag defaults
below come from the source's own flag registration, which is the authoritative definition anyway).

### The flags and their real defaults

From `pkg/modules/chromium/chromium.go:461-466`:

```go
fs.Int64("chromium-restart-after", 100, "Number of conversions after which Chromium will automatically restart. Set to 0 to disable this feature")
fs.Int64("chromium-max-queue-size", 0, "Maximum request queue size for Chromium. Set to 0 to disable this feature")
fs.Duration("chromium-idle-shutdown-timeout", 0, "Shutdown Chromium after being idle for the given duration. Set to 0 to disable this feature")
fs.Int64("chromium-max-concurrency", 6, "Maximum number of concurrent conversions. Chromium supports up to 6")
fs.Bool("chromium-auto-start", false, "Automatically launch Chromium upon initialization if set to true; otherwise, Chromium will start at the time of the first conversion")
fs.Duration("chromium-start-timeout", time.Duration(20)*time.Second, "Maximum duration to wait for Chromium to start or restart")
```

Wired at `chromium.go:541`:

```go
mod.supervisor = gotenberg.NewProcessSupervisor(mod.logger, "chromium", mod.browser,
    flags.MustInt64("chromium-restart-after"),
    flags.MustInt64("chromium-max-queue-size"),
    mod.maxConcurrency,
    flags.MustDuration("chromium-idle-shutdown-timeout"))
```

Two cross-checks worth noting:

- `chromium-max-concurrency` defaults to **6**, and `chromium.go:681` hard-rejects anything
  outside 1-6 (`"chromium-max-concurrency must be between 1 and 6, got %d"`). This repo's
  `MaxConcurrencyPerInstance` default of 6 matches the reference implementation exactly.
- `chromium-restart-after` and `chromium-max-queue-size` are **separate** concerns. Restart-after
  is proactive recycling. Max-queue-size is admission control (backpressure), which is the gap
  `load-test-results.md:106-113` flags separately. Do not conflate them.

### Architectural caveat before borrowing anything

**Gotenberg runs exactly one Chromium process per container** and scales horizontally by running
more containers. Its supervisor therefore supervises a single process and is free to stop the
world while restarting it. This repo runs N instances *inside one process*. So Gotenberg's
`ProcessSupervisor` maps to a **per-slot** supervisor here, not a pool-level one — and the
"drain everything before restarting" step must drain only the affected slot.

### `Run()` — the request path

`supervisor.go:285-368`. Per request, in order:

1. **Queue admission** — CAS loop on `reqQueueSize` against `maxQueueSize`; returns
   `ErrMaximumQueueSizeExceeded` if full. The CAS (rather than load-then-store) is an explicit
   fix for a TOCTOU race, cited to gotenberg#951. The decrement is deferred over the *whole* of
   `Run`, cited to gotenberg#1502.
2. **`acquireSlot`** — semaphore of size `maxConcurrency`. Crucially, after acquiring a token it
   re-checks `isRestarting` and, if a restart drain is in progress, immediately hands the token
   back and returns `ErrProcessAlreadyRestarting` (supervisor.go:451-469).
3. **`ensureStarted`** — lazy first launch, serialised on a `sync.Mutex` (not `sync.Once`,
   deliberately, so a failed launch can be retried by the next caller instead of poisoning the
   supervisor for the container's lifetime — cited to gotenberg#1538).
4. **`ensureHealthy`** — if `!isRestarting && !process.Healthy()`, do a **synchronous** restart
   before running the task (supervisor.go:521-533). This is the crash-recovery path.
5. Run the task.
6. **`maybeRestartAfterTask`** — proactive recycling, below.

The outer loop retries the whole thing on `ErrProcessAlreadyRestarting` after a 10 ms sleep.

### `chromium-restart-after` — exactly what it does

`supervisor.go:539-561`:

```go
func (s *processSupervisor) maybeRestartAfterTask(logger *slog.Logger) bool {
	if s.maxReqLimit <= 0 || s.reqCounter.Load() < s.maxReqLimit {
		return false
	}
	if !s.restartMutex.TryLock() {
		return false
	}
	s.logger.DebugContext(context.Background(), "max request limit reached, restarting eagerly...")
	go func() {
		restartErr := s.doRestartLocked(context.Background(), "max_requests")
		s.restartMutex.Unlock()
		...
		<-s.semaphore   // releases the caller's slot, which this goroutine now owns
	}()
	return true
}
```

So: after every completed conversion, if the conversion counter has reached the limit, the
supervisor `TryLock`s the restart mutex (non-blocking — a losing caller just declines and the
next completion tries again) and restarts **asynchronously**, taking ownership of the finishing
caller's semaphore token so the slot is not handed to a new request mid-restart.
`restart()` (supervisor.go:205-225) is Stop-then-Launch and resets `reqCounter` to 0.

### The drain, which is the part worth copying

`doRestartLocked` (supervisor.go:573-602):

```go
s.isRestarting.Store(true)
defer s.isRestarting.Store(false)

// Drain all other active semaphore slots so no other tasks are running during the restart.
slotsToAcquire := s.maxConcurrency - 1
for range slotsToAcquire {
    select {
    case s.semaphore <- struct{}{}:
        acquired = append(acquired, struct{}{})
    case <-ctx.Done():
        for range acquired { <-s.semaphore }
        return fmt.Errorf("drain active tasks before restart: %w", ctx.Err())
    }
}
err := s.tracedLaunch(ctx, reason, func() error { return s.runWithDeadline(ctx, s.restart) })
for range acquired { <-s.semaphore }
```

The restart acquires **every remaining semaphore token** before touching the process, so the
restart is guaranteed to happen with zero in-flight tasks. It backs the whole acquisition out
if the context dies mid-drain. This is the correct answer to "what about in-flight renders on
the instance being replaced": you already own a semaphore of exactly the right size; use it as
the drain barrier.

### Health check

`processSupervisor.Healthy()` (supervisor.go:227-283) adds three things on top of the raw probe:

- **Non-started is healthy** — because Gotenberg lazily starts Chromium, reporting unhealthy
  before the first request would make orchestrators kill the pod. *Not applicable here*: this
  repo starts eagerly in `NewRenderer`, deliberately.
- **A 2-second success cache** (`healthCheckCacheTTL`) so kubelet-style liveness+readiness probes
  don't put a CDP roundtrip on the websocket every few seconds. Failures are never cached.
- **A consecutive-failure threshold of 2** (`healthFailureThreshold`): "Under load, a single
  blown CDP timeout is more likely transient pressure than a dead process." The code cites
  gotenberg#1561 for this.

> **Uncertainty flagged**: I read `healthFailureThreshold = 2` and its comment directly from
> source. When I fetched gotenberg#1561 the visible thread was an open report about intermittent
> ~30s CDP silence/hangs, and I could **not** see the threshold discussion in it. The constant
> and its rationale are verified; the issue thread as the source of that rationale is not.

Sources:
- https://github.com/gotenberg/gotenberg/blob/main/pkg/gotenberg/supervisor.go
- https://github.com/gotenberg/gotenberg/blob/main/pkg/modules/chromium/chromium.go
- https://github.com/gotenberg/gotenberg/blob/main/pkg/modules/chromium/browser.go
- https://github.com/gotenberg/gotenberg/issues/951, /1502, /1538, /1561, /1599

---

## Q2 — How chromedp signals that the browser is dead

### The documented answer: `context canceled`

From chromedp's own README FAQ (verified verbatim):

> **I'm seeing "context canceled" errors**
>
> When the connection to the browser is lost, `chromedp` cancels the context, and it may result
> in this error. This occurs, for example, if the browser is closed manually, or if the browser
> process has been killed or otherwise terminated.

— https://github.com/chromedp/chromedp#readme

That is the whole of the documented contract. It is thin, and — as shown below — dangerously
ambiguous for this codebase. The mechanism behind it is worth reading, because it hands us a far
better signal.

### The mechanism: `Browser.LostConnection` → cancel of the browser context

`chromedp/browser.go:44-47`:

```go
// LostConnection is closed when the websocket connection to Chrome is
// dropped. This can be useful to make sure that Browser's context is
// cancelled (and the handler stopped) once the connection has failed.
LostConnection chan struct{}
```

It is closed by the websocket read goroutine's `defer` when `conn.Read` returns any error
(`browser.go:255-266`).

`chromedp/allocate.go:263-276`, inside `ExecAllocator.Allocate`:

```go
go func() {
    // If the browser loses connection, kill the entire process and
    // handler at once. ...
    <-browser.LostConnection
    select {
    case <-browser.closingGracefully:
    default:
        c.cancel()
    }
}()
```

`c` here is `FromContext(ctx)` where `ctx` is the context passed to `Allocate` — i.e. the
context created by `chromedp.NewContext(allocCtx)` (`allocate.go:127-131`, and
`chromedp.go:290-308` `initContextBrowser`).

**Therefore, in this repo: when the Chromium process dies or its websocket drops,
`Renderer.browserCtx` is cancelled.** `r.browserCtx.Done()` is a free, allocation-free,
zero-CDP-roundtrip liveness signal that already exists in the struct today. This is the single
most useful finding for this codebase.

Two corollaries:

- `chromedp.FromContext(browserCtx).Browser.LostConnection` is also directly readable if a
  channel is preferred to `Done()`. `Browser.Process() *os.Process` (`browser.go:147`) is
  likewise exposed and documented for "a monitoring system to collect process metrics".
- The process is started with `exec.CommandContext(ctx, ...)` (`allocate.go:172`), so cancelling
  the browser context also kills the process. The signal and the kill are the same edge.

### Caveat: this is derived behaviour, not a documented API

chromedp does not document "cancelling browserCtx means the browser died" as a stable contract;
it documents the *symptom* (`context canceled`). The propagation path above is an implementation
detail of `ExecAllocator.Allocate` on `master` as of this research. Mitigation: pin chromedp,
and keep an active probe (below) as a second, contract-stable signal.

### Caveat: `context.Canceled` is ambiguous *in this codebase specifically*

`RenderHTML` (`chromium.go:107-121`) builds `taskCtx` from `browserCtx`, then layers
`opts.Timeout` and then a goroutine that cancels `taskCtx` when the **caller's** `ctx` is done.
So `chromedp.Run(taskCtx, ...)` returns `context.Canceled` in at least three unrelated cases:

1. the browser died (browserCtx cancelled),
2. the HTTP client hung up (caller ctx cancelled),
3. some other action cancelled the task context.

Tracing the failure path confirms case 1 surfaces as plain `context.Canceled`: `Run` →
`initContextBrowser` (Browser is non-nil, inherited) → `newTarget` → `!c.first` →
`target.CreateTarget(...).Do(...)` → `Browser.execute`, which returns `ctx.Err()` from its
`select` (`browser.go:227-229`). There is no distinguishing sentinel.

**Conclusion: do not classify instance death from the error value.** Classify from
`r.browserCtx.Err() != nil`. `chromedp`'s exported sentinels (`errors.go`) — `ErrChannelClosed`,
`ErrInvalidTarget`, `ErrInvalidContext` — are also reachable on a dying browser but are not
reliable discriminators either.

### A third signal: renderer/tab crash without browser death

`cdproto` exposes `inspector.EventTargetCrashed` ("fired when debugging target has crashed") and
`target.EventTargetCrashed{TargetID, Status, ErrorCode}` ("issued when a target has crashed").
A tab crash ("Aw, Snap") kills one target, not the browser — the instance is still perfectly
healthy. This matters for the inverse error: **not every failed render means a dead instance.**
Restarting an instance on a tab crash would be a self-inflicted capacity loss.

Sources:
- https://github.com/chromedp/chromedp/blob/master/browser.go
- https://github.com/chromedp/chromedp/blob/master/allocate.go
- https://github.com/chromedp/chromedp/blob/master/chromedp.go
- https://github.com/chromedp/chromedp/blob/master/errors.go
- https://github.com/chromedp/chromedp#readme
- https://github.com/chromedp/cdproto/blob/master/inspector/events.go
- https://github.com/chromedp/cdproto/blob/master/target/events.go
- https://github.com/chromedp/chromedp/issues/653, /801, /1290

---

## Q3 — Established Go patterns for replacing a pool member in place

### The canonical stdlib precedent: `database/sql`

`database/sql` is the closest thing Go has to a normative answer for "a pooled resource died;
now what", and it is worth mirroring because reviewers will already know it.

`database/sql/driver/driver.go:152-164`:

```go
// ErrBadConn should be returned by a driver to signal to the database/sql
// package that a driver.Conn is in a bad state (such as the server
// having earlier closed the connection) and the database/sql package should
// retry on a new connection.
//
// To prevent duplicate operations, ErrBadConn should NOT be returned
// if there's a possibility that the database server might have
// performed the operation. ...
var ErrBadConn = errors.New("driver: bad connection")
```

`database/sql/sql.go:1574-1589`:

```go
// maxBadConnRetries is the number of maximum retries if the driver returns
// driver.ErrBadConn to signal a broken connection before forcing a new
// connection to be opened.
const maxBadConnRetries = 2

func (db *DB) retry(fn func(strategy connReuseStrategy) error) error {
	for i := int64(0); i < maxBadConnRetries; i++ {
		err := fn(cachedOrNewConn)
		if err == nil || !errors.Is(err, driver.ErrBadConn) { return err }
	}
	return fn(alwaysNewConn)
}
```

And `driver.Validator` (`driver.go:306-315`):

```go
// Validator may be implemented by Conn to allow drivers to
// signal if a connection is valid or if it should be discarded.
type Validator interface {
	// IsValid is called prior to placing the connection into the
	// connection pool. The connection will be discarded if false is returned.
	IsValid() bool
}
```

Three transferable rules:

1. **A typed sentinel for "this pool member was bad", distinct from "the work failed".** This
   codebase currently has neither, and (per Q2) cannot derive one from chromedp's error value.
2. **A small, bounded number of retries on a different member** (2), then force a fresh one.
3. **`IsValid()` is checked on checkin, not just checkout** — the pool refuses to *return* a
   known-bad member to circulation. Cheaper than probing on every acquire.

The `ErrBadConn` idempotency warning transfers with a twist: HTML→PDF rendering is locally
side-effect-free, so a retry is safe *if* the failure happened before any external fetch. Once
`Navigate`/`SetDocumentContent` has run, customer HTML may already have issued requests with
side effects. Retry only on pre-render failures (couldn't create a target / instance already
known dead).

### The reference implementation of "drain then swap": puppeteer-cluster

`puppeteer-cluster` is the JS analogue and its `SingleBrowserImplementation.repair()` is a
compact, complete statement of the concurrency hazards:

```ts
private repairing: boolean = false;
private repairRequested: boolean = false;
private openInstances: number = 0;
private waitingForRepairResolvers: (() => void)[] = [];

private async repair() {
    if (this.openInstances !== 0 || this.repairing) {
        // already repairing or there are still pages open? wait for start/finish
        await new Promise<void>(resolve => this.waitingForRepairResolvers.push(resolve));
        return;
    }
    this.repairing = true;
    try { await timeoutExecute(BROWSER_TIMEOUT, this.browser.close()); }
    catch (e) { debug('Unable to close browser.'); }
    try { this.browser = await this.puppeteer.launch(this.options); }
    catch (err) { throw new Error('Unable to restart chrome.'); }
    this.repairRequested = false;
    this.repairing = false;
    this.waitingForRepairResolvers.forEach(resolve => resolve());
    this.waitingForRepairResolvers = [];
}
```

Note: `repairRequested` (a request pending) is separate from `repairing` (in progress); repair is
deferred until `openInstances === 0`; the old browser's `close()` is *expected to fail* and is
best-effort with a timeout; waiters are parked and released. `Worker.ts` calls `repair()` both
when it cannot obtain a page and when it cannot close one, with `BROWSER_INSTANCE_TRIES` bounding
the attempts. README: "Auto restarts the browser in case of a crash".

Its restart-storm control is `workerCreationDelay` — "Time between creation of two workers. …
You can use this to prevent a network peak right at the start."

### Restart storms: use a real backoff spec

The gRPC connection-backoff protocol is the best-cited primary source with concrete numbers:

> MIN_CONNECT_TIMEOUT = 20 seconds; INITIAL_BACKOFF = 1 second; MULTIPLIER = 1.6;
> MAX_BACKOFF = 120 seconds; JITTER = 0.2
>
> "Alternate implementations must ensure that connection backoffs started at the same time
> disperse, and must not attempt connections substantially more often than the above algorithm."
>
> "The back off should be reset to INITIAL_BACKOFF at some time point, so that the reconnecting
> behavior is consistent…"

— https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md

The "must disperse" clause is exactly the pool restart-storm hazard: if the node OOMs, all N
instances die at once and a naive supervisor relaunches all N simultaneously, re-OOMing the node.
Jitter is not optional.

### Catalogue of candidate mechanisms

| Mechanism | What it buys | Cost / hazard | Verdict here |
|---|---|---|---|
| **Skip dead slot at dispatch** (`acquire` scans past unhealthy slots) | Stops the 1-in-N bleed *immediately*, independent of restart | Needs a bounded scan + a "all slots dead" error; slight dispatch complexity | **Yes — do this first.** Highest value/effort ratio in the whole document |
| **`atomic.Pointer[Renderer]` swap in the slot** | Lock-free read on the hot path; replacement is one store | A render that loaded the old pointer keeps using it — correct, but the old renderer must not be `Close`d until it drains | **Yes**, paired with the semaphore drain |
| **Mutex + generation counter** | Simpler to reason about; generation makes "is this restart still relevant?" trivially checkable and kills double-restart | Lock on the hot path (negligible at ~27 req/s) | **Yes** — generation counter is the cheap fix for the double-restart race |
| **`restartMutex.TryLock()` + `isRestarting atomic.Bool`** (Gotenberg) | Exactly one restart in flight; losers decline instead of queueing | Two pieces of state to keep consistent | **Yes** — proven shape, copy it |
| **Semaphore full-drain before swap** (Gotenberg `doRestartLocked`) | Guarantees zero in-flight renders at swap time; satisfies `Renderer.Close`'s documented precondition | A slow render delays the restart up to its timeout; needs a ctx-bounded backout | **Yes** — the existing `sem chan struct{}` is already the right primitive |
| **`golang.org/x/sync/singleflight`** | Collapses N concurrent "restart slot i" requests into one | Another dependency; `TryLock`+generation already covers it | Optional; not needed |
| **Circuit breaker per instance** (e.g. `sony/gobreaker`) | Fails fast while an instance is bad | Circuit breakers exist for remote dependencies you cannot repair. A local child process you own should be *restarted*, not waited out. Adds a half-open probe state that duplicates the health check | **No** — slot-skip + restart strictly subsumes it |
| **Recreate the whole `Pool`** | Trivially correct | Drops all N instances' capacity for the full cold-start window (300-800 ms × N, plus warmup); turns a 1-instance fault into a total outage | **No** |
| **Health-probe on every acquire** | Catches hangs | Puts a CDP roundtrip in front of every render; Gotenberg explicitly caches to avoid this | **No** — probe on a ticker, cache successes |

### Concurrency hazards, enumerated

1. **In-flight renders on the instance being replaced.** `Renderer.Close` documents that it
   *aborts* rather than waits (`chromium.go:292-301`). Cancelling `browserCtx` kills the process
   via `exec.CommandContext`, so any concurrent `chromedp.Run` on a derived `taskCtx` dies
   mid-flight. Must drain first. The `sem` channel of capacity `MaxConcurrencyPerInstance` is
   the drain barrier: acquire all remaining tokens, then swap.
2. **Double-restart race.** Two failing renders on the same slot both conclude "dead" and both
   restart. Guard with `TryLock` + a per-slot generation: a restarter records the generation it
   observed and aborts if the slot has already advanced.
3. **Restart storms.** Correlated death (node OOM, bad Chromium binary, cgroup limit) kills all N.
   Per-slot exponential backoff with jitter, per the gRPC parameters; reset the backoff only
   after a render actually succeeds on the new instance, not merely after it starts.
4. **Restart-during-shutdown.** Adding a supervisor goroutine breaks `Pool.Close`'s current
   precondition contract (`pool.go:117-123`): a supervisor could observe the deliberate shutdown
   cancel as a crash and relaunch Chromium during teardown. Shutdown must stop supervisors first,
   then close renderers.
5. **Dispatch fairness after replacement.** `p.next.Add(1) % N` assumes all slots are equal. While
   slot *i* is restarting, its share must go somewhere; a naive "skip to *i+1*" doubles load on
   one neighbour. Prefer scanning forward for the first *available* slot (cheap at N ≤ 16).
6. **Starting a replacement is itself slow and can fail.** `NewRenderer` blocks on
   `chromedp.Run(browserCtx)`. Gotenberg bounds this with `--chromium-start-timeout` (20 s
   default) and `runWithDeadline`. A replacement launch must be time-bounded and must not hold
   the drain forever if it fails.
7. **Zombie processes.** Gotenberg's `Stop` explicitly enumerates OS processes and kills leftover
   `chromium/chromium`/`chrome/chrome` (`browser.go:258-285`), and notes Chromium recreating its
   user-profile dir right after deletion. This repo's `Close` does only `browserCancel();
   allocCancel()`. Under repeated *restarts* (as opposed to a single shutdown) leaked processes
   compound. Worth verifying empirically before shipping restart-in-place.

Sources:
- https://github.com/golang/go/blob/master/src/database/sql/sql.go
- https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go
- https://github.com/thomasdondorf/puppeteer-cluster/blob/master/src/concurrency/SingleBrowserImplementation.ts
- https://github.com/thomasdondorf/puppeteer-cluster/blob/master/src/Worker.ts
- https://github.com/thomasdondorf/puppeteer-cluster#readme
- https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md

---

## Q4 — Proactive recycling after N renders

### Is there a real reason?

**Yes, but the evidence is "acknowledged unbounded growth", not a clean published curve.**

Verified primary evidence:

- **Gotenberg ships restart-after-100 on by default.** That is the strongest single data point:
  the leading Go+Chromium PDF service considers a long-lived Chromium untrustworthy past ~100
  conversions and pays a cold start to recycle it, by default, for everyone.
  (`chromium.go:461`.)
- **Puppeteer #9283, "Chrome browser requests leak memory when running with Puppeteer"** — labelled
  `confirmed` by maintainers; reported ~0.5 MB/s growth in the renderer process during repeated
  in-page HTTP requests; **closed as not planned**. i.e. acknowledged as real and *not* fixed
  upstream. https://github.com/puppeteer/puppeteer/issues/9283
- **Gotenberg #987, "Memory leak route POST /forms/chromium/convert/url"** — production report on
  AWS Fargate of slow continuous memory growth with dips only at restarts, correlating with
  `context deadline exceeded`. https://github.com/gotenberg/gotenberg/issues/987
- **Gotenberg #1169** — memory-leak report on Cloud Run: the container hits its memory limit
  roughly twice a day regardless of memory size or concurrency limit. Notably, Gotenberg's
  `Healthy()` implementation cites this issue in-code as the reason for the
  `Browser.getVersion` roundtrip. https://github.com/gotenberg/gotenberg/issues/1169
- **This repo's own load test**: ~349 MB/instance idle → ~750-790 MB/instance under sustained
  heavy-document load (`docs/research/load-test-results.md`). Even without a leak, the working
  set more than doubles and does not visibly return. At 8-16 instances that is 6-13 GB.

> **Uncertainty flagged, explicitly**: I could **not** find any primary source — Gotenberg
> changelog, PR description, issue, or benchmark — explaining *why 100*. It appears to be a
> pragmatic default, not a measured optimum. Treat 100 as "the value the reference implementation
> ships", not as a validated threshold. There is also no published number for this repo's
> workload; the right N here should be derived from a soak test that plots RSS against
> conversions-since-restart, which does not exist yet.

### What N do real projects use?

| Project | Knob | Default | Verified from |
|---|---|---|---|
| Gotenberg | `--chromium-restart-after` (conversions) | **100** | source, `chromium.go:461` |
| Gotenberg | `--chromium-idle-shutdown-timeout` (time) | 0 (off) | source, `chromium.go:463` |
| Browserless | `CHROME_REFRESH_TIME` (time, not count) | reported as 1800000 ms (30 min) | **unverified** — search-result summary only; I could not confirm this from browserless source or docs |
| puppeteer-cluster | none — restarts on *error* only | n/a | source |

Note the axis difference: Gotenberg counts **conversions**; browserless (reportedly) counts
**elapsed time**. Counting conversions is the better fit here, because memory growth tracks work
done, not wall-clock — an idle pool overnight should not churn.

### The cost side

A recycle costs one instance's cold start. `SPEC-render-engines.md` puts that at 300-800 ms, and
the load test shows the pool saturates at ~26-27 req/s on 3-4 instances. Losing 1 of 3 instances
for ~0.5-1 s at 27 req/s during a recycle is on the order of 5-10 queued requests — acceptable
if and only if recycles are (a) asynchronous, (b) never simultaneous across slots, and (c) done
after draining rather than by aborting live renders.

**Un-sourced recommendation of mine**: jitter the per-slot threshold (e.g. `N × (0.8 + 0.4·rand)`
chosen once per slot at start) so instances do not all hit 100 on the same request burst and
recycle in lockstep. Gotenberg does not need this — it has one process per container. This repo
does. Flagging it as reasoning, not a citation.

---

## Q5 — Hung vs. dead

This is the sharpest distinction in the whole problem, and the two states need different signals.

**Dead** = the process exited or the websocket dropped. `conn.Read` errors → `LostConnection`
closes → `browserCtx` is cancelled. **Free to detect, no probe needed.** Every subsequent
`chromedp.Run` fails fast.

**Hung** = the process is alive, the socket is open, but the browser's message loop is not
answering. `conn.Read` simply blocks. `LostConnection` never closes. `browserCtx` stays valid.
`Browser.Process()` reports a live PID (and a zombie would too). Renders fail by *timeout* —
`opts.Timeout` / `measureHeightTimeout` — which is indistinguishable from "the customer sent a
genuinely slow document".

The community answer, as implemented by Gotenberg, is an **active, cheap, browser-level CDP
roundtrip with its own timeout**, plus hysteresis. `pkg/modules/chromium/browser.go:328-360`:

```go
func (b *chromiumBrowser) Healthy(logger *slog.Logger) bool {
	if !b.isStarted.Load() { return false }
	b.ctxMu.RLock(); defer b.ctxMu.RUnlock()

	// Create a timeout based on the existing browser context (b.ctx).
	// IMPORTANT: We do NOT call chromedp.NewContext here.
	// We want to execute this against the main browser connection,
	// avoiding the creation of a new target (tab).
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()

	// Check if the browser is responsive by asking for its version.
	// This involves a simple JSON payload roundtrip over the websocket.
	// See https://github.com/gotenberg/gotenberg/issues/1169.
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, _, err := cdprotobrowser.GetVersion().Do(ctx)
		return err
	}))
	if err != nil { logger.Error(...); return false }
	return true
}
```

Five design points, all of which transfer directly:

1. **`Browser.getVersion`, not a page load.** Smallest possible request/response pair on the
   browser websocket. It proves the message loop is turning.
2. **Run it against the *existing browser context*, never `chromedp.NewContext`.** Creating a tab
   to probe health allocates a target, costs real time, and can itself hang. The in-code comment
   calls this out in capitals. This repo's `Renderer` derives every task from `browserCtx`, so a
   probe would need an `ActionFunc` executed with the `Browser` executor — not the default
   `Target` executor `chromedp.Run` uses.
3. **A hard, short timeout (5 s).** Without it the probe hangs exactly like the render did.
4. **Hysteresis: 2 consecutive failures before declaring dead** (`healthFailureThreshold`), on the
   stated reasoning that under load a single blown CDP timeout is more likely transient pressure
   than a dead process.
5. **Cache successes (2 s), never cache failures**, so probe traffic doesn't pile onto a busy
   websocket but recovery is visible on the very next probe.

What explicitly does **not** work as a hang detector:

- Checking `browserCtx.Err()` — stays nil while hung. (It is the *dead* detector.)
- Signalling the PID (`Process().Signal(syscall.Signal(0))`) — a wedged or zombie process passes.
- Render timeouts alone — cannot separate a hung browser from a slow document. They can *feed*
  a probe ("N timeouts on this slot → probe it now"), but must not directly trigger a restart.

---

## Comparison of end-to-end approaches

| Approach | Detects dead | Detects hung | In-flight safety | Complexity | Notes |
|---|---|---|---|---|---|
| **A. Do nothing (today)** | no | no | n/a | none | 1-in-N failures forever |
| **B. Skip-dead-at-dispatch only** (`browserCtx.Done()` check in `acquire`) | yes, free | no | trivially safe | ~30 LOC | Stops the bleed; capacity permanently reduced until process restart |
| **C. B + reactive replace on death** | yes | no | needs drain | moderate | Restores capacity. Misses hangs entirely |
| **D. C + periodic `getVersion` probe** | yes | yes | needs drain | moderate+ | Gotenberg parity for a pool |
| **E. D + restart-after-N recycling** | yes | yes | needs drain | high | Full Gotenberg parity; bounds memory growth |
| **F. Recreate whole `Pool` on any death** | yes | no | catastrophic | low | Turns 1-instance faults into total outages. Rejected |
| **G. Per-instance circuit breaker** | indirectly | indirectly | n/a | moderate | Wrong tool: never repairs, only avoids. Subsumed by C |

B is strictly a subset of C, C of D, D of E. They are shippable in that order, each independently
useful — which matters, because B alone removes the production-blocking symptom.

---

## RECOMMENDATION

Implement **E**, delivered in the order **B → C → D → E**, as a per-slot supervisor inside
`renderengines`. Rationale: `Pool` already has the two primitives this needs — a per-instance
semaphore of known capacity (the drain barrier) and a `Renderer` whose `browserCtx` is already a
liveness signal. Almost none of this is new machinery; it is wiring up signals the code already
holds.

### Step B — stop the bleed (do this first, independently shippable)

1. Add `func (r *Renderer) Alive() bool { return r.browserCtx.Err() == nil }` and
   `func (r *Renderer) Done() <-chan struct{} { return r.browserCtx.Done() }`. Justify both in a
   doc comment with the chromedp mechanism from Q2 (`LostConnection` → `Allocate`'s goroutine →
   `c.cancel()`), and note the pinned chromedp version, because this is derived behaviour rather
   than a documented contract.
2. Change `Pool.acquire` from "index and commit" to "scan forward from the round-robin index for
   the first slot that is both alive and has semaphore capacity". Keep the atomic counter as the
   *starting* offset so fairness is preserved. Bound the scan at `len(p.instances)`; if every
   slot is dead, return a distinct typed error (see 3) rather than blocking forever.
3. Introduce a package sentinel — `renderengines.ErrInstanceUnavailable` or similar — playing the
   role `driver.ErrBadConn` plays in `database/sql`. Do **not** try to derive instance death from
   `chromedp.Run`'s error; per Q2 it is `context.Canceled` and ambiguous with caller cancellation
   in this exact code path. Derive it from `Alive()`.

### Step C — reactive replacement

4. Restructure `poolSlot` so the renderer is replaceable: `renderer atomic.Pointer[Renderer]`
   (or a mutex-guarded field), plus `generation uint64`, `restartMu sync.Mutex`, and
   `restarting atomic.Bool`. Keep `sem` at its current capacity — it is the drain barrier and
   must not be recreated on restart, or in-flight accounting is lost.
5. One supervisor goroutine per slot, `select`ing on `renderer.Done()` and a stop channel. On
   death: set `restarting` (so `acquire` skips the slot even before the swap), `restartMu.TryLock`
   (bail if another restart is in flight), re-read the generation and abort if it moved, then
   **acquire every remaining token in `sem`** to drain in-flight renders — mirroring Gotenberg's
   `doRestartLocked` — backing the acquisition out if a bounded drain deadline expires.
6. Swap: `old.Close()` best-effort (expect it to fail — the process is already gone; puppeteer-
   cluster wraps this in a timeout and ignores errors), `NewRenderer` under a start deadline
   (Gotenberg uses 20 s), publish the new pointer, bump the generation, release all drained
   tokens, clear `restarting`.
7. Backoff on failed replacement, per the gRPC parameters: 1 s initial, ×1.6, jitter ±20%, cap
   ~120 s (a 120 s cap may be too slow for a 3-instance pool — consider capping at 30 s and say
   so in the code comment). Reset the backoff only after a render *succeeds* on the new instance.
   Jitter is mandatory: correlated death of all N instances is the expected failure mode under
   node OOM.
8. Bounded retry at the `Pool` level: if `RenderHTML` fails and the chosen instance is no longer
   `Alive()`, retry once on a different slot — `maxBadConnRetries = 2` in `database/sql` is the
   precedent. **Only retry when the failure is provably pre-render** (target creation failed /
   instance already dead). Once `SetDocumentContent` has run, customer HTML may have issued
   external requests with side effects; `driver.ErrBadConn`'s own doc comment makes exactly this
   argument ("ErrBadConn should NOT be returned if there's a possibility that the [server] might
   have performed the operation").
9. Fix `Pool.Close`: stop all supervisors and wait for them before closing renderers, or a
   supervisor will read the deliberate shutdown cancel as a crash and relaunch Chromium during
   teardown. Update the doc comment at `pool.go:117-123`, which currently describes a
   precondition that a supervisor invalidates.

### Step D — hang detection

10. Add `func (r *Renderer) Ping(ctx context.Context) error` running `browser.GetVersion()` **on
    the browser executor, against `r.browserCtx`, with no new tab** — `chromedp.Run` uses the
    Target executor by default, so this needs an `ActionFunc` with
    `cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)`. Give it a 5 s timeout.
11. Supervisor ticker (~10 s, staggered per slot). Require **2 consecutive failures** before
    declaring the instance dead, and cache successes ~2 s — both straight from Gotenberg, with
    its stated reasoning about transient CDP latency under load. A hang detected this way enters
    the exact same replacement path as step C, except `old.Close()` is now load-bearing (the
    process is alive and must actually be killed) — which is where the zombie-process concern in
    Q3/hazard 7 becomes real, and where Gotenberg's explicit process sweep may need copying.
12. Optionally feed render timeouts into the probe as a trigger ("this slot timed out twice in a
    row → probe now"), but never let a timeout restart an instance directly. A slow customer
    document must not cost the pool an instance.

### Step E — proactive recycling

13. Add `PoolConfig.RestartAfter int` (0 = disabled), defaulting to **100**, documented as
    "matching Gotenberg's `--chromium-restart-after` default" — and documented equally clearly as
    *not* an empirically-derived optimum, because no primary source justifying 100 exists.
14. Jitter the effective threshold per slot (e.g. ±20%, drawn once at slot start) so instances do
    not recycle in lockstep. This is reasoning, not a citation — Gotenberg has one process per
    container and never faces it.
15. Trigger asynchronously *after* a render completes, exactly as `maybeRestartAfterTask` does:
    `TryLock`, hand the finishing caller's semaphore token to the restart goroutine, restart in
    the background. Never block the completing request on a recycle.
16. Before choosing a final default, run a soak test recording RSS per instance against
    conversions-since-restart. `load-test-results.md` already has the harness shape; this is the
    one number in this document that should come from measurement rather than from Gotenberg.

### Observability (do it in step C, not later)

Expose per slot: restart count, reason (`dead` / `hang` / `max_conversions`), conversions since
restart, active renders, current generation, and time of last successful probe. Gotenberg's
`ProcessSupervisor` interface exports precisely `RestartsCount`, `ActiveTasksCount`,
`ConversionsSinceRestart`, `ReqQueueSize` — a good shape to copy. Without these, a pool that is
silently restarting an instance every 30 seconds looks identical to a healthy one from outside.

### Deliberately out of scope

- **Admission control / bounded queue.** `load-test-results.md:106-113` correctly identifies that
  `Pool.acquire` blocks indefinitely with no queue ceiling. That is Gotenberg's
  `--chromium-max-queue-size`, a *different* mechanism from restart-after, and belongs with
  `job-orchestration`. Crash recovery does not fix it and should not be conflated with it.
- **Circuit breaker per instance.** Rejected above: a local child process should be restarted,
  not merely avoided.
- **Lazy start / idle shutdown.** Gotenberg has both; this repo deliberately starts eagerly
  (`chromium.go:55-57`) so the warm-pool cost is paid at startup. Do not import that complexity.

---

## Summary of what is verified vs. uncertain

**Verified from primary source (read the actual code/docs):**
- Gotenberg's restart-after default of 100, max-concurrency 6, start-timeout 20 s, and the full
  `ProcessSupervisor` algorithm including the semaphore drain and `TryLock` restart guard.
- Gotenberg's `Healthy()` = `Browser.getVersion` on the existing browser context, 5 s timeout,
  2 consecutive failures, 2 s success cache.
- chromedp closes `Browser.LostConnection` on websocket read error, and `ExecAllocator.Allocate`
  cancels the browser context in response — so `browserCtx.Done()` fires when Chromium dies.
- chromedp's README states lost connection surfaces as `context canceled`.
- `chromedp.Run` returning `context.Canceled` is ambiguous in *this* codebase's `RenderHTML`.
- `database/sql`'s `maxBadConnRetries = 2`, `driver.ErrBadConn` semantics, `driver.Validator`.
- puppeteer-cluster's `repair()` drain-and-relaunch with `repairing`/`repairRequested`/
  `openInstances` state.
- gRPC connection-backoff parameters and the "must disperse" requirement.
- Puppeteer #9283 is maintainer-`confirmed` and closed as not planned.

**Not verified / uncertain — do not treat as established:**
- **Why 100.** No source found. It is a shipped default, not a published optimum.
- **Browserless's `CHROME_REFRESH_TIME = 30 min`.** From a search summary only; I did not confirm
  it against browserless source or docs, and it may be stale or wrong.
- **Gotenberg #1561 as the origin of `healthFailureThreshold = 2`.** The constant and its in-code
  rationale are verified; the issue thread I fetched did not visibly contain that discussion.
- **That `browserCtx` cancellation on browser death is a stable chromedp contract.** It is
  current implementation behaviour on `master`, not documented API. Pin chromedp; keep the active
  probe as the contract-stable fallback.
- **Zombie/leaked Chromium processes under repeated in-process restarts.** Gotenberg found this
  serious enough to enumerate and kill stray processes on every `Stop`. Whether this repo's
  `browserCancel(); allocCancel()` is sufficient under *repeated* restarts (as opposed to a single
  clean shutdown) is untested here and should be verified empirically before shipping step C.
- **`gotenberg.dev` documentation** could not be fetched (blocked by this environment's egress
  proxy). All Gotenberg claims above come from repository source instead.

## Sources

- Gotenberg supervisor: https://github.com/gotenberg/gotenberg/blob/main/pkg/gotenberg/supervisor.go
- Gotenberg Chromium module: https://github.com/gotenberg/gotenberg/blob/main/pkg/modules/chromium/chromium.go
- Gotenberg Chromium browser: https://github.com/gotenberg/gotenberg/blob/main/pkg/modules/chromium/browser.go
- Gotenberg issues: [#951](https://github.com/gotenberg/gotenberg/issues/951), [#987](https://github.com/gotenberg/gotenberg/issues/987), [#1169](https://github.com/gotenberg/gotenberg/issues/1169), [#1502](https://github.com/gotenberg/gotenberg/issues/1502), [#1538](https://github.com/gotenberg/gotenberg/issues/1538), [#1561](https://github.com/gotenberg/gotenberg/issues/1561), [#1599](https://github.com/gotenberg/gotenberg/issues/1599)
- chromedp: https://github.com/chromedp/chromedp (README FAQ, `browser.go`, `allocate.go`, `chromedp.go`, `errors.go`)
- chromedp issues: [#653](https://github.com/chromedp/chromedp/issues/653), [#801](https://github.com/chromedp/chromedp/issues/801), [#1290](https://github.com/chromedp/chromedp/issues/1290)
- cdproto crash events: https://github.com/chromedp/cdproto/blob/master/inspector/events.go , https://github.com/chromedp/cdproto/blob/master/target/events.go
- CDP reference: https://chromedevtools.github.io/devtools-protocol/tot/Inspector#event-targetCrashed , https://chromedevtools.github.io/devtools-protocol/tot/Target#event-targetCrashed
- Go `database/sql`: https://github.com/golang/go/blob/master/src/database/sql/sql.go , https://github.com/golang/go/blob/master/src/database/sql/driver/driver.go
- puppeteer-cluster: https://github.com/thomasdondorf/puppeteer-cluster (README, `src/Worker.ts`, `src/concurrency/SingleBrowserImplementation.ts`)
- Puppeteer memory-leak issue: https://github.com/puppeteer/puppeteer/issues/9283
- gRPC connection backoff protocol: https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md
- In-repo: `docs/planning/SPEC-render-engines.md`, `docs/research/load-test-results.md`, `docs/research/go-pdf-generation-research.md`
