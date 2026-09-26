package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// maxAcceptanceBundleBytes caps the acceptance evidence request body at
// 256 KiB (E72.11 / #3447). Per ADR-049 decision refinement #5 the evidence
// blobs (logs, screenshots, traces) stay customer-side — only the structured
// verdict + per-criterion results + content_hash references to those blobs
// cross to Fishhawk — but the original 32 KiB figure predates the E72.4
// runner-injected replay block (one entry per replayed scenario) and the
// per-criterion prose fields (observed / steps_taken / expectation_basis),
// which together crossed 32 KiB on an ordinary 20-criteria run and stranded a
// PASSED verdict as settled-outcome-unknown. 256 KiB is the same plain-const
// bound the sibling transcript endpoint uses (#3106 rule: a plain const, never
// an env knob) and lands a 25-criteria × ~2 KiB-evidence verdict plus a
// 25-scenario replay block with headroom. MIRRORED by the runner as
// upload.MaxAcceptanceVerdictBytes (runner/internal/upload), which bounds the
// verdict client-side BEFORE shipping; keep the two in lockstep — a skew in
// either direction is absorbed at runtime by the runner re-bounding once to
// the details.limit_bytes this handler declares on its 413.
const maxAcceptanceBundleBytes = 256 * 1024

// Acceptance audit categories (E31.6 / #1534, ADR-049). Open-set strings —
// audit_entries.category has no CHECK, so these need no migration (only the
// artifacts kind CHECK was widened, by 0045). Kept in lockstep with the kinds
// issuecomment/status_template.go already renders (acceptance_dispatched /
// acceptance_outcome_recorded / acceptance_triage_decided, E31.3).
const (
	// CategoryAcceptanceDispatched records that an acceptance stage was
	// DISPATCHED. TWO emit sites since E64.53 / #3174, split by how the stage is
	// actually spawned: orchestrator.emitAcceptanceDispatched for a
	// backend-triggered (github_actions) dispatch, and this package's
	// host_dispatch.go::emitHostDispatchAcceptanceAnchor for a LOCAL host spawn
	// (marked at the host-dispatch endpoint, which is where the spawn actually
	// happens — the orchestrator only PARKS a local stage). Neither is the
	// acceptance-outcome handler below. The constant lives here so the outcome
	// and the dispatch categories are defined together, and it is what
	// latestAcceptanceDispatchSeq reads to anchor the validated head.
	CategoryAcceptanceDispatched = "acceptance_dispatched"
	// CategoryAcceptanceSkippedOutOfScope records that the orchestrator
	// AUTO-TERMINATED an acceptance stage (E38.3 / #1657) because the approved
	// plan declared verification.out_of_scope with zero acceptance_criteria —
	// there is no observable criterion for a validator to check. EMITTED by the
	// orchestrator (emitAcceptanceSkippedOutOfScope), which uses the raw string
	// literal at its emit site (matching the acceptance_dispatched convention);
	// this exported const is the single owner of the value. READ by auditcomplete
	// (which exempts the marked stage from the trace-required rule) and by the MCP
	// next_actions surface (which labels the succeeded_acceptance_skipped_out_of_scope
	// state). Keep the literal and this const byte-identical. Open-set string —
	// audit_entries.category has no CHECK, so no migration.
	CategoryAcceptanceSkippedOutOfScope = "acceptance_skipped_out_of_scope"
	// CategoryAcceptanceOutcomeRecorded records the persisted acceptance
	// artifact + its settled verdict. Written by handleShipAcceptance on every
	// successful artifact persist.
	CategoryAcceptanceOutcomeRecorded = "acceptance_outcome_recorded"
	// CategoryAcceptanceScenarioRegression records ONE failed replayed-scenario
	// row (E72.4 / #3328): a prior run's persisted scenario the acceptance agent
	// replayed against this head and observed to FAIL. Written by
	// handleShipAcceptance on the fresh-create path, one entry per failed
	// `scenario:`-prefixed row, with origin attribution taken ONLY from the
	// runner-injected body.replay.scenarios entry — never from agent prose and
	// never from the trigger issue.
	CategoryAcceptanceScenarioRegression = "acceptance_scenario_regression"
	// CategoryAcceptanceScenariosPushed records the acceptance runner's
	// post-verdict scenario-corpus commit (E72.4 / #3328): the run-authored
	// commit that persisted new scenarios and/or merged served retirements into
	// acceptance/scenarios/retired.yaml on the run branch. It is a reported-head
	// ledger category (auditcomplete.HeadReportCategoriesByPrecedence and
	// lineageLedgerCategories both admit it) so the scenario commit attributes
	// as run lineage without re-opening the settled acceptance verdict.
	CategoryAcceptanceScenariosPushed = "acceptance_scenarios_pushed"
	// CategoryAcceptanceScenarioRetirementDropped records that an APPROVED
	// scenario retirement served to the acceptance runner was NOT persisted to
	// the ledger (E72.4 / #3328): the runner exited after the prompt fetch on a
	// path that pushed no scenario commit. Audit-only plus a status-comment
	// refresh; it transitions nothing.
	CategoryAcceptanceScenarioRetirementDropped = "acceptance_scenario_retirement_dropped"
	// CategoryAcceptanceTriageDecided records the deterministic triage of a
	// failed acceptance verdict (E31.8 / #1536, ADR-049 decision #2). One
	// chained entry per triage, written AFTER acting so the disposition records
	// what actually happened. The class/disposition/criterion_ids payload tags
	// match the render contract issuecomment/status_template.go already ships
	// (renderAcceptanceTriageLine, E31.3) and the class-3 entry keyed by
	// criterion_ids is the durable per-criterion disposition record E31.11
	// consumes. Open-set string — audit_entries.category has no CHECK, so no
	// migration (same posture as acceptance_outcome_recorded).
	CategoryAcceptanceTriageDecided = "acceptance_triage_decided"
	// CategoryAcceptanceReopened records an operator-gated re-open of an
	// acceptance stage that settled `succeeded` with NO
	// acceptance_outcome_recorded verdict for that stage (E31.16 / #1567).
	// Written by the retry handler's acceptance-reopen branch
	// (server/retry.go) before the orchestrator handoff; no notifier ping of
	// its own — the status refresh rides notifyStatusUpdate. Open-set string
	// (audit_entries.category has no CHECK), so no migration.
	CategoryAcceptanceReopened = "acceptance_reopened"
	// CategoryAcceptanceVerdictUnshipped records that an acceptance stage
	// settled `succeeded` (its trace upload landed) but the runner's verdict
	// ship FAILED so NO acceptance_outcome_recorded entry exists for the
	// validation episode (E72.11 / #3447 — the 413 body_too_large strand).
	// Written by handleReapStageFailure's already-terminal branch
	// (server/reap_failure.go) when the detached reaper's report lands against
	// such a stage: one chained, stage-scoped entry carrying the reaper's
	// reason/detail/exit_code, no state transition, no orchestrator advance.
	// READ by acceptanceVerdictUnshippedLive, which treats the marker as LIVE
	// only while it is newer than the stage's latest dispatch/reopen anchor
	// AND newer than any stage-scoped acceptance_outcome_recorded entry — so a
	// retry re-open + re-dispatch retires it by construction. Same name as
	// drive.RuleAcceptanceVerdictUnshipped and the MCP next_actions state
	// (cross-module literal pins). Open-set string, no migration.
	CategoryAcceptanceVerdictUnshipped = "acceptance_verdict_unshipped"
)

// Acceptance triage class values (E31.8). Strings, matching
// renderAcceptanceTriageLine's "class-%s" contract:
//   - class 1: the code attempts the behavior and objectively fails
//     (failure_mode=error, or assertion_fail where every failed criterion is
//     explicit-source) → bounded fix-up pass.
//   - class 2: assertion_fail where no criterion failed but ≥1 was skipped —
//     validation could not complete (environment/flake) → re-open + re-run.
//   - class 3: a failed criterion is inferred-source or unresolvable against
//     the plan (bad/ambiguous criterion) → page the human, no transition.
//   - class 4: unitemized or provenance-ungroundable failure (works-as-planned,
//     disputed) → page the human, no transition.
//   - class 5: all-skip / externally-unvalidatable — every skipped criterion is
//     a posture-A can't-exhibit skip carrying expectation_basis; the trigger
//     requires an external event the default-deny egress sandbox cannot produce,
//     so retry is deterministically futile → terminal page, no state transition
//     (split off from class 2 so the acceptance stage stays succeeded/terminal
//     and fishhawk_audit_complete can clear; #1671).
const (
	acceptanceClass1 = "1"
	acceptanceClass2 = "2"
	acceptanceClass3 = "3"
	acceptanceClass4 = "4"
	acceptanceClass5 = "5"
)

// Acceptance triage disposition vocabulary (E31.8). The tags
// decodeAcceptanceActivity reads for the issue-comment render, and the tokens
// issuecomment/ping.go's page-class gate keys the must_page_human ping on.
const (
	acceptanceDispositionFixupDispatched  = "fixup_dispatched"
	acceptanceDispositionRetryDispatched  = "retry_dispatched"
	acceptanceDispositionPaged            = "paged"
	acceptanceDispositionRerunBudget      = "rerun_budget_exhausted"
	acceptanceDispositionFixupUnavailable = "fixup_unavailable_paged"
	acceptanceDispositionRetryUnavailable = "retry_unavailable_paged"
	acceptanceDispositionUnsettled        = "unsettled_paged"
	// acceptanceDispositionUnvalidatable is the terminal, non-re-opening paged
	// disposition for a class-5 all-skip externally-unvalidatable verdict
	// (#1671): the acceptance stage stays succeeded so fishhawk_audit_complete
	// clears and the operator arbitrates via the normal gate. Re-declared
	// verbatim in backend/internal/issuecomment/ping.go (string literal) and
	// backend/cmd/fishhawk-mcp/next_actions.go (const) — the three copies are
	// pinned byte-for-byte by a per-package assertion.
	acceptanceDispositionUnvalidatable = "externally_unvalidatable_paged"
)

// defaultMaxAcceptanceReruns bounds the number of auto-routed acceptance
// triage decisions (fixup_dispatched | retry_dispatched) per run before the
// disposition degrades to rerun_budget_exhausted (paged, no action) so
// non-convergence always lands on the human. Package const, no new env var:
// #1536 bounds re-runs at 1–2.
const defaultMaxAcceptanceReruns = 2

// acceptanceTriageSystemSubject is the token-less system identity the class-1
// fix-up routes under: non-anonymous with TokenID=="" passes
// identityHasGateScope (the shape fixupStageAs admits for in-process callers).
const acceptanceTriageSystemSubject = "system:acceptance-triage"

// Acceptance verdict + failure-mode values. Server-local open-set strings
// (like the deploy audit categories) — the audit category has no DB CHECK and
// E31.8 triage consumes failure_mode from this package. verdict is the
// pass/fail axis; failure_mode splits a failure into error (crash/500/
// exception) vs assertion_fail (behaved-but-unexpected) for E31.8 triage.
const (
	acceptanceVerdictPassed = "passed"
	acceptanceVerdictFailed = "failed"
	// acceptanceVerdictNotValidated is the SERVER-INTERNAL-ONLY third verdict
	// (#2347) the orchestrator's pre-spawn short-circuit records for a stage that
	// verified ZERO criteria. It is pinned to the plan-package constant so the
	// producer and this gate cannot drift. It is deliberately NOT admissible on
	// the wire — acceptanceBody.validate still rejects it — so it can only ever
	// originate server-side.
	acceptanceVerdictNotValidated = plan.AcceptanceVerdictNotValidated
	// acceptanceVerdictUndecidable is the SERVER-DERIVED fourth verdict (#2512,
	// E48.78 layer 4): the acceptance stage RAN, drove the preview, and reported
	// per-criterion rows of which at least one could not be DECIDED, with no row
	// failing. Pinned to the plan-package constant so producer and gate cannot
	// drift. Like not_validated it is deliberately NOT admissible on the wire —
	// acceptanceBody.validate still rejects it at the top level — so it can only
	// ever originate here, derived from the rows by aggregateAcceptanceResults.
	//
	// THE PARTITION (settled once, by construction rather than by convention):
	// the dispositions are decided by ONE total ladder over the row set
	// (acceptanceVerdictSeverity: passed < not_validated < undecidable < failed).
	// not_validated is reached PRE-SPAWN from the plan (no rows) OR POST-RUN when
	// every non-retired row is skipped / no rows shipped (#3397); undecidable is
	// reached POST-RUN when ≥1 row is undecidable and none failed; failed outranks
	// both. Because one ladder decides, the recorded verdicts stay mutually
	// exclusive even though not_validated can now carry all-skipped rows.
	acceptanceVerdictUndecidable = plan.AcceptanceVerdictUndecidable

	// acceptanceUndecidableBasisHeadUnresolved is the acceptance_outcome_recorded
	// `undecidable_basis` value the #3091 unbound-head clamp records: the stage's
	// validated head could not be resolved, so the verdict names no tree and a
	// `passed` is laddered to undecidable. Deliberately a SEPARATE key from the
	// #2581 `downgrade_basis`, which keeps its retirement meaning.
	acceptanceUndecidableBasisHeadUnresolved = "head_unresolved"

	acceptanceFailureError         = "error"
	acceptanceFailureAssertionFail = "assertion_fail"

	acceptanceResultPassed  = "passed"
	acceptanceResultFailed  = "failed"
	acceptanceResultSkipped = "skipped"
	// acceptanceResultUndecidable is the PER-CRITERION result value — the only
	// one of the undecidable triple a wire producer may ship (#2512). Pinned to
	// the plan-package constant, and mirrored by the runner twin's string
	// literal, whose agreement the shared docs/spec/acceptance-verdict-fixtures.json
	// corpus pins.
	acceptanceResultUndecidable = plan.AcceptanceResultUndecidable
)

// acceptanceCriterionResult is one per-criterion evidence entry. ID is the
// plan-criterion join key (E31.1); Result is the pass/fail/skip disposition.
// The optional prose fields carry the validator's observed behavior.
type acceptanceCriterionResult struct {
	ID     string `json:"id"`
	Result string `json:"result"`
	// UndecidableReason names WHAT the validator could not determine and WHY
	// (#2512). REQUIRED and a non-whitespace JSON string when Result is
	// undecidable; REJECTED whenever PRESENT on every other result.
	//
	// It is a json.RawMessage so PRESENCE is decided on the raw bytes (the
	// #2699 pattern, validateReapExpectedState): absent = nil raw; every PRESENT
	// value — a string, the empty string, an explicit JSON `null`, a number, an
	// object — keeps its raw bytes. A *string (the previous type) collapses an
	// explicit `null` onto nil, so `{"result":"passed","undecidable_reason":null}`
	// was admitted as if the key were absent, bypassing the presence rule
	// (#2787); a plain string would likewise admit `""`. Absent, null and empty
	// are THREE states, and only absent is admitted on a non-undecidable row.
	// decodeUndecidableReason is the single reader; the runner twin carries a
	// byte-identical copy, and the shared corpus pins their agreement.
	UndecidableReason json.RawMessage `json:"undecidable_reason,omitempty"`
	Observed          string          `json:"observed,omitempty"`
	Expected          string          `json:"expected,omitempty"`
	StepsTaken        string          `json:"steps_taken,omitempty"`
	// ExpectationBasis cites where the expectation came from (the criterion's
	// statement, the issue text, a spec section) so a failed assertion is
	// auditable against its source. Optional (E31.7 verdict shape, #1535).
	ExpectationBasis string `json:"expectation_basis,omitempty"`
	// ReproHandle is a re-run pointer for the observation — the command,
	// request, or script the validator used — so a human can reproduce the
	// evidence. Optional (E31.7 verdict shape, #1535).
	ReproHandle string `json:"repro_handle,omitempty"`
}

// acceptanceBody is the wire shape the acceptance validator (E31.7 runner) or
// an operator POSTs. It carries ADR-049's structured acceptance evidence.
// Stored verbatim as the artifact's content; v0 carries no schema_version
// because the field shape isn't yet schema-stable (mirroring deploymentBody).
type acceptanceBody struct {
	// Verdict is the settled disposition: passed | failed. Required.
	Verdict string `json:"verdict"`
	// FailureMode splits a failure for E31.8 triage: error (crash/500/
	// exception) | assertion_fail (behaved-but-unexpected). Required iff
	// verdict==failed; rejected when present on a pass.
	FailureMode string `json:"failure_mode,omitempty"`
	// Criteria carries one result per plan acceptance criterion, keyed by the
	// criterion id (the E31.1 join key). Optional — a verdict can settle
	// before per-criterion evidence is itemized. Decoded as a RawMessage so
	// validate() can losslessly coerce the historical object-keyed variant (a
	// JSON object keyed by criterion id) to the schema-required flat array with
	// each key folded into the element id (the #1574 class); see
	// coerceAcceptanceCriteria. The normalized flat slice lands in
	// normalizedCriteria. Moving off the typed slice means the top-level
	// DisallowUnknownFields decoder no longer descends into criteria elements —
	// coerceAcceptanceCriteria's array path re-applies that strictness.
	Criteria json.RawMessage `json:"criteria,omitempty"`
	// TargetURL is the running instance the validator drove, when declared.
	// Optional; a schemeless host[:port] is coerced to an http:// URL by
	// validate(), and any foreign scheme fails closed (the #1574 class).
	TargetURL string `json:"target_url,omitempty"`
	// EvidenceHashes references the customer-side evidence blobs by content
	// hash (ADR-049 #5 default residency customer-side). Optional. Decoded as
	// a RawMessage so validate() can losslessly coerce the historical
	// string-valued object-map variant to its sorted values (the #1574 class);
	// see coerceEvidenceHashes. The normalized flat slice lands in
	// normalizedEvidenceHashes.
	EvidenceHashes json.RawMessage `json:"evidence_hashes,omitempty"`
	// Notes is a declared home for the agent's free-text overflow (#1567):
	// a benign top-level remark that would otherwise fail closed against
	// DisallowUnknownFields. Free text, no validate() rule; stored verbatim
	// in the artifact and covered by the existing whole-verdict redaction on
	// the runner side. The wire twin of acceptanceVerdict.Notes.
	Notes string `json:"notes,omitempty"`
	// Replay is the RUNNER-INJECTED replay evidence (E72.4 / #3328): the cap
	// header computed once at the sampling site plus one attribution entry per
	// served scenario, built from the loaded corpus files. The runner injects
	// it AFTER validating the agent's verdict against the closed agent-facing
	// schema (upload.InjectReplay), so an agent-authored `replay` never reaches
	// this decoder. Optional: absent on every pre-corpus runner and on a
	// replay-disabled stage, where the recorded outcome carries replay:null.
	Replay *acceptanceReplay `json:"replay,omitempty"`
	// Transcript is the RUNNER-INJECTED ref to the acceptance transcript
	// artifact (E72.5 / #3329) shipped to POST
	// /v0/runs/{run_id}/acceptance/transcript BEFORE this verdict. Like
	// Replay it is injected AFTER the agent's verdict validated against the
	// runner's closed agent-facing schema (upload.InjectTranscript), so an
	// agent-authored `transcript` never reaches this decoder. Optional: absent
	// on every pre-transcript runner and when the transcript ship failed
	// (best-effort — the verdict still ships), where the recorded outcome
	// carries transcript:null. handleShipAcceptance cross-checks the ref
	// against the stored artifact fail-closed (resolveAcceptanceTranscriptRef).
	Transcript *acceptanceTranscriptRef `json:"transcript,omitempty"`

	// scenarioRows is the `scenario:`-prefixed partition of the verdict rows
	// (E72.4 / #3328), split out of normalizedCriteria by validate() so every
	// criterion-keyed consumer (tally, downgrade, precedence ladder, triage,
	// concern synthesis) reads criterion rows ONLY and a replayed scenario can
	// never be mistaken for a plan criterion. Unexported, never marshalled.
	scenarioRows []acceptanceCriterionResult

	// normalizedEvidenceHashes is the coerced/validated flat slice, populated
	// by validate() from EvidenceHashes. Unexported (no json tag) so it never
	// marshals into the stored artifact; buildOutcomePayload records it as the
	// canonical shape.
	normalizedEvidenceHashes []string

	// normalizedCriteria is the coerced/validated flat typed slice, populated by
	// validate() from Criteria (an object-keyed criteria field folds into it,
	// each key written into the element id, sorted). Unexported (no json tag) so
	// it never marshals into the stored artifact; every downstream consumer
	// (tally, triage classifier, plan-review-miss + concern synthesis) reads it
	// instead of the raw Criteria field.
	normalizedCriteria []acceptanceCriterionResult
}

// acceptanceReplay is the backend twin of the runner's scenario.ReplaySet
// (E72.4 / #3328): the top-level `replay` object the acceptance runner injects
// into the validated verdict body. cap / corpus_size / served / sampled_out /
// retired_excluded / seed are computed ONCE by scenario.Sample at the sampling
// site and copied VERBATIM onto acceptance_outcome_recorded.replay — the
// backend never reconstructs them.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/scenario.ReplaySet (runner/internal/scenario/scenario.go);
// the wirecontract manifest pins the pair ModeExact. A tag drift silently
// drops the cap evidence from the recorded outcome.
type acceptanceReplay struct {
	Cap             int                          `json:"cap"`
	CorpusSize      int                          `json:"corpus_size"`
	Served          int                          `json:"served"`
	SampledOut      int                          `json:"sampled_out"`
	RetiredExcluded int                          `json:"retired_excluded"`
	Seed            string                       `json:"seed"`
	Scenarios       []acceptanceReplayedScenario `json:"scenarios"`
}

// acceptanceReplayedScenario is the backend twin of the runner's
// scenario.ReplayedScenario: the per-served-scenario attribution built from
// the LOADED corpus file. It is the ONLY source of origin_* on an
// acceptance_scenario_regression entry; origin_pr 0 means the originating PR
// was unknown at record time and is recorded as origin_unresolved rather than
// substituted with the issue number.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/scenario.ReplayedScenario; the wirecontract manifest pins
// the pair ModeExact.
type acceptanceReplayedScenario struct {
	ScenarioID  string `json:"scenario_id"`
	OriginPR    int    `json:"origin_pr,omitempty"`
	OriginIssue int    `json:"origin_issue"`
	OriginRunID string `json:"origin_run_id"`
	Path        string `json:"path"`
}

// isScenarioRow reports whether a verdict row is a replayed-scenario row
// (E72.4): its id carries the runner's scenario.IDPrefix. The literal is
// mirrored here because the backend cannot import the runner module.
func isScenarioRow(id string) bool {
	return strings.HasPrefix(id, acceptanceScenarioIDPrefix)
}

// acceptanceScenarioIDPrefix mirrors runner/internal/scenario.IDPrefix.
const acceptanceScenarioIDPrefix = "scenario:"

// partitionScenarioRows splits rows into (criterion rows, scenario rows) by
// the scenario: prefix, preserving order within each partition.
func partitionScenarioRows(rows []acceptanceCriterionResult) (criteria, scenarios []acceptanceCriterionResult) {
	for _, r := range rows {
		if isScenarioRow(r.ID) {
			scenarios = append(scenarios, r)
			continue
		}
		criteria = append(criteria, r)
	}
	return criteria, scenarios
}

// validate returns a human-readable error if any field is missing or
// malformed. An acceptance record is the governance trail of an independent
// validation, so a 400 here means the producer shipped the wrong shape.
//
// It also applies the two lossless coercions of the #1574 class BEFORE the
// fail-closed rejections, mutating the receiver so the recorded outcome uses
// the normalized shape: a string-valued object-map evidence_hashes collapses
// to its sorted values, and a schemeless host[:port] target_url gains an
// http:// prefix. Anything lossy (a non-string/nested map value, a scalar
// evidence_hashes, or a foreign target_url scheme) still fails closed. The
// coercion twin lives in the runner (validateAcceptanceVerdict) — the two
// must stay behavior-identical or a runner-accepted verdict could be
// backend-rejected on ship. logger (nil-tolerant) receives a WARN per
// coercion so the shape drift is observable.
func (a *acceptanceBody) validate(ctx context.Context, logger *slog.Logger) error {
	switch a.Verdict {
	case acceptanceVerdictPassed:
		if a.FailureMode != "" {
			return fmt.Errorf("failure_mode must be omitted on a passed verdict, got %q", a.FailureMode)
		}
	case acceptanceVerdictFailed:
		switch a.FailureMode {
		case acceptanceFailureError, acceptanceFailureAssertionFail:
			// ok
		case "":
			return errors.New("failure_mode is required when verdict is failed (error | assertion_fail)")
		default:
			return fmt.Errorf("failure_mode must be error or assertion_fail, got %q", a.FailureMode)
		}
	case "":
		return errors.New("verdict is required")
	default:
		// INVARIANT (#2347): this switch is deliberately INCOMPLETE with respect
		// to the acceptanceVerdict* constant set. acceptanceVerdictNotValidated is
		// SERVER-INTERNAL ONLY — it is recorded exclusively by the orchestrator's
		// pre-spawn short-circuit, which never travels through this endpoint. A
		// wire producer that ships it must be rejected here, so do NOT 'complete'
		// the enum by adding a not_validated case: doing so would let a validator
		// forge a zero-criteria-verified outcome and walk it past the merge gate.
		//
		// #2512 extends the SAME invariant to acceptanceVerdictUndecidable: it is
		// likewise SERVER-DERIVED ONLY, computed here from the per-criterion rows
		// by aggregateAcceptanceResults. A producer expresses undecidability on a
		// criterion ROW (result=undecidable + undecidable_reason), never at the
		// top level. Do NOT add an undecidable case either — that would let a
		// validator forge a merge-eligible non-pass over evidence it never
		// itemized, which is exactly the forgery not_validated is closed against.
		return fmt.Errorf("verdict must be passed or failed, got %q", a.Verdict)
	}
	// Coerce criteria before the per-criterion fail-closed checks: an object
	// keyed by criterion id folds into the schema-required flat array with each
	// key written into the element id (the #1574 class); a non-object keyed
	// value, a key/element-id conflict, or a scalar fails closed. A flat array
	// passes through (strict-decoded so an unknown element field still fails
	// closed). The normalized slice is what every downstream consumer reads.
	criteria, coercedCriteria, err := coerceAcceptanceCriteria(a.Criteria)
	if err != nil {
		return err
	}
	a.normalizedCriteria = criteria
	for i, c := range criteria {
		if c.ID == "" {
			return fmt.Errorf("criteria[%d].id is required (the plan-criterion join key)", i)
		}
		switch c.Result {
		case acceptanceResultUndecidable:
			// #2512: a criterion the validator attempted but genuinely could not
			// decide. The reason is REQUIRED and must carry non-whitespace text —
			// an undecidable row with no reason is indistinguishable from a
			// silently dropped criterion. TrimSpace, not != "", is the twin rule:
			// a whitespace-only reason is the named divergence risk between this
			// validator and the runner's, pinned by the shared corpus row
			// undecidable-row-reason-whitespace-only.
			present, text, derr := decodeUndecidableReason(c.UndecidableReason)
			if !present {
				return fmt.Errorf(
					"criteria[%d].undecidable_reason is required and must be non-whitespace when result is undecidable", i)
			}
			// #2787: the raw bytes must be a JSON STRING. An explicit `null` and
			// every non-string shape (a number, an object) reject here, on a
			// message distinct from the absent branch — `null` is never read as
			// the four-character text 'null'.
			if derr != nil {
				return fmt.Errorf(
					"criteria[%d].undecidable_reason must be a JSON string when result is undecidable, got %s",
					i, string(bytes.TrimSpace(c.UndecidableReason)))
			}
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf(
					"criteria[%d].undecidable_reason is required and must be non-whitespace when result is undecidable", i)
			}
		case acceptanceResultPassed, acceptanceResultFailed, acceptanceResultSkipped:
			// The field is rejected on PRESENCE, not on emptiness or value
			// (binding condition 1, #2787): `"undecidable_reason": ""` and
			// `"undecidable_reason": null` on a passed row are producer errors
			// exactly as a non-empty one is — the shapes a plain-string and a
			// *string decode respectively would silently admit.
			if present, _, _ := decodeUndecidableReason(c.UndecidableReason); present {
				return fmt.Errorf(
					"criteria[%d].undecidable_reason must be omitted when result is %q", i, c.Result)
			}
		default:
			return fmt.Errorf("criteria[%d].result must be passed/failed/skipped/undecidable, got %q", i, c.Result)
		}
	}
	if coercedCriteria && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced object-keyed criteria to a flat array",
			slog.Int("count", len(criteria)))
	}
	// Coerce evidence_hashes before the fail-closed reject: a string-valued
	// object map collapses to its sorted values (lossless); a non-string/
	// nested value or a scalar fails closed. The normalized slice is what the
	// outcome payload records.
	hashes, coercedHashes, err := coerceEvidenceHashes(a.EvidenceHashes)
	if err != nil {
		return err
	}
	a.normalizedEvidenceHashes = hashes
	if coercedHashes && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced string-valued object-map evidence_hashes to sorted values",
			slog.Int("count", len(hashes)))
	}

	// Coerce target_url before the fail-closed reject: a schemeless host[:port]
	// gains an http:// prefix; a foreign scheme (anything with "://" that is
	// not exactly http:// or https://) fails closed.
	coercedURL, err := coerceAcceptanceTargetURL(&a.TargetURL)
	if err != nil {
		return err
	}
	if coercedURL && logger != nil {
		logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance verdict: coerced schemeless target_url to an http:// URL",
			slog.String("target_url", a.TargetURL))
	}
	// Scenario-row partition (E72.4 / #3328): replayed-scenario rows leave the
	// criterion slice HERE so no downstream criterion consumer ever sees one.
	a.normalizedCriteria, a.scenarioRows = partitionScenarioRows(a.normalizedCriteria)
	return nil
}

// coerceEvidenceHashes normalizes the acceptance verdict's evidence_hashes
// field. It returns the flat slice of hash strings, whether a coercion
// occurred, and an error on any lossy shape. The accepted inputs:
//   - absent / null / empty → nil, no coercion.
//   - a JSON array of strings → the array verbatim, no coercion (a non-string
//     element fails closed, matching the strict prior decode).
//   - a string-valued JSON object map (the #1574 variant) → its values,
//     SORTED, marked coerced; a non-string or nested value fails closed.
//   - anything else (a scalar) → fails closed.
func coerceEvidenceHashes(raw json.RawMessage) ([]string, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false, nil
	}
	switch trimmed[0] {
	case '[':
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, false, fmt.Errorf("evidence_hashes must be a flat array of strings: %w", err)
		}
		return arr, false, nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, false, fmt.Errorf("evidence_hashes object could not be decoded: %w", err)
		}
		vals := make([]string, 0, len(m))
		for _, rv := range m {
			var s string
			if err := json.Unmarshal(rv, &s); err != nil {
				return nil, false, errors.New("evidence_hashes object-map values must all be strings (lossy coercion refused)")
			}
			vals = append(vals, s)
		}
		sort.Strings(vals)
		return vals, true, nil
	default:
		return nil, false, errors.New("evidence_hashes must be a flat array of strings or a string-valued object map")
	}
}

// decodeUndecidableReason reads a criterion row's undecidable_reason from its
// raw bytes (#2787, the #2699 pattern). present is false ONLY when the field
// was absent from the wire (nil / zero-length raw); every present value —
// including an explicit JSON `null` — reports present=true. When present, the
// bytes must decode to a JSON string: text carries it and err is nil;
// otherwise err names the shape (a `null` decodes to a nil string pointer and
// is rejected explicitly, so it is never reported as the text 'null'). The
// runner twin carries a byte-identical copy; the shared corpus pins agreement.
func decodeUndecidableReason(raw json.RawMessage) (present bool, text string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false, "", nil
	}
	var decoded *string
	if uerr := json.Unmarshal(trimmed, &decoded); uerr != nil {
		return true, "", fmt.Errorf("undecidable_reason must be a JSON string: %w", uerr)
	}
	if decoded == nil {
		return true, "", errors.New("undecidable_reason must be a JSON string, got null")
	}
	return true, *decoded, nil
}

// coerceAcceptanceCriteria normalizes the acceptance verdict's criteria field.
// It returns the flat typed slice, whether a coercion occurred, and an error on
// any lossy or invalid shape. The twin of the runner coerceAcceptanceCriteria
// (runner/cmd/fishhawk-runner/acceptance.go) — the two must stay identical or a
// runner-accepted verdict could be backend-rejected on ship. Accepted:
//   - absent / null / empty → nil, no coercion.
//   - a JSON array → STRICT-decoded (DisallowUnknownFields) into the typed slice
//     verbatim, no coercion. The strict decode re-applies the unknown-field
//     rejection the top-level decoder no longer performs on this now-RawMessage
//     field (an unknown element field fails closed).
//   - a JSON object keyed by criterion id (the #1574 variant) → each value
//     strict-decoded into an element with the object key folded into its id,
//     the elements SORTED by id, marked coerced. A value that is not an object,
//     or a value carrying a non-empty explicit id that conflicts with its key,
//     fails closed.
//   - anything else (a scalar) → fails closed.
func coerceAcceptanceCriteria(raw json.RawMessage) ([]acceptanceCriterionResult, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, false, nil
	}
	switch trimmed[0] {
	case '[':
		arr, err := strictDecodeCriteriaArray(trimmed)
		if err != nil {
			return nil, false, err
		}
		return arr, false, nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, false, fmt.Errorf("criteria object could not be decoded: %w", err)
		}
		out := make([]acceptanceCriterionResult, 0, len(m))
		for key, rv := range m {
			vt := bytes.TrimSpace(rv)
			if len(vt) == 0 || vt[0] != '{' {
				return nil, false, fmt.Errorf("criteria object value for %q must be an object (lossy coercion refused)", key)
			}
			c, err := strictDecodeCriterion(vt)
			if err != nil {
				return nil, false, err
			}
			if c.ID != "" && c.ID != key {
				return nil, false, fmt.Errorf("criteria object key %q conflicts with element id %q", key, c.ID)
			}
			c.ID = key
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, true, nil
	default:
		return nil, false, errors.New("criteria must be a flat array or an object keyed by criterion id")
	}
}

// strictDecodeCriteriaArray decodes a criteria JSON array with
// DisallowUnknownFields so an unknown field inside an element fails closed —
// the strictness the top-level RawMessage field no longer enforces.
func strictDecodeCriteriaArray(raw json.RawMessage) ([]acceptanceCriterionResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var arr []acceptanceCriterionResult
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("criteria array could not be decoded: %w", err)
	}
	return arr, nil
}

// strictDecodeCriterion decodes a single criteria object value with
// DisallowUnknownFields (the object-keyed variant path).
func strictDecodeCriterion(raw json.RawMessage) (acceptanceCriterionResult, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c acceptanceCriterionResult
	if err := dec.Decode(&c); err != nil {
		return acceptanceCriterionResult{}, fmt.Errorf("criteria object value could not be decoded: %w", err)
	}
	return c, nil
}

// coerceAcceptanceTargetURL normalizes the verdict's target_url in place. A
// schemeless host[:port] gains an http:// prefix (coerced=true). A value
// already carrying an exact http:// or https:// prefix passes through
// unchanged. ANY other value containing "://" (a foreign or near-miss scheme
// such as ftp://, httpx://, or http+unix://) fails closed — the check matches
// ONLY the two exact prefixes, never HasPrefix("http"), so a scheme a naive
// prefix test would wrongly admit is rejected.
func coerceAcceptanceTargetURL(target *string) (bool, error) {
	v := *target
	if v == "" {
		return false, nil
	}
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return false, nil
	}
	if strings.Contains(v, "://") {
		return false, fmt.Errorf("target_url must be an http(s) URL when set, got %q", v)
	}
	*target = "http://" + v
	return true, nil
}

// acceptanceCriteriaTally returns the passed count and the total, used both
// for the audit payload (E31.8 carry-through) and the issue-comment render
// tally (criteria_passed / criteria_total).
func acceptanceCriteriaTally(criteria []acceptanceCriterionResult) (passed, failed, skipped, undecidable, total int) {
	for _, c := range criteria {
		switch c.Result {
		case acceptanceResultPassed:
			passed++
		case acceptanceResultFailed:
			failed++
		case acceptanceResultSkipped:
			skipped++
		case acceptanceResultUndecidable:
			undecidable++
		}
	}
	return passed, failed, skipped, undecidable, len(criteria)
}

// aggregateAcceptanceResults is the TOTAL precedence ladder over per-criterion
// rows: any failed row -> failed (#2512); else any undecidable row ->
// undecidable, INCLUDING when EVERY row is undecidable (#2512); else if EVERY
// row is skipped (a non-empty set with zero passed/failed/undecidable) ->
// not_validated, because a validator that ran and skipped every criterion
// verified nothing (#3397); else passed.
//
// PRECONDITION (load-bearing, enforced at the single call site): callers MUST
// guard on len(rows) > 0. The function is total, so it answers an EMPTY row set
// with `passed` — and "no evidence at all means passed" is precisely the hazard
// #2512/#3397 exist to remove. The guard, not this function, is what keeps that
// answer unreachable; TestAggregateAcceptanceResults_EmptyRowSet pins the
// behaviour so a caller that drops the guard cannot claim surprise, and the
// empty-set door itself is closed at the seam (binding condition 1, #3397): the
// call site ladders an empty non-retired set against not_validated directly.
//
// A row set containing an undecidable row can NEVER aggregate to passed: a green
// light over an unevaluated criterion is the dangerous direction, because nobody
// looks behind it. The same reasoning extends to an all-skip set (#3397): a
// validator that skipped every criterion verified nothing, so recording `passed`
// would certify an absence of verification.
func aggregateAcceptanceResults(rows []acceptanceCriterionResult) string {
	sawUndecidable := false
	sawVerifying := false // any passed row — the only row that actually verifies
	for _, c := range rows {
		switch c.Result {
		case acceptanceResultFailed:
			return acceptanceVerdictFailed
		case acceptanceResultUndecidable:
			sawUndecidable = true
		case acceptanceResultPassed:
			sawVerifying = true
		}
	}
	if sawUndecidable {
		return acceptanceVerdictUndecidable
	}
	// #3397: a non-empty set with no failed/undecidable row and NO passed row is
	// all-skipped — the validator observed nothing. len(rows) > 0 is the caller's
	// precondition, so reaching here with !sawVerifying means every row is skipped.
	if !sawVerifying && len(rows) > 0 {
		return acceptanceVerdictNotValidated
	}
	return acceptanceVerdictPassed
}

// acceptanceDowngradeBasisAddedOnly is the downgrade_basis recorded when the
// #3181 added-only downgrade neutralizes a failed verdict.
const acceptanceDowngradeBasisAddedOnly = "added_criteria_only"

// acceptanceAddedOnlyDowngrade reports whether a FAILED verdict must be recorded
// as passed because every failure names a criterion the OPERATOR ADDED at the
// approval gate (#3181). An added criterion never passed plan review, so it is
// ADVISORY: it is driven and reported, but it cannot sink an otherwise-passing
// acceptance. This is the enforcement of that guard rail —
// aggregateAcceptanceResults returns failed on ANY failed row and never reads
// the plan's blocking flag, so the added criterion's Blocking:false is
// provenance only. It mirrors acceptanceDowngrade (#2581), keyed on the added-id
// set, under the same conjunctive preconditions:
//
//	A1 verdict==failed AND failure_mode==assertion_fail (an `error` is never
//	   downgraded).
//	A2 EVERY failed row names an added id — or a RETIRED id, so the two
//	   neutralizations compose on a verdict failing both kinds — and at least
//	   one failed row names an added id.
//	A3 at least one reported row survives: an id that is neither added nor
//	   retired. A verdict reporting only added/retired rows evidences nothing
//	   about the reviewed contract.
//	A4 no surviving BLOCKING criterion reported `undecidable`, and no surviving
//	   blocking criterion reported `skipped` UNLESS the reviewed plan itself
//	   declared that skip (see below). An id absent from the live set is
//	   treated as blocking (fail closed).
//
// BASIS-CARRYING SKIPS (approval condition 2, the run 0aad7486 shape): a
// surviving row reported `skipped` whose live criterion is skip_expected WITH a
// non-empty expectation_basis DOES count as a surviving non-added row for A3,
// and does NOT violate A4's "no blocking criterion skipped" conjunct. The skip
// is the contract plan review approved, not an un-exercised criterion, so a
// failed operator-added criterion cannot sink a run whose reviewed contract is
// all basis-carrying skips. The recorded verdict is still honest: the ladder
// then runs over the surviving (all-skipped) rows and records not_validated,
// never passed. A skip on a criterion that is NOT skip_expected-with-basis
// still blocks when the criterion is blocking, exactly as D4.
//
// Returns the added ids and the retired ids that were reported failed (effective
// order).
func acceptanceAddedOnlyDowngrade(acc acceptanceBody, eff effectiveAcceptanceCriteria) (bool, []string, []string) {
	// A1.
	if acc.Verdict != acceptanceVerdictFailed || acc.FailureMode != acceptanceFailureAssertionFail {
		return false, nil, nil
	}
	// An empty added set needs no early return: no failed row can then name an
	// added id, so the A3 len(failedAdded) check refuses it.
	added := eff.addedIDSet()
	retired := eff.retiredIDSet()
	live := make(map[string]plan.AcceptanceCriterion, len(eff.Live))
	for _, c := range eff.Live {
		live[c.ID] = c
	}

	failedAdded := map[string]struct{}{}
	failedRetired := map[string]struct{}{}
	surviving := 0
	for _, res := range acc.normalizedCriteria {
		_, isAdded := added[res.ID]
		_, isRetired := retired[res.ID]
		if !isAdded && !isRetired {
			surviving++
		}
		switch res.Result {
		case acceptanceResultFailed:
			// A2.
			switch {
			case isAdded:
				failedAdded[res.ID] = struct{}{}
			case isRetired:
				failedRetired[res.ID] = struct{}{}
			default:
				return false, nil, nil
			}
		case acceptanceResultSkipped, acceptanceResultUndecidable:
			// A4. Added and retired rows are advisory / superseded.
			if isAdded || isRetired {
				continue
			}
			c, ok := live[res.ID]
			if ok && c.Blocking != nil && !*c.Blocking {
				continue
			}
			if ok && res.Result == acceptanceResultSkipped && c.SkipExpected && strings.TrimSpace(c.ExpectationBasis) != "" {
				continue
			}
			return false, nil, nil
		}
	}
	// A3.
	if surviving == 0 || len(failedAdded) == 0 {
		return false, nil, nil
	}
	addedIDs := make([]string, 0, len(failedAdded))
	for _, id := range eff.Added {
		if _, ok := failedAdded[id]; ok {
			addedIDs = append(addedIDs, id)
		}
	}
	var retiredIDs []string
	for _, r := range eff.Retired {
		if _, ok := failedRetired[r.ID]; ok {
			retiredIDs = append(retiredIDs, r.ID)
		}
	}
	return true, addedIDs, retiredIDs
}

// acceptanceVerdictSeverity ranks the recordable dispositions on the total order
// passed < not_validated < undecidable < failed (binding condition 3, #3397).
// not_validated sits BELOW undecidable so an all-skip ship on an unbound head is
// clamped to undecidable(head_unresolved) — "we do not know which tree was
// validated" outranks "we verified nothing on a known tree". Anything
// unrecognized ranks lowest (with passed), so it can never dominate a real
// verdict.
func acceptanceVerdictSeverity(v string) int {
	switch v {
	case acceptanceVerdictFailed:
		return 3
	case acceptanceVerdictUndecidable:
		return 2
	case acceptanceVerdictNotValidated:
		return 1
	default:
		return 0
	}
}

// acceptanceVerdictAtLeast resolves the shipped-verdict/derived-verdict mismatch
// SEVERITY-MONOTONE: it returns max(a, b) on the total order passed <
// not_validated < undecidable < failed, so the recorded verdict is a LOWER BOUND
// on what either source claims and nothing is ever softened below either one.
//
// The mismatch cases fall out of the single rule: shipped passed with a failed
// row records failed; shipped passed with an undecidable row and no failed row
// records undecidable; shipped passed whose rows are all skipped (or empty)
// records not_validated (#3397); and a shipped failed deriving anything lower
// still records failed — so the class-5 all-skip-with-basis triage path is
// unchanged.
func acceptanceVerdictAtLeast(a, b string) string {
	if acceptanceVerdictSeverity(b) > acceptanceVerdictSeverity(a) {
		return b
	}
	return a
}

// acceptanceOutcomeLabel maps a verdict to the issue-comment render vocabulary
// (accepted | not_validated | rejected) — the `outcome` field
// issuecomment/status_template.go's renderAcceptanceOutcomeLine reads.
//
// not_validated (#2347 / #3397) is mapped explicitly rather than falling into
// the binary default: a stage that verified zero criteria is neither an
// acceptance nor a rejection, and rendering it as "rejected" would be as
// dishonest in the other direction as the "accepted" it replaces. It is
// server-derived only (pre-spawn short-circuit OR the ingest ladder over an
// all-skip / no-rows verdict) — the wire endpoint still admits passed/failed
// only, so a validator cannot ship it directly.
func acceptanceOutcomeLabel(verdict string) string {
	switch verdict {
	case acceptanceVerdictPassed:
		return "accepted"
	case acceptanceVerdictNotValidated:
		return plan.AcceptanceOutcomeNotValidated
	case acceptanceVerdictUndecidable:
		// #2512: same reasoning as not_validated, for the other half of the
		// partition. A run whose evidence could not decide a criterion is neither
		// an acceptance nor a rejection; rendering it "rejected" would send a
		// correct change into triage, and rendering it "accepted" would be a green
		// light over an unevaluated criterion.
		return plan.AcceptanceOutcomeUndecidable
	}
	return "rejected"
}

// handleShipAcceptance implements POST /v0/runs/{run_id}/acceptance?stage_id=...
//
// Records ADR-049's signed acceptance-evidence artifact and its governance
// trail. Modeled on handleShipDeployment: dual-auth (Ed25519
// X-Fishhawk-Signature runner path OR bearer token with write:runs scope),
// idempotent on (stage_id, content_hash), and chained-audit recording. It
// persists the acceptance artifact (artifact.KindAcceptance), writes an
// acceptance_outcome_recorded audit entry carrying the verdict + failure_mode
// (the E31.8 error-vs-assertion_fail carry-through), and refreshes the run's
// living-anchor comment. NO stage-state transition happens here: the stage
// settles through the ordinary agent trace-bundle path (E31.2 landed
// acceptance with no new states); failure routing/triage is E31.8's scope.
func (s *Server) handleShipAcceptance(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil ||
		s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_upload_unconfigured",
			"acceptance upload requires signing, artifact, audit, and run repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist",
			map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}
	// An acceptance evidence artifact is scoped to an acceptance stage (ADR-049
	// / #1531). Without this guard a valid run signer or write:runs bearer could
	// pin a signed acceptance record + acceptance audit chain onto a plan/
	// implement/review/deploy stage. Reject any non-acceptance stage before any
	// persistence, mirroring the deploy-stage guard.
	if stage.Type != run.StageTypeAcceptance {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"acceptance artifacts may only be attached to an acceptance stage",
			map[string]any{"stage_id": stageID.String(), "stage_type": string(stage.Type)})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAcceptanceBundleBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxAcceptanceBundleBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"acceptance body exceeds size cap",
			map[string]any{"limit_bytes": maxAcceptanceBundleBytes})
		return
	}

	authMethod, actorKind, actorSubject, ok := s.authorizeAcceptance(w, r, runID, body)
	if !ok {
		return
	}

	var acc acceptanceBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&acc); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}
	// Reject trailing data after the single acceptance object. Without this an
	// EOF-unverified Decode would accept the first object of a concatenated body
	// (e.g. {"verdict":"passed"}{"verdict":"failed",...}) while the stored
	// artifact bytes are not the single AcceptanceArtifactBody object documented.
	if dec.More() {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body must contain a single JSON object", nil)
		return
	}
	if err := acc.validate(r.Context(), s.cfg.Logger); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
			"acceptance body missing or malformed fields",
			map[string]any{"error": err.Error()})
		return
	}

	// Transcript ref cross-check (E72.5 / #3329), fail-closed and BEFORE any
	// persistence: a ref that does not resolve to THIS stage's stored
	// acceptance_transcript with the referenced hash is a malformed verdict
	// body (a runner defect or a forged ref) and rejects the ship outright.
	// The per-criterion summary is then derived from the STORED content —
	// never from anything the verdict body carried — and cross-checked
	// against the verdict's own rows; a disagreement suppresses the rendered
	// summary but NEVER changes the verdict (approval condition 1: the
	// verdict is authoritative, the transcript descriptive).
	var transcriptSummary *acceptanceTranscriptSummary
	if acc.Transcript != nil {
		art, terr := s.resolveAcceptanceTranscriptRef(r.Context(), stageID, *acc.Transcript)
		if terr != nil {
			s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
				"acceptance transcript ref does not resolve to this stage's stored transcript",
				map[string]any{"error": terr.Error()})
			return
		}
		sum, serr := buildAcceptanceTranscriptSummary(art, append(append([]acceptanceCriterionResult(nil), acc.normalizedCriteria...), acc.scenarioRows...))
		if serr != nil {
			s.writeError(w, r, http.StatusBadRequest, "acceptance_invalid",
				"stored acceptance transcript no longer validates",
				map[string]any{"error": serr.Error()})
			return
		}
		transcriptSummary = &sum
	}

	contentHash := sha256Hex(body)
	passed, failed, skipped, undecidable, total := acceptanceCriteriaTally(acc.normalizedCriteria)

	// Bind the verdict to the head the acceptance stage ACTUALLY validated
	// (#1682, binding condition 2): the run's newest recorded head at the
	// moment the stage was DISPATCHED, not the run's latest head at
	// verdict-record time. A post-dispatch fixup_pushed / child_pushed must NOT
	// re-bind this verdict — otherwise Option C's head comparison (retry.go)
	// would see recorded==current for a verdict the fixup already invalidated.
	// Empty ("", false) for a pre-anchor / dispatch-less ship — Option C then
	// fails closed to today's 422 for that entry.
	validatedHead, _ := s.acceptanceValidatedHeadSHA(r.Context(), runID, stageID)

	// Retired-criterion neutralization (#2581): a FAILED verdict whose every
	// failure names a criterion the operator retired at the approval gate is
	// recorded as passed, under the four conjunctive preconditions in
	// acceptanceDowngrade. The shipped ARTIFACT BYTES are never rewritten — only
	// the governance acceptance_outcome_recorded payload carries the effective
	// verdict — and the raw tallies stay the agent's evidence. Both degrade
	// branches (plan unreadable, effective-set unreadable) take NO downgrade: the
	// fail direction is always toward recording the agent's verdict as-is.
	//
	// #2512 orders the two rewrites at this ONE seam and the order is
	// load-bearing: (1) the #2581 downgrade runs FIRST over the retired-id set;
	// (2) the precedence ladder then runs over the NON-RETIRED rows only, so a
	// retired criterion's undecidable row cannot pin the run to undecidable
	// forever and the operator's retirement keeps its effect; (3) the recorded
	// verdict is acceptanceVerdictAtLeast(post-downgrade, derived) — a
	// severity-monotone lower bound that never softens below either source.
	//
	// The retired-id set is resolved for EVERY verdict now (not just failed),
	// because step (2) needs it regardless of what the agent shipped. Both
	// degrade branches leave the set EMPTY, which makes the ladder run over all
	// rows — the conservative direction, since a retired row can then only raise
	// severity, never lower it.
	downgradedVerdict, downgradeRetiredIDs, downgradeBasis := "", []string(nil), ""
	downgradeAddedIDs := []string(nil)
	retiredIDs := map[string]struct{}{}
	// ladderExcluded is the set of row ids the precedence ladder skips: the
	// retired ids always, plus the operator-added ids when the #3181 added-only
	// downgrade fired (their failures were neutralized, so re-deriving `failed`
	// from them would undo it).
	ladderExcluded := map[string]struct{}{}
	{
		approvedPlan, perr := s.loadApprovedPlanForRun(r.Context(), runID)
		switch {
		case perr != nil:
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"acceptance: approved plan unreadable; recording the reported verdict unchanged",
				slog.String("run_id", runID.String()), slog.String("error", perr.Error()))
		case approvedPlan == nil:
			// No plan to anchor a retirement to — nothing can be retired.
		default:
			eff, eerr := s.resolveEffectiveAcceptanceCriteria(r.Context(), runID, approvedPlan, nil)
			if eerr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"acceptance: effective criteria unreadable; recording the reported verdict unchanged",
					slog.String("run_id", runID.String()), slog.String("error", eerr.Error()))
			} else {
				retiredIDs = eff.retiredIDSet()
				if down, ids, basis := acceptanceDowngrade(acc, eff); down {
					downgradedVerdict = acceptanceVerdictPassed
					downgradeRetiredIDs = ids
					downgradeBasis = basis
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
						"acceptance: failed verdict neutralized — every failure named a retired criterion",
						slog.String("run_id", runID.String()),
						slog.String("retired_criterion_ids", strings.Join(ids, ",")))
				} else if down, added, retiredFailed := acceptanceAddedOnlyDowngrade(acc, eff); down {
					// #3181: ordered AFTER the retired-only downgrade and BEFORE the
					// ladder at this same seam, so the two neutralizations compose.
					downgradedVerdict = acceptanceVerdictPassed
					downgradeAddedIDs = added
					downgradeRetiredIDs = retiredFailed
					downgradeBasis = acceptanceDowngradeBasisAddedOnly
					for id := range eff.addedIDSet() {
						ladderExcluded[id] = struct{}{}
					}
					s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo,
						"acceptance: failed verdict neutralized — every failure named an operator-added (advisory) criterion",
						slog.String("run_id", runID.String()),
						slog.String("added_criterion_ids", strings.Join(added, ",")))
				}
			}
		}
	}

	// Step (2)+(3): derive from the non-retired rows (and, when the #3181
	// added-only downgrade fired, the non-added rows) and take the severity max.
	// So an added-only neutralization over a surviving set that is all skipped
	// records not_validated, never passed.
	// The len(rows) > 0 guard is aggregateAcceptanceResults' documented
	// PRECONDITION — without it an empty row set would answer `passed` and
	// soften a shipped failed verdict into a pass, which is the exact hazard
	// #2512/#3397 remove.
	//
	// observedBasis names WHY a POST-RUN not_validated was derived, distinct from
	// the two pre-spawn bases (#3397). It is emitted on the payload ONLY when the
	// recorded verdict actually ends up not_validated (see buildOutcomePayload),
	// so a shipped `failed` whose empty/all-skip set the ladder leaves at failed
	// carries no basis.
	recordedVerdict := acc.Verdict
	if downgradedVerdict != "" {
		recordedVerdict = downgradedVerdict
	}
	for id := range retiredIDs {
		ladderExcluded[id] = struct{}{}
	}
	nonRetired := make([]acceptanceCriterionResult, 0, len(acc.normalizedCriteria))
	for _, c := range acc.normalizedCriteria {
		if _, excluded := ladderExcluded[c.ID]; !excluded {
			nonRetired = append(nonRetired, c)
		}
	}
	observedBasis := ""
	if len(nonRetired) > 0 {
		derived := aggregateAcceptanceResults(nonRetired)
		recordedVerdict = acceptanceVerdictAtLeast(recordedVerdict, derived)
		if derived == acceptanceVerdictNotValidated {
			// Every non-retired row was skipped: the validator ran and verified
			// nothing (#3397).
			observedBasis = plan.AcceptanceBasisAllSkipObserved
		}
	} else {
		// THE NEIGHBOURING DOOR (binding condition 1, #3397): an empty non-retired
		// row set — the validator shipped a verdict itemizing zero drivable rows
		// (e.g. {"verdict":"passed"} with no criteria) — verified nothing exactly
		// as an all-skip set does. Ladder against not_validated so a shipped
		// `passed` becomes not_validated while a shipped `failed` with an empty set
		// stays failed (severity-monotone, so the guard's anti-softening purpose is
		// intact). A separate basis keeps the verified-nothing origins tellable
		// apart.
		recordedVerdict = acceptanceVerdictAtLeast(recordedVerdict, acceptanceVerdictNotValidated)
		// NAME THE ORIGIN HONESTLY (fix-up, medium/untested-path): an empty
		// non-retired set has TWO distinct origins, which must not share one
		// operator-facing sentence. If the validator itemized rows but the operator
		// retired EVERY one (the #2581 all-rows-retired path), record
		// all-retired-observed — rows WERE recorded, so the no-rows wording
		// ("recorded no criteria") would be inaccurate. Only a truly empty
		// itemization records no-rows-observed.
		if len(acc.normalizedCriteria) > 0 {
			observedBasis = plan.AcceptanceBasisAllRetiredObserved
		} else {
			observedBasis = plan.AcceptanceBasisNoRowsObserved
		}
	}
	// Step (4), UNBOUND-HEAD CLAMP (#3091). A verdict whose validated head could
	// not be resolved names no tree: nothing ties the AGENT'S claimed pass to the
	// commit that was validated, so it can never be recorded as a pass. It is
	// laddered through the SAME severity-monotone acceptanceVerdictAtLeast
	// machinery, so the direction is fixed by construction: passed (0) <
	// undecidable (2) is raised, and a shipped failed (3) is NEVER softened. The
	// clamp is the LAST rewrite, after the retirement downgrade and the row
	// aggregation, because an unbound head invalidates whatever those two
	// concluded about the tree.
	//
	// The clamp applies to EVERY unbound ship, a #2581 retirement neutralization
	// INCLUDED (#3124). The two concerns are orthogonal: retirement decides WHICH
	// CRITERIA COUNT, an unresolvable validated head means we do not know WHICH
	// TREE WAS EXERCISED AT ALL — so a neutralized pass on an unbound head is no
	// more anchored to a validated commit than any other pass. The direction stays
	// safe by construction: the max ladder can only RAISE the neutralized passed(0)
	// to undecidable(1), which is strictly LESS soft than the passed(0) retirement
	// itself produced from the shipped failed(2), so no shipped `failed` is
	// softened. The operator's retirement decision stays fully visible on the
	// clamped payload via downgrade_basis / retired_criterion_ids / verdict_reported.
	undecidableBasis := ""
	if validatedHead == "" {
		if clamped := acceptanceVerdictAtLeast(recordedVerdict, acceptanceVerdictUndecidable); clamped != recordedVerdict {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"acceptance: validated head unresolvable — clamping the recorded verdict to undecidable",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("shipped_verdict", acc.Verdict))
			recordedVerdict = clamped
			// The basis is attached only when the clamp actually rewrote the
			// verdict, so a shipped `failed` (which the ladder leaves alone) never
			// carries an undecidable basis it did not earn.
			undecidableBasis = acceptanceUndecidableBasisHeadUnresolved
		}
	}

	// effectiveVerdict stays the "differs from what the agent shipped" signal the
	// #2581 response field and the replay echo already carry: empty when the
	// recorded verdict IS the shipped one, so an unaffected outcome marshals
	// byte-identically to today. It is computed AFTER the clamp so it reflects
	// the verdict actually recorded.
	effectiveVerdict := ""
	if recordedVerdict != acc.Verdict {
		effectiveVerdict = recordedVerdict
	}

	// buildOutcomePayload renders the acceptance_outcome_recorded payload. The
	// `outcome`/`criteria_passed`/`criteria_total` tags are the issue-comment
	// render contract (issuecomment/status_template.go); verdict/failure_mode +
	// the per-result counts are the E31.8 triage carry-through. head_sha is the
	// #1682 additive field (internal chained-audit map — no docs/spec schema
	// change); older entries simply lack it and Option C fails closed on absence.
	buildOutcomePayload := func(artifactID string) []byte {
		fields := map[string]any{
			"run_id":       runID.String(),
			"stage_id":     stageID.String(),
			"artifact_id":  artifactID,
			"content_hash": contentHash,
			"verdict":      recordedVerdict,
			// failure_mode stays what the AGENT reported: like the raw tallies
			// below it is evidence, not a verdict. On a downgrade it is read
			// alongside verdict_reported.
			"failure_mode": acc.FailureMode,
			"outcome":      acceptanceOutcomeLabel(recordedVerdict),
			// The raw per-result tallies are recorded UNCHANGED on a downgrade —
			// they are the agent's evidence and must not be rewritten.
			"criteria_passed":      passed,
			"criteria_failed":      failed,
			"criteria_skipped":     skipped,
			"criteria_undecidable": undecidable,
			"criteria_total":       total,
			"target_url":           acc.TargetURL,
			"evidence_hashes":      acc.normalizedEvidenceHashes,
			"auth_method":          authMethod,
			"head_sha":             validatedHead,
		}
		// verdict_reported is the ONE "what the agent said vs what was recorded"
		// field (#2581's key, reused by #2512 rather than duplicated): present
		// whenever the recorded verdict differs from the shipped one, for EITHER
		// reason. The two downgrade-specific keys stay gated on an actual
		// downgrade, so a ladder-only difference does not fabricate a retirement
		// basis. All three are OMITTED on an unaffected outcome, which therefore
		// marshals byte-identically to today.
		if effectiveVerdict != "" {
			fields["verdict_reported"] = acc.Verdict
		}
		if downgradeBasis != "" {
			fields["downgrade_basis"] = downgradeBasis
			// An added-only downgrade records retired_criterion_ids only when a
			// retired failure rode along (the composed case); the retired-only
			// basis always carries it, as before.
			if downgradeBasis == acceptanceDowngradeBasisRetiredOnly || len(downgradeRetiredIDs) > 0 {
				fields["retired_criterion_ids"] = downgradeRetiredIDs
			}
			if len(downgradeAddedIDs) > 0 {
				fields["added_criterion_ids"] = downgradeAddedIDs
			}
		}
		// undecidable_basis is the #3091 clamp's own key, DISTINCT from
		// downgrade_basis (which keeps its #2581 retirement meaning). It is
		// emitted only when the clamp actually fired, so an outcome with a
		// resolvable head marshals byte-identically to before.
		if undecidableBasis != "" {
			fields["undecidable_basis"] = undecidableBasis
		}
		// basis (#3397) names WHY a POST-RUN not_validated verdict was derived —
		// all-skip-observed or no-rows-observed. Emitted ONLY when the recorded
		// verdict is not_validated: a clamp that raised it to undecidable carries
		// undecidable_basis + verdict_reported and NO basis, and every other
		// outcome marshals byte-identically to before. The value is distinct from
		// the two pre-spawn bases, which the orchestrator (not this handler) emits.
		if recordedVerdict == acceptanceVerdictNotValidated && observedBasis != "" {
			fields[plan.AcceptanceBasisKey] = observedBasis
		}
		// Replay evidence (E72.4 / #3328): the runner-injected cap header copied
		// VERBATIM plus the scenario-row tallies counted here. Absent body.replay
		// records replay:null so a consumer can tell "no corpus replayed" from a
		// zero-valued header.
		fields["replay"] = acceptanceReplayPayload(acc)
		// Transcript summary (E72.5 / #3329): derived from the STORED
		// artifact. Absent body.transcript records transcript:null so a
		// consumer can tell "no transcript shipped" from a zero-valued block.
		if transcriptSummary != nil {
			fields["transcript"] = transcriptSummary
		} else {
			fields["transcript"] = nil
		}
		p, _ := json.Marshal(fields)
		return p
	}

	// Idempotency: dedup on (stage_id, content_hash). A re-delivery of the same
	// acceptance record returns the existing artifact rather than creating a
	// duplicate (and writing a second audit entry).
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		// Self-heal the chained governance audit entry (#1396). A prior attempt
		// may have persisted the artifact (Create succeeded) but failed its
		// acceptance_outcome_recorded append (AppendChained failed → 500); this
		// identical retry short-circuits here. Verify the outcome entry exists
		// for this artifact and append it idempotently if missing, so a
		// retry-after-partial-failure ends with BOTH the artifact and its
		// governance record. The helper fails closed on a read error.
		if _, herr := s.ensureGovernanceAuditEntry(r.Context(), runID,
			CategoryAcceptanceOutcomeRecorded, existing.ID.String(), func() error {
				_, aerr := s.appendAcceptanceOutcomeSerialized(r.Context(), stageID, audit.ChainAppendParams{
					RunID:        runID,
					StageID:      &stageID,
					Timestamp:    time.Now().UTC(),
					Category:     CategoryAcceptanceOutcomeRecorded,
					ActorKind:    &actorKind,
					ActorSubject: actorSubject,
					Payload:      buildOutcomePayload(existing.ID.String()),
				})
				return aerr
			}); herr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"heal governance audit entry failed", map[string]any{"error": herr.Error()})
			return
		}
		// #2581 replay fidelity: the effective verdict above was recomputed from
		// the CURRENT approval chain, which may carry a retirement recorded AFTER
		// the original ship (a later plan-gate approve on a re-planned run). The
		// replay response must not diverge from the acceptance_outcome_recorded
		// entry the merge gate reads, so it ECHOES the chain: the recorded entry
		// for this artifact wins whenever it can be read, and the freshly computed
		// value stands only when there is no readable entry (in which case the
		// heal above just appended exactly that value).
		replayEffectiveVerdict := effectiveVerdict
		if recorded, ok := s.recordedAcceptanceEffectiveVerdict(r.Context(), runID, existing.ID.String()); ok {
			replayEffectiveVerdict = recorded
		}
		s.writeJSON(w, r, http.StatusOK, acceptanceResponse{
			ID:               existing.ID,
			StageID:          existing.StageID,
			ContentHash:      existing.ContentHash,
			Verdict:          acc.Verdict,
			FailureMode:      acc.FailureMode,
			EffectiveVerdict: replayEffectiveVerdict,
			Idempotent:       true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing acceptance failed", map[string]any{"error": err.Error()})
		return
	}

	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindAcceptance,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 — graduate to acceptance_v1
		// once the field shape settles (mirroring deployment).
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create acceptance artifact failed", map[string]any{"error": err.Error()})
		return
	}

	if _, err := s.appendAcceptanceOutcomeSerialized(r.Context(), stageID, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceOutcomeRecorded,
		ActorKind:    &actorKind,
		ActorSubject: actorSubject,
		Payload:      buildOutcomePayload(created.ID.String()),
	}); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append audit entry failed", map[string]any{"error": err.Error()})
		return
	}

	// Scenario regressions (E72.4 / #3328): one acceptance_scenario_regression
	// entry per FAILED replayed-scenario row, attributed ONLY from the runner's
	// replay.scenarios entry. Fresh-create path only, so a re-delivered verdict
	// cannot double-record. Best-effort: an append failure WARN-logs and never
	// unwinds the artifact + outcome already committed.
	s.recordAcceptanceScenarioRegressions(r.Context(), runID, stageID, created.ID.String(), acc, actorKind, actorSubject)

	// Refresh the run's sticky living-anchor comment so the acceptance outcome
	// surfaces on the issue timeline (the acceptance audit categories render
	// data-drivenly through issuecomment's activityCategories set).
	s.notifyStatusUpdate(r.Context(), runID, "acceptance_recorded")

	// E31.8 (#1536): route a freshly persisted verdict:failed artifact through
	// deterministic triage. ONLY on this fresh-create path — the idempotent
	// replay branch above returns before here, so a re-delivered identical
	// verdict cannot double-route. Best-effort relative to the ship: any
	// internal error WARN-logs inside and never unwinds the 201 / artifact /
	// outcome audit already committed.
	//
	// A NEUTRALIZED verdict (#2581) skips triage entirely: there is no failure
	// left to route once every reported failure named a criterion the operator
	// retired at the approval gate.
	//
	// #2512: triage stays keyed on the AGENT's own failed claim (with its
	// failure_mode), NOT on the ladder-derived recorded verdict. A shipped
	// `passed` body carrying a failed row records `failed` and gates the merge
	// through acceptanceGateTriage, but it carries no failure_mode for the
	// deterministic classifier to read — so it routes to operator arbitration
	// rather than through a classifier fed an empty mode. An `undecidable`
	// recorded verdict routes NO triage at all: an undecidable row is not a
	// defect, so there is nothing to fix up or retry. A #3397 not_validated
	// recorded verdict likewise routes no triage — it can only arise from a
	// shipped `passed` (acc.Verdict != failed), so this branch is not entered;
	// a shipped `failed` whose rows are all-skip stays recorded `failed` and
	// still routes class-5 triage exactly as before.
	paged := false
	if acc.Verdict == acceptanceVerdictFailed && downgradedVerdict == "" {
		disposition := s.triageAcceptanceFailure(r.Context(), runID, stage, acc, created.ID.String())
		paged = acceptanceDispositionPages(disposition)
	}

	// Fire the page-class ping immediately (#1786), but ONLY when triage
	// actually PAGED: a paged acceptance-triage disposition is otherwise silent
	// on anchor edits, so pinging within the record window (after triage has
	// decided and written its acceptance_triage_decided entry) gets the
	// operator looking sooner than the next transition. A passed verdict (no
	// triage) or an auto-routed fixup_dispatched / retry_dispatched disposition
	// writes NO page-class event, so calling the hook then would only flush an
	// OLDER unpinged page-class event at this unrelated moment
	// (NotifyPageClassForRun evaluates the full audit history). Deduped on the
	// source Sequence, so it never double-posts.
	if paged {
		s.notifyPageClass(r.Context(), runID, "acceptance_recorded")
	}

	s.writeJSON(w, r, http.StatusCreated, acceptanceResponse{
		ID:               created.ID,
		StageID:          created.StageID,
		ContentHash:      created.ContentHash,
		Verdict:          acc.Verdict,
		FailureMode:      acc.FailureMode,
		EffectiveVerdict: effectiveVerdict,
		Idempotent:       false,
	})
}

// reopenAcceptanceOnFixupPush invalidates a stale acceptance verdict when a
// fix-up push lands a NEW head AFTER the acceptance stage already settled
// (#1682, Option A — the automatic in-band defense). It locates the run's
// acceptance stage; if that stage is StageStateSucceeded AND carries a recorded
// acceptance_outcome_recorded verdict, it re-opens the stage (succeeded →
// pending) via run.ReopenAcceptanceStage and appends an acceptance_reopened
// invalidation audit entry — the SAME kind the operator-gated #1567 re-open
// uses (no new issue-comment surface). next_actions then routes to
// acceptance_pending so the operator re-dispatches acceptance against the final
// commit.
//
// No-op (everything untouched) when: the run has no acceptance stage; the
// acceptance stage is not succeeded (a PRE-acceptance fix-up must not reopen
// anything); or the succeeded stage has NO recorded verdict (that outcome-less
// hole is the retry handler's #1567 operator-reopen path, not this one).
// Idempotent against a re-delivered fixup_pushed: the caller's (stage_id,
// head_sha) dedup short-circuits before this runs, and ReopenAcceptanceStage
// refuses a non-succeeded (already-pending) stage, so a second delivery cannot
// double-reopen. Best-effort: every failure WARN-logs and never unwinds the
// fix-up push success.
func (s *Server) reopenAcceptanceOnFixupPush(ctx context.Context, runID uuid.UUID, newHeadSHA string) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: list stages failed; skipping reopen",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return
	}
	var acceptance *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeAcceptance {
			acceptance = st
			break
		}
	}
	// A pre-acceptance fix-up (no acceptance stage, or one not yet succeeded)
	// leaves the gate alone — only a SETTLED acceptance stage can carry a stale
	// verdict to invalidate.
	if acceptance == nil || acceptance.State != run.StageStateSucceeded {
		return
	}
	hasVerdict, err := s.acceptanceStageHasVerdict(ctx, runID, acceptance.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: verdict lookup failed; skipping reopen",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptance.ID.String()),
			slog.String("error", err.Error()))
		return
	}
	if !hasVerdict {
		// Outcome-less succeeded acceptance stage — the #1567 operator-reopen
		// path owns it, not a fix-up invalidation.
		return
	}

	dec, err := run.ReopenAcceptanceStage(ctx, s.cfg.RunRepo, acceptance.ID)
	if err != nil {
		// A concurrent/re-delivered reopen (already pending) or a terminal run
		// refuses here — benign; the stage is already (being) re-opened.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: reopen refused; leaving stage as-is",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptance.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	subject := "system:fixup-acceptance-invalidation"
	actorKind := audit.ActorSystem
	payload, _ := json.Marshal(map[string]any{
		"stage_id":    dec.Stage.ID.String(),
		"prior_state": string(dec.PriorState),
		"head_sha":    newHeadSHA,
		"reason":      "a fix-up push landed a new head after the acceptance stage settled; the prior acceptance verdict is invalidated and the stage re-opened for re-validation against the final commit",
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		StageID:      &dec.Stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryAcceptanceReopened,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"fixup acceptance invalidation: append acceptance_reopened audit failed",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", dec.Stage.ID.String()),
			slog.String("error", err.Error()))
	}
	s.notifyStatusUpdate(ctx, runID, "acceptance_reopened")
}

// acceptanceStageHasVerdict reports whether the run's audit chain carries an
// acceptance_outcome_recorded entry scoped to stageID — i.e. the acceptance
// stage settled WITH a recorded verdict. Distinguishes the #1682 fix-up
// invalidation target (verdict-ful) from the #1567 outcome-less operator-reopen
// hole. Propagates the read error so the caller fails closed (skips the reopen)
// on an unreadable chain rather than acting on unknown evidence state.
func (s *Server) acceptanceStageHasVerdict(ctx context.Context, runID, stageID uuid.UUID) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			return true, nil
		}
	}
	return false, nil
}

// Acceptance drive-gate state values (E31.17 / #1568). The single source of
// truth acceptanceGateState returns, consumed by BOTH drive surfaces
// (ObserveParkedReviewForDrive's presentation status and AutoDriveRunGate's
// real merge). The pending / outcome-unknown / triage strings are pinned to the
// drive.Rule* constants (string conversions of constants are constant
// expressions) so the drive presentation status, the audit-rule name, and the
// MCP next_actions.state string cannot diverge. acceptanceGatePassed mirrors the
// MCP "acceptance_passed" state; the empty acceptanceGateNotDeclared marks a
// workflow with no acceptance stage (the merge is never acceptance-gated).
const (
	acceptanceGateNotDeclared    = ""
	acceptanceGatePending        = string(drive.RuleAcceptancePending)
	acceptanceGateOutcomeUnknown = string(drive.RuleAcceptanceOutcomeUnknown)
	acceptanceGateTriage         = string(drive.RuleAcceptanceTriage)
	acceptanceGatePassed         = "acceptance_passed"
	// acceptanceGateSkippedOutOfScope is a merge-eligible terminal disposition
	// (E38.3 / #1877): the acceptance stage settled with NO recorded verdict but
	// DOES carry a stage-scoped acceptance_skipped_out_of_scope marker — the
	// orchestrator auto-terminated it because the approved plan declared
	// verification.out_of_scope with zero acceptance_criteria. The skip is a
	// legitimate recorded outcome equivalent to a passed verdict for the merge
	// gate, so both drive surfaces fall through to the merge ritual rather than
	// parking in acceptance_settled_outcome_unknown. Value is the audit category
	// itself so the marker string and the gate state cannot diverge.
	acceptanceGateSkippedOutOfScope = CategoryAcceptanceSkippedOutOfScope
	// acceptanceGateNotValidated is the second merge-eligible terminal
	// disposition (#2347): the acceptance stage settled with a recorded
	// not_validated verdict. Two origins reach it: the orchestrator
	// short-circuited PRE-SPAWN (zero acceptance criteria, or every criterion
	// skip_expected with an expectation_basis), OR (since #3397) the stage RAN
	// and shipped a verdict whose every non-retired row was skipped / no rows at
	// all. Either way ZERO criteria were verified.
	//
	// It is merge-ELIGIBLE on purpose: a change with no live target must not be
	// stranded, and blocking here would trade a dishonest pass for a wedge. The
	// distinguishing signal is carried instead by the state STRING — the MCP
	// next_actions surface renders acceptance_not_validated with a reason telling
	// the operator zero criteria were verified and asking them to say so in their
	// merge verdict. It is deliberately NOT acceptanceGatePassed: a consumer that
	// wants to treat the two differently can, and a future gate that must
	// distinguish them does not have to re-derive the basis from the payload.
	acceptanceGateNotValidated = "acceptance_not_validated"
	// acceptanceGateArbitrated is the THIRD merge-eligible terminal disposition
	// (E66.37 / #2474), beside skipped-out-of-scope and not-validated: the newest
	// acceptance verdict FAILED, its triage disposition PAGED (no automatic route
	// fired), and an operator recorded an acceptance_triage_arbitrated discharge
	// BOUND BY SEQUENCE to that exact verdict.
	//
	// It is deliberately NOT acceptanceGatePassed: nothing about the evidence
	// changed — the operator overrode a failed verdict and said why on the audit
	// chain, which is a materially different claim from "acceptance passed". The
	// distinguishing signal is carried by the state STRING so every consumer (the
	// merge handler, the delegated merge, the drive presentation, the MCP
	// next_actions surface) can render the override honestly.
	//
	// Sequence binding is the invalidation mechanism: a later acceptance re-run
	// records a NEW outcome at a HIGHER sequence that no prior arbitration names,
	// so the gate drops back to acceptanceGateTriage by construction.
	acceptanceGateArbitrated = "acceptance_arbitrated"
	// acceptanceGateUndecidable is the FOURTH merge-eligible terminal
	// disposition (#2512 / E48.78): the acceptance stage RAN, drove the preview,
	// and reported per-criterion rows of which at least one could not be DECIDED,
	// with no row failing. The verdict is server-derived by the precedence ladder
	// and unforgeable — the ship endpoint admits only passed/failed at top level.
	//
	// It is merge-ELIGIBLE on purpose, and that IS the #2474 wedge-surface
	// reduction #2512 claims: today a validator that cannot decide a criterion
	// must ship `failed`, which lands in acceptance triage and can only be
	// discharged by an operator arbitration. With this state it lands
	// merge-eligible with no arbitration at all, and the distinguishing signal is
	// carried by the state STRING so the operator surface asks for an
	// acknowledgement in the merge verdict instead.
	//
	// It is deliberately NOT acceptanceGatePassed (nothing verified that
	// criterion) and NOT acceptanceGateNotValidated (verified nothing at all —
	// no passed/failed/undecidable row; the two stay mutually exclusive because
	// one severity ladder decides between them). It routes NO acceptance_triage_decided
	// disposition: an undecidable row is not a defect, so there is nothing to fix
	// up or retry.
	acceptanceGateUndecidable = "acceptance_undecidable"
	// acceptanceGateOmitted is the FIFTH merge-eligible terminal disposition
	// (E72.1 / #3325): the approved plan declared
	// verification.acceptance_surface: none, so the plan gate recorded an
	// acceptance_stage_omitted marker and DELETED the run's pending acceptance
	// stage before the orchestrator ever saw it. The workflow SPEC still
	// declares the stage — that is why acceptanceGateState gets past its
	// off-switch — but no stage ROW exists, and without this disposition every
	// such run would wedge at acceptance_pending ("not yet materialized").
	//
	// It is deliberately NOT acceptanceGatePassed: nothing was verified, because
	// the stage never existed to verify anything — a materially different claim
	// from a pass, and from not_validated (which is decided by a stage that DID
	// exist and short-circuited). The state STRING carries that distinction to
	// every consumer. It is read ONLY when no acceptance stage row exists: a
	// marker with the stage still present (the delete failed after the append)
	// takes the ordinary stage path, so the gate never admits a merge on the
	// marker alone while a stage row is live. Value is the audit category
	// itself so the marker string and the gate state cannot diverge.
	acceptanceGateOmitted = CategoryAcceptanceStageOmitted
	// acceptanceGateVerdictUnshipped is a NON-merge-admitting terminal state
	// (E72.11 / #3447): the acceptance stage settled succeeded but carries a
	// LIVE stage-scoped acceptance_verdict_unshipped marker — the runner's
	// verdict ship failed (a 413 body_too_large) after the trace upload had
	// already settled the stage. It is consulted BEFORE the verdict switch so
	// that a STALE earlier outcome (a first attempt that passed, then a re-run
	// whose verdict never shipped) cannot masquerade as the current verdict:
	// the marker wins whenever it is newer than the newest recorded outcome.
	// Deliberately NOT in acceptanceGateAdmitsMerge — nothing verified this
	// head — and it never blocks retryAcceptanceOutcomeUnknown's re-open,
	// which is the recovery (a fresh dispatch anchor retires the marker). The
	// value is pinned to the drive rule so the drive presentation status, the
	// audit-rule name, and the MCP next_actions.state string cannot diverge.
	acceptanceGateVerdictUnshipped = string(drive.RuleAcceptanceVerdictUnshipped)
)

// acceptanceGateAdmitsMerge reports whether an acceptanceGateState value admits
// a merge (E66.37 / #2474). It is the SINGLE predicate the three merge-adjacent
// consumers share — the operator merge endpoint (merge_run.go), the delegated
// may_merge arm (dispatchAcceptanceGatedMerge), and the drive presentation's
// fall-through — so a new merge-eligible state can never be admitted by two of
// them and refused by the third.
//
// The admitted set: not-declared (the workflow declares no acceptance stage),
// passed (a validated pass), skipped-out-of-scope (E38.3 / #1877), not-validated
// (#2347, zero criteria verified), arbitrated (#2474, an operator discharged
// a paged triage), undecidable (#2512, the stage ran and could not decide a
// criterion, with nothing failing), and omitted (E72.1 / #3325, the plan
// declared no acceptance surface and the stage was dropped at approval).
// Everything else — pending, triage, settled-outcome-unknown, and any future
// state — is refused.
//
// Callers must STILL gate on a nil read error themselves: this predicate sees
// only the state string, and acceptanceGateState returns ("", err) on a read
// failure, which this function would otherwise admit as not-declared.
func acceptanceGateAdmitsMerge(state string) bool {
	switch state {
	case acceptanceGateNotDeclared, acceptanceGatePassed,
		acceptanceGateSkippedOutOfScope, acceptanceGateNotValidated,
		acceptanceGateArbitrated, acceptanceGateUndecidable,
		acceptanceGateOmitted:
		return true
	default:
		return false
	}
}

// acceptanceOutcome is the decoded newest acceptance_outcome_recorded entry for
// a run (E66.37 / #2474). Recorded=false means the acceptance stage has shipped
// no verdict yet; Recorded=true with an empty Verdict is the recorded-but-
// unreadable hole. Sequence is the audit sequence the arbitration verb BINDS to,
// so a later re-run's higher-sequence outcome invalidates a prior arbitration by
// construction.
type acceptanceOutcome struct {
	Verdict         string
	Sequence        int64
	CriteriaFailed  int
	CriteriaSkipped int
	StageID         *uuid.UUID
	Recorded        bool
	// Transcript is the decoded `transcript` block (E72.5 / #3329): nil when
	// the payload carries null, no block, or an undecodable one. The producer
	// symbol the implement-review gate-evidence stamping consumes.
	Transcript *acceptanceTranscriptSummary
}

// latestAcceptanceOutcome returns the newest (highest-Sequence)
// acceptance_outcome_recorded audit entry for the run, decoded, plus a read
// error. It is the single reader behind both latestAcceptanceVerdict (the gate's
// verdict axis) and the arbitration endpoint (which additionally needs the
// sequence to bind to, the failed-criteria count to gate the acknowledgement on,
// and the acceptance stage the outcome was scoped to).
//
// A recorded entry whose payload cannot be decoded is reported with an empty
// Verdict and Recorded=true so the caller treats it as a settled-outcome-unknown
// hole, never a pass — byte-for-byte the prior posture. The audit read error is
// PROPAGATED (never swallowed) so acceptanceGateState can fail closed: the
// binding condition forbids resolving an unreadable acceptance outcome to
// passed/merge.
func (s *Server) latestAcceptanceOutcome(ctx context.Context, runID uuid.UUID) (acceptanceOutcome, error) {
	if s.cfg.AuditRepo == nil {
		return acceptanceOutcome{}, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return acceptanceOutcome{}, err
	}
	var latest *audit.Entry
	for _, e := range entries {
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return acceptanceOutcome{}, nil
	}
	out := acceptanceOutcome{Sequence: latest.Sequence, StageID: latest.StageID, Recorded: true}
	var p struct {
		Verdict         string          `json:"verdict"`
		CriteriaFailed  int             `json:"criteria_failed"`
		CriteriaSkipped int             `json:"criteria_skipped"`
		Transcript      json.RawMessage `json:"transcript"`
	}
	if uerr := json.Unmarshal(latest.Payload, &p); uerr != nil {
		// A malformed outcome payload is a recorded-but-unreadable verdict:
		// treat it as an outcome-unknown hole (recorded, empty verdict), never
		// a pass.
		return out, nil
	}
	out.Verdict = p.Verdict
	out.CriteriaFailed = p.CriteriaFailed
	out.CriteriaSkipped = p.CriteriaSkipped
	out.Transcript = decodeAcceptanceTranscriptSummary(p.Transcript)
	return out, nil
}

// latestAcceptanceVerdict is the thin verdict-axis wrapper over
// latestAcceptanceOutcome, kept so the pre-existing callers are untouched.
// recorded=false with a nil error means the acceptance stage has shipped no
// verdict yet; the read error is PROPAGATED so callers fail closed.
func (s *Server) latestAcceptanceVerdict(ctx context.Context, runID uuid.UUID) (verdict string, recorded bool, err error) {
	out, err := s.latestAcceptanceOutcome(ctx, runID)
	if err != nil {
		return "", false, err
	}
	return out.Verdict, out.Recorded, nil
}

// acceptanceArbitrationDischarges reports whether a paged acceptance triage has
// been discharged by an operator arbitration BOUND to the outcome at
// expectedSequence (E66.37 / #2474).
//
// Binding approval condition 2 (READ-SIDE REVALIDATION). The naive shape — read
// the newest outcome, then read the arbitrations — resolves acceptance_arbitrated
// on the strength of TWO INDEPENDENT reads, so a concurrent acceptance re-run
// landing between them can supersede the outcome the first read saw while the
// arbitration still matches it. The condition offered a consistent snapshot as
// the preferred remedy over optimistic revalidation, and audit.Repository
// provides one: ListForRun is a SINGLE query returning every entry for the run.
// So this reads ONE snapshot and evaluates BOTH halves inside it —
//
//	(a) the newest acceptance_outcome_recorded entry in the snapshot must STILL
//	    be the one at expectedSequence (the caller's outcome has not been
//	    superseded), and
//	(b) some acceptance_triage_arbitrated entry in that SAME snapshot must carry
//	    payload outcome_sequence EQUAL to it.
//
// — which closes the interleaving window by construction rather than papering
// over it: there is no instant between the two observations because there is
// only one observation. A snapshot whose newest outcome has moved returns false,
// and the gate falls back to acceptanceGateTriage.
//
// Correlation is payload outcome_sequence EQUALITY, never ordering. An
// arbitration appended AFTER a newer verdict but NAMING an older outcome is
// therefore ignored — the rule the MCP classifier mirrors exactly (binding
// condition 3) so the two surfaces cannot disagree.
//
// The read error is PROPAGATED so acceptanceGateState fails closed (never a
// merge-eligible state on unknown evidence).
func (s *Server) acceptanceArbitrationDischarges(ctx context.Context, runID uuid.UUID, expectedSequence int64) (bool, error) {
	if s.cfg.AuditRepo == nil {
		return false, nil
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		return false, err
	}
	newestOutcome := int64(0)
	haveOutcome := false
	for _, e := range entries {
		if e.Category != CategoryAcceptanceOutcomeRecorded {
			continue
		}
		if !haveOutcome || e.Sequence > newestOutcome {
			newestOutcome, haveOutcome = e.Sequence, true
		}
	}
	// Read-side revalidation: the outcome the caller classified is no longer the
	// newest one in this snapshot (a concurrent re-run superseded it), so no
	// arbitration can discharge it. Fail closed to triage.
	if !haveOutcome || newestOutcome != expectedSequence {
		return false, nil
	}
	for _, e := range entries {
		if e.Category != CategoryAcceptanceTriageArbitrated {
			continue
		}
		if seq, ok := arbitrationOutcomeSequence(e.Payload); ok && seq == newestOutcome {
			return true, nil
		}
	}
	return false, nil
}

// arbitrationOutcomeSequence decodes the outcome_sequence an
// acceptance_triage_arbitrated payload BINDS to. An undecodable payload or an
// absent field reports ok=false, so a malformed arbitration entry can never
// discharge a triage (it is skipped, not treated as a wildcard match).
func arbitrationOutcomeSequence(payload []byte) (int64, bool) {
	var p struct {
		OutcomeSequence *int64 `json:"outcome_sequence"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.OutcomeSequence == nil {
		return 0, false
	}
	return *p.OutcomeSequence, true
}

// acceptanceGateState is the single server-side acceptance-gate classifier
// (E31.17 / #1568) that both drive surfaces consult before advancing to merge.
// It mirrors the MCP next_actions classifier (acceptanceStageNextActions) so
// the backend drive path and the MCP presentation surface agree on when the
// acceptance stage blocks the merge (ADR-049 decision #6: the merge is gated on
// the acceptance_passed evidence condition).
//
// Returns, for a run whose workflow declares an acceptance stage:
//   - acceptanceGatePassed        — newest recorded verdict is passed (merge OK).
//   - acceptanceGateNotValidated  — newest recorded verdict is not_validated
//     (#2347 pre-spawn short-circuit, OR #3397 the stage RAN and shipped an
//     all-skip / no-rows verdict — either way ZERO criteria verified).
//     Merge-eligible, but a distinct state so the operator surface can say so
//     rather than reporting a pass.
//   - acceptanceGateUndecidable   — newest recorded verdict is undecidable
//     (#2512: the stage RAN and reported at least one criterion it could not
//     decide, with nothing failing). Merge-eligible; NOT a pass. Disjoint from
//     not_validated because one severity ladder decides between them.
//   - acceptanceGateTriage        — newest recorded verdict is failed and no
//     operator arbitration discharges it.
//   - acceptanceGateArbitrated    — newest recorded verdict is failed BUT an
//     operator recorded an acceptance_triage_arbitrated discharge bound to that
//     exact outcome sequence (E66.37 / #2474). Merge-eligible; NOT a pass.
//   - acceptanceGateSkippedOutOfScope — no readable verdict, the acceptance
//     stage is terminal, AND it carries a stage-scoped
//     acceptance_skipped_out_of_scope marker (E38.3 / #1877 auto-terminated
//     out-of-scope skip — a legitimate merge-eligible disposition).
//   - acceptanceGateVerdictUnshipped — the acceptance stage is terminal and
//     carries a LIVE stage-scoped acceptance_verdict_unshipped marker (E72.11 /
//     #3447: newer than the stage's latest dispatch/reopen anchor and newer
//     than any stage-scoped acceptance_outcome_recorded entry), and either no
//     outcome is recorded or the newest recorded outcome is OLDER than the
//     marker. Consulted BEFORE the verdict switch so a stale earlier verdict
//     cannot admit a merge over an unshipped re-run. NOT merge-eligible;
//     the recovery is retry_stage (re-open + re-dispatch retires the marker).
//   - acceptanceGateOutcomeUnknown— no readable verdict, the acceptance stage
//     is terminal, and NO skip marker (the genuine settled-outcome-unknown hole).
//   - acceptanceGateOmitted       — no verdict, NO acceptance stage row at all,
//     and the run carries an acceptance_stage_omitted marker (E72.1 / #3325:
//     the plan declared acceptance_surface: none and the plan gate dropped the
//     stage). Merge-eligible; NOT a pass. Read only when no stage row exists.
//   - acceptanceGatePending       — no verdict yet and the acceptance stage is
//     non-terminal, or not yet materialized with no omission marker.
//
// For a workflow with no acceptance stage it returns acceptanceGateNotDeclared
// ("") — the merge is never acceptance-gated. FAIL-CLOSED (binding condition):
// an acceptance-outcome audit read error is PROPAGATED, never resolved to
// passed — so a caller that cannot read the outcome skips advancing rather than
// merging on unknown evidence.
func (s *Server) acceptanceGateState(ctx context.Context, runRow *run.Run, stages []*run.Stage) (string, error) {
	// Off-switch: a workflow that declares no acceptance stage never gates the
	// merge (mirrors resolveAcceptanceStage's stage-conditional posture).
	if _, ok := s.resolveAcceptanceStageSpec(ctx, runRow); !ok {
		return acceptanceGateNotDeclared, nil
	}
	outcome, err := s.latestAcceptanceOutcome(ctx, runRow.ID)
	if err != nil {
		return "", err
	}
	// E72.11 / #3447: a LIVE unshipped-verdict marker on a terminal acceptance
	// stage wins over any recorded outcome OLDER than it — the run-8b911565
	// shape is a stale first-attempt `passed` outcome plus a newer marker from
	// a re-run whose verdict never shipped, and reading the stale outcome would
	// admit a merge over evidence the stage never delivered. An outcome NEWER
	// than the marker means the verdict did ship after all (or a later episode
	// shipped one), so the marker is ignored and the verdict switch decides.
	// FAIL-CLOSED: the marker read error is PROPAGATED, never resolved to a
	// merge-eligible state.
	if acc := acceptanceStageOf(stages); acc != nil && acc.State.IsTerminal() {
		markerSeq, live, merr := s.acceptanceVerdictUnshippedLive(ctx, runRow.ID, acc.ID)
		if merr != nil {
			return "", merr
		}
		if live && (!outcome.Recorded || outcome.Sequence < markerSeq) {
			return acceptanceGateVerdictUnshipped, nil
		}
	}
	if outcome.Recorded {
		switch outcome.Verdict {
		case acceptanceVerdictPassed:
			return acceptanceGatePassed, nil
		case acceptanceVerdictFailed:
			// E66.37 / #2474: a failed verdict is merge-eligible ONLY when an
			// operator arbitration discharges THIS outcome (sequence-bound, read
			// from one consistent audit snapshot per binding condition 2). No
			// matching arbitration — or one naming a superseded outcome — leaves
			// the run in triage exactly as before. FAIL-CLOSED: the snapshot read
			// error is propagated, never resolved to a merge-eligible state.
			arbitrated, aerr := s.acceptanceArbitrationDischarges(ctx, runRow.ID, outcome.Sequence)
			if aerr != nil {
				return "", aerr
			}
			if arbitrated {
				return acceptanceGateArbitrated, nil
			}
			return acceptanceGateTriage, nil
		case acceptanceVerdictNotValidated:
			// #2347 / #3397: a stage that verified zero criteria — short-circuited
			// pre-spawn, or run-then-all-skip/no-rows. Merge-eligible, but NOT
			// acceptanceGatePassed — and explicitly handled here rather than falling
			// through to the settled-outcome-unknown hole below, which would wedge
			// every verified-nothing run at a 409.
			return acceptanceGateNotValidated, nil
		case acceptanceVerdictUndecidable:
			// #2512: the stage ran and reported evidence that could not decide at
			// least one criterion, with nothing failing. Merge-eligible, but NOT
			// acceptanceGatePassed — and, like not_validated, handled BESIDE it
			// rather than falling through to the settled-outcome-unknown hole
			// below, which would wedge every undecidable run at a 409.
			return acceptanceGateUndecidable, nil
		}
		// Recorded but unreadable/empty verdict falls through to the
		// stage-terminality distinction below (outcome unknown vs pending).
	}
	// No settled verdict. Distinguish a terminal acceptance stage (it settled
	// but no verdict is visible) from a non-terminal or not-yet-materialized
	// stage (still pending). For a terminal stage, an out-of-scope skip marker
	// (E38.3 / #1877) is a legitimate merge-eligible disposition — consult it
	// before falling to the genuine settled-outcome-unknown hole. FAIL-CLOSED: a
	// marker read error is PROPAGATED (same posture as the verdict read), never
	// resolved to a merge-eligible state on unknown evidence.
	if acc := acceptanceStageOf(stages); acc != nil && acc.State.IsTerminal() {
		skipped, serr := s.acceptanceStageSkippedOutOfScope(ctx, runRow.ID, acc.ID)
		if serr != nil {
			return "", serr
		}
		if skipped {
			return acceptanceGateSkippedOutOfScope, nil
		}
		return acceptanceGateOutcomeUnknown, nil
	}
	// No acceptance stage ROW at all (the spec declares one — the off-switch
	// above passed). E72.1 / #3325: the plan gate deletes the pending stage when
	// the approved plan declares acceptance_surface: none, leaving an
	// acceptance_stage_omitted marker; that marker is the merge-eligible
	// disposition. Consulted ONLY on the no-row branch — a marker beside a live
	// stage row (delete failed after append) took the stage path above. No
	// marker → the stage is genuinely not yet materialized → pending, unchanged.
	// FAIL-CLOSED: the marker read error is PROPAGATED, same posture as the
	// verdict and skip-marker reads.
	if acceptanceStageOf(stages) == nil {
		omitted, oerr := s.acceptanceStageOmitted(ctx, runRow.ID)
		if oerr != nil {
			return "", oerr
		}
		if omitted {
			return acceptanceGateOmitted, nil
		}
	}
	return acceptanceGatePending, nil
}

// acceptanceStageOmitted reports whether the run's audit chain carries at least
// one acceptance_stage_omitted marker (E72.1 / #3325) — the plan gate's durable
// record that it dropped the run's pending acceptance stage because the
// approved plan declared acceptance_surface: none. Propagates the read error so
// acceptanceGateState fails closed rather than resolving a merge-eligible state
// on an unreadable chain.
func (s *Server) acceptanceStageOmitted(ctx context.Context, runID uuid.UUID) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceStageOmitted)
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// acceptanceStageSkippedOutOfScope reports whether the run's audit chain carries
// an acceptance_skipped_out_of_scope entry scoped to stageID — i.e. the
// orchestrator auto-terminated this acceptance stage because the approved plan
// declared verification.out_of_scope with zero acceptance_criteria (E38.3 /
// #1657). Mirrors acceptanceStageHasVerdict: the orchestrator's
// emitAcceptanceSkippedOutOfScope appends the marker with StageID set
// (orchestrator.go), so the stage-scoped filter matches exactly. Propagates the
// read error so acceptanceGateState fails closed (never resolves a merge-eligible
// state) on an unreadable chain rather than acting on unknown evidence.
func (s *Server) acceptanceStageSkippedOutOfScope(ctx context.Context, runID, stageID uuid.UUID) (bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceSkippedOutOfScope)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			return true, nil
		}
	}
	return false, nil
}

// acceptanceVerdictUnshippedLive reports whether the run's audit chain carries
// a LIVE acceptance_verdict_unshipped marker scoped to stageID (E72.11 /
// #3447), returning the marker's sequence when it does. Liveness is decided by
// audit SEQUENCE, mirroring the outcome-anchoring helpers: with M the newest
// stage-scoped marker, A the stage's newest validation-episode anchor
// (max(latestAcceptanceDispatchSeq, latestAcceptanceEpisodeRestartSeq)) and O
// the newest stage-scoped acceptance_outcome_recorded entry, the marker is live
// iff M > A && M > O. A retry re-open appends an acceptance_reopened marker
// and the re-dispatch appends a fresh acceptance_dispatched anchor, both at
// sequences above M, so the recovery retires the marker by construction; a
// verdict that ships after the marker (O > M) retires it the same way.
//
// Every read error is PROPAGATED so both callers fail closed: the gate never
// resolves a merge-eligible state on an unreadable chain, and the reap handler
// 500s (so the reaper's bounded retry re-attempts) rather than degrading to the
// pre-#3447 silent no-op. latestAcceptanceDispatchSeq collapses its read error
// into found=false by contract, so the dispatched anchor is read directly here
// instead of through it.
func (s *Server) acceptanceVerdictUnshippedLive(ctx context.Context, runID, stageID uuid.UUID) (int64, bool, error) {
	markerSeq, err := s.newestStageScopedSeq(ctx, runID, stageID, CategoryAcceptanceVerdictUnshipped)
	if err != nil {
		return 0, false, err
	}
	if markerSeq == 0 {
		return 0, false, nil
	}
	dispatchSeq, err := s.newestStageScopedSeq(ctx, runID, stageID, CategoryAcceptanceDispatched)
	if err != nil {
		return 0, false, err
	}
	reopenSeq, _, err := s.latestAcceptanceEpisodeRestartSeq(ctx, runID, stageID)
	if err != nil {
		return 0, false, err
	}
	anchor := max(dispatchSeq, reopenSeq)
	outcomeSeq, err := s.newestStageScopedSeq(ctx, runID, stageID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return 0, false, err
	}
	// Anchor comparison: a marker at or below the newest dispatch/reopen anchor
	// belongs to a PRIOR validation episode and is retired.
	if markerSeq <= anchor {
		return markerSeq, false, nil
	}
	// Outcome comparison: a stage-scoped verdict newer than the marker means
	// the verdict shipped after all.
	if markerSeq <= outcomeSeq {
		return markerSeq, false, nil
	}
	return markerSeq, true, nil
}

// appendAcceptanceOutcomeSerialized appends an acceptance_outcome_recorded
// entry under the per-stage admission fence (E72.12 / #3458). The lock is
// orchestrator.LockStageAdmission — the SAME single-process mutex
// host_dispatch.go, TryShortCircuitAcceptance (#1936) and the reap-failure
// marker path (recordAcceptanceVerdictUnshipped) take — so an outcome cannot
// land between the marker path's anchor/outcome/live-marker reads and its
// append, the interleaving that would leave a LIVE marker newer than a shipped
// verdict. The hold is deliberately NARROW: only the AppendChained call, NEVER
// across handleShipAcceptance's triage / fixup dispatch / Advance tail, which
// can re-enter the admission walk for the same stage and would deadlock on the
// non-reentrant mutex. No lock when Orchestrator is nil (behavior unchanged).
func (s *Server) appendAcceptanceOutcomeSerialized(ctx context.Context, stageID uuid.UUID, p audit.ChainAppendParams) (*audit.Entry, error) {
	if s.cfg.Orchestrator != nil {
		unlock := s.cfg.Orchestrator.LockStageAdmission(stageID)
		defer unlock()
	}
	return s.cfg.AuditRepo.AppendChained(ctx, p)
}

// acceptanceStageOf returns the run's acceptance stage from the supplied slice,
// or nil when the workflow declares one but it has not been materialized yet.
func acceptanceStageOf(stages []*run.Stage) *run.Stage {
	for _, st := range stages {
		if st.Type == run.StageTypeAcceptance {
			return st
		}
	}
	return nil
}

// acceptanceValidatedHeadSHA resolves the head the acceptance stage actually
// validated (#1682, binding condition 2): the run's newest recorded head at
// the moment the stage was DISPATCHED. Head-report entries with sequence
// at-or-before the latest acceptance_dispatched entry for THIS stage are the
// candidates; the precedence winner among them (via the shared
// auditcomplete.LatestReportedHeadSHA) is the validated head. Anchoring on the
// dispatch sequence excludes a fixup_pushed / child_pushed that lands AFTER
// dispatch, so the verdict binds to the commit the validator checked out —
// never a later commit that has not been validated.
//
// A decomposed PARENT writes no reported-head entry on its own chain at all
// (see resolveConsolidatedFanInHeadSHA), so when the reported-head walk finds
// nothing the resolver falls back to the parent's own incremental fan-in
// ledger (integration_commit_recorded.merge_sha), BOUNDED by the same dispatch
// anchor (#3091).
//
// A re-opened acceptance stage carries more than one acceptance_dispatched
// entry; the highest-sequence one is the current validation episode.
//
// ANCHOR PROVENANCE (E64.53 / #3174). The anchor has TWO emit sites, split by
// how the stage was actually spawned: orchestrator.Advance writes it for a
// BACKEND-TRIGGERED (github_actions) dispatch, and the host-dispatch marker
// (handleHostDispatchStage) writes it for a LOCAL host spawn, on the
// transitioned arm only. That split is what makes a re-opened stage's
// RE-dispatch advance the anchor: reopenAcceptanceOnFixupPush re-opens the
// settled stage via run.ReopenAcceptanceStage and never calls Advance, so
// before #3174 the local re-dispatch wrote no new entry and the anchor stayed
// pinned at the ORIGINAL dispatch — binding the verdict to the PRE-fix-up head.
// The `e.Sequence <= dispatchSeq` bound below is UNCHANGED by that fix and must
// NOT be loosened: it is what excludes a post-dispatch head from a verdict the
// stage never validated. Two residuals resolve to NO anchor and are therefore
// clamped to `undecidable` by the #3091 head_unresolved path below — a local
// ship that never went through the marker, and a FIRST-dispatch audit-append
// failure at the marker.
//
// The marker's anchor emit stays BEST-EFFORT and NEVER unwinds the dispatch. A
// RE-dispatch whose append fails leaves the PREVIOUS episode's anchor on the
// chain, but that stale anchor is now caught at RESOLUTION time by the
// episode-restart staleness guard below (E64.54 / #3176): an acceptance episode
// restart writes an acceptance_reopened marker BEFORE the re-dispatch, so an
// anchor at or below the newest such marker predates the current episode and is
// treated as no anchor — clamped to undecidable exactly like an absent one. The
// remaining residual, strictly narrower than #3174's: an episode whose
// acceptance_reopened append AND acceptance_dispatched append BOTH failed (the
// same DB fault hitting both writes), for which no restart marker exists and the
// stale anchor still answers. It cannot be closed by any write-based approach,
// since a write is what is failing.
//
// Returns
// ("", false) when no head is recorded at-or-before dispatch, or when the stage
// has no dispatch entry (a bare operator ship with no orchestrator dispatch, or
// a read error) — the caller then CLAMPS the recorded verdict (#3091): any
// `passed` whose validated head cannot be resolved is recorded `undecidable`
// with undecidable_basis=head_unresolved. The clamp is UNCONDITIONAL on an
// unresolvable head, a #2581 retirement-neutralized pass included (#3124) — an
// unbound head names no tree, so retirement (which decides which criteria count)
// cannot rescue it. Option C still fails closed to today's 422 for such an
// unanchored verdict.
func (s *Server) acceptanceValidatedHeadSHA(ctx context.Context, runID, stageID uuid.UUID) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	dispatchSeq, haveAnchor := s.latestAcceptanceDispatchSeq(ctx, runID, stageID)
	if !haveAnchor {
		return "", false
	}
	// EPISODE-RESTART STALENESS GUARD (E64.54 / #3176). The anchor above is the
	// NEWEST acceptance_dispatched entry for the stage, but on a RE-dispatch whose
	// anchor append FAILED (a DB fault at the host-dispatch marker), the newest
	// anchor still on the chain is the PREVIOUS episode's — and the reported-head
	// walk below would then resolve it and bind the verdict to the PRE-fix-up tree,
	// a confident wrong answer where every sibling path is fail-closed (#3174's
	// residual). Every acceptance episode restart writes an acceptance_reopened
	// entry scoped to the stage FIRST (reopenAcceptanceOnFixupPush for the #1682
	// fix-up invalidation, writeAcceptanceReopenAudit for the #1567 operator
	// re-open — the two are asserted to be the only non-test writers by
	// TestAcceptanceReopenedWriters_AreExactlyTheKnownSites). So an anchor at or
	// below the newest restart marker PREDATES the current validation episode and
	// is STALE: treat it exactly as an ABSENT anchor — return ("", false), which
	// the #3091 clamp records as undecidable(basis=head_unresolved). The comparison
	// is a pure within-chain SEQUENCE comparison, never a timestamp: a
	// DB-stamped-vs-Go-side comparison here would be the #3048 cross-clock trap,
	// and its near-tie direction would clamp EVERY verdict to undecidable since the
	// anchor is appended microseconds after the transition. No new audit write is
	// added on the failing path — a write is precisely what already failed.
	restartSeq, haveRestart, rerr := s.latestAcceptanceEpisodeRestartSeq(ctx, runID, stageID)
	if rerr != nil {
		// Fail closed: an unreadable restart ledger is treated as "cannot resolve
		// the validated head", never as "no restart, proceed on the newest anchor".
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance: episode-restart ledger unreadable — treating validated head as unresolvable",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", rerr.Error()))
		return "", false
	}
	if haveRestart && restartSeq >= dispatchSeq {
		// The newest anchor is AT OR BELOW the current validation episode's re-open:
		// it is a prior episode's anchor left in place by a failed re-dispatch
		// append. The comparison is `>=`, not `>`, to match the documented "at or
		// below is stale" contract in all three prose sites (this comment, the
		// function doc, and the README) and to fail CLOSED on the equal case: a
		// store that ever stamped a reopen marker and a dispatch anchor at the SAME
		// sequence (the production append path never does — the marker always
		// precedes the re-dispatch on a unique-per-entry chain, so equality is
		// unreachable there — but the plain auditFake stamps appends at 0) is then
		// treated as stale rather than proceeding on an ambiguous anchor.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance: newest dispatch anchor predates the current episode restart — treating validated head as unresolvable; the verdict will be clamped to undecidable",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.Int64("anchor_seq", dispatchSeq),
			slog.Int64("restart_seq", restartSeq))
		return "", false
	}
	var candidates []*audit.Entry
	for _, cat := range auditcomplete.HeadReportCategoriesByPrecedence {
		es, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return "", false
		}
		for _, e := range es {
			if e.Sequence <= dispatchSeq {
				candidates = append(candidates, e)
			}
		}
	}
	if sha, ok := auditcomplete.LatestReportedHeadSHA(candidates); ok {
		return sha, true
	}
	// Consolidated fan-in fallback (#3091), strictly SUBORDINATE to the
	// reported-head walk above and BOUNDED by the same dispatch anchor: a
	// decomposed parent's own chain carries no reported head at all, so the
	// verdict would otherwise bind to nothing. Bounding on dispatchSeq is what
	// stops a fan-in merge recorded AFTER the acceptance dispatch re-binding
	// the verdict to a tree the stage never validated.
	return s.resolveConsolidatedFanInHeadSHA(ctx, runID, dispatchSeq, true)
}

// latestAcceptanceDispatchSeq returns the highest audit sequence among the
// run's acceptance_dispatched entries scoped to stageID, and whether any exist.
// The dispatch anchor for acceptanceValidatedHeadSHA. Returning the LATEST
// entry is load-bearing for the re-dispatch case (E64.53 / #3174): a fix-up
// re-open leaves the first episode's entry on the chain, and the host-dispatch
// marker appends a second one at the re-spawn, so the newest is the current
// validation episode. A read error is reported
// as (0, false) so the caller treats an unreadable anchor as "no anchor" and
// records an empty head_sha (fail-closed for Option C), never a wrong head.
func (s *Server) latestAcceptanceDispatchSeq(ctx context.Context, runID, stageID uuid.UUID) (int64, bool) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceDispatched)
	if err != nil {
		return 0, false
	}
	var seq int64
	found := false
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			if !found || e.Sequence > seq {
				seq = e.Sequence
				found = true
			}
		}
	}
	return seq, found
}

// latestAcceptanceEpisodeRestartSeq returns the highest audit sequence among the
// run's acceptance_reopened entries scoped to stageID, whether any exist, and a
// read error. The staleness anchor for acceptanceValidatedHeadSHA's
// episode-restart guard (E64.54 / #3176): a reopen marker always precedes the
// re-dispatch anchor on the chain, so an acceptance_dispatched entry at or below
// the newest reopen marker belongs to a PRIOR validation episode.
//
// Unlike latestAcceptanceDispatchSeq this PROPAGATES the read error as a third
// return rather than collapsing it into found=false: the caller must distinguish
// "no restart markers" (a legacy / first-episode chain — proceed on the newest
// anchor) from "unreadable ledger" (fail closed — the validated head cannot be
// trusted), and folding both into found=false would silently PROCEED on an
// unreadable chain, defeating the guard on exactly the DB-fault path it exists to
// handle.
func (s *Server) latestAcceptanceEpisodeRestartSeq(ctx context.Context, runID, stageID uuid.UUID) (int64, bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceReopened)
	if err != nil {
		return 0, false, err
	}
	var seq int64
	found := false
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID {
			if !found || e.Sequence > seq {
				seq = e.Sequence
				found = true
			}
		}
	}
	return seq, found, nil
}

// authorizeAcceptance resolves the request's auth method + actor, mirroring
// authorizeDeployment's dual-auth block: an Ed25519 X-Fishhawk-Signature over
// sha256(body) (runner path — per ADR-050 decision #2 the acceptance agent
// ships via signature auth with NO MCP token) OR a bearer token with the
// existing write:runs scope (operator path). Deliberately NO new scope:
// deploy's write:deploy was a deploy-gate-specific governance tightening;
// acceptance evidence is advisory and adding a scope would trigger the
// Auth-change checklist's impact inventory for zero benefit. On failure it has
// already written the error response and returns ok=false.
func (s *Server) authorizeAcceptance(w http.ResponseWriter, r *http.Request, runID uuid.UUID, body []byte) (authMethod string, actorKind audit.ActorKind, actorSubject *string, ok bool) {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	id := IdentityFrom(r.Context())
	switch {
	case sigHeader != "":
		signature, err := hex.DecodeString(sigHeader)
		if err != nil {
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"X-Fishhawk-Signature is not valid hex",
				map[string]any{"error": err.Error()})
			return "", "", nil, false
		}
		message := signing.ComputeMessage(body)
		if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
			switch {
			case errors.Is(err, signing.ErrNotFound):
				s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
					"no signing key issued for this run", map[string]any{"run_id": runID.String()})
			case errors.Is(err, signing.ErrExpired):
				s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
					"signing key TTL has passed", map[string]any{"run_id": runID.String()})
			case errors.Is(err, signing.ErrSignatureInvalid):
				s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
					"signature does not match the run's stored public key", nil)
			default:
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"signature verification failed", map[string]any{"error": err.Error()})
			}
			return "", "", nil, false
		}
		return "ed25519", audit.ActorKind("system"), nil, true
	case !id.IsAnonymous() && hasScope(id, "write:runs"):
		// ADR-040 D4 (#1027): kind from the token subject — user or agent. The
		// ed25519 signature branch above is the runner path and is NOT
		// scope-gated. No write:acceptance scope (see doc comment).
		subj := id.Subject
		return "bearer", actorKindForSubject(id.Subject), &subj, true
	default:
		s.writeError(w, r, http.StatusUnauthorized, "signature_or_bearer_required",
			"request must include X-Fishhawk-Signature or an authenticated bearer token with write:runs scope", nil)
		return "", "", nil, false
	}
}

// resolveAcceptanceTargetURL is the single named wiring seam for the
// acceptance stage's target-instance URL (ADR-050 decision #1), activated by
// the E31.4/#1532 egress-allowance grammar: it returns the acceptance
// stage's first spec-declared egress target host as a full http(s) URL. A
// schemeless host or host:port gains an http:// prefix so buildAcceptance
// renders a URL (e.g. http://localhost:8090) rather than a bare authority —
// handing the validator the target already in URL form so its verdict's
// target_url does not need the twin decoders' schemeless coercion (the #1574
// class). An egress host that already carries a scheme passes through
// unchanged. A spec with no egress block (a pre-1.3 spec, or one relying on
// the documented interim posture) yields the empty string and buildAcceptance
// renders its explicit not-declared line.
//
// This SUPERSEDES ADR-050 decision #1's verbatim-host posture FOR THE PROMPT
// SEAM ONLY: the prompt text is the sole consumer, and #1574 showed a bare
// host:port here nudges the agent toward emitting a schemeless target_url.
// The sibling resolveAcceptanceEgressTargetHosts KEEPS the verbatim host:port
// grammar unchanged — the egress-proxy allow-list declares hosts, not URLs,
// so no scheme is fabricated there.
func (s *Server) resolveAcceptanceTargetURL(ctx context.Context, runRow *run.Run) string {
	hosts := s.resolveAcceptanceEgressTargetHosts(ctx, runRow)
	if len(hosts) == 0 {
		return ""
	}
	h := hosts[0]
	if strings.Contains(h, "://") {
		return h
	}
	return "http://" + h
}

// resolveAcceptanceEgressTargetHosts returns ALL of the acceptance stage's
// spec-declared egress target hosts (the E31.4/#1532 grammar), in declaration
// order. The full list — not just the first host the prompt-text seam renders
// — is served on the acceptance-stage prompt response as egress_target_hosts:
// the runner's ADR-050 egress-proxy allow-list input (E31.7 / #1535). nil for
// a spec with no egress block, so the response field stays omitted. Unlike
// resolveAcceptanceTargetURL (the prompt seam, which now prefixes http://),
// this KEEPS the verbatim host:port grammar per ADR-050 decision #1 — the
// allow-list matches authorities, not URLs, so no scheme is fabricated.
func (s *Server) resolveAcceptanceEgressTargetHosts(ctx context.Context, runRow *run.Run) []string {
	st, ok := s.resolveAcceptanceStageSpec(ctx, runRow)
	if !ok || st.Egress == nil || len(st.Egress.TargetHosts) == 0 {
		return nil
	}
	return st.Egress.TargetHosts
}

// acceptanceCriteriaIDsFromPlan extracts the approved plan's
// verification.acceptance_criteria ids, in plan order. Served on the
// acceptance-stage prompt response as acceptance_criteria_ids so the runner
// can validate the shipped verdict's criteria[].id join keys against the
// served set (E31.7 / #1535). nil for a nil plan or an empty criteria set,
// so the response field stays omitted.
func acceptanceCriteriaIDsFromPlan(p *plan.Plan) []string {
	if p == nil || len(p.Verification.AcceptanceCriteria) == 0 {
		return nil
	}
	ids := make([]string, 0, len(p.Verification.AcceptanceCriteria))
	for _, c := range p.Verification.AcceptanceCriteria {
		ids = append(ids, c.ID)
	}
	return ids
}

// classifyAcceptanceFailure is the pure triage classifier (E31.8 / #1536): it
// maps a failed verdict's failure_mode plus per-criterion results, resolved
// against the approved plan's acceptance-criteria provenance (explicit vs
// inferred), onto one of four classes. criteria is the approved plan's
// acceptance_criteria (nil/empty when the plan predates the typed contract or
// could not be loaded — provenance cannot be grounded). Returns the class,
// the criterion ids that key the disposition (the E31.11 per-criterion join
// key), and a one-line human-readable reason embedded in the audit payload.
func classifyAcceptanceFailure(acc acceptanceBody, criteria []plan.AcceptanceCriterion) (class string, criterionIDs []string, reason string) {
	// Provenance lookup by criterion id.
	provenance := make(map[string]plan.CriterionSource, len(criteria))
	for _, c := range criteria {
		provenance[c.ID] = c.Source
	}

	// failure_mode=error: the code errored attempting the behavior — it
	// objectively fails, so route to a bounded fix-up pass (class 1). Carry
	// the failed criteria ids when the verdict itemized them.
	if acc.FailureMode == acceptanceFailureError {
		return acceptanceClass1, failedCriterionIDs(acc.normalizedCriteria),
			"failure_mode=error: the code errored attempting the behavior; routing to a bounded fix-up pass"
	}

	// Replayed-scenario regression (E72.4 / #3328): a FAILED scenario row is a
	// prior run's recorded behaviour objectively broken by this head — class 1,
	// a bounded fix-up pass, keyed on the scenario ids (plus any failed criterion
	// ids so the disposition names everything that failed). Scenario rows never
	// enter the skip partition below: a skipped or undecidable replay is a
	// replay-budget signal, not an environment flake, so it can never route
	// class 2 / class 5 on its own.
	if regressed := failedCriterionIDs(acc.scenarioRows); len(regressed) > 0 {
		ids := append(regressed, failedCriterionIDs(acc.normalizedCriteria)...)
		return acceptanceClass1, ids,
			"assertion_fail: a replayed scenario regressed; the code objectively breaks behaviour a prior run recorded — routing to a bounded fix-up pass"
	}

	// assertion_fail (validate() guarantees failure_mode is error or
	// assertion_fail on a failed verdict). Partition the criteria results.
	var failed []string
	skipped := 0
	for _, c := range acc.normalizedCriteria {
		switch c.Result {
		case acceptanceResultFailed:
			failed = append(failed, c.ID)
		case acceptanceResultSkipped:
			skipped++
		}
	}

	if len(failed) > 0 {
		// Resolve every failed id against the plan provenance.
		var inferredOrUnresolvable []string
		allExplicit := true
		for _, id := range failed {
			src, ok := provenance[id]
			if !ok || src != plan.CriterionSourceExplicit {
				allExplicit = false
				inferredOrUnresolvable = append(inferredOrUnresolvable, id)
			}
		}
		if allExplicit {
			// Every failed criterion is explicit-source — the code objectively
			// fails a stated criterion (class 1).
			return acceptanceClass1, failed,
				"assertion_fail: every failed criterion is explicit-source; the code objectively fails a stated criterion"
		}
		// At least one failed criterion is inferred-source or unresolvable — a
		// bad/ambiguous criterion (class 3). The criterion_ids record the
		// per-criterion disposition E31.11 consumes.
		return acceptanceClass3, inferredOrUnresolvable,
			"assertion_fail: a failed criterion is inferred-source or unresolvable against the plan (bad/ambiguous criterion)"
	}

	// No failed criteria but ≥1 skip. Normally an environment/flake signal
	// (class 2, bounded re-run). Split off the posture-A can't-exhibit shape:
	// when EVERY skipped criterion carries a non-empty expectation_basis (the
	// #1612 signal that the egress-sandboxed acceptance agent could not produce
	// the external trigger — e.g. closing a GitHub issue), the re-run is
	// deterministically futile (the sandbox still can't reach the external
	// service). Route to the terminal class 5 that pages WITHOUT re-opening the
	// stage. A skip that lacks expectation_basis is genuinely ambiguous and
	// keeps the bounded class-2 flake path unchanged (#1671).
	if skipped > 0 {
		if allSkipsCarryExpectationBasis(acc.normalizedCriteria) {
			return acceptanceClass5, nil,
				"assertion_fail: no criterion failed and every skipped criterion is a posture-A can't-exhibit skip with expectation_basis; the external trigger cannot be produced in the default-deny egress sandbox — routing to a terminal page rather than a futile flake retry"
		}
		return acceptanceClass2, nil,
			"assertion_fail: no criterion failed but at least one was skipped; validation could not complete (environment/flake signal)"
	}

	// F empty and no skips, OR the plan carries no acceptance_criteria to
	// ground provenance: unitemized / provenance-ungroundable failure —
	// works-as-planned, disputed (class 4).
	return acceptanceClass4, nil,
		"unitemized or provenance-ungroundable failure; works-as-planned/disputed — paging the human"
}

// acceptanceReplayPayload renders the acceptance_outcome_recorded `replay`
// block (E72.4 / #3328): nil (JSON null) when the body carried no runner
// replay; otherwise the header fields copied VERBATIM from body.replay plus
// scenarios_passed / scenarios_failed / scenarios_skipped (skipped +
// undecidable) counted from the scenario rows, and served_mismatch:true when
// the runner's attribution list length disagrees with its own served count.
func acceptanceReplayPayload(acc acceptanceBody) map[string]any {
	if acc.Replay == nil {
		return nil
	}
	passed, failed, skipped, undecidable, _ := acceptanceCriteriaTally(acc.scenarioRows)
	return map[string]any{
		"cap":               acc.Replay.Cap,
		"corpus_size":       acc.Replay.CorpusSize,
		"served":            acc.Replay.Served,
		"sampled_out":       acc.Replay.SampledOut,
		"retired_excluded":  acc.Replay.RetiredExcluded,
		"seed":              acc.Replay.Seed,
		"scenarios_passed":  passed,
		"scenarios_failed":  failed,
		"scenarios_skipped": skipped + undecidable,
		"served_mismatch":   len(acc.Replay.Scenarios) != acc.Replay.Served,
	}
}

// recordAcceptanceScenarioRegressions appends one acceptance_scenario_regression
// chained entry per FAILED scenario row (E72.4 / #3328). origin_pr /
// origin_issue / origin_run_id / path come ONLY from the matching
// body.replay.scenarios entry: a row with no entry, or an entry whose
// origin_pr is 0, records origin_pr ABSENT and origin_unresolved:true — never
// the trigger issue number, never prose. Best-effort: every failure WARN-logs.
func (s *Server) recordAcceptanceScenarioRegressions(ctx context.Context, runID, stageID uuid.UUID, artifactID string, acc acceptanceBody, actorKind audit.ActorKind, actorSubject *string) {
	byID := map[string]acceptanceReplayedScenario{}
	if acc.Replay != nil {
		for _, e := range acc.Replay.Scenarios {
			byID[e.ScenarioID] = e
		}
	}
	for _, row := range acc.scenarioRows {
		if row.Result != acceptanceResultFailed {
			continue
		}
		fields := map[string]any{
			"run_id":       runID.String(),
			"stage_id":     stageID.String(),
			"artifact_id":  artifactID,
			"scenario_id":  row.ID,
			"observed":     row.Observed,
			"expected":     row.Expected,
			"repro_handle": row.ReproHandle,
		}
		entry, attributed := byID[row.ID]
		switch {
		case attributed && entry.OriginPR > 0:
			fields["origin_pr"] = entry.OriginPR
			fields["origin_issue"] = entry.OriginIssue
			fields["origin_run_id"] = entry.OriginRunID
			fields["path"] = entry.Path
		case attributed:
			fields["origin_issue"] = entry.OriginIssue
			fields["origin_run_id"] = entry.OriginRunID
			fields["path"] = entry.Path
			fields["origin_unresolved"] = true
		default:
			fields["origin_unresolved"] = true
		}
		payload, _ := json.Marshal(fields)
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:        runID,
			StageID:      &stageID,
			Timestamp:    time.Now().UTC(),
			Category:     CategoryAcceptanceScenarioRegression,
			ActorKind:    &actorKind,
			ActorSubject: actorSubject,
			Payload:      payload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"acceptance: append scenario regression entry failed",
				slog.String("run_id", runID.String()),
				slog.String("scenario_id", row.ID),
				slog.String("error", err.Error()))
		}
	}
}

// scenarioRegressionOriginNote renders the concern-note attribution for a
// failed replayed scenario: the recording PR when known, otherwise an explicit
// unknown — never the trigger issue number.
func scenarioRegressionOriginNote(acc acceptanceBody, scenarioID string) string {
	if acc.Replay != nil {
		for _, e := range acc.Replay.Scenarios {
			if e.ScenarioID == scenarioID && e.OriginPR > 0 {
				return fmt.Sprintf("Regression against scenario %s recorded by PR #%d", scenarioID, e.OriginPR)
			}
		}
	}
	return fmt.Sprintf("Regression against scenario %s (originating PR unknown)", scenarioID)
}

// failedCriterionIDs returns the ids of criteria whose result is failed, in
// order.
func failedCriterionIDs(criteria []acceptanceCriterionResult) []string {
	var ids []string
	for _, c := range criteria {
		if c.Result == acceptanceResultFailed {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// allSkipsCarryExpectationBasis reports whether every skipped criterion in the
// verdict carries a non-empty expectation_basis — the #1612 posture-A
// can't-exhibit discriminator that separates a deterministically-futile
// externally-unvalidatable skip (class 5) from a genuinely ambiguous flake
// skip (class 2). Callers gate on skipped>0 first, so an all-passed set (which
// makes this vacuously true) never reaches here.
func allSkipsCarryExpectationBasis(criteria []acceptanceCriterionResult) bool {
	for _, c := range criteria {
		if c.Result == acceptanceResultSkipped && strings.TrimSpace(c.ExpectationBasis) == "" {
			return false
		}
	}
	return true
}

// triageAcceptanceFailure routes a freshly persisted verdict:failed artifact
// (E31.8 / #1536). Called from handleShipAcceptance ONLY on the fresh-create
// path (never the idempotent replay) and only when acc.Verdict==failed. It is
// best-effort relative to the ship: every internal error WARN-logs and never
// unwinds the 201 / artifact / outcome audit. It ALWAYS ends by writing ONE
// acceptance_triage_decided chained entry recording what actually happened.
//
// Returns the realized disposition so the caller can gate the immediate
// page-class hook on whether triage actually PAGED (#1786) — an auto-routed
// fixup_dispatched / retry_dispatched disposition writes no page-class event,
// so firing the hook then would flush an older unpinged event at an unrelated
// moment. acceptanceDispositionPages classifies the returned value.
func (s *Server) triageAcceptanceFailure(ctx context.Context, runID uuid.UUID, stage *run.Stage, acc acceptanceBody, artifactID string) string {
	// Load the approved plan for provenance grounding (nil-tolerant → the
	// classifier grounds class 4).
	var criteria []plan.AcceptanceCriterion
	if p, err := s.loadApprovedPlanForRun(ctx, runID); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: load approved plan failed; grounding provenance as absent",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	} else if p != nil {
		criteria = p.Verification.AcceptanceCriteria
	}

	class, criterionIDs, reason := classifyAcceptanceFailure(acc, criteria)

	// E31.11 (#1539, ADR-049 decision #4): a class-3 decision is a
	// plan-review miss — a bad criterion the plan gate approved. Build the
	// durable per-criterion record ONCE here so every disposition branch
	// below (paged, unsettled, budget-exhausted) carries it. nil for
	// classes 1/2/4, so the payload field stays omitted there.
	var misses []agenteval.PlanReviewMiss
	if class == acceptanceClass3 {
		misses = buildPlanReviewMisses(acc, criteria, criterionIDs)
	}

	// Count prior auto-routed decisions from the audit chain (the durable
	// mirror of countFixupPasses). A count failure means we cannot bound
	// safely — degrade to paged without acting.
	prior, err := s.countAcceptanceTriageRoutes(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: count prior routed decisions failed; paging without action",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionPaged, criterionIDs, acc.FailureMode, prior,
			"triage route count failed; paging without action", misses)
		return acceptanceDispositionPaged
	}

	// Defensive settle check: an operator-bearer ship may race the trace-bundle
	// settle. If the acceptance stage row is not yet succeeded, record the
	// classification with unsettled_paged instead of acting.
	if stage.State != run.StageStateSucceeded {
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionUnsettled, criterionIDs, acc.FailureMode, prior,
			fmt.Sprintf("acceptance stage not yet settled (state %q); recording classification without acting", stage.State), misses)
		return acceptanceDispositionUnsettled
	}

	// Re-run bound: at the cap keep the classified class but degrade to a paged
	// variant so non-convergence lands on the human.
	if prior >= defaultMaxAcceptanceReruns {
		s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
			acceptanceDispositionRerunBudget, criterionIDs, acc.FailureMode, prior,
			fmt.Sprintf("re-run budget exhausted (%d of %d auto-routed passes used); paging", prior, defaultMaxAcceptanceReruns), misses)
		return acceptanceDispositionRerunBudget
	}

	// Route by class. Class 3 / class 4 take NO state transition — page. Class 5
	// (all-skip externally-unvalidatable) is ALSO terminal, no transition: it
	// pages under the distinct externally_unvalidatable_paged token so the
	// acceptance stage stays succeeded and never enters the futile class-2 retry
	// loop (#1671). Because it never re-opens the stage it never contributes to
	// the auto-routed count defaultMaxAcceptanceReruns bounds.
	var disposition string
	switch class {
	case acceptanceClass1:
		disposition = s.routeAcceptanceClass1(ctx, runID, stage, acc, criteria, criterionIDs, reason)
	case acceptanceClass2:
		disposition = s.routeAcceptanceClass2(ctx, runID, stage)
	case acceptanceClass5:
		disposition = acceptanceDispositionUnvalidatable
	default:
		disposition = acceptanceDispositionPaged
	}

	s.writeAcceptanceTriageAudit(ctx, runID, stage.ID, artifactID, class,
		disposition, criterionIDs, acc.FailureMode, prior, reason, misses)
	return disposition
}

// acceptanceDispositionPages reports whether a triage disposition is one that
// PAGES a human — the exact set issuecomment/ping.go's acceptanceTriageNeedsHuman
// keys the acceptance_triage_decided page-class event on (#1786). The
// auto-routed fixup_dispatched / retry_dispatched dispositions (and a passed
// verdict, which triages nothing) return false, so handleShipAcceptance skips
// the immediate page-class hook for them rather than flushing an older unpinged
// event at an unrelated moment.
func acceptanceDispositionPages(disposition string) bool {
	switch disposition {
	case acceptanceDispositionPaged, acceptanceDispositionRerunBudget,
		acceptanceDispositionFixupUnavailable, acceptanceDispositionRetryUnavailable,
		acceptanceDispositionUnsettled, acceptanceDispositionUnvalidatable:
		return true
	default:
		return false
	}
}

// buildPlanReviewMisses joins each class-3 criterion id with the approved
// plan criterion's provenance fields and the shipped verdict's per-criterion
// evidence for that id (E31.11 / #1539). An id that does not resolve against
// the plan still yields a record keyed by the id with empty provenance
// fields — unresolvable is itself the miss. Uses the shared
// agenteval.PlanReviewMiss wire type so the server marshal, the
// distill-corpus tool unmarshal, and the corpus loader cannot drift.
func buildPlanReviewMisses(acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string) []agenteval.PlanReviewMiss {
	planByID := make(map[string]plan.AcceptanceCriterion, len(criteria))
	for _, c := range criteria {
		planByID[c.ID] = c
	}
	resultByID := make(map[string]acceptanceCriterionResult, len(acc.normalizedCriteria))
	for _, c := range acc.normalizedCriteria {
		resultByID[c.ID] = c
	}

	out := make([]agenteval.PlanReviewMiss, 0, len(criterionIDs))
	for _, id := range criterionIDs {
		m := agenteval.PlanReviewMiss{CriterionID: id}
		if pc, ok := planByID[id]; ok {
			m.Statement = pc.Statement
			m.Source = string(pc.Source)
			m.SourceRef = pc.SourceRef
			m.Rationale = pc.Rationale
		}
		if r, ok := resultByID[id]; ok {
			m.Observed = r.Observed
			m.Expected = r.Expected
			m.StepsTaken = r.StepsTaken
			m.ExpectationBasis = r.ExpectationBasis
			m.ReproHandle = r.ReproHandle
			m.Result = r.Result
		}
		out = append(out, m)
	}
	return out
}

// routeAcceptanceClass1 synthesizes the behavioral evidence into
// implement-stage fix-up concerns and routes them via the existing
// fixupStageAs under a token-less system identity, with the triggering
// acceptance stage re-opened (FixupOptions.AcceptanceStageID). Returns the
// disposition: fixup_dispatched on success, fixup_unavailable_paged on ANY
// routing refusal (implement stage not found, budget/ceiling exhausted, stage
// not applicable) so the disposition always lands on the human at the cap.
func (s *Server) routeAcceptanceClass1(ctx context.Context, runID uuid.UUID, stage *run.Stage, acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string, reason string) string {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: list stages failed; paging",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return acceptanceDispositionFixupUnavailable
	}
	var implement *run.Stage
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			implement = st
			break
		}
	}
	if implement == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: no implement stage on run; paging",
			slog.String("run_id", runID.String()))
		return acceptanceDispositionFixupUnavailable
	}

	selected := synthesizeAcceptanceConcerns(acc, criteria, criterionIDs, reason)

	priorPasses, err := s.countFixupPasses(ctx, runID, implement.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: count fixup passes failed; paging",
			slog.String("run_id", runID.String()), slog.String("error", err.Error()))
		return acceptanceDispositionFixupUnavailable
	}

	acceptanceStageID := stage.ID
	// The PR-body-unsatisfiable set (#2782) is unused on the acceptance-triage
	// path — there is no operator tool result to warn on — so it is discarded;
	// the advisory audit entry is still written inside fixupStageAs.
	dec, _, ferr := s.fixupStageAs(ctx, Identity{Subject: acceptanceTriageSystemSubject}, fixupActionParams{
		StageID: implement.ID,
		Options: run.FixupOptions{
			PriorPassCount:    priorPasses,
			MaxPasses:         defaultMaxFixupPasses,
			HardCeiling:       defaultFixupCeiling,
			AcceptanceStageID: &acceptanceStageID,
		},
		Selected:    selected,
		PriorPasses: priorPasses,
		Reason:      reason,
	})
	if ferr != nil {
		// A refusal (ErrFixupBudgetExhausted / ErrFixupCeilingReached /
		// ErrFixupNotApplicable) or any other error degrades to a paged
		// disposition — the implement fixup budget therefore ALSO bounds
		// acceptance-driven passes.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: fixup route refused; paging",
			slog.String("run_id", runID.String()),
			slog.String("implement_stage_id", implement.ID.String()),
			slog.String("error", ferr.Error()))
		return acceptanceDispositionFixupUnavailable
	}
	// Read the acceptance-driven decision field (E31.8): the fixup helper
	// passed AcceptanceStageID through unchanged and re-opened the settled
	// acceptance stage. A nil here means the re-open did not fire as expected
	// (the acceptance stage was not settled at fixup time) — the fix-up still
	// dispatched, so log and proceed.
	if dec != nil && dec.ReopenedAcceptance == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-1: fix-up dispatched but acceptance stage was not re-opened",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", acceptanceStageID.String()))
	}
	return acceptanceDispositionFixupDispatched
}

// routeAcceptanceClass2 re-opens the settled acceptance stage (class-2:
// environment/flake) via run.ReopenAcceptanceStage, then runs the retry-shaped
// post-transition steps (orchestrator Advance, WARN-on-error; status notify).
// Returns retry_dispatched on success, retry_unavailable_paged on a reopen
// refusal.
func (s *Server) routeAcceptanceClass2(ctx context.Context, runID uuid.UUID, stage *run.Stage) string {
	dec, err := run.ReopenAcceptanceStage(ctx, s.cfg.RunRepo, stage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage class-2: acceptance re-open refused; paging",
			slog.String("run_id", runID.String()),
			slog.String("acceptance_stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return acceptanceDispositionRetryUnavailable
	}
	// Hand off to the orchestrator so it walks pending → dispatched and
	// rebuilds a fresh preview. WARN-on-error: the stage stays pending for a
	// manual re-fire, mirroring the retry handler.
	if dec.Stage.State == run.StageStatePending && s.cfg.Orchestrator != nil {
		if _, aerr := s.cfg.Orchestrator.Advance(ctx, runID); aerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"acceptance triage class-2: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("error", aerr.Error()))
		}
	}
	s.notifyStatusUpdate(ctx, runID, "acceptance_triage_reopen")
	return acceptanceDispositionRetryDispatched
}

// synthesizeAcceptanceConcerns builds the []planreview.Concern the class-1
// fix-up routes back to the implement agent: one per failed criterion with the
// behavioral evidence (observed/expected/steps_taken/expectation_basis/
// repro_handle) the verdict carried, composed with the plan criterion
// statement. When the verdict itemized nothing (an error verdict with no
// per-criterion results), a single concern is synthesized from the
// failure_mode / target_url / criteria tally.
func synthesizeAcceptanceConcerns(acc acceptanceBody, criteria []plan.AcceptanceCriterion, criterionIDs []string, reason string) []planreview.Concern {
	statementByID := make(map[string]string, len(criteria))
	for _, c := range criteria {
		statementByID[c.ID] = c.Statement
	}
	failedByID := make(map[string]acceptanceCriterionResult, len(acc.normalizedCriteria)+len(acc.scenarioRows))
	for _, c := range acc.normalizedCriteria {
		if c.Result == acceptanceResultFailed {
			failedByID[c.ID] = c
		}
	}
	// Failed replayed-scenario rows (E72.4) synthesize a concern too, with the
	// recording PR (or an explicit unknown) as the statement line so the fix-up
	// agent sees which prior behaviour regressed.
	for _, c := range acc.scenarioRows {
		if c.Result == acceptanceResultFailed {
			failedByID[c.ID] = c
			statementByID[c.ID] = scenarioRegressionOriginNote(acc, c.ID)
		}
	}

	var out []planreview.Concern
	for _, id := range criterionIDs {
		c, ok := failedByID[id]
		if !ok {
			continue
		}
		out = append(out, planreview.Concern{
			Severity: planreview.SeverityHigh,
			Category: "acceptance",
			Note:     composeAcceptanceConcernNote(c, statementByID[id]),
			// Provenance marks this concern as synthesized from the acceptance
			// agent's attacker-influenceable free-text so the fix-up renderer
			// quarantines it (ADR-050 / E31.8 / #1613). Category stays the
			// display classifier; Provenance is the authoritative structured
			// marker rather than the fragile Category-string coupling.
			Provenance: planreview.ConcernProvenanceAcceptance,
		})
	}
	if len(out) == 0 {
		out = append(out, planreview.Concern{
			Severity:   planreview.SeverityHigh,
			Category:   "acceptance",
			Note:       composeAcceptanceFallbackNote(acc, reason),
			Provenance: planreview.ConcernProvenanceAcceptance,
		})
	}
	return out
}

// composeAcceptanceConcernNote renders one failed criterion's behavioral
// evidence into a fix-up concern note.
func composeAcceptanceConcernNote(c acceptanceCriterionResult, statement string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Acceptance criterion %q failed validation.", c.ID)
	if statement != "" {
		fmt.Fprintf(&b, " Criterion: %s", statement)
	}
	if c.Observed != "" {
		fmt.Fprintf(&b, " Observed: %s", c.Observed)
	}
	if c.Expected != "" {
		fmt.Fprintf(&b, " Expected: %s", c.Expected)
	}
	if c.StepsTaken != "" {
		fmt.Fprintf(&b, " Steps taken: %s", c.StepsTaken)
	}
	if c.ExpectationBasis != "" {
		fmt.Fprintf(&b, " Expectation basis: %s", c.ExpectationBasis)
	}
	if c.ReproHandle != "" {
		fmt.Fprintf(&b, " Repro: %s", c.ReproHandle)
	}
	return b.String()
}

// composeAcceptanceFallbackNote renders a single fix-up concern from the
// verdict envelope when no per-criterion evidence was itemized.
func composeAcceptanceFallbackNote(acc acceptanceBody, reason string) string {
	var b strings.Builder
	b.WriteString("Acceptance validation failed and requires a fix-up.")
	if acc.FailureMode != "" {
		fmt.Fprintf(&b, " Failure mode: %s.", acc.FailureMode)
	}
	if acc.TargetURL != "" {
		fmt.Fprintf(&b, " Target: %s.", acc.TargetURL)
	}
	passed, failed, skipped, undecidable, total := acceptanceCriteriaTally(acc.normalizedCriteria)
	fmt.Fprintf(&b, " Criteria tally: %d passed / %d failed / %d skipped / %d undecidable of %d.",
		passed, failed, skipped, undecidable, total)
	if reason != "" {
		fmt.Fprintf(&b, " Triage: %s", reason)
	}
	return b.String()
}

// countAcceptanceTriageRoutes counts the run's prior acceptance_triage_decided
// entries whose disposition auto-routed (fixup_dispatched | retry_dispatched)
// — the durable mirror of countFixupPasses that bounds re-runs across
// restarts.
func (s *Server) countAcceptanceTriageRoutes(ctx context.Context, runID uuid.UUID) (int, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryAcceptanceTriageDecided)
	if err != nil {
		return 0, fmt.Errorf("list %s audit entries: %w", CategoryAcceptanceTriageDecided, err)
	}
	n := 0
	for _, e := range entries {
		switch acceptanceTriageDispositionOf(e.Payload) {
		case acceptanceDispositionFixupDispatched, acceptanceDispositionRetryDispatched:
			n++
		}
	}
	return n, nil
}

// acceptanceTriageDispositionOf reads the `disposition` field from an
// acceptance_triage_decided payload. Empty on any decode failure.
func acceptanceTriageDispositionOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Disposition string `json:"disposition"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Disposition
}

// writeAcceptanceTriageAudit appends the single acceptance_triage_decided
// chained entry recording the class + realized disposition + criterion_ids +
// bound accounting. Written AFTER acting so the disposition records what
// actually happened. Best-effort: a failure here WARN-logs (the ship is
// already committed). misses is the E31.11 per-criterion plan-review-miss
// record — additive: emitted as the plan_review_miss payload field only when
// non-empty (class 3), omitted entirely otherwise, so existing consumers
// (issuecomment decodeAcceptanceActivity, acceptanceTriageDispositionOf)
// that decode named fields are untouched.
func (s *Server) writeAcceptanceTriageAudit(ctx context.Context, runID, stageID uuid.UUID, artifactID, class, disposition string, criterionIDs []string, failureMode string, priorRoutedPasses int, reason string, misses []agenteval.PlanReviewMiss) {
	if criterionIDs == nil {
		criterionIDs = []string{}
	}
	systemKind := audit.ActorSystem
	fields := map[string]any{
		"run_id":              runID.String(),
		"stage_id":            stageID.String(),
		"artifact_id":         artifactID,
		"class":               class,
		"disposition":         disposition,
		"criterion_ids":       criterionIDs,
		"failure_mode":        failureMode,
		"prior_routed_passes": priorRoutedPasses,
		"reason":              reason,
	}
	if len(misses) > 0 {
		fields["plan_review_miss"] = misses
	}
	payload, _ := json.Marshal(fields)
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAcceptanceTriageDecided,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"acceptance triage: append acceptance_triage_decided audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}
}

// acceptanceResponse echoes the persisted artifact's identity back to the
// caller. Verdict + failure_mode are surfaced explicitly (even though they
// live in the artifact body) as the most operator-useful correlation fields.
type acceptanceResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	Verdict     string    `json:"verdict"`
	FailureMode string    `json:"failure_mode,omitempty"`
	// EffectiveVerdict is populated whenever the RECORDED verdict differs from the
	// one the producer shipped: a #2581 retirement neutralization (failed →
	// passed), a #2512 ladder derivation (a shipped passed carrying a failed /
	// undecidable row), a #3091 unbound-head clamp (→ undecidable), or a #3397
	// verified-nothing verdict (a shipped passed whose rows are all-skip / empty →
	// not_validated). Verdict/FailureMode keep echoing what the producer shipped;
	// this field names what the governance record settled on. Omitted when the
	// recorded verdict IS the shipped one, so a deployed runner sees a
	// byte-identical body. Additive for that runner either way:
	// runner/internal/upload decodes the 200/201 body with a plain json.Decoder
	// and does NOT call DisallowUnknownFields.
	EffectiveVerdict string `json:"effective_verdict,omitempty"`
	Idempotent       bool   `json:"idempotent"`
}
