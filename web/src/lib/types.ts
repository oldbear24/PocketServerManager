import type { RecordModel } from "pocketbase";

export type HostStatus = "online" | "degraded" | "offline";

export type SelfCheck = {
  name: string;
  ok: boolean;
  skipped: boolean;
  detail: string;
  checked_at: string;
};

export type AgentRuntime = {
  goroutines: number;
  heap_bytes: number;
  workers_busy: number;
  workers_max: number;
  docker_api_latency_ms: number;
  volume_free_bytes: number;
  log_level: string;
};

export type Host = RecordModel & {
  name: string;
  roles: string[];
  status: HostStatus | "";
  hostname: string;
  os: string;
  kernel: string;
  arch: string;
  cpu_cores: number;
  mem_bytes: number;
  ip4: string;
  ip6: string;
  agent_version: string;
  agent_image_digest: string;
  agent_container_id: string;
  docker_version: string;
  last_seen: string;
  rtt_ms: number;
  clock_skew_ms: number;
  connected_since: string;
  stream_open: boolean;
  uptime_s: number;
  load1: number;
  load5: number;
  load15: number;
  compose_version: string;
  cpu_percent: number;
  mem_used_bytes: number;
  disk_used_bytes: number;
  disk_total_bytes: number;
  runtime: AgentRuntime | null;
  self_checks: SelfCheck[] | null;
  inventory_at: string;
  cert_serial: string;
  cert_expires_at: string;
  host_shell_enabled: boolean;
};

export type CommandStatus =
  "queued" | "dispatched" | "running" | "succeeded" | "failed" | "timed_out" | "cancelled";

export type Command = RecordModel & {
  host: string;
  host_name: string;
  type: string;
  label: string;
  payload: unknown;
  status: CommandStatus;
  requested_by: string;
  requested_label: string;
  correlation_id: string;
  attempts: number;
  dispatched_at: string;
  started_at: string;
  finished_at: string;
  progress: { step?: string; message?: string; percent?: number } | null;
  result: Record<string, unknown> | null;
  error: string;
  output: string;
};

export type Alert = RecordModel & {
  rule: string;
  target: string;
  host: string;
  severity: "info" | "warning" | "critical";
  status: "open" | "acknowledged" | "resolved";
  first_seen: string;
  last_seen: string;
  message: string;
};

export type HostEvent = RecordModel & {
  host: string;
  kind: string;
  severity: "info" | "warning" | "critical";
  message: string;
  data: Record<string, unknown> | null;
};

export type Container = RecordModel & {
  host: string;
  stack: string;
  project: string;
  service: string;
  docker_id: string;
  name: string;
  image: string;
  state: string;
  status_text: string;
  health: string;
  system_role: string;
  restart_count: number;
  ports: { private: number; public: number; protocol: string; ip: string }[] | null;
  labels: Record<string, string> | null;
  started_at: string;
};

export type Stack = RecordModel & {
  host: string;
  name: string;
  compose_yaml: string;
  env: string;
  secret_keys: string[] | null;
  current_revision: number;
  deployed_revision: number;
  deployed_at: string;
  status: string;
  source: string;
};

export type StackRevision = RecordModel & {
  stack: string;
  number: number;
  compose_yaml: string;
  env: string;
  env_keys: string[] | null;
  author: string;
  deployed_at: string;
  note: string;
  expand?: { author?: { email?: string } };
};

export type Volume = RecordModel & {
  host: string;
  name: string;
  driver: string;
  size_bytes: number;
};

export type Network = RecordModel & {
  host: string;
  name: string;
  driver: string;
};

export type EnrollmentToken = RecordModel & {
  hint: string;
  name: string;
  roles: string[];
  expires_at: string;
  used_at: string;
  host: string;
};

export type AuditChange = { from?: unknown; to?: unknown } | "changed";

export type AuditEntry = RecordModel & {
  seq: number;
  at: string;
  category: "change" | "security" | "system";
  action: string;
  actor_kind: "user" | "token" | "superuser" | "system" | "anonymous";
  actor: string;
  actor_label: string;
  source: string;
  ip: string;
  user_agent: string;
  collection: string;
  record_id: string;
  record_label: string;
  host: string;
  changes: Record<string, AuditChange> | null;
  snapshot: Record<string, unknown> | null;
  command: string;
  revision: string;
  note: string;
  prev_hash: string;
  hash: string;
};

export type PanelInfo = {
  version: string;
  public_url: string;
  grpc_address: string;
  gateway_running: boolean;
  ca_pin: string;
  agent_image: string;
  updater_image: string;
  images_pinned: boolean;
  agents: { connected: number; total: number };
};

export type NewEnrollmentToken = {
  id: string;
  token: string;
  name: string;
  roles: string[];
  expires_at: string;
  panel: string;
  ca_pin: string;
  agent_image: string;
};

export type VerifyResult = {
  ok: boolean;
  checked: number;
  last_seq: number;
  bad_seq?: number;
  reason?: string;
};

export type DiagAgent = {
  id: string;
  connected: boolean;
  connected_since?: string;
  last_seen?: string;
  remote?: string;
  tls_version?: string;
  cert_expiry?: string;
  rtt_p50_ms: number;
  rtt_p95_ms: number;
  clock_skew_ms: number;
  messages_in: number;
  messages_out: number;
  bytes_in: number;
  bytes_out: number;
  reconnects: number;
  last_error?: string;
  last_reason?: string;
  capture_until?: string;
};

export type DiagConnectionEvent = {
  at: string;
  kind: "connected" | "disconnected";
  reason?: string;
  remote?: string;
};

export type DiagTraceEntry = {
  at: string;
  agent: string;
  direction: "in" | "out";
  method: string;
  kind: string;
  subject?: string;
  bytes?: number;
  code?: string;
  error?: string;
  payload?: string;
};

export type DiagAgentDetail = DiagAgent & {
  events: DiagConnectionEvent[] | null;
  trace: DiagTraceEntry[] | null;
};

export type DiagOverview = {
  gateway: { agents_connected: number; agents_known: number };
  messages: { per_minute: number; window_s: number };
  queue: {
    depth: Record<string, number>;
    oldest_queued_ms: number;
    retries: number;
    replays: number;
    timeouts: number;
    latency: Record<
      string,
      {
        dispatch_to_ack: { count: number; avg_ms: number; max_ms: number };
        ack_to_done: { count: number; avg_ms: number; max_ms: number };
      }
    >;
  };
  process: {
    uptime_s: number;
    goroutines: number;
    heap_bytes: number;
    total_mem_bytes: number;
    gc_cycles: number;
    realtime_clients: number;
    streams: number;
    sqlite_bytes: number;
    wal_bytes: number;
  };
  errors: { at: string; message: string; detail?: string }[] | null;
  cron: { id: string; expression: string }[] | null;
};
