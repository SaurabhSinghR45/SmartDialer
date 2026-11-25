package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/allocator"
	"smartdialer/internal/pacing"
	"smartdialer/internal/safety"
	"smartdialer/internal/store"
)

// PoolStats holds thread-safe runtime telemetry for the worker pool.
type PoolStats struct {
	TotalProposed int64 `json:"total_proposed"`
	TotalApproved int64 `json:"total_approved"`
	TotalPlaced   int64 `json:"total_placed"`
	TotalFailed   int64 `json:"total_failed"`
}

// Pool manages N concurrent worker goroutines executing the strictly bounded dialing pipeline.
type Pool struct {
	concurrency int
	store       store.Store
	engine      pacing.PacingEngine
	controller  *safety.Controller
	allocator   *allocator.Allocator
	logger      *slog.Logger

	stats  PoolStats
	stopCh chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	active bool
}

func NewPool(
	concurrency int,
	st store.Store,
	engine pacing.PacingEngine,
	ctrl *safety.Controller,
	alloc *allocator.Allocator,
	logger *slog.Logger,
) *Pool {
	if concurrency <= 0 {
		concurrency = 4
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{
		concurrency: concurrency,
		store:       st,
		engine:      engine,
		controller:  ctrl,
		allocator:   alloc,
		logger:      logger,
		stopCh:      make(chan struct{}),
	}
}

// SetEngine dynamically swaps between Progressive and Predictive modes safely.
func (p *Pool) SetEngine(engine pacing.PacingEngine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.engine = engine
}

// Stats returns current atomic performance metrics.
func (p *Pool) Stats() PoolStats {
	return PoolStats{
		TotalProposed: atomic.LoadInt64(&p.stats.TotalProposed),
		TotalApproved: atomic.LoadInt64(&p.stats.TotalApproved),
		TotalPlaced:   atomic.LoadInt64(&p.stats.TotalPlaced),
		TotalFailed:   atomic.LoadInt64(&p.stats.TotalFailed),
	}
}

// Start launches the worker goroutines.
func (p *Pool) Start(ctx context.Context, campaignID uuid.UUID) {
	p.mu.Lock()
	if p.active {
		p.mu.Unlock()
		return
	}
	p.active = true
	p.stopCh = make(chan struct{})
	p.mu.Unlock()

	p.logger.Info("starting worker pool", "concurrency", p.concurrency, "campaign_id", campaignID)

	for i := 1; i <= p.concurrency; i++ {
		workerID := fmt.Sprintf("worker-%d", i)
		p.wg.Add(1)
		go p.runWorker(ctx, workerID, campaignID)
	}
}

func (p *Pool) runWorker(ctx context.Context, workerID string, campaignID uuid.UUID) {
	defer p.wg.Done()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.executeCycle(ctx, workerID, campaignID)
		}
	}
}

// executeCycle runs one iteration of the strictly bounded pipeline:
// Campaign -> Pacing Engine -> Safety Controller -> Call Allocator -> Telecom Provider
func (p *Pool) executeCycle(ctx context.Context, workerID string, campaignID uuid.UUID) {
	// 1. Fetch real-time aggregate metrics
	metrics, err := p.store.GetPacingMetrics(ctx, campaignID)
	if err != nil {
		if ctx.Err() == nil {
			p.logger.Error("failed to query pacing metrics", "worker_id", workerID, "err", err)
		}
		return
	}

	p.mu.Lock()
	engine := p.engine
	p.mu.Unlock()

	// 2. Pacing Engine proposes dial rate
	proposal, err := engine.Propose(ctx, campaignID, metrics)
	if err != nil {
		p.logger.Error("pacing calculation failed", "err", err)
		return
	}
	atomic.AddInt64(&p.stats.TotalProposed, int64(proposal.ProposedCalls))

	if proposal.ProposedCalls <= 0 {
		return
	}

	// 3. Safety Controller evaluates invariants
	safetyResult, err := p.controller.Evaluate(ctx, proposal, metrics)
	if err != nil {
		p.logger.Error("safety evaluation failed", "err", err)
		return
	}
	atomic.AddInt64(&p.stats.TotalApproved, int64(safetyResult.ApprovedCalls))

	if safetyResult.ApprovedCalls <= 0 {
		return
	}

	// 4. Call Allocator locks DB resources and dispatches to Telecom Provider
	allocResults, err := p.allocator.AllocateAndDial(ctx, workerID, campaignID, safetyResult)
	if err != nil && ctx.Err() == nil {
		p.logger.Error("allocation error", "worker_id", workerID, "err", err)
	}

	for _, res := range allocResults {
		if res.Success {
			atomic.AddInt64(&p.stats.TotalPlaced, 1)
		} else {
			atomic.AddInt64(&p.stats.TotalFailed, 1)
		}
	}
}

// Stop initiates a clean graceful shutdown of all active worker goroutines.
func (p *Pool) Stop() {
	p.mu.Lock()
	if !p.active {
		p.mu.Unlock()
		return
	}
	p.active = false
	close(p.stopCh)
	p.mu.Unlock()

	p.wg.Wait()
	p.logger.Info("worker pool stopped cleanly")
}
