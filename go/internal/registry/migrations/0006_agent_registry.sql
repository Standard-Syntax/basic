CREATE TABLE agent_registrations (
    agent_name text NOT NULL,
    agent_version text NOT NULL,
    manifest_digest text NOT NULL,
    canonical_manifest bytea NOT NULL,
    registered_at timestamptz NOT NULL DEFAULT now()
);
