-- 001_init.sql: SmartDialer Initial PostgreSQL Schema

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Agents Table: Represents agents logged into the dialer system
CREATE TABLE IF NOT EXISTS agents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('OFFLINE', 'AVAILABLE', 'RESERVED', 'DIALING', 'CONNECTED', 'WRAP_UP', 'PAUSED')),
    current_call_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    version BIGINT NOT NULL DEFAULT 1
);

-- Borrowers Table: Contact records to dial
CREATE TABLE IF NOT EXISTS borrowers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    phone_number TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'RESERVED', 'DIALING', 'COMPLETED', 'FAILED')),
    retry_count INT NOT NULL DEFAULT 0,
    next_retry_at TIMESTAMPTZ
);

-- Calls Table: State of outbound dials
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

-- Call Events Table: Single source of truth for provider webhooks / events (Idempotency)
CREATE TABLE IF NOT EXISTS call_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    call_id UUID NOT NULL REFERENCES calls(id) ON DELETE CASCADE,
    provider_event_id TEXT UNIQUE NOT NULL,
    event_type TEXT NOT NULL,
    event_timestamp TIMESTAMPTZ NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Pacing Audit Logs Table: Auditable record of every Pacing proposal and Safety decision
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

-- Performance Indexes for SKIP LOCKED queries & metrics aggregation
CREATE INDEX IF NOT EXISTS idx_agents_state_updated_at ON agents (state, updated_at ASC);
CREATE INDEX IF NOT EXISTS idx_borrowers_pending_retry ON borrowers (state, next_retry_at, id);
CREATE INDEX IF NOT EXISTS idx_calls_state_updated_at ON calls (state, updated_at ASC);
CREATE INDEX IF NOT EXISTS idx_calls_campaign_state ON calls (campaign_id, state);
CREATE INDEX IF NOT EXISTS idx_call_events_call_id ON call_events (call_id);
CREATE INDEX IF NOT EXISTS idx_pacing_audit_campaign_ts ON pacing_audit_logs (campaign_id, timestamp DESC);
