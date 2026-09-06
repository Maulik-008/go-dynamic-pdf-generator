package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// soakSample is one point on the memory-growth curve.
type soakSample struct {
	render  int
	rssMB   float64
	latency time.Duration
}

// TestSoakMemoryGrowth answers a question this project refused to answer by
// copying someone else's constant: does a long-lived Chromium instance
// actually grow its memory footprint enough to need periodic recycling, and
// if so, after how many renders?
//
// Gotenberg ships `chromium-restart-after=100`, but the crash-recovery
// research found no published source justifying that specific number — it is
// a shipped default, not a measured optimum. Rather than cargo-cult it, this
// test renders the same heavy 3-page fixture used by the load test hundreds
// of times against ONE warm instance, sampling resident memory, so the
// decision rests on this environment's own data.
//
// Opt-in and slow by design:
//
//	SOAK=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestSoakMemoryGrowth -v -timeout 900s
//
// Tune with SOAK_RENDERS (default 400) and SOAK_SAMPLE_EVERY (default 25).
func TestSoakMemoryGrowth(t *testing.T) {
	if os.Getenv("SOAK") == "" {
		t.Skip("SOAK not set; skipping memory soak test (opt-in, slow)")
	}
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set")
	}

	renders := envIntOrDefault("SOAK_RENDERS", 400)
	sampleEvery := envIntOrDefault("SOAK_SAMPLE_EVERY", 25)

	// One instance, so every sample is attributable to that instance rather
	// than to round-robin spreading work across several.
	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      1,
		MaxConcurrencyPerInstance: 1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	html := heavyReportHTML(heavyReportRowsFor3Pages)
	ctx := context.Background()

	// Warm up before the first sample so startup allocation isn't mistaken
	// for growth caused by rendering.
	for i := 0; i < 3; i++ {
		if _, err := pool.RenderHTML(ctx, html, renderengines.DefaultRenderOptions()); err != nil {
			t.Fatalf("warm-up render %d: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond)

	var samples []soakSample
	baseline := chromiumRSSMB()
	samples = append(samples, soakSample{render: 0, rssMB: baseline})
	t.Logf("baseline after warm-up: %.1f MB", baseline)

	start := time.Now()
	var lastLatency time.Duration
	for i := 1; i <= renders; i++ {
		renderStart := time.Now()
		if _, err := pool.RenderHTML(ctx, html, renderengines.DefaultRenderOptions()); err != nil {
			t.Fatalf("render %d failed: %v", i, err)
		}
		lastLatency = time.Since(renderStart)

		if i%sampleEvery == 0 {
			rss := chromiumRSSMB()
			samples = append(samples, soakSample{render: i, rssMB: rss, latency: lastLatency})
			t.Logf("after %4d renders: RSS=%7.1f MB  (%+.1f MB vs baseline)  last render=%s",
				i, rss, rss-baseline, lastLatency.Round(time.Millisecond))
		}
	}
	elapsed := time.Since(start)

	final := samples[len(samples)-1]
	growth := final.rssMB - baseline
	perRender := growth / float64(renders)

	t.Logf("")
	t.Logf("=== SOAK RESULT ===")
	t.Logf("renders:            %d in %s", renders, elapsed.Round(time.Second))
	t.Logf("baseline RSS:       %.1f MB", baseline)
	t.Logf("final RSS:          %.1f MB", final.rssMB)
	t.Logf("total growth:       %+.1f MB (%.3f MB/render)", growth, perRender)
	if perRender > 0 {
		t.Logf("renders to +500MB:  %.0f", 500/perRender)
	}

	writeSoakReport(t, renders, sampleEvery, baseline, samples, elapsed)
}

// writeSoakReport records the raw series so the recycling decision is
// reproducible and auditable, rather than a number someone remembers.
func writeSoakReport(t *testing.T, renders, sampleEvery int, baseline float64,
	samples []soakSample, elapsed time.Duration) {
	t.Helper()

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Soak Test: Chromium memory growth over sustained rendering\n\n")
	fmt.Fprintf(&sb, "Raw data from `internal/api/soak_test.go` (`TestSoakMemoryGrowth`), run with\n")
	fmt.Fprintf(&sb, "`SOAK=1 CHROMIUM_PATH=... go test ./internal/api/... -run TestSoakMemoryGrowth -v -timeout 900s`.\n")
	fmt.Fprintf(&sb, "Generated %s.\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&sb, "## Why this test exists\n\n")
	fmt.Fprintf(&sb, "`docs/research/crash-recovery-research.md` recommends recycling a browser after N\n")
	fmt.Fprintf(&sb, "renders to bound memory growth, and notes that Gotenberg ships `restart-after=100`\n")
	fmt.Fprintf(&sb, "— but also records, explicitly, that **no published source justifies that number**.\n")
	fmt.Fprintf(&sb, "It is a shipped default, not a measured optimum. This test measures the actual\n")
	fmt.Fprintf(&sb, "growth curve in this environment so the threshold (or the decision not to have one)\n")
	fmt.Fprintf(&sb, "rests on data.\n\n")

	fmt.Fprintf(&sb, "## Method\n\n")
	fmt.Fprintf(&sb, "- **One** Chromium instance (pool size 1, concurrency 1), so every sample is\n")
	fmt.Fprintf(&sb, "  attributable to that instance.\n")
	fmt.Fprintf(&sb, "- Document under test: the same heavy 3-page fixture as the load test\n")
	fmt.Fprintf(&sb, "  (`heavyReportHTML`) — embedded images plus a large data table.\n")
	fmt.Fprintf(&sb, "- 3 warm-up renders before the baseline sample, so startup allocation is not\n")
	fmt.Fprintf(&sb, "  counted as render-driven growth.\n")
	fmt.Fprintf(&sb, "- %d renders, sampling total `headless_shell` RSS every %d.\n", renders, sampleEvery)
	fmt.Fprintf(&sb, "- Environment: %d CPU cores. Sandboxed dev box, not production hardware.\n\n", runtime.NumCPU())

	fmt.Fprintf(&sb, "## Results\n\n")
	fmt.Fprintf(&sb, "Total wall time: %s\n\n", elapsed.Round(time.Second))
	fmt.Fprintf(&sb, "| Renders | RSS (MB) | Delta vs baseline (MB) | Last render |\n")
	fmt.Fprintf(&sb, "|---|---|---|---|\n")
	for _, s := range samples {
		lat := "-"
		if s.latency > 0 {
			lat = s.latency.Round(time.Millisecond).String()
		}
		fmt.Fprintf(&sb, "| %d | %.1f | %+.1f | %s |\n", s.render, s.rssMB, s.rssMB-baseline, lat)
	}
	sb.WriteString("\n")

	final := samples[len(samples)-1]
	growth := final.rssMB - baseline
	perRender := growth / float64(renders)
	fmt.Fprintf(&sb, "**Total growth over %d renders: %+.1f MB (%.4f MB/render).**\n\n", renders, growth, perRender)

	fmt.Fprintf(&sb, "## Conclusion\n\n")
	fmt.Fprintf(&sb, "_Written by hand from the numbers above — deliberately not generated, so the\n")
	fmt.Fprintf(&sb, "interpretation is a judgement someone made and can be argued with._\n")

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	outPath := filepath.Join(repoRoot, "docs", "research", "soak-test-results.md")
	if err := os.WriteFile(outPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", outPath, err)
	}
	t.Logf("soak report written to %s", outPath)
}
