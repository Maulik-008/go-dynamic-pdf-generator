// Package orchestration implements a bounded admission-control layer in
// front of a render engine: a fixed number of worker goroutines plus a
// bounded queue, so requests beyond capacity are rejected immediately
// (ErrQueueFull) instead of piling up unboundedly. See
// docs/planning/SPEC-job-orchestration.md.
//
// This package is deliberately engine-agnostic — it only knows
// func(context.Context) (T, error) — so the same Pool type gates both
// renderengines.Pool (Chromium) and lightrender.Renderer (WeasyPrint) via
// two independently-sized instances, without either engine's code changing.
package orchestration

import (
	"context"
	"errors"
	"sync"
)

// ErrQueueFull is returned by Submit when both every worker and the queue
// are occupied — the caller should back off and retry later.
var ErrQueueFull = errors.New("orchestration: queue is full, try again later")

// PoolConfig configures a Pool.
type PoolConfig struct {
	// Workers is the number of jobs allowed to run concurrently. Values
	// below 1 are treated as 1.
	Workers int

	// QueueCapacity is how many jobs may wait beyond Workers before Submit
	// starts rejecting with ErrQueueFull. Values below 0 are treated as 0.
	// 0 is a legitimate, meaningful setting (no waiting line at all: admit
	// only if a worker is immediately free), not a sentinel for "unset".
	QueueCapacity int
}

// Pool is a fixed-size worker pool fronted by a bounded queue. Zero value is
// not usable; construct with NewPool.
//
// Two separate mechanisms do the work, deliberately not one:
//   - admission is a counting semaphore, capacity Workers+QueueCapacity,
//     gating the *total* number of jobs Submit will accept at once (running
//     plus queued) — this is the backpressure boundary ErrQueueFull enforces.
//   - jobs is the FIFO handoff to a fixed pool of Workers goroutines,
//     gating how many *run concurrently*. Its buffer is also sized
//     Workers+QueueCapacity, which is exactly the admission semaphore's
//     capacity, so a send to it can never block once admission has already
//     succeeded — no second, independent limit to keep in sync.
//
// A single channel sized QueueCapacity cannot express both "beyond Workers"
// and "immediately reject once full" correctly: once a worker picks a job
// off the channel, that job leaves the buffer while still running, which
// would let a QueueCapacity-only design over-admit past the intended total.
// It also races at startup — an unbuffered (QueueCapacity: 0) channel needs
// an already-scheduled worker for a non-blocking send to succeed at all, so
// the very first Submit after NewPool could be spuriously rejected before
// the worker goroutines finish starting. The semaphore has neither problem:
// its capacity always includes Workers' worth of headroom, and its count is
// only ever adjusted by Submit/completion, never implicitly by scheduling.
type Pool struct {
	admission chan struct{}
	jobs      chan func()
	queueCap  int
	workers   int
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewPool starts cfg.Workers worker goroutines immediately, ready to accept
// jobs via Submit.
func NewPool(cfg PoolConfig) *Pool {
	workers := cfg.Workers
	if workers < 1 {
		workers = 1
	}
	queueCapacity := cfg.QueueCapacity
	if queueCapacity < 0 {
		queueCapacity = 0
	}
	total := workers + queueCapacity

	p := &Pool{
		admission: make(chan struct{}, total),
		jobs:      make(chan func(), total),
		queueCap:  queueCapacity,
		workers:   workers,
	}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.wg.Done()
			for job := range p.jobs {
				job()
			}
		}()
	}
	return p
}

type submitResult[T any] struct {
	value T
	err   error
}

// Submit admits fn for execution if the pool has not reached
// Workers+QueueCapacity jobs already admitted (a single non-blocking
// channel send — no waiting to find out whether the system is saturated),
// then blocks until fn completes or ctx is done. Returns ErrQueueFull
// immediately, without blocking, once that many jobs are already admitted.
//
// If ctx is done before fn completes (including while fn is still queued,
// not yet running), Submit returns ctx.Err() promptly rather than waiting
// for fn to eventually run. fn keeps running to completion in the
// background in that case (Submit does not force-cancel it — that's fn's
// own responsibility, same as every render engine in this codebase already
// being ctx-driven); the result channel is buffered so that goroutine can
// always deliver its result and exit without blocking, even though nothing
// reads it anymore.
func Submit[T any](ctx context.Context, p *Pool, fn func(context.Context) (T, error)) (T, error) {
	var zero T

	select {
	case p.admission <- struct{}{}:
	default:
		return zero, ErrQueueFull
	}

	resultCh := make(chan submitResult[T], 1)
	job := func() {
		defer func() { <-p.admission }()
		v, err := fn(ctx)
		resultCh <- submitResult[T]{value: v, err: err}
	}
	p.jobs <- job // never blocks: capacity matches admission's, already claimed above

	select {
	case res := <-resultCh:
		return res.value, res.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// QueueDepth reports how many admitted jobs are currently waiting for a
// free worker (buffered in the dispatch channel, not yet picked up) — the
// autoscaling signal the capability map calls for.
func (p *Pool) QueueDepth() int { return len(p.jobs) }

// QueueCapacity reports the configured QueueCapacity.
func (p *Pool) QueueCapacity() int { return p.queueCap }

// Workers reports the configured number of concurrent workers.
func (p *Pool) Workers() int { return p.workers }

// InFlight reports how many jobs are currently admitted — running plus
// queued — out of a maximum of Workers+QueueCapacity.
//
// This, not QueueDepth, is the saturation signal. QueueDepth counts only
// jobs still waiting for a worker, so it reads 0 in two completely opposite
// situations: an idle pool, and a pool where every worker is busy but the
// queue happens to be momentarily empty — i.e. fully utilized and one
// arrival away from rejecting with ErrQueueFull. Anything alerting or
// autoscaling on queue depth alone would therefore see "all clear" at peak
// saturation.
func (p *Pool) InFlight() int { return len(p.admission) }

// Capacity reports the total number of jobs that may be admitted at once
// (Workers + QueueCapacity) — the denominator for InFlight.
func (p *Pool) Capacity() int { return cap(p.admission) }

// Close stops the worker goroutines once the queue drains and waits for
// them to exit. Safe to call more than once.
//
// Precondition: no concurrent Submit calls once Close begins — sending on a
// closed channel panics. cmd/api's shutdown ordering already guarantees
// this: httpServer.Shutdown drains every in-flight handler (and therefore
// every in-flight Submit) before any Pool.Close call runs, the same
// precondition already documented on renderengines.Renderer.Close.
func (p *Pool) Close() {
	p.closeOnce.Do(func() { close(p.jobs) })
	p.wg.Wait()
}
