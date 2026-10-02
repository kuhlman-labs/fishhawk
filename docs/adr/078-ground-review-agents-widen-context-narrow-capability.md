---
id: ADR-078
title: "Ground the review agents by widening context and narrowing capability: export the reviewed tree, scrub the inherited environment, grant read and search only"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/2519
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-078: Ground the review agents by widening context and narrowing capability: export the reviewed tree, scrub the inherited environment, grant read and search only

## Context

The plan- and implement-review prompts carry a MUST-NOT list whose entry reads `- Invoke any tools beyond reading repository files for context.` — i.e. everything *beyond* repo reads is forbidden, so repo reads are permitted. No reviewer adapter can honour that, and the gap is not a missing flag.

What actually prevents repo reads today is that **neither reviewer can use tools at all**:

- **codex** runs `exec --json --skip-git-repo-check --output-schema …` and DELIBERATELY omits `--dangerously-bypass-approvals-and-sandbox` (`backend/internal/codex/client.go:14`), so it runs under codex's default sandbox: read-only filesystem, no network, approvals unbypassed. The empty `os.MkdirTemp` scratch at `cmd.Dir` (`:220,308`) is a SECOND layer, not the first.
- **claude-code** runs `--print --output-format json` with no `--dangerously-skip-permissions` (`backend/internal/claudecode/client.go:10-15`). In non-interactive print mode nothing can answer a permission prompt, so tool use is effectively dead. The adapter header states this premise outright: "the review prompt forbids tool use".

So the two halves hold contradictory beliefs about the prompt, and the adapters were built on the opposite reading from the one the prompt states. Grounding the reviewers is therefore not a bug fix — it is a decision to TURN TOOL USE ON in a deliberately hardened path.

A second fact makes that decision security-relevant rather than merely scope-relevant: both adapters set `cmd.Env = os.Environ()` wholesale (`codex/client.go` ~316, `claudecode/client.go` ~190), and codex additionally injects `OPENAI_API_KEY`. Today that is inert because nothing can read it. Granting tools makes a reviewer that processes UNTRUSTED input — the diff, and the issue text that steers it — hold the operator's database URLs, GitHub tokens, and API keys in its own environment.

The cost of the status quo is measured, not theoretical. In the first external-repo run (2026-08-06) the claude reviewer found a feature shipped inert because a config flag never reached its consumer, and downgraded it from high to medium, stating it could not confirm the absence because the repo was not checked out. One `git grep` settled it: zero non-test call sites. The reviewer behaved correctly — it calibrated confidence to reachable evidence — and the environment forced the downgrade.

It recurred in-repo during run `9ad2b396-f903-4a31-9026-61b46da71035` (PR #2517): the codex reviewer blocked a merge by flagging a non-atomic count-then-append as a defect the PR introduced. The identical shape already existed in `countSchemaRetries`, in a file the diff did not touch. One read of that unchanged file would have resolved it; instead it cost an operator arbitration.

The structurally diff-invisible classes are: this symbol is never called; this interface is never satisfied; this config never reaches its consumer; this new branch is unreachable; this constant duplicates an existing one. No prompt improvement reaches any of them.

## Options

**A. Diff-only, made honest.** Change the prompt line to forbid tool use outright and state the diff-only contract. One line. Reviewers stop hedging on evidence they were promised. Permanently concedes every diff-invisible class.

**B. Mount a read-only checkout at the reviewed ref.** Point codex's `cmd.Dir` at it instead of the scratch; pass `--add-dir <checkout>` and set `cmd.Dir` for claude-code. Full capability. As literally proposed this points at a live checkout, which carries `.git` (all branches and history), untracked files, `.env`, and `logs/` — none of which the reviewer needs and all of which it would then hold while reading untrusted input.

**C. Scoped search tool only.** Keep both sandboxes; grant a grep/search tool scoped to the tree. Narrow blast radius; covers the call-site and unreachability classes. Does not cover "read this function's doc comment to see whether the ordering is deliberate" — which is precisely what resolved the contested finding in run 9ad2b396.

**D. Export the tree; grant read and search only; scrub the environment.** Materialize the reviewed tree with `git archive <ref>` into a throwaway directory — tracked files at that ref, no `.git`, no other branches, no untracked local files, no `.env`. Point `cmd.Dir` there. Grant file read plus grep/glob via an explicit allow-list, never shell and never network. Replace the wholesale `os.Environ()` inheritance with a minimal enumerated allow-list.

## Recommendation

**D.** It is simultaneously MORE context and LESS exposure than B, which is why it supersedes rather than compromises with it: an exported tree gives the reviewer strictly more useful material than a live checkout minus the parts it never needed, while removing the git history, untracked files, and dotfiles that a checkout hands over incidentally.

Against C: the safety C buys comes from limiting CAPABILITY, and an explicit read+search allow-list delivers that same limit while still permitting the bounded file read that C forecloses. C withholds the tree to achieve what a tool allow-list achieves without withholding it.

Against A: A is honest but strictly worse product, and it makes #2119's grounding-and-calibration work unsatisfiable — you cannot calibrate to evidence in an environment that can ground nothing.

The environment scrub is separable and should be treated as independently valuable: it is correct on its own merits, it is the single highest-leverage item here, and it should land even if the rest were dropped.

## Decision

Adopt **D**, with these binding requirements:

1. **Scrub the environment on BOTH review paths.** Replace wholesale `os.Environ()` with a minimal enumerated allow-list (the model credential the adapter needs, plus `PATH`/`HOME` and whatever the binary genuinely requires — enumerated, not prefix-matched). A test asserts a sentinel secret in the parent environment does NOT appear in the child's.
2. **Export the tree; do not mount the checkout.** `git archive <ref>` (or equivalent) into a throwaway directory as `cmd.Dir`. Removed when the review ends, including on the timeout/kill path.
3. **Read and search only — never shell, never network.** claude-code gets an explicit tool allow-list, NOT `--dangerously-skip-permissions`. codex keeps its default sandbox and keeps omitting the bypass flag; the export simply replaces the empty scratch as `cmd.Dir`. If a CLI cannot express read-only-plus-search without also granting execution, that is stated plainly rather than widened to fit.
4. **HEAD tree, not base.** The questions are about post-change state: the reviewer needs the new symbol and every existing call site at once. The prompt states which tree it is looking at.
5. **Both adapters, or the change is not done.** One path alone leaves the panel asymmetric — one reviewer with context and one without — which is the underlying defect, not a partial win.
6. **The prompt and the sandbox must not be able to diverge again.** A test pins the reviewer's working directory and tool grant against the prompt's claim, so removing the grant or moving the directory fails a test rather than silently restoring the aspirational-prompt state.

Implementation is tracked by #2486, whose body carries the same requirements.

## Consequences

**Gained.** Reviewers can resolve the diff-invisible classes and cite evidence instead of hedging. #2119's grounding-and-calibration work becomes satisfiable — its premise that reviewers may read repository files becomes true rather than aspirational. The panel becomes genuinely heterogeneous rather than one reviewer with context and one without.

**Accepted residual risk.** The reviewer reads untrusted diff content while holding repo read access. Without shell or network the exfiltration path is narrow but not nil: an injected instruction could still shape the review verdict itself. This is accepted because reviewer verdicts are ADVISORY and an operator arbitrates every split verdict. It is NOT a reason to widen the grant further, and it IS a standing reason not to make reviewer verdicts binding without revisiting this ADR.

**Costs.** A reviewer with a tree will wander and burn tokens, so review cost and latency rise; mitigate with prompt guidance (search, do not browse) rather than by blinding the reviewer. Materializing and removing an export adds a step per review, with a cleanup path that must survive timeout and kill.

**Interactions.** Lands before #2119, whose calibration guidance is unsatisfiable until this does. #2307 (E63 runner-hosted reviewers) moves reviewer execution but does not by itself give it a tree, so this decision survives that move and its requirements should be carried across.

**Reversal.** Reverting restores the diff-only reviewer. The environment scrub should NOT be reverted with it — it is correct independently and carries no dependency on the tree export.


---

## Correction (operator, 2026-08-07) — the Recommendation's central claim was wrong

The Recommendation above says option D is "simultaneously MORE context and LESS exposure than B". **The second half is false, and the Consequences section understated the residual risk as a result.**

Verified live during the #2486 implementation walk (#2520), using the shipped grounded argv:

- **claude-code** read an absolute path OUTSIDE the exported tree, `permission_denials: []`.
- **codex** read the same file AND listed `~/.ssh`, returning `id_ed25519` among the filenames.

`cmd.Dir` and `--add-dir` select a working directory; they do not jail filesystem reads. No CLI flag fixes this on either adapter — codex offers only `read-only`/`workspace-write`/`danger-full-access`, all of which grant broad reads, and claude-code has no jail flag. Confinement requires OS-level sandboxing, the same primitive #611 needs for the implement agent.

**What survives.** Exporting still removes `.git`, other branches, and untracked files from the working directory, so ordinary reviewer behaviour stays on the reviewed snapshot, and it is still the right shape. The export bounds what is CONVENIENT. It does not bound what is REACHABLE — that is the distinction this ADR originally elided.

**What changes.** The "Accepted residual risk" paragraph should be read as follows: a grounded reviewer reads untrusted diff content while able to read ANY file the daemon user can read, and its verdict text is an egress path into the audit log and PR comments. That is materially larger than the "narrow but not nil" exfiltration path stated above, and it is NOT acceptable by default.

**Consequent decision.** Grounding ships DORMANT — `FISHHAWKD_REVIEW_GROUNDING` defaults to FALSE — and enabling it is an explicit operator choice suitable only for a trusted single-tenant host. Confinement is tracked in #2522. Everything else this ADR decided stands and is live: the environment scrub, the export, the read+search-only grant, the MCP isolation (added after a live check found the grounded claude reviewer inheriting the operator's Gmail/browser/GitHub MCP tools), and the prompt/sandbox contract test.

The standing condition in Consequences — that reviewer verdicts must not become binding without revisiting this ADR — is reinforced, not weakened, by this correction.
