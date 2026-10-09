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
// The bulk of these 363 names are tool I/O request/response structs. The MCP
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
	// E72.5 / #3329: the typed acceptance_transcript block on
	// fishhawk_get_run_status (status + per-criterion row + failing-request
	// triple), exported for the SDK's jsonschema reflection like every DTO.
	"AcceptanceTranscriptCriterion",
	"AcceptanceTranscriptRequest",
	"AcceptanceTranscriptStatus",
	"AnswerClarificationInput",
	"AnswerClarificationOutput",
	// E75.5 / #3733: fishhawk_answer_divergence's I/O and the client mirror of
	// POST /v0/runs/{run_id}/divergence/{sequence}/answer, exported for the
	// SDK's jsonschema reflection like every DTO.
	"AnswerDivergenceInput",
	"AnswerDivergenceOutput",
	"AnswerDivergenceParams",
	"AnswerDivergenceResult",
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
	// E64.77 / #3318: the client mirrors of the bulk-waive response
	// (POST /v0/runs/{run_id}/concerns/waive). Exported because
	// WaiveConcernsOutput embeds BulkWaiveResult, which embeds
	// []BulkWaiveItem, and the SDK reflects over the whole tree to build the
	// tool's output schema.
	"BulkWaiveItem",
	"BulkWaiveResult",
	"CacheEfficiency",
	"CacheEfficiencyStage",
	"CalibrationParams",
	"CalibrationResult",
	"Campaign",
	"CampaignItem",
	"CampaignNextAction",
	"CampaignPauseReason",
	// #3647: fishhawk_preview_campaign's report wire structs — the
	// non-mutating dry run of POST /v0/campaigns. Exported for the same
	// reason every other tool I/O type here is: the MCP SDK builds the
	// output schema by reflection over the exported type and its fields.
	"CampaignPreview",
	"CampaignPreviewDangling",
	"CampaignPreviewItem",
	"CampaignRollup",
	"CampaignStatus",
	"CancelCampaignInput",
	"CancelCampaignOutput",
	"CancelRunInput",
	"CancelRunOutput",
	// E76.2 / #3765: fishhawk_captain's input, output and the REST-mirror
	// DTOs it nests, exported for the SDK's jsonschema reflection like every
	// DTO.
	"CaptainHistoryEntry",
	"CaptainInput",
	"CaptainOffer",
	"CaptainOutput",
	"CaptainRecord",
	"CaptainState",
	"CaptainVerbResult",
	"CategoryRunAutoDriven",
	"ChildCriteriaCheck",
	"ChildResult",
	"ChildStatus",
	"ChildrenStatus",
	"ClarificationAnswer",
	// #3964 / ADR-087: the local concurrency-slot block (and its holder
	// entries) carried on Stage, RunStageWait, HostDispatchResult,
	// StageWaitStatus and AwaitStageOutput, exported for the SDK's jsonschema
	// reflection like every DTO.
	"ConcurrencyHolder",
	"Config",
	"ConsolidateResult",
	"ConsolidateSlicesInput",
	"ConsolidateSlicesOutput",
	// E77.3 / #3737: the three crew-message tools' I/O and the client mirrors
	// of /v0/crew-messages, exported for the SDK's jsonschema reflection like
	// every DTO.
	"CrewMessageAnchor",
	"CrewMessageAnswer",
	"CrewMessageRecord",
	"CrewMessageRecordOutput",
	"CrewMessageView",
	"CriteriaPrecheck",
	"CrossSliceClaim",
	"CrossSliceCouplingFinding",
	"DecideCrewEscalationInput",
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
	// E76.1 / #3747: the fishhawk_delegation tool I/O plus the client mirrors of
	// GET /v0/repos/{owner}/{name}/delegation. Exported for the SDK's jsonschema
	// reflection like every other tool DTO; DelegationOutput embeds
	// delegationview.View, whose whole type tree the SDK reflects.
	"DelegationInput",
	"DelegationOutput",
	"DiagnosticBundle",
	"DiagnosticComponent",
	"DiagnosticFailingStage",
	"DiagnosticSeqRange",
	"DiagnosticStageFact",
	"DiagnosticVersions",
	"DiagnosticWedgeContext",
	"DiffSummary",
	// E75.6 / #3734: fishhawk_digest's input, output and mark-read result
	// mirror, exported for the SDK's jsonschema reflection like every DTO.
	"DigestInput",
	"DigestMarkReadResult",
	"DigestOutput",
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
	// E68.31 / #3081: the additive fix-up-recovery marker nested on
	// StageWaitStatus. Exported for the same reason as the other nested response
	// types — the MCP SDK's jsonschema reflection needs the type AND its fields
	// exported to advertise the shape on every tool that carries the block.
	"FixupRecovery",
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
	// E54.77 / #3232: the typed grooming_apply_status block on
	// fishhawk_get_run_status, exported for the SDK's jsonschema reflection
	// like every DTO.
	"GroomingApplyStatus",
	// #2712: the decoded /healthz slice the restart-strand probe reads
	// (process_start + the sibling identity fields). Exported alongside the
	// other apiClient result types.
	"GroomingDispositionEntry",
	// E76.4 / #3767: fishhawk_handover_brief's input and output, exported for
	// the SDK's jsonschema reflection like every DTO; HandoverBriefOutput wraps
	// the shared handoverbrief.Brief wire model.
	"HandoverBriefInput",
	"HandoverBriefOutput",
	"HealthInfo",
	"HostDispatchResult",
	"InitInput",
	"InitOutput",
	"Instructions",
	// E54.7 / #2239: LOCAL decode-only mirrors of the backend's advisory
	// intake-groom payload on the work-item filing response. Exported
	// deliberately — they are reachable through FiledWorkItem.Intake, so a
	// consumer must be able to name them — and deliberately NOT workmgmt
	// types: ADR-064's board-read guard forbids workmgmt being reachable from
	// this package at all, so mirroring the shape locally is the only way to
	// surface intake without breaking that invariant.
	"IntakeCitation",
	"IntakeDuplicate",
	"IntakeEpicSuggestion",
	"IntakeScore",
	"IntakeSignals",
	// #3774: the decode-only mirror of intake.derives_from entries, local for
	// the same ADR-064 reason as the intake satellites above.
	"IntakeSourceItem",
	"IntegrateWaveResult",
	"IssueComment",
	"IssueContext",
	"LatencyGate",
	"ListAuditInput",
	"ListAuditOutput",
	"ListGroomingDispositionsOutput",
	"ListRunAuditFilter",
	"ListRunsInput",
	"ListRunsOutput",
	// E79.1 / #3725: fishhawk_list_schedules' input and output (the output IS
	// the client mirror of GET /v0/schedules) plus the two nested DTOs,
	// exported for the SDK's jsonschema reflection like every DTO.
	"ListSchedulesInput",
	"ListSchedulesOutput",
	"ListUpkeepDispositionsOutput",
	"ListScopeAmendmentsInput",
	"ListScopeAmendmentsOutput",
	// E45.88 / #3623: the client wire mirror of the `observation` object on
	// POST /v0/runs/{run_id}/record-merge-observation. Exported for the same
	// reason as the ReconcileReviews* / BulkWaive* sibling mirrors below — it is
	// reached through RecordMergeObservationResult and follows the convention
	// every recovery verb's mirrors already use.
	"MergeObservationFact",
	"MergeRunInput",
	"MergeRunOutput",
	"MergeRunResult",
	"NewServer",
	"NextActions",
	"OnboardingApp",
	// The merge-gate mirror of the readiness report's new merge_gate rung (#3161).
	"OnboardingMergeGate",
	"OnboardingMergeGateSrc",
	"OnboardingReadinessReport",
	"OnboardingReviewer",
	"OnboardingScopes",
	"OnboardingSpec",
	"PRBodyObligation",
	// #4068: get_plan's acceptance-criterion mirror (verification block).
	"PlanAcceptanceCriterion",
	"PlanApproachStep",
	"PlanContent",
	"PlanDecomposed",
	"PlanDecomposition",
	"PlanGeneratedBy",
	// #4068: get_plan's per-sub-plan model_recommendation mirror.
	"PlanModelRecommendation",
	// E78.4 / #3748: get_plan's new_architectural_decision DTO.
	"PlanNewArchitecturalDecision",
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
	// #3647: fishhawk_preview_campaign's tool input/output.
	// E75.3 / #3731: the fishhawk_precedent tool I/O plus the client mirrors of
	// GET /v0/precedent. Exported for the SDK's jsonschema reflection like every
	// other tool DTO; PrecedentOutput embeds PrecedentResolvedContext and
	// PrecedentDegraded, and the SDK reflects the whole tree.
	"PrecedentDegraded",
	"PrecedentInput",
	"PrecedentOutput",
	"PrecedentParams",
	"PrecedentResolvedContext",
	"PrecedentResult",
	"PreviewCampaignInput",
	"PreviewCampaignOutput",
	// #3774: fishhawk_preview_issue's tool I/O, exported for the SDK's
	// jsonschema reflection like every other tool DTO.
	"PreviewIssueInput",
	"PreviewIssueOutput",
	"ProductReport",
	"ReadCrewMessagesInput",
	"ReadCrewMessagesOutput",
	"ReapFailureResult",
	"ReapStageInput",
	"ReapStageOutput",
	// #2712: fishhawk_reconcile_reviews' tool I/O plus the apiClient result
	// E45.88 / #3623: the MCP half of the #3083/#3136 merge-recovery verb pair
	// (merge_recovery.go). The tool I/O types MUST be exported — the MCP SDK's
	// jsonschema reflection reads each tool's input/output type AND its nested
	// row types to build the schema, so unexporting them breaks registration;
	// the *Result client mirrors follow the sibling convention below.
	"ReconcileMergeInput",
	"ReconcileMergeOutput",
	"ReconcileMergeResult",
	"ReconcileMergeStage",
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
	"RecordGroomingDispositionsInput",
	"RecordGroomingDispositionsOutput",
	// E45.88 / #3623: the observe half of the merge-recovery pair. Same reason
	// as the ReconcileMerge* block above — tool I/O and its nested observation
	// row must be exported for the SDK to advertise the schema.
	"RecordMergeObservationInput",
	"RecordMergeObservationObservation",
	"RecordMergeObservationOutput",
	"RecordMergeObservationResult",
	"RecordUpkeepDispositionsInput",
	"RecordUpkeepDispositionsOutput",
	"RecordedGroomingDisposition",
	"RecordedUpkeepDisposition",
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
	// E76.1 / #3747: the client mirrors of GET
	// /v0/repos/{owner}/{name}/delegation. RepoDelegationResult is an ALIAS of
	// delegationview.View — the backend's own projection type — so the two sides
	// cannot drift into two json-tag sets.
	"RepoDelegationParams",
	"RepoDelegationResult",
	// E64.23 / #3125: the fishhawk_rebase_run_branch surface. All three MUST
	// be exported — the MCP SDK reflects over the tool input/output structs to
	// build the wire schemas, so unexported types are not an option.
	"RebaseBranchResult",
	"RebaseRunBranchInput",
	"RebaseRunBranchOutput",
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
	"ScheduleEntry",
	"ScheduleOutcome",
	"ScopeAmendmentItem",
	"ScopeAmendmentPath",
	// #2591: the {path, operation} entry an `amend` scope-completeness
	// decision folds into the parked stage's scope. An ALIAS of
	// ScopeAmendmentPath (the amend channel IS the #961 channel), exported for
	// the same jsonschema-reflection reason as its neighbours.
	"ScopeAmendmentPathEntry",
	"ScopeCompletenessDecisionResult",
	// #2591: the {path, reason} entry naming a path whose owning slice the
	// backend's amend guard could not establish. Nested in
	// ScopeCompletenessDecisionResult, so exported for the same
	// jsonschema-reflection reason as its neighbours.
	"ScopeCompletenessUnresolvedOwner",
	"ScopePrecheck",
	"ScopePrecheckViolation",
	"SecurityFinding",
	"SendCrewMessageInput",
	"SessionGuidance",
	"Stage",
	"StageConcurrency",
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
	// E45.88 / #3623: the client wire mirror of one `superseded`/`repaired` row
	// on POST /v0/runs/{run_id}/reconcile-merge, reached through
	// ReconcileMergeResult. Same sibling-mirror convention as MergeObservationFact.
	"SupersededStageRow",
	"SurfaceSweep",
	"SurfaceSweepFinding",
	"TestSweep",
	"TestSweepFinding",
	// #3923: fishhawk_record_upkeep_dispositions' I/O structs and the nested
	// per-finding entry (UpkeepDispositionEntry), plus the ListUpkeep*/
	// RecordUpkeep*/RecordedUpkeep* names above. Exported for the same
	// SDK-reflection reason as the grooming-dispositions sibling.
	"UpkeepDispositionEntry",
	// #4016: fishhawk_record_comms_dispositions' I/O structs and the nested
	// view types its output carries (the per-draft entry, the recorded
	// disposition, the recorded preview and its error, the cluster split and
	// placement), plus ListCommsDispositionsOutput. Exported for the same
	// SDK-reflection reason as the upkeep- and grooming-dispositions siblings.
	"CommsClusterPlacement",
	"CommsClusterSplit",
	"CommsDispositionEntry",
	"CommsDraftPreviewError",
	"CommsDraftPreviewRecord",
	"ListCommsDispositionsOutput",
	"RecordCommsDispositionsInput",
	"RecordCommsDispositionsOutput",
	"RecordedCommsDisposition",
	// E45.65 / #3579: the fishhawk_validate tool's I/O structs. Exported for
	// the same SDK-reflection reason as every other tool I/O type here.
	"ValidateSpecDiagnostic",
	"ValidateSpecInput",
	"ValidateSpecOutput",
	"VerifyRunInput",
	"VerifyRunOutput",
	"VouchCommitInput",
	"VouchCommitOutput",
	"VouchCommitResult",
	"WaiveConcernInput",
	"WaiveConcernOutput",
	// E64.77 / #3318: the BULK waive tool's I/O structs
	// (fishhawk_waive_concerns). Exported for the same SDK-reflection reason
	// as every other tool I/O type in this baseline.
	"WaiveConcernsInput",
	"WaiveConcernsOutput",
	"WaivedConcern",
	// #3774: the decode-only mirror of POST /v0/work-items/preview's 200,
	// embedded in PreviewIssueOutput (local for the ADR-064 reason above).
	"WorkItemPreview",
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
		// The count is ALSO quoted in prose. Name that site here so a drift is
		// fixed everywhere in one pass rather than leaving a stale figure
		// standing where nothing will ever fail on it (#3013).
		t.Errorf("exported identifier count = %d, want %d — if this is an intentional export change, also update the figures in README.md \"Exported surface: why N identifiers, not 3\" and the count in this file's exportBaseline doc comment",
			len(got), len(exportBaseline))
	}
}

// TestEntryPointsExist asserts the three intended entry points are present
// and Config carries exactly the four documented exported fields — a fifth
// exported field on Config, or a dropped one, fails it. Config/NewServer/
// Instructions are the only surface #2390 consumes; this guards their shape
// independently of the bulk baseline. HTTPTransport joined {APIToken,
// BackendURL} with #2479 (the transport-conditional working_dir refusal), and
// AllowedRoots joined them with E66.63 / #3589 (the HTTP path-confinement
// allow-list), so the baseline is updated to the new EXACT four-field set —
// NOT loosened to a subset/contains check: a new exported field on Config must
// stay a conscious act.
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

	// Config's exported fields must be exactly {APIToken, AllowedRoots,
	// BackendURL, HTTPTransport} — an EXACT-SET check (not subset/contains), so
	// both a new leaked field and a dropped one fail.
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
	want := []string{"APIToken", "AllowedRoots", "BackendURL", "HTTPTransport"}
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
