package observability

// Per-request render trace: the data behind the `pdf_render` log record.
//
// The record exists so this service and the Node/Puppeteer service it is
// replacing can be compared request-for-request. Field names and phase names
// are therefore a contract shared with pdf-service-saas
// (utils/helpers/pdfTrace.js) — see docs/guides/09-render-logging.md before
// renaming anything.
//
// The trace rides in the request context. Every method is safe on a nil
// *Trace, so code that may run outside a request (tests, background work)
// records nothing and needs no nil checks.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"sync"
	"time"
)

// RenderInfo is the descriptive (non-timing) half of a pdf_render record.
type RenderInfo struct {
	ContentBytes  int
	ContentSHA256 string // first 16 hex chars; identifies the document across services

	PaperIn      string // e.g. "8.5x11"
	Landscape    bool
	FitToPage    bool
	EmbedImages  bool
	Overlay      bool
	OverlayPages string

	// Images is non-nil only when image embedding actually ran.
	Images *ImageInfo

	// OverlayReservedPx is the height of the spacer appended so the last-page
	// overlay cannot cover content; 0 when none was needed.
	OverlayReservedPx int

	PageCount int // pages of the Chromium output, before any overlay; 0 = unknown

	FitScale    *float64 // set only when fitToPage was requested
	FitOverflow *bool
}

// ImageInfo summarises one embedImages pass.
type ImageInfo struct {
	Found    int
	Embedded int
	Failed   int
	Skipped  int
	Bytes    int64
}

type traceState struct {
	mu     sync.Mutex
	phases map[string]time.Duration
	info   RenderInfo
	active bool
}

// Trace collects phase timings and descriptive fields for one request.
type Trace struct {
	state  *traceState
	prefix string // prepended to every phase name recorded through this view
}

// NewTrace returns an empty trace.
func NewTrace() *Trace {
	return &Trace{state: &traceState{phases: make(map[string]time.Duration)}}
}

type traceKey struct{}

// WithTrace stores t in ctx.
func WithTrace(ctx context.Context, t *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom returns the trace carried by ctx, or nil.
func TraceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// Scoped returns a view of the same trace that prefixes every phase name.
// The overlay fragment render runs the same engine code as the main render;
// scoping keeps its navigate/print_pdf timings from being added into the main
// document's.
func (t *Trace) Scoped(prefix string) *Trace {
	if t == nil {
		return nil
	}
	return &Trace{state: t.state, prefix: t.prefix + prefix}
}

// Start begins timing phase and returns the function that stops it. Repeated
// phases accumulate.
func (t *Trace) Start(phase string) func() {
	if t == nil {
		return func() {}
	}
	begin := time.Now()
	return func() { t.Add(phase, time.Since(begin)) }
}

// Add records d against phase.
func (t *Trace) Add(phase string, d time.Duration) {
	if t == nil {
		return
	}
	t.state.mu.Lock()
	t.state.phases[t.prefix+phase] += d
	t.state.active = true
	t.state.mu.Unlock()
}

// Update mutates the descriptive fields under the trace's lock.
func (t *Trace) Update(fn func(*RenderInfo)) {
	if t == nil {
		return
	}
	t.state.mu.Lock()
	fn(&t.state.info)
	t.state.active = true
	t.state.mu.Unlock()
}

// Active reports whether anything was recorded — i.e. whether this request
// was a render and so deserves a pdf_render record.
func (t *Trace) Active() bool {
	if t == nil {
		return false
	}
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	return t.state.active
}

// snapshot copies the trace for logging.
func (t *Trace) snapshot() (map[string]float64, RenderInfo) {
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	out := make(map[string]float64, len(t.state.phases))
	for k, d := range t.state.phases {
		out[k] = roundMs(d)
	}
	return out, t.state.info
}

// roundMs converts to milliseconds with 0.1ms resolution: renders here can be
// tens of milliseconds, where whole-millisecond rounding hides real differences.
func roundMs(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*10) / 10
}

// ContentFingerprint returns the byte length and a short SHA-256 of content.
// Both services hash the raw `content` string the caller sent (before image
// embedding), so the same document produces the same fingerprint everywhere.
func ContentFingerprint(content string) (int, string) {
	sum := sha256.Sum256([]byte(content))
	return len(content), hex.EncodeToString(sum[:8])
}

// pdfPageRe matches page objects but not the /Type /Pages tree node.
var pdfPageRe = regexp.MustCompile(`/Type\s*/Page(?:[^a-zA-Z]|$)`)

// CountPDFPages counts pages of a Chromium-produced PDF without parsing it
// (parsing a large document would add measurable time to the very thing being
// measured). It relies on Skia emitting page dictionaries uncompressed, which
// holds for Chromium's output but NOT for a PDF that has been re-saved with
// object streams (e.g. after an overlay stamp) — call it on the engine output,
// before any overlay. Returns 0 when it cannot tell.
func CountPDFPages(pdf []byte) int {
	return len(pdfPageRe.FindAllIndex(pdf, -1))
}
