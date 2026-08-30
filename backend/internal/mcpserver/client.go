package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// apiClient is the MCP server's typed wrapper around the Fishhawk
// backend HTTP API. Intentionally a thin local copy rather than an
// import of `cli/internal/httpclient` so the dependency direction
// stays clean (backend → cli would invert the module hierarchy and
// force a published cli version on every backend release).
//
// Only the slice of endpoints the MCP tools actually need lives
// here. Subsequent tool tickets (E19.4 / #344, E19.5 / #345,
// E19.6 / #346) extend this surface as they land.
type apiClient struct {
	baseURL string
	token   string
	// http is the 30s short client for every read/decide/file/direct-edit
	// arm — bodies return in well under a second.
	http *http.Client
	// httpLong is the minutes-long client for the two agent-backed refinement
	// arms (create-session, brief-amendment). A drafting-agent call is a
	// multi-minute LLM inference, so the 30s short client aborts it mid-flight
	// (aborting the request context and, server-side, killing the drafter).
	httpLong *http.Client
}

// refinementDraftClientTimeout bounds the MCP client's wait on the two
// agent-backed refinement arms (open + brief_amendment). It is set a couple of
// minutes ABOVE the server-side drafting budget (refinementDraftBudget, 20m in
// backend/internal/server/refinement.go) so the client waits for the server's
// own bounded response/error instead of aborting first. The 20m server budget
// is anchored to planreview's review-budget Cap (1200s /
// backend/internal/planreview/budget.go).
const refinementDraftClientTimeout = 22 * time.Minute

func newAPIClient(cfg config) *apiClient {
	return &apiClient{
		baseURL:  cfg.backendURL,
		token:    cfg.apiToken,
		http:     &http.Client{Timeout: 30 * time.Second},
		httpLong: &http.Client{Timeout: refinementDraftClientTimeout},
	}
}

// apiError is the typed form of the OpenAPI error envelope. Mirrors
// the CLI's *APIError so the wire shape stays consistent across
// surfaces; callers errors.As into this to switch on Code.
type apiError struct {
	StatusCode int
	Code       string
	Message    string
	Details    map[string]any
	// ErrorRef is the 5xx correlation handle (E67.15 / #2587): the backend's
	// error_ref envelope field, equal to the response X-Request-ID. On a 5xx
	// the backend redacts the raw cause out of Details and hands it back only
	// in the operator's server log keyed by this ref; callers surface the ref
	// so an operator can join the two. Empty on 4xx / a pre-#2587 backend.
	ErrorRef string
}

func (e *apiError) Error() string {
	var base string
	if e.Code == "" {
		base = fmt.Sprintf("fishhawk: HTTP %d", e.StatusCode)
	} else {
		base = fmt.Sprintf("fishhawk: HTTP %d (%s): %s", e.StatusCode, e.Code, e.Message)
	}
	// Append the parsed Details map when present so callers that render the
	// error via %v (e.g. run_children's between-wave integrate-wave transport
	// warning) surface the allow-listed structured detail keys instead of an
	// opaque HTTP-status stop. encoding/json marshals map keys in sorted order,
	// so the suffix is deterministic for tests. A nil/empty map appends nothing.
	// Post-#2587 the raw cause no longer rides Details on a 5xx — it is redacted
	// server-side; the operator recovers it from the log keyed by error_ref.
	if len(e.Details) > 0 {
		if b, err := json.Marshal(e.Details); err == nil {
			base += "; details: " + string(b)
		}
	}
	if e.ErrorRef != "" {
		base += " (error_ref=" + e.ErrorRef + ")"
	}
	return base
}

// Run mirrors the OpenAPI Run schema's wire shape. Subset: the MCP
// tools surface every operator-relevant field, but skip internal
// bookkeeping (workflow_sha etc.) the agent has no use for. JSON
// tags match the backend exactly so the renderer in tools.go can
// pass the decoded struct straight back to the MCP client without
// re-mapping.
//
// IDs are typed as `string` rather than `uuid.UUID` so the MCP
// SDK's auto-generated JSON schema (which uses reflection over the
// Go type) sees a string. `uuid.UUID` is a 16-byte array under the
// hood, which would surface in the schema as `type: array` and
// fail the SDK's response validation at the wire boundary — even
// though the JSON payload itself is a string. Tools that need a
// typed UUID parse the string locally (e.g. `uuid.Parse(in.RunID)`).
type Run struct {
	ID            string  `json:"id"`
	Repo          string  `json:"repo"`
	WorkflowID    string  `json:"workflow_id"`
	WorkflowSHA   string  `json:"workflow_sha"`
	TriggerSource string  `json:"trigger_source"`
	TriggerRef    *string `json:"trigger_ref"`
	State         string  `json:"state"`
	ParentRunID   *string `json:"parent_run_id"`
	// DecomposedFrom mirrors the backend runResponse.decomposed_from: the
	// parent run this row was minted as a fan-out child of (E48.99 / #2546).
	// nil for a non-decomposed run. The json tag MUST byte-match the backend
	// or the field silently decodes to nil (the #371 hand-maintained-wire-
	// mirror trap).
	DecomposedFrom *string `json:"decomposed_from,omitempty"`
	// SliceIndex mirrors the backend runResponse.slice_index: the decomposed
	// child's 0-based sub_plan position (E24.1 / #1141, surfaced by #2546).
	// nil for a non-child run. A *int so slice 0 is not confused with absent.
	// The json tag MUST byte-match the backend or the field silently decodes
	// to nil.
	SliceIndex *int `json:"slice_index,omitempty"`
	// SliceDependsOn mirrors the backend runResponse.slice_depends_on: the
	// decomposed child's declared dependency slice indices, resolved from the
	// parent's approved plan (E48.99 / #2546). The backend emits it on the
	// SINGLE-run read only (handleGetRun) — the list endpoint omits it (the
	// no-N+1 split) — so it is populated when this Run came from GetRun and nil
	// otherwise. childrenStatusFor reads it to compute per-child blocked /
	// blocked_by. An OLDER backend omits it entirely (nil-decode, the
	// mixed-version degrade). The json tag MUST byte-match the backend or the
	// field silently decodes to nil.
	SliceDependsOn     []int   `json:"slice_depends_on,omitempty"`
	UpstreamRunID      *string `json:"upstream_run_id,omitempty"`
	PullRequestURL     *string `json:"pull_request_url"`
	RetryAttempt       int     `json:"retry_attempt"`
	MaxRetriesSnapshot int     `json:"max_retries_snapshot"`
	RunnerKind         string  `json:"runner_kind,omitempty"`
	// RunnerKindResolved mirrors GET /v0/runs/{id}'s lock flag (#1355):
	// true once the run's first signed runner self-report LOCKED runner_kind
	// (#1346/#1348). The host-dispatch guard (guardHostDispatch) reads it to
	// reject a local host dispatch against a github_actions-locked run.
	RunnerKindResolved bool          `json:"runner_kind_resolved,omitempty"`
	IssueContext       *IssueContext `json:"issue_context,omitempty"`
	// Concerns is the run's OPEN review-concern summary (#964), mirrored
	// from GET /v0/runs/{run_id}: count, per-state breakdown, and the
	// stable concern IDs fishhawk_fixup_stage's concern_ids addressing
	// needs. The backend emits it on the single-run read only; omitted
	// when the run has no open concerns.
	Concerns *RunConcerns `json:"concerns,omitempty" jsonschema:"OPEN review concerns for the run: open count, by_state breakdown, and items carrying the stable concern IDs fishhawk_fixup_stage's concern_ids parameter addresses. Omitted when nothing is open"`
	// LiveValidation mirrors the backend run-status surface (#2045, E48.35):
	// the run's pending operator live-validation walk — the count of
	// requires_live_validation acceptance criteria awaiting an operator's live
	// check plus the walk work item that tracks it. The backend emits it on the
	// single-run read only (handleGetRun); omitted (nil) when the run carried no
	// requires_live_validation criterion. An OLDER backend omits it entirely, so
	// it decodes to nil and next_actions renders nothing — the mixed-version
	// degrade. The json tag MUST byte-match the backend's runLiveValidationPayload
	// or the field silently decodes to nil.
	LiveValidation *RunLiveValidation `json:"live_validation,omitempty" jsonschema:"the run's pending operator live-validation walk (count of requires_live_validation criteria + the tracking walk ref). Omitted when the run carries no such criterion"`
	// ReviewAuthority mirrors the backend run-status surface (E53.2 / #2225):
	// for each stage of the run's workflow that declares a reviewers block, the
	// resolved review authority mode (advisory | gating | gateless) and its
	// provenance (declared | derived), so the operator reads the mode instead
	// of re-deriving it from the reviewer counts. The backend emits it on the
	// single-run read only (handleGetRun); omitted (nil) when the run carries
	// no cached spec or declares no reviewers block. An OLDER backend omits it
	// entirely, so it decodes to nil — the mixed-version degrade. The json tags
	// MUST byte-match the backend's runReviewAuthorityPayload or the field
	// silently decodes to nil.
	ReviewAuthority []RunReviewAuthority `json:"review_authority,omitempty" jsonschema:"per-stage resolved review authority: each entry carries the stage id, its type, the resolved mode (advisory | gating | gateless) and its provenance (declared | derived). Omitted when the run's spec declares no reviewers block"`
	// WorkingDir mirrors the backend runResponse.working_dir (E66.42 /
	// #2482): the run's bound local checkout, recorded once at start_run and
	// inherited by every later runner-spawning verb. The
	// resolveWorkingDirForRun ladder reads it on an omitted-parameter call to
	// inherit the binding, and to refuse a conflicting explicit override. The
	// json tag MUST byte-match the backend's runResponse field or this mirror
	// silently decodes to empty and every inheriting verb reverts to demanding
	// the parameter — the #371-class hand-maintained-wire-mirror trap.
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"the run's bound local checkout (bound once at start_run; the later runner-spawning verbs inherit it). Omitted when the run carries no binding"`
	// PredictedRuntimeMinutes mirrors the backend runResponse
	// .predicted_runtime_minutes (E48.62 / #2489): the approved plan's own
	// runtime prediction, stamped onto the run row at plan approval. The
	// stage-wait poll-cadence derivation reads it to advertise a quarter of
	// each non-terminal stage's REMAINING predicted runtime instead of a flat
	// 30s. Zero when unstamped — a legacy row, or a run whose plan is not yet
	// approved (notably while the plan stage itself runs) — in which case the
	// derivation falls back to the elapsed-based branch.
	//
	// The json tag MUST byte-match the backend's runResponse field or this
	// mirror silently decodes to zero and the cadence degrades to that
	// fallback with NO error — the #371-class hand-maintained-wire-mirror
	// trap. That degrade is also the mixed-version behaviour against an older
	// backend that omits the key entirely, which is why it is a graceful
	// fallback rather than a failure.
	PredictedRuntimeMinutes int       `json:"predicted_runtime_minutes,omitempty" jsonschema:"the approved plan's predicted_runtime_minutes, stamped on the run at plan approval. Drives the advertised stage-wait poll cadence; omitted when the run's plan is not yet approved"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// init classifies the decomposition-lineage Run fields this change adds
// (decomposed_from / slice_index / slice_depends_on, E48.99 / #2546) in
// runStatusPathTable — the ONE source the byte-budget ladder, the floor's
// surface union and the nested-aware reflection pin all read. The pin walks
// Run{} structurally, so every wire field MUST be classified or it silently
// bypasses the budget; because the table literal lives in bound.go, the rows
// are appended here, next to the fields they mirror. Package-level var
// initialisation completes before any init() runs, so the append sees the
// fully-built table. Each is a small scalar mirrored from the backend run row
// and surfaced by the single-run REST read, so they classify exactly like
// run.parent_run_id: stored, retrievable from GET /v0/runs/{id}, retained
// through T1..T9 and dropped (itemised) only at the diagnosis skeleton.
func init() {
	runStatusPathTable = append(runStatusPathTable,
		pathClassification{Path: "run.decomposed_from", Tier: "skeleton", Class: classStored, Surfaces: restRun},
		pathClassification{Path: "run.slice_index", Tier: "skeleton", Class: classStored, Surfaces: restRun},
		pathClassification{Path: "run.slice_depends_on", Tier: "skeleton", Class: classStored, Surfaces: restRun},
	)
}

// RunReviewAuthority mirrors the backend's run-status review_authority entry
// (E53.2 / #2225 — backend/internal/server/runs.go's runReviewAuthorityPayload):
// one stage's resolved review authority and its provenance. The json tags MUST
// stay byte-identical with the backend field or the mirror decodes to nil
// silently (the #371-class hand-maintained-wire-mirror trap).
type RunReviewAuthority struct {
	Stage     string `json:"stage" jsonschema:"the stage id"`
	StageType string `json:"stage_type" jsonschema:"the stage type (plan | implement | review | deploy | acceptance)"`
	Authority string `json:"authority" jsonschema:"the resolved review authority: advisory (agent verdicts surfaced, cannot block), gating (an agent reject blocks advancement), or gateless (no agent reviewers)"`
	Source    string `json:"source" jsonschema:"how the authority was resolved: declared (spelled out via reviewers.authority) or derived (the count-derived ADR-027 default)"`
}

// RunLiveValidation mirrors the backend's run-status / gate-view live_validation
// block (#2045, E48.35 — backend/internal/server/live_validation_filing.go's
// runLiveValidationPayload): the run's pending operator live-validation walk.
// The json tags MUST stay byte-identical with the backend field or the mirror
// decodes to nil silently (the #371-class hand-maintained-wire-mirror trap).
//
// Rendering (binding condition A(1)): a consumer that reads only FilingFailed
// must NEVER render the healthy "walk: #X" variant for a run whose walk is not
// durably filed. FilingFailed is true for BOTH a linked-marker filing failure
// AND a stranded intent-only marker (the crash-window case); FilingIncomplete
// additionally flags the stranded-intent sub-case so the wording is "walk filing
// incomplete" vs "walk filing failed". A healthy walk has FilingFailed=false and
// a non-empty WalkRef.
type RunLiveValidation struct {
	PendingCriteriaCount int    `json:"pending_criteria_count" jsonschema:"number of requires_live_validation acceptance criteria awaiting an operator live check"`
	WalkRef              string `json:"walk_ref,omitempty" jsonschema:"the filed operator-validation walk ref (e.g. #123); empty when filing failed or is incomplete"`
	FilingFailed         bool   `json:"filing_failed" jsonschema:"true when the walk is not durably filed (a filing failure OR a stranded intent marker) — the operator files it by hand"`
	FilingIncomplete     bool   `json:"filing_incomplete,omitempty" jsonschema:"true for the stranded-intent crash-window sub-case, so the wording is 'walk filing incomplete' vs 'walk filing failed'"`
}

// RunConcerns mirrors the backend's run-status concerns block (#964):
// the run's OPEN review concerns (states raised, addressed_pending,
// reopened). Each item carries a BOUNDED note-derived short_summary label
// (at most 100 bytes, one line; #2488); the gate view and the originating
// *_reviewed audit entry remain the surfaces for the full untruncated note —
// the run-status block carries NO note field BY DESIGN (payload size).
//
// PRESENCE is authoritative (#3043): the backend emits this block whenever
// the concern-store read SUCCEEDED, INCLUDING a zero-open run (open:0,
// items omitted). It is ABSENT (nil) only when the store was UNAVAILABLE
// (unwired / read error). So a decoded non-nil block == an authoritative
// store read; nil == unavailable, NEVER "zero". The review-action hint reads
// this: a present block is the count authority; a nil block degrades it to
// the audit-derived fallback. An OLDER backend that omits the block for a
// zero-open run reads as nil (unavailable) here — the documented version-skew
// degrade, dev-loop-only since both binaries ship from this repo.
type RunConcerns struct {
	Open    int              `json:"open" jsonschema:"number of open review concerns on the run"`
	ByState map[string]int   `json:"by_state,omitempty" jsonschema:"open-concern count per lifecycle state (raised, addressed_pending, reopened)"`
	Items   []RunConcernItem `json:"items,omitempty" jsonschema:"the open concerns; each carries the stable id to pass to fishhawk_fixup_stage concern_ids"`
	// OpenImplement is the AUTHORITATIVE count of open IMPLEMENT-stage
	// concerns, computed by the backend over the FULL open set — not derived
	// from the (possibly-bounded) Items above (#3043). It equals
	// fishhawk_get_gate_view(stage_kind="implement").open. The review-action
	// hint reads THIS number so a trimmed Items list can never undercount.
	//
	// It is a POINTER because ABSENT and ZERO are different facts that must not
	// collapse: a peer that PREDATES this field returns a present concerns block
	// carrying no open_implement key, which must decode to nil (unknown) and
	// degrade the hint to the audit fallback — NOT to an authoritative zero,
	// which would silently SUPPRESS the hint while implement concerns are
	// genuinely open (the mixed-version defect this pointer closes). A plain int
	// cannot tell "the peer never sent it" from "the peer sent 0". `omitempty`
	// on a pointer omits ONLY nil, so a re-serialized authoritative zero still
	// emits open_implement:0, mirroring the backend's non-omitempty REST
	// contract (a present block always carries the key). The json tag MUST
	// byte-match the backend's runConcernsPayload field or it decodes to nil
	// silently — the #371-class hand-maintained-wire-mirror trap, exactly as the
	// adjacent ShortSummary field warns.
	OpenImplement *int `json:"open_implement,omitempty" jsonschema:"authoritative count of OPEN implement-stage concerns, computed over the full open set (not the bounded items); equals fishhawk_get_gate_view(stage_kind=implement).open. The review-action hint's concern count reads this. ABSENT (nil) means a backend peer that predates the field — the count degrades to the audit fallback rather than reading as an authoritative zero"`
}

// RunConcernItem is one open concern. ID is the stable server-minted
// UUID — the primary fix-up addressing scheme (positional indices are
// deprecated).
type RunConcernItem struct {
	ID        string `json:"id" jsonschema:"stable concern UUID — pass these to fishhawk_fixup_stage concern_ids"`
	StageKind string `json:"stage_kind" jsonschema:"plan or implement; only implement-stage concerns can be routed into an implement fix-up"`
	Severity  string `json:"severity"`
	Category  string `json:"category"`
	State     string `json:"state" jsonschema:"raised, addressed_pending, or reopened"`
	// ShortSummary is the bounded note-derived recognition label (#2488). The
	// json tag MUST byte-match the server's short_summary or the field decodes
	// to "" silently — this is the hand-maintained wire mirror the seam test
	// pins. Omitted for a concern whose note is blank after whitespace
	// collapsing.
	ShortSummary string `json:"short_summary,omitempty" jsonschema:"bounded (at most 100 bytes, one line) note-derived recognition label for reading concerns at a glance; equal to the whole collapsed note when it fits, marked with a trailing ... when cut, absent when the note is blank. A recognition label, NOT a unique key — id remains the addressing key for fishhawk_fixup_stage concern_ids; two concerns with a long shared note prefix may share a label"`
}

// IssueContext mirrors the OpenAPI shape: the GitHub issue payload
// fetched at run-create and cached on the run row (#415). The MCP
// server populates this from `gh issue view` when an agent passes
// the `issue` input — same pattern the CLI uses.
type IssueContext struct {
	Title    string         `json:"title"`
	Body     string         `json:"body"`
	URL      string         `json:"url"`
	Number   int            `json:"number"`
	Comments []IssueComment `json:"comments,omitempty"`
	// Labels are the issue's label NAMES, projected from `gh issue
	// view --json labels`'s label objects (E53.3 / #2226). They are
	// the producer for the `applies_to` predicate's `labels`
	// criterion, evaluated fail-closed at POST /v0/runs. The json tag
	// MUST byte-match the backend's issueContextPayload or the field
	// is silently dropped by the DisallowUnknownFields decoder's
	// counterpart on the way out and every labels-declaring workflow
	// rejects (the #371-class hand-maintained-wire-mirror trap).
	Labels []string `json:"labels,omitempty"`
}

// IssueComment is one issue comment carried alongside the body in
// IssueContext (#618). Captured at run-create from `gh issue view
// --json comments` so the plan agent sees refinements/decisions
// posted as comments, not just the title+body snapshot.
type IssueComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type listRunsResult struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// listRunsFilter scopes a runs query. Empty values drop from the
// query string. The MCP server consumes this for two surfaces:
// `get_active_run`'s resolver (which uses repo, pull_request_url,
// trigger_ref) and `list_runs`'s broader enumeration (which adds
// workflow_id, state, and cursor pagination). Same struct, both
// callers, no separate types.
type listRunsFilter struct {
	Repo           string
	PullRequestURL string
	TriggerRef     string
	WorkflowID     string
	State          string
	Limit          int
	Cursor         string
}

func (c *apiClient) GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var r Run
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+id.String(), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GateView mirrors the backend's GET /v0/runs/{run_id}/gate-view body
// (`backend/internal/server/gateview.go::gateViewResponse`, #1960): the
// gate-scoped decision read carrying each OPEN concern with its FULL note
// prose, the audit-reconstructed cross-round history (fix-up routing claims +
// re-review confirmations), the settled ledger, and the run's suppressed
// relitigations. Field names/tags MUST match the endpoint payload exactly —
// this is the cross-boundary wire seam. IDs are `string` (not uuid.UUID) so
// the MCP SDK's schema reflection sees a string, matching the other client
// types.
type GateView struct {
	RunID                   string                      `json:"run_id"`
	StageKind               string                      `json:"stage_kind,omitempty"`
	Open                    []GateViewConcern           `json:"open"`
	Settled                 []GateViewSettledConcern    `json:"settled"`
	SuppressedRelitigations []GateViewSuppressedRelitig `json:"suppressed_relitigations"`
	HistoryIncomplete       bool                        `json:"history_incomplete"`
	HistoryGaps             []string                    `json:"history_gaps,omitempty"`
	// LiveValidation mirrors the run-status surface (#2045, E48.35): the run's
	// pending operator live-validation walk, populated by buildGateView from the
	// newest live_validation walk marker. Omitted (nil) when the run carried no
	// requires_live_validation criterion or against an older backend. Same wire
	// contract as Run.LiveValidation — the json tag MUST byte-match the backend.
	LiveValidation *RunLiveValidation `json:"live_validation,omitempty"`
	// ReviewDiffTruncated surfaces that an implement review ran on a TRUNCATED
	// diff (#2875) — the runner cut the patch at 256 KiB or the forge capped a
	// compare, so the reviewer saw only a prefix. Omitted (nil) when the run
	// carried no such entry or against an older backend. The json tag MUST
	// byte-match the backend's gateViewResponse or the field silently decodes to
	// nil — the mixed-version degrade.
	ReviewDiffTruncated *gateViewReviewDiffTruncated `json:"review_diff_truncated,omitempty"`
}

// gateViewReviewDiffTruncated mirrors the backend's gateViewReviewDiffTruncated
// (#2875): the distilled newest implement_review_diff_truncated audit entry. The
// json tags MUST byte-match the backend or each field silently decodes to its
// zero value (the #371-class hand-maintained-wire-mirror trap). OmittedFiles is
// the COMPLETE list (unlike the reviewer prompt, which is capped);
// OmittedFilesResidual reports how many the prompt dropped. BestEffort marks a
// forge truncation whose inventory is itself capped.
//
// Deliberately UNEXPORTED — same rationale as gateViewDispute: the MCP jsonschema
// reflection walks it through the exported ReviewDiffTruncated field either way,
// and this package's exported surface (exportBaseline, export_surface_test.go) is
// not widened for a nested payload shape.
type gateViewReviewDiffTruncated struct {
	Reason               string   `json:"reason" jsonschema:"machine-readable origin of the cut: 'runner_patch_cap' (the runner's 256 KiB patch cap) or the forge's truncation reason"`
	ChangedFileCount     int      `json:"changed_file_count" jsonschema:"count of files the stage changed (from the uncapped name-status inventory)"`
	OmittedFileCount     int      `json:"omitted_file_count" jsonschema:"count of changed files not fully visible in the truncated hunks"`
	OmittedFiles         []string `json:"omitted_files,omitempty" jsonschema:"the complete list of not-fully-visible files, each '<path> (<why>)'"`
	OmittedFilesResidual int      `json:"omitted_files_residual,omitempty" jsonschema:"how many omitted files the reviewer's capped PROMPT list dropped past the cap"`
	DeltaReReview        bool     `json:"delta_re_review,omitempty" jsonschema:"true when the truncated diff was a post-fix-up delta re-review rather than the full diff"`
	BestEffort           bool     `json:"best_effort,omitempty" jsonschema:"true for a forge truncation whose file inventory is itself capped, so the omitted set may itself be incomplete"`
}

// GateViewConcern is one OPEN concern with full decision context. Note carries
// the reviewer's complete prose — the surface elides nothing (no compaction
// levers apply).
type GateViewConcern struct {
	ID                   string `json:"id"`
	StageKind            string `json:"stage_kind"`
	Round                int    `json:"round,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	State                string `json:"state"`
	StateReason          string `json:"state_reason,omitempty"`
	Note                 string `json:"note"`
	// NewEvidence / SettledRef mirror the server's gateViewConcern (E60.8 /
	// #2353): the reviewer's supporting evidence and the re-raise lineage tag.
	// The json tags MUST byte-match the backend — a typo silently yields an
	// empty field rather than a decode error.
	NewEvidence       string               `json:"new_evidence,omitempty"`
	SettledRef        string               `json:"settled_ref,omitempty"`
	HasSuggestedPatch bool                 `json:"has_suggested_patch"`
	Fixups            []GateViewFixup      `json:"fixups,omitempty"`
	Resolutions       []GateViewResolution `json:"resolutions,omitempty"`
	// Disputed / Disputes mirror the server's gateViewConcern (E48.103 /
	// #2551): a `confirmed` resolution was recorded on this concern and it is
	// STILL OPEN. Disputed is derived server-side from the durable concern row
	// plus the authoritative review payload, so it can be true with an EMPTY
	// Disputes when the (best-effort) veto record did not land. Same json-tag
	// byte-match rule as the fields above.
	Disputed bool              `json:"disputed"`
	Disputes []gateViewDispute `json:"disputes,omitempty"`
}

// gateViewDispute is one refused `confirmed` resolution: why the confirmation
// was vetoed, and by/against which reviewer (E48.103 / #2551). Deliberately
// UNEXPORTED — the MCP jsonschema reflection walks it through the exported
// Disputes field either way, and this package's exported surface is pinned by
// exportBaseline (export_surface_test.go), which no new type should widen for
// a nested payload shape.
type gateViewDispute struct {
	Sequence                int64  `json:"sequence"`
	Round                   int    `json:"round,omitempty"`
	VetoReason              string `json:"veto_reason"`
	Resolution              string `json:"resolution,omitempty"`
	ConfirmingReviewerModel string `json:"confirming_reviewer_model,omitempty"`
	RaisingReviewerModel    string `json:"raising_reviewer_model,omitempty"`
	Note                    string `json:"note,omitempty"`
}

// GateViewFixup is one fix-up routing claim joined to its outcome.
type GateViewFixup struct {
	Sequence  int64  `json:"sequence"`
	Reason    string `json:"reason,omitempty"`
	Outcome   string `json:"outcome"`
	ApplyPath string `json:"apply_path,omitempty"`
	HeadSHA   string `json:"head_sha,omitempty"`
}

// GateViewResolution is one re-review verdict on a concern.
type GateViewResolution struct {
	Sequence   int64  `json:"sequence"`
	Round      int    `json:"round,omitempty"`
	Resolution string `json:"resolution"`
	Note       string `json:"note,omitempty"`
}

// GateViewSettledConcern is one settled-ledger row.
type GateViewSettledConcern struct {
	ID            string `json:"id"`
	StageKind     string `json:"stage_kind"`
	State         string `json:"state"`
	Severity      string `json:"severity"`
	Category      string `json:"category"`
	ReviewerModel string `json:"reviewer_model,omitempty"`
	Note          string `json:"note"`
	StateReason   string `json:"state_reason,omitempty"`
	// NewEvidence / SettledRef mirror the server's gateViewSettledConcern
	// (E60.8 / #2353). Same byte-match requirement as GateViewConcern.
	NewEvidence string `json:"new_evidence,omitempty"`
	SettledRef  string `json:"settled_ref,omitempty"`
}

// GateViewSuppressedRelitig is one suppressed relitigation (#1913).
type GateViewSuppressedRelitig struct {
	SettledRef           string `json:"settled_ref"`
	SettledState         string `json:"settled_state"`
	Severity             string `json:"severity"`
	Category             string `json:"category"`
	Note                 string `json:"note"`
	ReviewerModel        string `json:"reviewer_model,omitempty"`
	OriginReviewSequence int64  `json:"origin_review_sequence"`
}

// GetGateView calls GET /v0/runs/{run_id}/gate-view with an optional
// stage_kind (plan|implement) filter. The payload passes through verbatim —
// none of compact.go's levers (stripReviewProse, auditPayloadStringCap) are
// applied — so the full concern notes reach the operator intact.
func (c *apiClient) GetGateView(ctx context.Context, runID uuid.UUID, stageKind string) (*GateView, error) {
	path := "/v0/runs/" + runID.String() + "/gate-view"
	if stageKind != "" {
		q := url.Values{}
		q.Set("stage_kind", stageKind)
		path = path + "?" + q.Encode()
	}
	var gv GateView
	if err := c.do(ctx, http.MethodGet, path, nil, &gv); err != nil {
		return nil, err
	}
	return &gv, nil
}

// BudgetStatus mirrors the backend's GET /v0/runs/{run_id}/budget body
// (`backend/internal/server/budget_status.go::budgetStatusResponse`):
// the current calendar-period status of the run's workflow periodic
// budget (#693 / ADR-030). DISPLAY-ONLY — surfaced in the tool outputs
// so the operator sees spend-vs-limit every stage; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient
// is a thin local copy (the import direction is `cli → backend`, not the
// reverse). The scalar fields are omitempty so the no-budget path — which
// GetRunBudget collapses to a nil pointer — never marshals a half-empty
// block.
type BudgetStatus struct {
	Period      string   `json:"period"`
	PeriodStart string   `json:"period_start,omitempty"`
	LimitUSD    float64  `json:"limit_usd,omitempty"`
	SpentUSD    float64  `json:"spent_usd,omitempty"`
	Fraction    float64  `json:"fraction,omitempty"`
	WarnAt      *float64 `json:"warn_at,omitempty"`
	Tier        string   `json:"tier,omitempty"`
	// AckRequired mirrors the backend's escalation boolean (#1371): true
	// once period spend reaches the configured ack multiple (the
	// ack_required or page tier), signalling that a plan-approval gate now
	// requires an explicit --ack-budget acknowledgment. omitempty so the
	// no-budget / sub-ack path stays byte-identical to today.
	AckRequired bool   `json:"ack_required,omitempty"`
	Enforcement string `json:"enforcement,omitempty"`
}

// GetRunBudget fetches the run's workflow periodic-budget status. The
// backend returns 200 with an empty object when no budget is configured;
// GetRunBudget collapses that to (nil, nil) so every caller treats "no
// budget" uniformly by checking for a nil pointer.
func (c *apiClient) GetRunBudget(ctx context.Context, runID uuid.UUID) (*BudgetStatus, error) {
	var b BudgetStatus
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/budget", nil, &b); err != nil {
		return nil, err
	}
	if b.Period == "" {
		return nil, nil
	}
	return &b, nil
}

// CacheEfficiency mirrors the backend's GET
// /v0/runs/{run_id}/cache-efficiency body
// (`backend/internal/server/cache_efficiency.go::cacheEfficiencyResponse`):
// the per-run prompt-cache efficiency metric derived from the run's
// cost_recorded ledger (ADR-044 slice 3 / #1352). DISPLAY-ONLY — surfaced
// in fishhawk_get_run_status so the operator sees cache-hit usage and the
// net dollar effect; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is
// a thin local copy (the import direction is `cli → backend`, not the
// reverse). The scalar fields are omitempty so the no-data path — which
// GetRunCacheEfficiency collapses to a nil pointer — never marshals a
// half-empty block.
type CacheEfficiency struct {
	FreshInputTokens    int                    `json:"fresh_input_tokens,omitempty"`
	CacheReadTokens     int                    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    int                    `json:"cache_write_tokens,omitempty"`
	OutputTokens        int                    `json:"output_tokens,omitempty"`
	CacheReadRatio      float64                `json:"cache_read_ratio,omitempty"`
	ReuseFactor         float64                `json:"reuse_factor,omitempty"`
	GrossReadSavingsUSD float64                `json:"gross_read_savings_usd,omitempty"`
	WritePenaltyUSD     float64                `json:"write_penalty_usd,omitempty"`
	NetSavingsUSD       float64                `json:"net_savings_usd,omitempty"`
	Stages              []CacheEfficiencyStage `json:"stages,omitempty"`
}

// CacheEfficiencyStage is the per-source breakdown row (plan_review /
// implement_review / agent).
type CacheEfficiencyStage struct {
	Source              string  `json:"source"`
	FreshInputTokens    int     `json:"fresh_input_tokens,omitempty"`
	CacheReadTokens     int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens    int     `json:"cache_write_tokens,omitempty"`
	OutputTokens        int     `json:"output_tokens,omitempty"`
	CacheReadRatio      float64 `json:"cache_read_ratio,omitempty"`
	ReuseFactor         float64 `json:"reuse_factor,omitempty"`
	GrossReadSavingsUSD float64 `json:"gross_read_savings_usd,omitempty"`
	WritePenaltyUSD     float64 `json:"write_penalty_usd,omitempty"`
	NetSavingsUSD       float64 `json:"net_savings_usd,omitempty"`
}

// GetRunCacheEfficiency fetches the run's cache-efficiency metric. The
// backend returns 200 with an empty object when the run has no cost data;
// GetRunCacheEfficiency collapses that to (nil, nil) so every caller treats
// "no data" uniformly by checking for a nil pointer. The presence sentinel
// is "all token buckets zero AND no stages" — analogous to budget's
// Period=="" check; a real run always reports output tokens, so the empty
// object never false-collapses a real metric.
func (c *apiClient) GetRunCacheEfficiency(ctx context.Context, runID uuid.UUID) (*CacheEfficiency, error) {
	var ce CacheEfficiency
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/cache-efficiency", nil, &ce); err != nil {
		return nil, err
	}
	if ce.FreshInputTokens == 0 && ce.CacheReadTokens == 0 && ce.CacheWriteTokens == 0 &&
		ce.OutputTokens == 0 && len(ce.Stages) == 0 {
		return nil, nil
	}
	return &ce, nil
}

// RunCost mirrors the backend's GET /v0/runs/{run_id}/cost body
// (`backend/internal/server/cost.go::costSummaryResponse`): the per-run
// estimated cost derived from the run's cost_recorded ledger, a per-stage
// (agent / plan_review / implement_review) breakdown, and — when the run
// resolved to a merged PR — the cost-per-merged-PR rollup (#1372).
// DISPLAY-ONLY — surfaced in fishhawk_get_run_status so the operator sees the
// cost to land work; it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is a
// thin local copy (the import direction is `cli → backend`, not the reverse).
// The scalar fields are omitempty so the no-data path — which GetRunCost
// collapses to a nil pointer — never marshals a half-empty block.
type RunCost struct {
	TotalCostUSD float64          `json:"total_cost_usd,omitempty"`
	Stages       []RunCostStage   `json:"stages,omitempty"`
	MergedPR     *RunMergedPRCost `json:"merged_pr,omitempty"`
}

// RunCostStage is the per-source cost breakdown row (agent / plan_review /
// implement_review).
type RunCostStage struct {
	Source  string  `json:"source"`
	CostUSD float64 `json:"cost_usd"`
}

// RunMergedPRCost is the cost-per-merged-PR rollup: the summed CostUSDTotal
// across every run sharing the PR URL, present only when the run resolved to a
// merged PR.
type RunMergedPRCost struct {
	PullRequestURL     string  `json:"pull_request_url"`
	CostPerMergedPRUSD float64 `json:"cost_per_merged_pr_usd"`
	RunCount           int     `json:"run_count"`
}

// GetRunCost fetches the run's cost summary. The backend returns 200 with an
// empty object when the run has no cost data; GetRunCost collapses that to
// (nil, nil) so every caller treats "no data" uniformly by checking for a nil
// pointer. The presence sentinel is "no per-stage rows AND no merged-PR
// rollup" — the empty object has neither, while any costed run reports at
// least one stage, so the empty object never false-collapses a real metric.
func (c *apiClient) GetRunCost(ctx context.Context, runID uuid.UUID) (*RunCost, error) {
	var rc RunCost
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/cost", nil, &rc); err != nil {
		return nil, err
	}
	if len(rc.Stages) == 0 && rc.MergedPR == nil {
		return nil, nil
	}
	return &rc, nil
}

// RunLatency mirrors the backend's GET /v0/runs/{run_id}/latency body
// (`backend/internal/server/latency.go::latencySummaryResponse`): the per-run
// gate-latency (wait-on-human) rollup derived from the run's audit-chain
// timestamps (#1702) — the time parked at each human gate (plan approval,
// implement-review → next dispatch, checks-green → merge), the total wait on
// human decisions, and the run's end-to-end wall clock. DISPLAY-ONLY —
// surfaced in fishhawk_get_run_status so the operator sees human-gate latency;
// it never gates a run.
//
// Repeated here rather than imported because the MCP server's apiClient is a
// thin local copy (the import direction is `cli → backend`, not the reverse).
// The scalar fields are omitempty so the no-data path — which GetRunLatency
// collapses to a nil pointer — never marshals a half-empty block.
type RunLatency struct {
	Gates                   []LatencyGate `json:"gates,omitempty"`
	TotalWaitOnHumanSeconds float64       `json:"total_wait_on_human_seconds,omitempty"`
	WallClockSeconds        float64       `json:"wall_clock_seconds,omitempty"`
}

// LatencyGate is one measured human gate: the interval between its opening and
// closing audit markers, with the wait in seconds.
type LatencyGate struct {
	Gate        string    `json:"gate"`
	OpenedAt    time.Time `json:"opened_at"`
	ClosedAt    time.Time `json:"closed_at"`
	WaitSeconds float64   `json:"wait_seconds"`
}

// GetRunLatency fetches the run's gate-latency rollup. The backend returns 200
// with an empty object when no gate interval resolves; GetRunLatency collapses
// that to (nil, nil) so every caller treats "no data" uniformly by checking for
// a nil pointer. The presence sentinel is "no gate rows" — the empty object has
// none, while any gated run reports at least one gate, so the empty object never
// false-collapses a real rollup.
func (c *apiClient) GetRunLatency(ctx context.Context, runID uuid.UUID) (*RunLatency, error) {
	var rl RunLatency
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/latency", nil, &rl); err != nil {
		return nil, err
	}
	if len(rl.Gates) == 0 {
		return nil, nil
	}
	return &rl, nil
}

// OnboardingReadinessReport mirrors the backend's GET
// /v0/onboarding/readiness body
// (`backend/internal/server/onboarding.go::onboardingReadinessResponse`, E29.4 /
// #1511): the four server-side-only readiness checks a repo's first run needs —
// GitHub App installation, the committed workflow spec's parse/validate state,
// per-reviewer availability on this deployment, and the caller token's scope
// adequacy. Repeated here rather than imported because the MCP server's
// apiClient is a thin local copy (the import direction is `cli → backend`, not
// the reverse). Every field is a scalar/string/slice — no UUID/raw-JSON field,
// so the #371 reflection trap does not apply. MUST stay byte-identical with the
// backend response's json tags.
type OnboardingReadinessReport struct {
	Repo      string               `json:"repo" jsonschema:"the target repo as owner/name that was probed"`
	App       OnboardingApp        `json:"app" jsonschema:"GitHub App installation readiness"`
	Spec      OnboardingSpec       `json:"spec" jsonschema:"committed workflow spec fetch/parse/validate readiness"`
	Reviewers []OnboardingReviewer `json:"reviewers" jsonschema:"per spec-declared reviewer availability on this deployment; empty when the spec is unavailable or invalid"`
	Scopes    OnboardingScopes     `json:"scopes" jsonschema:"caller-token run-driving scope adequacy"`
}

// OnboardingApp mirrors the backend appInstallReadiness sub-object: whether the
// GitHub App is installed on the target repo, with reason set when it is not.
type OnboardingApp struct {
	Installed      bool   `json:"installed" jsonschema:"true when the GitHub App is installed on the target repo"`
	InstallationID int64  `json:"installation_id,omitempty" jsonschema:"the resolved installation id when installed"`
	Reason         string `json:"reason,omitempty" jsonschema:"why the app is not installed / could not be resolved"`
}

// OnboardingSpec mirrors the backend specReadiness sub-object: the committed
// workflow spec's fetch + parse + validate state. Valid is only meaningful when
// Source == "fetched".
type OnboardingSpec struct {
	Source string `json:"source" jsonschema:"'fetched' when the spec was read from the repo, else 'unavailable'"`
	Valid  bool   `json:"valid" jsonschema:"true when the fetched spec parsed and validated; meaningful only when source is 'fetched'"`
	Error  string `json:"error,omitempty" jsonschema:"the parse or validation failure when the spec is invalid"`
	Note   string `json:"note,omitempty" jsonschema:"why the spec is unavailable (app not installed, spec not found, fetch error)"`
}

// OnboardingReviewer mirrors the backend reviewerReadiness sub-object: one
// spec-declared reviewer's availability on this deployment, with the adapter's
// missing-env-var hint when the provider cannot be resolved.
type OnboardingReviewer struct {
	Provider        string `json:"provider" jsonschema:"the reviewer provider (e.g. claudecode, codex)"`
	Model           string `json:"model,omitempty" jsonschema:"the reviewer model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty" jsonschema:"the reviewer reasoning-effort tier when set"`
	Available       bool   `json:"available" jsonschema:"true when this reviewer can be resolved on this deployment"`
	MissingHint     string `json:"missing_hint,omitempty" jsonschema:"the adapter's missing-env-var hint when the provider is unavailable"`
}

// OnboardingScopes mirrors the backend scopeReadiness sub-object: whether the
// caller token holds the run-driving scope subset, listing any missing scopes.
type OnboardingScopes struct {
	Adequate bool     `json:"adequate" jsonschema:"true when the caller token holds every run-driving scope"`
	Required []string `json:"required" jsonschema:"the run-driving scope subset the check requires"`
	Missing  []string `json:"missing" jsonschema:"the required scopes the caller lacks; empty when adequate"`
	Note     string   `json:"note,omitempty" jsonschema:"context, e.g. a cookie-session caller bypasses scope enforcement"`
}

// OnboardingReadiness fetches a repo's first-run readiness report via
// `GET /v0/onboarding/readiness?repo=owner/name` (E29.4 / #1511). The endpoint
// gates on AUTHENTICATION only (401 for anonymous) — scope adequacy is itself a
// reported field, not a gate — so a token with a run-driving scope gap still
// gets a 200 report naming its gap. 4xx surfaces as *apiError; the tool layer
// maps authentication_required (401) and validation_failed (400, malformed
// repo) onto clean tool errors.
func (c *apiClient) OnboardingReadiness(ctx context.Context, repo string) (*OnboardingReadinessReport, error) {
	var out OnboardingReadinessReport
	if err := c.do(ctx, http.MethodGet, "/v0/onboarding/readiness?repo="+url.QueryEscape(repo), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// createRunRequest mirrors the backend's `POST /v0/runs` request body
// (`backend/internal/server/runs.go::createRunRequest`). Repeated here
// rather than imported because the MCP server's apiClient is
// deliberately a thin local copy — the import-direction rule is
// `cli → backend`, not the other way around, and the same applies
// to this binary.
type createRunRequest struct {
	Repo           string        `json:"repo"`
	WorkflowID     string        `json:"workflow_id"`
	WorkflowSHA    string        `json:"workflow_sha"`
	TriggerSource  string        `json:"trigger_source"`
	TriggerRef     *string       `json:"trigger_ref,omitempty"`
	RunnerKind     string        `json:"runner_kind,omitempty"`
	WorkingDir     string        `json:"working_dir,omitempty"`
	WorkflowSpec   string        `json:"workflow_spec,omitempty"`
	IssueContext   *IssueContext `json:"issue_context,omitempty"`
	BudgetOverride bool          `json:"budget_override,omitempty"`
	UpstreamRunID  string        `json:"upstream_run_id,omitempty"`
	// AppliesToOverride + AppliesToOverrideReason are the audited
	// escape hatch past the workflow's `applies_to` routing
	// declaration (E53.3 / #2226). The reason is REQUIRED whenever the
	// override is set — the backend 400s an empty or whitespace-only
	// one — so the bypass is never unexplained.
	AppliesToOverride       bool   `json:"applies_to_override,omitempty"`
	AppliesToOverrideReason string `json:"applies_to_override_reason,omitempty"`
}

// StartRunParams is the typed input the apiClient takes for run
// creation. `IdempotencyKey` is optional and travels in the HTTP
// header per the backend's E8.2 contract — when set, a previously-
// created run with the same `(repo, key)` returns 200 instead of a
// fresh 201.
//
// RunnerKind / WorkflowSpec / IssueContext mirror the CLI's
// CreateRunInput surface (#411, #415, ADR-022) so an agent calling
// fishhawk_start_run via MCP has the same composition reach the
// CLI's `fishhawk run start` does.
type StartRunParams struct {
	Repo           string
	WorkflowID     string
	WorkflowSHA    string
	TriggerSource  string
	TriggerRef     string
	IdempotencyKey string
	RunnerKind     string
	// WorkingDir binds the run's local checkout (E66.42 / #2482); persisted
	// on the run row so every later runner-spawning verb inherits it. Empty
	// leaves the run unbound.
	WorkingDir              string
	WorkflowSpec            string
	IssueContext            *IssueContext
	BudgetOverride          bool
	UpstreamRunID           string
	AppliesToOverride       bool
	AppliesToOverrideReason string
}

// approvalRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/approvals` body
// (`backend/internal/server/approvals.go::approvalRequest`).
type approvalRequest struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
	// ApproverGithubLogin is the resolved GitHub login of the acting
	// operator (#751), threaded through so the issue-thread status
	// footer `@`-mentions the real login rather than the raw token
	// subject. Omitempty: SPA/CLI callers omit it and stay unaffected.
	ApproverGithubLogin string `json:"approver_github_login,omitempty"`
	// AddScopeFiles is the structured, authoritative scope amendment (#824):
	// repo-relative paths to fold into the implement stage's scope.files on
	// approve. A trailing slash marks a directory. The DisallowUnknownFields
	// decoder on the backend requires the field be declared here too; reject
	// and conditionless approve callers pass nil (omitempty).
	AddScopeFiles []string `json:"add_scope_files,omitempty"`
	// RemoveScopeFiles is the structured scope removal (#1726): repo-relative
	// paths to subtract from the implement stage's scope.files on approve — the
	// inverse of AddScopeFiles. Combined with it in the same approve call it
	// expresses a scope REPLACE. The DisallowUnknownFields decoder on the
	// backend requires the field be declared here too; reject and removal-less
	// approve callers pass nil (omitempty).
	RemoveScopeFiles []string `json:"remove_scope_files,omitempty"`
	// AddScopeFilesToSlice is the per-slice scope add for a DECOMPOSED plan
	// (#2515): a map of sub-plan title (or 0-based index) to the repo-relative
	// paths folded into THAT slice's scope.files only. The DisallowUnknownFields
	// decoder on the backend requires the field be declared here too; reject and
	// flat-plan approve callers pass nil (omitempty).
	AddScopeFilesToSlice map[string][]string `json:"add_scope_files_to_slice,omitempty"`
	// MoveScopeFilesToSlice is the per-slice scope MOVE for a DECOMPOSED plan
	// (#2596): a map of DESTINATION sub-plan title (or 0-based index) to the
	// already-declared repo-relative paths relocated into THAT slice; the source is
	// derived from ownership. The DisallowUnknownFields decoder on the backend
	// requires the field be declared here too; reject and no-move approve callers
	// pass nil (omitempty), so a no-move approve posts a byte-identical body.
	MoveScopeFilesToSlice map[string][]string `json:"move_scope_files_to_slice,omitempty"`
	// BindingAssertions is the operator-declared binding-assertion list (#1171)
	// the backend validates pre-Submit and records on the approval audit
	// payload. The DisallowUnknownFields decoder requires the field be declared
	// here too; reject and assertion-less approve callers pass nil (omitempty).
	BindingAssertions []BindingAssertion `json:"binding_assertions,omitempty"`
	// ClaimsConcernIDs is the operator-declared list of plan-stage concern ids
	// this approval's binding condition answers (#1956) the backend validates
	// pre-Submit and records on the approval audit payload. The
	// DisallowUnknownFields decoder requires the field be declared here too;
	// reject and claim-less approve callers pass nil (omitempty).
	ClaimsConcernIDs []string `json:"claims_concern_ids,omitempty"`
	// AmendAcceptanceCriteria is the operator's approve-time amendment of the
	// approved plan's acceptance criteria (#2581): retire or restate a criterion
	// by id, each with a required reason. The backend validates it pre-Submit and
	// records it on the approval audit payload. The DisallowUnknownFields decoder
	// requires the field be declared here too; reject and amendment-less approve
	// callers pass nil (omitempty).
	AmendAcceptanceCriteria []AcceptanceCriteriaAmendment `json:"amend_acceptance_criteria,omitempty"`
	// ImplementModel is the optional operator override for the implement-stage
	// model (#1013) — the highest rung of the resolution ladder. The backend
	// resolves the full ladder at the plan gate, validates the resolved value
	// against the allow-list (422 plan_invalid_model on an unknown model), and
	// records it as the model_resolved audit. The DisallowUnknownFields decoder
	// requires the field be declared here too; reject and override-less approve
	// callers pass "" (omitempty) and stay byte-identical to today.
	ImplementModel string `json:"implement_model,omitempty"`
}

// approvalResult is the decoded 200 body of POST /v0/stages/{id}/
// approvals (#986). On a first submission the duplicate fields are
// absent (zero values). On a duplicate — the same subject already
// decided this stage — DuplicateSubmission is true, the prior decision
// stands, the stage state is unchanged, and the backend ran NO gates
// and emitted NO audit entries; PriorDecision/PriorSubmittedAt carry
// the EXISTING approval row's provenance.
type approvalResult struct {
	Stage
	DuplicateSubmission bool   `json:"duplicate_submission"`
	PriorDecision       string `json:"prior_decision"`
	PriorSubmittedAt    string `json:"prior_submitted_at"`
}

// SubmitApproval posts an approve or reject decision against the
// given stage. `decision` must be "approve" or "reject"; `comment`
// is optional but recommended on rejects (the CLI emits a warning
// when missing). `approverGithubLogin` is the resolved GitHub login
// of the acting operator (#751) — empty when gh resolution was
// unavailable; the backend records it in the approval audit payload
// for issue-thread `@`-mention rendering while keeping the token
// subject as the provenance identity. `addScopeFiles` is the structured
// scope amendment (#824) folded into the implement stage's scope.files on
// approve; nil on reject and conditionless approve. `removeScopeFiles` is the
// inverse structured scope removal (#1726) subtracted from the implement
// stage's scope.files on approve (a scope REPLACE = addScopeFiles +
// removeScopeFiles in one call); nil on reject and removal-less approve.
// `addScopeFilesToSlice` is the per-slice add channel for a DECOMPOSED plan
// (#2515) — sub-plan title (or 0-based index) to the paths folded into THAT
// slice alone; nil on reject, on a flat plan (use addScopeFiles), and on a
// slice-targeting-less approve.
// `bindingAssertions` is the
// operator-declared binding-assertion list (#1171) the backend validates
// pre-Submit and records on the approval audit payload; nil on reject and
// assertion-less approve. `claimsConcernIDs` is the operator-declared list of
// plan-stage concern ids this approval's binding condition answers (#1956) the
// backend validates pre-Submit (approve-only, plan-stage-only, open plan-stage
// concerns of the same run) and records on the approval audit payload; nil on
// reject and claim-less approve. Returns the updated Stage. 4xx
// surfaces:
//   - 400 validation_failed (decision other than approve/reject; a malformed
//     binding_assertions declaration — unknown type, empty literal, a
//     test_asserts path not ending in _test.go)
//   - 404 stage_not_found
//   - 409 review_stage_managed_by_github (review-stage approvals
//     live on GitHub per ADR-018; not relevant for the MCP plan-
//     approval tools but the wrapper surfaces the code if a future
//     caller reaches this method with a review-stage id)
//   - 409 agent_review_pending (ADR-036: a configured agent plan
//     review is still in-flight; retryable once the review reaches
//     any terminal state — plan_reviewed / plan_review_failed /
//     plan_review_skipped. details carry configured_agents +
//     landed_terminal)
//   - 422 plan_violates_budget (plan predicted runtime exceeds the
//     implement-stage budget; decompose or --override-budget. #986:
//     refused PRE-insert — no approval row is recorded, so the same
//     subject's retry with the override flows normally)
//   - 422 plan_violates_scope_cap (#983: effective scope.files — plan
//     scope plus add_scope_files — exceeds the implement stage's
//     max_files_changed; re-scope the plan or include
//     --override-scope-cap in the comment. Pre-insert. The override
//     retry succeeds ONLY when the scope's minimum physical changed-file
//     count fits the cap; when min_changed_files already exceeds the cap
//     the override is refused (see plan_scope_cap_override_unavailable))
//   - 422 plan_scope_cap_override_unavailable (#2415: an approve carrying
//     --override-scope-cap was REFUSED because the effective scope's
//     minimum physical changed-file count already exceeds the
//     run-immutable max_files_changed cap — the override clears only the
//     declared-scope pre-check, but the implement stage re-checks the real
//     diff against a cap fixed for the run, so no override can make this
//     scope land. Pre-insert (no approval row), and a
//     plan_scope_cap_override_refused audit entry is written. details carry
//     stage_id, scoped_files, min_changed_files, max_files_changed,
//     add_scope_files_count, remove_scope_files_count. Remediation: drop
//     declared paths via remove_scope_files, or raise max_files_changed
//     through a governed spec change and start a fresh run)
//   - 422 plan_add_scope_files_fans_into_slices (#2103: add_scope_files
//     was supplied on a DECOMPOSED plan; an added path fans into EVERY
//     sub-plan slice, violating single-owner-file and guaranteeing an
//     add/add fan-in conflict. There is NO override and NO per-slice add
//     channel; details carry add_scope_files, slice_count, and slices
//     (the {index,title} of every inheriting slice). Pre-insert: re-plan
//     the decomposition so each added file lands in exactly one slice's
//     scope.files. Also fails closed when add_scope_files is non-empty and
//     the plan cannot be confirmed non-decomposed. The per-slice counterpart —
//     add_scope_files_to_slice (#2515) — is the channel that DOES work on a
//     decomposed plan)
//   - 422 plan_slice_add_scope_files_requires_decomposed_plan (#2515: the ONE
//     new code the per-slice channel introduces. add_scope_files_to_slice was
//     supplied but the plan is positively FLAT (details.reason
//     plan_not_decomposed — use plain add_scope_files) or could not be
//     confirmed decomposed (details.reason plan_indeterminate, fail-closed).
//     Pre-insert, no override)
//   - 400 validation_failed with details.field = add_scope_files_to_slice
//     (#2515 shape/ownership refusals, reusing the existing code: an
//     unresolvable or ambiguous slice key, two keys naming one slice, a path
//     under two slices, a path whose ownership overlaps a DIFFERENT slice's
//     declared scope.files — this channel ADDS and does not MOVE, so re-plan
//     the decomposition — an invalid path, or an empty path list. details
//     carry the offending key/path plus the ordered {index,title} slice list)
//   - 400 validation_failed with details.field = move_scope_files_to_slice
//     (#2596 move shape/ownership refusals, details.reason one of:
//     slice_key_ambiguous / slice_key_unresolvable / duplicate_slice_key /
//     empty_path / path_not_repo_relative / empty_path_list /
//     path_under_two_slices / path_in_both_scope_channels (also in
//     add_scope_files_to_slice) / path_not_in_declared_scope (nothing to move —
//     use add_scope_files_to_slice) / move_requires_exact_owned_path
//     (containment-only overlap of a directory entry — re-plan) /
//     path_already_owned_by_destination (no-op) / move_would_empty_source_slice)
//   - 422 plan_slice_move_scope_files_requires_decomposed_plan (#2596:
//     move_scope_files_to_slice was supplied but the plan is positively FLAT
//     (details.reason plan_not_decomposed — no slices to move between) or could
//     not be confirmed decomposed (details.reason plan_indeterminate,
//     fail-closed). Pre-insert, no override)
//   - 409 plan_slice_move_after_dispatch (#2596: a source/destination fan-out
//     child has already left run state 'pending' (details.reason
//     slice_already_started, with path/from_slice/to_slice/child_run_id/
//     child_state) so re-scoping its slice is refused; or the sibling listing
//     failed (details.reason dispatch_state_indeterminate, fail-closed). Revise
//     the plan or start a fresh run)
//   - 400 validation_failed with details.field = amend_acceptance_criteria
//     (#2581: the acceptance-criteria amendment refusals, each naming the
//     offending entry under details.id and the specific refusal under
//     details.rule — unknown_criterion_id, unknown_action, reason_required,
//     statement_required, duplicate_id, already_retired, or
//     amendment_not_approve_plan_stage. Pre-insert: fix the entry and re-approve)
//   - 422 acceptance_criteria_all_retired (#2581: the amendment would retire
//     EVERY acceptance criterion in the approved plan — the anti-silencing gate,
//     evaluated on the union of prior recorded retirements and this request's, so
//     it fires cumulatively too. There is NO override; re-plan rather than
//     emptying the acceptance contract. details carry retired_count and
//     criteria_count)
//   - 422 acceptance_criteria_unavailable (#2581: amend_acceptance_criteria was
//     supplied but the approved plan could not be loaded, carries zero
//     acceptance_criteria, or its prior amendments could not be read — fail
//     closed, since an amendment must anchor to a criterion that exists.
//     Remediation: re-plan so the criteria exist, or approve without the channel)
//   - 422 plan_invalid_model (#1013: the RESOLVED implement model — the
//     ladder of deployment default < spec executor.model < plan
//     model_recommendation < implement_model override — is not in the
//     deployment's per-adapter allow-list; details carry model,
//     model_source, and adapter. Pre-insert: retry with an allowed
//     implement_model, or widen the allow-list)
func (c *apiClient) SubmitApproval(ctx context.Context, stageID uuid.UUID, decision, comment, approverGithubLogin string, addScopeFiles, removeScopeFiles []string, addScopeFilesToSlice, moveScopeFilesToSlice map[string][]string, bindingAssertions []BindingAssertion, claimsConcernIDs []string, amendAcceptanceCriteria []AcceptanceCriteriaAmendment, implementModel string) (*approvalResult, error) {
	body, err := json.Marshal(approvalRequest{
		Decision:                decision,
		Comment:                 comment,
		ApproverGithubLogin:     approverGithubLogin,
		AddScopeFiles:           addScopeFiles,
		RemoveScopeFiles:        removeScopeFiles,
		AddScopeFilesToSlice:    addScopeFilesToSlice,
		MoveScopeFilesToSlice:   moveScopeFilesToSlice,
		BindingAssertions:       bindingAssertions,
		ClaimsConcernIDs:        claimsConcernIDs,
		AmendAcceptanceCriteria: amendAcceptanceCriteria,
		ImplementModel:          implementModel,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal approval: %w", err)
	}
	var res approvalResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/approvals", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ClarificationAnswer is one operator answer to a parked clarification
// question, matched back to the question by id. Exported (like
// RecoverScopePath) so the MCP tool's input schema can reuse the same
// shape the wire body carries.
type ClarificationAnswer struct {
	ID     string `json:"id" jsonschema:"the parked question's id, from the run's clarification_requested audit entry (read it via fishhawk_get_run_status / fishhawk_list_audit)"`
	Answer string `json:"answer" jsonschema:"the operator's answer to that question"`
}

// clarificationAnswerRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/clarification` body
// (`backend/internal/server/clarification_answer.go::clarificationAnswerRequest`).
type clarificationAnswerRequest struct {
	Answers []ClarificationAnswer `json:"answers"`
	Comment string                `json:"comment,omitempty"`
}

// AnswerClarification posts the operator's answers to a plan stage parked
// at awaiting_input by a clarification_request, re-opening it
// (AwaitingInput → Pending), via
// `POST /v0/stages/{stage_id}/clarification` (#1088, the #1057
// answer-and-resume seam). The answers are persisted as a dedicated
// clarification_answered audit entry — NOT an approval — and injected into
// the resumed plan prompt's binding conditions. Returns the re-opened
// Stage. 4xx surfaces:
//   - 400 validation_failed (empty answers / unknown fields)
//   - 400 clarification_answer_invalid (unknown / missing / duplicate
//     answer id relative to the parked questions)
//   - 404 stage_not_found
//   - 409 invalid_state_transition (the stage is not a plan stage parked
//     at awaiting_input)
func (c *apiClient) AnswerClarification(ctx context.Context, stageID uuid.UUID, answers []ClarificationAnswer, comment string) (*Stage, error) {
	body, err := json.Marshal(clarificationAnswerRequest{Answers: answers, Comment: comment})
	if err != nil {
		return nil, fmt.Errorf("marshal clarification answer: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/clarification", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RetryStage re-fires a failed stage via
// `POST /v0/stages/{stage_id}/retry`. Returns the updated Stage row
// (failed → pending → dispatched for category A/C; failed →
// awaiting_approval for category-D SLA-timeout). 4xx surfaces:
//   - 404 stage_not_found
//   - 422 retry_not_applicable (category B / gate-rejected D — the
//     workflow or spec needs to change first; a fresh run is the
//     right next step)
func (c *apiClient) RetryStage(ctx context.Context, id uuid.UUID) (*Stage, error) {
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/retry", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AcceptanceAdmissionResult mirrors the backend's acceptance-admission 200 body
// (#1928). ShortCircuited is the only always-present field: true means the
// backend settled the acceptance stage server-side (a not_validated verdict for
// the two basis-bearing predicates, #2347, or a skip marker for the out-of-scope
// predicate — NO runner needed either way) and Kind/Basis/CriteriaTotal/Stage are
// populated; false is the normal no-op path (proceed to spawn as today).
//
// NeedsTarget / TargetHosts / ExpectedHeadSHA (E48.6 / #1953) ride the
// short_circuited:false path: NeedsTarget:true means the approved plan requires
// LIVE validation and the spec declares egress target hosts, so the dispatch verb
// must probe the target host FROM THE DISPATCH HOST and refuse a doomed runner.
// TargetHosts is the verbatim spec-declared host list; ExpectedHeadSHA is the
// resolved merge-candidate head SHA (may be empty when ledger resolution failed —
// the verb then proceeds with a warning). An OLDER backend omits all three, so
// they decode to their zero values (NeedsTarget=false) and the verb spawns as
// today — the mixed-version degrade.
type AcceptanceAdmissionResult struct {
	ShortCircuited  bool     `json:"short_circuited"`
	Kind            string   `json:"kind"`
	Basis           string   `json:"basis"`
	CriteriaTotal   int      `json:"criteria_total"`
	Stage           *Stage   `json:"stage"`
	NeedsTarget     bool     `json:"needs_target"`
	TargetHosts     []string `json:"target_hosts"`
	ExpectedHeadSHA string   `json:"expected_head_sha"`
}

// AcceptanceDispatchAdmission POSTs the pre-spawn acceptance-admission check via
// `POST /v0/stages/{stage_id}/acceptance-admission` (#1928): the backend
// evaluates the approved plan's three short-circuit predicates and, on a hit,
// settles the acceptance stage to a not_validated verdict (#2347) or an
// out-of-scope skip marker (no verdict), WITHOUT a runner. The dispatch
// verbs call it for an acceptance stage before recording spawn evidence or
// spawning; a short_circuited:true result means skip the spawn. Follows
// RetryStage's shape — a non-2xx surfaces as *apiError so
// maybeShortCircuitAcceptance can classify it: a 4xx rejection (401 / 403
// cross_run_admission / 422 / a body-decoded 404 stage_not_found) fails CLOSED,
// while a transport error (network / 5xx) and a route-absent bare 404 — an older
// fishhawkd that predates the endpoint (version skew, #1937) — fail OPEN.
func (c *apiClient) AcceptanceDispatchAdmission(ctx context.Context, stageID uuid.UUID) (*AcceptanceAdmissionResult, error) {
	var res AcceptanceAdmissionResult
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/acceptance-admission", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// HostDispatchResult mirrors the backend's host-dispatch 200 body (#1912):
// whether this call drove the stage pending|awaiting_host_dispatch → dispatched
// (the spawn marker) and the resulting stage state. Transitioned:true is the
// common case (a parked stage marked as a spawn attempt); Transitioned:false is
// the idempotent no-op — the stage was already 'dispatched', a legal manual
// re-dispatch of a stage whose spawned runner died, which the caller proceeds on.
//
// BaseBranch (#2363) is the branch a DECOMPOSED FAN-OUT CHILD must spawn
// against — the parent's consolidated branch, returned only once every
// dependency slice that child declares is provably merged onto it. It is empty
// for every other caller (a non-decomposed run, the parent's own re-dispatch, a
// wave-0 child with no dependencies), and an empty value means "keep the base
// you already had". The server is the authority on the per-wave re-base; a
// caller must NOT derive one of its own.
type HostDispatchResult struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
	BaseBranch   string `json:"base_branch,omitempty"`
}

// HostDispatchStage marks a host spawn against a runner_kind-locked-local stage
// via POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch (#1912): the
// backend cannot spawn the host-local runner (ADR-024), so it parks an agent
// stage at 'awaiting_host_dispatch' rather than 'dispatched'. The MCP host-spawn
// verbs (fishhawk_run_stage, fishhawk_dispatch_stage, fishhawk_drive_run) call
// this IMMEDIATELY BEFORE spawning the runner and FAIL CLOSED on any error, so
// post-#1912 'dispatched' unambiguously means "a spawn attempt exists". The
// endpoint CAS-transitions {pending, awaiting_host_dispatch} → dispatched.
// Callers fail closed on a non-nil error (transport / 4xx). 4xx surfaces:
//   - 401 authentication_required / 403 insufficient_scope (needs write:runs)
//   - 404 stage_not_found (unknown stage, or the stage's run_id disagrees)
//   - 409 dispatch_not_admissible — either a running/terminal/awaiting_* gate
//     state (a live or settled stage can never be re-marked as a fresh spawn),
//     a run LOCKED to a non-local runner_kind, or a non-host-spawn stage (a
//     human-executed or auto-merge review-gate stage) — none of which is ever
//     host-spawned (#1912 fix-up)
//   - 409 dependency_not_satisfied — the decomposition wave-order guard
//     (E48.99 / #2546): this run is a fan-out child whose declared dependency
//     slice has not reached run state 'succeeded'. Annotated here ONCE (below)
//     as a deliberate ordering refusal, so every host-spawn verb
//     (fishhawk_dispatch_stage, fishhawk_run_stage, fishhawk_drive_run,
//     fishhawk_run_children) inherits it with no per-call-site edit.
//   - 409 wave_not_integrated — the per-wave integration guard (E50.13 /
//     #2363): this fan-out child's dependency slices have all SUCCEEDED (so the
//     wave-order guard admitted) but are not yet merged onto the parent's
//     consolidated branch, either because no integration has happened or
//     because the newest one is STALE. Annotated here ONCE (below) so every
//     host-spawn verb inherits an actionable message with no per-call-site
//     edit. It clears on its own: the server integrates between waves.
//   - 500 dependency_check_failed — the guard's parent-plan / sibling / audit
//     read errored (retryable), never a silent admit.
func (c *apiClient) HostDispatchStage(ctx context.Context, runID, stageID uuid.UUID) (*HostDispatchResult, error) {
	path := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/host-dispatch"
	var res HostDispatchResult
	if err := c.do(ctx, http.MethodPost, path, nil, &res); err != nil {
		// Annotate the wave-order refusal ONCE, at the client, as a deliberate
		// ordering refusal rather than an opaque infrastructure failure. The
		// wrap preserves the *apiError in the chain (%w), so callers that
		// errors.As for it — and every host-spawn verb, which fails closed on
		// ANY non-nil error — are unchanged. The blocking sibling is named in
		// the wrapped error's message + details; fishhawk_get_run_status's
		// children[] block answers "what may I dispatch next".
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "dependency_not_satisfied" {
			return nil, fmt.Errorf(
				"dispatch refused by the decomposition wave-order guard: this fan-out child depends on a sibling slice that has not succeeded yet — dispatch the blocking sibling first (see fishhawk_get_run_status children[] depends_on/blocked/blocked_by): %w",
				err)
		}
		// The per-wave integration refusal (#2363) is annotated the same way and
		// for the same reason, but says something different: nothing is wrong
		// and nothing must be dispatched first — the predecessors are done and
		// the server merges them between waves, so the action is to WAIT.
		if errors.As(err, &ae) && ae.Code == "wave_not_integrated" {
			return nil, fmt.Errorf(
				"dispatch refused by the per-wave integration guard: this fan-out child's predecessors have succeeded but are not yet integrated onto the parent's consolidated branch; the server integrates between waves — retry the dispatch shortly (fishhawk_await_children releases when the child becomes dispatchable): %w",
				err)
		}
		return nil, err
	}
	return &res, nil
}

// Reap-failure body caps (#1791). The reap-failure endpoint caps the request
// body at 32*1024 bytes (backend/internal/server/reap_failure.go
// maxReapFailureBodyBytes) and rejects an oversized body 413 body_too_large.
// The detached reaper re-POSTs the runner_failed line's detail here, so a
// category-B failure whose detail embeds the whole multi-module verify output
// would 413 the backstop too — the exact #1791 double-failure. reason is a short
// classification and detail is the large diagnostic, so detail gets the bulk of
// the budget; their sum plus the JSON envelope stays under 32*1024.
// aggressiveReapFailureBytes is the far smaller cap the bounded post-4xx retry
// re-marshals both fields with. Mirrors upload.MaxFailureReportReasonBytes /
// AggressiveFailureReportReasonBytes (a separate Go module — the helper cannot
// be shared, so each module defines its own with the same head+tail contract).
const (
	maxReapFailureReasonBytes  = 4 * 1024
	maxReapFailureDetailBytes  = 26 * 1024
	aggressiveReapFailureBytes = 2 * 1024
)

// truncateReason bounds s to at most max bytes for a reap-failure field (#1791).
// When s already fits it is returned byte-identical. Otherwise the middle is
// elided — a head + a "\n… [truncated N bytes] …\n" marker + a tail — so BOTH
// the leading classification and the trailing summary survive, and the result
// never exceeds max. Same contract as the runner's upload.TruncateReason; kept
// package-local because backend → runner is not an allowed import direction.
func truncateReason(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const markerFmt = "\n… [truncated %d bytes] …\n"
	// Size the head/tail budget against the marker rendered with the LARGEST
	// possible elided count (len(s)); the real marker cannot have more digits,
	// so the final head+marker+tail can only be <= max.
	upper := fmt.Sprintf(markerFmt, len(s))
	keep := max - len(upper)
	if keep <= 0 {
		return s[:max]
	}
	head := keep / 2
	tail := keep - head
	elided := len(s) - head - tail
	marker := fmt.Sprintf(markerFmt, elided)
	return s[:head] + marker + s[len(s)-tail:]
}

// reapFailureRequest mirrors the backend's
// `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` body
// (`backend/internal/server/reap_failure.go::reapFailureRequest`). Both
// category and reason are required; detail and exit_code are optional
// diagnostics.
//
// ExpectedState is the OPTIONAL compare-and-set precondition (E67.51 / #2699).
// It is a *string so ABSENCE (an unconditional report — the detached reaper and
// run_children's spawn-error compensation) is distinguishable on the wire from a
// PRESENT value: with omitempty a nil pointer emits NO key at all, keeping those
// callers' request bytes identical to what they were before #2699, while a
// non-nil pointer always emits the key. The server rejects a present-but-empty
// value 400 rather than reading it as unconditional, so an empty pin can never
// silently downgrade to an unpinned reap.
type reapFailureRequest struct {
	Category      string  `json:"category"`
	Reason        string  `json:"reason"`
	Detail        string  `json:"detail,omitempty"`
	ExitCode      int     `json:"exit_code,omitempty"`
	ExpectedState *string `json:"expected_state,omitempty"`
}

// ReapFailureResult mirrors the backend's reap-failure 200 body: whether this
// report drove the stage to failed (false on the idempotent already-terminal
// no-op) and the resulting stage state.
type ReapFailureResult struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
}

// reapPreconditionCode is the backend error code for a LOST expected_state
// precondition (E67.51 / #2699; backend/internal/server/reap_failure.go). One
// literal shared by the no-retry guard here and fishhawk_reap_stage's operator
// classifier.
const reapPreconditionCode = "stage_state_precondition_failed"

// isReapPreconditionLost reports whether ae is the server's 409 refusal of a
// pinned reap.
func isReapPreconditionLost(ae *apiError) bool {
	return ae != nil && ae.StatusCode == http.StatusConflict && ae.Code == reapPreconditionCode
}

// isReapExpectedStateRejected reports whether ae is a 400 attributable to the
// expected_state field — either an out-of-set value, or (the VERSION-SKEW case)
// an OLD fishhawkd rejecting the unknown field outright, since the reap-failure
// endpoint decodes with DisallowUnknownFields. Matching on the rendered error
// covers both, because the skew's message names the field while its details map
// does not.
func isReapExpectedStateRejected(ae *apiError) bool {
	return ae != nil && ae.StatusCode == http.StatusBadRequest && strings.Contains(ae.Error(), "expected_state")
}

// isReapExpectedStateOutOfSet narrows isReapExpectedStateRejected to the
// SAME-VERSION half of it: a current fishhawkd refusing a precondition that
// names a state outside its conditional anchor set. That 400 carries a
// STRUCTURED details map — field=expected_state plus the accepted list — which
// the version-skew 400 cannot, because an OLD fishhawkd rejects the field at the
// DECODER (DisallowUnknownFields) and never reaches the handler's validation.
// The two are otherwise indistinguishable, and telling an operator to rebuild
// fishhawkd when nothing is skewed sends them at the wrong repair (E67.51
// fix-up). Keyed on the details map, never on message text: the accepted list is
// what only the validating handler can produce.
func isReapExpectedStateOutOfSet(ae *apiError) bool {
	if ae == nil || ae.StatusCode != http.StatusBadRequest || ae.Details == nil {
		return false
	}
	if field, _ := ae.Details["field"].(string); field != "expected_state" {
		return false
	}
	_, hasAccepted := ae.Details["accepted"]
	return hasAccepted
}

// ReportStageFailure reports a spawn-phase runner failure — a detached runner
// that exited non-zero BEFORE reporting a terminal stage state — to the backend
// via `POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure` (#1747). The
// backend fails the stage (category C is the retryable infrastructure class),
// writes a dispatch_reaper_failed audit entry, and advances the run, so the
// stage lands in failed/category-C instead of stuck 'dispatched'. Idempotent: a
// double-report against an already-terminal stage returns
// {transitioned:false}. Mirrors VouchCommit/RetryStage. 4xx surfaces:
//   - 400 validation_failed (category other than B/C, empty reason, malformed
//     UUIDs/body)
//   - 401 authentication_required / 403 insufficient_scope (needs write:runs)
//   - 404 stage_not_found (unknown stage, or the stage's run_id disagrees)
func (c *apiClient) ReportStageFailure(ctx context.Context, runID, stageID uuid.UUID, category, reason, detail string, exitCode int) (*ReapFailureResult, error) {
	return c.ReportStageFailureFrom(ctx, runID, stageID, "", category, reason, detail, exitCode)
}

// ReportStageFailureFrom is ReportStageFailure with the OPTIONAL compare-and-set
// precondition (E67.51 / #2699). An expectedState of "" sends NO expected_state
// key — the unconditional report, byte-identical to the pre-#2699 body — and a
// non-empty value PINS the reap: the server refuses with 409
// stage_state_precondition_failed, atomically at its row-locked CAS, if the stage
// is not (or no longer) in that state. fishhawk_reap_stage passes the state IT
// last observed, which is what turns its client-side re-probe from a narrowing
// into a server-enforced compare-and-set.
//
// The empty-string-means-unconditional convention lives HERE, at the Go call
// boundary, and never reaches the wire: an empty expectedState omits the field,
// so the server's presence check (which rejects a present-but-empty value 400)
// is not weakened by it.
//
// Additional 4xx surfaces beyond ReportStageFailure's:
//   - 400 validation_failed — expected_state outside {pending, dispatched,
//     running}; ALSO what an OLD fishhawkd answers for the unknown field, since
//     the endpoint decodes with DisallowUnknownFields (the version-skew case
//     reapStage classifies).
//   - 409 stage_state_precondition_failed — the precondition lost.
func (c *apiClient) ReportStageFailureFrom(ctx context.Context, runID, stageID uuid.UUID, expectedState, category, reason, detail string, exitCode int) (*ReapFailureResult, error) {
	// Truncate both fields so the marshalled body fits the endpoint's 32*1024
	// cap (#1791) — otherwise this backstop 413s for the very oversized detail
	// that stranded the stage in the first place.
	reason = truncateReason(reason, maxReapFailureReasonBytes)
	detail = truncateReason(detail, maxReapFailureDetailBytes)
	var expected *string
	if expectedState != "" {
		expected = &expectedState
	}
	body, err := json.Marshal(reapFailureRequest{
		Category: category, Reason: reason, Detail: detail, ExitCode: exitCode, ExpectedState: expected,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal reap-failure: %w", err)
	}
	path := "/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/reap-failure"
	var res ReapFailureResult
	err = c.do(ctx, http.MethodPost, path, body, &res)
	if err == nil {
		return &res, nil
	}
	// A 4xx (esp. 413 body_too_large) means even the normal-cap body was
	// rejected. Re-marshal both fields with the aggressive cap and re-POST
	// exactly once (#1791). A 5xx, a network error, or a second 4xx surfaces
	// unchanged — no loop.
	var ae *apiError
	if errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 {
		// ...EXCEPT for the two responses a CONDITIONAL reap must not re-POST
		// (E67.51 / #2699). The aggressive retry exists for ONE reason — a 413
		// body_too_large caused by an oversized diagnostic — and re-sending the
		// same pinned body cannot change either of these answers: a lost
		// precondition is a fact about the STAGE, and an unknown-field 400 is a
		// fact about the SERVER's build. Re-POSTing them would also contradict what
		// the verb tells the operator ("the stage was NOT failed; re-read and
		// re-invoke") by taking a second reap attempt behind their back. Gated on
		// expected != nil so unconditional callers keep the retry arm verbatim.
		if expected != nil && (isReapPreconditionLost(ae) || isReapExpectedStateRejected(ae)) {
			return nil, err
		}
		// The aggressive re-marshal MUST carry the pin: dropping it here would
		// turn a 413-retried conditional reap into an UNPINNED one — the silent
		// downgrade this feature exists to make impossible.
		aggBody, mErr := json.Marshal(reapFailureRequest{
			Category:      category,
			Reason:        truncateReason(reason, aggressiveReapFailureBytes),
			Detail:        truncateReason(detail, aggressiveReapFailureBytes),
			ExitCode:      exitCode,
			ExpectedState: expected,
		})
		if mErr != nil {
			return nil, fmt.Errorf("marshal aggressive reap-failure: %w", mErr)
		}
		var aggRes ReapFailureResult
		if aggErr := c.do(ctx, http.MethodPost, path, aggBody, &aggRes); aggErr != nil {
			return nil, aggErr
		}
		return &aggRes, nil
	}
	return nil, err
}

// fixupRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/fixup` body
// (`backend/internal/server/fixup.go::fixupRequest`). ConcernIDs is the
// PRIMARY addressing scheme (#964): stable concern UUIDs from the run's
// concerns block. Concerns (positional indices into the stage's
// flattened resolved concern set) is DEPRECATED and only valid when
// ConcernIDs is absent — the backend rejects supplying both. Reason is
// an optional operator note recorded on the audit entry.
type fixupRequest struct {
	ConcernIDs []string `json:"concern_ids,omitempty"`
	Concerns   []int    `json:"concerns,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	// AllowCreate declares net-new files the fix-up will create (#823),
	// folded into the effective scope.files for that pass only. omitempty:
	// the common fix-up omits it and stays unaffected.
	AllowCreate []string `json:"allow_create,omitempty"`
	// ForceAdditionalPass is the bounded operator override (#860): grant ONE
	// fix-up pass beyond the normal budget, hard-capped at 3 total passes.
	// omitempty: the common fix-up omits it and stays on the default budget.
	ForceAdditionalPass bool `json:"force_additional_pass,omitempty"`
	// ImplementModel is the optional operator/driver model override for this
	// fix-up pass (#1164). omitempty: the common fix-up omits it and inherits
	// the run's already-resolved implement model (byte-identical default).
	ImplementModel string `json:"implement_model,omitempty"`
	// OperatorConcern is the optional free-text operator instruction routed
	// back to the agent with NO pre-existing review concern (#1311). omitempty:
	// the common fix-up omits it and addresses recorded concerns instead.
	OperatorConcern string `json:"operator_concern,omitempty"`
	// OperatorEvidence declares that the OPERATOR executed a reproduction of
	// the concerns this pass routes back (E48.103 / #2551), making the operator
	// the authority a reviewer confirmation alone cannot override. omitempty:
	// an unset value leaves the request body byte-identical.
	OperatorEvidence string `json:"operator_evidence,omitempty"`
}

// FixupStage routes one or more advisory implement-review concerns back
// to the implement agent for a bounded, operator-gated fix-up pass via
// `POST /v0/stages/{stage_id}/fixup`. Distinct from RetryStage: fix-up
// re-opens a HEALTHY review gate, commits onto the SAME PR branch, and
// is bounded (default one pass). It applies in either flow: the implement
// stage parked at its own gate (awaiting_approval → pending), or a
// succeeded implement stage whose run still holds a separate review stage
// at awaiting_approval (succeeded → pending, the review stage re-parked
// alongside — the push_and_open_pr flow, #780). Returns the re-opened
// Stage row (pending, or dispatched once the orchestrator advances it
// before the response returns). 4xx surfaces:
//   - 400 validation_failed (no concern selection / both concern_ids and
//     indices supplied / out-of-range index / unknown, foreign,
//     plan-stage, or non-open concern_id)
//   - 403 cross_run_fixup (a run-bound token reaching another run's stage)
//   - 404 stage_not_found
//   - 422 fixup_not_applicable (no recorded approve_with_concerns verdict,
//     or the stage is not at the gate / its review gate already resolved)
//   - 422 fixup_budget_exhausted (the NORMAL bounded pass count is spent;
//     details carry max_passes + used — one more pass is still available
//     via forceAdditionalPass below)
//   - 422 fixup_ceiling_reached (the hard ceiling of 3 total passes is
//     reached; the override cannot push past it — merge-with-follow-up or a
//     fresh run; details carry ceiling + used)
//   - 422 fixup_invalid_model (the resolved implement_model override is not in
//     the deployment's per-adapter allow-list; details carry the resolved
//     model, source, and adapter)
//
// allowCreate declares net-new files this pass will create (#823), folded
// into the effective scope.files for that dispatch only; an invalid entry
// (absolute / containing "..") surfaces 400 validation_failed.
// forceAdditionalPass is the bounded operator override (#860): grant ONE
// pass beyond the normal budget, hard-capped at 3 total passes.
// implementModel is the optional operator/driver model override for this pass
// (#1164), validated server-side against the deployment allow-list (422
// fixup_invalid_model on reject); empty inherits the run's implement model.
// operatorConcern is the optional free-text operator instruction routed back
// to the agent with NO pre-existing review concern (#1311); empty addresses
// recorded concerns instead.
// operatorEvidence declares the operator executed a reproduction of the routed
// concerns (E48.103 / #2551): those concerns can then no longer be retired by a
// reviewer's `confirmed` delta-verification verdict — only by a waive, a defer,
// or a genuine fix. 400 validation_failed on a whitespace-only or over-length
// (>4000 byte) value; it is NOT a selection input, so it does not by itself
// satisfy the at-least-one-of concern selection rule.
func (c *apiClient) FixupStage(ctx context.Context, id uuid.UUID, concernIDs []string, concerns []int, reason string, allowCreate []string, forceAdditionalPass bool, implementModel, operatorConcern, operatorEvidence string) (*Stage, error) {
	body, err := json.Marshal(fixupRequest{ConcernIDs: concernIDs, Concerns: concerns, Reason: reason, AllowCreate: allowCreate, ForceAdditionalPass: forceAdditionalPass, ImplementModel: implementModel, OperatorConcern: operatorConcern, OperatorEvidence: operatorEvidence})
	if err != nil {
		return nil, fmt.Errorf("marshal fixup: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+id.String()+"/fixup", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// reviseRequest mirrors the backend's
// `POST /v0/stages/{stage_id}/revise` body
// (`backend/internal/server/revise.go::reviseRequest`). Constraint is the
// operator's binding design constraint the planner must revise the prior
// plan to satisfy — REQUIRED. ForceAdditionalPass is the bounded operator
// override: grant ONE revise pass beyond the normal budget, hard-capped
// at 3 total passes per stage.
type reviseRequest struct {
	Constraint          string `json:"constraint"`
	ForceAdditionalPass bool   `json:"force_additional_pass,omitempty"`
}

// SubmitRevise re-opens a plan stage parked at its approval gate to
// re-plan IN PLACE against a binding operator design constraint via
// `POST /v0/stages/{stage_id}/revise` (#1099). The third plan-gate
// verdict alongside approve/reject: the constraint is injected into the
// re-dispatched plan prompt (the #558 binding channel, a dedicated
// "Revision constraint" section) with the prior plan as the revision
// base, and the stage re-enters the review → approve gate. Distinct from
// RetryStage: revise re-opens a HEALTHY plan gate and is bounded
// (default one pass). Returns the re-opened Stage row (pending, or
// dispatched once the orchestrator advances it before the response
// returns). 4xx surfaces:
//   - 400 validation_failed (empty constraint / malformed UUID)
//   - 403 cross_run_revise (a run-bound token reaching another run's
//     stage) or insufficient_scope
//   - 404 stage_not_found
//   - 409 revise_not_applicable (the stage is not a plan stage parked at
//     awaiting_approval)
//   - 409 revise_budget_exhausted (the NORMAL bounded pass count is spent;
//     one more pass is available via forceAdditionalPass)
//   - 409 revise_ceiling_reached (the hard ceiling of 3 total passes is
//     reached; the override cannot push past it — reject → fresh-run replan)
func (c *apiClient) SubmitRevise(ctx context.Context, stageID uuid.UUID, constraint string, forceAdditionalPass bool) (*Stage, error) {
	body, err := json.Marshal(reviseRequest{Constraint: constraint, ForceAdditionalPass: forceAdditionalPass})
	if err != nil {
		return nil, fmt.Errorf("marshal revise: %w", err)
	}
	var s Stage
	if err := c.do(ctx, http.MethodPost, "/v0/stages/"+stageID.String()+"/revise", body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// waiveConcernRequest mirrors the backend's
// `POST /v0/concerns/{concern_id}/waive` body
// (`backend/internal/server/waive.go::waiveConcernRequest`). Reason is
// REQUIRED — the backend refuses an empty reason with 400.
type waiveConcernRequest struct {
	Reason string `json:"reason"`
}

// WaivedConcern mirrors the backend's waive 200 body: the updated
// concern row, now in state waived with the operator's reason as
// state_reason.
type WaivedConcern struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	StageKind   string `json:"stage_kind"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Note        string `json:"note"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
}

// WaiveConcern transitions one open review concern to the terminal
// waived state with a required, audited reason via
// `POST /v0/concerns/{concern_id}/waive` (E22.X / #984). 4xx surfaces:
//   - 400 validation_failed (empty reason)
//   - 403 cross_run_waive (a run-bound token reaching another run's
//     concern) or insufficient_scope
//   - 404 concern_not_found
//   - 422 concern_waive_conflict (the concern is not in an open state —
//     already waived/superseded/addressed; details carry the from/to pair)
//   - 503 concern_store_unconfigured
func (c *apiClient) WaiveConcern(ctx context.Context, id uuid.UUID, reason string) (*WaivedConcern, error) {
	body, err := json.Marshal(waiveConcernRequest{Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal waive: %w", err)
	}
	var out WaivedConcern
	if err := c.do(ctx, http.MethodPost, "/v0/concerns/"+id.String()+"/waive", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// groomingDispositionRequestBody mirrors the backend's capture request body
// (`backend/internal/server/grooming_dispositions.go::groomingDispositionRequest`).
type groomingDispositionRequestBody struct {
	Dispositions []GroomingDispositionEntry `json:"dispositions"`
}

// RecordGroomingDispositions records a batch of per-entry grooming verdicts via
// `POST /v0/runs/{run_id}/grooming-dispositions` (E54.30 / #2843). The batch is
// validated ATOMICALLY server-side: a request naming one unknown entry id
// records NOTHING. 4xx/5xx surfaces:
//   - 400 validation_failed (unparseable body, empty batch, empty entry_id,
//     an entry_id repeated within one batch)
//   - 400 grooming_verdict_invalid (a verdict outside approved/rejected/amended)
//   - 403 run_token_forbidden (a run-bound agent token, even for its own run)
//   - 403 operator_agent_forbidden (a delegated operator-agent token)
//   - 403 insufficient_scope (token lacks write:approvals)
//   - 409 grooming_report_absent (the run shipped no grooming_report)
//   - 422 grooming_entry_unknown (an id the newest report does not declare)
//   - 503 grooming_dispositions_unconfigured
func (c *apiClient) RecordGroomingDispositions(ctx context.Context, runID uuid.UUID,
	dispositions []GroomingDispositionEntry) (*RecordGroomingDispositionsOutput, error) {
	body, err := json.Marshal(groomingDispositionRequestBody{Dispositions: dispositions})
	if err != nil {
		return nil, fmt.Errorf("marshal grooming dispositions: %w", err)
	}
	var out RecordGroomingDispositionsOutput
	if err := c.do(ctx, http.MethodPost,
		"/v0/runs/"+runID.String()+"/grooming-dispositions", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListGroomingDispositions reads back the recorded dispositions for the run's
// NEWEST grooming_report artifact via
// `GET /v0/runs/{run_id}/grooming-dispositions` (E54.30 / #2843). Read access
// only — the operator-only posture is scoped to CAPTURE. 4xx/5xx surfaces:
//   - 409 grooming_report_absent
//   - 503 grooming_dispositions_unconfigured
func (c *apiClient) ListGroomingDispositions(ctx context.Context, runID uuid.UUID) (*ListGroomingDispositionsOutput, error) {
	var out ListGroomingDispositionsOutput
	if err := c.do(ctx, http.MethodGet,
		"/v0/runs/"+runID.String()+"/grooming-dispositions", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// deferConcernRequest mirrors the backend's defer 200 request body
// (`backend/internal/server/defer_concern.go::deferConcernRequest`). The
// follow-up body is auto-drafted server-side; the operator supplies only
// the title coordinates + optional overrides.
type deferConcernRequest struct {
	ParentEpic string   `json:"parent_epic,omitempty"`
	N          string   `json:"n,omitempty"`
	Type       string   `json:"type,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// DeferConcernParams bundles the caller-supplied defer inputs the tool
// layer collects, so DeferConcern's signature stays readable.
type DeferConcernParams struct {
	ParentEpic string
	N          string
	Type       string
	Labels     []string
	Note       string
}

// DeferredConcern mirrors the backend defer 200 body's `concern` block:
// the updated concern row, now in state deferred with state_reason
// naming the filed follow-up issue.
type DeferredConcern struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	StageKind   string `json:"stage_kind"`
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Note        string `json:"note"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
}

// DeferFiledIssue mirrors the backend defer 200 body's `issue` block: the
// filed follow-up work item.
type DeferFiledIssue struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	// DefaultedLabels / MissingLabelNamespaces mirror the work-item filing's
	// LOUD label-completeness report (#1616) on the deferred follow-up.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// DeferredConcernResult mirrors the backend defer 200 body: the filed
// follow-up work item plus the now-deferred concern row.
type DeferredConcernResult struct {
	Concern DeferredConcern `json:"concern"`
	Issue   DeferFiledIssue `json:"issue"`
}

// DeferConcern converts one open review concern into a follow-up work
// item and transitions the concern to the terminal deferred state via
// `POST /v0/concerns/{concern_id}/defer` (E22.X / #1202). 4xx/5xx
// surfaces:
//   - 403 cross_run_defer (a run-bound token reaching another run's
//     concern) or insufficient_scope
//   - 404 concern_not_found
//   - 422 concern_defer_conflict (the concern is not open, or a
//     post-filing transition race — details may carry the filed issue url)
//   - 422 work_item_invalid (the follow-up violates the type's conventions)
//   - 501 provider_unimplemented / 502 work_item_filing_failed (the
//     provider could not file — the concern stays OPEN, no transition)
//   - 503 concern_store_unconfigured
func (c *apiClient) DeferConcern(ctx context.Context, id uuid.UUID, p DeferConcernParams) (*DeferredConcernResult, error) {
	body, err := json.Marshal(deferConcernRequest(p))
	if err != nil {
		return nil, fmt.Errorf("marshal defer: %w", err)
	}
	var out DeferredConcernResult
	if err := c.do(ctx, http.MethodPost, "/v0/concerns/"+id.String()+"/defer", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// resetBranchRequest mirrors the backend's
// `POST /v0/runs/{run_id}/reset-branch` body
// (`backend/internal/server/reset_branch.go::resetBranchRequest`).
// Confirm MUST be true — the reset is destructive (force-rewinds the PR
// head ref), so the backend refuses a missing/false confirm with 400.
type resetBranchRequest struct {
	Reason  string `json:"reason,omitempty"`
	Confirm bool   `json:"confirm"`
}

// ResetBranchResult mirrors the backend's reset-branch 200 body: the
// summary of a successful rewind. Surfaced back to the operator so the
// dropped commit + recovery path are visible.
type ResetBranchResult struct {
	RunID                 string `json:"run_id"`
	PRNumber              int    `json:"pr_number"`
	Branch                string `json:"branch"`
	DroppedOffendingSHA   string `json:"dropped_offending_sha"`
	ResetToSHA            string `json:"reset_to_sha"`
	PriorHeadSHA          string `json:"prior_head_sha"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	RecoveryNote          string `json:"recovery_note"`
}

// ResetRunBranch force-rewinds a run/PR branch back to its last
// run-authored HEAD, dropping a foreign commit pushed ON TOP of the run's
// commits (ADR-035 remediation, #867), via
// `POST /v0/runs/{run_id}/reset-branch`. Destructive + operator-gated:
// confirm is always sent true (the tool layer requires the operator's
// confirm). 4xx/5xx surfaces:
//   - 400 confirmation_required (confirm not true)
//   - 403 cross_run_reset (a run-bound token reaching another run's branch)
//   - 404 run_not_found
//   - 422 reset_out_of_scope (the foreign commit is an ancestor, not on
//     top — owned by prevention #861/#865)
//   - 422 reset_not_applicable (the tip is already the last run-authored
//     HEAD; nothing on top to drop)
//   - 422 reset_not_determinable (fail-closed: the lineage could not be
//     classified with certainty, or the lease re-check failed)
func (c *apiClient) ResetRunBranch(ctx context.Context, runID uuid.UUID, reason string) (*ResetBranchResult, error) {
	body, err := json.Marshal(resetBranchRequest{Reason: reason, Confirm: true})
	if err != nil {
		return nil, fmt.Errorf("marshal reset-branch: %w", err)
	}
	var res ResetBranchResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/reset-branch", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ReviveRestoredStage mirrors the backend's reviveRestoredStage wire shape
// (`backend/internal/server/revive.go`): one re-parked stage in a revive's
// batch. StageID is typed `string` (not `uuid.UUID`) per the #371 reflection
// rule so the MCP SDK's response-schema reflection sees a string — the JSON
// payload IS a string, and a `uuid.UUID` (a 16-byte array) would surface as
// `type: array` and reject at the wire boundary.
type ReviveRestoredStage struct {
	StageID       string `json:"stage_id" jsonschema:"the re-parked stage's UUID"`
	Type          string `json:"type" jsonschema:"the stage kind (plan/implement/review/…)"`
	PriorCategory string `json:"prior_category" jsonschema:"the stage's failure category before the revive (A/C, or a retryable D)"`
	PriorReason   string `json:"prior_reason" jsonschema:"the stage's failure_reason from before the revive"`
	RestoredState string `json:"restored_state" jsonschema:"the pre-dispatch state the stage was re-parked to (pending for A/C, awaiting_approval for a D SLA-timeout gate, awaiting_children for a decomposed-parent implement)"`
}

// ReviveRunResult mirrors the backend's revive 200 body (`reviveResponse`):
// the re-opened run (now running) plus the per-stage re-park summary. The
// nested Run reuses the client's Run type, which already decodes the backend's
// runResponse (it is the GET /v0/runs/{id} shape).
type ReviveRunResult struct {
	Run            Run                   `json:"run"`
	RestoredStages []ReviveRestoredStage `json:"restored_stages"`
	// AuditWarning mirrors the backend's reviveResponse.audit_warning (#1943):
	// set ONLY when the backend committed the revive (re-parks + reopen) but
	// then failed to append the run_revived chained provenance record. The
	// revive succeeded; the warning is the caller-visible signal the audit
	// record is missing. Omitted (empty) on a clean revive.
	AuditWarning string `json:"audit_warning,omitempty"`
}

// ReviveRun re-admits a terminal-FAILED run for another operator turn via
// `POST /v0/runs/{run_id}/revive` (#1915): the backend pre-validates that
// EVERY failed stage is retryable, then re-parks each failed stage in its
// correct gate-ordered pre-dispatch state (A/C → pending, D SLA-timeout →
// awaiting_approval, decomposed-parent implement → awaiting_children) and flips
// the run failed → running. CRUCIALLY revive performs NO orchestrator handoff
// and never dispatches — it re-parks only, so the #1700 wrong-order
// re-dispatch corruption is structurally impossible; dispatch happens later at
// each stage's proper gate turn via the existing verbs. Operator-token-only,
// modeled on ResetRunBranch/VouchCommit. 4xx/5xx surfaces:
//   - 403 agent_token_forbidden (a run-bound agent/mcp token attempted revive)
//   - 403 insufficient_scope (token lacks write:stages or write:retries)
//   - 404 run_not_found
//   - 409 invalid_state_transition (a concurrent transition raced the reopen)
//   - 422 revive_not_applicable (the run is not failed, has no failed stage,
//     or a failed stage is non-retryable — category-B, D-rejected, or no
//     recorded category; the message names the blocking stage. No partial
//     mutation: the whole revive is refused pre-transition)
//   - 503 revive_unconfigured (run/audit repositories not wired)
func (c *apiClient) ReviveRun(ctx context.Context, runID uuid.UUID) (*ReviveRunResult, error) {
	var res ReviveRunResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/revive", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// HealthInfo is the decoded slice of GET /healthz this package needs. Only
// the fields a tool reads are mirrored; the rest of the body is ignored.
//
// ProcessStartOK is the load-bearing field (#2712): it reports whether
// process_start was BOTH present AND parseable. A caller must branch on it
// rather than on ProcessStart being zero, because a zero time.Time compares as
// BEFORE every audit timestamp — silently turning every pending review into a
// false "the daemon restarted" verdict.
type HealthInfo struct {
	Version    string
	GitSHA     string
	StartNonce string

	ProcessStart   time.Time
	ProcessStartOK bool
}

// healthzBody is the wire shape /healthz answers with.
type healthzBody struct {
	Version      string `json:"version"`
	GitSHA       string `json:"git_sha"`
	StartNonce   string `json:"start_nonce"`
	ProcessStart string `json:"process_start"`
}

// Healthz reads GET /healthz on the short (30s) client. The request is
// deliberately UNAUTHENTICATED: /healthz is registered outside every auth
// wrapper (backend/internal/server/handlers.go) and carries no run-scoped
// data, so sending the caller's bearer would add a credential the endpoint
// neither needs nor reads.
//
// An absent or unparseable process_start is NOT an error — the daemon may
// predate the field (#2712) — it returns a HealthInfo with ProcessStartOK
// false, which every caller must treat as "undecidable", never as a boundary.
// A transport failure or non-200 IS returned as an error.
func (c *apiClient) Healthz(ctx context.Context) (*HealthInfo, error) {
	if c.baseURL == "" {
		return nil, errors.New("apiClient: baseURL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return nil, fmt.Errorf("build healthz request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read healthz body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("healthz: HTTP %d", resp.StatusCode)
	}
	var body healthzBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode healthz body: %w", err)
	}
	info := &HealthInfo{Version: body.Version, GitSHA: body.GitSHA, StartNonce: body.StartNonce}
	if body.ProcessStart != "" {
		if ts, perr := time.Parse(time.RFC3339Nano, body.ProcessStart); perr == nil {
			info.ProcessStart = ts
			info.ProcessStartOK = true
		}
	}
	return info, nil
}

// ReconciledReviewStage mirrors one row of the reconcile endpoint's `stages`
// array (backend/internal/server/review_reconcile.go::reconciledStage).
type ReconciledReviewStage struct {
	Stage            string `json:"stage"`
	ConfiguredAgents int    `json:"configured_agents"`
	LandedBefore     int    `json:"landed_before"`
	Synthesized      int    `json:"synthesized"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skip_reason,omitempty"`
}

// ReconcileReviewsResult mirrors the 200 body of
// POST /v0/runs/{run_id}/reviews/reconcile.
type ReconcileReviewsResult struct {
	RunID      string                  `json:"run_id"`
	Terminated bool                    `json:"terminated"`
	Stages     []ReconciledReviewStage `json:"stages"`
}

// ReconcileRunReviews invokes the on-demand orphaned-review recovery (#2712).
// It is idempotent: the endpoint synthesizes only the MISSING terminal entries
// for each stage's current round, so already-landed verdicts survive and a
// second call reports skip_reason=round_already_settled.
//
// 4xx surfaces: 400 validation_failed (bad UUID), 401/403 (unauthenticated /
// missing write:runs), 404 run_not_found.
func (c *apiClient) ReconcileRunReviews(ctx context.Context, runID uuid.UUID) (*ReconcileReviewsResult, error) {
	var res ReconcileReviewsResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/reviews/reconcile", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// mergeRunRequest mirrors the backend's `POST /v0/runs/{run_id}/merge`
// body (`backend/internal/server/merge_run.go::mergeRunRequest`, E48.7 /
// #1954). Verdict is REQUIRED — the merge records an audited operator
// verdict as a chained merge_verdict_recorded audit entry.
type mergeRunRequest struct {
	Verdict string `json:"verdict"`
}

// MergeRunResult mirrors the backend's merge 200 body: the merge was
// queued through the same GitHubMerger seam the delegated may_merge arm
// uses, with the chained verdict row's sequence surfaced back to the
// operator. AlreadyRecorded is the ENDPOINT-side idempotence signal
// (#1954 binding condition 1): a repeated POST that finds an existing
// merge_verdict_recorded row appends NO duplicate, responds
// already_recorded:true, and STILL dispatches the merge helper — so a
// 502-then-reinvoke re-queues the merge with no duplicate verdict row.
type MergeRunResult struct {
	MergeQueued     bool   `json:"merge_queued"`
	VerdictSequence int64  `json:"verdict_sequence"`
	PRURL           string `json:"pr_url"`
	AlreadyRecorded bool   `json:"already_recorded"`
}

// MergeRun records the operator merge verdict and queues the squash merge
// via `POST /v0/runs/{run_id}/merge` (E48.7 / #1954): the endpoint appends
// a chained merge_verdict_recorded audit entry (no duplicate on a repeated
// POST — already_recorded:true) and dispatches the shared merge helper the
// delegated may_merge arm uses. The endpoint does NOT wait — the terminal
// await is client-side in the fishhawk_merge_run tool, matching the
// await_audit no-server-state idiom. Verdict is REQUIRED. 4xx/5xx surfaces:
//   - 400 validation_failed (empty verdict)
//   - 403 run_token_forbidden (a run-bound agent token) / insufficient_scope
//     (token lacks write:approvals)
//   - 404 run_not_found
//   - 409 run_not_mergeable (no PR URL; run failed/cancelled),
//     acceptance_gate_not_passed (the acceptance gate is not
//     passed/not-declared/skipped-out-of-scope), or merge_checks_pending
//     (E67.56 / #2717 — the PR's required checks have not all passed, so GitHub
//     will not queue the merge; the verdict row is durable; details carry
//     verdict_sequence + reason:"checks_pending"). fishhawk_merge_run WAITS on
//     this one rather than surfacing it as an error.
//   - 502 merge_dispatch_failed (the verdict row is durable; the queue step
//     is retryable — re-POST re-queues with no duplicate row)
//   - 503 merge_unconfigured (the merger seam is not wired)
func (c *apiClient) MergeRun(ctx context.Context, runID uuid.UUID, verdict string) (*MergeRunResult, error) {
	body, err := json.Marshal(mergeRunRequest{Verdict: verdict})
	if err != nil {
		return nil, fmt.Errorf("marshal merge: %w", err)
	}
	var res MergeRunResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/merge", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// vouchCommitRequest mirrors the backend's
// `POST /v0/runs/{run_id}/vouch-commit` body
// (`backend/internal/server/vouch.go::vouchCommitRequest`). Both fields
// are required — the vouch is an audited operator declaration.
type vouchCommitRequest struct {
	SHA    string `json:"sha"`
	Reason string `json:"reason"`
}

// VouchCommitResult mirrors the backend's vouch-commit 200 body: the
// recorded declaration, surfaced back to the operator.
type VouchCommitResult struct {
	RunID      string `json:"run_id"`
	VouchedSHA string `json:"vouched_sha"`
	Reason     string `json:"reason"`
}

// VouchCommit declares a foreign commit on a run branch to be run-authored
// lineage (ADR-035 remediation, #1044), via
// `POST /v0/runs/{run_id}/vouch-commit`. The vouched SHA is unioned into
// the reported-head ledger, un-wedging the merge reconciler for an
// operator's mechanical remediation commit. Operator-token-only
// (write:stages); distinct from ResetRunBranch (which DROPS an on-top
// foreign commit) — vouch KEEPS the operator commit and attributes it.
// 4xx/5xx surfaces:
//   - 400 validation_failed (empty sha or reason)
//   - 403 run_token_forbidden (a run-bound agent token attempted the vouch)
//   - 403 insufficient_scope (token lacks write:stages)
//   - 404 run_not_found
//   - 503 vouch_unconfigured (run/audit repositories not wired)
func (c *apiClient) VouchCommit(ctx context.Context, runID uuid.UUID, sha, reason string) (*VouchCommitResult, error) {
	body, err := json.Marshal(vouchCommitRequest{SHA: sha, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal vouch-commit: %w", err)
	}
	var res VouchCommitResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/vouch-commit", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// acceptanceArbitrationRequest mirrors the backend's
// `POST /v0/runs/{run_id}/acceptance-arbitration` body
// (`backend/internal/server/acceptance_arbitration.go::acceptanceArbitrationRequest`).
// reason is required; acknowledge_failed_criteria is required only when the
// discharged verdict carries genuinely FAILED criteria.
type acceptanceArbitrationRequest struct {
	Reason                    string `json:"reason"`
	AcknowledgeFailedCriteria bool   `json:"acknowledge_failed_criteria"`
}

// ArbitrateAcceptanceResult mirrors the backend's acceptance-arbitration 200
// body (E66.37 / #2474): the recorded discharge. OutcomeSequence is the
// acceptance_outcome_recorded sequence the arbitration is BOUND to — a later
// re-run's higher-sequence verdict invalidates it by construction, which is why
// the field is surfaced rather than hidden.
type ArbitrateAcceptanceResult struct {
	RunID               string `json:"run_id"`
	AcceptanceGateState string `json:"acceptance_gate_state"`
	OutcomeSequence     int64  `json:"outcome_sequence"`
	ArbitrationSequence int64  `json:"arbitration_sequence"`
	AlreadyRecorded     bool   `json:"already_recorded"`
}

// ArbitrateAcceptance records the operator arbitration that discharges a PAGED
// acceptance triage (E66.37 / #2474), via
// `POST /v0/runs/{run_id}/acceptance-arbitration`. After it lands the acceptance
// gate reads acceptance_arbitrated and the ordinary merge verb admits the run,
// so the operator never has to leave the loop and hand-merge (losing the
// merge_verdict_recorded entry). Operator-token-only (write:approvals); a
// run-bound agent token is rejected outright. 4xx/5xx surfaces:
//   - 400 validation_failed (empty reason)
//   - 403 run_token_forbidden (a run-bound agent token attempted the arbitration)
//   - 403 insufficient_scope (token lacks write:approvals)
//   - 404 run_not_found
//   - 409 acceptance_arbitration_not_applicable (the gate is not parked at
//     acceptance_triage, or the correlated triage disposition did not page —
//     an auto-routed class-1/2 verdict keeps its automatic route)
//   - 409 acceptance_arbitration_requires_acknowledgement (the verdict carries
//     failed criteria and acknowledge_failed_criteria was not set)
//   - 409 acceptance_outcome_superseded (a newer acceptance verdict landed
//     while this arbitration was being evaluated — re-read and arbitrate the
//     current one)
//   - 503 acceptance_arbitration_unconfigured (run/audit repositories not wired)
func (c *apiClient) ArbitrateAcceptance(ctx context.Context, runID uuid.UUID, reason string, acknowledgeFailedCriteria bool) (*ArbitrateAcceptanceResult, error) {
	body, err := json.Marshal(acceptanceArbitrationRequest{
		Reason:                    reason,
		AcknowledgeFailedCriteria: acknowledgeFailedCriteria,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal acceptance-arbitration: %w", err)
	}
	var res ArbitrateAcceptanceResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/acceptance-arbitration", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// AutoDriveOutcome mirrors the backend's POST /v0/runs/{run_id}/auto-drive
// 200 body (#1700): the AutoDriveRunGate result the local drive verb switches
// on. A non-observe-only outcome is exactly one of three kinds — Acted (a
// delegated gate action fired), Paged (a must_page_human refusal), or
// DecisionRequired (a well-defined gate state the driver must STOP on and hand
// to the operator, e.g. an exhausted fix-up budget on the delegated
// route_fixup arm, #2091); observe-only is all three false. Repeated (not
// imported) per the thin local-copy rule — import direction is cli → backend,
// not the reverse. A mistyped json tag here would silently drop
// DecisionRequired to false and route the driver to the observe-only default,
// so client_test.go pins the wire decode.
type AutoDriveOutcome struct {
	Acted            bool   `json:"acted"`
	Action           string `json:"action,omitempty"`
	Paged            bool   `json:"paged"`
	PageEvent        string `json:"page_event,omitempty"`
	DecisionRequired bool   `json:"decision_required"`
	DecisionState    string `json:"decision_state,omitempty"`
	Note             string `json:"note"`
}

// AutoDriveRunGate calls POST /v0/runs/{run_id}/auto-drive (#1700): it drives
// the run's ONE parked gate under ADR-040 delegation, returning the outcome.
// The delegated action's own audit row is the authoritative record; the
// endpoint ALSO lands a supplementary run_auto_driven act:gate attribution
// row on an ACTED outcome. FAIL-LOUD: a supplementary-append failure surfaces
// as a 500 apiError (auto_drive_record_failed), and a genuine gate-dispatch
// failure as auto_drive_dispatch_failed — the drive loop stops acting on
// either rather than continuing on a silent success. 4xx/5xx surfaces:
//   - 401 authentication_required / 403 insufficient_scope (needs write:approvals)
//   - 404 run_not_found
//   - 500 auto_drive_dispatch_failed / auto_drive_record_failed
func (c *apiClient) AutoDriveRunGate(ctx context.Context, runID uuid.UUID) (*AutoDriveOutcome, error) {
	var res AutoDriveOutcome
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/auto-drive", []byte("{}"), &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// RecordAutoDriveAct is one record-before-dispatch call the drive verb makes
// before host-spawning a stage. Action is always "dispatch_stage"; Stage is
// one of plan|implement|acceptance|fixup_redispatch; Source is the driver
// tag ("fishhawk_drive_run").
type RecordAutoDriveAct struct {
	Action string `json:"action"`
	Stage  string `json:"stage"`
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

// RecordAutoDriveActResult mirrors the backend's POST
// /v0/runs/{run_id}/auto-drive/acts 200 body (#1700): the appended
// run_auto_driven act:dispatch attribution row's identifying fields.
type RecordAutoDriveActResult struct {
	RunID    string `json:"run_id"`
	Category string `json:"category"`
	Act      string `json:"act"`
	Action   string `json:"action"`
	Stage    string `json:"stage"`
	Source   string `json:"source"`
	Sequence int64  `json:"sequence"`
}

// RecordAutoDriveAct calls POST /v0/runs/{run_id}/auto-drive/acts (#1700): the
// server-owned write path the drive verb uses to record a stage dispatch
// BEFORE it host-spawns the runner. The audit chain stays server-owned; the
// MCP host never writes a chain entry itself. Validation fails CLOSED — an
// unknown run 404s and every missing/bad field 400s, appending nothing.
// FAIL-LOUD: a record-append failure surfaces as 500 auto_drive_record_failed
// so the caller does NOT dispatch. 4xx/5xx surfaces:
//   - 400 validation_failed (missing/bad action, stage, or source)
//   - 401 authentication_required / 403 insufficient_scope (needs write:approvals)
//   - 404 run_not_found
//   - 500 auto_drive_record_failed
func (c *apiClient) RecordAutoDriveAct(ctx context.Context, runID uuid.UUID, act RecordAutoDriveAct) (*RecordAutoDriveActResult, error) {
	body, err := json.Marshal(act)
	if err != nil {
		return nil, fmt.Errorf("marshal record auto-drive act: %w", err)
	}
	var res RecordAutoDriveActResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/auto-drive/acts", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ConsolidateResult mirrors the backend's consolidate 200 body (E24.2 /
// #1238): the outcome of running the decomposed-parent fan-in on demand.
// Outcome is "integrated" (every slice merged, parent implement succeeded,
// consolidated PR opened) or "slice_conflict" (a slice failed to merge, parent
// implement failed recoverable category-B). The conflict fields are set only
// on the slice_conflict outcome.
type ConsolidateResult struct {
	RunID                 string `json:"run_id"`
	Outcome               string `json:"outcome"`
	ResolvedToState       string `json:"resolved_to_state"`
	ConsolidatedBranch    string `json:"consolidated_branch,omitempty"`
	PullRequestURL        string `json:"pull_request_url,omitempty"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index,omitempty"`
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// ConsolidateRun runs the E24.2 fan-in for a decomposed parent on demand via
// `POST /v0/runs/{run_id}/consolidate` (#1238) — the operator path to
// complete a local decomposition where the 60s child-completion sweeper
// backstop is off. It returns the integrated/conflict outcome on 200, and
// SURFACES a fan-in failure the event-driven path would WARN-swallow. 4xx/5xx
// surfaces:
//   - 400 not_a_decomposed_parent (the run is a child, or has no children)
//   - 403 agent_token_forbidden (a run-bound agent token attempted it)
//   - 403 insufficient_scope (token lacks write:runs)
//   - 404 run_not_found
//   - 409 not_awaiting_children (already resolved, or not a decomposition)
//   - 409 children_in_flight (a child is still non-terminal)
//   - 409 children_failed (a child failed; resolve it before consolidating)
//   - 502 slice_integration_error (the fan-in failed; the error is surfaced)
func (c *apiClient) ConsolidateRun(ctx context.Context, id uuid.UUID) (*ConsolidateResult, error) {
	var res ConsolidateResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/consolidate", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// IntegrateWaveResult mirrors the backend's integrate-wave 200 body (#1278
// slice B): the outcome of the NON-settling per-wave fan-in the topological-
// wave run_children dispatch runs BETWEEN waves. Outcome is "integrated" (the
// succeeded slices so far merged onto the consolidated branch) or
// "slice_conflict" (a slice failed to merge). UNLIKE ConsolidateResult there
// is NO resolved_to_state — integrate-wave does not transition the parent
// stage. The conflict fields are set only on the slice_conflict outcome.
type IntegrateWaveResult struct {
	RunID                 string `json:"run_id"`
	Outcome               string `json:"outcome"`
	ConsolidatedBranch    string `json:"consolidated_branch,omitempty"`
	ConflictingSliceIndex *int   `json:"conflicting_slice_index,omitempty"`
	ConflictingChildRunID string `json:"conflicting_child_run_id,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// IntegrateWave runs the NON-settling per-wave fan-in for a decomposed parent
// via `POST /v0/runs/{run_id}/integrate-wave` (#1278 slice B) — the run_children
// wave loop calls it BETWEEN waves to merge the slices succeeded so far onto the
// consolidated branch so the next wave's dependent slices cut their branch from
// a tree carrying the predecessors' merged symbols. It does NOT require all
// children terminal, does NOT transition the parent stage, and does NOT
// advance/open the PR. 4xx/5xx surfaces:
//   - 400 not_a_decomposed_parent (the run is a child, or has no children)
//   - 403 agent_token_forbidden (a run-bound agent token attempted it)
//   - 403 insufficient_scope (token lacks write:runs)
//   - 404 run_not_found
//   - 502 slice_integration_error (the fan-in failed; the error is surfaced)
func (c *apiClient) IntegrateWave(ctx context.Context, id uuid.UUID) (*IntegrateWaveResult, error) {
	var res IntegrateWaveResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/integrate-wave", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ScopeCompletenessDecisionResult mirrors the backend's scope-completeness
// decision 200 body (`backend/internal/server/scope_completeness.go` — SLICE
// 1, #1231): the resolved park record. State is the implement stage's
// resulting state: `pending` on exempt (#2501 — the stage resumes in place and
// the orchestrator re-dispatches it, so the runner opens the held commit's PR
// with no agent re-run; on a local run it then parks at
// awaiting_host_dispatch for the operator's spawn), `failed` on a category-B
// fail. HeldCommitSHA is the exact gate-verified commit the runner pushed to
// the run branch at park time; PullRequestURL is set only once that
// re-dispatched runner has opened the PR from that held commit with NO agent
// re-invocation. MissingPaths echoes the declared scope paths the #1151
// shortfall gate flagged (empty on an assertion-class park, #2501, whose
// shortfall is read from the scope_completeness_* audit entry's
// unsatisfied_assertions). Repeated here rather than imported — the
// MCP server's apiClient is deliberately a thin local copy (import direction
// is `cli → backend`, not the reverse). MUST stay byte-identical with the
// backend handler's response shape.
type ScopeCompletenessDecisionResult struct {
	RunID          string   `json:"run_id"`
	StageID        string   `json:"stage_id"`
	Decision       string   `json:"decision"`
	State          string   `json:"state"`
	HeldCommitSHA  string   `json:"held_commit_sha"`
	RunBranch      string   `json:"run_branch,omitempty"`
	MissingPaths   []string `json:"missing_paths,omitempty"`
	PullRequestURL string   `json:"pull_request_url,omitempty"`

	// --- `amend` only (#2591) ---

	// AmendedPaths are the paths folded into the parked stage's effective
	// scope, each keeping its declared operation.
	AmendedPaths []ScopeAmendmentPathEntry `json:"amended_paths,omitempty"`
	// AmendmentID is the pre-approved #961 scope-amendment row the widening
	// landed on. Stable across a retry — the row is reused, never duplicated.
	AmendmentID string `json:"amendment_id,omitempty"`
	// OwnerAttributionUnresolved is true when the backend's owner-slice guard
	// could not establish ownership for at least one amended path. It can only
	// be true alongside AcknowledgedOwnerUnresolved: unacknowledged, that arm
	// is REFUSED 409 amend_owner_attribution_unresolved.
	OwnerAttributionUnresolved bool `json:"owner_attribution_unresolved,omitempty"`
	// OwnerUnresolvedPaths names each path whose ownership could not be
	// resolved, with the reason it could not, so the operator taking the risk
	// can see WHICH boundary is unprovable and WHY rather than only that one
	// was. Mirrors the handler's `owner_unresolved_paths`; without it the
	// per-path detail would be silently dropped by JSON decoding and reachable
	// only from the audit entry or the raw HTTP response.
	OwnerUnresolvedPaths []ScopeCompletenessUnresolvedOwner `json:"owner_unresolved_paths,omitempty"`
	// AcknowledgedOwnerUnresolved echoes the operator's explicit
	// acknowledge_owner_unresolved, so the relaxation reads as a deliberate,
	// audited decision rather than a silent default.
	AcknowledgedOwnerUnresolved bool `json:"acknowledged_owner_unresolved,omitempty"`
	// AgentAmendmentBudgetRemaining is how many of the stage's #961 mid-stage
	// amendment slots the RE-RUN agent still has; an operator amend consumes
	// one, so the cost is surfaced rather than hidden.
	AgentAmendmentBudgetRemaining int `json:"agent_amendment_budget_remaining,omitempty"`
}

// ScopeAmendmentPathEntry is one {path, operation} entry an `amend`
// scope-completeness decision folds into the parked stage's effective scope
// (#2591). It is an ALIAS of ScopeAmendmentPath rather than a second struct:
// the two are the same wire shape (the amend channel IS the #961 channel — the
// decision mints a pre-approved amendment row), so a duplicate declaration
// could only drift. The distinct name is what the amend surfaces refer to.
type ScopeAmendmentPathEntry = ScopeAmendmentPath

// ScopeCompletenessUnresolvedOwner is one submitted path whose OWNING slice the
// backend's amend guard could not establish, with the reason it could not
// (#2591). Mirrors the handler's `amendUnresolvedOwner` {path, reason} shape;
// declared here rather than imported for the same reason the result struct is —
// the MCP server's apiClient is a thin local copy of the wire shape.
type ScopeCompletenessUnresolvedOwner struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// scopeCompletenessDecisionRequest mirrors the backend's decision body
// (`backend/internal/server/scope_completeness.go::scopeCompletenessDecisionRequest`
// — SLICE 1, #1231). Both fields are required: the backend rejects a decision
// other than exempt/fail and an empty reason with 400.
type scopeCompletenessDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	// Paths carries the `amend` decision's widening (#2591). Required and
	// non-empty on amend; the backend 400s a non-amend decision that carries it.
	Paths []ScopeAmendmentPathEntry `json:"paths,omitempty"`
	// AcknowledgeOwnerUnresolved re-admits an amend whose owner-slice guard
	// could not resolve ownership for a submitted path. The guard fails CLOSED
	// without it (409 amend_owner_attribution_unresolved).
	AcknowledgeOwnerUnresolved bool `json:"acknowledge_owner_unresolved,omitempty"`
}

// DecideScopeCompleteness resolves an implement stage parked in
// awaiting_scope_decision via
// `POST /v0/runs/{run_id}/scope-completeness/decision` (#1231). decision is
// "exempt" (open the PR from the held commit with NO agent re-run), "amend"
// (widen the parked stage's effective scope with `paths` and RESUME it so the
// agent re-runs, #2591), or "fail" (fall through to category-B); reason is
// required. paths is required on amend and rejected otherwise;
// acknowledgeOwnerUnresolved re-admits an amend the owner-slice guard could not
// prove safe. Operator-token-only (write:stages); the backend rejects run-bound
// agent tokens (run_token_forbidden). 4xx surfaces:
//   - 400 validation_failed (decision not exempt/amend/fail, empty reason,
//     paths absent on amend or present on a non-amend, invalid path entry)
//   - 403 run_token_forbidden (a run-bound agent token attempted the decision)
//   - 403 insufficient_scope (token lacks write:stages)
//   - 404 run_not_found
//   - 409 scope_completeness_not_parked (the stage is not parked in
//     awaiting_scope_decision)
//   - 409 amend_incomplete_coverage (the amend omits a build_required_path)
//   - 409 amend_refused_owner_slice_active (a path's owning sibling slice has
//     already started its implement pass)
//   - 409 amend_owner_attribution_unresolved (ownership unprovable and not
//     acknowledged)
//   - 409 amend_budget_exhausted (the stage's amend-and-resume budget is spent)
//   - 500 amend_unrecorded / amend_resume_failed (the stage is left parked;
//     re-POST — the amendment row is reused, not duplicated)
func (c *apiClient) DecideScopeCompleteness(ctx context.Context, runID uuid.UUID, decision, reason string,
	paths []ScopeAmendmentPathEntry, acknowledgeOwnerUnresolved bool) (*ScopeCompletenessDecisionResult, error) {
	body, err := json.Marshal(scopeCompletenessDecisionRequest{
		Decision:                   decision,
		Reason:                     reason,
		Paths:                      paths,
		AcknowledgeOwnerUnresolved: acknowledgeOwnerUnresolved,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal scope-completeness decision: %w", err)
	}
	var res ScopeCompletenessDecisionResult
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/scope-completeness/decision", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// CancelRun transitions a run to the cancelled state via
// `POST /v0/runs/{run_id}/cancel`. Idempotent: cancelling an already-
// cancelled run returns 200 with the same body. 4xx surfaces:
//   - 404 run_not_found
//   - 409 invalid_state_transition (the run is already terminal in a
//     non-cancelled state, e.g. succeeded / failed)
func (c *apiClient) CancelRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	var r Run
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+id.String()+"/cancel", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// StartRun creates a new run. Returns the created (or replayed) run
// plus an `idempotent` flag indicating whether the backend served
// 200 (replay against an existing run) versus 201 (fresh). 4xx
// surfaces as *apiError; the MCP tool layer reads the code field to
// translate validation errors into clean tool errors.
func (c *apiClient) StartRun(ctx context.Context, p StartRunParams) (*Run, bool, error) {
	req := createRunRequest{
		Repo:                    p.Repo,
		WorkflowID:              p.WorkflowID,
		WorkflowSHA:             p.WorkflowSHA,
		TriggerSource:           p.TriggerSource,
		RunnerKind:              p.RunnerKind,
		WorkingDir:              p.WorkingDir,
		WorkflowSpec:            p.WorkflowSpec,
		IssueContext:            p.IssueContext,
		BudgetOverride:          p.BudgetOverride,
		UpstreamRunID:           p.UpstreamRunID,
		AppliesToOverride:       p.AppliesToOverride,
		AppliesToOverrideReason: p.AppliesToOverrideReason,
	}
	if p.TriggerRef != "" {
		ref := p.TriggerRef
		req.TriggerRef = &ref
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("marshal start_run: %w", err)
	}
	headers := map[string]string{}
	if p.IdempotencyKey != "" {
		headers["Idempotency-Key"] = p.IdempotencyKey
	}
	var run Run
	status, err := c.doWithStatus(ctx, http.MethodPost, "/v0/runs", body, headers, &run)
	if err != nil {
		return nil, false, err
	}
	// 200 = idempotent replay; 201 = newly created. Both are success.
	return &run, status == http.StatusOK, nil
}

// RecoverScopePath is one operator-named path on a recovery request
// (#978). Operation is "modify" or "create"; the backend defaults an
// empty value to modify.
type RecoverScopePath struct {
	Path      string `json:"path"`
	Operation string `json:"operation,omitempty"`
}

// RecoverExemptPath is one operator-justified-unchanged path on a recovery
// request (#1229): a DECLARED scope.files path the runner's #1151 shortfall
// gate subtracts. The inverse of RecoverScopePath — it carries a required
// {path, reason} and subtracts from the gate rather than widening scope.
// Exported (like RecoverScopePath) so the MCP tool's input schema can reuse
// the same shape the wire body carries.
type RecoverExemptPath struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// recoverRunRequest mirrors `server/recover.go::recoverRunRequest`.
type recoverRunRequest struct {
	AddScopeFiles    []RecoverScopePath  `json:"add_scope_files,omitempty"`
	ExemptScopeFiles []RecoverExemptPath `json:"exempt_scope_files,omitempty"`
	Reason           string              `json:"reason,omitempty"`
	BudgetOverride   bool                `json:"budget_override,omitempty"`
}

// RecoverRunParams bundles the inputs to RecoverRun. IdempotencyKey
// travels in the HTTP header per the backend's E8.2 contract, same
// keyspace as StartRun.
type RecoverRunParams struct {
	ParentRunID      uuid.UUID
	AddScopeFiles    []RecoverScopePath
	ExemptScopeFiles []RecoverExemptPath
	Reason           string
	BudgetOverride   bool
	IdempotencyKey   string
}

// RecoverRun mints a category-B recovery run via
// `POST /v0/runs/{run_id}/recover` (#978). Returns the created (or
// replayed) child run plus an `idempotent` flag mirroring StartRun.
// 4xx surfaces as *apiError; the tool layer maps the codes:
//   - 404 run_not_found
//   - 409 recovery_not_eligible (plan not succeeded / implement not
//     failed category-B)
//   - 422 recovery_unsupported (no cached workflow spec)
func (c *apiClient) RecoverRun(ctx context.Context, p RecoverRunParams) (*Run, bool, error) {
	body, err := json.Marshal(recoverRunRequest{
		AddScopeFiles:    p.AddScopeFiles,
		ExemptScopeFiles: p.ExemptScopeFiles,
		Reason:           p.Reason,
		BudgetOverride:   p.BudgetOverride,
	})
	if err != nil {
		return nil, false, fmt.Errorf("marshal recover_run: %w", err)
	}
	headers := map[string]string{}
	if p.IdempotencyKey != "" {
		headers["Idempotency-Key"] = p.IdempotencyKey
	}
	var run Run
	status, err := c.doWithStatus(ctx, http.MethodPost,
		"/v0/runs/"+p.ParentRunID.String()+"/recover", body, headers, &run)
	if err != nil {
		return nil, false, err
	}
	// 200 = idempotent replay; 201 = newly created. Both are success.
	return &run, status == http.StatusOK, nil
}

// Campaign mirrors the backend's `Campaign` wire schema
// (`backend/internal/server/campaigns.go::campaignResponse`): the campaign
// row POST /v0/campaigns and GET /v0/campaigns/{id}/status return. As with
// the Run struct above, IDs are typed `string` (not uuid.UUID) so the MCP
// SDK's reflection-built schema sees a string rather than a 16-byte array
// (which would surface as type:array and fail the SDK's response validation);
// tool handlers parse with uuid.Parse locally.
type Campaign struct {
	ID      string `json:"id"`
	Repo    string `json:"repo"`
	EpicRef string `json:"epic_ref"`
	State   string `json:"state"`
	// PausePolicy is the operator-chosen pause behavior on a gate hand-off
	// (E25.7): pause_campaign (block the whole campaign, the default) or
	// pause_item (continue-others). Always normalized on a persisted campaign.
	PausePolicy string `json:"pause_policy"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent delegation
	// override (E25.12 / #1451). When present it is the effective delegation
	// contract for EVERY issue-run the campaign drives — it wins WHOLESALE over
	// the per-run workflow operator_agent (campaign > gate > workflow, never
	// merged). Typed map[string]any (not json.RawMessage) so the MCP SDK's
	// reflection-built output schema sees an unconstrained object rather than a
	// []byte array (which would surface as type:array and fail response
	// validation) — the same reason the CalibrationResult.ConfidenceBandAccuracy
	// field documents. Omitted on a campaign with no override (the byte-identical
	// default — each issue-run inherits its workflow contract).
	OperatorAgent map[string]any `json:"operator_agent,omitempty"`
	// WorkingDir is the campaign-level checkout binding (E48.87 / #2527): the
	// absolute path every item run minted from this campaign inherits when
	// fishhawk_start_campaign_item_run passes no per-item working_dir. Omitted
	// on an unbound campaign (the unchanged default — each item run then needs
	// its own working_dir, and a local one without either is refused). Mirrors
	// Run.WorkingDir.
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"the absolute checkout path bound to this campaign; every item run minted from it inherits this unless the per-item call overrides it. Absent when the campaign carries no binding"`
	// GroomingSource is the DURABLE provenance of a campaign created from an
	// approved grooming run's ratified order (E54.6 / #2238), read back from the
	// campaigns.grooming_source column — so it is present on every read of the
	// campaign, not just the create response. Typed map[string]any for the same
	// reason OperatorAgent is: the MCP SDK builds the output schema by
	// reflection, and json.RawMessage would surface as type:array. Omitted on
	// every epic_ref / explicit-items campaign.
	GroomingSource map[string]any `json:"grooming_source,omitempty" jsonschema:"provenance of a campaign built from an approved grooming order: source_run_id, source_stage_id, report_artifact_id, report_content_hash, ordered_refs, excluded (with reasons), limit, omitted_by_limit, superseded_by. Absent unless the campaign was created from a grooming order"`
	// SatisfiedDependencies are the depends_on edges elided at assembly because
	// their out-of-set target was already closed-and-completed (#2953): a
	// prerequisite that already landed, so the campaign assembled instead of
	// failing with an unactionable dangling-dependency error. Typed struct (NOT
	// map[string]any / json.RawMessage) so the MCP SDK's reflection-built output
	// schema stays an object array. CREATE-response-only — the backend does not
	// persist it, so a later read of the campaign omits it.
	SatisfiedDependencies []campaignSatisfiedDependency `json:"satisfied_dependencies,omitempty" jsonschema:"depends_on edges elided at assembly because the target was already closed-and-completed (its work landed, so the dependency is satisfied). Present only on the create response, only when at least one edge was elided"`
	CreatedAt             time.Time                     `json:"created_at"`
	UpdatedAt             time.Time                     `json:"updated_at"`
}

// campaignSatisfiedDependency mirrors the backend's satisfiedDependencyPayload
// (#2953): one elided depends_on edge — the depending issue (from) → the
// already-completed target (to) — plus the target's observed state, so an
// operator can tell "this dependency is done" from "this dependency was ignored".
// Unexported (its FIELDS are exported for JSON) so it stays off the frozen MCP
// export surface while still decoding and reflecting into the tool output schema.
type campaignSatisfiedDependency struct {
	From        int    `json:"from" jsonschema:"the depending issue number"`
	To          int    `json:"to" jsonschema:"the already-completed target issue number the edge was elided against"`
	State       string `json:"state" jsonschema:"the target's observed issue state (e.g. closed)"`
	StateReason string `json:"state_reason" jsonschema:"the target's observed state_reason (e.g. completed)"`
}

// CampaignPauseReason mirrors the backend's campaign.PauseReason: why a paused
// item was handed off to a human (the page event + run/stage/gate). The
// run_id/stage_id are typed `string` here (the backend carries *uuid.UUID, which
// JSON-marshals to a string) for the same MCP-SDK reflection reason Campaign
// documents.
type CampaignPauseReason struct {
	PageEvent string `json:"page_event,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	StageID   string `json:"stage_id,omitempty"`
	Gate      string `json:"gate,omitempty"`
}

// CampaignItem mirrors the backend's `CampaignItem` wire schema
// (`campaignItemResponse`): one node in the campaign DAG. RunID is omitempty —
// an unlinked (pre-dispatch) item carries no run_id.
type CampaignItem struct {
	ID          string               `json:"id"`
	IssueRef    string               `json:"issue_ref"`
	DependsOn   []string             `json:"depends_on"`
	RunID       string               `json:"run_id,omitempty"`
	State       string               `json:"state"`
	PauseReason *CampaignPauseReason `json:"pause_reason,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
}

// CampaignRollup mirrors the backend's `CampaignRollup` wire schema
// (`campaignRollupPayload`): the engine's readiness partition over a campaign's
// items. Every slice holds issue refs and an item appears in exactly one slice.
type CampaignRollup struct {
	Eligible []string `json:"eligible"`
	// HumanLed holds deps-satisfied autonomy:low items diverted out of Eligible
	// (human-led work the auto-driver must never dispatch).
	HumanLed  []string `json:"human_led"`
	Blocked   []string `json:"blocked"`
	Running   []string `json:"running"`
	Done      []string `json:"done"`
	Failed    []string `json:"failed"`
	Cancelled []string `json:"cancelled"`
	Paused    []string `json:"paused"`
}

// CampaignNextAction mirrors the backend's `campaignNextActionPayload`: the
// single server-computed next step for the operator-agent, distilled from the
// rollup partition. Action is drawn from the closed set
// attention|resume|start_run|attend_human_led|wait|complete|closed
// (computeCampaignNextAction). `closed` (#2681) means the CAMPAIGN itself went
// terminal with an issue still unfinished: no campaign verb applies, and
// IssueRef carries the stranded ref when one exists.
type CampaignNextAction struct {
	Action   string `json:"action"`
	IssueRef string `json:"issue_ref,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// CampaignStatus mirrors the GET /v0/campaigns/{id}/status response body: the
// campaign + its items + the engine's readiness rollup + the distilled
// next_action. This is the surface the operator-agent polls to drive a campaign.
type CampaignStatus struct {
	Campaign   Campaign           `json:"campaign"`
	Items      []CampaignItem     `json:"items"`
	Rollup     CampaignRollup     `json:"rollup"`
	NextAction CampaignNextAction `json:"next_action"`
}

// campaignCreateRequest mirrors the backend's POST /v0/campaigns body
// (`backend/internal/server/campaigns.go::createCampaignRequest`). Repeated
// here for the same thin-local-copy reason as createRunRequest.
type campaignCreateRequest struct {
	Repo string `json:"repo"`
	// EpicRef has NO omitempty deliberately (#2051): the no-epic variant sends an
	// empty epic_ref, and the server treats an empty-string epic_ref as ABSENT
	// (routing to the items-only branch). omitempty would DROP the key when the
	// caller omits epic_ref — indistinguishable on the wire from sending "", so
	// keeping it non-omitempty is load-bearing but harmless (the server trims and
	// branches identically whether the key is "" or absent).
	EpicRef     string `json:"epic_ref"`
	PausePolicy string `json:"pause_policy,omitempty"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent override
	// (E25.12 / #1451), carried as opaque JSON the backend validates against
	// spec.OperatorAgent (unknown fields rejected -> 400 validation_failed).
	// json.RawMessage (not map[string]any) here because this is an HTTP request
	// body, not an MCP tool schema — omitempty drops a nil/empty value so a
	// campaign without an override sends no operator_agent key.
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
	// Items is the OPTIONAL subset filter (#2003): issue refs naming the subset
	// of the epic's children to scope the campaign to. Empty/nil omits the field
	// so the backend sweeps every child (the backward-compatible default).
	Items []string `json:"items,omitempty"`
	// WorkingDir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527) every item run minted from the campaign inherits. omitempty drops
	// an empty value so a campaign without a binding sends no working_dir key.
	// The backend validates it is absolute (400 validation_failed otherwise).
	WorkingDir string `json:"working_dir,omitempty"`
	// GroomingSource is the OPTIONAL THIRD campaign source (E54.6 / #2238): an
	// approved grooming run whose ratified priority order becomes the campaign
	// queue. A nil pointer drops the key entirely, so a campaign created from an
	// epic or an explicit item list sends a byte-identical body to pre-#2238.
	// The backend refuses it in combination with epic_ref or items.
	GroomingSource *campaignGroomingSource `json:"grooming_source,omitempty"`
}

// campaignGroomingSource is the grooming_source request block. It is a distinct
// TYPE rather than three loose parameters so it cannot be transposed with the
// existing string arguments at the call site.
type campaignGroomingSource struct {
	RunID string `json:"run_id"`
	// Limit caps the batch to the top N convertible entries by rank; omitted
	// (0) means no cap.
	Limit int `json:"limit,omitempty"`
	// AllowSuperseded explicitly acknowledges building from an order a NAMED
	// newer approved grooming run has superseded — that case and no other. A
	// scan that could not prove currency, or could not be run, is refused
	// whatever this carries (K2).
	AllowSuperseded bool `json:"allow_superseded,omitempty"`
}

// CreateCampaign assembles a campaign via `POST /v0/campaigns` (E25.4) and
// returns the created campaign (201 fresh). epicRef is OPTIONAL as of #2051: pass
// it to decompose an epic's children (optionally narrowed by items), OR leave it
// empty and pass items alone to assemble a NO-EPIC campaign over exactly that
// issue list (the server resolves each issue's depends_on directly). epicRef is
// serialized WITHOUT omitempty so an empty ref reaches the server, which treats
// an empty-string epic_ref as absent and routes to the no-epic branch. An empty
// request (neither epic_ref nor items) fails 400 validation_failed. pausePolicy
// is optional — empty normalizes to pause_campaign server-side. operatorAgent is
// the OPTIONAL campaign-level operator_agent override (E25.12 / #1451) carried as
// opaque JSON; empty/nil omits the field so the campaign inherits each issue-run's
// workflow contract. A write tool: requires an operator token with
// write:campaigns scope. 4xx/5xx surfaces as *apiError; the tool layer reads the
// code:
//   - 400 validation_failed (repo not owner/name, neither epic_ref nor items,
//     bad pause_policy, a malformed/unknown-field operator_agent, a non-absolute
//     working_dir, or a dependency cycle)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 422 repo_not_installed (the GitHub App is not on the target repo)
//   - 422 campaign_dangling_dependency (a depends_on target is outside the assembled set)
//   - 422 campaign_item_not_child (a requested items ref is not a child of the epic)
//   - 501 issue_set_resolution_unsupported (no-epic variant on a provider that
//     cannot resolve an arbitrary issue set)
//   - 503 campaign_repo_unconfigured (no campaign repository wired on the deploy)
//
// items is the OPTIONAL subset filter (#2003) WITH epicRef (issue refs naming the
// subset of the epic's children; empty/nil sweeps every child) or the
// AUTHORITATIVE issue set WITHOUT epicRef (the no-epic variant, #2051).
//
// workingDir is the OPTIONAL campaign-level checkout binding (E48.87 / #2527)
// every item run minted from the campaign inherits; empty omits the field (no
// binding). It is the LAST parameter, appended after items []string, so it
// cannot be transposed with another string argument at a call site.
func (c *apiClient) CreateCampaign(ctx context.Context, repo, epicRef, pausePolicy string, operatorAgent json.RawMessage, items []string, workingDir string, groomingSource *campaignGroomingSource) (*Campaign, error) {
	body, err := json.Marshal(campaignCreateRequest{Repo: repo, EpicRef: epicRef, PausePolicy: pausePolicy, OperatorAgent: operatorAgent, Items: items, WorkingDir: workingDir, GroomingSource: groomingSource})
	if err != nil {
		return nil, fmt.Errorf("marshal create campaign: %w", err)
	}
	var camp Campaign
	if _, err := c.doWithStatus(ctx, http.MethodPost, "/v0/campaigns", body, nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// GetCampaignStatus reads the campaign rollup + distilled next_action via
// `GET /v0/campaigns/{id}/status` (E25.4) — the surface the operator-agent
// polls to drive a campaign. Read-only. 4xx/5xx surfaces:
//   - 400 validation_failed (campaign_id not a UUID)
//   - 404 campaign_not_found
//   - 503 campaign_repo_unconfigured
func (c *apiClient) GetCampaignStatus(ctx context.Context, id uuid.UUID) (*CampaignStatus, error) {
	var st CampaignStatus
	if err := c.do(ctx, http.MethodGet, "/v0/campaigns/"+id.String()+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ResumeCampaign hands a paused campaign back to the auto-driver via
// `POST /v0/campaigns/{id}/resume` (E25.7) — the operator's hand-back after the
// driver paged a human at a run gate. It flips a paused campaign (and every
// paused item) back to running. A write tool: requires write:campaigns. 4xx/5xx
// surfaces:
//   - 400 validation_failed (campaign_id not a UUID)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 404 campaign_not_found
//   - 409 campaign_not_paused (nothing is paused on either axis — no item and
//     not the campaign — so there is nothing to resume)
//   - 503 campaign_repo_unconfigured
func (c *apiClient) ResumeCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	var camp Campaign
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+id.String()+"/resume", nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// CancelCampaign marks a campaign and its unfinished items cancelled via
// `POST /v0/campaigns/{id}/cancel` (#2355) — the operator's clean shutdown of an
// abandoned or rebuilt campaign so it stops showing as live `running` work in
// GET /v0/campaigns. It cancels every NON-terminal item and then the campaign
// itself; it deliberately does NOT cancel the linked RUNS (fishhawk_cancel_run
// owns that). A write tool: requires write:campaigns. 4xx/5xx surfaces:
//   - 400 validation_failed (campaign_id not a UUID)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 404 campaign_not_found
//   - 409 campaign_not_cancellable (the campaign is already terminal —
//     succeeded/failed/cancelled — so there is nothing to cancel)
//   - 503 campaign_repo_unconfigured
func (c *apiClient) CancelCampaign(ctx context.Context, id uuid.UUID) (*Campaign, error) {
	var camp Campaign
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+id.String()+"/cancel", nil, &camp); err != nil {
		return nil, err
	}
	return &camp, nil
}

// startCampaignItemRunRequest mirrors the backend's POST
// /v0/campaigns/{campaign_id}/runs body
// (`backend/internal/server/campaigns.go::startCampaignItemRunRequest`).
// Repeated here for the same thin-local-copy reason as createRunRequest. There
// is deliberately NO idempotency_key field — the backend does not dedup this
// create-link-transition sequence, so the request shape advertises none (#1443
// honesty).
type startCampaignItemRunRequest struct {
	IssueRef    string `json:"issue_ref"`
	WorkflowID  string `json:"workflow_id"`
	WorkflowRef string `json:"workflow_ref,omitempty"`
	RunnerKind  string `json:"runner_kind,omitempty"`
	// WorkingDir binds the minted run's local checkout (E48.69 / #2498) so the
	// later runner-spawning verbs inherit it. The MCP tool layer validates it
	// (required for a local item, absolute for any kind) before this round-trip;
	// the backend independently 400s a non-absolute value.
	WorkingDir string `json:"working_dir,omitempty"`
}

// StartCampaignItemRunResult mirrors the backend's POST
// /v0/campaigns/{campaign_id}/runs 201 body: the minted run plus the linked
// campaign item (now running, with run_id set).
type StartCampaignItemRunResult struct {
	Run  Run          `json:"run"`
	Item CampaignItem `json:"item"`
}

// StartCampaignItemRun starts a run for an eligible campaign item via
// `POST /v0/campaigns/{campaign_id}/runs` (E26.2 / #1481) — the operator-driven,
// campaign-aware run start that DAG-gates each run and links it to the campaign
// so the rollup advances as the operator drives the loop. workflowRef empty =
// the repo's default branch; runnerKind empty = github_actions ("local" for the
// local dogfood loop). A write tool: requires write:campaigns. 4xx/5xx surfaces:
//   - 400 validation_failed (campaign_id not a UUID, empty issue_ref/workflow_id,
//     bad runner_kind, unknown fields)
//   - 403 insufficient_scope (token lacks write:campaigns)
//   - 404 campaign_not_found / campaign_item_not_found
//   - 409 campaign_not_startable (the campaign is paused, cancelled or
//     succeeded — a terminal-FAILED campaign is REOPENED by this verb, #2681)
//   - 409 item_not_eligible (the item is blocked on a dependency, already
//     running, or terminal — the detail names the blocker)
//   - 400 validation_failed (a non-absolute working_dir; E48.69 / #2498 — the
//     backend refuses a relative binding the same way POST /v0/runs does — or a
//     non-absolute binding INHERITED from the campaign row, E48.87 / #2527)
//   - 400 working_dir_required (a local item whose campaign carries no
//     working_dir binding and whose call passed none; E48.87 / #2527 — bind it
//     once at start_campaign, or pass it for this item)
//   - 502 campaign_run_start_failed (the installation/spec could not be resolved)
//   - 503 campaign_repo_unconfigured
func (c *apiClient) StartCampaignItemRun(ctx context.Context, campaignID uuid.UUID, issueRef, workflowID, workflowRef, runnerKind, workingDir string) (*StartCampaignItemRunResult, error) {
	body, err := json.Marshal(startCampaignItemRunRequest{
		IssueRef:    issueRef,
		WorkflowID:  workflowID,
		WorkflowRef: workflowRef,
		RunnerKind:  runnerKind,
		WorkingDir:  workingDir,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal start campaign item run: %w", err)
	}
	var res StartCampaignItemRunResult
	if err := c.do(ctx, http.MethodPost, "/v0/campaigns/"+campaignID.String()+"/runs", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileWorkItemRequest mirrors the backend's POST /v0/work-items body
// (`backend/internal/server/workitems.go::workItemRequest`). The
// conventions layer turns this provider-neutral filing into a created
// item; only Repo, Type, and Summary are required. Repeated here rather
// than imported because the MCP server's apiClient is deliberately a thin
// local copy — the import-direction rule is `cli → backend`, not the
// reverse.
type FileWorkItemRequest struct {
	Repo            string             `json:"repo"`
	Type            string             `json:"type"`
	Summary         string             `json:"summary"`
	Body            string             `json:"body,omitempty"`
	Sections        map[string]string  `json:"sections,omitempty"`
	TitleVars       map[string]string  `json:"title_vars,omitempty"`
	Labels          []string           `json:"labels,omitempty"`
	Complexity      string             `json:"complexity,omitempty"`
	Status          string             `json:"status,omitempty"`
	Relations       *WorkItemRelations `json:"relations,omitempty"`
	ExistingNumbers []int              `json:"existing_numbers,omitempty"`
	RunID           string             `json:"run_id,omitempty"`
}

// WorkItemRelations mirrors the wire `relations` sub-object: the
// provider-neutral links the conventions layer resolves into provider
// link operations.
type WorkItemRelations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
}

// FiledWorkItem mirrors the backend's WorkItemResponse: the created item,
// echoing the conventions-resolved placement so the caller renders the
// result without a second fetch. Audited is true only when a
// work_item_filed audit entry was written (a run was in flight).
type FiledWorkItem struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	Complexity    string   `json:"complexity,omitempty"`
	Status        string   `json:"status,omitempty"`
	BoardColumn   string   `json:"board_column,omitempty"`
	// Boarded / EpicLinked report whether the best-effort post-create
	// enrichment landed (#1107). Board placement and epic linking are no
	// longer fatal: the issue is the durable result, so a placement/link
	// failure files the issue (boarded/epic_linked false) and carries the
	// cause in BoardingError / EpicLinkError rather than a 502.
	Boarded       bool   `json:"boarded"`
	EpicLinked    bool   `json:"epic_linked"`
	BoardingError string `json:"boarding_error,omitempty"`
	EpicLinkError string `json:"epic_link_error,omitempty"`
	Audited       bool   `json:"audited"`
	// DefaultedLabels / MissingLabelNamespaces surface the backend's LOUD
	// label-completeness report (#1616): every label the system added that the
	// caller did not supply (namespace defaults + handler-derived area), and
	// any required namespace still absent after merge/derivation/defaulting. A
	// missing namespace is reported, never a rejection.
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
	// Intake carries the ADVISORY intake-groom signals the backend derived at
	// filing time (#2239). Absent when the deployment produced none.
	Intake *IntakeSignals `json:"intake,omitempty"`
}

// IntakeSignals and its satellites are LOCAL, DECODE-ONLY mirrors of the
// backend's intake payload. They are deliberately NOT the backend's
// intakegroom (or workmgmt) types, and that is a structural requirement rather
// than a style choice: ADR-064 / #2230 forbids any board-READ path from
// reaching the MCP agent tool surface, and
// backend/internal/mcpserver/board_read_guard_test.go enforces it by banning
// both a transitive workmgmt import and a set of board-read symbol names in
// this package's non-test code. The reads that produce these signals happen
// SERVER-side, where the capability already lives; this package only decodes
// the JSON the 201 already carried. Mirroring the shape locally keeps that
// guard green while still surfacing the result to the filing agent.
//
// Everything here is ADVISORY. Nothing was closed, merged, relabelled or
// transitioned on the strength of it, and Degraded is a normal outcome.
type IntakeSignals struct {
	Duplicates      []IntakeDuplicate     `json:"duplicates,omitempty"`
	EpicSuggestion  *IntakeEpicSuggestion `json:"epic_suggestion,omitempty"`
	Score           IntakeScore           `json:"score"`
	Degraded        bool                  `json:"degraded"`
	DegradeReason   string                `json:"degrade_reason,omitempty"`
	ScannedItems    int                   `json:"scanned_items"`
	WindowTruncated bool                  `json:"window_truncated"`
	DurationMS      int64                 `json:"duration_ms"`
}

// IntakeDuplicate is one possible duplicate of the filed item — a CANDIDATE
// for a human, never a decision. Closed candidates are surfaced too: a closed
// duplicate is often the resolution.
type IntakeDuplicate struct {
	Number     int     `json:"number"`
	URL        string  `json:"url,omitempty"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Confidence string  `json:"confidence"`
	Basis      string  `json:"basis"`
	Closed     bool    `json:"closed"`
}

// IntakeEpicSuggestion is the suggested parent epic for a filing that declared
// none. It is a suggestion; no relation was created.
type IntakeEpicSuggestion struct {
	Number     int     `json:"number"`
	URL        string  `json:"url,omitempty"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
	Confidence string  `json:"confidence"`
	Basis      string  `json:"basis"`
}

// IntakeScore is the provisional charter-anchored structural score. Value is
// advisory only — the citations are what a reviewer reads. Unscored with a
// CharterGap means no rubric line could be cited from the filing's structure,
// which is reported as a finding rather than papered over with an invented
// citation.
type IntakeScore struct {
	Value      float64          `json:"value"`
	Citations  []IntakeCitation `json:"citations,omitempty"`
	Unscored   bool             `json:"unscored"`
	CharterGap string           `json:"charter_gap,omitempty"`
}

// IntakeCitation is one charter rubric line the score cites.
type IntakeCitation struct {
	RubricID string `json:"rubric_id"`
	Quote    string `json:"quote"`
	Note     string `json:"note"`
}

// FileWorkItem files a provider-agnostic work item via
// `POST /v0/work-items` (#1005). The backend loads the repo's
// work-management conventions, applies them, dispatches to the registered
// provider, and (when run_id names an in-flight run) writes a best-effort
// work_item_filed audit entry. 4xx/5xx surface as *apiError; the tool
// layer reads the code:
//   - 400 validation_failed (repo not owner/name, missing type/summary,
//     unknown fields)
//   - 401 authentication_required (anonymous caller)
//   - 422 work_item_invalid (the request violates the type's conventions)
//   - 501 provider_unimplemented (the configured provider id — e.g. the
//     interface-only jira — is not registered; details name it)
//   - 502 work_item_filing_failed (the provider rejected the filing)
func (c *apiClient) FileWorkItem(ctx context.Context, req FileWorkItemRequest) (*FiledWorkItem, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal file-work-item: %w", err)
	}
	var out FiledWorkItem
	if err := c.do(ctx, http.MethodPost, "/v0/work-items", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefinementDecision mirrors the backend's RefinementDecision schema: one
// append-only approve/reject verdict pinning a draft revision + its content
// hash. DraftID is a `string` (not uuid.UUID) so the MCP SDK's reflection-built
// output schema sees a string, not a 16-byte array (the #371 trap).
type RefinementDecision struct {
	Decision         string    `json:"decision" jsonschema:"approved or rejected"`
	Reason           string    `json:"reason"`
	DraftID          string    `json:"draft_id" jsonschema:"the decided revision's id"`
	DraftContentHash string    `json:"draft_content_hash" jsonschema:"sha256 of the decoded EpicDraft the decision pinned"`
	DecidedBy        string    `json:"decided_by,omitempty" jsonschema:"the deciding identity's subject; absent when unknown"`
	CreatedAt        time.Time `json:"created_at"`
}

// RefinementAcceptanceFinding mirrors one deterministic acceptance-criteria
// defect the intake pre-check flagged (backend plan.AcceptanceFinding): the
// machine-readable rule name, the offending criterion id (the criterion text at
// intake), and a human-readable detail.
type RefinementAcceptanceFinding struct {
	Rule        string `json:"rule" jsonschema:"the machine-readable rule: no_blocking_criterion, missing_source_ref, missing_rationale, empty_id, or duplicate_id"`
	CriterionID string `json:"criterion_id,omitempty" jsonschema:"the offending criterion (its text at intake); absent for the presence-level no_blocking_criterion finding"`
	Detail      string `json:"detail" jsonschema:"a short human-readable explanation of the defect"`
}

// ChildCriteriaCheck mirrors the backend's per-child intake acceptance-criteria
// pre-check: the 1-based child ordinal, its needs_attention marker, and its
// findings ([] when checked-and-clean).
type ChildCriteriaCheck struct {
	Ordinal        int                           `json:"ordinal" jsonschema:"the 1-based child ordinal this check is for"`
	NeedsAttention bool                          `json:"needs_attention,omitempty" jsonschema:"true when this child has an unjustified no_blocking_criterion finding (advisory — approval remains legal)"`
	Findings       []RefinementAcceptanceFinding `json:"findings" jsonschema:"the child's acceptance-criteria findings; [] when checked and clean"`
}

// CriteriaPrecheck mirrors the backend's E34.5 advisory acceptance-criteria
// pre-check over the latest draft (#1596): per-child findings plus a
// draft-level needs_attention marker. Advisory only — a flagged draft can still
// be approved; the guidance names the flagged child ordinals so the operator
// sees the defect before deciding.
type CriteriaPrecheck struct {
	NeedsAttention bool                 `json:"needs_attention" jsonschema:"true when any child has an unjustified no_blocking_criterion finding (advisory — approval remains legal)"`
	Children       []ChildCriteriaCheck `json:"children" jsonschema:"the per-child acceptance-criteria checks (one per draft child)"`
}

// RefinementSession mirrors the backend's RefinementSession schema
// (docs/api/v0.openapi.yaml): the refinement gate's session view — the DERIVED
// approval state, the revision count, the latest EpicDraft, the full filing
// preview, the wave DAG, the advisory acceptance-criteria pre-check, and the
// decision history. State is derived, never stored: a decision counts only when
// it targets the latest revision and its pinned hash still matches, so an edit
// after approval re-gates the session.
//
// SessionID is a `string` (not uuid.UUID) so the MCP SDK's reflection-built
// output schema sees a string (the #371 trap). Preview is []map[string]any —
// each item is an opaque work-item render (the backend serializes
// []workmgmt.WorkItem), typed as map[string]any (not json.RawMessage) so the
// SDK's schema reflection sees an object, not a base64 string.
type RefinementSession struct {
	SessionID        string               `json:"session_id"`
	State            string               `json:"state" jsonschema:"awaiting_approval, approved, or rejected (derived)"`
	Drifted          bool                 `json:"drifted,omitempty" jsonschema:"true when the latest revision's decision pins a content hash that no longer matches (fail-closed to awaiting_approval)"`
	RevisionCount    int                  `json:"revision_count" jsonschema:"number of draft revisions in the session"`
	LatestOrigin     string               `json:"latest_origin" jsonschema:"how the latest revision came to exist: brief, amendment, or edit"`
	LatestDraft      EpicDraft            `json:"latest_draft" jsonschema:"the latest structured epic/children draft"`
	Preview          []map[string]any     `json:"preview" jsonschema:"the full filing preview — the epic then each child, rendered exactly as it would file"`
	Waves            [][]int              `json:"waves" jsonschema:"the topological dispatch order as waves of 1-based child ordinals"`
	CriteriaPrecheck CriteriaPrecheck     `json:"criteria_precheck" jsonschema:"the advisory acceptance-criteria pre-check over the latest draft's children; needs_attention flags an unjustified missing blocking criterion (approval remains legal)"`
	Decisions        []RefinementDecision `json:"decisions" jsonschema:"the append-only decision history"`
}

// RefinementFilingEpic mirrors the file response's `epic` sub-object.
type RefinementFilingEpic struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// RefinementFilingChild mirrors one filed child in the file response.
type RefinementFilingChild struct {
	Ordinal int    `json:"ordinal" jsonschema:"1-based draft child ordinal"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
}

// RefinementFilingResult mirrors the backend's POST .../file 200 body: the
// outcome of filing an approved draft into tracker items (fresh, resumed, or an
// already-completed replay). SessionID / DraftID are strings (the #371 trap).
type RefinementFilingResult struct {
	SessionID        string                  `json:"session_id"`
	DraftID          string                  `json:"draft_id"`
	Repo             string                  `json:"repo"`
	Epic             RefinementFilingEpic    `json:"epic"`
	Children         []RefinementFilingChild `json:"children"`
	Resumed          bool                    `json:"resumed" jsonschema:"true when this invocation resumed a partially-filed session"`
	AlreadyCompleted bool                    `json:"already_completed" jsonschema:"true when replaying a fully-completed session (no writes performed)"`
	Verified         bool                    `json:"verified" jsonschema:"true when the filed epic passed the epic-children + campaign-assembly round-trip"`
}

// createRefinementSessionRequest mirrors the backend's POST
// /v0/refinement/sessions body.
type createRefinementSessionRequest struct {
	Brief string `json:"brief"`
}

// editRefinementDraftRequest mirrors the backend's PATCH
// /v0/refinement/sessions/{id}/draft body. Exactly one arm is serialized (both are
// omitempty): brief_amendment (agent re-draft) XOR draft (direct edit). The
// caller (the tool handler) guarantees the XOR before this is built.
type editRefinementDraftRequest struct {
	BriefAmendment string     `json:"brief_amendment,omitempty"`
	Draft          *EpicDraft `json:"draft,omitempty"`
}

// decideRefinementSessionRequest mirrors the backend's POST .../decision body.
type decideRefinementSessionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// fileRefinementSessionRequest mirrors the backend's POST .../file body.
type fileRefinementSessionRequest struct {
	Repo string `json:"repo"`
}

// CreateRefinementSession opens a refinement session over a natural-language
// brief via `POST /v0/refinement/sessions` (E34.2, ADR-052 option A): it drafts
// the initial epic/children revision and returns the session view. Nothing
// files here. Requires write:approvals. 4xx/5xx surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields)
//   - 403 insufficient_scope (token lacks write:approvals)
//   - 422 validation_failed (brief is empty)
//   - 502 refinement_drafting_failed (the drafting agent produced no valid draft)
//   - 503 refinement_repo_unconfigured / refinement_drafting_unavailable
func (c *apiClient) CreateRefinementSession(ctx context.Context, brief string) (*RefinementSession, error) {
	body, err := json.Marshal(createRefinementSessionRequest{Brief: brief})
	if err != nil {
		return nil, fmt.Errorf("marshal create-refinement-session: %w", err)
	}
	var out RefinementSession
	// Agent-backed open arm: the drafter runs for minutes, so route through the
	// long client (refinementDraftClientTimeout) — the 30s short client would
	// abort mid-inference and cancel the request context.
	if err := c.doLong(ctx, http.MethodPost, "/v0/refinement/sessions", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRefinementSession reads a session's preview + derived approval state via
// `GET /v0/refinement/sessions/{id}` (E34.2). Requires write:approvals. 4xx/5xx
// surface as *apiError: 400 validation_failed (non-UUID id), 403
// insufficient_scope, 404 refinement_session_not_found, 503
// refinement_repo_unconfigured.
func (c *apiClient) GetRefinementSession(ctx context.Context, sessionID uuid.UUID) (*RefinementSession, error) {
	var out RefinementSession
	if err := c.do(ctx, http.MethodGet, "/v0/refinement/sessions/"+sessionID.String(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EditRefinementDraft appends a new draft revision via `PATCH
// /v0/refinement/sessions/{id}/draft` (E34.2) — which is precisely what
// invalidates a prior approval. Exactly one arm: briefAmendment (non-empty -> agent re-draft,
// origin=amendment, bounded by a per-session budget of 3) XOR draft (non-nil ->
// a direct strict-decoded EpicDraft edit, origin=edit, no agent call). The
// caller guarantees the XOR. Requires write:approvals. 4xx/5xx surface as
// *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 amendment_budget_exhausted (the brief-amendment budget is spent)
//   - 422 validation_failed (neither/both arms, or a draft that fails strict
//     decode/validation — an empty field, a dangling or cyclic depends_on edge)
//   - 500 audit_append_failed (the edit's audit entry could not be recorded)
//   - 502 refinement_drafting_failed (brief-amendment arm)
//   - 503 refinement_repo_unconfigured / refinement_drafting_unavailable
func (c *apiClient) EditRefinementDraft(ctx context.Context, sessionID uuid.UUID, briefAmendment string, draft *EpicDraft) (*RefinementSession, error) {
	body, err := json.Marshal(editRefinementDraftRequest{BriefAmendment: briefAmendment, Draft: draft})
	if err != nil {
		return nil, fmt.Errorf("marshal edit-refinement-draft: %w", err)
	}
	var out RefinementSession
	path := "/v0/refinement/sessions/" + sessionID.String() + "/draft"
	// The brief-amendment arm re-runs the drafting agent (minutes), so route it
	// through the long client; the direct `draft` edit is a fast strict-decode
	// with no agent call and stays on the 30s short client.
	doFn := c.do
	if briefAmendment != "" {
		doFn = c.doLong
	}
	if err := doFn(ctx, http.MethodPatch, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecideRefinementSession records an append-only approve/reject verdict pinning
// the latest revision's draft_id + content hash via `POST .../decision`
// (E34.2). reason is REQUIRED. A second decision on the same revision is 409
// (re-gate by editing, not by deciding twice). Requires write:approvals.
// 4xx/5xx surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 decision_already_recorded (the latest revision already carries a decision)
//   - 422 validation_failed (decision not approved/rejected, or a blank reason)
//   - 500 audit_append_failed
//   - 503 refinement_repo_unconfigured
func (c *apiClient) DecideRefinementSession(ctx context.Context, sessionID uuid.UUID, decision, reason string) (*RefinementSession, error) {
	body, err := json.Marshal(decideRefinementSessionRequest{Decision: decision, Reason: reason})
	if err != nil {
		return nil, fmt.Errorf("marshal decide-refinement-session: %w", err)
	}
	var out RefinementSession
	if err := c.do(ctx, http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/decision", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileRefinementSession files an approved, un-drifted draft into tracker items
// (the epic then children in wave order) via `POST .../file` (E34.3). It is
// IDEMPOTENT: the target repo is pinned at first invoke (a re-invoke naming a
// different repo is 409 refinement_filing_repo_mismatch); a mid-sequence
// provider failure is 502 refinement_filing_failed with the filed-so-far items
// + failing ordinal in details, and re-invoking resumes at the first unfiled
// ordinal; a fully completed session replays as 200 with already_completed.
// Requires write:approvals (no new scope — the E34.2 precedent). 4xx/5xx
// surface as *apiError:
//   - 400 validation_failed (malformed JSON / unknown fields / non-UUID id /
//     repo not owner/name)
//   - 403 insufficient_scope
//   - 404 refinement_session_not_found
//   - 409 refinement_not_approved / refinement_draft_drifted /
//     refinement_filing_repo_mismatch
//   - 500 internal_error (audit_append_failed on the completion close)
//   - 502 refinement_filing_failed (resumable) /
//     refinement_filing_verification_failed
//   - 503 refinement_repo_unconfigured
func (c *apiClient) FileRefinementSession(ctx context.Context, sessionID uuid.UUID, repo string) (*RefinementFilingResult, error) {
	body, err := json.Marshal(fileRefinementSessionRequest{Repo: repo})
	if err != nil {
		return nil, fmt.Errorf("marshal file-refinement-session: %w", err)
	}
	var out RefinementFilingResult
	if err := c.do(ctx, http.MethodPost, "/v0/refinement/sessions/"+sessionID.String()+"/file", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DiagnosticBundle mirrors the backend's product-facts-only diagnostic
// bundle (GET /v0/runs/{run_id}/diagnostics, #1006). Thin local copy —
// same import-direction rule as the other mirrored shapes here. Every
// field is a structured product fact safe to surface verbatim; the
// bundle carries NO diffs, paths, prompts, or free text by construction,
// so the report tool can echo it as a transparency preview of what its
// egress attached.
type DiagnosticBundle struct {
	RunID              string                  `json:"run_id"`
	WorkflowID         string                  `json:"workflow_id"`
	WorkflowSpecHash   string                  `json:"workflow_spec_hash"`
	RunnerKind         string                  `json:"runner_kind"`
	RunState           string                  `json:"run_state"`
	Stages             []DiagnosticStageFact   `json:"stages,omitempty"`
	FailingStage       *DiagnosticFailingStage `json:"failing_stage,omitempty"`
	AuditSequenceRange *DiagnosticSeqRange     `json:"audit_sequence_range,omitempty"`
	Versions           DiagnosticVersions      `json:"versions"`
	WedgeContext       *DiagnosticWedgeContext `json:"wedge_context,omitempty"`
}

// DiagnosticWedgeContext mirrors the bundle's wedge block (#1737): WHY a
// stuck run is stuck, in structured facts only. Absent on a healthy run.
type DiagnosticWedgeContext struct {
	// BlockingChecks are red required-check context names. Empty on a run
	// with no resolved required-checks snapshot (every local run) — the
	// block degrades rather than fabricating names.
	BlockingChecks []string `json:"blocking_checks,omitempty"`
	// CampaignItemState is a campaign item-state enum literal.
	CampaignItemState string `json:"campaign_item_state,omitempty"`
	// BlockedDependents counts sibling campaign items waiting on this one.
	BlockedDependents int `json:"blocked_dependents,omitempty"`
	// IntegrateWaveError is a closed marker for a fan-in failure, never a
	// free-text reason.
	IntegrateWaveError string `json:"integrate_wave_error,omitempty"`
}

// DiagnosticStageFact is one stage's position + state in the bundle.
type DiagnosticStageFact struct {
	Sequence int    `json:"sequence"`
	Type     string `json:"type"`
	State    string `json:"state"`
}

// DiagnosticFailingStage names which stage failed and how (structured
// facts only — category + audit-surface enum, never the free-text
// failure reason).
type DiagnosticFailingStage struct {
	Sequence        int    `json:"sequence"`
	Type            string `json:"type"`
	FailureCategory string `json:"failure_category"`
	FailureSurface  string `json:"failure_surface,omitempty"`
}

// DiagnosticSeqRange is the [min,max] of the run's audit sequence numbers.
type DiagnosticSeqRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// DiagnosticVersions carries the backend's build identity.
type DiagnosticVersions struct {
	Fishhawkd        DiagnosticComponent `json:"fishhawkd"`
	MinRunnerVersion string              `json:"min_runner_version"`
}

// DiagnosticComponent is a single build's version + git SHA.
type DiagnosticComponent struct {
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

// GetDiagnostics fetches a run's product-facts-only diagnostic bundle via
// `GET /v0/runs/{run_id}/diagnostics` (#1006, slice 1). Read-only; the
// report tool uses it to surface a transparency preview of exactly which
// structured facts its egress attached. 4xx surfaces as *apiError.
func (c *apiClient) GetDiagnostics(ctx context.Context, runID uuid.UUID) (*DiagnosticBundle, error) {
	var b DiagnosticBundle
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/diagnostics", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// productReportBody mirrors the backend's
// `POST /v0/runs/{run_id}/product-reports` request body
// (`backend/internal/server/product_report.go::productReportRequest`).
// Kind selects the report flavor (bug default; feature). Description is
// operator free text that crosses the boundary ONLY when IncludeFreeText
// is true, and is run through the shared redaction module server-side
// first (#1006, slice 3 consent boundary).
type productReportBody struct {
	Kind            string `json:"kind,omitempty"`
	Description     string `json:"description,omitempty"`
	IncludeFreeText bool   `json:"include_free_text,omitempty"`
}

// ProductReport mirrors the backend's product-report response: what left
// the boundary, echoed so the caller renders the outcome without a second
// fetch. Action is "created" on a dedup miss or "occurrence" on a hit.
type ProductReport struct {
	Fingerprint string `json:"fingerprint"`
	Action      string `json:"action"`
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
	// Boarded reports whether a newly filed report was placed on the
	// tracker board (#1737). Best-effort: the report is filed either way.
	Boarded bool `json:"boarded"`
	// BoardingStatus disambiguates boarded=false: "boarded",
	// "not_attempted_no_project" (no project configured — a configuration
	// state, not an error), "not_attempted_no_report" (a dedup hit created
	// nothing to board), "not_attempted_project_not_authorized" (a board
	// is configured but its coordinates are not authorized for the fixed
	// product tracker — also a configuration state), or "failed"
	// (attempted and failed; see BoardingError).
	BoardingStatus string `json:"boarding_status,omitempty"`
	// BoardingError is the placement failure cause, set only when
	// BoardingStatus is "failed" — and then always non-empty. A CLOSED
	// product-owned literal naming which step failed, never the
	// provider's raw error.
	BoardingError string `json:"boarding_error,omitempty"`
}

// ReportProductIssue files a deduped, audited upstream product report for
// a run via `POST /v0/runs/{run_id}/product-reports` (#1006). The backend
// collects the run's product-facts bundle, fingerprints the failure,
// dedup-searches the fixed product repo, and either files a new
// fingerprint-marked report or appends an occurrence comment — then writes
// a source-side product_report_filed audit entry. Free text crosses only
// when includeFreeText is true (server-side redacted first). 4xx/5xx
// surface as *apiError; the tool layer reads the code:
//   - 400 validation_failed (bad run_id / kind, unknown fields)
//   - 401 authentication_required (anonymous caller)
//   - 403 run_not_entitled (not the run's own run-bound token)
//   - 403 product_feedback_disabled (per-repo kill-switch)
//   - 404 run_not_found
//   - 501 provider_unimplemented (the configured feedback provider id is
//     not registered)
//   - 502 product_report_failed (the dedup search / file / comment failed)
func (c *apiClient) ReportProductIssue(ctx context.Context, runID uuid.UUID, kind, description string, includeFreeText bool) (*ProductReport, error) {
	body, err := json.Marshal(productReportBody{
		Kind:            kind,
		Description:     description,
		IncludeFreeText: includeFreeText,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal product-report: %w", err)
	}
	var out ProductReport
	if err := c.do(ctx, http.MethodPost, "/v0/runs/"+runID.String()+"/product-reports", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stage mirrors the wire shape. The fields cover both get_plan's
// "find the plan stage" use case and get_run_status's "tell me
// what's happening" view: type/state for the lifecycle, sequence
// for ordering, executor + timestamps + failure fields for the
// agent's context.
type Stage struct {
	ID              string        `json:"id"`
	RunID           string        `json:"run_id"`
	Sequence        int           `json:"sequence"`
	Type            string        `json:"type"`
	Executor        StageExecutor `json:"executor"`
	State           string        `json:"state"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	EndedAt         *time.Time    `json:"ended_at,omitempty"`
	FailureCategory *string       `json:"failure_category,omitempty"`
	FailureReason   *string       `json:"failure_reason,omitempty"`
	// AgentTimeoutSeconds mirrors the backend stageResponse.agent_timeout_seconds
	// (#2540): the spec-resolved agent wall clock the runner enforces for this
	// stage. The stage-wait deadline derivation reads it to report a non-terminal
	// stage's remaining budget (deadline_seconds_remaining). Zero when the
	// backend could not resolve it (legacy row, absent/unparseable spec) or on an
	// older backend that omits the key — the derivation then reports no deadline
	// (nil), the mixed-version degrade. The json tag MUST byte-match the backend
	// or the field silently decodes to zero — the #371-class wire-mirror trap.
	AgentTimeoutSeconds int `json:"agent_timeout_seconds,omitempty"`
	// Progress mirrors the backend stageResponse.progress (#2541): the runner's
	// mid-execution stage_progress heartbeat, so a mid-stage poll projects real
	// activity (last_event / turns / tokens) onto the stage-wait status instead
	// of a single 'running' bit. Nil when the stage has not reported, on a
	// legacy run, or on an older backend that omits the key. The json tag MUST
	// byte-match the backend or the field silently decodes to nil — the
	// #371-class wire-mirror trap.
	Progress  *StageProgress `json:"progress,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// StageProgress mirrors the backend stageResponse.progress sub-schema (#2541).
// The counters are cumulative WITHIN THE CURRENT AGENT ATTEMPT ONLY and reset on
// an in-driver re-spawn — the per-attempt meaning is encoded in the field names.
// Elapsed is NOT carried here: the operator-facing elapsed_seconds on the
// stage-wait status is derived server-side from the stage's started_at, which
// makes it cumulative and monotonic across re-spawns. Each json tag MUST
// byte-match the backend or that field silently decodes to zero (the #371 trap).
type StageProgress struct {
	LastEvent         string    `json:"last_event"`
	TurnsThisAttempt  int       `json:"turns_this_attempt"`
	TokensThisAttempt int       `json:"tokens_this_attempt"`
	ReportedAt        time.Time `json:"reported_at"`
}

// StageExecutor mirrors the OpenAPI sub-schema. The closed-set
// kind field (`agent` | `human`) is what an agent reads to know
// whether a downstream stage will be self-driven or wait for a
// human.
type StageExecutor struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type listStagesResult struct {
	Items []Stage `json:"items"`
}

// ListRunStages calls GET /v0/runs/{run_id}/stages. Stages come back
// ordered by sequence ascending; the tool layer picks the plan
// stage from the list.
func (c *apiClient) ListRunStages(ctx context.Context, runID uuid.UUID) ([]Stage, error) {
	var res listStagesResult
	if err := c.do(ctx, http.MethodGet, "/v0/runs/"+runID.String()+"/stages", nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// awaitStagePerCallWaitSeconds is the per-iteration ?wait long-poll the
// fishhawk_await_stage handler hands to GetRunStageWait (#2491). It is
// deliberately below TWO ceilings: the backend's own maxRunStageWaitSeconds
// (30) cap on the ?wait long-poll, AND — the binding one — the apiClient short
// client's 30s timeout (c.http). A per-call ?wait of 30 would let the backend
// hold the connection right up to the client's own deadline, so a held
// long-poll would race the client timeout and surface as a transport error
// rather than a clean settled/unsettled read. 15s is the same value
// cli/cmd/fishhawk/watch.go uses, and it leaves a full 15s of client-timeout
// headroom over the longest held ?wait.
const awaitStagePerCallWaitSeconds = 15

// RunStageWait is the MCP-server-side projection of the GET
// /v0/runs/{run_id}/stages/{stage_id} response's wait envelope (#1252,
// surfaced by fishhawk_await_stage #2491). It is a PURPOSE-BUILT struct, NOT
// an embed of the package's Stage type: the backend's runStageWaitResponse
// embeds the canonical stage shape (which carries its OWN `state` json tag)
// and adds a TOP-LEVEL state/terminal envelope, so embedding Stage would
// shadow-collide on the `state` key and hide the top-level `terminal` flag.
// Only the fields fishhawk_await_stage needs are projected; the mirror in
// cli/internal/httpclient projects the same envelope for `fishhawk run watch`.
type RunStageWait struct {
	ID              string     `json:"id"`
	RunID           string     `json:"run_id"`
	Type            string     `json:"type"`
	State           string     `json:"state"`
	Terminal        bool       `json:"terminal"`
	FailureCategory *string    `json:"failure_category,omitempty"`
	FailureReason   *string    `json:"failure_reason,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
}

// GetRunStageWait calls GET /v0/runs/{run_id}/stages/{stage_id}, decoding the
// top-level {state, terminal, failure_*} wait envelope (#1252). A positive
// waitSeconds appends the opt-in bounded server-side long-poll (?wait=<n>) so
// the call returns the moment the stage SETTLES (terminal OR parked); a
// non-positive value omits the query for an immediate single read. The value
// is clamped to awaitStagePerCallWaitSeconds so a caller can never hand the
// backend a ?wait that would race the short client's 30s deadline. Routed
// through c.do (the 30s short client).
func (c *apiClient) GetRunStageWait(ctx context.Context, runID, stageID uuid.UUID, waitSeconds int) (*RunStageWait, error) {
	path := "/v0/runs/" + runID.String() + "/stages/" + stageID.String()
	if waitSeconds > 0 {
		if waitSeconds > awaitStagePerCallWaitSeconds {
			waitSeconds = awaitStagePerCallWaitSeconds
		}
		path += "?wait=" + strconv.Itoa(waitSeconds)
	}
	var res RunStageWait
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Artifact is the wire shape with content inline. The backend
// returns content directly on the listStageArtifacts endpoint (per
// the OpenAPI Artifact schema), so the MCP tool doesn't need a
// separate /v0/artifacts/{id} fetch.
// Content is typed as `any` rather than `json.RawMessage` so the MCP
// SDK's schema reflection sees an unconstrained value. `RawMessage`
// is `[]byte` under the hood, which would surface as `type: array`
// and reject the object/scalar payloads each artifact kind carries.
// The decode side (tryGetPlanForRun) re-marshals + unmarshals into
// the typed PlanContent shape; the cost is one extra round-trip
// through json.Marshal per plan fetch, which is negligible.
type Artifact struct {
	ID            string    `json:"id"`
	StageID       string    `json:"stage_id"`
	Kind          string    `json:"kind"`
	SchemaVersion *string   `json:"schema_version,omitempty"`
	ContentHash   string    `json:"content_hash"`
	Content       any       `json:"content,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type listArtifactsResult struct {
	Items []Artifact `json:"items"`
}

// ListStageArtifacts calls GET /v0/stages/{stage_id}/artifacts.
// Artifacts come back ordered by created_at ascending; callers
// pick the most-recent (the SPA pre-trace does the same — see
// `frontend/src/routes/stage-detail.tsx`).
func (c *apiClient) ListStageArtifacts(ctx context.Context, stageID uuid.UUID) ([]Artifact, error) {
	var res listArtifactsResult
	if err := c.do(ctx, http.MethodGet, "/v0/stages/"+stageID.String()+"/artifacts", nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// releaseNotesPersistRequest mirrors the backend's POST /v0/releases/notes
// request body (`backend/internal/server/release_notes.go`, E33.2 / #1587).
// stage_id keys the persisted release_notes artifact — the persist endpoint is
// stage-scoped because no first-class release stage type exists yet. Repeated
// here (not imported) per the thin-local-copy rule: the import direction is
// cli → backend, not the reverse.
type releaseNotesPersistRequest struct {
	Repo    string `json:"repo"`
	From    string `json:"from"`
	To      string `json:"to"`
	StageID string `json:"stage_id"`
}

// ReleaseNotesPersistResult mirrors the backend's POST /v0/releases/notes 201
// body: the persisted artifact id, the coordinates, the content hash, and the
// rendered markdown (which carries the advisory semver bump hint after E33.4).
// IDs are typed string per the #371 reflection rule so the MCP SDK's schema
// reflection sees a string, not a uuid byte array.
type ReleaseNotesPersistResult struct {
	ArtifactID  string `json:"artifact_id"`
	StageID     string `json:"stage_id"`
	Repo        string `json:"repo"`
	From        string `json:"from"`
	To          string `json:"to"`
	ContentHash string `json:"content_hash"`
	Markdown    string `json:"markdown"`
}

// PreviewReleaseNotes renders the release-notes markdown for the ref range via
// GET /v0/releases/notes/preview?repo=&from=&to= (E33.2 / #1587) WITHOUT
// persisting anything. The endpoint responds with a text/markdown body (NOT a
// JSON envelope), so this reads the raw body via getText rather than decoding
// into a struct. 4xx surfaces as *apiError:
//   - 400 validation_failed (missing repo/from/to)
//   - 401 authentication_required (anonymous)
//   - 503 release_notes_unconfigured (a required repository is not wired)
func (c *apiClient) PreviewReleaseNotes(ctx context.Context, repo, from, to string) (string, error) {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("from", from)
	q.Set("to", to)
	return c.getText(ctx, "/v0/releases/notes/preview?"+q.Encode())
}

// PersistReleaseNotes renders exactly as the preview endpoint and persists the
// notes as a release_notes artifact keyed to stageID, via
// POST /v0/releases/notes (E33.2 / #1587). Returns the persisted artifact id +
// coordinates + rendered markdown. 4xx/5xx surfaces as *apiError:
//   - 400 validation_failed (missing repo/from/to/stage_id, malformed stage_id)
//   - 401 authentication_required (anonymous) / 403 insufficient_scope (needs
//     write:runs)
//   - 404 stage_not_found (stage_id references no stages row)
//   - 503 release_notes_unconfigured (a required repository is not wired)
func (c *apiClient) PersistReleaseNotes(ctx context.Context, repo, from, to, stageID string) (*ReleaseNotesPersistResult, error) {
	body, err := json.Marshal(releaseNotesPersistRequest{Repo: repo, From: from, To: to, StageID: stageID})
	if err != nil {
		return nil, fmt.Errorf("marshal release-notes persist: %w", err)
	}
	var res ReleaseNotesPersistResult
	if err := c.do(ctx, http.MethodPost, "/v0/releases/notes", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// getText performs a GET and returns the raw response body as a string. Unlike
// do, it does NOT json-decode the body — used for the text/markdown
// release-notes preview (E33.2), whose body is rendered markdown, not a JSON
// envelope. On a non-2xx response the body IS parsed as the OpenAPI error
// envelope and returned as *apiError, so callers get the same typed error
// surface as the JSON methods. Routed through the 30s short client.
func (c *apiClient) getText(ctx context.Context, path string) (string, error) {
	if c.baseURL == "" {
		return "", errors.New("apiClient: baseURL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/markdown")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{StatusCode: resp.StatusCode}
		var env struct {
			Error struct {
				Code     string         `json:"code"`
				Message  string         `json:"message"`
				Details  map[string]any `json:"details"`
				ErrorRef string         `json:"error_ref"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Code = env.Error.Code
			ae.Message = env.Error.Message
			ae.Details = env.Error.Details
			ae.ErrorRef = env.Error.ErrorRef
		}
		return "", ae
	}
	return string(raw), nil
}

// AuditEntry mirrors the OpenAPI AuditEntry schema. Payload is
// left as json.RawMessage so the MCP tool can pass the typed shape
// directly through to the client without re-encoding category-
// specific payloads — the agent introspects them as JSON.
// Payload is typed `any` for the same reason Artifact.Content is —
// the SDK's schema reflection treats `json.RawMessage` as an array,
// but per-category payloads are arbitrary JSON objects. Agents
// reading the response introspect each category's shape directly.
type AuditEntry struct {
	ID           string    `json:"id"`
	Sequence     int64     `json:"sequence"`
	RunID        string    `json:"run_id"`
	StageID      *string   `json:"stage_id,omitempty"`
	Timestamp    time.Time `json:"ts"`
	Category     string    `json:"category"`
	ActorKind    *string   `json:"actor_kind,omitempty"`
	ActorSubject *string   `json:"actor_subject,omitempty"`
	Payload      any       `json:"payload,omitempty"`
	PrevHash     *string   `json:"prev_hash,omitempty"`
	// EntryHash carries omitempty so the compact get_run_status projection can
	// blank it to drop the verifier-only hash-chain field from the wire (#1749).
	// Safe for fishhawk_list_audit: a real audit entry always has a non-empty
	// entry_hash there, so omitempty never elides it on the verifier surface.
	EntryHash string `json:"entry_hash,omitempty"`
}

type listAuditResult struct {
	Items      []AuditEntry `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// ListRunAuditFilter scopes a per-run audit query. Empty values
// drop from the query string; zero Limit lets the server pick its
// default (100, per the OpenAPI; 500 max). The MCP tool layer
// clamps to a lower cap before calling.
type ListRunAuditFilter struct {
	Category string
	StageID  string
	// SinceSequence narrows the response to entries with sequence
	// strictly greater than this value (#962) — the anchor the
	// fishhawk_await_audit primitive polls from. Zero drops from the
	// query string (the server treats 0 as a no-op anyway).
	SinceSequence int64
	Limit         int
	Cursor        string
	// AllowUnknown sets allow_unknown=true on the request (#1764), telling
	// the endpoint to skip its known-category validation. The MCP tool sets
	// it when the operator has opted into an unknown category via
	// fishhawk_await_audit's allow_unknown flag, so the tool's own polling
	// calls are not re-rejected by the endpoint. False omits the param and
	// stays byte-identical to the prior request.
	AllowUnknown bool
}

// ListRunAudit calls GET /v0/runs/{run_id}/audit with optional
// category / stage_id / limit / cursor filters. Returns entries
// sequence-ascending (matches the API surface: per-run scope for
// the run-detail UI + verifier path). For "most-recent-first"
// queries use ListRecentRunAudit which hits the cross-chain
// endpoint with time-descending order.
func (c *apiClient) ListRunAudit(ctx context.Context, runID uuid.UUID, f ListRunAuditFilter) ([]AuditEntry, string, error) {
	q := url.Values{}
	if f.Category != "" {
		q.Set("category", f.Category)
	}
	if f.StageID != "" {
		q.Set("stage_id", f.StageID)
	}
	if f.SinceSequence > 0 {
		q.Set("since_sequence", strconv.FormatInt(f.SinceSequence, 10))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("cursor", f.Cursor)
	}
	if f.AllowUnknown {
		q.Set("allow_unknown", "true")
	}
	path := "/v0/runs/" + runID.String() + "/audit"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res listAuditResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, "", err
	}
	return res.Items, res.NextCursor, nil
}

// ListRecentRunAudit calls GET /v0/audit?run_id=<id>&limit=<N>.
// Returns rows time-descending — exactly the order an agent wants
// when surfacing "what's happened recently" in the get_run_status
// view. The cross-chain endpoint is the only way to get
// descending order without a paginate-to-end walk; per-run rows
// for the queried run are the only thing returned because global
// rows have run_id IS NULL and don't match the filter.
//
// The MCP tool layer is responsible for clamping limit to the
// server's range before calling this; the client passes it
// through verbatim.
func (c *apiClient) ListRecentRunAudit(ctx context.Context, runID uuid.UUID, limit int) ([]AuditEntry, error) {
	q := url.Values{}
	q.Set("run_id", runID.String())
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var res listAuditResult
	if err := c.do(ctx, http.MethodGet, "/v0/audit?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// PlanDecomposed is the decoded plan_decomposed audit payload (E24.6 /
// #1146) the orchestrator emits when it mints a decomposed parent's
// children: the minted child run ids and the orchestrator-resolved
// effective concurrency cap (0 == unlimited). The fishhawk_run_children
// tool reads it to discover which children to dispatch and at what
// concurrency — the MCP cannot reach the workflow spec or the
// FISHHAWK_MAX_PARALLEL_CHILDREN default, so the cap is read from here.
type PlanDecomposed struct {
	ChildRunIDs          []string `json:"child_run_ids"`
	EffectiveMaxParallel int      `json:"effective_max_parallel"`
	// Waves carries the topological dispatch order (#1258 slice B) as ordered
	// waves of slice indices into ChildRunIDs (ChildRunIDs[i] is the child
	// minted for slice i). The run_children wave loop dispatches each wave
	// concurrently and integrates between waves. omitempty + nil-decodes
	// back-compat: an old plan_decomposed entry (or a no-depends_on
	// decomposition) carries no waves, which the loop collapses to a single
	// all-indices wave.
	Waves [][]int `json:"waves,omitempty"`
}

// LatestPlanDecomposed returns the decoded payload of the run's most-recent
// plan_decomposed audit entry, or (nil, nil) when the run has none (it is
// not a decomposed parent). The per-run audit endpoint returns entries
// sequence-ascending, so the authoritative entry is the last one. A corrupt
// payload surfaces as a decode error — unlike the best-effort plan-gate
// advisory reads (loadScopePrecheck et al.), run_children cannot proceed
// without the child ids, so a malformed entry must fail loud rather than
// silently dispatch nothing.
func (c *apiClient) LatestPlanDecomposed(ctx context.Context, runID uuid.UUID) (*PlanDecomposed, error) {
	entries, _, err := c.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: "plan_decomposed",
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	newest := entries[len(entries)-1]
	if newest.Payload == nil {
		return nil, fmt.Errorf("plan_decomposed entry %s has no payload", newest.ID)
	}
	raw, err := json.Marshal(newest.Payload)
	if err != nil {
		return nil, fmt.Errorf("re-encode plan_decomposed payload: %w", err)
	}
	var pd PlanDecomposed
	if err := json.Unmarshal(raw, &pd); err != nil {
		return nil, fmt.Errorf("decode plan_decomposed payload: %w", err)
	}
	return &pd, nil
}

// CalibrationParams scopes a GET /v0/calibration request. Empty
// fields drop from the query string; StageType defaults to "implement"
// server-side when omitted.
type CalibrationParams struct {
	WorkflowID string
	StageType  string
	Since      string
}

// CalibrationResult mirrors the /v0/calibration response body.
// ConfidenceBandAccuracy is typed as map[string]any so the MCP
// SDK's schema reflection sees an unconstrained object — the
// per-level bucket shape (samples + within_1.5x) is stable but
// the confidence keys ('low', 'medium', 'high') are a variable set.
type CalibrationResult struct {
	WorkflowID             string         `json:"workflow_id,omitempty"`
	StageType              string         `json:"stage_type"`
	Samples                int            `json:"samples"`
	PredictedP50Minutes    float64        `json:"predicted_p50_minutes"`
	ActualP50Minutes       float64        `json:"actual_p50_minutes"`
	ActualP95Minutes       float64        `json:"actual_p95_minutes"`
	CalibrationRatio       float64        `json:"calibration_ratio"`
	ConfidenceBandAccuracy map[string]any `json:"confidence_band_accuracy"`
}

// GetCalibration calls GET /v0/calibration. Returns aggregate runtime
// statistics across all runtime_observed audit entries that match the
// supplied filters. An empty CalibrationParams returns stats across all
// implement stages.
func (c *apiClient) GetCalibration(ctx context.Context, p CalibrationParams) (*CalibrationResult, error) {
	q := url.Values{}
	if p.WorkflowID != "" {
		q.Set("workflow_id", p.WorkflowID)
	}
	if p.StageType != "" {
		q.Set("stage_type", p.StageType)
	}
	if p.Since != "" {
		q.Set("since", p.Since)
	}
	path := "/v0/calibration"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res CalibrationResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *apiClient) ListRuns(ctx context.Context, f listRunsFilter) (*listRunsResult, error) {
	q := url.Values{}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
	}
	if f.PullRequestURL != "" {
		q.Set("pull_request_url", f.PullRequestURL)
	}
	if f.TriggerRef != "" {
		q.Set("trigger_ref", f.TriggerRef)
	}
	if f.WorkflowID != "" {
		q.Set("workflow_id", f.WorkflowID)
	}
	if f.State != "" {
		q.Set("state", f.State)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.Cursor != "" {
		q.Set("cursor", f.Cursor)
	}
	path := "/v0/runs"
	if encoded := q.Encode(); encoded != "" {
		path = path + "?" + encoded
	}
	var res listRunsResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// do is the no-extra-headers wrapper around doWithStatus that
// discards the response status code. Most readers only need to
// know that the call succeeded; the StartRun path needs the 200
// vs 201 distinction (idempotent replay vs fresh create) so it
// reaches for doWithStatus directly.
func (c *apiClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	_, err := c.doWithStatus(ctx, method, path, body, nil, out)
	return err
}

// doLong is the long-client analogue of do: it routes the request through
// c.httpLong (refinementDraftClientTimeout) instead of the 30s short client,
// for the two agent-backed refinement arms whose bodies take minutes.
func (c *apiClient) doLong(ctx context.Context, method, path string, body []byte, out any) error {
	_, err := c.doWithStatusUsing(c.httpLong, ctx, method, path, body, nil, out)
	return err
}

// doWithStatus performs the request on the 30s short client. See
// doWithStatusUsing for the mechanics.
func (c *apiClient) doWithStatus(ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, out any) (int, error) {
	return c.doWithStatusUsing(c.http, ctx, method, path, body, extraHeaders, out)
}

// doWithStatusUsing performs the request on the supplied client and decodes the
// JSON body into out. On non-2xx the body is parsed as the OpenAPI error
// envelope and returned as *apiError. `extraHeaders` is merged into the
// request — used for E8.2's Idempotency-Key on POST /v0/runs. Same posture as
// the CLI's httpclient.do. The client parameter selects the per-arm timeout
// (short vs refinementDraftClientTimeout).
func (c *apiClient) doWithStatusUsing(client *http.Client, ctx context.Context, method, path string, body []byte, extraHeaders map[string]string, out any) (int, error) {
	if c.baseURL == "" {
		return 0, errors.New("apiClient: baseURL not set")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		ae := &apiError{StatusCode: resp.StatusCode}
		var env struct {
			Error struct {
				Code     string         `json:"code"`
				Message  string         `json:"message"`
				Details  map[string]any `json:"details"`
				ErrorRef string         `json:"error_ref"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Code = env.Error.Code
			ae.Message = env.Error.Message
			ae.Details = env.Error.Details
			ae.ErrorRef = env.Error.ErrorRef
		}
		return resp.StatusCode, ae
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	return resp.StatusCode, nil
}
