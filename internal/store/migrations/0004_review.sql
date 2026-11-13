-- patchbot:foreign_keys=off
-- M21 opt-in reviewer. SQLite cannot alter a CHECK constraint in place, so the
-- runs and steps tables are rebuilt with the 'review' phase (the documented
-- twelve-step procedure; the migrator disables foreign keys and runs
-- foreign_key_check before commit). runs gains the review selection frozen at
-- approval; reviews records every verdict bound to spec and diff hashes.
CREATE TABLE runs_new (
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
    phase TEXT NOT NULL CHECK (phase IN ('preflight','refine','clone','baseline','code','verify','review','publish')),
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
    waiting_since INTEGER CHECK (waiting_since >= 0),
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    review_json TEXT NOT NULL DEFAULT '' CHECK (review_json = '' OR json_valid(review_json))
) STRICT;
INSERT INTO runs_new (id,repo,issue_number,issue_title,mode,retry_of,trigger,provider,refine_model,code_model,
    dry_run,status,phase,outcome,base_sha,head_sha,branch,pr_number,pr_url,pr_draft,summary,tests_status,
    test_attempts,turns,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,limits_json,
    error_kind,error_message,created_at,updated_at,started_at,finished_at,heartbeat_at,waiting_since,generation)
SELECT id,repo,issue_number,issue_title,mode,retry_of,trigger,provider,refine_model,code_model,
    dry_run,status,phase,outcome,base_sha,head_sha,branch,pr_number,pr_url,pr_draft,summary,tests_status,
    test_attempts,turns,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,limits_json,
    error_kind,error_message,created_at,updated_at,started_at,finished_at,heartbeat_at,waiting_since,generation
FROM runs;
-- Keep AUTOINCREMENT monotonic: run IDs name branches and must never be reused.
CREATE TEMP TABLE m21_runs_seq AS SELECT COALESCE(MAX(seq), 0) AS seq FROM sqlite_sequence WHERE name = 'runs';
DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;
DELETE FROM sqlite_sequence WHERE name = 'runs';
INSERT INTO sqlite_sequence(name, seq) SELECT 'runs', MAX((SELECT seq FROM m21_runs_seq), COALESCE((SELECT MAX(id) FROM runs), 0));
DROP TABLE temp.m21_runs_seq;
CREATE UNIQUE INDEX runs_one_active_per_issue ON runs(repo, issue_number)
    WHERE status IN ('queued','running','needs_info','awaiting_approval','approved');
CREATE INDEX runs_created_at ON runs(created_at DESC, id DESC);
CREATE INDEX runs_status ON runs(status, created_at DESC, id DESC);
CREATE INDEX runs_repo_issue ON runs(repo, issue_number, created_at DESC, id DESC);

CREATE TABLE steps_new (
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq INTEGER NOT NULL CHECK (seq > 0),
    phase TEXT NOT NULL CHECK (phase IN ('preflight','refine','clone','baseline','code','verify','review','publish')),
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
INSERT INTO steps_new SELECT run_id,seq,phase,kind,tool,status,turn,input_json,output_text,stop_reason,
    input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_micros,exit_code,duration_ms,
    started_at,finished_at FROM steps;
DROP TABLE steps;
ALTER TABLE steps_new RENAME TO steps;
CREATE INDEX steps_run_kind ON steps(run_id, kind);

CREATE TABLE reviews (
    id INTEGER PRIMARY KEY AUTOINCREMENT CHECK (id > 0),
    run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round > 0),
    spec_version INTEGER NOT NULL CHECK (spec_version > 0),
    spec_hash TEXT NOT NULL CHECK (length(spec_hash) = 64 AND spec_hash NOT GLOB '*[^0-9a-f]*'),
    diff_hash TEXT NOT NULL CHECK (length(diff_hash) = 64 AND diff_hash NOT GLOB '*[^0-9a-f]*'),
    base_sha TEXT NOT NULL DEFAULT '',
    tree_id TEXT NOT NULL DEFAULT '',
    verdict TEXT NOT NULL CHECK (verdict IN ('approve','request_changes','incomplete')),
    reason TEXT NOT NULL DEFAULT '',
    findings_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(findings_json)),
    criteria_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(criteria_json)),
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    UNIQUE (run_id, round)
) STRICT;
CREATE INDEX reviews_run ON reviews(run_id, round);
