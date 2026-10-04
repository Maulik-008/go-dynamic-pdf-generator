// Package observability provides the service's structured logging and
// request instrumentation — the "you can see what production is doing" half
// of running this service for real. See docs/research/observability-research.md
// for the sourcing behind these choices.
//
// It is deliberately stdlib-only (log/slog, crypto/rand, net/http). That is
// not a compromise: slog covers structured JSON logging outright, and the
// alternative for metrics (prometheus/client_golang) would pull roughly four
// times this repo's entire dependency tree — including protobuf, oauth2 and
// JWT — to serve a service whose queue-depth signal can be exported without
// it. The research names the specific conditions under which that dependency
// would become justified.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"time"
)

// requestIDHeader is both read (to honour a caller's own correlation id) and
// written (so a client can quote it when reporting a problem).
const requestIDHeader = "X-Request-Id"

// validRequestID is the whitelist an inbound request id must satisfy before
// it is trusted. An inbound header is untrusted input that ends up in log
// records, so a value containing newlines could forge additional log lines;
// unbounded length would let a caller bloat every record. Envoy takes the
// same posture — sanitize at the edge, preserve internally.
//
// slog's JSONHandler independently defuses log forging by escaping control
// characters inside JSON strings, so this check is defence in depth rather
// than the only barrier.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ctxKey is unexported so no other package can collide with, or overwrite,
// the value stored under it.
type ctxKey struct{}

// RequestIDFrom returns the request id carried by ctx, or "" if there is
// none (e.g. a background context, or code not running under a request).
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// newRequestID returns a fresh, random request id.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on any supported
		// platform since Go 1.24; fall back to a timestamp rather than
		// panicking in a logging path, which must never take the service
		// down.
		return "ts-" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// responseRecorder captures the status code and body size actually sent, so
// the access log reports what the client really received.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool

	// errBody keeps the first bytes of an error response so the pdf_render
	// record can report the error code the client actually received, without
	// every handler call site having to pass it up.
	errBody []byte
}

const maxErrBodyCapture = 2048

func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		// net/http ignores a second WriteHeader; the recorder must agree,
		// or the log will disagree with the response the client got.
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		// An implicit 200: net/http sends one when a handler writes a body
		// without calling WriteHeader.
		r.WriteHeader(http.StatusOK)
	}
	if r.status >= http.StatusBadRequest && len(r.errBody) < maxErrBodyCapture {
		room := maxErrBodyCapture - len(r.errBody)
		if len(b) < room {
			room = len(b)
		}
		r.errBody = append(r.errBody, b[:room]...)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter,
// so wrapping does not hide optional behaviour (Flush, Hijack, deadlines)
// from handlers. This is the Go 1.20+ answer to the older approach of
// hand-implementing every optional interface combination.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestLogger returns middleware that assigns each request a correlation
// id and logs one structured record per completed request.
//
// Before this existed the service logged nothing per request — only startup
// and shutdown — so a failure in production left no trace at all.
//
// Render requests additionally produce one `pdf_render` record (see
// logRender), written to renderLog when WithRenderLog is given and to logger
// otherwise.
func RequestLogger(logger *slog.Logger, opts ...Option) func(http.Handler) http.Handler {
	cfg := options{renderLog: logger}
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			clientSupplied := validRequestID.MatchString(id)
			if !clientSupplied {
				id = newRequestID()
			}

			// Echo it back so a user reporting "request X failed" gives us
			// the exact key to find that request in the logs.
			w.Header().Set(requestIDHeader, id)

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			// Keep a handle on the exact request handed downstream: net/http
			// records the matched route on *that* value, not on the outer
			// one. Verified directly — after ServeHTTP returns, the outer
			// request's Pattern is still "" while the passed-down copy's is
			// "POST /v1/pdf/html". Reading the outer one yields a route
			// label that is silently always empty.
			trace := NewTrace()
			routed := r.WithContext(WithTrace(context.WithValue(r.Context(), ctxKey{}, id), trace))

			start := time.Now()
			next.ServeHTTP(rec, routed)
			elapsed := time.Since(start)

			attrs := []any{
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", elapsed.Milliseconds(),
			}
			// The matched route (e.g. "POST /v1/pdf/html") is a
			// low-cardinality label, unlike the raw path — useful for
			// grouping. It is only populated once routing has happened, so
			// it must be read after ServeHTTP returns, and from the routed
			// request (see above).
			if routed.Pattern != "" {
				attrs = append(attrs, "route", routed.Pattern)
			}
			if clientSupplied {
				attrs = append(attrs, "request_id_source", "client")
			}

			logger.LogAttrs(routed.Context(), levelFor(r.URL.Path, rec.status),
				"http_request", toAttrs(attrs)...)

			if trace.Active() {
				logRender(cfg.renderLog, routed.Context(), id, routed.Pattern, rec, elapsed, trace)
			}
		})
	}
}

// probePaths are the health endpoints, logged at debug so a probe running
// every few seconds does not bury real traffic. At one probe per 10s that is
// ~8.6k records per replica per day saying nothing happened; they remain
// available by setting LOG_LEVEL=debug when a probe itself is suspect.
var probePaths = map[string]bool{"/livez": true, "/readyz": true, "/healthz": true}

// levelFor picks the severity for a completed request.
//
// The distinction that matters: 503 in this service is always a deliberate,
// designed response — the admission queue shedding load (QUEUE_FULL), the
// instance draining during shutdown, WeasyPrint not being configured, or a
// degraded pool. Logging those as ERROR alongside genuine faults is how an
// ERROR level becomes noise that operators learn to ignore, so they are
// warnings. Any other 5xx is an unhandled fault and stays ERROR.
func levelFor(path string, status int) slog.Level {
	switch {
	case probePaths[path]:
		return slog.LevelDebug
	case status == http.StatusServiceUnavailable:
		return slog.LevelWarn
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// toAttrs converts the alternating key/value slice into typed slog.Attrs.
// LogAttrs is used rather than the variadic form because it avoids the
// silent "!BADKEY" failure mode when a key/value pair is accidentally
// unbalanced.
func toAttrs(kv []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		attrs = append(attrs, slog.Any(key, kv[i+1]))
	}
	return attrs
}

// Option configures RequestLogger.
type Option func(*options)

type options struct {
	renderLog *slog.Logger
}

// WithRenderLog sends pdf_render records to l instead of the main logger —
// typically a logger that also writes a dedicated JSONL file so the records
// can be collected and compared without filtering the whole service log.
func WithRenderLog(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.renderLog = l
		}
	}
}

// Engine identifies this service in pdf_render records. The Node service logs
// "node-puppeteer"; the shared field is what lets the two be told apart.
const Engine = "go-chromedp"

// logRender emits the single `pdf_render` record for a completed render
// request. Its field and phase names are a contract with the Node service —
// see docs/guides/09-render-logging.md.
func logRender(l *slog.Logger, ctx context.Context, id, route string, rec *responseRecorder, elapsed time.Duration, t *Trace) {
	phases, info := t.snapshot()

	timings := make([]any, 0, len(phases)+1)
	timings = append(timings, slog.Float64("total", roundMs(elapsed)))
	names := make([]string, 0, len(phases))
	for k := range phases {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		timings = append(timings, slog.Float64(k, phases[k]))
	}

	outcome := "ok"
	if rec.status >= http.StatusBadRequest {
		outcome = "error"
	}

	attrs := []slog.Attr{
		slog.String("engine", Engine),
		slog.String("request_id", id),
		slog.String("route", route),
		slog.Int("status", rec.status),
		slog.String("outcome", outcome),
	}
	if outcome == "error" {
		code, msg := parseErrorEnvelope(rec.errBody)
		attrs = append(attrs, slog.String("error_code", code), slog.String("error_message", truncate(msg, 200)))
	}
	attrs = append(attrs,
		slog.Int("content_bytes", info.ContentBytes),
		slog.String("content_sha256", info.ContentSHA256),
		slog.Group("options",
			"paper_in", info.PaperIn,
			"landscape", info.Landscape,
			"fit_to_page", info.FitToPage,
			"embed_images", info.EmbedImages,
			"overlay", info.Overlay,
			"overlay_pages", info.OverlayPages,
		),
	)
	if img := info.Images; img != nil {
		attrs = append(attrs, slog.Group("images",
			"found", img.Found, "embedded", img.Embedded,
			"failed", img.Failed, "skipped", img.Skipped, "bytes", img.Bytes))
	}
	if outcome == "ok" {
		attrs = append(attrs, slog.Int("output_bytes", rec.bytes))
	}
	if info.PageCount > 0 {
		attrs = append(attrs, slog.Int("page_count", info.PageCount))
	}
	if info.OverlayReservedPx > 0 {
		attrs = append(attrs, slog.Int("overlay_reserved_px", info.OverlayReservedPx))
	}
	if info.FitScale != nil {
		attrs = append(attrs, slog.Float64("fit_scale", *info.FitScale))
	}
	if info.FitOverflow != nil {
		attrs = append(attrs, slog.Bool("fit_overflow", *info.FitOverflow))
	}
	attrs = append(attrs, slog.Group("timings_ms", timings...))

	l.LogAttrs(ctx, levelFor("", rec.status), "pdf_render", attrs...)
}

// parseErrorEnvelope extracts {"error":{"code","message"}} from a captured
// error body; both are empty if the body is not that shape.
func parseErrorEnvelope(body []byte) (code, message string) {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return "", ""
	}
	return env.Error.Code, env.Error.Message
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
