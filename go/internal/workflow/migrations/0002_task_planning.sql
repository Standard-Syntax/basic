ALTER TABLE workflow_runs
    ADD COLUMN task_graph_uri text,
    ADD COLUMN task_graph_digest text,
    ADD CONSTRAINT workflow_runs_task_graph_complete CHECK (
        (task_graph_uri IS NULL) = (task_graph_digest IS NULL)
    ),
    ADD CONSTRAINT workflow_runs_task_graph_digest CHECK (
        task_graph_digest IS NULL OR task_graph_digest ~ '^[a-f0-9]{64}$'
    );

CREATE TABLE workflow_tasks (
    task_id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES workflow_runs(run_id),
    state text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    max_attempts integer NOT NULL CHECK (max_attempts > 0),
    current_attempt integer NOT NULL DEFAULT 0 CHECK (
        current_attempt >= 0 AND current_attempt <= max_attempts
    ),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (run_id, task_id)
);

CREATE TABLE workflow_task_dependencies (
    run_id uuid NOT NULL REFERENCES workflow_runs(run_id),
    task_id uuid NOT NULL,
    depends_on_task_id uuid NOT NULL,
    PRIMARY KEY (run_id, task_id, depends_on_task_id),
    CHECK (task_id <> depends_on_task_id),
    FOREIGN KEY (run_id, task_id) REFERENCES workflow_tasks(run_id, task_id),
    FOREIGN KEY (run_id, depends_on_task_id) REFERENCES workflow_tasks(run_id, task_id)
);

CREATE INDEX workflow_task_dependencies_prerequisite_idx
    ON workflow_task_dependencies(run_id, depends_on_task_id);
