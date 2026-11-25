package pacing_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/pacing"
	"smartdialer/internal/store"
)

func TestProgressiveEngine(t *testing.T) {
	engine := pacing.NewProgressiveEngine()
	assert.Equal(t, "PROGRESSIVE", engine.Mode())

	ctx := context.Background()
	campaignID := uuid.New()

	t.Run("Available exceeds InFlight", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents: 10,
			InFlightCalls:   4,
		}
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 6, prop.ProposedCalls)
		assert.Equal(t, "PROGRESSIVE", prop.Mode)
		assert.NotEmpty(t, prop.Rationale.Explanation)
	})

	t.Run("InFlight equals Available", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents: 5,
			InFlightCalls:   5,
		}
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 0, prop.ProposedCalls)
	})

	t.Run("InFlight exceeds Available", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents: 2,
			InFlightCalls:   8,
		}
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 0, prop.ProposedCalls)
	})
}

func TestPredictiveEngine(t *testing.T) {
	engine := pacing.NewPredictiveEngine()
	assert.Equal(t, "PREDICTIVE", engine.Mode())

	ctx := context.Background()
	campaignID := uuid.New()

	t.Run("Little's Law and Erlang-inspired Buffer", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents:     10,
			ConnectedCalls:      20,
			RingingCalls:        5,
			RollingAnswerRate:   0.25, // 25% answer rate
			AvgSetupDurationSec: 4.5,
			AvgTalkDurationSec:  90.0,
		}
		// Expected completions = 20 * (4.5 / 90) = 20 * 0.05 = 1.0
		// Effective agents = 10 + 1 = 11.0
		// Target InFlight = 11.0 / 0.25 = 44.0
		// Delta = floor(44.0 - 5) = 39
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 39, prop.ProposedCalls)
		assert.InDelta(t, 1.0, prop.Rationale.ExpectedCompletions, 0.01)
		assert.InDelta(t, 44.0, prop.Rationale.TargetInFlight, 0.01)
	})

	t.Run("Answer rate floor at 5%", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents:     5,
			ConnectedCalls:      0,
			RingingCalls:        0,
			RollingAnswerRate:   0.01, // Below 5% floor
			AvgSetupDurationSec: 5.0,
			AvgTalkDurationSec:  90.0,
		}
		// Effective agents = 5
		// Answer rate clamped to 0.05
		// Target = 5 / 0.05 = 100
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 100, prop.ProposedCalls)
	})

	t.Run("Ringing calls already satisfy target", func(t *testing.T) {
		metrics := &store.PacingMetrics{
			AvailableAgents:     2,
			ConnectedCalls:      0,
			RingingCalls:        50,
			RollingAnswerRate:   0.20,
			AvgSetupDurationSec: 5.0,
			AvgTalkDurationSec:  90.0,
		}
		// Target = 2 / 0.20 = 10
		// Ringing = 50 -> Delta = 10 - 50 = -40 -> clamped to 0
		prop, err := engine.Propose(ctx, campaignID, metrics)
		require.NoError(t, err)
		assert.Equal(t, 0, prop.ProposedCalls)
	})
}
