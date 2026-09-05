// Package lightrender implements a separate, explicitly opt-in static
// HTML/CSS -> PDF path via a WeasyPrint subprocess (no JavaScript
// execution) — deliberately independent of internal/renderengines
// (Chromium). Never auto-selected, never a silent substitute for the
// Chromium path; see docs/planning/SPEC-lightweight-renderer.md for the
// accuracy/performance trade-offs a caller is accepting by using this.
package lightrender

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultTimeout bounds a render when RenderOptions.Timeout is unset. Matches
// renderengines.DefaultRenderOptions()'s own 30s default for consistency.
// Load-bearing, not just a nicety: RenderHTML has no page-load/JS-execution
// phase to wait on the way the Chromium path does, but a pathological
// CSS/HTML input (e.g. an expensive layout on a deeply nested table) can
// still make WeasyPrint run for a very long time. Without this, such a
// render would be bounded only by the caller's ctx — fine for an HTTP
// request a client eventually gives up on, but internal/orchestration gates
// this path through a small, fixed-size worker pool (see
// docs/planning/SPEC-job-orchestration.md), and a render that never returns
// would pin one of those few workers forever, permanently shrinking the
// pool's real capacity one hung request at a time.
const defaultTimeout = 30 * time.Second

// Config configures a Renderer.
type Config struct {
	// WeasyPrintPath is the path to the weasyprint executable (or a bare
	// name resolvable via PATH). If empty, the WEASYPRINT_PATH environment
	// variable is used.
	WeasyPrintPath string
}

// RenderOptions controls how a document is rendered.
type RenderOptions struct {
	// BaseURL resolves relative resource references (images, stylesheets)
	// in the submitted HTML, exactly like a <base href> tag — see
	// weasyprint's own --base-url flag. Optional; if empty, relative
	// references won't resolve, the same limitation documented for the
	// Chromium path in renderengines.RenderHTML.
	BaseURL string

	// Uncompressed skips PDF stream compression (weasyprint's own
	// --uncompressed-pdf flag). Mainly useful for tests/debugging that need
	// to inspect or substring-search the raw content stream; production
	// callers generally want compression on (the default, false).
	Uncompressed bool

	// Timeout bounds the whole render call (the WeasyPrint subprocess's
	// entire run), independent of and in addition to ctx. Zero (the Go zero
	// value, e.g. an unmodified RenderOptions{}) means defaultTimeout, not
	// "no timeout" — see defaultTimeout's doc comment for why this is
	// enforced unconditionally rather than left to the caller's ctx alone.
	Timeout time.Duration
}

// Renderer renders static HTML/CSS to PDF via a WeasyPrint subprocess, one
// per call — no warm pool (unlike renderengines.Pool). WeasyPrint's cost
// profile is different from Chromium's: ~500ms of Python-interpreter-plus-
// import startup per render (measured directly against this environment's
// installed weasyprint 69.0), not a multi-hundred-ms browser cold start
// amortized across many calls by staying resident. Pooling long-lived
// Python workers is a documented future optimization, not built
// speculatively ahead of real usage — see SPEC-lightweight-renderer.md.
type Renderer struct {
	weasyPrintPath string
}

// NewRenderer validates that a WeasyPrint executable path is configured.
// It does not verify the binary actually runs — that's proven on the
// first real RenderHTML call, same as renderengines.Renderer's lazy
// process-start philosophy, just without a persistent process to keep warm.
func NewRenderer(cfg Config) (*Renderer, error) {
	path := cfg.WeasyPrintPath
	if path == "" {
		path = os.Getenv("WEASYPRINT_PATH")
	}
	if path == "" {
		return nil, fmt.Errorf("lightrender: no weasyprint path configured (set Config.WeasyPrintPath or WEASYPRINT_PATH)")
	}
	return &Renderer{weasyPrintPath: path}, nil
}

// RenderHTML renders the given static HTML string to PDF bytes via a
// WeasyPrint subprocess. JavaScript in the input is never executed — this
// is the defining, permanent property of this path, not a current
// limitation to be lifted later (see package doc and
// SPEC-lightweight-renderer.md). ctx bounds the subprocess's lifetime;
// cancelling it kills the process (exec.CommandContext's own behavior).
// opts.Timeout (defaultTimeout if unset) additionally bounds the render
// regardless of ctx, so a pathological input can't hang indefinitely.
func (r *Renderer) RenderHTML(ctx context.Context, html string, opts RenderOptions) ([]byte, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := make([]string, 0, 5)
	if opts.BaseURL != "" {
		args = append(args, "--base-url", opts.BaseURL)
	}
	if opts.Uncompressed {
		args = append(args, "--uncompressed-pdf")
	}
	args = append(args, "-", "-") // stdin -> stdout

	cmd := exec.CommandContext(ctx, r.weasyPrintPath, args...)
	cmd.Stdin = strings.NewReader(html)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("lightrender: weasyprint render failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
