package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- E106.28 / #5213: merge and acceptance guidance on the served surfaces ---
//
// Every assertion reads the text an operator actually receives: tool
// descriptions and output schemas SERVED over an in-memory MCP ListTools,
// handler outputs, next_actions, or the embedded runbook. Anchors are
// lowercased and whitespace-flattened so a copy-edit does not redden a test
// while removing the rule does. The helpers are the fixup_guidance_test.go ones.

// mergeGuidanceBannedTokens are the repository-specific values product text
// must never carry (ADR-110): they live in docs/OPERATING_MODEL.md only.
var mergeGuidanceBannedTokens = []string{"kuhlman-labs", "scripts/", "gen-site-reference", "30 min", "thirty minutes"}

// mergeGuidanceRequire fails once per missing anchor, naming the rule.
func mergeGuidanceRequire(t *testing.T, surface, text string, anchors []struct{ rule, anchor string }) {
	t.Helper()
	for _, a := range anchors {
		if !strings.Contains(text, a.anchor) {
			t.Errorf("%s: %s lacks %q", a.rule, surface, a.anchor)
		}
	}
}

// mergeGuidanceADR110 fails when a NEW paragraph names a repository-specific
// value.
func mergeGuidanceADR110(t *testing.T, label, section string) {
	t.Helper()
	for _, banned := range mergeGuidanceBannedTokens {
		if strings.Contains(section, banned) {
			t.Errorf("ADR-110: new %s guidance names a repository-specific value %q:\n%s", label, banned, section)
		}
	}
}

// mergeGuidanceNoUndeterminedDemand fails when text demands an EMPTY
// undetermined[] before approval (approval condition R1). A healthy pull request
// that is not blocked reports undetermined, so the demand would be circular.
func mergeGuidanceNoUndeterminedDemand(t *testing.T, surface, text string) {
	t.Helper()
	for _, banned := range []string{
		"undetermined[] is empty", "undetermined[] are empty", "undetermined[] must be empty",
		"undetermined[] empty", "blockers[] and undetermined[]", "and undetermined[] are",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("R1: %s demands an empty undetermined[] (%q); a non-empty list is informational:\n%s", surface, banned, text)
		}
	}
	for _, want := range []string{"empty except approval_dismissed", "undetermined[]", "informational"} {
		if !strings.Contains(text, want) {
			t.Errorf("R1: %s lacks %q", surface, want)
		}
	}
}

// TestMergeRun_DescriptionStatesMergeGuidance pins one anchor per folded rule on
// the SERVED fishhawk_merge_run description.
func TestMergeRun_DescriptionStatesMergeGuidance(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_merge_run")

	mergeGuidanceRequire(t, "fishhawk_merge_run description", desc, []struct{ rule, anchor string }{
		{"16", "every call is a real merge attempt"},
		{"16", "placeholder verdict"},
		{"16", "fishhawk_get_gate_view merge_readiness"},
		{"1", "re-read the pr head once acceptance settles"},
		{"1", "scenario commit"},
		{"3", "push, vouch (fishhawk_vouch_commit), wait for checks green at that head, approve, then call this verb"},
		{"2", "pre-approval checklist on fishhawk_get_gate_view"},
		{"9", "bound the wait: set timeout_seconds"},
		{"9", "never watch the checks unbounded"},
		{"15", "read the fishhawk_audit_complete check output on the pull request"},
		{"15", "where the repository's branch protection makes that check gate the merge"},
		{"10", "a pass that settled not_executed"},
		{"10", "re-trigger it with fishhawk_rebase_run_branch, then, on a local runner, dispatch the re-opened stage"},
		{"6", "a fresh consolidated round starts on the new head"},
		{"6", "await it before re-invoking"},
		{"13", "re-invoke as soon as it settles"},
		{"13", "the merge queue re-runs this overlap guard at merge-on-pass"},
		// R4: the new wording must not contradict the refusal's own text.
		{"13", "nothing merged or queued"},
	})

	mergeGuidanceADR110(t, "fishhawk_merge_run", fixupGuidanceSlice(t, desc, "every call is a real merge attempt", "records a chained"))
	mergeGuidanceADR110(t, "fishhawk_merge_run timeout", fixupGuidanceSlice(t, desc, "if the merge stays queued while the pull request is blocked", "\"run_terminal\""))
	mergeGuidanceADR110(t, "fishhawk_merge_run checks_pending", fixupGuidanceSlice(t, desc, "bound the wait: set timeout_seconds", "\"conflicting\""))
	mergeGuidanceADR110(t, "fishhawk_merge_run unverified", fixupGuidanceSlice(t, desc, "a pass that settled not_executed", "\"merge_candidate_held\""))
	mergeGuidanceADR110(t, "fishhawk_merge_run overlap", fixupGuidanceSlice(t, desc, "re-invoke as soon as it settles", "\"overlap_undetermined\""))
}

// TestMergeRun_ChecksPendingNamesBoundedWait drives the handler into the
// checks_pending checkpoint and asserts the bounded-wait sentence is appended
// after the byte-unchanged lead sentence, with no number in it (rule 9).
func TestMergeRun_ChecksPendingNamesBoundedWait(t *testing.T) {
	out := checksPendingOutput(5, time.Now(), nil)
	if out.Status != "checks_pending" {
		t.Fatalf("status = %q, want checks_pending", out.Status)
	}
	msg := strings.ToLower(out.Message)
	for _, want := range []string{
		"github will not queue the merge because the pull request's required checks have not all passed",
		"timeout_seconds",
		"never watch the checks unbounded",
		"read the check status",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("checks_pending message lacks %q:\n%s", want, out.Message)
		}
	}
	for _, banned := range []string{"minute", "30 "} {
		if strings.Contains(msg, banned) {
			t.Errorf("checks_pending message carries a repository-specific bound %q:\n%s", banned, out.Message)
		}
	}
}

// TestMergeRun_TimeoutNamesAuditCompleteRead runs the real handler into the
// timeout arm and asserts the BLOCKED-pull-request read is named, conditional on
// branch protection, and free of the phrase the timeout control forbids (rule
// 15).
func TestMergeRun_TimeoutNamesAuditCompleteRead(t *testing.T) {
	out := runMergeAdvisory(t, mergeAdvisoryFake("", ""))
	for _, want := range []string{
		"BLOCKED",
		"fishhawk_audit_complete check output",
		"fishhawk_get_gate_view merge_readiness",
		"the repository's branch protection makes that check gate the merge",
		"no pr_merged / post_merge_observed entry landed within",
	} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("timeout message lacks %q:\n%s", want, out.Message)
		}
	}
	if strings.Contains(out.Message, "acceptance stage") {
		t.Errorf("the no-advisory timeout message names an acceptance stage (the control forbids it): %q", out.Message)
	}
}

// TestMergeOverlapOutput_ReasonSaysMergeOnceSettled anchors on the NEW content
// only (approval condition R4): re-invoke the moment the stage settles, and the
// merge queue re-runs the overlap guard at merge-on-pass. The server's message
// passes through verbatim, so the refusal's own "nothing recorded, merged or
// queued" text is never contradicted.
func TestMergeOverlapOutput_ReasonSaysMergeOnceSettled(t *testing.T) {
	const serverMsg = "another live run's implement stage scopes a path this pull request changes; nothing recorded, merged or queued"
	ae := &apiError{
		StatusCode: http.StatusConflict,
		Code:       "overlapping_implement_in_flight",
		Message:    serverMsg,
		Details:    map[string]any{"blocking_run_id": uuid.NewString(), "stage_id": uuid.NewString()},
	}
	out := mergeOverlapOutput(ae, "overlapping_implement_in_flight", uuid.New(), time.Now())
	if out.NextAction == nil {
		t.Fatal("no next_action on overlapping_implement_in_flight")
	}
	reason := out.NextAction.Reason
	for _, want := range []string{
		"re-invoke fishhawk_merge_run the moment it does",
		"the merge queue re-runs this overlap guard at merge-on-pass",
		"queued merge",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("overlap next_action reason lacks %q:\n%s", want, reason)
		}
	}
	if out.Message != serverMsg {
		t.Errorf("the server's message must pass through verbatim; got %q", out.Message)
	}
	if out.MergeQueued || out.VerdictRecorded {
		t.Errorf("an overlap refusal records and queues nothing: %+v", out)
	}
}

// TestVouchCommit_DescriptionStatesApprovalAndOrder pins rules 3 and 5 on the
// SERVED fishhawk_vouch_commit description (approval condition R5a included).
func TestVouchCommit_DescriptionStatesApprovalAndOrder(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_vouch_commit")

	mergeGuidanceRequire(t, "fishhawk_vouch_commit description", desc, []struct{ rule, anchor string }{
		{"5", "approval and order"},
		{"5", "restarts and, where the repository's branch protection dismisses stale approvals, the existing pr approval is dismissed"},
		{"5", "approval_dismissed"},
		{"5", "dismissing_cause vouch_commit"},
		{"R5a", "fishhawk_merge_run answers forge_approval_required on an approval-gated run until you re-approve the vouched head"},
		{"3", "push, vouch, wait for checks green at the vouched head, approve, then fishhawk_merge_run"},
		{"R5a", "a vouch also serves a fix-up head advance"},
		{"5", "approval_note"},
	})
	mergeGuidanceADR110(t, "fishhawk_vouch_commit", fixupGuidanceSlice(t, desc, "approval and order", "returns the recorded declaration"))
}

// TestVouchCommit_OutputSchemaCarriesApprovalNote reads the approval_note
// property description off the SERVED output schema.
func TestVouchCommit_OutputSchemaCarriesApprovalNote(t *testing.T) {
	note := fixupGuidanceOutputPropertyDescription(t, fixupGuidanceTools(t), "fishhawk_vouch_commit", "approval_note")
	mergeGuidanceRequire(t, "approval_note output-schema description", note, []struct{ rule, anchor string }{
		{"R5a", "if this pull request is headed for merge"},
		{"5", "re-approve the vouched head"},
		{"3", "push, vouch, checks green, approve, merge"},
		{"R5a", "a vouch also serves a fix-up head advance"},
	})
	mergeGuidanceADR110(t, "approval_note", note)
}

// TestVouchCommit_SuccessCarriesApprovalNote asserts a successful vouch
// carries the populated approval_note: re-approval named, the order named, and
// the wording conditional on a pull request headed for merge (R5a).
func TestVouchCommit_SuccessCarriesApprovalNote(t *testing.T) {
	_, srv := newVouchFakeBackend(t)
	r := newResolver(srv, nil)
	_, out, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: uuid.NewString(), SHA: vouchFullSHA, Reason: "formatter output",
	})
	if err != nil {
		t.Fatalf("vouchCommit: %v", err)
	}
	if out.ApprovalNote == "" {
		t.Fatal("approval_note is empty on a successful vouch")
	}
	note := strings.ToLower(out.ApprovalNote)
	for _, want := range []string{
		"if this pull request is headed for merge",
		"re-approve the vouched head",
		"push, vouch, wait for checks green, approve, merge",
		"forge_approval_required",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("approval_note lacks %q:\n%s", want, out.ApprovalNote)
		}
	}
	mergeGuidanceADR110(t, "approval_note", note)
}

// TestVouchCommit_FailureCarriesNoApprovalNote: a refused vouch recorded
// nothing, so it must not advise re-approving a head that did not move.
func TestVouchCommit_FailureCarriesNoApprovalNote(t *testing.T) {
	fb, srv := newVouchFakeBackend(t)
	fb.vouchStatus = http.StatusNotFound
	fb.vouchErrBody = `{"error":{"code":"run_not_found","message":"no such run"}}`
	r := newResolver(srv, nil)
	_, out, err := r.vouchCommit(context.Background(), nil, VouchCommitInput{
		RunID: uuid.NewString(), SHA: vouchFullSHA, Reason: "formatter output",
	})
	if err == nil {
		t.Fatal("want an error on a 404")
	}
	if out.ApprovalNote != "" {
		t.Errorf("a refused vouch carries approval_note %q", out.ApprovalNote)
	}
}

// TestVouchCommit_ApprovalNoteCrossesMCPBoundary proves approval_note survives
// the served CallTool path (approval condition R2): a real in-process
// server/session pair, the registered tool, and the populated field in the
// structured response.
func TestVouchCommit_ApprovalNoteCrossesMCPBoundary(t *testing.T) {
	ctx := context.Background()
	_, backend := newVouchFakeBackend(t)
	resolver := newResolver(backend, nil)
	srv := buildServer(config{backendURL: backend.URL, apiToken: "tok-test"})
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_vouch_commit",
		Arguments: map[string]any{"run_id": uuid.NewString(), "sha": vouchFullSHA, "reason": "formatter output"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var got struct {
		ApprovalNote string `json:"approval_note"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode structured content: %v (%s)", err, raw)
	}
	if got.ApprovalNote == "" {
		t.Fatalf("approval_note is absent or empty in the served response: %s", raw)
	}
	if got.ApprovalNote != vouchApprovalNote {
		t.Errorf("approval_note = %q, want the vouchApprovalNote constant", got.ApprovalNote)
	}
}

// TestGetGateView_DescriptionStatesPreApprovalChecklist pins rules 2 and 16 on
// the SERVED fishhawk_get_gate_view description, and approval condition R1: the
// checklist must not demand an empty undetermined[].
func TestGetGateView_DescriptionStatesPreApprovalChecklist(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_get_gate_view")

	mergeGuidanceRequire(t, "fishhawk_get_gate_view description", desc, []struct{ rule, anchor string }{
		{"2", "pre-approval checklist."},
		{"2", "merge_candidate_verdict.head_sha is the head every piece of evidence is bound to"},
		{"1", "an acceptance scenario commit moves it"},
		{"2", "review_head_mismatch is absent"},
		{"2", "control_unpinned"},
		{"2", "the acceptance outcome is known"},
		{"3", "approve after any vouch or hardening push, never before it"},
		{"16", "side-effect-free readiness read"},
		{"16", "never call fishhawk_merge_run with a placeholder verdict to probe"},
	})
	section := fixupGuidanceSlice(t, desc, "pre-approval checklist.", "approve after any vouch or hardening push, never before it")
	mergeGuidanceNoUndeterminedDemand(t, "fishhawk_get_gate_view checklist", section)
	mergeGuidanceADR110(t, "fishhawk_get_gate_view", fixupGuidanceSlice(t, desc, "this is the side-effect-free readiness read", "approve after any vouch or hardening push"))
}

// TestRebaseRunBranch_DescriptionStatesParentReviewRound pins rule 6 on the
// SERVED fishhawk_rebase_run_branch description. Only the "await the new round
// before merge" claim is pinned unconditionally: the new-round trigger for a
// parent with NO declared verify command is not confirmed, so the text states it
// only behind the verify-only pass.
func TestRebaseRunBranch_DescriptionStatesParentReviewRound(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_rebase_run_branch")

	mergeGuidanceRequire(t, "fishhawk_rebase_run_branch description", desc, []struct{ rule, anchor string }{
		{"6", "decomposed parent."},
		{"6", "where the workflow declares a verify command"},
		{"6", "a fresh consolidated implement-review round starts on the new head"},
		{"6", "await that round with fishhawk_await_review"},
		{"6", "before fishhawk_merge_run"},
		{"6", "judged a tree the pr no longer carries"},
		{"6", "review_coverage_incomplete"},
	})
	mergeGuidanceADR110(t, "fishhawk_rebase_run_branch", fixupGuidanceSlice(t, desc, "decomposed parent.", "verify-only re-run of a failed head"))
}

// TestReconcileMerge_DescriptionStatesSettledRunIsNotAnError pins rule 4 on the
// SERVED fishhawk_reconcile_merge description, including the conditional (R5b).
func TestReconcileMerge_DescriptionStatesSettledRunIsNotAnError(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_reconcile_merge")

	mergeGuidanceRequire(t, "fishhawk_reconcile_merge description", desc, []struct{ rule, anchor string }{
		{"4", "a run the merge webhook already settled"},
		{"4", "an empty-list 200"},
		{"R5b", "neither is an error when fishhawk_get_run_status shows the run already succeeded"},
		{"R5b", "otherwise follow the server message"},
	})
	mergeGuidanceADR110(t, "fishhawk_reconcile_merge", fixupGuidanceSlice(t, desc, "a run the merge webhook already settled", "reconcile_merge_stage_live"))
}

// TestReconcileMerge_NotApplicableWrappedWithSettledHint covers both arms of
// the code-specific wrap (rule 4 / R5b): a 409 reconcile_merge_not_applicable
// keeps the server message AND gains the conditional settled-run hint; a 409
// reconcile_merge_pr_not_merged is left UNWRAPPED.
func TestReconcileMerge_NotApplicableWrappedWithSettledHint(t *testing.T) {
	t.Run("not_applicable is wrapped", func(t *testing.T) {
		r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONErr(w, http.StatusConflict, "reconcile_merge_not_applicable", "nothing here for this verb to settle")
		})
		_, _, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: uuid.NewString()})
		if err == nil {
			t.Fatal("want an error on reconcile_merge_not_applicable")
		}
		for _, want := range []string{
			"nothing here for this verb to settle",
			"reconcile_merge_not_applicable",
			"fishhawk_get_run_status",
			"already succeeded",
			"this is not an error",
			"otherwise follow the server message",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "reconcile_merge_not_applicable" {
			t.Errorf("the wrapped error no longer carries the *apiError: %v", err)
		}
	})
	t.Run("pr_not_merged is not wrapped", func(t *testing.T) {
		r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSONErr(w, http.StatusConflict, "reconcile_merge_pr_not_merged", "the pull request is not merged")
		})
		_, _, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: uuid.NewString()})
		if err == nil {
			t.Fatal("want an error on reconcile_merge_pr_not_merged")
		}
		if !strings.Contains(err.Error(), "the pull request is not merged") {
			t.Errorf("the server message was not preserved: %v", err)
		}
		if strings.Contains(err.Error(), reconcileMergeNotApplicableHint) || strings.Contains(err.Error(), "already succeeded") {
			t.Errorf("a refusal other than reconcile_merge_not_applicable must not carry the settled-run hint: %v", err)
		}
	})
}

// TestDriveRun_DescriptionStatesSettledAcceptanceNotRedispatched pins rule 11.
func TestDriveRun_DescriptionStatesSettledAcceptanceNotRedispatched(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_drive_run")

	mergeGuidanceRequire(t, "fishhawk_drive_run description", desc, []struct{ rule, anchor string }{
		{"11", "never re-dispatches a stage that already settled"},
		{"11", "a succeeded acceptance stage is not re-run by re-invoking the drive"},
		{"11", "re-open acceptance through the verb its next_actions name"},
		{"11", "acceptance_stale next_step"},
	})
	mergeGuidanceADR110(t, "fishhawk_drive_run", fixupGuidanceSlice(t, desc, "the loop never re-dispatches a stage", "then re-invoke the drive."))
}

// TestArbitrateAcceptance_DescriptionNamesRetireScenarioAtApproval pins rule 14.
func TestArbitrateAcceptance_DescriptionNamesRetireScenarioAtApproval(t *testing.T) {
	desc := fixupGuidanceDescription(t, fixupGuidanceTools(t), "fishhawk_arbitrate_acceptance")

	mergeGuidanceRequire(t, "fishhawk_arbitrate_acceptance description", desc, []struct{ rule, anchor string }{
		{"14", "the cheaper lever is retiring the stale scenario at a plan approval"},
		{"14", "fishhawk_approve_plan amend_acceptance_criteria action retire_scenario"},
		{"14", "a hand edit of retired.yaml is an operator pull request on the default branch that can need an admin merge"},
	})
	mergeGuidanceADR110(t, "fishhawk_arbitrate_acceptance", fixupGuidanceSlice(t, desc, "the cheaper lever", "for a genuine"))
}

// TestMergeRitualActions_ApprovePRCarriesPreApprovalChecklist covers the
// acceptance_passed arm and the plain succeeded_pr_open arm, through the real
// classifier, and the rule-16 mergeRunAction precondition (R5d). It also pins
// approval condition R1 on the approve_pr text and condition R5c: the merge
// action still names both merge_gate and fishhawk_audit_complete.
func TestMergeRitualActions_ApprovePRCarriesPreApprovalChecklist(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	withPR := func(r *Run) *Run { r.PullRequestURL = &prURL; return r }

	cases := []struct {
		name      string
		run       *Run
		stages    []Stage
		verdict   string
		wantState string
	}{
		{"acceptance_passed", withPR(naLocalRun("running")), naAcceptanceStages("succeeded"), "passed", "acceptance_passed"},
		{"succeeded_pr_open", withPR(naRun("succeeded")), []Stage{naStage("plan", "succeeded"), naStage("implement", "succeeded")}, "", "succeeded_pr_open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			na := nextActionsFor(tc.run, tc.stages, nil, naReviewStatus("implement", "complete"), nil, nil, false, false, false, tc.verdict, "", releaseSignals{})
			if na == nil || na.State != tc.wantState {
				t.Fatalf("state = %+v, want %s", na, tc.wantState)
			}
			var approve, merge *SuggestedAction
			for i := range na.Actions {
				switch na.Actions[i].Action {
				case "approve_pr":
					approve = &na.Actions[i]
				case "fishhawk_merge_run":
					merge = &na.Actions[i]
				}
			}
			if approve == nil || merge == nil {
				t.Fatalf("merge ritual lacks approve_pr or fishhawk_merge_run: %+v", na.Actions)
			}
			pre := fixupGuidanceFlatten(approve.Precondition)
			mergeGuidanceRequire(t, "approve_pr precondition", pre, []struct{ rule, anchor string }{
				{"1", "merge_candidate_verdict.head_sha"},
				{"1", "re-read the pr head after acceptance settles"},
				{"1", "an acceptance scenario commit moves it"},
				{"9", "bound the wait with a timeout, never watch unbounded"},
				{"2", "no open blocking concern"},
				{"2", "declared counterfactuals were observed red"},
				{"2", "no control_unpinned concern is open"},
				{"2", "the acceptance outcome is named in your merge verdict"},
			})
			mergeGuidanceNoUndeterminedDemand(t, "approve_pr precondition", pre)
			mergeGuidanceRequire(t, "approve_pr reason", fixupGuidanceFlatten(approve.Reason), []struct{ rule, anchor string }{
				{"3", "approve after any vouch or hardening push, never before it"},
			})
			mergeGuidanceADR110(t, "approve_pr", pre+" "+fixupGuidanceFlatten(approve.Reason))

			// Rule 16 (R5d) and R5c on the merge action.
			mp := fixupGuidanceFlatten(merge.Precondition)
			mergeGuidanceRequire(t, "fishhawk_merge_run precondition", mp, []struct{ rule, anchor string }{
				{"16", "every call is a real merge attempt"},
				{"16", "use fishhawk_get_gate_view merge_readiness to check readiness without side effects"},
				{"R5c", "merge_gate"},
				{"R5c", "fishhawk_audit_complete"},
			})
		})
	}
}

// TestAcceptancePendingNextActions_NamesAllSkipExpectation asserts the
// acceptance pending dispatch reason names the all-skip expectation (rule 12).
// The fixture is runner_kind local: dispatchOrPollActions returns only a poll
// action for github_actions (approval condition R3), so only local carries a
// dispatch to annotate.
func TestAcceptancePendingNextActions_NamesAllSkipExpectation(t *testing.T) {
	for _, st := range []string{"pending", "awaiting_host_dispatch"} {
		t.Run(st, func(t *testing.T) {
			run := naLocalRun("running")
			na := acceptanceStageNextActions(run, &Stage{ID: uuid.NewString(), Type: "acceptance", State: st}, false, false, "", "")
			if na == nil || na.State != "acceptance_pending" {
				t.Fatalf("state = %+v, want acceptance_pending", na)
			}
			var dispatch *SuggestedAction
			for i := range na.Actions {
				if na.Actions[i].Action == "fishhawk_dispatch_stage" {
					dispatch = &na.Actions[i]
				}
			}
			if dispatch == nil {
				t.Fatalf("no fishhawk_dispatch_stage action on a local acceptance_pending state: %+v", na.Actions)
			}
			reason := fixupGuidanceFlatten(dispatch.Reason)
			mergeGuidanceRequire(t, "acceptance dispatch reason", reason, []struct{ rule, anchor string }{
				{"12", "every criterion is skip_expected with a basis settles not_validated with no runner"},
				{"12", "bind a preview or target only when a criterion drives a runtime http or mcp surface"},
				{"12", "a prompt, script, struct or pr-text observable is expected to all-skip"},
			})
			mergeGuidanceADR110(t, "acceptance dispatch", reason)
			for _, a := range na.Actions {
				if a.Precondition == "" || a.Consumes == "" || a.Reason == "" {
					t.Errorf("action %q has an empty Precondition/Consumes/Reason: %+v", a.Action, a)
				}
			}
		})
	}
}

// TestAcceptancePendingNextActions_GithubActionsPollCarriesNoNote: a
// github_actions run auto-dispatches, so the pending arm is a poll with no
// dispatch action and therefore no all-skip note to attach.
func TestAcceptancePendingNextActions_GithubActionsPollCarriesNoNote(t *testing.T) {
	run := naRun("running")
	run.RunnerKind = "github_actions"
	na := acceptanceStageNextActions(run, &Stage{ID: uuid.NewString(), Type: "acceptance", State: "pending"}, false, false, "", "")
	if na == nil || na.State != "acceptance_pending" {
		t.Fatalf("state = %+v, want acceptance_pending", na)
	}
	for _, a := range na.Actions {
		if a.Action == "fishhawk_dispatch_stage" {
			t.Errorf("a github_actions acceptance_pending state offers a dispatch: %+v", a)
		}
		if strings.Contains(a.Reason, "skip_expected") {
			t.Errorf("a poll action carries the all-skip note: %+v", a)
		}
	}
}

// TestDispatchActions_ImplementAndPlanDoNotCarryAllSkip is the scoping guard
// for rule 12: the all-skip text belongs to the ACCEPTANCE dispatch only, so the
// shared dispatchOrPollActions text for plan and implement stays unchanged. It
// goes RED if the note is moved into the shared function.
func TestDispatchActions_ImplementAndPlanDoNotCarryAllSkip(t *testing.T) {
	run := naLocalRun("running")
	for _, stageType := range []string{"plan", "implement"} {
		for _, a := range dispatchOrPollActions(run, stageType, &Stage{ID: uuid.NewString(), Type: stageType, State: "pending"}) {
			if strings.Contains(a.Reason, "skip_expected") || strings.Contains(a.Precondition, "skip_expected") {
				t.Errorf("%s dispatch action carries the acceptance-only all-skip note: %+v", stageType, a)
			}
		}
	}
}

// TestRunbook_MergeAndAcceptanceSubsection reads the embedded runbook, slices
// the new subsection and asserts the judgement half: the checklist (with the R1
// undetermined exception), the order, the bounded wait, all-skip, the union
// recommendation (R5g), never-probe and the settled-run reconcile note, plus
// the ADR-110 token absence.
func TestRunbook_MergeAndAcceptanceSubsection(t *testing.T) {
	const heading = "### Merge and acceptance: approve, wait and recover"
	start := strings.Index(runbookMarkdown, heading)
	if start < 0 {
		t.Fatalf("runbook.md lacks the %q subsection", heading)
	}
	rest := runbookMarkdown[start+len(heading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		rest = rest[:end]
	}
	section := fixupGuidanceFlatten(strings.NewReplacer("`", "", "**", "").Replace(rest))

	mergeGuidanceRequire(t, "runbook subsection", section, []struct{ rule, anchor string }{
		{"2", "pre-approval checklist."},
		{"1", "merge_candidate_verdict.head_sha is the head every piece of evidence is bound to"},
		{"2", "review_head_mismatch is absent"},
		{"2", "no blocking concern is open"},
		{"2", "counterfactual the plan declared was observed red"},
		{"2", "the acceptance outcome is known"},
		{"3", "push, vouch (fishhawk_vouch_commit), wait for the checks to go green at the vouched head, approve, then fishhawk_merge_run"},
		{"3", "re-approve after any push"},
		{"5", "forge_approval_required"},
		{"1", "re-read the pr head once acceptance settles"},
		{"9", "bound every ci wait."},
		{"9", "never watch checks unbounded"},
		{"15", "fishhawk_audit_complete check output on the pull request"},
		{"12", "a prompt, script, struct or pr-text observable is expected to all-skip"},
		{"12", "bind a preview or target only when a criterion drives a runtime http or mcp surface"},
		{"11", "a settled acceptance stage is not re-run by re-invoking fishhawk_drive_run"},
		{"6", "a rebase on a decomposed parent means waiting for the new round."},
		{"16", "never probe fishhawk_merge_run."},
		{"7", "recommended, not required"},
		{"7", "union of both sides, then re-run the repository's generators"},
		{"4", "reconcile_merge_not_applicable"},
	})
	mergeGuidanceNoUndeterminedDemand(t, "runbook subsection", section)
	mergeGuidanceADR110(t, "runbook", section)

	// Step 6 points at the subsection.
	if !strings.Contains(fixupGuidanceFlatten(runbookMarkdown), "merge and acceptance: approve, wait and recover\" in the edge-case playbook") {
		t.Errorf("happy-path step 6 does not point at the %q subsection", heading)
	}
}
