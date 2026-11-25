package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/domain"
	"smartdialer/internal/store"
)

func setupShardedTestStore(t *testing.T) (*store.PostgresStore, func()) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://postgres:postgres@localhost:5432/smartdialer?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("Postgres unavailable for sharded test, skipping: %v", err)
	}

	st := store.NewPostgresStore(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	// Clean any previous test data before starting
	_, _ = pool.Exec(ctx, "TRUNCATE TABLE call_events, calls, agents, borrowers, pacing_audit_logs CASCADE;")

	cleanup := func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_, _ = pool.Exec(ctxClean, "TRUNCATE TABLE call_events, calls, agents, borrowers, pacing_audit_logs CASCADE;")
		st.Close()
	}

	return st, cleanup
}

func TestPostgresStore_ShardedReservation(t *testing.T) {
	st, cleanup := setupShardedTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Seed agents across two shards (0 and 1)
	agent0, err := st.CreateAgentWithShard(ctx, "Agent-Shard-0", domain.AgentAvailable, 0)
	require.NoError(t, err)
	agent1, err := st.CreateAgentWithShard(ctx, "Agent-Shard-1", domain.AgentAvailable, 1)
	require.NoError(t, err)

	// Seed borrowers across two shards
	b0, err := st.CreateBorrowerWithCompliance(ctx, "+15550001", 0, "America/New_York")
	require.NoError(t, err)
	b1, err := st.CreateBorrowerWithCompliance(ctx, "+15550002", 1, "America/Los_Angeles")
	require.NoError(t, err)

	campaignID := uuid.New()

	// Reserve on Shard 0 -> must pick Agent-Shard-0 and b0
	call0, resAgent0, resB0, err := st.ReserveAgentAndBorrowerSharded(ctx, campaignID, "worker-0", "mock-a", 0)
	require.NoError(t, err)
	require.Equal(t, agent0.ID, resAgent0.ID)
	require.Equal(t, b0.ID, resB0.ID)
	require.Equal(t, int64(2), call0.LeaseToken, "Lease token should be incremented to 2")
	require.Equal(t, int64(2), resAgent0.LeaseToken)

	// Reserve on Shard 1 -> must pick Agent-Shard-1 and b1
	call1, resAgent1, resB1, err := st.ReserveAgentAndBorrowerSharded(ctx, campaignID, "worker-1", "mock-b", 1)
	require.NoError(t, err)
	require.Equal(t, agent1.ID, resAgent1.ID)
	require.Equal(t, b1.ID, resB1.ID)
	require.Equal(t, int64(2), call1.LeaseToken)

	// Now Shard 0 is empty. If we ask Shard 0 again with fallback, it should return ErrNoAvailableAgent
	_, _, _, err = st.ReserveAgentAndBorrowerSharded(ctx, campaignID, "worker-0", "mock-a", 0)
	require.ErrorIs(t, err, store.ErrNoAvailableAgent)
}

func TestPostgresStore_FencingToken_PreventsSplitBrain(t *testing.T) {
	st, cleanup := setupShardedTestStore(t)
	defer cleanup()
	ctx := context.Background()

	agent, err := st.CreateAgentWithShard(ctx, "Zombie-Target-Agent", domain.AgentAvailable, 0)
	require.NoError(t, err)
	_, err = st.CreateBorrowerWithCompliance(ctx, "+15559999", 0, "America/Chicago")
	require.NoError(t, err)

	campaignID := uuid.New()
	call, _, _, err := st.ReserveAgentAndBorrowerSharded(ctx, campaignID, "worker-original", "mock-a", 0)
	require.NoError(t, err)
	originalLease := call.LeaseToken

	// Simulate Reaper reclaiming the stuck call and releasing the agent
	err = st.RecoverStuckCall(ctx, call.ID, "simulated worker stall")
	require.NoError(t, err)

	// Agent's lease token was bumped during reap to invalidate old worker
	reapedAgent, err := st.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Greater(t, reapedAgent.LeaseToken, originalLease)

	// Zombie worker wakes up late and tries to push ANSWERED event using originalLease token
	zombieEvent := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: fmt.Sprintf("zombie-evt-%s", uuid.New().String()),
		EventType:       "ANSWERED",
		EventTimestamp:  time.Now().UTC(),
		Payload: map[string]any{
			"provider_call_id": "prov-zombie-123",
		},
	}

	// Because expectedLeaseToken does not match the recovered call / agent state, it MUST fail
	// First let's test mismatched lease token directly
	err = st.ProcessProviderEventWithFencing(ctx, zombieEvent, originalLease)
	// Because call state was marked FAILED by reaper, transition from FAILED to ANSWERED is also rejected by state machine,
	// or token mismatch fails. If we test with a stale token against an existing call:
	require.Error(t, err)
}

func TestPostgresStore_FencingToken_StaleTokenRejected(t *testing.T) {
	st, cleanup := setupShardedTestStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := st.CreateAgentWithShard(ctx, "Agent-Active", domain.AgentAvailable, 0)
	require.NoError(t, err)
	_, err = st.CreateBorrowerWithCompliance(ctx, "+15558888", 0, "America/New_York")
	require.NoError(t, err)

	campaignID := uuid.New()
	call, _, _, err := st.ReserveAgentAndBorrowerSharded(ctx, campaignID, "worker-active", "mock-a", 0)
	require.NoError(t, err)
	correctLease := call.LeaseToken

	// Worker provides wrong / stale token
	staleLease := correctLease - 1
	staleEvent := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: fmt.Sprintf("stale-evt-%s", uuid.New().String()),
		EventType:       "INITIATED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{},
	}

	err = st.ProcessProviderEventWithFencing(ctx, staleEvent, staleLease)
	require.ErrorIs(t, err, store.ErrStaleFencingToken, "Old fencing token must be rejected")

	// Worker provides valid token -> must succeed
	validEvent := domain.CallEvent{
		ID:              uuid.New(),
		CallID:          call.ID,
		ProviderEventID: fmt.Sprintf("valid-evt-%s", uuid.New().String()),
		EventType:       "INITIATED",
		EventTimestamp:  time.Now().UTC(),
		Payload:         map[string]any{},
	}
	err = st.ProcessProviderEventWithFencing(ctx, validEvent, correctLease)
	require.NoError(t, err, "Matching fencing token must succeed")
}
