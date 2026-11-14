-- Dashboard-managed repositories (M24). The config file stays the bootstrap
-- allowlist and is never written; these rows are added to it. settings_json is
-- the redacted repository settings (commands argv map, test_command, protected
-- paths, optional digest-pinned image and container PATH), validated by the
-- same rules as a [repos."owner/name"] table before it is stored. Edits bump
-- version (optimistic concurrency). History stays when a row is removed.
CREATE TABLE dashboard_repos (
    name TEXT PRIMARY KEY CHECK (length(name) BETWEEN 3 AND 140 AND name = lower(name)),
    settings_json TEXT NOT NULL CHECK (json_valid(settings_json)),
    version INTEGER NOT NULL CHECK (version >= 1),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    created_by TEXT NOT NULL,
    updated_at INTEGER NOT NULL CHECK (updated_at >= 0),
    updated_by TEXT NOT NULL
) STRICT;

-- Append-only audit of every add, edit and remove with the acting identity.
-- before_json and after_json are redacted settings snapshots ('' when absent).
CREATE TABLE repo_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('add', 'edit', 'remove')),
    actor TEXT NOT NULL,
    channel TEXT NOT NULL,
    before_json TEXT NOT NULL DEFAULT '',
    after_json TEXT NOT NULL DEFAULT '',
    at INTEGER NOT NULL CHECK (at >= 0)
) STRICT;

CREATE INDEX repo_audit_repo ON repo_audit (repo, id);

CREATE TRIGGER repo_audit_no_update BEFORE UPDATE ON repo_audit
BEGIN SELECT RAISE(ABORT, 'repo_audit is append-only'); END;

CREATE TRIGGER repo_audit_no_delete BEFORE DELETE ON repo_audit
BEGIN SELECT RAISE(ABORT, 'repo_audit is append-only'); END;
