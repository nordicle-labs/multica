# Tony Factory intake engine

`tonyfactory` is the fail-closed orchestration core for Tony and product-channel intake. It is deliberately transport-neutral: production adapters must implement `IssueResolver`, `Dispatcher`, and `Notifier`; this package never guesses a Multica project, agent, repository, or channel.

## Version 1 contract

`Config.Version` must be `1`. Each source (`tony` or `telegram` plus its stable ID) must match exactly one unexpired `Binding`, which names one product, one Multica project, and one risk policy. Each explicit intake prefix (`idea:`, `bug:`, `feature:`, `incident:`, `research:`, `project:`, or `release:`) must have a registered route and an allowed role. Missing, ambiguous, stale, unclassified, unauthorized, or ungated input stops before issue creation or dispatch.

The canonical issue adapter receives both `ProjectID` and `CanonicalKey`; it must search within that project and honor `IdempotencyKey`. The dispatch adapter receives a stable idempotency key and must return a real run ID with non-zero `StartedAt`. Board status, run, artifact SHA, review, QA, merge, activation, and production evidence are stored separately. Evidence advances in that order and every stage is pinned to the same 40-character SHA.

The file store uses an OS file lock and atomic rename. A pending record is persisted before dispatch. After restart, `Resume` reuses the same external idempotency key; retries are bounded by `MaxWakeups`. Cancellation is durable. Notifications contain only source identity, correlation key, issue ID, run ID, and status—never message contents, secrets, or local paths.

## Controlled canaries

Run from `server/`:

```sh
GOTMPDIR="$PWD/../.gotmp" go test -race ./internal/tonyfactory \
  -run 'TestCanaryTonyCreatesAndResumesAfterRestart|TestCanaryProductChannelHasNoCrossTalk' \
  -count=1 -v
```

The Tony canary proves create-once, a real `started_at`, correlated notification, and restart resume without redispatch. The Telegram canary reuses the same message and canonical keys on two product channels and proves distinct product, project, workflow, run, and issue identities.

## Operations and rollback

1. Validate a candidate registry with the package tests before replacing the active versioned configuration.
2. Keep the previous configuration file. Apply the candidate atomically; do not edit bindings in place.
3. If a canary fails, restore the previous file atomically and restart only the intake worker. Existing state remains keyed by contract version and source identity.
4. Never delete the state file to retry. Correct the adapter or registry, then call `Resume`; deleting state removes the deduplication and wakeup boundary.
5. Production activation is outside this package and outside HERM-1005. A caller must not interpret `in_progress`, review, QA, merge, or activation evidence as proof of production.
