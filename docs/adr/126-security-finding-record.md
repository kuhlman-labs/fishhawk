---
id: ADR-126
title: "Security findings are confidential records from intake to fix, with human-only disposition"
status: accepted
date: 2026-10-28
issue: https://github.com/kuhlman-labs/fishhawk/issues/4769
supersedes: []
superseded_by: []
amends: ["ADR-103", "ADR-117"]
applies_to: []
---

# ADR-126: Security findings are confidential records from intake to fix, with human-only disposition

## Status

**Accepted (v2.1).** Ratified by the Captain on 2026-10-28. Drafted by the first officer the same day at the Captain's direction. v2.1 folds the v2 confirmation round: fable (two blockers: where the cut happens and where pre-gate text lives; the disclosure-state gate) and astra (complete-payload confidentiality, including historical rows; an intake store; attested remediation; public-issue timing; release carry-forward).

**Review history.** v1 was reviewed by gpt-6-astra and claude-fable-5-1, and both said revise.
- **Split (fable).** The release half (affected ranges, advisory publication) amends ADR-051 machinery that cannot be exercised yet: no tag has ever been cut (#3716, #4546), published Releases are immutable (#4521), and there is no ancestry oracle beyond `CompareCommits`. v2 covers the record and intake only. The release integration is a second ADR, written after #4521 and the first tag; its starting requirements are listed under Consequences.
- **Leak paths (both).** The reproduction leaks upstream of any record today, through concern notes. v2 makes security concerns confidential at ingest.

Ratified by the Captain.

## Context

**Sources:**
- the security reviewer persona (ADR-084);
- the diff-secrets and permission-drift checks (E80.3, E80.4);
- ADR-117 escapes with `category=security`;
- user reports (E81);
- the adversarial probe (ADR-103, accepted, not built);
- operators.

The dependency advisory watch (E80.2) reports upstream advisories that are already public.

**The live leak.** In this public repository, a security reviewer concern's free-text note already reaches:
- the public PR review comment (`issuecomment/pr_review_template.go`);
- the release notes, when the concern is deferred (`releaseevidence/assemble.go` → `releasenotes/render.go` → `release_publish.go`, which sets the public Release body);
- the implement-review prompt (`prompt/prompt.go`).

Nothing distinguishes a security concern's text from any other.

**The filing gap.**
- **No signal.** `FilingRequest` carries no severity, and `fishhawk_file_issue` has no security input, so ADR-103 D6's "refuse a public HIGH filing" has nothing to act on.
- **Paths.** Most filing paths funnel through `applyAndFileWorkItem`. Product-report feedback (`workmgmt/github/feedback.go`) bypasses it.
- **Dedup.** Intake dedup can fold a proposal into an existing public issue.

**What the operator did by hand** (2026-10-22 to 10-28). For three HIGHs, the overseer created draft advisories, wrote fix commits back, and decided disposition (closed unpublished on 2026-10-28: pre-alpha, never shipped). #4516 records the convention.

## Options

- **A. Keep it an operator convention (#4516).** Rejected: the leak paths are product paths, and a convention cannot close them.
- **B. One ADR covering intake through publication (v1).** Rejected for now: its release half cannot be specified against machinery that does not exist yet.
- **C. Record and intake now; release disclosure in a second ADR once releases exist (recommended).**

## Recommendation

**Option C.** It amends ADR-103 D6 (its confidential filing generalizes to every source and creates this record) and ADR-117 A1.5 (`advisory_id` is re-pointed).

**D1. The record.**
- **Fields.** A security finding is a product record with:
  - `security_finding_id`, repository, `security_class` (a closed slug), severity, CWE;
  - source;
  - the public issue reference (class plus fix contract only);
  - the confidential reference (the provider's private object, which holds the reproduction);
  - linked runs, fix commit(s), introducing commit(s) when known, and an upstream CVE or GHSA id when one exists.
- **History.** Append-only audit entries. The record holds identifiers and allow-listed metadata, never reproduction text.
- **No record needed.** E80.2 upkeep findings about already-public upstream advisories stay on their public path.

**D2. Two dimensions, not one line.** This keeps the later release ADR expressible.
- **Remediation:** `open → fixing → fixed`, plus `reopened`. `fixed` is attested (D6), never inferred from a merge. A revert of a fix commit, or a new reproduction, appends `reopened` and invalidates the earlier fix claim.
- **Triage:** `pending → accepted | rejected | duplicate(survivor_id)`.
- **Disclosure** is a third dimension the release ADR will define, starting as `undisclosed`, with `held`, `published` and `closed_unpublished` reserved for it. Until that ADR lands, disclosure stays `undisclosed`. The operator may set only `closed_unpublished`, with a reason, at the D5 gate.
- **What evidence can and cannot do.** Server-observed events (a run linked, a merge, a revert) may append evidence and advance remediation. They never change triage or disclosure.

**D3. Confidential at ingest.**
- **What is confidential.** Every `category=security` concern is confidential at ingest, regardless of its asserted severity, and so is every crew finding from the `security` role. Persona severity is agent-asserted and untrusted, so the floor governs public filing (D4), not ingest confidentiality. Server-check concerns (diff secrets, permission drift) are trusted.
- **What is reduced.** The COMPLETE source payload: the note, `free_form`, evidence, patches, resolution and state reasons, and any later evidence or reason attached to the concern. Classification happens BEFORE any severity clamp, and an automatic downgrade never removes protection.
- **Where the cut happens.** The reduction happens on the verdict BEFORE the review-terminal audit row is appended, and before any concern row is inserted. The audit payload, the concern row, crew messages (including `crew_finding` conversions) and every projection carry class plus location references only.
- **Projections filter by category.** Every projection (PR and activity comments, the gate view, audit and MCP reads, prompts, release evidence) renders allow-listed metadata for `category=security` concerns AT PROJECTION TIME. That also covers historical rows stored before this decision.
- **Where the full text lives.** In a repository-scoped confidential intake store, before any provider object exists; the audit chain records only identifiers. It is withheld from run-bound reads, the audit read APIs and MCP reads, logs (including the review-verdict write-failure log) and gate echoes. It is readable only under the D5 gate predicate, and it is copied to the provider's confidential reference when a human creates one. A failed transfer keeps the intake pending and never falls back to ordinary storage.
- **What the public sees:** every public projection (PR review comments, activity comments, release-notes evidence) renders the class and the public issue link only.
- **Deferral.** `fishhawk_defer_concern` on such a concern files under D4, with the concern's class.
- **What agents see:** the fix agent and later reviewers receive the fix contract, not the reproduction. A reviewer that must re-check a fix reads only the location references.
- **Probe artifacts.** ADR-103's reproduction-bearing probe artifacts are confidential source artifacts. They are readable only with explicit repository-scoped access, and they never enter ordinary prompts, notes, gate echoes, logs or audit projections. This amends ADR-103 D5's retention to that access rule.

**D4. One filing contract, fail-closed.**
- **The declaration.** Every filing declares one of:
  - `security: {class, severity}`;
  - `security: {class, severity, upstream: <GHSA/CVE id>}`, for an already-public upstream advisory, which may file publicly;
  - `security: none`.

  A label is never the signal.
- **The refusal.** `applyAndFileWorkItem` and EVERY provider-File path, including product-report feedback, refuse a public filing at or above the floor unless the confidential reference exists. On a provider without the `confidential` capability, or with `none` declared, the refusal is `work_item_confidential_unavailable`. The record is kept pending, and nothing is filed publicly.
- **Unknown severity** stays confidential until triaged.
- **The floor** is HIGH and cannot be lowered. A repository may RAISE protection to cover lower severities.
- **When the public class issue is filed.** At the gate (D5), with class and fix contract only. It may be filed BEFORE the fix merges, because the issue-driven fix loop needs it. This matches ADR-103 D6, where only the reproduction waits for the fix. Nothing beyond the class and fix contract becomes public before the release ADR's disclosure decision.

**D5. Human-only disposition.**
- **What needs the gate:** creating the confidential reference for a proposal; filing the public class issue; and every triage disposition (accept, reject, duplicate naming its survivor). So do severity, class and attribution changes, and reference or policy changes.
- **The gate predicate:** a forge-verified credential of kind `session`, `oauth` or `oauth_as`, derived per ADR-123 D1. It refuses an agent-kind subject and the `delegated` marker independently. A standing order never covers it. Residual (ADR-113, ADR-117 A1.3): a process holding an admitted human credential is indistinguishable from the human.
- **Proposals cannot vanish.** Agents PROPOSE. Every proposal is an audit row, so a proposal cannot disappear between its source and the gate.
- **Dedup never dispositions.** Dedup never auto-dispositions a security proposal: `duplicate` is a human disposition naming the survivor.
- **Transitions** are version-checked and atomic, including for findings with no run.

**D6. Fix evidence and attestation.**
- **On merge.** When a run linked to a record merges, the merge path appends the commit as CANDIDATE fix evidence and advances remediation to `fixing`, not `fixed`.
- **`fixed`** requires an explicit remediation attestation under the D5 gate predicate, bound to the finding revision and its complete fix set.
- **Reverts.** A revert of any commit in the set appends `reopened` and invalidates the attestation.
- **Disclosure** is unchanged throughout.

**D7. ADR-117 A1.5 re-pointed.** An escape's `advisory_id` becomes `security_finding_id`, and the record carries the provider's advisory id. Neither field exists in the tree yet (#4694 is replanning), so this changes no stored data.

**D8. Portability (ADR-110).**
- **What a repository declares:** its confidential target (`github_security_advisory`, `gitlab_confidential_issue`, `jira_security_level` or `none`) and any raised floor.
- **Privacy strength is stated per provider** in `docs/board-capability-matrix.md`:
  - a GitHub draft advisory is visible to admins and invited collaborators;
  - a GitLab confidential issue is readable by Reporter and above;
  - a Jira security level needs administrator setup.
- **Unsupported targets.** `none`, an unsupported provider, or unverifiable privacy keeps the record and refuses public filing at or above the floor.
- **GitHub credentials.** GitHub requires `repository_advisories: write`, an Auth change checklist item.

## Decision

**Accepted 2026-10-28:** ratified by the Captain (v2.1). Adopt option C as stated in the Recommendation (D1–D8).

## Consequences

- **Gains:**
  - today's live leak path through security concern notes closes;
  - every source files through one fail-closed contract;
  - a finding cannot be hidden by an agent or by dedup;
  - the operator ritual becomes product behaviour;
  - the release ADR gets a record to build on.
- **Costs:**
  - the record and its audit categories (`backend/internal/audit/categories.go`, plus `issuecomment` `activityCategories` and the run-bound read deny-list);
  - ingest-time confidentiality for security concerns;
  - the `security` filing declaration and the choke-point refusal (including the feedback path);
  - the provider `confidential` capability (#4516: GitHub draft advisories and GitLab confidential issues first, Jira refusing);
  - the gate;
  - fix evidence, remediation attestation and revert detection.
- **Out of scope:** CVE assignment (the record stores an id), embargo windows, multi-repository findings, and automated introducing-commit attribution (E94.5 may supply it later).
- **The release ADR, filed after #4521 and the first tag (#3716), starts from these requirements:**
  - every published version is classified affected, unaffected or unknown, from immutable release identities and reviewed per-release-line introduction and fix evidence, with tag-containment ancestry as a new forge seam and ancestry as supporting evidence only;
  - an unknown introducing commit recommends `hold`, except in a repository that has never published a release, where it recommends `close_unpublished`;
  - fixes and `released` are recorded per release line;
  - a dedicated, non-delegable disclosure decision binds the finding revision, release identity, affected versions, exact public payload and embargo conditions, and changed evidence invalidates it;
  - publication reconciles provider state on retry;
  - urgent pre-release publication has its own path;
  - every release reconciles open and held findings, including ones discovered after a version shipped;
  - a reopened or replacement record links to its predecessor and preserves advisory identity and history;
  - complete fix sets and backport equivalence are defined;
  - `closed_unpublished` needs affirmative evidence that no affected distribution exists;
  - publication needs confirmation of the required distribution;
  - terminal states need a confirmed provider outcome.

  Until that ADR is implemented, product publication of an advisory is UNAVAILABLE. Only the D2 `closed_unpublished` operator disposition exists.
- **Sequencing:**
  1. ingest-time confidentiality for security concerns (closes the live leak);
  2. the filing declaration and the choke-point refusal;
  3. the record and the gate;
  4. the provider capability (#4516);
  5. fix evidence and remediation attestation.
- **Autonomy:** `autonomy:low` for the gate and the provider capability.
- **Record metadata.** amends: ["ADR-103", "ADR-117"]; supersedes: []; applies_to: []
- **Related:** ADR-051, ADR-084, ADR-110, ADR-113, ADR-121, ADR-123, E80 (#3701), #4516, #4521, #3716, #4694, E94.5 (#4696).
