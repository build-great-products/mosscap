-- Prefix-free opaque identifiers are 22 raw base64url characters. The legacy
-- prefixed form remains valid so upgrades do not make durable references stale.

CREATE TABLE slack_effects_v6 (
  effect_id TEXT PRIMARY KEY CHECK (((length(effect_id) = 22 AND effect_id NOT GLOB '*[^A-Za-z0-9_-]*') OR (length(effect_id) = 26 AND substr(effect_id, 1, 4) = 'eff_' AND substr(effect_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'))),
  idempotency_key TEXT NOT NULL UNIQUE CHECK (length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128 AND instr(idempotency_key, char(0)) = 0),
  predecessor_effect_id TEXT REFERENCES slack_effects_v6(effect_id) ON DELETE RESTRICT,
  related_task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK (kind IN ('send', 'edit', 'delete', 'react', 'unreact', 'file_upload')),
  team_id TEXT NOT NULL CHECK (length(CAST(team_id AS BLOB)) BETWEEN 2 AND 128 AND substr(team_id, 1, 1) = 'T' AND team_id NOT GLOB '*[^A-Z0-9]*'),
  channel_id TEXT NOT NULL CHECK (length(CAST(channel_id AS BLOB)) BETWEEN 2 AND 128 AND substr(channel_id, 1, 1) IN ('C', 'G') AND channel_id NOT GLOB '*[^A-Z0-9]*'),
  target_message_ts TEXT CHECK (target_message_ts IS NULL OR (length(target_message_ts) BETWEEN 8 AND 32 AND instr(target_message_ts, '.') > 1 AND length(substr(target_message_ts, instr(target_message_ts, '.') + 1)) = 6 AND substr(target_message_ts, 1, instr(target_message_ts, '.') - 1) NOT GLOB '*[^0-9]*' AND substr(target_message_ts, instr(target_message_ts, '.') + 1) NOT GLOB '*[^0-9]*')),
  thread_ts TEXT CHECK (thread_ts IS NULL OR (length(thread_ts) BETWEEN 8 AND 32 AND instr(thread_ts, '.') > 1 AND length(substr(thread_ts, instr(thread_ts, '.') - 1)) >= 1 AND length(substr(thread_ts, instr(thread_ts, '.') + 1)) = 6 AND substr(thread_ts, 1, instr(thread_ts, '.') - 1) NOT GLOB '*[^0-9]*' AND substr(thread_ts, instr(thread_ts, '.') + 1) NOT GLOB '*[^0-9]*')),
  text TEXT CHECK (text IS NULL OR (length(CAST(text AS BLOB)) BETWEEN 1 AND 40000 AND instr(text, char(0)) = 0)),
  emoji TEXT CHECK (emoji IS NULL OR (length(CAST(emoji AS BLOB)) BETWEEN 1 AND 100 AND (emoji NOT GLOB '*[^A-Za-z0-9_+-]*' OR (length(emoji) > 13 AND substr(emoji, length(emoji) - 12) GLOB '::skin-tone-[2-6]' AND substr(emoji, 1, length(emoji) - 13) NOT GLOB '*[^A-Za-z0-9_+-]*')))),
  operator_reason TEXT CHECK (operator_reason IS NULL OR (length(CAST(operator_reason AS BLOB)) BETWEEN 1 AND 4096 AND instr(operator_reason, char(0)) = 0)),
  payload_hash TEXT NOT NULL CHECK (length(payload_hash) = 64 AND payload_hash NOT GLOB '*[^0-9a-f]*'),
  state TEXT NOT NULL CHECK (state IN ('prepared', 'submitted', 'confirmed', 'failed', 'ambiguous')),
  attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
  prepared_at_us INTEGER NOT NULL CHECK (prepared_at_us > 0),
  submitted_at_us INTEGER,
  completed_at_us INTEGER,
  response_message_ts TEXT CHECK (response_message_ts IS NULL OR (length(response_message_ts) BETWEEN 8 AND 32 AND instr(response_message_ts, '.') > 1 AND length(substr(response_message_ts, instr(response_message_ts, '.') + 1)) = 6 AND substr(response_message_ts, 1, instr(response_message_ts, '.') - 1) NOT GLOB '*[^0-9]*' AND substr(response_message_ts, instr(response_message_ts, '.') + 1) NOT GLOB '*[^0-9]*')),
  response_file_id TEXT CHECK (response_file_id IS NULL OR (length(CAST(response_file_id AS BLOB)) BETWEEN 2 AND 128 AND substr(response_file_id, 1, 1) = 'F' AND response_file_id NOT GLOB '*[^A-Z0-9]*')),
  diagnostic_code TEXT CHECK (diagnostic_code IS NULL OR (length(diagnostic_code) BETWEEN 1 AND 64 AND diagnostic_code NOT GLOB '*[^a-z0-9_]*')),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  CHECK (predecessor_effect_id IS NULL OR predecessor_effect_id <> effect_id),
  CHECK (prepared_at_us BETWEEN created_at_us AND updated_at_us),
  CHECK (submitted_at_us IS NULL OR submitted_at_us BETWEEN prepared_at_us AND updated_at_us),
  CHECK (completed_at_us IS NULL OR ((submitted_at_us IS NOT NULL AND completed_at_us BETWEEN submitted_at_us AND updated_at_us) OR (kind = 'file_upload' AND state = 'failed' AND attempt_count = 0 AND completed_at_us BETWEEN prepared_at_us AND updated_at_us))),
  CHECK ((state = 'prepared' AND attempt_count = 0 AND submitted_at_us IS NULL AND completed_at_us IS NULL) OR (state = 'submitted' AND attempt_count = 1 AND submitted_at_us IS NOT NULL AND completed_at_us IS NULL) OR (state IN ('confirmed', 'failed', 'ambiguous') AND attempt_count = 1 AND submitted_at_us IS NOT NULL AND completed_at_us IS NOT NULL) OR (kind = 'file_upload' AND state = 'failed' AND attempt_count = 0 AND submitted_at_us IS NULL AND completed_at_us IS NOT NULL)),
  CHECK (kind <> 'file_upload' OR (target_message_ts IS NULL AND emoji IS NULL AND response_message_ts IS NULL AND response_file_id IS NULL))
) STRICT;

CREATE TABLE files_v6 (
  file_id TEXT PRIMARY KEY CHECK (((length(file_id) = 22 AND file_id NOT GLOB '*[^A-Za-z0-9_-]*') OR (length(file_id) = 26 AND substr(file_id, 1, 4) = 'fil_' AND substr(file_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'))),
  direction TEXT NOT NULL CHECK (direction IN ('from_slack', 'to_slack')),
  slack_file_id TEXT CHECK (slack_file_id IS NULL OR (length(CAST(slack_file_id AS BLOB)) BETWEEN 2 AND 128 AND substr(slack_file_id, 1, 1) = 'F' AND slack_file_id NOT GLOB '*[^A-Z0-9]*')),
  permalink TEXT CHECK (permalink IS NULL OR (length(CAST(permalink AS BLOB)) BETWEEN 1 AND 4096 AND instr(permalink, char(0)) = 0)),
  original_name TEXT NOT NULL CHECK (length(CAST(original_name AS BLOB)) BETWEEN 1 AND 1024 AND instr(original_name, char(0)) = 0),
  declared_mime TEXT CHECK (declared_mime IS NULL OR (length(CAST(declared_mime AS BLOB)) BETWEEN 1 AND 255 AND instr(declared_mime, char(0)) = 0)),
  declared_size_bytes INTEGER CHECK (declared_size_bytes IS NULL OR declared_size_bytes >= 0),
  validated_size_bytes INTEGER CHECK (validated_size_bytes IS NULL OR validated_size_bytes >= 0),
  sha256 TEXT CHECK (sha256 IS NULL OR (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*')),
  related_task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  related_effect_id TEXT REFERENCES slack_effects_v6(effect_id) ON DELETE RESTRICT,
  effect_ordinal INTEGER CHECK (effect_ordinal IS NULL OR effect_ordinal >= 0),
  source_attached INTEGER NOT NULL DEFAULT 1 CHECK (source_attached IN (0, 1)),
  source_detached_at_us INTEGER,
  source_path TEXT CHECK (source_path IS NULL OR (length(CAST(source_path AS BLOB)) BETWEEN 1 AND 4096 AND substr(source_path, 1, 1) = '/' AND instr(source_path, char(0)) = 0)),
  source_device INTEGER,
  source_inode INTEGER,
  source_mtime_ns INTEGER,
  transfer_state TEXT NOT NULL CHECK (transfer_state IN ('metadata_only', 'validated', 'unavailable', 'oversized', 'content_changed', 'prepared', 'submitted', 'confirmed', 'failed', 'ambiguous')),
  diagnostic_code TEXT CHECK (diagnostic_code IS NULL OR (length(diagnostic_code) BETWEEN 1 AND 64 AND diagnostic_code NOT GLOB '*[^a-z0-9_]*')),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  CHECK ((validated_size_bytes IS NULL AND sha256 IS NULL) OR (validated_size_bytes IS NOT NULL AND sha256 IS NOT NULL)),
  CHECK ((direction = 'from_slack' AND slack_file_id IS NOT NULL AND related_task_id IS NOT NULL AND related_effect_id IS NULL AND effect_ordinal IS NULL AND source_path IS NULL AND source_device IS NULL AND source_inode IS NULL AND source_mtime_ns IS NULL AND transfer_state IN ('metadata_only', 'validated', 'unavailable', 'oversized', 'content_changed')) OR (direction = 'to_slack' AND related_task_id IS NULL AND related_effect_id IS NOT NULL AND effect_ordinal IS NOT NULL AND source_path IS NOT NULL AND validated_size_bytes IS NOT NULL AND sha256 IS NOT NULL AND source_attached = 0 AND source_detached_at_us IS NULL AND transfer_state IN ('prepared', 'submitted', 'confirmed', 'failed', 'ambiguous'))),
  CHECK ((direction = 'from_slack' AND ((source_attached = 1 AND source_detached_at_us IS NULL) OR (source_attached = 0 AND source_detached_at_us IS NOT NULL))) OR (direction = 'to_slack' AND source_attached = 0 AND source_detached_at_us IS NULL)),
  CHECK (source_detached_at_us IS NULL OR source_detached_at_us BETWEEN created_at_us AND updated_at_us),
  UNIQUE (slack_file_id, related_task_id),
  UNIQUE (related_effect_id, effect_ordinal)
) STRICT;

CREATE TABLE file_temporary_transfers_v6 (
  transfer_id TEXT PRIMARY KEY CHECK (((length(transfer_id) = 22 AND transfer_id NOT GLOB '*[^A-Za-z0-9_-]*') OR (length(transfer_id) = 26 AND substr(transfer_id, 1, 4) = 'xfr_' AND substr(transfer_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'))),
  file_id TEXT NOT NULL REFERENCES files_v6(file_id) ON DELETE RESTRICT,
  generated_name TEXT NOT NULL UNIQUE CHECK (length(generated_name) = 44 AND substr(generated_name, 1, 12) = 'mosscap-xfr-' AND substr(generated_name, 13) NOT GLOB '*[^0-9a-f]*'),
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
  expires_at_us INTEGER NOT NULL CHECK (expires_at_us > 0),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  CHECK (expires_at_us >= created_at_us)
) STRICT;

CREATE TABLE pi_deliveries_v6 (
  delivery_id TEXT PRIMARY KEY CHECK (
    ((length(delivery_id) = 22 AND delivery_id NOT GLOB '*[^A-Za-z0-9_-]*') OR (length(delivery_id) = 26 AND substr(delivery_id, 1, 4) = 'dlv_' AND substr(delivery_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'))
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
  predecessor_delivery_id TEXT UNIQUE REFERENCES pi_deliveries_v6(delivery_id) ON DELETE RESTRICT,
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

-- Predecessor rows are created before their successors; preserve that order for
-- the self-referential foreign keys while copying.
INSERT INTO slack_effects_v6 SELECT * FROM slack_effects ORDER BY rowid;
INSERT INTO files_v6 SELECT * FROM files ORDER BY rowid;
INSERT INTO file_temporary_transfers_v6 SELECT * FROM file_temporary_transfers ORDER BY rowid;
INSERT INTO pi_deliveries_v6 SELECT * FROM pi_deliveries ORDER BY rowid;

DROP TABLE file_temporary_transfers;
DROP TABLE files;
DROP TABLE slack_effects;
DROP TABLE pi_deliveries;
ALTER TABLE slack_effects_v6 RENAME TO slack_effects;
ALTER TABLE files_v6 RENAME TO files;
ALTER TABLE file_temporary_transfers_v6 RENAME TO file_temporary_transfers;
ALTER TABLE pi_deliveries_v6 RENAME TO pi_deliveries;

CREATE INDEX slack_effects_state_updated_idx ON slack_effects(state, updated_at_us, effect_id);
CREATE INDEX slack_effects_predecessor_idx ON slack_effects(predecessor_effect_id, effect_id) WHERE predecessor_effect_id IS NOT NULL;
CREATE INDEX slack_effects_exceptional_idx ON slack_effects(updated_at_us, effect_id) WHERE state IN ('failed', 'ambiguous');
CREATE INDEX files_related_task_idx ON files(related_task_id, file_id) WHERE related_task_id IS NOT NULL;
CREATE INDEX files_related_effect_idx ON files(related_effect_id, effect_ordinal, file_id) WHERE related_effect_id IS NOT NULL;
CREATE INDEX file_temporary_transfers_expiry_idx ON file_temporary_transfers(expires_at_us, transfer_id);
CREATE INDEX file_temporary_transfers_file_idx ON file_temporary_transfers(file_id, transfer_id);

CREATE UNIQUE INDEX pi_deliveries_active_attempt_idx
  ON pi_deliveries(task_id, task_version, pi_session_id)
  WHERE state IN ('pending', 'inserting');
CREATE INDEX pi_deliveries_session_state_updated_idx
  ON pi_deliveries(pi_session_id, state, updated_at_us, delivery_id);
CREATE INDEX pi_deliveries_connection_inserting_idx
  ON pi_deliveries(connection_id, state)
  WHERE state = 'inserting';

