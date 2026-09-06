package overlay

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// fakeFragmentRenderer records what it was asked to render and returns a
// real one-page PDF so the pdfcpu stamp step downstream has valid input.
type fakeFragmentRenderer struct {
	t        *testing.T
	gotHTML  string
	gotOpts  renderengines.RenderOptions
	called   int
	failWith error
}

func (f *fakeFragmentRenderer) RenderHTML(_ context.Context, html string, opts renderengines.RenderOptions) ([]byte, error) {
	f.called++
	f.gotHTML = html
	f.gotOpts = opts
	if f.failWith != nil {
		return nil, f.failWith
	}
	return makeFragmentPDF(f.t, "fragment"), nil
}

func baseLetter() renderengines.RenderOptions {
	o := renderengines.DefaultRenderOptions()
	o.PaperWidth, o.PaperHeight = 8.5, 11
	o.Landscape = true
	o.MarginTop, o.MarginBottom, o.MarginLeft, o.MarginRight = 0.4, 0.4, 0.4, 0.4
	o.DisplayHeaderFooter = true
	o.HeaderTemplate, o.FooterTemplate = "<div>h</div>", "<div>f</div>"
	o.Scale = 0.9
	return o
}

func TestApply_SubstitutesTotalPagesToken(t *testing.T) {
	fr := &fakeFragmentRenderer{t: t}
	main := makePDF(t, 3)

	_, err := Apply(context.Background(), fr, main,
		Spec{HTML: "<div>Total: " + totalPagesToken + " pages</div>", Pages: "last"}, baseLetter())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if fr.gotHTML != "<div>Total: 3 pages</div>" {
		t.Fatalf("fragment HTML = %q, want the token replaced with the real page count", fr.gotHTML)
	}
}

func TestApply_FragmentRenderedEdgeToEdge(t *testing.T) {
	fr := &fakeFragmentRenderer{t: t}

	_, err := Apply(context.Background(), fr, makePDF(t, 2),
		Spec{HTML: "<div>x</div>", Pages: "last"}, baseLetter())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	o := fr.gotOpts
	if o.MarginTop != 0 || o.MarginBottom != 0 || o.MarginLeft != 0 || o.MarginRight != 0 {
		t.Errorf("fragment margins must all be zero, got %+v", o)
	}
	if o.DisplayHeaderFooter || o.HeaderTemplate != "" || o.FooterTemplate != "" {
		t.Errorf("fragment must not carry the main doc's Chromium header/footer, got %+v", o)
	}
	if o.Scale != 1 {
		t.Errorf("fragment scale = %v, want 1 (native)", o.Scale)
	}
	if !o.PrintBackground {
		t.Error("fragment must print backgrounds so its own box paints")
	}
	// Page geometry is inherited so the stamp lines up 1:1.
	if o.PaperWidth != 8.5 || o.PaperHeight != 11 || !o.Landscape {
		t.Errorf("fragment page geometry not inherited from base: %+v", o)
	}
}

func TestApply_StampsSelectedPagesPreservingCount(t *testing.T) {
	fr := &fakeFragmentRenderer{t: t}
	main := makePDF(t, 4)

	out, err := Apply(context.Background(), fr, main, Spec{HTML: "<div>x</div>", Pages: "2-3"}, baseLetter())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, _ := pageCount(out); got != 4 {
		t.Fatalf("output pages = %d, want 4", got)
	}
	if err := api.Validate(bytes.NewReader(out), pdfConf()); err != nil {
		t.Fatalf("output fails validation: %v", err)
	}
	if fr.called != 1 {
		t.Fatalf("fragment renderer called %d times, want exactly 1", fr.called)
	}
}

func TestApply_PagesOutOfRangeError(t *testing.T) {
	fr := &fakeFragmentRenderer{t: t}
	_, err := Apply(context.Background(), fr, makePDF(t, 2), Spec{HTML: "<div>x</div>", Pages: "5"}, baseLetter())
	if err == nil {
		t.Fatal("Apply with page 5 of a 2-page doc: want error, got nil")
	}
	if fr.called != 0 {
		t.Error("fragment should not be rendered when the page selection is already invalid")
	}
}

func TestApply_InvalidMainPDFError(t *testing.T) {
	fr := &fakeFragmentRenderer{t: t}
	_, err := Apply(context.Background(), fr, []byte("garbage"), Spec{HTML: "<div>x</div>"}, baseLetter())
	if err == nil || !strings.Contains(err.Error(), "count main pages") {
		t.Fatalf("want a 'count main pages' error, got: %v", err)
	}
}

func TestApply_RendererErrorIsWrappedNotMasked(t *testing.T) {
	sentinel := errors.New("queue is full")
	fr := &fakeFragmentRenderer{t: t, failWith: sentinel}

	_, err := Apply(context.Background(), fr, makePDF(t, 1), Spec{HTML: "<div>x</div>"}, baseLetter())
	if err == nil {
		t.Fatal("Apply: want the renderer error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("renderer error must stay unwrapped for errors.Is at the HTTP boundary, got: %v", err)
	}
	if !strings.Contains(err.Error(), "render fragment") {
		t.Errorf("error should name the failing step, got: %v", err)
	}
}
