package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
)

// approvals.members against the REAL captain store (#4116). An agent-kind
// DELEGATED submission has no human principal of its own; its principal is
// the seated captain — the on_behalf_of writeApprovalAudit records with basis
// "captain" (E76.3 / #3766). Config.CaptainStore is a concrete *captain.Store,
// so the captain is seated BY CONSTRUCTION on a pgtest chain
// (captainPG.seedCaptain) and read through captain chain → approvalPrincipal →
// the members leg.

// driveE2ECaptainRepo is the repo startDriveE2ERun creates runs for, so the
// captain seat is keyed to the run row's repo.
const driveE2ECaptainRepo = "x/y"

// T7: an agent-kind delegated submission (operator-agent/driver, MET
// may_approve rule) is admitted when the SEATED captain is listed and refused
// when the seat is VACANT (no principal, fail-closed). CF3c vehicle: an
// approvalPrincipal agent branch returning "" unconditionally turns the
// seated arm into a 403; the vacant arm stays green.
func TestSubmitApproval_Members_DelegatedAgentKindUsesCaptain(t *testing.T) {
	agent := operatorrole.TokenSubjectPrefix + "driver"

	t.Run("seated listed captain admits", func(t *testing.T) {
		s, repo, au, _, ar := newDelegatedApprovalServer(t)
		cpg := newCaptainPG(t, nil)
		s.cfg.CaptainStore = cpg.srv.cfg.CaptainStore
		cpg.seedCaptain(t, driveE2ECaptainRepo, "github:alice")
		planStage := startDelegatedMembersRun(t, s, repo, au, `["github:alice"]`)

		w := submitApprovalAs(t, s, planStage.ID, agent, `{"decision":"approve","delegated":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (the seated captain github:alice is listed):\n%s", w.Code, w.Body.String())
		}
		rows, _ := ar.ListForStage(context.Background(), planStage.ID)
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want exactly one", rows)
		}
		_, payload := approvalAuditBySubject(t, au, rows[0].ApproverSubject)
		if payload["on_behalf_of"] != "github:alice" {
			t.Errorf("on_behalf_of = %v, want github:alice (the seated captain)", payload["on_behalf_of"])
		}
		snap, _ := payload["predicate_snapshot"].(map[string]any)
		if snap["members_principal"] != "github:alice" || snap["quorum_reached"] != false {
			t.Errorf("snapshot members_principal=%v quorum_reached=%v, want github:alice / false (admitted, never counted)", snap["members_principal"], snap["quorum_reached"])
		}
	})

	t.Run("vacant seat refuses", func(t *testing.T) {
		s, repo, au, _, ar := newDelegatedApprovalServer(t)
		cpg := newCaptainPG(t, nil)
		s.cfg.CaptainStore = cpg.srv.cfg.CaptainStore
		planStage := startDelegatedMembersRun(t, s, repo, au, `["github:alice"]`)

		w := submitApprovalAs(t, s, planStage.ID, agent, `{"decision":"approve","delegated":true}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (no captain seated → no principal):\n%s", w.Code, w.Body.String())
		}
		code, details := decodeApprovalError(t, w)
		assertMembersRefusal(t, code, details, "", []string{"github:alice"})
		rows, _ := ar.ListForStage(context.Background(), planStage.ID)
		if len(rows) != 0 {
			t.Errorf("rows = %+v, want none", rows)
		}
	})
}

// TestApprovalPrincipal_CaptainBranches pins approvalPrincipal's delegated
// agent-kind branch against the real store: the seated captain, or "" when the
// seat is vacant. A delegated HUMAN stays the human even with a different
// captain seated.
func TestApprovalPrincipal_CaptainBranches(t *testing.T) {
	agent := operatorrole.TokenSubjectPrefix + "driver"
	f := newCaptainApprovalFixture(t, true, 1)

	if got := f.srv.approvalPrincipal(context.Background(), f.stage, agent, true); got != "" {
		t.Errorf("vacant seat: approvalPrincipal = %q, want empty", got)
	}
	f.cap.seedCaptain(t, captainTestRepo, "github:alice")
	if got := f.srv.approvalPrincipal(context.Background(), f.stage, agent, true); got != "github:alice" {
		t.Errorf("seated: approvalPrincipal = %q, want github:alice", got)
	}
	if got := f.srv.approvalPrincipal(context.Background(), f.stage, "github:bob", true); got != "github:bob" {
		t.Errorf("delegated human: approvalPrincipal = %q, want github:bob (never the captain)", got)
	}
	if got := f.srv.approvalPrincipal(context.Background(), f.stage, agent, false); got != agent {
		t.Errorf("non-delegated agent: approvalPrincipal = %q, want %q", got, agent)
	}
}
