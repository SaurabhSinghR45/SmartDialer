export interface AgentStates {
  AVAILABLE: number;
  RESERVED: number;
  DIALING: number;
  CONNECTED: number;
  WRAP_UP: number;
  PAUSED: number;
  OFFLINE: number;
  TOTAL: number;
}

export interface ActiveCalls {
  connected: number;
  ringing: number;
  in_flight: number;
}

export interface Telemetry {
  rolling_answer_rate: number;
  rolling_abandonment_rate: number;
  provider_error_rate: number;
  agent_drop_rate_5s: number;
}

export interface WorkerStats {
  total_proposed: number;
  total_approved: number;
  total_placed: number;
  total_failed: number;
}

export interface SafetyDecisionItem {
  id: string;
  mode: string;
  proposed_calls: number;
  approved_calls: number;
  decision: 'APPROVE' | 'REDUCE' | 'REJECT' | 'FALLBACK_TO_PROGRESSIVE';
  reason: string;
  timestamp: string;
}

export interface LiveMetricsResponse {
  timestamp: string;
  mode: string;
  agent_states: AgentStates;
  active_calls: ActiveCalls;
  telemetry: Telemetry;
  circuit_breaker: 'CLOSED' | 'OPEN' | 'HALF-OPEN';
  worker_stats: WorkerStats;
  recent_decisions: SafetyDecisionItem[];
}

export interface ModeMetrics {
  mode: string;
  calls_initiated: number;
  calls_connected: number;
  abandoned_calls: number;
  abandonment_pct: number;
  agent_util_pct: number;
  approve_count: number;
  reduce_count: number;
  reject_count: number;
  fallback_count: number;
}

export interface ScenarioComparison {
  name: string;
  answer_rate: number;
  talk_time_sec: number;
  progressive: ModeMetrics;
  predictive: ModeMetrics;
}

export interface TimelinePoint {
  seconds: number;
  answer_rate_pct: number;
  ringing_calls: number;
  connected: number;
  mode: string;
  event?: string;
}

export interface FailureCaseReport {
  id: number;
  name: string;
  passed: boolean;
  description: string;
  event_trace: string;
}

export interface LoadTestLevel {
  workers: number;
  successful_reservations: number;
  double_allocations: number;
  throughput_rps: number;
  total_duration_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}

export interface LoadTestSummary {
  double_allocations: number;
  levels: LoadTestLevel[];
  p99_latency: string;
}

export interface FullSimulationExport {
  generated_at: string;
  scenarios: ScenarioComparison[];
  scenario_d_timeline: TimelinePoint[];
  failure_cases: FailureCaseReport[];
  load_test: LoadTestSummary;
}
