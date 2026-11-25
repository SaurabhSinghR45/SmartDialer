package pacing

import (
	"math"
	"sync"
)

// KalmanAnswerRateFilter implements a 1-Dimensional recursive Kalman Filter
// to track the non-stationary borrower answer rate in real-time.
// Unlike fixed EWMA, the Kalman Filter dynamically adjusts its gain based on
// batch size (measurement uncertainty) and process noise (regime change variance).
type KalmanAnswerRateFilter struct {
	mu sync.RWMutex

	// x is the estimated state (true answer rate between 0.01 and 0.99)
	x float64

	// p is the estimation error covariance (uncertainty of the current state)
	p float64

	// q is the process noise covariance (rate of volatility in borrower answer rates)
	q float64

	// rBase is the base observation noise variance
	rBase float64
}

// NewKalmanAnswerRateFilter initializes a filter with sensible collections dialing priors.
func NewKalmanAnswerRateFilter(initialEstimate, processNoise, baseMeasurementNoise float64) *KalmanAnswerRateFilter {
	if initialEstimate <= 0.0 || initialEstimate >= 1.0 {
		initialEstimate = 0.30
	}
	if processNoise <= 0.0 {
		processNoise = 0.005 // Allows tracking shifts without wild oscillation
	}
	if baseMeasurementNoise <= 0.0 {
		baseMeasurementNoise = 0.05
	}

	return &KalmanAnswerRateFilter{
		x:     initialEstimate,
		p:     0.1, // Initial estimation uncertainty
		q:     processNoise,
		rBase: baseMeasurementNoise,
	}
}

// Update incorporates a new batch observation and updates the filter state.
// batchSize determines measurement variance: larger batches have lower measurement noise.
func (f *KalmanAnswerRateFilter) Update(measuredRate float64, batchSize int) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	// 1. Time Update (Predict Step)
	// For 1D random walk of answer rate: x_minus = x, P_minus = P + Q
	xMinus := f.x
	pMinus := f.p + f.q

	// 2. Compute Measurement Noise R based on batch sample size
	// Variance of sample proportion is p*(1-p)/N. We scale rBase by 1/sqrt(N) or 1/N.
	if batchSize < 1 {
		batchSize = 1
	}
	r := f.rBase / math.Sqrt(float64(batchSize))
	if r < 0.001 {
		r = 0.001
	}

	// 3. Measurement Update (Correct Step)
	// Kalman Gain K = P_minus / (P_minus + R)
	k := pMinus / (pMinus + r)

	// Updated state estimate
	measuredRate = math.Max(0.0, math.Min(1.0, measuredRate))
	f.x = xMinus + k*(measuredRate-xMinus)

	// Updated error covariance
	f.p = (1.0 - k) * pMinus

	// Clamping state within operational limits [0.01, 0.99]
	if f.x < 0.01 {
		f.x = 0.01
	} else if f.x > 0.99 {
		f.x = 0.99
	}

	return f.x
}

// Estimate returns the current filtered answer rate estimate.
func (f *KalmanAnswerRateFilter) Estimate() float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.x
}

// Variance returns the current estimation uncertainty.
func (f *KalmanAnswerRateFilter) Variance() float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.p
}

// Reset re-initializes the filter state with a known prior.
func (f *KalmanAnswerRateFilter) Reset(initialRate float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if initialRate < 0.01 {
		initialRate = 0.01
	} else if initialRate > 0.99 {
		initialRate = 0.99
	}
	f.x = initialRate
	f.p = 0.1
}
