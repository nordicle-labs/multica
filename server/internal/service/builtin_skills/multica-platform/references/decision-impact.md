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
2. Follow parent/child and dependency edges, exact issue identifiers, and
   shared project resources, then inspect the complete paginated comment
   history (roots and replies) plus active/deferred runs for every candidate. Lexical
   similarity can add an ambiguous candidate; it is never mutation proof.
3. Record one row per candidate as `issue | relation | impact | action | proof`,
   with action `continue`, `amend`, `pause`, `close`, `supersede/cancel`, or
   `no impact proved`.
4. Cancel only an exact active run UUID carrying structured proof that names
   the same decision, issue, and run. Free text, negated text, a missing UUID,
   or agent-authored owner attribution authorizes nothing. Stop obsolete runs
   before posting comments that could trigger them.
5. Re-read every mutated issue and run. Submit the immutable `scan_id` shown by
   the pending review; a completion prepared for an older scan is rejected.
   Put only confirmed mutations in
   `impacted_and_corrected`; unresolved rows stay in `pending_impact`. Persist
   `decision_changed`, `not_impacted` with reasons, and `active_after_review`,
   then mark the review `completed`. Any read or write failure leaves it
   `failed` so execution remains blocked.

The exhaustive candidate matrix is normalized server-side; issue metadata keeps
only the bounded scan identity and counts. Completion locks and revalidates the
candidate issues and active/deferred runs before a revision-CAS changes the gate
to `completed`. A row cannot be declared corrected while a run remains active,
and close/supersede actions require the issue to be terminal.

The review is an authorization boundary: only an authenticated workspace member
whose current role is `owner` may introduce the decision marker or approve
destructive proof. A task-token/agent identity is not owner authority, even when
its accountable human is an owner. The review record cannot be deleted; a
correction is a new owner-authored scan and complete audit matrix.
