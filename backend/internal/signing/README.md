# backend/internal/signing

Per-run Ed25519 signing keys for runner artifact uploads (trace, plan,
PR artifact, installation token). Rows live in the `signing_keys`
table; `Verify` authenticates a signature against the run's unexpired
keys.

## Key TTL (active-or-next rule, #1030)

TTL = `max(30m, resolved stage budget + 5m)`. The stage resolves via
the active-or-next rule (fallback: first dispatched/running stage, else
first non-terminal), so a local-runner first-stage run (decomposition
child) gets budget + buffer rather than the 30m default. The budget is
sourced from `spec.ResolveStageTimeout`, so timed-out stages retain a
valid key long enough to upload their trace.

## Terminal-upload key refresh (#1182)

The runner re-issues the signing key immediately before the terminal
signed-egress sequence (trace / plan / PR-artifact /
installation-token), so a stage whose wall-clock (agent runtime +
scope-amendment blocks + verify reinvokes) outlives the start-of-stage
key still signs its terminal uploads with an unexpired key.

## Multi-key issuance and rotation tolerance (#1872)

Migration `0012_signing_keys_allow_rotation`: each `IssueKey` appends a
new row, and `Verify` accepts a signature from ANY unexpired key for
the run — newest-first — so a sibling stage's key rotation does not
invalidate an in-flight runner's still-open upload.

The #1182 refresh is **best-effort**: on a pre-0012 backend
(`ErrAlreadyIssued`) or any transient issuance error it degrades to the
existing start-of-stage key with a logged warning, never worse than
before.
