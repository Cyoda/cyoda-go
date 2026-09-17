DROP INDEX IF EXISTS idx_entities_model_entity_id;

CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C")
    WHERE NOT deleted;
