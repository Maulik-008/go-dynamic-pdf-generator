// Command api runs the minimal HTTP server for the render-engines Phase 1
// slice (HTML/Markdown -> PDF only). See docs/planning/SPEC-render-engines.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Maulik-zuru/great-pdf-generator/internal/api"
	"github.com/Maulik-zuru/great-pdf-generator/internal/lightrender"
	"github.com/Maulik-zuru/great-pdf-generator/internal/observability"
	"github.com/Maulik-zuru/great-pdf-generator/internal/orchestration"
	"github.com/Maulik-zuru/great-pdf-generator/internal/renderengines"
)

// healthcheckFlag runs the process as a one-shot health probe instead of a
// server. It exists so container images can declare a HEALTHCHECK without
// shipping curl or wget — the binary probes itself and exits 0 (healthy) or
// 1 (not), keeping the runtime image free of extra tooling around a service
// that executes customer-supplied HTML.
var healthcheckFlag = flag.Bool("healthcheck", false,
	"probe this service's own liveness endpoint, print the result, and exit 0 (healthy) or 1")

// runHealthcheck probes /livez on the local server. It deliberately targets
// liveness, not readiness: a container supervisor reacting to a failed probe
// restarts the process, and readiness legitimately reports failure while
// draining or while the Chromium pool repairs itself — restarting on either
// would destroy recovery that was already underway.
func runHealthcheck() int {
	url := "http://127.0.0.1" + addr() + "/livez"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	fmt.Printf("healthcheck: %s ok\n", url)
	return 0
}

func main() {
	flag.Parse()
	if *healthcheckFlag {
		os.Exit(runHealthcheck())
	}

	// Install the structured logger first, before anything can log. This
	// also redirects the standard log package into the same JSON handler
	// (verified directly), so every existing log.Printf/log.Fatalf below
	// emits structured output without being rewritten.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(logger)

	chromiumPath := os.Getenv("CHROMIUM_PATH")
	if chromiumPath == "" {
		log.Fatal("CHROMIUM_PATH must be set to a headless Chromium / chrome-headless-shell binary")
	}

	poolSize := envInt("POOL_SIZE", 2)
	maxConcurrency := envInt("MAX_CONCURRENCY_PER_INSTANCE", 6)

	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: chromiumPath},
		Size:                      poolSize,
		MaxConcurrencyPerInstance: maxConcurrency,
	})
	if err != nil {
		log.Fatalf("starting renderer pool: %v", err)
	}
	defer pool.Close()

	// job-orchestration: gates admission into the Chromium pool so
	// requests beyond its real concurrent-render capacity are rejected
	// fast (503 + Retry-After) instead of piling up unboundedly — see
	// docs/planning/SPEC-job-orchestration.md. Workers matches the pool's
	// own total concurrent-render capacity (measured in
	// docs/research/load-test-results.md), so this layer's admission
	// limit doesn't introduce a second, differently-tuned bottleneck.
	chromiumWorkers := poolSize * maxConcurrency
	chromiumJobPool := orchestration.NewPool(orchestration.PoolConfig{
		Workers:       chromiumWorkers,
		QueueCapacity: envInt("JOB_QUEUE_CAPACITY", chromiumWorkers),
	})
	defer chromiumJobPool.Close()

	// lightweight-render (WeasyPrint) is a separate, optional module — see
	// docs/planning/SPEC-lightweight-renderer.md. Deliberately built as an
	// explicit if/else assigning to srv, not a typed-nil *lightrender.Renderer
	// passed unconditionally: passing a nil *lightrender.Renderer through an
	// interface parameter produces a non-nil interface wrapping a nil pointer
	// (a classic Go trap), which would make the handler's `== nil` check
	// pass right through and panic on first use instead of returning 503.
	var srv *api.Server
	if weasyPrintPath := os.Getenv("WEASYPRINT_PATH"); weasyPrintPath != "" {
		staticRenderer, err := lightrender.NewRenderer(lightrender.Config{WeasyPrintPath: weasyPrintPath})
		if err != nil {
			log.Fatalf("starting lightweight renderer: %v", err)
		}
		// WeasyPrint has no concurrency bound of its own today (each call
		// spawns an unbounded subprocess) — this is its first-ever cap,
		// sized conservatively to available cores per the CPU-bound
		// finding in docs/research/resource-optimization-research.md.
		weasyWorkers := envInt("WEASYPRINT_WORKERS", runtime.NumCPU())
		weasyJobPool := orchestration.NewPool(orchestration.PoolConfig{
			Workers:       weasyWorkers,
			QueueCapacity: envInt("WEASYPRINT_QUEUE_CAPACITY", weasyWorkers),
		})
		defer weasyJobPool.Close()
		srv = api.NewServer(pool, staticRenderer, chromiumJobPool, weasyJobPool,
			api.WithRenderPoolHealth(pool))
		log.Printf("lightweight-render (WeasyPrint) enabled at %s (workers=%d)", weasyPrintPath, weasyWorkers)
	} else {
		srv = api.NewServer(pool, nil, chromiumJobPool, nil,
			api.WithRenderPoolHealth(pool))
		log.Print("lightweight-render (WeasyPrint) not configured (WEASYPRINT_PATH unset); /v1/pdf/html-lite will return 503")
	}
	httpServer := &http.Server{
		Addr:              addr(),
		Handler:           observability.RequestLogger(logger)(srv.Routes()),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout is a few seconds above the default render budget
		// (renderengines.DefaultRenderOptions().Timeout, currently 30s) so a
		// legitimate slow render isn't cut off mid-response, while still
		// bounding a slow-reading client from holding a handler goroutine
		// open indefinitely after the render itself has completed.
		WriteTimeout: 40 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (pool size=%d, max concurrency/instance=%d, job queue workers=%d capacity=%d)",
			httpServer.Addr, poolSize, maxConcurrency, chromiumWorkers, chromiumJobPool.QueueCapacity())
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// Fail readiness *before* draining, so a load balancer or orchestrator
	// stops routing new requests here while in-flight ones finish. Liveness
	// deliberately stays healthy throughout, so nothing SIGKILLs the process
	// mid-drain. The grace period gives probes time to observe the change
	// before the listener actually closes.
	srv.BeginShutdown()
	slog.Info("draining: readiness now failing", "grace", shutdownGrace.String())
	time.Sleep(shutdownGrace)

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("http server shutdown: %v", err)
	}
}

// shutdownGrace is how long readiness reports failure before the listener
// closes, giving load balancers a window to notice and stop sending traffic.
// Zero is a legitimate setting for local development, where waiting on every
// Ctrl+C is just friction.
var shutdownGrace = envDuration("SHUTDOWN_GRACE", 5*time.Second)

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warning: invalid value %q for %s, using default %s", v, key, def)
		return def
	}
	return d
}

// logLevel reads LOG_LEVEL (debug|info|warn|error), defaulting to info.
func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func addr() string {
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warning: invalid value %q for %s, using default %d", v, key, def)
		return def
	}
	return n
}
