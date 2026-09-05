package renderengines

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// defaultRestartCooldown is the minimum interval between restart attempts for
// a single pool instance. It is load-bearing, not hygiene: a failed
// NewRenderer costs only ~1ms (measured directly against this environment's
// Chromium), so if Chromium becomes genuinely unstartable — a bad path, an
// OOM-constrained host, a full disk — an uncooled restart-on-demand would let
// every incoming request fork another attempt, thousands per second. The
// cooldown converts that fork storm into a cheap, bounded fast-fail.
const defaultRestartCooldown = 5 * time.Second

// Health-probe defaults. A hung browser — as opposed to a dead one — keeps
// its browser context valid, so the passive alive() check cannot see it;
// only an active CDP round trip can. Verified by freezing a live instance
// with SIGSTOP: alive() reported true throughout while the probe correctly
// timed out and a real render hung until its own deadline.
const (
	// defaultHealthProbeInterval is 0: probe on every dispatch, caching
	// nothing.
	//
	// The obvious design caches a successful probe for a couple of seconds
	// to keep round trips off a busy websocket, which is what Gotenberg
	// does — but Gotenberg probes on a background schedule, where the
	// trade-off is different. Measured here instead of assumed, at 150
	// renders per configuration, twice each:
	//
	//	sequential:  cached 2s → 45.00 / 46.13 ms per render (4 probes)
	//	             every dispatch → 45.36 / 46.30 ms (153 probes)
	//	concurrent:  cached 2s → 49.4 / 51.7 renders per second
	//	             every dispatch → 49.6 / 51.8 renders per second
	//
	// The gap is smaller than the run-to-run variance of a single
	// configuration, and under concurrency probing every time was
	// marginally *faster*. So the cache buys nothing measurable — while
	// costing a real blind window: a browser that wedges just after a
	// successful probe stays trusted for the whole interval, and requests
	// arriving in it wait out a full 30s render timeout. Observed live at
	// exactly that: 30s for one request with a 2s cache.
	//
	// A non-zero interval remains configurable for anyone whose workload
	// measures differently; the default now refuses to trade a 30-second
	// worst case for an unmeasurable gain.
	defaultHealthProbeInterval = 0

	// defaultHealthProbeTimeout bounds one probe.
	defaultHealthProbeTimeout = 5 * time.Second

	// defaultHealthProbeFailureThreshold is the hysteresis: consecutive
	// failures required before declaring an instance hung.
	defaultHealthProbeFailureThreshold = 2
)

// ErrInstanceUnavailable is returned when every instance in the pool is dead
// and still inside its restart cooldown, so there is nothing healthy to
// render on and retrying immediately would only add load.
var ErrInstanceUnavailable = errors.New("renderengines: no healthy chromium instance available")

// errInstanceSuspect tells acquire to move on to the next instance without
// condemning this one. It deliberately wraps ErrInstanceUnavailable: if
// every instance is skipped the caller still sees an availability failure,
// which the HTTP layer maps to 503 + Retry-After rather than a 422 that
// would wrongly blame the caller's document.
var errInstanceSuspect = fmt.Errorf("instance failed a health probe: %w", ErrInstanceUnavailable)

// PoolConfig configures a Pool.
type PoolConfig struct {
	Config // ChromiumPath etc., shared by every instance in the pool

	// Size is the number of separate warm Chromium processes to keep
	// running. Defaults to 1 if unset. Real parallelism across processes,
	// not just tabs — so one slow/CPU-heavy render doesn't bottleneck every
	// other instance, and one crashed browser doesn't take down all
	// in-flight renders.
	Size int

	// MaxConcurrencyPerInstance bounds how many concurrent renders are
	// dispatched to a single Chromium instance's tabs at once. Defaults to
	// 6, matching Gotenberg's own default (docs/research/
	// go-pdf-generation-research.md Part 5.4) — a previously-validated
	// starting point, not a made-up number. This is the concrete
	// implementation of the v1 scale target in PLATFORM-SPEC.md ("~8-12
	// Chromium instances, each ~6 concurrent conversions ≈ 50-70 total").
	MaxConcurrencyPerInstance int

	// RestartCooldown is the minimum interval between restart attempts for
	// one instance. Defaults to defaultRestartCooldown.
	RestartCooldown time.Duration

	// HealthProbeInterval is how long a successful health probe is trusted
	// before the instance is probed again. Defaults to 0 — probe every
	// dispatch — because caching measured no faster while leaving a window
	// in which a wedged browser is still trusted (see
	// defaultHealthProbeInterval). Set a non-zero value to trade detection
	// latency for fewer round trips. Failures are never cached either way.
	HealthProbeInterval time.Duration

	// HealthProbeTimeout bounds a single probe. Defaults to
	// defaultHealthProbeTimeout. Without a hard bound the probe would hang
	// exactly like the render it is meant to diagnose.
	HealthProbeTimeout time.Duration

	// HealthProbeFailureThreshold is how many consecutive probe failures
	// must occur before an instance is treated as hung and rebuilt.
	// Defaults to defaultHealthProbeFailureThreshold. More than one is
	// deliberate hysteresis: under heavy CPU pressure a single blown probe
	// is more likely transient than a wedged browser, and needlessly
	// rebuilding instances during a load spike is how a busy service turns
	// into a restarting one.
	HealthProbeFailureThreshold int
}

// PoolStats is a point-in-time snapshot of pool health, used both by the
// health endpoints (so an operator sees a dead instance instead of a
// permanently-200 /healthz) and by the recovery tests.
type PoolStats struct {
	// Size is the configured number of instances.
	Size int
	// Alive is how many instances currently have a live browser.
	Alive int
	// Restarts counts instances successfully rebuilt after being found dead.
	Restarts uint64
	// RestartAttempts counts rebuild attempts, including failed ones. A
	// RestartAttempts value climbing while Restarts stays flat means Chromium
	// cannot be started at all — the single most useful signal for diagnosing
	// a host-level problem (bad path, OOM, full disk).
	RestartAttempts uint64
	// HangsDetected counts instances rebuilt because they stopped answering
	// CDP while still appearing alive. Distinguished from a plain crash
	// because the two have different causes: crashes point at memory or
	// browser bugs, hangs at CPU starvation or a wedged message loop.
	HangsDetected uint64
	// ProbesPerformed counts active health probes issued, so the cost of
	// probing is observable rather than assumed.
	ProbesPerformed uint64
}

// Pool manages a fixed set of warm Chromium instances, all started
// immediately at construction (paying every cold-start cost once, at
// startup, never per request — see chromium.go), and spreads render calls
// across them round-robin. Each instance internally bounds its own
// concurrent tab count to MaxConcurrencyPerInstance; a caller whose chosen
// instance is already at capacity blocks until a slot frees up or its
// context is done, rather than silently queuing forever.
//
// Instances self-heal. If a browser dies (OOM kill, browser crash, dropped
// CDP websocket), chromedp cancels that instance's browser context, which
// the pool detects on the next dispatch and repairs by building a
// replacement in place. Before this existed, a dead instance kept receiving
// its round-robin share of traffic forever and failing every one of those
// requests, with no self-repair — the gap recorded in
// docs/planning/SPEC-render-engines.md.
type Pool struct {
	instances       []*poolSlot
	next            atomic.Uint64
	restartCooldown time.Duration

	probeInterval   time.Duration
	probeTimeout    time.Duration
	probeFailureMax int
	restarts        atomic.Uint64
	restartAttempts atomic.Uint64
	hangsDetected   atomic.Uint64
	probesPerformed atomic.Uint64
}

type poolSlot struct {
	// renderer is swapped atomically so the hot path (every render) reads it
	// without taking a lock; only the rare restart path locks.
	renderer atomic.Pointer[Renderer]
	sem      chan struct{}
	cfg      Config

	// restartMu serializes rebuilds of this slot so that concurrent requests
	// finding the same dead instance produce exactly one replacement browser,
	// not one per request.
	restartMu sync.Mutex

	// probeMu guards the health-probe bookkeeping below. It is separate from
	// restartMu so a probe never blocks behind an in-progress rebuild.
	probeMu       sync.Mutex
	lastProbeOK   time.Time // guarded by probeMu
	probeFailures int       // consecutive failures; guarded by probeMu

	// lastFailure is when this slot's last rebuild *attempt failed*, and is
	// the anchor for the restart cooldown. It is deliberately not
	// "lastAttempt": the cooldown exists to back off from a browser that
	// cannot be started, not to rate-limit recovery itself. Keying it on any
	// attempt (including successful ones) refuses a legitimate second
	// recovery when an instance crashes twice in quick succession — e.g. a
	// document that reliably OOM-kills Chromium — converting a recoverable
	// blip into a self-inflicted outage window. A successful rebuild clears
	// this, so the next genuine crash is repaired immediately.
	lastFailure time.Time // guarded by restartMu
}

// NewPool starts cfg.Size Chromium instances immediately. If any instance
// fails to start, every instance already started is closed before returning
// the error — no partial, leaking pool.
func NewPool(cfg PoolConfig) (*Pool, error) {
	size := cfg.Size
	if size < 1 {
		size = 1
	}
	maxConcurrency := cfg.MaxConcurrencyPerInstance
	if maxConcurrency < 1 {
		maxConcurrency = 6
	}
	cooldown := cfg.RestartCooldown
	if cooldown <= 0 {
		cooldown = defaultRestartCooldown
	}
	probeInterval := cfg.HealthProbeInterval
	if probeInterval < 0 {
		probeInterval = defaultHealthProbeInterval
	}
	probeTimeout := cfg.HealthProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = defaultHealthProbeTimeout
	}
	probeFailureMax := cfg.HealthProbeFailureThreshold
	if probeFailureMax < 1 {
		probeFailureMax = defaultHealthProbeFailureThreshold
	}

	slots := make([]*poolSlot, 0, size)
	for i := 0; i < size; i++ {
		r, err := NewRenderer(cfg.Config)
		if err != nil {
			for _, s := range slots {
				if old := s.renderer.Load(); old != nil {
					old.Close()
				}
			}
			return nil, fmt.Errorf("renderengines: pool: starting instance %d/%d: %w", i+1, size, err)
		}
		slot := &poolSlot{sem: make(chan struct{}, maxConcurrency), cfg: cfg.Config}
		slot.renderer.Store(r)
		slots = append(slots, slot)
	}
	return &Pool{
		instances:       slots,
		restartCooldown: cooldown,
		probeInterval:   probeInterval,
		probeTimeout:    probeTimeout,
		probeFailureMax: probeFailureMax,
	}, nil
}

// Stats returns a point-in-time snapshot of pool health.
func (p *Pool) Stats() PoolStats {
	alive := 0
	for _, s := range p.instances {
		if r := s.renderer.Load(); r != nil && r.alive() {
			alive++
		}
	}
	return PoolStats{
		Size:            len(p.instances),
		Alive:           alive,
		Restarts:        p.restarts.Load(),
		RestartAttempts: p.restartAttempts.Load(),
		HangsDetected:   p.hangsDetected.Load(),
		ProbesPerformed: p.probesPerformed.Load(),
	}
}

// RenderHTML dispatches to one instance in the pool, round-robin, blocking
// until that instance has spare tab capacity or ctx is done.
func (p *Pool) RenderHTML(ctx context.Context, html string, opts RenderOptions) ([]byte, error) {
	slot, r, release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	pdf, err := r.RenderHTML(ctx, html, opts)
	err = classifyRenderErr(r, err)
	slot.noteRenderOutcome(err)
	return pdf, err
}

// RenderMarkdown dispatches to one instance in the pool, same as RenderHTML.
func (p *Pool) RenderMarkdown(ctx context.Context, markdown string, opts RenderOptions) ([]byte, error) {
	slot, r, release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	pdf, err := r.RenderMarkdown(ctx, markdown, opts)
	err = classifyRenderErr(r, err)
	slot.noteRenderOutcome(err)
	return pdf, err
}

// MeasureHTMLHeight dispatches to one instance in the pool, same as
// RenderHTML — see Renderer.MeasureHTMLHeight.
func (p *Pool) MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error) {
	slot, r, release, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	height, err := r.MeasureHTMLHeight(ctx, htmlFragment, viewportWidthPx)
	err = classifyRenderErr(r, err)
	slot.noteRenderOutcome(err)
	return height, err
}

// classifyRenderErr re-labels a render failure as ErrInstanceUnavailable
// when the instance died during the render itself. Without this, a request
// already executing on a Chromium instance at the moment it crashes (OOM
// kill, browser bug, dropped CDP websocket) surfaces whatever raw error
// chromedp.Run happens to return — never ErrInstanceUnavailable, since that
// classification previously only ever happened at the *next* acquire() call
// — which the HTTP layer then reports as a generic render failure (422),
// wrongly telling the caller their perfectly valid document is broken and
// that retrying is pointless, when in fact the browser died out from under
// them and retrying against the pool's self-healed replacement is exactly
// the right thing to do.
//
// Classification is by browserCtx.Err() (via alive()), never by inspecting
// the error itself, for the same reason acquire's dead-detection already
// works this way: chromedp.Run returns a plain context.Canceled on browser
// death, which by itself is indistinguishable from the caller's own request
// context being cancelled. Checking alive() after the call correctly
// reclassifies the former and leaves the latter untouched, since a caller
// cancelling their own request never touches the instance's browserCtx.
//
// Found by a chaos test that kills every Chromium process while real,
// concurrent HTTP traffic is already flowing — every prior recovery test
// killed Chromium against an otherwise-idle pool and so never had a render
// in flight at the moment of death.
func classifyRenderErr(r *Renderer, err error) error {
	if err == nil || r.alive() {
		return err
	}
	return fmt.Errorf("renderengines: instance died during render: %w", ErrInstanceUnavailable)
}

// noteRenderOutcome feeds a render's result back into health tracking. A
// failed render invalidates this slot's cached "recently proven healthy"
// verdict, so the next dispatch probes instead of trusting a stale success.
//
// This deliberately does NOT restart on a failed render. A render can fail
// for reasons that say nothing about the browser — a malformed document, a
// caller cancelling, or genuinely slow content hitting its deadline — and
// rebuilding a healthy instance because a customer sent a heavy document
// would be a self-inflicted outage. Invalidating the cache only removes a
// stale assumption; the probe still decides.
//
// This matters because the success cache is otherwise a blind window: a
// browser that wedges just after a successful probe is trusted for up to
// HealthProbeInterval, and every request arriving in that window waits out
// its full render timeout. Observed live — a browser frozen one second after
// a healthy probe cost the next request 35s before the cache expired.
func (s *poolSlot) noteRenderOutcome(err error) {
	if err == nil {
		return
	}
	s.probeMu.Lock()
	s.lastProbeOK = time.Time{}
	s.probeMu.Unlock()
}

// acquire picks an instance round-robin, reserves one of its concurrency
// slots, and returns a live Renderer for it. If the chosen instance is dead
// it is rebuilt in place; if it cannot be rebuilt (still inside its restart
// cooldown, or Chromium genuinely won't start) the next instance is tried,
// bounded by the pool size so a fully broken pool fails fast instead of
// looping.
func (p *Pool) acquire(ctx context.Context) (*poolSlot, *Renderer, func(), error) {
	var lastErr error
	for attempt := 0; attempt < len(p.instances); attempt++ {
		idx := p.next.Add(1) % uint64(len(p.instances))
		slot := p.instances[idx]

		select {
		case slot.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, nil, nil, fmt.Errorf("renderengines: pool: %w", ctx.Err())
		}

		r, err := p.healthyRenderer(slot)
		if err != nil {
			<-slot.sem // this instance is unusable; free its slot and try another
			lastErr = err
			continue
		}
		return slot, r, func() { <-slot.sem }, nil
	}
	if lastErr == nil {
		lastErr = ErrInstanceUnavailable
	}
	return nil, nil, nil, fmt.Errorf("renderengines: pool: %w", lastErr)
}

// healthyRenderer returns a usable Renderer for the slot, rebuilding it if
// the browser is dead or hung.
//
// Two different faults with two different detectors:
//
//   - Dead (process gone, websocket dropped): chromedp cancels the browser
//     context, so alive() sees it for free.
//   - Hung (process alive, socket open, message loop stopped): the browser
//     context stays valid, so alive() reports the instance as perfectly
//     healthy. Only an active CDP round trip catches this.
func (p *Pool) healthyRenderer(slot *poolSlot) (*Renderer, error) {
	r := slot.renderer.Load()
	if r == nil || !r.alive() {
		return p.restart(slot, r)
	}
	switch p.probeInstance(slot, r) {
	case probeHung:
		p.hangsDetected.Add(1)
		return p.restart(slot, r)
	case probeSuspect:
		// Failed a probe, but not enough times to be condemned. Skip this
		// instance for this request rather than rebuilding it — and, just as
		// importantly, rather than dispatching onto it anyway.
		//
		// Dispatching anyway is what the first version did, and it made
		// hysteresis cost 30 seconds: measured live, a request against a
		// frozen browser spent 5s failing the probe and then a further 30s
		// waiting out the render timeout. Skipping keeps the caution (a
		// single transient probe failure still never rebuilds a browser)
		// without making the caller pay for it.
		return nil, errInstanceSuspect
	}
	return r, nil
}

// probeVerdict is the outcome of a health probe.
type probeVerdict int

const (
	// probeHealthy: responsive, or recently proven so.
	probeHealthy probeVerdict = iota
	// probeSuspect: failed a probe, but not yet enough times to condemn.
	// Skip the instance for this request; do not rebuild it.
	probeSuspect
	// probeHung: failed enough consecutive probes to be treated as wedged.
	probeHung
)

// probeInstance probes the instance and classifies it.
//
// Successful probes are cached for probeInterval so a busy pool does not add
// a round trip to every single render; failures are never cached, so an
// instance that recovers on its own is trusted again immediately.
func (p *Pool) probeInstance(slot *poolSlot, r *Renderer) probeVerdict {
	slot.probeMu.Lock()
	defer slot.probeMu.Unlock()

	if time.Since(slot.lastProbeOK) < p.probeInterval {
		return probeHealthy // recently proven responsive
	}

	p.probesPerformed.Add(1)
	if err := r.probe(r.browserCtx, p.probeTimeout); err != nil {
		slot.probeFailures++
		if slot.probeFailures >= p.probeFailureMax {
			slot.probeFailures = 0
			return probeHung
		}
		return probeSuspect
	}

	slot.probeFailures = 0
	slot.lastProbeOK = time.Now()
	return probeHealthy
}

// restart rebuilds a faulty instance in place. bad is the Renderer the
// caller found unusable (nil if the slot was empty). Only one goroutine
// rebuilds a given slot at a time; the rest re-check under the lock and
// reuse whatever the winner produced.
func (p *Pool) restart(slot *poolSlot, bad *Renderer) (*Renderer, error) {
	slot.restartMu.Lock()
	defer slot.restartMu.Unlock()

	// Another goroutine may have rebuilt this slot while we waited for the
	// lock. Identity is the test, not liveness: a *hung* browser still
	// passes alive(), so re-checking liveness alone would hand the caller
	// straight back the wedged instance it just diagnosed and skip the
	// rebuild entirely. Comparing against the specific Renderer that was
	// found faulty distinguishes "someone already replaced it" from "it is
	// still the broken one".
	if cur := slot.renderer.Load(); cur != nil && cur != bad && cur.alive() {
		return cur, nil
	}
	if !slot.lastFailure.IsZero() && time.Since(slot.lastFailure) < p.restartCooldown {
		return nil, ErrInstanceUnavailable
	}
	p.restartAttempts.Add(1)

	fresh, err := NewRenderer(slot.cfg)
	if err != nil {
		slot.lastFailure = time.Now() // start the backoff window
		return nil, fmt.Errorf("restarting instance: %w", err)
	}
	slot.lastFailure = time.Time{} // recovered: the next genuine crash repairs immediately
	// Swap first, then close the corpse: any in-flight render still holding
	// the old pointer is already doomed (a dead instance fails immediately,
	// measured at ~0ms, rather than hanging), and closing an already-dead
	// Renderer is a verified no-op.
	if old := slot.renderer.Swap(fresh); old != nil {
		old.Close()
	}
	p.restarts.Add(1)
	return fresh, nil
}

// Close terminates every Chromium instance in the pool. No further render
// calls should be made on this Pool afterward.
//
// Precondition: same as Renderer.Close — callers must ensure no render is
// still in flight, since this aborts rather than waits. See Renderer.Close's
// doc comment for how cmd/api's shutdown ordering currently guarantees this.
func (p *Pool) Close() error {
	var firstErr error
	for _, s := range p.instances {
		if r := s.renderer.Load(); r != nil {
			if err := r.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
