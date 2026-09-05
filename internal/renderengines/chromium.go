// Package renderengines implements the platform's fidelity-path renderer:
// HTML (and Markdown, compiled to HTML first) to PDF, driven over the Chrome
// DevTools Protocol against a warm, reused headless Chromium process — no
// Node, no per-request browser launch. See
// docs/planning/SPEC-render-engines.md.
package renderengines

import (
	"context"
	"fmt"
	"os"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Config configures a Renderer.
type Config struct {
	// ChromiumPath is the path to the headless Chromium (or
	// chrome-headless-shell) binary. If empty, the CHROMIUM_PATH
	// environment variable is used.
	//
	// This Renderer runs Chromium with its own OS-level sandbox disabled
	// (--no-sandbox — required in most container environments, since the
	// setuid sandbox needs privileges containers don't grant), and it
	// executes arbitrary customer-supplied HTML/JS via RenderHTML. Once
	// this is deployed, the container/pod boundary is the only real
	// isolation layer — Chromium's own sandbox isn't providing one. Run
	// each instance in its own properly isolated container, never sharing
	// one with another tenant's process.
	ChromiumPath string
}

// Renderer renders HTML/Markdown to PDF using a warm, reused headless
// Chromium process. Each render call gets its own CDP browser context (a
// fresh tab/incognito-equivalent) so requests can't see each other's page
// state, while the underlying browser process itself — the expensive part,
// 300-800ms to cold-start — is started once and reused across every call.
//
// This reuse depends on deriving each call's tab context from browserCtx
// (a context that already has a Browser attached), never from allocCtx
// directly — chromedp only creates a new tab on an existing browser when the
// parent context it's given already carries one; handed the bare allocator
// instead, it allocates a brand new browser process per call, which is
// exactly the spawn-per-request anti-pattern this type exists to avoid.
type Renderer struct {
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
}

// NewRenderer starts a Chromium process immediately (not lazily on first
// request) so the warm-pool cost is paid once at startup, not by whichever
// caller happens to arrive first. Call Close when done.
func NewRenderer(cfg Config) (*Renderer, error) {
	path := cfg.ChromiumPath
	if path == "" {
		path = os.Getenv("CHROMIUM_PATH")
	}
	if path == "" {
		return nil, fmt.Errorf("renderengines: no chromium path configured (set Config.ChromiumPath or CHROMIUM_PATH)")
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.NoSandbox, // required in most container environments
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(browserCtx); err != nil { // forces the browser to start now
		browserCancel()
		allocCancel()
		return nil, fmt.Errorf("renderengines: starting chromium: %w", err)
	}

	return &Renderer{
		allocCtx:      allocCtx,
		allocCancel:   allocCancel,
		browserCtx:    browserCtx,
		browserCancel: browserCancel,
	}, nil
}

// probe issues the smallest possible browser-level CDP round trip and
// reports whether the browser's message loop is actually turning.
//
// This is the *hang* detector, and it exists because alive() provably
// cannot see a hung browser. Measured by freezing a live instance with
// SIGSTOP — process still present, websocket still open, message loop
// stopped: alive() kept reporting true while this probe correctly timed
// out, and a real render hung until its own timeout fired.
//
// Two details are load-bearing rather than stylistic:
//
//   - It runs against the *existing* browser context. Allocating a fresh
//     tab to ask a health question would cost a real target and could
//     itself hang on exactly the browser being diagnosed.
//   - It issues a real CDP command. A no-op action is not a probe: measured
//     directly, chromedp.Run with an empty ActionFunc returns nil against a
//     genuinely dead browser, because it never talks to the browser at all.
//
// Costs ~1ms against a healthy browser.
func (r *Renderer) probe(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, _, err := cdpbrowser.GetVersion().Do(ctx)
		return err
	}))
}

// alive reports whether this Renderer's browser is still usable.
//
// chromedp cancels browserCtx when the underlying browser process goes away
// — verified directly by hard-killing a live headless_shell and observing
// browserCtx.Err() become context.Canceled while allocCtx stayed nil. That
// makes death a free, event-driven signal: no polling loop and no CDP round
// trip are needed to detect it.
//
// Note that a no-op chromedp action is NOT a valid liveness check — it
// returns nil against a dead browser because it never talks to the browser
// at all. Any *active* probe must issue a real CDP command; this passive
// context check avoids needing one.
func (r *Renderer) alive() bool {
	return r.browserCtx.Err() == nil
}

// RenderHTML renders the given HTML string to PDF bytes. ctx bounds
// cancellation from the caller's side (e.g. an HTTP request context);
// opts.Timeout additionally bounds the render itself.
//
// Relative resource URLs (e.g. <img src="logo.png">, <link href="style.css">)
// cannot resolve on their own: the document is injected via
// page.SetDocumentContent after navigating to "about:blank", which has no
// path structure to resolve a relative reference against — the browser
// leaves it as the raw, unfetchable literal rather than erroring. The render
// still succeeds; that resource is just silently absent from the output. So
// callers must do one of:
//   - use absolute URLs for every external reference,
//   - inline resources as data: URIs, or
//   - include a <base href="https://example.com/..."> tag in the submitted
//     HTML, which makes relative references resolve exactly as they would
//     on a real page (proven by TestRenderHTML_BaseHrefResolvesRelativeURLs).
func (r *Renderer) RenderHTML(ctx context.Context, html string, opts RenderOptions) ([]byte, error) {
	opts = opts.withDefaults()

	taskCtx, taskCancel := chromedp.NewContext(r.browserCtx)
	defer taskCancel()

	taskCtx, timeoutCancel := context.WithTimeout(taskCtx, opts.Timeout)
	defer timeoutCancel()
	// also honor the caller's own context, independent of opts.Timeout
	taskCtx, callerCancel := context.WithCancel(taskCtx)
	defer callerCancel()
	go func() {
		select {
		case <-ctx.Done():
			callerCancel()
		case <-taskCtx.Done():
		}
	}()

	var pdfBuf []byte
	err := chromedp.Run(taskCtx,
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return fmt.Errorf("get frame tree: %w", err)
			}
			if err := page.SetDocumentContent(frameTree.Frame.ID, html).Do(ctx); err != nil {
				return fmt.Errorf("set document content: %w", err)
			}
			return nil
		}),
		chromedp.WaitReady("body", chromedp.ByQuery),
		waitForLoad(opts),
		printToPDF(opts, &pdfBuf),
	)
	if err != nil {
		return nil, fmt.Errorf("renderengines: render html: %w", err)
	}
	return pdfBuf, nil
}

// measureHeightTimeout bounds MeasureHTMLHeight — it has no opts.Timeout of
// its own (it's not a RenderOptions-shaped call), so a fixed, generous
// budget matching DefaultRenderOptions().Timeout is used instead.
const measureHeightTimeout = 30 * time.Second

// MeasureHTMLHeight renders htmlFragment (treated as body content — a
// snippet, not a full document, matching Chromium's own header/footer
// template convention) in a plain (non-print) page at the given viewport
// width, and returns the rendered content's height in CSS pixels
// (document.body.scrollHeight, after waiting for document.fonts.ready so
// text metrics are accurate) — used by
// customization.ValidateHeaderFooterHeight to measure a header/footer
// template's real rendered height before an actual PDF render, per
// docs/planning/SPEC-customization-layer.md's auto-height-validation slice.
//
// The fragment is wrapped in a minimal `<!DOCTYPE html>` document before
// injection — not optional formatting, a correctness requirement found via
// a real, reproduced bug: injecting a bare fragment via
// page.SetDocumentContent (no doctype) puts the document in quirks mode
// (document.compatMode == "BackCompat"), and in quirks mode `body` stretches
// to fill the viewport, so document.body.scrollHeight reports the viewport
// height rather than the content's actual height regardless of how tall
// the content is — confirmed directly (a 20px div and a 400px div both
// measured as exactly the viewport height) before this fix, and confirmed
// fixed (the 400px div measures ~400px) after wrapping in a doctype forces
// standards mode (compatMode == "CSS1Compat").
//
// This is still a best-effort approximation of Chromium's internal print
// margin-box rendering, not a byte-for-byte replica of it: Chromium's
// PrintToPDF pipeline lays out headerTemplate/footerTemplate through its
// own internal pass, which isn't exposed for direct introspection over
// CDP. Measuring the same content in a normal, standards-mode page load at
// the configured paper width is the same workaround the wider
// Puppeteer/Chromium community uses for this exact problem, for lack of a
// direct API — a documented, deliberate limitation, not an oversight.
func (r *Renderer) MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error) {
	taskCtx, taskCancel := chromedp.NewContext(r.browserCtx)
	defer taskCancel()

	taskCtx, timeoutCancel := context.WithTimeout(taskCtx, measureHeightTimeout)
	defer timeoutCancel()
	// also honor the caller's own context, independent of measureHeightTimeout
	taskCtx, callerCancel := context.WithCancel(taskCtx)
	defer callerCancel()
	go func() {
		select {
		case <-ctx.Done():
			callerCancel()
		case <-taskCtx.Done():
		}
	}()

	content := "<!DOCTYPE html><html><head><meta charset=\"utf-8\"></head><body>" + htmlFragment + "</body></html>"

	var height float64
	err := chromedp.Run(taskCtx,
		chromedp.EmulateViewport(viewportWidthPx, 1000),
		chromedp.Navigate("about:blank"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			frameTree, err := page.GetFrameTree().Do(ctx)
			if err != nil {
				return fmt.Errorf("get frame tree: %w", err)
			}
			if err := page.SetDocumentContent(frameTree.Frame.ID, content).Do(ctx); err != nil {
				return fmt.Errorf("set document content: %w", err)
			}
			return nil
		}),
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Evaluate(`document.fonts.ready`, nil,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
				return p.WithAwaitPromise(true)
			}),
		chromedp.Evaluate(`document.body.scrollHeight`, &height),
	)
	if err != nil {
		return 0, fmt.Errorf("renderengines: measure html height: %w", err)
	}
	return height, nil
}

// waitForLoad waits for fonts and/or images to finish loading, per opts, so
// the PDF snapshot doesn't get taken before the page is actually ready — the
// most-cited accuracy bug across the engines surveyed in the research doc.
func waitForLoad(opts RenderOptions) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if opts.WaitForFonts {
			if err := chromedp.Evaluate(`document.fonts.ready`, nil,
				func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
					return p.WithAwaitPromise(true)
				}).Do(ctx); err != nil {
				return fmt.Errorf("wait for fonts: %w", err)
			}
		}
		if opts.WaitForImages {
			if err := chromedp.Evaluate(imagesLoadedJS, nil,
				func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
					return p.WithAwaitPromise(true)
				}).Do(ctx); err != nil {
				return fmt.Errorf("wait for images: %w", err)
			}
		}
		return nil
	})
}

// imagesLoadedJS resolves once every <img> on the page has finished loading
// (successfully or not) — a bounded, event-driven wait rather than a blind
// fixed delay.
const imagesLoadedJS = `new Promise((resolve) => {
	const imgs = Array.from(document.images);
	const pending = imgs.filter((img) => !img.complete);
	if (pending.length === 0) { resolve(true); return; }
	let remaining = pending.length;
	pending.forEach((img) => {
		const done = () => { remaining--; if (remaining <= 0) resolve(true); };
		img.addEventListener('load', done, { once: true });
		img.addEventListener('error', done, { once: true });
	});
})`

func printToPDF(opts RenderOptions, out *[]byte) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		params := page.PrintToPDF().
			WithLandscape(opts.Landscape).
			WithPrintBackground(opts.PrintBackground).
			WithScale(opts.Scale).
			WithPaperWidth(opts.PaperWidth).
			WithPaperHeight(opts.PaperHeight).
			WithMarginTop(opts.MarginTop).
			WithMarginBottom(opts.MarginBottom).
			WithMarginLeft(opts.MarginLeft).
			WithMarginRight(opts.MarginRight).
			WithDisplayHeaderFooter(opts.DisplayHeaderFooter).
			WithHeaderTemplate(opts.HeaderTemplate).
			WithFooterTemplate(opts.FooterTemplate)

		data, _, err := params.Do(ctx)
		if err != nil {
			return fmt.Errorf("print to pdf: %w", err)
		}
		*out = data
		return nil
	})
}

// Close terminates the underlying Chromium process. No further render calls
// should be made on this Renderer afterward.
//
// Precondition: callers must ensure no RenderHTML/RenderMarkdown call is
// still in flight when Close is called — it does not wait for one to finish,
// it aborts it. cmd/api's shutdown path gets this for free because
// http.Server.Shutdown blocks until in-flight handlers return before this
// Renderer's Close (deferred) ever runs; a caller wiring this up differently
// (e.g. a future job-orchestration module) must preserve an equivalent
// guarantee itself.
func (r *Renderer) Close() error {
	r.browserCancel()
	r.allocCancel()
	return nil
}
