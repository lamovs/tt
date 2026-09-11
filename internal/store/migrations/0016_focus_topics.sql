CREATE TABLE focus_topics (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL,
    type                   TEXT NOT NULL DEFAULT '',
    status                 INTEGER NOT NULL DEFAULT 0,
    sort_order             INTEGER NOT NULL DEFAULT 0,
    pomodoro_time          INTEGER NOT NULL DEFAULT 0,
    raw                    TEXT NOT NULL,
    credential_fingerprint TEXT NOT NULL,
    refreshed_at           INTEGER NOT NULL
);

CREATE INDEX idx_focus_topics_name ON focus_topics(name COLLATE NOCASE, id);

ALTER TABLE focus_sessions ADD COLUMN topic_id TEXT NOT NULL DEFAULT '';
ALTER TABLE focus_sessions ADD COLUMN topic_name TEXT NOT NULL DEFAULT '';
ALTER TABLE focus_sessions ADD COLUMN topic_credential TEXT NOT NULL DEFAULT '';

ALTER TABLE timer_state ADD COLUMN topic_id TEXT NOT NULL DEFAULT '';
ALTER TABLE timer_state ADD COLUMN topic_name TEXT NOT NULL DEFAULT '';
ALTER TABLE timer_state ADD COLUMN topic_credential TEXT NOT NULL DEFAULT '';
