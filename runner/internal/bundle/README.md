# runner/internal/bundle

`*.jsonl.gz` trace-bundle pack/unpack per ADR-007 (#71). The manifest is a wire format shared with `backend/internal/bundle` — the two `ManifestData` structs live in separate Go modules with **no schema-sync CI** (the bundle is a wire format, not a JSON Schema), so add fields on both sides in lockstep.

## Category-A signal (E8.5 / #163)

The runner stamps category-A failures into the bundle manifest: `ManifestData` carries `AgentFailed bool` + `AgentFailureReason string` (both `omitempty`, so older bundles parse as `AgentFailed=false`); `runner/cmd/fishhawk-runner/main.go` sets them when `agent.Result.FailureCategory == "A"`.

The backend's `bundle.ExtractManifest` reads the field; the trace handler in `server/trace.go` routes to `run.FailStage(stageID, run.FailureA, reason)` when `AgentFailed` is true, skipping both the policy re-evaluation and the `awaiting_approval` advance (no plan exists when the agent fails).

ADR-007 (#71) records the additive change.

## Push forward-gate booleans

The same lockstep rule governs three mutually-exclusive booleans — exactly one of the three may be set per stage kind, and each is `omitempty` → false on older bundles:

- **`PushAndOpenPR bool` (#742):** the runner stamps it for standalone implement stages; the trace handler reads it in `pushAndOpenPRGated` to forward-gate the implement terminal transition onto the `/pull-request` upload — see the push-and-open-pr advancement invariant in `docs/ARCHITECTURE.md` §4.2.1.
- **`PushToSharedBranch bool` (#771, mutually exclusive with `PushAndOpenPR`):** extends the same forward-gate to decomposed children. The runner stamps it for decomposed-child implement stages; the trace handler reads it in `childPushGated` to defer the child's terminal transition onto a `/pull-request` `{outcome:"pushed"}`/`{outcome:"failed"}` report — closing the decomposition-child analogue of the #742 zombie.
- **`PushFixup bool` (#794, mutually exclusive with both):** extends the forward-gate to fix-up re-dispatches. The runner stamps it for fix-up implement passes; the trace handler reads it in `fixupPushGated` to defer the fix-up's terminal transition onto a `/pull-request` `{outcome:"fixup_pushed"}`/`{outcome:"failed"}` report — closing the fix-up analogue of the #742 zombie, where the un-gated fix-up swallowed a push/compile-gate failure and let the implement re-review approve an unlanded diff.
