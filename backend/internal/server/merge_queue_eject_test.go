package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-093 slice 3 (D4, E87.6 / #4297): the merge_queue_ejected record. The
// red-pass tests drive the verdict upload's handler half
// (succeedMergeCandidateVerifiedStage) over the real Postgres queue, run and
// audit repositories, so the D6 route and the ejection row cross the same
// boundary production does.

// waveSpec is mcqFixture.wave on spec: A admitted (live, verifying at a1) and
// B held behind it.
func (f *mcqFixture) waveSpec(spec string) (a, b *mcqRun) {
	f.t.Helper()
	a = f.seed("a", "a0", []byte(spec))
	b = f.seed("b", "b0", []byte(spec))
	f.behind(a, "a0", "a1")
	f.behind(b, "b0", "b1")
	if resp := f.rebase(a); resp.MergeCandidateVerifyState != mergeCandidateStateInFlight {
		f.t.Fatalf("A rebase verify state = %q, want in_flight", resp.MergeCandidateVerifyState)
	}
	if resp := f.rebase(b); resp.MergeCandidateVerifyState != mergeCandidateStateHeld {
		f.t.Fatalf("B rebase verify state = %q, want held", resp.MergeCandidateVerifyState)
	}
	return a, b
}

// shipVerdict delivers the runner's merge-candidate report for r at head
// through the upload handler and returns its answer. The re-opened implement
// stage is moved to running first, as the runner's stage start does, and the
// review gate is re-opened as the orchestrator's Advance (unwired here) does
// once the implement stage settles back.
func (f *mcqFixture) shipVerdict(r *mcqRun, head, result string) pullRequestMergeCandidateResponse {
	f.t.Helper()
	f.setStage(r.impl.ID, run.StageStateRunning)
	f.setStage(r.review.ID, run.StageStateAwaitingApproval)
	stage, err := f.repo.GetStage(context.Background(), r.impl.ID)
	if err != nil {
		f.t.Fatalf("get stage: %v", err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+r.id.String()+"/stages/"+r.impl.ID.String()+"/pull-request", nil)
	subj := mergeCandidateRunnerSubject
	f.srv.succeedMergeCandidateVerifiedStage(w, req, r.id, stage, &pullRequestBody{
		Outcome: outcomeMergeCandidateVerified, Branch: r.branch, HeadSHA: head, MergeCandidateResult: result,
	}, audit.ActorSystem, &subj)
	if w.Code != http.StatusOK {
		f.t.Fatalf("ship %s verdict = %d:\n%s", result, w.Code, w.Body.String())
	}
	var resp pullRequestMergeCandidateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("decode: %v", err)
	}
	return resp
}

// ejectedRows decodes r's merge_queue_ejected rows.
func (f *mcqFixture) ejectedRows(r *mcqRun) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, e := range f.rows(r, CategoryMergeQueueEjected) {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			f.t.Fatalf("decode merge_queue_ejected: %v", err)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem {
			f.t.Fatalf("merge_queue_ejected actor = %v, want system", e.ActorKind)
		}
		out = append(out, p)
	}
	return out
}

// oneEjection requires EXACTLY one merge_queue_ejected row for r naming
// reason and next_step, and returns it.
func (f *mcqFixture) oneEjection(r *mcqRun, reason, next string) map[string]any {
	f.t.Helper()
	rows := f.ejectedRows(r)
	if len(rows) != 1 {
		f.t.Fatalf("merge_queue_ejected rows = %d (%v), want exactly 1", len(rows), rows)
	}
	p := rows[0]
	if p["reason"] != reason || p["next_step"] != next {
		f.t.Fatalf("ejection = reason %v next_step %v, want %s/%s (%v)", p["reason"], p["next_step"], reason, next, p)
	}
	if p["entry_id"] != f.entry(r).ID.String() || p["base_ref"] != mcqBase || p["remedy"] == "" {
		f.t.Fatalf("ejection entry/base/remedy = %v", p)
	}
	return p
}

// appendStageRow appends a row on r's implement stage BY CONSTRUCTION.
func (f *mcqFixture) appendStageRow(r *mcqRun, category string, payload map[string]any) *audit.Entry {
	f.t.Helper()
	raw, _ := json.Marshal(payload)
	kind, subj := audit.ActorSystem, "test"
	id := r.impl.ID
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, StageID: &id,
		Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, ActorSubject: &subj, Payload: raw})
	if err != nil {
		f.t.Fatalf("seed %s: %v", category, err)
	}
	return e
}

// Mode (1): a red pass whose workflow delegates fix-up routing ejects A,
// admits B in the same pump, routes ONE D6 fix-up and records EXACTLY one
// ejection row stating the route.
func TestRedPass_DelegatedRoutesFixupAndRecordsEjection(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.waveSpec(mcvDelegatedV0SpecYAML)
	resp := f.shipVerdict(a, "a1", mergeCandidateResultFailed)
	if !resp.FixupRouted {
		t.Fatalf("routed = false (refusal %q), want the D6 fix-up routed", resp.FixupRefusal)
	}
	f.requireState(a, mergequeue.StateEjected, mergeCandidateQueueReasonVerifyFailed)
	f.requireState(b, mergequeue.StateLive, "")
	if n := len(f.rows(a, CategoryStageFixupTriggered)); n != 1 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 1", n)
	}
	p := f.oneEjection(a, mergeCandidateQueueReasonVerifyFailed, mergeQueueEjectNextAwaitDelegatedFixup)
	if p["fixup_routed"] != true || p["merge_intent"] != false || p["anchored_head_sha"] != "a1" {
		t.Fatalf("ejection = %v, want fixup_routed true, no merge intent, anchored a1", p)
	}
	if _, ok := p["refusal"]; ok {
		t.Fatalf("a routed fix-up carries no refusal: %v", p)
	}
	// The row follows the route on the chain (it states what D6 did).
	ej := f.rows(a, CategoryMergeQueueEjected)[0]
	if fx := f.rows(a, CategoryStageFixupTriggered)[0]; ej.Sequence <= fx.Sequence {
		t.Fatalf("merge_queue_ejected sequence %d precedes stage_fixup_triggered %d", ej.Sequence, fx.Sequence)
	}
}

// Mode (2): not delegated — fixup_routed false, next_step fishhawk_fixup_stage,
// a refusal naming it, and no stage_fixup_triggered row.
func TestRedPass_NotDelegatedNamesFixupStage(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.waveSpec(mcvUndelegatedSpecYAML)
	resp := f.shipVerdict(a, "a1", mergeCandidateResultFailed)
	if resp.FixupRouted {
		t.Fatal("routed = true on an undelegated workflow")
	}
	f.requireState(b, mergequeue.StateLive, "")
	p := f.oneEjection(a, mergeCandidateQueueReasonVerifyFailed, mergeQueueEjectNextFixupStage)
	if p["fixup_routed"] != false || !strings.Contains(fmt.Sprint(p["refusal"]), "fishhawk_fixup_stage") {
		t.Fatalf("ejection = %v, want fixup_routed false and a refusal naming fishhawk_fixup_stage", p)
	}
	if n := len(f.rows(a, CategoryStageFixupTriggered)); n != 0 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 0", n)
	}
}

// Mode (3): delegated, but the route is refused because the fix-up budget is
// spent (prior passes seeded at the ceiling).
func TestRedPass_RouteRefusedNamesFixupStage(t *testing.T) {
	f := newMCQFixture(t)
	a, _ := f.waveSpec(mcvDelegatedV0SpecYAML)
	for i := 0; i < defaultFixupCeiling; i++ {
		f.appendStageRow(a, CategoryStageFixupTriggered, map[string]any{"pass": i + 1})
	}
	resp := f.shipVerdict(a, "a1", mergeCandidateResultFailed)
	if resp.FixupRouted {
		t.Fatal("routed = true with the fix-up budget spent")
	}
	p := f.oneEjection(a, mergeCandidateQueueReasonVerifyFailed, mergeQueueEjectNextFixupStage)
	if p["fixup_routed"] != false || !strings.Contains(fmt.Sprint(p["refusal"]), "route was refused") ||
		!strings.Contains(fmt.Sprint(p["refusal"]), "fishhawk_fixup_stage") {
		t.Fatalf("ejection = %v, want fixup_routed false with the route's refusal", p)
	}
}

// Exactly once: a redelivered settle of the same entry writes no second row,
// and a real NON-ejected settle (b dropped) writes none. MECHANISM: writing
// the row regardless of SettleResult.Settled appends a second row on the
// redelivery; a writer that appended merge_queue_ejected on every settling
// call writes a row for b's drop (RED on b's rows). Sibling:
// TestEjectionRow_DroppedSettleWritesNone pins the drop alone.
func TestEjectionRow_WrittenOnlyByTheSettlingCall(t *testing.T) {
	f := newMCQFixture(t)
	a, b := f.wave()
	st, found, err := f.store.ActiveForRun(context.Background(), a.id)
	if err != nil || !found {
		t.Fatalf("active entry: %v %v", found, err)
	}
	e := st.Entry
	if _, ok := f.srv.settleMergeCandidateQueueEntryWith(context.Background(), e, mergequeue.StateEjected,
		mergeCandidateQueueReasonPassNotStartable, nil); !ok {
		t.Fatal("first settle did not settle the entry")
	}
	if _, ok := f.srv.settleMergeCandidateQueueEntryWith(context.Background(), e, mergequeue.StateEjected,
		mergeCandidateQueueReasonPassNotStartable, nil); ok {
		t.Fatal("redelivered settle reported settling an already-ejected entry")
	}
	if out := f.srv.settleMergeCandidateQueueEntryAtWith(context.Background(), e, mergequeue.StateEjected,
		mergeCandidateQueueReasonPassNotStartable, nil); out != admitSettled {
		t.Fatalf("redelivered SettleAt = %v, want admitSettled", out)
	}
	f.oneEjection(a, mergeCandidateQueueReasonPassNotStartable, mergeQueueEjectNextGetRunStatus)
	// A real non-ejected settle: b's entry (live or held after a's settle
	// pumped the base, read fresh) dropped writes no row.
	bst, found, err := f.store.ActiveForRun(context.Background(), b.id)
	if err != nil || !found {
		t.Fatalf("b active entry: %v %v", found, err)
	}
	if _, ok := f.srv.settleMergeCandidateQueueEntryWith(context.Background(), bst.Entry, mergequeue.StateDropped,
		mergeCandidateQueueReasonRunTerminal, nil); !ok {
		t.Fatal("dropping b's entry did not settle it")
	}
	if rows := f.ejectedRows(a); len(rows) != 1 {
		t.Fatalf("a merge_queue_ejected rows = %d, want 1", len(rows))
	}
	if rows := f.ejectedRows(b); len(rows) != 0 {
		t.Fatalf("b merge_queue_ejected rows = %v, want none for a drop", rows)
	}
}

// A dropped settle is not an ejection: no merge_queue_ejected row.
func TestEjectionRow_DroppedSettleWritesNone(t *testing.T) {
	f := newMCQFixture(t)
	a, _ := f.wave()
	st, _, _ := f.store.ActiveForRun(context.Background(), a.id)
	f.srv.settleMergeCandidateQueueEntry(context.Background(), st.Entry, mergequeue.StateDropped, mergeCandidateQueueReasonRunTerminal)
	if rows := f.ejectedRows(a); len(rows) != 0 {
		t.Fatalf("merge_queue_ejected rows = %v, want none for a drop", rows)
	}
}

// M2 (approval condition): a verify_failed ejection outside the verdict hook
// (here merge-on-pass seeing a failed verdict) derives fixup_routed from the
// run's stage_fixup_triggered row for that failed verdict, matched by CHAIN
// SEQUENCE. Arms: a delegated D6 route after the verdict routes it (so the run
// re-enters automatically); the same row BEFORE the verdict, an
// operator-routed fix-up, and a fix-up after a LATER verdict do not.
// MECHANISMS: dropping the sequence comparison turns "fix-up before the
// verdict" RED; dropping the delegated-label check turns "operator-routed"
// RED; dropping the window close turns "after a later verdict" RED.
func TestEjection_VerifyFailedAtMergeOnPassDerivesRoutedFixup(t *testing.T) {
	d6 := map[string]any{"delegated": mergeCandidateDelegatedRule}
	failed := map[string]any{"head_sha": "h1", "result": mergeCandidateResultFailed}
	cases := []struct {
		name   string
		seed   func(w *mqmWorld, r *mcqRun)
		routed bool
	}{
		{"delegated route after the failed verdict", func(w *mqmWorld, r *mcqRun) {
			w.appendStageRow(r, CategoryMergeCandidateVerified, failed)
			w.appendStageRow(r, CategoryStageFixupTriggered, d6)
		}, true},
		{"delegated route before the failed verdict", func(w *mqmWorld, r *mcqRun) {
			w.appendStageRow(r, CategoryStageFixupTriggered, d6)
			w.appendStageRow(r, CategoryMergeCandidateVerified, failed)
		}, false},
		{"operator-routed fix-up after the failed verdict", func(w *mqmWorld, r *mcqRun) {
			w.appendStageRow(r, CategoryMergeCandidateVerified, failed)
			w.appendStageRow(r, CategoryStageFixupTriggered, map[string]any{"delegated": ""})
		}, false},
		{"delegated route after a later verdict on the stage", func(w *mqmWorld, r *mcqRun) {
			w.appendStageRow(r, CategoryMergeCandidateVerified, failed)
			w.appendStageRow(r, CategoryMergeCandidateVerified, map[string]any{"head_sha": "other", "result": mergeCandidateResultPassed})
			w.appendStageRow(r, CategoryStageFixupTriggered, d6)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newMQMWorld(t)
			r, req := w.ready("m2", "h1", mcvUndelegatedSpecYAML)
			tc.seed(w, r)
			out := w.decide(req)
			requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonVerifyFailed)
			n := 0
			if !w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, out, &n) {
				t.Fatal("the verify_failed settle did not continue the pump")
			}
			next := mergeQueueEjectNextFixupStage
			if tc.routed {
				next = mergeQueueEjectNextAwaitDelegatedFixup
			}
			p := w.oneEjection(r, mergeCandidateQueueReasonVerifyFailed, next)
			if p["fixup_routed"] != tc.routed || p["merge_intent"] != true {
				t.Fatalf("ejection = %v, want fixup_routed %v with merge intent", p, tc.routed)
			}
		})
	}
}

// Undeterminable routing (an audit read failure) fails closed: fixup_routed
// false, next_step fishhawk_fixup_stage.
func TestMergeQueueVerifyFailedRouted_FailsClosed(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if s.mergeQueueVerifyFailedRouted(context.Background(), uuid.New(), "h") {
		t.Fatal("no audit repository read as routed")
	}
	s.cfg.AuditRepo = mqejErrAudit{}
	if s.mergeQueueVerifyFailedRouted(context.Background(), uuid.New(), "h") {
		t.Fatal("an audit read failure read as routed")
	}
	if s.mergeQueueVerifyFailedRouted(context.Background(), uuid.New(), "") {
		t.Fatal("an empty head read as routed")
	}
	ej := s.defaultMergeQueueEjection(context.Background(), mergequeue.Entry{EjectReason: mergeCandidateQueueReasonVerifyFailed, AnchoredHeadSHA: "h"})
	if ej.FixupRouted == nil || *ej.FixupRouted || !strings.Contains(ej.Refusal, "fishhawk_fixup_stage") {
		t.Fatalf("default ejection = %+v, want fixup_routed false with a refusal naming fishhawk_fixup_stage", ej)
	}
	if ej := s.defaultMergeQueueEjection(context.Background(), mergequeue.Entry{EjectReason: mergeCandidateQueueReasonAdvanceConflict}); ej.FixupRouted != nil || ej.Refusal != "" {
		t.Fatalf("advance_conflict default ejection = %+v, want empty", ej)
	}
}

// mqejErrAudit fails every category read.
type mqejErrAudit struct{ audit.Repository }

func (mqejErrAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, errors.New("injected audit read failure")
}

// A failed append is best-effort: the entry stays ejected, no row, no panic.
func TestWriteMergeQueueEjected_AppendFailureIsBestEffort(t *testing.T) {
	w := newMQMWorld(t)
	r, _ := w.ready("af", "h1", mcvUndelegatedSpecYAML)
	w.audit.failCategory = CategoryMergeQueueEjected
	w.srv.writeMergeQueueEjected(context.Background(), w.active(r), mergeQueueEjection{})
	if rows := w.rows(r, CategoryMergeQueueEjected); len(rows) != 0 {
		t.Fatalf("rows = %d after a failed append", len(rows))
	}
	s := New(Config{Addr: "127.0.0.1:0"})
	s.writeMergeQueueEjected(context.Background(), mergequeue.Entry{}, mergeQueueEjection{}) // no audit repo: no-op
}

// TestMergeQueueEjectNextStep_Total: every server eject reason maps to a
// non-empty next_step and remedy built from the vocabulary; an unknown reason
// maps to fishhawk_merge_run; the typed forge refusals map to their codes.
func TestMergeQueueEjectNextStep_Total(t *testing.T) {
	for _, tc := range []struct {
		reason string
		routed bool
		want   string
	}{
		{mergeCandidateQueueReasonVerifyFailed, true, mergeQueueEjectNextAwaitDelegatedFixup},
		{mergeCandidateQueueReasonVerifyFailed, false, mergeQueueEjectNextFixupStage},
		{mergeCandidateQueueReasonAdvanceConflict, false, mergeQueueEjectNextRebaseRunBranch},
		{mergeCandidateQueueReasonForgeMergeRefused, false, mergeQueueEjectNextMergeRun},
		{mergeCandidateQueueReasonPassNotStartable, false, mergeQueueEjectNextGetRunStatus},
		{"some_future_reason", false, mergeQueueEjectNextMergeRun},
		{"", false, mergeQueueEjectNextMergeRun},
	} {
		got := mergeQueueEjectNextStep(tc.reason, tc.routed)
		if got != tc.want {
			t.Errorf("next_step(%q, %v) = %q, want %q", tc.reason, tc.routed, got, tc.want)
		}
		if remedy := mergeQueueEjectRemedy(got, ""); remedy == "" || !strings.Contains(remedy, "fishhawk_") {
			t.Errorf("remedy(%q) = %q, want a non-empty remedy naming a verb", got, remedy)
		}
	}
	if r := mergeQueueEjectRemedy(mergeQueueEjectNextMergeRun, mergeQueueForgeReasonChecksNotSettled); !strings.Contains(r, "not settled at the merge attempt") ||
		!strings.Contains(r, "pending checks hold before dispatch") {
		t.Errorf("checks_not_settled remedy = %q, want the hold/eject asymmetry stated", r)
	}
	if r := mergeQueueEjectRemedy(mergeQueueEjectNextMergeRun, mergeQueueForgeReasonNotMergeable); !strings.Contains(r, "exactly once") {
		t.Errorf("forge remedy = %q", r)
	}
	for err, want := range map[error]string{
		fmt.Errorf("x: %w", forge.ErrPullRequestUnstableStatus):                   mergeQueueForgeReasonChecksNotSettled,
		fmt.Errorf("x: %w", forge.ErrPullRequestNotMergeable):                     mergeQueueForgeReasonNotMergeable,
		fmt.Errorf("x: %w: %w", forge.ErrValidation, forge.ErrMergeUnprocessable): mergeQueueForgeReasonUnprocessable,
		errors.New("connection reset"):                                            "",
	} {
		if got := mergeQueueForgeReasonFor(err); got != want {
			t.Errorf("forge reason(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestMergeQueueForgeReason_MergeMethodRefused (E93.2 / #4658): a
// *MergeMethodRefusalError riding alongside ErrPullRequestNotMergeable maps to
// merge_method_refused (decided BEFORE not_mergeable), and its remedy names
// the settings-or-by-hand remedy and the admission-spec semantics, never forge
// text. Counterfactual: deleting the merge_method_refused case maps the
// dual-sentinel refusal to not_mergeable (RED).
func TestMergeQueueForgeReason_MergeMethodRefused(t *testing.T) {
	ref := &MergeMethodRefusalError{Method: "merge", Source: MergeMethodSourceDeclared, Reason: MergeMethodRefusalForgeDisallows,
		Allowed: []string{"squash"}, Cause: fmt.Errorf("%w: %s", forge.ErrPullRequestNotMergeable, fmmSecretText)}
	if !errors.Is(ref, forge.ErrPullRequestNotMergeable) {
		t.Fatalf("fixture: the refusal must also carry ErrPullRequestNotMergeable (isolates the case ordering)")
	}
	for _, err := range []error{ref, fmt.Errorf("pinned merge: %w", ref)} {
		if got := mergeQueueForgeReasonFor(err); got != mergeQueueForgeReasonMergeMethodRefused {
			t.Errorf("mergeQueueForgeReasonFor(%T) = %q, want %q", err, got, mergeQueueForgeReasonMergeMethodRefused)
		}
	}
	r := mergeQueueEjectRemedy(mergeQueueEjectNextMergeRun, mergeQueueForgeReasonMergeMethodRefused)
	for _, want := range []string{"merge settings", "by hand", "admitted after it merges", "fishhawk_merge_run"} {
		if !strings.Contains(r, want) {
			t.Errorf("remedy = %q, want it to contain %q", r, want)
		}
	}
	if strings.Contains(r, "ghs_SENTINEL") {
		t.Errorf("remedy carries forge text: %q", r)
	}
}
