package lightrender

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// testRenderer returns a Renderer configured against the WeasyPrint binary
// available in this environment (WEASYPRINT_PATH), skipping the test if
// none is configured — these tests run a real WeasyPrint subprocess, not a
// mock, matching internal/renderengines' own testing philosophy.
func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	path := os.Getenv("WEASYPRINT_PATH")
	if path == "" {
		t.Skip("WEASYPRINT_PATH not set; skipping real-WeasyPrint test")
	}
	r, err := NewRenderer(Config{WeasyPrintPath: path})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func TestRenderHTML_ProducesValidPDF(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pdf, err := r.RenderHTML(ctx, `<html><body><h1>Hello, static PDF</h1></body></html>`, RenderOptions{})
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- magic bytes")
	}
	if len(pdf) < 100 {
		t.Fatalf("output suspiciously small (%d bytes) for a rendered page", len(pdf))
	}
}

// TestRenderHTML_DoesNotExecuteJavaScript proves the one property that
// defines this rendering path — see the package doc and
// docs/planning/SPEC-lightweight-renderer.md. A script that, if executed,
// would document.write a distinctive marker string must leave zero trace
// in the rendered output. Uses RenderOptions.Uncompressed so the marker
// would be found via a direct substring search if it were ever drawn —
// comparing compressed PDF bytes for exact equality is NOT a reliable way
// to test this: two renders of identical *visible* content can still
// differ byte-for-byte because of incidental metadata (e.g. an embedded
// creation timestamp) cascading through DEFLATE-compressed streams, which
// is what an earlier version of this test actually hit and had to be
// fixed to avoid — a false alarm confirmed by direct comparison at the
// time, not assumed.
func TestRenderHTML_DoesNotExecuteJavaScript(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const marker = "MARKER_INJECTED_TEXT"
	html := `<html><body>
<p>Static paragraph only.</p>
<script>
  for (let i = 0; i < 200; i++) {
    document.write('<p>` + marker + `_' + i + '</p>');
  }
</script>
</body></html>`

	pdf, err := r.RenderHTML(ctx, html, RenderOptions{Uncompressed: true})
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- magic bytes")
	}
	if bytes.Contains(pdf, []byte(marker)) {
		t.Fatalf("found %q in the rendered PDF — the <script> appears to have executed via "+
			"document.write, which must never happen on this path", marker)
	}
}

func TestRenderHTML_ContextCancellationAborts(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call starts

	_, err := r.RenderHTML(ctx, `<html><body>should not render</body></html>`, RenderOptions{})
	if err == nil {
		t.Fatal("expected an error from RenderHTML with an already-cancelled context, got nil")
	}
}

// TestRenderHTML_OptsTimeoutBoundsRenderIndependentOfCtx proves the fix for
// a real gap found in review: without an internal default timeout,
// RenderHTML was bounded only by the caller's ctx, so a pathologically slow
// render would run forever against a caller ctx with no deadline (a normal
// HTTP request context has none until the client disconnects). That matters
// specifically because internal/orchestration gates this path through a
// small, fixed-size worker pool — one render that never returns would pin a
// worker slot permanently. Uses context.Background() (no deadline at all)
// with a deliberately tiny opts.Timeout to prove the render is still bounded.
func TestRenderHTML_OptsTimeoutBoundsRenderIndependentOfCtx(t *testing.T) {
	r := testRenderer(t)

	start := time.Now()
	_, err := r.RenderHTML(context.Background(), `<html><body>hi</body></html>`, RenderOptions{Timeout: 1 * time.Nanosecond})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a 1ns render timeout, got nil")
	}
	const generousBound = 5 * time.Second // real WeasyPrint startup alone is ~500ms; this only guards against "never returns"
	if elapsed > generousBound {
		t.Fatalf("RenderHTML took %s to fail with an expired opts.Timeout and no ctx deadline — "+
			"want well under %s (it must not depend on the caller's ctx to bound the render)", elapsed, generousBound)
	}
}

func TestRenderHTML_InvalidWeasyPrintPathErrors(t *testing.T) {
	if os.Getenv("WEASYPRINT_PATH") == "" {
		t.Skip("WEASYPRINT_PATH not set; skipping")
	}
	r, err := NewRenderer(Config{WeasyPrintPath: "/nonexistent/weasyprint-binary"})
	if err != nil {
		t.Fatalf("NewRenderer should not fail until the subprocess actually runs: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.RenderHTML(ctx, `<html></html>`, RenderOptions{}); err == nil {
		t.Fatal("expected an error rendering with a nonexistent weasyprint binary, got nil")
	}
}

func TestNewRenderer_NoPathConfigured(t *testing.T) {
	t.Setenv("WEASYPRINT_PATH", "")
	if _, err := NewRenderer(Config{}); err == nil {
		t.Fatal("expected an error when no WeasyPrint path is configured, got nil")
	}
}
