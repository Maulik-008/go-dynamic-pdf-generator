package renderengines

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A last-page overlay shaped like the real disclaimer box: fixed to the page
// bottom, two lines of 9px text plus a page-number line — about 49px tall,
// i.e. ~12px more than the 10mm bottom margin already keeps clear.
const disclaimerLikeOverlay = `<style>body{margin:0}.d{position:fixed;left:0;bottom:0;width:100%;padding:5px 15px;border-top:1px solid #000;font:9px/1.3 Arial,sans-serif;box-sizing:border-box;background:#fff}</style>` +
	`<div class="d"><div>Disclaimer: one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty ` +
	`one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty ` +
	`one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty ` +
	`one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty</div><div style="text-align:center">99 / 99</div></div>`

// twoPageDoc is a full first page followed by a last page whose single block
// is lastPx tall, so lastPx controls how far down the last page content ends.
func twoPageDoc(lastPx int) string {
	return fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>body{margin:0}</style></head><body>`+
		`<div style="height:1000px">page one</div><div style="break-before:page;height:%dpx">last</div></body></html>`, lastPx)
}

func renderPages(t *testing.T, r *Renderer, html string, opts RenderOptions) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pdf, err := r.RenderHTML(ctx, html, opts)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	return countPages(pdf)
}

// Without a reservation, content ending inside the overlay zone stays on the
// last page (and would be covered). With one, the spacer pushes the overlay
// onto its own page instead — content is never underneath it.
func TestReserveOverlay_PushesCoveredContentClear(t *testing.T) {
	r := testRenderer(t)
	nearBottom := twoPageDoc(1012) // ends ~6px above the page content limit: inside the zone

	plain := letterOpts()
	if got := renderPages(t, r, nearBottom, plain); got != 2 {
		t.Fatalf("baseline (no reservation) pages = %d, want 2", got)
	}

	reserved := letterOpts()
	reserved.OverlayReserveHTML = disclaimerLikeOverlay
	if got := renderPages(t, r, nearBottom, reserved); got != 3 {
		t.Errorf("with reservation pages = %d, want 3 (overlay pushed to its own page)", got)
	}
}

func TestReserveOverlay_NoExtraPageWhenThereIsRoom(t *testing.T) {
	r := testRenderer(t)
	reserved := letterOpts()
	reserved.OverlayReserveHTML = disclaimerLikeOverlay
	if got := renderPages(t, r, twoPageDoc(600), reserved); got != 2 {
		t.Errorf("pages = %d, want 2: a last page with room to spare must not grow", got)
	}
}

// An overlay that draws nothing against the bottom edge reserves nothing.
func TestReserveOverlay_MidPageOverlayReservesNothing(t *testing.T) {
	r := testRenderer(t)
	reserved := letterOpts()
	reserved.OverlayReserveHTML = `<div style="position:fixed;top:300px;left:0">watermark</div>`
	if got := renderPages(t, r, twoPageDoc(1012), reserved); got != 2 {
		t.Errorf("pages = %d, want 2: only bottom-anchored overlays should reserve space", got)
	}
}

// fitToPage must account for the reserved zone: content that fits on its own
// but not alongside the overlay is scaled down rather than covered.
func TestReserveOverlay_FitToPageScalesForOverlay(t *testing.T) {
	r := testRenderer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	doc := `<!DOCTYPE html><html><body style="margin:0"><div style="height:1010px">tall</div></body></html>`

	plain, err := r.RenderHTMLFitted(ctx, doc, letterOpts())
	if err != nil {
		t.Fatal(err)
	}
	if plain.Scale != 1.0 {
		t.Fatalf("baseline scale = %v, want 1.0", plain.Scale)
	}

	opts := letterOpts()
	opts.OverlayReserveHTML = disclaimerLikeOverlay
	res, err := r.RenderHTMLFitted(ctx, doc, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scale >= 1.0 {
		t.Errorf("scale with overlay reservation = %v, want < 1.0", res.Scale)
	}
}
