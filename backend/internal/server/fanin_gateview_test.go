package server

// Cross-boundary test for the gate view's fan_in_conflict block (#4343): the
// real orchestrator fan-in through POST /v0/runs/{id}/consolidate over the
// commitGraph forge fake (fanin_basetip_test.go's newBaseTipHarness) → the
// slices_integrated audit row → GET /v0/runs/{id}/gate-view through the real
// handler. A repeated no-op POST /integrate-wave after the conflicted
// consolidate writes a newer not_attempted row, which must NOT hide the
// conflict (#4343 approval condition C1).

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// slicesIntegratedRows reads the parent's slices_integrated rows with their
// stamped sequences, as the gate view reads them.
func (h *baseTipHarness) slicesIntegratedRows() []*audit.Entry {
	h.t.Helper()
	rows, err := h.au.ListForRunByCategory(context.Background(), h.parent.ID, "slices_integrated")
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func TestFanInGateView_BaseFoldConflictSurvivesNoOpIntegrateWave(t *testing.T) {
	h := newBaseTipHarness(t, run.StageStateAwaitingChildren)
	h.integrateFirstWave()
	h.advanceMainAndFinishSlice1("c.go", "c.go")
	h.s.cfg.Orchestrator = h.o
	h.o.ConsolidatedReview = nil

	w := postConsolidate(t, h.s, h.parent.ID, withAuth)
	if w.Code != http.StatusOK {
		t.Fatalf("consolidate status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if resp := decodeConsolidate(t, w); resp.Outcome != "integrated" {
		t.Fatalf("consolidate outcome = %q (%s), want integrated", resp.Outcome, resp.Detail)
	}

	assertBlock := func(stage string, wantSeq int64) {
		t.Helper()
		resp := decodeGateView(t, getGateView(t, h.s, h.parent.ID, ""))
		fc := resp.FanInConflict
		if fc == nil {
			t.Fatalf("%s: fan_in_conflict absent; gaps=%v", stage, resp.HistoryGaps)
		}
		if fc.Kind != fanInConflictKindBaseFold || fc.BaseRef != "main" || fc.BaseSHA != "B1" {
			t.Errorf("%s: block = %+v, want base_fold_conflict against main@B1", stage, fc)
		}
		if fc.BaseAttribution != "paths_overlap" || !slices.Equal(fc.ConflictingBasePaths, []string{"c.go"}) {
			t.Errorf("%s: base = (%q, %v), want (paths_overlap, [c.go])", stage, fc.BaseAttribution, fc.ConflictingBasePaths)
		}
		if fc.ConflictingSliceIndex == nil || *fc.ConflictingSliceIndex != 1 || fc.ConflictingChildRunID != h.child1.ID.String() {
			t.Errorf("%s: attribution = (%v, %q), want (1, %s)", stage, fc.ConflictingSliceIndex, fc.ConflictingChildRunID, h.child1.ID)
		}
		if !strings.Contains(fc.Remedy, "fishhawk_rebase_run_branch") {
			t.Errorf("%s: remedy = %q, want it to name fishhawk_rebase_run_branch", stage, fc.Remedy)
		}
		if wantSeq != 0 && fc.Sequence != wantSeq {
			t.Errorf("%s: sequence = %d, want the conflicted row's %d", stage, fc.Sequence, wantSeq)
		}
	}
	si := h.slicesIntegratedRows()
	if len(si) == 0 {
		t.Fatal("no slices_integrated row after consolidate")
	}
	conflictSeq := si[len(si)-1].Sequence
	assertBlock("after consolidate", conflictSeq)

	// A repeated fan-in: nothing is pending, so the pass writes a newer
	// not_attempted row without folding. The head is still behind main.
	ww := postIntegrateWave(t, h.s, h.parent.ID, withAuth)
	if ww.Code != http.StatusOK {
		t.Fatalf("integrate-wave status = %d, want 200:\n%s", ww.Code, ww.Body.String())
	}
	si = h.slicesIntegratedRows()
	if !strings.Contains(string(si[len(si)-1].Payload), `"base_fold_outcome":"not_attempted"`) || si[len(si)-1].Sequence <= conflictSeq {
		t.Fatalf("integrate-wave wrote %s at seq %d, want a newer not_attempted slices_integrated row", si[len(si)-1].Payload, si[len(si)-1].Sequence)
	}
	assertBlock("after a no-op integrate-wave", conflictSeq)
}
