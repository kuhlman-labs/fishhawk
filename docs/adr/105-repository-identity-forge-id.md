---
id: ADR-105
title: "Repository identity keyed on the forge's immutable repository id; names resolve through a time-bounded history; an owner change re-establishes trust (amends ADR-057, ADR-061)"
status: accepted
date: 2026-10-17
issue: https://github.com/kuhlman-labs/fishhawk/issues/4550
supersedes: []
superseded_by: []
amends: ["ADR-057", "ADR-061"]
applies_to: []
---

# ADR-105: Repository identity keyed on the forge's immutable repository id; names resolve through a time-bounded history; an owner change re-establishes trust (amends ADR-057, ADR-061)

## Context

Fishhawk identifies a repository by its `owner/name` text. That causes two failures:
- A forge-side rename or transfer splits one repository into two identities.
- Reusing an old name merges two different repositories into one.

#4542 tracks the product fix. #4549 is the interim operator command that unblocks the `kuhlman-labs/fishhawk` → `pandion-labs/fishhawk` move (E90 #4540) without it.

**What exists (at `619415db`):**
- **GitHub intake reads only the name.**
  - The parse struct carries `repository.full_name` and `installation.id` (`webhook/webhook.go:236-248`, `ev.Repo = FullName` at :274). Nothing in `webhook/` or `server/` reads `repository.id` or `node_id`.
  - Installation scaffolding reads `repositories[].full_name` only (`dispatcher.go:765-784`).
  - GitLab intake already reads `project.id` (`webhook/gitlab.go:87-92, 142-143`), but only to build the credential `gitlab:<id>`; `ev.Repo` is still `path_with_namespace`.
- **No rename or transfer handling.** `MatchEvent` covers issues, issue_comment, workflow_run, check_run, installation(_repositories), branch_protection_rule and repository_ruleset (`dispatcher.go:290-301`). It also skips any event without an installation id (:286-288). GitHub's `repository` event payload makes `installation` optional, so that guard would drop it even if it were matched.
- **No forge repository id is stored as identity.**
  - Ten tables key on repo text. Nine are `repo TEXT NOT NULL`: `runs`, `campaigns`, `refinement_filings`, `repo_acl_entries`, `decision_index`, `captain_read_watermarks`, `user_report_cursors`, `alert_incidents`, `merge_candidate_queue_entries`. The tenth is `specialist_jobs.subject_repo TEXT NULL` (0103:44).
  - The GitLab project id survives only inside credential strings (`runs.installation_ref`, `installations.installation_ref`; 0076, 0052).
  - GitLab intake authorization binds by name: `installations.project_path` (0078:34), compared exactly (`account/queries.sql:132`, `gitlab_project_path_unbound` in `gitlab_dispatch.go`). A GitLab rename therefore breaks intake, not just history.
- **The forge abstraction has no id.** `forge.RepoRef{Owner, Name}` (`forge/types.go:112-119`). The GitLab adapter resolves path → id and parses it back out of the credential scope (`forge/gitlab/gitlab.go:226-241, 277`).
- **Authority is keyed by name.**
  - `repo_acl_entries` (the forge-permission mirror) is `UNIQUE(provider, subject, repo)`, sits outside RLS, has no `account_id`, and is looked up by exact text (`repoacl/repoacl.go:186-201`).
  - Captain and delegation-confirmation entries are audit rows read by `payload->>'repo' = $2 AND account_id IS NOT DISTINCT FROM $3` (`captain/store.go:105-113`, `delegationconfirm/store.go:94-102`).
  - Both stores serialize on an advisory lock that hashes the repo TEXT (`captain/store.go:81-93`, `delegationconfirm/store.go:69-87`, pinned byte-identical by `TestAppend_SharesCaptainLock`).
- **Tenancy is per owner.**
  - `accounts(provider, account_key, granularity)`: `account_key` is the owner login, at `user`/`organization`/`enterprise` granularity (0052, 0077).
  - GitHub runs reach an account through their installation (`AccountIDForInstallation`, `dispatcher.go:1365-1374`; ADR-061: an installation belongs to exactly one workspace).
  - On GitHub a transfer to a different owner therefore normally means a different installation AND a different account partition, and RLS hides every RLS-covered table (runs, campaigns, decision_index, watermarks, cursors, audit_entries) across it.
  - `Dispatcher.Accounts` has no production wiring today, so local deployments run with a NULL account.
- **Name-keyed config.** `FISHHAWKD_SCHEDULER_REPOS` (`serve.go:2043`, `schedules.go:102`), `FISHHAWKD_OPERATOR_REPO` (`serve.go:2120`).
- **Case handling is inconsistent.** GitHub and GitLab names are case-insensitive. 14 Go sites in 11 files compare repos with `strings.EqualFold` (e.g. `workitems.go:418`, `plan_ticket_guard.go:168`, `mergeoutcome/ticker.go:395`). The ACL lookup, both audit reads, `campaign_grooming_source.go:244` and GitLab `project_path` compare exactly, and no SQL uses `lower()`.
- **Name-addressed surfaces.**
  - API: 8 `/v0/repos/{owner}/{name}/...` routes and 7 `repo` query params (`docs/api/v0.openapi.yaml`).
  - MCP: 43 tool params (`repo`, some `github_repo`).
  - 24 `splitRepoFullName` call sites.
  - The frontend route `repos/:owner/:name` (`frontend/src/App.tsx:37`) and the CLI `--repo` flag.

**What breaks:**
1. **Rename.** New events carry the new name, so every earlier row is orphaned:
   - ACL mirror rows, delegation confirmations and the captain seat stop applying;
   - watermarks and cursors reset;
   - the decision index loses history;
   - `/health` and `/throughput` restart from zero;
   - on GitLab, intake authorization fails.
2. **Transfer.** Everything in (1), plus on GitHub a new installation and a new account partition. Even a correct re-key leaves the history in the old tenant's partition.
3. **Name reuse (security).** After a transfer, a NEW repository created under the old `owner/name` inherits the old one's mirrored ACL grants (until re-checked), delegation confirmations, captain seat and history, because nothing distinguishes the two.
4. **Case variants** of one name resolve differently depending on which code path reads them.

**External behavior.** Verified unless marked otherwise:
- GitHub's `repository` event has `renamed` (`changes.repository.name.from`) and `transferred` (`changes.owner.from`) actions and is deliverable to Apps with Metadata:read. Which installation receives `transferred` is not documented.
- `GET /repos/{old}/{name}` answers `301 Location: /repositories/{id}`, and the redirect is deleted when the old name is reused.
- GitLab project and group webhooks have no rename or transfer event. Instance system hooks (`project_rename`, `project_transfer`, admin-only) carry `project_id` and `old_path_with_namespace`. Path redirects last until the old path is reclaimed, but the API has historically 404'd on old paths (gitlab-org/gitlab#30013).
- Id stability across rename and transfer is observed on both forges (GitHub's `/repositories/{id}` redirect target) but stated in neither vendor's docs.

Related: ADR-057 and ADR-061 (tenancy, RLS, workspace ↔ installation), ADR-058 (GitLab, `gitlab:<project_id>` credentials), ADR-055 (forge-verified approval identity), ADR-080 (the repository is the unit of operation), ADR-101 (precedent: `issues.transferred` voids eligibility).

## Options

- **A. Status quo plus the operator re-key command (#4549) as the supported path.**
  - Every rename or transfer needs a human to notice it and run a command.
  - Name reuse stays open.
  - Rejected as a product answer; kept as the interim tool.
- **B. Re-key the text on rename/transfer events.** Handle `repository` `renamed`/`transferred` and `UPDATE` the name everywhere. No schema change, but:
  - an undelivered event leaves the split silently;
  - audit rows cannot be rewritten, so the captain and delegation reads still need an alias;
  - name reuse stays open.
  - Rejected.
- **C. Name alias table only.** Keep text keys and add `old → current` aliases. This fixes lookups after a rename but not name reuse, unless the alias records the forge id, which is option D without the key change. Rejected.
- **D. Installation repository list as the source of truth.** Treat each installation's `GET /installation/repositories` (id + full_name) as the registry.
  - It is good as a backfill source and a drift sweep.
  - It is not an identity model: it is per installation (so it doesn't survive a transfer between installations), and GitLab has no equivalent.
  - Folded into E as a mechanism, not chosen as the model.
- **E. Key identity on the forge's immutable repository id; names become a resolved, time-bounded history (RECOMMENDED).**

## Recommendation

Option E. This amends ADR-057 (the repo-ACL key) and ADR-061 (workspace ↔ installation, D5's adopt).

**D1 One repository identity.**
- A new `repositories` table:
  - `id UUID` primary key;
  - `provider`;
  - `forge_host` (github.com, a GHES host, a GitLab instance);
  - `forge_repo_id` (text: the GitHub numeric repository id, the GitLab project id);
  - `account_id` (nullable: the owning account at last observation);
  - `display_name` (the current `owner/name`, in forge case);
  - `UNIQUE(provider, forge_host, forge_repo_id)`.
- **Numeric id, not GraphQL `node_id`.** GitHub changed its global-id format in 2021, and the numeric id is what the rename redirects expose.
- Every repo-keyed table gains `repository_id UUID REFERENCES repositories(id)`. The existing `repo` text column stays as the name observed at write time and stops being a key.
- `forge.RepoRef` gains `ID` and `Host`; `String()` is unchanged.

**D2 Names are a history, matched case-insensitively.**
- `repository_names(provider, forge_host, name_folded, display_name, repository_id, first_seen, verified_at, superseded_at)`.
- A partial unique index `ON (provider, forge_host, name_folded) WHERE superseded_at IS NULL` enforces one live owner per folded name.
- Matching folds case everywhere, ending the exact-vs-`EqualFold` split. Display keeps the forge's case.

**D3 One resolver for every name-addressed surface.**
- Webhooks, routes, query params, MCP params, the frontend route, the CLI and the name-keyed config keep accepting `owner/name`, so there is no API break. They resolve through one function: name → live claim → `repository_id`.
- An event that carries the forge id (`repository.id`, `project.id`) resolves by id, never by name.
- **Authority-bearing reads never trust a stale claim.** This covers ACL checks, captain and delegation reads, approval eligibility and token minting for `FISHHAWKD_OPERATOR_REPO`. When the name's claim is superseded, or its `verified_at` is older than a bounded TTL, the resolver re-fetches at the forge and compares ids before answering. That closes the window between a new repository's creation under a reused name and Fishhawk's first event from it.
- Re-resolution of a known repository is id-addressed: `GET /repositories/{id}` or `GET /projects/:id`. Name-addressed lookups (`GET /repos/{owner}/{repo}`, `GET /projects/:path`) are used only for names not yet seen.
- A superseded name resolves to its repository only until a different forge id claims it; from then on it resolves to the new repository. That ends the alias and closes name reuse.

**D4 Drift detection, not event dependence.**
- Every inbound event compares (forge id, payload name) against the live claim and records a rename when they differ.
- GitHub `repository` `renamed`/`transferred` events and GitLab system hooks (where an operator configures them) are accelerators, not dependencies. The `repository` event bypasses the installation-id skip (`dispatcher.go:286-288`), since its `installation` is optional.
- A periodic sweep per registered installation (`GET /installation/repositories`) catches renames that produced no event.

**D5 Owner and account changes re-establish trust.**
- **Rename** (same owner): carries everything — history, ACL mirror, delegation, captain seat.
- **Owner change within one account** (an enterprise-granularity account spanning both owners, or a NULL-account local deployment): carries history but not authority.
  - ACL mirror rows are invalidated and re-fetched.
  - Delegation confirmations and the captain seat are suspended, fail closed, until the captain re-confirms.
  - In-flight runs are not cancelled; they block at their next gate that needs the suspended authority.
- **Owner change across accounts** (the normal GitHub case: a user → an org, or org → org):
  - The repository needs the new owner's installation registered.
  - History stays in the old account's partition (RLS unchanged), and the new account starts empty for that repository.
  - Linking the prior history requires an explicit **adopt**. It must be authorized by the PRIOR account's captain, or an operator who holds both seats; it is audited in both partitions, refused when the prior account is unknown, and never carries authority. Adopt is a disclosure of one tenant's history to another, so the giving side authorizes it.
  - The new account's captain confirms delegation fresh.
- **Tables outside RLS** state their rule:
  - `repo_acl_entries`: rows invalidated on any owner change.
  - `merge_candidate_queue_entries`: entries for the repository are re-admitted under the new identity.
  - `alert_incidents`: follow `repository_id`.

**D6 The audit log is never rewritten.**
- New audit entries carry `repository_id` alongside the observed name.
- The captain and delegation-confirmation reads, and per-repository audit queries, select by `repository_id`; for pre-migration rows they select by every name in the history within its validity window, subject to D5.
- The shared captain/delegation advisory lock key hashes `repository_id` instead of the repo text, in both copies and their parity test.
- The chain and the verifier are untouched.

**D7 Migration.**
1. Add `repositories`, `repository_names` and nullable `repository_id` columns.
2. Backfill with `fishhawkd repo backfill` (dry run by default):
   - first from each registered installation's repository list (one call yields id + name for every visible repository);
   - then per remaining distinct `(provider, repo)` by forge lookup (GitHub's rename redirect resolves already-renamed repositories).
   - Rows that still don't resolve keep a NULL `repository_id` and are listed.
3. Readers resolve by `repository_id` and fall back to a folded text match for NULL rows, logged as a fallback hit so the residue is measurable.
4. Writers set `repository_id`. A `CHECK (repository_id IS NOT NULL OR created_at < <cutoff>)` per table makes it mandatory for new rows without touching history.
5. Rollback: the columns stay nullable, and the text column is retained throughout.
6. #4549's text re-key is retired, or kept as the manual path for unresolved rows.

Implementation notes for this repo:
- `sqlc generate` output is hand-edited when generation aborts.
- The patch-coverage gate applies to the resolver and its readers, not to migrations.

**D8 Out of scope.**
- Cross-instance moves (GitHub.com ↔ GHES, GitLab instance ↔ instance) mint a new identity and use D5's adopt.
- Repository deletion and restore keep their id on GitHub and are covered by D3.
- Forks are separate repositories.

## Decision

Accepted (2026-10-17) by the Captain: Option E as recommended, D1–D8. Amends ADR-057 (the repo-ACL key) and ADR-061 (workspace ↔ installation, the two-sided adopt).

## Consequences

- **Positive.**
  - Renames need no operator action.
  - Name reuse no longer inherits authority.
  - One case rule everywhere.
  - A transfer becomes an explicit, two-sided trust event instead of a silent split.
  - The API, MCP, frontend and CLI keep their name-based shape.
- **Negative.**
  - A migration across ten tables, the audit read paths and the lock key; every repo-keyed query moves to `repository_id`.
  - Authority-bearing reads add a forge round trip whenever a claim is stale (bounded by the TTL).
  - The backfill needs forge access for every historical repository.
  - Cross-account transfers need work from BOTH captains (adopt plus re-confirmation) and the new installation. Our own `kuhlman-labs` → `pandion-labs` transfer would need that if it ran after this ships. Before this ships it runs on #4549, and local deployments have a NULL account.
- **Follow-ups.**
  - Schema and migration (D1, D2, D7).
  - Webhook parsing of `repository.id`, `repository` event handling and the installation-skip bypass (D3, D4).
  - The resolver with authority-read revalidation, and its adoption across routes, MCP, frontend, CLI and config (D3).
  - The D5 suspension and the two-sided adopt verb, with audit categories registered in `audit/categories.go`.
  - The audit reads and lock key (D6).
  - The drift sweep (D4).
  - Retiring or reshaping #4549.

Review: Fable adversarial review, 2026-10-17. Verdict on the first draft: revise (4 HIGH, 5 MEDIUM, 3 LOW). All HIGH findings were verified against the code and folded into this revision: the D5 account model, two-sided adopt, authority-read revalidation (D3), and the lock key (D6).

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":1574} -->
