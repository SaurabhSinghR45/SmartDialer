package domain

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidAgentTransition    = errors.New("invalid agent state transition")
	ErrInvalidBorrowerTransition = errors.New("invalid borrower state transition")
	ErrInvalidCallTransition     = errors.New("invalid call state transition")
)

// AgentState represents the lifecycle state of an agent.
type AgentState string

const (
	AgentOffline   AgentState = "OFFLINE"
	AgentAvailable AgentState = "AVAILABLE"
	AgentReserved  AgentState = "RESERVED"
	AgentDialing   AgentState = "DIALING"
	AgentConnected AgentState = "CONNECTED"
	AgentWrapUp    AgentState = "WRAP_UP"
	AgentPaused    AgentState = "PAUSED"
)

// Valid Agent Transitions
var validAgentTransitions = map[AgentState]map[AgentState]bool{
	AgentOffline: {
		AgentAvailable: true,
	},
	AgentAvailable: {
		AgentReserved: true,
		AgentPaused:   true,
		AgentOffline:  true,
	},
	AgentReserved: {
		AgentDialing:   true,
		AgentAvailable: true, // Allocator abort or reaper rollback
	},
	AgentDialing: {
		AgentConnected: true,
		AgentAvailable: true, // Call not answered / busy / failed
	},
	AgentConnected: {
		AgentWrapUp: true,
	},
	AgentWrapUp: {
		AgentAvailable: true,
		AgentPaused:    true,
		AgentOffline:   true,
	},
	AgentPaused: {
		AgentAvailable: true,
		AgentOffline:   true,
	},
}

// ValidateAgentTransition checks if moving from current to target is allowed.
func ValidateAgentTransition(current, target AgentState) error {
	if current == target {
		return nil
	}
	allowed, exists := validAgentTransitions[current]
	if !exists || !allowed[target] {
		return fmt.Errorf("%w: cannot transition agent from %s to %s", ErrInvalidAgentTransition, current, target)
	}
	return nil
}

// BorrowerState represents the collections lead state.
type BorrowerState string

const (
	BorrowerPending   BorrowerState = "PENDING"
	BorrowerReserved  BorrowerState = "RESERVED"
	BorrowerDialing   BorrowerState = "DIALING"
	BorrowerCompleted BorrowerState = "COMPLETED"
	BorrowerFailed    BorrowerState = "FAILED"
)

var validBorrowerTransitions = map[BorrowerState]map[BorrowerState]bool{
	BorrowerPending: {
		BorrowerReserved: true,
	},
	BorrowerReserved: {
		BorrowerDialing: true,
		BorrowerPending: true, // Released by reaper or alloc failure
	},
	BorrowerDialing: {
		BorrowerCompleted: true,
		BorrowerFailed:    true,
		BorrowerPending:   true, // Re-queued for retry with backoff
	},
	BorrowerFailed: {
		BorrowerPending: true, // Manual or batch retry reset
	},
	BorrowerCompleted: {}, // Terminal
}

// ValidateBorrowerTransition checks if moving borrower from current to target is allowed.
func ValidateBorrowerTransition(current, target BorrowerState) error {
	if current == target {
		return nil
	}
	allowed, exists := validBorrowerTransitions[current]
	if !exists || !allowed[target] {
		return fmt.Errorf("%w: cannot transition borrower from %s to %s", ErrInvalidBorrowerTransition, current, target)
	}
	return nil
}

// CallState represents the lifecycle state of a dial attempt.
type CallState string

const (
	CallQueued    CallState = "QUEUED"
	CallReserved  CallState = "RESERVED"
	CallInitiated CallState = "INITIATED"
	CallRinging   CallState = "RINGING"
	CallAnswered  CallState = "ANSWERED"
	CallConnected CallState = "CONNECTED"
	CallCompleted CallState = "COMPLETED"
	CallFailed    CallState = "FAILED"
	CallCancelled CallState = "CANCELLED"
)

// Monotonic Call State Rankings
func CallStateRank(s CallState) int {
	switch s {
	case CallQueued:
		return 0
	case CallReserved:
		return 10
	case CallInitiated:
		return 20
	case CallRinging:
		return 30
	case CallAnswered:
		return 40
	case CallConnected:
		return 50
	case CallCompleted, CallFailed, CallCancelled:
		return 100
	default:
		return -1
	}
}

// IsTerminal returns true if the call has reached an irreversible end state.
func (s CallState) IsTerminal() bool {
	return s == CallCompleted || s == CallFailed || s == CallCancelled
}

// TransitionResolution describes the outcome of an event-driven call state transition.
type TransitionResolution int

const (
	// TransitionApplied means the state changed forward successfully.
	TransitionApplied TransitionResolution = iota
	// TransitionIgnoredDuplicate means the state is already identical (idempotent).
	TransitionIgnoredDuplicate
	// TransitionIgnoredStale means an out-of-order event arrived after a later or terminal state.
	TransitionIgnoredStale
)

// ResolveCallTransition evaluates an incoming call state event against the current state.
// It enforces monotonic progression:
// 1. Duplicate events are safely ignored.
// 2. Terminal states (COMPLETED, FAILED, CANCELLED) can never be regressed.
// 3. Out-of-order past events (e.g., RINGING arriving after ANSWERED or COMPLETED) are safely ignored without regressing state.
// 4. Skips (e.g. INITIATED directly to ANSWERED or COMPLETED) are valid forward progress.
func ResolveCallTransition(current, incoming CallState) (CallState, TransitionResolution, error) {
	// 1. Identical state: idempotent no-op
	if current == incoming {
		return current, TransitionIgnoredDuplicate, nil
	}

	// 2. Terminal states cannot be altered
	if current.IsTerminal() {
		return current, TransitionIgnoredStale, nil
	}

	// 3. Monotonic check: target has lower or equal rank
	currentRank := CallStateRank(current)
	incomingRank := CallStateRank(incoming)

	if incomingRank < 0 || currentRank < 0 {
		return current, TransitionIgnoredStale, fmt.Errorf("%w: unknown state '%s' or '%s'", ErrInvalidCallTransition, current, incoming)
	}

	// If incoming rank is strictly less than current rank, it is a stale out-of-order event
	if incomingRank <= currentRank {
		return current, TransitionIgnoredStale, nil
	}

	// 4. Valid forward transitions:
	// A non-terminal call can always move to a terminal state (hangup, disconnect, error, cancel)
	if incoming.IsTerminal() {
		return incoming, TransitionApplied, nil
	}

	// Forward progression rules between non-terminal states
	switch current {
	case CallQueued:
		if incoming == CallReserved {
			return incoming, TransitionApplied, nil
		}
	case CallReserved:
		// Can transition to INITIATED, or skip directly to RINGING or ANSWERED (carrier fast-connect)
		if incoming == CallInitiated || incoming == CallRinging || incoming == CallAnswered {
			return incoming, TransitionApplied, nil
		}
	case CallInitiated:
		// Can transition to RINGING or directly to ANSWERED (many carriers skip 180 Ringing)
		if incoming == CallRinging || incoming == CallAnswered {
			return incoming, TransitionApplied, nil
		}
	case CallRinging:
		if incoming == CallAnswered {
			return incoming, TransitionApplied, nil
		}
	case CallAnswered:
		if incoming == CallConnected {
			return incoming, TransitionApplied, nil
		}
	case CallConnected:
		// Only terminal states allowed from Connected (handled above)
	}

	return current, TransitionIgnoredStale, fmt.Errorf("%w: cannot transition from %s to %s", ErrInvalidCallTransition, current, incoming)
}
