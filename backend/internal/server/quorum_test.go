package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

// TestQuorum_NoStall_LastCommitterAdvancesOnce is the server-layer liveness
// half of the #1734 resolution: it pins the orchestration outcome that the
// unserialized read-after-write quorum REACHES quorum and advances the stage
// exactly once. Two DISTINCT eligible approvers submit against a count=2
// acceptance gate; the stage stays awaiting_approval after the first vote, then
// transitions to succeeded EXACTLY ONCE on the second (last-committing) vote,
// whose predicate snapshot records count_eligible == 2 — the last committer
// observes the full quorum, so there is no stall. The concurrent interleaving
// itself is proven against real Postgres in
// approval.TestPostgres_Submit_ConcurrentQuorum; here the deterministic server
// fakes pin the resulting advance semantics.
func TestQuorum_NoStall_LastCommitterAdvancesOnce(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stage := seedQuorumStage(rr, au, 2, "github:author")

	// First eligible approve: recorded, below quorum, no advance.
	if w := submitApprovalAs(t, s, stage.ID, "github:r1", `{"decision":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("r1 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := rr.stages[stage.ID].State; got != run.StageStateAwaitingApproval {
		t.Fatalf("after r1 state = %q, want awaiting_approval (below quorum)", got)
	}

	// Second DISTINCT eligible approve: quorum reached, stage advances.
	if w := submitApprovalAs(t, s, stage.ID, "github:r2", `{"decision":"approve"}`); w.Code != http.StatusOK {
		t.Fatalf("r2 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if got := rr.stages[stage.ID].State; got != run.StageStateSucceeded {
		t.Fatalf("after r2 state = %q, want succeeded (quorum reached — no stall)", got)
	}

	// Advanced EXACTLY ONCE: a single succeeded transition, no double-advance.
	succeeded := 0
	for _, tr := range rr.transitions {
		if tr.StageID == stage.ID && tr.To == run.StageStateSucceeded {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Errorf("succeeded transitions = %d, want exactly 1 (advance-once)", succeeded)
	}

	// The last committer's snapshot observed the full quorum.
	snap, _ := approvalPayloadFor(t, au, "github:r2")["predicate_snapshot"].(map[string]any)
	if snap["quorum_reached"] != true {
		t.Errorf("r2 quorum_reached = %v, want true (last committer sees full quorum)", snap["quorum_reached"])
	}
	if snap["count_eligible"].(float64) != 2 {
		t.Errorf("r2 count_eligible = %v, want 2", snap["count_eligible"])
	}
}

// TestQuorum_Safety_InvalidTransitionGuard pins the safety half of the #1734
// resolution: the double-advance guard. When the quorum advance reaches a stage
// whose TransitionStage rejects the transition (advanceStage returning
// run.InvalidTransitionError — what a genuine late/racing second advance against
// an already-advanced stage hits), approveStageAs surfaces it through the
// InvalidTransition path — an *approveActionError whose failedAt is
// gateActionAdvance, unwrapping to the run.InvalidTransitionError the HTTP
// handler maps to 409 — and records NO state transition, so no stage is
// advanced twice.
func TestQuorum_Safety_InvalidTransitionGuard(t *testing.T) {
	s, ar, rr, au := newApprovalServer(t)
	stage := seedQuorumStage(rr, au, 1, "github:author")

	// Force the advance to hit the transition guard, standing in for a genuine
	// double-advance against an already-advanced stage. The existing
	// approvalRunRepo.transitionErr field drives TransitionStage to return this
	// error — no fake defined elsewhere is extended.
	rr.transitionErr = run.InvalidTransitionError{
		Kind: "stage",
		From: string(run.StageStateSucceeded),
		To:   string(run.StageStateSucceeded),
	}

	result, err := s.approveStageAs(context.Background(),
		Identity{Subject: "github:r1"},
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err == nil {
		t.Fatalf("approveStageAs err = nil, want InvalidTransition advance error; result=%+v", result)
	}

	// Surfaced as an advance-phase failure...
	var aerr *approveActionError
	if !errors.As(err, &aerr) {
		t.Fatalf("err = %T, want *approveActionError", err)
	}
	if aerr.failedAt != gateActionAdvance {
		t.Errorf("failedAt = %v, want gateActionAdvance", aerr.failedAt)
	}
	// ...unwrapping to the run.InvalidTransitionError the handler maps to 409.
	var inv run.InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("err does not unwrap to run.InvalidTransitionError: %v", err)
	}

	// No stage was advanced: the guard blocked the transition (no double-advance).
	for _, tr := range rr.transitions {
		if tr.StageID == stage.ID {
			t.Errorf("recorded a transition to %q despite the guard; want none", tr.To)
		}
	}
	// The approval row WAS submitted — Submit precedes advance, so the guard
	// fires only at the transition, never before the row is durable.
	rows, _ := ar.ListForStage(context.Background(), stage.ID)
	if len(rows) != 1 {
		t.Errorf("approval rows = %d, want 1 (Submit succeeded before the advance guard)", len(rows))
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
