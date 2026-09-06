package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/orchestration"
	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// fakePoolHealth reports whatever pool state a test wants, so health
// behaviour can be exercised without killing real browsers.
type fakePoolHealth struct{ stats renderengines.PoolStats }

func (f fakePoolHealth) Stats() renderengines.PoolStats { return f.stats }

func getJSON(t *testing.T, srv *Server, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s body is not JSON: %v; body=%s", path, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

// TestLivez_StaysUpWhileDependenciesAreDown is the core liveness contract:
// liveness answers "is this process wedged?", not "are my dependencies
// healthy?". If it failed whenever Chromium was down, an orchestrator would
// SIGKILL the process — destroying the pool's own in-progress self-repair
// and turning a recoverable fault into a restart loop.
func TestLivez_StaysUpWhileDependenciesAreDown(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 2, Alive: 0}}))

	if code, _ := getJSON(t, srv, "/livez"); code != http.StatusOK {
		t.Fatalf("/livez = %d with every Chromium instance dead, want 200 — "+
			"liveness must not depend on dependency health", code)
	}
}

func TestReadyz_OKWhenPoolIsHealthy(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 2, Alive: 2}}))

	code, body := getJSON(t, srv, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", code)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}
	chromium, _ := body["chromium"].(map[string]any)
	if chromium == nil {
		t.Fatal("chromium block missing from readiness body")
	}
	if chromium["alive"] != float64(2) || chromium["size"] != float64(2) {
		t.Fatalf("chromium block = %v, want alive=2 size=2", chromium)
	}
}

// TestReadyz_FailsWhenNoInstanceIsAlive is the whole point of replacing the
// always-200 /healthz: a server that cannot render anything must stop
// claiming it is ready.
func TestReadyz_FailsWhenNoInstanceIsAlive(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 2, Alive: 0}}))

	code, body := getJSON(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d with zero live instances, want 503", code)
	}
	if body["status"] != "degraded" {
		t.Fatalf("status = %v, want degraded", body["status"])
	}
}

func TestReadyz_OKWhenPoolIsPartiallyDegraded(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 3, Alive: 1}}))

	if code, _ := getJSON(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d with 1 of 3 instances alive, want 200 — "+
			"reduced capacity still serves traffic", code)
	}
}

// TestReadyz_DoesNotFailWhenMerelySaturated encodes a deliberate, contested
// decision. A saturated replica that reported "not ready" would be pulled
// from the load balancer, shifting its traffic onto its peers and saturating
// them in turn — the classic cascading-failure mode. Envoy needs a dedicated
// panic threshold to defend against exactly this; Kubernetes has no
// equivalent. This service already sheds load correctly at the request level
// with 503 QUEUE_FULL + Retry-After, which is the right layer for it.
func TestReadyz_DoesNotFailWhenMerelySaturated(t *testing.T) {
	jobPool := orchestration.NewPool(orchestration.PoolConfig{Workers: 1, QueueCapacity: 0})
	defer jobPool.Close()

	// Fill the pool so it is at 100% admitted capacity.
	blocked := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _ = orchestration.Submit(context.Background(), jobPool, func(context.Context) (int, error) {
			close(started)
			<-blocked
			return 0, nil
		})
	}()
	<-started

	srv := NewServer(&fakeRenderer{}, nil, jobPool, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 1, Alive: 1}}))

	code, body := getJSON(t, srv, "/readyz")
	close(blocked)

	if code != http.StatusOK {
		t.Fatalf("/readyz = %d at full saturation, want 200 — failing readiness under load "+
			"sheds traffic onto peers and cascades", code)
	}
	queue, _ := body["queue"].(map[string]any)
	if queue == nil {
		t.Fatal("queue block missing from readiness body")
	}
	// Saturation must still be *reported*, even though it does not fail the check.
	if queue["in_flight"] != float64(1) {
		t.Fatalf("queue.in_flight = %v, want 1 — saturation must be visible even when tolerated", queue["in_flight"])
	}
}

// TestReadyz_FailsOnceShuttingDown is the highest-value probe behaviour:
// during graceful shutdown the server must stop advertising readiness so a
// load balancer drains it, while liveness stays up so nothing SIGKILLs it
// mid-drain.
func TestReadyz_FailsOnceShuttingDown(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 1, Alive: 1}}))

	if code, _ := getJSON(t, srv, "/readyz"); code != http.StatusOK {
		t.Fatalf("precondition failed: /readyz = %d before shutdown, want 200", code)
	}

	srv.BeginShutdown()

	code, body := getJSON(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d after BeginShutdown, want 503", code)
	}
	if body["status"] != "shutting_down" {
		t.Fatalf("status = %v, want shutting_down", body["status"])
	}
	if code, _ := getJSON(t, srv, "/livez"); code != http.StatusOK {
		t.Fatalf("/livez = %d during graceful shutdown, want 200 — "+
			"failing liveness here invites a SIGKILL mid-drain", code)
	}
}

// TestHealthz_RemainsAvailableAndIsNowMeaningful keeps the documented
// endpoint working while giving it real content — previously it returned a
// bare 200 unconditionally and checked nothing at all.
func TestHealthz_RemainsAvailableAndIsNowMeaningful(t *testing.T) {
	healthy := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 1, Alive: 1}}))
	if code, _ := getJSON(t, healthy, "/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d on a healthy server, want 200", code)
	}

	dead := NewServer(&fakeRenderer{}, nil, nil, nil,
		WithRenderPoolHealth(fakePoolHealth{renderengines.PoolStats{Size: 1, Alive: 0}}))
	if code, _ := getJSON(t, dead, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz = %d with no live instance, want 503 — "+
			"it must no longer report OK unconditionally", code)
	}
}

// TestReadyz_WithoutPoolHealthStillAnswers covers the handler-test wiring
// where no render pool is supplied: readiness must not crash, and must not
// silently claim health it cannot verify.
func TestReadyz_WithoutPoolHealthStillAnswers(t *testing.T) {
	srv := NewServer(&fakeRenderer{}, nil, nil, nil)
	code, body := getJSON(t, srv, "/readyz")
	if code != http.StatusOK {
		t.Fatalf("/readyz = %d with no pool wired, want 200", code)
	}
	if _, present := body["chromium"]; present {
		t.Fatal("chromium block should be absent when pool health is not wired, not fabricated")
	}
}

func TestReadyz_ReportsLightweightRenderConfiguration(t *testing.T) {
	off := NewServer(&fakeRenderer{}, nil, nil, nil)
	_, body := getJSON(t, off, "/readyz")
	lw, _ := body["lightweight_render"].(map[string]any)
	if lw == nil || lw["configured"] != false {
		t.Fatalf("lightweight_render = %v, want configured=false", lw)
	}

	on := NewServer(&fakeRenderer{}, &fakeStaticRenderer{}, nil, nil)
	_, body = getJSON(t, on, "/readyz")
	lw, _ = body["lightweight_render"].(map[string]any)
	if lw == nil || lw["configured"] != true {
		t.Fatalf("lightweight_render = %v, want configured=true", lw)
	}
}
