package provider_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/provider"
)

func TestCarrierRouter_LCRSelection(t *testing.T) {
	router := provider.NewCarrierRouter(500 * time.Millisecond)

	// Tier 1 carrier: $0.012/min (1.2 cents)
	tier1 := provider.NewMockProviderA(0.40, 100*time.Millisecond)
	// Discount carrier: $0.006/min (0.6 cents)
	discount := provider.NewMockProviderB(0.40, 100*time.Millisecond)

	router.AddCarrier(tier1, 1.2, 1)
	router.AddCarrier(discount, 0.6, 2)

	// LCR should pick the discount carrier because 0.6 < 1.2
	selected, err := router.SelectLCRCarrier()
	require.NoError(t, err)
	require.Equal(t, discount.Name(), selected.Provider.Name())
}

func TestCarrierRouter_FailoverOnCircuitBreaker(t *testing.T) {
	router := provider.NewCarrierRouter(500 * time.Millisecond)

	tier1 := provider.NewMockProviderA(0.40, 100*time.Millisecond)
	discount := provider.NewMockProviderB(0.40, 100*time.Millisecond)

	router.AddCarrier(tier1, 1.2, 1)
	router.AddCarrier(discount, 0.6, 2)

	// Trip discount provider into outage mode
	discount.SetOutage(true)

	ctx := context.Background()
	// Trigger 4 consecutive calls to trip the circuit
	for i := 0; i < 4; i++ {
		_, _, _ = router.PlaceCallWithHedging(ctx, uuid.New(), "+15551234567")
	}

	// Now discount carrier's circuit is OPEN; LCR must automatically route to Tier 1
	selected, err := router.SelectLCRCarrier()
	require.NoError(t, err)
	require.Equal(t, tier1.Name(), selected.Provider.Name(), "Should failover to Tier 1 carrier when discount is tripped")
}

func TestCarrierRouter_HedgingSuccessOnPrimaryFailure(t *testing.T) {
	router := provider.NewCarrierRouter(50 * time.Millisecond)

	primary := provider.NewMockProviderB(0.40, 100*time.Millisecond)
	primary.SetOutage(true) // Primary is completely dead

	backup := provider.NewMockProviderA(0.50, 100*time.Millisecond) // Backup is healthy

	router.AddCarrier(primary, 0.5, 1)
	router.AddCarrier(backup, 1.0, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	carrierName, provCallID, err := router.PlaceCallWithHedging(ctx, uuid.New(), "+15552345678")
	require.NoError(t, err)
	require.Equal(t, backup.Name(), carrierName, "Call should succeed on hedged backup carrier")
	require.NotEmpty(t, provCallID)
}

func TestCarrierRouter_AllCarriersDown(t *testing.T) {
	router := provider.NewCarrierRouter(50 * time.Millisecond)

	prov1 := provider.NewMockProviderB(0.40, 100*time.Millisecond)
	prov1.SetOutage(true)

	prov2 := provider.NewMockProviderB(0.40, 100*time.Millisecond)
	prov2.SetOutage(true)

	router.AddCarrier(prov1, 0.5, 1)
	router.AddCarrier(prov2, 0.8, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, _, err := router.PlaceCallWithHedging(ctx, uuid.New(), "+15559990000")
	require.Error(t, err)
	require.Contains(t, err.Error(), "all hedged carriers failed")
}
