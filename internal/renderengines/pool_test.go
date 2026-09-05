package renderengines

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func testPool(t *testing.T, size int) *Pool {
	t.Helper()
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium test")
	}
	p, err := NewPool(PoolConfig{
		Config:                    Config{ChromiumPath: path},
		Size:                      size,
		MaxConcurrencyPerInstance: 6,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// countProcs counts running Chromium processes — used to assert the pool
// reuses browser processes rather than spawning one per render, which is
// the entire point of this module (see chromium.go's doc comment on the
// spawn-per-request anti-pattern it was written to avoid).
//
// It matches each process's real executable (/proc/<pid>/exe) rather than
// its command line. The previous `pgrep -f headless_shell` approach counted
// any process whose *command line* merely mentioned the binary — including
// the shell running the test suite, which carries CHROMIUM_PATH on its own
// command line. Measured directly: with zero Chromium actually running,
// `pgrep -f headless_shell` still reported 2 matches. That inflated,
// neighbour-sensitive count is what made TestPool_ClosesWithoutOrphaned-
// Processes intermittently fail with "before=38, during=38" while passing
// in isolation — a false alarm about the code under test, caused purely by
// the measurement.
//
// The name parameter is retained for call-site readability; matching is by
// executable path (CHROMIUM_PATH), which is what "a Chromium process"
// actually means here.
func countProcs(t *testing.T, _ string) int {
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
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		if exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err == nil && exe == chromiumPath {
			n++
		}
	}
	return n
}

func TestPool_ReusesProcessesAcrossRenders(t *testing.T) {
	p := testPool(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := p.RenderHTML(ctx, "<html><body>one</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("first render: %v", err)
	}
	after1 := countProcs(t, "headless_shell")

	for i := 0; i < 5; i++ {
		if _, err := p.RenderHTML(ctx, fmt.Sprintf("<html><body>%d</body></html>", i), DefaultRenderOptions()); err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
	}
	after6 := countProcs(t, "headless_shell")

	if after6 > after1+1 { // small slack for transient renderer subprocess timing
		t.Fatalf("process count grew from %d to %d across 5 more renders on a 2-instance pool; expected reuse, not growth", after1, after6)
	}
}

func TestPool_ConcurrentRendersAreSafe(t *testing.T) {
	p := testPool(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("marker-%d", i)
			pdf, err := p.RenderHTML(ctx, fmt.Sprintf("<html><body>%s</body></html>", marker), DefaultRenderOptions())
			if err != nil {
				errs <- fmt.Errorf("render %d: %w", i, err)
				return
			}
			if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
				errs <- fmt.Errorf("render %d: output missing %%PDF- prefix", i)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestPool_ClosesWithoutOrphanedProcesses(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium test")
	}
	before := countProcs(t, "headless_shell")

	p, err := NewPool(PoolConfig{Config: Config{ChromiumPath: path}, Size: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := p.RenderHTML(ctx, "<html><body>x</body></html>", DefaultRenderOptions()); err != nil {
		t.Fatalf("render: %v", err)
	}
	duringCount := countProcs(t, "headless_shell")
	if duringCount <= before {
		t.Fatalf("expected chromium processes to start (before=%d, during=%d)", before, duringCount)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// give the OS a moment to actually reap the terminated processes
	deadline := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = countProcs(t, "headless_shell")
		if after <= before {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if after > before {
		t.Fatalf("processes leaked after Close: before=%d, after=%d", before, after)
	}
}

func TestPool_ContextCancellationDuringAcquireAborts(t *testing.T) {
	p := testPool(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.RenderHTML(ctx, "<html><body>x</body></html>", DefaultRenderOptions())
	if err == nil {
		t.Fatal("expected an error with an already-cancelled context, got nil")
	}
}

func TestPool_MeasureHTMLHeight(t *testing.T) {
	p := testPool(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	height, err := p.MeasureHTMLHeight(ctx, `<div style="height:250px">content</div>`, 800)
	if err != nil {
		t.Fatalf("MeasureHTMLHeight: %v", err)
	}
	if height < 200 || height > 350 {
		t.Fatalf("measured height %.1fpx for a 250px-tall div is out of the expected ballpark", height)
	}
}
