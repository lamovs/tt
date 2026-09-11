CREATE TABLE focus_record_imports (
    preview_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL UNIQUE REFERENCES focus_sessions(id)
);

CREATE TABLE focus_remote_deletions (
    remote_id TEXT NOT NULL,
    focus_type INTEGER NOT NULL CHECK (focus_type IN (0, 1)),
    confirmed_at INTEGER NOT NULL,
    PRIMARY KEY (remote_id, focus_type)
);
