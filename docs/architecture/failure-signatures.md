# Failure-signature catalog

**Registry version: `v1`** · Source: `backend/internal/failuresig/registry.go` · Issue: [#1703](https://github.com/kuhlman-labs/fishhawk/issues/1703)

## What this is

When a stage fails, the product already holds the evidence that identifies *which* failure it is: the failure category, the failure-reason string the runner or backend wrote, and the runner's `stage_progress` counters. Until now, turning that evidence into a recovery required operator memory — knowing that `lineage_lock` means "dispatch decomposition children sequentially", or that a zero-exit strand is not worth retrying.

The failure-signature registry is that memory, shipped as product behaviour. It matches a failed stage's evidence against an ordered catalog of named signatures and, on a match, `next_actions` carries an additional `signature` block naming what the failure means and the recommended recovery sequence.

## Reading a signature block

`fishhawk_get_run_status` (and the run-terminal `fishhawk_run_stage` result) render it inside `next_actions`:

```json
{
  "next_actions": {
    "state": "implement_failed",
    "actions": [ "…unchanged…" ],
    "signature": {
      "registry_version": "v1",
      "id": "lineage_lock_contention",
      "title": "Lineage lock contention",
      "means": "The runner refused to start because another runner already holds this run's lineage lock …",
      "playbook": [
        "confirm no live runner still holds the lock: pgrep -f fishhawk-runner",
        "…"
      ]
    }
  }
}
```

- `id` is the stable key. Every id has a section below.
- `means` is the diagnosis — what the failure IS, not what to do.
- `playbook` is ordered. Step 1 first.

## Contract

- **Display-only.** The block never gates a run, never reorders or replaces the `actions` list, and no server-side applicability predicate reads it. A hint that gates a run would be a bug.
- **Fail-open.** When nothing in the catalog matches, the block is absent and the surrounding `next_actions` output is byte-identical to what it would be without the registry. A registry that changed behaviour on a *non*-match would be worse than no registry at all.
- **Constant-size.** A signature block echoes only registry-owned strings — never your failure reason, never any other run data — so a matched hint cannot grow with the run and cannot crowd out the rest of the response.
- **Best-effort string contracts.** Most signatures key on a substring of a literal the runner or backend emits. The runner is a separate Go module and cannot share a constant with the backend, so a wording change on the emitting side stops a matcher firing. The failure mode is a *missing* hint, never a *wrong* one.
- **First match wins.** The catalog below is in precedence order. Where one failure can satisfy two entries, the earlier one decides — see `external_api_incident` vs `infra_flake_recurred`.

## The catalog

### external_api_incident — Terminal external API error (upstream incident)

**What it means.** The agent's model provider returned a terminal error after exhausting its retries — most often a 529 overload. This is an upstream platform incident, not a defect in the task.

**How it is recognized.** The stage's failure reason carries the runner's `terminal external API error <N> (retries exhausted): …` phrase.

**Recovery playbook.**
1. Check status.claude.com (or the provider's status page) for an active incident.
2. Back off until the incident clears — an immediate retry re-hits the same incident and burns retry budget.
3. Then `fishhawk_retry_stage` to retry the stage in place.

**Provenance.** Distilled from the retry-hint work in `next_actions` (the pre-existing category-A external-API arm), whose reason prose this signature now backs with an explicit playbook.

**Precedence note.** Checked FIRST. A failure detail can cite both an external-API incident and an absorbed infra flake; the recoveries are opposites (back off vs retry immediately), so the incident wins.

### model_quota_exhausted — Model quota exhausted

**What it means.** The agent could not obtain model quota — a usage or rate cap, not a transient crash. The stage will fail identically until the cap resets.

**How it is recognized.** The failure reason carries the runner's `could not obtain model quota` phrase.

**Recovery playbook.**
1. Confirm the cap from the failure reason — the agent made no model call (0 tokens).
2. Wait for the usage window to reset rather than burning retry budget against the wall.
3. Then `fishhawk_retry_stage`.

**Provenance.** [#2085](https://github.com/kuhlman-labs/fishhawk/issues/2085), which introduced the runner-side phrase.

### slice_integration_conflict — Slice integration conflict during fan-in

**What it means.** A decomposed parent's fan-in could not merge one slice branch onto the consolidated branch. The consolidated branch already holds the earlier slices, so the parent is not the thing to re-drive.

**How it is recognized.** The parent implement stage's failure reason carries the fan-in's `slice integration conflict` prefix.

**Recovery playbook.**
1. Read `conflicting_child_run_id` from the newest `slice_integration_conflict` audit entry's structured payload — never parse it out of the reason string.
2. `fishhawk_resume_run` pointed at **that child's** run id, to re-drive only the conflicting slice in place.
3. Do NOT point resume at the parent: it replans from scratch and discards the succeeded sibling slices.

**Provenance.** [ADR-041 / #1142](https://github.com/kuhlman-labs/fishhawk/issues/1142).

### lineage_lock_contention — Lineage lock contention

**What it means.** The runner refused to start because another runner already holds this run's lineage lock — two runners were pointed at the same lineage, or a previous runner's lock outlived it.

**How it is recognized.** The stage failed category **C** and its failure reason carries the runner's `lineage_lock` refusal reason. The category is part of the match: `lineage_lock` is a short token that could plausibly appear in unrelated prose.

**Recovery playbook.**
1. Confirm no live runner still holds the lock: `pgrep -f fishhawk-runner`.
2. If one IS live, wait for it — a second runner into the same lineage will refuse again.
3. Dispatch decomposition children **sequentially**, not concurrently: a concurrent same-lineage dispatch is the usual cause.
4. Then `fishhawk_retry_stage` once the lock is clear.

**Provenance.** The local-decomposition dispatch discipline; the concurrent-dispatch cause is tracked by [#2084](https://github.com/kuhlman-labs/fishhawk/issues/2084).

### zero_exit_strand — Runner exited 0 without settling the stage

**What it means.** The runner exited successfully having settled nothing — it re-entered a phase that had already completed, did no work, and left the stage stranded. A sticky scope-completeness exemption is the known cause.

**How it is recognized.** The reaper's synthesized reason `runner exited 0 without settling the stage (state=…)`.

**Recovery playbook.**
1. Read the runner log for the dispatch: a strand looks like a very short (~seconds) run with no `runner_completed` event.
2. A retry usually re-runs the same no-op — do not spend more than one.
3. If it recurs, `fishhawk_cancel_run` and start a fresh run rather than retrying into the same strand.

**Provenance.** [#2630](https://github.com/kuhlman-labs/fishhawk/issues/2630).

### runner_died_before_reporting — Runner died before reporting a terminal state

**What it means.** The spawned runner exited non-zero without ever reporting a terminal stage state, so the backend reaped the stage on its behalf. The real cause is in the runner log, not in the stage's failure reason.

**How it is recognized.** The reaper's synthesized reason `runner exited <N> before reporting a terminal state`.

**Recovery playbook.**
1. Read the dispatch's `log_path` — the runner's own last lines carry the real cause.
2. Check the host for a crash the runner could not report (out of memory, a killed process, a missing binary).
3. Then `fishhawk_retry_stage` to re-spawn in place.

**Provenance.** The detached-runner reaper in `backend/internal/mcpserver/run_stage.go`.

### infra_flake_recurred — Absorbed infra flake recurred

**What it means.** The stage's verify gate hit an infrastructure flake, absorbed one in-place re-run, and the flake recurred — so the failure is the environment, not the change.

**How it is recognized.** The failure reason cites the `verify_infra_flake_retry` trace event.

**Recovery playbook.**
1. `fishhawk_retry_stage` — a recurring absorbed flake is the cheapest thing to retry.
2. If it recurs again, check the local Docker daemon / testcontainers state before spending a third retry.

**Provenance.** The runner's committed-tree verify gate, which absorbs exactly one infra flake per stage.

### agent_no_progress_repeat — Agent made no progress on a repeat attempt

**What it means.** A repeat attempt failed category-A while its `stage_progress` heartbeat reported zero turns and zero tokens — the agent never got going, so this is a harness or provider-side stall rather than a hard task.

**How it is recognized.** This is the one **counter-anchored** signature: it has no failure-reason literal to key on. It requires failure category **A**, a heartbeat that actually arrived (`stage_progress` present), zero turns AND zero tokens on that attempt, and a retry attempt greater than zero. An *absent* heartbeat leaves the counters at zero and is deliberately NOT read as observed inactivity; a *first* attempt reporting zero turns has simply not got going yet.

**Recovery playbook.**
1. Check the provider status page before spending another retry — a zero-token attempt rarely means a hard task.
2. Read the stage trace for a harness error (a 400 on the first turn, a zero-token hang).
3. Then `fishhawk_retry_stage`; if a third attempt also reports zero turns, stop retrying and read the runner log.

**Provenance.** [#579](https://github.com/kuhlman-labs/fishhawk/issues/579) (transient agent failures are worth a retry before blaming the task), combined with the `stage_progress` heartbeat added by [#2541](https://github.com/kuhlman-labs/fishhawk/issues/2541).

## Adding a signature

Three edits, all in one PR:

1. A matcher + catalog entry in `backend/internal/failuresig/registry.go`, placed at the right point in precedence order.
2. A table case in `backend/internal/failuresig/failuresig_test.go` driving realistic evidence to the new id.
3. A `### <id>` section here.

`TestCatalogDocumentsEverySignature` fails if step 3 is skipped, and `TestCatalogDocumentsNoUnknownSignature` fails if a section outlives its entry — the documentation requirement is enforced, not asserted.
