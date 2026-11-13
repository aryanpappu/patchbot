CREATE TABLE webhook_deliveries (
 delivery_id TEXT PRIMARY KEY,
 event TEXT NOT NULL,
 repo TEXT NOT NULL,
 issue INTEGER NOT NULL,
 processed INTEGER NOT NULL DEFAULT 0 CHECK(processed IN (0,1))
) STRICT;

ALTER TABLE runs ADD COLUMN ingress_key TEXT;
CREATE UNIQUE INDEX runs_ingress_key ON runs(ingress_key) WHERE ingress_key IS NOT NULL;

CREATE TABLE webhook_labels (
 repo TEXT NOT NULL,
 issue INTEGER NOT NULL,
 label TEXT NOT NULL,
 generation INTEGER NOT NULL,
 PRIMARY KEY(repo,issue)
) STRICT;
