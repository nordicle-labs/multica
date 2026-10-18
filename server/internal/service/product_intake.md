# Product intake operations

Product intake is inactive until an explicit PostgreSQL binding exists. A binding pins one authenticated source identity to one workspace actor, target agent, product key, project, and risk policy. Tony bindings use `multica_chat` with the Chat session UUID as both source and conversation identity. Telegram bindings use `telegram` with the channel installation UUID as the source and `chat_id[:thread_id]` as the conversation identity.

Create or change a binding only through `ProductIntakeService.CreateBindingRevision`. The service validates workspace membership, target agent, project scope, policy, and expiry; serializes concurrent updates; revokes the previous active row; and appends an audited monotone revision. Never edit or delete binding history in place.

Before enabling a source, exercise a controlled message with an explicit `idea|bug|feature|incident|research|project|release` prefix. Verify the response contains the expected issue and task IDs. A queued task has no `started_at`; verify `started_at` after the runtime claims it before treating dispatch as started. Board state, task state, Git SHA, review, QA, merge, activation, and production remain independent records; none implies another.

Risk policies are cumulative:

- `targeted_check`: idea and research.
- `tests_review`: bug, feature, and project, plus lower-risk classes.
- `security_integration`: incident, plus lower-risk classes.
- `explicit_approval_rollback`: release, plus lower-risk classes. Release requests must carry the authenticated member's UUID as `approval_id`; message text cannot grant approval.

To revoke or roll back configuration, append a new binding revision containing the last approved project/policy and a fresh expiry. If no replacement is safe, revoke the active row in a transaction and retain its audit fields. Do not delete history, issue rows, or tasks. Redelivery is bounded by existing channel deduplication and task retry/cancellation/wakeup mechanisms; every resumed ingress re-resolves the current binding, membership, target, project, policy, and approval before creating or resuming work.

Notifications contain only the issue identifier, task ID, and queued/started state. They never include message contents, credentials, or local paths. Production activation and deployment are outside this feature.