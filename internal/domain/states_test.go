package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
)

func TestAgentStateTransitions(t *testing.T) {
	// Valid transitions
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentOffline, domain.AgentAvailable))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentAvailable, domain.AgentReserved))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentReserved, domain.AgentDialing))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentDialing, domain.AgentConnected))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentConnected, domain.AgentWrapUp))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentWrapUp, domain.AgentAvailable))
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentReserved, domain.AgentAvailable)) // Call canceled/reaped
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentDialing, domain.AgentAvailable))  // No answer

	// Idempotent self-transition
	assert.NoError(t, domain.ValidateAgentTransition(domain.AgentAvailable, domain.AgentAvailable))

	// Invalid transitions
	assert.ErrorIs(t, domain.ValidateAgentTransition(domain.AgentOffline, domain.AgentConnected), domain.ErrInvalidAgentTransition)
	assert.ErrorIs(t, domain.ValidateAgentTransition(domain.AgentConnected, domain.AgentReserved), domain.ErrInvalidAgentTransition)
	assert.ErrorIs(t, domain.ValidateAgentTransition(domain.AgentDialing, domain.AgentWrapUp), domain.ErrInvalidAgentTransition)
}

func TestBorrowerStateTransitions(t *testing.T) {
	// Valid transitions
	assert.NoError(t, domain.ValidateBorrowerTransition(domain.BorrowerPending, domain.BorrowerReserved))
	assert.NoError(t, domain.ValidateBorrowerTransition(domain.BorrowerReserved, domain.BorrowerDialing))
	assert.NoError(t, domain.ValidateBorrowerTransition(domain.BorrowerDialing, domain.BorrowerCompleted))
	assert.NoError(t, domain.ValidateBorrowerTransition(domain.BorrowerDialing, domain.BorrowerPending)) // Retry
	assert.NoError(t, domain.ValidateBorrowerTransition(domain.BorrowerReserved, domain.BorrowerPending)) // Reaper release

	// Invalid transitions
	assert.ErrorIs(t, domain.ValidateBorrowerTransition(domain.BorrowerCompleted, domain.BorrowerDialing), domain.ErrInvalidBorrowerTransition)
	assert.ErrorIs(t, domain.ValidateBorrowerTransition(domain.BorrowerPending, domain.BorrowerCompleted), domain.ErrInvalidBorrowerTransition)
}

func TestCallStateMonotonicTransitions(t *testing.T) {
	t.Run("Normal Sequential Progression", func(t *testing.T) {
		state := domain.CallQueued

		next, res, err := domain.ResolveCallTransition(state, domain.CallReserved)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallReserved, next)

		next, res, err = domain.ResolveCallTransition(next, domain.CallInitiated)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallInitiated, next)

		next, res, err = domain.ResolveCallTransition(next, domain.CallRinging)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallRinging, next)

		next, res, err = domain.ResolveCallTransition(next, domain.CallAnswered)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallAnswered, next)

		next, res, err = domain.ResolveCallTransition(next, domain.CallConnected)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallConnected, next)

		next, res, err = domain.ResolveCallTransition(next, domain.CallCompleted)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallCompleted, next)
		assert.True(t, next.IsTerminal())
	})

	t.Run("Duplicate Webhook Events (Idempotency)", func(t *testing.T) {
		state := domain.CallAnswered

		next, res, err := domain.ResolveCallTransition(state, domain.CallAnswered)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionIgnoredDuplicate, res)
		assert.Equal(t, domain.CallAnswered, next)
	})

	t.Run("Out-Of-Order Event: COMPLETED arrives before ANSWERED or RINGING", func(t *testing.T) {
		// Call is marked COMPLETED
		state := domain.CallCompleted

		// Delayed ANSWERED event arrives
		next, res, err := domain.ResolveCallTransition(state, domain.CallAnswered)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionIgnoredStale, res)
		assert.Equal(t, domain.CallCompleted, next, "Terminal state must not regress")

		// Delayed RINGING event arrives
		next, res, err = domain.ResolveCallTransition(next, domain.CallRinging)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionIgnoredStale, res)
		assert.Equal(t, domain.CallCompleted, next, "Terminal state must not regress")
	})

	t.Run("Out-Of-Order Event: RINGING arrives after ANSWERED", func(t *testing.T) {
		state := domain.CallAnswered

		// Delayed RINGING event arrives
		next, res, err := domain.ResolveCallTransition(state, domain.CallRinging)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionIgnoredStale, res)
		assert.Equal(t, domain.CallAnswered, next, "Answered state must not regress to Ringing")
	})

	t.Run("Valid Skip: INITIATED directly to ANSWERED (Carrier skips 180 Ringing)", func(t *testing.T) {
		state := domain.CallInitiated

		next, res, err := domain.ResolveCallTransition(state, domain.CallAnswered)
		require.NoError(t, err)
		assert.Equal(t, domain.TransitionApplied, res)
		assert.Equal(t, domain.CallAnswered, next)
	})

	t.Run("Terminal Drops from any active state", func(t *testing.T) {
		states := []domain.CallState{
			domain.CallQueued,
			domain.CallReserved,
			domain.CallInitiated,
			domain.CallRinging,
			domain.CallAnswered,
			domain.CallConnected,
		}

		for _, s := range states {
			next, res, err := domain.ResolveCallTransition(s, domain.CallFailed)
			require.NoError(t, err)
			assert.Equal(t, domain.TransitionApplied, res)
			assert.Equal(t, domain.CallFailed, next)

			next, res, err = domain.ResolveCallTransition(s, domain.CallCancelled)
			require.NoError(t, err)
			assert.Equal(t, domain.TransitionApplied, res)
			assert.Equal(t, domain.CallCancelled, next)
		}
	})

	t.Run("Invalid Forward Jump", func(t *testing.T) {
		_, _, err := domain.ResolveCallTransition(domain.CallQueued, domain.CallConnected)
		assert.ErrorIs(t, err, domain.ErrInvalidCallTransition)
	})
}
