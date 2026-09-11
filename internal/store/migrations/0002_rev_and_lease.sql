





DROP INDEX idx_tasks_dirty;
CREATE INDEX idx_tasks_dirty ON tasks(dirty) WHERE dirty <> 0;




ALTER TABLE outbox ADD COLUMN rev INTEGER;













UPDATE outbox SET rev = (SELECT dirty FROM tasks t WHERE t.id = outbox.task_id)
WHERE task_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = outbox.task_id AND t.dirty <> 0);






ALTER TABLE outbox ADD COLUMN lease_token TEXT;
