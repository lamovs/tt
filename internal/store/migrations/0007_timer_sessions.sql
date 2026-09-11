
ALTER TABLE timer_state ADD COLUMN session_id TEXT;
ALTER TABLE timer_state ADD COLUMN focus_type INTEGER CHECK (focus_type IN (0, 1));
ALTER TABLE timer_state ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE timer_state ADD COLUMN time_precision INTEGER NOT NULL DEFAULT 0 CHECK (time_precision IN (0, 3));
ALTER TABLE timer_state ADD COLUMN planned_ms INTEGER;
ALTER TABLE timer_state ADD COLUMN pause_ms INTEGER;
ALTER TABLE timer_state ADD COLUMN last_event_at INTEGER;

ALTER TABLE focus_sessions ADD COLUMN focus_type INTEGER CHECK (focus_type IN (0, 1));
ALTER TABLE focus_sessions ADD COLUMN time_precision INTEGER NOT NULL DEFAULT 0 CHECK (time_precision IN (0, 3));
ALTER TABLE focus_sessions ADD COLUMN planned_ms INTEGER;
ALTER TABLE focus_sessions ADD COLUMN pause_ms INTEGER;
ALTER TABLE focus_sessions ADD COLUMN notification_claimed INTEGER NOT NULL DEFAULT 0 CHECK (notification_claimed IN (0, 1));
