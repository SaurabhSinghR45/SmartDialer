package compliance

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

type ComplianceVerdict string

const (
	VerdictApproved                     ComplianceVerdict = "APPROVED"
	VerdictBlockedOutsideTCPAHours      ComplianceVerdict = "BLOCKED_OUTSIDE_TCPA_HOURS"
	VerdictBlockedRegFWeeklyLimit       ComplianceVerdict = "BLOCKED_REG_F_WEEKLY_LIMIT"
	VerdictBlockedRegFConversationWait ComplianceVerdict = "BLOCKED_REG_F_CONVERSATION_WAIT"
	VerdictBlockedDNCRegistry           ComplianceVerdict = "BLOCKED_DNC_REGISTRY"
)

// DNCBloomFilter is a space-efficient probabilistic filter with exact set verification
// for instant O(1) Do-Not-Call lookups.
type DNCBloomFilter struct {
	mu       sync.RWMutex
	bitset   []bool
	size     uint32
	exactMap map[string]struct{}
}

func NewDNCBloomFilter(size uint32) *DNCBloomFilter {
	if size < 1024 {
		size = 65536
	}
	return &DNCBloomFilter{
		bitset:   make([]bool, size),
		size:     size,
		exactMap: make(map[string]struct{}),
	}
}

func (b *DNCBloomFilter) hash(s string, seed uint32) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(fmt.Sprintf("%d:%s", seed, s)))
	return h.Sum32() % b.size
}

// Add adds a phone number to both the Bloom filter and exact registry.
func (b *DNCBloomFilter) Add(phoneNumber string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.bitset[b.hash(phoneNumber, 1)] = true
	b.bitset[b.hash(phoneNumber, 2)] = true
	b.bitset[b.hash(phoneNumber, 3)] = true
	b.exactMap[phoneNumber] = struct{}{}
}

// IsDNC checks whether a phone number is registered on the Do-Not-Call list.
func (b *DNCBloomFilter) IsDNC(phoneNumber string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// 1. Fast Bloom filter check
	if !b.bitset[b.hash(phoneNumber, 1)] ||
		!b.bitset[b.hash(phoneNumber, 2)] ||
		!b.bitset[b.hash(phoneNumber, 3)] {
		return false
	}

	// 2. Exact set lookup eliminates false positives
	_, exists := b.exactMap[phoneNumber]
	return exists
}

// ComplianceGate enforces federal TCPA calling windows and CFPB Regulation F rules.
type ComplianceGate struct {
	dncFilter *DNCBloomFilter
}

func NewComplianceGate(dncFilter *DNCBloomFilter) *ComplianceGate {
	if dncFilter == nil {
		dncFilter = NewDNCBloomFilter(65536)
	}
	return &ComplianceGate{
		dncFilter: dncFilter,
	}
}

// DNCRegistry returns the internal DNC filter for administrative population.
func (g *ComplianceGate) DNCRegistry() *DNCBloomFilter {
	return g.dncFilter
}

// IsCallingWindowAllowed checks if the current time falls between 08:00 AM and 09:00 PM
// in the borrower's local IANA timezone per TCPA guidelines.
func IsCallingWindowAllowed(timezone string, now time.Time) (bool, string) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		// Fallback to Eastern Standard Time if timezone invalid
		loc, _ = time.LoadLocation("America/New_York")
	}

	localTime := now.In(loc)
	hour := localTime.Hour()

	// 08:00 AM to 09:00 PM (21:00)
	if hour < 8 || hour >= 21 {
		return false, fmt.Sprintf("TCPA violation: local time is %02d:%02d in %s (allowed: 08:00-21:00)", hour, localTime.Minute(), loc.String())
	}

	return true, ""
}

// CheckRegulationF enforces CFPB 12 CFR § 1006.14 (7 calls in 7 days, conversation cool-off).
func CheckRegulationF(attemptsLast7Days int, lastConnectedAt *time.Time, now time.Time) (bool, ComplianceVerdict, string) {
	// Rule 1: Max 7 attempts in rolling 7-day period
	if attemptsLast7Days >= 7 {
		return false, VerdictBlockedRegFWeeklyLimit, fmt.Sprintf("Reg F violation: %d call attempts in last 7 days (cap is 7)", attemptsLast7Days)
	}

	// Rule 2: Minimum 7-day cool-off after a conversation occurred
	if lastConnectedAt != nil {
		sevenDays := 7 * 24 * time.Hour
		if now.Sub(*lastConnectedAt) < sevenDays {
			remaining := sevenDays - now.Sub(*lastConnectedAt)
			return false, VerdictBlockedRegFConversationWait, fmt.Sprintf("Reg F violation: debtor conversation occurred %v ago (must wait %v)", now.Sub(*lastConnectedAt).Round(time.Hour), remaining.Round(time.Hour))
		}
	}

	return true, VerdictApproved, ""
}

// ValidateDial checks all regulatory gates for an outbound call attempt.
func (g *ComplianceGate) ValidateDial(
	phoneNumber string,
	timezone string,
	attemptsLast7Days int,
	lastConnectedAt *time.Time,
	now time.Time,
) (ComplianceVerdict, string) {
	// 1. DNC Check
	if g.dncFilter.IsDNC(phoneNumber) {
		return VerdictBlockedDNCRegistry, "Phone number is listed on the Do-Not-Call registry"
	}

	// 2. TCPA Timezone Window
	allowed, reason := IsCallingWindowAllowed(timezone, now)
	if !allowed {
		return VerdictBlockedOutsideTCPAHours, reason
	}

	// 3. Regulation F 7-in-7 & Conversation Window
	regFAllowed, verdict, regFReason := CheckRegulationF(attemptsLast7Days, lastConnectedAt, now)
	if !regFAllowed {
		return verdict, regFReason
	}

	return VerdictApproved, "Compliance checks passed"
}
