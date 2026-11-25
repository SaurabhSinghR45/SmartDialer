# SmartDialer: Production-Grade Outbound Dialing Engine

SmartDialer is a zero-bloat, high-concurrency outbound debt collections dialing prototype built in **Go (1.22+)**, **PostgreSQL 16**, and a **React + TypeScript + Tailwind CSS (Vite)** operations dashboard. It solves outbound dialing by uniting high-throughput **Progressive** and **Predictive** pacing with a strict, non-bypassable **Safety Controller**.

---

## Architecture Highlights

1. **Strict Architectural Pipeline:**
   ```
   Campaign -> Pacing Engine -> Safety Controller -> Call Allocator -> Telecom Provider
   ```
   Enforced in code via Go interfaces. The Pacing Engine can never directly dial or allocate resources.
2. **Dual Dialing Modes:**
   - **Progressive Dialing:** Dial only when agents are available ($N = \max(0, A - C_{\text{inflight}})$). Zero regulatory abandonment risk.
   - **Predictive Dialing:** Little's Law statistical engine with Erlang-inspired completion buffers that over-dials based on rolling answer rates and average talk times to maximize agent utilization.
3. **Non-Bypassable Safety Controller:**
   - **Sudden Drop Guard:** Rejects predictive dialing bursts if available agent count drops by $>20\%$ within 5 seconds.
   - **Abandonment Guard:** Forces instant fallback to Progressive mode if rolling abandonment exceeds 2.0% or provider error rate exceeds 25%.
   - **Capacity Cap:** Restricts ringing calls so anticipated answers cannot exceed $1.15 \times$ available agent capacity.
4. **PostgreSQL 16 as Single Source of Truth:**
   - Zero split-brain issues (no Kafka, Redis, or microservices bloat).
   - Atomic multi-resource locking using `SELECT ... FOR UPDATE SKIP LOCKED`.
   - Event idempotency via unique provider event IDs in `call_events`.
   - Monotonic state rankings (`CallStateRank`) ensuring out-of-order telecom webhooks never regress call state.
5. **Stale-State Reaper:**
   - Detects stuck calls from crashed workers or dropped carrier webhooks (>10s reserved, >5s answered without connected).
   - Safely rolls back agent states and reschedules borrowers with exponential backoff.
6. **Operational Dark-Mode Dashboard (`/web`):**
   - **Tab 1: Live Operations & Safety Monitor:** Real-time agent state grid (AVAILABLE, RESERVED, DIALING, CONNECTED, WRAP_UP, PAUSED, OFFLINE), circuit breaker status (CLOSED, OPEN, HALF-OPEN), pipeline visualizer, and live feed of Safety Controller decisions with reasons.
   - **Tab 2: Efficiency & Simulation Results Panel:** One-click scenario triggers (A, B, C, D), Progressive vs. Predictive side-by-side comparison table, and Scenario D dynamic shock timeline visualizer (70% &rarr; 10% drop with automatic safety fallback).
   - **Tab 3: Failure Cases & Load Test Verification:** Status cards for all 5 Failure Cases (with event trace logs) and concurrency load test metrics (100 &rarr; 1,000 workers, 0 double allocations, p50 / p95 / p99 reservation latency).

---

## 6 Enterprise Robustness Upgrades (Dedicated Documentation)

In addition to core progressive and predictive pacing, the platform includes 6 enterprise-grade robustness modules, each documented with problem statements, mathematical foundations, code breakdowns, and test verifications:

1. [**01. Sharded B-Tree Bucketing & PgBouncer High-Concurrency Layer**](./docs/01_DATABASE_SHARDING_AND_PGBOUNCER.md)
   - *Problem*: B-Tree index head contention and buffer lock hotspots under 100+ concurrent workers; PgBouncer transaction-mode state leakage.
   - *Solution*: $N$-way concurrency partitioning (`shard_id`), sharded indexes (`idx_agents_sharded_available`), starvation fallback, and stateless transaction boundaries.
2. [**02. Predictive Intelligence: 1D Kalman Filter & Answering Machine Detection (AMD)**](./docs/02_KALMAN_FILTER_AND_AMD.md)
   - *Problem*: Static EWMA lags during carrier blocks causing abandonment spikes; voicemails tie up agents for 30–60s on 40–60% of answered calls.
   - *Solution*: 1D recursive Kalman filter with dynamic sample-size noise scaling ($R \propto 1/\sqrt{N}$) + acoustic temporal energy classifier (Human vs. Machine).
3. [**03. Multi-Carrier Trunk Hedging & Dynamic Least-Cost Routing (LCR)**](./docs/03_MULTI_CARRIER_LCR_AND_CIRCUIT_BREAKER.md)
   - *Problem*: Single carrier outage risks, high tail latencies (P99 > 3000ms), and unoptimized telephony rate cards.
   - *Solution*: Multi-trunk LCR selector, concurrent trunk hedging on primary stall (>1200ms), and adaptive per-trunk circuit breakers.
4. [**04. Distributed Leases with Monotonic Fencing Tokens**](./docs/04_DISTRIBUTED_LEASES_AND_FENCING_TOKENS.md)
   - *Problem*: Zombie workers waking from GC/network pauses overwrite reclaimed agents, creating split-brain double-allocations.
   - *Solution*: Monotonic `lease_token BIGINT` incremented on every reservation and reaper reset; stale writes rejected with `ErrStaleFencingToken`.
5. [**05. TCPA & Regulation F Compliance Gate**](./docs/05_TCPA_AND_REGULATION_F_COMPLIANCE.md)
   - *Problem*: \$500–\$1,500 statutory fines per call for dialing outside 8:00 AM–9:00 PM local time; CFPB 7-in-7 weekly attempt limits; Do-Not-Call (DNC) violations.
   - *Solution*: Timezone window engine with IANA conversions, 7-in-7 rolling attempt tracker, conversation wait period, and in-memory DNC Bloom filter.
6. [**06. Observability: OpenTelemetry Distributed Tracing & Chaos Engineering Test Harness**](./docs/06_OBSERVABILITY_TRACING_AND_CHAOS.md)
   - *Problem*: Untraceable async webhooks across distributed stages; distributed edge-case failures only surfacing during production peaks.
   - *Solution*: Standard OpenTelemetry W3C `traceparent` propagation + configurable Chaos fault injector (carrier 503 drops, jitter, worker panics).

---

## Directory Structure
```
cmd/
  dialer/main.go            # Dialer REST API server + Worker pool + Graceful shutdown
  simulator/main.go         # Simulator for failure scenarios and predictive benchmarks
internal/
  domain/                   # Entities, explicit Agent & Call state machines, monotonic guards
  store/                    # PostgreSQL migrations, pgxpool, sharded reservations, fencing tokens
  pacing/                   # Progressive Engine + Predictive Little's Law + 1D Kalman Filter
  safety/                   # Non-bypassable Safety Controller & Circuit Breaker
  allocator/                # Atomic agent & borrower reservation + carrier dispatch
  provider/                 # TelecomProvider, MockProviderA/B, AMD Classifier, Multi-Carrier LCR Router
  compliance/               # TCPA local timezone windows, CFPB Reg F 7-in-7 cap, DNC Bloom filter
  telemetry/                # OpenTelemetry W3C distributed tracing context propagation
  chaos/                    # Chaos engineering fault injection harness (jitter, carrier drop, panic)
  worker/                   # Distributed worker loop + stale-state reaper / recovery
  tests/                    # Automated integration tests for Failure Cases 1-5
web/                        # React + TypeScript + Tailwind CSS (Vite) operational dashboard
docs/
  ARCHITECTURE.md           # Master architecture, ADRs, state machines, scale analysis
  SYSTEM_GUIDE.md           # Comprehensive high-level and low-level system design guide
  01_DATABASE_SHARDING_AND_PGBOUNCER.md
  02_KALMAN_FILTER_AND_AMD.md
  03_MULTI_CARRIER_LCR_AND_CIRCUIT_BREAKER.md
  04_DISTRIBUTED_LEASES_AND_FENCING_TOKENS.md
  05_TCPA_AND_REGULATION_F_COMPLIANCE.md
  06_OBSERVABILITY_TRACING_AND_CHAOS.md
docker-compose.yml          # PostgreSQL 16 + Backend + Frontend
Makefile                    # make up, make test, make sim, make loadtest
```

---

## Quickstart

### 1. Run via Docker Compose (One-Command Setup)
```bash
make up
# or
docker compose up -d
```
Access points:
- **Frontend Dashboard:** http://localhost:3000 (or http://localhost:5173 during local dev)
- **Backend API & Health:** http://localhost:8080/health
- **PostgreSQL 16:** localhost:5432

### 2. Run All Automated Tests
```bash
make test
# or
go test -v ./...
```

### 3. Run Full Simulation Suite (Scenarios A–D + Failure Cases 1–5 + Load Test)
```bash
make sim
# or
go run cmd/simulator/main.go --all
```
This generates `simulation_results.json` and updates both backend and frontend reporting.

### 4. Run Concurrency Load Test (100 -> 1,000 Agents)
```bash
make loadtest
# or
go run cmd/simulator/main.go --load
```

### 5. Run Frontend Development Server
```bash
cd web
npm install
npm run dev
```

---

## API Endpoints (`/api/v1`)

- `GET /health` - Service health and carrier status
- `GET /api/v1/metrics/live` - Real-time agent state grid, active calls, telemetry, circuit breaker state, and safety decisions feed
- `POST /api/v1/simulator/run` - Trigger simulation runs from UI (`target`: `all`, `scenarios`, `failures`, `load`)
- `GET /api/v1/simulator/results` - Fetch structured `simulation_results.json`
- `POST /api/agents` - Create or update an agent (`name`, `state`)
- `POST /api/borrowers` - Create a borrower debt lead (`phone_number`)
- `POST /api/campaigns/start` - Start dialer worker pool (`mode`: `PROGRESSIVE` or `PREDICTIVE`)
- `POST /api/campaigns/stop` - Stop dialer worker pool
- `POST /api/webhooks/{provider}` - Ingest asynchronous carrier events (`INITIATED`, `RINGING`, `ANSWERED`, `COMPLETED`, `FAILED`)
