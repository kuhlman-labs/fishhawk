# runner/internal/bundle

`*.jsonl.gz` trace-bundle pack/unpack per ADR-007 (#71). The manifest is a wire format shared with `backend/internal/bundle` — the two `ManifestData` structs live in separate Go modules with **no schema-sync CI** (the bundle is a wire format, not a JSON Schema), so add fields on both sides in lockstep; a shared golden under `testdata/wire/` pins a new field where one exists (see Verify pin claim).

## Category-A signal (E8.5 / #163)

The runner stamps category-A failures into the bundle manifest: `ManifestData` carries `AgentFailed bool` + `AgentFailureReason string` (both `omitempty`, so older bundles parse as `AgentFailed=false`); `runner/cmd/fishhawk-runner/main.go` sets them when `agent.Result.FailureCategory == "A"`.

The backend's `bundle.ExtractManifest` reads the field; the trace handler in `server/trace.go` routes to `run.FailStage(stageID, run.FailureA, reason)` when `AgentFailed` is true, skipping both the policy re-evaluation and the `awaiting_approval` advance (no plan exists when the agent fails).

ADR-007 (#71) records the additive change.

## Push forward-gate booleans

The same lockstep rule governs three mutually-exclusive booleans — exactly one of the three may be set per stage kind, and each is `omitempty` → false on older bundles:

- **`PushAndOpenPR bool` (#742):** the runner stamps it for standalone implement stages; the trace handler reads it in `pushAndOpenPRGated` to forward-gate the implement terminal transition onto the `/pull-request` upload — see the push-and-open-pr advancement invariant in `docs/ARCHITECTURE.md` §4.2.1.
- **`PushToSharedBranch bool` (#771, mutually exclusive with `PushAndOpenPR`):** extends the same forward-gate to decomposed children. The runner stamps it for decomposed-child implement stages; the trace handler reads it in `childPushGated` to defer the child's terminal transition onto a `/pull-request` `{outcome:"pushed"}`/`{outcome:"failed"}` report — closing the decomposition-child analogue of the #742 zombie.
- **`PushFixup bool` (#794, mutually exclusive with both):** extends the forward-gate to fix-up re-dispatches. The runner stamps it for fix-up implement passes; the trace handler reads it in `fixupPushGated` to defer the fix-up's terminal transition onto a `/pull-request` `{outcome:"fixup_pushed"}`/`{outcome:"failed"}` report — closing the fix-up analogue of the #742 zombie, where the un-gated fix-up swallowed a push/compile-gate failure and let the implement re-review approve an unlanded diff.

## Bounds (#5535)

Pack never writes a bundle the backend's bounded reader must refuse, and Open reads with the same bounds. Both constants are KEEP IN SYNC with `backend/internal/bundle`: `MaxLineBytes` (16 MiB) caps one decompressed line, measured as `json.Marshal` output (HTML escaping included) without its `\n`, and `MaxDecompressedBytes` (256 MiB) caps the whole decompressed stream. `agent.MaxTraceLineBytes` equals `MaxLineBytes`. `MaxBundleBytes` (64 MiB) still caps the gzipped output.

`Open` reads through an `io.LimitedReader` of `MaxDecompressedBytes+1` and fails with `ErrDecompressedTooLarge` past it, and with `ErrLineTooLong` on a line over `MaxLineBytes`.

`Pack` marshals every line first, applies the bounds, then hashes the final lines and writes the trailer, so the trailer's hash and `event_count` (always `len(events)`) describe what ships.

- **Per line.** An ordinary event over `MaxLineBytes` becomes a `KindEventElided` (`event_elided`) placeholder with the original `seq` and `ts`. Its data (`ElidedEventData`) carries `elided_kind`, `original_bytes`, the `sha256` of the original line, `bound: "line"`, `max_line_bytes`, and a bounded `excerpt_head`/`excerpt_tail`. Pack takes these from the bytes it is given, so in the redacted variant they come from the already-redacted event. A `verify_run` line is never elided wholesale: only its `output` becomes a head+tail excerpt with an elision marker, and `output_elided_bytes` is added. `head_sha`, `tree_sha`, `change_id`, `command`, `exit_code` and `outcome` keep their values, because the backend's `ExtractHeadSHA`, `ExtractVerifyHead` and `ExtractVerifyIdentity` read them.
- **Total.** If the manifest, events and trailer exceed `MaxDecompressedBytes`, ordinary events are elided largest first (ties: higher `seq` first, `bound: "total"`) until the total fits, then `verify_run` outputs are trimmed largest first.
- **Fail closed.** A manifest, `git_diff`, `gate_evidence` or `policy_event` line over `MaxLineBytes`, or a `verify_run` still over it after trimming (or with no string `output`), fails with `ErrLineTooLarge`. A bundle still over the total after both passes fails with `ErrDecompressedTooLarge`. The backend reads those kinds (`ExtractManifest`, `ExtractDiff`, `ExtractGateEvidence`, `ExtractScopeDrift`, `ExtractScopeAmendmentsFolded`), so dropping one would change what it decides.
- **Log.** Each elision writes `{"event":"bundle_event_elided", run_id, stage_id, variant, kind, seq, original_bytes, sha256, bound}` and each trim writes `bundle_verify_output_trimmed` to `PackInputs.Log`; nil writes to `os.Stderr`. `variant` is `PackInputs.LogVariant`, omitted when empty; the runner passes its log sink with `raw` and `redacted`.

The tests inject small bounds through the unexported `pack`/`open` (`packLimits`).

## Verify pin claim (#5535)

`ManifestData.VerifyPinHeadSHA` (`verify_pin_head_sha`, `omitempty`) is the commit the runner pinned at `refs/fishhawk/checkpoints/<run>/<stage>-verify` before the trace upload. It is a CROSS-MODULE WIRE CONTRACT with `backend/internal/bundle.Manifest.VerifyPinHeadSHA`, pinned by the shared golden `testdata/wire/bundle_manifest_verify_pin.json`: `TestPack_ManifestVerifyPinGolden` packs a fixed `PackInputs` and asserts the manifest data equals it, and asserts the manifest line is unchanged when the field is empty.
