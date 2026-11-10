CREATE TABLE runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    repo TEXT NOT NULL CHECK (length(repo) > 0 AND repo = lower(trim(repo))),
    issue_number INTEGER NOT NULL CHECK (issue_number > 0),
    issue_title TEXT NOT NULL DEFAULT '',
    mode TEXT NOT NULL CHECK (mode IN ('fix','refine','investigate')),
    retry_of INTEGER REFERENCES runs(id),
    trigger TEXT NOT NULL CHECK (trigger IN ('cli','ui','webhook','poll')),
    provider TEXT NOT NULL,
    refine_model TEXT NOT NULL,
    code_model TEXT NOT NULL,
    dry_run INTEGER NOT NULL CHECK (dry_run IN (0,1)),
    status TEXT NOT NULL CHECK (status IN ('queued','running','needs_info','awaiting_approval','approved','succeeded','failed','cancelled','rejected')),
    phase TEXT NOT NULL CHECK (phase IN ('preflight','refine','clone','baseline','code','verify','publish')),
    outcome TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','pr_opened','pr_draft','investigated','spec_approved','rejected','expired','no_changes','dry_run','error','cancelled')),
    base_sha TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    pr_number INTEGER CHECK (pr_number > 0),
    pr_url TEXT NOT NULL DEFAULT '',
    pr_draft INTEGER NOT NULL DEFAULT 0 CHECK (pr_draft IN (0,1)),
    summary TEXT NOT NULL DEFAULT '',
    tests_status TEXT NOT NULL DEFAULT '' CHECK (tests_status IN ('','passed','failed','not_run')),
    test_attempts INTEGER NOT NULL DEFAULT 0 CHECK (test_attempts >= 0),
    turns INTEGER NOT NULL DEFAULT 0 CHECK (turns >= 0),
    input_tokens INTEGER CHECK (input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens >= 0),
    cache_read_tokens INTEGER CHECK (cache_read_tokens >= 0),
    cache_write_tokens INTEGER CHECK (cache_write_tokens >= 0),
    cost_micros INTEGER CHECK (cost_micros >= 0),
    limits_json TEXT NOT NULL CHECK (json_valid(limits_json)),
    error_kind TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    updated_at INTEGER NOT NULL CHECK (updated_at >= 0),
    started_at INTEGER CHECK (started_at >= 0),
    finished_at INTEGER CHECK (finished_at >= 0),
    heartbeat_at INTEGER CHECK (heartbeat_at >= 0),
    waiting_since INTEGER CHECK (waiting_since >= 0)
) STRICT;
CREATE UNIQUE INDEX runs_one_active_per_issue ON runs(repo, issue_number)
    WHERE status IN ('queued','running','needs_info','awaiting_approval','approved');
CREATE INDEX runs_created_at ON runs(created_at DESC, id DESC);
CREATE INDEX runs_status ON runs(status, created_at DESC, id DESC);
CREATE INDEX runs_repo_issue ON runs(repo, issue_number, created_at DESC, id DESC);

CREATE TABLE specs (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    body_md TEXT NOT NULL,
    hash TEXT NOT NULL CHECK (length(hash) = 64 AND hash NOT GLOB '*[^0-9a-f]*'),
    issue_hash TEXT NOT NULL DEFAULT '' CHECK (issue_hash = '' OR (length(issue_hash) = 64 AND issue_hash NOT GLOB '*[^0-9a-f]*')),
    source TEXT NOT NULL CHECK (source IN ('agent','ui_edit','revise')),
    notes TEXT NOT NULL DEFAULT '',
    comment_id INTEGER CHECK (comment_id > 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    PRIMARY KEY (run_id, version)
) STRICT;

CREATE TABLE approvals (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    run_id INTEGER NOT NULL,
    spec_version INTEGER NOT NULL CHECK (spec_version > 0),
    spec_hash TEXT NOT NULL CHECK (length(spec_hash) = 64 AND spec_hash NOT GLOB '*[^0-9a-f]*'),
    decision TEXT NOT NULL CHECK (decision IN ('approve','revise','reject')),
    actor TEXT NOT NULL CHECK (length(actor) > 0),
    channel TEXT NOT NULL CHECK (channel IN ('ui','cli','github')),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    FOREIGN KEY (run_id, spec_version) REFERENCES specs(run_id, version) ON DELETE CASCADE
) STRICT;
CREATE INDEX approvals_spec ON approvals(run_id, spec_version, id DESC);

CREATE TABLE questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    body TEXT NOT NULL CHECK (length(body) > 0),
    answer TEXT,
    answered_by TEXT,
    answer_channel TEXT CHECK (answer_channel IN ('ui','cli','github')),
    comment_id INTEGER CHECK (comment_id > 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    answered_at INTEGER CHECK (answered_at >= 0),
    CHECK ((answer IS NULL AND answered_by IS NULL AND answer_channel IS NULL AND answered_at IS NULL)
        OR (answer IS NOT NULL AND answered_by IS NOT NULL AND answer_channel IS NOT NULL AND answered_at IS NOT NULL))
) STRICT;
CREATE INDEX questions_run ON questions(run_id, id);

CREATE TABLE steps (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    phase TEXT NOT NULL CHECK (phase IN ('preflight','refine','clone','baseline','code','verify','publish')),
    kind TEXT NOT NULL CHECK (kind IN ('llm','tool','test','git','publish')),
    tool TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('running','ok','error')),
    turn INTEGER CHECK (turn >= 0),
    input_json TEXT NOT NULL DEFAULT '',
    output_text TEXT NOT NULL DEFAULT '',
    stop_reason TEXT NOT NULL DEFAULT '',
    input_tokens INTEGER CHECK (input_tokens >= 0),
    output_tokens INTEGER CHECK (output_tokens >= 0),
    cache_read_tokens INTEGER CHECK (cache_read_tokens >= 0),
    cache_write_tokens INTEGER CHECK (cache_write_tokens >= 0),
    cost_micros INTEGER CHECK (cost_micros >= 0),
    exit_code INTEGER,
    duration_ms INTEGER CHECK (duration_ms >= 0),
    started_at INTEGER NOT NULL CHECK (started_at >= 0),
    finished_at INTEGER CHECK (finished_at >= 0),
    PRIMARY KEY (run_id, seq)
) STRICT;
CREATE INDEX steps_run_kind ON steps(run_id, kind);

CREATE TABLE run_events (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    type TEXT NOT NULL CHECK (length(type) > 0),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    PRIMARY KEY (run_id, seq)
) STRICT, WITHOUT ROWID;

CREATE TABLE run_artifacts (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (length(kind) > 0),
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    content TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    PRIMARY KEY (run_id, kind, attempt)
) STRICT;

CREATE TABLE actions (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('start','edit_spec','approve','revise','reject','cancel','retry','answer')),
    actor TEXT NOT NULL CHECK (length(actor) > 0),
    channel TEXT NOT NULL CHECK (channel IN ('ui','cli','github')),
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
    created_at INTEGER NOT NULL CHECK (created_at >= 0)
) STRICT;
CREATE INDEX actions_run ON actions(run_id, id);
