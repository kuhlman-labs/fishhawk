package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// First-officer seat lifecycle + surfacing tests (E85.6 / #4148, slice 2).
// Counterfactuals run by hand against these tests (control deleted or
// mutated, test run, RED observed, file restored byte-identically) are
// recorded in the PR notes and the counterfactual sidecar:
//
//   - C22 attach human-only: deleting the isAgentSubject refusal in
//     authorizeSeatCaptainAct (and the attach pre-check) let the
//     operator-agent token attach 201 — TestFirstOfficerSeatAttach_Refusals
//     "agent caller" RED.
//   - C24 one seat: deleting the existence check AND the convergence revoke
//     let the second attach answer 201 with two active tokens — RED.
//   - C25 terminal attach: deleting the rec.terminal check attached 201 to a
//     cancelled run — RED.
//   - C26 retire reason: deleting the empty-reason check retired 200 — RED.
//   - C27 autodrive hold: deleting the may_merge firstOfficerAutoDriveHold
//     branch called the merger — TestAutoDriveRunGate_FirstOfficerHold RED.
//   - C28 gate-view blocker: deleting the gateview append left
//     merge_readiness without first_officer_finding_open — RED.
//   - C23 attach captain: pinned over a real captain store in
//     first_officer_pg_test.go.

const seatHuman = "github:alice"

func (f *seatFixture) human(scopes ...string) string {
	if len(scopes) == 0 {
		scopes = []string{"write:approvals", "read:runs"}
	}
	return f.mint(seatHuman, scopes...)
}

func attachBody(kind firstofficer.AnchorKind, id uuid.UUID) string {
	return `{"` + string(kind) + `_id":"` + id.String() + `"}`
}

func decodeSeatAttach(t *testing.T, body []byte) firstOfficerSeatAttachResponse {
	t.Helper()
	var resp firstOfficerSeatAttachResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode attach: %v\n%s", err, body)
	}
	return resp
}

func TestFirstOfficerSeatAttach_RunSeat(t *testing.T) {
	f := newSeatFixture(t)
	runID := f.seedRun(run.StateRunning, "")
	w := f.do(http.MethodPost, "/v0/first-officer-seats", f.human(), attachBody(firstofficer.AnchorRun, runID))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	resp := decodeSeatAttach(t, w.Body.Bytes())
	if resp.SeatSubject != runSeat(runID) || !slices.Equal(resp.Scopes, firstofficer.SeatScopes()) || resp.Token == "" {
		t.Fatalf("attach = %+v, want the run seat subject, SeatScopes and a token", resp)
	}
	if resp.Anchor.Kind != "run" || resp.Anchor.ID != runID.String() {
		t.Errorf("anchor = %+v", resp.Anchor)
	}
	// The returned token authenticates as the seat with exactly SeatScopes.
	id, _, reached := f.through(resp.Token)
	if !reached || id.Subject != runSeat(runID) || !slices.Equal(id.Scopes, firstofficer.SeatScopes()) {
		t.Fatalf("seat identity = %+v reached=%v", id, reached)
	}
	rows := f.aud.chainedOf(auditFirstOfficerSeatAttached)
	if len(rows) != 1 || rows[0].RunID != runID {
		t.Fatalf("attached rows = %+v, want one on the run chain", rows)
	}
	if strings.Contains(string(rows[0].Payload), resp.Token) {
		t.Error("the attach row carries the plaintext token")
	}
	for _, want := range []string{`"captain_basis":"unavailable"`, `"attached_by":"github:alice"`, resp.TokenID} {
		if !strings.Contains(string(rows[0].Payload), want) {
			t.Errorf("attach payload missing %s: %s", want, rows[0].Payload)
		}
	}

	// GET reports it attached.
	g := f.do(http.MethodGet, "/v0/first-officer-seats?run_id="+runID.String(), f.human("read:runs"), "")
	if g.Code != http.StatusOK {
		t.Fatalf("get = %d:\n%s", g.Code, g.Body.String())
	}
	var st firstOfficerSeatStatusResponse
	_ = json.Unmarshal(g.Body.Bytes(), &st)
	if !st.Attached || len(st.TokenIDs) != 1 || st.TokenIDs[0] != resp.TokenID || st.AnchorState != string(run.StateRunning) || st.AnchorTerminal {
		t.Errorf("status = %+v", st)
	}
}

func TestFirstOfficerSeatAttach_CampaignSeatGlobalRow(t *testing.T) {
	f := newSeatFixture(t)
	campID := f.seedCampaign(campaign.StateRunning, "")
	w := f.do(http.MethodPost, "/v0/first-officer-seats", f.human(), attachBody(firstofficer.AnchorCampaign, campID))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if rows := f.aud.globalOf(auditFirstOfficerSeatAttached); len(rows) != 1 {
		t.Fatalf("global attached rows = %d, want 1", len(rows))
	}
}

func TestFirstOfficerSeatAttach_Refusals(t *testing.T) {
	foreign := uuid.NewString()
	for _, tc := range []struct {
		name   string
		setup  func(f *seatFixture) (bearer, body string)
		status int
		code   string
	}{
		{"missing write:approvals", func(f *seatFixture) (string, string) {
			return f.human("read:runs"), attachBody(firstofficer.AnchorRun, f.seedRun(run.StateRunning, ""))
		}, http.StatusForbidden, "insufficient_scope"},
		{"agent caller", func(f *seatFixture) (string, string) {
			return f.mint("operator-agent/operator-role-v0", "write:approvals"), attachBody(firstofficer.AnchorRun, f.seedRun(run.StateRunning, ""))
		}, http.StatusForbidden, "first_officer_attach_requires_human"},
		{"foreign-account anchor", func(f *seatFixture) (string, string) {
			return f.human(), attachBody(firstofficer.AnchorRun, f.seedRun(run.StateRunning, foreign))
		}, http.StatusNotFound, "run_not_found"},
		{"foreign-account campaign", func(f *seatFixture) (string, string) {
			return f.human(), attachBody(firstofficer.AnchorCampaign, f.seedCampaign(campaign.StateRunning, foreign))
		}, http.StatusNotFound, "campaign_not_found"},
		{"missing anchor", func(f *seatFixture) (string, string) {
			return f.human(), attachBody(firstofficer.AnchorRun, uuid.New())
		}, http.StatusNotFound, "run_not_found"},
		{"terminal anchor", func(f *seatFixture) (string, string) {
			return f.human(), attachBody(firstofficer.AnchorRun, f.seedRun(run.StateCancelled, ""))
		}, http.StatusConflict, "first_officer_anchor_terminal"},
		{"terminal campaign", func(f *seatFixture) (string, string) {
			return f.human(), attachBody(firstofficer.AnchorCampaign, f.seedCampaign(campaign.StateCancelled, ""))
		}, http.StatusConflict, "first_officer_anchor_terminal"},
		{"both anchors", func(f *seatFixture) (string, string) {
			return f.human(), `{"run_id":"` + uuid.NewString() + `","campaign_id":"` + uuid.NewString() + `"}`
		}, http.StatusBadRequest, "validation_failed"},
		{"no anchor", func(f *seatFixture) (string, string) { return f.human(), `{}` }, http.StatusBadRequest, "validation_failed"},
		{"non-uuid anchor", func(f *seatFixture) (string, string) { return f.human(), `{"run_id":"nope"}` }, http.StatusBadRequest, "validation_failed"},
		{"reason on attach", func(f *seatFixture) (string, string) {
			return f.human(), `{"run_id":"` + uuid.NewString() + `","reason":"x"}`
		}, http.StatusBadRequest, "validation_failed"},
		{"unknown field", func(f *seatFixture) (string, string) { return f.human(), `{"scopes":["write:approvals"]}` }, http.StatusBadRequest, "validation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			bearer, body := tc.setup(f)
			w := f.do(http.MethodPost, "/v0/first-officer-seats", bearer, body)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status = %d, want %d %s:\n%s", w.Code, tc.status, tc.code, w.Body.String())
			}
			// Committed state: no seat token exists for any anchor.
			f.tokens.mu.Lock()
			defer f.tokens.mu.Unlock()
			for _, tok := range f.tokens.rows {
				if firstofficer.IsSeatSubject(tok.Subject) {
					t.Errorf("a seat token %s was minted on a refused attach", tok.Subject)
				}
			}
		})
	}
}

func TestFirstOfficerSeatAttach_ExistingSeat409(t *testing.T) {
	f := newSeatFixture(t)
	runID := f.seedRun(run.StateRunning, "")
	if w := f.do(http.MethodPost, "/v0/first-officer-seats", f.human(), attachBody(firstofficer.AnchorRun, runID)); w.Code != http.StatusCreated {
		t.Fatalf("first attach = %d:\n%s", w.Code, w.Body.String())
	}
	w := f.do(http.MethodPost, "/v0/first-officer-seats", f.human(), attachBody(firstofficer.AnchorRun, runID))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "first_officer_seat_exists") {
		t.Fatalf("second attach = %d, want 409 first_officer_seat_exists:\n%s", w.Code, w.Body.String())
	}
	if n := f.activeTokens(runSeat(runID)); n != 1 {
		t.Errorf("active seat tokens = %d, want 1", n)
	}
}

// racingTokenRepo simulates a concurrent attach: on the first seat Issue it
// also mints a competitor token with a chosen created_at, AFTER the
// handler's existence pre-check has passed.
type racingTokenRepo struct {
	*fakeTokenRepo
	competitorAge time.Duration // negative = older than ours
	competitor    uuid.UUID
	failList      int // fail the Nth ListForSubject call (1-based); 0 = never
	listCalls     int
}

func (r *racingTokenRepo) Issue(ctx context.Context, subject string, scopes []string) (*apitoken.Token, error) {
	tok, err := r.fakeTokenRepo.Issue(ctx, subject, scopes)
	if err != nil || !firstofficer.IsSeatSubject(subject) || r.competitor != uuid.Nil {
		return tok, err
	}
	comp, err := r.fakeTokenRepo.Issue(ctx, subject, scopes)
	if err != nil {
		return nil, err
	}
	r.competitor = comp.ID
	r.mu.Lock()
	r.rows[comp.ID].CreatedAt = r.rows[tok.ID].CreatedAt.Add(r.competitorAge)
	r.mu.Unlock()
	return tok, nil
}

func (r *racingTokenRepo) ListForSubject(ctx context.Context, subject string) ([]*apitoken.Token, error) {
	r.listCalls++
	if r.failList != 0 && r.listCalls == r.failList {
		return nil, errors.New("tokens down")
	}
	return r.fakeTokenRepo.ListForSubject(ctx, subject)
}

func newRacingSeatFixture(t *testing.T, r *racingTokenRepo) *seatFixture {
	f := newSeatFixture(t)
	r.fakeTokenRepo = f.tokens
	f.s.cfg.APITokenRepo = r
	return f
}

func TestFirstOfficerSeatAttach_PostIssueConvergence(t *testing.T) {
	t.Run("older competitor wins: this request 409, its token revoked", func(t *testing.T) {
		r := &racingTokenRepo{competitorAge: -time.Second}
		f := newRacingSeatFixture(t, r)
		human := f.human()
		runID := f.seedRun(run.StateRunning, "")
		w := f.do(http.MethodPost, "/v0/first-officer-seats", human, attachBody(firstofficer.AnchorRun, runID))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "first_officer_seat_exists") {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		toks, _ := f.tokens.ListForSubject(context.Background(), runSeat(runID))
		if len(toks) != 1 || toks[0].ID != r.competitor {
			t.Fatalf("active tokens = %+v, want only the competitor %s", toks, r.competitor)
		}
		if n := len(f.aud.chainedOf(auditFirstOfficerSeatAttached)); n != 0 {
			t.Errorf("attached rows = %d, want 0 for the losing request", n)
		}
	})
	t.Run("newer competitor loses: 201, competitor revoked", func(t *testing.T) {
		r := &racingTokenRepo{competitorAge: time.Second}
		f := newRacingSeatFixture(t, r)
		human := f.human()
		runID := f.seedRun(run.StateRunning, "")
		w := f.do(http.MethodPost, "/v0/first-officer-seats", human, attachBody(firstofficer.AnchorRun, runID))
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		resp := decodeSeatAttach(t, w.Body.Bytes())
		toks, _ := f.tokens.ListForSubject(context.Background(), runSeat(runID))
		if len(toks) != 1 || toks[0].ID.String() != resp.TokenID {
			t.Fatalf("active tokens = %+v, want only ours %s", toks, resp.TokenID)
		}
	})
	t.Run("convergence read error revokes ours, 500", func(t *testing.T) {
		r := &racingTokenRepo{competitor: uuid.New(), failList: 2}
		f := newRacingSeatFixture(t, r)
		human := f.human()
		runID := f.seedRun(run.StateRunning, "")
		w := f.do(http.MethodPost, "/v0/first-officer-seats", human, attachBody(firstofficer.AnchorRun, runID))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
		}
		if n := f.activeTokens(runSeat(runID)); n != 0 {
			t.Errorf("active seat tokens = %d, want 0", n)
		}
	})
}

func TestFirstOfficerSeatAttach_AuditFailureRevokes(t *testing.T) {
	f := newSeatFixture(t)
	f.aud.globalErr = errors.New("chain down")
	campID := f.seedCampaign(campaign.StateRunning, "")
	w := f.do(http.MethodPost, "/v0/first-officer-seats", f.human(), attachBody(firstofficer.AnchorCampaign, campID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if n := f.activeTokens(campaignSeat(campID)); n != 0 {
		t.Errorf("active seat tokens = %d, want 0 (an unrecorded seat must not exist)", n)
	}
}

func TestFirstOfficerSeat_Unconfigured503(t *testing.T) {
	f := newSeatFixture(t)
	human := f.human()
	f.s.cfg.CampaignRepo = nil
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v0/first-officer-seats", attachBody(firstofficer.AnchorRun, uuid.New())},
		{http.MethodGet, "/v0/first-officer-seats?run_id=" + uuid.NewString(), ""},
		{http.MethodPost, "/v0/first-officer-seats/retire", `{"run_id":"` + uuid.NewString() + `","reason":"x"}`},
	} {
		w := f.do(c.method, c.path, human, c.body)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "first_officer_seats_unconfigured") {
			t.Errorf("%s %s = %d, want 503:\n%s", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

func TestFirstOfficerSeatGet(t *testing.T) {
	f := newSeatFixture(t)
	campID := f.seedCampaign(campaign.StateSucceeded, "")
	w := f.do(http.MethodGet, "/v0/first-officer-seats?campaign_id="+campID.String(), f.human("read:runs"), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var st firstOfficerSeatStatusResponse
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Attached || len(st.TokenIDs) != 0 || !st.AnchorTerminal || st.SeatSubject != campaignSeat(campID) {
		t.Errorf("status = %+v, want unattached terminal campaign seat", st)
	}
	if w := f.do(http.MethodGet, "/v0/first-officer-seats?campaign_id="+campID.String(), f.human("write:approvals"), ""); w.Code != http.StatusForbidden {
		t.Errorf("without read:runs = %d, want 403", w.Code)
	}
	if w := f.do(http.MethodGet, "/v0/first-officer-seats", f.human("read:runs"), ""); w.Code != http.StatusBadRequest {
		t.Errorf("no anchor = %d, want 400", w.Code)
	}
	f.tokens.listErr = errors.New("tokens down")
	if w := f.do(http.MethodGet, "/v0/first-officer-seats?campaign_id="+campID.String(), f.human("read:runs"), ""); w.Code != http.StatusInternalServerError {
		t.Errorf("list error = %d, want 500", w.Code)
	}
}

func TestFirstOfficerSeatRetire(t *testing.T) {
	f := newSeatFixture(t)
	human := f.human()
	runID := f.seedRun(run.StateRunning, "")
	if w := f.do(http.MethodPost, "/v0/first-officer-seats", human, attachBody(firstofficer.AnchorRun, runID)); w.Code != http.StatusCreated {
		t.Fatalf("attach = %d:\n%s", w.Code, w.Body.String())
	}
	retire := func(bearer, body string) (int, string) {
		w := f.do(http.MethodPost, "/v0/first-officer-seats/retire", bearer, body)
		return w.Code, w.Body.String()
	}
	for _, reason := range []string{`""`, `"   "`} {
		if code, body := retire(human, `{"run_id":"`+runID.String()+`","reason":`+reason+`}`); code != http.StatusBadRequest {
			t.Fatalf("reason %s: status = %d, want 400:\n%s", reason, code, body)
		}
	}
	if code, body := retire(human, `{"run_id":"`+runID.String()+`","reason":"`+strings.Repeat("x", firstOfficerSeatNoteMaxLen+1)+`"}`); code != http.StatusBadRequest {
		t.Fatalf("over-long reason: status = %d, want 400:\n%s", code, body)
	}
	if code, body := retire(f.mint("operator-agent/operator-role-v0", "write:approvals"), `{"run_id":"`+runID.String()+`","reason":"done"}`); code != http.StatusForbidden || !strings.Contains(body, "first_officer_attach_requires_human") {
		t.Fatalf("agent retire = %d:\n%s", code, body)
	}
	if n := f.activeTokens(runSeat(runID)); n != 1 {
		t.Fatalf("refused retires revoked the seat: active = %d", n)
	}
	code, body := retire(human, `{"run_id":"`+runID.String()+`","reason":"campaign wound down"}`)
	if code != http.StatusOK {
		t.Fatalf("retire = %d:\n%s", code, body)
	}
	if n := f.activeTokens(runSeat(runID)); n != 0 {
		t.Errorf("active seat tokens after retire = %d, want 0", n)
	}
	rows := f.aud.chainedOf(auditFirstOfficerSeatRetired)
	if len(rows) != 1 || !strings.Contains(string(rows[0].Payload), `"reason":"captain_detached"`) ||
		!strings.Contains(string(rows[0].Payload), `"note":"campaign wound down"`) {
		t.Fatalf("retired rows = %+v", rows)
	}
	if code, body := retire(human, `{"run_id":"`+runID.String()+`","reason":"again"}`); code != http.StatusNotFound || !strings.Contains(body, "first_officer_seat_not_attached") {
		t.Errorf("second retire = %d, want 404 first_officer_seat_not_attached:\n%s", code, body)
	}
}

func TestFirstOfficerSeatRetire_RevokeError500(t *testing.T) {
	f := newSeatFixture(t)
	human := f.human()
	runID := f.seedRun(run.StateRunning, "")
	f.mint(runSeat(runID), firstofficer.SeatScopes()...)
	f.tokens.revokeErr = errors.New("revoke down")
	w := f.do(http.MethodPost, "/v0/first-officer-seats/retire", human, `{"run_id":"`+runID.String()+`","reason":"done"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if n := len(f.aud.chainedOf(auditFirstOfficerSeatRetired)); n != 0 {
		t.Errorf("retired rows = %d, want 0 when nothing was revoked", n)
	}
}

// ---- auto-drive + gate-view surfacing ---------------------------------------

// TestAutoDriveRunGate_FirstOfficerHold pins the delegated arms' hold (C27):
// an open first_officer concern makes may_approve and may_merge
// decision_required first_officer_finding_open with nothing submitted or
// merged; a read error is observe-only (fail-closed).
func TestAutoDriveRunGate_FirstOfficerHold(t *testing.T) {
	t.Run("approve held", func(t *testing.T) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, stages := startAutoDriveRun(t, s, repo)
		seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		hr := &holdReadConcernRepo{fakeConcernRepo: cr, fo: []*concern.Concern{foConcern(runID, concern.StageKindPlan)}}
		s.cfg.ConcernRepo = hr

		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if out.Acted || !out.DecisionRequired || out.DecisionState != codeFirstOfficerFindingOpen || out.Action != delegation.ActionApprove {
			t.Fatalf("outcome = %+v, want decision_required first_officer_finding_open", out)
		}
		if n := countAudit(au, "approval_submitted"); n != 0 {
			t.Errorf("approval_submitted = %d, want 0", n)
		}
		if stages[0].State != run.StageStateAwaitingApproval {
			t.Errorf("plan stage = %q, want still awaiting_approval", stages[0].State)
		}
		// Dispositioned (no longer open) -> the arm proceeds and approves.
		hr.fo = nil
		out, err = s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil || !out.Acted {
			t.Fatalf("after waive outcome = %+v err = %v, want acted approve", out, err)
		}
	})
	t.Run("approve ignores implement-kind on a plan gate", func(t *testing.T) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, _ := startAutoDriveRun(t, s, repo)
		seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr, fo: []*concern.Concern{foConcern(runID, concern.StageKindImplement)}}
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil || !out.Acted {
			t.Fatalf("outcome = %+v err = %v, want acted (no plan-gate hold on an implement-kind concern)", out, err)
		}
	})
	t.Run("approve read error observe-only", func(t *testing.T) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, _ := startAutoDriveRun(t, s, repo)
		seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
		seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr, err: errors.New("concerns down")}
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
		if err != nil || out.Acted || out.DecisionRequired || !strings.Contains(out.Note, "first-officer findings could not be read") {
			t.Fatalf("outcome = %+v err = %v, want observe-only", out, err)
		}
		if n := countAudit(au, "approval_submitted"); n != 0 {
			t.Errorf("approval_submitted = %d, want 0", n)
		}
	})

	mergeReady := func(t *testing.T) (*Server, *autoDriveRepo, *fakeConcernRepo, uuid.UUID) {
		s, repo, au, cr := newAutoDriveServer(t)
		runID, stages := startAutoDriveRun(t, s, repo)
		stages[0].State = run.StageStateSucceeded
		stages[1].State = run.StageStateSucceeded
		if _, err := repo.TransitionRun(context.Background(), runID, run.StateRunning); err != nil {
			t.Fatalf("TransitionRun: %v", err)
		}
		if _, err := repo.SetRunPullRequestURL(context.Background(), runID, "https://github.com/x/y/pull/7"); err != nil {
			t.Fatalf("SetRunPullRequestURL: %v", err)
		}
		seedReviewEntry(t, au, runID, 5, drive.Category, drive.Advance{Rule: drive.RuleChecksGreenAwaitingMerge})
		return s, repo, cr, runID
	}
	t.Run("merge held", func(t *testing.T) {
		s, repo, cr, runID := mergeReady(t)
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr, fo: []*concern.Concern{foConcern(runID, concern.StageKindImplement)}}
		merger := &fakeMerger{}
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if merger.called != 0 || out.Acted || !out.DecisionRequired || out.DecisionState != codeFirstOfficerFindingOpen || out.Action != delegation.ActionMerge {
			t.Fatalf("outcome = %+v merger called %d, want held with no merge", out, merger.called)
		}
	})
	t.Run("merge read error observe-only", func(t *testing.T) {
		s, repo, cr, runID := mergeReady(t)
		merger := &fakeMerger{}
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr, err: errors.New("concerns down")}
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
		if err != nil {
			t.Fatalf("AutoDriveRunGate: %v", err)
		}
		if merger.called != 0 || out.Acted || out.DecisionRequired || !strings.Contains(out.Note, "first-officer findings could not be read") {
			t.Fatalf("outcome = %+v merger called %d, want observe-only", out, merger.called)
		}
	})
	t.Run("unheld merge proceeds", func(t *testing.T) {
		s, repo, cr, runID := mergeReady(t)
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr}
		merger := &fakeMerger{}
		out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), merger, nil)
		if err != nil || !out.Acted || merger.called != 1 {
			t.Fatalf("outcome = %+v err = %v merger %d, want merged", out, err, merger.called)
		}
	})
}

// holdReadConcernRepo answers ListOpenByRun differently ONLY when the caller
// is firstOfficerHoldRefusal: it adds fo (or returns err) there and passes
// every other read through. The delegated may_approve/may_merge conditions and
// the gate view's concern block read the same repo and already react to an
// open concern, which would MASK the hold under test (rule f); this isolates
// the hold so its deletion is observable.
type holdReadConcernRepo struct {
	*fakeConcernRepo
	fo  []*concern.Concern
	err error
}

func calledFromFirstOfficerHold() bool {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		fr, more := frames.Next()
		if strings.HasSuffix(fr.Function, ".firstOfficerHoldRefusal") {
			return true
		}
		if !more {
			return false
		}
	}
}

func (r *holdReadConcernRepo) ListOpenByRun(ctx context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	base, err := r.fakeConcernRepo.ListOpenByRun(ctx, runID)
	if err != nil || !calledFromFirstOfficerHold() {
		return base, err
	}
	if r.err != nil {
		return nil, r.err
	}
	return append(base, r.fo...), nil
}

func foConcern(runID uuid.UUID, kind string) *concern.Concern {
	return &concern.Concern{ID: uuid.New(), RunID: runID, StageKind: kind, Provenance: concern.ProvenanceFirstOfficer,
		State: concern.StateRaised, Severity: "high", Category: crewConvertedConcernCategory}
}

// TestGateView_MergeReadiness_FirstOfficerHold pins C28: an open
// first_officer concern is a merge_readiness blocker; a read error is an
// Undetermined entry plus the merge_readiness history gap.
func TestGateView_MergeReadiness_FirstOfficerHold(t *testing.T) {
	t.Run("blocker", func(t *testing.T) {
		s, repo, _, cr := readinessGateViewServer(t)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, nil)
		c := seedFOConcern(t, cr, runRow.ID, concern.StageKindImplement, concern.ProvenanceFirstOfficer)
		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		b := mergeBlockerByCode(resp.MergeReadiness, codeFirstOfficerFindingOpen)
		if b == nil {
			t.Fatalf("blockers = %+v, want first_officer_finding_open", resp.MergeReadiness.Blockers)
		}
		if ids, _ := b.Details["concern_ids"].([]any); len(ids) != 1 || ids[0] != c.ID.String() {
			t.Errorf("concern_ids = %v, want [%s]", b.Details["concern_ids"], c.ID)
		}
	})
	t.Run("reviewer concern is no blocker", func(t *testing.T) {
		s, repo, _, cr := readinessGateViewServer(t)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, nil)
		seedFOConcern(t, cr, runRow.ID, concern.StageKindImplement, "")
		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		if mergeBlockerByCode(resp.MergeReadiness, codeFirstOfficerFindingOpen) != nil {
			t.Fatalf("blockers = %+v, want no first-officer hold", resp.MergeReadiness.Blockers)
		}
	})
	t.Run("read error undetermined", func(t *testing.T) {
		s, repo, _, cr := readinessGateViewServer(t)
		runRow := seedMergeReadinessRun(repo, run.StateRunning, mergePR, nil)
		s.cfg.ConcernRepo = &holdReadConcernRepo{fakeConcernRepo: cr, err: errors.New("concerns down")}
		resp := decodeGateView(t, getGateView(t, s, runRow.ID, ""))
		if resp.MergeReadiness == nil || !undeterminedContains(resp.MergeReadiness, "first-officer findings") {
			t.Fatalf("merge_readiness = %+v, want a first-officer undetermined entry", resp.MergeReadiness)
		}
		if !resp.HistoryIncomplete || !containsString(resp.HistoryGaps, gateViewGapMergeReadiness) {
			t.Errorf("history_incomplete=%v gaps=%v, want the merge_readiness gap", resp.HistoryIncomplete, resp.HistoryGaps)
		}
	})
}
