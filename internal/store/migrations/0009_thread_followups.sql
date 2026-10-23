-- A follow-up is a task admitted from an unmentioned reply in a tracked
-- thread. parent_task_id links it to the earliest task in that thread. It is
-- deliberately not a foreign key: it is provenance only, and purging the
-- parent must not be blocked by, or cascade into, retained follow-ups.
ALTER TABLE slack_task_sources ADD COLUMN parent_task_id TEXT CHECK (
  parent_task_id IS NULL OR (
    length(CAST(parent_task_id AS BLOB)) BETWEEN 1 AND 128
    AND parent_task_id NOT GLOB '*[^A-Za-z0-9_-]*'
    AND parent_task_id <> task_id
  )
);

CREATE INDEX slack_task_sources_thread_idx
  ON slack_task_sources(team_id, channel_id, thread_ts, created_at_us, task_id);
CREATE INDEX slack_effects_thread_idx
  ON slack_effects(team_id, channel_id, thread_ts)
  WHERE thread_ts IS NOT NULL;
CREATE INDEX slack_effects_response_idx
  ON slack_effects(team_id, channel_id, response_message_ts)
  WHERE response_message_ts IS NOT NULL;
