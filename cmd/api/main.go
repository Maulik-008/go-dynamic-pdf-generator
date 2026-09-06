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

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/api"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/assets"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/auth"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/lightrender"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/observability"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/orchestration"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
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

	// API-key auth (auth-and-tenancy v1). Fail closed: refuse to start with
	// no keys unless AUTH_DISABLED=true is set explicitly, so a deployment
	// can never be left open by forgetting an env var.
	authKeys := splitList(os.Getenv("API_KEYS"))
	if len(authKeys) == 0 && os.Getenv("AUTH_DISABLED") != "true" {
		log.Fatal("API_KEYS must be set (comma-separated) or AUTH_DISABLED=true for local development")
	}
	authn := auth.New(authKeys, "/livez", "/readyz", "/healthz")

	// Deployment-level render defaults (PDF_DEFAULT_*). Per-request options
	// override these; these override renderengines.DefaultRenderOptions.
	renderDefaults, err := renderDefaultsFromEnv()
	if err != nil {
		log.Fatalf("render defaults: %v", err)
	}

	serverOpts := []api.ServerOption{api.WithRenderDefaults(renderDefaults)}
	if os.Getenv("EMBED_IMAGES_ENABLED") == "true" {
		serverOpts = append(serverOpts, api.WithImageEmbedding(assets.Config{
			AllowPrivate:        os.Getenv("EMBED_IMAGES_ALLOW_PRIVATE") == "true",
			AllowedHostSuffixes: splitList(os.Getenv("EMBED_IMAGES_ALLOWED_HOSTS")),
		}))
		log.Print("image embedding enabled (options.embedImages honored)")
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
			append(serverOpts, api.WithRenderPoolHealth(pool))...)
		log.Printf("lightweight-render (WeasyPrint) enabled at %s (workers=%d)", weasyPrintPath, weasyWorkers)
	} else {
		srv = api.NewServer(pool, nil, chromiumJobPool, nil,
			append(serverOpts, api.WithRenderPoolHealth(pool))...)
		log.Print("lightweight-render (WeasyPrint) not configured (WEASYPRINT_PATH unset); /v1/pdf/html-lite will return 503")
	}

	if authn.Enabled() {
		log.Printf("api-key auth enabled (%d key(s) configured)", len(authKeys))
	} else {
		log.Print("WARNING: api-key auth DISABLED (AUTH_DISABLED=true) — never run this way outside local development")
	}

	httpServer := &http.Server{
		Addr: addr(),
		// Auth sits inside request logging so a rejected request is still
		// logged with its correlation id.
		Handler:           observability.RequestLogger(logger)(authn.Middleware(srv.Routes())),
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

// splitList parses a comma-separated env value into trimmed, non-empty
// entries. Used for API_KEYS and EMBED_IMAGES_ALLOWED_HOSTS.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderDefaultsFromEnv builds the deployment's base render options from the
// PDF_DEFAULT_* vars, starting from renderengines.DefaultRenderOptions. Only
// the vars that are set are applied; an invalid value is a startup error, not
// a silent fallback, so a typo in a deployment config is caught immediately.
func renderDefaultsFromEnv() (renderengines.RenderOptions, error) {
	d := renderengines.DefaultRenderOptions()

	if v := os.Getenv("PDF_DEFAULT_PAPER"); v != "" {
		w, h, ok := api.PaperSizeInches(v)
		if !ok {
			return d, fmt.Errorf("PDF_DEFAULT_PAPER: unknown paper size %q", v)
		}
		d.PaperWidth, d.PaperHeight = w, h
	}

	margins := []struct {
		key string
		dst *float64
	}{
		{"PDF_DEFAULT_MARGIN_TOP", &d.MarginTop},
		{"PDF_DEFAULT_MARGIN_RIGHT", &d.MarginRight},
		{"PDF_DEFAULT_MARGIN_BOTTOM", &d.MarginBottom},
		{"PDF_DEFAULT_MARGIN_LEFT", &d.MarginLeft},
	}
	for _, m := range margins {
		v := os.Getenv(m.key)
		if v == "" {
			continue
		}
		in, err := api.ParseDimension(v)
		if err != nil {
			return d, fmt.Errorf("%s: %w", m.key, err)
		}
		*m.dst = in
	}

	if v := os.Getenv("PDF_DEFAULT_LANDSCAPE"); v != "" {
		d.Landscape = v == "true"
	}
	if v := os.Getenv("PDF_DEFAULT_PRINT_BACKGROUND"); v != "" {
		d.PrintBackground = v == "true"
	}
	if v := os.Getenv("PDF_DEFAULT_DISPLAY_HEADER_FOOTER"); v != "" {
		d.DisplayHeaderFooter = v == "true"
	}
	if v := os.Getenv("PDF_DEFAULT_FOOTER_TEMPLATE"); v != "" {
		d.FooterTemplate = v
	}
	if v := os.Getenv("PDF_DEFAULT_HEADER_TEMPLATE"); v != "" {
		d.HeaderTemplate = v
	}
	return d, nil
}
