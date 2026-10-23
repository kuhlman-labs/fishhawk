---
id: ADR-113
title: "ADR ratification as a forge-verified, hash-bound approval in the plan-gate approvals shape, a deterministic record writer, and catch-rate decoupling (Option B′)"
status: accepted
date: 2026-10-22
issue: https://github.com/kuhlman-labs/fishhawk/issues/4628
supersedes: []
superseded_by: []
amends: ["ADR-055"]
applies_to: []
---

# ADR-113: ADR ratification as a forge-verified, hash-bound approval in the plan-gate approvals shape, a deterministic record writer, and catch-rate decoupling (Option B′)

**Status: accepted (v5.1, ratified by the Captain 2026-10-22).**
- **Review history:**
  - v1, a full `adr_change` workflow: fable (approve_with_concerns, 12 findings) and Opus substituting for Codex during its quota outage (reject, 13 findings). Both pointed to a cheaper "Option B′".
  - v2 (device-flow step-up) and v3 and v4 (SSH-signed statements with a key roster) tried to make ratification provable as a HUMAN act, distinct from the operator session. Reviews showed that on a single-operator host this rests on how the maintainer's keys are protected, which the product cannot observe.
  - v5 (Captain, 2026-10-22) sets the goal as the same as a plan approval: verify the approver's identity against the forge, by OAuth or by an SSH signature checked against the forge's published signing keys. There is no key roster. It keeps the hash binding, the approvals shape, the record writer and the catch-rate decoupling.
  - v5 reviews: fable and Opus, both approve_with_concerns. v5.1 folds in every finding, chiefly: an OAuth credential-kind check, a single pending ratification that owns the rendered body, a pending-ratification state machine with a retryable apply, run-free predicate evaluation, and a complete residual.
- **Bootstrap:** this ADR is itself ratified through the manual path, on the Captain's explicit instruction. The first ADR the D2 verb can ratify is the one after it.
- **Freeze:** tracker-only until the 2026-11-01 catch-rate re-pin; the in-repo record follows the upkeep rule.

## Context

ADRs are tracker issues (`[ADR-NNN]`, type `adr`; numbered server-side, #1269), transcribed byte-exact into `docs/adr/NNN-slug.md` with front matter and the index `docs/adr/index.json`, and gated by `scripts/check-adr`. The architect persona reads the index (ADR-084).

Between 2026-10-20 and 10-22 the process ran by hand for ADR-103 and ADR-106 through ADR-112. Four risks showed:
1. **Review failures.** Codex hit its usage limit twice (10-21 19:36 and 10-22 06:58, out until 10-27). Reviews were retried by hand-written timers.
2. **Unbound ratification.** The ratified text is hash-bound nowhere, and the Decision is recorded by hand-editing the tracker body. Nothing records which identity ratified which text.
3. **Error-prone transcription.** Byte-equality scripts were needed each time, `applies_to` choices broke selection tests (ADR-110's blanket globs crowded out ADR-046), and records drifted from amended tracker bodies.
4. **The catch-rate coupling.** An ADR landing changes the architect persona's prompt input (it embeds the index and the matched records), so its catch-rate evidence goes stale (`backend/internal/agenteval/README.md`). Records are therefore batched behind re-pins (#4600), against `docs/adr/README.md`'s upkeep rule ("one by one").

## Options

- **A. Status quo.**
- **B′. Targeted fixes with a forge-verified, hash-bound ratification, no new workflow (recommended).**
- **C. A full `adr_change` workflow.** Deferred; recorded below as a direction with the reviewers' requirements.

## Recommendation (decided now)

**Option B′.**

**D1 — Reviewer quota is ADR-094 D6's problem, not this ADR's.**
- Product review rounds park on quota exhaustion until the reset time (ADR-094 D6, implementation #4242).
- Until #4242 lands:
  - **Implement reviews:** `fishhawk_rerun_review` (ADR-095 D2) re-runs only a NON-GATING implement-review round with zero verdicts.
  - **Plan reviews:** there is no re-run verb; the operator recovers with a no-change `fishhawk_revise_plan`.
  - **A reviewer out for days:** the operator substitutes it by a governed spec change (as #4624 did on 2026-10-22).
- Hand-run ADR reviews are outside the product. That a quota failure counts as a non-verdict, never a verdict, is a convention there, not a control.
- This ADR adds no retry classifier.

**D2 — Ratification is a forge-verified approval, bound to the exact body.**

*Who may ratify (the approvals predicate).*
- **Declaration.** The ratification approvers are an `approvals` block with exactly the shape of a plan gate's (workflow-v2 `$defs/approvals`, ADR-055): `count` (required), `members`, `min_permission`, `member_of` and `not: [author, agent]`. It is declared at `types.adr.ratify.approvals` (D5).
  - It is read from the default-branch blob only, never from a break-glass override, and its blob SHA is recorded.
- **Default and this repository.** With no declaration, ratification is disabled with a named reason. This repository declares `{count: 1, members: ["github:kuhlman-labs"], not: [agent]}` (Captain, 2026-10-22): `author` is dropped because the maintainer may open an ADR issue themselves.
- **Evaluation.** The plan gate's run-free leaves are reused through one extracted function, `evaluateApprover(ctx, forge, repo, subject, approvals)`, which both the plan gate and ratification call, pinned by a parity test:
  - `identity.SubjectListed` for `members`;
  - `resolvePredicates` for `min_permission` and `member_of`, with `repo` the repository hosting the ADR issue and `forge` the conventions loader's provider. It needs a configured identity provider with a REST token accessor, not a device client;
  - `eligibleApprover`, given ratification's own author resolver (the issue opener) and `isAgentSubject` (`actor.go`) for `agent`;
  - no escalations apply.
  - The `predicate_snapshot` struct is recorded.
  - Counting uses a new pending-ratification store, not `ApprovalRepo`, because there is no run or stage.

*How identity is verified (against the forge).*
- **(a) OAuth.** The submitting bearer must be an API token with `AuthMethod == "oauth"`.
  - Static tokens, `fho_` authorization-server tokens and cookie sessions are refused with `ratify_oauth_required`.
  - This is STRICTER than a plan gate today: a plan gate's `members` leg checks the subject's shape, not the credential kind.
  - **Prerequisite:** the OAuth path depends on #4627, which binds the token login to the deployment's OAuth client.
- **(b) SSH signature.** The approver signs the canonical statement with `ssh-keygen -Y sign -n fishhawk-adr-ratify`. The server verifies the signature under namespace `fishhawk-adr-ratify` and requires the signing key to be among the subject's forge-published SSH SIGNING keys:
  - GitHub `GET /users/{login}/ssh_signing_keys`; GitLab user keys whose `usage_type` covers signing;
  - read at decision time, paginated;
  - fingerprints compared as `SHA256:` unpadded base64;
  - a 404 or a rate-limited read refuses (fail closed).
  - This needs `IdentityProvider.ListSigningKeys` on both providers.
  - The forge's numeric user id and the key observation are recorded.
  - The signature is independently verifiable against the forge's key list, as long as that key stays published.
- `ratify.methods` (D5) lists the accepted methods; the default is both.

*Routes.*
- `POST /v0/adr/{issue}/ratification-challenge`, `POST /v0/adr/{issue}/ratification`, `POST /v0/adr/{issue}/ratification/apply` (retry) and `GET /v0/adr/{issue}/ratification` (state, used by `fishhawk adr record`).
- **Authorization:** authenticated, a new `adr:ratify` scope, an account binding to the issue's repository, and never an agent subject (`isAgentSubject`). Cookie sessions require the admin role through a new run-less enforcement helper.
- **Rate limit:** a per-subject limiter in the shape of `oauthCIMDLimiter`.
- **Audit:** rows go on the global chain (no run id).

*The pending ratification.*
- **Creation.** The FIRST challenge on an issue creates the pending ratification and fixes:
  - the verdict, the decision text and the render date (UTC);
  - the rendered final body, with `base_body_sha256` and `final_body_sha256`;
  - the approvals blob SHA.
- **Joining.** Later challenges JOIN it. A `--verdict` or `--decision` flag that differs is refused; both may be omitted.
- **Challenges.** One live challenge per (issue, subject), with an expiry of at most 15 minutes. The CLI prints the statement and the final body's Status and Decision lines before approving.
- **States:** `collecting → completing → applied | apply_failed | discarded`.
  - A change to the base body, or `ratify.pending_ttl` (default 7 days), moves it to `discarded`.
  - "Count met" is decided inside the approval's transaction under `SELECT … FOR UPDATE` on the pending row.
  - At completion every counted subject is RE-EVALUATED against the current predicate. If any no longer qualifies, or the approvals blob SHA changed, the ratification stays `collecting` and the affected approvals are dropped.

*The canonical statement.* UTF-8, LF line endings, one field per line in this order, exactly one trailing LF:
  ```
  fishhawk-adr-ratification v1
  repo: <owner/name>
  issue: <decimal>
  adr: ADR-<NNN>
  verdict: accept|reject
  subject: <provider-qualified subject>
  approvals_sha256: <64 lowercase hex>
  base_body_sha256: <64 lowercase hex>
  final_body_sha256: <64 lowercase hex>
  date: <YYYY-MM-DD, UTC, the approver's own date>
  nonce: <base64url, as issued>
  expires: <RFC 3339 UTC, byte-exact as issued>
  ```
  An editorial re-ratification (D6) adds `prior_final_body_sha256: <64 lowercase hex>` after `final_body_sha256`.

*Submission and order.*
- **Submission.** `POST …/ratification` takes {statement} plus, for (b), {signature}. The server proceeds only if all of these hold:
  - the statement equals the challenge's statement bytes;
  - for (a), the bearer's subject equals `subject`;
  - for (b), the signature verifies and the key is published for `subject`;
  - `subject` satisfies the approvals predicate.
- **Order:**
  1. Verification.
  2. Idempotency: an approval already accepted for the same issue, subject and `final_body_sha256` consumes the nonce and returns the current state, but only while the pending ratification is `collecting` or `applied`.
  3. In one transaction: consume the nonce, add the approval, and decide whether the count is met (moving to `completing`).
  4. When `completing`: the apply. Success writes `adr_ratification_applied` and moves the ratification to `applied`.
  - **Failure:** writes `adr_apply_failed` with reason `tracker_body_changed` (then `discarded`) or `write_failed` / `verification_failed` (then `apply_failed`).
  - An `apply_failed` ratification is retried by `POST …/ratification/apply`, which re-applies the same rendered body.

*Rendering and apply.*
- **Rendering.** The final body is the base body with exactly two deterministic changes:
  - **The Status line.** The first line beginning `**Status:` is replaced WHOLE by `**Status: accepted (ratified YYYY-MM-DD).**` or `**Status: rejected (YYYY-MM-DD).**`, using the render date.
    - When there is no such line (the `adr` body skeleton carries none), it is inserted as the first line, followed by one blank line.
  - **The Decision section.** It must have exactly one heading that is exactly `## Decision`, else `adr_body_unrenderable`. Its content becomes `**Accepted YYYY-MM-DD** by forge-verified approval (<count> of <count>; approvers in the ratification comment). <decision text>`, or the same template with `**Rejected**`.
    - The body names no approver, so every approver approves the same final body.
    - `check-adr` validates `accepted` against the acceptance word this supplies, and the record's `date` comes from it.
- **Apply (read-compare-write-verify).** No tracker API offers a conditional body write, so:
  1. re-read the body and its `updated_at` (the GitHub client gains `updated_at`), and require `H(current) == base_body_sha256`;
  2. write the final body;
  3. re-read and require `H(current) == final_body_sha256`.
  - **Residual:** an edit landing between steps 1 and 2 is overwritten and invisible to step 3. A changed `updated_at` with a matching hash is recorded as a warning.
  - **On success:** post the approvers, their methods and every counted statement (with its signature, for SSH) as an issue comment, and close the issue (completed on accept, not planned on reject).
- **The hash function `H`.** `sha256` over the body after: CRLF → LF; trailing whitespace at the end of the document removed; exactly one trailing `\n`. Nothing else changes.

*Audit and vocabulary.*
- **Rows:**
  - `adr_ratification_approved`, one per accepted approval;
  - `adr_ratified`, when the count is met;
  - `adr_ratification_applied`;
  - `adr_apply_failed`.
  All four are registered in `categories.go`; the issue comment is an issue-comment surface (`docs/issue-comment-surfaces.md`).
- **What they carry:**
  - the statements and signatures;
  - the subjects and the forge user ids;
  - both hashes and the approvals blob SHA;
  - the key observations;
  - the `predicate_snapshot`.
- **ADR-055 amendment:** `auth_method` gains `ssh_signature`, and `channel` keeps its meaning. An SSH approval also records `submitted_by` (the bearer subject and its `auth_method`). The OpenAPI `auth_method` enum widens accordingly.

*Claims.*
- **What is claimed.** For each of `count` subjects that satisfy the approvals predicate at completion, the forge vouched for that identity (an OAuth credential bound to this deployment's client, or a key published on the forge account at decision time), and it approved exactly this base body, final body and verdict. That is at least the assurance of a plan approval.
- **What is not claimed:** that a human, rather than a process holding that identity's credentials, acted. On a single-operator host the operator session can hold:
  - a device-flow token (the forge vouched for the identity when the token was minted, not at ratification);
  - the forge CLI token;
  - a signing key loaded in a reachable ssh-agent, or an unprotected key file;
  - and any forge credential with signing-key write scope, which can publish a new key and then sign.
  The approvals predicate is also repository configuration the operator session can edit. The control against an operator agent ratifying is procedural: the maintainer's rule that the agent never ratifies. Direction H records the stronger options.
- **No delegation.** The standing plan-approval order never covers ratification.

**D3 — A deterministic record writer.**
- `fishhawk adr record <issue>` (CLI) writes `docs/adr/NNN-slug.md` plus a sidecar `docs/adr/ratification/NNN.json`:
  - **Body:** the tracker body, refused unless `H(body)` equals the `final_body_sha256` of the newest `adr_ratification_applied` row, of either verdict (read through `GET /v0/adr/{issue}/ratification`).
  - **Sidecar:** each counted approval's statement, subject, forge user id and method, plus the signature and key observation for SSH approvals, and the audit sequence of the `adr_ratification_applied` row.
  - **Front matter:**
    - `id`, `title`, `date` and `status` come from the statement and the rendered Decision; `issue` comes from the tracker URL; a new record gets `superseded_by: []`;
    - `amends`, `supersedes` and `applies_to` come from the body's record-metadata line (below).
    - No new front-matter keys and no index fields.
  - **Reciprocal edges:** the writer adds `amended_by` / `superseded_by` to the referenced records, editing only their front matter.
    - A referenced record that does not exist is refused, naming the backfill.
    - An older record's `status` changes only on a full `supersedes`, never on `amends`.
- **The record-metadata line.** Exactly one line in the Consequences section:
  ```
  - **Record metadata.** amends: [<ids>]; supersedes: [<ids>]; applies_to: [<globs>]
  ```
  - Each value is a JSON array; all three keys are present, in this order.
  - The line is authoritative over the README's text-inference rules for these keys.
  - Ratification refuses a body without a well-formed line (`adr_metadata_line_invalid`).
  - The line's `applies_to` globs are still subject to `check-adr`'s stem-in-body rule.
- **The hashed record region** is the text after the `# ADR-NNN: <title>` line and the blank line following it, normalised by `H`. It equals the statements' `final_body_sha256`.
- **`check-adr` rules (offline):**
  - For records at or after the cutoff (`record.signed_from`, D5), the sidecar is REQUIRED.
  - Every statement's `final_body_sha256` must equal `H(region)`.
  - The record-metadata line must equal the front matter.
- **`check-adr --online` (opt-in, run by the operator before merging a record):** `check-adr` is Python, so it execs a CLI verb, `fishhawk adr verify-record <path>`, built on a Go sshsig verifier (`golang.org/x/crypto/ssh`; none exists in-tree, and the gate image has no `openssh-client`).
  - The verb verifies each SSH signature against the forge's current keys for the recorded user id.
  - A key that is no longer published is reported distinctly, as a warning, not as a signature failure.
- **What these checks are:** drift checks. The sidecar and `record.signed_from` are repository files the operator session can edit. The external evidence is the forge-published keys (for SSH approvals) and the audit chain.
- **Landing:** the record lands through an operator PR or a `human_led_change` run (governance text never lands unattended), in the same change as the ratification or the change immediately after (the upkeep rule).
- **Repository pin tests** (the `decisionrecord` and agenteval edge tables) stay a reviewed edit in the same PR.
- **README fallback.** The README's "read from the tracker" fallback reads `--state all`, because ratification closes the issue before the record lands.

**D4 — Decouple ADR churn from catch-rate staleness.**
- **Frozen tree.** The architect catch-rate arm renders against a FROZEN decision-record tree: a committed directory holding the index plus every record any corpus case matches, committed with its evidence record. Production plan reviews keep reading the live tree.
- **Checks that stay LIVE in verify:**
  - governing entries present after truncation;
  - corpus premises and `min_matched_records` against the real index;
  - the index-size headroom against the 32 KiB cap.
- **Selection-equivalence tripwire (new verify leg).** For each corpus case, `decisionrecord.Assemble` over the live tree must produce the same digest over its rendered selected documents and matched index entries as over the frozen tree, not only the same Included / Excerpted / Dropped ids. A mismatch reddens verify, names the ADR, and calls for a re-pin.
  - A supersession or amendment that changes an included record's bytes therefore trips it.
- **Residual:** growth of the rest of the index, which production embeds whole, is accepted and not measured until the next re-pin.
- **Result:** an ADR with `applies_to: []` that touches no record a case includes never stales the arm. The upkeep rule ("one by one") applies again, and #4600 is the last held batch.
- **Timing:** this changes the eval instrument, so it lands WITH the 2026-11-01 re-pin (agenteval is a frozen trigger path until then). The tripwire leg is registered in `basedelta`'s leg table (ADR-096 D3).

**D5 — Configuration (ADR-110).**
- **Ratification and record configuration** go under `types.adr` in `.fishhawk/work-management.yaml`:
  - `ratify.approvals`, the plan-gate `approvals` shape (D2); its schema is pinned identical to workflow-v2 `$defs/approvals` by a test;
  - `ratify.methods` (`oauth`, `ssh_signature`; default both);
  - `ratify.pending_ttl` (default 7 days);
  - `record.location` (default `docs/adr/`);
  - `record.check`, the repository's record check, run by the record writer and failing open per ADR-110 rule 5;
  - `record.signed_from` (the first ADR id whose record must carry a sidecar);
  - `source_of_truth: tracker`.
  - This is an additive `work-management-v0` schema change (`work_item_type` is `additionalProperties: false`), made through the spec-change checklist with its embedded mirror.
  - `work_item_type` is shared by every type, so a semantic check refuses `ratify` and `record` on any type other than `adr`.
- **Fail-open:** a repository without these keys keeps today's behaviour, and the manual path remains.
- **Unsupported:**
  - a repository-first ADR model (MADR, adr-tools), until declared (ADR-110 rule 3(c));
  - SSH approvals on a forge with no published signing keys, which return `provider_unsupported` (OAuth still works).
- **Portability:** a non-Fishhawk fixture runs through the production ratify, render and record code.
- **The record file grammar** (front matter, hashed region, sidecar, record-metadata line) moves into `docs/spec/` as a product contract (ADR-110 rule 5).

**D6 — Revisions.**
- **A change to a ratified decision** is a NEW ADR carrying `amends` or `supersedes` (README model).
- **An editorial re-ratification** (of an issue that may already be closed) is allowed only when the Recommendation section, the Decision section and the record-metadata line are byte-identical to the prior ratified body.
  - Neither the Decision nor the Status line is re-rendered, so `final_body_sha256` equals `base_body_sha256`. The statement carries `prior_final_body_sha256`.
  - The server refuses it otherwise.
  - The record's sidecar is replaced by the newest approvals.
- **Grandfathering:** ADRs ratified before D2 ships (this ADR and earlier, including #4600) keep the manual path and carry no sidecar. `record.signed_from` names the first one that must.

## Directions (not decided here)

**Direction H — Proof of a human act,** if forge identity proves insufficient. Options reviewed in v2–v4:
- a hardware security key (`sk-ssh-ed25519@openssh.com` with `verify-required`), whose signatures carry server-checkable presence and verification flags;
- WebAuthn with user verification in the SPA, whose challenge is `H(statement)`;
- a signer roster rooted in a genesis, so a rewrite of host or repository state is detectable.
Each would be an additional `ratify.methods` value, not a replacement.

**Direction C — The full `adr_change` workflow,** if B′ shows a remaining gap. Its design must meet:
- the grooming shape (a plan-type `draft` stage whose approval gate is ratify-and-apply, plus a human `confirm`), with no `review`-type stage;
- a review-prompt render and catch-rate arm for ADR drafts, and decision-record selection by stated relations;
- an ADR-069 amendment for type eligibility (`on_demand` plus a required ADR-issue input);
- D2's hash-bound statement;
- a revise budget beyond the server ceiling of 3, or a fresh-run fallback that inherits the draft;
- `issue_body_set` and `close_completed` apply mutations with D2's read-compare-write-verify.

## Decision

**Accepted 2026-10-22** by the Captain (repository maintainer): **Option B′**, i.e. D1–D6 as stated in the Recommendation (v5.1), decided now. Ratification is a forge-verified approval (OAuth or SSH signature) bound to the exact body, with approvers declared in the plan-gate `approvals` shape. This repository declares `{count: 1, members: ["github:kuhlman-labs"], not: [agent]}`. Directions H and C are NOT decided. It amends ADR-055 (`auth_method` gains `ssh_signature`). Implementation follows the order in Consequences, with #4627 as the prerequisite for D2(a).

## Consequences

- **Gains:**
  - who may ratify is declared and evaluated exactly like a plan gate's approvers;
  - each approval binds a forge-verified identity to the exact base body, final body and verdict, and SSH approvals can be verified offline by anyone;
  - the tracker write is read back and verified, and records are written mechanically and checked against the ratified hash and the metadata line;
  - an ADR that touches no included record stops staling the architect arm, and the upkeep rule is restored, while content drift in included records still reddens verify;
  - reviewer quota is handled where ADR-094 already decided it.
- **Costs:**
  - three CLI verbs (`ratify`, `record`, `verify-record`) and four REST routes, with an `adr:ratify` scope, a run-less enforcement helper and a per-subject limiter;
  - `IdentityProvider.ListSigningKeys` on both providers, a Go sshsig verifier, an extracted `evaluateApprover`, and a pending-ratification store;
  - the ADR-055 `auth_method` widening, including the OpenAPI enum;
  - four audit categories and one issue-comment surface;
  - new `check-adr` rules, an `--online` leg and the sidecar;
  - a frozen-tree mechanism and a tripwire verify leg in agenteval, which need a re-pin;
  - an additive work-management schema change;
  - the record grammar in `docs/spec/`.
- **Residuals:**
  - a process holding an approver's forge credentials, or able to edit the approvals predicate, can ratify as that approver; the control is procedural (Direction H records the alternatives);
  - an SSH approval stops being independently verifiable if its key is unpublished later;
  - an edit between read and write in the apply is overwritten;
  - index growth outside included records is unmeasured between re-pins;
  - grandfathered records have no sidecar;
  - `/v0/tokens/login` is fixed separately (#4627).
- **Implementation (after ratification):**
  0. #4627 (prerequisite for D2(a));
  1. D2's routes, pending-ratification store, both methods, `evaluateApprover`, `ListSigningKeys` and the CLI verb (product, backend + CLI);
  2. D3's record writer, sidecar, `verify-record` and `check-adr` rules;
  3. D5's schema change and portability fixture;
  4. D4's frozen tree and tripwire, with the 11-01 re-pin;
  5. D6.

  D2 and D3 are agent-drivable. `.fishhawk/**` and `.github/**` are operator-authored.
- **Record metadata.** amends: ["ADR-055"]; supersedes: []; applies_to: []
- **Related:** ADR-055 (amended), ADR-069, ADR-084, ADR-094 (D6), ADR-095 (D2), ADR-096 (D3), ADR-110, E78.1 #3722, E78.13 #4471, #1269, #4116, #4242, #4600, #4627, the `record-adr` skill.
