CREATE TABLE execution_ledger (
    execution_id uuid PRIMARY KEY,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    owner_id uuid NOT NULL,
    reserved_until timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('reserved', 'completed')),
    execution_timestamp timestamptz NOT NULL,
    final_transition_at timestamptz,
    result_json jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    CHECK (
        (state = 'reserved' AND result_json IS NULL AND completed_at IS NULL)
        OR (state = 'completed' AND result_json IS NOT NULL
            AND completed_at IS NOT NULL AND final_transition_at IS NOT NULL)
    )
);

CREATE OR REPLACE FUNCTION protect_execution_ledger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'execution ledger rows are immutable';
    END IF;
    IF OLD.state = 'completed' THEN
        RAISE EXCEPTION 'completed execution ledger rows are immutable';
    END IF;
    IF NEW.execution_id <> OLD.execution_id
       OR NEW.request_digest <> OLD.request_digest
       OR NEW.execution_timestamp <> OLD.execution_timestamp
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'execution reservation identity is immutable';
    END IF;
    IF OLD.final_transition_at IS NOT NULL
       AND NEW.final_transition_at IS DISTINCT FROM OLD.final_transition_at THEN
        RAISE EXCEPTION 'final execution transition timestamp is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER execution_ledger_protect_update
BEFORE UPDATE ON execution_ledger
FOR EACH ROW EXECUTE FUNCTION protect_execution_ledger();

CREATE TRIGGER execution_ledger_protect_delete
BEFORE DELETE ON execution_ledger
FOR EACH ROW EXECUTE FUNCTION protect_execution_ledger();
