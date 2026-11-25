package domain

import (
	"time"

	"github.com/google/uuid"
)

// Agent represents a call center agent handling collections calls.
type Agent struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	State         AgentState `json:"state"`
	CurrentCallID *uuid.UUID `json:"current_call_id,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Version       int64      `json:"version"`
	ShardID       int        `json:"shard_id"`
	LeaseToken    int64      `json:"lease_token"`
}

// Borrower represents a debtor to be contacted.
type Borrower struct {
	ID                     uuid.UUID     `json:"id"`
	PhoneNumber            string        `json:"phone_number"`
	State                  BorrowerState `json:"state"`
	RetryCount             int           `json:"retry_count"`
	NextRetryAt            *time.Time    `json:"next_retry_at,omitempty"`
	ShardID                int           `json:"shard_id"`
	Timezone               string        `json:"timezone"`
	LastCalledAt           *time.Time    `json:"last_called_at,omitempty"`
	CallAttemptsLast7Days  int           `json:"call_attempts_last_7_days"`
}

// Call represents an outbound dial attempt linking a borrower, an agent, and a provider.
type Call struct {
	ID               uuid.UUID  `json:"id"`
	CampaignID       uuid.UUID  `json:"campaign_id"`
	AgentID          *uuid.UUID `json:"agent_id,omitempty"`
	BorrowerID       uuid.UUID  `json:"borrower_id"`
	ProviderName     string     `json:"provider_name"`
	ProviderCallID   *string    `json:"provider_call_id,omitempty"`
	State            CallState  `json:"state"`
	WorkerID         *string    `json:"worker_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	LeaseToken       int64      `json:"lease_token"`
	IsMachine        bool       `json:"is_machine"`
	CarrierLatencyMs int        `json:"carrier_latency_ms"`
}

// CallEvent is an immutable provider webhook or system event for idempotency and audits.
type CallEvent struct {
	ID              uuid.UUID      `json:"id"`
	CallID          uuid.UUID      `json:"call_id"`
	ProviderEventID string         `json:"provider_event_id"`
	EventType       string         `json:"event_type"`
	EventTimestamp  time.Time      `json:"event_timestamp"`
	Payload         map[string]any `json:"payload"`
	ProcessedAt     time.Time      `json:"processed_at"`
}

// PacingDecision specifies the safety controller's verdict.
type SafetyDecision string

const (
	DecisionApprove              SafetyDecision = "APPROVE"
	DecisionReduce               SafetyDecision = "REDUCE"
	DecisionReject               SafetyDecision = "REJECT"
	DecisionFallbackProgressive SafetyDecision = "FALLBACK_TO_PROGRESSIVE"
)

// PacingAuditLog stores audit records of pacing proposals and safety decisions.
type PacingAuditLog struct {
	ID              uuid.UUID      `json:"id"`
	CampaignID      uuid.UUID      `json:"campaign_id"`
	Timestamp       time.Time      `json:"timestamp"`
	Mode            string         `json:"mode"`
	ProposedCalls   int            `json:"proposed_calls"`
	ApprovedCalls   int            `json:"approved_calls"`
	Decision        SafetyDecision `json:"decision"`
	Reason          string         `json:"reason"`
	MetricsSnapshot map[string]any `json:"metrics_snapshot"`
}
