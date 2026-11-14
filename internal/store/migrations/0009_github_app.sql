-- The GitHub App created by the "Connect GitHub" manifest flow (M28). A single
-- row: the app ID, its slug and the installation ID (0 until resolved). The
-- private key is never stored here; it lives in the 0600 key file. The
-- manifest conversion also returns a client secret and a webhook secret; both
-- are discarded and have no column.
CREATE TABLE github_app (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    app_id INTEGER NOT NULL CHECK (app_id > 0),
    slug TEXT NOT NULL CHECK (length(slug) > 0 AND length(slug) <= 100),
    installation_id INTEGER NOT NULL DEFAULT 0 CHECK (installation_id >= 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0)
) STRICT;
