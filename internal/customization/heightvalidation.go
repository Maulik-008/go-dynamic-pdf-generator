package customization

import (
	"context"
	"fmt"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// pxPerInch is the reference DPI used to convert the CSS-pixel heights
// MeasureHTMLHeight returns into inches, comparable against
// RenderOptions.MarginTop/MarginBottom (already in inches) — 96 is the CSS
// spec's standard "reference pixel" density and matches Chromium's own
// internal print-margin conversion.
const pxPerInch = 96.0

// HeightMeasurer is satisfied by *renderengines.Pool (and
// *renderengines.Renderer) — kept minimal and interface-based, matching
// internal/api's pdfRenderer pattern, so unit tests don't need a real
// Chromium instance.
type HeightMeasurer interface {
	MeasureHTMLHeight(ctx context.Context, htmlFragment string, viewportWidthPx int64) (float64, error)
}

// HeightMismatchError reports that a header or footer template renders
// taller than the margin box it has to fit in — a structured,
// field-scoped error (naming the exact region and the numbers involved),
// not a bare rejection, per PLATFORM-SPEC.md's error-model requirement.
type HeightMismatchError struct {
	Region         string // "header" or "footer"
	MeasuredInches float64
	MarginInches   float64
}

func (e *HeightMismatchError) Error() string {
	return fmt.Sprintf(
		"customization: %s template renders %.2fin tall, taller than its %.2fin margin — it will overlap body content",
		e.Region, e.MeasuredInches, e.MarginInches,
	)
}

// ValidateHeaderFooterHeight measures opts.HeaderTemplate/opts.FooterTemplate
// (when opts.DisplayHeaderFooter is set and the respective template is
// non-empty) at opts.PaperWidth and returns a *HeightMismatchError if
// either would overflow its margin box (MarginTop for the header,
// MarginBottom for the footer) — the auto-height-validation fast-fail from
// PLATFORM-SPEC.md's Accuracy pillar, guarding against a chronic,
// unresolved bug class in Chromium/Puppeteer's own issue tracker (header
// or footer content silently overlapping body content when it doesn't fit
// its margin box). Call this before RenderHTML/RenderMarkdown to fail fast
// with a specific error instead of rendering the overlap.
//
// See docs/planning/SPEC-customization-layer.md for why this measures at
// full PaperWidth (a documented approximation of Chromium's internal print
// margin-box layout, not a byte-for-byte replica of it) and
// renderengines.Renderer.MeasureHTMLHeight's doc comment for the quirks-
// mode bug this measurement had to be fixed to avoid.
func ValidateHeaderFooterHeight(ctx context.Context, m HeightMeasurer, opts renderengines.RenderOptions) error {
	if !opts.DisplayHeaderFooter {
		return nil
	}
	widthPx := int64(opts.PaperWidth * pxPerInch)

	if opts.HeaderTemplate != "" {
		if err := checkRegionHeight(ctx, m, "header", opts.HeaderTemplate, widthPx, opts.MarginTop); err != nil {
			return err
		}
	}
	if opts.FooterTemplate != "" {
		if err := checkRegionHeight(ctx, m, "footer", opts.FooterTemplate, widthPx, opts.MarginBottom); err != nil {
			return err
		}
	}
	return nil
}

func checkRegionHeight(ctx context.Context, m HeightMeasurer, region, tmpl string, widthPx int64, marginInches float64) error {
	heightPx, err := m.MeasureHTMLHeight(ctx, tmpl, widthPx)
	if err != nil {
		return fmt.Errorf("customization: measuring %s height: %w", region, err)
	}
	heightInches := heightPx / pxPerInch
	if heightInches > marginInches {
		return &HeightMismatchError{Region: region, MeasuredInches: heightInches, MarginInches: marginInches}
	}
	return nil
}
