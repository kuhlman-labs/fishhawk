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
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "write", MemberOf: "acme/reviewers"}, spec.ComposedRequirements{}))
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
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "maintain"}, spec.ComposedRequirements{}))
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
			effectiveApprovals(&spec.Approvals{Count: one, MemberOf: "acme/reviewers"}, spec.ComposedRequirements{}))
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
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "write"}, spec.ComposedRequirements{}))
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable", outcome)
		}
	})

	t.Run("unavailable: ResolveMembership error", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin, memberErr: errors.New("boom")}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "write", MemberOf: "acme/reviewers"}, spec.ComposedRequirements{}))
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable", outcome)
		}
	})

	t.Run("unavailable: empty repo when permission required (fail closed)", func(t *testing.T) {
		idp := &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "", "github:op",
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "write"}, spec.ComposedRequirements{}))
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
			effectiveApprovals(&spec.Approvals{Count: one, MinPermission: "superuser"}, spec.ComposedRequirements{}))
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
	// The change author is excluded when the gate declares not: [author].
	if eligibleApprover(author, author, true) {
		t.Errorf("change author must not be eligible")
	}
	// An agent-kind subject (operator-agent token prefix) is excluded.
	agent := operatorrole.CampaignActorSubject
	if eligibleApprover(agent, author, true) {
		t.Errorf("agent-kind subject must not be eligible")
	}
	// A distinct human subject is eligible.
	if !eligibleApprover("github:reviewer", author, true) {
		t.Errorf("distinct human must be eligible")
	}
	// With no resolved author, only the agent leg applies.
	if !eligibleApprover("github:anyone", "", true) {
		t.Errorf("with unresolved author, a human is eligible")
	}
	if eligibleApprover(agent, "", true) {
		t.Errorf("agent stays ineligible even with unresolved author")
	}
}

// TestEligibleApprover_AuthorLegConditional pins #2358's asymmetry: the AUTHOR
// leg fires only when the gate's `not:` names "author" (excludeAuthor), while
// the AGENT leg is an unconditional floor asserted in BOTH permutations.
func TestEligibleApprover_AuthorLegConditional(t *testing.T) {
	const author = "github:author"
	agent := operatorrole.CampaignActorSubject

	cases := []struct {
		name          string
		subject       string
		changeAuthor  string
		excludeAuthor bool
		want          bool
	}{
		{"author refused when excluded", author, author, true, false},
		{"author ELIGIBLE when not excluded", author, author, false, true},
		{"non-author eligible when excluded", "github:reviewer", author, true, true},
		{"non-author eligible when not excluded", "github:reviewer", author, false, true},
		// The unconditional agent floor, asserted in both directions.
		{"agent refused when author excluded", agent, author, true, false},
		{"agent refused when author NOT excluded", agent, author, false, false},
		{"agent refused with unresolved author, not excluded", agent, "", false, false},
		// An unresolved author disables the author leg even when declared.
		{"unresolved author → human eligible", "github:anyone", "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eligibleApprover(tc.subject, tc.changeAuthor, tc.excludeAuthor); got != tc.want {
				t.Errorf("eligibleApprover(%q, %q, %v) = %v, want %v",
					tc.subject, tc.changeAuthor, tc.excludeAuthor, got, tc.want)
			}
		})
	}
}

// TestApprovalsExcludeAuthor pins the `not:` reader (#2358). Before this the
// field was parsed, schema-closed and documented but read by no backend code,
// so a declared `not:` was grammar rather than enforcement.
func TestApprovalsExcludeAuthor(t *testing.T) {
	cases := []struct {
		name string
		a    *spec.Approvals
		want bool
	}{
		{"nil block", nil, false},
		{"nil Not", &spec.Approvals{}, false},
		{"empty Not", &spec.Approvals{Not: []string{}}, false},
		{"agent only", &spec.Approvals{Not: []string{"agent"}}, false},
		{"author only", &spec.Approvals{Not: []string{"author"}}, true},
		{"author and agent", &spec.Approvals{Not: []string{"author", "agent"}}, true},
		{"unrecognised member ignored", &spec.Approvals{Not: []string{"Author", "bot"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalsExcludeAuthor(tc.a); got != tc.want {
				t.Errorf("approvalsExcludeAuthor = %v, want %v", got, tc.want)
			}
		})
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

// categoryUserEntry builds a user-kind entry carrying an explicit audit
// category, so the authorship allow-list (#2358) can be exercised per category.
func categoryUserEntry(category, subject string) *audit.Entry {
	e := userEntry(subject)
	e.Category = category
	return e
}

// vouchEntry is the ONLY authorship-category entry today: the operator's
// audited declaration that a hand-pushed commit belongs to this run's lineage.
func vouchEntry(subject string) *audit.Entry {
	return categoryUserEntry(CategoryOperatorCommitVouched, subject)
}

func TestResolveChangeAuthor(t *testing.T) {
	newServer := func(f *resolveChangeAuthorFake) *Server {
		return New(Config{AuditRepo: f})
	}

	t.Run("earliest authorship-category user actor wins", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{
			agentEntry("op:agent"), // agent first — skipped
			vouchEntry("github:human"),
			vouchEntry("github:other"),
		}}
		got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New())
		if !ok || got != "github:human" {
			t.Errorf("resolveChangeAuthor = (%q, %v), want (github:human, true)", got, ok)
		}
	})

	// The positive control. Without it every negative case below would pass
	// vacuously — an allow-list that matched nothing would look identical.
	t.Run("operator_commit_vouched alone → RESOLVED", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{vouchEntry("github:op")}}
		got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New())
		if !ok || got != "github:op" {
			t.Errorf("resolveChangeAuthor = (%q, %v), want (github:op, true)", got, ok)
		}
	})

	// The governance categories: each is operator GATE PARTICIPATION, not
	// authorship, and must resolve NO author (#2358).
	governance := []struct {
		name     string
		category string
	}{
		// The live regression: run e288e92b, audit sequence 48558 — the
		// driver's mechanical record-before-dispatch attribution row.
		{"run_auto_driven", "run_auto_driven"},
		// The case in the issue body.
		{"clarification_answered", "clarification_answered"},
		// Approving with binding conditions is governance, not authorship.
		{"approval_submitted", "approval_submitted"},
		{"scope_amendment_decided", "scope_amendment_decided"},
	}
	for _, g := range governance {
		t.Run("earliest is "+g.name+" → UNRESOLVED", func(t *testing.T) {
			f := &resolveChangeAuthorFake{entries: []*audit.Entry{
				categoryUserEntry(g.category, "github:operator"),
			}}
			if got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New()); ok {
				t.Errorf("resolveChangeAuthor = (%q, true), want unresolved for %s", got, g.category)
			}
		})
	}

	// Proves earliest-match runs over the FILTERED set: a naive "skip if
	// governance but still return the first raw entry" implementation fails.
	t.Run("governance row then vouch → the VOUCHED subject", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{
			categoryUserEntry("run_auto_driven", "github:operator"),
			vouchEntry("github:pusher"),
		}}
		got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New())
		if !ok || got != "github:pusher" {
			t.Errorf("resolveChangeAuthor = (%q, %v), want (github:pusher, true)", got, ok)
		}
	})

	// The ActorUser leg survives the category leg.
	t.Run("agent-kind vouch row → UNRESOLVED", func(t *testing.T) {
		e := agentEntry("op:agent")
		e.Category = CategoryOperatorCommitVouched
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{e}}
		if got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New()); ok {
			t.Errorf("resolveChangeAuthor = (%q, true), want unresolved (agent-kind vouch)", got)
		}
	})

	t.Run("empty ActorSubject skipped, later vouch wins", func(t *testing.T) {
		blank := vouchEntry("")
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{blank, vouchEntry("github:real")}}
		got, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New())
		if !ok || got != "github:real" {
			t.Errorf("resolveChangeAuthor = (%q, %v), want (github:real, true)", got, ok)
		}
	})

	t.Run("no user-kind actor → not found", func(t *testing.T) {
		f := &resolveChangeAuthorFake{entries: []*audit.Entry{agentEntry("op:agent")}}
		if _, ok := newServer(f).resolveChangeAuthor(context.Background(), uuid.New()); ok {
			t.Errorf("resolveChangeAuthor ok = true, want false (no user actor)")
		}
	})

	t.Run("nil audit repo → fail-open not found", func(t *testing.T) {
		if _, ok := New(Config{}).resolveChangeAuthor(context.Background(), uuid.New()); ok {
			t.Errorf("resolveChangeAuthor ok = true with nil AuditRepo, want false")
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
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author, true); got != 2 {
		t.Errorf("countDistinctEligibleApprovers = %d, want 2", got)
	}
	// #2358: on a gate whose `not:` omits "author" the author's row COUNTS.
	// Gating only the 403 and not this count would record a permitted
	// author's approval and then refuse to count it — quorum permanently one
	// short.
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author, false); got != 3 {
		t.Errorf("countDistinctEligibleApprovers(excludeAuthor=false) = %d, want 3 (author counted)", got)
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
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author, true); got != 1 {
		t.Errorf("countDistinctEligibleApprovers = %d, want 1 (delegated github:r1 excluded)", got)
	}
	// The delegated exclusion holds regardless of the author leg (#2358).
	if got := s.countDistinctEligibleApprovers(context.Background(), runID, stageID, author, false); got != 1 {
		t.Errorf("countDistinctEligibleApprovers(excludeAuthor=false) = %d, want 1 (delegated github:r1 still excluded)", got)
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

// seedQuorumRunStageV2 is seedQuorumRunStage's workflow-v2 twin: it caches a
// version "2" spec whose plan-stage approval gate declares only the
// forge-neutral `approvals` block (the sole approval predicate at major 2,
// E52.2 / #2214) with the given distinct-approver count and not: [author,
// agent]. Used by the v2 eligibility-decision tests.
func seedQuorumRunStageV2(t *testing.T, rr *approvalRunRepo, count int, state run.StageState) *run.Stage {
	t.Helper()
	st := rr.seedStage(state)
	workflowSpec := []byte(fmt.Sprintf(`version: "2"
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
              not: [author, agent]
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

// TestFetchApprovalsForStage_V2ApprovalsGate is the quorum-side half of the
// cross-boundary proof (acceptance criterion 6): a run row caching a v2
// approvals-only spec resolves through fetchApprovalsForStage to the gate's
// forge-neutral Approvals block, so a v2 run's eligibility is decided entirely
// by the predicate path — the deleted legacy Approvers form is never involved.
func TestFetchApprovalsForStage_V2ApprovalsGate(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage := seedQuorumRunStageV2(t, rr, 1, run.StageStateAwaitingApproval)

	approvals, err := s.fetchApprovalsForStage(context.Background(), stage)
	if err != nil {
		t.Fatalf("fetchApprovalsForStage: %v", err)
	}
	if approvals == nil {
		t.Fatal("Approvals = nil, want the v2 gate's forge-neutral block")
	}
	if approvals.Count == nil || *approvals.Count != 1 {
		t.Errorf("Approvals.Count = %v, want 1", approvals.Count)
	}
	if len(approvals.Not) != 2 || approvals.Not[0] != "author" || approvals.Not[1] != "agent" {
		t.Errorf("Approvals.Not = %v, want [author agent]", approvals.Not)
	}
}

// TestV2ApprovalsPredicateDecidesEligibility is the operator's binding
// CONDITION 1: it drives a REAL v2 spec cached on a run row through to an
// actual eligibility DECISION and asserts BOTH directions of the
// approvals: {count: 1, not: [author, agent]} predicate. A permissive bug
// shows up only in the refusal case; a broken/bypassed eligibility path shows
// up only in the permit case, so both halves are load-bearing. It asserts the
// decision the resolver path returns (the advanced stage state), not an
// intermediate struct — the retrieval assertions live in
// TestFetchApprovalsForStage_V2ApprovalsGate and the fetchGateForStage twin.
func TestV2ApprovalsPredicateDecidesEligibility(t *testing.T) {
	const author = "github:author"
	s, _, rr, au := newApprovalServer(t)
	stage := seedQuorumRunStageV2(t, rr, 1, run.StageStateAwaitingApproval)
	// Seed the run's change author so resolveChangeAuthor identifies it — the
	// `not: [author]` exclusion turns on this. The seed MUST carry an
	// AUTHORSHIP category (#2358): under the allow-list a bare user-kind entry
	// resolves no author, so DIRECTION 1 below would pass vacuously — asserting
	// a refusal that refuses nobody.
	au.seedRunEntries(stage.RunID, vouchEntry(author))

	// Sanity: the forge-neutral predicate path is the one consulted — the v2
	// gate's approvals block resolves for this stage (not the deleted legacy
	// Approvers path).
	if approvals, err := s.fetchApprovalsForStage(context.Background(), stage); err != nil || approvals == nil {
		t.Fatalf("fetchApprovalsForStage = (%v, %v), want the v2 approvals block", approvals, err)
	}

	// DIRECTION 1 — the PR author is REFUSED. The author's own approve is
	// recorded, but the author is not an eligible approver, so the count:1
	// quorum is NOT reached and the stage stays awaiting_approval (no advance).
	resAuthor, err := s.approveStageAs(context.Background(), eligibleApproverIdentity(author),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("author approve: %v", err)
	}
	if resAuthor.Stage == nil || resAuthor.Stage.State != run.StageStateAwaitingApproval {
		t.Fatalf("after author approve state = %v, want awaiting_approval (author REFUSED, quorum not reached)", resAuthor.Stage)
	}
	if len(rr.transitions) != 0 {
		t.Fatalf("author approve recorded %d stage transitions, want 0 (the author does not count toward the quorum)", len(rr.transitions))
	}

	// DIRECTION 2 — a non-author human is PERMITTED. Their approve is an
	// eligible vote that reaches the count:1 quorum and advances the stage to
	// succeeded.
	resHuman, err := s.approveStageAs(context.Background(), eligibleApproverIdentity("github:reviewer"),
		approveActionParams{Stage: stage, Decision: approval.DecisionApprove})
	if err != nil {
		t.Fatalf("non-author approve: %v", err)
	}
	if resHuman.Stage == nil || resHuman.Stage.State != run.StageStateSucceeded {
		t.Fatalf("after non-author approve state = %v, want succeeded (non-author PERMITTED, quorum reached)", resHuman.Stage)
	}
	if len(rr.transitions) != 1 || rr.transitions[0].To != run.StageStateSucceeded {
		t.Fatalf("transitions = %+v, want exactly one → succeeded", rr.transitions)
	}
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

// TestResolvePredicates_EscalatedConjunction pins the membership CONJUNCTION
// a fired escalation produces (E53.4 / #2227): EVERY composed group must
// resolve true. Two escalations naming disjoint groups therefore produce a
// gate no single approver can clear — the correct fail-closed reading for a
// control that may only raise, surfacing as a gate that cannot clear rather
// than one silently weakened.
func TestResolvePredicates_EscalatedConjunction(t *testing.T) {
	base := &spec.Approvals{Count: func(i int) *int { return &i }(1), MemberOf: "acme/leads"}
	escalated := spec.ComposedRequirements{MemberOf: []string{"acme/security"}}

	t.Run("every group resolved: satisfied, one forge call per group", func(t *testing.T) {
		idp := &fakeIdentityProvider{member: true}
		s := New(Config{IdentityProvider: idp})
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			effectiveApprovals(base, escalated))
		if outcome != predicateSatisfied {
			t.Fatalf("outcome = %v, want satisfied", outcome)
		}
		if idp.memberCalls != 2 {
			t.Errorf("memberCalls = %d, want 2 (the conjunction resolves each group)", idp.memberCalls)
		}
	})

	t.Run("a member of only ONE composed group is refused", func(t *testing.T) {
		idp := &perGroupIdentityProvider{member: map[string]bool{"acme/leads": true, "acme/security": false}}
		s := New(Config{IdentityProvider: idp})
		outcome, res, predicate := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			effectiveApprovals(base, escalated))
		if outcome != predicateRejected {
			t.Fatalf("outcome = %v, want rejected — membership is a conjunction", outcome)
		}
		if predicate != "member_of" {
			t.Errorf("predicate = %q, want member_of", predicate)
		}
		if res.MemberResolved == nil || *res.MemberResolved {
			t.Errorf("member resolved = %v, want the failing group's false", res.MemberResolved)
		}
	})

	t.Run("a membership error on an escalated group fails closed", func(t *testing.T) {
		idp := &perGroupIdentityProvider{
			member: map[string]bool{"acme/leads": true},
			errs:   map[string]error{"acme/security": errors.New("forge down")},
		}
		s := New(Config{IdentityProvider: idp})
		outcome, _, predicate := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			effectiveApprovals(base, escalated))
		if outcome != predicateUnavailable {
			t.Fatalf("outcome = %v, want unavailable", outcome)
		}
		if predicate != "member_of" {
			t.Errorf("predicate = %q, want member_of", predicate)
		}
	})

	t.Run("an escalation adds a predicate to a gate declaring none", func(t *testing.T) {
		idp := &fakeIdentityProvider{member: false}
		s := New(Config{IdentityProvider: idp})
		countOnly := &spec.Approvals{Count: func(i int) *int { return &i }(1)}
		outcome, _, _ := s.resolvePredicates(context.Background(), "acme/repo", "github:op",
			effectiveApprovals(countOnly, escalated))
		if outcome != predicateRejected {
			t.Fatalf("outcome = %v, want rejected — the escalation added the group this gate never declared", outcome)
		}
	})
}

// perGroupIdentityProvider resolves membership PER GROUP, which the shared
// fakeIdentityProvider (one bool for every ref) cannot express — and the
// conjunction's whole point is that different groups resolve differently.
type perGroupIdentityProvider struct {
	fakeIdentityProvider
	member map[string]bool
	errs   map[string]error
}

func (f *perGroupIdentityProvider) ResolveMembership(_ context.Context, ref, _ string) (bool, error) {
	f.memberCalls++
	if err, ok := f.errs[ref]; ok {
		return false, err
	}
	return f.member[ref], nil
}
