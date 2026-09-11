CREATE TABLE reminder_deliveries (
    task_id    TEXT    NOT NULL,
    due_date   TEXT    NOT NULL,
    trigger    TEXT    NOT NULL,
    decision   TEXT    NOT NULL CHECK (decision IN ('provider', 'local')),
    claimed_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, due_date, trigger)
);

CREATE INDEX idx_reminder_deliveries_claimed ON reminder_deliveries(claimed_at);
