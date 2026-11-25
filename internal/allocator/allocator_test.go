package allocator_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"smartdialer/internal/allocator"
	"smartdialer/internal/domain"
	"smartdialer/internal/provider"
	"smartdialer/internal/safety"
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
		t.Skipf("Skipping integration test: unable to connect to DB: %v", err)
		return nil
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("Skipping integration test: ping failed: %v", err)
		return nil
	}
	return pool
}

func setupAllocatorTest(t *testing.T) (*store.PostgresStore, context.Context) {
	pool := getTestPool(t)
	st := store.NewPostgresStore(pool)
	ctx := context.Background()

	_ = st.Migrate(ctx)
	_, _ = pool.Exec(ctx, "TRUNCATE pacing_audit_logs, call_events, calls, borrowers, agents CASCADE;")

	return st, ctx
}

func TestAllocator_SuccessFlow(t *testing.T) {
	st, ctx := setupAllocatorTest(t)
	defer st.Close()

	// Seed 2 available agents and 2 borrowers
	_, err := st.CreateAgent(ctx, "Agent-1", domain.AgentAvailable)
	require.NoError(t, err)
	_, err = st.CreateAgent(ctx, "Agent-2", domain.AgentAvailable)
	require.NoError(t, err)

	_, err = st.CreateBorrower(ctx, "+15551111111")
	require.NoError(t, err)
	_, err = st.CreateBorrower(ctx, "+15552222222")
	require.NoError(t, err)

	prov := provider.NewMockProviderA(1.0, 50*time.Millisecond)
	// Wire up webhook handler to update store states
	prov.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		return st.ProcessProviderEvent(ctx, event)
	})

	alloc := allocator.NewAllocator(st, prov)
	campaignID := uuid.New()

	ticket := safety.SafetyResult{
		CampaignID:    campaignID,
		ApprovedCalls: 2,
		Decision:      domain.DecisionApprove,
	}

	results, err := alloc.AllocateAndDial(ctx, "worker-1", campaignID, ticket)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.True(t, results[0].Success)
	assert.True(t, results[1].Success)
	assert.NotEmpty(t, results[0].ProviderCallID)

	// Verify both calls were created in the database and moved to INITIATED
	call1, err := st.GetCall(ctx, results[0].CallID)
	require.NoError(t, err)
	assert.Equal(t, domain.CallInitiated, call1.State)
}

func TestAllocator_ProviderFailureRollback(t *testing.T) {
	st, ctx := setupAllocatorTest(t)
	defer st.Close()

	agent, err := st.CreateAgent(ctx, "Agent-Failover", domain.AgentAvailable)
	require.NoError(t, err)
	_, err = st.CreateBorrower(ctx, "+15559990000")
	require.NoError(t, err)

	prov := provider.NewMockProviderB(1.0, 50*time.Millisecond)
	prov.SetOutage(true) // Force provider failure

	alloc := allocator.NewAllocator(st, prov)
	campaignID := uuid.New()

	ticket := safety.SafetyResult{
		CampaignID:    campaignID,
		ApprovedCalls: 1,
		Decision:      domain.DecisionApprove,
	}

	results, err := alloc.AllocateAndDial(ctx, "worker-1", campaignID, ticket)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.False(t, results[0].Success)

	// Verify agent was rolled back to AVAILABLE
	ag, err := st.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AgentAvailable, ag.State)
	assert.Nil(t, ag.CurrentCallID)
}
