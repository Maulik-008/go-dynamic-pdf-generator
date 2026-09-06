// Package overlay renders a caller-supplied HTML fragment on a transparent,
// full-page canvas and stamps it onto selected pages of an
// already-rendered PDF — the "put this box on the last page only" job that
// Chromium's own headerTemplate/footerTemplate can't do, because those
// repeat on every page.
//
// It is the first, deliberately narrow use of the direct-construction
// "fast path" that CAPABILITY-MAP.md assigns to render-engines: the stamp
// itself is a pure pdfcpu operation (no browser), while the fragment is
// rendered through the same Chromium path as any other document so its CSS
// behaves identically. Full text/image watermarking (all pages, opacity,
// rotation, positioning grammar) stays the separate deferred
// customization-layer slice; this package does one composited overlay.
package overlay

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// totalPagesToken is swapped for the main document's final page count in
// Spec.HTML before the fragment is rendered. A single documented literal
// substring replacement, not a template engine: a fragment containing any
// other brace sequence is passed through byte-for-byte, and there is no
// missing-key or escaping behavior to reason about.
const totalPagesToken = "{{totalPages}}"

// FragmentRenderer renders a standalone HTML fragment to PDF bytes.
// Satisfied by *renderengines.Pool and *renderengines.Renderer directly;
// the api package wraps one in an admission-gated adapter so a fragment
// render is subject to the same backpressure as every other Chromium
// render.
type FragmentRenderer interface {
	RenderHTML(ctx context.Context, html string, opts renderengines.RenderOptions) ([]byte, error)
}

// Spec is a validated overlay request. HTML is the fragment to render;
// Pages names the 1-based pages of the main document to stamp it onto —
// "" and "last" mean the final page, "first" page 1, "all" every page, or
// an explicit list like "3", "3-5", "2,4".
type Spec struct {
	HTML  string
	Pages string
}

// Apply renders spec.HTML — with {{totalPages}} resolved against mainPDF's
// page count — on a transparent page the exact size of mainPDF's pages,
// then stamps that page onto every page named by spec.Pages. On any error
// it returns nil, so a caller must keep its own reference to mainPDF.
func Apply(ctx context.Context, r FragmentRenderer, mainPDF []byte, spec Spec, base renderengines.RenderOptions) ([]byte, error) {
	count, err := pageCount(mainPDF)
	if err != nil {
		return nil, fmt.Errorf("overlay: count main pages: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("overlay: main document has no pages")
	}

	pages, err := resolvePages(spec.Pages, count)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}

	html := strings.ReplaceAll(spec.HTML, totalPagesToken, strconv.Itoa(count))

	stampPDF, err := r.RenderHTML(ctx, html, fragmentOptions(base))
	if err != nil {
		// Wrapped, not replaced: orchestration.ErrQueueFull /
		// renderengines.ErrInstanceUnavailable stay detectable by
		// errors.Is at the HTTP boundary so a fragment render that hits
		// backpressure still maps to 503, not 422.
		return nil, fmt.Errorf("overlay: render fragment: %w", err)
	}

	out, err := stampPages(mainPDF, stampPDF, pages)
	if err != nil {
		return nil, fmt.Errorf("overlay: stamp: %w", err)
	}
	return out, nil
}

// fragmentOptions renders the fragment edge-to-edge on a page matching the
// main document: same paper size and orientation, zero margins, native
// scale, no Chromium header/footer. The fragment places itself with its
// own CSS (typically position:fixed; bottom:0), and any area it doesn't
// paint stays transparent — so stampPages composites only its visible box
// onto the target page, exactly like pdf-lib's page.drawPage(fp, {x:0,
// y:0, width, height}).
func fragmentOptions(base renderengines.RenderOptions) renderengines.RenderOptions {
	o := base
	o.MarginTop, o.MarginBottom, o.MarginLeft, o.MarginRight = 0, 0, 0, 0
	o.Scale = 1
	o.DisplayHeaderFooter = false
	o.HeaderTemplate, o.FooterTemplate = "", ""
	o.PrintBackground = true
	return o
}

// ValidatePages reports whether sel is a syntactically valid page selector
// — "last" (also ""), "first", "all", or a comma-separated list of single
// pages and ascending "lo-hi" ranges ("3", "3-5", "2,4"). It does not
// check the pages against a real document; Apply does that once the page
// count is known. Exposed so the HTTP layer can reject a malformed
// selector with a 400 before any render work starts.
func ValidatePages(sel string) error {
	_, _, err := parseSelector(sel)
	return err
}

// parseSelector normalizes and grammar-checks a page selector. It returns
// a friendly keyword ("last"/"first"/"all") when sel is one, otherwise the
// explicit [lo, hi] ranges (already checked to be ascending, positive).
// Bounds against a real page count are not applied here.
func parseSelector(sel string) (keyword string, ranges [][2]int, err error) {
	switch strings.ToLower(strings.TrimSpace(sel)) {
	case "", "last":
		return "last", nil, nil
	case "first":
		return "first", nil, nil
	case "all":
		return "all", nil, nil
	}

	for _, part := range strings.Split(strings.TrimSpace(sel), ",") {
		part = strings.TrimSpace(part)
		lo, hi, perr := parseRange(part)
		if perr != nil {
			return "", nil, fmt.Errorf("pages %q: want \"last\", \"first\", \"all\", or a list like \"3\", \"3-5\", \"2,4\"", sel)
		}
		if lo < 1 || lo > hi {
			return "", nil, fmt.Errorf("pages %q: %q is not an ascending range of positive page numbers", sel, part)
		}
		ranges = append(ranges, [2]int{lo, hi})
	}
	return "", ranges, nil
}

// parseRange reads "N" as [N, N] and "N-M" as [N, M]. Any non-numeric part
// or overflow is an error.
func parseRange(part string) (lo, hi int, err error) {
	if dash := strings.IndexByte(part, '-'); dash >= 0 {
		if lo, err = strconv.Atoi(part[:dash]); err != nil {
			return 0, 0, err
		}
		if hi, err = strconv.Atoi(part[dash+1:]); err != nil {
			return 0, 0, err
		}
		return lo, hi, nil
	}
	n, err := strconv.Atoi(part)
	if err != nil {
		return 0, 0, err
	}
	return n, n, nil
}

// resolvePages turns the selector into the []string pdfcpu wants (nil =
// every page), checking each page against the real page count so an
// out-of-range request fails here with a clear message rather than
// silently stamping nothing.
func resolvePages(sel string, count int) ([]string, error) {
	keyword, ranges, err := parseSelector(sel)
	if err != nil {
		return nil, err
	}
	switch keyword {
	case "last":
		return []string{strconv.Itoa(count)}, nil
	case "first":
		return []string{"1"}, nil
	case "all":
		return nil, nil
	}

	out := make([]string, 0, len(ranges))
	for _, r := range ranges {
		lo, hi := r[0], r[1]
		if hi > count {
			return nil, fmt.Errorf("pages %q: page %d is past the document's %d page(s)", sel, hi, count)
		}
		if lo == hi {
			out = append(out, strconv.Itoa(lo))
		} else {
			out = append(out, fmt.Sprintf("%d-%d", lo, hi))
		}
	}
	return out, nil
}
