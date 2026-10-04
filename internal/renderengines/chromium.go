// Package renderengines implements the platform's fidelity-path renderer:
// HTML (and Markdown, compiled to HTML first) to PDF, driven over the Chrome
// DevTools Protocol against a warm, reused headless Chromium process — no
// Node, no per-request browser launch. See
// docs/planning/SPEC-render-engines.md.
package renderengines

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/observability"
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

	tr := observability.TraceFrom(ctx)
	var pdfBuf []byte
	err := chromedp.Run(taskCtx,
		tabOpened(tr),
		timed(tr, "navigate", chromedp.Navigate("about:blank")),
		timed(tr, "set_content", injectDocument(html)),
		timed(tr, "wait_ready", chromedp.WaitReady("body", chromedp.ByQuery)),
		waitForLoad(tr, opts),
		reserveOverlaySpace(tr, opts),
		timed(tr, "print_pdf", printToPDF(opts, &pdfBuf)),
	)
	if err != nil {
		return nil, fmt.Errorf("renderengines: render html: %w", err)
	}
	return pdfBuf, nil
}

// FittedRender is the result of a "fit to page" render: the PDF, plus the
// print scale that was actually applied to make the content fit on a single
// page and whether the content still overflowed even at MinFitScale.
type FittedRender struct {
	PDF []byte
	// Scale is 1.0 when the content already fit, or a value in
	// [MinFitScale, 1.0) when it had to be shrunk.
	Scale float64
	// Overflowed is true when the exact scale needed was below MinFitScale:
	// one page is still produced, at MinFitScale, so a caller can warn the
	// author rather than silently emitting an unreadable page.
	Overflowed bool
}

const (
	// MinFitScale is the readability floor for RenderHTMLFitted. Matches
	// pdf-service-saas's MIN_FIT_SCALE — an unclamped fit would happily
	// render a long legal document at 25% with no signal to anyone.
	MinFitScale = 0.6

	// cssPxPerInch converts the inch-based paper/margin figures in
	// RenderOptions to the CSS pixels document.body.scrollHeight is
	// reported in.
	cssPxPerInch = 96
)

// RenderHTMLFitted renders html to a single page, scaling the content down
// uniformly if its natural height exceeds the printable page height implied
// by opts (paper size minus top/bottom margins, orientation-aware).
//
// It is Chromium's own PrintToPDF `scale` that is used, not a CSS transform:
// a CSS `transform: scale()` is paint-only and never changes print
// pagination, whereas PrintToPDF scale changes how many CSS pixels fit on a
// physical page without reflowing text. This mirrors the approach proven in
// pdf-service-saas/services/pdfGenerator.js.
//
// The natural height is measured in the same tab, at a layout viewport equal
// to the print page width, so the measurement reflects the same wrapping the
// print will use.
func (r *Renderer) RenderHTMLFitted(ctx context.Context, html string, opts RenderOptions) (FittedRender, error) {
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

	// Orientation-aware page box. In landscape PrintToPDF rotates the paper,
	// so the long edge (PaperHeight) becomes the width and the short edge
	// (PaperWidth) the height; top/bottom margins always apply to the final
	// orientation.
	pageWidthIn, pageHeightIn := opts.PaperWidth, opts.PaperHeight
	if opts.Landscape {
		pageWidthIn, pageHeightIn = opts.PaperHeight, opts.PaperWidth
	}
	viewportWidthPx := int64(math.Round(pageWidthIn * cssPxPerInch))
	availableHeightPx := (pageHeightIn - opts.MarginTop - opts.MarginBottom) * cssPxPerInch

	tr := observability.TraceFrom(ctx)
	scale := 1.0
	overflowed := false
	var pdfBuf []byte
	err := chromedp.Run(taskCtx,
		tabOpened(tr),
		timed(tr, "viewport", chromedp.EmulateViewport(viewportWidthPx, 1000)),
		timed(tr, "navigate", chromedp.Navigate("about:blank")),
		timed(tr, "set_content", injectDocument(html)),
		timed(tr, "wait_ready", chromedp.WaitReady("body", chromedp.ByQuery)),
		waitForLoad(tr, opts),
		reserveOverlaySpace(tr, opts),
		chromedp.ActionFunc(func(ctx context.Context) error {
			stopMeasure := tr.Start("measure")
			var natural float64
			err := chromedp.Evaluate(naturalHeightJS, &natural).Do(ctx)
			stopMeasure()
			if err != nil {
				return fmt.Errorf("measure content height: %w", err)
			}
			if availableHeightPx > 0 && natural > availableHeightPx {
				exact := availableHeightPx / natural
				if exact < MinFitScale {
					scale, overflowed = MinFitScale, true
				} else {
					scale = exact
				}
			}
			fitted := opts
			fitted.Scale = scale
			defer tr.Start("print_pdf")()
			data, _, err := printToPDFParams(fitted).Do(ctx)
			if err != nil {
				return fmt.Errorf("print to pdf: %w", err)
			}
			pdfBuf = data
			return nil
		}),
	)
	if err != nil {
		return FittedRender{}, fmt.Errorf("renderengines: render html fitted: %w", err)
	}
	return FittedRender{PDF: pdfBuf, Scale: scale, Overflowed: overflowed}, nil
}

// naturalHeightJS reports the unscaled content height of the current
// document, taking the largest of the usual four measures so a document
// that sizes itself via <html> rather than <body> (or vice versa) is still
// measured correctly. Matches the measurement pdf-service-saas takes.
const naturalHeightJS = `Math.max(
	document.body.scrollHeight, document.body.offsetHeight,
	document.documentElement.scrollHeight, document.documentElement.offsetHeight
)`

// measureHeightTimeout bounds MeasureHTMLHeight — it has no opts.Timeout of
// its own (it's not a RenderOptions-shaped call), so a fixed, generous
// budget matching DefaultRenderOptions().Timeout is used instead.
const measureHeightTimeout = 45 * time.Second

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
		injectDocument(content),
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
func waitForLoad(tr *observability.Trace, opts RenderOptions) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if opts.WaitForFonts {
			stop := tr.Start("wait_fonts")
			err := chromedp.Evaluate(`document.fonts.ready`, nil,
				func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
					return p.WithAwaitPromise(true)
				}).Do(ctx)
			stop()
			if err != nil {
				return fmt.Errorf("wait for fonts: %w", err)
			}
		}
		if opts.WaitForImages {
			stop := tr.Start("wait_images")
			err := chromedp.Evaluate(imagesLoadedJS, nil,
				func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
					return p.WithAwaitPromise(true)
				}).Do(ctx)
			stop()
			if err != nil {
				return fmt.Errorf("wait for images: %w", err)
			}
		}
		return nil
	})
}

// reserveOverlayJS measures the bottom-anchored height of an overlay fragment
// in a hidden same-page iframe (so no extra tab is opened) and appends a
// spacer to the document if the fragment would reach further up the page than
// the print margin already keeps content away. Returns the spacer height in CSS
// px, 0 when nothing was needed or the measurement could not be made.
//
// Only elements touching the bottom edge of the fragment's page count: that is
// what "stamped at the bottom of the last page" means, and it stops an overlay
// that also draws something mid-page from reserving most of the sheet. Trailing
// whitespace the body already ends with (padding/border/margin) is credited,
// since the overlay may legitimately cover blank space.
const reserveOverlayJS = `async (html, vw, vh, marginPx) => {
	try {
		const frame = document.createElement('iframe');
		frame.setAttribute('aria-hidden', 'true');
		frame.style.cssText = 'position:fixed;left:-100000px;top:0;border:0;visibility:hidden;width:' + vw + 'px;height:' + vh + 'px';
		await new Promise((resolve) => {
			frame.onload = resolve;
			frame.srcdoc = html;
			document.body.appendChild(frame);
		});
		const doc = frame.contentDocument;
		if (doc.fonts && doc.fonts.ready) await doc.fonts.ready;
		let top = Infinity;
		for (const el of doc.body.querySelectorAll('*')) {
			const r = el.getBoundingClientRect();
			if (r.width > 0 && r.height > 0 && r.bottom >= vh - 2) top = Math.min(top, r.top);
		}
		frame.remove();
		if (!isFinite(top)) return 0;
		const cs = getComputedStyle(document.body);
		const trailing = (parseFloat(cs.paddingBottom) || 0) + (parseFloat(cs.borderBottomWidth) || 0) + (parseFloat(cs.marginBottom) || 0);
		const need = Math.ceil((vh - top) - marginPx - trailing);
		if (need <= 0) return 0;
		const spacer = document.createElement('div');
		spacer.setAttribute('data-overlay-reserve', '');
		spacer.style.cssText = 'height:' + need + 'px;margin:0;padding:0;border:0;';
		document.body.appendChild(spacer);
		return need;
	} catch (e) {
		return 0;
	}
}`

// reserveOverlaySpace runs reserveOverlayJS when opts.OverlayReserveHTML is
// set. Best effort by design: if the measurement fails the document renders
// exactly as it did before this existed, rather than failing the request.
func reserveOverlaySpace(tr *observability.Trace, opts RenderOptions) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if opts.OverlayReserveHTML == "" {
			return nil
		}
		defer tr.Start("reserve_overlay")()

		w, h := opts.PaperWidth, opts.PaperHeight
		if opts.Landscape {
			w, h = h, w
		}
		htmlJSON, err := json.Marshal(opts.OverlayReserveHTML)
		if err != nil {
			return nil
		}
		expr := fmt.Sprintf("(%s)(%s, %d, %d, %f)", reserveOverlayJS, htmlJSON,
			int(math.Round(w*cssPxPerInch)), int(math.Round(h*cssPxPerInch)), opts.MarginBottom*cssPxPerInch)

		var reserved float64
		err = chromedp.Evaluate(expr, &reserved, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}).Do(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			return nil
		}
		if reserved > 0 {
			tr.Update(func(i *observability.RenderInfo) { i.OverlayReservedPx = int(reserved) })
		}
		return nil
	})
}

// timed records how long a has to run under phase on tr. A nil tr makes it a
// pass-through, so untraced callers pay nothing.
func timed(tr *observability.Trace, phase string, a chromedp.Action) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		defer tr.Start(phase)()
		return a.Do(ctx)
	})
}

// tabOpened records the "tab_open" phase: chromedp allocates the new tab
// lazily inside Run, before its first action executes, so the time from here
// to that first action is the cost of opening a tab on the warm browser.
// Must be the first action passed to a Run, with this call made immediately
// before it.
func tabOpened(tr *observability.Trace) chromedp.Action {
	begin := time.Now()
	return chromedp.ActionFunc(func(context.Context) error {
		tr.Add("tab_open", time.Since(begin))
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

// injectDocument replaces the current about:blank document with html. Used
// instead of navigating to a data: URL so an arbitrarily large document is
// not subject to URL-length limits. Shared by every render/measure path so
// they all inject content the same way.
func injectDocument(html string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		frameTree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return fmt.Errorf("get frame tree: %w", err)
		}
		if err := page.SetDocumentContent(frameTree.Frame.ID, html).Do(ctx); err != nil {
			return fmt.Errorf("set document content: %w", err)
		}
		return nil
	})
}

// printToPDFParams builds the CDP PrintToPDF parameters from opts. Split out
// from printToPDF so RenderHTMLFitted can invoke the print step directly
// after computing its scale, without a second Chromium round trip.
func printToPDFParams(opts RenderOptions) *page.PrintToPDFParams {
	return page.PrintToPDF().
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
}

func printToPDF(opts RenderOptions, out *[]byte) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		data, _, err := printToPDFParams(opts).Do(ctx)
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
