# Release notes: kuhlman-labs/fishhawk

Range: `abc1234..def5678`

suggested bump: patch (because no breaking or additive signal detected; doc/test-only changes)

Total cost: $7.50

## Changes

### #201: Wire the evidence assembler

- PR: https://github.com/kuhlman-labs/fishhawk/pull/201
- Plan: https://github.com/kuhlman-labs/fishhawk/pull/201

Assemble merged-run evidence between two refs.

Reviewer verdicts:
- claude-opus-4-8: approve

Acceptance: passed

Cost: $5.00

### #202: Add the preview endpoint

- PR: https://github.com/kuhlman-labs/fishhawk/pull/202
- Plan: https://github.com/kuhlman-labs/fishhawk/pull/202

Expose GET /v0/releases/notes/preview.

Reviewer verdicts:
- claude-opus-4-8: approve
- gpt-5.5: request_changes

Acceptance: failed (failure mode: assertion_fail)

Deferred concerns:
- [medium/correctness] handle the empty-range case

Cost: $2.50

