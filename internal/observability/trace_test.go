package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestTrace_NilIsSafe(t *testing.T) {
	var tr *Trace
	tr.Start("x")()
	tr.Add("x", time.Second)
	tr.Update(func(*RenderInfo) { t.Fatal("update must not run on a nil trace") })
	if tr.Active() {
		t.Error("nil trace must not be active")
	}
	if tr.Scoped("a_") != nil {
		t.Error("scoping a nil trace must stay nil")
	}
}

func TestTrace_PhasesAccumulateAndScope(t *testing.T) {
	tr := NewTrace()
	tr.Add("render", 10*time.Millisecond)
	tr.Add("render", 5*time.Millisecond)
	tr.Scoped("overlay_").Add("render", 7*time.Millisecond)

	phases, _ := tr.snapshot()
	if phases["render"] != 15 {
		t.Errorf("render = %v, want 15 (accumulated)", phases["render"])
	}
	if phases["overlay_render"] != 7 {
		t.Errorf("overlay_render = %v, want 7 (scoped, not mixed into render)", phases["overlay_render"])
	}
}

func TestTrace_ContextRoundTrip(t *testing.T) {
	tr := NewTrace()
	if TraceFrom(WithTrace(context.Background(), tr)) != tr {
		t.Error("TraceFrom must return the stored trace")
	}
	if TraceFrom(context.Background()) != nil {
		t.Error("TraceFrom on a bare context must be nil")
	}
}

func TestContentFingerprint_StableAndShort(t *testing.T) {
	n, sum := ContentFingerprint("<p>hi</p>")
	if n != 9 {
		t.Errorf("bytes = %d, want 9", n)
	}
	if len(sum) != 16 {
		t.Errorf("sha = %q, want 16 hex chars", sum)
	}
	if _, again := ContentFingerprint("<p>hi</p>"); again != sum {
		t.Error("fingerprint must be deterministic")
	}
	// Multi-byte content is hashed/measured as UTF-8 bytes, matching Node's
	// Buffer.byteLength — this is what lets the two services agree.
	if n, _ := ContentFingerprint("é"); n != 2 {
		t.Errorf("é = %d bytes, want 2", n)
	}
}

func TestCountPDFPages(t *testing.T) {
	pdf := []byte("<</Type /Pages /Count 2 /Kids [1 0 R 2 0 R]>>\n<</Type /Page /Parent 3 0 R>>\n<</Type /Page\n/Parent 3 0 R>>\n<</Type/Page/Parent 3 0 R>>")
	if got := CountPDFPages(pdf); got != 3 {
		t.Errorf("pages = %d, want 3 (the /Pages tree node must not count)", got)
	}
	if got := CountPDFPages([]byte("%PDF- no pages")); got != 0 {
		t.Errorf("pages = %d, want 0", got)
	}
}

// Against a real Chromium-produced PDF, if one is on hand.
func TestCountPDFPages_RealFile(t *testing.T) {
	path := os.Getenv("TEST_PDF_FILE")
	want := os.Getenv("TEST_PDF_PAGES")
	if path == "" {
		t.Skip("TEST_PDF_FILE not set")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := CountPDFPages(b); want != "" && strconv.Itoa(got) != want {
		t.Errorf("pages = %d, want %s", got, want)
	}
}

func TestRequestLogger_EmitsPDFRenderRecord(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr := TraceFrom(r.Context())
		tr.Add("queue_wait", 2*time.Millisecond)
		tr.Add("render", 40*time.Millisecond)
		tr.Update(func(i *RenderInfo) {
			i.ContentBytes, i.ContentSHA256, i.PaperIn, i.PageCount = 9, "abcd", "8.5x11", 3
		})
		w.Write([]byte("%PDF"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/pdf/html", nil))

	rec := record() // last record = pdf_render
	if rec["msg"] != "pdf_render" {
		t.Fatalf("last record msg = %v, want pdf_render", rec["msg"])
	}
	if rec["engine"] != Engine || rec["outcome"] != "ok" || rec["status"] != float64(200) {
		t.Errorf("engine/outcome/status = %v/%v/%v", rec["engine"], rec["outcome"], rec["status"])
	}
	if rec["output_bytes"] != float64(4) || rec["page_count"] != float64(3) {
		t.Errorf("output_bytes/page_count = %v/%v", rec["output_bytes"], rec["page_count"])
	}
	timings, _ := rec["timings_ms"].(map[string]any)
	if timings["render"] != float64(40) || timings["queue_wait"] != float64(2) {
		t.Errorf("timings_ms = %v", timings)
	}
	if _, ok := timings["total"]; !ok {
		t.Error("timings_ms.total missing")
	}
}

func TestRequestLogger_PDFRenderCarriesErrorCode(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		TraceFrom(r.Context()).Add("request_parse", time.Millisecond)
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":{"code":"RENDER_ERROR","message":"rendering pdf: boom"}}`))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/pdf/html", nil))

	rec := record()
	if rec["outcome"] != "error" || rec["error_code"] != "RENDER_ERROR" || rec["error_message"] != "rendering pdf: boom" {
		t.Errorf("outcome/code/message = %v/%v/%v", rec["outcome"], rec["error_code"], rec["error_message"])
	}
	if _, ok := rec["output_bytes"]; ok {
		t.Error("output_bytes must be absent on an error response")
	}
}

func TestRequestLogger_NoPDFRenderRecordForNonRender(t *testing.T) {
	logger, record := captureLogs(t)
	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec := record(); rec["msg"] == "pdf_render" {
		t.Error("a request that recorded nothing must not emit pdf_render")
	}
}
