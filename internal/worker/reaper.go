package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"smartdialer/internal/store"
)

// ReaperConfig configures stale call reclamation thresholds.
type ReaperConfig struct {
	ScanInterval     time.Duration
	ReservedTimeout  time.Duration // Stuck RESERVED or INITIATED threshold (e.g. 10s)
	AnsweredTimeout  time.Duration // Stuck ANSWERED without CONNECTED threshold (e.g. 5s)
}

func DefaultReaperConfig() ReaperConfig {
	return ReaperConfig{
		ScanInterval:    1 * time.Second,
		ReservedTimeout: 10 * time.Second,
		AnsweredTimeout: 5 * time.Second,
	}
}

// Reaper identifies and reconciles abandoned or hung calls from crashed workers.
type Reaper struct {
	store   store.Store
	cfg     ReaperConfig
	logger  *slog.Logger
	mu      sync.Mutex
	running bool
}

func NewReaper(st store.Store, cfg ReaperConfig, logger *slog.Logger) *Reaper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reaper{
		store:  st,
		cfg:    cfg,
		logger: logger,
	}
}

// ReapOnce executes a single scan-and-reclaim cycle.
func (r *Reaper) ReapOnce(ctx context.Context) (int, error) {
	stuckCalls, err := r.store.GetStuckCalls(ctx, r.cfg.ReservedTimeout, r.cfg.AnsweredTimeout)
	if err != nil {
		return 0, fmt.Errorf("failed to scan stuck calls: %w", err)
	}

	reclaimed := 0
	for _, call := range stuckCalls {
		select {
		case <-ctx.Done():
			return reclaimed, ctx.Err()
		default:
		}

		reason := fmt.Sprintf("stale_call_reaped: state=%s age > threshold", call.State)
		if err := r.store.RecoverStuckCall(ctx, call.ID, reason); err != nil {
			r.logger.Error("failed to recover stuck call", "call_id", call.ID, "err", err)
			continue
		}
		reclaimed++
		r.logger.Warn("reclaimed stuck call", "call_id", call.ID, "state", call.State, "worker_id", call.WorkerID)
	}

	return reclaimed, nil
}

// Start launches the periodic background reclamation loop until ctx cancellation.
func (r *Reaper) Start(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	ticker := time.NewTicker(r.cfg.ScanInterval)
	defer ticker.Stop()

	r.logger.Info("stale-state reaper started",
		"reserved_timeout", r.cfg.ReservedTimeout,
		"answered_timeout", r.cfg.AnsweredTimeout,
		"interval", r.cfg.ScanInterval,
	)

	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			r.logger.Info("stale-state reaper stopped")
			return
		case <-ticker.C:
			if _, err := r.ReapOnce(ctx); err != nil && ctx.Err() == nil {
				r.logger.Error("reaper cycle failed", "err", err)
			}
		}
	}
}
