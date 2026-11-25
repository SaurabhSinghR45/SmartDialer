package pacing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/store"
)

// ProgressiveEngine proposes calls strictly based on 1:1 available agent capacity minus in-flight dials.
type ProgressiveEngine struct{}

func NewProgressiveEngine() *ProgressiveEngine {
	return &ProgressiveEngine{}
}

func (e *ProgressiveEngine) Mode() string {
	return "PROGRESSIVE"
}

func (e *ProgressiveEngine) Propose(ctx context.Context, campaignID uuid.UUID, metrics *store.PacingMetrics) (PacingProposal, error) {
	if metrics == nil {
		metrics = &store.PacingMetrics{}
	}

	available := metrics.AvailableAgents
	inFlight := metrics.InFlightCalls

	// Progressive formula: N = max(0, available_agents - in_flight_calls)
	n := available - inFlight
	if n < 0 {
		n = 0
	}

	proposal := PacingProposal{
		CampaignID:    campaignID,
		Mode:          e.Mode(),
		ProposedCalls: n,
		Rationale: PacingRationale{
			AvailableAgents:   available,
			InFlightCalls:     inFlight,
			ConnectedCalls:    metrics.ConnectedCalls,
			RingingCalls:      metrics.RingingCalls,
			RollingAnswerRate: metrics.RollingAnswerRate,
			TargetInFlight:    float64(available),
			Formula:           "N = max(0, available_agents - in_flight_calls)",
			Explanation: fmt.Sprintf(
				"Progressive 1:1 pacing: %d available agents minus %d in-flight dials yields %d new dials.",
				available, inFlight, n,
			),
		},
		Timestamp: time.Now().UTC(),
	}

	return proposal, nil
}
