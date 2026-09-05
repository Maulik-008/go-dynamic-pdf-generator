package renderengines

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// testRenderer returns a Renderer configured against the Chromium binary
// available in this environment (CHROMIUM_PATH), skipping the test if none
// is configured — these tests render against a real browser, not a mock,
// per docs/planning/SPEC-render-engines.md's Testing Strategy.
func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium test")
	}
	r, err := NewRenderer(Config{ChromiumPath: path})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRenderHTML_ProducesValidPDF(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pdf, err := r.RenderHTML(ctx, `<html><body><h1>Hello, PDF</h1></body></html>`, DefaultRenderOptions())
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- magic bytes, got first 20 bytes: %q", pdf[:min(20, len(pdf))])
	}
	if len(pdf) < 100 {
		t.Fatalf("output suspiciously small (%d bytes) for a rendered page", len(pdf))
	}
}

func TestRenderHTML_WaitsForFonts(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// document.fonts.ready must resolve even with zero custom fonts loaded
	// (the browser's own default font set) — this proves the wait-for-fonts
	// path doesn't hang on ordinary pages, not just pages with web fonts.
	opts := DefaultRenderOptions()
	opts.WaitForFonts = true
	pdf, err := r.RenderHTML(ctx, `<html><body>plain text, no custom fonts</body></html>`, opts)
	if err != nil {
		t.Fatalf("RenderHTML with WaitForFonts: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- magic bytes")
	}
}

func TestRenderHTML_ContextCancellationAborts(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call starts

	_, err := r.RenderHTML(ctx, `<html><body>should not render</body></html>`, DefaultRenderOptions())
	if err == nil {
		t.Fatal("expected an error from RenderHTML with an already-cancelled context, got nil")
	}
}

func TestRenderHTML_InvalidHTMLDoesNotPanic(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Deliberately malformed/incomplete markup — Chromium's HTML parser is
	// forgiving by design, so this should still render something valid
	// rather than error or panic.
	pdf, err := r.RenderHTML(ctx, `<html><body><div>unclosed`, DefaultRenderOptions())
	if err != nil {
		t.Fatalf("RenderHTML with malformed HTML: %v", err)
	}
	if !strings.HasPrefix(string(pdf[:min(5, len(pdf))]), "%PDF-") {
		t.Fatalf("output does not start with %%PDF- magic bytes")
	}
}

// resolvedImageSrc renders html and returns the browser-resolved (always
// absolute, or "about:blank"-relative if unresolvable) src of its first
// <img> — used to prove relative-URL resolution behavior directly, without
// needing a real network fetch to succeed.
func (r *Renderer) resolvedImageSrc(ctx context.Context, html string) (string, error) {
	taskCtx, cancel := chromedp.NewContext(r.browserCtx)
	defer cancel()

	var src string
	err := chromedp.Run(taskCtx,
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return err
			}
			return page.SetDocumentContent(frameTree.Frame.ID, html).Do(ctx)
		}),
		chromedp.WaitReady("img", chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector('img').src`, &src),
	)
	return src, err
}

func TestRenderHTML_BaseHrefResolvesRelativeURLs(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Without a <base href>, per RenderHTML's doc comment, "about:blank" has
	// no path structure to resolve a relative reference against, so the
	// browser leaves the resolved .src as the raw, unfetchable attribute
	// value — proving the documented limitation is real, not just asserted.
	src, err := r.resolvedImageSrc(ctx, `<html><body><img src="logo.png"></body></html>`)
	if err != nil {
		t.Fatalf("resolvedImageSrc (no base): %v", err)
	}
	if src != "logo.png" {
		t.Fatalf("expected the unresolved literal %q without <base href>, got %q", "logo.png", src)
	}

	// The documented workaround: a <base href> tag makes relative resources
	// resolve exactly as they would on a real page.
	src, err = r.resolvedImageSrc(ctx, `<html><head><base href="https://example.com/assets/"></head><body><img src="logo.png"></body></html>`)
	if err != nil {
		t.Fatalf("resolvedImageSrc (with base): %v", err)
	}
	if src != "https://example.com/assets/logo.png" {
		t.Fatalf("expected <base href> to resolve the relative src to an absolute URL, got %q", src)
	}
}

// TestMeasureHTMLHeight_ReflectsContentSize proves the measurement is real
// signal, not a constant/stub value: a deliberately taller block of content
// must measure taller than a short one, at the same viewport width — the
// property internal/customization's header/footer auto-height validation
// depends on entirely (see docs/planning/SPEC-customization-layer.md).
func TestMeasureHTMLHeight_ReflectsContentSize(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	short, err := r.MeasureHTMLHeight(ctx, `<div style="height:20px">short</div>`, 800)
	if err != nil {
		t.Fatalf("MeasureHTMLHeight (short): %v", err)
	}
	tall, err := r.MeasureHTMLHeight(ctx, `<div style="height:400px">tall</div>`, 800)
	if err != nil {
		t.Fatalf("MeasureHTMLHeight (tall): %v", err)
	}
	if tall <= short {
		t.Fatalf("tall content (%.1fpx) was not measured taller than short content (%.1fpx)", tall, short)
	}
	// A loose sanity bound, not an exact-pixel assertion (chrome adds body
	// margin/UA stylesheet defaults on top of the explicit div height) — the
	// point is the measurement is in the right ballpark, not a stub.
	if tall < 350 || tall > 500 {
		t.Fatalf("measured height %.1fpx for a 400px-tall div is out of the expected ballpark", tall)
	}
}

func TestMeasureHTMLHeight_ContextCancellationAborts(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call starts

	_, err := r.MeasureHTMLHeight(ctx, `<div>content</div>`, 800)
	if err == nil {
		t.Fatal("expected an error from MeasureHTMLHeight with an already-cancelled context, got nil")
	}
}

func TestRenderHTML_TimeoutDuringActiveRenderAbortsCleanly(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A page that overrides document.fonts.ready with a Promise that never
	// resolves, so waitForLoad's wait-for-fonts step hangs on its own until
	// something cuts it off — deliberately not network-based (an earlier
	// version of this test used a local httptest.Server that never
	// responds, but Chromium's Private Network Access policy blocks
	// about:blank-origin fetches to private addresses outright, which
	// fails fast rather than hanging and defeats the point of this test).
	// This exists to prove opts.Timeout is what bounds a render that's
	// genuinely in flight, not one that fails instantly — the only
	// previously-tested cancellation case was "already cancelled before
	// the call starts".
	html := `<html><head><script>
		Object.defineProperty(document, 'fonts', { value: { ready: new Promise(() => {}) }, configurable: true });
	</script></head><body>hang test</body></html>`

	opts := DefaultRenderOptions()
	opts.Timeout = 500 * time.Millisecond
	opts.WaitForFonts = true

	start := time.Now()
	_, err := r.RenderHTML(ctx, html, opts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when wait-for-fonts exceeds opts.Timeout, got nil")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("render took %s to return an error; expected it to abort close to the 500ms opts.Timeout, not hang", elapsed)
	}
}
