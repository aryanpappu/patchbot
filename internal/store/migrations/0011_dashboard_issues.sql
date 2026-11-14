-- Issues created from the dashboard (M26). One row per client idempotency key.
-- A row is inserted as 'pending' BEFORE GitHub is called, so a crash between
-- the API call and the commit leaves evidence; a retry searches GitHub for the
-- hidden marker (which embeds the key) before it may create another issue.
-- actor is the dashboard user who asked (the GitHub author is the bot or PAT
-- user). No foreign key on run_id: the record must outlive run cleanup.
CREATE TABLE dashboard_issues (
    id INTEGER PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 8 AND 128),
    request_hash TEXT NOT NULL CHECK (length(request_hash) = 64),
    repo TEXT NOT NULL CHECK (length(repo) > 0),
    actor TEXT NOT NULL CHECK (length(actor) > 0),
    state TEXT NOT NULL CHECK (state IN ('pending', 'created')),
    issue_number INTEGER CHECK (issue_number IS NULL OR issue_number > 0),
    issue_node_id TEXT NOT NULL DEFAULT '',
    issue_url TEXT NOT NULL DEFAULT '',
    run_id INTEGER CHECK (run_id IS NULL OR run_id > 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    updated_at INTEGER NOT NULL CHECK (updated_at >= 0),
    CHECK ((state = 'pending') = (issue_number IS NULL))
) STRICT;
CREATE UNIQUE INDEX dashboard_issues_issue ON dashboard_issues(repo, issue_number) WHERE issue_number IS NOT NULL;
