package pacing_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"smartdialer/internal/pacing"
)

func TestKalmanFilter_InitialEstimateAndBounds(t *testing.T) {
	kf := pacing.NewKalmanAnswerRateFilter(0.35, 0.005, 0.05)
	require.InDelta(t, 0.35, kf.Estimate(), 0.001)
	require.Greater(t, kf.Variance(), 0.0)

	// Filter clamps values within [0.01, 0.99]
	kfClampedLow := pacing.NewKalmanAnswerRateFilter(-0.5, 0.005, 0.05)
	require.InDelta(t, 0.30, kfClampedLow.Estimate(), 0.001, "Defaults on invalid input")

	// Update with extreme values
	kf.Update(1.5, 10)
	require.LessOrEqual(t, kf.Estimate(), 0.99)

	kf.Update(-0.5, 100)
	require.GreaterOrEqual(t, kf.Estimate(), 0.01)
}

func TestKalmanFilter_NoiseSmoothing(t *testing.T) {
	kf := pacing.NewKalmanAnswerRateFilter(0.40, 0.001, 0.05)
	initialVariance := kf.Variance()

	// Simulate oscillating noisy measurements around 0.40
	noisyInputs := []float64{0.48, 0.32, 0.45, 0.35, 0.43, 0.37, 0.42, 0.39}
	for _, obs := range noisyInputs {
		kf.Update(obs, 20)
	}

	// The estimate should remain tightly clustered near 0.40
	require.InDelta(t, 0.40, kf.Estimate(), 0.03)

	// Estimation uncertainty should decrease with observations
	require.Less(t, kf.Variance(), initialVariance)
}

func TestKalmanFilter_RegimeShiftAdaptation(t *testing.T) {
	// High process noise (0.01) allows fast tracking of regime shift (e.g. 70% down to 15%)
	kf := pacing.NewKalmanAnswerRateFilter(0.70, 0.01, 0.04)

	// Feed 8 batches of 15% answer rate
	for i := 0; i < 8; i++ {
		kf.Update(0.15, 25)
	}

	// Filter should have tracked down significantly toward 0.15
	require.Less(t, kf.Estimate(), 0.25, "Kalman filter must adapt to sudden carrier drop")
}

func TestKalmanFilter_BatchSizeWeighting(t *testing.T) {
	kfSmallBatch := pacing.NewKalmanAnswerRateFilter(0.30, 0.005, 0.05)
	kfLargeBatch := pacing.NewKalmanAnswerRateFilter(0.30, 0.005, 0.05)

	// A single observation of 0.80 with batch size 1 vs 100
	kfSmallBatch.Update(0.80, 1)
	kfLargeBatch.Update(0.80, 100)

	// The large batch should shift the state much more aggressively because measurement uncertainty is lower
	diffSmall := kfSmallBatch.Estimate() - 0.30
	diffLarge := kfLargeBatch.Estimate() - 0.30
	require.Greater(t, diffLarge, diffSmall, "Large batch should have higher Kalman gain than small batch")
}
