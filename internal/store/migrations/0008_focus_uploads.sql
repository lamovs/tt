CREATE TABLE focus_uploads (
    session_id TEXT PRIMARY KEY REFERENCES focus_sessions(id),
    phase TEXT NOT NULL CHECK (phase IN ('armed', 'accepted', 'rejected', 'confirmed')),
    request TEXT NOT NULL,
    prior_ids TEXT NOT NULL,
    remote_id TEXT NOT NULL DEFAULT '',
    response TEXT NOT NULL DEFAULT '',
    confirmation TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT ''
);
