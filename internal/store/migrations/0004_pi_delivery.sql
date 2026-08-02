CREATE TABLE pi_deliveries (
  delivery_id TEXT PRIMARY KEY CHECK (
    length(delivery_id) = 26
    AND substr(delivery_id, 1, 4) = 'dlv_'
    AND substr(delivery_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'
  ),
  task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE RESTRICT,
  task_version INTEGER NOT NULL CHECK (task_version >= 1),
  pi_session_id TEXT NOT NULL CHECK (
    length(CAST(pi_session_id AS BLOB)) BETWEEN 1 AND 256
    AND instr(pi_session_id, char(0)) = 0
  ),
  delivery_key TEXT NOT NULL CHECK (
    length(delivery_key) = 71
    AND substr(delivery_key, 1, 7) = 'sha256:'
    AND substr(delivery_key, 8) NOT GLOB '*[^0-9a-f]*'
  ),
  predecessor_delivery_id TEXT UNIQUE REFERENCES pi_deliveries(delivery_id) ON DELETE RESTRICT,
  connection_id TEXT CHECK (
    connection_id IS NULL OR (
      length(CAST(connection_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(connection_id, char(0)) = 0
    )
  ),
  state TEXT NOT NULL CHECK (state IN ('pending','inserting','persisted','uncertain','rejected')),
  diagnostic_code TEXT CHECK (
    diagnostic_code IS NULL OR (
      length(diagnostic_code) BETWEEN 1 AND 64
      AND diagnostic_code NOT GLOB '*[^a-z0-9_]*'
    )
  ),
  requeue_reason TEXT CHECK (
    requeue_reason IS NULL OR (
      length(CAST(requeue_reason AS BLOB)) BETWEEN 1 AND 4096
      AND instr(requeue_reason, char(0)) = 0
    )
  ),
  offered_at_us INTEGER,
  inserting_at_us INTEGER,
  completed_at_us INTEGER,
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  CHECK (predecessor_delivery_id IS NULL OR predecessor_delivery_id <> delivery_id),
  CHECK (offered_at_us IS NULL OR offered_at_us BETWEEN created_at_us AND updated_at_us),
  CHECK (inserting_at_us IS NULL OR inserting_at_us BETWEEN created_at_us AND updated_at_us),
  CHECK (completed_at_us IS NULL OR completed_at_us BETWEEN created_at_us AND updated_at_us)
) STRICT;

CREATE UNIQUE INDEX pi_deliveries_active_attempt_idx
  ON pi_deliveries(task_id, task_version, pi_session_id)
  WHERE state IN ('pending', 'inserting');
CREATE INDEX pi_deliveries_session_state_updated_idx
  ON pi_deliveries(pi_session_id, state, updated_at_us, delivery_id);
CREATE INDEX pi_deliveries_connection_inserting_idx
  ON pi_deliveries(connection_id, state)
  WHERE state = 'inserting';
