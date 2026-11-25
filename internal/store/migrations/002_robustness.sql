-- 002_robustness.sql: Sharded Concurrency, Fencing Tokens & Compliance Columns

-- 1. Sharded Concurrency & Fencing Tokens on Agents
ALTER TABLE agents ADD COLUMN IF NOT EXISTS shard_id INT NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS lease_token BIGINT NOT NULL DEFAULT 1;

-- 2. Sharding, TCPA Timezones & FDCPA/Reg F Frequency Caps on Borrowers
ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS shard_id INT NOT NULL DEFAULT 0;
ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS timezone TEXT NOT NULL DEFAULT 'America/New_York';
ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS last_called_at TIMESTAMPTZ;
ALTER TABLE borrowers ADD COLUMN IF NOT EXISTS call_attempts_last_7_days INT NOT NULL DEFAULT 0;

-- 3. Telemetry, Answering Machine Detection (AMD) & Fencing on Calls
ALTER TABLE calls ADD COLUMN IF NOT EXISTS lease_token BIGINT NOT NULL DEFAULT 1;
ALTER TABLE calls ADD COLUMN IF NOT EXISTS is_machine BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE calls ADD COLUMN IF NOT EXISTS carrier_latency_ms INT NOT NULL DEFAULT 0;

-- 4. High-Performance Sharded Indexes (Eliminates B-Tree Head Contention)
CREATE INDEX IF NOT EXISTS idx_agents_sharded_available ON agents (shard_id, state, updated_at ASC);
CREATE INDEX IF NOT EXISTS idx_borrowers_sharded_pending ON borrowers (shard_id, state, next_retry_at, id);
CREATE INDEX IF NOT EXISTS idx_borrowers_timezone ON borrowers (timezone, state);
