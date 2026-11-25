package safety

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/domain"
	"smartdialer/internal/pacing"
	"smartdialer/internal/store"
)

// SafetyResult contains the vetted, non-bypassable dial allocation decision.
type SafetyResult struct {
	CampaignID    uuid.UUID             `json:"campaign_id"`
	ApprovedCalls int                   `json:"approved_calls"`
	Decision      domain.SafetyDecision `json:"decision"`
	Reason        string                `json:"reason"`
	Mode          string                `json:"mode"`
	Timestamp     time.Time             `json:"timestamp"`
}

// Controller enforces regulatory, capacity, and system failure boundaries.
type Controller struct {
	store             store.Store
	progressiveEngine *pacing.ProgressiveEngine
}

func NewController(st store.Store) *Controller {
	return &Controller{
		store:             st,
		progressiveEngine: pacing.NewProgressiveEngine(),
	}
}

// Evaluate applies non-bypassable safety invariants to a pacing proposal.
// Pipeline: PacingProposal -> Invariant Checks -> Approved Tickets -> DB Audit Log
func (c *Controller) Evaluate(
	ctx context.Context,
	proposal pacing.PacingProposal,
	metrics *store.PacingMetrics,
) (SafetyResult, error) {
	if metrics == nil {
		metrics = &store.PacingMetrics{}
	}

	result := SafetyResult{
		CampaignID:    proposal.CampaignID,
		ApprovedCalls: proposal.ProposedCalls,
		Decision:      domain.DecisionApprove,
		Reason:        "Proposal within safe operating boundaries",
		Mode:          proposal.Mode,
		Timestamp:     time.Now().UTC(),
	}

	// -------------------------------------------------------------
	// INVARIANT 1: Sudden Agent Drop Guard (>20% drop in last 5s)
	// Prevents severe predictive call flushes when agents disconnect.
	// -------------------------------------------------------------
	if metrics.AgentDropRateLast5s > 0.20 && proposal.ProposedCalls > 0 {
		result.ApprovedCalls = 0
		result.Decision = domain.DecisionReject
		result.Reason = fmt.Sprintf(
			"Sudden Drop Guard triggered: %.1f%% of agents disconnected in last 5s (>20%% threshold). All new bursts rejected.",
			metrics.AgentDropRateLast5s*100,
		)
		c.logAudit(ctx, proposal, result, metrics)
		return result, nil
	}

	// -------------------------------------------------------------
	// INVARIANT 2: Abandonment Rate & Provider Error Rate Guard
	// Forces instant fallback to Progressive mode if:
	// - Abandonment Rate > 2.0% (strict regulatory compliance)
	// - Telecom Provider Error Rate > 25% (unstable carrier)
	// -------------------------------------------------------------
	if proposal.Mode == "PREDICTIVE" {
		isHighAbandonment := metrics.RollingAbandonmentRate > 0.02
		isHighProviderError := metrics.ProviderErrorRate > 0.25

		if isHighAbandonment || isHighProviderError {
			// Recalculate using progressive 1:1 capacity
			progProposal, _ := c.progressiveEngine.Propose(ctx, proposal.CampaignID, metrics)
			result.ApprovedCalls = progProposal.ProposedCalls
			result.Decision = domain.DecisionFallbackProgressive
			result.Mode = "PROGRESSIVE_FALLBACK"

			if isHighAbandonment && isHighProviderError {
				result.Reason = fmt.Sprintf(
					"Fallback to Progressive: Abandonment Rate %.2f%% (>2.0%%) and Provider Error Rate %.1f%% (>25%%).",
					metrics.RollingAbandonmentRate*100, metrics.ProviderErrorRate*100,
				)
			} else if isHighAbandonment {
				result.Reason = fmt.Sprintf(
					"Fallback to Progressive: Abandonment Rate %.2f%% exceeded regulatory 2.0%% limit.",
					metrics.RollingAbandonmentRate*100,
				)
			} else {
				result.Reason = fmt.Sprintf(
					"Fallback to Progressive: Carrier Error Rate %.1f%% exceeded 25%% threshold.",
					metrics.ProviderErrorRate*100,
				)
			}

			c.logAudit(ctx, proposal, result, metrics)
			return result, nil
		}
	}

	// -------------------------------------------------------------
	// INVARIANT 3: Hard Capacity Cap
	// (ringing_calls + approved) * rolling_answer_rate <= available_agents * 1.15
	// -------------------------------------------------------------
	answerRate := math.Max(metrics.RollingAnswerRate, 0.05)
	maxAllowedAnticipatedAnswers := float64(metrics.AvailableAgents) * 1.15
	anticipatedAnswersWithProposal := float64(metrics.RingingCalls+result.ApprovedCalls) * answerRate

	if anticipatedAnswersWithProposal > maxAllowedAnticipatedAnswers && result.ApprovedCalls > 0 {
		// Calculate maximum calls that can be approved without exceeding 115% capacity
		allowedInFlight := maxAllowedAnticipatedAnswers / answerRate
		maxApproved := int(math.Floor(allowedInFlight - float64(metrics.RingingCalls)))
		if maxApproved < 0 {
			maxApproved = 0
		}

		if maxApproved < result.ApprovedCalls {
			if maxApproved == 0 {
				result.ApprovedCalls = 0
				result.Decision = domain.DecisionReject
				result.Reason = fmt.Sprintf(
					"Capacity Cap rejected: Anticipated answers %.2f exceeds 115%% of available agents (%.2f max allowed).",
					anticipatedAnswersWithProposal, maxAllowedAnticipatedAnswers,
				)
			} else {
				result.ApprovedCalls = maxApproved
				result.Decision = domain.DecisionReduce
				result.Reason = fmt.Sprintf(
					"Capacity Cap reduced from %d to %d calls: Capped to keep anticipated answers under 115%% of available capacity.",
					proposal.ProposedCalls, maxApproved,
				)
			}
			c.logAudit(ctx, proposal, result, metrics)
			return result, nil
		}
	}

	// -------------------------------------------------------------
	// INVARIANT 4: Zero Available Agents Safeguard in Progressive Mode
	// -------------------------------------------------------------
	if proposal.Mode == "PROGRESSIVE" && metrics.AvailableAgents == 0 && result.ApprovedCalls > 0 {
		result.ApprovedCalls = 0
		result.Decision = domain.DecisionReject
		result.Reason = "No available agents in Progressive mode."
	}

	c.logAudit(ctx, proposal, result, metrics)
	return result, nil
}

func (c *Controller) logAudit(
	ctx context.Context,
	proposal pacing.PacingProposal,
	result SafetyResult,
	metrics *store.PacingMetrics,
) {
	if c.store == nil {
		return
	}

	audit := domain.PacingAuditLog{
		ID:            uuid.New(),
		CampaignID:    proposal.CampaignID,
		Timestamp:     result.Timestamp,
		Mode:          result.Mode,
		ProposedCalls: proposal.ProposedCalls,
		ApprovedCalls: result.ApprovedCalls,
		Decision:      result.Decision,
		Reason:        result.Reason,
		MetricsSnapshot: map[string]any{
			"available_agents":         metrics.AvailableAgents,
			"total_agents":             metrics.TotalAgents,
			"connected_calls":          metrics.ConnectedCalls,
			"ringing_calls":            metrics.RingingCalls,
			"in_flight_calls":          metrics.InFlightCalls,
			"rolling_answer_rate":      metrics.RollingAnswerRate,
			"rolling_abandonment_rate": metrics.RollingAbandonmentRate,
			"provider_error_rate":      metrics.ProviderErrorRate,
			"agent_drop_rate_5s":       metrics.AgentDropRateLast5s,
			"proposal_rationale":       proposal.Rationale,
		},
	}

	_ = c.store.RecordPacingAudit(ctx, audit)
}
