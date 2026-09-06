// Package api is the platform's public HTTP surface: one endpoint per
// conversion type, a single request envelope, and a single structured
// error envelope across all of them — see
// docs/planning/SPEC-conversion-api.md. Earlier module slices
// (render-engines, lightweight-render, job-orchestration,
// customization-layer) built this incrementally with a deliberately
// minimal, inconsistent HTTP shape (raw document bodies on some routes,
// JSON on others, plain-text errors); this module is where that gets
// unified into the platform's real, stable surface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/assets"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/customization"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/lightrender"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/orchestration"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/overlay"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// maxBodyBytes is a conservative placeholder ceiling for this slice.
// auth-and-tenancy owns any per-key limit policy later.
const maxBodyBytes = 10 << 20 // 10MB

// queueFullRetryAfterSeconds is a small fixed Retry-After hint for a
// 503 QUEUE_FULL response — a deliberate v1 simplification (see
// docs/planning/SPEC-job-orchestration.md), not derived from actual
// queue depth or engine-specific render latency.
const queueFullRetryAfterSeconds = "1"

// pdfRenderer is satisfied by *renderengines.Pool (and *renderengines.Renderer
// in tests) — kept minimal and interface-based so handler tests don't need a
// real Chromium instance.
type pdfRenderer interface {
	RenderHTML(ctx context.Context, html string, opts renderengines.RenderOptions) ([]byte, error)
	RenderMarkdown(ctx context.Context, markdown string, opts renderengines.RenderOptions) ([]byte, error)
	RenderHTMLFitted(ctx context.Context, html string, opts renderengines.RenderOptions) (renderengines.FittedRender, error)
}

// staticPdfRenderer is satisfied by *lightrender.Renderer — kept as its own,
// separate interface rather than folded into pdfRenderer, because
// lightrender.RenderOptions is deliberately a different type: this path
// must never share a code path with the Chromium renderer, per
// docs/planning/SPEC-lightweight-renderer.md's Boundaries.
type staticPdfRenderer interface {
	RenderHTML(ctx context.Context, html string, opts lightrender.RenderOptions) ([]byte, error)
}

// Server holds the HTTP handlers for every conversion endpoint.
type Server struct {
	renderer       pdfRenderer
	staticRenderer staticPdfRenderer // nil if lightweight-render isn't configured

	// jobPool and staticJobPool gate admission into renderer and
	// staticRenderer respectively (see internal/orchestration and
	// docs/planning/SPEC-job-orchestration.md) — two separate pools, so
	// one engine being saturated never rejects the other engine's
	// requests. A nil pool means "no admission gate, call the render
	// function directly" — kept so handler tests can exercise handler
	// logic against fakes without wiring an orchestration pool; real
	// deployments (cmd/api/main.go) always supply both.
	jobPool       *orchestration.Pool
	staticJobPool *orchestration.Pool

	// renderPool supplies live Chromium pool health to the readiness probe.
	// Optional: nil in handler tests that use a fake renderer, in which case
	// readiness simply omits the chromium block rather than inventing one.
	renderPool renderPoolHealth

	// renderDefaults is the deployment-level base every request's options
	// object is layered on top of (see options.go). Defaults to
	// renderengines.DefaultRenderOptions; overridden by WithRenderDefaults.
	renderDefaults renderengines.RenderOptions

	// imageEmbedding, when non-nil, allows options.embedImages to inline
	// remote <img> sources server-side before rendering. nil means the
	// feature is off and such a request is rejected — fetching URLs named in
	// a submitted document is SSRF surface and stays opt-in per deployment.
	imageEmbedding *assets.Config

	// shuttingDown makes readiness fail as soon as graceful shutdown starts,
	// so a load balancer drains this instance before in-flight work is
	// finished. Liveness deliberately stays up throughout.
	shuttingDown atomic.Bool
}

// ServerOption configures optional Server behaviour. Options exist so that
// production wiring can add capabilities (health reporting today, more
// later) without changing NewServer's signature for every existing caller —
// the additive-extension principle from api-and-interface-design.
type ServerOption func(*Server)

// WithRenderPoolHealth wires live Chromium pool statistics into the
// readiness probe.
func WithRenderPoolHealth(p renderPoolHealth) ServerOption {
	return func(s *Server) { s.renderPool = p }
}

// WithRenderDefaults sets the deployment-level base render options that a
// request's options object is layered on top of. Typically built from the
// PDF_DEFAULT_* env vars in cmd/api. Fields left at their
// DefaultRenderOptions value are unaffected.
func WithRenderDefaults(d renderengines.RenderOptions) ServerOption {
	return func(s *Server) { s.renderDefaults = d }
}

// WithImageEmbedding enables options.embedImages: remote <img> sources are
// fetched and inlined server-side before rendering, using cfg. Off unless
// called, because it is SSRF surface (see internal/assets).
func WithImageEmbedding(cfg assets.Config) ServerOption {
	return func(s *Server) { s.imageEmbedding = &cfg }
}

// BeginShutdown marks this instance as draining: readiness starts failing
// immediately, while liveness stays healthy. Call it before
// http.Server.Shutdown so load balancers stop sending new traffic while
// in-flight requests finish.
func (s *Server) BeginShutdown() { s.shuttingDown.Store(true) }

// NewServer wraps a pdfRenderer (typically a *renderengines.Pool) with HTTP
// handlers. staticRenderer is optional — pass nil if lightweight-render
// (WeasyPrint) isn't configured for this deployment; the /v1/pdf/html-lite
// route then responds 503 rather than panicking. jobPool and staticJobPool
// are optional admission-control gates (see orchestration.Pool) — pass nil
// for either to call the corresponding renderer directly, ungated.
func NewServer(renderer pdfRenderer, staticRenderer staticPdfRenderer, jobPool, staticJobPool *orchestration.Pool, opts ...ServerOption) *Server {
	s := &Server{
		renderer:       renderer,
		staticRenderer: staticRenderer,
		jobPool:        jobPool,
		staticJobPool:  staticJobPool,
		renderDefaults: renderengines.DefaultRenderOptions(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// submit runs fn directly if pool is nil, otherwise gates it through the
// pool's admission control (see orchestration.Submit).
func submit[T any](ctx context.Context, pool *orchestration.Pool, fn func(context.Context) (T, error)) (T, error) {
	if pool == nil {
		return fn(ctx)
	}
	return orchestration.Submit(ctx, pool, fn)
}

// Routes returns the HTTP handler for every conversion endpoint.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pdf/html", s.handleHTML)
	mux.HandleFunc("POST /v1/pdf/markdown", s.handleMarkdown)
	mux.HandleFunc("POST /v1/pdf/html-lite", s.handleHTMLLite)

	// Split probes: liveness answers "is this process wedged?" and readiness
	// answers "should this instance get traffic?". /healthz is kept as the
	// documented, backwards-compatible name and now maps to the readiness
	// semantics — previously it returned a bare 200 unconditionally and
	// verified nothing, so it could not detect a dead pool.
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /healthz", s.handleReady)
	return mux
}

// conversionRequest is the single request shape every conversion endpoint
// accepts. Payload is optional: when present, Content is treated as a
// template and merged via customization.Merge before rendering; when
// absent, Content is rendered as-is. Options is optional: it carries page
// setup / wait strategy / engine switches (see options.go). See
// docs/planning/SPEC-conversion-api.md.
type conversionRequest struct {
	Content string         `json:"content"`
	Payload map[string]any `json:"payload,omitempty"`
	Options *optionsInput  `json:"options,omitempty"`
}

// errorEnvelope is the single error shape every conversion endpoint
// returns — one consistent format instead of a plain-text http.Error body,
// per api-and-interface-design's "pick one error strategy and use it
// everywhere" principle.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSONError writes the single error envelope every conversion
// endpoint uses, per docs/planning/SPEC-conversion-api.md's status/code
// table.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// decodeConversionRequest reads, size-checks, and JSON-decodes the request
// body into the single conversionRequest shape every conversion endpoint
// shares, writing the appropriate INVALID_REQUEST/REQUEST_TOO_LARGE error
// and reporting false if anything is wrong.
func decodeConversionRequest(w http.ResponseWriter, r *http.Request) (conversionRequest, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "reading request body: "+err.Error())
		return conversionRequest{}, false
	}
	if len(body) > maxBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body too large")
		return conversionRequest{}, false
	}
	if len(body) == 0 {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must not be empty")
		return conversionRequest{}, false
	}

	var req conversionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "decoding request body: "+err.Error())
		return conversionRequest{}, false
	}
	if req.Content == "" {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", `"content" is required`)
		return conversionRequest{}, false
	}
	return req, true
}

// mergeIfNeeded applies customization.Merge when req.Payload is present —
// engine-agnostic plumbing shared by every conversion endpoint (Chromium
// and WeasyPrint alike), writing a TEMPLATE_ERROR and reporting false on
// failure.
func mergeIfNeeded(w http.ResponseWriter, req conversionRequest) (string, bool) {
	if req.Payload == nil {
		return req.Content, true
	}
	merged, err := customization.Merge(req.Content, req.Payload)
	if err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, "TEMPLATE_ERROR", err.Error())
		return "", false
	}
	return merged, true
}

// writeRenderError writes the appropriate error envelope for a render
// failure and reports whether one was written. orchestration.ErrQueueFull
// gets its own 503 QUEUE_FULL + Retry-After — distinct from RENDER_ERROR —
// so a client can tell "the system is busy, retry" apart from "your
// document failed to render."
func writeRenderError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, orchestration.ErrQueueFull) {
		w.Header().Set("Retry-After", queueFullRetryAfterSeconds)
		writeJSONError(w, http.StatusServiceUnavailable, "QUEUE_FULL", err.Error())
		return true
	}
	// "No healthy browser" is a server-side availability problem, not a
	// problem with the caller's document. Reporting it as RENDER_ERROR/422
	// tells the client their HTML is invalid and that retrying is pointless,
	// when in fact the document may be perfectly fine and the right response
	// is to retry shortly — the pool repairs itself. It also poisons the
	// error signal for operators, since a spike caused by dying browsers
	// would be indistinguishable from customers sending bad markup.
	if errors.Is(err, renderengines.ErrInstanceUnavailable) {
		w.Header().Set("Retry-After", queueFullRetryAfterSeconds)
		writeJSONError(w, http.StatusServiceUnavailable, "ENGINE_UNAVAILABLE", err.Error())
		return true
	}
	writeJSONError(w, http.StatusUnprocessableEntity, "RENDER_ERROR", "rendering pdf: "+err.Error())
	return true
}

// writePDF writes a successful conversion response — raw PDF bytes, not
// JSON-wrapped, matching the category convention (DocRaptor, PDFShift,
// cloudlayer.io all return raw PDF bytes on synchronous success) cited in
// docs/planning/SPEC-conversion-api.md.
func writePDF(w http.ResponseWriter, pdf []byte) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.Itoa(len(pdf)))
	w.WriteHeader(http.StatusOK)
	w.Write(pdf)
}

// routeCaps says which engine-adjacent options a route supports. embedImages
// and fitToPage are HTML-only: fitToPage measures a rendered DOM (there is
// no DOM before goldmark runs on the markdown path), and embedImages
// rewrites <img> tags that markdown image syntax hasn't produced yet.
// overlay is HTML-only for the same structural reason as fitToPage plus a
// practical one — it is a Chromium-rendered composition step, and the
// markdown and WeasyPrint paths have no equivalent notion.
type routeCaps struct {
	allowEmbedImages bool
	allowFitToPage   bool
	allowOverlay     bool
}

// gatedHTMLRenderer adapts the server's renderer + job pool to
// overlay.FragmentRenderer, so the overlay fragment render passes through
// the same admission control as any other Chromium render. It is called
// only after the main render's own Submit has returned, never nested
// inside it, so the two renders take an admission slot one at a time
// rather than deadlocking a saturated pool.
type gatedHTMLRenderer struct{ s *Server }

func (g gatedHTMLRenderer) RenderHTML(ctx context.Context, html string, opts renderengines.RenderOptions) ([]byte, error) {
	return submit(ctx, g.s.jobPool, func(ctx context.Context) ([]byte, error) {
		return g.s.renderer.RenderHTML(ctx, html, opts)
	})
}

// maybeOverlay applies spec to pdf when one was requested, returning pdf
// unchanged otherwise. base is the fully-resolved render options for the
// main document — the fragment is rendered at the same page size and
// orientation.
func (s *Server) maybeOverlay(ctx context.Context, pdf []byte, spec *overlay.Spec, base renderengines.RenderOptions) ([]byte, error) {
	if spec == nil {
		return pdf, nil
	}
	return overlay.Apply(ctx, gatedHTMLRenderer{s}, pdf, *spec, base)
}

// prepareRender runs the shared front half of every Chromium-backed route:
// decode -> resolve options against the deployment defaults -> merge the
// payload -> (optionally) inline remote images. On any failure it has
// already written the error envelope and returns ok=false. overlaySpec is
// non-nil only when the request asked for one and the route allows it.
func (s *Server) prepareRender(w http.ResponseWriter, r *http.Request, caps routeCaps) (content string, opts renderengines.RenderOptions, fitToPage bool, overlaySpec *overlay.Spec, ok bool) {
	req, ok := decodeConversionRequest(w, r)
	if !ok {
		return "", opts, false, nil, false
	}

	opts, embedImages, fitToPage, err := req.Options.resolve(s.renderDefaults)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return "", opts, false, nil, false
	}
	if fitToPage && !caps.allowFitToPage {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "options.fitToPage is only supported on /v1/pdf/html")
		return "", opts, false, nil, false
	}
	if embedImages && !caps.allowEmbedImages {
		writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "options.embedImages is only supported on /v1/pdf/html")
		return "", opts, false, nil, false
	}

	if req.Options != nil {
		overlaySpec, err = req.Options.Overlay.spec()
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return "", opts, false, nil, false
		}
		if overlaySpec != nil && !caps.allowOverlay {
			writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "options.overlay is only supported on /v1/pdf/html")
			return "", opts, false, nil, false
		}
	}

	content, ok = mergeIfNeeded(w, req)
	if !ok {
		return "", opts, false, nil, false
	}

	if embedImages {
		if s.imageEmbedding == nil {
			writeJSONError(w, http.StatusBadRequest, "INVALID_REQUEST", "options.embedImages is not enabled on this server")
			return "", opts, false, nil, false
		}
		embedded, stats := assets.Embed(r.Context(), content, *s.imageEmbedding)
		content = embedded
		if stats.Total > 0 {
			slog.InfoContext(r.Context(), "embedded remote images",
				"found", stats.Total, "embedded", stats.Embedded,
				"failed", stats.Failed, "skipped", stats.Skipped, "bytes", stats.Bytes)
		}
	}
	return content, opts, fitToPage, overlaySpec, true
}

func (s *Server) handleHTML(w http.ResponseWriter, r *http.Request) {
	content, opts, fitToPage, overlaySpec, ok := s.prepareRender(w, r, routeCaps{allowEmbedImages: true, allowFitToPage: true, allowOverlay: true})
	if !ok {
		return
	}

	if fitToPage {
		res, err := submit(r.Context(), s.jobPool, func(ctx context.Context) (renderengines.FittedRender, error) {
			return s.renderer.RenderHTMLFitted(ctx, content, opts)
		})
		if writeRenderError(w, err) {
			return
		}
		pdf, err := s.maybeOverlay(r.Context(), res.PDF, overlaySpec, opts)
		if writeRenderError(w, err) {
			return
		}
		// fitToPage returns raw PDF bytes like every other success, so the
		// scale actually applied travels in headers rather than a JSON body.
		w.Header().Set("X-Fit-Scale", strconv.FormatFloat(res.Scale, 'f', -1, 64))
		w.Header().Set("X-Fit-Overflow", strconv.FormatBool(res.Overflowed))
		writePDF(w, pdf)
		return
	}

	pdf, err := submit(r.Context(), s.jobPool, func(ctx context.Context) ([]byte, error) {
		return s.renderer.RenderHTML(ctx, content, opts)
	})
	if writeRenderError(w, err) {
		return
	}
	pdf, err = s.maybeOverlay(r.Context(), pdf, overlaySpec, opts)
	if writeRenderError(w, err) {
		return
	}
	writePDF(w, pdf)
}

func (s *Server) handleMarkdown(w http.ResponseWriter, r *http.Request) {
	content, opts, _, _, ok := s.prepareRender(w, r, routeCaps{})
	if !ok {
		return
	}
	pdf, err := submit(r.Context(), s.jobPool, func(ctx context.Context) ([]byte, error) {
		return s.renderer.RenderMarkdown(ctx, content, opts)
	})
	if writeRenderError(w, err) {
		return
	}
	writePDF(w, pdf)
}

// handleHTMLLite serves the separate, explicitly opt-in lightweight-render
// (WeasyPrint, no JavaScript) path. It shares the request envelope and merge
// behavior but keeps its own render call on a distinct code path, per
// docs/planning/SPEC-lightweight-renderer.md. Of the options envelope only
// timeoutMs is honored here — WeasyPrint has no concept of the Chromium
// page-setup fields, so paperSize/margin/header/footer are ignored on this
// route rather than silently half-applied.
func (s *Server) handleHTMLLite(w http.ResponseWriter, r *http.Request) {
	if s.staticRenderer == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "NOT_CONFIGURED",
			"lightweight (WeasyPrint) rendering is not configured on this server")
		return
	}

	content, opts, _, _, ok := s.prepareRender(w, r, routeCaps{})
	if !ok {
		return
	}

	pdf, err := submit(r.Context(), s.staticJobPool, func(ctx context.Context) ([]byte, error) {
		return s.staticRenderer.RenderHTML(ctx, content, lightrender.RenderOptions{Timeout: opts.Timeout})
	})
	if writeRenderError(w, err) {
		return
	}
	writePDF(w, pdf)
}
