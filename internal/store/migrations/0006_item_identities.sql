

CREATE TABLE item_identities (
    task_id   TEXT NOT NULL REFERENCES tasks(id) ON UPDATE CASCADE ON DELETE CASCADE,
    item_key  TEXT NOT NULL,
    server_id TEXT,
    state     TEXT NOT NULL,
    PRIMARY KEY (task_id, item_key),
    CHECK (
        (
            item_key = '' AND
            server_id IS NULL AND
            state IN ('recovery_idle', 'recovery_pending')
        ) OR (
            item_key <> '' AND (
                (state = 'unbound' AND server_id IS NULL) OR
                (
                    state = 'bound' AND
                    server_id IS NOT NULL AND
                    server_id <> '' AND
                    substr(server_id, 1, 6) <> 'local-'
                ) OR (
                    state IN ('uncertain', 'abandoned') AND
                    (
                        server_id IS NULL OR (
                            server_id <> '' AND
                            substr(server_id, 1, 6) <> 'local-'
                        )
                    )
                )
            )
        )
    )
);

CREATE UNIQUE INDEX idx_item_identities_active_server
ON item_identities(task_id, server_id)
WHERE item_key <> ''
  AND state IN ('bound', 'uncertain')
  AND server_id IS NOT NULL;
