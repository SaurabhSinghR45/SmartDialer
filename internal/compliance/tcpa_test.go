package compliance_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"smartdialer/internal/compliance"
)

func TestTCPATimezoneCallingWindow(t *testing.T) {
	// Fixed UTC point: 13:30 UTC
	// In New York (EDT, UTC-4): 09:30 AM -> ALLOWED
	// In Los Angeles (PDT, UTC-7): 06:30 AM -> BLOCKED (<08:00)
	fixedUTC := time.Date(2026, 6, 15, 13, 30, 0, 0, time.UTC)

	allowedNY, _ := compliance.IsCallingWindowAllowed("America/New_York", fixedUTC)
	require.True(t, allowedNY, "09:30 AM local time should be allowed under TCPA")

	allowedLA, reasonLA := compliance.IsCallingWindowAllowed("America/Los_Angeles", fixedUTC)
	require.False(t, allowedLA, "06:30 AM local time should be blocked under TCPA")
	require.Contains(t, reasonLA, "TCPA violation")

	// Fixed UTC point: 02:30 UTC next day
	// In New York: 10:30 PM (22:30) -> BLOCKED (>=21:00)
	// In Los Angeles: 07:30 PM (19:30) -> ALLOWED
	eveningUTC := time.Date(2026, 6, 16, 2, 30, 0, 0, time.UTC)

	allowedNYEve, _ := compliance.IsCallingWindowAllowed("America/New_York", eveningUTC)
	require.False(t, allowedNYEve, "22:30 local time should be blocked")

	allowedLAEve, _ := compliance.IsCallingWindowAllowed("America/Los_Angeles", eveningUTC)
	require.True(t, allowedLAEve, "19:30 local time should be allowed")
}

func TestRegulationF_FrequencyCap(t *testing.T) {
	now := time.Now()

	// 6 attempts in last 7 days -> Allowed
	allowed, verdict, _ := compliance.CheckRegulationF(6, nil, now)
	require.True(t, allowed)
	require.Equal(t, compliance.VerdictApproved, verdict)

	// 7 attempts in last 7 days -> Blocked by 7-in-7 rule
	allowed7, verdict7, reason7 := compliance.CheckRegulationF(7, nil, now)
	require.False(t, allowed7)
	require.Equal(t, compliance.VerdictBlockedRegFWeeklyLimit, verdict7)
	require.Contains(t, reason7, "Reg F violation")

	// 8 attempts in last 7 days -> Blocked
	allowed8, verdict8, _ := compliance.CheckRegulationF(8, nil, now)
	require.False(t, allowed8)
	require.Equal(t, compliance.VerdictBlockedRegFWeeklyLimit, verdict8)
}

func TestRegulationF_ConversationCooloff(t *testing.T) {
	now := time.Now()

	// Had a conversation 3 days ago -> Blocked
	recentConn := now.Add(-3 * 24 * time.Hour)
	allowed, verdict, reason := compliance.CheckRegulationF(2, &recentConn, now)
	require.False(t, allowed)
	require.Equal(t, compliance.VerdictBlockedRegFConversationWait, verdict)
	require.Contains(t, reason, "debtor conversation occurred")

	// Had a conversation 8 days ago -> Allowed
	oldConn := now.Add(-8 * 24 * time.Hour)
	allowedOld, verdictOld, _ := compliance.CheckRegulationF(2, &oldConn, now)
	require.True(t, allowedOld)
	require.Equal(t, compliance.VerdictApproved, verdictOld)
}

func TestDNCBloomFilter_RegistrationAndLookup(t *testing.T) {
	filter := compliance.NewDNCBloomFilter(1024)

	dncNumber := "+15551239999"
	cleanNumber := "+15551238888"

	filter.Add(dncNumber)

	require.True(t, filter.IsDNC(dncNumber), "DNC number must be flagged")
	require.False(t, filter.IsDNC(cleanNumber), "Clean number must not be flagged")
}

func TestComplianceGate_UnifiedValidation(t *testing.T) {
	gate := compliance.NewComplianceGate(nil)
	gate.DNCRegistry().Add("+19990001111")

	// 10:00 AM NY time
	validTime := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)

	// Case 1: DNC Number
	v1, _ := gate.ValidateDial("+19990001111", "America/New_York", 0, nil, validTime)
	require.Equal(t, compliance.VerdictBlockedDNCRegistry, v1)

	// Case 2: Outside TCPA Window (6:00 AM in LA)
	earlyLA := time.Date(2026, 6, 15, 13, 0, 0, 0, time.UTC)
	v2, _ := gate.ValidateDial("+15550002222", "America/Los_Angeles", 0, nil, earlyLA)
	require.Equal(t, compliance.VerdictBlockedOutsideTCPAHours, v2)

	// Case 3: Reg F limit reached
	v3, _ := gate.ValidateDial("+15550003333", "America/New_York", 7, nil, validTime)
	require.Equal(t, compliance.VerdictBlockedRegFWeeklyLimit, v3)

	// Case 4: Compliant dial
	v4, _ := gate.ValidateDial("+15550004444", "America/New_York", 2, nil, validTime)
	require.Equal(t, compliance.VerdictApproved, v4)
}
