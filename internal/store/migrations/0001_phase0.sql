CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY CHECK (version > 0),
  checksum TEXT NOT NULL CHECK (length(checksum) = 64),
  applied_at INTEGER NOT NULL CHECK (applied_at > 0)
) STRICT;

CREATE TABLE tasks (
  task_id TEXT PRIMARY KEY,
  version INTEGER NOT NULL CHECK (version >= 1),
  state TEXT NOT NULL CHECK (state IN ('open','working','done')),
  text TEXT NOT NULL CHECK (length(text) > 0 AND instr(text, char(0)) = 0),
  source_kind TEXT NOT NULL CHECK (source_kind IN ('local','slack')),
  created_at INTEGER NOT NULL CHECK (created_at > 0),
  updated_at INTEGER NOT NULL CHECK (updated_at >= created_at),
  done_at INTEGER,
  CHECK ((state = 'done' AND done_at IS NOT NULL) OR (state <> 'done' AND done_at IS NULL))
) STRICT;

CREATE TABLE task_state_events (
  event_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(task_id) ON DELETE RESTRICT,
  prior_state TEXT NOT NULL CHECK (prior_state IN ('open','working','done')),
  new_state TEXT NOT NULL CHECK (new_state IN ('open','working','done')),
  actor TEXT NOT NULL CHECK (length(actor) BETWEEN 1 AND 128),
  note TEXT CHECK (note IS NULL OR length(note) <= 4096),
  request_id TEXT NOT NULL UNIQUE,
  committed_at INTEGER NOT NULL CHECK (committed_at > 0),
  CHECK (prior_state <> new_state)
) STRICT;

CREATE TABLE mutation_requests (
  request_id TEXT PRIMARY KEY,
  operation TEXT NOT NULL,
  input_hash TEXT NOT NULL CHECK (length(input_hash) = 64),
  outcome TEXT NOT NULL CHECK (outcome IN ('success','error')),
  result_type TEXT,
  result_json TEXT,
  error_code TEXT,
  error_message TEXT,
  created_at INTEGER NOT NULL CHECK (created_at > 0),
  completed_at INTEGER NOT NULL CHECK (completed_at >= created_at),
  expiry_eligible_at INTEGER NOT NULL CHECK (expiry_eligible_at >= completed_at),
  CHECK ((outcome='success' AND result_type IS NOT NULL AND result_json IS NOT NULL AND error_code IS NULL AND error_message IS NULL) OR (outcome='error' AND result_type IS NULL AND result_json IS NULL AND error_code IS NOT NULL AND error_message IS NOT NULL))
) STRICT;

CREATE INDEX tasks_state_updated_idx ON tasks(state, updated_at, task_id);
CREATE INDEX task_state_events_task_committed_idx ON task_state_events(task_id, committed_at, event_id);
CREATE INDEX mutation_requests_expiry_idx ON mutation_requests(expiry_eligible_at);
