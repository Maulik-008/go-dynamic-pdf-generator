package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

// realPDF builds an n-page PDF in memory with pdfcpu (no Chromium) so the
// overlay handler's post-render pdfcpu step has parseable input in tests
// that use a fake renderer.
func realPDF(t *testing.T, n int) []byte {
	t.Helper()
	pages := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		pages = append(pages, fmt.Sprintf(
			`"%d":{"content":{"text":[{"value":"p%d","anchor":"center","font":{"name":"Helvetica","size":14}}]}}`, i, i))
	}
	var out bytes.Buffer
	if err := pdfcpuapi.Create(nil, strings.NewReader(`{"paper":"Letter","pages":{`+strings.Join(pages, ",")+`}}`), &out, nil); err != nil {
		t.Fatalf("realPDF(%d): %v", n, err)
	}
	return out.Bytes()
}

func TestHandleHTML_Overlay_WiresThroughAndSubstitutesToken(t *testing.T) {
	fr := &fakeRenderer{htmlPDF: realPDF(t, 2)}
	srv := NewServer(fr, nil, nil, nil)

	body := `{
		"content": "<h1>main</h1>",
		"options": { "overlay": { "html": "<div>page {{totalPages}}</div>", "pages": "last" } }
	}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.htmlCalls) != 2 {
		t.Fatalf("RenderHTML called %d times, want 2 (main + overlay fragment): %#v", len(fr.htmlCalls), fr.htmlCalls)
	}
	if fr.htmlCalls[0] != "<h1>main</h1>" {
		t.Errorf("first render should be the main content, got %q", fr.htmlCalls[0])
	}
	if fr.htmlCalls[1] != "<div>page 2</div>" {
		t.Errorf("fragment render should have {{totalPages}} replaced with 2, got %q", fr.htmlCalls[1])
	}
	fragOpts := fr.htmlOpts[1]
	if fragOpts.MarginTop != 0 || fragOpts.MarginBottom != 0 || fragOpts.DisplayHeaderFooter {
		t.Errorf("fragment opts should be zero-margin, no header/footer: %+v", fragOpts)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatal("response body is not a PDF")
	}
	if c, err := pdfcpuapi.PageCount(bytes.NewReader(rec.Body.Bytes()), nil); err != nil || c != 2 {
		t.Fatalf("stamped output page count = %d (err %v), want 2", c, err)
	}
}

func TestHandleHTML_Overlay_DefaultsToLastPageWhenPagesOmitted(t *testing.T) {
	fr := &fakeRenderer{htmlPDF: realPDF(t, 3)}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"<h1>m</h1>","options":{"overlay":{"html":"<div>d</div>"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.htmlCalls) != 2 {
		t.Fatalf("want 2 RenderHTML calls, got %d", len(fr.htmlCalls))
	}
}

func TestHandleHTML_Overlay_WithFitToPage(t *testing.T) {
	fr := &fakeRenderer{htmlPDF: realPDF(t, 1)}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"<h1>m</h1>","options":{"fitToPage":true,"overlay":{"html":"<div>{{totalPages}}</div>"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Fit-Scale") == "" {
		t.Error("X-Fit-Scale header should still be set when overlay follows a fitToPage render")
	}
	if len(fr.fittedCalls) != 1 || len(fr.htmlCalls) != 1 {
		t.Fatalf("want 1 fitted render + 1 fragment render, got fitted=%d html=%d", len(fr.fittedCalls), len(fr.htmlCalls))
	}
	if fr.htmlCalls[0] != "<div>1</div>" {
		t.Errorf("fragment token not substituted against the fitted 1-page doc, got %q", fr.htmlCalls[0])
	}
}

func TestHandleHTML_Overlay_PageOutOfRangeIs422(t *testing.T) {
	fr := &fakeRenderer{htmlPDF: realPDF(t, 2)}
	srv := NewServer(fr, nil, nil, nil)

	// "9" is grammatically valid, so it passes the 400 check; it only fails
	// once the real 2-page count is known, after the main render.
	body := `{"content":"<h1>m</h1>","options":{"overlay":{"html":"<div>d</div>","pages":"9"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "RENDER_ERROR" {
		t.Fatalf("error code = %q, want RENDER_ERROR", got)
	}
}

func TestHandleHTML_Overlay_EmptyHTMLRejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"<h1>m</h1>","options":{"overlay":{"pages":"last"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", got)
	}
	if len(fr.htmlCalls) != 0 {
		t.Error("no render should happen when the overlay is malformed")
	}
}

func TestHandleHTML_Overlay_BadPagesSelectorRejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"<h1>m</h1>","options":{"overlay":{"html":"<div>d</div>","pages":"middle"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.htmlCalls) != 0 {
		t.Error("no render should happen for an invalid page selector")
	}
}

func TestHandleMarkdown_Overlay_Rejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"# m","options":{"overlay":{"html":"<div>d</div>"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/markdown", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", got)
	}
	if len(fr.markdownCalls) != 0 {
		t.Error("markdown should not render when it also carries an unsupported overlay option")
	}
}

func TestHandleHTMLLite_Overlay_Rejected(t *testing.T) {
	fsr := &fakeStaticRenderer{}
	srv := NewServer(&fakeRenderer{}, fsr, nil, nil)

	body := `{"content":"<p>m</p>","options":{"overlay":{"html":"<div>d</div>"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html-lite", body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(fsr.calls) != 0 {
		t.Error("html-lite should not render when it also carries an unsupported overlay option")
	}
}
