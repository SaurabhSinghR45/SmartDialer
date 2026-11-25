package pacing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/store"
)

// PacingRationale provides an explainable audit trail for the calculated proposal.
type PacingRationale struct {
	AvailableAgents     int     `json:"available_agents"`
	InFlightCalls       int     `json:"in_flight_calls"`
	ConnectedCalls      int     `json:"connected_calls"`
	RingingCalls        int     `json:"ringing_calls"`
	RollingAnswerRate   float64 `json:"rolling_answer_rate"`
	ExpectedCompletions float64 `json:"expected_completions"`
	TargetInFlight      float64 `json:"target_in_flight"`
	Formula             string  `json:"formula"`
	Explanation         string  `json:"explanation"`
}

// PacingProposal represents a dial rate suggestion from an engine before safety vetting.
type PacingProposal struct {
	CampaignID    uuid.UUID       `json:"campaign_id"`
	Mode          string          `json:"mode"`
	ProposedCalls int             `json:"proposed_calls"`
	Rationale     PacingRationale `json:"rationale"`
	Timestamp     time.Time       `json:"timestamp"`
}

// PacingEngine defines the strict contract for pacing calculations.
// It CANNOT execute calls, reserve agents, or interact with telecom providers directly.
type PacingEngine interface {
	Mode() string
	Propose(ctx context.Context, campaignID uuid.UUID, metrics *store.PacingMetrics) (PacingProposal, error)
}
