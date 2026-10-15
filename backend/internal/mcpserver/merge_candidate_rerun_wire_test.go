package mcpserver

import (
	"encoding/json"
	"testing"
)

// TestMergeCandidateRerunWire (A3, E86.54 / #4391) decodes backend-shaped
// golden JSON into the hand-maintained MCP mirrors: the run read's
// merge_candidate_rerun block, the rebase verb's
// merge_candidate_fixup_superseded flag, and a gate-view fix-up whose outcome
// is superseded. The goldens use the backend's json tags
// (server/merge_candidate_rerun_signal.go runMergeCandidateRerunPayload,
// server/rebase_branch.go rebaseBranchResponse, server/gateview.go
// gateViewFixup); a renamed client tag decodes to the zero value and fails.
func TestMergeCandidateRerunWire(t *testing.T) {
	t.Run("run read merge_candidate_rerun", func(t *testing.T) {
		raw := `{"id":"r1","state":"running","merge_candidate_rerun":{"head_sha":"bbbb","cause":"base_advance","failed_sequence":12,"queued_fixup_stage_id":"s1","queued_fixup_sequence":14,"note":"n"}}`
		var r Run
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		got := r.MergeCandidateRerun
		if got == nil {
			t.Fatal("Run.MergeCandidateRerun = nil, want the decoded block")
		}
		want := runMergeCandidateRerun{HeadSHA: "bbbb", Cause: "base_advance", FailedSequence: 12,
			QueuedFixupStageID: "s1", QueuedFixupSequence: 14, Note: "n"}
		if *got != want {
			t.Errorf("Run.MergeCandidateRerun = %+v, want %+v", *got, want)
		}
	})
	t.Run("run read without the block", func(t *testing.T) {
		var r Run
		if err := json.Unmarshal([]byte(`{"id":"r1","state":"running"}`), &r); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if r.MergeCandidateRerun != nil {
			t.Errorf("Run.MergeCandidateRerun = %+v, want nil when the backend omits it", r.MergeCandidateRerun)
		}
	})
	t.Run("rebase merge_candidate_fixup_superseded", func(t *testing.T) {
		raw := `{"already_up_to_date":true,"merge_candidate_verify_state":"in_flight","merge_candidate_verify_rerun":true,"merge_candidate_fixup_superseded":true}`
		var res RebaseBranchResult
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("decode rebase result: %v", err)
		}
		if !res.MergeCandidateFixupSuperseded || !res.MergeCandidateVerifyRerun {
			t.Errorf("rebase result superseded %v rerun %v, want both true", res.MergeCandidateFixupSuperseded, res.MergeCandidateVerifyRerun)
		}
	})
	t.Run("gate-view fix-up outcome superseded", func(t *testing.T) {
		var f GateViewFixup
		if err := json.Unmarshal([]byte(`{"sequence":14,"outcome":"superseded"}`), &f); err != nil {
			t.Fatalf("decode gate-view fix-up: %v", err)
		}
		if f.Outcome != "superseded" || f.Sequence != 14 {
			t.Errorf("gate-view fix-up = %+v, want outcome superseded at 14", f)
		}
	})
}
