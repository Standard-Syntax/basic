CREATE TABLE verification_ledger (
    verification_id uuid PRIMARY KEY,
    request_digest text NOT NULL CHECK (request_digest ~ '^[a-f0-9]{64}$'),
    owner_id uuid NOT NULL,
    reserved_until timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('reserved', 'evidence_ready', 'completed')),
    verification_timestamp timestamptz NOT NULL,
    evidence_json jsonb,
    final_transition_at timestamptz,
    result_json jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    evidence_ready_at timestamptz,
    completed_at timestamptz,
    CHECK (
        (state = 'reserved' AND evidence_json IS NULL AND evidence_ready_at IS NULL
            AND result_json IS NULL AND completed_at IS NULL)
        OR (state = 'evidence_ready' AND evidence_json IS NOT NULL
            AND evidence_ready_at IS NOT NULL AND result_json IS NULL AND completed_at IS NULL)
        OR (state = 'completed' AND evidence_json IS NOT NULL
            AND evidence_ready_at IS NOT NULL AND result_json IS NOT NULL
            AND completed_at IS NOT NULL AND final_transition_at IS NOT NULL)
    )
);

CREATE OR REPLACE FUNCTION protect_verification_ledger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'verification ledger rows are immutable';
    END IF;
    IF OLD.state = 'completed' THEN
        RAISE EXCEPTION 'completed verification ledger rows are immutable';
    END IF;
    IF NEW.verification_id <> OLD.verification_id
       OR NEW.request_digest <> OLD.request_digest
       OR NEW.verification_timestamp <> OLD.verification_timestamp
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'verification reservation identity is immutable';
    END IF;
    IF OLD.state = 'evidence_ready'
       AND (NEW.evidence_json IS DISTINCT FROM OLD.evidence_json
            OR NEW.evidence_ready_at IS DISTINCT FROM OLD.evidence_ready_at
            OR NEW.owner_id <> OLD.owner_id) THEN
        RAISE EXCEPTION 'verification evidence is immutable';
    END IF;
    IF OLD.final_transition_at IS NOT NULL
       AND NEW.final_transition_at IS DISTINCT FROM OLD.final_transition_at THEN
        RAISE EXCEPTION 'final verification transition timestamp is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER verification_ledger_protect_update
BEFORE UPDATE ON verification_ledger
FOR EACH ROW EXECUTE FUNCTION protect_verification_ledger();

CREATE TRIGGER verification_ledger_protect_delete
BEFORE DELETE ON verification_ledger
FOR EACH ROW EXECUTE FUNCTION protect_verification_ledger();
