package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
)

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
	if got := s.countDistinctEligibleApprovers(context.Background(), stageID, author); got != 2 {
		t.Errorf("countDistinctEligibleApprovers = %d, want 2", got)
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
}
