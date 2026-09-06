package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/lightrender"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// TestDecodeConversionRequest_BodyOneByteOverLimitRejected and its sibling
// below pin the REQUEST_TOO_LARGE boundary down to the exact byte, not just
// "some big body eventually gets rejected" — the size check in
// decodeConversionRequest is `len(body) > maxBodyBytes`, so the boundary
// itself (exactly maxBodyBytes) must NOT trip REQUEST_TOO_LARGE while
// maxBodyBytes+1 must. Neither had a test before this file: the existing
// suite only exercised small bodies.
func TestDecodeConversionRequest_BodyOneByteOverLimitRejected(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/pdf/html", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	_, ok := decodeConversionRequest(rec, req)
	if ok {
		t.Fatal("expected decodeConversionRequest to reject a body one byte over the limit")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if got := decodeError(t, rec).Code; got != "REQUEST_TOO_LARGE" {
		t.Fatalf("error code = %q, want REQUEST_TOO_LARGE", got)
	}
}

// TestDecodeConversionRequest_BodyExactlyAtLimitIsNotTooLarge proves the
// limit is not off-by-one in the *other* direction: a body of exactly
// maxBodyBytes must clear the size check (it may still fail for an
// unrelated reason — this body isn't valid JSON — but that failure must be
// INVALID_REQUEST, never REQUEST_TOO_LARGE).
func TestDecodeConversionRequest_BodyExactlyAtLimitIsNotTooLarge(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxBodyBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/pdf/html", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	_, ok := decodeConversionRequest(rec, req)
	if ok {
		t.Fatal("a body of raw 'a' bytes is not valid JSON and must not decode successfully")
	}
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a body of exactly maxBodyBytes (%d) was rejected as too large — the boundary is off by one", maxBodyBytes)
	}
	if got := decodeError(t, rec).Code; got != "INVALID_REQUEST" {
		t.Fatalf("error code = %q, want INVALID_REQUEST (malformed JSON, not a size problem)", got)
	}
}

// TestHandleHTML_RequestTooLargeReturns413 proves the full HTTP path (not
// just the decode helper in isolation) rejects an oversized real request and
// never reaches the renderer.
func TestHandleHTML_RequestTooLargeReturns413(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	oversized := fmt.Sprintf(`{"content":"%s"}`, strings.Repeat("a", maxBodyBytes))
	req := httptest.NewRequest(http.MethodPost, "/v1/pdf/html", strings.NewReader(oversized))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "REQUEST_TOO_LARGE" {
		t.Fatalf("error code = %q, want REQUEST_TOO_LARGE", got)
	}
	if len(fr.htmlCalls) != 0 {
		t.Fatal("renderer must not be invoked for a request rejected as too large")
	}
}

// TestRoutes_UnknownPathReturns404 and TestRoutes_WrongMethodReturns405
// pin down the routing behaviour at the edges of the mux — nothing in the
// existing suite exercised a path or method outside the five registered
// routes.
func TestRoutes_UnknownPathReturns404(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/pdf/does-not-exist", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRoutes_WrongMethodReturns405(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/pdf/html", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for GET against a POST-only route", rec.Code)
	}
}

// TestHandleHTML_ContentLengthHeaderMatchesBody guards against a header/body
// mismatch, which would otherwise silently truncate or hang a well-behaved
// HTTP/1.1 client reading exactly Content-Length bytes.
func TestHandleHTML_ContentLengthHeaderMatchesBody(t *testing.T) {
	fr := &fakeRenderer{}
	srv := NewServer(fr, nil, nil, nil)

	req := jsonReq(t, "/v1/pdf/html", `{"content":"<html><body>hi</body></html>"}`)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	wantLen := strconv.Itoa(rec.Body.Len())
	if got := rec.Header().Get("Content-Length"); got != wantLen {
		t.Fatalf("Content-Length header = %q, want %q (actual body length)", got, wantLen)
	}
}

// TestWriteJSONError_AlwaysSetsJSONContentType is a blanket check that every
// error path — not just one sampled case — sets Content-Type: application/json,
// per conversion-api's "one consistent error envelope" requirement. A plain
// http.Error-style regression would leave this as text/plain.
func TestWriteJSONError_AlwaysSetsJSONContentType(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		srv  *Server
	}{
		{"empty body", jsonReq(t, "/v1/pdf/html", ""), NewServer(&fakeRenderer{}, nil, nil, nil)},
		{"malformed JSON", jsonReq(t, "/v1/pdf/html", "{not json"), NewServer(&fakeRenderer{}, nil, nil, nil)},
		{"render error", jsonReq(t, "/v1/pdf/html", `{"content":"<p>x</p>"}`), NewServer(&fakeRenderer{err: fmt.Errorf("boom")}, nil, nil, nil)},
		{"not configured", jsonReq(t, "/v1/pdf/html-lite", `{"content":"<p>x</p>"}`), NewServer(&fakeRenderer{}, nil, nil, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.srv.Routes().ServeHTTP(rec, tc.req)
			if rec.Code < 400 {
				t.Fatalf("%s: status = %d, want an error status", tc.name, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("%s: Content-Type = %q, want application/json", tc.name, ct)
			}
		})
	}
}

// echoingFakeRenderer/echoingFakeStaticRenderer embed the input html
// verbatim into their "PDF" output, with no shared mutable state at all —
// deliberately different from fakeRenderer (which records call history in an
// unsynchronized slice, fine for the sequential tests it's used in, but not
// safe to share across concurrent goroutines without its own locking).
type echoingFakeRenderer struct{}

func (echoingFakeRenderer) RenderHTML(_ context.Context, html string, _ renderengines.RenderOptions) ([]byte, error) {
	return []byte("%PDF-1.4 " + html), nil
}
func (echoingFakeRenderer) RenderMarkdown(_ context.Context, markdown string, _ renderengines.RenderOptions) ([]byte, error) {
	return []byte("%PDF-1.4 " + markdown), nil
}
func (echoingFakeRenderer) RenderHTMLFitted(_ context.Context, html string, _ renderengines.RenderOptions) (renderengines.FittedRender, error) {
	return renderengines.FittedRender{PDF: []byte("%PDF-1.4 " + html), Scale: 1}, nil
}

type echoingFakeStaticRenderer struct{}

func (echoingFakeStaticRenderer) RenderHTML(_ context.Context, html string, _ lightrender.RenderOptions) ([]byte, error) {
	return []byte("%PDF-1.4 " + html), nil
}

// TestConcurrentMixedRoutes_NoCrossTalk fires many goroutines at all three
// conversion routes simultaneously, each with a unique marker in its
// request body, and verifies every response corresponds to its own request.
// This is the concurrency proof the existing suite lacked at the HTTP-handler
// layer: TestHandleHTML_QueueFullReturns503WithRetryAfter proves admission
// control with exactly two requests in a controlled sequence, but nothing
// exercised many concurrent, *distinct* requests across all three routes at
// once for cross-talk. Run with -race.
func TestConcurrentMixedRoutes_NoCrossTalk(t *testing.T) {
	srv := NewServer(echoingFakeRenderer{}, echoingFakeStaticRenderer{}, nil, nil)

	const n = 60 // 20 per route
	routes := []string{"/v1/pdf/html", "/v1/pdf/markdown", "/v1/pdf/html-lite"}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			route := routes[i%len(routes)]
			marker := fmt.Sprintf("marker-%d-%s", i, strings.TrimPrefix(route, "/v1/pdf/"))
			body := fmt.Sprintf(`{"content":"%s"}`, marker)
			req := jsonReq(t, route, body)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("route %s marker %s: status = %d, body=%s", route, marker, rec.Code, rec.Body.String())
				return
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(marker)) {
				t.Errorf("route %s: response does not contain its own marker %q — possible cross-talk", route, marker)
			}
		}(i)
	}
	wg.Wait()
}

// TestGracefulShutdown_InFlightRequestCompletesWhileReadyzReports503 proves
// the two documented shutdown behaviours actually compose at the Server
// level: BeginShutdown makes /readyz fail immediately (proven in isolation
// by TestReadyz_FailsOnceShuttingDown), and an in-flight request is not
// aborted (implied, but never directly proven together in one scenario).
// This is the exact sequence a real deployment relies on: begin draining,
// let the load balancer see 503 on /readyz, but still finish work already
// accepted.
func TestGracefulShutdown_InFlightRequestCompletesWhileReadyzReports503(t *testing.T) {
	fr := newBlockingFakeRenderer()
	srv := NewServer(fr, nil, nil, nil)

	reqDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := jsonReq(t, "/v1/pdf/html", `{"content":"<html></html>"}`)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		reqDone <- rec
	}()

	select {
	case <-fr.started:
	case <-time.After(time.Second):
		t.Fatal("in-flight request never started rendering")
	}

	srv.BeginShutdown()

	readyRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(readyRec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readyRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d after BeginShutdown while a request is in flight, want 503", readyRec.Code)
	}

	close(fr.release)

	select {
	case rec := <-reqDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("in-flight request status = %d after shutdown began, want 200 (already-admitted work must finish)", rec.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request never completed after being released")
	}
}
