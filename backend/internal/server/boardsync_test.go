package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeTransitionProvider is a workmgmt.Provider + Transitioner test double
// that records the TransitionRequest the board-sync hook dispatched (the
// lifecycle->provider seam the cross-boundary test asserts) and returns a
// canned TransitionResult or a configured error.
type fakeTransitionProvider struct {
	name     string
	calls    []workmgmt.TransitionRequest
	result   *workmgmt.TransitionResult
	transErr error
	fileErr  error
}

func (f *fakeTransitionProvider) Name() string { return f.name }

func (f *fakeTransitionProvider) File(_ context.Context, _ workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: f.name}, f.fileErr
}

func (f *fakeTransitionProvider) Transition(_ context.Context, req workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	f.calls = append(f.calls, req)
	if f.transErr != nil {
		return nil, f.transErr
	}
	if f.result != nil {
		return f.result, nil
	}
	return &workmgmt.TransitionResult{Moved: true, From: "Backlog", To: "In Progress"}, nil
}

// registerTransitionProvider registers p under the default conventions'
// provider id (github_projects) so the hook's workmgmt.Get resolves it.
func registerTransitionProvider(t *testing.T, p *fakeTransitionProvider) {
	t.Helper()
	if p.name == "" {
		p.name = workmgmt.Default().Provider
	}
	workmgmt.Register(p)
}

// boardSyncRun seeds a run repo + audit fake wired to a Server with the
// given run, returning the pieces a board-sync test asserts against.
func boardSyncServer(t *testing.T, rn *run.Run) (*Server, *promptRunRepo, *auditFake) {
	t.Helper()
	rr := newPromptRunRepo()
	rr.getRuns[rn.ID] = rn
	au := newAuditFake()
	return New(Config{RunRepo: rr, AuditRepo: au}), rr, au
}

func issueRun(issue string) *run.Run {
	inst := int64(99)
	ref := issue
	return &run.Run{
		ID:             uuid.New(),
		Repo:           "kuhlman-labs/fishhawk",
		State:          run.StateRunning,
		TriggerRef:     &ref,
		InstallationID: &inst,
	}
}

func transitionAudits(au *auditFake) []map[string]any {
	var out []map[string]any
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, p := range au.appended {
		if p.Category != categoryWorkItemTransitioned {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(p.Payload, &m)
		out = append(out, m)
	}
	return out
}

// TestNotifyBoardTransition_LifecycleSeam drives the full cross-boundary
// seam (#1012 / condition (6)): each lifecycle edge resolves the canonical
// state through the conventions, dispatches the registered Transitioner with
// the right canonical state + expected sources, and appends a
// work_item_transitioned audit on the run. A per-layer-only suite would pass
// while this lifecycle->provider->audit seam silently no-ops.
func TestNotifyBoardTransition_LifecycleSeam(t *testing.T) {
	cases := []struct {
		event         string
		wantCanonical string
		wantExpectSrc []string
	}{
		{lifecycleRunStarted, workmgmt.CanonicalStateInProgress, []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext}},
		{lifecyclePROpened, workmgmt.CanonicalStateInReview, []string{workmgmt.CanonicalStateInProgress}},
		{lifecycleRunMerged, workmgmt.CanonicalStateDone, []string{workmgmt.CanonicalStateInReview, workmgmt.CanonicalStateInProgress, workmgmt.CanonicalStateBlocked}},
		{lifecycleRunFailed, workmgmt.CanonicalStateBlocked, []string{workmgmt.CanonicalStateInProgress, workmgmt.CanonicalStateInReview}},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			fp := &fakeTransitionProvider{}
			registerTransitionProvider(t, fp)
			rn := issueRun("issue:1012")
			s, _, au := boardSyncServer(t, rn)

			s.notifyBoardTransition(context.Background(), rn.ID, tc.event)

			if len(fp.calls) != 1 {
				t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
			}
			got := fp.calls[0]
			if got.CanonicalState != tc.wantCanonical {
				t.Errorf("canonical state = %q, want %q", got.CanonicalState, tc.wantCanonical)
			}
			if got.IssueNumber != 1012 {
				t.Errorf("issue number = %d, want 1012", got.IssueNumber)
			}
			if got.Trigger != tc.event {
				t.Errorf("trigger = %q, want %q", got.Trigger, tc.event)
			}
			if got.Target.Scope != forge.FromGitHubInstallationID(99) {
				t.Errorf("scope = %q, want scope for installation 99", got.Target.Scope.Ref())
			}
			if !sameSet(got.ExpectedSourceStates, tc.wantExpectSrc) {
				t.Errorf("expected sources = %v, want %v", got.ExpectedSourceStates, tc.wantExpectSrc)
			}
			// The states map must be carried so the provider resolves options.
			if got.States[workmgmt.CanonicalStateInProgress] != "In Progress" {
				t.Errorf("states map not carried: %v", got.States)
			}

			audits := transitionAudits(au)
			if len(audits) != 1 {
				t.Fatalf("work_item_transitioned audits = %d, want 1", len(audits))
			}
			if audits[0]["trigger"] != tc.event || audits[0]["canonical_state"] != tc.wantCanonical {
				t.Errorf("audit payload = %v, want trigger=%q canonical=%q", audits[0], tc.event, tc.wantCanonical)
			}
			if audits[0]["moved"] != true {
				t.Errorf("audit moved = %v, want true", audits[0]["moved"])
			}
		})
	}
}

// TestExpectedSourceStates_RunMerged_IncludesBlocked pins the #1815 fix at the
// derivation layer: run_merged's expected-source set must include Blocked (the
// run_failed target in the Default conventions) so a Blocked-parked card the
// prior run_failed edge left behind is an expected source for the Done move —
// alongside the InReview / InProgress sources it already carried. The negative
// assertion pins never-fight-the-human unchanged for genuinely-parked columns:
// pr_opened's expected-source set must NOT gain Blocked, so no OTHER lifecycle
// edge can advance a Blocked card.
func TestExpectedSourceStates_RunMerged_IncludesBlocked(t *testing.T) {
	conv := workmgmt.Default()

	merged := expectedSourceStates(lifecycleRunMerged, conv)
	for _, want := range []string{
		workmgmt.CanonicalStateBlocked,
		workmgmt.CanonicalStateInReview,
		workmgmt.CanonicalStateInProgress,
	} {
		if !containsState(merged, want) {
			t.Errorf("expectedSourceStates(run_merged) = %v, want to contain %q", merged, want)
		}
	}

	opened := expectedSourceStates(lifecyclePROpened, conv)
	if containsState(opened, workmgmt.CanonicalStateBlocked) {
		t.Errorf("expectedSourceStates(pr_opened) = %v, must NOT contain %q (never-fight-the-human)", opened, workmgmt.CanonicalStateBlocked)
	}
}

// containsState reports whether states contains want.
func containsState(states []string, want string) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// TestExpectedSourceStates_CampaignStarted pins the #1816 expected-source sets:
// campaign_started accepts the backlog/unset entry state ONLY (a card a human
// already advanced past Backlog is left untouched), while run_started ALSO
// accepts up_next so a campaign-queued Up Next card advances to In Progress when
// its run starts.
func TestExpectedSourceStates_CampaignStarted(t *testing.T) {
	conv := workmgmt.Default()

	campaignStarted := expectedSourceStates(lifecycleCampaignStarted, conv)
	if !sameSet(campaignStarted, []string{workmgmt.CanonicalStateBacklog}) {
		t.Errorf("expectedSourceStates(campaign_started) = %v, want [%q] only", campaignStarted, workmgmt.CanonicalStateBacklog)
	}

	runStarted := expectedSourceStates(lifecycleRunStarted, conv)
	if !sameSet(runStarted, []string{workmgmt.CanonicalStateBacklog, workmgmt.CanonicalStateUpNext}) {
		t.Errorf("expectedSourceStates(run_started) = %v, want {backlog, up_next}", runStarted)
	}
}

// campaignBoardServer wires a Server with a global-chain audit recorder and a
// GitHub stub serving the installation endpoint, so the campaign-scoped board
// hook resolves an installation and audits on the global chain.
func campaignBoardServer(t *testing.T) (*Server, *campaignAuditRecorder) {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	au := &campaignAuditRecorder{}
	gh := recordingInstallGitHubClient(t, 12345, &installRecorder{})
	return New(Config{AuditRepo: au, GitHub: gh}), au
}

// campaignTransitionAudits decodes the global-chain work_item_transitioned
// entries the campaign-scoped hook wrote.
func campaignTransitionAudits(au *campaignAuditRecorder) []map[string]any {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []map[string]any
	for _, e := range au.entries {
		if e.Category != categoryWorkItemTransitioned {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(e.Payload, &m)
		out = append(out, m)
	}
	return out
}

// TestBoardTransitionForCampaignItem_MovesBacklogToUpNext drives the campaign
// entry point moving a backlog/unset card to Up Next: the fake Transitioner is
// dispatched campaign_started with the up_next canonical state + a backlog-only
// expected-source set, and the move is audited on the GLOBAL chain carrying the
// campaign id + issue number.
func TestBoardTransitionForCampaignItem_MovesBacklogToUpNext(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{Moved: true, From: "Backlog", To: "Up Next"}}
	registerTransitionProvider(t, fp)
	s, au := campaignBoardServer(t)
	c := &campaign.Campaign{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk"}

	s.boardTransitionForCampaignItem(context.Background(), c, 1816, lifecycleCampaignStarted)

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	got := fp.calls[0]
	if got.CanonicalState != workmgmt.CanonicalStateUpNext {
		t.Errorf("canonical state = %q, want %q", got.CanonicalState, workmgmt.CanonicalStateUpNext)
	}
	if got.Trigger != lifecycleCampaignStarted {
		t.Errorf("trigger = %q, want %q", got.Trigger, lifecycleCampaignStarted)
	}
	if got.IssueNumber != 1816 {
		t.Errorf("issue number = %d, want 1816", got.IssueNumber)
	}
	if got.Target.Scope != forge.FromGitHubInstallationID(12345) {
		t.Errorf("scope = %q, want scope for installation 12345 (resolved from repo)", got.Target.Scope.Ref())
	}
	if !sameSet(got.ExpectedSourceStates, []string{workmgmt.CanonicalStateBacklog}) {
		t.Errorf("expected sources = %v, want [backlog] only", got.ExpectedSourceStates)
	}
	if got.States[workmgmt.CanonicalStateUpNext] != "Up Next" {
		t.Errorf("states map not carried: %v", got.States)
	}

	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("work_item_transitioned audits = %d, want 1", len(audits))
	}
	a := audits[0]
	if a["moved"] != true || a["trigger"] != lifecycleCampaignStarted || a["canonical_state"] != workmgmt.CanonicalStateUpNext {
		t.Errorf("audit = %v, want moved=true trigger=campaign_started canonical=up_next", a)
	}
	if a["campaign_id"] != c.ID.String() {
		t.Errorf("audit campaign_id = %v, want %q", a["campaign_id"], c.ID.String())
	}
	// issue_number decodes as a float64 through the map[string]any round-trip.
	if a["issue_number"] != float64(1816) {
		t.Errorf("audit issue_number = %v, want 1816", a["issue_number"])
	}
}

// TestBoardTransitionForCampaignItem_SkipNeverFightHuman asserts a card a human
// already advanced (In Progress) is left untouched — the provider returns a
// Skipped result — and the skip is STILL audited on the global chain.
func TestBoardTransitionForCampaignItem_SkipNeverFightHuman(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{
		Skipped: true, From: "In Progress", To: "Up Next",
		SkipReason: `current status "In Progress" is not in the expected source set`,
	}}
	registerTransitionProvider(t, fp)
	s, au := campaignBoardServer(t)
	c := &campaign.Campaign{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk"}

	s.boardTransitionForCampaignItem(context.Background(), c, 1816, lifecycleCampaignStarted)

	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1 (skips are audited)", len(audits))
	}
	if audits[0]["skipped"] != true || audits[0]["moved"] != false {
		t.Errorf("audit = %v, want skipped=true moved=false", audits[0])
	}
}

// TestBoardTransitionForCampaignItem_TokenAbsentSkip asserts the projects-token-
// absent skip (#1107/#1114) — the provider degrades a user-owned board move to a
// Skipped result rather than an error — is AUDITED, not errored.
func TestBoardTransitionForCampaignItem_TokenAbsentSkip(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{
		Skipped: true, To: "Up Next",
		SkipReason: "user-owned project board unreachable: no projects token configured",
	}}
	registerTransitionProvider(t, fp)
	s, au := campaignBoardServer(t)
	c := &campaign.Campaign{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk"}

	s.boardTransitionForCampaignItem(context.Background(), c, 1816, lifecycleCampaignStarted)

	audits := campaignTransitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1 (token-absent skip is audited, not errored)", len(audits))
	}
	if audits[0]["skipped"] != true {
		t.Errorf("audit = %v, want skipped=true", audits[0])
	}
}

// TestBoardTransitionForCampaignItem_ProviderError_Swallowed asserts a genuine
// provider error is best-effort: the Transition is attempted but no audit is
// written and nothing unwinds.
func TestBoardTransitionForCampaignItem_ProviderError_Swallowed(t *testing.T) {
	fp := &fakeTransitionProvider{transErr: context.DeadlineExceeded}
	registerTransitionProvider(t, fp)
	s, au := campaignBoardServer(t)
	c := &campaign.Campaign{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk"}

	s.boardTransitionForCampaignItem(context.Background(), c, 1816, lifecycleCampaignStarted)

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written despite provider error, want none")
	}
}

// TestBoardTransitionForCampaignItem_NilGitHub_NoOp asserts the nil-GitHub-client
// branch is a logged no-op: no provider Transition call, no audit. (An
// installation cannot be resolved without a client.)
func TestBoardTransitionForCampaignItem_NilGitHub_NoOp(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	au := &campaignAuditRecorder{}
	s := New(Config{AuditRepo: au}) // no GitHub client
	c := &campaign.Campaign{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk"}

	s.boardTransitionForCampaignItem(context.Background(), c, 1816, lifecycleCampaignStarted)

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times with nil GitHub client, want 0", len(fp.calls))
	}
	if len(campaignTransitionAudits(au)) != 0 {
		t.Errorf("audit written with nil GitHub client, want none")
	}
}

// TestNotifyBoardTransition_AuditsSkip asserts a deliberate provider skip (the
// never-fight-the-human case) is still audited (condition (4)).
func TestNotifyBoardTransition_AuditsSkip(t *testing.T) {
	fp := &fakeTransitionProvider{result: &workmgmt.TransitionResult{
		Skipped: true, From: "Blocked", To: "In Progress", SkipReason: "current status \"Blocked\" is not in the expected source set",
	}}
	registerTransitionProvider(t, fp)
	rn := issueRun("issue:1012")
	s, _, au := boardSyncServer(t, rn)

	s.notifyBoardTransition(context.Background(), rn.ID, lifecycleRunStarted)

	audits := transitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("audits = %d, want 1 (skips are audited)", len(audits))
	}
	if audits[0]["skipped"] != true || audits[0]["moved"] != false {
		t.Errorf("audit = %v, want skipped=true moved=false", audits[0])
	}
}

// TestNotifyBoardTransition_NonIssueTrigger_NoOp asserts an ad-hoc/CLI run
// (no issue: trigger ref) produces no provider call and no audit.
func TestNotifyBoardTransition_NonIssueTrigger_NoOp(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	rn := issueRun("manual")
	rn.TriggerRef = nil
	s, _, au := boardSyncServer(t, rn)

	s.notifyBoardTransition(context.Background(), rn.ID, lifecycleRunStarted)

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for non-issue trigger, want 0", len(fp.calls))
	}
	if len(transitionAudits(au)) != 0 {
		t.Errorf("audit written for non-issue trigger, want none")
	}
}

// TestNotifyBoardTransition_UnmappedEvent_NoOp asserts an event with no
// configured transition is a silent no-op.
func TestNotifyBoardTransition_UnmappedEvent_NoOp(t *testing.T) {
	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)
	rn := issueRun("issue:1012")
	s, _, au := boardSyncServer(t, rn)

	s.notifyBoardTransition(context.Background(), rn.ID, "child_pushed")

	if len(fp.calls) != 0 {
		t.Errorf("Transition called for unmapped event, want 0")
	}
	if len(transitionAudits(au)) != 0 {
		t.Errorf("audit written for unmapped event, want none")
	}
}

// TestNotifyBoardTransition_ProviderError_Swallowed asserts a genuine provider
// error is best-effort: no audit, no panic, never unwinds.
func TestNotifyBoardTransition_ProviderError_Swallowed(t *testing.T) {
	fp := &fakeTransitionProvider{transErr: context.DeadlineExceeded}
	registerTransitionProvider(t, fp)
	rn := issueRun("issue:1012")
	s, _, au := boardSyncServer(t, rn)

	s.notifyBoardTransition(context.Background(), rn.ID, lifecycleRunStarted)

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	if len(transitionAudits(au)) != 0 {
		t.Errorf("audit written despite provider error, want none")
	}
}

// TestCreateRun_EmitsRunStartedBoardTransition is the #1123 end-to-end
// guard: a local-runner / API-created issue run posted through the real
// handleCreateRun HTTP handler must move its card to In Progress via the
// run_started board transition, crossing every layer — create handler ->
// boardTransitionForRun hook -> registered Transitioner -> work_item_transitioned
// audit. A per-layer unit would pass while this create-handler->hook seam
// silently no-ops (the exact #1123 failure mode, cf. #618). The webhook
// dispatcher creates runs via its own internal createRun and never reaches
// handleCreateRun, so the e2e asserts EXACTLY ONE run_started audit — no
// double-fire.
func TestCreateRun_EmitsRunStartedBoardTransition(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)

	rr := newFakeRepo()
	au := newAuditFake()
	s := New(Config{RunRepo: rr, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"repo":           "kuhlman-labs/fishhawk",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"trigger_ref":    "issue:932",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	if len(fp.calls) != 1 {
		t.Fatalf("Transition calls = %d, want 1", len(fp.calls))
	}
	got := fp.calls[0]
	if got.Trigger != lifecycleRunStarted {
		t.Errorf("trigger = %q, want %q", got.Trigger, lifecycleRunStarted)
	}
	if got.CanonicalState != workmgmt.CanonicalStateInProgress {
		t.Errorf("canonical state = %q, want %q", got.CanonicalState, workmgmt.CanonicalStateInProgress)
	}
	if got.IssueNumber != 932 {
		t.Errorf("issue number = %d, want 932", got.IssueNumber)
	}
	containsBacklog := false
	for _, src := range got.ExpectedSourceStates {
		if src == workmgmt.CanonicalStateBacklog {
			containsBacklog = true
		}
	}
	if !containsBacklog {
		t.Errorf("expected sources = %v, want to contain %q", got.ExpectedSourceStates, workmgmt.CanonicalStateBacklog)
	}

	audits := transitionAudits(au)
	if len(audits) != 1 {
		t.Fatalf("work_item_transitioned audits = %d, want exactly 1", len(audits))
	}
	if audits[0]["trigger"] != lifecycleRunStarted || audits[0]["moved"] != true {
		t.Errorf("audit payload = %v, want trigger=%q moved=true", audits[0], lifecycleRunStarted)
	}
}

// TestCreateRun_NonIssueRun_NoBoardTransition pins the internal no-op that
// keeps the unconditional handleCreateRun emit safe for ad-hoc runs (#1123,
// condition (3)): a run with no issue: trigger ref produces NO provider
// Transition call and NO work_item_transitioned audit.
func TestCreateRun_NonIssueRun_NoBoardTransition(t *testing.T) {
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return workmgmt.Default(), nil }
	t.Cleanup(func() { conventionsLoader = prev })

	fp := &fakeTransitionProvider{}
	registerTransitionProvider(t, fp)

	rr := newFakeRepo()
	au := newAuditFake()
	s := New(Config{RunRepo: rr, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"repo":           "kuhlman-labs/fishhawk",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	if len(fp.calls) != 0 {
		t.Errorf("Transition called %d times for non-issue run, want 0", len(fp.calls))
	}
	if len(transitionAudits(au)) != 0 {
		t.Errorf("work_item_transitioned audit written for non-issue run, want none")
	}
}

// sameSet reports whether a and b contain the same elements (order-insensitive).
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}
