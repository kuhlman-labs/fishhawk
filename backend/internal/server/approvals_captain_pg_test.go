package server

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The delegation leg of ADR-083 rule 6 (E76.3 / #3766): a DELEGATED approve
// names the repository's seated captain on the existing approval_submitted
// entry. These tests drive the REAL approveStageAs -> writeApprovalAudit ->
// currentCaptain -> captain.Store.Read -> audit_entries path, with the captain
// seated BY CONSTRUCTION on a pgtest chain (captainPG.seedCaptain).

const captainTestRepo = "acme/captained"

// agentDriverIdentity is an ALREADY agent-kind operator identity:
// effectiveApprovalSubject leaves it unchanged, so the delegated remap names
// no human principal and today's on_behalf_of is empty.
func agentDriverIdentity() Identity {
	return Identity{Subject: operatorrole.TokenSubjectPrefix + "driver", TokenID: "tok-driver", Scopes: []string{"write:approvals"}}
}

// captainApprovalFixture is an approval server (fake approval/run/audit repos)
// whose CaptainStore is the REAL store over a pgtest database. store=false
// leaves CaptainStore nil (the unwired deployment).
type captainApprovalFixture struct {
	srv   *Server
	rr    *approvalRunRepo
	au    *approvalAuditFake
	cap   *captainPG
	stage *run.Stage
}

func newCaptainApprovalFixture(t *testing.T, store bool, count int) *captainApprovalFixture {
	t.Helper()
	s, _, rr, au := newApprovalServer(t)
	stage := seedQuorumRunStage(t, rr, count, run.StageStateAwaitingApproval)
	rr.runs[stage.RunID].Repo = captainTestRepo
	f := &captainApprovalFixture{srv: s, rr: rr, au: au, stage: stage}
	if store {
		f.cap = newCaptainPG(t, nil)
		s.cfg.CaptainStore = f.cap.srv.cfg.CaptainStore
	}
	return f
}

// approve submits one approve under id (delegated when rule != "") and
// returns the resulting approval_submitted payload, read back from the audit
// the call wrote.
func (f *captainApprovalFixture) approve(t *testing.T, id Identity, rule string) map[string]any {
	t.Helper()
	before := len(f.au.appended)
	if _, err := f.srv.approveStageAs(context.Background(), id, approveActionParams{
		Stage: f.stage, Decision: approval.DecisionApprove, DelegatedRule: rule,
	}); err != nil {
		t.Fatalf("approveStageAs(%s): %v", id.Subject, err)
	}
	return findApprovalSubmittedPayload(t, f.au.appended[before:])
}

func approvalPayloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertNoCaptainKeys asserts the three E76.3 keys are absent — the
// byte-identical-to-today posture of every degrade branch.
func assertNoCaptainKeys(t *testing.T, p map[string]any) {
	t.Helper()
	for _, k := range []string{"on_behalf_of_basis", "captain_subject"} {
		if v, ok := p[k]; ok {
			t.Errorf("payload carries %s=%v, want the key absent", k, v)
		}
	}
}

// TestDelegatedApprove_AgentDriverRecordsCaptainAsPrincipal (C7): an
// already-agent identity leaves today's on_behalf_of EMPTY, so the seated
// captain becomes the principal with basis "captain".
func TestDelegatedApprove_AgentDriverRecordsCaptainAsPrincipal(t *testing.T) {
	f := newCaptainApprovalFixture(t, true, 1)
	f.cap.seedCaptain(t, captainTestRepo, "github:alice")

	p := f.approve(t, agentDriverIdentity(), "rule-a")
	if p["on_behalf_of"] != "github:alice" || p["on_behalf_of_basis"] != onBehalfOfBasisCaptain {
		t.Errorf("on_behalf_of=%v basis=%v, want github:alice / captain", p["on_behalf_of"], p["on_behalf_of_basis"])
	}
	if _, ok := p["captain_subject"]; ok {
		t.Errorf("captain_subject=%v, want absent (the captain IS the recorded principal)", p["captain_subject"])
	}
	if p["delegated"] != "rule-a" {
		t.Errorf("delegated = %v, want rule-a (existing key unchanged)", p["delegated"])
	}
}

// TestDelegatedApprove_HumanDriverRecordsBoth (C8): a delegated approve
// driven by a human token holder keeps on_behalf_of as that human and adds the
// DIFFERENT seated captain alongside.
func TestDelegatedApprove_HumanDriverRecordsBoth(t *testing.T) {
	f := newCaptainApprovalFixture(t, true, 1)
	f.cap.seedCaptain(t, captainTestRepo, "github:alice")

	p := f.approve(t, eligibleApproverIdentity("github:bob"), "rule-a")
	if p["on_behalf_of"] != "github:bob" {
		t.Errorf("on_behalf_of = %v, want github:bob (the real operator, unchanged)", p["on_behalf_of"])
	}
	if p["captain_subject"] != "github:alice" {
		t.Errorf("captain_subject = %v, want github:alice", p["captain_subject"])
	}
	if _, ok := p["on_behalf_of_basis"]; ok {
		t.Errorf("on_behalf_of_basis = %v, want absent (on_behalf_of names the human, not the seat)", p["on_behalf_of_basis"])
	}
}

// TestDelegatedApprove_HumanDriverIsCaptainOmitsCaptainSubject isolates the
// inequality in C8: when the human driver IS the captain no redundant
// captain_subject is written.
func TestDelegatedApprove_HumanDriverIsCaptainOmitsCaptainSubject(t *testing.T) {
	f := newCaptainApprovalFixture(t, true, 1)
	f.cap.seedCaptain(t, captainTestRepo, "github:bob")

	p := f.approve(t, eligibleApproverIdentity("github:bob"), "rule-a")
	if p["on_behalf_of"] != "github:bob" {
		t.Errorf("on_behalf_of = %v, want github:bob", p["on_behalf_of"])
	}
	assertNoCaptainKeys(t, p)
}

// TestDelegatedApprove_NonDelegatedNeverReadsCaptain: an ordinary
// (non-delegated) approve on a captained repo carries no captain key — the
// branch is gated on the delegated rule.
func TestDelegatedApprove_NonDelegatedNeverReadsCaptain(t *testing.T) {
	f := newCaptainApprovalFixture(t, true, 1)
	f.cap.seedCaptain(t, captainTestRepo, "github:alice")

	p := f.approve(t, eligibleApproverIdentity("github:bob"), "")
	if _, ok := p["on_behalf_of"]; ok {
		t.Errorf("on_behalf_of = %v, want absent on a non-delegated approve", p["on_behalf_of"])
	}
	assertNoCaptainKeys(t, p)
}

// TestDelegatedApprove_DegradesToToday: every branch of the trichotomy that
// is not "captain" — a vacant seat, an unwired store, a failed read, a run
// account that is not a UUID — records EXACTLY today's payload keys, for both
// the agent-driver and the human-driver shape.
func TestDelegatedApprove_DegradesToToday(t *testing.T) {
	// today = the unwired-store payload key sets, the pre-E76.3 baseline.
	today := func(t *testing.T, id Identity) []string {
		f := newCaptainApprovalFixture(t, false, 1)
		return approvalPayloadKeys(f.approve(t, id, "rule-a"))
	}
	drivers := map[string]Identity{
		"agent driver": agentDriverIdentity(),
		"human driver": eligibleApproverIdentity("github:bob"),
	}
	arms := map[string]func(t *testing.T, f *captainApprovalFixture){
		"vacant (never seated)": func(*testing.T, *captainApprovalFixture) {},
		"vacant (relinquished)": func(t *testing.T, f *captainApprovalFixture) {
			f.cap.seedCaptain(t, captainTestRepo, "github:alice")
			f.cap.seedEntry(t, captain.CategoryRelinquished, map[string]any{"repo": captainTestRepo, "subject": "github:alice", "identity_verified": true})
		},
		"unavailable (read error)": func(t *testing.T, f *captainApprovalFixture) {
			f.cap.seedCaptain(t, captainTestRepo, "github:alice")
			f.cap.pool.Close() // every later Read errors
		},
		"unavailable (run account not a UUID)": func(t *testing.T, f *captainApprovalFixture) {
			f.cap.seedCaptain(t, captainTestRepo, "github:alice")
			f.rr.runs[f.stage.RunID].AccountID = "not-a-uuid"
		},
	}
	for dname, id := range drivers {
		want := today(t, id)
		t.Run(dname+"/unavailable (store not wired)", func(t *testing.T) {
			f := newCaptainApprovalFixture(t, false, 1)
			if got := approvalPayloadKeys(f.approve(t, id, "rule-a")); !reflect.DeepEqual(got, want) {
				t.Errorf("keys = %v, want today's %v", got, want)
			}
		})
		for aname, arm := range arms {
			t.Run(dname+"/"+aname, func(t *testing.T) {
				f := newCaptainApprovalFixture(t, true, 1)
				arm(t, f)
				p := f.approve(t, id, "rule-a")
				if got := approvalPayloadKeys(p); !reflect.DeepEqual(got, want) {
					t.Errorf("keys = %v, want today's %v", got, want)
				}
				assertNoCaptainKeys(t, p)
				if id.Subject == "github:bob" && p["on_behalf_of"] != "github:bob" {
					t.Errorf("on_behalf_of = %v, want github:bob unchanged", p["on_behalf_of"])
				}
			})
		}
	}
}

// TestDelegatedApprove_ReadsRunAccountPartition: the captain is read in the
// RUN's account partition. The seat exists ONLY in acct's partition, so a
// read of the untenanted partition would report vacant and write nothing.
func TestDelegatedApprove_ReadsRunAccountPartition(t *testing.T) {
	f := newCaptainApprovalFixture(t, true, 1)
	acct := uuid.New()
	f.cap.seedAccountCaptain(t, acct, captainTestRepo, "github:alice")
	f.rr.runs[f.stage.RunID].AccountID = acct.String()

	p := f.approve(t, agentDriverIdentity(), "rule-a")
	if p["on_behalf_of"] != "github:alice" {
		t.Errorf("on_behalf_of = %v, want github:alice from the run's account partition", p["on_behalf_of"])
	}
}

// TestDelegatedApprove_CaptainDoesNotInterfereWithQuorum (C9, ADR-083 rule
// 1) is an INVARIANT: the same approve sequence against identical count:2
// fixtures — once with no captain record, once with a seated captain who is
// NOT an approver — yields identical eligible counts, quorum decisions, stage
// states and transitions at every step.
func TestDelegatedApprove_CaptainDoesNotInterfereWithQuorum(t *testing.T) {
	type step struct {
		eligible int
		required int
		reached  bool
		state    run.StageState
	}
	sequence := func(t *testing.T, seated bool) ([]step, []run.StageState) {
		f := newCaptainApprovalFixture(t, seated, 2)
		if seated {
			f.cap.seedCaptain(t, captainTestRepo, "github:alice")
		}
		var steps []step
		for _, a := range []struct {
			id   Identity
			rule string
		}{
			{agentDriverIdentity(), "rule-a"},                 // delegated, never counts
			{eligibleApproverIdentity("github:r1"), "rule-a"}, // delegated human, never counts
			{eligibleApproverIdentity("github:r1"), ""},       // r1's real vote
			{eligibleApproverIdentity("github:r2"), ""},       // reaches quorum
		} {
			res, err := f.srv.approveStageAs(context.Background(), a.id, approveActionParams{
				Stage: f.stage, Decision: approval.DecisionApprove, DelegatedRule: a.rule,
			})
			if err != nil {
				t.Fatalf("approve %s: %v", a.id.Subject, err)
			}
			snap := lastPredicateSnapshot(t, f.au)
			steps = append(steps, step{snap.CountEligible, snap.CountRequired, snap.QuorumReached, res.Stage.State})
		}
		var to []run.StageState
		for _, tr := range f.rr.transitions {
			to = append(to, tr.To)
		}
		return steps, to
	}
	s0, t0 := sequence(t, false)
	s1, t1 := sequence(t, true)
	if !reflect.DeepEqual(s0, s1) || !reflect.DeepEqual(t0, t1) {
		t.Errorf("a seated captain changed the quorum path:\n  no captain: %+v transitions %v\n  captain:    %+v transitions %v", s0, t0, s1, t1)
	}
	// Anchor the invariant to the real decision so a both-arms regression
	// cannot pass as "identical".
	last := s1[len(s1)-1]
	if last.eligible != 2 || !last.reached || last.state != run.StageStateSucceeded || len(t1) != 1 {
		t.Errorf("final step = %+v transitions %v, want 2 eligible, quorum reached, succeeded once", last, t1)
	}
}
