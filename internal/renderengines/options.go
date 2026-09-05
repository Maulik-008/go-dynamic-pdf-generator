package renderengines

import "time"

// RenderOptions controls how a document is rendered to PDF. Start from
// DefaultRenderOptions and override individual fields — the Go zero value
// (all bools false, all numbers 0) is deliberately not a sensible default,
// since a zero-valued struct would disable font/image waiting and print at
// 0x0 inches.
type RenderOptions struct {
	// Landscape orientation. Default: false (portrait).
	Landscape bool

	// PrintBackground includes background colors/images. Default: true —
	// most business documents expect their own styling to show up in the
	// PDF, not just text.
	PrintBackground bool

	// Scale of the rendered content, 0.1-2.0.
	Scale float64

	// PaperWidth/PaperHeight in inches.
	PaperWidth  float64
	PaperHeight float64

	// Margins in inches.
	MarginTop, MarginBottom, MarginLeft, MarginRight float64

	// DisplayHeaderFooter enables HeaderTemplate/FooterTemplate.
	DisplayHeaderFooter bool
	HeaderTemplate      string
	FooterTemplate      string

	// WaitForFonts waits for document.fonts.ready before printing. Default:
	// true. This is the single most-cited cause of missing/incorrect text
	// in HTML-to-PDF output across every engine surveyed in
	// docs/research/go-pdf-generation-research.md Part 4 — defaulting it on
	// is a deliberate accuracy decision from docs/planning/PLATFORM-SPEC.md.
	WaitForFonts bool

	// WaitForImages waits for all <img> elements to finish loading (success
	// or error) before printing. Default: true.
	WaitForImages bool

	// Timeout bounds the whole render call, including any waiting above.
	Timeout time.Duration
}

// DefaultRenderOptions returns the recommended defaults: A4 portrait,
// backgrounds on, wait for fonts and images, 30s timeout — "just works" for
// a typical business document, per the platform spec's Ease-of-use pillar.
func DefaultRenderOptions() RenderOptions {
	return RenderOptions{
		PrintBackground: true,
		Scale:           1.0,
		PaperWidth:      8.27, // A4, inches
		PaperHeight:     11.69,
		MarginTop:       0.4, // ~1cm, matches Chromium's own PrintToPDF default
		MarginBottom:    0.4,
		MarginLeft:      0.4,
		MarginRight:     0.4,
		WaitForFonts:    true,
		WaitForImages:   true,
		Timeout:         30 * time.Second,
	}
}

// withDefaults fills in zero-valued numeric fields that would otherwise
// produce a nonsensical render (e.g. a 0x0-inch page) if a caller passes a
// RenderOptions{} they didn't build from DefaultRenderOptions. It does not
// touch bool fields, since Go can't distinguish "unset" from "false" there —
// callers are expected to start from DefaultRenderOptions for those.
func (o RenderOptions) withDefaults() RenderOptions {
	d := DefaultRenderOptions()
	if o.Scale == 0 {
		o.Scale = d.Scale
	}
	if o.PaperWidth == 0 {
		o.PaperWidth = d.PaperWidth
	}
	if o.PaperHeight == 0 {
		o.PaperHeight = d.PaperHeight
	}
	if o.Timeout == 0 {
		o.Timeout = d.Timeout
	}
	return o
}
