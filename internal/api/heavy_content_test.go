package api

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// syntheticChartPNG generates a deterministic, non-trivial-to-compress PNG
// (a gradient + sine-wave pattern, standing in for a real embedded chart
// image) as a data: URI, so heavyReportHTML doesn't depend on any external
// asset file — the load test stays fully self-contained and reproducible.
func syntheticChartPNG(w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r := uint8((x * 255) / w)
			g := uint8((y * 255) / h)
			b := uint8(128 + 127*math.Sin(float64(x+y)/20))
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// heavyReportHTML builds a self-contained "heavy" business report: three
// embedded chart-like images plus a large itemized data table with `rows`
// rows — standing in for a realistic invoice/report workload rather than a
// trivial single-line fixture. rows=50 was empirically calibrated (see
// docs/research/load-test-results.md) to render to exactly 3 A4 pages at
// the platform's default render options.
func heavyReportHTML(rows int) string {
	chart := syntheticChartPNG(400, 200)

	var sb bytes.Buffer
	sb.WriteString(`<html><head><style>
		body { font-family: sans-serif; font-size: 11px; }
		h1 { font-size: 20px; } h2 { font-size: 14px; margin-top: 24px; }
		table { border-collapse: collapse; width: 100%; }
		th, td { border: 1px solid #ccc; padding: 4px 6px; text-align: left; }
		tr:nth-child(even) { background: #f2f2f2; }
	</style></head><body>`)
	sb.WriteString(`<h1>Quarterly Operations Report</h1>`)
	sb.WriteString(`<p>Load-test fixture: a heavy multi-page document with embedded images and a large data table.</p>`)
	sb.WriteString(`<div class="charts">`)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&sb, `<img src="%s" width="200" height="100">`, chart)
	}
	sb.WriteString(`</div>`)
	sb.WriteString(`<h2>Itemized Transactions</h2>`)
	sb.WriteString(`<table><thead><tr><th>#</th><th>Date</th><th>Description</th><th>Category</th><th>Qty</th><th>Unit Price</th><th>Total</th></tr></thead><tbody>`)
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&sb, `<tr><td>%d</td><td>2026-%02d-%02d</td><td>Line item description for transaction number %d covering standard product/service usage</td><td>Category %d</td><td>%d</td><td>$%.2f</td><td>$%.2f</td></tr>`,
			i, (i%12)+1, (i%28)+1, i, (i%7)+1, (i%20)+1,
			float64((i%100)+1)*1.5, float64((i%100)+1)*1.5*float64((i%20)+1))
	}
	sb.WriteString(`</tbody></table></body></html>`)
	return sb.String()
}

// heavyReportRowsFor3Pages is the row count empirically calibrated to
// produce a 3-page PDF at the platform's default render options (A4,
// default margins) — see docs/research/load-test-results.md for the
// calibration sweep (30 rows -> 2 pages, 50 -> 3, 70 -> 5, ...).
const heavyReportRowsFor3Pages = 50
