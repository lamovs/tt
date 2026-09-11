CREATE TABLE resource_entities (
    kind TEXT NOT NULL,
    entity_key TEXT NOT NULL,
    server_id TEXT NOT NULL DEFAULT '',
    project_key TEXT NOT NULL DEFAULT '',
    data TEXT NOT NULL DEFAULT '{}',
    base TEXT NOT NULL DEFAULT '{}',
    revision INTEGER NOT NULL DEFAULT 1,
    dirty INTEGER NOT NULL DEFAULT 0,
    deleted INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, entity_key)
);

CREATE UNIQUE INDEX idx_resource_server_id ON resource_entities(kind, server_id)
    WHERE server_id <> '';
CREATE INDEX idx_resource_project ON resource_entities(kind, project_key);

CREATE TABLE entity_operation_targets (
    seq INTEGER PRIMARY KEY REFERENCES outbox(seq) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    entity_key TEXT NOT NULL
);
CREATE INDEX idx_entity_operation_target ON entity_operation_targets(kind, entity_key, seq);

CREATE TABLE entity_operation_dependencies (
    seq INTEGER NOT NULL REFERENCES outbox(seq) ON DELETE CASCADE,
    depends_on INTEGER NOT NULL REFERENCES outbox(seq) ON DELETE CASCADE,
    PRIMARY KEY (seq, depends_on),
    CHECK (depends_on < seq)
);
