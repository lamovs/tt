CREATE TABLE timer_control (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL
);
INSERT INTO timer_control VALUES (1, 0);
ALTER TABLE focus_uploads ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
