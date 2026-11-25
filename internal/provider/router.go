package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrAllCarriersUnavailable = errors.New("all telecom trunks unavailable or circuit open")
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitHalfOpen CircuitState = "HALF-OPEN"
	CircuitOpen     CircuitState = "OPEN"
)

// CarrierRoute wraps a TelecomProvider with commercial rate cards and resilience telemetry.
type CarrierRoute struct {
	Provider          TelecomProvider
	CostPerMinuteCents float64
	Priority          int // 1 = highest priority

	mu                 sync.RWMutex
	CircuitState       CircuitState
	ConsecutiveErrors  int
	LastFailureTime    time.Time
	TotalAttempts      int64
	SuccessAttempts    int64
	P95LatencyMs       int64
}

// CarrierRouter manages multi-trunk dynamic routing, LCR, and trunk hedging.
type CarrierRouter struct {
	mu             sync.RWMutex
	routes         []*CarrierRoute
	webhookHandler WebhookHandler

	// Circuit Breaker settings
	failureThreshold   int           // e.g. 4 consecutive errors
	circuitCooldown    time.Duration // e.g. 10 seconds before half-open
	hedgeDeadline      time.Duration // e.g. 1200ms before hedged retry
}

// NewCarrierRouter initializes a multi-carrier LCR engine.
func NewCarrierRouter(hedgeDeadline time.Duration) *CarrierRouter {
	if hedgeDeadline <= 0 {
		hedgeDeadline = 1200 * time.Millisecond
	}
	return &CarrierRouter{
		routes:           make([]*CarrierRoute, 0),
		failureThreshold: 4,
		circuitCooldown:  10 * time.Second,
		hedgeDeadline:    hedgeDeadline,
	}
}

// AddCarrier registers a provider with its cost-per-minute rate.
func (r *CarrierRouter) AddCarrier(p TelecomProvider, costPerMinCents float64, priority int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	route := &CarrierRoute{
		Provider:           p,
		CostPerMinuteCents: costPerMinCents,
		Priority:           priority,
		CircuitState:       CircuitClosed,
	}
	r.routes = append(r.routes, route)

	if r.webhookHandler != nil {
		p.RegisterWebhookHandler(r.webhookHandler)
	}
}

// RegisterWebhookHandler multiplexes inbound webhook processing across all registered carriers.
func (r *CarrierRouter) RegisterWebhookHandler(handler WebhookHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.webhookHandler = handler

	for _, route := range r.routes {
		route.Provider.RegisterWebhookHandler(handler)
	}
}

// SelectLCRCarrier picks the lowest-cost healthy carrier whose circuit breaker is not OPEN.
func (r *CarrierRouter) SelectLCRCarrier() (*CarrierRoute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	var candidates []*CarrierRoute

	for _, route := range r.routes {
		route.mu.Lock()
		// Check for circuit half-open transition
		if route.CircuitState == CircuitOpen && now.Sub(route.LastFailureTime) >= r.circuitCooldown {
			route.CircuitState = CircuitHalfOpen
		}

		if route.CircuitState == CircuitClosed || route.CircuitState == CircuitHalfOpen {
			candidates = append(candidates, route)
		}
		route.mu.Unlock()
	}

	if len(candidates) == 0 {
		return nil, ErrAllCarriersUnavailable
	}

	// Sort candidates by CostPerMinuteCents ascending, then Priority ascending
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CostPerMinuteCents != candidates[j].CostPerMinuteCents {
			return candidates[i].CostPerMinuteCents < candidates[j].CostPerMinuteCents
		}
		return candidates[i].Priority < candidates[j].Priority
	})

	return candidates[0], nil
}

// PlaceCallWithHedging places a call using Least-Cost Routing with automatic trunk hedging.
// If the primary trunk experiences high latency (>hedgeDeadline) or immediate failure,
// the secondary backup trunk is hedge-dispatched concurrently.
func (r *CarrierRouter) PlaceCallWithHedging(
	ctx context.Context,
	callID uuid.UUID,
	phoneNumber string,
) (carrierName string, providerCallID string, err error) {
	primary, err := r.SelectLCRCarrier()
	if err != nil {
		return "", "", err
	}

	type callResult struct {
		route          *CarrierRoute
		providerCallID string
		err            error
	}

	resultChan := make(chan callResult, 2)
	hedgeTimer := time.NewTimer(r.hedgeDeadline)
	defer hedgeTimer.Stop()

	// 1. Dispatch Primary Trunk
	go func() {
		start := time.Now()
		provID, callErr := primary.Provider.PlaceCall(ctx, callID, phoneNumber)
		primary.recordOutcome(callErr, time.Since(start), r.failureThreshold)
		resultChan <- callResult{route: primary, providerCallID: provID, err: callErr}
	}()

	var primaryFailed bool
	var lastErr error

	// 2. Await Primary or Trigger Hedging
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case res := <-resultChan:
		if res.err == nil {
			return res.route.Provider.Name(), res.providerCallID, nil
		}
		primaryFailed = true
		lastErr = res.err
	case <-hedgeTimer.C:
		// Primary took longer than hedge deadline; hedge to secondary
	}

	// 3. Dispatch Secondary Backup Carrier if available
	secondary := r.selectBackupCarrier(primary)
	if secondary == nil {
		if primaryFailed {
			return "", "", fmt.Errorf("all hedged carriers failed: %w", lastErr)
		}
		// Primary is still in flight; wait for it
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case res := <-resultChan:
			if res.err != nil {
				return "", "", fmt.Errorf("all hedged carriers failed: %w", res.err)
			}
			return res.route.Provider.Name(), res.providerCallID, nil
		}
	}

	// Secondary is available; dispatch it
	go func() {
		start := time.Now()
		provID, callErr := secondary.Provider.PlaceCall(ctx, callID, phoneNumber)
		secondary.recordOutcome(callErr, time.Since(start), r.failureThreshold)
		resultChan <- callResult{route: secondary, providerCallID: provID, err: callErr}
	}()

	// How many more results are we waiting for?
	// If primary already failed, we only wait for 1 result (from secondary).
	// If primary is still in flight (hedged), we wait for whichever finishes first, or up to 2.
	expectedResults := 1
	if !primaryFailed {
		expectedResults = 2
	}

	for i := 0; i < expectedResults; i++ {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case res := <-resultChan:
			if res.err == nil {
				return res.route.Provider.Name(), res.providerCallID, nil
			}
			lastErr = res.err
		}
	}

	return "", "", fmt.Errorf("all hedged carriers failed: %w", lastErr)
}

func (r *CarrierRouter) selectBackupCarrier(exclude *CarrierRoute) *CarrierRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.routes {
		if route == exclude {
			continue
		}
		route.mu.RLock()
		state := route.CircuitState
		route.mu.RUnlock()

		if state != CircuitOpen {
			return route
		}
	}
	return nil
}

func (route *CarrierRoute) recordOutcome(err error, latency time.Duration, threshold int) {
	route.mu.Lock()
	defer route.mu.Unlock()

	route.TotalAttempts++
	if err != nil {
		route.ConsecutiveErrors++
		route.LastFailureTime = time.Now()

		if route.ConsecutiveErrors >= threshold {
			route.CircuitState = CircuitOpen
		}
	} else {
		route.SuccessAttempts++
		route.ConsecutiveErrors = 0
		if route.CircuitState == CircuitHalfOpen {
			route.CircuitState = CircuitClosed
		}
		route.P95LatencyMs = latency.Milliseconds()
	}
}

// CarrierHealthSummary returns telemetry snapshots across all trunks.
func (r *CarrierRouter) CarrierHealthSummary() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var summary []map[string]any
	for _, route := range r.routes {
		route.mu.RLock()
		summary = append(summary, map[string]any{
			"provider":         route.Provider.Name(),
			"cost_per_minute":  route.CostPerMinuteCents,
			"circuit_state":    string(route.CircuitState),
			"consecutive_errs": route.ConsecutiveErrors,
			"total_attempts":   route.TotalAttempts,
			"success_attempts": route.SuccessAttempts,
			"latency_ms":       route.P95LatencyMs,
		})
		route.mu.RUnlock()
	}
	return summary
}
