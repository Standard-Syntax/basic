CREATE TABLE agent_registrations (
    agent_name text NOT NULL,
    agent_version text NOT NULL,
    manifest_digest text NOT NULL UNIQUE
        CHECK (manifest_digest ~ '^[a-f0-9]{64}$'),
    canonical_manifest bytea NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_name, agent_version)
);

CREATE FUNCTION reject_agent_registration_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'agent registrations are immutable';
END;
$$;

CREATE TRIGGER agent_registrations_immutable
BEFORE UPDATE OR DELETE ON agent_registrations
FOR EACH ROW EXECUTE FUNCTION reject_agent_registration_mutation();
