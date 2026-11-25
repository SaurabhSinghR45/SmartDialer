package safety_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
	"smartdialer/internal/pacing"
	"smartdialer/internal/safety"
	"smartdialer/internal/store"
)

// mockStore implements minimal Store to receive audit logs without DB.
type mockStore struct {
	store.Store
	loggedAudits []domain.PacingAuditLog
}

func (m *mockStore) RecordPacingAudit(ctx context.Context, audit domain.PacingAuditLog) error {
	m.loggedAudits = append(m.loggedAudits, audit)
	return nil
}

func TestSafetyController_Invariants(t *testing.T) {
	ctx := context.Background()
	campaignID := uuid.New()

	t.Run("Standard Safe Proposal Approved", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:   10,
			TotalAgents:       10,
			RingingCalls:      2,
			RollingAnswerRate: 0.20,
		}

		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 10,
			Timestamp:     time.Now().UTC(),
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionApprove, res.Decision)
		assert.Equal(t, 10, res.ApprovedCalls)
		assert.Len(t, mock.loggedAudits, 1)
	})

	t.Run("Sudden Agent Drop Guard Triggered", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:     10,
			TotalAgents:         50,
			AgentDropRateLast5s: 0.25, // 25% dropped in last 5s (> 20% limit)
		}

		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 15,
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionReject, res.Decision)
		assert.Equal(t, 0, res.ApprovedCalls)
		assert.Contains(t, res.Reason, "Sudden Drop Guard")
	})

	t.Run("Abandonment Guard Forces Fallback to Progressive", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:        8,
			InFlightCalls:          3,
			RollingAbandonmentRate: 0.035, // 3.5% (> 2.0% limit)
			RollingAnswerRate:      0.25,
		}

		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 20, // Aggressive predictive proposal
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionFallbackProgressive, res.Decision)
		// Under progressive fallback: max(0, available (8) - inflight (3)) = 5
		assert.Equal(t, 5, res.ApprovedCalls)
		assert.Equal(t, "PROGRESSIVE_FALLBACK", res.Mode)
		assert.Contains(t, res.Reason, "Abandonment Rate 3.50%")
	})

	t.Run("Carrier Outage / High Error Rate Forces Fallback to Progressive", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:   5,
			InFlightCalls:     2,
			ProviderErrorRate: 0.30, // 30% error rate (> 25% threshold)
			RollingAnswerRate: 0.20,
		}

		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 15,
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionFallbackProgressive, res.Decision)
		// Progressive capacity: 5 - 2 = 3
		assert.Equal(t, 3, res.ApprovedCalls)
		assert.Contains(t, res.Reason, "Carrier Error Rate 30.0%")
	})

	t.Run("Capacity Cap Reduces Excessive Dial Proposal", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:   10, // Max answers = 10 * 1.15 = 11.5
			RingingCalls:      10,
			RollingAnswerRate: 0.50, // 50% answer rate
		}
		// Max allowed in-flight = 11.5 / 0.50 = 23.0
		// Max approved = floor(23.0 - 10 ringing) = 13
		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 25, // Excessive proposal
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionReduce, res.Decision)
		assert.Equal(t, 13, res.ApprovedCalls)
		assert.Contains(t, res.Reason, "Capacity Cap reduced")
	})

	t.Run("Capacity Cap Rejects when Ringing Already Saturated", func(t *testing.T) {
		mock := &mockStore{}
		ctrl := safety.NewController(mock)

		metrics := &store.PacingMetrics{
			AvailableAgents:   10,
			RingingCalls:      30, // 30 * 0.50 = 15 expected answers (> 11.5 max)
			RollingAnswerRate: 0.50,
		}

		proposal := pacing.PacingProposal{
			CampaignID:    campaignID,
			Mode:          "PREDICTIVE",
			ProposedCalls: 5,
		}

		res, err := ctrl.Evaluate(ctx, proposal, metrics)
		require.NoError(t, err)
		assert.Equal(t, domain.DecisionReject, res.Decision)
		assert.Equal(t, 0, res.ApprovedCalls)
		assert.Contains(t, res.Reason, "Capacity Cap rejected")
	})
}
