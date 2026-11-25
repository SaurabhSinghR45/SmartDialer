package pacing

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/store"
)

// PredictiveEngine utilizes Little's Law and Erlang-inspired buffering to over-dial based on historical answer rates.
type PredictiveEngine struct{}

func NewPredictiveEngine() *PredictiveEngine {
	return &PredictiveEngine{}
}

func (e *PredictiveEngine) Mode() string {
	return "PREDICTIVE"
}

func (e *PredictiveEngine) Propose(ctx context.Context, campaignID uuid.UUID, metrics *store.PacingMetrics) (PacingProposal, error) {
	if metrics == nil {
		metrics = &store.PacingMetrics{}
	}

	available := float64(metrics.AvailableAgents)
	connected := float64(metrics.ConnectedCalls)
	ringing := float64(metrics.RingingCalls)

	// Ensure realistic baselines for setup and talk times
	setupSec := metrics.AvgSetupDurationSec
	if setupSec <= 0 {
		setupSec = 5.0
	}
	talkSec := metrics.AvgTalkDurationSec
	if talkSec <= 0 {
		talkSec = 90.0
	}

	// 1. Expected agent completions during setup window (Little's Law approximation):
	// Expected completions = Connected Calls * (Setup Time / Talk Time)
	expectedCompletions := connected * (setupSec / talkSec)

	// 2. Effective agent capacity anticipated at time of call answer
	effectiveAgents := available + expectedCompletions

	// 3. Prevent division by zero; floor answer rate at 5%
	answerRate := metrics.RollingAnswerRate
	if answerRate < 0.05 {
		answerRate = 0.05
	}

	// 4. Target in-flight dials required to produce answers equal to effective agent capacity
	targetInFlight := effectiveAgents / answerRate

	// 5. Delta: new dials to launch = floor(target - ringing_calls)
	delta := int(math.Floor(targetInFlight - ringing))
	if delta < 0 {
		delta = 0
	}

	proposal := PacingProposal{
		CampaignID:    campaignID,
		Mode:          e.Mode(),
		ProposedCalls: delta,
		Rationale: PacingRationale{
			AvailableAgents:     metrics.AvailableAgents,
			InFlightCalls:       metrics.InFlightCalls,
			ConnectedCalls:      metrics.ConnectedCalls,
			RingingCalls:        metrics.RingingCalls,
			RollingAnswerRate:   metrics.RollingAnswerRate,
			ExpectedCompletions: math.Round(expectedCompletions*100) / 100,
			TargetInFlight:      math.Round(targetInFlight*100) / 100,
			Formula:             "target = (available_agents + (connected * setup/talk)) / max(answer_rate, 0.05); delta = floor(target - ringing)",
			Explanation: fmt.Sprintf(
				"Predictive Little's Law: Effective Agents %.2f (Available %d + Expected Completions %.2f) with Answer Rate %.1f%% requires Target %.1f in-flight. Current Ringing %d -> Delta %d new dials.",
				effectiveAgents, metrics.AvailableAgents, expectedCompletions, answerRate*100, targetInFlight, metrics.RingingCalls, delta,
			),
		},
		Timestamp: time.Now().UTC(),
	}

	return proposal, nil
}
