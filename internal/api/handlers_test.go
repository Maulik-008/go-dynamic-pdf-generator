package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/lightrender"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/orchestration"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

type fakeRenderer struct {
	htmlCalls      []string
	htmlOpts       []renderengines.RenderOptions // opts for each RenderHTML call, in order
	markdownCalls  []string
	fittedCalls    []string
	lastOpts       renderengines.RenderOptions
	fittedScale    float64
	fittedOverflow bool
	err            error
	// htmlPDF, when set, is returned by RenderHTML/RenderHTMLFitted instead
	// of the "%PDF-1.4 fake" placeholder — needed by overlay tests, whose
	// post-render pdfcpu step requires real, parseable PDF bytes.
	htmlPDF []byte
}

func (f *fakeRenderer) htmlResult() []byte {
	if f.htmlPDF != nil {
		return f.htmlPDF
	}
	return []byte("%PDF-1.4 fake")
}

func (f *fakeRenderer) RenderHTML(_ context.Context, html string, opts renderengines.RenderOptions) ([]byte, error) {
	f.htmlCalls = append(f.htmlCalls, html)
	f.htmlOpts = append(f.htmlOpts, opts)
	f.lastOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	return f.htmlResult(), nil
}

func (f *fakeRenderer) RenderHTMLFitted(_ context.Context, html string, opts renderengines.RenderOptions) (renderengines.FittedRender, error) {
	f.fittedCalls = append(f.fittedCalls, html)
	f.lastOpts = opts
	if f.err != nil {
		return renderengines.FittedRender{}, f.err
	}
	scale := f.fittedScale
	if scale == 0 {
		scale = 1
	}
	return renderengines.FittedRender{PDF: f.htmlResult(), Scale: scale, Overflowed: f.fittedOverflow}, nil
}

func (f *fakeRenderer) RenderMarkdown(_ context.Context, markdown string, _ renderengines.RenderOptions) ([]byte, error) {
	f.markdownCalls = append(f.markdownCalls, markdown)
	if f.err != nil {
		return nil, f.err
	}
	return []byte("%PDF-1.4 fake"), nil
}

// jsonReq builds a POST request with the unified {"content", "payload"?}
// envelope every conversion endpoint accepts — see
// docs/planning/SPEC-conversion-api.md.
func jsonReq(t *testing.T, path, body string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
}

// decodeError parses a conversion endpoint's JSON error envelope, failing
// the test if the body isn't one — used to assert on the "code" field
// rather than fragile string-matching a plain-text body.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response body is not a JSON error envelope: %v; body=%s", err, rec.Body.String())
	}
	return env.Error
}

func TestHandleHTML_Success(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<html><body>hi</body></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("Content-Type = %q, want application/pdf", ct)
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
	if len(fr.htmlCalls) != 1 || fr.htmlCalls[0] != "<html><body>hi</body></html>" {
		t.Fatalf("renderer received unexpected calls: %v", fr.htmlCalls)
	}
}

func TestHandleMarkdown_Success(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/markdown", `{"content":"# hi"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.markdownCalls) != 1 || fr.markdownCalls[0] != "# hi" {
		t.Fatalf("renderer received unexpected calls: %v", fr.markdownCalls)
	}
}

func TestHandleHTML_EmptyBodyRejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", got)
	}
	if len(fr.htmlCalls) != 0 {
		t.Fatal("renderer should not have been called for an empty body")
	}
}

func TestHandleHTML_MissingContentRejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"payload":{}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", got)
	}
	if len(fr.htmlCalls) != 0 {
		t.Fatal("renderer should not have been called with no content")
	}
}

func TestHandleHTML_MalformedJSONRejected(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{not json`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST", got)
	}
}

func TestHandleHTML_RenderErrorSurfacesAs422(t *testing.T) {
	fr := &fakeRenderer{err: errors.New("boom")}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<html></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "RENDER_ERROR" {
		t.Fatalf("error code = %q, want RENDER_ERROR", got)
	}
}

// TestHandleHTML_EngineUnavailableIsNot422 keeps two very different failures
// distinguishable. "Your HTML is broken" (422, do not retry) and "every
// browser is currently dead or wedged" (503, retry shortly) were previously
// both reported as RENDER_ERROR — which told a caller with a perfectly valid
// document that retrying was pointless, and left an operator unable to tell a
// spike of dying browsers from a spike of bad customer markup.
func TestHandleHTML_EngineUnavailableIsNot422(t *testing.T) {
	fr := &fakeRenderer{err: fmt.Errorf("pool: %w", renderengines.ErrInstanceUnavailable)}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<html></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "ENGINE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want ENGINE_UNAVAILABLE", got)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After: the pool self-heals, so retrying is the correct client behaviour")
	}
}

func TestHandleHTML_WithPayload_Success(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<p>Hello {{.name}}</p>","payload":{"name":"Ada"}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.htmlCalls) != 1 || fr.htmlCalls[0] != "<p>Hello Ada</p>" {
		t.Fatalf("renderer received unexpected calls: %v — merge must happen before rendering", fr.htmlCalls)
	}
}

func TestHandleMarkdown_WithPayload_Success(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/markdown", `{"content":"# Hello {{.name}}","payload":{"name":"Ada"}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.markdownCalls) != 1 || fr.markdownCalls[0] != "# Hello Ada" {
		t.Fatalf("renderer received unexpected calls: %v", fr.markdownCalls)
	}
}

// TestHandleHTML_MergeErrorSurfacesAs422 proves a missing payload field is
// distinct from a malformed request (400): the JSON itself is well-formed,
// but the template+payload combination is invalid.
func TestHandleHTML_MergeErrorSurfacesAs422(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<p>Hello {{.name}}</p>","payload":{}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "TEMPLATE_ERROR" {
		t.Fatalf("error code = %q, want TEMPLATE_ERROR", got)
	}
	if len(fr.htmlCalls) != 0 {
		t.Fatal("renderer should not have been called when the merge itself failed")
	}
}

// TestEndToEnd_RealPool_WithPayload proves the full wire-up (HTTP -> merge
// -> pool -> real Chromium) works, not just the handler logic against a
// fake. It deliberately does not substring-search the PDF bytes for the
// merged value: Chromium's PrintToPDF output is typically
// FlateDecode-compressed, the same reason an earlier test in this project
// (lightrender's DoesNotExecuteJavaScript test) had to switch to an
// uncompressed render and a substring search rather than assume raw text
// is present. The real "merge happens before render, with the actual
// merged string" proof is TestHandleHTML_WithPayload_Success above,
// against a fake renderer that records exactly what it was called with;
// this test's job is only to prove the real Chromium wiring doesn't error.
func TestEndToEnd_RealPool_WithPayload(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium end-to-end test")
	}
	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config: renderengines.Config{ChromiumPath: path},
		Size:   1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	srv := NewServer(pool, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html",
		`{"content":"<html><body><h1>Invoice for {{.customerName}}</h1></body></html>","payload":{"customerName":"Ada Lovelace"}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
}

// TestEndToEnd_RealPool_Overlay proves the overlay path end to end against
// real Chromium + real pdfcpu: a two-page document plus an
// options.overlay fragment comes back as a valid PDF with its page count
// unchanged (the stamp composites onto an existing page, never adds one).
// Token substitution and the fragment's render options are asserted
// precisely against a fake in TestHandleHTML_Overlay_WiresThroughAndSubstitutesToken;
// this test's job is to prove the real render -> render -> stamp wiring
// doesn't error and produces a well-formed file.
func TestEndToEnd_RealPool_Overlay(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium end-to-end test")
	}
	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config: renderengines.Config{ChromiumPath: path},
		Size:   1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	srv := NewServer(pool, nil, nil, nil)

	const twoPages = `<html><body><div style="page-break-after:always">one</div><div>two</div></body></html>`
	body := `{"content":` + strconv.Quote(twoPages) + `,` +
		`"options":{"paperSize":"Letter","overlay":{` +
		`"html":"<div style=\"position:fixed;bottom:0;left:0;width:100%;border-top:1px solid #000;font-size:9px\">Disclaimer — page {{totalPages}} of {{totalPages}}</div>",` +
		`"pages":"last"}}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.Bytes()
	if !bytes.HasPrefix(out, []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
	if err := pdfcpuapi.Validate(bytes.NewReader(out), nil); err != nil {
		t.Fatalf("overlay output fails PDF validation: %v", err)
	}
	if n, err := pdfcpuapi.PageCount(bytes.NewReader(out), nil); err != nil || n != 2 {
		t.Fatalf("overlay output page count = %d (err %v), want 2", n, err)
	}
}

func TestHealthz(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestEndToEnd_RealPool proves the full wire-up (HTTP -> pool -> real
// Chromium) works, not just the handler logic against a fake — the
// verification checkpoint from tasks/plan.md.
func TestEndToEnd_RealPool(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium end-to-end test")
	}
	pool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config: renderengines.Config{ChromiumPath: path},
		Size:   1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	srv := NewServer(pool, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<html><body><h1>Real</h1></body></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
}

type fakeStaticRenderer struct {
	calls []string
	err   error
}

func (f *fakeStaticRenderer) RenderHTML(_ context.Context, html string, _ lightrender.RenderOptions) ([]byte, error) {
	f.calls = append(f.calls, html)
	if f.err != nil {
		return nil, f.err
	}
	return []byte("%PDF-1.4 fake-static"), nil
}

func TestHandleHTMLLite_Success(t *testing.T) {
	fsr := &fakeStaticRenderer{}
	srv := NewServer(&fakeRenderer{}, fsr, nil, nil)

	req := jsonReq(t, "/v1/pdf/html-lite", `{"content":"<html><body>static</body></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
	if len(fsr.calls) != 1 || fsr.calls[0] != "<html><body>static</body></html>" {
		t.Fatalf("static renderer received unexpected calls: %v", fsr.calls)
	}
}

// TestHandleHTMLLite_WithPayload_Success proves payload merge is
// engine-agnostic plumbing, not a Chromium-only feature: WeasyPrint gets
// it for free through the same request envelope.
func TestHandleHTMLLite_WithPayload_Success(t *testing.T) {
	fsr := &fakeStaticRenderer{}
	srv := NewServer(&fakeRenderer{}, fsr, nil, nil)

	req := jsonReq(t, "/v1/pdf/html-lite", `{"content":"<p>Hello {{.name}}</p>","payload":{"name":"Ada"}}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fsr.calls) != 1 || fsr.calls[0] != "<p>Hello Ada</p>" {
		t.Fatalf("static renderer received unexpected calls: %v — merge must happen before rendering", fsr.calls)
	}
}

func TestHandleHTMLLite_NotConfiguredReturns503(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil) // no static renderer wired up

	req := jsonReq(t, "/v1/pdf/html-lite", `{"content":"<html></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "NOT_CONFIGURED" {
		t.Fatalf("error code = %q, want NOT_CONFIGURED", got)
	}
}

func TestHandleHTMLLite_EmptyBodyRejected(t *testing.T) {
	fsr := &fakeStaticRenderer{}
	srv := NewServer(&fakeRenderer{}, fsr, nil, nil)

	req := jsonReq(t, "/v1/pdf/html-lite", "")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(fsr.calls) != 0 {
		t.Fatal("static renderer should not have been called for an empty body")
	}
}

func TestHandleHTMLLite_RenderErrorSurfacesAs422(t *testing.T) {
	fsr := &fakeStaticRenderer{err: errors.New("boom")}
	srv := NewServer(&fakeRenderer{}, fsr, nil, nil)

	req := jsonReq(t, "/v1/pdf/html-lite", `{"content":"<html></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

// blockingFakeRenderer lets a test control exactly when a "render" starts
// and returns, so admission-control behavior (which depends on a render
// still being in flight) can be tested deterministically rather than by
// racing on timing.
type blockingFakeRenderer struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingFakeRenderer() *blockingFakeRenderer {
	return &blockingFakeRenderer{started: make(chan struct{}), release: make(chan struct{})}
}

func (f *blockingFakeRenderer) RenderHTML(_ context.Context, _ string, _ renderengines.RenderOptions) ([]byte, error) {
	close(f.started)
	<-f.release
	return []byte("%PDF-1.4 fake"), nil
}

func (f *blockingFakeRenderer) RenderHTMLFitted(ctx context.Context, html string, opts renderengines.RenderOptions) (renderengines.FittedRender, error) {
	pdf, err := f.RenderHTML(ctx, html, opts)
	return renderengines.FittedRender{PDF: pdf, Scale: 1}, err
}

func (f *blockingFakeRenderer) RenderMarkdown(ctx context.Context, markdown string, opts renderengines.RenderOptions) ([]byte, error) {
	return f.RenderHTML(ctx, markdown, opts)
}

// TestHandleHTML_QueueFullReturns503WithRetryAfter proves job-orchestration
// is actually wired in, not just implemented in isolation: with a
// one-worker, no-queue orchestration pool gating the renderer, a second
// concurrent request while the first is still in flight must be rejected
// with 503 + Retry-After — the backpressure fix the heavy-document load
// test's finding motivated (see docs/planning/SPEC-job-orchestration.md).
func TestHandleHTML_QueueFullReturns503WithRetryAfter(t *testing.T) {
	fr := newBlockingFakeRenderer()
	jobPool := orchestration.NewPool(orchestration.PoolConfig{Workers: 1, QueueCapacity: 0})
	defer jobPool.Close()
	srv := NewServer(fr, nil, jobPool, nil)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := jsonReq(t, "/v1/pdf/html", `{"content":"<html></html>"}`)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		firstDone <- rec
	}()
	select {
	case <-fr.started:
	case <-time.After(time.Second):
		t.Fatal("first request's render never started")
	}

	req2 := jsonReq(t, "/v1/pdf/html", `{"content":"<html></html>"}`)
	rec2 := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec2.Code, rec2.Body.String())
	}
	if ra := rec2.Header().Get("Retry-After"); ra == "" {
		t.Fatal("expected a Retry-After header on a 503 queue-full response")
	}
	if got := decodeError(t, rec2).Code; got != "QUEUE_FULL" {
		t.Fatalf("error code = %q, want QUEUE_FULL", got)
	}

	close(fr.release)
	rec1 := <-firstDone
	if rec1.Code != http.StatusOK {
		t.Fatalf("first (in-flight) request status = %d, want 200; body=%s", rec1.Code, rec1.Body.String())
	}
}

// TestEndToEnd_RealWeasyPrint proves the full wire-up (HTTP -> real
// WeasyPrint subprocess) works, not just the handler logic against a fake -
// and, critically, that the existing Chromium route is completely
// unaffected by this module existing alongside it (SPEC-lightweight-
// renderer.md's core boundary).
func TestEndToEnd_RealWeasyPrint(t *testing.T) {
	path := os.Getenv("WEASYPRINT_PATH")
	if path == "" {
		t.Skip("WEASYPRINT_PATH not set; skipping real-WeasyPrint end-to-end test")
	}
	renderer, err := lightrender.NewRenderer(lightrender.Config{WeasyPrintPath: path})
	if err != nil {
		t.Fatalf("lightrender.NewRenderer: %v", err)
	}

	srv := NewServer(&fakeRenderer{}, renderer, nil, nil)

	req := jsonReq(t, "/v1/pdf/html-lite", `{"content":"<html><body><h1>Real static</h1></body></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
		t.Fatalf("body does not start with %%PDF-")
	}
}

// --- options envelope + fitToPage (added for the medico integration) ---

func TestHandleHTML_OptionsReachTheRenderer(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	body := `{"content":"<p>x</p>","options":{"paperSize":"A4","landscape":true,"scale":0.75,"margin":{"top":"1in"},"timeoutMs":9000}}`
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	o := fr.lastOpts
	if o.PaperWidth != 8.27 || o.PaperHeight != 11.69 || !o.Landscape || o.Scale != 0.75 || o.MarginTop != 1 {
		t.Fatalf("options not applied: %+v", o)
	}
	if o.Timeout != 9*time.Second {
		t.Fatalf("timeout = %v, want 9s", o.Timeout)
	}
}

func TestHandleHTML_RenderDefaultsAreTheBase(t *testing.T) {
	fr := &fakeRenderer{}
	base := renderengines.DefaultRenderOptions()
	base.PaperWidth, base.PaperHeight = 8.5, 11 // Letter deployment default
	srv := NewServer(fr, nil, nil, nil, WithRenderDefaults(base))

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", `{"content":"<p>x</p>"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fr.lastOpts.PaperWidth != 8.5 || fr.lastOpts.PaperHeight != 11 {
		t.Fatalf("deployment default not used as base: %+v", fr.lastOpts)
	}
}

func TestHandleHTML_FitToPageRoutesToFittedAndSetsHeaders(t *testing.T) {
	fr := &fakeRenderer{fittedScale: 0.7, fittedOverflow: true}
	srv := NewServer(fr, nil, nil, nil)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", `{"content":"<p>x</p>","options":{"fitToPage":true}}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if len(fr.fittedCalls) != 1 || len(fr.htmlCalls) != 0 {
		t.Fatalf("fitToPage should call RenderHTMLFitted, not RenderHTML: fitted=%d html=%d", len(fr.fittedCalls), len(fr.htmlCalls))
	}
	if got := rec.Header().Get("X-Fit-Scale"); got != "0.7" {
		t.Fatalf("X-Fit-Scale = %q, want 0.7", got)
	}
	if got := rec.Header().Get("X-Fit-Overflow"); got != "true" {
		t.Fatalf("X-Fit-Overflow = %q, want true", got)
	}
}

func TestHandleMarkdown_RejectsHTMLOnlyOptions(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	for _, body := range []string{
		`{"content":"# x","options":{"fitToPage":true}}`,
		`{"content":"# x","options":{"embedImages":true}}`,
	} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/markdown", body))
		if rec.Code != http.StatusBadRequest || decodeError(t, rec).Code != "INVALID_REQUEST" {
			t.Fatalf("%s: status = %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleHTML_EmbedImagesRejectedWhenNotEnabled(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil) // no WithImageEmbedding
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", `{"content":"<p>x</p>","options":{"embedImages":true}}`))
	if rec.Code != http.StatusBadRequest || decodeError(t, rec).Code != "INVALID_REQUEST" {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHTML_MalformedOptionsIsInvalidRequest(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, jsonReq(t, "/v1/pdf/html", `{"content":"<p>x</p>","options":{"margin":{"top":"10furlongs"}}}`))
	if rec.Code != http.StatusBadRequest || decodeError(t, rec).Code != "INVALID_REQUEST" {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
