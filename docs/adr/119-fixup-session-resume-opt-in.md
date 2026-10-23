---
id: ADR-119
title: "Fix-up session resume is opt-in and local-only, bound to the reported head and a signed anchor"
status: accepted
date: 2026-10-23
issue: https://github.com/kuhlman-labs/fishhawk/issues/4708
supersedes: []
superseded_by: []
amends: ["ADR-042"]
applies_to: ["runner/cmd/fishhawk-runner/fixupresume.go", "runner/internal/agent/claudecode/**"]
---

# ADR-119: Fix-up session resume is opt-in and local-only, bound to the reported head and a signed anchor

## Status

**ACCEPTED (v2, ratified by the Captain 2026-10-23).** Drafted by the first officer on 2026-10-23, following the Captain's ADR status rulings ("as recommended", 2026-10-23).
- **Review history:** v1 was reviewed by claude-fable-5-1 and claude-opus-5-5. Both returned reject (direction endorsed) with converging fixes, and v2 folds in every finding.
- **Freeze:** tracker-only until ratified; the in-repo record follows the upkeep rule.

## Context

**ADR-042 (#1264), as confirmed by the Captain on 2026-10-23.**
- **Status:** `accepted` (2026-06-21), recorded in PR #4707.
- **Decision:** "Option D, with B as the incremental path. True cross-stage session resume (Option A/C) is DECLINED / out of scope."
- **Condition:** resume, if ever revisited, must be "local-runner-only behind a follow-up ADR that defines the fix-up-tree attestation rule — resume the *context*, not the *trust boundary*".
- **Rationale:**
  1. Attestation: each stage's own signed trace is over its own committed tree.
  2. The key and MCP-token TTL across the stage boundary.
  3. Portability: `~/.claude` does not survive CI, and resume couples Fishhawk to one CLI.
- **Cost:** Option B was to be pursued only "if a measured post-#1164 fix-up cost still justifies it".

**What shipped.** E83.56 / #4089, PR #4583 (run e6375948, merged 2026-10-21), shipped fix-up session resume without an ADR: `runner/cmd/fishhawk-runner/fixupresume.go` and `claude --resume`.
- **When it resumes:** on an agent-invoked local implement fix-up's first attempt (`primaryResumeDecision`). A self-retry spawns fresh.
- **What it reuses:** a claude session id recorded after a successful implement or fix-up, in `<git-common-dir>/fishhawk-worktrees/run-<root>.agent-sessions.json`. A fix-up re-records, so a second fix-up resumes the latest session.
- **Fallbacks, each traced `fixup_session_resume`:**
  - the kill switch `FISHHAWK_FIXUP_RESUME=off`;
  - an unsupported agent;
  - a different binary path, model or working directory;
  - no record;
  - `self_retry`;
  - a resume failure.

  The thinking-block retry drops `--resume`.
- **The deterministic-apply path** (`appliedFixup`) emits no resume event.

**What already holds:**
- The fix-up is its own runner process, with its own key, token and signed trace (ADR-042 rationale 2).
- The CI half of rationale 3 holds, but only incidentally: `github_actions` falls back because its VM has no ledger, and there is no channel check.
- The claude-only coupling is ACCEPTED, not satisfied: codex falls back.
- The fix-up's starting tip is already pinned to the newest reported head (ADR-035, `fixup_expected_head_sha`). A tip the run did not report is fatal (`fixup_base_mismatch`).

**What does not hold:**
- **(a) Stale context.** The session is recorded after the agent loop and before the stage's commit and push. Verify-fix and base-rebase re-invokes are unrecorded. The resumed conversation may therefore reason from a tree older than the one the fix-up starts from.
- **(b) Unenveloped untrusted content.** The implement and fix-up renders carry no issue body: there is an issue link only (ADR-029). But a resume re-imports, unenveloped, whatever untrusted content the implement agent itself fetched or read: issue text through tools, file reads, verify output.
- **(c) Local trust domain.** The ledger (under the git common dir) and the transcript (under the operator's `$HOME/.claude/projects/…`, since `HOME` is allow-listed) are both writable by the unsandboxed agent and by other agents on the host. The session id reaches argv with no shape check (#4584, waived low on PR #4583).
- **(d) Trace replay.** Whether `--resume` replays implement-era history into the fix-up's SIGNED trace is unproven. If it does, ADR-042's fresh per-stage trace is broken.
- **(e) Cost.** The dollar benefit is unmeasured: the shipped claim is "≥3x cheaper in TokensUsed, not in dollars", and #4089 is open.
- **(f) No declared rule.** The attestation rule was never declared.

**Why no gate caught it.** The architect persona saw ADR-042's index line only. Its body was not selected: `applies_to: []` (`Select` skips it), and its status read `unknown` because the decision sat in an untranscribed comment. The plan review cited ADR-013 only.

## Options

- **A. Disable resume until a rule exists.** Rejected as the end state: the remaining gaps can be closed.
- **B. Ratify resume as built.** Rejected: it leaves (a)–(f) open.
- **C. Ratify resume behind an opt-in, with a context-not-trust rule and binding conditions, and off until they land (recommended).**

## Recommendation

**Option C.** This amends ADR-042: fix-up session resume is permitted on the local runner under these rules, and is OFF until D2, D3 and D5 land.

**D1. Context, never trust.**
- The fix-up's own signed trace, over its own committed tree, and the committed-tree verify gate are the ONLY vouched evidence.
- The resumed conversation is un-attested, and is not part of the vouched lineage.
- Reviewers judge the PR's cumulative diff and the gate evidence. They never judge the conversation.

**D2. Opt-in, and local only.**
- Resume is OFF by default. It runs only when the host sets `FISHHAWK_FIXUP_RESUME=on`; today's `off` value becomes the default.
- `decideFixupResume` refuses any runner kind other than `local`, with reason `unsupported_channel`.

**D3. Binding to the reported head.**
- **What is recorded.** The ledger records the session against the head the stage REPORTED: the SHA it pushed (`CommitAndPushResult`), written after the stage's final push.
- **Invalidation.** A stage in which a verify-fix or base-rebase re-invoke ran records NO resumable session.
- **On resume,** the runner compares the recorded head with the tip `checkoutFixupBase` checked out:
  - **Equal:** resume.
  - **Advanced only by commits this run reported** (the fix-up's own earlier push, a runner-authored acceptance scenario commit, a reconciled rebase): resume, with `head_advanced {recorded, current}` in the trace event. The fix-up prompt already carries the current prior diff.
  - **Otherwise:** fall back, with reason `tree_mismatch`.
- No tree SHA is recorded: a commit determines its tree.

**D4. The anchor is outside the local trust domain.**
- **What is signed.** The signed `agent_session_recorded` bundle event carries `session_id`, the reported `head_sha` and `resumes`.
- **Wire.** The backend serves the prior stage's values with the fix-up prompt fetch. This is a cross-module wire field, registered with `wirecontract`.
- **Refusal.** The runner resumes only a ledger entry that EQUALS the served values; otherwise it falls back with `session_unattested`. This also closes cross-run reuse, where a planted id points at another session on the host.
- **Session id format.** It must be an RFC 4122 UUID, checked at record time and at decide time, before any path is built from it. Otherwise the reason is `session_id_invalid` (#4584).
- **Visibility.** Every `session_unattested` and `session_id_invalid` fallback writes an operator-visible audit entry, registered in `categories.go`.
- **The residual, stated plainly.** On the local runner the implement agent and the fix-up agent run as the same OS user. The integrity of the transcript against that agent cannot be enforced, as with `~/.claude` itself. D1 and D5 bound the consequence.

**D5. Proof before on.**
- An operator walk shows whether `--resume` replays implement-era events into the fix-up's stream. The observable is implement-era `system.init` or `assistant` events, with pre-fix-up timestamps, in the fix-up bundle. The result is recorded in both runner READMEs.
- **If replay occurs,** the runner drops every event before the resumed turn before signing. Until that ships, resume stays off.
- **Cost walk.** A USD cost walk compares resumed and fresh fix-ups. If resume is not cheaper in dollars, it stays off (ADR-042's cost condition).

**D6. Untrusted-content measurement and the cap.**
- **Live test.** Resumed renders get a live injection case: a fixture implement session whose transcript carries an injected tool result and a forged user turn, resumed under the real fix-up prompt and judged on the compliance marker. It runs after the 2026-11-01 re-pin. There is no Fishhawk render to contain offline, so the offline pin stays `TestInjectionCorpus_AbsentFromImplementRender`, extended to the fix-up render.
- **The cap.** A `resumes` counter on the ledger record, carried in the D4 anchor, caps resumes at 1 per recorded session, with reason `resume_cap`. A fresh fix-up's own session may itself be resumed once. The cap bounds transcript growth and cost; it does not reduce untrusted content.

**D7. Visibility of the decision record.**
- This record's `applies_to` names `runner/cmd/fishhawk-runner/fixupresume.go` and `runner/internal/agent/claudecode/**`.
- ADR-042's record gains `amended_by: [ADR-119]`, and the same `applies_to`, so that a change to resume selects both records. (`Select` follows amendments forward only.)
- The `applies_to: []` blind spot as a whole is the decision-record scaling ADR's subject.

## Decision

**Accepted 2026-10-23** by the Captain (repository maintainer): **Option C**, i.e. D1–D7 as stated in the Recommendation (v2), decided now. It amends ADR-042: fix-up session resume is permitted on the local runner only under these rules, and is OFF until D2, D3 and D5 land; hosts set `FISHHAWK_FIXUP_RESUME=off` now.

## Consequences

- **Gains.**
  - Resume becomes a declared, opt-in capability whose trust posture is enforced.
  - The ratified ADR-042 and the shipped code agree.
  - The next change to resume is visible to the architect.
- **Costs:**
  - the opt-in default flip and the channel check;
  - record-after-push with re-invoke invalidation;
  - the head-advance comparison;
  - the signed anchor and its wire field (`wirecontract` manifest row);
  - the UUID check and its audit category;
  - the replay walk and event drop;
  - the USD walk;
  - the live injection case;
  - the resume counter;
  - README and ARCHITECTURE §5.3 rows for the new reasons (`unsupported_channel`, `tree_mismatch`, `session_unattested`, `session_id_invalid`, `resume_cap`).
- **Residuals.**
  - Same-user transcript tampering (D4).
  - A resumed session may reason from context that is stale within one head.
  - Live behavioural resistance is unmeasured until after the re-pin, and no live injection arm has run yet (#3187).
- **Implementation order:**
  1. Set `FISHHAWK_FIXUP_RESUME=off` on hosts now, at the Captain's decision.
  2. D2 and D4's UUID check.
  3. D3 and D4's anchor.
  4. D5's walks.
  5. D6.

  Every runner change here hits the E64.5 self-host bootstrap, so build the runner from the run branch before dispatching.
- **Record metadata.** amends: ["ADR-042"]; supersedes: []; applies_to: ["runner/cmd/fishhawk-runner/fixupresume.go", "runner/internal/agent/claudecode/**"]
- **Related:** ADR-013, ADR-029, ADR-035, ADR-042, ADR-084, ADR-117 (to be recorded as an escape once its D3 verb exists), #1264, #3187, #4089, #4583, #4584.
