package renderengines

import (
	"context"
	"strings"
	"testing"
	"time"
)

// countPages is a crude but reliable page count: Chromium's PrintToPDF
// output has one "/Type /Page" object per page. Good enough to tell "one
// page" from "several".
func countPages(pdf []byte) int {
	return strings.Count(string(pdf), "/Type /Page\n") + strings.Count(string(pdf), "/Type/Page/")
}

func letterOpts() RenderOptions {
	o := DefaultRenderOptions()
	o.PaperWidth, o.PaperHeight = 8.5, 11
	o.MarginTop, o.MarginBottom, o.MarginLeft, o.MarginRight = 0, 10.0/25.4, 0, 0
	return o
}

func TestRenderHTMLFitted_ShortContentIsNotScaled(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := r.RenderHTMLFitted(ctx, `<html><body><h1>Short</h1><p>one line</p></body></html>`, letterOpts())
	if err != nil {
		t.Fatalf("RenderHTMLFitted: %v", err)
	}
	if res.Scale != 1.0 || res.Overflowed {
		t.Fatalf("short content should not scale: scale=%v overflow=%v", res.Scale, res.Overflowed)
	}
	if !strings.HasPrefix(string(res.PDF), "%PDF-") {
		t.Fatalf("not a PDF")
	}
}

func TestRenderHTMLFitted_TallContentScalesToOnePage(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ~45 lines: overflows one Letter page unscaled, but fits above the
	// MinFitScale floor once scaled.
	var b strings.Builder
	b.WriteString("<html><body style='margin:0'>")
	for i := 0; i < 45; i++ {
		b.WriteString("<p style='margin:6px 0'>Paragraph line number ")
		b.WriteByte(byte('a' + i%26))
		b.WriteString(" with a bit of text to give it height.</p>")
	}
	b.WriteString("</body></html>")

	res, err := r.RenderHTMLFitted(ctx, b.String(), letterOpts())
	if err != nil {
		t.Fatalf("RenderHTMLFitted: %v", err)
	}
	if res.Scale >= 1.0 {
		t.Fatalf("tall content should have been scaled down, scale=%v", res.Scale)
	}
	if res.Scale < MinFitScale {
		t.Fatalf("scale %v below floor %v", res.Scale, MinFitScale)
	}
	if n := countPages(res.PDF); n != 1 {
		t.Fatalf("expected 1 page after fitting, got %d", n)
	}
}

func TestRenderHTMLFitted_ExtremeContentClampsAndFlagsOverflow(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < 400; i++ {
		b.WriteString("<p>Line to make this document far taller than any single page can hold.</p>")
	}
	b.WriteString("</body></html>")

	res, err := r.RenderHTMLFitted(ctx, b.String(), letterOpts())
	if err != nil {
		t.Fatalf("RenderHTMLFitted: %v", err)
	}
	if res.Scale != MinFitScale || !res.Overflowed {
		t.Fatalf("extreme content should clamp at MinFitScale and flag overflow: scale=%v overflow=%v", res.Scale, res.Overflowed)
	}
}
