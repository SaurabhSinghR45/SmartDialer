package worker_test

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
	"smartdialer/internal/pacing"
	"smartdialer/internal/provider"
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

func setupWorkerTest(t *testing.T) (*store.PostgresStore, context.Context) {
	pool := getTestPool(t)
	st := store.NewPostgresStore(pool)
	ctx := context.Background()

	_ = st.Migrate(ctx)
	_, _ = pool.Exec(ctx, "TRUNCATE pacing_audit_logs, call_events, calls, borrowers, agents CASCADE;")

	return st, ctx
}

func TestWorkerPool_ProgressiveExecution(t *testing.T) {
	st, ctx := setupWorkerTest(t)
	defer st.Close()

	// Seed 4 available agents
	for i := 1; i <= 4; i++ {
		_, err := st.CreateAgent(ctx, "Agent-"+string(rune('0'+i)), domain.AgentAvailable)
		require.NoError(t, err)
	}

	// Seed 4 borrowers
	for i := 1; i <= 4; i++ {
		_, err := st.CreateBorrower(ctx, "+1555333000"+string(rune('0'+i)))
		require.NoError(t, err)
	}

	prov := provider.NewMockProviderA(1.0, 50*time.Millisecond)
	prov.RegisterWebhookHandler(func(ctx context.Context, event domain.CallEvent) error {
		return st.ProcessProviderEvent(ctx, event)
	})

	engine := pacing.NewProgressiveEngine()
	ctrl := safety.NewController(st)
	alloc := allocator.NewAllocator(st, prov)

	pool := worker.NewPool(2, st, engine, ctrl, alloc, nil)
	campaignID := uuid.New()

	poolCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pool.Start(poolCtx, campaignID)

	// Wait for workers to dial all 4 borrowers
	assert.Eventually(t, func() bool {
		stats := pool.Stats()
		return stats.TotalPlaced >= 4
	}, 3*time.Second, 100*time.Millisecond)

	pool.Stop()
	stats := pool.Stats()
	assert.True(t, stats.TotalPlaced >= 4)
	assert.Equal(t, int64(0), stats.TotalFailed)
}

func TestReaper_PeriodicScanAndReclaim(t *testing.T) {
	st, ctx := setupWorkerTest(t)
	defer st.Close()

	a, err := st.CreateAgent(ctx, "Agent-Orphan", domain.AgentAvailable)
	require.NoError(t, err)
	b, err := st.CreateBorrower(ctx, "+15558887777")
	require.NoError(t, err)

	call, _, _, err := st.ReserveAgentAndBorrower(ctx, uuid.New(), "crashed-worker", "MockProviderA")
	require.NoError(t, err)

	// Age call by 12 seconds in DB
	_, err = st.Pool().Exec(ctx, "UPDATE calls SET updated_at = NOW() - INTERVAL '12 seconds' WHERE id = $1;", call.ID)
	require.NoError(t, err)

	cfg := worker.ReaperConfig{
		ScanInterval:    100 * time.Millisecond,
		ReservedTimeout: 5 * time.Second,
		AnsweredTimeout: 2 * time.Second,
	}
	reaper := worker.NewReaper(st, cfg, nil)

	reapCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	reclaimed, err := reaper.ReapOnce(reapCtx)
	require.NoError(t, err)
	assert.Equal(t, 1, reclaimed)

	// Verify agent was recovered
	ag, err := st.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.AgentAvailable, ag.State)

	// Verify borrower was rescheduled
	bo, err := st.GetBorrower(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.BorrowerPending, bo.State)
	assert.Equal(t, 1, bo.RetryCount)
}
