package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/domain"
	"smartdialer/internal/provider"
	"smartdialer/internal/safety"
	"smartdialer/internal/store"
)

var (
	ErrZeroTicketsApproved = errors.New("zero tickets approved by safety controller")
	ErrAllocationAborted   = errors.New("allocation aborted due to resource exhaustion")
)

// AllocationResult records the outcome of an allocated dial attempt.
type AllocationResult struct {
	CallID         uuid.UUID  `json:"call_id"`
	AgentID        *uuid.UUID `json:"agent_id,omitempty"`
	BorrowerID     uuid.UUID  `json:"borrower_id"`
	ProviderCallID string     `json:"provider_call_id,omitempty"`
	Success        bool       `json:"success"`
	Error          string     `json:"error,omitempty"`
}

// Allocator is the strictly gated component that reserves DB resources and dispatches to Telecom Providers.
// ONLY the Safety Controller's approved tickets may be passed to the Allocator.
type Allocator struct {
	store    store.Store
	provider provider.TelecomProvider
}

func NewAllocator(st store.Store, prov provider.TelecomProvider) *Allocator {
	return &Allocator{
		store:    st,
		provider: prov,
	}
}

// AllocateAndDial takes approved tickets from the Safety Controller, atomically locks Agent + Borrower,
// and initiates the telecom call.
func (a *Allocator) AllocateAndDial(
	ctx context.Context,
	workerID string,
	campaignID uuid.UUID,
	ticket safety.SafetyResult,
) ([]AllocationResult, error) {
	if ticket.ApprovedCalls <= 0 {
		return nil, ErrZeroTicketsApproved
	}

	results := make([]AllocationResult, 0, ticket.ApprovedCalls)

	for i := 0; i < ticket.ApprovedCalls; i++ {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		// 1. Atomic reservation via SKIP LOCKED
		call, agent, borrower, err := a.store.ReserveAgentAndBorrower(ctx, campaignID, workerID, a.provider.Name())
		if err != nil {
			if errors.Is(err, store.ErrNoAvailableAgent) || errors.Is(err, store.ErrNoPendingBorrower) {
				// Normal condition: queue drained or all agents occupied
				break
			}
			return results, fmt.Errorf("reservation failed: %w", err)
		}

		res := AllocationResult{
			CallID:     call.ID,
			AgentID:    &agent.ID,
			BorrowerID: borrower.ID,
		}

		// 2. Dispatch to Telecom Provider
		provCallID, dialErr := a.provider.PlaceCall(ctx, call.ID, borrower.PhoneNumber)
		if dialErr != nil {
			// Carrier failed (e.g. outage or timeout): rollback call and release agent
			res.Success = false
			res.Error = dialErr.Error()

			_ = a.store.ProcessProviderEvent(ctx, domain.CallEvent{
				ID:              uuid.New(),
				CallID:          call.ID,
				ProviderEventID: fmt.Sprintf("alloc-fail-%s-%d", call.ID.String()[:8], time.Now().UnixNano()),
				EventType:       "FAILED",
				EventTimestamp:  time.Now().UTC(),
				Payload:         map[string]any{"reason": "telecom_dispatch_failure", "err": dialErr.Error()},
				ProcessedAt:     time.Now().UTC(),
			})
		} else {
			res.Success = true
			res.ProviderCallID = provCallID

			// Emit initial INITIATED state to confirm dispatch
			_ = a.store.ProcessProviderEvent(ctx, domain.CallEvent{
				ID:              uuid.New(),
				CallID:          call.ID,
				ProviderEventID: fmt.Sprintf("init-%s", provCallID),
				EventType:       "INITIATED",
				EventTimestamp:  time.Now().UTC(),
				Payload:         map[string]any{"provider_call_id": provCallID},
				ProcessedAt:     time.Now().UTC(),
			})
		}

		results = append(results, res)
	}

	return results, nil
}
