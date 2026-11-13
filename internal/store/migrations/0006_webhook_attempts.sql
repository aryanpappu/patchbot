ALTER TABLE webhook_deliveries ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0);
ALTER TABLE webhook_deliveries ADD COLUMN processed_at INTEGER;
UPDATE webhook_deliveries SET processed_at = CAST(strftime('%s','now') AS INTEGER) * 1000 WHERE processed = 1;
CREATE INDEX webhook_deliveries_pending ON webhook_deliveries(processed, processed_at);

ALTER TABLE webhook_labels ADD COLUMN handled INTEGER NOT NULL DEFAULT 0 CHECK(handled IN (0,1));

CREATE TABLE webhook_repos (
 repo TEXT PRIMARY KEY,
 baselined_at INTEGER NOT NULL
) STRICT;
INSERT INTO webhook_repos(repo,baselined_at) SELECT DISTINCT repo, CAST(strftime('%s','now') AS INTEGER) * 1000 FROM webhook_labels;
