CREATE TABLE IF NOT EXISTS settings (
  key text PRIMARY KEY,
  value bytea NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS hosts (
  id text PRIMARY KEY,
  name text UNIQUE NOT NULL,
  labels jsonb NOT NULL DEFAULT '{}',
  agent_version text NOT NULL DEFAULT '',
  os text NOT NULL DEFAULT '',
  cores int NOT NULL DEFAULT 0,
  mem_gb real NOT NULL DEFAULT 0,
  first_seen timestamptz NOT NULL DEFAULT now(),
  last_seen timestamptz,
  connected boolean NOT NULL DEFAULT false,
  last_sample jsonb,
  last_sample_at timestamptz,
  inventory jsonb NOT NULL DEFAULT '[]',
  revoked boolean NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS enroll_tokens (
  token_hash text PRIMARY KEY,
  labels jsonb NOT NULL DEFAULT '{}',
  created_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  used_at timestamptz
);

CREATE TABLE IF NOT EXISTS batches (
  id text PRIMARY KEY,
  type text NOT NULL,
  params jsonb NOT NULL DEFAULT '{}',
  host_ids jsonb NOT NULL,
  wave_size int NOT NULL,
  fail_limit int NOT NULL,
  current_wave int NOT NULL DEFAULT 0,
  total_waves int NOT NULL,
  state text NOT NULL,
  message text NOT NULL DEFAULT '',
  requested_by text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS commands (
  id text PRIMARY KEY,
  type text NOT NULL,
  host_id text NOT NULL REFERENCES hosts(id),
  params jsonb NOT NULL DEFAULT '{}',
  state text NOT NULL,
  message text NOT NULL DEFAULT '',
  requested_by text NOT NULL DEFAULT '',
  batch_id text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS commands_host_idx ON commands (host_id, created_at DESC);
CREATE INDEX IF NOT EXISTS commands_state_idx ON commands (state);

CREATE TABLE IF NOT EXISTS command_events (
  id bigserial PRIMARY KEY,
  command_id text NOT NULL,
  ts timestamptz NOT NULL DEFAULT now(),
  line text NOT NULL
);
CREATE INDEX IF NOT EXISTS command_events_idx ON command_events (command_id, id);

CREATE TABLE IF NOT EXISTS audit_log (
  id bigserial PRIMARY KEY,
  ts timestamptz NOT NULL DEFAULT now(),
  actor text NOT NULL,
  action text NOT NULL,
  target text NOT NULL DEFAULT '',
  detail jsonb NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS metrics_raw (
  host_id text NOT NULL,
  ts timestamptz NOT NULL,
  cpu real, mem real, disk real, disk_free real, load1 real, vms int
) PARTITION BY RANGE (ts);

CREATE TABLE IF NOT EXISTS metrics_5m (
  host_id text NOT NULL,
  ts timestamptz NOT NULL,
  cpu real, cpu_max real, mem real, mem_max real, disk real, vms real
) PARTITION BY RANGE (ts);
