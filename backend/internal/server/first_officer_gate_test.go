package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// First-officer findings and the gate hold (ADR-089 D4, E85.6 / #4148).
//
// Counterfactuals (run by hand during implementation, each RED, each file
// restored byte-identically):
//   - C11 sender role: seat branch role = RoleCaptain -> the seat send's
//     sender_role is captain; TestFirstOfficerSend_NoticeSendsAsFirstOfficer RED.
//   - C12 type allow-list deleted -> the seat's work_request answers 201;
//     TestFirstOfficerSend_Refusals/work_request RED.
//   - C13 recipient check deleted -> a finding to planner answers 201;
//     TestFirstOfficerSend_Refusals/non_captain_recipient RED.
//   - C14 anchor check deleted -> a finding on a run outside the campaign
//     answers 201; TestFirstOfficerSend_Refusals/out_of_seat_run RED.
//   - C15 BlockingEligible(...) forced true at the call site -> a bearer
//     failing job mints a concern; TestFirstOfficerSend_AdvisoryEvidence/bearer
//     reads one concern row, RED.
//   - C16 verdict check accepts pass -> a runner_attested PASS job mints a
//     concern; TestFirstOfficerSend_AdvisoryEvidence/pass RED.
//   - C17 evidence read error treated as advisory -> the send answers 201;
//     TestFirstOfficerSend_EvidenceReadError503NothingAppended RED.
//   - C18 provenance filter dropped -> an OPEN reviewer concern holds the
//     merge; TestMergeRun_ReviewerConcernDoesNotTriggerFirstOfficerHold RED.
//   - C19/C20 approvals.go call deleted -> the plan / review approve records
//     an approval row; TestSubmitApproval_FirstOfficerHold_* RED.
//   - C21 merge_run.go call deleted -> the merge answers a later code;
//     TestMergeRun_FirstOfficerFindingOpen_409NoWrite RED.
//   - Unwired-repo fail-open (approval condition 2) mutated to fail closed ->
//     TestFirstOfficerHoldRefusal_UnwiredAndReadError/unwired RED.
//   - Also RED under deletion: the wired read error (read_error), the approve
//     and merge 500 arms, reject-never-held, the plan stage-kind filter, the
//     handler's invalid-seat 401, the seat write:messages check, the in-seat
//     503, the run-less arm (run_less_detail), cited-sequence membership, the
//     completed-state check, the store ErrNotFound skip, the non-UUID account
//     refusal, and insert-then-dispose (a dispose on insert failure leaves the
//     finding accepted; ConversionFailuresLeaveFindingOpen/insert_error).
// Declared unverifiable by counterfactual: the store-row subject.run_id ==
// anchor re-check is masked by the chain-scoped ListForRunByCategory lookup on
// every constructible fixture; TestResolveFirstOfficerEvidence pins it only as
// a positive row (foreign_subject_run).

// ---- unit fakes -------------------------------------------------------------

// foEvidenceAudit serves specialist_job_completed entries per run.
type foEvidenceAudit struct {
	audit.BaseFake
	mu      sync.Mutex
	entries map[uuid.UUID][]*audit.Entry
	listErr error
}

func (a *foEvidenceAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	var out []*audit.Entry
	for _, e := range a.entries[runID] {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

// foJobStore is a SpecialistJobStore whose Get reads a map; every other
// method is unused here (nil embedded interface).
type foJobStore struct {
	SpecialistJobStore
	mu     sync.Mutex
	jobs   map[uuid.UUID]*specialistjob.Job
	getErr error
	gets   int
}

func newFOJobStore() *foJobStore { return &foJobStore{jobs: map[uuid.UUID]*specialistjob.Job{}} }

func (s *foJobStore) Get(_ context.Context, _ *uuid.UUID, jobID uuid.UUID) (*specialistjob.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	j, ok := s.jobs[jobID]
	if !ok {
		return nil, specialistjob.ErrNotFound
	}
	return j, nil
}

func (s *foJobStore) put(runID uuid.UUID, verdict specialistjob.Verdict, provenance string) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.New()
	s.jobs[id] = &specialistjob.Job{
		ID: id, Kind: specialistjob.KindDraft, State: specialistjob.StateCompleted, Verdict: verdict,
		Provenance: provenance, Subject: specialistjob.Subject{RunID: runID.String()},
	}
	return id
}

func completedEntry(seq int64, jobID string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"job_id": jobID})
	return &audit.Entry{Sequence: seq, Category: specialistjob.CategoryCompleted, Payload: payload}
}

func auditEvidence(refs ...string) []crewmessage.EvidenceReference {
	out := make([]crewmessage.EvidenceReference, 0, len(refs))
	for _, r := range refs {
		out = append(out, crewmessage.EvidenceReference{Kind: crewmessage.EvidenceAuditEntry, Ref: r})
	}
	return out
}

// ---- evidence resolution ----------------------------------------------------

func TestResolveFirstOfficerEvidence(t *testing.T) {
	anchor := &run.Run{ID: uuid.New()}
	other := uuid.New()
	jobs := newFOJobStore()
	attestedFail := jobs.put(anchor.ID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	bearerFail := jobs.put(anchor.ID, specialistjob.VerdictFail, specialistjob.ProvenanceBearer)
	attestedPass := jobs.put(anchor.ID, specialistjob.VerdictPass, specialistjob.ProvenanceRunnerAttested)
	attestedInconclusive := jobs.put(anchor.ID, specialistjob.VerdictInconclusive, specialistjob.ProvenanceRunnerAttested)
	foreignSubject := jobs.put(other, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	running := jobs.put(anchor.ID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	jobs.jobs[running].State = specialistjob.StateRunning
	otherRunJob := jobs.put(other, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)

	au := &foEvidenceAudit{entries: map[uuid.UUID][]*audit.Entry{
		anchor.ID: {
			completedEntry(10, attestedFail.String()),
			completedEntry(11, bearerFail.String()),
			completedEntry(12, attestedPass.String()),
			completedEntry(13, attestedInconclusive.String()),
			completedEntry(14, foreignSubject.String()),
			completedEntry(15, uuid.NewString()), // the store does not hold it
			completedEntry(16, "not-a-uuid"),
			{Sequence: 17, Category: specialistjob.CategoryCompleted, Payload: json.RawMessage(`[]`)},
			completedEntry(18, running.String()),
		},
		other: {completedEntry(20, otherRunJob.String())},
	}}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, SpecialistJobs: jobs})

	cases := []struct {
		name     string
		evidence []crewmessage.EvidenceReference
		want     *uuid.UUID
	}{
		{"runner_attested_fail_blocks", auditEvidence("10"), &attestedFail},
		{"first_blocking_ref_among_advisory", auditEvidence("11", "x", "10"), &attestedFail},
		{"bearer", auditEvidence("11"), nil},
		{"pass", auditEvidence("12"), nil},
		{"inconclusive", auditEvidence("13"), nil},
		{"foreign_subject_run", auditEvidence("14"), nil},
		{"job_not_in_store", auditEvidence("15"), nil},
		{"payload_job_id_not_uuid", auditEvidence("16"), nil},
		{"payload_undecodable", auditEvidence("17"), nil},
		{"job_not_completed", auditEvidence("18"), nil},
		{"other_runs_sequence", auditEvidence("20"), nil},
		{"non_numeric_ref", auditEvidence("ten"), nil},
		{"non_positive_ref", auditEvidence("0", "-10"), nil},
		{"non_audit_kind", []crewmessage.EvidenceReference{{Kind: crewmessage.EvidenceRun, Ref: "10"}}, nil},
		{"no_evidence", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, tc.evidence)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("basis = %+v, want advisory (nil)", got)
			case tc.want != nil && (got == nil || got.JobID != *tc.want):
				t.Fatalf("basis = %+v, want job %s", got, *tc.want)
			}
		})
	}
	if got, _ := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, auditEvidence("10")); got.CompletedSequence != 10 {
		t.Fatalf("completed_sequence = %d, want 10", got.CompletedSequence)
	}
}

func TestResolveFirstOfficerEvidence_FailClosedAndUnwired(t *testing.T) {
	anchor := &run.Run{ID: uuid.New()}
	jobs := newFOJobStore()
	jobID := jobs.put(anchor.ID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	au := &foEvidenceAudit{entries: map[uuid.UUID][]*audit.Entry{anchor.ID: {completedEntry(10, jobID.String())}}}

	t.Run("audit_read_error", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &foEvidenceAudit{listErr: errors.New("chain down")}, SpecialistJobs: jobs})
		if _, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, auditEvidence("10")); err == nil {
			t.Fatal("err = nil, want the chain read error")
		}
	})
	t.Run("store_read_error", func(t *testing.T) {
		failing := newFOJobStore()
		failing.getErr = errors.New("store down")
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, SpecialistJobs: failing})
		if _, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, auditEvidence("10")); err == nil {
			t.Fatal("err = nil, want the store read error")
		}
	})
	t.Run("run_account_not_uuid", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, SpecialistJobs: jobs})
		bad := &run.Run{ID: anchor.ID, AccountID: "not-a-uuid"}
		if _, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), bad, auditEvidence("10")); err == nil {
			t.Fatal("err = nil, want a refusal to read the untenanted partition")
		}
	})
	t.Run("store_unwired_is_advisory", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
		got, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, auditEvidence("10"))
		if err != nil || got != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", got, err)
		}
	})
	t.Run("no_numeric_ref_reads_nothing", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: &foEvidenceAudit{listErr: errors.New("must not be read")}, SpecialistJobs: jobs})
		if got, err := s.resolveFirstOfficerBlockingEvidence(context.Background(), anchor, auditEvidence("abc")); err != nil || got != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil) with no read", got, err)
		}
	})
}

// ---- the hold predicate -----------------------------------------------------

func seedFOConcern(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID, kind, provenance string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: runID, StageID: uuid.New(), StageKind: kind, Provenance: provenance, OriginReviewSequence: 7,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: crewConvertedConcernCategory, Note: "verify failed"}},
	})
	if err != nil {
		t.Fatalf("seed concern: %v", err)
	}
	return rows[0]
}

// Approval condition 2: an UNCONFIGURED concern repository is no hold (the
// explicit fail-open); a read ERROR on a wired one is returned (fail closed).
func TestFirstOfficerHoldRefusal_UnwiredAndReadError(t *testing.T) {
	t.Run("unwired", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		ref, err := s.firstOfficerHoldRefusal(context.Background(), uuid.New(), "")
		if ref != nil || err != nil {
			t.Fatalf("got (%+v, %v), want (nil, nil)", ref, err)
		}
	})
	t.Run("read_error", func(t *testing.T) {
		cr := newFakeConcernRepo()
		cr.listErr = errors.New("concerns down")
		s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr})
		if _, err := s.firstOfficerHoldRefusal(context.Background(), uuid.New(), ""); err == nil {
			t.Fatal("err = nil, want the read error")
		}
	})
}

func TestFirstOfficerHoldRefusal_FiltersProvenanceStateAndKind(t *testing.T) {
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr})
	runID := uuid.New()
	seedFOConcern(t, cr, runID, concern.StageKindImplement, "")
	seedFOConcern(t, cr, runID, concern.StageKindImplement, concern.ProvenanceServerCheck)
	waived := seedFOConcern(t, cr, runID, concern.StageKindPlan, concern.ProvenanceFirstOfficer)
	if _, err := cr.ApplyResolution(context.Background(), waived.ID, concern.StateWaived, "accepted risk"); err != nil {
		t.Fatalf("waive: %v", err)
	}
	if ref, err := s.firstOfficerHoldRefusal(context.Background(), runID, ""); ref != nil || err != nil {
		t.Fatalf("no open first_officer concern: got (%+v, %v), want (nil, nil)", ref, err)
	}
	impl := seedFOConcern(t, cr, runID, concern.StageKindImplement, concern.ProvenanceFirstOfficer)
	if ref, _ := s.firstOfficerHoldRefusal(context.Background(), runID, concern.StageKindPlan); ref != nil {
		t.Fatalf("plan-kind hold = %+v, want nil (the open one is implement-kind)", ref)
	}
	ref, err := s.firstOfficerHoldRefusal(context.Background(), runID, concern.StageKindImplement)
	if err != nil || ref == nil || ref.Code != codeFirstOfficerFindingOpen {
		t.Fatalf("implement-kind hold = (%+v, %v), want %s", ref, err, codeFirstOfficerFindingOpen)
	}
	ids, _ := ref.Details["concern_ids"].([]string)
	if len(ids) != 1 || ids[0] != impl.ID.String() {
		t.Fatalf("concern_ids = %v, want [%s]", ref.Details["concern_ids"], impl.ID)
	}
	if ref.Details["next_step"] != firstOfficerHoldNextStep {
		t.Fatalf("next_step = %v", ref.Details["next_step"])
	}
	if any, _ := s.firstOfficerHoldRefusal(context.Background(), runID, ""); any == nil {
		t.Fatal("merge (any-kind) hold = nil, want a hold")
	}
}

// ---- approvals --------------------------------------------------------------

func TestSubmitApproval_FirstOfficerHold_PlanApprove409NoRow(t *testing.T) {
	s, ar, rr, au, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	c := seedFOConcern(t, cr, stage.RunID, concern.StageKindPlan, concern.ProvenanceFirstOfficer)

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	code, _, details := approvalErrorBody(t, w)
	if code != codeFirstOfficerFindingOpen {
		t.Fatalf("code = %q, want %s", code, codeFirstOfficerFindingOpen)
	}
	if ids, _ := details["concern_ids"].([]any); len(ids) != 1 || ids[0] != c.ID.String() {
		t.Errorf("details.concern_ids = %v, want [%s]", details["concern_ids"], c.ID)
	}
	assertNoApprovalRecorded(t, ar, au)
}

func TestSubmitApproval_FirstOfficerHold_ReviewApprove409NoRow(t *testing.T) {
	s, ar, rr, _ := newApprovalServer(t)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	st := seedReviewStage(rr, "backlog_grooming", groomingReviewSpec, run.ExecutorHuman, true)
	seedFOConcern(t, cr, st.RunID, concern.StageKindImplement, concern.ProvenanceFirstOfficer)

	w := submitApproval(t, s, st.ID, `{"decision":"approve","comment":"checked the forge: all mutations landed"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if code, _, _ := approvalErrorBody(t, w); code != codeFirstOfficerFindingOpen {
		t.Fatalf("code = %q, want %s", code, codeFirstOfficerFindingOpen)
	}
	assertReviewGateUntouched(t, ar, rr, st.ID)
}

func TestSubmitApproval_FirstOfficerHold_ReadError500(t *testing.T) {
	t.Run("plan", func(t *testing.T) {
		s, ar, rr, au, cr := newApprovalServerWithConcerns(t)
		cr.listErr = errors.New("concerns down")
		stage := rr.seedStage(run.StageStateAwaitingApproval)
		w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "read open first-officer concerns failed") {
			t.Fatalf("status = %d, want 500 read open first-officer concerns failed:\n%s", w.Code, w.Body.String())
		}
		assertNoApprovalRecorded(t, ar, au)
	})
	t.Run("review", func(t *testing.T) {
		s, ar, rr, _ := newApprovalServer(t)
		cr := newFakeConcernRepo()
		cr.listErr = errors.New("concerns down")
		s.cfg.ConcernRepo = cr
		st := seedReviewStage(rr, "backlog_grooming", groomingReviewSpec, run.ExecutorHuman, true)
		w := submitApproval(t, s, st.ID, `{"decision":"approve","comment":"checked the forge: all mutations landed"}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		assertReviewGateUntouched(t, ar, rr, st.ID)
	})
}

// A reject is never held: the captain can always send the plan back.
func TestSubmitApproval_FirstOfficerHold_RejectNotHeld(t *testing.T) {
	s, ar, rr, _, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	seedFOConcern(t, cr, stage.RunID, concern.StageKindPlan, concern.ProvenanceFirstOfficer)
	w := submitApproval(t, s, stage.ID, `{"decision":"reject","comment":"replan"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Fatalf("approval rows = %d, want 1", len(ar.all))
	}
}

// A plan approve with only an implement-kind first_officer concern open is
// not held (the plan gate checks plan-kind).
func TestSubmitApproval_FirstOfficerHold_PlanIgnoresImplementKind(t *testing.T) {
	s, ar, rr, _, cr := newApprovalServerWithConcerns(t)
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	seedFOConcern(t, cr, stage.RunID, concern.StageKindImplement, concern.ProvenanceFirstOfficer)
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(ar.all) != 1 {
		t.Fatalf("approval rows = %d, want 1", len(ar.all))
	}
}

// ---- merge ------------------------------------------------------------------

func TestMergeRun_FirstOfficerFindingOpen_409NoWrite(t *testing.T) {
	h := newCoverageHarness(t)
	cr := newFakeConcernRepo()
	h.s.cfg.ConcernRepo = cr
	c := seedFOConcern(t, cr, h.parentID, concern.StageKindImplement, concern.ProvenanceFirstOfficer)

	w := postMergeRun(t, h.s, h.parentID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	for _, want := range []string{`"code":"first_officer_finding_open"`, c.ID.String(), `"next_step"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("body missing %s:\n%s", want, w.Body.String())
		}
	}
	if h.merger.called != 0 || len(mergeVerdictRows(h.au)) != 0 {
		t.Errorf("merger called %d, verdict rows %d, want 0 and 0", h.merger.called, len(mergeVerdictRows(h.au)))
	}

	// Dispositioned (waived) -> the hold clears and the merge proceeds.
	if _, err := cr.ApplyResolution(context.Background(), c.ID, concern.StateWaived, "accepted risk"); err != nil {
		t.Fatalf("waive: %v", err)
	}
	w = postMergeRun(t, h.s, h.parentID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("after waive: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

func TestMergeRun_FirstOfficerHoldReadError_500NoWrite(t *testing.T) {
	h := newCoverageHarness(t)
	cr := newFakeConcernRepo()
	cr.listErr = errors.New("concerns down")
	h.s.cfg.ConcernRepo = cr
	w := postMergeRun(t, h.s, h.parentID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "read open first-officer concerns failed") {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if h.merger.called != 0 || len(mergeVerdictRows(h.au)) != 0 {
		t.Errorf("merger called %d, verdict rows %d, want 0 and 0", h.merger.called, len(mergeVerdictRows(h.au)))
	}
}

// C18: an OPEN reviewer concern (empty provenance) never triggers the
// first-officer hold — no existing advisory concern became blocking.
func TestMergeRun_ReviewerConcernDoesNotTriggerFirstOfficerHold(t *testing.T) {
	h := newCoverageHarness(t)
	cr := newFakeConcernRepo()
	h.s.cfg.ConcernRepo = cr
	seedFOConcern(t, cr, h.parentID, concern.StageKindImplement, "")
	w := postMergeRun(t, h.s, h.parentID, mergeRunRequest{Verdict: "lgtm"}, withMergeOperator)
	if strings.Contains(w.Body.String(), codeFirstOfficerFindingOpen) {
		t.Fatalf("a reviewer concern held the merge as a first-officer finding:\n%s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// ---- the seat send branch ---------------------------------------------------

func seatIdentity(a firstofficer.Anchor, scopes ...string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		id := Identity{Subject: firstofficer.SubjectFor(a), TokenID: "tok-seat", Scopes: scopes}
		return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
	}
}

func rawSubjectIdentity(subject string, scopes ...string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		id := Identity{Subject: subject, TokenID: "tok-seat", Scopes: scopes}
		return r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
	}
}

// foSendPG is the pg-backed seat send fixture: real mailbox, audit chain, run
// and concern stores; fakes for campaigns and specialist jobs.
type foSendPG struct {
	*crewPG
	t    *testing.T
	jobs *foJobStore
	camp *foCampaignRepo
}

func newFOSendPG(t *testing.T) *foSendPG {
	t.Helper()
	f := &foSendPG{crewPG: newCrewPG(t), t: t, jobs: newFOJobStore(), camp: newFOCampaignRepo()}
	f.srv = New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        run.NewPostgresRepository(f.pool),
		AuditRepo:      f.audit,
		CrewMailbox:    f.mailbox,
		ConcernRepo:    concern.NewPostgresRepository(f.pool),
		CampaignRepo:   f.camp,
		SpecialistJobs: f.jobs,
	})
	return f
}

// seedCompletion appends a specialist_job_completed entry for a job on runID's
// chain and returns its sequence — seeded BY CONSTRUCTION.
func (f *foSendPG) seedCompletion(t *testing.T, runID uuid.UUID, verdict specialistjob.Verdict, provenance string) (uuid.UUID, int64) {
	t.Helper()
	jobID := f.jobs.put(runID, verdict, provenance)
	payload, _ := json.Marshal(map[string]any{"job_id": jobID.String()})
	kind := audit.ActorSystem
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: specialistjob.CategoryCompleted, ActorKind: &kind, Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed completion: %v", err)
	}
	return jobID, e.Sequence
}

func findingCiting(runID uuid.UUID, seqs ...int64) string {
	ev := make([]string, 0, len(seqs))
	for _, s := range seqs {
		ev = append(ev, `{"kind":"audit_entry","ref":"`+strconv.FormatInt(s, 10)+`"}`)
	}
	return `{"schema_version":"crew-message-v1","type":"finding","recipient_role":"captain","anchor":` + runAnchor(runID) +
		`,"payload":{"summary":"the verify battery failed","severity":"high"},"evidence":[` + strings.Join(ev, ",") + `]}`
}

func seatSeatScopes(a firstofficer.Anchor) func(*http.Request) *http.Request {
	return seatIdentity(a, firstofficer.SeatScopes()...)
}

func TestFirstOfficerSend_Refusals(t *testing.T) {
	f := newFOSendPG(t)
	inSeat := f.seedRun(t, run.StageTypePlan)
	outside := f.seedRun(t, run.StageTypePlan)
	campID := uuid.New()
	f.camp.byRun[inSeat] = []*campaign.Item{{CampaignID: campID}}
	campSeat := seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorCampaign, ID: campID})
	runSeat := seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: inSeat})
	notice := func(recipient, anchor string) string {
		return crewDoc("notice", "", recipient, anchor, `{"summary":"heads up"}`)
	}
	cases := []struct {
		name     string
		body     string
		decorate func(*http.Request) *http.Request
		status   int
		code     string
	}{
		{"work_request", crewDoc("work_request", "", "captain", runAnchor(inSeat), `{"title":"file it","summary":"x","rationale":"y"}`), runSeat, http.StatusForbidden, "first_officer_type_not_permitted"},
		{"consult", crewDoc("consult", "", "captain", runAnchor(inSeat), crewConsultPayload), runSeat, http.StatusForbidden, "first_officer_type_not_permitted"},
		{"escalation", crewDoc("escalation", "", "captain", runAnchor(inSeat), `{"summary":"x","recommended_default":"y","tradeoffs":"z"}`), runSeat, http.StatusForbidden, "first_officer_type_not_permitted"},
		{"non_captain_recipient", crewDoc("finding", "", "planner", runAnchor(inSeat), `{"summary":"x"}`), runSeat, http.StatusForbidden, "first_officer_recipient_not_permitted"},
		{"out_of_seat_run", notice("captain", runAnchor(outside)), campSeat, http.StatusForbidden, "first_officer_anchor_out_of_seat"},
		{"run_seat_other_run", notice("captain", runAnchor(outside)), runSeat, http.StatusForbidden, "first_officer_anchor_out_of_seat"},
		{"run_less_anchor", notice("captain", `{"issue_ref":"kuhlman-labs/fishhawk#4148"}`), runSeat, http.StatusForbidden, "first_officer_anchor_out_of_seat"},
		{"sender_role_captain_claimed", crewDoc("notice", "captain", "captain", runAnchor(inSeat), `{"summary":"x"}`), runSeat, http.StatusBadRequest, "sender_role_not_settable"},
		{"missing_write_messages", notice("captain", runAnchor(inSeat)), seatIdentity(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: inSeat}, "read:runs", "read:audit"), http.StatusForbidden, "insufficient_scope"},
		{"invalid_seat_subject", notice("captain", runAnchor(inSeat)), rawSubjectIdentity("first_officer/run/not-a-uuid", firstofficer.SeatScopes()...), http.StatusUnauthorized, "first_officer_seat_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.count(t, crewmessage.CategorySent)
			w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", tc.body, tc.decorate)
			requireCrewRefusal(t, w, tc.status, tc.code)
			if after := f.count(t, crewmessage.CategorySent); after != before {
				t.Fatalf("crew_message_sent entries %d -> %d, want no append on a refusal", before, after)
			}
		})
	}

	// The run-less arm returns the same code as !inSeat, which would mask its
	// deletion; its distinct detail isolates it.
	t.Run("run_less_detail", func(t *testing.T) {
		w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
			notice("captain", `{"issue_ref":"kuhlman-labs/fishhawk#4148"}`), runSeat)
		requireCrewRefusal(t, w, http.StatusForbidden, "first_officer_anchor_out_of_seat")
		if !strings.Contains(w.Body.String(), `"anchor":"run_less"`) {
			t.Fatalf("details lack anchor run_less: %s", w.Body.String())
		}
	})
	t.Run("in_seat_read_error_503", func(t *testing.T) {
		f.camp.listErr = errors.New("campaign store down")
		defer func() { f.camp.listErr = nil }()
		w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", notice("captain", runAnchor(inSeat)), campSeat)
		requireCrewRefusal(t, w, http.StatusServiceUnavailable, "first_officer_seat_unverifiable")
	})
	t.Run("campaign_seat_in_seat_run_admitted", func(t *testing.T) {
		resp := f.send(t, notice("captain", runAnchor(inSeat)), campSeat)
		if resp.SenderRole != string(crewmessage.RoleFirstOfficer) {
			t.Fatalf("sender_role = %q, want first_officer", resp.SenderRole)
		}
	})
}

// C11: a seat sends as first_officer, with actor agent, never as captain.
func TestFirstOfficerSend_NoticeSendsAsFirstOfficer(t *testing.T) {
	f := newFOSendPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	resp := f.send(t, crewDoc("notice", "", "captain", runAnchor(runID), `{"summary":"watching the fan-in"}`),
		seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}))
	if resp.SenderRole != string(crewmessage.RoleFirstOfficer) || resp.RecipientRole != string(crewmessage.RoleCaptain) {
		t.Fatalf("roles = %s -> %s, want first_officer -> captain", resp.SenderRole, resp.RecipientRole)
	}
	if resp.BlockingConcernID != nil || resp.BlockingConversionError != "" {
		t.Fatalf("a notice carried blocking fields: %+v", resp)
	}
	var actorKind string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT actor_kind FROM audit_entries WHERE sequence = $1`, resp.SentSequence).Scan(&actorKind); err != nil {
		t.Fatalf("read actor_kind: %v", err)
	}
	if actorKind != string(audit.ActorAgent) {
		t.Fatalf("actor_kind = %q, want agent", actorKind)
	}
}

func TestFirstOfficerSend_BlockingFindingConverts(t *testing.T) {
	f := newFOSendPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	jobID, seq := f.seedCompletion(t, runID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)

	resp := f.send(t, findingCiting(runID, seq), seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}))
	if resp.BlockingConcernID == nil || resp.BlockingConversionError != "" {
		t.Fatalf("blocking_concern_id = %v, blocking_conversion_error = %q; want a concern", resp.BlockingConcernID, resp.BlockingConversionError)
	}
	rows := concernsOn(t, f.crewPG, runID)
	if len(rows) != 1 {
		t.Fatalf("concern rows = %d, want 1", len(rows))
	}
	c := rows[0]
	if c.ID != *resp.BlockingConcernID || c.Provenance != concern.ProvenanceFirstOfficer ||
		c.CheckKey != "first_officer:job:"+jobID.String() || c.Severity != "high" ||
		c.Category != crewConvertedConcernCategory || c.StageKind != concern.StageKindPlan || c.State != concern.StateRaised {
		t.Fatalf("concern = %+v, want an open first_officer crew_finding concern keyed on the job", c)
	}
	row, err := f.mailbox.Store().Get(context.Background(), resp.SentSequence)
	if err != nil {
		t.Fatalf("get crew row: %v", err)
	}
	if row.State != crewmessage.StateAccepted {
		t.Fatalf("finding state = %s, want accepted (disposed after the insert)", row.State)
	}
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryCrewFindingConverted)
	if err != nil || len(entries) != 1 {
		t.Fatalf("crew_finding_converted entries = %d (%v), want 1", len(entries), err)
	}
	var p crewFindingConvertedPayload
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode converted payload: %v", err)
	}
	if p.Provenance != concern.ProvenanceFirstOfficer || p.BlockingBasis == nil ||
		p.BlockingBasis.JobID != jobID || p.BlockingBasis.CompletedSequence != seq || p.DispositionSequence == nil {
		t.Fatalf("converted payload = %+v, want provenance first_officer + blocking_basis {%s, %d}", p, jobID, seq)
	}
	if entries[0].ActorKind == nil || *entries[0].ActorKind != audit.ActorSystem {
		t.Fatalf("converted actor_kind = %v, want system", entries[0].ActorKind)
	}

	// The hold now refuses merge with the named code.
	hold, err := f.srv.firstOfficerHoldRefusal(context.Background(), runID, "")
	if err != nil || hold == nil || hold.Code != codeFirstOfficerFindingOpen {
		t.Fatalf("hold = (%+v, %v), want %s", hold, err, codeFirstOfficerFindingOpen)
	}
}

func TestFirstOfficerSend_AdvisoryEvidence(t *testing.T) {
	f := newFOSendPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	other := f.seedRun(t, run.StageTypePlan)
	seat := seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID})
	_, bearer := f.seedCompletion(t, runID, specialistjob.VerdictFail, specialistjob.ProvenanceBearer)
	_, pass := f.seedCompletion(t, runID, specialistjob.VerdictPass, specialistjob.ProvenanceRunnerAttested)
	_, inconclusive := f.seedCompletion(t, runID, specialistjob.VerdictInconclusive, specialistjob.ProvenanceRunnerAttested)
	_, foreign := f.seedCompletion(t, other, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	nonNumeric := `{"schema_version":"crew-message-v1","type":"finding","recipient_role":"captain","anchor":` + runAnchor(runID) +
		`,"payload":{"summary":"judgement only"},"evidence":[{"kind":"audit_entry","ref":"latest"}]}`
	for name, body := range map[string]string{
		"bearer":       findingCiting(runID, bearer),
		"pass":         findingCiting(runID, pass),
		"inconclusive": findingCiting(runID, inconclusive),
		"foreign_run":  findingCiting(runID, foreign),
		"non_numeric":  nonNumeric,
	} {
		t.Run(name, func(t *testing.T) {
			resp := f.send(t, body, seat)
			if resp.BlockingConcernID != nil || resp.BlockingConversionError != "" {
				t.Fatalf("advisory finding carried blocking fields: %+v", resp)
			}
			if rows := concernsOn(t, f.crewPG, runID); len(rows) != 0 {
				t.Fatalf("concern rows = %d, want 0 (advisory)", len(rows))
			}
			row, err := f.mailbox.Store().Get(context.Background(), resp.SentSequence)
			if err != nil || row.State != crewmessage.StateOpen {
				t.Fatalf("finding state = %v (%v), want open", row.State, err)
			}
		})
	}
}

// evidenceErrAudit fails the specialist_job_completed lookup only.
type evidenceErrAudit struct {
	audit.Repository
}

func (a evidenceErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == specialistjob.CategoryCompleted {
		return nil, errors.New("chain read failed")
	}
	return a.Repository.ListForRunByCategory(ctx, runID, category)
}

// C17: an evidence read error refuses the send with NOTHING appended.
func TestFirstOfficerSend_EvidenceReadError503NothingAppended(t *testing.T) {
	f := newFOSendPG(t)
	runID := f.seedRun(t, run.StageTypePlan)
	_, seq := f.seedCompletion(t, runID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
	f.srv.cfg.AuditRepo = evidenceErrAudit{Repository: f.audit}
	before := f.count(t, crewmessage.CategorySent)
	w := crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", findingCiting(runID, seq),
		seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}))
	requireCrewRefusal(t, w, http.StatusServiceUnavailable, "first_officer_evidence_unreadable")
	if after := f.count(t, crewmessage.CategorySent); after != before {
		t.Fatalf("crew_message_sent entries %d -> %d, want 0 appends", before, after)
	}

	// The store-side read error is the same refusal.
	f.srv.cfg.AuditRepo = f.audit
	f.jobs.getErr = errors.New("store down")
	w = crewCall(t, f.srv.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", findingCiting(runID, seq),
		seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}))
	requireCrewRefusal(t, w, http.StatusServiceUnavailable, "first_officer_evidence_unreadable")
	if after := f.count(t, crewmessage.CategorySent); after != before {
		t.Fatalf("crew_message_sent entries %d -> %d, want 0 appends", before, after)
	}
}

// A failed concern insert leaves the finding OPEN and visible, and the 201
// carries blocking_conversion_error (insert-then-dispose).
func TestFirstOfficerSend_ConversionFailuresLeaveFindingOpen(t *testing.T) {
	cases := map[string]func(f *foSendPG) uuid.UUID{
		"insert_error": func(f *foSendPG) uuid.UUID {
			f.srv.cfg.ConcernRepo = &failingConcernRepo{Repository: concern.NewPostgresRepository(f.pool), insertErr: errors.New("insert failed")}
			return f.seedRun(f.t, run.StageTypePlan)
		},
		"insert_empty": func(f *foSendPG) uuid.UUID {
			f.srv.cfg.ConcernRepo = &failingConcernRepo{Repository: concern.NewPostgresRepository(f.pool), empty: true}
			return f.seedRun(f.t, run.StageTypePlan)
		},
		"concern_repo_unwired": func(f *foSendPG) uuid.UUID {
			f.srv.cfg.ConcernRepo = nil
			return f.seedRun(f.t, run.StageTypePlan)
		},
		"no_plan_or_implement_stage": func(f *foSendPG) uuid.UUID {
			return f.seedRun(f.t, run.StageTypeReview)
		},
		"stage_read_error": func(f *foSendPG) uuid.UUID {
			f.srv.cfg.RunRepo = &failingStageRunRepo{Repository: run.NewPostgresRepository(f.pool), err: errors.New("stages down")}
			return f.seedRun(f.t, run.StageTypePlan)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFOSendPG(t)
			runID := setup(f)
			_, seq := f.seedCompletion(t, runID, specialistjob.VerdictFail, specialistjob.ProvenanceRunnerAttested)
			resp := f.send(t, findingCiting(runID, seq), seatSeatScopes(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runID}))
			if resp.BlockingConcernID != nil || resp.BlockingConversionError == "" {
				t.Fatalf("blocking_concern_id = %v, blocking_conversion_error = %q; want an error and no concern", resp.BlockingConcernID, resp.BlockingConversionError)
			}
			if rows := concernsOn(t, f.crewPG, runID); len(rows) != 0 {
				t.Fatalf("concern rows = %d, want 0", len(rows))
			}
			row, err := f.mailbox.Store().Get(context.Background(), resp.SentSequence)
			if err != nil || row.State != crewmessage.StateOpen {
				t.Fatalf("finding state = %v (%v), want open (never disposed without a concern)", row.State, err)
			}
		})
	}
}
