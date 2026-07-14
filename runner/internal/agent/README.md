# runner/internal/agent

The agent abstraction (`Invoker`, `Invocation`, `Result`, `Event`) shared by the provider adapters (`claudecode/`, `codex/` — see their READMEs for adapter internals).

## Loop / duplicate-action detection (#653)

`loopdetect.go`'s `LoopDetector` is a pure, no-I/O detector that trips when the same tool-call signature recurs in an unbroken consecutive run of length ≥ `threshold` (default `DefaultLoopThreshold = 8`, conservative so legit repeats — re-reading a file, retrying a flaky command a couple of times — never false-abort; any differing signature resets the streak).

The claudecode adapter (`claudecode/claudecode.go`) builds a signature per `tool_use` block via `toolCallSignatures` = tool name + `canonicalInput` (keys sorted, so `Read a.go` ≠ `Read b.go` but two identical `Read a.go` calls collide), fail-open exactly like `outOfTreeWrites`.

The scan loop feeds each signature to the detector; on trip it appends a `loop_detected` trace event (signature + count + run/stage), kills `claude`, and the terminal switch maps `loopHit` to the peer sentinel `agent.ErrLoopDetected` with a category-A `failureResult` (outcome `loop_detected`, reason naming the count).

The sentinel does NOT wrap `ErrAgentFailed` (unambiguous `err_class`); unlike a thinking-block fault a loop is terminal and **not** retried — re-running the same prompt would just loop again. `Invoker.LoopThreshold` (0 → default) lets tests lower it.

Cross-stage no-progress is out of scope (it belongs to the orchestrator); this is runner-side, per-invocation only.

### #1273 wait-poll exemption

The claudecode feed loop skips feeding the sanctioned scope-amendments `?wait=` long-poll signature to the detector (predicate `isSanctionedWaitPoll` — `Bash ` prefix + `scope-amendments` path + `wait=` as a query parameter of that path, narrow so a bare `wait=` elsewhere / a non-waiting GET / a non-Bash tool are NOT exempt).

That long-poll is a deliberately-repeated identical action the prompt instructs the agent to issue (`backend/internal/prompt/prompt.go`, ~line 1020), so without the exemption it tripped `loop_detected` (~8 polls / ~4 min) before the prompt's ~15-minute proceed-as-denied cap.

The skip is a no-op exactly like the empty-signature case — it neither accumulates nor resets a streak — so an interleaved real loop still trips; a genuinely-stuck poller is reached by the agent's proceed-as-denied path and, failing that, backstopped by the implement-stage budget (ADR-025), not the loop detector.
