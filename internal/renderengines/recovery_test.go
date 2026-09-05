package renderengines

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// killAllChromium hard-kills every running Chromium process, simulating an
// OOM kill or browser crash. Tests using this must own every running
// instance (i.e. not run in parallel with other Chromium tests), which is
// why they're deliberately not t.Parallel().
//
// Matching is done on each process's real executable (/proc/<pid>/exe), not
// on its command line. A command-line match is actively dangerous here: the
// shell running these tests has CHROMIUM_PATH on its own command line, so a
// `pgrep -f headless_shell` sweep matches — and SIGKILLs — the test runner
// itself. That is not hypothetical; it silently killed this very test run
// (empty output, exit 1) before the check was tightened.
func killAllChromium(t *testing.T) int {
	t.Helper()
	chromiumPath, err := filepath.EvalSymlinks(os.Getenv("CHROMIUM_PATH"))
	if err != nil {
		t.Fatalf("resolving CHROMIUM_PATH: %v", err)
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("reading /proc: %v", err)
	}
	killed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || exe != chromiumPath {
			continue // not a Chromium process (or not ours to inspect)
		}
		if exec.Command("kill", "-9", strconv.Itoa(pid)).Run() == nil {
			killed++
		}
	}
	return killed
}

// TestPool_RecoversFromKilledInstance is the core self-healing proof: after
// the underlying Chromium process is hard-killed (simulating an OOM kill or
// a browser crash), the very next render must succeed because the pool
// detected the dead instance and rebuilt it — rather than failing forever,
// which is what this pool did before recovery existed (the gap recorded in
// docs/planning/SPEC-render-engines.md).
func TestPool_RecoversFromKilledInstance(t *testing.T) {
	p := testPool(t, 1)
	ctx := context.Background()

	if _, err := p.RenderHTML(ctx, "<html><body>before</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("baseline render failed: %v", err)
	}
	if got := p.Stats().Restarts; got != 0 {
		t.Fatalf("Restarts = %d before any kill, want 0", got)
	}

	if killed := killAllChromium(t); killed == 0 {
		t.Fatal("no chromium processes were killed; test cannot prove recovery")
	}
	// Give chromedp a moment to observe the death (browserCtx cancellation).
	time.Sleep(500 * time.Millisecond)

	pdf, err := p.RenderHTML(ctx, "<html><body>after</body></html>", DefaultRenderOptions())
	if err != nil {
		t.Fatalf("render after kill failed — the pool did not self-heal: %v", err)
	}
	if len(pdf) < 5 || string(pdf[:5]) != "%PDF-" {
		t.Fatalf("recovered render did not produce a valid PDF (%d bytes)", len(pdf))
	}
	if got := p.Stats().Restarts; got != 1 {
		t.Fatalf("Restarts = %d after one kill, want exactly 1", got)
	}
	if st := p.Stats(); st.Alive != st.Size {
		t.Fatalf("after recovery Alive=%d, want Size=%d", st.Alive, st.Size)
	}
}

// TestPool_InFlightRenderReclassifiesAsInstanceUnavailableWhenKilledMidRender
// closes a gap TestPool_RecoversFromKilledInstance and every other recovery
// test in this file cannot see: they all kill Chromium against an otherwise-
// idle pool and then issue a *new* render, proving the pool repairs itself
// for the *next* caller. None of them have a render actually in flight at the
// moment of death. Found by a chaos test in internal/api that runs continuous
// concurrent HTTP traffic and kills Chromium partway through: a request
// already executing when the browser dies got whatever raw error
// chromedp.Run happened to return — never ErrInstanceUnavailable — which the
// HTTP layer reported as a generic 422 render failure, wrongly telling the
// caller their perfectly valid document was broken. Pool.RenderHTML now
// reclassifies via Renderer.alive() (browserCtx.Err()) after the call
// returns, which this test proves directly.
func TestPool_InFlightRenderReclassifiesAsInstanceUnavailableWhenKilledMidRender(t *testing.T) {
	p := testPool(t, 1)
	ctx := context.Background()

	// Same technique as TestRenderHTML_TimeoutDuringActiveRenderAbortsCleanly
	// (chromium_test.go): override document.fonts.ready with a Promise that
	// never resolves, so wait-for-fonts genuinely hangs rather than failing
	// instantly — this render is truly in flight when Chromium is killed,
	// not already finished or not yet started.
	html := `<html><head><script>
		Object.defineProperty(document, 'fonts', { value: { ready: new Promise(() => {}) }, configurable: true });
	</script></head><body>hang test</body></html>`
	opts := DefaultRenderOptions()
	opts.Timeout = 8 * time.Second
	opts.WaitForFonts = true

	renderDone := make(chan error, 1)
	go func() {
		_, err := p.RenderHTML(ctx, html, opts)
		renderDone <- err
	}()

	time.Sleep(300 * time.Millisecond) // let the render actually start hanging on wait-for-fonts
	if killed := killAllChromium(t); killed == 0 {
		t.Fatal("no chromium processes were killed; test cannot prove reclassification")
	}

	select {
	case err := <-renderDone:
		if err == nil {
			t.Fatal("expected an error after killing chromium mid-render, got nil")
		}
		if !errors.Is(err, ErrInstanceUnavailable) {
			t.Fatalf("err = %v, want wrapped ErrInstanceUnavailable — the browser died mid-render, "+
				"this must not surface as a generic render failure that blames the caller's document", err)
		}
	case <-time.After(3 * time.Second): // well under opts.Timeout=8s
		t.Fatal("render did not return promptly after chromium was killed mid-render; " +
			"classification must not need to wait out the full opts.Timeout")
	}
}

// TestPool_ConcurrentRequestsRestartInstanceOnlyOnce proves the restart is
// guarded: when many requests hit a dead instance simultaneously, exactly one
// rebuild happens, not one per request (which would fork a storm of browsers).
func TestPool_ConcurrentRequestsRestartInstanceOnlyOnce(t *testing.T) {
	p := testPool(t, 1)
	ctx := context.Background()

	if _, err := p.RenderHTML(ctx, "<html><body>warm</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("baseline render failed: %v", err)
	}
	if killAllChromium(t) == 0 {
		t.Fatal("no chromium processes were killed")
	}
	time.Sleep(500 * time.Millisecond)

	const concurrent = 8
	var wg sync.WaitGroup
	errs := make([]error, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.RenderHTML(ctx, "<html><body>concurrent</body></html>", DefaultRenderOptions())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent render %d failed: %v", i, err)
		}
	}
	if got := p.Stats().Restarts; got != 1 {
		t.Fatalf("Restarts = %d, want exactly 1 — concurrent requests must not each rebuild the instance", got)
	}
}

// TestPool_RestartCooldownPreventsStorm proves the cooldown is real. A failed
// NewRenderer costs only ~1ms (measured), so without a cooldown a permanently
// broken Chromium (bad path, OOM, full disk) would let every request fork a
// new attempt — thousands per second. Within the cooldown window the pool
// must fail fast instead of retrying.
func TestPool_RestartCooldownPreventsStorm(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	// A pool whose binary does not exist can never start, so NewPool itself
	// fails — construct a pool that starts healthy, then point its restart
	// config at a broken binary to simulate "Chromium became unstartable".
	p := testPool(t, 1)
	p.instances[0].cfg = Config{ChromiumPath: "/nonexistent/chromium-binary"}
	p.restartCooldown = time.Hour // effectively: only one attempt allowed

	if killAllChromium(t) == 0 {
		t.Fatal("no chromium processes were killed")
	}
	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()
	const attempts = 5
	for i := 0; i < attempts; i++ {
		if _, err := p.RenderHTML(ctx, "<html><body>x</body></html>", DefaultRenderOptions()); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded against a broken binary", i)
		}
	}

	if got := p.Stats().Restarts; got != 0 {
		t.Fatalf("Restarts = %d, want 0 (every attempt failed, none succeeded)", got)
	}
	if got := p.Stats().RestartAttempts; got != 1 {
		t.Fatalf("RestartAttempts = %d after %d requests, want exactly 1 — "+
			"the cooldown must stop every request from retrying", got, attempts)
	}
}

// TestPool_RepeatedRestartsDoNotLeakProcesses answers a specific risk raised
// by docs/research/crash-recovery-research.md: Gotenberg considered stray
// Chromium processes serious enough to explicitly enumerate and kill them on
// every stop, whereas this repo relies on cancelling the chromedp contexts.
// That was verified for a single clean shutdown but NOT under repeated
// in-process restarts — exactly what crash recovery now does. This closes
// that gap with a measurement instead of an assumption.
func TestPool_RepeatedRestartsDoNotLeakProcesses(t *testing.T) {
	p := testPool(t, 1)
	ctx := context.Background()

	if _, err := p.RenderHTML(ctx, "<html><body>warm</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("baseline render failed: %v", err)
	}
	// Process count for one healthy instance, as the reference.
	steady := countProcs(t, "chromium")
	if steady == 0 {
		t.Fatal("expected a running chromium instance to count")
	}

	const cycles = 5
	for i := 0; i < cycles; i++ {
		if killAllChromium(t) == 0 {
			t.Fatalf("cycle %d: nothing killed", i)
		}
		time.Sleep(300 * time.Millisecond)
		if _, err := p.RenderHTML(ctx, "<html><body>cycle</body></html>", DefaultRenderOptions()); err != nil {
			t.Fatalf("cycle %d: render after kill failed: %v", i, err)
		}
	}

	if got := p.Stats().Restarts; got != cycles {
		t.Fatalf("Restarts = %d after %d kill cycles, want %d", got, cycles, cycles)
	}

	// After N restarts the pool must still hold roughly one instance's worth
	// of processes — not N instances' worth. Allow generous slack for
	// children still being reaped.
	deadline := time.Now().Add(10 * time.Second)
	var final int
	for time.Now().Before(deadline) {
		final = countProcs(t, "chromium")
		if final <= steady+2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if final > steady+2 {
		t.Fatalf("process leak across %d restarts: steady-state was %d, now %d — "+
			"cancelling chromedp contexts is not reclaiming replaced browsers",
			cycles, steady, final)
	}
	t.Logf("after %d restart cycles: steady=%d final=%d (no leak)", cycles, steady, final)
}

// signalChromium sends sig to every running Chromium process, matching on
// the real executable (see killAllChromium for why matching on command line
// is unsafe here).
func signalChromium(t *testing.T, sig string) int {
	t.Helper()
	chromiumPath, err := filepath.EvalSymlinks(os.Getenv("CHROMIUM_PATH"))
	if err != nil {
		t.Fatalf("resolving CHROMIUM_PATH: %v", err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("reading /proc: %v", err)
	}
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || exe != chromiumPath {
			continue
		}
		if exec.Command("kill", "-"+sig, strconv.Itoa(pid)).Run() == nil {
			n++
		}
	}
	return n
}

// TestPool_DetectsAndRecoversFromHungInstance is the proof for the fault the
// passive liveness check provably cannot see.
//
// SIGSTOP freezes the browser: the process still exists and its websocket
// stays open, but the message loop stops turning. Measured before this
// existed — alive() reported true throughout, so the pool considered the
// instance perfectly healthy while every render on it hung until its own
// 30s timeout, forever, with no self-repair.
func TestPool_DetectsAndRecoversFromHungInstance(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	p, err := NewPool(PoolConfig{
		Config: Config{ChromiumPath: os.Getenv("CHROMIUM_PATH")},
		Size:   1,
		// No success caching and a single failure threshold, so the test
		// exercises detection deterministically rather than waiting out the
		// production hysteresis.
		HealthProbeInterval:         time.Nanosecond,
		HealthProbeTimeout:          2 * time.Second,
		HealthProbeFailureThreshold: 1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() {
		signalChromium(t, "CONT") // never leave frozen processes behind
		p.Close()
	})

	ctx := context.Background()
	if _, err := p.RenderHTML(ctx, "<html><body>before</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("baseline render failed: %v", err)
	}

	frozen := signalChromium(t, "STOP")
	if frozen == 0 {
		t.Fatal("no chromium processes were frozen; test cannot prove hang detection")
	}

	// The passive check must still consider it alive — that is exactly the
	// blind spot this feature exists to cover. Asserted, so that if chromedp
	// ever starts cancelling the context on a hang, this test says so rather
	// than silently passing for the wrong reason.
	r := p.instances[0].renderer.Load()
	if !r.alive() {
		t.Fatal("precondition failed: the passive liveness check noticed the freeze, " +
			"so this test is no longer exercising hang detection")
	}

	// A render must now succeed anyway: the probe catches the hang and the
	// instance is rebuilt underneath the request.
	pdf, err := p.RenderHTML(ctx, "<html><body>after</body></html>", DefaultRenderOptions())
	if err != nil {
		t.Fatalf("render against a hung instance failed — hang detection did not recover it: %v", err)
	}
	if len(pdf) < 5 || string(pdf[:5]) != "%PDF-" {
		t.Fatalf("recovered render did not produce a valid PDF (%d bytes)", len(pdf))
	}

	st := p.Stats()
	if st.HangsDetected != 1 {
		t.Fatalf("HangsDetected = %d, want 1", st.HangsDetected)
	}
	if st.Restarts != 1 {
		t.Fatalf("Restarts = %d, want 1", st.Restarts)
	}
	if st.ProbesPerformed == 0 {
		t.Fatal("ProbesPerformed = 0; no probe was actually issued")
	}
}

// TestPool_AllInstancesUnusableReportsUnavailable pins the error identity
// that decides the HTTP status. When every instance is skipped or dead, the
// failure is an availability problem (503, retry — the pool self-heals), not
// a rejection of the caller's document (422, do not retry). Verified through
// errors.Is rather than string matching, because the API layer switches on
// exactly this.
func TestPool_AllInstancesUnusableReportsUnavailable(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	p, err := NewPool(PoolConfig{
		Config:                      Config{ChromiumPath: os.Getenv("CHROMIUM_PATH")},
		Size:                        1,
		HealthProbeTimeout:          time.Second,
		HealthProbeFailureThreshold: 2, // so the first failure is "suspect", not "hung"
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() {
		signalChromium(t, "CONT")
		p.Close()
	})

	if _, err := p.RenderHTML(context.Background(), "<html><body>warm</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("warm render: %v", err)
	}
	if signalChromium(t, "STOP") == 0 {
		t.Fatal("no chromium processes were frozen")
	}

	_, err = p.RenderHTML(context.Background(), "<html><body>x</body></html>", DefaultRenderOptions())
	if err == nil {
		t.Fatal("expected an error when the only instance is unusable")
	}
	if !errors.Is(err, ErrInstanceUnavailable) {
		t.Fatalf("error %v does not wrap ErrInstanceUnavailable — the API layer would report this "+
			"as a 422 document problem instead of a 503 availability problem", err)
	}
}

// TestPool_HysteresisToleratesASingleProbeFailure proves the threshold is
// real. Rebuilding a browser on one blown probe would mean a load spike that
// briefly starves CPU could trigger restarts across the pool — turning a busy
// service into a restarting one at precisely the worst moment.
func TestPool_HysteresisToleratesASingleProbeFailure(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	p, err := NewPool(PoolConfig{
		Config:                      Config{ChromiumPath: os.Getenv("CHROMIUM_PATH")},
		Size:                        1,
		HealthProbeInterval:         time.Nanosecond,
		HealthProbeTimeout:          time.Second,
		HealthProbeFailureThreshold: 2, // production default
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	slot := p.instances[0]
	r := slot.renderer.Load()

	// Simulate one transient probe failure without touching the browser.
	slot.probeMu.Lock()
	slot.probeFailures = 1
	slot.probeMu.Unlock()

	// A healthy probe now resets the counter rather than accumulating toward
	// a restart, so a single blip never compounds into one.
	if v := p.probeInstance(slot, r); v != probeHealthy {
		t.Fatalf("verdict = %v for a healthy instance with one prior failure, want probeHealthy", v)
	}
	slot.probeMu.Lock()
	failures := slot.probeFailures
	slot.probeMu.Unlock()
	if failures != 0 {
		t.Fatalf("probeFailures = %d after a successful probe, want 0 (a success must reset the run)", failures)
	}
	if got := p.Stats().HangsDetected; got != 0 {
		t.Fatalf("HangsDetected = %d, want 0", got)
	}
}

// TestPool_FailedRenderInvalidatesProbeCache closes a blind window found in
// live testing, not in the unit tests.
//
// The probe caches successes, so an instance that wedges immediately after a
// healthy probe stays trusted for up to HealthProbeInterval — and every
// request arriving in that window waits out its full render timeout. Observed
// against a running server: a browser frozen one second after a healthy probe
// cost the next request 35 seconds. A failed render is evidence worth acting
// on, so it clears the cached verdict and forces the next dispatch to probe.
//
// It must NOT restart on a failed render: a render can fail because the
// document is malformed or the caller cancelled, and rebuilding a healthy
// browser for that would be a self-inflicted outage. The probe still decides.
func TestPool_FailedRenderInvalidatesProbeCache(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	p, err := NewPool(PoolConfig{
		Config:              Config{ChromiumPath: os.Getenv("CHROMIUM_PATH")},
		Size:                1,
		HealthProbeInterval: time.Hour, // without invalidation, nothing would re-probe
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	ctx := context.Background()
	if _, err := p.RenderHTML(ctx, "<html><body>warm</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("warm render: %v", err)
	}
	slot := p.instances[0]

	slot.probeMu.Lock()
	cachedBefore := !slot.lastProbeOK.IsZero()
	slot.probeMu.Unlock()
	if !cachedBefore {
		t.Fatal("precondition failed: a successful probe should have been cached")
	}

	// A render that fails after acquiring an instance, by blowing its own
	// deadline — which is exactly how a hung browser presents. (An
	// already-cancelled caller context would not do: acquire rejects it
	// before any instance is involved, so there is no health evidence to
	// record.)
	tinyTimeout := DefaultRenderOptions()
	tinyTimeout.Timeout = time.Nanosecond
	if _, err := p.RenderHTML(ctx, "<html><body>x</body></html>", tinyTimeout); err == nil {
		t.Fatal("expected the render to fail on its own deadline")
	}

	slot.probeMu.Lock()
	cachedAfter := !slot.lastProbeOK.IsZero()
	slot.probeMu.Unlock()
	if cachedAfter {
		t.Fatal("a failed render left the cached health verdict intact — " +
			"the next request would keep trusting a possibly-wedged browser")
	}

	// Crucially, the instance was not rebuilt: a failed render is a reason to
	// re-check, never a reason to restart.
	if got := p.Stats().Restarts; got != 0 {
		t.Fatalf("Restarts = %d after a failed render, want 0 — a failed render must not trigger a rebuild", got)
	}

	// And the pool still works, re-probing on the next dispatch.
	if _, err := p.RenderHTML(ctx, "<html><body>after</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("render after cache invalidation failed: %v", err)
	}
}

// TestPool_HealthyProbeIsCached keeps the probe off the hot path: a healthy
// instance must not pay a CDP round trip on every single render.
func TestPool_HealthyProbeIsCached(t *testing.T) {
	if os.Getenv("CHROMIUM_PATH") == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	p, err := NewPool(PoolConfig{
		Config:              Config{ChromiumPath: os.Getenv("CHROMIUM_PATH")},
		Size:                1,
		HealthProbeInterval: time.Minute, // one probe should cover the whole test
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close() })

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := p.RenderHTML(ctx, "<html><body>x</body></html>", DefaultRenderOptions()); err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
	}
	if got := p.Stats().ProbesPerformed; got != 1 {
		t.Fatalf("ProbesPerformed = %d across 10 renders, want 1 — successful probes must be cached", got)
	}
}

// TestPool_HealthyPoolReportsAllAlive is the baseline for the health
// endpoint: a freshly built pool reports every instance alive.
func TestPool_HealthyPoolReportsAllAlive(t *testing.T) {
	p := testPool(t, 2)
	st := p.Stats()
	if st.Size != 2 {
		t.Fatalf("Size = %d, want 2", st.Size)
	}
	if st.Alive != 2 {
		t.Fatalf("Alive = %d, want 2 for a freshly started pool", st.Alive)
	}
	if st.Restarts != 0 || st.RestartAttempts != 0 {
		t.Fatalf("expected no restart activity on a fresh pool, got %+v", st)
	}
}
