package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// approvals.members enforcement (#4116): the gate's members list is an
// exact-match approver allow-list, enforced PRE-Submit in
// checkApprovalPredicates (403 approver_predicate_unmet, no row) and at COUNT
// time in approveStageAs (filterListedSubjects). These tests drive the real
// POST /v0/stages/{id}/approvals handler (and approveStageAs directly for the
// in-process path that bypasses pre-Submit) over the approval-repo, run-repo
// and audit fakes, asserting the persisted row count, the audit payloads and
// the stage transition.

// approveStageAsFn is the fully-qualified name callerScopedGetRunFailRepo
// matches to fail ONLY approveStageAs' own (count-time members) run-row read.
const approveStageAsFn = "github.com/kuhlman-labs/fishhawk/backend/internal/server.(*Server).approveStageAs"

// membersPredicateSpec builds a version 1.0 acceptance-stage approvals block
// (modelled on quorumPredicateSpec) with the given count and members list,
// and NO forge predicate, so the members leg is the only pre-Submit control on
// the gate. members == nil omits the key; an empty non-nil slice emits
// `members: []`.
func membersPredicateSpec(count int, members []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `version: "1.0"
workflows:
  feature_change:
    stages:
      - id: gate
        type: acceptance
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: %d
              not: [author, agent]
`, count)
	if members != nil {
		quoted := make([]string, len(members))
		for i, m := range members {
			quoted[i] = fmt.Sprintf("%q", m)
		}
		fmt.Fprintf(&b, "              members: [%s]\n", strings.Join(quoted, ", "))
	}
	return []byte(b.String())
}

// seedMembersStage seeds an acceptance stage awaiting approval on a run whose
// cached spec is membersPredicateSpec(count, members). installationRef != ""
// sets the run's forge family (observationForgeID splits on the first colon).
func seedMembersStage(t *testing.T, rr *approvalRunRepo, au *approvalAuditFake, count int, members []string, installationRef string) *run.Stage {
	t.Helper()
	specBytes := membersPredicateSpec(count, members)
	if _, err := spec.ParseBytes(specBytes); err != nil {
		t.Fatalf("fixture spec does not parse: %v\n%s", err, specBytes)
	}
	stage := seedQuorumStageSpec(rr, au, specBytes)
	if installationRef != "" {
		rr.mu.Lock()
		rr.runs[stage.RunID].InstallationRef = &installationRef
		rr.mu.Unlock()
	}
	return stage
}

func newMembersRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func membersRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v0/stages/x/approvals", nil)
}

func approvalRowCount(t *testing.T, ar *fakeApprovalRepo, stage *run.Stage) int {
	t.Helper()
	rows, err := ar.ListForStage(context.Background(), stage.ID)
	if err != nil {
		t.Fatalf("ListForStage: %v", err)
	}
	return len(rows)
}

func membersStageState(rr *approvalRunRepo, stage *run.Stage) run.StageState {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return rr.stages[stage.ID].State
}

// assertMembersRefusal asserts the members-leg 403 shape and returns its
// details.
func assertMembersRefusal(t *testing.T, code string, details map[string]any, wantPrincipal string, wantMembers []string) {
	t.Helper()
	if code != "approver_predicate_unmet" {
		t.Errorf("code = %q, want approver_predicate_unmet", code)
	}
	if details["predicate"] != "members" {
		t.Errorf("details.predicate = %v, want members", details["predicate"])
	}
	if details["members_principal"] != wantPrincipal {
		t.Errorf("details.members_principal = %v, want %q", details["members_principal"], wantPrincipal)
	}
	if details["result"] != "rejected" {
		t.Errorf("details.result = %v, want rejected", details["result"])
	}
	got, _ := details["members"].([]any)
	if len(got) != len(wantMembers) {
		t.Fatalf("details.members = %v, want %v", details["members"], wantMembers)
	}
	for i, m := range wantMembers {
		if got[i] != m {
			t.Errorf("details.members[%d] = %v, want %q", i, got[i], m)
		}
	}
}

// T1: an unlisted human on a members-only gate is refused 403 with NO row,
// the stage stays awaiting_approval, and the rejection snapshot records the
// list and members_listed=false. CF1 vehicle: with the members leg's condition
// forced false the handler inserts mallory's row and returns 200.
func TestSubmitApproval_Members_UnlistedRefused(t *testing.T) {
	s, ar, rr, au, idp := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	stage := seedMembersStage(t, rr, au, 1, []string{"github:alice"}, "")

	w := submitApprovalAs(t, s, stage.ID, "github:mallory", `{"decision":"approve"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeApprovalError(t, w)
	assertMembersRefusal(t, code, details, "github:mallory", []string{"github:alice"})
	if details["subject"] != "github:mallory" {
		t.Errorf("details.subject = %v, want github:mallory", details["subject"])
	}
	if n := approvalRowCount(t, ar, stage); n != 0 {
		t.Errorf("approval rows = %d, want 0 (refused pre-Submit)", n)
	}
	if got := membersStageState(rr, stage); got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval", got)
	}
	snap := predicateRejectionSnapshot(t, au, "github:mallory")
	if !reflect.DeepEqual(snap["members"], []any{"github:alice"}) {
		t.Errorf("rejection snapshot members = %v, want [github:alice]", snap["members"])
	}
	if snap["members_listed"] != false {
		t.Errorf("rejection snapshot members_listed = %v, want false", snap["members_listed"])
	}
	if snap["members_principal"] != "github:mallory" {
		t.Errorf("rejection snapshot members_principal = %v, want github:mallory", snap["members_principal"])
	}
	if idp.permCalls != 0 || idp.memberCalls != 0 {
		t.Errorf("forge calls perm=%d member=%d, want 0 (members is a local check)", idp.permCalls, idp.memberCalls)
	}
}

// T2: listed approvers are admitted and counted; the second advances the gate,
// and the approval_submitted snapshot carries the declared list plus the
// members verdict copied from the pre-Submit resolution (C6: the members-only
// early return populates MembersPrincipal / MembersListed).
func TestSubmitApproval_Members_ListedCounted(t *testing.T) {
	s, ar, rr, au, idp := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	members := []string{"github:alice", "github:bob"}
	stage := seedMembersStage(t, rr, au, 2, members, "")

	w := submitApprovalAs(t, s, stage.ID, "github:alice", `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("alice status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := membersStageState(rr, stage); got != run.StageStateAwaitingApproval {
		t.Fatalf("after alice: state = %q, want awaiting_approval (count 2)", got)
	}
	w = submitApprovalAs(t, s, stage.ID, "github:bob", `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("bob status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := membersStageState(rr, stage); got != run.StageStateSucceeded {
		t.Errorf("after bob: state = %q, want succeeded", got)
	}
	if n := approvalRowCount(t, ar, stage); n != 2 {
		t.Errorf("approval rows = %d, want 2", n)
	}
	snap, _ := approvalPayloadFor(t, au, "github:bob")["predicate_snapshot"].(map[string]any)
	if !reflect.DeepEqual(snap["members"], []any{"github:alice", "github:bob"}) {
		t.Errorf("snapshot members = %v, want the declared list", snap["members"])
	}
	if snap["quorum_reached"] != true {
		t.Errorf("snapshot quorum_reached = %v, want true", snap["quorum_reached"])
	}
	if snap["members_principal"] != "github:bob" || snap["members_listed"] != true {
		t.Errorf("snapshot members_principal=%v members_listed=%v, want github:bob / true", snap["members_principal"], snap["members_listed"])
	}
	if snap["count_eligible"] != float64(2) {
		t.Errorf("snapshot count_eligible = %v, want 2", snap["count_eligible"])
	}
	if idp.permCalls != 0 || idp.memberCalls != 0 {
		t.Errorf("forge calls perm=%d member=%d, want 0 — the members-only gate returns before resolvePredicates", idp.permCalls, idp.memberCalls)
	}
}

// T2b (C6): the members-only early return's SHAPE, read directly — a listed
// principal on a gate with no forge predicate returns a non-nil resolution
// carrying MembersPrincipal / MembersListed=true and no forge fields.
func TestCheckApprovalPredicates_Members_ListedNoForgeReturnShape(t *testing.T) {
	s, _, rr, au, idp := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	stage := seedMembersStage(t, rr, au, 1, []string{"github:alice"}, "")

	w := newMembersRecorder()
	res, ok := s.checkApprovalPredicates(w, membersRequest(), stage, "github:alice", false)
	if !ok {
		t.Fatalf("ok = false, want true:\n%s", w.Body.String())
	}
	if res == nil || res.MembersPrincipal != "github:alice" || res.MembersListed == nil || !*res.MembersListed {
		t.Fatalf("resolution = %+v, want MembersPrincipal=github:alice MembersListed=true", res)
	}
	if res.ResolvedPermission != "" || res.MemberResolved != nil {
		t.Errorf("forge fields = %q / %v, want empty (no forge predicate evaluated)", res.ResolvedPermission, res.MemberResolved)
	}
	if idp.permCalls != 0 || idp.memberCalls != 0 {
		t.Errorf("forge calls perm=%d member=%d, want 0", idp.permCalls, idp.memberCalls)
	}
}

// T2c: a members gate that ALSO declares a forge predicate runs both legs; the
// satisfied forge resolution carries the members verdict too.
func TestCheckApprovalPredicates_Members_WithForgePredicateCarriesBoth(t *testing.T) {
	s, _, rr, au, idp := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	specBytes := []byte(strings.Replace(string(quorumPredicateSpec(1, "", "acme/reviewers")),
		"              member_of: acme/reviewers\n",
		"              member_of: acme/reviewers\n              members: [\"github:alice\"]\n", 1))
	stage := seedQuorumStageSpec(rr, au, specBytes)

	res, ok := s.checkApprovalPredicates(newMembersRecorder(), membersRequest(), stage, "github:alice", false)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if res == nil || res.MemberResolved == nil || !*res.MemberResolved || res.MembersListed == nil || !*res.MembersListed || res.MembersPrincipal != "github:alice" {
		t.Fatalf("resolution = %+v, want member_of resolved true AND members listed for github:alice", res)
	}
	if idp.memberCalls != 1 {
		t.Errorf("memberCalls = %d, want 1 (the forge leg still runs after a listed members leg)", idp.memberCalls)
	}

	// And an unlisted subject is refused by the members leg BEFORE any forge call.
	s2, _, rr2, au2, idp2 := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	stage2 := seedQuorumStageSpec(rr2, au2, specBytes)
	w := newMembersRecorder()
	if _, ok := s2.checkApprovalPredicates(w, membersRequest(), stage2, "github:mallory", false); ok {
		t.Fatal("ok = true, want the members leg to refuse github:mallory")
	}
	code, details := decodeApprovalError(t, w)
	assertMembersRefusal(t, code, details, "github:mallory", []string{"github:alice"})
	if idp2.memberCalls != 0 {
		t.Errorf("memberCalls = %d, want 0 (refused before the forge leg)", idp2.memberCalls)
	}
}

// T3: a plain member is qualified with the RUN's forge family.
func TestSubmitApproval_Members_PlainMemberQualifiedByRunForge(t *testing.T) {
	t.Run("github run admits github:alice", func(t *testing.T) {
		s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
		stage := seedMembersStage(t, rr, au, 1, []string{"alice"}, "")
		w := submitApprovalAs(t, s, stage.ID, "github:alice", `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if n := approvalRowCount(t, ar, stage); n != 1 {
			t.Errorf("approval rows = %d, want 1", n)
		}
		if got := membersStageState(rr, stage); got != run.StageStateSucceeded {
			t.Errorf("state = %q, want succeeded", got)
		}
	})
	t.Run("gitlab run refuses github:alice", func(t *testing.T) {
		s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
		stage := seedMembersStage(t, rr, au, 1, []string{"alice"}, "gitlab:42")
		w := submitApprovalAs(t, s, stage.ID, "github:alice", `{"decision":"approve"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
		}
		if n := approvalRowCount(t, ar, stage); n != 0 {
			t.Errorf("approval rows = %d, want 0", n)
		}
	})
	t.Run("gitlab run admits gitlab:alice", func(t *testing.T) {
		s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
		stage := seedMembersStage(t, rr, au, 1, []string{"alice"}, "gitlab:42")
		w := submitApprovalAs(t, s, stage.ID, "gitlab:alice", `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		if n := approvalRowCount(t, ar, stage); n != 1 {
			t.Errorf("approval rows = %d, want 1", n)
		}
	})
}

// T4: a plain (static-token) subject is never qualified, so it matches no
// member — even one spelled identically. CF4 vehicle: qualifying the subject
// too would canonicalise both sides of the first case to github:alice.
func TestSubmitApproval_Members_StaticSubjectNeverMatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []string
		subject string
	}{
		{"plain member vs plain subject", []string{"alice"}, "alice"},
		{"forge member vs static MCP subject", []string{"github:kuhlman-labs"}, "brett@local-mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
			stage := seedMembersStage(t, rr, au, 1, tc.members, "")
			w := submitApprovalAs(t, s, stage.ID, tc.subject, `{"decision":"approve"}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
			}
			code, details := decodeApprovalError(t, w)
			assertMembersRefusal(t, code, details, tc.subject, tc.members)
			if n := approvalRowCount(t, ar, stage); n != 0 {
				t.Errorf("approval rows = %d, want 0", n)
			}
		})
	}
}

// delegatedMembersSpecYAML is delegatedQuorumSpecYAML (a met
// clean_dual_approval may_approve rule on a count-1 plan gate) with the plan
// gate's approvals block carrying members.
func delegatedMembersSpecYAML(t *testing.T, members string) string {
	t.Helper()
	out := strings.Replace(delegatedQuorumSpecYAML,
		"            approvals:\n              count: 1\n",
		"            approvals:\n              count: 1\n              members: "+members+"\n", 1)
	if out == delegatedQuorumSpecYAML {
		t.Fatal("delegated members fixture did not substitute")
	}
	if _, err := spec.ParseBytes([]byte(out)); err != nil {
		t.Fatalf("delegated members fixture does not parse: %v", err)
	}
	return out
}

// startDelegatedMembersRun stands up the delegated-approval harness on a run
// whose plan gate declares members, with clean_dual_approval MET.
func startDelegatedMembersRun(t *testing.T, s *Server, repo *driveE2ERepo, au *auditFake, members string) *run.Stage {
	t.Helper()
	runID, planStage := startDriveE2ERun(t, s, repo, map[string]any{
		"repo": "x/y", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": delegatedMembersSpecYAML(t, members),
	})
	seedReviewEntry(t, au, runID, 1, "plan_review_started", planreview.ReviewStartedPayload{ConfiguredAgents: 2})
	seedReviewEntry(t, au, runID, 2, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	seedReviewEntry(t, au, runID, 3, "plan_reviewed", planreview.PlanReviewedPayload{ReviewerKind: "agent", Verdict: planreview.VerdictApprove})
	return planStage
}

// rejectionSnapshotFromAuditFake is predicateRejectionSnapshot for the
// delegated harness's *auditFake.
func rejectionSnapshotFromAuditFake(t *testing.T, au *auditFake, subject string) map[string]any {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.appended {
		if e.Category != "approval_predicate_rejected" || e.ActorSubject == nil || *e.ActorSubject != subject {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal rejection payload: %v", err)
		}
		snap, _ := payload["predicate_snapshot"].(map[string]any)
		return snap
	}
	t.Fatalf("no approval_predicate_rejected entry for %q", subject)
	return nil
}

// T5: a delegated approve by the LISTED github:alice is admitted (recorded
// under the synthetic delegated subject, on_behalf_of=github:alice) and never
// counted. CF3 vehicle: evaluating the synthetic subject instead of the
// on_behalf_of principal refuses it.
func TestSubmitApproval_Members_DelegatedListedAdmitted(t *testing.T) {
	s, repo, au, _, ar := newDelegatedApprovalServer(t)
	planStage := startDelegatedMembersRun(t, s, repo, au, `["github:alice"]`)

	w := submitApprovalAs(t, s, planStage.ID, "github:alice", `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows, _ := ar.ListForStage(context.Background(), planStage.ID)
	if len(rows) != 1 || rows[0].ApproverSubject != operatorrole.DelegatedApprovalActorSubject {
		t.Fatalf("rows = %+v, want exactly one under %q", rows, operatorrole.DelegatedApprovalActorSubject)
	}
	_, payload := approvalAuditBySubject(t, au, operatorrole.DelegatedApprovalActorSubject)
	if payload["on_behalf_of"] != "github:alice" {
		t.Errorf("on_behalf_of = %v, want github:alice", payload["on_behalf_of"])
	}
	snap, _ := payload["predicate_snapshot"].(map[string]any)
	if snap["quorum_reached"] != false {
		t.Errorf("quorum_reached = %v, want false (admitted, recorded, never counted)", snap["quorum_reached"])
	}
	if snap["members_principal"] != "github:alice" || snap["members_listed"] != true {
		t.Errorf("snapshot members_principal=%v members_listed=%v, want github:alice / true", snap["members_principal"], snap["members_listed"])
	}
	if planStage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage = %q, want awaiting_approval (a delegated vote never advances)", planStage.State)
	}
}

// T6: a delegated approve by the UNLISTED github:mallory under a MET
// may_approve rule is refused 403 with no row; the principal is mallory, never
// the synthetic delegated subject. CF3b vehicle: a delegated early-return
// before the members leg would insert a row under operator-agent/delegated.
func TestSubmitApproval_Members_DelegatedUnlistedRefused(t *testing.T) {
	s, repo, au, _, ar := newDelegatedApprovalServer(t)
	planStage := startDelegatedMembersRun(t, s, repo, au, `["github:alice"]`)

	w := submitApprovalAs(t, s, planStage.ID, "github:mallory", `{"decision":"approve","delegated":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeApprovalError(t, w)
	assertMembersRefusal(t, code, details, "github:mallory", []string{"github:alice"})
	rows, _ := ar.ListForStage(context.Background(), planStage.ID)
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
	snap := rejectionSnapshotFromAuditFake(t, au, "github:mallory")
	if snap["members_principal"] != "github:mallory" {
		t.Errorf("rejection members_principal = %v, want github:mallory (never %s)", snap["members_principal"], operatorrole.DelegatedApprovalActorSubject)
	}
}

// T8: an escalation cannot widen members. A firing count-raise + member_of
// escalation (membership satisfied by the identity fake) still refuses a
// subject absent from the baseline members. CF5 vehicle: dropping members in
// effectiveApprovals admits github:mallory.
func TestSubmitApproval_Members_EscalationCannotWiden(t *testing.T) {
	specYAML := strings.Replace(escalationApprovalSpecYAML,
		"            approvals:\n              count: 1\n",
		"            approvals:\n              count: 1\n              members: [\"github:alice\"]\n", 1)
	if specYAML == escalationApprovalSpecYAML {
		t.Fatal("escalation members fixture did not substitute")
	}

	s, ar, rr, _, _ := newApprovalServerWithIdentity(t,
		&fakeIdentityProvider{perm: identity.PermissionAdmin, member: true})
	stage := seedEscalationApprovalRun(t, rr, specYAML)

	w := submitApprovalAs(t, s, stage.ID, "github:mallory", `{"decision":"approve"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeApprovalError(t, w)
	assertMembersRefusal(t, code, details, "github:mallory", []string{"github:alice"})
	if details["escalated"] != true {
		t.Errorf("details.escalated = %v, want true (the escalation fired)", details["escalated"])
	}
	if n := approvalRowCount(t, ar, stage); n != 0 {
		t.Errorf("approval rows = %d, want 0", n)
	}

	// The listed approver is admitted and the escalated count (2) still holds.
	w = submitApprovalAs(t, s, stage.ID, "github:alice", `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("alice status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := membersStageState(rr, stage); got != run.StageStateAwaitingApproval {
		t.Errorf("state = %q, want awaiting_approval (escalated count 2)", got)
	}
}

// T8 companion: the schema gives an escalation's require.approvals no members
// key, so a spec trying to WIDEN the baseline list through an escalation is
// refused at parse time. Pins a PRE-EXISTING schema control
// (additionalProperties:false) this change relies on.
func TestParseRefusesEscalationMembers(t *testing.T) {
	specYAML := strings.Replace(escalationApprovalSpecYAML,
		"            count: 2\n            member_of: acme/security\n",
		"            count: 2\n            member_of: acme/security\n            members: [\"github:alice\", \"github:mallory\"]\n", 1)
	specYAML = strings.Replace(specYAML,
		"            approvals:\n              count: 1\n",
		"            approvals:\n              count: 1\n              members: [\"github:alice\"]\n", 1)
	if !strings.Contains(specYAML, "github:mallory") {
		t.Fatal("fixture did not substitute")
	}
	if _, err := spec.ParseBytes([]byte(specYAML)); err == nil {
		t.Fatal("spec.ParseBytes accepted an escalation declaring require.approvals.members; an escalation must not be able to widen members")
	}
}

// T9: an absent or empty members list leaves today's behaviour unchanged — an
// arbitrary subject is admitted and advances, and the snapshot carries no
// members key.
func TestSubmitApproval_Members_EmptyOrAbsentUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []string
	}{
		{"absent", nil},
		{"empty", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
			stage := seedMembersStage(t, rr, au, 1, tc.members, "")
			if tc.members != nil && !strings.Contains(string(membersPredicateSpec(1, tc.members)), "members: []") {
				t.Fatal("empty fixture did not emit `members: []`")
			}
			res, ok := s.checkApprovalPredicates(newMembersRecorder(), membersRequest(), stage, "brett@local-mcp", false)
			if !ok || res != nil {
				t.Fatalf("checkApprovalPredicates = (%v, %v), want (nil, true) — not predicate-guarded", res, ok)
			}
			w := submitApprovalAs(t, s, stage.ID, "brett@local-mcp", `{"decision":"approve"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if got := membersStageState(rr, stage); got != run.StageStateSucceeded {
				t.Errorf("state = %q, want succeeded", got)
			}
			if n := approvalRowCount(t, ar, stage); n != 1 {
				t.Errorf("approval rows = %d, want 1", n)
			}
			snap, _ := approvalPayloadFor(t, au, "brett@local-mcp")["predicate_snapshot"].(map[string]any)
			for _, k := range []string{"members", "members_principal", "members_listed"} {
				if _, present := snap[k]; present {
					t.Errorf("snapshot carries %s=%v, want the key absent", k, snap[k])
				}
			}
		})
	}
}

// T10: the members leg's run-row read fails → the retryable 503
// run_row_unreadable shape with predicate:members and no row. CF6 vehicle: a
// github fallback would admit github:alice (200).
func TestSubmitApproval_Members_RunRowUnreadable(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
	stage := seedMembersStage(t, rr, au, 1, []string{"github:alice"}, "")
	fake := &callerScopedGetRunFailRepo{approvalRunRepo: rr, fn: checkApprovalPredicatesFn,
		err: errors.New("dial tcp: connection reset by peer")}
	s.cfg.RunRepo = fake

	w := submitApprovalAs(t, s, stage.ID, "github:alice", `{"decision":"approve"}`)
	if fake.Fired() < 1 {
		t.Fatalf("fault never fired (fired=%d); the checkApprovalPredicates run-row read is not a direct GetRun call in that function", fake.Fired())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeApprovalError(t, w)
	if code != "forge_unavailable" || details["reason"] != "run_row_unreadable" || details["retryable"] != true || details["predicate"] != "members" {
		t.Errorf("code=%q details=%v, want forge_unavailable / run_row_unreadable / retryable / predicate members", code, details)
	}
	if n := approvalRowCount(t, ar, stage); n != 0 {
		t.Errorf("approval rows = %d, want 0", n)
	}
}

// T11: the in-process path (approveStageAs called directly, no pre-Submit
// leg) records an unlisted human's row but does NOT count it. CF2 vehicle: an
// identity filterListedSubjects advances the stage.
func TestApproveStageAs_Members_UnlistedNotCounted(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
	stage := seedMembersStage(t, rr, au, 1, []string{"github:alice"}, "")

	res, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:mallory"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if res.Stage.State != run.StageStateAwaitingApproval {
		t.Errorf("returned state = %q, want awaiting_approval", res.Stage.State)
	}
	if got := membersStageState(rr, stage); got != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval — an unlisted approval must not count", got)
	}
	if n := approvalRowCount(t, ar, stage); n != 1 {
		t.Errorf("approval rows = %d, want 1 (recorded, not counted)", n)
	}
	snap, _ := approvalPayloadFor(t, au, "github:mallory")["predicate_snapshot"].(map[string]any)
	if snap["count_eligible"] != float64(0) {
		t.Errorf("count_eligible = %v, want 0", snap["count_eligible"])
	}
	if !reflect.DeepEqual(snap["members"], []any{"github:alice"}) {
		t.Errorf("snapshot members = %v, want [github:alice]", snap["members"])
	}
}

// T11b: the in-process path still counts a LISTED approver (the filter keeps
// listed subjects; it is not a blanket refusal).
func TestApproveStageAs_Members_ListedCounted(t *testing.T) {
	s, _, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
	stage := seedMembersStage(t, rr, au, 1, []string{"alice"}, "")

	if _, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:alice"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove}); err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if got := membersStageState(rr, stage); got != run.StageStateSucceeded {
		t.Errorf("stage state = %q, want succeeded", got)
	}
}

// T12: the count-time run-row read fails → not advanced (fail toward NOT
// advancing). The member is PLAIN so the forge is actually consulted. CF7
// vehicle: a github fallback qualifies alice → github:alice and advances.
func TestApproveStageAs_Members_CountTimeRunReadFails_NotAdvanced(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
	stage := seedMembersStage(t, rr, au, 1, []string{"alice"}, "")
	fake := &callerScopedGetRunFailRepo{approvalRunRepo: rr, fn: approveStageAsFn,
		err: errors.New("dial tcp: connection reset by peer")}
	s.cfg.RunRepo = fake

	res, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:alice"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("approveStageAs: %v", err)
	}
	if fake.Fired() < 1 {
		t.Fatalf("fault never fired (fired=%d); approveStageAs' members run-row read is not a direct GetRun call in that function", fake.Fired())
	}
	if res.Stage.State != run.StageStateAwaitingApproval || membersStageState(rr, stage) != run.StageStateAwaitingApproval {
		t.Errorf("state = %q / %q, want awaiting_approval — an unreadable forge must not advance the gate", res.Stage.State, membersStageState(rr, stage))
	}
	if n := approvalRowCount(t, ar, stage); n != 1 {
		t.Errorf("approval rows = %d, want 1", n)
	}
}

// T13: a NON-delegated agent-kind subject on a members gate is now refused
// 403 with no row (it was previously recorded but never counted) — the
// deliberate 200→403 change. CF1 vehicle alongside T1.
func TestSubmitApproval_Members_NonDelegatedAgentKindRefused(t *testing.T) {
	s, ar, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{})
	stage := seedMembersStage(t, rr, au, 1, []string{"github:alice"}, "")
	agent := operatorrole.TokenSubjectPrefix + "driver"
	if actorKindForSubject(agent) != audit.ActorAgent {
		t.Fatalf("fixture subject %q is not agent-kind", agent)
	}

	w := submitApprovalAs(t, s, stage.ID, agent, `{"decision":"approve"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	code, details := decodeApprovalError(t, w)
	assertMembersRefusal(t, code, details, agent, []string{"github:alice"})
	if n := approvalRowCount(t, ar, stage); n != 0 {
		t.Errorf("approval rows = %d, want 0", n)
	}
	snap := predicateRejectionSnapshot(t, au, agent)
	if !reflect.DeepEqual(snap["members"], []any{"github:alice"}) || snap["members_principal"] != agent {
		t.Errorf("rejection snapshot members=%v members_principal=%v, want [github:alice] / %s", snap["members"], snap["members_principal"], agent)
	}
}
