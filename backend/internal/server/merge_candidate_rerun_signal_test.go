package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// rerunSignalQueue is a test-local mergequeue.Store answering ListForBase from
// a fixed entry list (the occupied-base pre-check) and ActiveForRun with no
// entry (the run read's merge_candidate_queue block); nothing else is reached.
type rerunSignalQueue struct {
	mergequeue.Store
	entries []mergequeue.Entry
}

func (q *rerunSignalQueue) ListForBase(context.Context, mergequeue.Base) ([]mergequeue.Entry, error) {
	return q.entries, nil
}

func (q *rerunSignalQueue) ActiveForRun(context.Context, uuid.UUID) (mergequeue.Status, bool, error) {
	return mergequeue.Status{}, false, nil
}

// getRerunSignal drives the real GET /v0/runs/{run_id} handler and returns the
// decoded merge_candidate_rerun block (nil when the key is ABSENT) and the raw
// body.
func getRerunSignal(t *testing.T, sd *rebaseSeed) (*runMergeCandidateRerunPayload, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+sd.runID.String(), nil)
	req.SetPathValue("run_id", sd.runID.String())
	w := httptest.NewRecorder()
	sd.s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var envelope struct {
		MergeCandidateRerun *runMergeCandidateRerunPayload `json:"merge_candidate_rerun"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	return envelope.MergeCandidateRerun, w.Body.String()
}

// TestMergeCandidateRerunSignal (A1) pins the run read's advisory
// merge_candidate_rerun block through GET /v0/runs/{run_id}. Each absent
// subtest seeds ONLY its own disqualifier on a fixture that otherwise
// advertises, and each check's CF (delete it in mergeCandidateRerunSignalFor or
// the shared predicate) makes that subtest's block appear.
func TestMergeCandidateRerunSignal(t *testing.T) {
	t.Run("present at the review gate", func(t *testing.T) {
		sd := seedMCVRebase(t, upToDateRebaseStub())
		_, _, fSeq := seedFailedRebaseHead(t, sd)
		got, _ := getRerunSignal(t, sd)
		if got == nil {
			t.Fatal("merge_candidate_rerun absent, want the gate block")
		}
		if got.HeadSHA != rebaseNewHeadSHA || got.Cause != mergeCandidateCauseBaseAdvance || got.FailedSequence != fSeq {
			t.Errorf("block = %+v, want head %s cause %s failed %d", got, rebaseNewHeadSHA, mergeCandidateCauseBaseAdvance, fSeq)
		}
		if got.QueuedFixupStageID != "" || got.QueuedFixupSequence != 0 {
			t.Errorf("queued_fixup_* = %q/%d at the gate, want omitted", got.QueuedFixupStageID, got.QueuedFixupSequence)
		}
		if !strings.Contains(got.Note, "rerun_failed_verify:true") {
			t.Errorf("note = %q, want the verb named", got.Note)
		}
	})

	for _, state := range []run.StageState{run.StageStateAwaitingHostDispatch, run.StageStatePending} {
		t.Run("present with a queued D6 fix-up at "+string(state), func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			impl, tSeq, fSeq := seedQueuedD6Fixup(t, sd, state)
			got, _ := getRerunSignal(t, sd)
			if got == nil {
				t.Fatal("merge_candidate_rerun absent, want the queued-fix-up block")
			}
			if got.QueuedFixupStageID != impl.ID.String() || got.QueuedFixupSequence != tSeq || got.FailedSequence != fSeq {
				t.Errorf("block = %+v, want queued stage %s seq %d, failed %d", got, impl.ID, tSeq, fSeq)
			}
			if !strings.Contains(got.Note, "IN PLACE") || !strings.Contains(got.Note, "refunds") {
				t.Errorf("note = %q, want the in-place supersession named", got.Note)
			}
		})
	}

	t.Run("present beside an already-up-to-date rebase row", func(t *testing.T) {
		// Negative control for the performed-merge filter: an already-up-to-date
		// branch_rebased writes no head.
		sd := seedMCVRebase(t, upToDateRebaseStub())
		seedFailedRebaseHead(t, sd)
		seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{"already_up_to_date": true})
		if got, _ := getRerunSignal(t, sd); got == nil {
			t.Fatal("merge_candidate_rerun absent after an already-up-to-date rebase, want present")
		}
	})

	t.Run("present when only this run is on the base", func(t *testing.T) {
		// Negative control for the occupied-base case: the queue routes the
		// cause and its only active entry is this run's own.
		sd := seedMCVRebase(t, upToDateRebaseStub())
		seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
		sd.s.cfg.MergeQueue = &rerunSignalQueue{entries: []mergequeue.Entry{{RunID: sd.runID, State: mergequeue.StateHeld}}}
		if got, _ := getRerunSignal(t, sd); got == nil {
			t.Fatal("merge_candidate_rerun absent with only this run on the base, want present")
		}
	})

	absent := []struct {
		name string
		seed func(t *testing.T, sd *rebaseSeed)
	}{
		{"no verdict", func(t *testing.T, sd *rebaseSeed) {}},
		{"newest verdict passed", func(t *testing.T, sd *rebaseSeed) {
			impl, tSeq, _ := seedFailedRebaseHead(t, sd)
			seedMCVRow(sd, &impl.ID, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
				HeadSHA: rebaseNewHeadSHA, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
				Result: mergeCandidateResultPassed, TriggerSequence: tSeq})
		}},
		{"verify trigger follows the failed row", func(t *testing.T, sd *rebaseSeed) {
			impl, _, _ := seedFailedRebaseHead(t, sd)
			seedMCVRow(sd, &impl.ID, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
				Branch: rebaseBranchName, BaseRef: rebaseBaseRef, ExpectedHeadSHA: rebaseNewHeadSHA,
				Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand})
		}},
		{"fixup_pushed follows", headWriter(CategoryFixupPushed)},
		{"conflict_resolution_pushed follows", headWriter(CategoryConflictResolutionPushed)},
		{"child_pushed follows", headWriter("child_pushed")},
		{"operator_commit_vouched follows", headWriter(CategoryOperatorCommitVouched)},
		{"performed branch_rebased follows", func(t *testing.T, sd *rebaseSeed) {
			seedFailedRebaseHead(t, sd)
			seedMCVRow(sd, nil, CategoryBranchRebased, map[string]any{"new_head_sha": "f00d", "already_up_to_date": false})
		}},
		{"terminal run", func(t *testing.T, sd *rebaseSeed) {
			seedFailedRebaseHead(t, sd)
			sd.rr.getRuns[sd.runID].State = run.StateSucceeded
		}},
		{"no implement stage", func(t *testing.T, sd *rebaseSeed) {
			impl, _, _ := seedFailedRebaseHead(t, sd)
			var kept []*run.Stage
			for _, st := range sd.rr.stagesByRunID[sd.runID] {
				if st.ID != impl.ID {
					kept = append(kept, st)
				}
			}
			sd.rr.stagesByRunID[sd.runID] = kept
		}},
		{"implement stage running", func(t *testing.T, sd *rebaseSeed) {
			impl, _, _ := seedFailedRebaseHead(t, sd)
			impl.State = run.StageStateRunning
		}},
		{"queued fix-up with a slot waiter", func(t *testing.T, sd *rebaseSeed) {
			impl, _, _ := seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
			sd.s.cfg.Concurrency = &rerunStatusStore{status: map[uuid.UUID]concurrency.Status{
				impl.ID: {State: concurrency.SlotQueued, GroupKey: "local-implement:h1", Position: 2, WaiterLive: true}}}
		}},
		{"queued operator-routed fix-up", func(t *testing.T, sd *rebaseSeed) {
			impl, _, _ := seedFailedRebaseHead(t, sd)
			seedMCVRow(sd, &impl.ID, CategoryStageFixupTriggered, map[string]any{
				"prior_state": string(run.StageStateSucceeded), "reparked_review_stage_id": sd.review.ID.String()})
			impl.State = run.StageStateAwaitingHostDispatch
		}},
		{"queued fix-up on an occupied base", func(t *testing.T, sd *rebaseSeed) {
			// Approval condition C3: the verb refuses up front when another
			// run's active entry holds the base, so the block is omitted.
			seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
			sd.s.cfg.MergeQueue = &rerunSignalQueue{entries: []mergequeue.Entry{{RunID: uuid.New(), State: mergequeue.StateLive}}}
		}},
		{"queued fix-up whose failed pass's trigger is unreadable", func(t *testing.T, sd *rebaseSeed) {
			seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
			sd.au.mu.Lock()
			for i := range sd.au.appended {
				if sd.au.appended[i].Category == CategoryStageMergeCandidateVerifyTriggered {
					sd.au.appended[i].Payload = []byte(`{"branch":""}`)
				}
			}
			sd.au.mu.Unlock()
		}},
	}
	for _, tc := range absent {
		t.Run("absent: "+tc.name, func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			tc.seed(t, sd)
			got, body := getRerunSignal(t, sd)
			if got != nil || strings.Contains(body, `"merge_candidate_rerun"`) {
				t.Fatalf("merge_candidate_rerun = %+v, want the key absent", got)
			}
		})
	}

	readErrors := []struct {
		category string
		queued   bool
	}{
		// Gate-shaped: ignoring the error would read the category as empty and
		// advertise the gate block.
		{CategoryStageMergeCandidateVerifyTriggered, false},
		{CategoryFixupPushed, false},
		{CategoryBranchRebased, false},
		// Queued-shaped: the shared predicate's own read fails closed.
		{CategoryStageFixupTriggered, true},
	}
	for _, re := range readErrors {
		t.Run("omitted on a "+re.category+" read error", func(t *testing.T) {
			sd := seedMCVRebase(t, upToDateRebaseStub())
			if re.queued {
				seedQueuedD6Fixup(t, sd, run.StageStateAwaitingHostDispatch)
			} else {
				seedFailedRebaseHead(t, sd)
			}
			sd.au.listByCategoryErrCategory = re.category
			got, body := getRerunSignal(t, sd)
			if got != nil || strings.Contains(body, `"merge_candidate_rerun"`) {
				t.Fatalf("merge_candidate_rerun = %+v on a read error, want the key absent", got)
			}
		})
	}
}

// headWriter seeds the gate-shaped failed head followed by one head-writing
// row in category.
func headWriter(category string) func(t *testing.T, sd *rebaseSeed) {
	return func(t *testing.T, sd *rebaseSeed) {
		impl, _, _ := seedFailedRebaseHead(t, sd)
		seedMCVRow(sd, &impl.ID, category, map[string]any{"head_sha": "f00d"})
	}
}
