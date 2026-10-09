# Decision Impact Review

An owner-authored `DECISION CLEMENT` / `DÉCISION CLÉMENT` marker opens a durable,
fail-closed review before the issue edit can dispatch work. The reserved review
state is `pending`, `failed`, or `completed`; every issue-scoped task insert
(assignment, mention/thread, squad leader, wakeup, recovery, deferred task, and
retry) reads that state from the locked database row. `pending` and `failed`
write no task. Do not work around the gate by choosing another trigger.

Complete the review deterministically before starting the new direction:

1. Page through the whole project board, then the workspace when shared
   resources or explicit cross-project references require it. Advance by the
   page length and read through an empty page when totals are unavailable.
2. Follow parent/child edges and exact issue identifiers, then inspect recent
   comments and active/deferred runs for every open candidate. Lexical
   similarity can add an ambiguous candidate; it is never mutation proof.
3. Record one row per candidate as `issue | relation | impact | action | proof`,
   with action `continue`, `amend`, `pause`, `close`, `supersede/cancel`, or
   `no impact proved`.
4. Cancel only an exact active run UUID carrying structured proof that names
   the same decision, issue, and run. Free text, negated text, a missing UUID,
   or agent-authored owner attribution authorizes nothing. Stop obsolete runs
   before posting comments that could trigger them.
5. Re-read every mutated issue and run. Put only confirmed mutations in
   `impacted_and_corrected`; unresolved rows stay in `pending_impact`. Persist
   `decision_changed`, `not_impacted` with reasons, and `active_after_review`,
   then mark the review `completed`. Any read or write failure leaves it
   `failed` so execution remains blocked.

The review is an authorization boundary: only an authenticated workspace member
whose current role is `owner` may introduce the decision marker or approve
destructive proof. A task-token/agent identity is not owner authority, even when
its accountable human is an owner. The review record cannot be deleted; a
correction is a new owner-authored state with its complete audit matrix.
