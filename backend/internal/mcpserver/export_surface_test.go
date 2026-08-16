package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// exportBaseline is the sorted set of exported top-level identifiers this
// package presents after the E66.7 / #2408 extraction. It is generated FROM
// THE TREE (parse every non-test .go file, collect exported non-method
// FuncDecls, TypeSpecs and ValueSpecs), not transcribed, so a drift between
// an estimate and reality would surface as a test written from the tree.
//
// The bulk of these 235 names are tool I/O request/response structs. The MCP
// SDK's jsonschema reflection requires each tool's input/output type — and
// its fields — to be EXPORTED to build the tool's schema, so unexporting them
// would break tool registration. In `package main` their exportedness was
// cosmetic; the move to a library package makes it real. Unexporting them is
// explicitly OUT OF SCOPE (#2390 needs only Config and NewServer); they are
// grandfathered by this baseline. The baseline fails in EITHER direction, so
// a NEW export added later is caught while the pre-existing surface is pinned.
var exportBaseline = []string{
	"AcceptanceAdmissionResult",
	// #2581: the per-entry shape of fishhawk_approve_plan's
	// amend_acceptance_criteria input. Exported for the same reason as
	// BindingAssertion below — the MCP SDK's jsonschema reflection needs the
	// nested type (and its fields) exported to advertise the entry shape.
	"AcceptanceCriteriaAmendment",
	"AcceptanceNeedsTarget",
	"AcceptanceSlot",
	"AcceptanceSlotClaim",
	"AnswerClarificationInput",
	"AnswerClarificationOutput",
	"ApproveDeployInput",
	"ApproveDeployOutput",
	"ApprovePlanInput",
	"ApprovePlanOutput",
	"ArbitrateAcceptanceInput",
	"ArbitrateAcceptanceOutput",
	"ArbitrateAcceptanceResult",
	"Artifact",
	"AuditEntry",
	"AuditPointer",
	"AutoDriveOutcome",
	"AwaitAuditInput",
	"AwaitAuditOutput",
	"AwaitChildrenInput",
	"AwaitChildrenOutput",
	"AwaitReviewInput",
	"AwaitReviewOutput",
	"AwaitStageInput",
	"AwaitStageOutput",
	"BindingAssertion",
	"BudgetStatus",
	"CacheEfficiency",
	"CacheEfficiencyStage",
	"CalibrationParams",
	"CalibrationResult",
	"Campaign",
	"CampaignItem",
	"CampaignNextAction",
	"CampaignPauseReason",
	"CampaignRollup",
	"CampaignStatus",
	"CancelCampaignInput",
	"CancelCampaignOutput",
	"CancelRunInput",
	"CancelRunOutput",
	"CategoryRunAutoDriven",
	"ChildCriteriaCheck",
	"ChildResult",
	"ChildStatus",
	"ChildrenStatus",
	"ClarificationAnswer",
	"Config",
	"ConsolidateResult",
	"ConsolidateSlicesInput",
	"ConsolidateSlicesOutput",
	"CriteriaPrecheck",
	"CrossSliceClaim",
	"CrossSliceCouplingFinding",
	"DecideScopeAmendmentInput",
	"DecideScopeAmendmentOutput",
	"DecideScopeCompletenessInput",
	"DecideScopeCompletenessOutput",
	"DeferConcernInput",
	"DeferConcernOutput",
	"DeferConcernParams",
	"DeferFiledIssue",
	"DeferredConcern",
	"DeferredConcernResult",
	"DiagnosticBundle",
	"DiagnosticComponent",
	"DiagnosticFailingStage",
	"DiagnosticSeqRange",
	"DiagnosticStageFact",
	"DiagnosticVersions",
	"DiffSummary",
	"DispatchStageInput",
	"DispatchStageOutput",
	"DoctorInput",
	"DoctorOutput",
	"DraftEpicInput",
	"DraftEpicOutput",
	"DriveRunInput",
	"DriveRunOutput",
	"DriveStatus",
	"DriveStep",
	// ADR-077 / #2508: the get_run_status response byte bound's wire DTO. These
	// two MUST stay exported — the MCP SDK validates marshalled tool output
	// against the reflected output schema, and jsonschema-go skips unexported
	// fields while forbidding additional properties, so an unexported variant
	// breaks the tool at runtime (see the type comment in bound.go).
	"ElidedField",
	"Elisions",
	"EpicDraft",
	"EpicDraftChild",
	"EpicDraftEpic",
	"ErrGhNotInstalled",
	"FileIssueInput",
	"FileIssueOutput",
	"FileIssueRelations",
	"FileWorkItemRequest",
	"FiledWorkItem",
	"FixupStageInput",
	"FixupStageOutput",
	"GateView",
	"GateViewConcern",
	"GateViewFixup",
	"GateViewResolution",
	"GateViewSettledConcern",
	"GateViewSuppressedRelitig",
	"GetActiveRunInput",
	"GetActiveRunOutput",
	"GetCampaignStatusInput",
	"GetCampaignStatusOutput",
	"GetGateViewInput",
	"GetGateViewOutput",
	"GetPlanInput",
	"GetPlanOutput",
	"GetRunStatusInput",
	"GetRunStatusOutput",
	// #2712: the decoded /healthz slice the restart-strand probe reads
	// (process_start + the sibling identity fields). Exported alongside the
	// other apiClient result types.
	"HealthInfo",
	"HostDispatchResult",
	"InitInput",
	"InitOutput",
	"Instructions",
	"IntegrateWaveResult",
	"IssueComment",
	"IssueContext",
	"LatencyGate",
	"ListAuditInput",
	"ListAuditOutput",
	"ListRunAuditFilter",
	"ListRunsInput",
	"ListRunsOutput",
	"ListScopeAmendmentsInput",
	"ListScopeAmendmentsOutput",
	"MergeRunInput",
	"MergeRunOutput",
	"MergeRunResult",
	"NewServer",
	"NextActions",
	"OnboardingApp",
	"OnboardingReadinessReport",
	"OnboardingReviewer",
	"OnboardingScopes",
	"OnboardingSpec",
	"PlanApproachStep",
	"PlanContent",
	"PlanDecomposed",
	"PlanDecomposition",
	"PlanGeneratedBy",
	"PlanReachability",
	"PlanReachabilityPhase",
	"PlanReachabilityViolation",
	"PlanReview",
	"PlanReviewConcern",
	"PlanScope",
	"PlanScopeFile",
	"PlanSplitCapException",
	"PlanSplitFiling",
	"PlanSplitFilingChild",
	"PlanSplitPhase",
	"PlanSplitProposal",
	"PlanSubPlan",
	"PlanTicketRef",
	"PlanVerification",
	"ProductReport",
	"ReapFailureResult",
	"ReapStageInput",
	"ReapStageOutput",
	// #2712: fishhawk_reconcile_reviews' tool I/O plus the apiClient result
	// types for POST /v0/runs/{run_id}/reviews/reconcile. Exported for the
	// same reason as the ReapStage* sibling verb above — the MCP SDK's
	// jsonschema reflection advertises the nested per-stage row shape.
	"ReconcileReviewsInput",
	"ReconcileReviewsOutput",
	"ReconcileReviewsResult",
	"ReconcileReviewsStage",
	"ReconciledReviewStage",
	"RecordAutoDriveAct",
	"RecordAutoDriveActResult",
	"RecoverExemptPath",
	"RecoverRunParams",
	"RecoverScopePath",
	"RefinementAcceptanceFinding",
	"RefinementDecision",
	"RefinementFilingChild",
	"RefinementFilingEpic",
	"RefinementFilingResult",
	"RefinementSession",
	"RejectDeployInput",
	"RejectDeployOutput",
	"RejectPlanInput",
	"RejectPlanOutput",
	"ReleaseNotesInput",
	"ReleaseNotesOutput",
	"ReleaseNotesPersistResult",
	"ReportProductIssueInput",
	"ReportProductIssueOutput",
	"ResetBranchResult",
	"ResetRunBranchInput",
	"ResetRunBranchOutput",
	"ResumeCampaignInput",
	"ResumeCampaignOutput",
	"ResumeRunInput",
	"ResumeRunOutput",
	"RetryStageInput",
	"RetryStageOutput",
	"ReviewActionHint",
	"ReviewStatus",
	"RevisePlanInput",
	"RevisePlanOutput",
	"ReviveRestoredStage",
	"ReviveRunInput",
	"ReviveRunOutput",
	"ReviveRunResult",
	"Run",
	"RunAutoAdvance",
	"RunChildrenInput",
	"RunChildrenOutput",
	"RunConcernItem",
	"RunConcerns",
	"RunCost",
	"RunCostStage",
	"RunLatency",
	"RunLiveValidation",
	"RunMergedPRCost",
	"RunNextAction",
	"RunReviewAuthority",
	"RunStageInput",
	"RunStageOutput",
	"RunStageWait",
	"RunnerEvent",
	"RuntimeCalibrationInput",
	"RuntimeCalibrationOutput",
	"ScopeAmendmentItem",
	"ScopeAmendmentPath",
	"ScopeCompletenessDecisionResult",
	"ScopePrecheck",
	"ScopePrecheckViolation",
	"SecurityFinding",
	"SessionGuidance",
	"Stage",
	"StageExecutor",
	"StageProgress",
	"StageWaitStatus",
	"StartCampaignInput",
	"StartCampaignItemRunInput",
	"StartCampaignItemRunOutput",
	"StartCampaignItemRunResult",
	"StartCampaignOutput",
	"StartRunInput",
	"StartRunOutput",
	"StartRunParams",
	"SuggestedAction",
	"SurfaceSweep",
	"SurfaceSweepFinding",
	"TestSweep",
	"TestSweepFinding",
	"VerifyRunInput",
	"VerifyRunOutput",
	"VouchCommitInput",
	"VouchCommitOutput",
	"VouchCommitResult",
	"WaiveConcernInput",
	"WaiveConcernOutput",
	"WaivedConcern",
	"WorkItemRelations",
}

// collectExportedIdents parses every non-test .go file in the package
// directory (ignoring build constraints via parser mode 0, so both
// run_stage_unix.go and run_stage_windows.go contribute and the baseline is
// platform-independent — neither exports anything today) and returns the
// sorted set of exported top-level identifier names.
func collectExportedIdents(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	set := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					continue // a method is not a top-level exported identifier
				}
				if ast.IsExported(d.Name.Name) {
					set[d.Name.Name] = struct{}{}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(s.Name.Name) {
							set[s.Name.Name] = struct{}{}
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if ast.IsExported(n.Name) {
								set[n.Name] = struct{}{}
							}
						}
					}
				}
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestExportedSurfaceMatchesBaseline fails if the exported top-level
// identifier set diverges from the recorded baseline in EITHER direction: a
// new export (a leaked helper, an accidentally-exported field-carrying type)
// or a removed one. The baseline is the anti-scope-creep guard the corrected
// #2408 requirement asks for — the 229 pre-existing tool I/O identifiers are
// grandfathered, and only Config/NewServer/Instructions were added.
func TestExportedSurfaceMatchesBaseline(t *testing.T) {
	got := collectExportedIdents(t)
	want := map[string]struct{}{}
	for _, n := range exportBaseline {
		want[n] = struct{}{}
	}
	gotSet := map[string]struct{}{}
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	for _, n := range got {
		if _, ok := want[n]; !ok {
			t.Errorf("NEW exported identifier %q not in the baseline — either revert the export or, if intentional, add it to exportBaseline with a rationale", n)
		}
	}
	for _, n := range exportBaseline {
		if _, ok := gotSet[n]; !ok {
			t.Errorf("baseline identifier %q is no longer exported — removing a grandfathered export is a behaviour change; confirm the MCP SDK no longer needs it", n)
		}
	}
	if len(got) != len(exportBaseline) {
		t.Errorf("exported identifier count = %d, want %d", len(got), len(exportBaseline))
	}
}

// TestEntryPointsExist asserts the three intended entry points are present
// and Config carries exactly the three documented exported fields — a fourth
// exported field on Config, or a dropped one, fails it. Config/NewServer/
// Instructions are the only surface #2390 consumes; this guards their shape
// independently of the bulk baseline. HTTPTransport joined {APIToken,
// BackendURL} with #2479 (the transport-conditional working_dir refusal), so
// the baseline is updated to the new EXACT three-field set — NOT loosened to a
// subset/contains check: a new exported field on Config must stay a conscious
// act.
func TestEntryPointsExist(t *testing.T) {
	surface := map[string]struct{}{}
	for _, n := range collectExportedIdents(t) {
		surface[n] = struct{}{}
	}
	for _, want := range []string{"Config", "NewServer", "Instructions"} {
		if _, ok := surface[want]; !ok {
			t.Errorf("intended entry point %q is not exported", want)
		}
	}

	// Config's exported fields must be exactly {APIToken, BackendURL,
	// HTTPTransport} — an EXACT-SET check (not subset/contains), so both a new
	// leaked field and a dropped one fail.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var fields []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Config" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatal("Config is not a struct type")
			}
			for _, fld := range st.Fields.List {
				for _, n := range fld.Names {
					if ast.IsExported(n.Name) {
						fields = append(fields, n.Name)
					}
				}
			}
		}
	}
	sort.Strings(fields)
	want := []string{"APIToken", "BackendURL", "HTTPTransport"}
	mismatch := len(fields) != len(want)
	for i := range want {
		if i >= len(fields) || fields[i] != want[i] {
			mismatch = true
			break
		}
	}
	if mismatch {
		t.Errorf("Config exported fields = %v, want exactly %v", fields, want)
	}
}
