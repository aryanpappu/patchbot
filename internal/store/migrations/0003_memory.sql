CREATE TABLE memory_entries (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    repo TEXT NOT NULL CHECK (length(repo) > 0 AND repo = lower(trim(repo))),
    kind TEXT NOT NULL CHECK (kind IN ('observation','lesson')),
    status TEXT NOT NULL CHECK (status IN ('active','pending','approved','rejected','revoked')),
    source TEXT NOT NULL CHECK (source IN ('harness','model','operator')),
    run_id INTEGER REFERENCES runs(id) ON DELETE SET NULL,
    command_id TEXT NOT NULL DEFAULT '',
    exit_code INTEGER,
    timed_out INTEGER CHECK (timed_out IN (0,1)),
    base_sha TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    policy_digest TEXT NOT NULL DEFAULT '',
    evidence_ref TEXT NOT NULL DEFAULT '',
    observed_at INTEGER CHECK (observed_at >= 0),
    text TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL CHECK (length(content_hash) = 64 AND content_hash NOT GLOB '*[^0-9a-f]*'),
    created_by TEXT NOT NULL CHECK (length(created_by) > 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    decided_by TEXT,
    decided_at INTEGER CHECK (decided_at >= 0),
    CHECK ((decided_by IS NULL) = (decided_at IS NULL)),
    -- Observations are typed harness records only. They never carry prose.
    CHECK ((kind = 'observation' AND source = 'harness' AND status IN ('active','revoked')
            AND length(command_id) > 0 AND exit_code IS NOT NULL AND timed_out IS NOT NULL
            AND length(policy_digest) = 64 AND policy_digest NOT GLOB '*[^0-9a-f]*'
            AND observed_at IS NOT NULL AND text = '')
        OR (kind = 'lesson' AND source IN ('model','operator') AND status IN ('pending','approved','rejected','revoked')
            AND command_id = '' AND exit_code IS NULL AND timed_out IS NULL AND observed_at IS NULL
            AND (length(text) > 0 OR status = 'revoked')))
) STRICT;
CREATE INDEX memory_retrieval ON memory_entries(repo, kind, status, id DESC);

CREATE TABLE memory_actions (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    entry_id INTEGER NOT NULL REFERENCES memory_entries(id) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('create','approve','reject','forget')),
    actor TEXT NOT NULL CHECK (length(actor) > 0),
    channel TEXT NOT NULL CHECK (channel IN ('ui','cli','harness')),
    created_at INTEGER NOT NULL CHECK (created_at >= 0)
) STRICT;
CREATE INDEX memory_actions_entry ON memory_actions(entry_id, id);
