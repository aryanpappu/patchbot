-- Every GitHub comment patchbot posts, recorded right after a successful post.
-- With a shared identity (the token owner is also an approver), comment sync
-- refuses any comment whose ID is listed here. No foreign key: the record must
-- outlive the run so a later run never mistakes it for an owner command.
CREATE TABLE posted_comments (
    comment_id INTEGER PRIMARY KEY CHECK (comment_id > 0),
    run_id INTEGER NOT NULL CHECK (run_id > 0),
    repo TEXT NOT NULL CHECK (length(repo) > 0),
    issue_number INTEGER NOT NULL CHECK (issue_number > 0),
    kind TEXT NOT NULL CHECK (length(kind) > 0),
    created_at INTEGER NOT NULL CHECK (created_at >= 0)
) STRICT;
