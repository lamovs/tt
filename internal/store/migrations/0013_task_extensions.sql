ALTER TABLE tasks ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN child_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE tasks ADD COLUMN column_id TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN column_name TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN estimated_duration INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN estimated_pomo INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN focus_summaries TEXT NOT NULL DEFAULT 'null';
UPDATE tasks SET
    parent_id = CASE WHEN json_type(raw, '$.parentId') = 'text' THEN json_extract(raw, '$.parentId') ELSE '' END,
    child_ids = CASE WHEN json_type(raw, '$.childIds') = 'array' THEN json_extract(raw, '$.childIds') ELSE '[]' END,
    column_id = CASE WHEN json_type(raw, '$.columnId') = 'text' THEN json_extract(raw, '$.columnId') ELSE '' END,
    column_name = CASE WHEN json_type(raw, '$.columnName') = 'text' THEN json_extract(raw, '$.columnName') ELSE '' END,
    estimated_duration = CASE WHEN json_array_length(raw, '$.focusSummaries') = 1 AND json_type(raw, '$.focusSummaries[0].estimatedDuration') = 'integer' THEN json_extract(raw, '$.focusSummaries[0].estimatedDuration') ELSE 0 END,
    estimated_pomo = CASE WHEN json_array_length(raw, '$.focusSummaries') = 1 AND json_type(raw, '$.focusSummaries[0].estimatedPomo') = 'integer' THEN json_extract(raw, '$.focusSummaries[0].estimatedPomo') ELSE 0 END,
    focus_summaries = CASE WHEN json_type(raw, '$.focusSummaries') IN ('array', 'object') THEN json_extract(raw, '$.focusSummaries') ELSE 'null' END
WHERE json_valid(raw);
CREATE INDEX idx_tasks_parent ON tasks(parent_id) WHERE parent_id <> '';
CREATE INDEX idx_tasks_column ON tasks(project_id, column_id, sort_order);
ALTER TABLE projects ADD COLUMN color TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN group_id TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN view_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN permission TEXT NOT NULL DEFAULT '';
