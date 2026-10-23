CREATE TABLE task_change_sequence (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  last_seq INTEGER NOT NULL CHECK (last_seq >= 0)
) STRICT;

ALTER TABLE tasks ADD COLUMN change_seq INTEGER NOT NULL DEFAULT 0 CHECK (change_seq >= 0);
ALTER TABLE tasks ADD COLUMN change_kind TEXT NOT NULL DEFAULT 'created' CHECK (change_kind IN ('created','edited','state'));

UPDATE tasks SET
  change_seq = ranked.seq,
  change_kind = CASE WHEN tasks.version > 1 THEN 'edited' WHEN tasks.state <> 'open' THEN 'state' ELSE 'created' END
FROM (SELECT task_id, row_number() OVER (ORDER BY updated_at, task_id) AS seq FROM tasks) AS ranked
WHERE ranked.task_id = tasks.task_id;

INSERT INTO task_change_sequence(singleton, last_seq) VALUES(1, (SELECT count(*) FROM tasks));

CREATE UNIQUE INDEX tasks_change_seq_idx ON tasks(change_seq);

CREATE TRIGGER tasks_change_after_insert AFTER INSERT ON tasks
BEGIN
  UPDATE task_change_sequence SET last_seq = last_seq + 1 WHERE singleton = 1;
  UPDATE tasks SET change_seq = (SELECT last_seq FROM task_change_sequence WHERE singleton = 1), change_kind = 'created' WHERE task_id = NEW.task_id;
END;

CREATE TRIGGER tasks_change_after_update AFTER UPDATE OF version, state ON tasks
WHEN NEW.version <> OLD.version OR NEW.state <> OLD.state
BEGIN
  UPDATE task_change_sequence SET last_seq = last_seq + 1 WHERE singleton = 1;
  UPDATE tasks SET change_seq = (SELECT last_seq FROM task_change_sequence WHERE singleton = 1), change_kind = CASE WHEN NEW.version <> OLD.version THEN 'edited' ELSE 'state' END WHERE task_id = NEW.task_id;
END;
