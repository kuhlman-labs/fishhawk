package server

import (
	"context"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// issueEventServer wires a Server with a global-chain audit recorder and the
// default conventions loader, the fixture the issue-lifecycle reconciler tests
// assert against. Unlike the campaign path it needs NO GitHub client — the
// installation comes off the webhook Event, not a repo lookup.
func issueEventServer(t *testing.T) (*Server, *campaignAuditRecorder) {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	au := &campaignAuditRecorder{}
	return New(Config{AuditRepo: au}), au
}

// issuesEvent builds an `issues` webhook.Event fixture with the given action,
// issue number, and state_reason.
func issuesEvent(action string, issueNum int, stateReason string) webhook.Event {
	body := `{
		"action": "` + action + `",
		"repository": {"full_name": "kuhlman-labs/fishhawk"},
		"installation": {"id": 4242},
		"issue": {"number": ` + itoa(issueNum) + `, "state_reason": "` + stateReason + `"}
	}`
	return webhook.Event{
		Type:           "issues",
		Action:         action,
		Repo:           "kuhlman-labs/fishhawk",
		InstallationID: 4242,
		RawBody:        []byte(body),
	}
}

// (a) closed as completed moves a non-terminal card to done and audits
// moved=true on the global chain carrying repo + issue_number + state_reason.
func TestHandleIssueLifecycle_ClosedCompleted_Moves(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "In Progress", To: "Done"}}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	got := fp.calls[0]
	if got.Trigger != lifecycleIssueClosed || got.CanonicalState != workmgmt.CanonicalStateDone {
		t.Errorf("call = trigger %q canonical %q, want issue_closed/done", got.Trigger, got.CanonicalState)
	}
	if got.IssueNumber != 1817 || got.Target.InstallationID != 4242 {
		t.Errorf("call issue=%d install=%d, want 1817/4242", got.IssueNumber, got.Target.InstallationID)
	}
	// The handler must pass the derived issue_closed expected-source set (all
	// configured states minus the target) — a wiring bug that passed nil would
	// slip past the fake provider, which ignores the set.
	if !sameSet(got.ExpectedSourceStates, issueExpectedSourceStates(lifecycleIssueClosed, workmgmt.Default())) {
		t.Errorf("expected sources = %v, want the derived issue_closed set (all states minus done)", got.ExpectedSourceStates)
	}

	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a["moved"] != true || a["trigger"] != lifecycleIssueClosed {
		t.Errorf("audit = %v, want moved=true trigger=issue_closed", a)
	}
	if a["repo"] != "kuhlman-labs/fishhawk" || a["issue_number"] != float64(1817) || a["state_reason"] != "completed" {
		t.Errorf("audit = %v, want repo/issue_number/state_reason from event", a)
	}
}

// (b) closed when the card is already in Done yields a provider skip audited
// skipped=true — the run_merged idempotency overlap: exactly one audit row, no
// second move.
func TestHandleIssueLifecycle_ClosedAlreadyDone_AuditedSkip(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{
		Skipped: true, From: "Done", To: "Done",
		SkipReason: `current status "Done" is not in the expected source set`,
	}}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1 (provider decides the skip)", len(fp.calls))
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want exactly 1 (skips are audited, no second move)", len(audits))
	}
	if audits[0]["skipped"] != true || audits[0]["moved"] != false {
		t.Errorf("audit = %v, want skipped=true moved=false", audits[0])
	}
}

// (c) closed as not_planned leaves the card — the provider is NOT called — and
// audits the deliberate leave-in-place skip naming the state_reason.
func TestHandleIssueLifecycle_ClosedNotPlanned_LeftInPlace(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "not_planned"))

	if len(fp.calls) != 0 {
		t.Fatalf("Transition called %d times for not_planned close, want 0 (leave in place)", len(fp.calls))
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1 (leave-in-place is audited)", len(audits))
	}
	a := audits[0]
	if a["skipped"] != true || a["moved"] != false {
		t.Errorf("audit = %v, want skipped=true moved=false", a)
	}
	if a["state_reason"] != "not_planned" || a["skip_reason"] == "" {
		t.Errorf("audit = %v, want state_reason=not_planned and a non-empty skip_reason", a)
	}
}

// (d) closed as duplicate behaves as not_planned: left in place, audited skip.
func TestHandleIssueLifecycle_ClosedDuplicate_LeftInPlace(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "duplicate"))

	if len(fp.calls) != 0 {
		t.Fatalf("Transition called %d times for duplicate close, want 0", len(fp.calls))
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 || audits[0]["skipped"] != true || audits[0]["state_reason"] != "duplicate" {
		t.Fatalf("audits = %v, want one skipped=true audit naming state_reason=duplicate", audits)
	}
}

// (d2) closed with an absent/null state_reason (GitHub's REST default, which
// closes the issue as completed) advances the card like an explicit "completed":
// the provider IS called and the move is audited with an empty state_reason.
func TestHandleIssueLifecycle_ClosedNullStateReason_Moves(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "In Progress", To: "Done"}}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	// state_reason is absent entirely from the payload (unmarshals to "").
	ev := webhook.Event{
		Type:           "issues",
		Action:         "closed",
		Repo:           "kuhlman-labs/fishhawk",
		InstallationID: 4242,
		RawBody:        []byte(`{"action":"closed","repository":{"full_name":"kuhlman-labs/fishhawk"},"installation":{"id":4242},"issue":{"number":1817}}`),
	}
	s.handleIssueLifecycleBoardSync(context.Background(), ev)

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1 (null state_reason advances like completed)", len(fp.calls))
	}
	if got := fp.calls[0]; got.Trigger != lifecycleIssueClosed || got.CanonicalState != workmgmt.CanonicalStateDone {
		t.Errorf("call = trigger %q canonical %q, want issue_closed/done", got.Trigger, got.CanonicalState)
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 || audits[0]["moved"] != true {
		t.Fatalf("audits = %v, want one moved=true", audits)
	}
	if audits[0]["state_reason"] != "" {
		t.Errorf("audit state_reason = %v, want empty for a null/absent state_reason", audits[0]["state_reason"])
	}
}

// (e) reopened from Done moves the card back to backlog.
func TestHandleIssueLifecycle_Reopened_MovesToBacklog(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "Done", To: "Backlog"}}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("reopened", 1817, ""))

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	got := fp.calls[0]
	if got.Trigger != lifecycleIssueReopened || got.CanonicalState != workmgmt.CanonicalStateBacklog {
		t.Errorf("call = trigger %q canonical %q, want issue_reopened/backlog", got.Trigger, got.CanonicalState)
	}
	// The expected-source set is the issue_closed target (Done) ONLY.
	if !sameSet(got.ExpectedSourceStates, []string{workmgmt.CanonicalStateDone}) {
		t.Errorf("expected sources = %v, want [done] only", got.ExpectedSourceStates)
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 || audits[0]["moved"] != true {
		t.Fatalf("audits = %v, want one moved=true", audits)
	}
}

// (f) reopened when the card sits in a human-chosen non-done column is a
// provider skip: the expected-source set is the issue_closed target only, so
// the never-fight-the-human guard leaves it. Still audited.
func TestHandleIssueLifecycle_ReopenedElsewhere_Skip(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{
		Skipped: true, From: "In Progress", To: "Backlog",
		SkipReason: `current status "In Progress" is not in the expected source set`,
	}}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("reopened", 1817, ""))

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	if !sameSet(fp.calls[0].ExpectedSourceStates, []string{workmgmt.CanonicalStateDone}) {
		t.Errorf("expected sources = %v, want [done] only (reopen pulls from done only)", fp.calls[0].ExpectedSourceStates)
	}
	audits := campaignTransitionAudits(au)
	if len(audits) != 1 || audits[0]["skipped"] != true {
		t.Fatalf("audits = %v, want one skipped=true", audits)
	}
}

// (g1) an unmapped action (e.g. issues.opened) is a silent no-op.
func TestHandleIssueLifecycle_UnmappedAction_NoOp(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("opened", 1817, ""))

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for unmapped action, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written for unmapped action, want none")
	}
}

// (g1b) a mapped action (issues.closed) whose corresponding transition key is
// absent from conv.Transitions is a silent no-op: the action IS recognized but
// no board edge is configured for it, so the provider is never called.
func TestHandleIssueLifecycle_UnmappedTransition_NoOp(t *testing.T) {
	prev := conventionsLoader
	conv := workmgmt.Default()
	conv.Transitions = map[string]string{} // states present, but no issue_closed edge
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	au := &campaignAuditRecorder{}
	s := New(Config{AuditRepo: au})

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for an unmapped transition key, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written for an unmapped transition key, want none")
	}
}

// (g2) an empty states map (no board configured) is a silent no-op even for a
// mapped action.
func TestHandleIssueLifecycle_EmptyStates_NoOp(t *testing.T) {
	prev := conventionsLoader
	conv := workmgmt.Default()
	conv.States = nil
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return conv, nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	au := &campaignAuditRecorder{}
	s := New(Config{AuditRepo: au})

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times with empty states map, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written with empty states map, want none")
	}
}

// (g3) a malformed payload / missing issue number is a silent no-op.
func TestHandleIssueLifecycle_MalformedPayload_NoOp(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	ev := webhook.Event{Type: "issues", Action: "closed", Repo: "kuhlman-labs/fishhawk", RawBody: []byte(`{not json`)}
	s.handleIssueLifecycleBoardSync(context.Background(), ev)
	// Well-formed JSON but no issue number is also a no-op.
	ev2 := webhook.Event{Type: "issues", Action: "closed", Repo: "kuhlman-labs/fishhawk", RawBody: []byte(`{"action":"closed"}`)}
	s.handleIssueLifecycleBoardSync(context.Background(), ev2)

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for malformed payload, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written for malformed payload, want none")
	}
}

// (h) a genuine provider error is best-effort: attempted, no audit, no panic.
func TestHandleIssueLifecycle_ProviderError_Swallowed(t *testing.T) {
	fp := &fakeTransitionProvider{transErr: context.DeadlineExceeded}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written despite provider error, want none")
	}
}

// (i) a provider that does not implement Transitioner is a silent no-op.
func TestHandleIssueLifecycle_NonTransitioner_NoOp(t *testing.T) {
	workmgmt.Register(&fileOnlyProvider{name: workmgmt.Default().Provider})
	t.Cleanup(func() { registerTransitionProvider(t, &fakeTransitionProvider{}) })
	au := &campaignAuditRecorder{}
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })
	s := New(Config{AuditRepo: au})

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written for non-Transitioner provider, want none")
	}
}

// (k) a conventions-load error is best-effort: WARN log + return, provider
// never called, no audit written — the reconciler must not proceed past a
// failed conventions load.
func TestHandleIssueLifecycle_ConventionsError_NoOp(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{}, context.DeadlineExceeded
	}
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	au := &campaignAuditRecorder{}
	s := New(Config{AuditRepo: au})

	s.handleIssueLifecycleBoardSync(context.Background(), issuesEvent("closed", 1817, "completed"))

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times after a conventions-load error, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written after a conventions-load error, want none")
	}
}

// (l) an unsplittable repo full name (no owner/name) is a silent no-op: the
// transition target cannot be built, so the provider is never called.
func TestHandleIssueLifecycle_UnsplittableRepo_NoOp(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	s, au := issueEventServer(t)

	// A repo with no "/" fails splitRepoFullName after conventions load and
	// provider resolution succeed (the default loader ignores the repo arg).
	ev := webhook.Event{
		Type:           "issues",
		Action:         "closed",
		Repo:           "not-a-valid-repo",
		InstallationID: 4242,
		RawBody:        []byte(`{"action":"closed","installation":{"id":4242},"issue":{"number":1817,"state_reason":"completed"}}`),
	}
	s.handleIssueLifecycleBoardSync(context.Background(), ev)

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for an unsplittable repo, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written for an unsplittable repo, want none")
	}
}

// (j) the issue_closed expected-source derivation excludes the target state and
// includes every OTHER configured canonical state.
func TestIssueExpectedSourceStates_ClosedExcludesTarget(t *testing.T) {
	conv := workmgmt.Default() // issue_closed -> done

	closed := issueExpectedSourceStates(lifecycleIssueClosed, conv)
	if containsState(closed, workmgmt.CanonicalStateDone) {
		t.Errorf("issue_closed sources = %v, must NOT contain the target %q", closed, workmgmt.CanonicalStateDone)
	}
	for state := range conv.States {
		if state == workmgmt.CanonicalStateDone {
			continue
		}
		if !containsState(closed, state) {
			t.Errorf("issue_closed sources = %v, want to contain every non-target state incl %q", closed, state)
		}
	}
	// Exactly all-states-minus-one.
	if len(closed) != len(conv.States)-1 {
		t.Errorf("issue_closed sources = %v (len %d), want len %d (all states minus the target)", closed, len(closed), len(conv.States)-1)
	}

	// issue_reopened accepts the issue_closed target only.
	reopened := issueExpectedSourceStates(lifecycleIssueReopened, conv)
	if !sameSet(reopened, []string{workmgmt.CanonicalStateDone}) {
		t.Errorf("issue_reopened sources = %v, want [done] only", reopened)
	}
}

// fileOnlyProvider implements workmgmt.Provider but NOT Transitioner, so the
// reconciler's provider-capability guard no-ops.
type fileOnlyProvider struct{ name string }

func (f *fileOnlyProvider) Name() string { return f.name }
func (f *fileOnlyProvider) File(_ context.Context, _ workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: f.name}, nil
}
