package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// makeCheckRunPayload builds the verbatim JSON GitHub would deliver
// for a check_run event. Keeps tests honest about the wire shape
// (the ingester parses a small subset, but the bytes round-trip
// through Append's Payload field).
func makeCheckRunPayload(action, checkName, headSHA, status string, conclusion *string, prNumbers []int, runID int64) []byte {
	body := map[string]any{
		"action": action,
		"check_run": map[string]any{
			"id":           runID,
			"name":         checkName,
			"head_sha":     headSHA,
			"status":       status,
			"conclusion":   conclusion,
			"completed_at": "2026-05-07T12:00:00Z",
			"pull_requests": func() []map[string]any {
				out := make([]map[string]any, 0, len(prNumbers))
				for _, n := range prNumbers {
					out = append(out, map[string]any{"number": n})
				}
				return out
			}(),
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func TestIngestCheckRun_AppendsRowForEveryMatchingStage(t *testing.T) {
	scs := newStageCheckRepoFake()
	stage1 := uuid.New()
	stage2 := uuid.New()
	scs.matchedStages = []uuid.UUID{stage1, stage2}
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})

	body := makeCheckRunPayload("completed", "ci_pass", "abc123", "completed", ptrStr("success"), []int{42}, 999)
	s.ingestCheckRun(context.Background(), body)

	if len(scs.appendCalls) != 2 {
		t.Fatalf("Append calls = %d, want 2 (one per matching stage)", len(scs.appendCalls))
	}
	for _, call := range scs.appendCalls {
		if call.Name != "ci_pass" || call.HeadSHA != "abc123" {
			t.Errorf("Append params: %+v", call)
		}
		if call.GitHubCheckRunID == nil || *call.GitHubCheckRunID != 999 {
			t.Errorf("GitHubCheckRunID = %v", call.GitHubCheckRunID)
		}
		if call.Conclusion == nil || *call.Conclusion != "success" {
			t.Errorf("Conclusion = %v", call.Conclusion)
		}
	}
}

func TestIngestCheckRun_NoMatchingStage_Silent(t *testing.T) {
	scs := newStageCheckRepoFake()
	scs.matchedStages = nil // no matches
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})

	body := makeCheckRunPayload("completed", "ci_pass", "abc", "completed", ptrStr("success"), []int{42}, 999)
	s.ingestCheckRun(context.Background(), body)

	if len(scs.appendCalls) != 0 {
		t.Errorf("expected no Append calls, got %d", len(scs.appendCalls))
	}
}

func TestIngestCheckRun_SkipsNonStateBearingActions(t *testing.T) {
	scs := newStageCheckRepoFake()
	scs.matchedStages = []uuid.UUID{uuid.New()}
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})

	body := makeCheckRunPayload("requested_action", "ci_pass", "abc", "completed", ptrStr("success"), []int{42}, 999)
	s.ingestCheckRun(context.Background(), body)

	if len(scs.appendCalls) != 0 {
		t.Errorf("requested_action should be ignored, got %d Append calls", len(scs.appendCalls))
	}
}

func TestIngestCheckRun_NoPullRequests_Silent(t *testing.T) {
	// Org-level checks fire without pull_requests. Skip them
	// rather than try to match — the gate model is per-PR.
	scs := newStageCheckRepoFake()
	scs.matchedStages = []uuid.UUID{uuid.New()}
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})

	body := makeCheckRunPayload("completed", "ci_pass", "abc", "completed", ptrStr("success"), nil, 999)
	s.ingestCheckRun(context.Background(), body)

	if len(scs.appendCalls) != 0 {
		t.Errorf("expected no Append calls for empty pull_requests, got %d", len(scs.appendCalls))
	}
}

func TestIngestCheckRun_StateDerivedFromConclusion(t *testing.T) {
	cases := []struct {
		conclusion string
		want       stagecheck.State
	}{
		{"success", stagecheck.StatePass},
		{"failure", stagecheck.StateFail},
		{"timed_out", stagecheck.StateFail},
	}
	for _, c := range cases {
		t.Run(c.conclusion, func(t *testing.T) {
			scs := newStageCheckRepoFake()
			stageID := uuid.New()
			scs.matchedStages = []uuid.UUID{stageID}
			s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})

			body := makeCheckRunPayload("completed", "ci_pass", "abc", "completed", ptrStr(c.conclusion), []int{42}, 1)
			s.ingestCheckRun(context.Background(), body)

			if len(scs.appendCalls) != 1 {
				t.Fatalf("Append calls = %d", len(scs.appendCalls))
			}
			// Append result already runs DeriveState in the fake;
			// confirm by checking the fake-returned Check via
			// LatestForStageAndName — but the fake doesn't store
			// what Append yields, so derive directly:
			got := stagecheck.DeriveState(scs.appendCalls[0].Status, scs.appendCalls[0].Conclusion)
			if got != c.want {
				t.Errorf("derive(completed, %s) = %q, want %q", c.conclusion, got, c.want)
			}
		})
	}
}

func TestIngestCheckRun_Unconfigured_NoOp(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no StageCheckRepo
	// Should not panic; should not error. Just walk and exit.
	s.ingestCheckRun(context.Background(), makeCheckRunPayload(
		"completed", "ci_pass", "abc", "completed", ptrStr("success"), []int{1}, 1,
	))
}

func TestIngestCheckRun_MalformedJSON_Logs(t *testing.T) {
	scs := newStageCheckRepoFake()
	s := New(Config{Addr: "127.0.0.1:0", StageCheckRepo: scs})
	s.ingestCheckRun(context.Background(), []byte("{not json"))
	if len(scs.appendCalls) != 0 {
		t.Errorf("malformed payload should not produce Append calls")
	}
}
