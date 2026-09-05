package customization

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Maulik-zuru/great-pdf-generator/internal/renderengines"
)

// fakeHeightMeasurer lets tests control exactly what height a template
// "measures" as, without a real Chromium instance — the unit tests here are
// about ValidateHeaderFooterHeight's own decision logic (compare measured
// height to margin, name the right region), not about measurement accuracy
// itself (that's renderengines.MeasureHTMLHeight's own, separately-tested
// job).
type fakeHeightMeasurer struct {
	heightsByHTML map[string]float64
	err           error
}

func (f *fakeHeightMeasurer) MeasureHTMLHeight(_ context.Context, html string, _ int64) (float64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.heightsByHTML[html], nil
}

func TestValidateHeaderFooterHeight_NoOpWhenDisplayHeaderFooterFalse(t *testing.T) {
	m := &fakeHeightMeasurer{heightsByHTML: map[string]float64{"<div>tall header</div>": 96 * 10}}
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = false
	opts.HeaderTemplate = "<div>tall header</div>"
	opts.MarginTop = 0.4

	if err := ValidateHeaderFooterHeight(context.Background(), m, opts); err != nil {
		t.Fatalf("expected no validation when DisplayHeaderFooter is false, got: %v", err)
	}
}

func TestValidateHeaderFooterHeight_NoOpWhenTemplatesEmpty(t *testing.T) {
	m := &fakeHeightMeasurer{}
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	// HeaderTemplate/FooterTemplate left empty

	if err := ValidateHeaderFooterHeight(context.Background(), m, opts); err != nil {
		t.Fatalf("expected no validation with no header/footer templates configured, got: %v", err)
	}
}

func TestValidateHeaderFooterHeight_PassesWhenHeaderFitsMargin(t *testing.T) {
	const header = "<div>short header</div>"
	m := &fakeHeightMeasurer{heightsByHTML: map[string]float64{header: 20}} // 20px ≈ 0.21in
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	opts.HeaderTemplate = header
	opts.MarginTop = 0.4 // 0.4in margin, plenty of room for a 0.21in header

	if err := ValidateHeaderFooterHeight(context.Background(), m, opts); err != nil {
		t.Fatalf("expected no error when the header fits its margin, got: %v", err)
	}
}

// TestValidateHeaderFooterHeight_RejectsHeaderTallerThanMargin proves the
// core fast-fail behavior PLATFORM-SPEC.md requires: "Header/footer height
// mismatches are rejected at request time with a specific error, not
// silently rendered overlapping" — the named regression class from real
// Puppeteer issues (#10024, #13738, #4132/#4266-style overlap bugs).
func TestValidateHeaderFooterHeight_RejectsHeaderTallerThanMargin(t *testing.T) {
	const header = "<div>tall header</div>"
	m := &fakeHeightMeasurer{heightsByHTML: map[string]float64{header: 96}} // 96px = exactly 1in
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	opts.HeaderTemplate = header
	opts.MarginTop = 0.4 // 0.4in margin, header measures 1in — will overflow

	err := ValidateHeaderFooterHeight(context.Background(), m, opts)
	if err == nil {
		t.Fatal("expected a height-mismatch error, got nil")
	}
	var mismatch *HeightMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is not a *HeightMismatchError: %v", err)
	}
	if mismatch.Region != "header" {
		t.Fatalf("Region = %q, want %q", mismatch.Region, "header")
	}
	if !strings.Contains(err.Error(), "header") {
		t.Fatalf("error message %q does not name the region", err.Error())
	}
}

func TestValidateHeaderFooterHeight_RejectsFooterTallerThanMargin(t *testing.T) {
	const footer = "<div>tall footer</div>"
	m := &fakeHeightMeasurer{heightsByHTML: map[string]float64{footer: 96}} // 1in
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	opts.FooterTemplate = footer
	opts.MarginBottom = 0.4

	err := ValidateHeaderFooterHeight(context.Background(), m, opts)
	if err == nil {
		t.Fatal("expected a height-mismatch error, got nil")
	}
	var mismatch *HeightMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is not a *HeightMismatchError: %v", err)
	}
	if mismatch.Region != "footer" {
		t.Fatalf("Region = %q, want %q", mismatch.Region, "footer")
	}
}

func TestValidateHeaderFooterHeight_ChecksHeaderBeforeFooter(t *testing.T) {
	m := &fakeHeightMeasurer{heightsByHTML: map[string]float64{
		"<div>bad header</div>": 96,
		"<div>bad footer</div>": 96,
	}}
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	opts.HeaderTemplate = "<div>bad header</div>"
	opts.FooterTemplate = "<div>bad footer</div>"
	opts.MarginTop = 0.4
	opts.MarginBottom = 0.4

	err := ValidateHeaderFooterHeight(context.Background(), m, opts)
	var mismatch *HeightMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error is not a *HeightMismatchError: %v", err)
	}
	if mismatch.Region != "header" {
		t.Fatalf("Region = %q, want %q (header should be checked first)", mismatch.Region, "header")
	}
}

func TestValidateHeaderFooterHeight_MeasurementErrorPropagates(t *testing.T) {
	m := &fakeHeightMeasurer{err: errors.New("boom")}
	opts := renderengines.DefaultRenderOptions()
	opts.DisplayHeaderFooter = true
	opts.HeaderTemplate = "<div>x</div>"
	opts.MarginTop = 0.4

	err := ValidateHeaderFooterHeight(context.Background(), m, opts)
	if err == nil {
		t.Fatal("expected the underlying measurement error to propagate, got nil")
	}
}

// TestValidateHeaderFooterHeight_WithRealRenderEnginesPool proves the whole
// real pipeline — not just the fake — correctly rejects a genuinely tall
// header and accepts a genuinely short one, against the actual Chromium
// measurement. Skipped without CHROMIUM_PATH, matching the existing
// convention throughout this codebase.
func TestValidateHeaderFooterHeight_WithRealRenderEnginesPool(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium test")
	}
	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config: renderengines.Config{ChromiumPath: path},
		Size:   1,
	})
	if err != nil {
		t.Fatalf("renderengines.NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	tallOpts := renderengines.DefaultRenderOptions()
	tallOpts.DisplayHeaderFooter = true
	tallOpts.HeaderTemplate = `<div style="height:200px">way too tall for a 0.4in margin</div>`
	tallOpts.MarginTop = 0.4

	if err := ValidateHeaderFooterHeight(context.Background(), pool, tallOpts); err == nil {
		t.Fatal("expected a real height-mismatch error for a 200px header against a 0.4in margin, got nil")
	}

	shortOpts := renderengines.DefaultRenderOptions()
	shortOpts.DisplayHeaderFooter = true
	shortOpts.HeaderTemplate = `<div style="height:10px">fits</div>`
	shortOpts.MarginTop = 0.4

	if err := ValidateHeaderFooterHeight(context.Background(), pool, shortOpts); err != nil {
		t.Fatalf("expected no error for a 10px header against a 0.4in margin, got: %v", err)
	}
}
