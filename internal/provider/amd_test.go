package provider_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"smartdialer/internal/provider"
)

func TestAMDClassifier_HumanSpeech(t *testing.T) {
	classifier := provider.NewDefaultAMDClassifier()

	// Typical human response: "Hello?" lasting 800ms, followed by 1000ms pause
	humanAudio := provider.AMDAnalysis{
		InitialSilence:      200 * time.Millisecond,
		GreetingDuration:    800 * time.Millisecond,
		PostGreetingSilence: 1000 * time.Millisecond,
	}

	signal, reason := classifier.Classify(humanAudio)
	require.Equal(t, provider.AMDSignalHuman, signal)
	require.Contains(t, reason, "Short human greeting")
}

func TestAMDClassifier_VoicemailLongGreeting(t *testing.T) {
	classifier := provider.NewDefaultAMDClassifier()

	// Typical IVR or Voicemail: continuous greeting lasting 3200ms
	machineAudio := provider.AMDAnalysis{
		InitialSilence:      100 * time.Millisecond,
		GreetingDuration:    3200 * time.Millisecond,
		PostGreetingSilence: 200 * time.Millisecond,
	}

	signal, reason := classifier.Classify(machineAudio)
	require.Equal(t, provider.AMDSignalMachine, signal)
	require.Contains(t, reason, "Long greeting duration")
}

func TestAMDClassifier_ContinuousToneVoicemailBeep(t *testing.T) {
	classifier := provider.NewDefaultAMDClassifier()

	// Carrier or answering machine beep tone (e.g. 1000Hz for 1200ms)
	toneAudio := provider.AMDAnalysis{
		ContinuousToneDuration: 1200 * time.Millisecond,
		ToneFrequencyHz:        1000.0,
	}

	signal, reason := classifier.Classify(toneAudio)
	require.Equal(t, provider.AMDSignalMachine, signal)
	require.Contains(t, reason, "Sustained tone detected")
}

func TestAMDClassifier_DeadAirSilence(t *testing.T) {
	classifier := provider.NewDefaultAMDClassifier()

	// Dead air: 5500ms with zero speech
	deadAirAudio := provider.AMDAnalysis{
		InitialSilence: 5500 * time.Millisecond,
	}

	signal, reason := classifier.Classify(deadAirAudio)
	require.Equal(t, provider.AMDSignalSilence, signal)
	require.Contains(t, reason, "Dead air")
}

func TestAMDClassifier_UnsureInterrupted(t *testing.T) {
	classifier := provider.NewDefaultAMDClassifier()

	// Short utterance of 500ms, but post-greeting silence is only 100ms so far
	inFlightAudio := provider.AMDAnalysis{
		GreetingDuration:    500 * time.Millisecond,
		PostGreetingSilence: 100 * time.Millisecond,
	}

	signal, _ := classifier.Classify(inFlightAudio)
	require.Equal(t, provider.AMDSignalUnsure, signal)
}
