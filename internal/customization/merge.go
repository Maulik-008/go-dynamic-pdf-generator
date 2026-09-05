// Package customization implements the merge-variable templating system:
// an HTML or Markdown *template* string plus a JSON-shaped payload
// produces the final document string that internal/renderengines then
// renders to PDF unchanged — turning "my way, any kind of customisation"
// into something that scales past one-off HTML/Markdown strings. See
// docs/planning/SPEC-customization-layer.md.
//
// v1 scope: template+payload merge only. Header/footer auto-height
// validation, watermarking, and PDF/A are this module's other named
// responsibilities but are separate, independently-testable slices —
// deferred, not dropped, per the spec.
package customization

import (
	"bytes"
	"fmt"
	"html/template"
)

// Merge fills tmpl with values from payload (already JSON-decoded, e.g.
// via json.Unmarshal into map[string]any) using Go's html/template, not a
// Liquid-style engine (see SPEC-customization-layer.md's Tech Stack for
// why) — payload values are contextually HTML-escaped by construction, a
// real security property since the merged result is later executed in a
// real Chromium instance by internal/renderengines: an unescaped payload
// value containing markup would be a genuine injection vector, not just a
// cosmetic bug. This applies uniformly to Markdown templates too, since
// goldmark (internal/renderengines/markdown.go) passes raw HTML through
// verbatim per CommonMark's own semantics.
//
// A missing payload field is a template execution error naming the exact
// field — Option("missingkey=error") overrides html/template's default
// behavior of silently substituting "<no value>" for a missing map key,
// matching PLATFORM-SPEC.md's "name the exact missing template variable"
// requirement.
func Merge(tmpl string, payload map[string]any) (string, error) {
	t, err := template.New("customization").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("customization: parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, payload); err != nil {
		return "", fmt.Errorf("customization: execute template: %w", err)
	}
	return buf.String(), nil
}
