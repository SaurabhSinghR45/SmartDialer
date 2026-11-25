package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"

	"smartdialer/internal/domain"
)

var (
	ErrProviderDown    = errors.New("telecom provider outage / circuit open")
	ErrProviderTimeout = errors.New("telecom provider call setup timed out")
)

// ProviderHealth summarizes the provider's operational status.
type ProviderHealth struct {
	IsHealthy bool    `json:"is_healthy"`
	LatencyMs int64   `json:"latency_ms"`
	ErrorRate float64 `json:"error_rate"`
}

// WebhookHandler receives and processes asynchronous provider webhooks.
type WebhookHandler func(ctx context.Context, event domain.CallEvent) error

// TelecomProvider defines the contract for telecommunications carriers.
type TelecomProvider interface {
	Name() string
	PlaceCall(ctx context.Context, callID uuid.UUID, phoneNumber string) (providerCallID string, err error)
	Health() ProviderHealth
	RegisterWebhookHandler(handler WebhookHandler)
}

// MockProviderA simulates a reliable, low-latency, strictly ordered carrier.
type MockProviderA struct {
	mu             sync.RWMutex
	handler        WebhookHandler
	answerRate     float64
	talkDuration   time.Duration
	failRate       float64
	rng            *rand.Rand
}

func NewMockProviderA(answerRate float64, talkDuration time.Duration) *MockProviderA {
	return &MockProviderA{
		answerRate:   answerRate,
		talkDuration: talkDuration,
		failRate:     0.01, // 99% reliability
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (p *MockProviderA) Name() string {
	return "MockProviderA"
}

func (p *MockProviderA) RegisterWebhookHandler(handler WebhookHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
}

func (p *MockProviderA) Health() ProviderHealth {
	return ProviderHealth{
		IsHealthy: true,
		LatencyMs: 35,
		ErrorRate: 0.01,
	}
}

func (p *MockProviderA) PlaceCall(ctx context.Context, callID uuid.UUID, phoneNumber string) (string, error) {
	// Simulate 20-50ms placement latency
	latency := 20 + time.Duration(rand.Intn(30))*time.Millisecond
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(latency):
	}

	p.mu.Lock()
	isFail := p.rng.Float64() < p.failRate
	isAnswer := p.rng.Float64() < p.answerRate
	handler := p.handler
	p.mu.Unlock()

	provCallID := fmt.Sprintf("prova-%s", uuid.New().String()[:8])

	if isFail {
		// Immediately return provider failure
		return "", ErrProviderDown
	}

	// Asynchronously emit ordered webhooks
	if handler != nil {
		go p.emitLifecycle(callID, provCallID, isAnswer)
	}

	return provCallID, nil
}

func (p *MockProviderA) emitLifecycle(callID uuid.UUID, provCallID string, isAnswer bool) {
	ctx := context.Background()

	// 1. INITIATED
	time.Sleep(30 * time.Millisecond)
	p.sendEvent(ctx, callID, "INITIATED", provCallID, nil)

	// 2. RINGING
	time.Sleep(50 * time.Millisecond)
	p.sendEvent(ctx, callID, "RINGING", provCallID, nil)

	if !isAnswer {
		time.Sleep(100 * time.Millisecond)
		p.sendEvent(ctx, callID, "NO_ANSWER", provCallID, map[string]any{"reason": "borrower_no_answer"})
		return
	}

	// 3. ANSWERED
	time.Sleep(80 * time.Millisecond)
	p.sendEvent(ctx, callID, "ANSWERED", provCallID, nil)

	// In real workflow, dialer will bridge call and transition to CONNECTED.
	// We simulate talk duration before COMPLETED
	talk := p.talkDuration
	if talk <= 0 {
		talk = 150 * time.Millisecond
	}
	time.Sleep(talk)

	// 4. COMPLETED
	p.sendEvent(ctx, callID, "COMPLETED", provCallID, map[string]any{"duration_ms": talk.Milliseconds()})
}

func (p *MockProviderA) sendEvent(ctx context.Context, callID uuid.UUID, eventType, provCallID string, payload map[string]any) {
	p.mu.RLock()
	handler := p.handler
	p.mu.RUnlock()

	if handler == nil {
		return
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	payload["provider_call_id"] = provCallID

	event := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          callID,
		ProviderEventID: fmt.Sprintf("%s-%s-%d", provCallID, eventType, time.Now().UnixNano()),
		EventType:       eventType,
		EventTimestamp:  time.Now().UTC(),
		Payload:         payload,
		ProcessedAt:     time.Now().UTC(),
	}
	_ = handler(ctx, event)
}

// MockProviderB simulates a flaky, higher-latency carrier with intentional duplicate and out-of-order events.
type MockProviderB struct {
	mu           sync.RWMutex
	handler      WebhookHandler
	answerRate   float64
	talkDuration time.Duration
	outageMode   bool
	duplicateSeq bool
	outOfOrder   bool
	rng          *rand.Rand
}

func NewMockProviderB(answerRate float64, talkDuration time.Duration) *MockProviderB {
	return &MockProviderB{
		answerRate:   answerRate,
		talkDuration: talkDuration,
		outageMode:   false,
		duplicateSeq: true,
		outOfOrder:   false,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (p *MockProviderB) Name() string {
	return "MockProviderB"
}

func (p *MockProviderB) SetOutage(outage bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outageMode = outage
}

func (p *MockProviderB) SetOutOfOrder(outOfOrder bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outOfOrder = outOfOrder
}

func (p *MockProviderB) SetDuplicateSeq(dup bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.duplicateSeq = dup
}

func (p *MockProviderB) RegisterWebhookHandler(handler WebhookHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler = handler
}

func (p *MockProviderB) Health() ProviderHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.outageMode {
		return ProviderHealth{IsHealthy: false, LatencyMs: 500, ErrorRate: 1.0}
	}
	return ProviderHealth{IsHealthy: true, LatencyMs: 220, ErrorRate: 0.15}
}

func (p *MockProviderB) PlaceCall(ctx context.Context, callID uuid.UUID, phoneNumber string) (string, error) {
	p.mu.RLock()
	outage := p.outageMode
	p.mu.RUnlock()

	if outage {
		return "", ErrProviderDown
	}

	// 150-400ms latency
	latency := 150 + time.Duration(rand.Intn(250))*time.Millisecond
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(latency):
	}

	p.mu.RLock()
	if p.outageMode {
		p.mu.RUnlock()
		return "", ErrProviderDown
	}
	isAnswer := p.rng.Float64() < p.answerRate
	handler := p.handler
	outOfOrder := p.outOfOrder
	duplicateSeq := p.duplicateSeq
	p.mu.RUnlock()

	provCallID := fmt.Sprintf("provb-%s", uuid.New().String()[:8])

	if handler != nil {
		go p.emitComplexLifecycle(callID, provCallID, isAnswer, outOfOrder, duplicateSeq)
	}

	return provCallID, nil
}

func (p *MockProviderB) emitComplexLifecycle(callID uuid.UUID, provCallID string, isAnswer, outOfOrder, duplicateSeq bool) {
	ctx := context.Background()

	if outOfOrder {
		// Out-of-order scenario: emit COMPLETED, then ANSWERED, then RINGING
		time.Sleep(100 * time.Millisecond)
		p.sendSpecificEvent(ctx, callID, "COMPLETED", provCallID, "evt-completed-1", nil)

		time.Sleep(40 * time.Millisecond)
		p.sendSpecificEvent(ctx, callID, "ANSWERED", provCallID, "evt-answered-1", nil)

		time.Sleep(30 * time.Millisecond)
		p.sendSpecificEvent(ctx, callID, "RINGING", provCallID, "evt-ringing-1", nil)
		return
	}

	// Standard or Duplicate Sequence
	time.Sleep(50 * time.Millisecond)
	p.sendSpecificEvent(ctx, callID, "INITIATED", provCallID, fmt.Sprintf("%s-init", provCallID), nil)

	time.Sleep(60 * time.Millisecond)
	p.sendSpecificEvent(ctx, callID, "RINGING", provCallID, fmt.Sprintf("%s-ring", provCallID), nil)

	if !isAnswer {
		time.Sleep(100 * time.Millisecond)
		p.sendSpecificEvent(ctx, callID, "NO_ANSWER", provCallID, fmt.Sprintf("%s-noans", provCallID), nil)
		return
	}

	time.Sleep(80 * time.Millisecond)
	ansEventID := fmt.Sprintf("%s-ans-dup", provCallID)

	// Emit ANSWERED
	p.sendSpecificEvent(ctx, callID, "ANSWERED", provCallID, ansEventID, nil)

	if duplicateSeq {
		// Intentionally emit duplicate ANSWERED with the identical provider_event_id
		time.Sleep(20 * time.Millisecond)
		p.sendSpecificEvent(ctx, callID, "ANSWERED", provCallID, ansEventID, nil)
	}

	talk := p.talkDuration
	if talk <= 0 {
		talk = 150 * time.Millisecond
	}
	time.Sleep(talk)

	// Emit COMPLETED
	p.sendSpecificEvent(ctx, callID, "COMPLETED", provCallID, fmt.Sprintf("%s-comp", provCallID), nil)
}

func (p *MockProviderB) sendSpecificEvent(ctx context.Context, callID uuid.UUID, eventType, provCallID, eventID string, payload map[string]any) {
	p.mu.RLock()
	handler := p.handler
	p.mu.RUnlock()

	if handler == nil {
		return
	}
	if payload == nil {
		payload = make(map[string]any)
	}
	payload["provider_call_id"] = provCallID

	event := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          callID,
		ProviderEventID: eventID,
		EventType:       eventType,
		EventTimestamp:  time.Now().UTC(),
		Payload:         payload,
		ProcessedAt:     time.Now().UTC(),
	}
	_ = handler(ctx, event)
}
