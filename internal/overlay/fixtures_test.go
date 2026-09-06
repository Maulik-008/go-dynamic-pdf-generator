package overlay

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// makePDF builds an n-page Letter PDF in memory, each page carrying a
// distinct visible marker, using pdfcpu's own JSON creation API — no
// Chromium, so overlay's pure stamp/selection logic is testable
// everywhere the macOS note in docs/guides/07-medico-integration.md
// applies. It is the test stand-in for "a PDF the render engine already
// produced".
func makePDF(t *testing.T, n int) []byte {
	t.Helper()
	if n < 1 {
		t.Fatalf("makePDF: n must be >= 1, got %d", n)
	}
	pages := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		pages = append(pages, fmt.Sprintf(
			`"%d":{"content":{"text":[{"value":"BODY PAGE %d","anchor":"center","font":{"name":"Helvetica","size":18}}]}}`,
			i, i))
	}
	js := `{"paper":"Letter","pages":{` + strings.Join(pages, ",") + `}}`

	var out bytes.Buffer
	if err := api.Create(nil, strings.NewReader(js), &out, nil); err != nil {
		t.Fatalf("makePDF(%d): %v", n, err)
	}
	return out.Bytes()
}

// makeFragmentPDF builds the one-page transparent "stamp" PDF a real
// FragmentRenderer would return: a single visible box near the bottom, the
// rest of the page unpainted.
func makeFragmentPDF(t *testing.T, label string) []byte {
	t.Helper()
	js := fmt.Sprintf(`{"paper":"Letter","pages":{"1":{"content":{"text":[`+
		`{"value":%q,"anchor":"bottomCenter","font":{"name":"Helvetica","size":9},`+
		`"bgCol":"#FFFFFF","border":{"width":1,"col":"#000000"}}]}}}}`, label)

	var out bytes.Buffer
	if err := api.Create(nil, strings.NewReader(js), &out, nil); err != nil {
		t.Fatalf("makeFragmentPDF: %v", err)
	}
	return out.Bytes()
}
