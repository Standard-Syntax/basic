ALTER TABLE runtime_run_bindings
    ADD COLUMN beta_policy_uri text,
    ADD COLUMN beta_policy_digest text,
    ADD COLUMN execution_image_digest text,
    ADD COLUMN verification_image_digest text,
    ADD CONSTRAINT runtime_beta_policy_pair
        CHECK ((beta_policy_uri IS NULL) = (beta_policy_digest IS NULL)),
    ADD CONSTRAINT runtime_beta_policy_digest_shape
        CHECK (beta_policy_digest IS NULL OR beta_policy_digest ~ '^[a-f0-9]{64}$'),
    ADD CONSTRAINT runtime_execution_image_shape
        CHECK (execution_image_digest IS NULL OR execution_image_digest ~ '^sha256:[a-f0-9]{64}$'),
    ADD CONSTRAINT runtime_verification_image_shape
        CHECK (verification_image_digest IS NULL OR verification_image_digest ~ '^sha256:[a-f0-9]{64}$');

CREATE OR REPLACE FUNCTION protect_runtime_run_binding()
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
       OR NEW.beta_policy_uri IS DISTINCT FROM OLD.beta_policy_uri
       OR NEW.beta_policy_digest IS DISTINCT FROM OLD.beta_policy_digest
       OR NEW.execution_image_digest IS DISTINCT FROM OLD.execution_image_digest
       OR NEW.verification_image_digest IS DISTINCT FROM OLD.verification_image_digest
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
