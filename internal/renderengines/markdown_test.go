package renderengines

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestRenderMarkdown_ProducesValidPDF(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pdf, err := r.RenderMarkdown(ctx, "# Hello, Markdown\n\nSome **bold** text.", DefaultRenderOptions())
	if err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("output does not start with %%PDF- magic bytes")
	}
	if len(pdf) < 100 {
		t.Fatalf("output suspiciously small (%d bytes) for a rendered page", len(pdf))
	}
}
