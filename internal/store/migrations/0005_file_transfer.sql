CREATE TABLE slack_effects_v5 (
  effect_id TEXT PRIMARY KEY CHECK (length(effect_id) = 26 AND substr(effect_id, 1, 4) = 'eff_' AND substr(effect_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'),
  idempotency_key TEXT NOT NULL UNIQUE CHECK (length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128 AND instr(idempotency_key, char(0)) = 0),
  predecessor_effect_id TEXT REFERENCES slack_effects_v5(effect_id) ON DELETE RESTRICT,
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

INSERT INTO slack_effects_v5 SELECT * FROM slack_effects;

CREATE TABLE files_v5 (
  file_id TEXT PRIMARY KEY CHECK (length(file_id) = 26 AND substr(file_id, 1, 4) = 'fil_' AND substr(file_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'),
  direction TEXT NOT NULL CHECK (direction IN ('from_slack', 'to_slack')),
  slack_file_id TEXT CHECK (slack_file_id IS NULL OR (length(CAST(slack_file_id AS BLOB)) BETWEEN 2 AND 128 AND substr(slack_file_id, 1, 1) = 'F' AND slack_file_id NOT GLOB '*[^A-Z0-9]*')),
  permalink TEXT CHECK (permalink IS NULL OR (length(CAST(permalink AS BLOB)) BETWEEN 1 AND 4096 AND instr(permalink, char(0)) = 0)),
  original_name TEXT NOT NULL CHECK (length(CAST(original_name AS BLOB)) BETWEEN 1 AND 1024 AND instr(original_name, char(0)) = 0),
  declared_mime TEXT CHECK (declared_mime IS NULL OR (length(CAST(declared_mime AS BLOB)) BETWEEN 1 AND 255 AND instr(declared_mime, char(0)) = 0)),
  declared_size_bytes INTEGER CHECK (declared_size_bytes IS NULL OR declared_size_bytes >= 0),
  validated_size_bytes INTEGER CHECK (validated_size_bytes IS NULL OR validated_size_bytes >= 0),
  sha256 TEXT CHECK (sha256 IS NULL OR (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*')),
  related_task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  related_effect_id TEXT REFERENCES slack_effects_v5(effect_id) ON DELETE RESTRICT,
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

INSERT INTO files_v5(file_id,direction,slack_file_id,permalink,original_name,declared_mime,declared_size_bytes,validated_size_bytes,sha256,related_task_id,related_effect_id,effect_ordinal,source_attached,source_detached_at_us,source_path,source_device,source_inode,source_mtime_ns,transfer_state,diagnostic_code,created_at_us,updated_at_us)
  SELECT file_id,direction,slack_file_id,permalink,original_name,declared_mime,size_bytes,CASE WHEN sha256 IS NULL THEN NULL ELSE size_bytes END,sha256,related_task_id,related_effect_id,NULL,1,NULL,NULL,NULL,NULL,NULL,CASE WHEN transfer_state = 'metadata_only' THEN 'metadata_only' WHEN transfer_state = 'confirmed' THEN 'validated' WHEN transfer_state = 'failed' THEN 'unavailable' WHEN transfer_state = 'temporary' THEN 'validated' WHEN transfer_state = 'expired' THEN 'validated' ELSE 'metadata_only' END,diagnostic_code,created_at_us,updated_at_us
  FROM files WHERE direction = 'from_slack';

DROP TABLE files;
DROP TABLE slack_effects;
ALTER TABLE slack_effects_v5 RENAME TO slack_effects;
ALTER TABLE files_v5 RENAME TO files;

CREATE TABLE file_temporary_transfers (
  transfer_id TEXT PRIMARY KEY CHECK (length(transfer_id) = 26 AND substr(transfer_id, 1, 4) = 'xfr_' AND substr(transfer_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'),
  file_id TEXT NOT NULL REFERENCES files(file_id) ON DELETE RESTRICT,
  generated_name TEXT NOT NULL UNIQUE CHECK (length(generated_name) = 44 AND substr(generated_name, 1, 12) = 'mosscap-xfr-' AND substr(generated_name, 13) NOT GLOB '*[^0-9a-f]*'),
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
  expires_at_us INTEGER NOT NULL CHECK (expires_at_us > 0),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  CHECK (expires_at_us >= created_at_us)
) STRICT;

CREATE INDEX slack_effects_state_updated_idx ON slack_effects(state, updated_at_us, effect_id);
CREATE INDEX slack_effects_predecessor_idx ON slack_effects(predecessor_effect_id, effect_id) WHERE predecessor_effect_id IS NOT NULL;
CREATE INDEX slack_effects_exceptional_idx ON slack_effects(updated_at_us, effect_id) WHERE state IN ('failed', 'ambiguous');
CREATE INDEX files_related_task_idx ON files(related_task_id, file_id) WHERE related_task_id IS NOT NULL;
CREATE INDEX files_related_effect_idx ON files(related_effect_id, effect_ordinal, file_id) WHERE related_effect_id IS NOT NULL;
CREATE INDEX file_temporary_transfers_expiry_idx ON file_temporary_transfers(expires_at_us, transfer_id);
CREATE INDEX file_temporary_transfers_file_idx ON file_temporary_transfers(file_id, transfer_id);
