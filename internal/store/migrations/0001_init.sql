


















CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT    NOT NULL DEFAULT '',
    kind       TEXT    NOT NULL DEFAULT '',
    sort_order INTEGER NOT NULL DEFAULT 0,
    closed     INTEGER NOT NULL DEFAULT 0,
    search     TEXT    NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE tasks (
    id             TEXT PRIMARY KEY,
    project_id     TEXT    NOT NULL DEFAULT '',
    title          TEXT    NOT NULL DEFAULT '',
    content        TEXT    NOT NULL DEFAULT '',
    status         INTEGER NOT NULL DEFAULT 0,
    priority       INTEGER NOT NULL DEFAULT 0,
    due_date       TEXT,
    start_date     TEXT,
    is_all_day     INTEGER NOT NULL DEFAULT 0,
    time_zone      TEXT    NOT NULL DEFAULT '',
    repeat_flag    TEXT    NOT NULL DEFAULT '',
    reminders      TEXT    NOT NULL DEFAULT '[]',
    tags           TEXT    NOT NULL DEFAULT '[]',
    kind           TEXT    NOT NULL DEFAULT '',
    sort_order     INTEGER NOT NULL DEFAULT 0,
    created_time   TEXT,
    modified_time  TEXT,
    completed_time TEXT,

    search         TEXT    NOT NULL DEFAULT '',
    raw            TEXT    NOT NULL DEFAULT '{}',
    local          INTEGER NOT NULL DEFAULT 0,
    dirty          INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_status  ON tasks(status);
CREATE INDEX idx_tasks_due     ON tasks(due_date);





CREATE INDEX idx_tasks_dirty ON tasks(dirty) WHERE dirty = 1;



CREATE TABLE items (
    task_id        TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE ON UPDATE CASCADE,
    id             TEXT    NOT NULL,
    title          TEXT    NOT NULL DEFAULT '',
    status         INTEGER NOT NULL DEFAULT 0,
    sort_order     INTEGER NOT NULL DEFAULT 0,
    start_date     TEXT,
    is_all_day     INTEGER NOT NULL DEFAULT 0,
    time_zone      TEXT    NOT NULL DEFAULT '',
    completed_time TEXT,
    PRIMARY KEY (task_id, id)
);

CREATE TABLE outbox (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    target      TEXT    NOT NULL CHECK (target IN ('openapi', 'v2')),
    op          TEXT    NOT NULL,
    task_id     TEXT,
    project_id  TEXT,
    payload     TEXT    NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'inflight', 'failed')),


    inflight_at INTEGER
);

CREATE INDEX idx_outbox_state_seq ON outbox(state, seq);

CREATE TABLE focus_sessions (
    id          TEXT PRIMARY KEY,
    task_id     TEXT,
    kind        TEXT    NOT NULL CHECK (kind IN ('focus', 'short_break', 'long_break')),
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    planned_sec INTEGER NOT NULL DEFAULT 0,
    pause_sec   INTEGER NOT NULL DEFAULT 0,
    outcome     TEXT CHECK (outcome IS NULL OR outcome IN ('done', 'aborted')),
    note        TEXT    NOT NULL DEFAULT '',
    synced_at   INTEGER
);

CREATE INDEX idx_focus_started ON focus_sessions(started_at);


CREATE TABLE timer_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    kind        TEXT    NOT NULL CHECK (kind IN ('focus', 'short_break', 'long_break')),
    task_id     TEXT,
    started_at  INTEGER NOT NULL,
    planned_sec INTEGER NOT NULL,
    paused_at   INTEGER,
    pause_sec   INTEGER NOT NULL DEFAULT 0,
    cycle       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE events (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    at      INTEGER NOT NULL,
    kind    TEXT    NOT NULL,
    payload TEXT    NOT NULL DEFAULT '{}'
);


CREATE TABLE listing (
    pos        INTEGER PRIMARY KEY,
    task_id    TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE undo_log (
    seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    at      INTEGER NOT NULL,
    kind    TEXT    NOT NULL,
    payload TEXT    NOT NULL DEFAULT '{}'
);
