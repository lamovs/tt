ALTER TABLE timer_state ADD COLUMN indicator_mode INTEGER NOT NULL DEFAULT 0
    CHECK (indicator_mode IN (0, 1, 2));
