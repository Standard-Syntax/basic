# Workflow state machine

The future kernel will own explicit run and task state machines. Every
transition must validate current state, actor authority, and required artifacts,
then append an event and update current state atomically. Invalid transitions
must make no change.

Phase 0–1 intentionally implements no state, persistence, transition, command,
approval, or event runtime. Reasoning messages are proposals only and cannot
advance a workflow. The complete state enumeration remains in
`docs/plan-brief.md` until the Phase 2 audited design is implemented.
