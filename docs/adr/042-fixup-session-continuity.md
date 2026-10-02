---
id: ADR-042
title: "Fix-up session continuity: resume the prior implement agent session vs. prompt-level continuity"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/1264
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-042: Fix-up session continuity: resume the prior implement agent session vs. prompt-level continuity

## Context

#1166 (the #1152 lever-4 stretch) proposes resuming the **prior implement agent's CLI session** on a fix-up pass (e.g. `claude --resume <session-id>`) instead of cold-starting a fresh agent, so the fix-up retains the context it just wrote and re-pays no orientation cost. It is framed as the biggest token win of the #1152 levers. This ADR resolves whether/how to do that, because it collides with Fishhawk's per-stage trust model and is not uniformly portable.

### How it works today (grounded)

- **Stateless spawn, no resume.** The runner spawns the implement agent as a fresh, stateless process: `claude --print --verbose --output-format stream-json --dangerously-skip-permissions [--add-dir …] [--model <m>] -p <prompt>` (`runner/internal/agent/claudecode/claudecode.go:277-293`). **No `--resume`/`--continue` flag is passed and no session id is captured.** The only per-invocation knob is `--model` (#1013). `Invocation` (`runner/internal/agent/agent.go`) carries nothing session-related. The codex adapter is analogous.
- **Implement and fix-up are SEPARATE runner invocations.** A fix-up re-invokes the implement agent cold via `buildImplementFixup()` (`backend/internal/prompt/prompt.go`) — a slim prompt (#1152/#1157) carrying only the binding concerns, the operator approval conditions, the prior implement commit's unified diff (#1163/#1211), the issue link, and the scope-amendment escape hatch. **No conversation history** from the original implement agent is available.
- **Per-stage vouched trace + run-bound keys (ADR-031/ADR-035).** Each stage signs and uploads its own trace bundle (ed25519 over the bundle SHA, `runner/internal/upload/upload.go`); a run-bound signing key is issued at run start (`IssueKey(runID, ttl=0)`, `runner/cmd/fishhawk-runner/main.go`), with a fresh key minted before terminal egress. The vouch contract is **one signed trace per stage** over the stage's own committed tree, plus ADR-035 sole-writer-per-run-id branch lineage. The MCP token (`fhm_`) is run-bound, issued once at run start, reused for scope-amendment polling, with a backend-set TTL.
- **Worktree persists; session files do not.** The lineage worktree (`runner/cmd/fishhawk-runner/worktree.go`) persists git state across stages within a run lineage. But the CLI's conversation/session state lives in `~/.claude/` on the host and is **not** captured, persisted, or re-attached. On the local runner the home dir survives if the machine stays alive, but stages are separate invocations and the lineage lock is released between them; on CI (`github_actions`) the per-job runner is **ephemeral** — session files do not survive across job boundaries.

### Why this needs a decision

True session resume conflicts with three load-bearing properties:
1. **Attestation (ADR-031/035).** A single resumed process spanning implement→fix-up produces work for two separately-gated stages under one session/trace. The vouch model assumes one signed trace per stage over that stage's committed tree. How is the fix-up's tree attested independently if the conversation is continuous?
2. **Token/key scope + TTL.** The run-bound signing key (ttl=0) and MCP token are sized for one invocation's wall-clock. A process spanning both stages can outlive the token; there is no refresh-at-the-implement→fix-up-boundary path today.
3. **Portability.** Resume relies on CLI session files that do not survive CI's ephemeral runners, and couples Fishhawk to a specific CLI's session-file format/location (claude vs codex differ). It would work, if at all, only on a persistent local runner.

## Options

- **Option A — True cross-stage session resume.** Capture the implement session id, persist `~/.claude/` session state in the lineage worktree, re-attach on fix-up via `--resume`. Pros: maximal token win (no re-orientation, no re-injected diff). Cons: breaks per-stage attestation (needs a defined story for vouching the fix-up tree under a continuous session); requires token/key refresh at the stage boundary; CLI-coupled and **non-portable to CI**; the resumed trace mixes original-implement reasoning into the fix-up trace (redaction/lineage ambiguity).
- **Option B — Prompt-level continuity (enrich the current cold path).** Keep separate invocations + per-stage vouch; make the slim fix-up prompt carry more of the prior agent's *artifacts* (it already gets the prior diff #1211 + concerns; could add a structured prior-reasoning/plan-step summary distilled from the implement trace). Pros: no trust-model change, CI-portable, no CLI coupling. Cons: still re-pays partial orientation cost; the win is incremental, not the "skip re-orientation entirely" of A.
- **Option C — Hybrid, runner-kind-gated.** True resume (A) only on the persistent local runner where session files survive and the trust model is adapted; fall back to prompt-continuity (B) on CI/ephemeral. Pros: captures the win where feasible. Cons: two divergent code paths; still must answer A's attestation/token questions for the local path.
- **Option D — Decline resume; invest in cheap cold-start.** Treat prompt continuity as the portable answer and double down on making the cold fix-up cheap/rich (the #1157 slim prompt, #1163/#1211 prior-diff injection, #1210 verify sidecar, plus the #1164 cheaper-model routing already landed). Close #1166's resume ambition; the model-right-sizing (#1164) already addresses much of the cost motivation.

## Recommendation

Lean **D, with B as the incremental path**, and treat A as explicitly out-of-scope unless a future persistent-runner-only optimization justifies re-opening it behind a defined attestation model. Rationale: the per-stage vouch contract (ADR-031/035) is a core trust property and a continuous resumed session muddies it; resume is non-portable to CI; and the original cost motivation is now substantially met by **#1164 (cheaper model for low-complexity fix-ups, landed)** plus the slim prompt + prior-diff injection. If A is ever revisited, the minimal attestation rule should likely be: the fix-up still emits its **own** fresh per-stage signed trace over its committed tree, and the resumed conversation is an un-attested efficiency detail NOT part of the vouched lineage — i.e. resume the *context*, not the *trust boundary*.

## Decision

_TBD — operator. (Agent proposes, human disposes.)_

## Consequences

- **If D/B:** #1166 is rescoped from "resume the session" to "richer portable prompt continuity"; the resume ambition is closed or parked. No trust-model change. Re-survey the actual remaining token cost after #1164 to decide if even B is worth it.
- **If A/C:** requires a follow-up defining (1) session-state capture/persist/re-attach in the lineage worktree, (2) signing-key + MCP-token refresh at the implement→fix-up boundary, (3) the fix-up-tree attestation rule for a continuous session, and (4) a CI fallback. Local-runner-only at first.

## Notes

Drafted to unblock #1166 (its three open design questions — trace provenance, session persistence, token/key scope — are answered above with current-code grounding). Companion to #1152 (the lever this is lever-4 of) and #1164 (the cost-motivation overlap, landed this session). Evidence run 12531de4 (#1164, the most recent fix-up loop showing the current cold-start path).
