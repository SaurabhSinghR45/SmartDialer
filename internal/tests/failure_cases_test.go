package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
	"smartdialer/internal/pacing"
	"smartdialer/internal/safety"
	"smartdialer/internal/store"
	"smartdialer/internal/worker"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "postgres://postgres:postgres@localhost:5432/smartdialer?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Skipf("Skipping integration test: unable to connect to DB: %v", err)
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("Skipping integration test: ping failed: %v", err)
		return nil
	}
	return pool
}

func setupFailureTest(t *testing.T) (*store.PostgresStore, *pgxpool.Pool, context.Context) {
	pool := getTestPool(t)
	st := store.NewPostgresStore(pool)
	ctx := context.Background()

	_ = st.Migrate(ctx)
	_, _ = pool.Exec(ctx, "TRUNCATE pacing_audit_logs, call_events, calls, borrowers, agents CASCADE;")

	return st, pool, ctx
}

// Failure Case 1: Worker crash after INITIATED -> Crash Reaper recovers agent & call
func TestFailureCase1_WorkerCrashMidSetup(t *testing.T) {
	st, pool, ctx := setupFailureTest(t)
	defer st.Close()

	agent, err := st.CreateAgent(ctx, "Agent-CrashVictim", domain.AgentAvailable)
	require.NoError(t, err)
	borrower, err := st.CreateBorrower(ctx, "+15551000001")
	require.NoError(t, err)

	call, _, _, err := st.ReserveAgentAndBorrower(ctx, uuid.New(), "crashed-worker-7", "MockProviderA")
	require.NoError(t, err)

	// Transition to INITIATED
	err = st.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-init-crash",
		EventType:       "INITIATED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-crash-1"},
	})
	require.NoError(t, err)

	// Simulate worker crash by aging the call beyond reaper threshold (15s)
	_, err = pool.Exec(ctx, "UPDATE calls SET updated_at = NOW() - INTERVAL '15 seconds' WHERE id = $1;", call.ID)
	require.NoError(t, err)

	reaper := worker.NewReaper(st, worker.ReaperConfig{
		ReservedTimeout: 10 * time.Second,
		AnsweredTimeout: 5 * time.Second,
	}, nil)

	reclaimed, err := reaper.ReapOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, reclaimed, "Reaper must reclaim exactly 1 stuck call")

	repairedAgent, err := st.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AgentAvailable, repairedAgent.State, "Agent must be returned to AVAILABLE")
	assert.Nil(t, repairedAgent.CurrentCallID)

	repairedCall, err := st.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallFailed, repairedCall.State, "Call must be transitioned to FAILED")

	repairedBorrower, err := st.GetBorrower(ctx, borrower.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.BorrowerPending, repairedBorrower.State, "Borrower must be rescheduled to PENDING")
	assert.Equal(t, 1, repairedBorrower.RetryCount)
}

// Failure Case 2: Provider outage -> Circuit Breaker trips & protects system
func TestFailureCase2_ProviderOutageCircuitBreaker(t *testing.T) {
	st, _, ctx := setupFailureTest(t)
	defer st.Close()

	ctrl := safety.NewController(st)
	predEngine := pacing.NewPredictiveEngine()
	campaignID := uuid.New()

	// High carrier error rate: 35% (> 25% threshold)
	metrics := &store.PacingMetrics{
		AvailableAgents:   10,
		InFlightCalls:     2,
		ProviderErrorRate: 0.35,
		RollingAnswerRate: 0.25,
	}

	proposal, err := predEngine.Propose(ctx, campaignID, metrics)
	require.NoError(t, err)

	decision, err := ctrl.Evaluate(ctx, proposal, metrics)
	require.NoError(t, err)
	assert.Equal(t, domain.DecisionFallbackProgressive, decision.Decision)
	assert.Equal(t, "PROGRESSIVE_FALLBACK", decision.Mode)
	assert.Equal(t, 8, decision.ApprovedCalls, "Should fallback to progressive 10 - 2 = 8")
	assert.Contains(t, decision.Reason, "Carrier Error Rate 35.0%")
}

// Failure Case 3: Sudden agent drop (100 -> 60 in <2s) -> Safety Controller clamps/falls back
func TestFailureCase3_SuddenAgentDrop(t *testing.T) {
	st, _, ctx := setupFailureTest(t)
	defer st.Close()

	ctrl := safety.NewController(st)
	predEngine := pacing.NewPredictiveEngine()
	campaignID := uuid.New()

	metrics := &store.PacingMetrics{
		TotalAgents:         100,
		AvailableAgents:     60,
		AgentDropRateLast5s: 0.40, // 40% dropped (> 20% guard)
		RollingAnswerRate:   0.30,
	}

	proposal, err := predEngine.Propose(ctx, campaignID, metrics)
	require.NoError(t, err)

	decision, err := ctrl.Evaluate(ctx, proposal, metrics)
	require.NoError(t, err)
	assert.Equal(t, domain.DecisionReject, decision.Decision)
	assert.Equal(t, 0, decision.ApprovedCalls, "Must approve 0 calls during sudden drop")
	assert.Contains(t, decision.Reason, "Sudden Drop Guard")
}

// Failure Case 4: Duplicate events (ANSWERED x3) -> Idempotent single transition
func TestFailureCase4_DuplicateEventsIdempotency(t *testing.T) {
	st, pool, ctx := setupFailureTest(t)
	defer st.Close()

	_, _ = st.CreateAgent(ctx, "Agent-Dup", domain.AgentAvailable)
	_, _ = st.CreateBorrower(ctx, "+15554440001")
	campaignID := uuid.New()

	call, _, _, err := st.ReserveAgentAndBorrower(ctx, campaignID, "w1", "MockProviderB")
	require.NoError(t, err)

	dupEventID := "provider-b-evt-dup-x3"
	event := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: dupEventID,
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "pb-dup"},
	}

	// Deliver 3 times
	err1 := st.ProcessProviderEvent(ctx, event)
	err2 := st.ProcessProviderEvent(ctx, event)
	err3 := st.ProcessProviderEvent(ctx, event)

	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.NoError(t, err3)

	var count int
	_ = pool.QueryRow(ctx, "SELECT COUNT(*) FROM call_events WHERE provider_event_id = $1;", dupEventID).Scan(&count)
	assert.Equal(t, 1, count, "Exactly 1 event row must be recorded in call_events")

	updatedCall, err := st.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallAnswered, updatedCall.State)
}

// Failure Case 5: Out-of-order events (COMPLETED -> ANSWERED -> RINGING) -> No state regression
func TestFailureCase5_OutOfOrderEventsMonotonic(t *testing.T) {
	st, _, ctx := setupFailureTest(t)
	defer st.Close()

	_, _ = st.CreateAgent(ctx, "Agent-Order", domain.AgentAvailable)
	_, _ = st.CreateBorrower(ctx, "+15555550001")
	campaignID := uuid.New()

	call, _, _, err := st.ReserveAgentAndBorrower(ctx, campaignID, "w1", "MockProviderB")
	require.NoError(t, err)

	// Step 1: Deliver COMPLETED first
	err = st.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-comp-1st",
		EventType:       "COMPLETED",
		EventTimestamp:  time.Now().UTC(),
	})
	require.NoError(t, err)

	c1, err := st.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallCompleted, c1.State)

	// Step 2: Late ANSWERED
	err = st.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-ans-late",
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
	})
	require.NoError(t, err)

	c2, err := st.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallCompleted, c2.State, "Must not regress to ANSWERED")

	// Step 3: Late RINGING
	err = st.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-ring-late",
		EventType:       "RINGING",
		EventTimestamp:  time.Now().UTC(),
	})
	require.NoError(t, err)

	c3, err := st.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallCompleted, c3.State, "Must not regress to RINGING")
}
