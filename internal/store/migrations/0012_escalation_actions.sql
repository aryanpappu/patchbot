-- patchbot:foreign_keys=off
-- M30 support-level escalation. The audit log records 'escalated' on the run
-- that ended and 'escalation' on the run started one level up, by patchbot
-- itself on the 'escalation' channel or by a person from the dashboard.
-- SQLite cannot alter a CHECK constraint in place, so actions is rebuilt.
CREATE TABLE actions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('start','edit_spec','approve','revise','reject','cancel','retry','answer','escalated','escalation')),
    actor TEXT NOT NULL CHECK (length(actor) > 0),
    channel TEXT NOT NULL CHECK (channel IN ('ui','cli','github','escalation')),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at INTEGER NOT NULL CHECK (created_at >= 0)
) STRICT;
INSERT INTO actions_new(id, run_id, kind, actor, channel, payload_json, created_at)
    SELECT id, run_id, kind, actor, channel, payload_json, created_at FROM actions;
DROP TABLE actions;
ALTER TABLE actions_new RENAME TO actions;
CREATE INDEX actions_run ON actions(run_id, id);
