package provider_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
	"smartdialer/internal/provider"
)

func TestMockProviderA(t *testing.T) {
	p := provider.NewMockProviderA(1.0, 50*time.Millisecond)
	assert.Equal(t, "MockProviderA", p.Name())
	assert.True(t, p.Health().IsHealthy)

	var mu sync.Mutex
	var receivedEvents []string

	p.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		mu.Lock()
		receivedEvents = append(receivedEvents, event.EventType)
		mu.Unlock()
		return nil
	})

	callID := uuid.New()
	provCallID, err := p.PlaceCall(context.Background(), callID, "+15551234567")
	require.NoError(t, err)
	assert.NotEmpty(t, provCallID)

	// Wait for async webhooks to complete
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(receivedEvents) >= 4
	}, 1*time.Second, 20*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"INITIATED", "RINGING", "ANSWERED", "COMPLETED"}, receivedEvents)
}

func TestMockProviderB_Outage(t *testing.T) {
	p := provider.NewMockProviderB(1.0, 50*time.Millisecond)
	p.SetOutage(true)

	assert.False(t, p.Health().IsHealthy)
	_, err := p.PlaceCall(context.Background(), uuid.New(), "+15559876543")
	assert.ErrorIs(t, err, provider.ErrProviderDown)
}

func TestMockProviderB_DuplicateEvents(t *testing.T) {
	p := provider.NewMockProviderB(1.0, 50*time.Millisecond)
	p.SetDuplicateSeq(true)
	p.SetOutOfOrder(false)

	var mu sync.Mutex
	var eventIDs []string

	p.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		mu.Lock()
		eventIDs = append(eventIDs, event.ProviderEventID)
		mu.Unlock()
		return nil
	})

	_, err := p.PlaceCall(context.Background(), uuid.New(), "+15559876543")
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// Should receive INITIATED, RINGING, ANSWERED (1), ANSWERED (duplicate), COMPLETED
		return len(eventIDs) >= 5
	}, 1*time.Second, 20*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	// Verify that at least two events share identical ProviderEventID (duplicate delivery)
	idCounts := make(map[string]int)
	for _, id := range eventIDs {
		idCounts[id]++
	}
	hasDup := false
	for _, count := range idCounts {
		if count > 1 {
			hasDup = true
			break
		}
	}
	assert.True(t, hasDup, "Provider B must emit duplicate event ID")
}

func TestMockProviderB_OutOfOrder(t *testing.T) {
	p := provider.NewMockProviderB(1.0, 50*time.Millisecond)
	p.SetOutOfOrder(true)

	var mu sync.Mutex
	var receivedEvents []string

	p.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		mu.Lock()
		receivedEvents = append(receivedEvents, event.EventType)
		mu.Unlock()
		return nil
	})

	_, err := p.PlaceCall(context.Background(), uuid.New(), "+15559876543")
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(receivedEvents) >= 3
	}, 1*time.Second, 20*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"COMPLETED", "ANSWERED", "RINGING"}, receivedEvents)
}
