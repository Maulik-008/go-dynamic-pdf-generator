package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/orchestration"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// conversionRequestBody JSON-encodes html into the unified {"content"}
// envelope every conversion endpoint now expects (see
// docs/planning/SPEC-conversion-api.md) — json.Marshal, not fmt.Sprintf,
// since the heavy fixture embeds quotes and base64 data: URIs that a naive
// string-quoted body would corrupt.
func conversionRequestBody(html string) []byte {
	body, err := json.Marshal(conversionRequest{Content: html})
	if err != nil {
		panic(err) // encoding a string field can't fail
	}
	return body
}

// TestLoadHeavyDocument is an opt-in (LOADTEST=1), slow load test against a
// heavy 3-page document (heavyReportHTML), fired through the real HTTP path
// (httptest.Server wrapping the actual Routes()) at increasing concurrency
// levels. It exists to give the platform's speed budgets in
// docs/planning/PLATFORM-SPEC.md a real, measured answer instead of a
// guess, per the performance-optimization skill's measure-first approach.
//
// Tune via env vars: LOADTEST_POOL_SIZE, LOADTEST_MAX_CONCURRENCY,
// LOADTEST_REQUESTS_PER_WORKER. Run with a generous -timeout — a full sweep
// across several concurrency levels against a genuinely heavy document is
// slow by design.
func TestLoadHeavyDocument(t *testing.T) {
	if os.Getenv("LOADTEST") == "" {
		t.Skip("LOADTEST not set; skipping heavy load test (opt-in, slow — set LOADTEST=1)")
	}
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set")
	}

	poolSize := envIntOrDefault("LOADTEST_POOL_SIZE", 3)
	maxConcurrency := envIntOrDefault("LOADTEST_MAX_CONCURRENCY", 4)
	requestsPerWorker := envIntOrDefault("LOADTEST_REQUESTS_PER_WORKER", 8)

	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      poolSize,
		MaxConcurrencyPerInstance: maxConcurrency,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	srv := httptest.NewServer(NewServer(pool, nil, nil, nil).Routes())
	defer srv.Close()

	html := heavyReportHTML(heavyReportRowsFor3Pages)
	t.Logf("heavy document: %d bytes HTML, calibrated to 3 PDF pages (see heavy_content_test.go)", len(html))

	time.Sleep(500 * time.Millisecond) // let pool warm-up settle before baselining
	baselineRSS := chromiumRSSMB()
	t.Logf("baseline chromium RSS (pool size=%d, idle): %.1f MB", poolSize, baselineRSS)

	sanity := runLoadLevel(srv.URL, html, 1, 1)
	if sanity.succeeded != 1 {
		t.Fatalf("sanity check failed before load sweep: %+v", sanity)
	}
	t.Logf("sanity check: single heavy render took %s", sanity.latencies[0])

	concurrencyLevels := []int{1, 2, 4, 8, 16}

	var peakRSS float64
	stopSampler := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if rss := chromiumRSSMB(); rss > peakRSS {
					peakRSS = rss
				}
			case <-stopSampler:
				return
			}
		}
	}()

	var results []loadResult
	for _, c := range concurrencyLevels {
		res := runLoadLevel(srv.URL, html, c, requestsPerWorker)
		results = append(results, res)
		p50 := percentile(res.latencies, 0.50)
		p90 := percentile(res.latencies, 0.90)
		p99 := percentile(res.latencies, 0.99)
		t.Logf("concurrency=%2d total=%3d ok=%3d failed=%d wall=%s throughput=%.2f/s p50=%s p90=%s p99=%s",
			c, res.total, res.succeeded, res.failed, res.duration.Round(time.Millisecond),
			res.throughputPerSec(), p50.Round(time.Millisecond), p90.Round(time.Millisecond), p99.Round(time.Millisecond))
	}

	close(stopSampler)
	samplerWG.Wait()
	t.Logf("peak chromium RSS observed during load: %.1f MB", peakRSS)

	reportPath := writeLoadTestReport(t, poolSize, maxConcurrency, requestsPerWorker, len(html), baselineRSS, peakRSS, results)
	t.Logf("full report written to %s", reportPath)
}

// TestLoadHeavyDocument_WithOrchestration is the direct follow-up to
// TestLoadHeavyDocument, re-run with the same heavy fixture through a
// server that now gates admission via internal/orchestration (see
// docs/planning/SPEC-job-orchestration.md). It exists specifically to
// answer, with real numbers, the gap the original run found and quantified:
// "nothing in this pool rejects excess load; it just queues longer." This
// fires well beyond the pool's real concurrent-render capacity and asserts
// the pool now rejects the excess fast and deterministically (bounded
// latency, real 503s) instead of letting latency grow unbounded with no
// failures at all.
func TestLoadHeavyDocument_WithOrchestration(t *testing.T) {
	if os.Getenv("LOADTEST") == "" {
		t.Skip("LOADTEST not set; skipping heavy load test (opt-in, slow — set LOADTEST=1)")
	}
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set")
	}

	poolSize := envIntOrDefault("LOADTEST_POOL_SIZE", 3)
	maxConcurrency := envIntOrDefault("LOADTEST_MAX_CONCURRENCY", 4)

	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      poolSize,
		MaxConcurrencyPerInstance: maxConcurrency,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// Same sizing heuristic as cmd/api/main.go's default wiring: Workers
	// matches the render pool's own real concurrent-render capacity,
	// QueueCapacity allows roughly double that in flight before rejecting.
	workers := poolSize * maxConcurrency
	jobPool := orchestration.NewPool(orchestration.PoolConfig{Workers: workers, QueueCapacity: workers})
	defer jobPool.Close()

	srv := httptest.NewServer(NewServer(pool, nil, jobPool, nil).Routes())
	defer srv.Close()

	html := heavyReportHTML(heavyReportRowsFor3Pages)
	admitCapacity := workers + workers // Workers + QueueCapacity
	concurrency := admitCapacity * 3   // deliberately well beyond capacity
	t.Logf("chromium capacity=%d, orchestration admits up to %d at once, firing %d concurrent requests",
		workers, admitCapacity, concurrency)

	time.Sleep(500 * time.Millisecond) // let pool warm-up settle

	res := runOverloadLevel(srv.URL, html, concurrency)
	t.Logf("total=%d ok=%d rejected503=%d otherFailed=%d wall=%s",
		res.total, res.ok, res.rejected503, res.otherFailed, res.duration.Round(time.Millisecond))
	if len(res.rejectedLatencies) > 0 {
		sort.Slice(res.rejectedLatencies, func(i, j int) bool { return res.rejectedLatencies[i] < res.rejectedLatencies[j] })
		t.Logf("rejected-request latency: min=%s p50=%s max=%s",
			res.rejectedLatencies[0].Round(time.Millisecond),
			percentile(res.rejectedLatencies, 0.5).Round(time.Millisecond),
			res.rejectedLatencies[len(res.rejectedLatencies)-1].Round(time.Millisecond))
	}

	if res.otherFailed != 0 {
		t.Fatalf("expected only 200s and 503s under overload, got %d other failures", res.otherFailed)
	}
	if res.ok == 0 {
		t.Fatal("expected at least some requests to succeed even under overload")
	}
	if res.rejected503 == 0 {
		t.Fatal("expected some requests to be rejected (503) at 3x admitted capacity — backpressure is not engaging")
	}

	// The defining property this test exists to prove: rejections are fast
	// (admission control, not a request that waited then failed) — bound
	// generously (well above real scheduling noise) so this stays robust
	// on a busy CI box while still catching a regression back to unbounded
	// blocking.
	const maxAcceptableRejectionLatency = 500 * time.Millisecond
	for _, l := range res.rejectedLatencies {
		if l > maxAcceptableRejectionLatency {
			t.Fatalf("a 503 rejection took %s, want under %s — rejections must be immediate, not delayed",
				l.Round(time.Millisecond), maxAcceptableRejectionLatency)
		}
	}

	appendOrchestrationLoadTestReport(t, poolSize, maxConcurrency, workers, admitCapacity, concurrency, res)
}

// overloadResult tracks outcomes by status code (unlike loadResult, which
// only records latency for successful requests) — the 503 latencies
// themselves are exactly what this test needs to prove backpressure is
// immediate, not the graceful-degradation-in-disguise the original load
// test found.
type overloadResult struct {
	total             int
	ok                int
	rejected503       int
	otherFailed       int
	duration          time.Duration
	rejectedLatencies []time.Duration
}

func runOverloadLevel(baseURL, html string, concurrency int) overloadResult {
	type outcome struct {
		status  int
		latency time.Duration
	}
	outcomes := make(chan outcome, concurrency)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 60 * time.Second}
			reqStart := time.Now()
			resp, err := client.Post(baseURL+"/v1/pdf/html", "application/json", bytes.NewReader(conversionRequestBody(html)))
			if err != nil {
				outcomes <- outcome{status: 0, latency: time.Since(reqStart)}
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			outcomes <- outcome{status: resp.StatusCode, latency: time.Since(reqStart)}
		}()
	}
	wg.Wait()
	close(outcomes)
	duration := time.Since(start)

	var res overloadResult
	res.total = concurrency
	res.duration = duration
	for o := range outcomes {
		switch o.status {
		case http.StatusOK:
			res.ok++
		case http.StatusServiceUnavailable:
			res.rejected503++
			res.rejectedLatencies = append(res.rejectedLatencies, o.latency)
		default:
			res.otherFailed++
		}
	}
	return res
}

// appendOrchestrationLoadTestReport appends this follow-up's findings to
// the existing docs/research/load-test-results.md (written by
// writeLoadTestReport during TestLoadHeavyDocument) rather than overwriting
// it — that file's original data and hand-written conclusion stay intact;
// this is additive, direct evidence for the specific gap that conclusion
// named.
func appendOrchestrationLoadTestReport(t *testing.T, poolSize, maxConcurrency, workers, admitCapacity, concurrency int, res overloadResult) {
	t.Helper()

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n## Follow-up: job-orchestration backpressure (re-run with admission control)\n\n")
	fmt.Fprintf(&sb, "Raw data from `internal/api/loadtest_test.go` (`TestLoadHeavyDocument_WithOrchestration`), run with\n")
	fmt.Fprintf(&sb, "`LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument_WithOrchestration -v`.\n")
	fmt.Fprintf(&sb, "Generated %s.\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "Direct answer to this file's own \"Fix recommendation\" above: same pool config (%d instances x %d),\n", poolSize, maxConcurrency)
	fmt.Fprintf(&sb, "now gated by an `internal/orchestration.Pool` sized to the pool's real capacity\n")
	fmt.Fprintf(&sb, "(Workers=%d, QueueCapacity=%d, admits %d in flight at once), fired at %dx that capacity\n", workers, workers, admitCapacity, concurrency/admitCapacity)
	fmt.Fprintf(&sb, "(%d concurrent requests) — deliberately far beyond what the earlier run's %d-way sweep reached.\n\n", concurrency, 16)
	fmt.Fprintf(&sb, "| Total requests | Succeeded (200) | Rejected (503) | Other failures |\n")
	fmt.Fprintf(&sb, "|---|---|---|---|\n")
	fmt.Fprintf(&sb, "| %d | %d | %d | %d |\n\n", res.total, res.ok, res.rejected503, res.otherFailed)

	if len(res.rejectedLatencies) > 0 {
		min := res.rejectedLatencies[0]
		max := res.rejectedLatencies[len(res.rejectedLatencies)-1]
		fmt.Fprintf(&sb, "503 rejection latency: min=%s p50=%s max=%s — every rejection is an immediate admission-control\n",
			min.Round(time.Millisecond), percentile(res.rejectedLatencies, 0.5).Round(time.Millisecond), max.Round(time.Millisecond))
		fmt.Fprintf(&sb, "decision, not a request that waited and then failed.\n\n")
	}

	fmt.Fprintf(&sb, "**Conclusion**: this closes the gap the original run's point 3 identified. At %dx admitted capacity,\n", concurrency/admitCapacity)
	fmt.Fprintf(&sb, "%d of %d requests were rejected immediately (bounded, well under the unbounded latency growth\n", res.rejected503, res.total)
	fmt.Fprintf(&sb, "previously observed) while the remaining %d succeeded normally — the platform now has a real ceiling\n", res.ok)
	fmt.Fprintf(&sb, "(reject past admitted capacity) instead of only queueing past it.\n")

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	outPath := filepath.Join(repoRoot, "docs", "research", "load-test-results.md")
	f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", outPath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(sb.String()); err != nil {
		t.Fatalf("append to %s: %v", outPath, err)
	}
	t.Logf("follow-up report appended to %s", outPath)
}

func envIntOrDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

type loadResult struct {
	concurrency int
	total       int
	succeeded   int
	failed      int
	duration    time.Duration
	latencies   []time.Duration // successful requests only, sorted ascending
}

func (r loadResult) throughputPerSec() float64 {
	if r.duration <= 0 {
		return 0
	}
	return float64(r.succeeded) / r.duration.Seconds()
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// chromiumRSSMB sums resident memory (MB) across every headless Chromium
// process currently running, via ps — a simple, dependency-free way to
// characterize this pool's real memory footprint under load.
func chromiumRSSMB() float64 {
	out, err := exec.Command("ps", "-eo", "rss,comm").Output()
	if err != nil {
		return -1
	}
	var totalKB float64
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if !strings.Contains(fields[1], "headless_shell") {
			continue
		}
		kb, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		totalKB += kb
	}
	return totalKB / 1024
}

// runLoadLevel fires concurrency workers, each issuing requestsPerWorker
// sequential POST /v1/pdf/html requests against baseURL with the given
// (heavy) html body, and returns per-request latencies plus aggregate
// success/failure counts and wall-clock duration for the whole batch.
func runLoadLevel(baseURL, html string, concurrency, requestsPerWorker int) loadResult {
	total := concurrency * requestsPerWorker
	latCh := make(chan time.Duration, total)
	var succeeded, failed int64

	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 60 * time.Second}
			for i := 0; i < requestsPerWorker; i++ {
				reqStart := time.Now()
				resp, err := client.Post(baseURL+"/v1/pdf/html", "application/json", bytes.NewReader(conversionRequestBody(html)))
				if err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || readErr != nil || !bytes.HasPrefix(body, []byte("%PDF-")) {
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&succeeded, 1)
				latCh <- time.Since(reqStart)
			}
		}()
	}
	wg.Wait()
	close(latCh)
	duration := time.Since(start)

	latencies := make([]time.Duration, 0, total)
	for l := range latCh {
		latencies = append(latencies, l)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	return loadResult{
		concurrency: concurrency,
		total:       total,
		succeeded:   int(succeeded),
		failed:      int(failed),
		duration:    duration,
		latencies:   latencies,
	}
}

// writeLoadTestReport writes the raw methodology/environment/results data to
// docs/research/load-test-results.md. It deliberately stops at the data —
// the Conclusion section is written by hand afterward, based on what these
// numbers actually show, not generated here.
func writeLoadTestReport(t *testing.T, poolSize, maxConcurrency, requestsPerWorker, htmlBytes int, baselineRSS, peakRSS float64, results []loadResult) string {
	t.Helper()

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Load Test Results: Heavy Multi-Page Document\n\n")
	fmt.Fprintf(&sb, "Raw data from `internal/api/loadtest_test.go` (`TestLoadHeavyDocument`), run with\n")
	fmt.Fprintf(&sb, "`LOADTEST=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestLoadHeavyDocument -v`.\n")
	fmt.Fprintf(&sb, "Generated %s.\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&sb, "## Test environment\n\n")
	fmt.Fprintf(&sb, "- CPU cores (`runtime.NumCPU()`): %d\n", runtime.NumCPU())
	fmt.Fprintf(&sb, "- This is a sandboxed development environment, not production-target hardware —\n")
	fmt.Fprintf(&sb, "  absolute numbers below should be re-validated on real deployment hardware before\n")
	fmt.Fprintf(&sb, "  being treated as SLAs, per PLATFORM-SPEC.md's own framing (\"targets to validate\n")
	fmt.Fprintf(&sb, "  with our own load tests, not facts\").\n\n")

	fmt.Fprintf(&sb, "## Methodology\n\n")
	fmt.Fprintf(&sb, "- **Document under test**: `heavyReportHTML(%d)` — 3 embedded synthetic chart images\n", heavyReportRowsFor3Pages)
	fmt.Fprintf(&sb, "  (400x200 PNG each, base64-inlined) plus a %d-row itemized data table, %d bytes of\n", heavyReportRowsFor3Pages, htmlBytes)
	fmt.Fprintf(&sb, "  HTML. Empirically calibrated to render to exactly **3 PDF pages** at A4/default\n")
	fmt.Fprintf(&sb, "  margins (calibration sweep: 30 rows -> 2 pages, 50 -> 3, 70 -> 5, 90 -> 7, 110 -> 8).\n")
	fmt.Fprintf(&sb, "- **Path under test**: real HTTP, `POST /v1/pdf/html`, via `httptest.Server` wrapping\n")
	fmt.Fprintf(&sb, "  the actual `internal/api` routes — not a direct in-process `Pool.RenderHTML` call.\n")
	fmt.Fprintf(&sb, "- **Pool config**: %d Chromium instances, %d max concurrent renders per instance\n", poolSize, maxConcurrency)
	fmt.Fprintf(&sb, "  (%d total concurrent-render capacity).\n", poolSize*maxConcurrency)
	fmt.Fprintf(&sb, "- **Load pattern**: for each concurrency level, N workers each fire %d sequential\n", requestsPerWorker)
	fmt.Fprintf(&sb, "  requests; latency is measured per-request (queue wait + render + response transfer\n")
	fmt.Fprintf(&sb, "  combined, i.e. what a real client actually experiences), throughput is\n")
	fmt.Fprintf(&sb, "  successful-requests / wall-clock-duration-of-the-whole-batch.\n")
	fmt.Fprintf(&sb, "- **Resource sampling**: total resident memory (RSS) across every `headless_shell`\n")
	fmt.Fprintf(&sb, "  process, sampled every 300ms throughout the run via `ps`; baseline taken at idle\n")
	fmt.Fprintf(&sb, "  right after pool warm-up, peak taken as the max sample across the entire sweep.\n\n")

	fmt.Fprintf(&sb, "## Results\n\n")
	fmt.Fprintf(&sb, "- Baseline Chromium RSS (idle, %d warm instances): **%.1f MB**\n", poolSize, baselineRSS)
	fmt.Fprintf(&sb, "- Peak Chromium RSS observed during the full sweep: **%.1f MB**\n\n", peakRSS)

	fmt.Fprintf(&sb, "| Concurrency | Requests | Succeeded | Failed | Wall time | Throughput (req/s) | p50 | p90 | p99 | min | max |\n")
	fmt.Fprintf(&sb, "|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		var p50, p90, p99, min, max time.Duration
		if len(r.latencies) > 0 {
			p50 = percentile(r.latencies, 0.50)
			p90 = percentile(r.latencies, 0.90)
			p99 = percentile(r.latencies, 0.99)
			min = r.latencies[0]
			max = r.latencies[len(r.latencies)-1]
		}
		fmt.Fprintf(&sb, "| %d | %d | %d | %d | %s | %.2f | %s | %s | %s | %s | %s |\n",
			r.concurrency, r.total, r.succeeded, r.failed,
			r.duration.Round(time.Millisecond), r.throughputPerSec(),
			p50.Round(time.Millisecond), p90.Round(time.Millisecond), p99.Round(time.Millisecond),
			min.Round(time.Millisecond), max.Round(time.Millisecond))
	}
	sb.WriteString("\n")

	// Locate the repo root (this file lives at internal/api/loadtest_test.go)
	// so the report always lands at docs/research/ regardless of the
	// working directory `go test` was invoked from.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	outDir := filepath.Join(repoRoot, "docs", "research")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}
	outPath := filepath.Join(outDir, "load-test-results.md")
	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", outPath, err)
	}
	return outPath
}
