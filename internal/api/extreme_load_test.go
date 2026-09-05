package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maulik-zuru/great-pdf-generator/internal/orchestration"
	"github.com/Maulik-zuru/great-pdf-generator/internal/renderengines"
)

// This file is the "heavy to heavy super load testing" + chaos-under-load
// tier the existing loadtest_test.go doesn't cover: TestLoadHeavyDocument
// measures latency/throughput at increasing concurrency, and
// TestLoadHeavyDocument_WithOrchestration proves one overload burst is
// rejected fast — but neither sustains overload across repeated waves to
// check for a slow leak, and neither injects a real Chromium failure while
// concurrent traffic is actually flowing (the existing crash/hang recovery
// tests in internal/renderengines all recover against an otherwise-idle
// pool). Both gaps matter for a real "is this production ready" verdict:
// a pool that recovers fine at rest can still behave differently while
// contended, and a backpressure mechanism that works once can still leak
// resources under sustained punishment.
//
// All tests here are opt-in (LOADTEST=1, CHROMIUM_PATH set), matching the
// existing convention, and use real Chromium — no mocking, consistent with
// this project's testing philosophy throughout.

func skipUnlessLoadTest(t *testing.T) string {
	t.Helper()
	if os.Getenv("LOADTEST") == "" {
		t.Skip("LOADTEST not set; skipping (opt-in, slow — set LOADTEST=1)")
	}
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set")
	}
	return path
}

// killAllChromiumProcesses hard-kills every running Chromium process,
// matched by its real executable (/proc/<pid>/exe), never by command line —
// see internal/renderengines/recovery_test.go's killAllChromium for why a
// command-line match (`pgrep -f headless_shell`) is actively dangerous: the
// test runner's own shell has CHROMIUM_PATH on its command line and would
// match itself.
func killAllChromiumProcesses(t *testing.T, chromiumPath string) int {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(chromiumPath)
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
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil || exe != resolved {
			continue
		}
		if exec.Command("kill", "-9", strconv.Itoa(pid)).Run() == nil {
			killed++
		}
	}
	return killed
}

// countChromiumProcesses mirrors chromiumRSSMB's own matching strategy
// (process name, not command line) but returns a count instead of memory —
// used here purely as a leak check after Close(), where the concern is
// "did anything survive", not "how much memory does it use".
func countChromiumProcesses() int {
	out, err := exec.Command("ps", "-eo", "comm").Output()
	if err != nil {
		return -1
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "headless_shell") {
			count++
		}
	}
	return count
}

// waitForCondition polls cond until it's true or timeout elapses, returning
// the final result of cond — used for the goroutine-count settle check
// below instead of a fixed sleep, since GC/finalizer timing varies.
func waitForCondition(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return cond()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExtremeLoad_SustainedOverloadWaves is the "heavy to heavy super load
// testing" tier: instead of one overload burst
// (TestLoadHeavyDocument_WithOrchestration), it fires several consecutive
// waves of far-beyond-capacity concurrent requests against the heavy
// 3-page fixture, checking that backpressure holds steady wave over wave
// (not just once) and that neither Chromium memory nor goroutine count
// creeps upward across waves — the signature of a slow leak that a single
// burst can't reveal.
func TestExtremeLoad_SustainedOverloadWaves(t *testing.T) {
	path := skipUnlessLoadTest(t)

	poolSize := envIntOrDefault("EXTREME_POOL_SIZE", 2)
	maxConcurrency := envIntOrDefault("EXTREME_MAX_CONCURRENCY", 4)
	waves := envIntOrDefault("EXTREME_WAVES", 5)
	overloadFactor := envIntOrDefault("EXTREME_OVERLOAD_FACTOR", 4)

	baselineGoroutines := runtime.NumGoroutine()

	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      poolSize,
		MaxConcurrencyPerInstance: maxConcurrency,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	workers := poolSize * maxConcurrency
	jobPool := orchestration.NewPool(orchestration.PoolConfig{Workers: workers, QueueCapacity: workers})

	srv := httptest.NewServer(NewServer(pool, nil, jobPool, nil, WithRenderPoolHealth(pool)).Routes())

	html := heavyReportHTML(heavyReportRowsFor3Pages)
	admitCapacity := workers + workers
	concurrency := admitCapacity * overloadFactor

	time.Sleep(500 * time.Millisecond) // let the pool's warm-up settle before baselining

	var peakRSS float64
	type waveSummary struct {
		ok, rejected, other int
		peakRSSAfter        float64
	}
	var waves_ []waveSummary

	t.Logf("chromium capacity=%d, admits up to %d at once, firing %d waves of %d concurrent requests each",
		workers, admitCapacity, waves, concurrency)

	for w := 0; w < waves; w++ {
		res := runOverloadLevel(srv.URL, html, concurrency)
		rss := chromiumRSSMB()
		if rss > peakRSS {
			peakRSS = rss
		}
		waves_ = append(waves_, waveSummary{ok: res.ok, rejected: res.rejected503, other: res.otherFailed, peakRSSAfter: rss})
		t.Logf("wave %d/%d: ok=%d rejected503=%d other=%d chromium_rss=%.1fMB goroutines=%d",
			w+1, waves, res.ok, res.rejected503, res.otherFailed, rss, runtime.NumGoroutine())

		if res.otherFailed != 0 {
			t.Errorf("wave %d: expected only 200s and 503s under sustained overload, got %d other failures", w+1, res.otherFailed)
		}
		if res.ok == 0 {
			t.Errorf("wave %d: expected at least some requests to succeed even under overload", w+1)
		}
		if res.rejected503 == 0 {
			t.Errorf("wave %d: expected some requests rejected at %dx admitted capacity — backpressure did not engage", w+1, overloadFactor)
		}
	}

	// A slow leak shows up as the last wave's RSS being substantially above
	// the first wave's, not as a one-time bump (warm-up, GC timing) between
	// wave 1 and 2. Compare the last wave against the *second* wave (after
	// initial warm-up settles) with a generous tolerance — this is a smoke
	// check for gross leak-shaped growth, not a precise memory budget.
	if len(waves_) >= 3 {
		early := waves_[1].peakRSSAfter
		late := waves_[len(waves_)-1].peakRSSAfter
		if early > 0 && late > early*2 {
			t.Errorf("chromium RSS grew from %.1fMB (wave 2) to %.1fMB (wave %d) — more than doubled across sustained overload waves, possible leak",
				early, late, len(waves_))
		}
	}
	t.Logf("peak chromium RSS observed across all waves: %.1fMB", peakRSS)

	// --- Leak checks: everything must clean up completely on Close(). ---
	jobPool.Close()
	if err := pool.Close(); err != nil {
		t.Errorf("pool.Close() after sustained overload: %v", err)
	}
	srv.Close()

	if !waitForCondition(5*time.Second, func() bool { return countChromiumProcesses() == 0 }) {
		t.Errorf("chromium processes still running %v after Close() following sustained overload — process leak", countChromiumProcesses())
	}

	const goroutineSlack = 20 // testing/runtime bookkeeping goroutines unrelated to this pool
	if !waitForCondition(5*time.Second, func() bool { return runtime.NumGoroutine() <= baselineGoroutines+goroutineSlack }) {
		t.Errorf("goroutine count = %d after Close(), baseline was %d (slack %d) — possible goroutine leak under sustained overload",
			runtime.NumGoroutine(), baselineGoroutines, goroutineSlack)
	}
}

// loadTimelineEntry records one request's outcome during the chaos-under-
// load test below, timestamped relative to test start so the recovery
// timeline can be reconstructed and asserted on afterward.
type loadTimelineEntry struct {
	t      time.Duration
	status int
	code   string // parsed error envelope "code", empty on 200
}

// parseErrorCode extracts the {"error":{"code":...}} envelope's code field
// from a real *http.Response body — the real-network equivalent of this
// package's decodeError helper, which only works against an
// httptest.ResponseRecorder.
func parseErrorCode(body []byte) string {
	var env errorEnvelope
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.Error.Code
}

// TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled closes a
// real gap: every existing crash/hang-recovery test
// (internal/renderengines/recovery_test.go) kills or freezes Chromium
// against an otherwise-idle pool and then issues one render to prove
// recovery. None of them prove recovery *while concurrent HTTP traffic is
// actively flowing* — which is the only scenario that happens in
// production, since a real deployment is never idle when a browser crashes.
// This test runs continuous concurrent load through the real HTTP path,
// hard-kills every Chromium process partway through, and asserts: (1) some
// requests fail with 503 ENGINE_UNAVAILABLE during the outage window (proof
// detection engaged), (2) no request gets an unexpected status/code, and
// (3) the tail of the timeline shows sustained recovery (proof the pool
// self-healed under load, not just at rest).
func TestExtremeLoad_RecoversUnderConcurrentLoadAfterChromiumKilled(t *testing.T) {
	path := skipUnlessLoadTest(t)

	poolSize := envIntOrDefault("CHAOS_POOL_SIZE", 2)
	maxConcurrency := envIntOrDefault("CHAOS_MAX_CONCURRENCY", 2)
	workers := poolSize * maxConcurrency

	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      poolSize,
		MaxConcurrencyPerInstance: maxConcurrency,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	jobPool := orchestration.NewPool(orchestration.PoolConfig{Workers: workers, QueueCapacity: workers})
	defer jobPool.Close()

	srv := httptest.NewServer(NewServer(pool, nil, jobPool, nil, WithRenderPoolHealth(pool)).Routes())
	defer srv.Close()

	const content = `{"content":"<html><body><h1>chaos</h1></body></html>"}`

	time.Sleep(500 * time.Millisecond) // let the pool's warm-up settle

	var mu sync.Mutex
	var timeline []loadTimelineEntry
	start := time.Now()
	stop := make(chan struct{})
	var loopers sync.WaitGroup

	for i := 0; i < workers; i++ {
		loopers.Add(1)
		go func() {
			defer loopers.Done()
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				reqStart := time.Now()
				resp, err := client.Post(srv.URL+"/v1/pdf/html", "application/json", bytes.NewReader([]byte(content)))
				if err != nil {
					mu.Lock()
					timeline = append(timeline, loadTimelineEntry{t: time.Since(start), status: 0, code: "TRANSPORT_ERROR"})
					mu.Unlock()
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				code := ""
				if resp.StatusCode != http.StatusOK {
					code = parseErrorCode(body)
				}
				mu.Lock()
				timeline = append(timeline, loadTimelineEntry{t: reqStart.Sub(start), status: resp.StatusCode, code: code})
				mu.Unlock()
			}
		}()
	}

	time.Sleep(1 * time.Second) // pre-kill window: establish steady healthy traffic

	killed := killAllChromiumProcesses(t, path)
	if killed == 0 {
		t.Fatal("no chromium processes were killed; test cannot prove recovery under load")
	}
	t.Logf("killed %d chromium process(es) at t=%s while %d loopers were sending continuous traffic", killed, time.Since(start), workers)

	time.Sleep(6 * time.Second) // post-kill window: long enough to detect, restart, and resume

	close(stop)
	loopers.Wait()

	mu.Lock()
	defer mu.Unlock()

	if len(timeline) == 0 {
		t.Fatal("no requests were recorded at all")
	}

	var okCount, engineUnavailable, queueFull, transportErr, unexpected int
	for _, e := range timeline {
		switch {
		case e.status == http.StatusOK:
			okCount++
		case e.code == "ENGINE_UNAVAILABLE":
			engineUnavailable++
		case e.code == "QUEUE_FULL":
			queueFull++
		case e.code == "TRANSPORT_ERROR":
			transportErr++
		default:
			unexpected++
			t.Errorf("unexpected outcome at t=%s: status=%d code=%q", e.t, e.status, e.code)
		}
	}
	t.Logf("timeline: %d total, %d ok, %d ENGINE_UNAVAILABLE, %d QUEUE_FULL, %d transport errors, %d unexpected",
		len(timeline), okCount, engineUnavailable, queueFull, transportErr, unexpected)

	if engineUnavailable == 0 {
		t.Error("expected at least one 503 ENGINE_UNAVAILABLE during the outage window — detection did not visibly engage under load")
	}
	if transportErr != 0 {
		t.Errorf("%d requests failed at the transport level — only the render engine should have been affected, not the HTTP server itself", transportErr)
	}

	// Recovery proof: the final second of the timeline must be
	// overwhelmingly successful — sustained recovery under load, not a
	// lucky single request.
	var tailTotal, tailOK int
	tailStart := time.Since(start) - time.Second
	for _, e := range timeline {
		if e.t >= tailStart {
			tailTotal++
			if e.status == http.StatusOK {
				tailOK++
			}
		}
	}
	if tailTotal == 0 {
		t.Fatal("no requests recorded in the final second of the timeline")
	}
	if successRate := float64(tailOK) / float64(tailTotal); successRate < 0.8 {
		t.Errorf("final-second success rate = %.0f%% (%d/%d) — expected sustained recovery (>=80%%) well after the kill",
			successRate*100, tailOK, tailTotal)
	}

	if got := pool.Stats().Restarts; got == 0 {
		t.Error("Pool.Stats().Restarts = 0 after killing every chromium process — recovery did not actually rebuild any instance")
	}
	if st := pool.Stats(); st.Alive != st.Size {
		t.Errorf("after recovery Alive=%d, want Size=%d — pool did not fully recover", st.Alive, st.Size)
	}

	// --- Leak check specific to the chaos path: recovery involves killing
	// and rebuilding instances mid-load, which is exactly the scenario most
	// likely to strand an orphaned process if restart() ever failed to
	// clean up the instance it replaced. ---
	jobPool.Close()
	if err := pool.Close(); err != nil {
		t.Errorf("pool.Close() after chaos test: %v", err)
	}
	if !waitForCondition(5*time.Second, func() bool { return countChromiumProcesses() == 0 }) {
		t.Errorf("%d chromium processes still running after Close() following mid-load kill+recovery — process leak", countChromiumProcesses())
	}
}
