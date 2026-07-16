# Fishhawk operator runbook

You are the operator of a Fishhawk run. The agent proposes work and writes
the code; you decide at every gate and own all version-control actions
(approve the PR, then `fishhawk_merge_run`). This runbook is the in-band counterpart to
the operator-role contract (ADR-040): read it when you are driving a run
without prior operator memory.

The MCP server's `instructions` field carries the short happy-path map. This
resource is the long form: the full procedure plus the edge cases that strand
a run when handled wrong.

## The operator role

The agent's verdicts and PR bodies are proposals, not facts. Verify the
committed code at the PR head — not the agent's prose — before you approve or
merge. The agent must not file follow-up issues, take git actions, or merge;
those are yours. When a run-status response carries a delegation block marking
an action `delegated:true`, you may take it under the operator-agent token;
otherwise loop a human.

Prefer the server's `next_actions` block (on `fishhawk_get_run_status`) over
procedure recall: it is the authoritative, run-state-aware "what to do next"
and supersedes any step sequence written here when the two disagree.

## Happy-path loop

One issue → one run → one PR.

1. **`fishhawk_start_run`** — open a run for the issue. For the local dogfood
   loop pass `runner_kind:local` (see below — this is the single most common
   mistake). The call returns the run id every later verb takes.
2. **`fishhawk_run_stage` (plan)** — the agent writes a `standard_v1` plan.
   The call blocks until the plan stage settles.
3. **Read the plan and its reviews** with `fishhawk_get_plan`, then
   **`fishhawk_approve_plan`** (approval notes are delivered to the implement
   agent as binding conditions) or **`fishhawk_reject_plan`** with a reason —
   rejection feedback propagates to a fresh planning run.
4. **`fishhawk_dispatch_stage` (implement)** — execute the approved plan.
5. **`fishhawk_await_review`** — block until the implement review reaches a
   terminal verdict. Re-poll `fishhawk_get_run_status` if it times out; that
   poll is the authoritative path to a terminal status. Once the review
   settles, read the gate with **`fishhawk_get_gate_view`** (see below) to
   decide fix-up vs merge — one call carrying the full concern notes.
   For an hour-scale wait (a full implement pass or review round), supply a
   `progressToken` plus `timeout_seconds` up to **7200** (#1963): the per-tick
   MCP progress heartbeat keeps the client's idle clock alive, so one call
   replaces the old 600s re-arm loop. Without a token the cap stays 600s and
   the resumable re-arm contract is unchanged. The same token-conditional cap +
   heartbeat applies to `fishhawk_await_audit`.
6. **Approve the PR, then `fishhawk_merge_run`.** Approve the PR with an
   operator verdict (`gh pr review --approve`, under your own GitHub identity —
   App-identity approval is deferred to E39) before every merge — no
   exceptions. Then call **`fishhawk_merge_run`**: one verb records your merge
   verdict as a chained audit entry, queues the squash merge through the same
   seam `drive_run`'s `may_merge` arm uses, awaits the terminal run state, and
   surfaces your post-merge dev-host step (`scripts/dev post-merge`, surfaced
   for you to run — ADR-038 keeps host mutation out of the MCP surface). The
   endpoint is idempotent: a timed-out re-invoke or a 502 retry re-queues the
   merge with no duplicate verdict row, so it is safe to re-call. Queueing the
   merge before the approval settles is also safe — GitHub fires it once branch
   protection is satisfied.

## Edge-case playbook

### Reading the review gate (`fishhawk_get_gate_view`)

At a review or fix-up gate, `fishhawk_get_gate_view` answers "what is still open
and why" in ONE call — do NOT stitch `fishhawk_get_run_status` (whose concerns
block elides the note text) with `fishhawk_list_audit` (walking `implement_reviewed`
for the full notes, `stage_fixup_triggered`/`fixup_pushed` for the routing
history) to reconstruct the same picture. Pass `{run_id, stage_kind:"implement"}`
(or `plan`); the response gives each OPEN concern with its **full note**, a
derived `round`, `has_suggested_patch`, and the per-concern `fixups[]` (the prior
routing reasons + their `pushed`/`no_changes`/`pending` outcomes) and
`resolutions[]` (the re-review confirmations/reopens) — the exact context for a
fix-up-vs-merge decision. The settled ledger and `suppressed_relitigations` ride
along so you can see what was already waived/deferred and what a reviewer tried
to re-litigate. If `history_incomplete` is true, `history_gaps` names which audit
join failed — the concerns are still complete; only the cross-references may be
missing. This surface applies none of the compaction levers, so the notes are
never truncated.

### runner_kind:local for the local dogfood loop

`fishhawk_start_run` defaults `runner_kind` to `github_actions`. The local
dogfood loop dispatches the runner on this host, so a run started with the
default tag has an execution/tag mismatch: the auto-advance from exempt to
PR-open never fires, fixup does not auto-spawn the runner, and the next-action
hints read "nothing to run" even though work is pending. **Always pass
`runner_kind:local`** when driving the local loop. A run already started with
the wrong kind cannot be retagged — cancel it and start fresh.

### Local-drive fixup needs an explicit dispatch_stage

On a local-drive run, `fishhawk_fixup_stage` re-opens the implement stage to
state `awaiting_host_dispatch` ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912) —
the parked-for-host-spawn state; the backend cannot spawn the host-local runner)
but does **not** spawn the runner itself. It returns no `log_path`, and the
"github_actions auto-dispatches / nothing to run" next-action hint is **false**
for local drive. After a fixup, dispatch the parked stage: either call
`fishhawk_dispatch_stage` (implement) by hand, or let **`fishhawk_drive_run`
auto-dispatch it** — the driver now treats `awaiting_host_dispatch` as
host-spawnable and dispatches a parked implement with no manual handoff (#1912).
Leaving the stage parked without one of those strands the run with a re-opened
stage and no runner.

Note also that a fixup re-drives the **entire** implement agent (tens of
thousands of tokens), not a patch. A no-op fixup (zero diff) still burns the
budget and wedges the run. Before approving a fixup, cross-check the
files-changed against the plan scope; if a pass would be a no-op, abandon and
start a fresh run instead.

### Failed-run revive (`fishhawk_revive_run`)

When a stage fails, the **whole run** flips terminal-`failed`. On a run with more
than one failed stage — or a healthy stage whose review is still settling when a
sibling's failure flipped the run terminal — the old recovery was a
**retry-without-dispatch dance**: `fishhawk_retry_stage` each failed stage one at
a time, being careful **not** to let any re-opened stage dispatch out of gate
order (the #1700 wrong-order re-dispatch corruption), and hand-park the rest.
That dance is **retired**. Use the single verb instead:

**`fishhawk_revive_run` (run_id)** re-admits the terminal-`failed` run in one
call. The backend **pre-validates** that *every* failed stage is retryable, then
re-parks each in its correct gate-ordered pre-dispatch state (A/C → `pending`,
D SLA-timeout → `awaiting_approval`, decomposed-parent implement →
`awaiting_children`) and flips the run **failed → running**. A single
non-retryable failed stage (category-B, D-rejected, or one with no recorded
category) refuses the **whole** revive with `422 revive_not_applicable` naming
the blocking stage — **no partial mutation**, so you never end up half-re-parked.

The load-bearing property: revive **re-parks only — it never dispatches**. Each
re-parked stage sits in its pre-dispatch state until you dispatch it at its
proper gate turn via the existing verbs (`fishhawk_dispatch_stage` /
`fishhawk_run_stage` on the local runner). Because no orchestrator `Advance`
fires during the revive, the #1700 wrong-order re-dispatch is **structurally**
impossible — you no longer have to hand-sequence it. Poll
`fishhawk_get_run_status` after reviving and follow `next_actions` for each
re-parked stage.

Distinct from `fishhawk_retry_stage`, which re-opens **one** stage and
**auto-dispatches** it: reach for **retry** when you want a single stage re-run
immediately; reach for **revive** when a run has flipped terminal and you want a
safe **batch** re-park (especially while sibling reviews are still settling).
Each re-park consumes that stage's per-stage retry budget exactly like a retry —
revive is a batch retry-shaped re-open, not a budget bypass. Revive is
**operator-token only** (`write:stages` or `write:retries`); a run-bound agent
token is refused `403 agent_token_forbidden`. The failed-run `next_actions` arms
(`implement_failed_category_a`, `implement_failed`) surface it alongside
`fishhawk_retry_stage`.

**Provenance.** This one-verb section supersedes the pre-#1915
retry-without-dispatch dance the 2026-07-12/13 drives used ([#1916](https://github.com/kuhlman-labs/fishhawk/issues/1916)):
reach for `fishhawk_revive_run`, not a hand-sequenced per-stage retry that you have
to keep from re-dispatching out of gate order.

**Pre-dispatch check for a re-parked acceptance stage.** Revive re-parks only —
you dispatch each re-parked stage at its gate turn. One case needs a read first: a
re-parked **acceptance** stage may already carry a recorded outcome (or a retried
acceptance may short-circuit straight to one). Before dispatching it,
`fishhawk_list_audit` on `acceptance_outcome_recorded` for the stage; the server
`422 retry_not_applicable`s when a verdict is already recorded (the same guard the
settled-outcome-unknown recovery relies on). Confirm no verdict exists, then
dispatch.

### Decomposed-parent native path (`fishhawk_run_children` / `fishhawk_consolidate_slices`)

When a plan is **decomposed** into child slices, the parent's implement stage
parks in `awaiting_children` and its child slices own it. Do **NOT**
`fishhawk_dispatch_stage` / `fishhawk_run_stage` the parent's implement stage:
since [PR #1902](https://github.com/kuhlman-labs/fishhawk/pull/1902)
`guardSiblingStageInFlight` **refuses** an `awaiting_children` target and its error
names the correct verbs — a spawned runner would `409 stage_not_runnable` and the
detached reaper's failure report would destroy the park (`awaiting_children →
failed` is a legal sweeper edge). Drive the native path instead:

1. **Approve the parent plan** as usual.
2. **`fishhawk_run_children` (parent run_id)** — discovers the children from the
   `plan_decomposed` audit entry, sequences their `depends_on` edges into
   topological **waves**, and provisions a per-child isolated worktree. A wave-1
   child bases on the wave-0 integration commit, so a producer→consumer slice pair
   typechecks against the already-integrated symbol. Re-invocation is idempotent
   (in-flight and terminal children are reported as-is, only pending ones spawn).
3. **`fishhawk_consolidate_slices` (parent run_id)** — once **every** child is
   terminal, run the fan-in that merges each slice branch onto the consolidated
   branch and opens the consolidated PR. The 60s child-completion sweeper backstop
   is OFF by default in `fishhawkd` (`--enable-child-completion-sweeper`), so on
   the local loop this operator verb **is** the fan-in. The endpoint refuses a
   partial set — a failed child must be resolved or re-driven first.
4. **Review the consolidated PR.**

The parent's own human gates are unchanged around the implement fan-out:
**pre(plan)** before the children run and **post(review)** on the consolidated
result.

### Driving with `fishhawk_drive_run`

`fishhawk_drive_run` ([ADR-040 / #1700](https://github.com/kuhlman-labs/fishhawk/issues/1700))
is the local auto-driver: a bounded, resumable loop that executes every mechanical
operator step between human gates on a `runner_kind:local` run and stops at the
first genuine decision. Invoke it **after `fishhawk_start_run`** (`runner_kind:local`).

Each iteration the driver dispatches only the **earliest gate-ordered non-terminal
stage** (plan → implement after plan succeeds → acceptance after implement's review
settles). It **auto-approves the plan gate** when the run's ADR-040 delegation rule
is satisfied (e.g. `clean_dual_approval`) — post-read the auto-approved plan with
`fishhawk_get_plan`. Post-[#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)
it **auto-dispatches a locally-parked `awaiting_host_dispatch` stage** with no
manual handoff (the backend cannot spawn the host-local runner, so a plan-approved
implement parks there and the driver treats it as host-spawnable). This
**supersedes** the pre-#1912 manual handoff for the parked implement — that
hand-dispatch is retired.

It **stops** and returns on the first genuine decision, and every stop is
resumable:

- `decision_required:<state>` — an operator gate (a plan gate without
  `may_approve`, a split reviewer verdict, or a pending scope amendment).
- `paged:<event>` — a paged disposition you arbitrate.
- `dispatched_stale` — past the liveness threshold the driver **probes host
  runner liveness itself** ([#1955](https://github.com/kuhlman-labs/fishhawk/issues/1955),
  building on [#1924](https://github.com/kuhlman-labs/fishhawk/issues/1924)/[#1927](https://github.com/kuhlman-labs/fishhawk/issues/1927)):
  a **dead** runner (no process matching the stage's `--stage-id`) is
  **auto-recovered** — re-dispatched with **no operator action**. This stop now
  fires **only when the probe is ambiguous** — a live-but-unregistered process,
  or no `pgrep` on the host — and THAT is when you `pgrep -f fishhawk-runner`
  (and read the dispatch `log_path`) by hand before re-dispatching.

**Every stop is resumable by re-invoking with the SAME `run_id`.** `max_minutes`
clamps to **[1,240]** (default **60**); a timeout is itself a resumable stop.

### Batch-as-campaign (local campaign drive)

A batch instruction — "run these N issues through the loop" — maps to **one
campaign**, not N hand-sequenced `fishhawk_start_run`/`fishhawk_drive_run`
cycles tracked out-of-band. A campaign is the DAG-ordered batch counterpart to
the single-run loop: it assembles once from an epic and you drive its
constituent runs locally through the campaign verbs.

**Validated end-to-end (E48.12 / #1959).** This section is the write-up of a
real walk, not aspirational prose. Campaign
`80a69eba-1ca1-4deb-a12e-db1d8ad4d9f7` on epic
[#1940](https://github.com/kuhlman-labs/fishhawk/issues/1940) drove **16 real
`feature_change` items** end-to-end on `runner_kind:local` across 2026-07-15/16,
with a per-item `fishhawk_drive_run` handoff and `fishhawk_get_campaign_status`
read as the **single status surface**. Every gap that walk surfaced was filed
under E48:
[#1970](https://github.com/kuhlman-labs/fishhawk/issues/1970),
[#1972](https://github.com/kuhlman-labs/fishhawk/issues/1972),
[#1975](https://github.com/kuhlman-labs/fishhawk/issues/1975),
[#1980](https://github.com/kuhlman-labs/fishhawk/issues/1980),
[#1983](https://github.com/kuhlman-labs/fishhawk/issues/1983),
[#1987](https://github.com/kuhlman-labs/fishhawk/issues/1987),
[#1989](https://github.com/kuhlman-labs/fishhawk/issues/1989),
[#1995](https://github.com/kuhlman-labs/fishhawk/issues/1995).

**Shape the batch as an epic first.** A campaign assembles from an **epic ref**
whose children optionally carry `depends_on` edges. If the batch is not already
one, file it via `fishhawk_draft_epic` (the refinement intake loop) or
`fishhawk_file_issue` with `relations.parent_epic` / `depends_on`, then start the
campaign against that epic.

**1. Start the campaign.** `fishhawk_start_campaign` (`repo` + `epic_ref`
required; optional `pause_policy` of `pause_campaign` (default) / `pause_item`,
fixed at create time). It resolves the epic's children + their `depends_on`
edges, wave-orders the DAG, and persists the campaign — the batch counterpart to
`fishhawk_start_run`. A dependency targeting a non-child fails
`campaign_dangling_dependency`; an un-installed repo fails `repo_not_installed`.

**2. The drive-tick loop — `fishhawk_get_campaign_status` is the single status
surface.** It is reconcile-on-read: each poll settles every terminal item run and
advances the campaign in DAG order (the same state-guarded transitions the GHA
driver's ADVANCE pass uses), so you need neither `--enable-campaign-driver` nor
GitHub Actions. The response carries the readiness `rollup`
(`eligible`/`blocked`/`running`/`done`/`failed`/`cancelled`/`paused`) and a
`next_actions` block naming the legal move: `start_run` (an item is eligible),
`wait` (re-poll), `attention` (read the failed item's run and retry/abandon),
`resume` (a paged gate is handled), `complete` (terminal). Poll it as your
drive tick; do not track batch state out-of-band.

**3. Start ONE item — `runner_kind:local` always.**
`fishhawk_start_campaign_item_run` (`campaign_id` + `issue_ref` +
`workflow_id`) when `next_action` is `start_run`. **Always pass
`runner_kind:local`** — the same execution/tag-mismatch hazard as
`fishhawk_start_run` (see the `runner_kind:local` section above): a
`github_actions`-tagged item never dispatches on this host. The call refuses an
ineligible item with `item_not_eligible`, naming the blocking dependency; a
paused/terminal campaign refuses with `campaign_not_startable`; an unknown ref is
`campaign_item_not_found`. It mints the run, links it to the item, and moves the
item to `running`.

**4. Per-item handoff — drive the minted run to merge.** Hand the returned
`run_id` to `fishhawk_drive_run` and drive it to merge **exactly as a solo local
run**. Every local edge case above applies per item: the fixup explicit-dispatch
rule, `awaiting_host_dispatch` auto-dispatch, the `dispatched_stale`
liveness-probe stop, heterogeneous-review two-verdict waits, and post-failure
clean-tree discipline.

**5. Serialize — one item at a time.** The manual path has deliberately **no
server-side concurrency cap** (unlike the GHA driver's `MaxParallel`): the
operator decides when to start each eligible item. Start **one item at a time**
and serialize local verifies until the
[#1918](https://github.com/kuhlman-labs/fishhawk/issues/1918) two-concurrent-local-runs
experiment settles the rule. There is also no `idempotency_key` — the eligibility
gate already refuses a re-start against a running item.

**6. Per-item post-merge — before the next eligible item.** After each item's
merge, run `scripts/dev post-merge <issue>` **before starting the next eligible
item**, so the next run mints from the advanced `main` (the ordered discipline
the 16-item walk followed). Then re-poll `fishhawk_get_campaign_status` for the
now-eligible descendant.

**7. Failure handling.** A failed item settles on the next status poll and
surfaces as `attention` (never auto-advanced past). Read the failed item's run
(`fishhawk_get_run_status`), then retry/revive or abandon through the run verbs
and re-poll — a recovered descendant re-links per the reconcile recovery arm.
`fishhawk_resume_campaign` is legal **only against a paused campaign**
(`campaign_not_paused`, 409, otherwise) — reach for it after handling a paged
gate, not to advance an eligible item.

**Human-led (run-less) items settle themselves.** A dependency-satisfied item
with **no linked run** — a human-led (`autonomy:low`) issue closed by a
maintainer PR — settles `succeeded` automatically on the next status poll once
its issue is CLOSED as completed (`state_reason=completed`), with **no operator
action**; its descendants then become eligible.

### Acceptance stage

Some workflows declare an **acceptance stage** (E31.9 / ADR-049) after the
implement review: an advisory, runner-hosted validator that drives the change
against a **running instance you provision** and ships a structured verdict.
The default placement is a **pre-merge preview** built from the PR ref;
release-acceptance against a staging or release instance is the documented
variant. The preview/target instance is customer-provisioned — Fishhawk does
not stand it up.

**Dispatch it like implement.** After the implement review settles with no open
concerns, `fishhawk_dispatch_stage` (acceptance) — non-blocking is the default
because acceptance runs long against the live instance, and it keeps the session
free. `fishhawk_run_stage` (acceptance) is the blocking opt-in. The stage takes
no new argv (no `--plan-out`, no `--check-base-ref`); its egress target hosts and
criteria ids arrive via `--fetch-prompt`.

**Await the verdict.** Poll `fishhawk_get_run_status` — `acceptance_stage_wait_status`
tracks the stage's execution — or `fishhawk_await_audit` anchored at the
`acceptance_dispatched` sequence. `fishhawk_await_review` does **not** fit: it is
shaped around configured-reviewer verdicts and the acceptance stage has no
reviewers; its settle signal is the audit trail.

**Verdict vs. stage state (load-bearing).** A **failed** acceptance verdict
leaves the STAGE `succeeded` — the stage settles through the ordinary agent
trace-bundle path regardless of pass/fail. Read the `verdict` from the
`acceptance_outcome_recorded` audit entry, never from the stage state. Merge only
on the `acceptance_passed` next-actions state (ADR-049 decision #6: the merge is
gated on the acceptance_passed evidence condition).

**Deterministic triage of a failure** (ADR-049 decision #2, server-side, bounded
at **2 auto re-runs** per run):

| Class | Failure | Auto disposition |
|---|---|---|
| 1 | the code errors, or every failed criterion is explicit-source | `fixup_dispatched` — re-opens the implement stage as a fix-up pass |
| 2 | no criterion failed but ≥1 skipped (environment/flake) | `retry_dispatched` — re-opens the acceptance stage for a re-run |
| 3 | a failed criterion is inferred-source/unresolvable (bad/ambiguous criterion) | `paged` — no transition, you arbitrate |
| 4 | unitemized / provenance-ungroundable (works-as-planned, disputed) | `paged` — no transition, you arbitrate |

At the re-run budget, or when the fix-up/retry route is unavailable, the
disposition degrades to a paged variant (`rerun_budget_exhausted`,
`fixup_unavailable_paged`, `retry_unavailable_paged`, `unsettled_paged`) so
non-convergence always lands on the human.

**LOCAL-runner re-open rule.** An auto-routed re-open (`fixup_dispatched` or
`retry_dispatched`) re-opens the stage server-side but **never spawns the local
runner** — the same rule as local-drive fixup above, generalized. You MUST
`fishhawk_dispatch_stage` the re-opened stage explicitly: the **implement** stage
after `fixup_dispatched`, the **acceptance** stage after `retry_dispatched`.
`next_actions` surfaces this as the `acceptance_triage_rerouting` state; on the
next snapshot the re-opened stage's own dispatch arm serves the move.

**Paged arbitration.** For a paged-family disposition, `next_actions` gives the
`acceptance_triage_paged` arm: read the evidence first (`fishhawk_list_audit` on
`acceptance_outcome_recorded` for the criteria results and
`acceptance_triage_decided` for the class + reason), then arbitrate —
`fishhawk_fixup_stage` (a manual fix-up pass, consumes the shared fix-up budget),
`merge_and_file_follow_up` (accept-and-ship, e.g. a class-3 bad criterion), or
`fishhawk_cancel_run`.

**Settled-outcome-unknown recovery (E31.16 / #1567).** A different failure from
the paged case: the acceptance stage settled `succeeded` but **no**
`acceptance_outcome_recorded` verdict shipped at all — the agent emitted a
non-schema field and the verdict failed closed before it reached the backend
(the run-f7a4b71b hole). `next_actions` surfaces this as the
`acceptance_settled_outcome_unknown` state — deliberately NEVER the merge ritual
(fail toward read, not toward merge). First `fishhawk_list_audit` on
`acceptance_outcome_recorded` to CONFIRM no verdict exists for the stage (the
default `audit_limit` is 5, so a real verdict can merely have aged out of the
window). Once confirmed, `fishhawk_retry_stage` on the **acceptance stage id**
re-opens it `succeeded → pending` (operator token only; an agent token is
refused 403, and the server 422s `retry_not_applicable` if a verdict IS
recorded). The reopen lands the stage in pending, so on the local runner
`fishhawk_dispatch_stage` (acceptance) — surfaced by the `acceptance_pending`
arm on the next snapshot — spawns the actual re-run.

### Late CI/SAST finding after the fix-up ceiling

The bounded fix-up budget is hard-capped at 3 total passes per implement stage
(normal pass + operator overrides). Once that ceiling is reached,
`fishhawk_fixup_stage` refuses with `422 fixup_ceiling_reached` and the MCP
`review_action_hint` stops offering an override. A required external check
(CodeQL/SAST) can still surface a late finding at that point, and there is no
fix-up pass left to route it through the agent.

The sanctioned in-loop remedy is the operator-vouched patch path (#1068/#1044),
NOT a separate CI/SAST budget: commit the one-line fix on the run branch
yourself, then `fishhawk_vouch_commit` it. The vouch unions your operator
commit into the run's reported-head ledger so it clears the ADR-035
sole-writer lineage gate — the run is not wedged with a `foreign_commit_on_branch`
failure, and the operator commit is attributed in the audit chain.

Use the **operator / operator-agent token** for the vouch. `fishhawk_vouch_commit`
rejects a run-bound `mcp:run:<uuid>` (`fhm_`) token outright
(`run_token_forbidden`) by design — an agent self-declaring lineage for a
commit on its own branch would defeat the cross-write protection the vouch
exists to preserve. So the surfaced remedy is only actionable with the operator
token. If the finding is not worth an in-loop fix, the ceiling-reached hint's
other arms still apply: merge with a follow-up issue, or start a fresh run.

### Scope-amendment decide / naming flow

When the implement agent discovers a file it must change that is outside the
approved `scope.files` (a coupled test, a registration table, a doc
companion), it does not edit it silently — it files an operator-gated
scope amendment and waits. You decide with `fishhawk_decide_scope_amendment` (or
`fishhawk_list_scope_amendments` to enumerate pending requests). Watch the
runner log for `scope_amendment_pending` (the runner-log event), not
`scope_amendment_requested` (the audit category); missing it lets the agent
loop on its wait-poll until the stage is killed.

To **add** files at the plan-approval gate, name them explicitly as
`dir/file.ext` in the approval reason (this folds them into scope) or use the
add-scope-files path. Do **not** write a repo-relative path into approval
*rationale* prose to merely explain it — that folds the path into required
scope, and an untouched required file fails the stage. Pending amendments
survive a stage failure: approve post-failure, then `fishhawk_retry_stage`
(one stage) or `fishhawk_revive_run` (the whole terminal-`failed` run, re-parked
without dispatch) folds them at restart.

### Heterogeneous-review two-verdict waits

A `feature_change` run is reviewed by two agents concurrently (e.g. an Opus
reviewer and a GPT reviewer) on both the plan and implement stages. Wait for
**both** verdicts before acting — the review status stays `pending` until every
configured reviewer has landed a terminal verdict. Advisory disagreement is
normal and expected: one reviewer rejecting while the other approves does
**not** block the run. You arbitrate — read both verbatim verdicts and decide.
A reviewer marked external/OOM-failed is usually a misclassified adapter error
and is non-blocking; do not treat it as a real rejection without checking the
underlying error.

### Post-failure clean-tree discipline

A failed run can leave the working tree dirty: `working_tree_restored` leaves
untracked new files behind, and a failed `fishhawk_fixup_stage` can leave you
on the run branch with staged changes. Run `git status` before every
`fishhawk_run_stage` / `fishhawk_dispatch_stage`; an unclean tree lets the next
runner sweep leftovers into the commit and fail the stage on scope drift
(category-B). Recover with `git checkout -f main` (or remove the untracked
files) so each run starts from a clean base. The runner owns all commit /
branch / push operations — never commit or switch branches yourself.

### Refinement intake loop

Distinct from the run loop above: `fishhawk_draft_epic` (E34.4 / ADR-052) turns
a natural-language **brief** into a structured epic + children, gated behind a
preview + approval step before anything files. It is **one tool with five
mutually-exclusive arms** — approve and file are arms on it, not
`fishhawk_approve_plan` (which is stage-gated and resolves a run/stage; a
refinement session is neither a run nor a stage). Every result carries a
`session_guidance` block naming the exact next arm + arguments for the derived
state, so you never guess the next verb.

The arms and the happy path (brief → draft → preview → edit → approve → file):

| Arm | Input | Does |
|---|---|---|
| open | `brief` alone | drafts the epic + children, opens a session, returns `awaiting_approval` |
| preview | `session_id` alone | reads the current draft + derived approval `state` |
| edit | `session_id` + (`brief_amendment` \| `draft`) | appends a new revision — agent re-draft, or a direct `EpicDraft` field edit |
| decide | `session_id` + `decision` (`approved`\|`rejected`) + `reason` | records the verdict on the latest revision |
| file | `session_id` + `repo` | files the approved, un-drifted draft into the tracker |

Arm dispatch **fails closed with no HTTP call** when zero arms or an illegal
combination is populated (e.g. `brief` + `decision`, or both edit arms) — the
error enumerates the legal combinations.

**Criteria pre-check is advisory (E34.5 / #1596).** The open, preview, and edit
results — and the session view — carry a `criteria_precheck` block: a
per-drafted-child acceptance-criteria screen run through the same deterministic
rule set as the plan-stage acceptance pre-check. Each finding names the child
**ordinal** (1-based) and the **rule** (`no_blocking_criterion` is the one that
sets a child's `needs_attention`; a non-empty epic `out_of_scope` is the
justified escape hatch that suppresses it across the draft). The `decide`
guidance names any flagged ordinals so you see the defect before deciding. Read
it before you approve — but it is **advisory only**: a flagged draft never blocks
`decide approved` or `file`. It informs your verdict; it does not make it.

**Rejection / re-draft path.** A `rejected` verdict does not end the session:
re-draft via `brief_amendment` (bounded by a **per-session budget of 3**; a
further amendment returns `amendment_budget_exhausted` — switch to a direct
`draft` edit, which has no budget) or a direct `draft` edit. Either **appends a
new revision and re-gates the session to `awaiting_approval`**. An edit **after
approval** re-gates the same way — session state is derived, never stored.

**Decide-reason + decide-once rules.** `reason` is required on every decision.
A revision carries at most one decision: a second decision on the same revision
is `decision_already_recorded` — **re-gate by editing, never decide twice.**

**Drift is fail-closed.** If an edit lands after approval, the approval's pinned
content hash no longer matches: the session view reports `drifted: true` and
fail-closes to `awaiting_approval`. `session_guidance` says **re-decide the
latest revision** — a premature `file` arm returns `refinement_draft_drifted`.

**Idempotent filing resume.** The `file` arm pins the target `repo` at first
invoke (a re-invoke naming a different repo is `refinement_filing_repo_mismatch`).
A mid-sequence provider failure is `refinement_filing_failed` (502) carrying the
filed-so-far items + failing ordinal — **re-invoke the `file` arm with the SAME
repo**; it resumes at the first unfiled ordinal and never re-files a recorded
one. A fully completed session replays as `already_completed: true` and files
nothing.

**Auth.** A write tool requiring `write:approvals` — **no new scope** (the E34.2
precedent), so the operator token already driving `fishhawk_approve_plan` works
unchanged.
