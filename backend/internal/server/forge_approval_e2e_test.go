package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-112 (#4622) slice 2 (#4619): the cross-boundary forge-approval suite.
// GraphQL/REST wire (the mcq fake on the NON-Fishhawk fixture repository x/y,
// no required-review protection, so reviewDecision is null unless planted) →
// the PRODUCTION githubclient decode → the fold → the merge endpoint, the
// delegated may_merge arm and merge-on-pass → the real Postgres audit chain,
// queue and merge-dispatch ledger. Every row reads committed state.

// faeGateSpecYAML is mqeSpecYAML (v1.0, a verify command) with delegated
// may_merge and a review approval gate WITHOUT members: any write-or-higher
// human qualifies.
var faeGateSpecYAML = strings.Replace(strings.Replace(mqeSpecYAML,
	"              members: [\"github:alice\"]\n", "", 1),
	"  feature_change:\n", "  feature_change:\n    operator_agent:\n      may_merge: gates_resolved_ci_green\n", 1)

// faeUnparseableSpecYAML is admqSpecYAML plus a v0.5 review approval gate
// with no approvers: the merge-candidate gate admits it, but the fold's spec
// parse fails (a RECORDED spec that cannot be read → undetermined, C4).
const faeUnparseableSpecYAML = admqSpecYAML + `      - id: review
        type: review
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
`

// faeInline seeds a verified inline-mergeable run on spec at h1.
func (w *mqmWorld) faeInline(name, spec string) *mcqRun {
	w.t.Helper()
	r := w.seed(name, "h1", []byte(spec))
	w.mqeRebased(r, "pre-h1", "h1", "h1", false)
	w.mqePassed(r, "h1")
	return r
}

// faeRefusal decodes a 409 forge_approval_required body's details.
func faeRefusal(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if env.Error.Code != mergeCodeForgeApprovalRequired {
		t.Fatalf("code = %q, want %s: %s", env.Error.Code, mergeCodeForgeApprovalRequired, body)
	}
	return env.Error.Details
}

// requireNothingRecorded asserts no verdict, no authorization row, no claim
// and no inline dispatch.
func (w *mqmWorld) requireNothingRecorded(r *mcqRun, m *mqeMerger) {
	w.t.Helper()
	for _, c := range []string{CategoryMergeVerdictRecorded, CategoryMergeQueueAuthorized} {
		if n := len(w.rows(r, c)); n != 0 {
			w.t.Fatalf("%s rows = %d, want 0", c, n)
		}
	}
	if n := w.claims(r); n != 0 {
		w.t.Fatalf("claims = %d, want 0", n)
	}
	if m != nil && m.inlineCalls() != 0 {
		w.t.Fatalf("inline dispatches = %d, want 0", m.inlineCalls())
	}
}

func TestForgeApprovalE2E_InlineWriteNonMemberMergesWithEvidenceFirst(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("ok", faeGateSpecYAML)
	w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
	resp := w.mergeRun(r)
	if resp.Code != http.StatusOK {
		t.Fatalf("merge = %d %s, want 200", resp.Code, resp.Body.String())
	}
	auth, verdicts := w.rows(r, CategoryMergeQueueAuthorized), w.rows(r, CategoryMergeVerdictRecorded)
	if len(auth) != 1 || len(verdicts) != 1 || auth[0].Sequence >= verdicts[0].Sequence {
		t.Fatalf("authorized=%d verdict=%d rows; want one each, authorized first", len(auth), len(verdicts))
	}
	p := w.payload(auth[0])
	if p["path"] != mergequeue.DispatchPathInline || p["head_sha"] != "h1" || p["basis"] != forgeApprovalBasisLive {
		t.Fatalf("authorized payload = %v", p)
	}
	if m.inlineCalls() != 1 {
		t.Fatalf("inline dispatches = %d, want 1", m.inlineCalls())
	}
}

// TestForgeApprovalE2E_InlineRefusals pins every inline non-approved shape:
// 409 forge_approval_required with fold / next_step and a bounded detail, and
// NOTHING recorded.
func TestForgeApprovalE2E_InlineRefusals(t *testing.T) {
	cases := []struct {
		name     string
		plant    func(w *mqmWorld, r *mcqRun)
		fold     string
		reason   string
		nextStep string
		noGraph  bool
	}{
		{"read-permission approval", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, nil, mqmReview(1, "carol", "APPROVED", "h1"))
			w.gh.setPermission("carol", "read", "read")
		}, forgeApprovalAbsent, forgeApprovalReasonNoQualifying, forgeApprovalNextApprovePR, false},
		{"none permission approval", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, nil, mqmReview(1, "carol", "APPROVED", "h1"))
			w.gh.setPermission("carol", "", "none")
		}, forgeApprovalAbsent, forgeApprovalReasonNoQualifying, forgeApprovalNextApprovePR, false},
		{"dismissed approval", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "DISMISSED", "h1"))
		}, forgeApprovalDismissed, forgeApprovalReasonDismissed, forgeApprovalNextApprovePR, false},
		{"write veto", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol", "dave"}, mqmReview(1, "carol", "APPROVED", "h1"), mqmReview(2, "dave", "CHANGES_REQUESTED", "h1"))
		}, forgeApprovalVetoed, forgeApprovalReasonVeto, forgeApprovalNextDismissReview, false},
		{"snapshot transport failure", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			w.gh.set(func() { w.gh.graphQLFail[r.pr] = 5 })
		}, forgeApprovalUndetermined, forgeApprovalReasonSnapshotUnreadable, forgeApprovalNextRetryAfter, false},
		{"unparseable recorded spec", func(w *mqmWorld, r *mcqRun) {
			w.exec(`UPDATE runs SET workflow_spec = $1 WHERE id = $2`, []byte(faeUnparseableSpecYAML), r.id)
		}, forgeApprovalUndetermined, forgeApprovalReasonSpecUndeterminable, forgeApprovalNextRetryAfter, true},
		{"gitlab run with a declared gate", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			w.exec(`UPDATE runs SET installation_ref = 'gitlab:5' WHERE id = $1`, r.id)
		}, forgeApprovalProviderUnsupported, forgeApprovalReasonForgeUnsupported, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, m := newMQEWorld(t)
			r := w.faeInline("rf", faeGateSpecYAML)
			tc.plant(w, r)
			resp := w.mergeRun(r)
			if resp.Code != http.StatusConflict {
				t.Fatalf("merge = %d %s, want 409", resp.Code, resp.Body.String())
			}
			d := faeRefusal(t, resp.Body.Bytes())
			detail, _ := d["detail"].(string)
			if d["fold"] != tc.fold || d["reason"] != tc.reason || d["next_step"] != tc.nextStep || len(detail) > forgeApprovalDetailMaxBytes {
				t.Fatalf("details = %v, want fold %s reason %s next_step %q", d, tc.fold, tc.reason, tc.nextStep)
			}
			w.requireNothingRecorded(r, m)
			if tc.noGraph && w.gh.snapshotCalls() != 0 {
				t.Fatalf("snapshot calls = %d, want 0", w.gh.snapshotCalls())
			}
		})
	}
}

// TestForgeApprovalE2E_InlineHeadMovedAfterGate is H2 on the inline path: the
// PR head moves between the merge-candidate gate (which verified h1) and the
// in-lock snapshot, so the fold refuses undetermined head_moved: no claim,
// no verdict, no merge.
func TestForgeApprovalE2E_InlineHeadMovedAfterGate(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("hm", faeGateSpecYAML)
	w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
	w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() { w.gh.set(func() { w.gh.prs[r.pr].head = "h2" }) }}
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", resp.Code, resp.Body.String())
	}
	d := faeRefusal(t, resp.Body.Bytes())
	if d["fold"] != forgeApprovalUndetermined || d["reason"] != forgeApprovalReasonHeadMoved ||
		!strings.Contains(d["detail"].(string), forgeApprovalReasonHeadMoved) || d["head_sha"] != "h1" {
		t.Fatalf("details = %v, want undetermined head_moved at h1", d)
	}
	w.requireNothingRecorded(r, m)
}

func TestForgeApprovalE2E_InlineAuthorizationAppendFailure(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("af", faeGateSpecYAML)
	w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
	w.audit.failCategory = CategoryMergeQueueAuthorized
	resp := w.mergeRun(r)
	if resp.Code != http.StatusServiceUnavailable || mqeCode(t, resp.Body.Bytes()) != mergeCodeAuthorizationUnrecorded {
		t.Fatalf("merge = %d %s, want 503 %s", resp.Code, resp.Body.String(), mergeCodeAuthorizationUnrecorded)
	}
	w.requireNothingRecorded(r, m)
}

// TestForgeApprovalE2E_InlineOwnedRefusalWritesNoAuthorization pins that a
// refusal returning before the fold (409 merge_queue_owns_merge) writes no
// merge_queue_authorized row and reads no snapshot.
func TestForgeApprovalE2E_InlineOwnedRefusalWritesNoAuthorization(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("own", faeGateSpecYAML)
	w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
	w.srv.cfg.MergeQueue = mqeEnqueueOnLock{Store: w.store, once: &sync.Once{}, onRun: func() {
		if _, err := w.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "h1", MergeIntent: true}); err != nil {
			t.Errorf("enqueue: %v", err)
		}
	}}
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict || mqeCode(t, resp.Body.Bytes()) != "merge_queue_owns_merge" {
		t.Fatalf("merge = %d %s, want 409 merge_queue_owns_merge", resp.Code, resp.Body.String())
	}
	w.requireNothingRecorded(r, m)
	if n := w.gh.snapshotCalls(); n != 0 {
		t.Fatalf("snapshot calls = %d, want 0 (the fold never ran)", n)
	}
}

// faeDelegated seeds a may_merge-ready run on faeGateSpecYAML at h1.
func (w *mqmWorld) faeDelegated(name string) *mcqRun {
	w.t.Helper()
	r := w.seed(name, "h1", []byte(faeGateSpecYAML))
	w.setStage(r.review.ID, run.StageStateSucceeded)
	w.mqeRebased(r, "pre-h1", "h1", "h1", false)
	w.mqePassed(r, "h1")
	w.mqeAppend(r, drive.Category, map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)})
	return r
}

func TestForgeApprovalE2E_Delegated(t *testing.T) {
	t.Run("qualifying approval dispatches with evidence", func(t *testing.T) {
		w := newMQMWorld(t)
		r := w.faeDelegated("dq")
		w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
		m := &admqMerger{}
		out := w.admqDrive(r, m)
		if !out.Acted || m.calls() != 1 {
			t.Fatalf("outcome = %+v merger calls %d, want acted with 1 call", out, m.calls())
		}
		auth := w.rows(r, CategoryMergeQueueAuthorized)
		if len(auth) != 1 || w.payload(auth[0])["path"] != mergequeue.DispatchPathDelegated {
			t.Fatalf("authorized rows = %d", len(auth))
		}
	})
	for _, tc := range []struct {
		name  string
		plant func(w *mqmWorld, r *mcqRun)
		note  string
	}{
		{"non-qualifying approval", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, nil, mqmReview(1, "carol", "APPROVED", "h1"))
			w.gh.setPermission("carol", "triage", "read")
		}, autoDriveNoteApprovalNotQualified},
		{"head moved after the gate", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			w.srv.cfg.MergeQueue = mqmOnLockQueue{Store: w.store, onLock: func() { w.gh.set(func() { w.gh.prs[r.pr].head = "h2" }) }}
		}, autoDriveNoteApprovalNotQualified},
		{"gitlab run", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			w.exec(`UPDATE runs SET installation_ref = 'gitlab:5' WHERE id = $1`, r.id)
		}, autoDriveNoteApprovalNotQualified},
		{"authorization append failure", func(w *mqmWorld, r *mcqRun) {
			w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
			w.audit.failCategory = CategoryMergeQueueAuthorized
		}, autoDriveNoteAuthorizationUnrecorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r := w.faeDelegated("dn")
			tc.plant(w, r)
			m := &admqMerger{}
			requireObserveOnly(t, w.admqDrive(r, m), tc.note)
			if m.calls() != 0 {
				t.Fatalf("merger calls = %d, want 0", m.calls())
			}
			w.requireNothingRecorded(r, nil)
		})
	}
}

// TestForgeApprovalE2E_MergeOnPassHeadMovedAfterRecheck is H2 on the queue
// path: the head moves after the in-lock PR re-read (at the base re-probe), so
// the in-lock snapshot's headRefOid differs from the verified head and the
// fold's head binding re-advances: no claim, no verdict, no merge.
func TestForgeApprovalE2E_MergeOnPassHeadMovedAfterRecheck(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("hm", "h1", faeGateSpecYAML)
	w.forgeReviews(r, []string{"carol"}, mqmReview(1, "carol", "APPROVED", "h1"))
	w.onResponse = func(path string) {
		if strings.Contains(path, "/compare/") {
			w.gh.set(func() { w.gh.prs[r.pr].head = "h2" })
		}
	}
	requireOutcome(t, w.decide(req), mergeOnPassReadvance, mergeOnPassReasonHeadMoved)
	w.requireNoMergeWrites(r)
}
