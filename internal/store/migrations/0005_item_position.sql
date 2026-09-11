











ALTER TABLE items ADD COLUMN position INTEGER NOT NULL DEFAULT 0;








UPDATE items SET position = (
    SELECT count(*) FROM items earlier
    WHERE earlier.task_id = items.task_id AND earlier.rowid < items.rowid
);
