package api

// The per-request render options envelope. `content` + `payload` (see
// conversionRequest) carry *what* to render; `options` carries *how* — page
// size, orientation, margins, scale, header/footer, wait strategy, and the
// two engine-adjacent switches (embedImages, fitToPage). Every field is
// optional: an absent field falls back to the deployment default
// (Server.renderDefaults, itself layered on
// renderengines.DefaultRenderOptions), so an existing caller sending just
// {content} is completely unaffected. See docs/planning/SPEC-conversion-api.md
// (options-envelope section).

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/overlay"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// optionsInput is the decoded `options` object. Optional scalars are
// pointers so "field absent" is distinguishable from "field set to the zero
// value" (e.g. landscape:false must be able to override a landscape:true
// deployment default). dimension fields carry their own "set" flag instead.
type optionsInput struct {
	PaperSize *string   `json:"paperSize"`
	Width     dimension `json:"width"`
	Height    dimension `json:"height"`

	Landscape       *bool        `json:"landscape"`
	Margin          *marginInput `json:"margin"`
	Scale           *float64     `json:"scale"`
	PrintBackground *bool        `json:"printBackground"`

	DisplayHeaderFooter *bool   `json:"displayHeaderFooter"`
	HeaderTemplate      *string `json:"headerTemplate"`
	FooterTemplate      *string `json:"footerTemplate"`

	WaitForFonts  *bool `json:"waitForFonts"`
	WaitForImages *bool `json:"waitForImages"`
	TimeoutMs     *int  `json:"timeoutMs"`

	EmbedImages *bool `json:"embedImages"`
	FitToPage   *bool `json:"fitToPage"`

	Overlay *overlayInput `json:"overlay"`
}

type marginInput struct {
	Top    dimension `json:"top"`
	Right  dimension `json:"right"`
	Bottom dimension `json:"bottom"`
	Left   dimension `json:"left"`
}

// overlayInput is the decoded options.overlay object: an HTML fragment
// rendered on its own transparent page and stamped onto selected pages of
// the finished PDF (see internal/overlay). It is HTML-route only, and
// resolved separately from the render options because it isn't one — it's
// a post-render composition step.
type overlayInput struct {
	HTML  string  `json:"html"`
	Pages *string `json:"pages"` // absent => "last"
}

// spec validates the overlay object and returns it as an *overlay.Spec, or
// nil when no overlay was requested. A returned error is always a
// caller-side problem — the handler maps it to 400 INVALID_REQUEST.
func (o *overlayInput) spec() (*overlay.Spec, error) {
	if o == nil {
		return nil, nil
	}
	if strings.TrimSpace(o.HTML) == "" {
		return nil, fmt.Errorf("options.overlay: \"html\" is required")
	}
	pages := "last"
	if o.Pages != nil {
		pages = *o.Pages
	}
	// Validate the selector now (page-count-independent checks) so a typo
	// is a 400 before any rendering work starts; the bounds check against
	// the real page count happens in overlay.Apply.
	if err := overlay.ValidatePages(pages); err != nil {
		return nil, fmt.Errorf("options.overlay: %w", err)
	}
	return &overlay.Spec{HTML: o.HTML, Pages: pages}, nil
}

// dimension is a length accepted either as a number (inches) or a string
// with a CSS-style unit ("10mm", "0.5in", "72pt", "96px", "1cm"). A bare
// number string ("0.4") is also inches. Zero value = not set.
type dimension struct {
	inches float64
	set    bool
}

func (d *dimension) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		in, err := ParseDimension(s)
		if err != nil {
			return err
		}
		d.inches, d.set = in, true
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("expected a number or a quoted length like \"10mm\": %w", err)
	}
	if f < 0 {
		return fmt.Errorf("must not be negative")
	}
	d.inches, d.set = f, true
	return nil
}

// ParseDimension converts a CSS-style length to inches. Bare numbers are
// inches. Exported so cmd/api can parse the PDF_DEFAULT_MARGIN_* env values
// with the exact same rules the request options use.
func ParseDimension(s string) (float64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, fmt.Errorf("empty length")
	}
	var factor float64 = 1 // inches
	for unit, f := range map[string]float64{
		"in": 1,
		"mm": 1.0 / 25.4,
		"cm": 1.0 / 2.54,
		"pt": 1.0 / 72.0,
		"px": 1.0 / 96.0,
	} {
		if strings.HasSuffix(s, unit) {
			factor = f
			s = strings.TrimSpace(strings.TrimSuffix(s, unit))
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid length %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("length must not be negative")
	}
	return v * factor, nil
}

// paperSizes are the named sizes accepted by options.paperSize and the
// PDF_DEFAULT_PAPER env var, in {width, height} inches (portrait).
var paperSizes = map[string][2]float64{
	"letter":  {8.5, 11},
	"legal":   {8.5, 14},
	"tabloid": {11, 17},
	"a3":      {11.69, 16.54},
	"a4":      {8.27, 11.69},
	"a5":      {5.83, 8.27},
}

// PaperSizeInches resolves a named paper size (case-insensitive) to portrait
// width/height in inches. Exported for cmd/api's env parsing.
func PaperSizeInches(name string) (w, h float64, ok bool) {
	s, found := paperSizes[strings.ToLower(strings.TrimSpace(name))]
	if !found {
		return 0, 0, false
	}
	return s[0], s[1], true
}

const (
	minScale     = 0.1
	maxScale     = 2.0
	minTimeoutMs = 1000
	maxTimeoutMs = 120_000
)

// resolve layers the request options on top of base (the deployment
// defaults) and returns the effective render options plus the two
// engine-adjacent switches. A returned error is always a caller-side
// problem — the handler maps it to 400 INVALID_REQUEST.
func (o *optionsInput) resolve(base renderengines.RenderOptions) (opts renderengines.RenderOptions, embedImages, fitToPage bool, err error) {
	opts = base
	if o == nil {
		return opts, false, false, nil
	}

	switch {
	case o.Width.set != o.Height.set:
		return opts, false, false, fmt.Errorf("options: width and height must be set together")
	case o.Width.set && o.Height.set:
		if o.PaperSize != nil {
			return opts, false, false, fmt.Errorf("options: set either paperSize or width+height, not both")
		}
		opts.PaperWidth, opts.PaperHeight = o.Width.inches, o.Height.inches
	case o.PaperSize != nil:
		w, h, ok := PaperSizeInches(*o.PaperSize)
		if !ok {
			return opts, false, false, fmt.Errorf("options: unknown paperSize %q", *o.PaperSize)
		}
		opts.PaperWidth, opts.PaperHeight = w, h
	}

	if o.Landscape != nil {
		opts.Landscape = *o.Landscape
	}
	if o.PrintBackground != nil {
		opts.PrintBackground = *o.PrintBackground
	}
	if o.Scale != nil {
		if *o.Scale < minScale || *o.Scale > maxScale {
			return opts, false, false, fmt.Errorf("options: scale must be between %g and %g", minScale, maxScale)
		}
		opts.Scale = *o.Scale
	}
	if o.DisplayHeaderFooter != nil {
		opts.DisplayHeaderFooter = *o.DisplayHeaderFooter
	}
	if o.HeaderTemplate != nil {
		opts.HeaderTemplate = *o.HeaderTemplate
	}
	if o.FooterTemplate != nil {
		opts.FooterTemplate = *o.FooterTemplate
	}
	if o.WaitForFonts != nil {
		opts.WaitForFonts = *o.WaitForFonts
	}
	if o.WaitForImages != nil {
		opts.WaitForImages = *o.WaitForImages
	}
	if o.TimeoutMs != nil {
		ms := *o.TimeoutMs
		if ms < minTimeoutMs {
			ms = minTimeoutMs
		}
		if ms > maxTimeoutMs {
			ms = maxTimeoutMs
		}
		opts.Timeout = time.Duration(ms) * time.Millisecond
	}
	if o.Margin != nil {
		if o.Margin.Top.set {
			opts.MarginTop = o.Margin.Top.inches
		}
		if o.Margin.Right.set {
			opts.MarginRight = o.Margin.Right.inches
		}
		if o.Margin.Bottom.set {
			opts.MarginBottom = o.Margin.Bottom.inches
		}
		if o.Margin.Left.set {
			opts.MarginLeft = o.Margin.Left.inches
		}
	}

	if o.EmbedImages != nil {
		embedImages = *o.EmbedImages
	}
	if o.FitToPage != nil {
		fitToPage = *o.FitToPage
	}
	return opts, embedImages, fitToPage, nil
}
