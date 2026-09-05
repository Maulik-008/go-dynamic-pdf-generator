package renderengines

import (
	"bytes"
	"context"
	"fmt"

	"github.com/yuin/goldmark"
)

// RenderMarkdown compiles the given CommonMark-compliant Markdown to HTML via
// goldmark, then renders it through the same Chromium fidelity path as
// RenderHTML — one engine, two input formats, per
// docs/planning/SPEC-render-engines.md.
func (r *Renderer) RenderMarkdown(ctx context.Context, markdown string, opts RenderOptions) ([]byte, error) {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(markdown), &buf); err != nil {
		return nil, fmt.Errorf("renderengines: convert markdown: %w", err)
	}
	return r.RenderHTML(ctx, buf.String(), opts)
}
