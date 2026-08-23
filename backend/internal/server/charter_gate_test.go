package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// mustParseWorkflow parses a spec document and returns the named workflow.
func mustParseWorkflow(t *testing.T, doc, name string) spec.Workflow {
	t.Helper()
	s, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	wf, ok := s.Workflows[name]
	if !ok {
		t.Fatalf("workflows = %v, want %q", s.Workflows, name)
	}
	return wf
}

// Handler coverage for the CHARTER admission gate (ADR-065 / E54.4 / #2236).
//
// One test per enumerated branch, not the happy path plus a subset (#1199).
// Three branches share the 422, so every refusal test asserts details.reason as
// well as the status — otherwise deleting one branch would leave the others'
// tests green. The gate's effect also includes COMMITTED STATE (no run row, one
// GLOBAL audit entry), so the refusal tests read the fakes AFTER the call
// returns rather than trusting the error envelope alone.
//
// conventionsLoader is a process-wide package var, so every case here swaps it
// with a t.Cleanup restore and none may call t.Parallel.

// groomingSpecYAML is a minimal workflow that PRODUCES a grooming report. The
// workflow key is deliberately NOT `backlog_grooming`: the gate discriminates
// STRUCTURALLY on the produced artifact, and a name-keyed gate would be evaded
// by exactly this rename.
const groomingSpecYAML = `version: "2"
workflows:
  tidy_the_backlog:
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
      - id: apply
        type: implement
        executor:
          agent: claude-code
`

// nonGroomingSpecYAML is an ordinary code-change workflow: it declares no
// grooming_report, so the gate must never reach the conventions loader for it.
const nonGroomingSpecYAML = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// stubConventions swaps the package-wide loader for the duration of one test.
func stubConventions(t *testing.T, conv workmgmt.Conventions, err error) {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) { return conv, err }
	t.Cleanup(func() { conventionsLoader = prev })
}

// conventionsWithCharter is the shipped default's posture: a charter IS
// declared. Asserted rather than assumed, so a change to the default that
// dropped the block would fail here loudly instead of silently making every
// admit-case vacuous.
func conventionsWithCharter(t *testing.T) workmgmt.Conventions {
	t.Helper()
	d := workmgmt.Default()
	if d.Charter == nil || d.Charter.Path == "" {
		t.Fatalf("workmgmt.Default() declares no charter (%+v); this fixture depends on it", d.Charter)
	}
	return d
}

// conventionsWithoutCharter is the same config with the charter block removed.
func conventionsWithoutCharter(t *testing.T) workmgmt.Conventions {
	t.Helper()
	d := workmgmt.Default()
	d.Charter = nil
	return d
}

// charterRunBody is the create-request skeleton every case below varies.
func charterRunBody(workflowID, specYAML string) map[string]any {
	return map[string]any{
		"repo": "x/y", "workflow_id": workflowID, "workflow_sha": "abc",
		"trigger_source": "cli", "workflow_spec": specYAML,
	}
}

// TestCharterGate_GroomingWithCharter_Admits is non-vacuity control N1: the
// gate must not refuse everything it inspects.
func TestCharterGate_GroomingWithCharter_Admits(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)

	w := createRunViaHandler(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML))
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201 — a grooming workflow in a chartered repo is admitted:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 0 {
		t.Errorf("run_rejected_missing_charter appends = %d, want 0 on an admitted run", n)
	}
}

// TestCharterGate_NonGroomingWorkflowWithoutCharter_Admits is non-vacuity
// control N2 — the gate is NARROW. An ordinary code-change workflow is admitted
// even when the repo declares no charter at all.
//
// COUNTERFACTUAL: delete the WorkflowRequiresCharter early return in
// checkCharterDeclared (making the gate fire on every workflow) and this test
// goes RED with a 422.
func TestCharterGate_NonGroomingWorkflowWithoutCharter_Admits(t *testing.T) {
	s, repo, _, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithoutCharter(t), nil)

	w := createRunViaHandler(t, s, charterRunBody("feature_change", nonGroomingSpecYAML))
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201 — the charter rule governs GROOMING workflows only:\n%s", w.Code, w.Body.String())
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
}

// TestCharterGate_MissingCharterBlock_Refuses is F1, the load-bearing refusal.
// It asserts the status, the code, the actionable message, details.reason, AND
// the committed state (no run row, exactly one GLOBAL audit entry, zero
// run-scoped ones — the gate fires pre-insert, so there is no run to scope to).
//
// COUNTERFACTUAL: delete the `conv.Charter == nil` arm in checkCharterDeclared
// and this test goes RED with a 201.
func TestCharterGate_MissingCharterBlock_Refuses(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithoutCharter(t), nil)

	w := createRunViaHandler(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML))
	if w.Code != 422 {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	body := decodeErrorEnvelope(t, w)
	if body.Code != "charter_required" {
		t.Errorf("code = %q, want charter_required", body.Code)
	}
	for _, want := range []string{"tidy_the_backlog", conventionsPathForMessage, "charter"} {
		if !contains(body.Message, want) {
			t.Errorf("message %q does not name %q", body.Message, want)
		}
	}
	if got := body.Details["reason"]; got != reasonCharterAbsent {
		t.Errorf("details.reason = %v, want %q", got, reasonCharterAbsent)
	}
	if got := body.Details["workflow_id"]; got != "tidy_the_backlog" {
		t.Errorf("details.workflow_id = %v, want tidy_the_backlog", got)
	}
	if got := body.Details["conventions_path"]; got != conventionsPathForMessage {
		t.Errorf("details.conventions_path = %v, want %q", got, conventionsPathForMessage)
	}
	// Committed state, read AFTER the call: a refusal that rolled back would
	// return a byte-identical envelope, so the envelope alone proves nothing.
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows = %d, want 0 — the gate is PRE-INSERT", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 1 {
		t.Errorf("global run_rejected_missing_charter appends = %d, want exactly 1", n)
	}
	au.mu.Lock()
	runScoped := len(au.appended)
	au.mu.Unlock()
	if runScoped != 0 {
		t.Errorf("run-scoped appends = %d, want 0 — there is no run to scope an entry to", runScoped)
	}
}

// TestCharterGate_EmptyCharterPath_Refuses is F2: a declared block whose path
// is empty (or whitespace) anchors nothing, so it is refused with its OWN
// reason. Without the distinct reason this branch's deletion would hide behind
// F1's assertions.
func TestCharterGate_EmptyCharterPath_Refuses(t *testing.T) {
	for _, path := range []string{"", "   "} {
		t.Run("path="+path, func(t *testing.T) {
			s, repo, au, _ := newDelegationServer(t)
			conv := conventionsWithCharter(t)
			conv.Charter = &workmgmt.Charter{Path: path}
			stubConventions(t, conv, nil)

			w := createRunViaHandler(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML))
			if w.Code != 422 {
				t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
			}
			body := decodeErrorEnvelope(t, w)
			if body.Code != "charter_required" {
				t.Errorf("code = %q, want charter_required", body.Code)
			}
			if got := body.Details["reason"]; got != reasonCharterPathEmpty {
				t.Errorf("details.reason = %v, want %q", got, reasonCharterPathEmpty)
			}
			if n := len(repo.runs); n != 0 {
				t.Errorf("run rows = %d, want 0", n)
			}
			if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 1 {
				t.Errorf("global appends = %d, want exactly 1", n)
			}
		})
	}
}

// TestCharterGate_ConventionsLoadError_FailsClosed is F3: an unreadable
// conventions file REFUSES rather than admits. Admitting on a transient forge
// fault would let a grooming run start unanchored, which is the one outcome
// ADR-065-as-amended rules out.
func TestCharterGate_ConventionsLoadError_FailsClosed(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, workmgmt.Conventions{}, errors.New("forge unreachable"))

	w := createRunViaHandler(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML))
	if w.Code != 422 {
		t.Fatalf("status = %d, want 422 — a conventions load failure must FAIL CLOSED:\n%s", w.Code, w.Body.String())
	}
	body := decodeErrorEnvelope(t, w)
	if got := body.Details["reason"]; got != reasonConventionsUnavailable {
		t.Errorf("details.reason = %v, want %q", got, reasonConventionsUnavailable)
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows = %d, want 0", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 1 {
		t.Errorf("global appends = %d, want exactly 1", n)
	}
}

// TestCharterGate_ReplayShortCircuitsBeforeGate is F4: the gate is POST-REPLAY.
// A create that succeeded under a charter, replayed with the same
// Idempotency-Key after the charter has gone, returns the ORIGINAL run rather
// than re-evaluating the configuration decision — and appends no second entry.
func TestCharterGate_ReplayShortCircuitsBeforeGate(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)

	const key = "charter-replay-1"
	first := createRunViaHandlerWithKey(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML), key)
	if first.Code != 201 {
		t.Fatalf("first create status = %d, want 201:\n%s", first.Code, first.Body.String())
	}

	// The charter disappears between the two deliveries of the SAME request.
	stubConventions(t, conventionsWithoutCharter(t), nil)
	replay := createRunViaHandlerWithKey(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML), key)
	if replay.Code != 200 {
		t.Fatalf("replay status = %d, want 200 — the idempotency lookup short-circuits BEFORE the gate:\n%s", replay.Code, replay.Body.String())
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1 — a replay creates no second run", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 0 {
		t.Errorf("run_rejected_missing_charter appends = %d, want 0 — a replay re-evaluates no configuration decision", n)
	}
}

// TestCharterGate_AuditAppendFailure_RefusalStands is F5: the refusal audit is
// BEST-EFFORT. An audit outage must not convert into a governance outage by
// letting the run through — the refusal already went the safe way.
func TestCharterGate_AuditAppendFailure_RefusalStands(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	au.appendErr = errors.New("audit store unreachable")
	stubConventions(t, conventionsWithoutCharter(t), nil)

	w := createRunViaHandler(t, s, charterRunBody("tidy_the_backlog", groomingSpecYAML))
	if w.Code != 422 {
		t.Fatalf("status = %d, want 422 — the refusal stands even when it cannot be recorded:\n%s", w.Code, w.Body.String())
	}
	if got := decodeErrorEnvelope(t, w).Details["reason"]; got != reasonCharterAbsent {
		t.Errorf("details.reason = %v, want %q", got, reasonCharterAbsent)
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows = %d, want 0", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 0 {
		t.Errorf("global appends = %d, want 0 — the fake refused every append", n)
	}
}

// TestWorkflowRequiresCharter_StructuralDiscriminator pins the predicate
// itself: it keys on the PRODUCED ARTIFACT, so a rename cannot evade it and a
// workflow named backlog_grooming that produces no report does not trip it.
func TestWorkflowRequiresCharter_StructuralDiscriminator(t *testing.T) {
	grooming := mustParseWorkflow(t, groomingSpecYAML, "tidy_the_backlog")
	if !WorkflowRequiresCharter(grooming) {
		t.Error("WorkflowRequiresCharter = false for a workflow producing grooming_report, want true")
	}
	ordinary := mustParseWorkflow(t, nonGroomingSpecYAML, "feature_change")
	if WorkflowRequiresCharter(ordinary) {
		t.Error("WorkflowRequiresCharter = true for an ordinary code-change workflow, want false")
	}
	// A workflow NAMED backlog_grooming that produces no report is not a
	// grooming workflow: the name is not the discriminator.
	named := mustParseWorkflow(t, `version: "2"
workflows:
  backlog_grooming:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`, "backlog_grooming")
	if WorkflowRequiresCharter(named) {
		t.Error("WorkflowRequiresCharter keyed off the workflow NAME; it must key off the produced artifact")
	}
}

// --- the haveStageDefs admission region (review follow-up) -------------------

// TestCharterGate_NoStageDefsPathCarriesNoWorkflow answers the review's
// question about the gate's `haveStageDefs` guard executably rather than by
// assertion.
//
// THE INVARIANT. `haveStageDefs` is set true by BOTH spec-resolution branches
// in handleCreateRun — the inline `workflow_spec` branch and the GitHub-fetch
// branch — in the same statement that assigns `workflowDef`, and each branch
// rejects a workflow with zero stages BEFORE reaching that assignment. So
// `haveStageDefs == false` means no workflow definition was resolved AT ALL:
// `workflowDef` is the zero spec.Workflow, no stage rows are created, and
// WorkflowRequiresCharter — which iterates wf.Stages — is false by
// construction. A grooming-capable workflow necessarily DECLARES a stage
// producing grooming_report, so it can never be behind that branch. The only
// other CreateRunForTrigger caller, the campaign driver, passes
// HaveStageDefs: true unconditionally and errors out before the call when the
// resolved workflow has no stages.
//
// This test pins the observable half: the no-spec path really does create a
// run with ZERO stages, and it does so with the loader stubbed to the WORST
// case (no charter declared at all) — so if that path ever began carrying a
// grooming-capable workflow, the gate would refuse and the 201 below would be
// a 422.
func TestCharterGate_NoStageDefsPathCarriesNoWorkflow(t *testing.T) {
	s, repo, au, _ := newDelegationServer(t)
	stubConventions(t, conventionsWithoutCharter(t), nil)

	// No workflow_spec, and newDelegationServer wires no GitHub client — the
	// ONE input combination that reaches CreateRunForTrigger with
	// haveStageDefs false.
	w := createRunViaHandler(t, s, map[string]any{
		"repo": "x/y", "workflow_id": "backlog_grooming", "workflow_sha": "abc",
		"trigger_source": "cli",
	})
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if n := len(repo.stagesFor(created.ID)); n != 0 {
		t.Errorf("stage rows = %d, want 0 — haveStageDefs=false means NO workflow definition was resolved, "+
			"so a run behind that branch cannot declare a grooming_report-producing stage", n)
	}
	// The value the branch actually carries into CreateRunForTrigger. Note the
	// workflow_id above IS `backlog_grooming`: the name does not make a run
	// grooming-capable, the resolved stages do, and there are none.
	if WorkflowRequiresCharter(spec.Workflow{}) {
		t.Error("WorkflowRequiresCharter(zero Workflow) = true — the haveStageDefs=false branch would then need its own gate")
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 0 {
		t.Errorf("run_rejected_missing_charter appends = %d, want 0 — there is no workflow to refuse", n)
	}
}

// --- AC7: report mode at the RUNTIME consumer -------------------------------
//
// The spec-package TestShippedGroomingExample_ReportModeDerivesNoAuthorityOrGate
// asserts the STATIC shape of the shipped declaration: empty may_* knobs, the
// declared gate inventory, an empty page list. That is necessary and not
// sufficient — a runtime consumer could begin treating a report-mode class as
// gated, or park the run on it, without touching either the workflow's Gates
// slices or ResolveAutonomy's PageHumanOn. The BEHAVIOURAL half is therefore
// pinned here, at the real consumers: delegation.Evaluate (which decides what
// authority a class derives) and AutoDriveRunGate's `mode: report` arm (the
// only runtime site that reads a report entry), driven over the SHIPPED
// declaration read from disk.

// groomingExampleFromServer is the shipped declaration under test, at this
// package's depth. Read from disk for the same reason the spec-package family
// does: a drift in the example must redden this test.
const groomingExampleFromServer = "../../../docs/spec/examples/workflow-v2-backlog-grooming.yaml"

// startShippedGroomingRun creates a run governed by the shipped grooming
// declaration and returns its id plus its three stages (groom/apply/confirm).
//
// NO applies_to_override (E54.22 / #2826). This helper previously carried an
// audited override whose stated reason was that no producer emitted the
// non-diff trigger forms, so admission would refuse before the run-state path
// under test was reachable. `on_demand` IS that producer now, so the run is
// admitted on the declaration's own terms — which converts a standing
// workaround into a REGRESSION TEST: if TriggerFormForSource stops mapping
// on_demand to spec.TriggerOnDemand, every TestShippedGrooming* case using
// this helper fails at admission with a 422, against the SHIPPED declaration
// read from disk rather than a fixture.
//
// The issue_context is what makes the run issue-anchored, matching the
// declaration's required github_issue input.
func startShippedGroomingRun(t *testing.T, s *Server, repo *autoDriveRepo) (uuid.UUID, []*run.Stage) {
	t.Helper()
	raw, err := os.ReadFile(groomingExampleFromServer)
	if err != nil {
		t.Fatalf("read %s: %v", groomingExampleFromServer, err)
	}
	w := createRunViaHandler(t, s, map[string]any{
		"repo": "x/y", "workflow_id": "backlog_grooming", "workflow_sha": "abc",
		"trigger_source": string(run.TriggerOnDemand), "workflow_spec": string(raw),
		"trigger_ref":   "issue:2826",
		"issue_context": issueCtx(),
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d, want 201 — the shipped declaration routes on trigger: [scheduled, on_demand] and on_demand is its producer:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	stages := repo.stagesFor(created.ID)
	if len(stages) != 3 {
		t.Fatalf("stage rows = %d, want 3 (groom/apply/confirm)", len(stages))
	}
	return created.ID, stages
}

// TestShippedGrooming_ReportModeNeitherGatesNorParksAtTheConsumer is AC7's
// behavioural half (approval condition G2, sharpened by the implement review).
//
// The shipped `scoping: report` class is carried all the way to the runtime
// consumer, and the consumer is asserted to do NOTHING with it: it derives no
// delegated decision, surfaces no proposal, dispatches no action, emits no
// page, parks neither the stage nor the run, and appends no run_auto_driven
// row. Every assertion reads state AFTER the real AutoDriveRunGate call
// returns, not the resolved matrix it was handed.
//
// COUNTERFACTUALS (run, not reasoned — see the PR notes):
//   - mapping the `scoping` class to a delegation verb in
//     delegation.actionClasses makes the report arm find a live gate and
//     append an act:report row → RED on Reported / the row count.
//   - flipping the shipped `scoping` default to `gated` → RED on the
//     precondition below, which is what keeps this test from being a test
//     about report mode that never saw one.
func TestShippedGrooming_ReportModeNeitherGatesNorParksAtTheConsumer(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	stubConventions(t, conventionsWithCharter(t), nil)
	runID, stages := startShippedGroomingRun(t, s, repo)
	groom := stages[0]
	// Park the propose stage at its declared approval gate and seed the clean
	// two-agent review round: the gate is LIVE, which is precisely the state
	// the report arm fires on. A test run at a dead gate would prove nothing.
	groom.State = run.StageStateAwaitingApproval
	seedCleanPlanApproval(t, au, runID)
	runBefore := getRun(t, repo, runID)
	stateBefore := runBefore.State

	// PRECONDITION — non-vacuity. The matrix the consumer reads must actually
	// carry `scoping` at mode: report, and hygiene at auto (proving the whole
	// grooming block reached the consumer, not just a default).
	res, _, ok := s.evaluateRunDelegation(context.Background(), runBefore, nil)
	if !ok || res == nil {
		t.Fatal("evaluateRunDelegation returned no result; the consumer never saw the shipped matrix")
	}
	scoping, found := res.MatrixEntry(spec.ActionGroomScoping)
	if !found || scoping.Mode != spec.ModeReport {
		t.Fatalf("consumer matrix entry for scoping = %+v (found %v), want mode: report", scoping, found)
	}
	if hygiene, ok := res.MatrixEntry(spec.ActionGroomHygiene); !ok || hygiene.Mode != spec.ModeAuto {
		t.Fatalf("consumer matrix entry for hygiene = %+v (found %v), want mode: auto", hygiene, ok)
	}

	// NO AUTHORITY at the enforcement site: a report-mode class produces no
	// delegated Decision, and — being an extension class with no
	// backend-evaluable condition — no Report decision either. res.Actions is
	// what every delegated dispatch arm reads.
	for _, d := range res.Actions {
		t.Errorf("delegated decision %+v derived under a grooming matrix; autonomy: low delegates nothing", d)
	}
	for _, d := range res.Reports {
		t.Errorf("report decision %+v derived for an extension class with no backend-evaluable condition", d)
	}

	// THE RUNTIME PATH. This is the call the campaign driver and the
	// POST /auto-drive endpoint both make.
	out, err := s.AutoDriveRunGate(context.Background(), runBefore, campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if out.Acted || out.Reported || out.Paged || out.DecisionRequired {
		t.Errorf("outcome = %+v; a report-mode grooming class must neither act, surface a proposal, page, nor park", out)
	}

	// NO GATE, NO PARK — read as COMMITTED STATE after the call, because an
	// outcome struct alone cannot distinguish "did nothing" from "did
	// something and reported nothing".
	if groom.State != run.StageStateAwaitingApproval {
		t.Errorf("groom stage = %q, want awaiting_approval — report mode moved no stage", groom.State)
	}
	for _, st := range stages[1:] {
		if st.State != run.StageStatePending {
			t.Errorf("stage %q = %q, want pending — report mode created no new gate", st.Type, st.State)
		}
	}
	if after := getRun(t, repo, runID); after.State != stateBefore {
		t.Errorf("run state = %q, want %q (unchanged) — report mode must not park the run", after.State, stateBefore)
	}
	if n := countAudit(au, CategoryRunAutoDriven); n != 0 {
		t.Errorf("run_auto_driven rows = %d, want 0 — the consumer acted on and reported nothing", n)
	}
	if n := countAudit(au, CategoryCampaignGatePaged); n != 0 {
		t.Errorf("campaign_gate_paged rows = %d, want 0 — a report-mode class pages nobody", n)
	}
	if n := countAudit(au, "approval_submitted"); n != 0 {
		t.Errorf("approval_submitted rows = %d, want 0", n)
	}
}

// TestShippedGrooming_ReportArmIsLiveInThisHarness is the PAIRED CONTROL for
// the zeros above. Same server harness, same AutoDriveRunGate call, same live
// plan-approval gate — but with the report entry on a BACKEND-KNOWN class. It
// DOES surface a proposal and DOES append the act:report row, so the previous
// test's zeros are the consumer answering rather than the consumer never
// running.
func TestShippedGrooming_ReportArmIsLiveInThisHarness(t *testing.T) {
	s, repo, au, _ := newAutoDriveServer(t)
	runID, _ := startAutoDriveRunWithSpec(t, s, repo, v2ReportSpecYAML(`    autonomy: medium
    actions:
      approve:
        mode: report`))
	seedCleanPlanApproval(t, au, runID)

	out, err := s.AutoDriveRunGate(context.Background(), getRun(t, repo, runID), campaignOperatorIdentity(), nil, nil)
	if err != nil {
		t.Fatalf("AutoDriveRunGate: %v", err)
	}
	if !out.Reported {
		t.Fatalf("outcome = %+v, want Reported — the report arm is inert in this harness, so the negative test proves nothing", out)
	}
	if n := countReportRows(t, au); n != 1 {
		t.Errorf("act:report rows = %d, want 1", n)
	}
}

// --- the CAMPAIGN seam (implement-review follow-up) --------------------------
//
// CreateRunForTrigger has two callers: handleCreateRun (gated above) and
// StartRunForCampaignIssue, which the campaign item-run endpoint and the
// campaign driver both reach. Unlike the haveStageDefs=false branch, that seam
// is NOT structurally barred from carrying a grooming workflow — it fetches the
// repo's spec from the forge and resolves an operator-named workflow_id out of
// it — so it is gated rather than documented away, and the gate is pinned here.

// newCharterCampaignServer wires the run + audit fakes plus a GitHub stub
// serving specYAML, so StartRunForCampaignIssue resolves a real installation
// and a real spec and reaches (or is refused before) CreateRunForTrigger.
func newCharterCampaignServer(t *testing.T, specYAML string) (*Server, *driveE2ERepo, *auditFake) {
	t.Helper()
	repo := &driveE2ERepo{fakeRepo: newFakeRepo()}
	au := newAuditFake()
	ghSrv := newFakeGitHubForRuns(specYAML).server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, GitHub: gh})
	return s, repo, au
}

// startCampaignRun drives the campaign seam for one workflow id.
func startCampaignRun(t *testing.T, s *Server, workflowID string) (*run.Run, error) {
	t.Helper()
	return s.StartRunForCampaignIssue(context.Background(), StartRunForCampaignIssueParams{
		Repo:       "x/y",
		IssueRef:   "issue:100",
		WorkflowID: workflowID,
		RunnerKind: "local",
	})
}

// TestCharterGate_CampaignSeam_MissingCharter_Refuses is the seam's F1: the
// campaign path refuses a grooming workflow in an uncharted repo BEFORE any run
// row exists, with the same reason and the same actionable message the HTTP
// seam's 422 carries.
//
// COUNTERFACTUAL: delete the ensureCharterDeclared call in
// StartRunForCampaignIssue and this test goes RED — a run is minted and err is
// nil.
func TestCharterGate_CampaignSeam_MissingCharter_Refuses(t *testing.T) {
	s, repo, au := newCharterCampaignServer(t, groomingSpecYAML)
	stubConventions(t, conventionsWithoutCharter(t), nil)

	got, err := startCampaignRun(t, s, "tidy_the_backlog")
	if err == nil {
		t.Fatalf("StartRunForCampaignIssue err = nil, want a charter refusal (run=%+v)", got)
	}
	// Error IDENTITY, not message shape: the seam has several other failure
	// modes (installation, fetch, parse, zero stages) whose text a substring
	// assertion could match.
	if !errors.Is(err, errCharterRequired) {
		t.Errorf("err = %v, want errCharterRequired", err)
	}
	for _, want := range []string{reasonCharterAbsent, "tidy_the_backlog", conventionsPathForMessage} {
		if !contains(err.Error(), want) {
			t.Errorf("err %q does not name %q", err.Error(), want)
		}
	}
	// COMMITTED STATE after the call: AC8's property on this seam.
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows = %d, want 0 — the campaign seam's gate is PRE-MINT", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 1 {
		t.Errorf("global run_rejected_missing_charter appends = %d, want exactly 1", n)
	}
}

// TestCharterGate_CampaignSeam_ConventionsLoadError_FailsClosed is the seam's
// F3 — the campaign path fails CLOSED on an unreadable conventions file for the
// same reason the HTTP path does, and with its own distinguishable reason.
func TestCharterGate_CampaignSeam_ConventionsLoadError_FailsClosed(t *testing.T) {
	s, repo, au := newCharterCampaignServer(t, groomingSpecYAML)
	stubConventions(t, workmgmt.Conventions{}, errors.New("forge unreachable"))

	if _, err := startCampaignRun(t, s, "tidy_the_backlog"); err == nil {
		t.Fatal("StartRunForCampaignIssue err = nil, want a fail-closed charter refusal")
	} else if !contains(err.Error(), reasonConventionsUnavailable) {
		t.Errorf("err %q does not carry reason %q", err.Error(), reasonConventionsUnavailable)
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows = %d, want 0", n)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 1 {
		t.Errorf("global appends = %d, want exactly 1", n)
	}
}

// TestCharterGate_CampaignSeam_GroomingWithCharter_Admits is the seam's
// non-vacuity control N1: the gate does not refuse everything this path
// resolves. It is also what proves the refusals above are the GATE answering
// rather than the harness failing to resolve a spec at all.
func TestCharterGate_CampaignSeam_GroomingWithCharter_Admits(t *testing.T) {
	s, repo, au := newCharterCampaignServer(t, groomingSpecYAML)
	stubConventions(t, conventionsWithCharter(t), nil)

	created, err := startCampaignRun(t, s, "tidy_the_backlog")
	if err != nil {
		t.Fatalf("StartRunForCampaignIssue: %v — a grooming workflow in a chartered repo is admitted", err)
	}
	if created == nil || len(repo.runs) != 1 {
		t.Errorf("run rows = %d (created=%v), want exactly 1", len(repo.runs), created)
	}
	if n := countGlobalAppends(au, "run_rejected_missing_charter"); n != 0 {
		t.Errorf("run_rejected_missing_charter appends = %d, want 0 on an admitted run", n)
	}
}

// TestCharterGate_CampaignSeam_NonGroomingWithoutCharter_Admits is the seam's
// non-vacuity control N2: the campaign path's gate is as NARROW as the HTTP
// path's. An ordinary code-change workflow starts in a repo with no charter.
func TestCharterGate_CampaignSeam_NonGroomingWithoutCharter_Admits(t *testing.T) {
	s, repo, _ := newCharterCampaignServer(t, nonGroomingSpecYAML)
	stubConventions(t, conventionsWithoutCharter(t), nil)

	if _, err := startCampaignRun(t, s, "feature_change"); err != nil {
		t.Fatalf("StartRunForCampaignIssue: %v — the charter rule governs GROOMING workflows only", err)
	}
	if n := len(repo.runs); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
}

// TestCharterGate_EveryCreateRunForTriggerCallerIsGated is the SOURCE-LEVEL pin
// for "every run-minting seam is gated". The two behavioural families above
// prove the two callers that exist TODAY are gated; only a source scan proves a
// THIRD one cannot be added ungated — which is exactly how this seam was missed
// in the first place.
//
// Modelled on backend/internal/run/childparams_gate_test.go: AST-based (a line
// regex misses an aliased receiver and a multi-line call), keyed by enclosing
// function, and FAIL-CLOSED on a parse error.
func TestCharterGate_EveryCreateRunForTriggerCallerIsGated(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the backend module root")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..") // backend/

	// The gate calls that discharge the obligation. Both arms consume the same
	// evaluateCharterAdmission core, so either satisfies it.
	gateCalls := map[string]bool{"checkCharterDeclared": true, "ensureCharterDeclared": true}

	var ungated []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "db" || name == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr) // fail closed
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		for _, decl := range file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn || fn.Body == nil || fn.Name.Name == "CreateRunForTrigger" {
				continue
			}
			var mints, gated bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				if !isCall {
					return true
				}
				sel, isSel := call.Fun.(*ast.SelectorExpr)
				if !isSel {
					return true
				}
				switch {
				case sel.Sel.Name == "CreateRunForTrigger":
					mints = true
				case gateCalls[sel.Sel.Name]:
					gated = true
				}
				return true
			})
			if mints && !gated {
				ungated = append(ungated, rel+"::"+fn.Name.Name)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scan %s: %v", root, walkErr)
	}
	if len(ungated) > 0 {
		sort.Strings(ungated)
		t.Errorf("CreateRunForTrigger caller(s) with no charter gate: %v\n"+
			"Every run-minting seam must call checkCharterDeclared (HTTP) or ensureCharterDeclared (ctx/error) "+
			"BEFORE minting, or a grooming run starts unanchored on that seam (ADR-065 / #2236).", ungated)
	}
	// NON-VACUITY: the scan must actually have found the known callers. A
	// walker that matched nothing would pass the assertion above trivially.
	var found int
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil //nolint:nilerr // best-effort recount; the authoritative walk above already failed closed
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, isSel := n.(*ast.SelectorExpr); isSel && sel.Sel.Name == "CreateRunForTrigger" {
				found++
			}
			return true
		})
		return nil
	})
	if found < 2 {
		t.Errorf("scan found %d CreateRunForTrigger reference(s), want >= 2 (the declaration plus at least one caller); "+
			"the walker is not reaching the source it is supposed to gate", found)
	}
}
