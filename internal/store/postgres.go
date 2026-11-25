package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"smartdialer/internal/domain"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

func (s *PostgresStore) Pool() *pgxpool.Pool {
	return s.pool
}

// Migrate executes the initialization DDL.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	schema := `
	CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

	CREATE TABLE IF NOT EXISTS agents (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('OFFLINE', 'AVAILABLE', 'RESERVED', 'DIALING', 'CONNECTED', 'WRAP_UP', 'PAUSED')),
		current_call_id UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		version BIGINT NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS borrowers (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		phone_number TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('PENDING', 'RESERVED', 'DIALING', 'COMPLETED', 'FAILED')),
		retry_count INT NOT NULL DEFAULT 0,
		next_retry_at TIMESTAMPTZ
	);

	CREATE TABLE IF NOT EXISTS calls (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		campaign_id UUID NOT NULL,
		agent_id UUID REFERENCES agents(id) ON DELETE SET NULL,
		borrower_id UUID NOT NULL REFERENCES borrowers(id) ON DELETE CASCADE,
		provider_name TEXT NOT NULL,
		provider_call_id TEXT,
		state TEXT NOT NULL CHECK (state IN ('QUEUED', 'RESERVED', 'INITIATED', 'RINGING', 'ANSWERED', 'CONNECTED', 'COMPLETED', 'FAILED', 'CANCELLED')),
		worker_id TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS call_events (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		call_id UUID NOT NULL REFERENCES calls(id) ON DELETE CASCADE,
		provider_event_id TEXT UNIQUE NOT NULL,
		event_type TEXT NOT NULL,
		event_timestamp TIMESTAMPTZ NOT NULL,
		payload JSONB NOT NULL DEFAULT '{}'::jsonb,
		processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS pacing_audit_logs (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		campaign_id UUID NOT NULL,
		timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		mode TEXT NOT NULL,
		proposed_calls INT NOT NULL,
		approved_calls INT NOT NULL,
		decision TEXT NOT NULL CHECK (decision IN ('APPROVE', 'REDUCE', 'REJECT', 'FALLBACK_TO_PROGRESSIVE')),
		reason TEXT NOT NULL,
		metrics_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb
	);

	CREATE INDEX IF NOT EXISTS idx_agents_state_updated_at ON agents (state, updated_at ASC);
	CREATE INDEX IF NOT EXISTS idx_borrowers_pending_retry ON borrowers (state, next_retry_at, id);
	CREATE INDEX IF NOT EXISTS idx_calls_state_updated_at ON calls (state, updated_at ASC);
	CREATE INDEX IF NOT EXISTS idx_calls_campaign_state ON calls (campaign_id, state);
	CREATE INDEX IF NOT EXISTS idx_call_events_call_id ON call_events (call_id);
	CREATE INDEX IF NOT EXISTS idx_pacing_audit_campaign_ts ON pacing_audit_logs (campaign_id, timestamp DESC);

	-- Robustness Upgrades: Sharding, Fencing Tokens & Compliance
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS shard_id INT NOT NULL DEFAULT 0;
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS lease_token BIGINT NOT NULL DEFAULT 1;

	ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS shard_id INT NOT NULL DEFAULT 0;
	ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'America/New_York';
	ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS last_called_at TIMESTAMPTZ;
	ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS call_attempts_last_7_days INT NOT NULL DEFAULT 0;

	ALTER TABLE calls ADD COLUMN IF NOT EXISTS lease_token BIGINT NOT NULL DEFAULT 1;
	ALTER TABLE calls ADD COLUMN IF NOT EXISTS is_machine BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE calls ADD COLUMN IF NOT EXISTS carrier_latency_ms INT NOT NULL DEFAULT 0;

	CREATE INDEX IF NOT EXISTS idx_agents_sharded_available ON agents (shard_id, state, updated_at ASC);
	CREATE INDEX IF NOT EXISTS idx_borrowers_sharded_pending ON borrowers (shard_id, state, next_retry_at, id);
	CREATE INDEX IF NOT EXISTS idx_borrowers_timezone ON borrowers (timezone, state);
	`
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// ReserveAgentAndBorrower performs atomic multi-row reservation using SKIP LOCKED.
func (s *PostgresStore) ReserveAgentAndBorrower(
	ctx context.Context,
	campaignID uuid.UUID,
	workerID, providerName string,
) (*domain.Call, *domain.Agent, *domain.Borrower, error) {
	return s.ReserveAgentAndBorrowerSharded(ctx, campaignID, workerID, providerName, -1)
}

// ReserveAgentAndBorrowerSharded performs atomic multi-row reservation within a partition shard
// to eliminate B-tree index leaf lock contention, assigning monotonic lease fencing tokens.
func (s *PostgresStore) ReserveAgentAndBorrowerSharded(
	ctx context.Context,
	campaignID uuid.UUID,
	workerID, providerName string,
	shardID int,
) (*domain.Call, *domain.Agent, *domain.Borrower, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Lock 1 Available Agent (sharded query with global fallback)
	var agent domain.Agent
	var agentQuery string
	var agentArgs []any

	if shardID >= 0 {
		agentQuery = `
			SELECT id, name, state, current_call_id, updated_at, version, shard_id, lease_token
			FROM agents
			WHERE shard_id = $1 AND state = 'AVAILABLE'
			ORDER BY updated_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED;
		`
		agentArgs = []any{shardID}
	} else {
		agentQuery = `
			SELECT id, name, state, current_call_id, updated_at, version, shard_id, lease_token
			FROM agents
			WHERE state = 'AVAILABLE'
			ORDER BY updated_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED;
		`
		agentArgs = []any{}
	}

	err = tx.QueryRow(ctx, agentQuery, agentArgs...).Scan(
		&agent.ID, &agent.Name, &agent.State, &agent.CurrentCallID, &agent.UpdatedAt, &agent.Version, &agent.ShardID, &agent.LeaseToken,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && shardID >= 0 {
			// Fallback: search across all shards to prevent idle starvation
			err = tx.QueryRow(ctx, `
				SELECT id, name, state, current_call_id, updated_at, version, shard_id, lease_token
				FROM agents
				WHERE state = 'AVAILABLE'
				ORDER BY updated_at ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED;
			`).Scan(&agent.ID, &agent.Name, &agent.State, &agent.CurrentCallID, &agent.UpdatedAt, &agent.Version, &agent.ShardID, &agent.LeaseToken)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, ErrNoAvailableAgent
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to select available agent: %w", err)
		}
	}

	// 2. Lock 1 Pending Borrower (sharded query with global fallback)
	var borrower domain.Borrower
	var borrowerQuery string
	var borrowerArgs []any

	if shardID >= 0 {
		borrowerQuery = `
			SELECT id, phone_number, state, retry_count, next_retry_at, shard_id, timezone, last_called_at, call_attempts_last_7_days
			FROM borrowers
			WHERE shard_id = $1 AND state = 'PENDING'
			  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
			ORDER BY id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED;
		`
		borrowerArgs = []any{shardID}
	} else {
		borrowerQuery = `
			SELECT id, phone_number, state, retry_count, next_retry_at, shard_id, timezone, last_called_at, call_attempts_last_7_days
			FROM borrowers
			WHERE state = 'PENDING'
			  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
			ORDER BY id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED;
		`
		borrowerArgs = []any{}
	}

	err = tx.QueryRow(ctx, borrowerQuery, borrowerArgs...).Scan(
		&borrower.ID, &borrower.PhoneNumber, &borrower.State, &borrower.RetryCount, &borrower.NextRetryAt,
		&borrower.ShardID, &borrower.Timezone, &borrower.LastCalledAt, &borrower.CallAttemptsLast7Days,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && shardID >= 0 {
			// Fallback: search across all shards
			err = tx.QueryRow(ctx, `
				SELECT id, phone_number, state, retry_count, next_retry_at, shard_id, timezone, last_called_at, call_attempts_last_7_days
				FROM borrowers
				WHERE state = 'PENDING'
				  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
				ORDER BY id ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED;
			`).Scan(
				&borrower.ID, &borrower.PhoneNumber, &borrower.State, &borrower.RetryCount, &borrower.NextRetryAt,
				&borrower.ShardID, &borrower.Timezone, &borrower.LastCalledAt, &borrower.CallAttemptsLast7Days,
			)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, ErrNoPendingBorrower
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to select pending borrower: %w", err)
		}
	}

	callID := uuid.New()
	newLeaseToken := agent.LeaseToken + 1

	call := domain.Call{
		ID:               callID,
		CampaignID:       campaignID,
		AgentID:          &agent.ID,
		BorrowerID:       borrower.ID,
		ProviderName:     providerName,
		State:            domain.CallReserved,
		WorkerID:         &workerID,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		LeaseToken:       newLeaseToken,
		IsMachine:        false,
		CarrierLatencyMs: 0,
	}

	// 3. Insert Call with lease_token
	_, err = tx.Exec(ctx, `
		INSERT INTO calls (id, campaign_id, agent_id, borrower_id, provider_name, state, worker_id, created_at, updated_at, lease_token, is_machine, carrier_latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
	`, call.ID, call.CampaignID, call.AgentID, call.BorrowerID, call.ProviderName, call.State, call.WorkerID, call.CreatedAt, call.UpdatedAt, call.LeaseToken, call.IsMachine, call.CarrierLatencyMs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert call: %w", err)
	}

	// 4. Update Agent to RESERVED and increment lease_token
	err = tx.QueryRow(ctx, `
		UPDATE agents
		SET state = 'RESERVED', current_call_id = $1, updated_at = NOW(), version = version + 1, lease_token = lease_token + 1
		WHERE id = $2
		RETURNING lease_token;
	`, call.ID, agent.ID).Scan(&agent.LeaseToken)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to update agent: %w", err)
	}
	agent.State = domain.AgentReserved
	agent.CurrentCallID = &call.ID

	// 5. Update Borrower to RESERVED & increment call count
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE borrowers
		SET state = 'RESERVED', last_called_at = $1, call_attempts_last_7_days = call_attempts_last_7_days + 1
		WHERE id = $2;
	`, now, borrower.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to update borrower: %w", err)
	}
	borrower.State = domain.BorrowerReserved
	borrower.LastCalledAt = &now
	borrower.CallAttemptsLast7Days++

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to commit reservation: %w", err)
	}

	return &call, &agent, &borrower, nil
}

// ProcessProviderEvent idempotently logs events and reconciles call/agent/borrower states.
func (s *PostgresStore) ProcessProviderEvent(ctx context.Context, event domain.CallEvent) error {
	return s.ProcessProviderEventWithFencing(ctx, event, 0)
}

// ProcessProviderEventWithFencing idempotently logs events, checks monotonic fencing tokens to prevent
// split-brain overwrites from delayed workers, and reconciles state.
func (s *PostgresStore) ProcessProviderEventWithFencing(ctx context.Context, event domain.CallEvent, expectedLeaseToken int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	payloadJSON, err := json.Marshal(event.Payload)
	if err != nil {
		payloadJSON = []byte("{}")
	}

	// 1. Insert into call_events ON CONFLICT DO NOTHING (Idempotency)
	var insertedID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO call_events (id, call_id, provider_event_id, event_type, event_timestamp, payload, processed_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (provider_event_id) DO NOTHING
		RETURNING id;
	`, event.ID, event.CallID, event.ProviderEventID, event.EventType, event.EventTimestamp, payloadJSON).Scan(&insertedID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Duplicate event detected! Return nil immediately for strict idempotency
			return nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return nil
		}
		return fmt.Errorf("failed to insert call event: %w", err)
	}

	// 2. Lock Call Row for Update
	var call domain.Call
	row := tx.QueryRow(ctx, `
		SELECT id, campaign_id, agent_id, borrower_id, provider_name, provider_call_id, state, worker_id, created_at, updated_at, lease_token, is_machine, carrier_latency_ms
		FROM calls
		WHERE id = $1
		FOR UPDATE;
	`, event.CallID)

	if err := row.Scan(
		&call.ID, &call.CampaignID, &call.AgentID, &call.BorrowerID, &call.ProviderName, &call.ProviderCallID,
		&call.State, &call.WorkerID, &call.CreatedAt, &call.UpdatedAt, &call.LeaseToken, &call.IsMachine, &call.CarrierLatencyMs,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCallNotFound
		}
		return fmt.Errorf("failed to select call for update: %w", err)
	}

	// 2b. Monotonic Fencing Token check: prevents split-brain state modification by zombie workers
	if expectedLeaseToken > 0 && call.LeaseToken != expectedLeaseToken {
		return ErrStaleFencingToken
	}

	// 3. Map event type to target CallState
	targetState := mapEventTypeToCallState(event.EventType)
	nextState, resolution, err := domain.ResolveCallTransition(call.State, targetState)
	if err != nil {
		// Log the transition error, but keep the event recorded
		_ = tx.Commit(ctx)
		return err
	}

	// If stale or duplicate, safely commit the event record without mutating state
	if resolution == domain.TransitionIgnoredDuplicate || resolution == domain.TransitionIgnoredStale {
		return tx.Commit(ctx)
	}

	// 4. Mutate Call State
	var provCallID *string
	if val, ok := event.Payload["provider_call_id"].(string); ok && val != "" {
		provCallID = &val
	}
	isMachine := call.IsMachine
	if val, ok := event.Payload["is_machine"].(bool); ok {
		isMachine = val
	}
	carrierLatency := call.CarrierLatencyMs
	if val, ok := event.Payload["carrier_latency_ms"].(float64); ok {
		carrierLatency = int(val)
	}

	var tag pgconn.CommandTag
	if expectedLeaseToken > 0 {
		tag, err = tx.Exec(ctx, `
			UPDATE calls
			SET state = $1,
			    provider_call_id = COALESCE($2, provider_call_id),
			    is_machine = $3,
			    carrier_latency_ms = $4,
			    updated_at = NOW()
			WHERE id = $5 AND lease_token = $6;
		`, nextState, provCallID, isMachine, carrierLatency, call.ID, expectedLeaseToken)
	} else {
		tag, err = tx.Exec(ctx, `
			UPDATE calls
			SET state = $1,
			    provider_call_id = COALESCE($2, provider_call_id),
			    is_machine = $3,
			    carrier_latency_ms = $4,
			    updated_at = NOW()
			WHERE id = $5;
		`, nextState, provCallID, isMachine, carrierLatency, call.ID)
	}
	if err != nil {
		return fmt.Errorf("failed to update call state: %w", err)
	}
	if expectedLeaseToken > 0 && tag.RowsAffected() == 0 {
		return ErrStaleFencingToken
	}

	// 5. Cascade state changes to Agent and Borrower
	switch nextState {
	case domain.CallInitiated, domain.CallRinging:
		if call.AgentID != nil {
			_, _ = tx.Exec(ctx, `
				UPDATE agents
				SET state = 'DIALING', updated_at = NOW(), version = version + 1
				WHERE id = $1 AND state = 'RESERVED';
			`, *call.AgentID)
		}
		_, _ = tx.Exec(ctx, `UPDATE borrowers SET state = 'DIALING' WHERE id = $1;`, call.BorrowerID)

	case domain.CallConnected:
		if call.AgentID != nil {
			_, _ = tx.Exec(ctx, `
				UPDATE agents
				SET state = 'CONNECTED', updated_at = NOW(), version = version + 1
				WHERE id = $1;
			`, *call.AgentID)
		}

	case domain.CallCompleted:
		if call.AgentID != nil {
			_, _ = tx.Exec(ctx, `
				UPDATE agents
				SET state = 'WRAP_UP', updated_at = NOW(), version = version + 1
				WHERE id = $1;
			`, *call.AgentID)
		}
		_, _ = tx.Exec(ctx, `UPDATE borrowers SET state = 'COMPLETED' WHERE id = $1;`, call.BorrowerID)

	case domain.CallFailed, domain.CallCancelled:
		if call.AgentID != nil {
			// Release agent back to AVAILABLE
			_, _ = tx.Exec(ctx, `
				UPDATE agents
				SET state = 'AVAILABLE', current_call_id = NULL, updated_at = NOW(), version = version + 1
				WHERE id = $1;
			`, *call.AgentID)
		}
		// Reschedule borrower with backoff or mark failed if retry limit reached
		_, _ = tx.Exec(ctx, `
			UPDATE borrowers
			SET retry_count = retry_count + 1,
			    state = CASE WHEN retry_count + 1 >= 3 THEN 'FAILED' ELSE 'PENDING' END,
			    next_retry_at = CASE WHEN retry_count + 1 >= 3 THEN NULL ELSE NOW() + (INTERVAL '10 seconds' * (retry_count + 1)) END
			WHERE id = $1;
		`, call.BorrowerID)
	}

	return tx.Commit(ctx)
}

func mapEventTypeToCallState(eventType string) domain.CallState {
	switch eventType {
	case "INITIATED":
		return domain.CallInitiated
	case "RINGING":
		return domain.CallRinging
	case "ANSWERED":
		return domain.CallAnswered
	case "CONNECTED":
		return domain.CallConnected
	case "COMPLETED":
		return domain.CallCompleted
	case "FAILED", "BUSY", "NO_ANSWER", "REJECTED":
		return domain.CallFailed
	case "CANCELLED":
		return domain.CallCancelled
	default:
		return domain.CallState(eventType)
	}
}

// RecordPacingAudit inserts a Safety Controller decision log.
func (s *PostgresStore) RecordPacingAudit(ctx context.Context, audit domain.PacingAuditLog) error {
	payloadJSON, err := json.Marshal(audit.MetricsSnapshot)
	if err != nil {
		payloadJSON = []byte("{}")
	}

	if audit.ID == uuid.Nil {
		audit.ID = uuid.New()
	}
	if audit.Timestamp.IsZero() {
		audit.Timestamp = time.Now().UTC()
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO pacing_audit_logs (id, campaign_id, timestamp, mode, proposed_calls, approved_calls, decision, reason, metrics_snapshot)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
	`, audit.ID, audit.CampaignID, audit.Timestamp, audit.Mode, audit.ProposedCalls, audit.ApprovedCalls, audit.Decision, audit.Reason, payloadJSON)
	return err
}

// GetPacingMetrics aggregates statistical telemetry for the pacing & safety calculations.
func (s *PostgresStore) GetPacingMetrics(ctx context.Context, campaignID uuid.UUID) (*PacingMetrics, error) {
	metrics := &PacingMetrics{
		RollingAnswerRate:   0.30, // Default prior when insufficient history
		AvgSetupDurationSec: 5.0,  // Standard telecom setup time
		AvgTalkDurationSec:  90.0, // Standard collections talk time
	}

	// 1. Agent Counts
	err := s.pool.QueryRow(ctx, `
		SELECT 
			COALESCE(COUNT(*), 0) AS total,
			COALESCE(COUNT(*) FILTER (WHERE state = 'AVAILABLE'), 0) AS available
		FROM agents;
	`).Scan(&metrics.TotalAgents, &metrics.AvailableAgents)
	if err != nil {
		return nil, fmt.Errorf("failed to query agent metrics: %w", err)
	}

	// 2. Call State Counts for Campaign
	err = s.pool.QueryRow(ctx, `
		SELECT 
			COALESCE(COUNT(*) FILTER (WHERE state = 'CONNECTED'), 0) AS connected,
			COALESCE(COUNT(*) FILTER (WHERE state IN ('INITIATED', 'RINGING')), 0) AS ringing,
			COALESCE(COUNT(*) FILTER (WHERE state IN ('RESERVED', 'INITIATED', 'RINGING')), 0) AS in_flight
		FROM calls
		WHERE campaign_id = $1;
	`, campaignID).Scan(&metrics.ConnectedCalls, &metrics.RingingCalls, &metrics.InFlightCalls)
	if err != nil {
		return nil, fmt.Errorf("failed to query call state counts: %w", err)
	}

	// 3. Rolling Telemetry (Recent 15 minutes)
	var totalRecent, answeredRecent, abandonedRecent, errorRecent int
	err = s.pool.QueryRow(ctx, `
		SELECT 
			COALESCE(COUNT(*), 0) AS total_recent,
			COALESCE(COUNT(*) FILTER (WHERE state IN ('ANSWERED', 'CONNECTED', 'COMPLETED')), 0) AS answered_recent,
			COALESCE(COUNT(*) FILTER (WHERE state = 'FAILED' AND updated_at - created_at > INTERVAL '5 seconds'), 0) AS abandoned_recent,
			COALESCE(COUNT(*) FILTER (WHERE state = 'FAILED' AND updated_at - created_at <= INTERVAL '5 seconds'), 0) AS error_recent
		FROM calls
		WHERE campaign_id = $1 AND created_at >= NOW() - INTERVAL '15 minutes';
	`, campaignID).Scan(&totalRecent, &answeredRecent, &abandonedRecent, &errorRecent)

	if err == nil && totalRecent >= 5 {
		metrics.RollingAnswerRate = float64(answeredRecent) / float64(totalRecent)
		if answeredRecent > 0 {
			metrics.RollingAbandonmentRate = float64(abandonedRecent) / float64(answeredRecent)
		}
		metrics.ProviderErrorRate = float64(errorRecent) / float64(totalRecent)
	}

	// 4. Sudden Agent Drop Detection (Agents dropped in last 5 seconds)
	var droppedLast5s int
	_ = s.pool.QueryRow(ctx, `
		SELECT COALESCE(COUNT(*), 0)
		FROM agents
		WHERE state IN ('PAUSED', 'OFFLINE')
		  AND updated_at >= NOW() - INTERVAL '5 seconds';
	`).Scan(&droppedLast5s)

	if metrics.TotalAgents > 0 {
		metrics.AgentDropRateLast5s = float64(droppedLast5s) / float64(metrics.TotalAgents)
	}

	return metrics, nil
}

// CreateAgent inserts a new agent record with default shard.
func (s *PostgresStore) CreateAgent(ctx context.Context, name string, state domain.AgentState) (*domain.Agent, error) {
	return s.CreateAgentWithShard(ctx, name, state, 0)
}

// CreateAgentWithShard inserts a new agent record assigned to a specific concurrency shard.
func (s *PostgresStore) CreateAgentWithShard(ctx context.Context, name string, state domain.AgentState, shardID int) (*domain.Agent, error) {
	agent := &domain.Agent{
		ID:         uuid.New(),
		Name:       name,
		State:      state,
		UpdatedAt:  time.Now().UTC(),
		Version:    1,
		ShardID:    shardID,
		LeaseToken: 1,
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO agents (id, name, state, updated_at, version, shard_id, lease_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7);
	`, agent.ID, agent.Name, agent.State, agent.UpdatedAt, agent.Version, agent.ShardID, agent.LeaseToken)
	if err != nil {
		return nil, err
	}
	return agent, nil
}

// SetAgentState updates an agent's state directly.
func (s *PostgresStore) SetAgentState(ctx context.Context, agentID uuid.UUID, state domain.AgentState) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE agents
		SET state = $1, updated_at = NOW(), version = version + 1
		WHERE id = $2;
	`, state, agentID)
	return err
}

// CreateBorrower inserts a new borrower record with default compliance values.
func (s *PostgresStore) CreateBorrower(ctx context.Context, phoneNumber string) (*domain.Borrower, error) {
	return s.CreateBorrowerWithCompliance(ctx, phoneNumber, 0, "America/New_York")
}

// CreateBorrowerWithCompliance inserts a new borrower record with timezone and shard assignments.
func (s *PostgresStore) CreateBorrowerWithCompliance(ctx context.Context, phoneNumber string, shardID int, timezone string) (*domain.Borrower, error) {
	if timezone == "" {
		timezone = "America/New_York"
	}
	b := &domain.Borrower{
		ID:                    uuid.New(),
		PhoneNumber:           phoneNumber,
		State:                 domain.BorrowerPending,
		RetryCount:            0,
		ShardID:               shardID,
		Timezone:              timezone,
		CallAttemptsLast7Days: 0,
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO borrowers (id, phone_number, state, retry_count, shard_id, timezone, call_attempts_last_7_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7);
	`, b.ID, b.PhoneNumber, b.State, b.RetryCount, b.ShardID, b.Timezone, b.CallAttemptsLast7Days)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// GetAgent fetches agent by ID.
func (s *PostgresStore) GetAgent(ctx context.Context, id uuid.UUID) (*domain.Agent, error) {
	var a domain.Agent
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, state, current_call_id, updated_at, version, shard_id, lease_token
		FROM agents
		WHERE id = $1;
	`, id).Scan(&a.ID, &a.Name, &a.State, &a.CurrentCallID, &a.UpdatedAt, &a.Version, &a.ShardID, &a.LeaseToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		return nil, err
	}
	return &a, nil
}

// GetCall fetches call by ID.
func (s *PostgresStore) GetCall(ctx context.Context, id uuid.UUID) (*domain.Call, error) {
	var c domain.Call
	err := s.pool.QueryRow(ctx, `
		SELECT id, campaign_id, agent_id, borrower_id, provider_name, provider_call_id, state, worker_id, created_at, updated_at, lease_token, is_machine, carrier_latency_ms
		FROM calls
		WHERE id = $1;
	`, id).Scan(
		&c.ID, &c.CampaignID, &c.AgentID, &c.BorrowerID, &c.ProviderName, &c.ProviderCallID,
		&c.State, &c.WorkerID, &c.CreatedAt, &c.UpdatedAt, &c.LeaseToken, &c.IsMachine, &c.CarrierLatencyMs,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCallNotFound
		}
		return nil, err
	}
	return &c, nil
}

// GetBorrower fetches borrower by ID.
func (s *PostgresStore) GetBorrower(ctx context.Context, id uuid.UUID) (*domain.Borrower, error) {
	var b domain.Borrower
	err := s.pool.QueryRow(ctx, `
		SELECT id, phone_number, state, retry_count, next_retry_at, shard_id, timezone, last_called_at, call_attempts_last_7_days
		FROM borrowers
		WHERE id = $1;
	`, id).Scan(
		&b.ID, &b.PhoneNumber, &b.State, &b.RetryCount, &b.NextRetryAt,
		&b.ShardID, &b.Timezone, &b.LastCalledAt, &b.CallAttemptsLast7Days,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBorrowerNotFound
		}
		return nil, err
	}
	return &b, nil
}

// GetStuckCalls finds orphaned calls exceeding safety durations.
func (s *PostgresStore) GetStuckCalls(ctx context.Context, reservedTimeout, answeredTimeout time.Duration) ([]*domain.Call, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, campaign_id, agent_id, borrower_id, provider_name, provider_call_id, state, worker_id, created_at, updated_at, lease_token, is_machine, carrier_latency_ms
		FROM calls
		WHERE (state IN ('RESERVED', 'INITIATED') AND updated_at <= NOW() - $1::interval)
		   OR (state = 'ANSWERED' AND updated_at <= NOW() - $2::interval)
		ORDER BY updated_at ASC;
	`, fmt.Sprintf("%f seconds", reservedTimeout.Seconds()), fmt.Sprintf("%f seconds", answeredTimeout.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("failed to query stuck calls: %w", err)
	}
	defer rows.Close()

	var stuck []*domain.Call
	for rows.Next() {
		var c domain.Call
		if err := rows.Scan(
			&c.ID, &c.CampaignID, &c.AgentID, &c.BorrowerID, &c.ProviderName, &c.ProviderCallID,
			&c.State, &c.WorkerID, &c.CreatedAt, &c.UpdatedAt, &c.LeaseToken, &c.IsMachine, &c.CarrierLatencyMs,
		); err != nil {
			return nil, err
		}
		stuck = append(stuck, &c)
	}
	return stuck, nil
}

// RecoverStuckCall safely transitions an orphaned call to FAILED and reclaims the agent, incrementing lease_token.
func (s *PostgresStore) RecoverStuckCall(ctx context.Context, callID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var call domain.Call
	err = tx.QueryRow(ctx, `
		SELECT id, campaign_id, agent_id, borrower_id, state
		FROM calls
		WHERE id = $1
		FOR UPDATE;
	`, callID).Scan(&call.ID, &call.CampaignID, &call.AgentID, &call.BorrowerID, &call.State)
	if err != nil {
		return err
	}

	if call.State.IsTerminal() {
		return tx.Commit(ctx)
	}

	// 1. Mark call FAILED and increment lease_token to invalidate zombie workers
	_, err = tx.Exec(ctx, `
		UPDATE calls
		SET state = 'FAILED', updated_at = NOW(), lease_token = lease_token + 1
		WHERE id = $1;
	`, call.ID)
	if err != nil {
		return err
	}

	// 2. Release Agent and increment lease_token to invalidate zombie workers
	if call.AgentID != nil {
		_, _ = tx.Exec(ctx, `
			UPDATE agents
			SET state = 'AVAILABLE', current_call_id = NULL, updated_at = NOW(), version = version + 1, lease_token = lease_token + 1
			WHERE id = $1;
		`, *call.AgentID)
	}

	// 3. Reset Borrower for retry
	_, _ = tx.Exec(ctx, `
		UPDATE borrowers
		SET retry_count = retry_count + 1,
		    state = 'PENDING',
		    next_retry_at = NOW() + INTERVAL '10 seconds'
		WHERE id = $1;
	`, call.BorrowerID)

	// 4. Record Call Event
	eventID := fmt.Sprintf("reaper-%s-%d", call.ID.String(), time.Now().UnixNano())
	_, _ = tx.Exec(ctx, `
		INSERT INTO call_events (id, call_id, provider_event_id, event_type, event_timestamp, payload, processed_at)
		VALUES ($1, $2, $3, 'FAILED', NOW(), $4, NOW())
		ON CONFLICT (provider_event_id) DO NOTHING;
	`, uuid.New(), call.ID, eventID, fmt.Sprintf(`{"reason": %q}`, reason))

	return tx.Commit(ctx)
}
