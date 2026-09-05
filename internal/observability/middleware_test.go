package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLogs returns a logger writing JSON into buf, plus a helper that
// decodes the single record written.
func captureLogs(t *testing.T) (*slog.Logger, func() map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, func() map[string]any {
		t.Helper()
		line := strings.TrimSpace(buf.String())
		if line == "" {
			t.Fatal("no log record was written")
		}
		// Take the last record if several were written.
		if i := strings.LastIndex(line, "\n"); i >= 0 {
			line = line[i+1:]
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v; line=%s", err, line)
		}
		return rec
	}
}

func TestRequestLogger_LogsCoreFields(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/pdf/html", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	rec := record()
	if got := rec["method"]; got != "POST" {
		t.Errorf("method = %v, want POST", got)
	}
	if got := rec["path"]; got != "/v1/pdf/html" {
		t.Errorf("path = %v, want /v1/pdf/html", got)
	}
	if got := rec["status"]; got != float64(http.StatusTeapot) {
		t.Errorf("status = %v, want %d", got, http.StatusTeapot)
	}
	if got := rec["bytes"]; got != float64(5) {
		t.Errorf("bytes = %v, want 5", got)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("duration_ms missing — request timing is the whole point of this log line")
	}
	if id, _ := rec["request_id"].(string); id == "" {
		t.Error("request_id missing or empty")
	}
}

// TestRequestLogger_DefaultsStatusTo200 covers the classic wrapper bug: a
// handler that writes a body without ever calling WriteHeader still results
// in a 200 on the wire, so the log must say 200 — not 0.
func TestRequestLogger_DefaultsStatusTo200(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("no explicit WriteHeader"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := record()["status"]; got != float64(http.StatusOK) {
		t.Fatalf("status = %v, want 200 for a handler that never called WriteHeader", got)
	}
}

// TestRequestLogger_RecordsOnlyFirstWriteHeader covers the second classic
// wrapper bug: net/http ignores a second WriteHeader, so the recorder must
// too, or the log will disagree with what the client actually received.
func TestRequestLogger_RecordsOnlyFirstWriteHeader(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusInternalServerError) // ignored by net/http
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := record()["status"]; got != float64(http.StatusNotFound) {
		t.Fatalf("status = %v, want 404 (the first WriteHeader wins)", got)
	}
}

// TestRequestLogger_PreservesResponseWriterInterfaces guards the pitfall
// that made pre-Go-1.20 middleware painful: naively wrapping a
// ResponseWriter hides optional interfaces like http.Flusher from the
// handler. Go 1.20's http.ResponseController resolves this via Unwrap, so
// the wrapper must implement it.
func TestRequestLogger_PreservesResponseWriterInterfaces(t *testing.T) {
	logger, _ := captureLogs(t)

	var flushOK bool
	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushOK = http.NewResponseController(w).Flush() == nil
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if !flushOK {
		t.Fatal("http.ResponseController could not Flush through the wrapper — Unwrap is missing or wrong")
	}
}

func TestRequestLogger_MakesRequestIDAvailableToHandler(t *testing.T) {
	logger, record := captureLogs(t)

	var seen string
	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if seen == "" {
		t.Fatal("handler could not read the request ID from its context")
	}
	if logged, _ := record()["request_id"].(string); logged != seen {
		t.Fatalf("handler saw request_id %q but the log recorded %q", seen, logged)
	}
}

func TestRequestLogger_EchoesRequestIDHeader(t *testing.T) {
	logger, _ := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := rec.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("X-Request-Id not echoed to the client — it is what makes a reported error traceable in logs")
	}
}

// TestRequestLogger_AcceptsWellFormedClientRequestID lets a caller correlate
// its own trace with ours.
func TestRequestLogger_AcceptsWellFormedClientRequestID(t *testing.T) {
	logger, record := captureLogs(t)

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "client-supplied_ID-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got, _ := record()["request_id"].(string); got != "client-supplied_ID-123" {
		t.Fatalf("request_id = %q, want the client-supplied value to be preserved", got)
	}
}

// TestRequestLogger_RejectsMalformedClientRequestID is the security half:
// an inbound header is untrusted input that lands in logs, so anything with
// newlines (log forging), control characters, or absurd length must be
// discarded in favour of a generated ID rather than trusted.
func TestRequestLogger_RejectsMalformedClientRequestID(t *testing.T) {
	for name, bad := range map[string]string{
		"newline injection":  "abc\nlevel=ERROR msg=forged",
		"carriage return":    "abc\rdef",
		"spaces":             "not a valid id",
		"too long":           strings.Repeat("a", 65),
		"empty":              "",
		"control characters": "abc\x00def",
	} {
		t.Run(name, func(t *testing.T) {
			logger, record := captureLogs(t)
			h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("X-Request-Id", bad)
			h.ServeHTTP(httptest.NewRecorder(), req)

			got, _ := record()["request_id"].(string)
			if got == bad && bad != "" {
				t.Fatalf("malformed client request id %q was trusted verbatim", bad)
			}
			if got == "" {
				t.Fatal("no request id was generated to replace the rejected one")
			}
		})
	}
}

// TestRequestLogger_LogsMatchedRoute guards a bug that fails silently
// rather than loudly: net/http records the matched route on the request it
// actually routed, not on the outer one the middleware received. Because
// this middleware passes a WithContext copy downstream, reading Pattern
// from the outer request yields "" on every single request — a route label
// that is always empty, with nothing to indicate it is broken. Verified
// directly before fixing: outer Pattern="" while the routed copy carried
// "POST /v1/pdf/html".
//
// The route matters because it is low-cardinality (unlike the raw path),
// which is what makes it usable for grouping and, later, metric labels.
func TestRequestLogger_LogsMatchedRoute(t *testing.T) {
	logger, record := captureLogs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pdf/html", func(w http.ResponseWriter, r *http.Request) {})

	RequestLogger(logger)(mux).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/pdf/html", nil),
	)

	got, _ := record()["route"].(string)
	if got != "POST /v1/pdf/html" {
		t.Fatalf("route = %q, want %q — the matched-route label is silently empty", got, "POST /v1/pdf/html")
	}
}

// TestRequestLogger_SeverityReflectsIntent guards against the failure mode
// where "ERROR" stops meaning anything. In this service a 503 is always a
// designed response — the queue shedding load, the instance draining, a
// dependency not configured — so logging it beside genuine faults would
// train an operator to ignore the level that actually matters.
func TestRequestLogger_SeverityReflectsIntent(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		status    int
		wantLevel string
	}{
		{"success is info", "/v1/pdf/html", http.StatusOK, "INFO"},
		{"client error is info", "/v1/pdf/html", http.StatusBadRequest, "INFO"},
		{"deliberate load shedding is a warning", "/v1/pdf/html", http.StatusServiceUnavailable, "WARN"},
		{"genuine fault is an error", "/v1/pdf/html", http.StatusInternalServerError, "ERROR"},
		{"readiness probe is debug", "/readyz", http.StatusOK, "DEBUG"},
		{"failing readiness probe is still debug", "/readyz", http.StatusServiceUnavailable, "DEBUG"},
		{"liveness probe is debug", "/livez", http.StatusOK, "DEBUG"},
		{"legacy healthz probe is debug", "/healthz", http.StatusOK, "DEBUG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, record := captureLogs(t)
			h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))

			if got := record()["level"]; got != tc.wantLevel {
				t.Fatalf("level = %v for %s %d, want %s", got, tc.path, tc.status, tc.wantLevel)
			}
		})
	}
}

// TestRequestLogger_ProbesAreSilentAtInfoLevel is the operational point of
// logging probes at debug: at the default level an orchestrator polling
// every few seconds must not bury real traffic.
func TestRequestLogger_ProbesAreSilentAtInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := RequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for i := 0; i < 5; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	}
	if buf.Len() != 0 {
		t.Fatalf("probe requests produced log output at info level: %s", buf.String())
	}

	// Real traffic must still be logged.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/pdf/html", nil))
	if buf.Len() == 0 {
		t.Fatal("real traffic was not logged at info level")
	}
}

func TestNewRequestID_IsUniqueAndWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := newRequestID()
		if !validRequestID.MatchString(id) {
			t.Fatalf("generated id %q does not match the accepted format", id)
		}
		if seen[id] {
			t.Fatalf("generated a duplicate request id %q", id)
		}
		seen[id] = true
	}
}

func TestRequestIDFrom_EmptyWhenAbsent(t *testing.T) {
	if got := RequestIDFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Fatalf("RequestIDFrom on a bare context = %q, want empty", got)
	}
}
