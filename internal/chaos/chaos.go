package chaos

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"
)

var (
	ErrInjectedCarrierDrop   = errors.New("chaos: simulated SIP 503 carrier drop")
	ErrInjectedDatabaseStall = errors.New("chaos: simulated database lock contention timeout")
)

// FaultProfile defines active failure injection probabilities.
type FaultProfile struct {
	CarrierDropRate    float64 // Probability of carrier 503 drop [0.0, 1.0]
	MaxNetworkJitterMs int     // Max milliseconds of artificial network delay
	DatabaseStallRate  float64 // Probability of slow DB query stall
	WorkerPanicRate    float64 // Probability of simulated goroutine panic
}

// ChaosInjector dynamically injects controlled failures for resilience testing.
type ChaosInjector struct {
	mu      sync.RWMutex
	enabled bool
	profile FaultProfile
	rng     *rand.Rand
}

func NewChaosInjector(profile FaultProfile) *ChaosInjector {
	return &ChaosInjector{
		enabled: true,
		profile: profile,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (c *ChaosInjector) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
}

// InjectNetworkJitter injects artificial network delay if configured.
func (c *ChaosInjector) InjectNetworkJitter(ctx context.Context) error {
	c.mu.RLock()
	if !c.enabled || c.profile.MaxNetworkJitterMs <= 0 {
		c.mu.RUnlock()
		return nil
	}
	jitter := time.Duration(c.rng.Intn(c.profile.MaxNetworkJitterMs)) * time.Millisecond
	c.mu.RUnlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(jitter):
		return nil
	}
}

// MaybeInjectCarrierDrop returns an injected telecom outage error according to CarrierDropRate.
func (c *ChaosInjector) MaybeInjectCarrierDrop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.enabled || c.profile.CarrierDropRate <= 0 {
		return nil
	}
	if c.rng.Float64() < c.profile.CarrierDropRate {
		return ErrInjectedCarrierDrop
	}
	return nil
}

// MaybeInjectWorkerPanic triggers a panic according to WorkerPanicRate to test Crash Reaper recovery.
func (c *ChaosInjector) MaybeInjectWorkerPanic() {
	c.mu.Lock()
	shouldPanic := c.enabled && c.profile.WorkerPanicRate > 0 && c.rng.Float64() < c.profile.WorkerPanicRate
	c.mu.Unlock()

	if shouldPanic {
		panic("chaos: simulated worker thread crash")
	}
}
