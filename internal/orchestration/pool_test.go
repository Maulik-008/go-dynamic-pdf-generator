package orchestration

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maulik-008/go-dynamic-pdf-generator/internal/renderengines"
)

// blockingJob is a fake unit of work whose execution is fully controlled by
// the test: it signals started the instant it begins running (proving it
// was actually dispatched to a worker, not merely admitted into the queue),
// then blocks until release is closed.
type blockingJob struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingJob() *blockingJob {
	return &blockingJob{started: make(chan struct{}), release: make(chan struct{})}
}

func (j *blockingJob) fn(_ context.Context) (int, error) {
	close(j.started)
	<-j.release
	return 42, nil
}

func (j *blockingJob) hasStarted() bool {
	select {
	case <-j.started:
		return true
	default:
		return false
	}
}

const shortWait = 100 * time.Millisecond

// waitUntil polls cond every 2ms until it returns true or timeout elapses.
// Used in place of a fixed time.Sleep for "this should eventually become
// true" assertions, so these tests stay robust on a slower or more
// contended box instead of assuming a fixed duration is always enough —
// the same class of timing assumption that caused the real startup race
// this package's admission design had to fix (see pool.go's doc comment).
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return cond()
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSubmit_RunsAndReturnsResult(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 2})
	defer p.Close()

	got, err := Submit(context.Background(), p, func(context.Context) (string, error) {
		return "hello", nil
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestSubmit_PropagatesFnError(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 1})
	defer p.Close()

	wantErr := errors.New("boom")
	_, err := Submit(context.Background(), p, func(context.Context) (int, error) {
		return 0, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestPool_EnforcesConcurrencyCap proves, with a live in-flight counter
// (not inferred from timing), that no more than Workers jobs ever run
// simultaneously even when more than Workers jobs are admitted at once.
func TestPool_EnforcesConcurrencyCap(t *testing.T) {
	const workers = 2
	const totalJobs = 5
	p := NewPool(PoolConfig{Workers: workers, QueueCapacity: totalJobs})
	defer p.Close()

	var running int64
	var peak int64
	var wg sync.WaitGroup
	release := make(chan struct{})

	for i := 0; i < totalJobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = Submit(context.Background(), p, func(context.Context) (int, error) {
				n := atomic.AddInt64(&running, 1)
				for {
					old := atomic.LoadInt64(&peak)
					if n <= old || atomic.CompareAndSwapInt64(&peak, old, n) {
						break
					}
				}
				<-release
				atomic.AddInt64(&running, -1)
				return 0, nil
			})
		}()
	}

	// Exactly `workers` jobs should be running, the rest queued and not yet
	// started — poll rather than a fixed sleep so this stays robust under
	// scheduling delays instead of assuming a fixed settle time is enough.
	if !waitUntil(2*time.Second, func() bool { return atomic.LoadInt64(&running) == workers }) {
		t.Fatalf("running = %d, want %d (queued jobs must not start early)", atomic.LoadInt64(&running), workers)
	}

	close(release)
	wg.Wait()

	if peak > workers {
		t.Fatalf("peak concurrent jobs = %d, want <= %d", peak, workers)
	}
}

// TestPool_QueueFullRejectsImmediately fills every worker and queue slot
// with jobs blocked on a gate, then asserts the very next Submit is
// rejected with ErrQueueFull immediately (bounded by a short deadline, not
// hoped-for speed), and that a queued (not yet running) job does
// eventually run once a slot frees up.
func TestPool_QueueFullRejectsImmediately(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 1, QueueCapacity: 1})
	defer p.Close()

	job1 := newBlockingJob()
	job2 := newBlockingJob()
	done := make(chan struct{}, 2)

	go func() {
		_, _ = Submit(context.Background(), p, job1.fn)
		done <- struct{}{}
	}()
	select {
	case <-job1.started:
	case <-time.After(shortWait):
		t.Fatal("job1 never started running")
	}

	go func() {
		_, _ = Submit(context.Background(), p, job2.fn)
		done <- struct{}{}
	}()
	time.Sleep(shortWait) // give job2 a chance to be (wrongly) dispatched
	if job2.hasStarted() {
		t.Fatal("job2 started running while the single worker was still busy with job1")
	}

	// Worker (job1) + queue slot (job2) are both occupied now: the next
	// Submit must be rejected immediately, not block.
	start := time.Now()
	_, err := Submit(context.Background(), p, func(context.Context) (int, error) { return 0, nil })
	elapsed := time.Since(start)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if elapsed > shortWait {
		t.Fatalf("Submit took %s to reject; want an immediate, non-blocking rejection", elapsed)
	}

	close(job1.release)
	select {
	case <-job2.started:
	case <-time.After(shortWait):
		t.Fatal("job2 never started running after job1's slot freed up")
	}
	close(job2.release)

	<-done
	<-done
}

func TestSubmit_ContextCancelledWhileQueuedReturnsPromptly(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 1, QueueCapacity: 1})
	defer p.Close()

	job1 := newBlockingJob()
	go func() { _, _ = Submit(context.Background(), p, job1.fn) }()
	select {
	case <-job1.started:
	case <-time.After(shortWait):
		t.Fatal("job1 never started running")
	}

	job2 := newBlockingJob()
	ctx2, cancel2 := context.WithCancel(context.Background())
	submitDone := make(chan error, 1)
	go func() {
		_, err := Submit(ctx2, p, job2.fn)
		submitDone <- err
	}()
	// Let job2 be admitted into the queue slot (not yet running) before
	// cancelling — poll QueueDepth() rather than a fixed sleep.
	if !waitUntil(2*time.Second, func() bool { return p.QueueDepth() == 1 }) {
		t.Fatalf("job2 was never admitted into the queue (QueueDepth = %d, want 1)", p.QueueDepth())
	}
	cancel2()

	select {
	case err := <-submitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(shortWait):
		t.Fatal("Submit did not return promptly after ctx cancellation while queued")
	}

	// Clean up: job2 still runs to completion once dispatched (Submit
	// doesn't force-cancel fn); release both so the pool drains cleanly.
	close(job1.release)
	select {
	case <-job2.started:
	case <-time.After(shortWait):
		t.Fatal("job2 never started running after job1's slot freed up")
	}
	close(job2.release)
}

func TestPool_QueueDepthAndCapacityReportLiveState(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 1, QueueCapacity: 3})
	defer p.Close()

	if got := p.QueueCapacity(); got != 3 {
		t.Fatalf("QueueCapacity() = %d, want 3", got)
	}
	if got := p.QueueDepth(); got != 0 {
		t.Fatalf("QueueDepth() = %d, want 0 initially", got)
	}

	job1 := newBlockingJob()
	go func() { _, _ = Submit(context.Background(), p, job1.fn) }()
	select {
	case <-job1.started:
	case <-time.After(shortWait):
		t.Fatal("job1 never started running")
	}

	queued := []*blockingJob{newBlockingJob(), newBlockingJob()}
	var wg sync.WaitGroup
	for _, j := range queued {
		wg.Add(1)
		go func(j *blockingJob) {
			defer wg.Done()
			_, _ = Submit(context.Background(), p, j.fn)
		}(j)
	}
	if !waitUntil(2*time.Second, func() bool { return p.QueueDepth() == len(queued) }) {
		t.Fatalf("QueueDepth() = %d, want %d", p.QueueDepth(), len(queued))
	}

	close(job1.release)
	for _, j := range queued {
		close(j.release)
	}
	// Close (deferred) closes p.jobs; it must not run until every
	// in-flight Submit above has returned, per Pool.Close's documented
	// precondition — otherwise Close could race with one of these
	// goroutines' own send on p.jobs.
	wg.Wait()
}

// TestPool_InFlightIsTheSaturationSignal pins down why InFlight exists
// alongside QueueDepth. With every worker busy and the queue momentarily
// empty, the pool is 100% utilized and one arrival away from rejecting —
// yet QueueDepth reads 0, exactly as it does when the pool is idle. Any
// health check or autoscaler keyed on queue depth alone would report "all
// clear" at peak saturation.
func TestPool_InFlightIsTheSaturationSignal(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 2, QueueCapacity: 2})
	defer p.Close()

	if got := p.QueueDepth(); got != 0 {
		t.Fatalf("idle QueueDepth = %d, want 0", got)
	}
	if got := p.InFlight(); got != 0 {
		t.Fatalf("idle InFlight = %d, want 0", got)
	}
	if got := p.Capacity(); got != 4 {
		t.Fatalf("Capacity = %d, want 4 (2 workers + 2 queued)", got)
	}
	if got := p.Workers(); got != 2 {
		t.Fatalf("Workers = %d, want 2", got)
	}

	// Occupy exactly both workers, leaving the queue empty.
	jobs := []*blockingJob{newBlockingJob(), newBlockingJob()}
	for _, j := range jobs {
		go func(j *blockingJob) { _, _ = Submit(context.Background(), p, j.fn) }(j)
	}
	for i, j := range jobs {
		select {
		case <-j.started:
		case <-time.After(2 * time.Second):
			t.Fatalf("job %d never started", i)
		}
	}

	// The pool is now fully utilized...
	if !waitUntil(2*time.Second, func() bool { return p.InFlight() == 2 }) {
		t.Fatalf("InFlight = %d with both workers busy, want 2", p.InFlight())
	}
	// ...yet queue depth is indistinguishable from idle. This is the bug
	// InFlight exists to avoid, asserted rather than described.
	if got := p.QueueDepth(); got != 0 {
		t.Fatalf("QueueDepth = %d; this test's premise (depth reads 0 while saturated) no longer holds", got)
	}

	for _, j := range jobs {
		close(j.release)
	}
	if !waitUntil(2*time.Second, func() bool { return p.InFlight() == 0 }) {
		t.Fatalf("InFlight = %d after all jobs finished, want 0", p.InFlight())
	}
}

func TestPool_NewPoolAppliesDefaults(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 0, QueueCapacity: -5})
	defer p.Close()

	if got := p.QueueCapacity(); got != 0 {
		t.Fatalf("QueueCapacity() = %d, want 0 (negative config clamped)", got)
	}
	// Workers < 1 must still be usable (clamped to 1), not deadlocked.
	got, err := Submit(context.Background(), p, func(context.Context) (int, error) { return 7, nil })
	if err != nil || got != 7 {
		t.Fatalf("Submit = (%d, %v), want (7, nil)", got, err)
	}
}

func TestPool_CloseIsIdempotentAndDrainsQueuedWork(t *testing.T) {
	p := NewPool(PoolConfig{Workers: 1, QueueCapacity: 1})

	var ran int64
	_, err := Submit(context.Background(), p, func(context.Context) (int, error) {
		atomic.AddInt64(&ran, 1)
		return 0, nil
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	p.Close()
	p.Close() // must not panic or block

	if atomic.LoadInt64(&ran) != 1 {
		t.Fatalf("job did not run before Close")
	}
}

// TestPool_WithRealRenderEnginesPool proves the wrapper works against the
// real Chromium pool, not just fakes — skipped without CHROMIUM_PATH,
// matching the existing skip pattern used throughout this codebase.
func TestPool_WithRealRenderEnginesPool(t *testing.T) {
	path := os.Getenv("CHROMIUM_PATH")
	if path == "" {
		t.Skip("CHROMIUM_PATH not set; skipping real-Chromium orchestration test")
	}

	renderPool, err := renderengines.NewPool(renderengines.PoolConfig{
		Config:                    renderengines.Config{ChromiumPath: path},
		Size:                      1,
		MaxConcurrencyPerInstance: 2,
	})
	if err != nil {
		t.Fatalf("renderengines.NewPool: %v", err)
	}
	defer renderPool.Close()

	jobPool := NewPool(PoolConfig{Workers: 2, QueueCapacity: 2})
	defer jobPool.Close()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pdf, err := Submit(context.Background(), jobPool, func(ctx context.Context) ([]byte, error) {
				return renderPool.RenderHTML(ctx, "<html><body><h1>orchestrated</h1></body></html>", renderengines.DefaultRenderOptions())
			})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			if len(pdf) < 5 || string(pdf[:5]) != "%PDF-" {
				t.Errorf("result does not start with %%PDF-")
			}
		}()
	}
	wg.Wait()
}
