package store_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
	"smartdialer/internal/store"
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
		t.Skipf("Skipping Postgres integration tests: unable to connect: %v", err)
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("Skipping Postgres integration tests: ping failed: %v", err)
		return nil
	}
	return pool
}

func setupTestStore(t *testing.T) (*store.PostgresStore, context.Context) {
	pool := getTestPool(t)
	s := store.NewPostgresStore(pool)
	ctx := context.Background()

	err := s.Migrate(ctx)
	require.NoError(t, err)

	// Clean tables before test
	_, _ = pool.Exec(ctx, "TRUNCATE pacing_audit_logs, call_events, calls, borrowers, agents CASCADE;")

	return s, ctx
}

func TestPostgresStore_AtomicReservation_Concurrency(t *testing.T) {
	s, ctx := setupTestStore(t)
	defer s.Close()

	// Seed 5 available agents
	agentIDs := make([]uuid.UUID, 5)
	for i := 0; i < 5; i++ {
		a, err := s.CreateAgent(ctx, "Agent-"+uuid.New().String()[:4], domain.AgentAvailable)
		require.NoError(t, err)
		agentIDs[i] = a.ID
	}

	// Seed 5 pending borrowers
	for i := 0; i < 5; i++ {
		_, err := s.CreateBorrower(ctx, "+1555000"+string(rune('0'+i)))
		require.NoError(t, err)
	}

	campaignID := uuid.New()
	var wg sync.WaitGroup
	type res struct {
		call     *domain.Call
		agent    *domain.Agent
		borrower *domain.Borrower
		err      error
	}

	// Spawn 10 concurrent reservation workers competing for 5 agents and 5 borrowers
	results := make([]res, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, a, b, err := s.ReserveAgentAndBorrower(ctx, campaignID, "worker-"+string(rune('0'+idx)), "MockProviderA")
			results[idx] = res{call: c, agent: a, borrower: b, err: err}
		}(i)
	}
	wg.Wait()

	successCount := 0
	exhaustedCount := 0
	reservedAgents := make(map[uuid.UUID]bool)
	reservedBorrowers := make(map[uuid.UUID]bool)

	for _, r := range results {
		if r.err == nil {
			successCount++
			assert.NotNil(t, r.call)
			assert.NotNil(t, r.agent)
			assert.NotNil(t, r.borrower)

			// Invariant: No two workers can ever reserve the same Agent or Borrower (SKIP LOCKED safety)
			assert.False(t, reservedAgents[r.agent.ID], "Agent must not be double-reserved")
			assert.False(t, reservedBorrowers[r.borrower.ID], "Borrower must not be double-reserved")
			reservedAgents[r.agent.ID] = true
			reservedBorrowers[r.borrower.ID] = true
		} else {
			exhaustedCount++
			assert.True(t, r.err == store.ErrNoAvailableAgent || r.err == store.ErrNoPendingBorrower)
		}
	}

	assert.Equal(t, 5, successCount, "Exactly 5 reservations must succeed")
	assert.Equal(t, 5, exhaustedCount, "Remaining 5 workers must receive ErrNoAvailableAgent/Borrower")
}

func TestPostgresStore_ProcessProviderEvent_Idempotency(t *testing.T) {
	s, ctx := setupTestStore(t)
	defer s.Close()

	a, err := s.CreateAgent(ctx, "Agent-1", domain.AgentAvailable)
	require.NoError(t, err)
	_, err = s.CreateBorrower(ctx, "+15551112222")
	require.NoError(t, err)

	campaignID := uuid.New()
	call, _, _, err := s.ReserveAgentAndBorrower(ctx, campaignID, "w1", "MockProviderA")
	require.NoError(t, err)

	// Step 1: Process INITIATED
	err = s.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-init-100",
		EventType:       "INITIATED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-100"},
	})
	require.NoError(t, err)

	updatedCall, err := s.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallInitiated, updatedCall.State)

	// Step 2: Send ANSWERED
	err = s.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-ans-100",
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-100"},
	})
	require.NoError(t, err)

	// Step 3: Send duplicate ANSWERED with IDENTICAL provider_event_id
	err = s.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-ans-100", // Duplicate!
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-100"},
	})
	require.NoError(t, err, "Duplicate event must return nil without error (idempotent)")

	// Verify agent is DIALING
	agent, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AgentDialing, agent.State)
}

func TestPostgresStore_ProcessProviderEvent_OutOfOrder(t *testing.T) {
	s, ctx := setupTestStore(t)
	defer s.Close()

	_, err := s.CreateAgent(ctx, "Agent-1", domain.AgentAvailable)
	require.NoError(t, err)
	_, err = s.CreateBorrower(ctx, "+15553334444")
	require.NoError(t, err)

	call, _, _, err := s.ReserveAgentAndBorrower(ctx, uuid.New(), "w1", "MockProviderB")
	require.NoError(t, err)

	// Simulate Provider B sending COMPLETED first!
	err = s.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-comp-1",
		EventType:       "COMPLETED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-b-1"},
	})
	require.NoError(t, err)

	c1, err := s.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallCompleted, c1.State)

	// Later, delayed ANSWERED event arrives
	err = s.ProcessProviderEvent(ctx, domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: "evt-ans-late",
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{"provider_call_id": "prov-b-1"},
	})
	require.NoError(t, err)

	c2, err := s.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallCompleted, c2.State, "Call state must NOT regress back to ANSWERED")
}

func TestPostgresStore_ReaperRecovery(t *testing.T) {
	s, ctx := setupTestStore(t)
	defer s.Close()

	a, err := s.CreateAgent(ctx, "Agent-Stuck", domain.AgentAvailable)
	require.NoError(t, err)
	b, err := s.CreateBorrower(ctx, "+15559998888")
	require.NoError(t, err)

	call, _, _, err := s.ReserveAgentAndBorrower(ctx, uuid.New(), "crashed-worker", "MockProviderA")
	require.NoError(t, err)

	// Manually age the call updated_at in the database to simulate 15s elapsed
	_, err = s.Pool().Exec(ctx, `
		UPDATE calls
		SET updated_at = NOW() - INTERVAL '15 seconds'
		WHERE id = $1;
	`, call.ID)
	require.NoError(t, err)

	// Scan stuck calls (> 10s reserved)
	stuck, err := s.GetStuckCalls(ctx, 10*time.Second, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, stuck, 1)
	assert.Equal(t, call.ID, stuck[0].ID)

	// Recover the stuck call
	err = s.RecoverStuckCall(ctx, call.ID, "worker_heartbeat_timeout")
	require.NoError(t, err)

	// Verify call state transitioned to FAILED
	recoveredCall, err := s.GetCall(ctx, call.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallFailed, recoveredCall.State)

	// Verify agent was released back to AVAILABLE
	recoveredAgent, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AgentAvailable, recoveredAgent.State)
	assert.Nil(t, recoveredAgent.CurrentCallID)

	// Verify borrower was rescheduled back to PENDING
	recoveredBorrower, err := s.GetBorrower(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.BorrowerPending, recoveredBorrower.State)
	assert.Equal(t, 1, recoveredBorrower.RetryCount)
	assert.NotNil(t, recoveredBorrower.NextRetryAt)
}
