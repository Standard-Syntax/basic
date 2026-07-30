ALTER TABLE runtime_run_bindings
    ALTER COLUMN repository_map_uri DROP NOT NULL,
    ALTER COLUMN repository_map_digest DROP NOT NULL;

ALTER TABLE runtime_run_bindings
    ADD CONSTRAINT runtime_repository_map_pair
    CHECK ((repository_map_uri IS NULL) = (repository_map_digest IS NULL));

ALTER TABLE runtime_api_idempotency
    ADD COLUMN reservation_expires_at timestamptz,
    ADD COLUMN reservation_generation bigint;

UPDATE runtime_api_idempotency
SET reservation_expires_at = CASE
        WHEN status_code IS NULL AND completed_at IS NULL
            THEN now() + interval '30 seconds'
        ELSE COALESCE(completed_at, created_at)
    END,
    reservation_generation = 1
WHERE reservation_expires_at IS NULL
   OR reservation_generation IS NULL;

ALTER TABLE runtime_api_idempotency
    ALTER COLUMN reservation_expires_at SET NOT NULL,
    ALTER COLUMN reservation_generation SET NOT NULL,
    ADD CONSTRAINT runtime_idempotency_generation_positive
        CHECK (reservation_generation > 0);

CREATE FUNCTION protect_runtime_run_binding()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'runtime run bindings are immutable';
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id
       OR NEW.intake_uri IS DISTINCT FROM OLD.intake_uri
       OR NEW.intake_digest IS DISTINCT FROM OLD.intake_digest
       OR NEW.base_commit IS DISTINCT FROM OLD.base_commit
       OR (OLD.repository_map_uri IS NOT NULL AND
           (NEW.repository_map_uri IS DISTINCT FROM OLD.repository_map_uri OR
            NEW.repository_map_digest IS DISTINCT FROM OLD.repository_map_digest))
       OR (OLD.approved_specification_uri IS NOT NULL AND
           (NEW.approved_specification_uri IS DISTINCT FROM OLD.approved_specification_uri OR
            NEW.approved_specification_digest IS DISTINCT FROM OLD.approved_specification_digest))
       OR (OLD.approved_task_graph_uri IS NOT NULL AND
           (NEW.approved_task_graph_uri IS DISTINCT FROM OLD.approved_task_graph_uri OR
            NEW.approved_task_graph_digest IS DISTINCT FROM OLD.approved_task_graph_digest))
       OR (OLD.composite_approval_uri IS NOT NULL AND
           (NEW.composite_approval_uri IS DISTINCT FROM OLD.composite_approval_uri OR
            NEW.composite_approval_digest IS DISTINCT FROM OLD.composite_approval_digest))
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'established runtime run bindings are immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runtime_run_bindings_immutable
BEFORE UPDATE OR DELETE ON runtime_run_bindings
FOR EACH ROW EXECUTE FUNCTION protect_runtime_run_binding();

CREATE FUNCTION protect_runtime_task_binding()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'runtime task bindings are immutable';
END;
$$;

CREATE TRIGGER runtime_task_bindings_immutable
BEFORE UPDATE OR DELETE ON runtime_task_bindings
FOR EACH ROW EXECUTE FUNCTION protect_runtime_task_binding();
