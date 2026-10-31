package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// issue_approval_channel_test.go pins the ADR-123 D4 (#4755) issue-comment
// approval-channel refusal: the channel (slash command, reply comment,
// forwarded reaction) is REFUSED on every workflow-v2 gate, approve and reject,
// and fails closed wherever the gate's approval rules cannot be verified. Each
// test drives the real Server.HandleApprovalCommand and reads COMMITTED STATE
// (the stage stays awaiting_approval, no approval row) plus the
// approval_comment_refused entry, never just a returned error: the handler
// returns nil on a refusal and on a success alike.

// channelGitHubIssueRun seeds a GitHub issue run (issue:42 on x/y, installation
// 99) with an awaiting-approval plan stage.
func channelGitHubIssueRun(rr *orchestratorRepo) (*run.Run, *run.Stage) {
	r := rr.seedRun()
	r.TriggerSource = run.TriggerGitHubIssue
	triggerRef := "issue:42"
	r.TriggerRef = &triggerRef
	r.InstallationID = ptrInt64(99)
	r.Repo = "x/y"
	r.WorkflowID = "feature_change"
	stage := rr.seedStage(r.ID, 0, run.StageStateAwaitingApproval)
	stage.Type = run.StageTypePlan
	return r, stage
}

// channelServer wires the server like slashApprovalFixture over the supplied
// run repo and resolver (nil leaves Config.RoleResolver unset).
func channelServer(rr run.Repository, base *orchestratorRepo, resolver *role.Resolver) (*Server, *fakeApprovalRepo, *slashGitHubRecorder, *auditCompleteAuditFake) {
	ar := newFakeApprovalRepo()
	au := newAuditCompleteAuditFake()
	gh := newSlashGitHubRecorder()
	cfg := Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ApprovalRepo: ar,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: base},
		ExternalURL:  "https://app.fishhawk.example.com",
	}
	if resolver != nil {
		cfg.RoleResolver = resolver
	}
	s := New(cfg)
	s.issueNotifier = issuecomment.New(issuecomment.Deps{
		GitHub: gh, Runs: base, Audit: au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	return s, ar, gh, au
}

// v2CommentGateSpec renders a PARSEABLE workflow-v2 spec whose plan stage
// carries gateYAML verbatim (a full `gates:` block indented for the stage, or
// "" for a plan stage with NO approval gate — a v2 approval gate must declare
// `approvals`, so the no-approvals shape omits the gate).
func v2CommentGateSpec(gateYAML string) string {
	return `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
` + gateYAML + `      - id: implement
        type: implement
        executor:
          agent: claude-code
`
}

// v2MembersGate is a v2 approval gate admitting only github:alice.
const v2MembersGate = `        gates:
          - type: approval
            approvals:
              count: 1
              members: ["github:alice"]
`

// v2CommentGateFixture seeds a GitHub issue run whose cached spec is the v2
// spec above with gateYAML on the plan stage. The resolver is wired when
// non-nil: the V2Gate tests wire one so the role_resolver_unconfigured refusal
// cannot MASK a deleted v2 refusal (with it wired, a v2 spec that slipped past
// the v2 check falls through to the legacy path, whose no-approvers outcome
// ADMITS — that is the counterfactual those tests discriminate).
func v2CommentGateFixture(t *testing.T, gateYAML string, resolver *role.Resolver) (*Server, *run.Stage, *fakeApprovalRepo, *slashGitHubRecorder, *auditCompleteAuditFake) {
	t.Helper()
	rr := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(rr)
	r.WorkflowSpec = []byte(v2CommentGateSpec(gateYAML))
	s, ar, gh, au := channelServer(rr, rr, resolver)
	return s, stage, ar, gh, au
}

// channelRefusalPayload is the decoded approval_comment_refused payload of a
// channel refusal. SpecMajor is a pointer so an absent key is distinguishable
// from a zero value.
type channelRefusalPayload struct {
	StageID   string `json:"stage_id"`
	Decision  string `json:"decision"`
	Surface   string `json:"surface"`
	Approver  string `json:"approver"`
	Silent    bool   `json:"silent"`
	Reason    string `json:"reason"`
	Cause     string `json:"cause"`
	SpecMajor *int   `json:"spec_major"`
}

// assertChannelRefused asserts the committed state of a channel refusal — the
// stage untouched, no approval row — and exactly one approval_comment_refused
// entry with reason comment_channel_refused, the given cause and silent flag.
// It returns the decoded payload for further assertions.
func assertChannelRefused(t *testing.T, stage *run.Stage, ar *fakeApprovalRepo, au *auditCompleteAuditFake, wantCause string, wantSilent bool) channelRefusalPayload {
	t.Helper()
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (a refused comment must transition nothing)", stage.State)
	}
	if len(ar.all) != 0 {
		t.Errorf("approval rows = %d, want 0 (a refused comment must record nothing): %+v", len(ar.all), ar.all)
	}
	entries := refusalEntries(t, au)
	if len(entries) != 1 {
		t.Fatalf("approval_comment_refused entries = %d, want 1", len(entries))
	}
	var p channelRefusalPayload
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode refusal payload: %v", err)
	}
	if p.Reason != approvalCommentRefusedReasonChannel {
		t.Errorf("reason = %q, want %q", p.Reason, approvalCommentRefusedReasonChannel)
	}
	if p.Cause != wantCause {
		t.Errorf("cause = %q, want %q", p.Cause, wantCause)
	}
	if p.Silent != wantSilent {
		t.Errorf("silent = %v, want %v", p.Silent, wantSilent)
	}
	if p.StageID != stage.ID.String() || entries[0].StageID == nil || *entries[0].StageID != stage.ID {
		t.Errorf("refusal must be anchored on stage %s; payload stage_id = %q, entry stage_id = %v", stage.ID, p.StageID, entries[0].StageID)
	}
	return p
}

// assertSpecMajor asserts the payload's spec_major key is present and equal to
// want.
func assertSpecMajor(t *testing.T, p channelRefusalPayload, want int) {
	t.Helper()
	if p.SpecMajor == nil {
		t.Fatalf("spec_major key absent, want %d", want)
	}
	if *p.SpecMajor != want {
		t.Errorf("spec_major = %d, want %d", *p.SpecMajor, want)
	}
}

// assertNoSpecMajor asserts the payload carries no spec_major key (a
// spec_unreadable refusal omits it).
func assertNoSpecMajor(t *testing.T, p channelRefusalPayload) {
	t.Helper()
	if p.SpecMajor != nil {
		t.Errorf("spec_major = %d, want the key absent on a spec_unreadable refusal", *p.SpecMajor)
	}
}

// oneReply returns the single posted comment body, failing otherwise.
func oneReply(t *testing.T, gh *slashGitHubRecorder) string {
	t.Helper()
	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("posted %d comments, want exactly one reply: %v", len(calls), commentBodies(calls))
	}
	return calls[0].body
}

// assertNamesSupportedChannels asserts a refusal reply names every channel that
// can still decide the gate.
func assertNamesSupportedChannels(t *testing.T, reply string) {
	t.Helper()
	for _, want := range []string{"fishhawk_approve_plan", "fishhawk_reject_plan", "fishhawk plan approve", "fishhawk plan reject", "web UI"} {
		if !strings.Contains(reply, want) {
			t.Errorf("refusal reply should name %q: %q", want, reply)
		}
	}
}

func slashCmd(sender string, decision webhook.MatchAction) webhook.ApprovalCommandParams {
	return webhook.ApprovalCommandParams{
		Repo: "x/y", IssueNumber: 42, InstallationID: 99,
		SenderLogin: sender, Decision: decision, Source: webhook.ApprovalSourceSlash,
	}
}

// TestHandleApprovalCommand_V2Gate_NonMemberApproveRefused: a non-member's
// slash approve on a v2 gate is refused by the channel, not by the predicate.
// Counterfactual (A): with the v2 refusal disabled the v2 spec falls through
// to the legacy path, whose no-approvers outcome admits, and the committed-state
// assertions go RED.
func TestHandleApprovalCommand_V2Gate_NonMemberApproveRefused(t *testing.T) {
	s, stage, ar, gh, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
	if p.Decision != string(approval.DecisionApprove) || p.Approver != "bob" || p.Surface != string(approval.SurfaceGitHubComment) {
		t.Errorf("payload = %+v, want an approve by bob on the github_comment surface", p)
	}
	reply := oneReply(t, gh)
	if !strings.Contains(reply, "not accepted on workflow-v2 gates") || !strings.Contains(reply, "ADR-123") {
		t.Errorf("reply should say comment decisions are not accepted on v2 gates (ADR-123): %q", reply)
	}
	if !strings.Contains(reply, "still awaiting approval") || !strings.Contains(reply, "(Caller: @bob)") {
		t.Errorf("reply should say nothing was recorded and name the caller: %q", reply)
	}
	assertNamesSupportedChannels(t, reply)
}

// TestHandleApprovalCommand_V2Gate_NonMemberRejectRefused: a reject is refused
// too — a comment reject on a v2 gate does not fail the stage.
func TestHandleApprovalCommand_V2Gate_NonMemberRejectRefused(t *testing.T) {
	s, stage, ar, gh, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	cmd := slashCmd("bob", webhook.MatchActionReject)
	cmd.Comment = "scope is too wide"
	if err := s.HandleApprovalCommand(context.Background(), cmd); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	if stage.State == run.StageStateFailed {
		t.Fatalf("stage failed under a refused comment reject")
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
	if p.Decision != string(approval.DecisionReject) {
		t.Errorf("decision = %q, want reject", p.Decision)
	}
	assertNamesSupportedChannels(t, oneReply(t, gh))
}

// TestHandleApprovalCommand_V2Gate_ListedMemberRefused: membership is not the
// question — a LISTED member's comment approve is refused as well.
func TestHandleApprovalCommand_V2Gate_ListedMemberRefused(t *testing.T) {
	s, stage, ar, gh, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
	if !strings.Contains(oneReply(t, gh), "(Caller: @alice)") {
		t.Errorf("reply should name the caller alice")
	}
}

// TestHandleApprovalCommand_V2Gate_NoApprovalsBlockRefused: a v2 plan stage
// with NO approval gate is refused too. fetchGateForStage would return the
// no-approvers context for it, so under counterfactual (A) the legacy path
// ADMITS and this test goes RED.
func TestHandleApprovalCommand_V2Gate_NoApprovalsBlockRefused(t *testing.T) {
	s, stage, ar, _, au := v2CommentGateFixture(t, "", legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
}

// TestHandleApprovalCommand_V2Gate_ReplyCommentRefusedSilently: the reply-comment
// ("+1") channel keeps its silent suppression — no comment is posted — but the
// refusal is still audited, with silent=true.
func TestHandleApprovalCommand_V2Gate_ReplyCommentRefusedSilently(t *testing.T) {
	s, stage, ar, gh, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	cmd := slashCmd("alice", webhook.MatchActionApprove)
	cmd.Source = webhook.ApprovalSourceReplyComment
	if err := s.HandleApprovalCommand(context.Background(), cmd); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, true)
	assertSpecMajor(t, p, 2)
	if p.Surface != string(approval.SurfaceGitHubReplyComment) {
		t.Errorf("surface = %q, want %q", p.Surface, approval.SurfaceGitHubReplyComment)
	}
	if calls := gh.calls(); len(calls) != 0 {
		t.Errorf("posted %d comments, want 0 on the silent reply-comment channel: %v", len(calls), commentBodies(calls))
	}
}

// TestHandleApprovalCommand_V2Gate_NilResolverStillRefused: with no resolver the
// v2 refusal still names workflow_v2_gate — the version check precedes the
// resolver check.
func TestHandleApprovalCommand_V2Gate_NilResolverStillRefused(t *testing.T) {
	s, stage, ar, _, au := v2CommentGateFixture(t, v2MembersGate, nil)
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
}

// TestHandleApprovalCommand_V2Gate_RefusalReadableViaAuditEndpoint reads the
// refusal back through GET /v0/runs/{id}/audit?category=approval_comment_refused
// (handler -> audit chain -> HTTP read surface).
func TestHandleApprovalCommand_V2Gate_RefusalReadableViaAuditEndpoint(t *testing.T) {
	s, stage, _, _, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	if n := len(refusalEntries(t, au)); n != 1 {
		t.Fatalf("approval_comment_refused entries = %d, want 1", n)
	}

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v0/runs/%s/audit?category=%s", stage.RunID, CategoryApprovalCommentRefused), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Items []auditEntryResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("filtered read returned %d items, want 1:\n%s", len(got.Items), w.Body.String())
	}
	if got.Items[0].Category != CategoryApprovalCommentRefused {
		t.Errorf("category = %q, want %q", got.Items[0].Category, CategoryApprovalCommentRefused)
	}
	var p channelRefusalPayload
	if err := json.Unmarshal(got.Items[0].Payload, &p); err != nil {
		t.Fatalf("decode item payload: %v", err)
	}
	if p.Reason != approvalCommentRefusedReasonChannel || p.Cause != commentRefusalWorkflowV2Gate {
		t.Errorf("read-back payload reason/cause = %q/%q, want %q/%q", p.Reason, p.Cause, approvalCommentRefusedReasonChannel, commentRefusalWorkflowV2Gate)
	}
}

// TestHandleApprovalCommand_V2Gate_DispatcherE2E drives a GitHub issue_comment
// `/fishhawk approve` event through the REAL webhook.Dispatcher (classifier ->
// handleApprovalCommand) into the real Server, and asserts the same refusal.
func TestHandleApprovalCommand_V2Gate_DispatcherE2E(t *testing.T) {
	s, stage, ar, gh, au := v2CommentGateFixture(t, v2MembersGate, legacyCommentResolver())
	d := &webhook.Dispatcher{ApprovalHandler: s}
	ev := webhook.Event{
		Type:           "issue_comment",
		Action:         "created",
		Repo:           "x/y",
		Sender:         "bob",
		SenderType:     "User",
		InstallationID: 99,
		RawBody:        []byte(`{"comment":{"body":"/fishhawk approve"},"issue":{"number":42}}`),
	}
	if err := d.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Dispatcher.Handle: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalWorkflowV2Gate, false)
	assertSpecMajor(t, p, 2)
	if p.Approver != "bob" || p.Surface != string(approval.SurfaceGitHubComment) {
		t.Errorf("payload = %+v, want bob on the github_comment surface", p)
	}
	assertNamesSupportedChannels(t, oneReply(t, gh))
}

// TestHandleApprovalCommand_SpecUnreadableRefused: a run whose cached spec is
// absent or does not parse refuses (cause spec_unreadable, no spec_major key)
// with a resolver wired. Counterfactual (B): restoring the fail-open admit on
// the first spec_unreadable return admits both subtests' approve.
func TestHandleApprovalCommand_SpecUnreadableRefused(t *testing.T) {
	cases := []struct {
		name string
		spec []byte
	}{
		{"empty cached spec", nil},
		{"unparseable cached spec", []byte("version: \"2\"\nworkflows: [")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			r, stage := channelGitHubIssueRun(rr)
			r.WorkflowSpec = tc.spec
			s, ar, gh, au := channelServer(rr, rr, legacyCommentResolver())
			if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
				t.Fatalf("HandleApprovalCommand: %v", err)
			}
			p := assertChannelRefused(t, stage, ar, au, commentRefusalSpecUnreadable, false)
			assertNoSpecMajor(t, p)
			reply := oneReply(t, gh)
			if !strings.Contains(reply, "could not verify this gate's approval rules") || !strings.Contains(reply, "`spec_unreadable`") {
				t.Errorf("reply should say the rules could not be verified and name the cause: %q", reply)
			}
			assertNamesSupportedChannels(t, reply)
		})
	}
}

// getRunFailingRepo lets stage discovery (ListRuns / ListStagesForRun) succeed
// and fails GetRun, the read commentGateSpecMajor re-does.
type getRunFailingRepo struct {
	*orchestratorRepo
}

func (r *getRunFailingRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("get run boom")
}

// TestHandleApprovalCommand_GetRunErrorRefused: stage discovery succeeds, then
// the run re-read fails — the channel refuses (spec_unreadable) rather than
// admitting.
func TestHandleApprovalCommand_GetRunErrorRefused(t *testing.T) {
	base := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(base)
	legacyCommentGate(r)
	s, ar, _, au := channelServer(&getRunFailingRepo{orchestratorRepo: base}, base, legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalSpecUnreadable, false)
	assertNoSpecMajor(t, p)
}

// TestHandleApprovalCommand_NilRoleResolverRefused: a PARSEABLE legacy v0 gate
// with no RoleResolver refuses (role_resolver_unconfigured), since the legacy
// approvers check cannot run. Counterfactual (C): admitting there approves.
func TestHandleApprovalCommand_NilRoleResolverRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(rr)
	legacyCommentGate(r)
	s, ar, gh, au := channelServer(rr, rr, nil)
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalResolverUnconfigured, false)
	assertSpecMajor(t, p, 0)
	if !strings.Contains(oneReply(t, gh), "`role_resolver_unconfigured`") {
		t.Errorf("reply should name the role_resolver_unconfigured cause")
	}
}

// TestHandleApprovalCommand_LegacyGateUnresolvableRefused: a v0 spec that
// parses but does not carry the run's workflow id refuses (spec_unreadable, no
// spec_major key). Counterfactual (E): admitting there approves.
func TestHandleApprovalCommand_LegacyGateUnresolvableRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(rr)
	legacyCommentGate(r)
	r.WorkflowID = "missing"
	s, ar, _, au := channelServer(rr, rr, legacyCommentResolver())
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalSpecUnreadable, false)
	assertNoSpecMajor(t, p)
}

// TestGitLabApproval_LegacyGateNoInstallationIDRefused: a v0 GitLab run carries
// no installation_id, so the legacy gate cannot be resolved and the comment
// approval now REFUSES (spec_unreadable) instead of the prior best-effort allow
// (accepted by design, ADR-123 D4 / #4755). Driven through the real dispatcher
// from a GitLab note.
func TestGitLabApproval_LegacyGateNoInstallationIDRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(rr)
	legacyCommentGate(r)
	ref := "gitlab:5"
	r.InstallationRef = &ref
	r.InstallationID = nil
	r.RunnerKind = run.RunnerKindGitLabCI
	r.Repo = "gitlab-org/gitlab-test"
	s, ar, gh, au := channelServer(rr, rr, legacyCommentResolver())

	d := &webhook.Dispatcher{ApprovalHandler: s}
	if err := d.Handle(context.Background(), gitLabNoteEvent("gitlab-org/gitlab-test", "gitlab:5", "root", "/fishhawk approve", 42)); err != nil {
		t.Fatalf("Dispatcher.Handle: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalSpecUnreadable, false)
	assertNoSpecMajor(t, p)
	if p.Approver != "gitlab:root" {
		t.Errorf("approver = %q, want the namespaced gitlab:root", p.Approver)
	}
	calls := gh.calls()
	if len(calls) != 1 {
		t.Fatalf("posted %d comments, want the refusal reply: %v", len(calls), commentBodies(calls))
	}
	if got, want := calls[0].scope, forge.FromRef("gitlab:5"); got != want {
		t.Errorf("reply scope = %+v, want FromRef(gitlab:5) %+v", got, want)
	}
}

// erroringTeamLister fails every team lookup, so role resolution errors.
type erroringTeamLister struct{}

func (erroringTeamLister) ListTeamMembers(context.Context, forge.CredentialScope, string, string) ([]role.TeamMember, error) {
	return nil, errors.New("team api boom")
}

// legacyApproversSpec renders a PARSEABLE legacy spec at version whose plan
// stage carries a real `approvers: {any_of: [tech_lead]}` gate over the
// @org/tech-leads team.
func legacyApproversSpec(version string) string {
	return `version: "` + version + `"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
`
}

// TestHandleApprovalCommand_LegacyRoleResolutionErrorRefused: the legacy
// approvers check errors (team API failure) and the channel refuses
// (role_resolution_failed) rather than admitting. Counterfactual (D): admitting
// there approves.
func TestHandleApprovalCommand_LegacyRoleResolutionErrorRefused(t *testing.T) {
	rr := newOrchestratorRepo()
	r, stage := channelGitHubIssueRun(rr)
	r.WorkflowSpec = []byte(legacyApproversSpec("0.3"))
	s, ar, _, au := channelServer(rr, rr, role.NewResolver(erroringTeamLister{}))
	if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
		t.Fatalf("HandleApprovalCommand: %v", err)
	}
	p := assertChannelRefused(t, stage, ar, au, commentRefusalRoleResolutionFailed, false)
	assertSpecMajor(t, p, 0)
}

// TestHandleApprovalCommand_LegacyGateBehaviourPinned pins the v0 AND v1 legacy
// outcomes ADR-123 D4 keeps: a listed team member is approved, a non-member's
// slash approve gets the existing deny reply with no row and NO refusal entry,
// and a stage with no approval gate is approved.
func TestHandleApprovalCommand_LegacyGateBehaviourPinned(t *testing.T) {
	members := map[string][]role.TeamMember{"org/tech-leads": {{Login: "alice"}}}
	for _, version := range []string{"0.3", "1.0"} {
		t.Run("v"+version+"/listed member approved", func(t *testing.T) {
			rr := newOrchestratorRepo()
			r, stage := channelGitHubIssueRun(rr)
			r.WorkflowSpec = []byte(legacyApproversSpec(version))
			s, ar, _, au := channelServer(rr, rr, role.NewResolver(&stubTeamLister{teamMembers: members}))
			if err := s.HandleApprovalCommand(context.Background(), slashCmd("alice", webhook.MatchActionApprove)); err != nil {
				t.Fatalf("HandleApprovalCommand: %v", err)
			}
			if len(ar.all) != 1 || ar.all[0].ApproverSubject != "alice" {
				t.Fatalf("approval rows = %+v, want one row for alice", ar.all)
			}
			if stage.State != run.StageStateSucceeded {
				t.Errorf("stage state = %q, want succeeded", stage.State)
			}
			if n := len(refusalEntries(t, au)); n != 0 {
				t.Errorf("approval_comment_refused entries = %d, want 0", n)
			}
		})
		t.Run("v"+version+"/non-member denied unaudited", func(t *testing.T) {
			rr := newOrchestratorRepo()
			r, stage := channelGitHubIssueRun(rr)
			r.WorkflowSpec = []byte(legacyApproversSpec(version))
			s, ar, gh, au := channelServer(rr, rr, role.NewResolver(&stubTeamLister{teamMembers: members}))
			if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
				t.Fatalf("HandleApprovalCommand: %v", err)
			}
			if len(ar.all) != 0 {
				t.Errorf("approval rows = %d, want 0 for a non-member", len(ar.all))
			}
			if stage.State != run.StageStateAwaitingApproval {
				t.Errorf("stage state = %q, want awaiting_approval", stage.State)
			}
			if reply := oneReply(t, gh); !strings.Contains(reply, "not in this stage's approvers list") {
				t.Errorf("reply = %q, want the existing approvers-list deny", reply)
			}
			if n := len(refusalEntries(t, au)); n != 0 {
				t.Errorf("approval_comment_refused entries = %d, want 0 — the legacy deny is not a channel refusal", n)
			}
		})
		t.Run("v"+version+"/no approval gate approved", func(t *testing.T) {
			rr := newOrchestratorRepo()
			r, stage := channelGitHubIssueRun(rr)
			r.WorkflowSpec = []byte(strings.Replace(legacyCommentGateSpec, `version: "0.3"`, `version: "`+version+`"`, 1))
			s, ar, _, au := channelServer(rr, rr, legacyCommentResolver())
			if err := s.HandleApprovalCommand(context.Background(), slashCmd("bob", webhook.MatchActionApprove)); err != nil {
				t.Fatalf("HandleApprovalCommand: %v", err)
			}
			if len(ar.all) != 1 {
				t.Fatalf("approval rows = %d, want 1 on a stage with no approval gate", len(ar.all))
			}
			if stage.State != run.StageStateSucceeded {
				t.Errorf("stage state = %q, want succeeded", stage.State)
			}
			if n := len(refusalEntries(t, au)); n != 0 {
				t.Errorf("approval_comment_refused entries = %d, want 0", n)
			}
		})
	}
}
