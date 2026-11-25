package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/domain"
)

var (
	ErrNoAvailableAgent    = errors.New("no available agent found")
	ErrNoPendingBorrower   = errors.New("no pending borrower found")
	ErrCallNotFound        = errors.New("call not found")
	ErrAgentNotFound       = errors.New("agent not found")
	ErrBorrowerNotFound    = errors.New("borrower not found")
	ErrOptimisticLockFail  = errors.New("optimistic lock failure")
	ErrDuplicateEventFound = errors.New("duplicate event already processed")
	ErrStaleFencingToken   = errors.New("stale fencing token: call was reclaimed or reassigned")
)

// PacingMetrics contains real-time aggregates needed by Pacing and Safety Controllers.
type PacingMetrics struct {
	AvailableAgents        int
	TotalAgents            int
	ConnectedCalls         int
	RingingCalls           int
	InFlightCalls          int // RESERVED + INITIATED + RINGING
	RollingAnswerRate      float64
	RollingAbandonmentRate float64
	ProviderErrorRate      float64
	AgentDropRateLast5s    float64
	AvgSetupDurationSec    float64
	AvgTalkDurationSec     float64
}

// Store defines the database interactions for the dialer.
type Store interface {
	// Migrate runs DDL migrations.
	Migrate(ctx context.Context) error

	// ReserveAgentAndBorrower atomically reserves 1 Available Agent and 1 Pending Borrower
	// using SELECT ... FOR UPDATE SKIP LOCKED and creates a RESERVED call record.
	ReserveAgentAndBorrower(ctx context.Context, campaignID uuid.UUID, workerID, providerName string) (*domain.Call, *domain.Agent, *domain.Borrower, error)

	// ReserveAgentAndBorrowerSharded reserves within a partition shard to eliminate B-tree lock contention,
	// incrementing and assigning monotonic lease_tokens.
	ReserveAgentAndBorrowerSharded(ctx context.Context, campaignID uuid.UUID, workerID, providerName string, shardID int) (*domain.Call, *domain.Agent, *domain.Borrower, error)

	// ProcessProviderEvent idempotently logs the event and reconciles Call & Agent states.
	ProcessProviderEvent(ctx context.Context, event domain.CallEvent) error

	// ProcessProviderEventWithFencing validates monotonic fencing tokens before mutating state,
	// preventing split-brain writes from delayed/zombie workers.
	ProcessProviderEventWithFencing(ctx context.Context, event domain.CallEvent, expectedLeaseToken int64) error

	// RecordPacingAudit records a Safety Controller audit entry.
	RecordPacingAudit(ctx context.Context, audit domain.PacingAuditLog) error

	// GetPacingMetrics fetches real-time statistical metrics for a campaign.
	GetPacingMetrics(ctx context.Context, campaignID uuid.UUID) (*PacingMetrics, error)

	// Agent & Borrower Management
	CreateAgent(ctx context.Context, name string, state domain.AgentState) (*domain.Agent, error)
	SetAgentState(ctx context.Context, agentID uuid.UUID, state domain.AgentState) error
	CreateBorrower(ctx context.Context, phoneNumber string) (*domain.Borrower, error)
	GetAgent(ctx context.Context, id uuid.UUID) (*domain.Agent, error)
	GetCall(ctx context.Context, id uuid.UUID) (*domain.Call, error)
	GetBorrower(ctx context.Context, id uuid.UUID) (*domain.Borrower, error)

	// Reaper Support
	GetStuckCalls(ctx context.Context, reservedTimeout, answeredTimeout time.Duration) ([]*domain.Call, error)
	RecoverStuckCall(ctx context.Context, callID uuid.UUID, reason string) error

	// Close releases pool resources.
	Close()
}
