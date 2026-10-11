package plan

import "testing"

// TestClassifyUndecidableReason pins the E72.63 / #4095 cause classifier. The
// first four positive rows are the VERBATIM undecidable_reasons the #4094 run
// (82487694, acceptance artifact bc3d7587) shipped, so the SIGNATURE arm is
// pinned against real producer output rather than prose written to match it.
//
// Each negative row isolates one conjunct, so deleting that conjunct turns
// exactly that row RED:
//   - "sandbox egress proxy blocks ..." has a sandbox denial and NO database
//     marker (pins the database conjunct);
//   - "the preview database returned no rows" has a database marker and NO
//     sandbox denial (pins the sandbox-denial conjunct);
//   - "the sandbox checkout lacks the database migration file ..." (approval
//     condition 2) has a database marker AND the word "sandbox", but the
//     sandbox is a LOCATION there, not a refusal. The markers were TIGHTENED so
//     it does not classify: the word "sandbox" names a denial only beside a
//     refusal verb. The accepted residual is a reason that ALSO carries a
//     refusal verb for an unrelated cause ("... so the server could not
//     start"); see ClassifyUndecidableReason's doc comment.
//
// The token-only row carries no database marker, so only the TOKEN arm can
// classify it; the upper-cased token row pins case-insensitivity.
func TestClassifyUndecidableReason(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "4094/rejects-unsigned-mis-signed-replayed",
			reason: "The verify-layer rejections, including the far-future 999999999999 ErrStale regression, pass. The HTTP 401 status and code mapping, the replay (nonce) rejection and the 'files nothing' property are covered only by pgtest-backed tests. Those could not start because the sandbox denies the loopback connection to the testcontainers Postgres, and the live preview has no alert source configured to exercise them.",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "4094/one-issue-per-fingerprint",
			reason: "Both named dedup tests need pgtest Postgres, which the sandbox cannot reach. The live preview has no alert sources configured, so it cannot file or dedup an alert. No evidence either way.",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "4094/auto-start-default-off",
			reason: "The config-level default (auto_start false unless set) and the next-step rendering are verified. The handler-level property that an alert from such a source files the issue without starting a run is verified only by a DB-backed test, which could not start in the sandbox.",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "4094/auto-started-run-carries-alert-trigger-source",
			reason: "The start path's trigger_source and trigger_ref are verified against a fake. The persisted-row end-to-end test, which the criterion names, needs pgtest Postgres, which the sandbox blocks.",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "eperm signature without the word sandbox",
			reason: "TestAlertTrigger_Dedup (pgtest) failed: dial tcp [::1]:5432: connect: operation not permitted",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "token only, no database marker",
			reason: "sandbox_denied_db: could not run TestAlertTrigger_Dedup",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "token upper-cased",
			reason: "SANDBOX_DENIED_DB: could not run TestAlertTrigger_Dedup",
			want:   UndecidableCauseSandboxDeniedDB,
		},
		{
			name:   "unrelated cause",
			reason: "the observation does not distinguish the two outcomes",
			want:   "",
		},
		{
			name:   "sandbox denial without a database marker",
			reason: "the sandbox egress proxy blocks POST /v0/runs",
			want:   "",
		},
		{
			name:   "database marker without a sandbox denial",
			reason: "the preview database returned no rows",
			want:   "",
		},
		{
			name:   "condition 2: sandbox as a location plus a database word, unrelated cause",
			reason: "the sandbox checkout lacks the database migration file the criterion names",
			want:   "",
		},
		{
			name:   "blocking criterion is not a refusal",
			reason: "the blocking criterion names a database fixture the sandbox checkout does not carry",
			want:   "",
		},
		{
			name:   "empty",
			reason: "",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyUndecidableReason(tc.reason); got != tc.want {
				t.Errorf("ClassifyUndecidableReason(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

// TestUndecidableCauseConstants pins the wire spellings: the payload key and
// the cause token are read by operators and named in the acceptance prompt, so
// a rename is a contract change, not a refactor.
func TestUndecidableCauseConstants(t *testing.T) {
	if UndecidableCausesKey != "undecidable_causes" {
		t.Errorf("UndecidableCausesKey = %q, want undecidable_causes", UndecidableCausesKey)
	}
	if UndecidableCauseSandboxDeniedDB != "sandbox_denied_db" {
		t.Errorf("UndecidableCauseSandboxDeniedDB = %q, want sandbox_denied_db", UndecidableCauseSandboxDeniedDB)
	}
}
