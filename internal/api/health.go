package api

import (
	"encoding/json"
	"net/http"

	"github.com/Maulik-zuru/great-pdf-generator/internal/renderengines"
)

// renderPoolHealth is the health-reporting slice of *renderengines.Pool,
// kept as a tiny interface so handler tests can supply any pool state
// without killing real browsers.
type renderPoolHealth interface {
	Stats() renderengines.PoolStats
}

// healthResponse is the body returned by the probe endpoints. Everything
// optional is omitted rather than zero-filled, so a caller can tell "not
// wired up" apart from "wired up and reporting zero".
type healthResponse struct {
	Status            string         `json:"status"` // ok | degraded | shutting_down
	Chromium          *chromiumBlock `json:"chromium,omitempty"`
	Queue             *queueBlock    `json:"queue,omitempty"`
	LightweightRender lightweight    `json:"lightweight_render"`
}

type chromiumBlock struct {
	Size int `json:"size"`
	// Alive is the number of instances with a live browser right now.
	Alive int `json:"alive"`
	// Restarts and RestartAttempts together are the key diagnostic:
	// attempts climbing while restarts stay flat means Chromium cannot be
	// started at all (bad path, OOM, full disk), which no amount of
	// retrying will fix.
	Restarts        uint64 `json:"restarts"`
	RestartAttempts uint64 `json:"restart_attempts"`
	// HangsDetected counts rebuilds triggered by an instance that stopped
	// answering CDP while still looking alive. Reported separately from
	// crashes because the causes differ: crashes point at memory or browser
	// bugs, hangs at CPU starvation or a wedged message loop.
	HangsDetected uint64 `json:"hangs_detected"`
}

type queueBlock struct {
	// InFlight is admitted work (running + queued) and is the real
	// saturation signal; Depth alone reads 0 both when idle and when every
	// worker is busy with a momentarily empty queue. See
	// orchestration.Pool.InFlight.
	InFlight int `json:"in_flight"`
	Depth    int `json:"depth"`
	Capacity int `json:"capacity"`
	Workers  int `json:"workers"`
}

type lightweight struct {
	Configured bool `json:"configured"`
}

// handleLive answers the liveness probe: is this process itself still
// functioning? It is deliberately dependency-free and returns 200 whenever
// the HTTP server can serve at all — including while Chromium is down and
// while the service is draining.
//
// Making liveness depend on Chromium would be actively harmful: an
// orchestrator reacting to a failed liveness probe SIGKILLs the process,
// which would destroy the pool's own in-progress self-repair (see
// renderengines crash recovery) and convert a recoverable fault into a
// restart loop. Readiness is the probe that reflects dependency health.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:            "ok",
		LightweightRender: lightweight{Configured: s.staticRenderer != nil},
	})
}

// handleReady answers the readiness probe: should this instance receive
// traffic right now?
//
// It reports not-ready in exactly two cases: the service is shutting down,
// or it has a render pool with zero live instances (it cannot render
// anything at all). Notably it does NOT report not-ready merely because the
// service is busy — see the extended reasoning on the saturation decision in
// docs/research/observability-research.md and the test
// TestReadyz_DoesNotFailWhenMerelySaturated. Overload is already handled at
// the right layer, per-request, with 503 QUEUE_FULL + Retry-After.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	resp := healthResponse{
		Status:            "ok",
		LightweightRender: lightweight{Configured: s.staticRenderer != nil},
	}
	status := http.StatusOK

	if s.renderPool != nil {
		st := s.renderPool.Stats()
		resp.Chromium = &chromiumBlock{
			Size:            st.Size,
			Alive:           st.Alive,
			Restarts:        st.Restarts,
			RestartAttempts: st.RestartAttempts,
			HangsDetected:   st.HangsDetected,
		}
		if st.Alive == 0 {
			resp.Status = "degraded"
			status = http.StatusServiceUnavailable
		}
	}

	if s.jobPool != nil {
		resp.Queue = &queueBlock{
			InFlight: s.jobPool.InFlight(),
			Depth:    s.jobPool.QueueDepth(),
			Capacity: s.jobPool.Capacity(),
			Workers:  s.jobPool.Workers(),
		}
	}

	// Shutdown wins over everything else: once draining, this instance must
	// stop advertising readiness even if it is otherwise perfectly healthy.
	if s.shuttingDown.Load() {
		resp.Status = "shutting_down"
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
