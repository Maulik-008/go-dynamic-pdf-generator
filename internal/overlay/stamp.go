package overlay

import (
	"bytes"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

func init() {
	// This service is stateless and ships its own fonts via Chromium: never
	// read or create ~/.config/pdfcpu. Must run before the first pdfcpu
	// call, so package init is the right place.
	api.DisableConfigDir()
}

// stampDesc lays the stamp page's lower-left corner on the target page's
// lower-left corner at native size — no scaling, no rotation, fully
// opaque. Because stampPages feeds a stamp page rendered at the same
// dimensions as the target, this is an exact 1:1 overlay.
const stampDesc = "scale:1 abs, pos:bl, off:0 0, rot:0, op:1"

// pdfConf is pdfcpu's default configuration with relaxed validation:
// Chromium's PrintToPDF output is well-formed, but relaxed mode also
// tolerates the minor spec deviations real-world PDFs carry, matching the
// "only add an external-tool fallback if we hit a real pdfcpu gap" stance
// in the research doc.
func pdfConf() *model.Configuration {
	c := model.NewDefaultConfiguration()
	c.ValidationMode = model.ValidationRelaxed
	return c
}

// pageCount reports the number of pages in pdf.
func pageCount(pdf []byte) (int, error) {
	return api.PageCount(bytes.NewReader(pdf), pdfConf())
}

// stampPages composites page 1 of stampPDF onto every page of mainPDF named
// in pages (pdfcpu selection strings; nil means all pages) and returns the
// new document. Page count and existing page content are preserved — the
// stamp is drawn on top.
func stampPages(mainPDF, stampPDF []byte, pages []string) ([]byte, error) {
	wm, err := api.PDFWatermarkForReadSeeker(bytes.NewReader(stampPDF), 1, stampDesc, true /* onTop = stamp */, false, types.POINTS)
	if err != nil {
		return nil, fmt.Errorf("build stamp from fragment pdf: %w", err)
	}

	var out bytes.Buffer
	if err := api.AddWatermarks(bytes.NewReader(mainPDF), &out, pages, wm, pdfConf()); err != nil {
		return nil, fmt.Errorf("apply stamp to %v: %w", pages, err)
	}
	return out.Bytes(), nil
}
