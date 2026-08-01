CREATE TABLE slack_source_events (
  source_event_id TEXT PRIMARY KEY CHECK (
    length(CAST(source_event_id AS BLOB)) BETWEEN 1 AND 128
    AND instr(source_event_id, char(0)) = 0
  ),
  envelope_id TEXT CHECK (
    envelope_id IS NULL OR (
      length(CAST(envelope_id AS BLOB)) BETWEEN 1 AND 128
      AND instr(envelope_id, char(0)) = 0
    )
  ),
  event_kind TEXT NOT NULL CHECK (
    length(event_kind) BETWEEN 1 AND 64
    AND event_kind NOT GLOB '*[^a-z0-9_]*'
  ),
  team_id TEXT CHECK (
    team_id IS NULL OR (
      length(CAST(team_id AS BLOB)) BETWEEN 2 AND 128
      AND substr(team_id, 1, 1) = 'T'
      AND team_id NOT GLOB '*[^A-Z0-9]*'
    )
  ),
  channel_id TEXT CHECK (
    channel_id IS NULL OR (
      length(CAST(channel_id AS BLOB)) BETWEEN 2 AND 128
      AND substr(channel_id, 1, 1) IN ('C', 'G')
      AND channel_id NOT GLOB '*[^A-Z0-9]*'
    )
  ),
  message_ts TEXT CHECK (
    message_ts IS NULL OR (
      length(message_ts) BETWEEN 8 AND 32
      AND instr(message_ts, '.') > 1
      AND length(substr(message_ts, instr(message_ts, '.') + 1)) = 6
      AND substr(message_ts, 1, instr(message_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(message_ts, instr(message_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  ),
  thread_ts TEXT CHECK (
    thread_ts IS NULL OR (
      length(thread_ts) BETWEEN 8 AND 32
      AND instr(thread_ts, '.') > 1
      AND length(substr(thread_ts, instr(thread_ts, '.') + 1)) = 6
      AND substr(thread_ts, 1, instr(thread_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(thread_ts, instr(thread_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  ),
  user_id TEXT CHECK (
    user_id IS NULL OR (
      length(CAST(user_id AS BLOB)) BETWEEN 2 AND 128
      AND substr(user_id, 1, 1) IN ('U', 'W', 'B')
      AND user_id NOT GLOB '*[^A-Z0-9]*'
    )
  ),
  slack_event_at_us INTEGER CHECK (slack_event_at_us IS NULL OR slack_event_at_us > 0),
  received_at_us INTEGER NOT NULL CHECK (received_at_us > 0),
  committed_at_us INTEGER NOT NULL CHECK (committed_at_us >= received_at_us),
  admission_result TEXT NOT NULL CHECK (admission_result IN (
    'admitted', 'updated', 'deleted', 'duplicate',
    'ignored_workspace', 'ignored_channel', 'ignored_untrusted',
    'ignored_unmentioned', 'ignored_bot', 'ignored_stale',
    'ignored_untracked', 'malformed'
  )),
  task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  diagnostic_code TEXT CHECK (
    diagnostic_code IS NULL OR (
      length(diagnostic_code) BETWEEN 1 AND 64
      AND diagnostic_code NOT GLOB '*[^a-z0-9_]*'
    )
  ),
  task_version INTEGER NOT NULL CHECK (task_version >= 0),
  changed INTEGER NOT NULL CHECK (changed IN (0, 1)),
  redelivery_count INTEGER NOT NULL DEFAULT 0 CHECK (
    redelivery_count BETWEEN 0 AND 9223372036854775807
  ),
  last_seen_at_us INTEGER NOT NULL CHECK (last_seen_at_us >= committed_at_us),
  CHECK (thread_ts IS NULL OR message_ts IS NOT NULL),
  CHECK (admission_result NOT IN ('admitted', 'updated', 'deleted') OR task_id IS NOT NULL),
  CHECK ((task_id IS NULL AND task_version = 0) OR (task_id IS NOT NULL AND task_version > 0)),
  CHECK (
    (admission_result IN ('admitted', 'deleted') AND changed = 1)
    OR (admission_result = 'updated')
    OR (admission_result NOT IN ('admitted', 'updated', 'deleted') AND changed = 0)
  ),
  UNIQUE (source_event_id, task_id)
) STRICT;

CREATE TABLE slack_task_sources (
  task_id TEXT PRIMARY KEY REFERENCES tasks(task_id) ON DELETE RESTRICT,
  initial_source_event_id TEXT NOT NULL UNIQUE,
  team_id TEXT NOT NULL CHECK (
    length(CAST(team_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(team_id, 1, 1) = 'T'
    AND team_id NOT GLOB '*[^A-Z0-9]*'
  ),
  channel_id TEXT NOT NULL CHECK (
    length(CAST(channel_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(channel_id, 1, 1) IN ('C', 'G')
    AND channel_id NOT GLOB '*[^A-Z0-9]*'
  ),
  author_user_id TEXT NOT NULL CHECK (
    length(CAST(author_user_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(author_user_id, 1, 1) IN ('U', 'W')
    AND author_user_id NOT GLOB '*[^A-Z0-9]*'
  ),
  message_ts TEXT NOT NULL CHECK (
    length(message_ts) BETWEEN 8 AND 32
    AND instr(message_ts, '.') > 1
    AND length(substr(message_ts, instr(message_ts, '.') + 1)) = 6
    AND substr(message_ts, 1, instr(message_ts, '.') - 1) NOT GLOB '*[^0-9]*'
    AND substr(message_ts, instr(message_ts, '.') + 1) NOT GLOB '*[^0-9]*'
  ),
  thread_ts TEXT NOT NULL CHECK (
    length(thread_ts) BETWEEN 8 AND 32
    AND instr(thread_ts, '.') > 1
    AND length(substr(thread_ts, instr(thread_ts, '.') + 1)) = 6
    AND substr(thread_ts, 1, instr(thread_ts, '.') - 1) NOT GLOB '*[^0-9]*'
    AND substr(thread_ts, instr(thread_ts, '.') + 1) NOT GLOB '*[^0-9]*'
  ),
  permalink TEXT CHECK (
    permalink IS NULL OR (
      length(CAST(permalink AS BLOB)) BETWEEN 1 AND 4096
      AND instr(permalink, char(0)) = 0
    )
  ),
  latest_event_at_us INTEGER NOT NULL CHECK (latest_event_at_us > 0),
  source_deleted_at_us INTEGER CHECK (
    source_deleted_at_us IS NULL OR source_deleted_at_us >= latest_event_at_us
  ),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  UNIQUE (team_id, channel_id, message_ts),
  FOREIGN KEY (initial_source_event_id, task_id)
    REFERENCES slack_source_events(source_event_id, task_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE slack_effects (
  effect_id TEXT PRIMARY KEY CHECK (
    length(effect_id) = 26
    AND substr(effect_id, 1, 4) = 'eff_'
    AND substr(effect_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'
  ),
  idempotency_key TEXT NOT NULL UNIQUE CHECK (
    length(CAST(idempotency_key AS BLOB)) BETWEEN 1 AND 128
    AND instr(idempotency_key, char(0)) = 0
  ),
  predecessor_effect_id TEXT REFERENCES slack_effects(effect_id) ON DELETE RESTRICT,
  related_task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK (kind IN ('send', 'edit', 'delete', 'react', 'unreact', 'file_upload')),
  team_id TEXT NOT NULL CHECK (
    length(CAST(team_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(team_id, 1, 1) = 'T'
    AND team_id NOT GLOB '*[^A-Z0-9]*'
  ),
  channel_id TEXT NOT NULL CHECK (
    length(CAST(channel_id AS BLOB)) BETWEEN 2 AND 128
    AND substr(channel_id, 1, 1) IN ('C', 'G')
    AND channel_id NOT GLOB '*[^A-Z0-9]*'
  ),
  target_message_ts TEXT CHECK (
    target_message_ts IS NULL OR (
      length(target_message_ts) BETWEEN 8 AND 32
      AND instr(target_message_ts, '.') > 1
      AND length(substr(target_message_ts, instr(target_message_ts, '.') + 1)) = 6
      AND substr(target_message_ts, 1, instr(target_message_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(target_message_ts, instr(target_message_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  ),
  thread_ts TEXT CHECK (
    thread_ts IS NULL OR (
      length(thread_ts) BETWEEN 8 AND 32
      AND instr(thread_ts, '.') > 1
      AND length(substr(thread_ts, instr(thread_ts, '.') + 1)) = 6
      AND substr(thread_ts, 1, instr(thread_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(thread_ts, instr(thread_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  ),
  text TEXT CHECK (
    text IS NULL OR (
      length(CAST(text AS BLOB)) BETWEEN 1 AND 40000
      AND instr(text, char(0)) = 0
    )
  ),
  emoji TEXT CHECK (
    emoji IS NULL OR (
      length(CAST(emoji AS BLOB)) BETWEEN 1 AND 100
      AND (
        emoji NOT GLOB '*[^A-Za-z0-9_+-]*'
        OR (
          length(emoji) > 13
          AND substr(emoji, length(emoji) - 12) GLOB '::skin-tone-[2-6]'
          AND substr(emoji, 1, length(emoji) - 13) NOT GLOB '*[^A-Za-z0-9_+-]*'
        )
      )
    )
  ),
  operator_reason TEXT CHECK (
    operator_reason IS NULL OR (
      length(CAST(operator_reason AS BLOB)) BETWEEN 1 AND 4096
      AND instr(operator_reason, char(0)) = 0
    )
  ),
  payload_hash TEXT NOT NULL CHECK (
    length(payload_hash) = 64
    AND payload_hash NOT GLOB '*[^0-9a-f]*'
  ),
  state TEXT NOT NULL CHECK (state IN ('prepared', 'submitted', 'confirmed', 'failed', 'ambiguous')),
  attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
  prepared_at_us INTEGER NOT NULL CHECK (prepared_at_us > 0),
  submitted_at_us INTEGER,
  completed_at_us INTEGER,
  response_message_ts TEXT CHECK (
    response_message_ts IS NULL OR (
      length(response_message_ts) BETWEEN 8 AND 32
      AND instr(response_message_ts, '.') > 1
      AND length(substr(response_message_ts, instr(response_message_ts, '.') + 1)) = 6
      AND substr(response_message_ts, 1, instr(response_message_ts, '.') - 1) NOT GLOB '*[^0-9]*'
      AND substr(response_message_ts, instr(response_message_ts, '.') + 1) NOT GLOB '*[^0-9]*'
    )
  ),
  response_file_id TEXT CHECK (
    response_file_id IS NULL OR (
      length(CAST(response_file_id AS BLOB)) BETWEEN 2 AND 128
      AND substr(response_file_id, 1, 1) = 'F'
      AND response_file_id NOT GLOB '*[^A-Z0-9]*'
    )
  ),
  diagnostic_code TEXT CHECK (
    diagnostic_code IS NULL OR (
      length(diagnostic_code) BETWEEN 1 AND 64
      AND diagnostic_code NOT GLOB '*[^a-z0-9_]*'
    )
  ),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  CHECK (predecessor_effect_id IS NULL OR predecessor_effect_id <> effect_id),
  CHECK (prepared_at_us BETWEEN created_at_us AND updated_at_us),
  CHECK (submitted_at_us IS NULL OR submitted_at_us BETWEEN prepared_at_us AND updated_at_us),
  CHECK (completed_at_us IS NULL OR (
    submitted_at_us IS NOT NULL
    AND completed_at_us BETWEEN submitted_at_us AND updated_at_us
  )),
  CHECK (
    (state = 'prepared' AND attempt_count = 0 AND submitted_at_us IS NULL AND completed_at_us IS NULL)
    OR (state = 'submitted' AND attempt_count = 1 AND submitted_at_us IS NOT NULL AND completed_at_us IS NULL)
    OR (state IN ('confirmed', 'failed', 'ambiguous') AND attempt_count = 1 AND submitted_at_us IS NOT NULL AND completed_at_us IS NOT NULL)
  )
) STRICT;

CREATE TABLE files (
  file_id TEXT PRIMARY KEY CHECK (
    length(file_id) = 26
    AND substr(file_id, 1, 4) = 'fil_'
    AND substr(file_id, 5) NOT GLOB '*[^A-Za-z0-9_-]*'
  ),
  direction TEXT NOT NULL CHECK (direction IN ('from_slack', 'to_slack')),
  slack_file_id TEXT CHECK (
    slack_file_id IS NULL OR (
      length(CAST(slack_file_id AS BLOB)) BETWEEN 2 AND 128
      AND substr(slack_file_id, 1, 1) = 'F'
      AND slack_file_id NOT GLOB '*[^A-Z0-9]*'
    )
  ),
  permalink TEXT CHECK (
    permalink IS NULL OR (
      length(CAST(permalink AS BLOB)) BETWEEN 1 AND 4096
      AND instr(permalink, char(0)) = 0
    )
  ),
  original_name TEXT NOT NULL CHECK (
    length(CAST(original_name AS BLOB)) BETWEEN 1 AND 1024
    AND instr(original_name, char(0)) = 0
  ),
  declared_mime TEXT CHECK (
    declared_mime IS NULL OR (
      length(CAST(declared_mime AS BLOB)) BETWEEN 1 AND 255
      AND instr(declared_mime, char(0)) = 0
    )
  ),
  size_bytes INTEGER CHECK (size_bytes IS NULL OR size_bytes >= 0),
  sha256 TEXT CHECK (
    sha256 IS NULL OR (
      length(sha256) = 64
      AND sha256 NOT GLOB '*[^0-9a-f]*'
    )
  ),
  related_task_id TEXT REFERENCES tasks(task_id) ON DELETE RESTRICT,
  related_effect_id TEXT REFERENCES slack_effects(effect_id) ON DELETE RESTRICT,
  transfer_state TEXT NOT NULL CHECK (transfer_state IN (
    'metadata_only', 'prepared', 'submitted', 'temporary',
    'confirmed', 'failed', 'ambiguous', 'expired'
  )),
  diagnostic_code TEXT CHECK (
    diagnostic_code IS NULL OR (
      length(diagnostic_code) BETWEEN 1 AND 64
      AND diagnostic_code NOT GLOB '*[^a-z0-9_]*'
    )
  ),
  temporary_path TEXT CHECK (
    temporary_path IS NULL OR (
      length(CAST(temporary_path AS BLOB)) BETWEEN 1 AND 4096
      AND instr(temporary_path, char(0)) = 0
    )
  ),
  created_at_us INTEGER NOT NULL CHECK (created_at_us > 0),
  updated_at_us INTEGER NOT NULL CHECK (updated_at_us >= created_at_us),
  CHECK (related_task_id IS NOT NULL OR related_effect_id IS NOT NULL),
  CHECK (direction <> 'from_slack' OR (slack_file_id IS NOT NULL AND related_task_id IS NOT NULL)),
  CHECK (temporary_path IS NULL OR transfer_state = 'temporary'),
  UNIQUE (slack_file_id, related_task_id)
) STRICT;

CREATE INDEX slack_task_sources_source_idx
  ON slack_task_sources(team_id, channel_id, message_ts);
CREATE INDEX slack_source_events_committed_result_idx
  ON slack_source_events(committed_at_us, admission_result);
CREATE INDEX slack_effects_state_updated_idx
  ON slack_effects(state, updated_at_us, effect_id);
CREATE INDEX slack_effects_predecessor_idx
  ON slack_effects(predecessor_effect_id, effect_id)
  WHERE predecessor_effect_id IS NOT NULL;
CREATE INDEX slack_effects_exceptional_idx
  ON slack_effects(updated_at_us, effect_id)
  WHERE state IN ('failed', 'ambiguous');
CREATE INDEX files_related_task_idx
  ON files(related_task_id, file_id)
  WHERE related_task_id IS NOT NULL;
CREATE INDEX files_related_effect_idx
  ON files(related_effect_id, file_id)
  WHERE related_effect_id IS NOT NULL;
