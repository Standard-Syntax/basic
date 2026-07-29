ALTER TABLE reasoning_invocations
    ADD COLUMN provider_response_artifact_uri text,
    ADD COLUMN provider_response_digest text
        CHECK (provider_response_digest ~ '^[a-f0-9]{64}$'),
    ADD COLUMN provider_request_id text;

ALTER TABLE reasoning_invocations
    ADD CONSTRAINT reasoning_invocations_provider_response_pair
    CHECK (
        (provider_response_artifact_uri IS NULL) =
        (provider_response_digest IS NULL)
    )
    NOT VALID;
