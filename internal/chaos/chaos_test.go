package chaos_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"smartdialer/internal/chaos"
)

func TestChaosInjector_CarrierDropRate(t *testing.T) {
	// 100% carrier drop profile
	alwaysDrop := chaos.NewChaosInjector(chaos.FaultProfile{CarrierDropRate: 1.0})
	err := alwaysDrop.MaybeInjectCarrierDrop()
	require.ErrorIs(t, err, chaos.ErrInjectedCarrierDrop)

	// 0% carrier drop profile
	neverDrop := chaos.NewChaosInjector(chaos.FaultProfile{CarrierDropRate: 0.0})
	err = neverDrop.MaybeInjectCarrierDrop()
	require.NoError(t, err)

	// Disabled injector
	alwaysDrop.SetEnabled(false)
	require.NoError(t, alwaysDrop.MaybeInjectCarrierDrop(), "Disabled injector must not inject faults")
}

func TestChaosInjector_NetworkJitter(t *testing.T) {
	jitterInjector := chaos.NewChaosInjector(chaos.FaultProfile{MaxNetworkJitterMs: 50})

	ctx := context.Background()
	start := time.Now()
	err := jitterInjector.InjectNetworkJitter(ctx)
	require.NoError(t, err)
	duration := time.Since(start)

	require.Less(t, duration, 100*time.Millisecond)
}

func TestChaosInjector_WorkerPanic(t *testing.T) {
	panicInjector := chaos.NewChaosInjector(chaos.FaultProfile{WorkerPanicRate: 1.0})

	require.Panics(t, func() {
		panicInjector.MaybeInjectWorkerPanic()
	}, "Must panic when WorkerPanicRate is 1.0")
}
