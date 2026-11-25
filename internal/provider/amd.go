package provider

import (
	"fmt"
	"time"
)

// AMDSignal specifies the classification verdict of Answering Machine Detection.
type AMDSignal string

const (
	AMDSignalHuman   AMDSignal = "HUMAN"
	AMDSignalMachine AMDSignal = "MACHINE"
	AMDSignalSilence AMDSignal = "SILENCE"
	AMDSignalUnsure  AMDSignal = "UNSURE"
)

// AMDAnalysis represents acoustic and temporal metrics extracted from call audio.
type AMDAnalysis struct {
	// InitialSilence is the duration before any acoustic energy is heard.
	InitialSilence time.Duration

	// GreetingDuration is the duration of continuous initial speech energy.
	GreetingDuration time.Duration

	// PostGreetingSilence is the pause following the initial greeting burst.
	PostGreetingSilence time.Duration

	// ContinuousToneDuration is the duration of a sustained single frequency (e.g. voicemail beep or SIT tone).
	ContinuousToneDuration time.Duration

	// ToneFrequencyHz is the primary frequency detected (e.g. 1000Hz or 440Hz).
	ToneFrequencyHz float64

	// EnergyLevelDb is the peak energy level in decibels.
	EnergyLevelDb float64
}

// AMDClassifier evaluates audio temporal patterns to determine whether a call was answered by a human or voicemail.
type AMDClassifier struct {
	MaxHumanGreetingDuration   time.Duration // E.g., 1800ms ("Hello?", "Smith residence")
	MinMachineGreetingDuration time.Duration // E.g., 2200ms ("Hi, you have reached the voicemail of...")
	MinHumanPostSilence        time.Duration // E.g., 700ms (Human waits for a response)
	ContinuousToneThreshold    time.Duration // E.g., 1000ms (Sustained beep tone)
	MaxInitialSilenceTimeout   time.Duration // E.g., 5000ms (Dead air / silence)
}

// NewDefaultAMDClassifier provides standard collections industry thresholds.
func NewDefaultAMDClassifier() *AMDClassifier {
	return &AMDClassifier{
		MaxHumanGreetingDuration:   1800 * time.Millisecond,
		MinMachineGreetingDuration: 2200 * time.Millisecond,
		MinHumanPostSilence:        700 * time.Millisecond,
		ContinuousToneThreshold:    1000 * time.Millisecond,
		MaxInitialSilenceTimeout:   5000 * time.Millisecond,
	}
}

// Classify runs the decision tree on acoustic metrics.
func (c *AMDClassifier) Classify(a AMDAnalysis) (AMDSignal, string) {
	// 1. Sustained tone check (Voicemail beep or Telecom SIT intercept)
	if a.ContinuousToneDuration >= c.ContinuousToneThreshold {
		return AMDSignalMachine, fmt.Sprintf("Sustained tone detected (%.0fHz for %v)", a.ToneFrequencyHz, a.ContinuousToneDuration)
	}

	// 2. Dead air / Initial Silence Timeout
	if a.InitialSilence >= c.MaxInitialSilenceTimeout && a.GreetingDuration == 0 {
		return AMDSignalSilence, fmt.Sprintf("Dead air: initial silence exceeded %v", c.MaxInitialSilenceTimeout)
	}

	// 3. Long continuous greeting -> Voicemail message
	if a.GreetingDuration >= c.MinMachineGreetingDuration {
		return AMDSignalMachine, fmt.Sprintf("Long greeting duration (%v >= %v)", a.GreetingDuration, c.MinMachineGreetingDuration)
	}

	// 4. Short greeting followed by human conversational pause -> Human
	if a.GreetingDuration <= c.MaxHumanGreetingDuration && a.GreetingDuration > 100*time.Millisecond {
		if a.PostGreetingSilence >= c.MinHumanPostSilence {
			return AMDSignalHuman, fmt.Sprintf("Short human greeting (%v) followed by pause (%v)", a.GreetingDuration, a.PostGreetingSilence)
		}
	}

	// 5. Short greeting without adequate silence window yet
	if a.GreetingDuration <= c.MaxHumanGreetingDuration && a.PostGreetingSilence < c.MinHumanPostSilence {
		return AMDSignalUnsure, "Awaiting conversational silence after short greeting"
	}

	return AMDSignalUnsure, "Acoustic signature indeterminate"
}
