package customization

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
)

func TestMerge_SimpleField(t *testing.T) {
	got, err := Merge(`<p>Hello {{.name}}</p>`, map[string]any{"name": "Ada"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got != `<p>Hello Ada</p>` {
		t.Fatalf("got %q, want %q", got, `<p>Hello Ada</p>`)
	}
}

func TestMerge_NestedField(t *testing.T) {
	payload := map[string]any{
		"customer": map[string]any{"name": "Ada Lovelace"},
	}
	got, err := Merge(`<p>Bill to: {{.customer.name}}</p>`, payload)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got != `<p>Bill to: Ada Lovelace</p>` {
		t.Fatalf("got %q, want nested field substituted", got)
	}
}

// TestMerge_RangeOverArray proves the invoice line-item use case explicitly
// cited in docs/planning/PLATFORM-SPEC.md ("data-driven templates... turns
// 'my way' into something that scales") works with Go's stdlib template
// syntax, without needing a Liquid-style engine.
func TestMerge_RangeOverArray(t *testing.T) {
	payload := map[string]any{
		"items": []any{
			map[string]any{"description": "Widget", "amount": "10.00"},
			map[string]any{"description": "Gadget", "amount": "25.00"},
		},
	}
	tmpl := `<ul>{{range .items}}<li>{{.description}}: ${{.amount}}</li>{{end}}</ul>`
	got, err := Merge(tmpl, payload)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	want := `<ul><li>Widget: $10.00</li><li>Gadget: $25.00</li></ul>`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestMerge_MissingFieldErrorsNamingField proves the platform's own
// requirement ("name the exact missing template variable" —
// PLATFORM-SPEC.md) — Go's html/template silently substitutes "<no value>"
// for a missing map key by default, which is exactly the footgun this test
// guards against via Option("missingkey=error").
func TestMerge_MissingFieldErrorsNamingField(t *testing.T) {
	_, err := Merge(`<p>Hello {{.name}}</p>`, map[string]any{})
	if err == nil {
		t.Fatal("expected an error for a missing payload field, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("error %q does not name the missing field %q", err.Error(), "name")
	}
}

func TestMerge_MalformedTemplateSyntaxErrors(t *testing.T) {
	_, err := Merge(`<p>Hello {{.name</p>`, map[string]any{"name": "Ada"})
	if err == nil {
		t.Fatal("expected a parse error for malformed template syntax, got nil")
	}
}

// TestMerge_EscapesHTMLInPayloadValue_HTMLTemplate proves the security
// property this module's design depends on: render-engines executes the
// merged result in a real browser, so an unescaped payload value
// containing markup would be a genuine HTML/script injection vector, not
// just a cosmetic bug. html/template's contextual escaping must prevent it.
func TestMerge_EscapesHTMLInPayloadValue_HTMLTemplate(t *testing.T) {
	payload := map[string]any{"name": `<script>alert(1)</script>`}
	got, err := Merge(`<p>Hello {{.name}}</p>`, payload)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if strings.Contains(got, "<script>") {
		t.Fatalf("payload value was not escaped — got %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("expected an HTML-escaped script tag in output, got %q", got)
	}
}

// TestMerge_EscapesHTMLInPayloadValue_MarkdownTemplate proves the same
// property end-to-end through the real goldmark conversion
// (internal/renderengines/markdown.go's exact dependency), not just at the
// Merge step in isolation — confirmed via a direct check that
// goldmark.Convert passes raw HTML through verbatim (CommonMark's own
// documented behavior), so an unescaped merge here would produce real,
// executable <script> markup in the final rendered HTML.
func TestMerge_EscapesHTMLInPayloadValue_MarkdownTemplate(t *testing.T) {
	payload := map[string]any{"name": `<script>alert(1)</script>`}
	merged, err := Merge("Hello {{.name}}\n", payload)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(merged), &buf); err != nil {
		t.Fatalf("goldmark.Convert: %v", err)
	}
	html := buf.String()
	if strings.Contains(html, "<script>") {
		t.Fatalf("final HTML contains an executable <script> tag from payload data — got %q", html)
	}
}
