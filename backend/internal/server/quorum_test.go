package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

func TestResolvePredicates(t *testing.T) {
	ptr := func(i int) *int { return &i }
	one := ptr(1)

	t.Run("satisfied: both predicates pass, each called once", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true}
		s := New(Config{IdentityProvider: idp})
		outcome, res, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MinPermission: "write", MemberOf: "acme/reviewers"})
		if outcome != predicateSatisfied {
			t.Fatalf("outcome = %v, want satisfied", outcome)
		}
		if res.ResolvedPermission != "admin" {
			t.Errorf("resolved permission = %q, want admin", res.ResolvedPermission)
		}
		if res.MemberResolved == nil || !*res.MemberResolved {
			t.Errorf("member resolved = %v, want true", res.MemberResolved)
		}
		if idp.permCalls != 1 || idp.memberCalls != 1 {
			t.Errorf("calls = perm %d/member %d, want 1/1", idp.permCalls, idp.memberCalls)
		}
	})

	t.Run("rejected: permission below required tier", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionWrite}
		s := New(Config{IdentityProvider: idp})
		outcome, res, predicate := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MinPermission: "maintain"})
		if outcome != predicateRejected {
			t.Fatalf("outcome = %v, want rejected", outcome)
		}
		if res.ResolvedPermission != "write" {
			t.Errorf("resolved permission = %q, want write", res.ResolvedPermission)
		}
		if predicate != "min_permission" {
			t.Errorf("predicate = %q, want min_permission", predicate)
		}
	})

	t.Run("rejected: non-member", func(t *testing.T) {
		idp := &fakeIdentityProvider{member: false}
		s := New(Config{IdentityProvider: idp})
		outcome, res, predicate := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MemberOf: "acme/reviewers"})
		if outcome != predicateRejected {
			t.Fatalf("outcome = %v, want rejected", outcome)
		}
		if res.MemberResolved == nil || *res.MemberResolved {
			t.Errorf("member resolved = %v, want false", res.MemberResolved)
		}
		if predicate != "member_of" {
			t.Errorf("predicate = %q, want member_of", predicate)
		}
	})

	t.Run("unavailable: PermissionLevel error", func(t *testing.T) {
		idp := &fakeIdentityProvider{permErr: identity.ErrRateLimited}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MinPermission: "write"})
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable", outcome)
		}
	})

	t.Run("unavailable: ResolveMembership error", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin, memberErr: errors.New("boom")}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MinPermission: "write", MemberOf: "acme/reviewers"})
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable", outcome)
		}
	})

	t.Run("unavailable: empty repo when permission required (fail closed)", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "", "github:op",
			&spec.Approvals{Count: one, MinPermission: "write"})
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable (empty repo)", outcome)
		}
		if idp.permCalls != 0 {
			t.Errorf("permCalls = %d, want 0 (short-circuit before the forge call)", idp.permCalls)
		}
	})

	t.Run("unavailable: unparseable min_permission (fail closed)", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			&spec.Approvals{Count: one, MinPermission: "superuser"})
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable (unparseable tier)", outcome)
		}
	})
}

func TestSplitProviderSubject(t *testing.T) {
	cases := []struct {
		in           string
		wantProvider string
		wantRest     string
	}{
		{"github:octocat", "github", "octocat"},
		{"gitlab:alice", "gitlab", "alice"},
		{"octocat", "", "octocat"},   // prefixless
		{"", "", ""},                 // empty
		{":octocat", "", ":octocat"}, // leading colon → no provider
		{"github:a:b", "github", "a:b"},
	}
	for _, c := range cases {
		gotProvider, gotRest := splitProviderSubject(c.in)
		if gotProvider != c.wantProvider || gotRest != c.wantRest {
			t.Errorf("splitProviderSubject(%q) = (%q, %q), want (%q, %q)",
				c.in, gotProvider, gotRest, c.wantProvider, c.wantRest)
		}
	}
}

func TestApprovalChannel(t *testing.T) {
	// delegated wins regardless of identity shape.
	if got := approvalChannel(Identity{TokenID: "tok"}, true); got != "delegated" {
		t.Errorf("delegated channel = %q, want delegated", got)
	}
	// cookie session (no TokenID) → interactive.
	if got := approvalChannel(Identity{Subject: "github:op"}, false); got != "interactive" {
		t.Errorf("session channel = %q, want interactive", got)
	}
	// bearer token → api.
	if got := approvalChannel(Identity{Subject: "github:op", TokenID: "tok"}, false); got != "api" {
		t.Errorf("token channel = %q, want api", got)
	}
}

func TestEligibleApprover(t *testing.T) {
	const author = "github:author"
	// The change author is excluded.
	if eligibleApprover(author, author) {
		t.Errorf("change author must not be eligible")
	}
	// An agent-kind subject (operator-agent token prefix) is excluded.
	agent := operatorrole.CampaignActorSubject
	if eligibleApprover(agent, author) {
		t.Errorf("agent-kind subject must not be eligible")
	}
	// A distinct human subject is eligible.
	if !eligibleApprover("github:reviewer", author) {
		t.Errorf("distinct human must be eligible")
	}
	// With no resolved author, only the agent leg applies.
	if !eligibleApprover("github:anyone", "") {
		t.Errorf("with unresolved author, a human is eligible")
	}
	if eligibleApprover(agent, "") {
		t.Errorf("agent stays ineligible even with unresolved author")
	}
}

func TestSubmitterClass(t *testing.T) {
	const author = "github:author"
	if got := submitterClass(author, author, false); got != "author" {
		t.Errorf("class = %q, want author", got)
	}
	if got := submitterClass("op:agent", author, true); got != "agent" {
		t.Errorf("class = %q, want agent", got)
	}
	if got := submitterClass("github:reviewer", author, false); got != "eligible" {
		t.Errorf("class = %q, want eligible", got)
	}
	// author beats agent when the subject is both the author and agent-kind.
	if got := submitterClass(author, author, true); got != "author" {
		t.Errorf("class = %q, want author (author beats agent)", got)
	}
}

// resolveChangeAuthorFake is a minimal audit repo returning a seeded
// ListForRun so resolveChangeAuthor's found / not-found branches are unit
// testable without Postgres.
type resolveChangeAuthorFake struct {
	audit.BaseFake
	entries []*audit.Entry
	listErr error
}

func (f *resolveChangeAuthorFake) ListForRun(context.Context, uuid.UUID) ([]*audit.Entry, error) {
	return f.entries, f.listErr
}

func userEntry(subject string) *audit.Entry {
	k := audit.ActorUser
	s := subject
	return &audit.Entry{ActorKind: &k, ActorSubject: &s}
}

func agentEntry(subject string) *audit.Entry {
	k := audit.ActorAgent
	s := subject
	return &audit.Entry{ActorKind: &k, ActorSubject: &s}
}

func TestResolveChangeAuthor(t *testing.T) {
	newServer := func(f *resolveChangeAuthorFake) *Server {
		return New(Config{AuditRepo: f})
	}

	t.Run("earliest user-kind actor wins", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{
			agentEntry("op:agent"), // agent first — skipped
			userEntry("github:human"),
			userEntry("github:other"),
		}}
		got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New())
		if !ok || got != "github:human" {
			t.Errorf("resolveChangeAuthor = (%q, %v), want (github:human, true)", got, ok)
		}
	})

	t.Run("no user-kind actor → not found", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{agentEntry("op:agent")}}
		if _, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New()); ok {
			t.Errorf("resolveChangeAuthor ok = true, want false (no user actor)")
		}
	})

	t.Run("list error → fail-open not found", func(t *testing.T) {
		f := &resolveChangeAuthorFake{listErr: context.DeadlineExceeded}
		if _, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New()); ok {
			t.Errorf("resolveChangeAuthor ok = true on list error, want false")
		}
	})
}

func TestCountDistinctEligibleApprovers(t *testing.T) {
	const author = "github:author"
	runID := uuid.New()
	stageID := uuid.New()
	repo := newFakeApprovalRepo()
	seed := func(subject string, d approval.Decision) {
		repo.all = append(repo.all, &approval.Approval{
			StageID: stageID, ApproverSubject: subject, Decision: d,
		})
	}
	seed("github:r1", approval.DecisionApprove)
	seed("github:r1", approval.DecisionApprove)                       // duplicate subject — counts once
	seed("github:r2", approval.DecisionApprove)                       // distinct eligible
	seed(author, approval.DecisionApprove)                            // author — excluded
	seed(operatorrole.CampaignActorSubject, approval.DecisionApprove) // agent — excluded
	seed("github:r3", approval.DecisionReject)                        // reject — not counted

	s := New(Config{ApprovalRepo: repo})
	// No AuditRepo wired → delegatedApproverSubjects fails open to empty.
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author); got != 2 {
		t.Errorf("countDistinctEligibleApprovers = %d, want 2", got)
	}
}

// TestCountDistinctEligibleApprovers_ExcludesDelegatedHuman pins the fix-up:
// a prior DELEGATED non-agent (human) approval — recorded in the audit log
// with a non-empty `delegated` rule — is excluded from the human quorum, even
// though its approval row is indistinguishable from a normal human approve and
// eligibleApprover would otherwise count it (#1709).
func TestCountDistinctEligibleApprovers_ExcludesDelegatedHuman(t *testing.T) {
	const author = "github:author"
	runID := uuid.New()
	stageID := uuid.New()
	repo := newFakeApprovalRepo()
	repo.all = append(repo.all,
		&approval.Approval{StageID: stageID, ApproverSubject: "github:r1", Decision: approval.DecisionApprove},
		&approval.Approval{StageID: stageID, ApproverSubject: "github:r2", Decision: approval.DecisionApprove},
	)

	au := &resolveChangeAuthorFake{entries: []*audit.Entry{
		// github:r1's prior approval was delegated → must not count.
		approvalSubmittedEntry("github:r1", `{"delegated":"clean_dual_approval"}`),
		// github:r2's approval carries no delegated rule → counts.
		approvalSubmittedEntry("github:r2", `{}`),
	}}

	s := New(Config{ApprovalRepo: repo, AuditRepo: au})
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author); got != 1 {
		t.Errorf("countDistinctEligibleApprovers = %d, want 1 (delegated github:r1 excluded)", got)
	}
}

// approvalSubmittedEntry builds an approval_submitted audit entry for a subject
// with the given raw JSON payload, for the delegated-exclusion count tests.
func approvalSubmittedEntry(subject, payload string) *audit.Entry {
	s := subject
	return &audit.Entry{
		Category:     "approval_submitted",
		ActorSubject: &s,
		Payload:      json.RawMessage(payload),
	}
}

func TestPredicateSnapshotMarshaling(t *testing.T) {
	reached := &predicateSnapshot{
		CountRequired:  2,
		CountEligible:  2,
		Identity:       snapshotIdentityFor("github:op"),
		SubmitterClass: "eligible",
		AuthMethod:     "static",
		Channel:        "api",
		MinPermission:  "write",
		MemberOf:       "acme/reviewers",
		QuorumReached:  true,
	}
	raw, err := json.Marshal(reached)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"count_required", "count_eligible", "identity", "submitter_class", "auth_method", "channel", "min_permission", "member_of", "quorum_reached"} {
		if _, ok := m[k]; !ok {
			t.Errorf("snapshot missing key %q: %s", k, raw)
		}
	}
	if m["quorum_reached"] != true {
		t.Errorf("quorum_reached = %v, want true", m["quorum_reached"])
	}
	id, _ := m["identity"].(map[string]any)
	if id["provider"] != "github" || id["subject"] != "github:op" {
		t.Errorf("identity = %v, want {github, github:op}", id)
	}

	// Below-quorum snapshot: omitempty fields drop when empty, quorum_reached false.
	below := &predicateSnapshot{
		CountRequired:  2,
		CountEligible:  1,
		Identity:       snapshotIdentityFor("op:agent"),
		SubmitterClass: "agent",
		Channel:        "delegated",
		QuorumReached:  false,
	}
	rawBelow, _ := json.Marshal(below)
	var mb map[string]any
	if err := json.Unmarshal(rawBelow, &mb); err != nil {
		t.Fatalf("unmarshal below: %v", err)
	}
	if mb["quorum_reached"] != false {
		t.Errorf("quorum_reached = %v, want false", mb["quorum_reached"])
	}
	if _, ok := mb["auth_method"]; ok {
		t.Errorf("auth_method must be omitted when empty: %s", rawBelow)
	}
	if _, ok := mb["min_permission"]; ok {
		t.Errorf("min_permission must be omitted when empty: %s", rawBelow)
	}
	// The additive forge-resolution fields drop when unset (byte-identical
	// to a #1709 count-only snapshot).
	for _, k := range []string{"resolved_permission", "member_resolved", "predicate_result"} {
		if _, ok := mb[k]; ok {
			t.Errorf("%s must be omitted when unset: %s", k, rawBelow)
		}
	}

	// A resolved snapshot carries resolved_permission, member_resolved, and
	// predicate_result.
	member := true
	resolved := &predicateSnapshot{
		CountRequired:      1,
		CountEligible:      1,
		Identity:           snapshotIdentityFor("github:op"),
		SubmitterClass:     "eligible",
		Channel:            "api",
		MinPermission:      "write",
		MemberOf:           "acme/reviewers",
		QuorumReached:      true,
		ResolvedPermission: "admin",
		MemberResolved:     &member,
		PredicateResult:    "satisfied",
	}
	rawResolved, _ := json.Marshal(resolved)
	var mr map[string]any
	if err := json.Unmarshal(rawResolved, &mr); err != nil {
		t.Fatalf("unmarshal resolved: %v", err)
	}
	if mr["resolved_permission"] != "admin" {
		t.Errorf("resolved_permission = %v, want admin", mr["resolved_permission"])
	}
	if mr["member_resolved"] != true {
		t.Errorf("member_resolved = %v, want true", mr["member_resolved"])
	}
	if mr["predicate_result"] != "satisfied" {
		t.Errorf("predicate_result = %v, want satisfied", mr["predicate_result"])
	}
}

// seedQuorumRunStage seeds a plan stage in `state` plus a run row whose cached
// workflow-v1 spec carries an approval gate with the given distinct-approver
// count, so fetchApprovalsForStage resolves the quorum block for the stage. The
// approvals predicate is a workflow-v1 feature, so the spec pins version 1.0.
func seedQuorumRunStage(t *testing.T, rr *approvalRunRepo, count int, state run.StageState) *run.Stage {
	t.Helper()
	st := rr.seedStage(state)
	workflowSpec := []byte(fmt.Sprintf(`version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvals:
              count: %d
      - id: implement
        type: implement
        executor:
          agent: claude-code
`, count))
	rr.seedRun(&run.Run{ID: st.RunID, WorkflowID: "feature_change", WorkflowSpec: workflowSpec})
	return st
}

// eligibleApproverIdentity builds a token identity (TokenID non-empty so the
// write-scope check actually runs) that carries write:approvals and is a
// distinct human subject — an eligible quorum voter.
func eligibleApproverIdentity(subject string) Identity {
	return Identity{Subject: subject, TokenID: "tok-" + subject, Scopes: []string{"write:approvals"}}
}

// lastPredicateSnapshot decodes the predicate_snapshot from the most recent
// approval_submitted audit entry the fake captured.
func lastPredicateSnapshot(t *testing.T, au *approvalAuditFake) predicateSnapshot {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := len(au.appended) - 1; i >= 0; i-- {
		e := au.appended[i]
		if e.Category != "approval_submitted" {
			continue
		}
		var payload struct {
			PredicateSnapshot *predicateSnapshot `json:"predicate_snapshot"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal approval_submitted payload: %v", err)
		}
		if payload.PredicateSnapshot == nil {
			t.Fatalf("approval_submitted entry carries no predicate_snapshot: %s", e.Payload)
		}
		return *payload.PredicateSnapshot
	}
	t.Fatalf("no approval_submitted audit entry captured")
	return predicateSnapshot{}
}

// TestApproveStageAs_Quorum_TwoApproversAdvanceOnce is the liveness / no-stall
// assertion of the #1734 resolution at the server-orchestration layer: two
// distinct eligible approvers against a count:2 gate advance the stage to
// succeeded exactly once, and the advancing row's predicate snapshot observed
// the full quorum (count_eligible == 2). The first below-quorum approve records
// its row without advancing; the second reaches quorum and advances.
func TestApproveStageAs_Quorum_TwoApproversAdvanceOnce(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := seedQuorumRunStage(t, rr, 2, run.StageStateAwaitingApproval)

	// First distinct eligible approver: recorded, below quorum, no advance.
	res1, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:r1"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if res1.Duplicate != nil {
		t.Fatalf("first approve returned a duplicate on a fresh stage")
	}
	if res1.Stage == nil || res1.Stage.State != run.StageStateAwaitingApproval {
		t.Fatalf("after first approve state = %v, want awaiting_approval (below quorum)", res1.Stage)
	}
	if len(rr.transitions) != 0 {
		t.Fatalf("first approve recorded %d transitions, want 0 (below quorum)", len(rr.transitions))
	}

	// Second distinct eligible approver: quorum reached, advance exactly once.
	res2, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:r2"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("second approve: %v", err)
	}
	if res2.Stage == nil || res2.Stage.State != run.StageStateSucceeded {
		t.Fatalf("after second approve state = %v, want succeeded (quorum reached)", res2.Stage)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want exactly one → succeeded", rr.transitions)
	}

	// The counted-approver snapshot on the advancing row observed the full
	// quorum: count_eligible == 2, quorum_reached true.
	snap := lastPredicateSnapshot(t, au)
	if snap.CountEligible != 2 {
		t.Errorf("count_eligible = %d, want 2 (last committer sees full quorum)", snap.CountEligible)
	}
	if !snap.QuorumReached {
		t.Errorf("quorum_reached = false, want true")
	}
}

// TestApproveStageAs_Quorum_LateApproveOnSettledStage_NoDoubleAdvance is the
// safety / no-double-advance assertion of the #1734 resolution. A count:1 gate
// whose stage has ALREADY settled to a terminal state (a prior reject failed it
// category-D, or the SLA timed out) receives a late approve that reaches quorum
// in the count read — but advanceStage's TransitionStage guard rejects
// failed → succeeded with InvalidTransitionError, which approveStageAs surfaces
// as an advance-phase error (mapped to 409 on the HTTP path). The guard fires
// before any state mutation, so the settled stage is never resurrected /
// double-advanced.
func TestApproveStageAs_Quorum_LateApproveOnSettledStage_NoDoubleAdvance(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := seedQuorumRunStage(t, rr, 1, run.StageStateFailed)

	_, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:late"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err == nil {
		t.Fatalf("late approve on a settled stage returned nil error, want an advance-phase InvalidTransition error")
	}
	var aerr *approveActionError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %v, want *approveActionError", err)
	}
	if aerr.failedAt != gateActionAdvance {
		t.Errorf("failedAt = %v, want gateActionAdvance", aerr.failedAt)
	}
	var inv run.InvalidTransitionError
	if !errors.As(aerr.err, &inv) {
		t.Fatalf("wrapped err = %v, want run.InvalidTransitionError", aerr.err)
	}
	if inv.To != string(run.StageStateSucceeded) {
		t.Errorf("invalid transition To = %q, want succeeded", inv.To)
	}
	// No successful transition was recorded — the guard blocked the
	// double-advance before mutating state.
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %+v, want none (guard blocked the double-advance)", rr.transitions)
	}
}
