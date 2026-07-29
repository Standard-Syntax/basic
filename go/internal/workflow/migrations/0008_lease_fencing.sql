ALTER TABLE workflow_tasks
    ADD COLUMN lease_fencing_token bigint;

UPDATE workflow_tasks
SET lease_fencing_token = current_attempt
WHERE lease_id IS NOT NULL;

ALTER TABLE workflow_tasks
    DROP CONSTRAINT workflow_tasks_lease_complete,
    ADD CONSTRAINT workflow_tasks_lease_complete CHECK (
        (lease_id IS NULL) = (lease_owner_id IS NULL)
        AND (lease_id IS NULL) = (lease_expires_at IS NULL)
        AND (lease_id IS NULL) = (lease_fencing_token IS NULL)
    ) NOT VALID,
    ADD CONSTRAINT workflow_tasks_lease_fencing_token CHECK (
        lease_fencing_token IS NULL OR lease_fencing_token > 0
    ) NOT VALID;

ALTER TABLE workflow_tasks
    VALIDATE CONSTRAINT workflow_tasks_lease_complete;

ALTER TABLE workflow_tasks
    VALIDATE CONSTRAINT workflow_tasks_lease_fencing_token;
