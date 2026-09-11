ALTER TABLE focus_sessions ADD COLUMN note_review_intent INTEGER NOT NULL DEFAULT 0 CHECK (note_review_intent IN (0, 1));
ALTER TABLE focus_sessions ADD COLUMN note_review_pending INTEGER NOT NULL DEFAULT 0 CHECK (note_review_pending IN (0, 1));
