package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// fakeBackend is a thin httptest server that records the last
// /v0/runs query (so tests can assert filter forwarding) and a
// /v0/runs/{id} fetch path (so the FISHHAWK_RUN_ID branch has
// somewhere to land). E19.4 / #344 added the per-run stage list
// and per-stage artifact list endpoints so the get_plan tests can
// drive the parent-walk loop without a full backend.
type fakeBackend struct {
	mu sync.Mutex

	lastListQuery string
	listResp      listRunsResult
	listStatus    int

	// /v0/runs/{run_id} fetches consult getRunByID first; the
	// fallback getResp is the default when the id isn't keyed.
	getRunByID map[uuid.UUID]Run
	getResp    Run
	getStatus  int

	// getRunExtraByID overlays extra top-level JSON fields onto a
	// keyed getRunByID response — response fields the thin client.go
	// Run mirror doesn't carry, like the drive read surfaces (#1023)
	// runDriveView decodes from the same body.
	getRunExtraByID map[uuid.UUID]map[string]any

	// getStatusByID overrides the GET /v0/runs/{id} status for a single
	// id (#1147), so a children-status best-effort test can fail ONE
	// child's GetRun (404) while its siblings resolve.
	getStatusByID map[uuid.UUID]int

	// Per-call response overrides keyed by query string for tests
	// that exercise multiple resolution paths in one server.
	listByQuery map[string]listRunsResult

	// E19.4 fixtures: stages keyed by run id, artifacts keyed by
	// stage id. Empty map → 200 with empty items list (mirrors the
	// backend's behavior for runs that haven't created stages yet).
	stagesByRun      map[uuid.UUID][]Stage
	artifactsByStage map[uuid.UUID][]Artifact
	stagesStatus     int
	// stagesFailOnCall, when > 0, makes the Nth (1-based, per run_id)
	// GET /v0/runs/{run_id}/stages call return 500 while every other call
	// uses stagesStatus. Used by the dispatch post-dispatch-fetch-failure and
	// acceptance-short-circuit post-fetch tests to fail the THIRD stages read
	// (the classify) while the first two — stage-id resolution and the #1872
	// sibling guard — succeed. Defaults to 0 (disabled).
	//
	// PRECONDITION (#2687): ordinal-indexed injection is only valid when the test
	// has exactly ONE reader of this endpoint for the run. A real detached spawn
	// adds a SECOND, concurrent reader — the reaper's zero-exit strand probe
	// (reapZeroExitStrand, #2630) reads ListRunStages on attempt 0 with no
	// pre-sleep — racing the handler's own read for the injected 500 and making
	// which call it lands on nondeterministic. A test using this knob must keep the
	// endpoint single-reader: stub the detached-spawn seam via
	// withStubbedDispatchSpawn (dispatch_stage_test.go) instead of real-spawning,
	// or exercise a no-spawn path (the acceptance short-circuit). Both users assert
	// the total per-run read count equals postDispatchStagesCall so a future extra
	// reader fails by name rather than silently re-targeting the injection.
	stagesFailOnCall int
	// stagesStatusByRun overrides the GET /v0/runs/{run_id}/stages status for a
	// SINGLE run id (E48.71 / #2503), so an acceptance-slot degrade test can fail
	// ONE candidate item's stage read (500) while its siblings resolve. Distinct
	// from stagesFailOnCall (which fails the Nth call per run regardless of run):
	// the slot fan-out issues exactly one read per candidate run, so the
	// per-run-id override is the natural way to degrade one specific item.
	stagesStatusByRun map[uuid.UUID]int
	// stagesFlip, when set, is invoked under fb.mu on every
	// GET /v0/runs/{run_id}/stages call AFTER the response snapshot is taken,
	// with the run id and the 1-based per-run call ordinal. It lets a test mutate
	// fb.stagesByRun the instant a specific read has been ANSWERED — i.e. inject a
	// concurrent advance into the window between an observation and whatever the
	// caller does next (E67.51: the park-resolved-before-the-POST race). Mirrors
	// stageWaitFlip's contract for the per-stage wait endpoint.
	stagesFlip        func(runID uuid.UUID, callNum int)
	artifactsStatus   int
	stagesCalledByID  map[uuid.UUID]int
	artifactsCalledID map[uuid.UUID]int

	// E19.5 fixtures: per-run audit responses. Captured limit lets
	// tests verify clamping behavior end-to-end.
	auditByRun      map[uuid.UUID][]AuditEntry
	auditStatus     int
	lastAuditLimit  string
	auditCalledByID map[uuid.UUID]int

	// E19.6 fixtures: per-run audit responses + recorded query
	// state for the /v0/runs/{id}/audit endpoint. Distinct from
	// the cross-chain capture above so tests can verify which
	// surface a tool routed to (and let the same backend serve
	// both shapes for the test suite that mixes them).
	perRunAuditByRun         map[uuid.UUID][]AuditEntry
	perRunAuditNextByRun     map[uuid.UUID]string
	perRunAuditStatus        int
	perRunAuditLastQueryByID map[uuid.UUID]string
	// perRunAuditNeverEndByRun, when set for a run, makes the audit endpoint
	// return an ALWAYS-advancing non-empty next_cursor (offset+limit) regardless
	// of how much data exists — simulating an unbounded / pathological fan-in
	// history so the paginated fan-in walk's page-cap exhaustion is testable
	// without seeding hundreds of thousands of entries (#2695, binding
	// condition 1).
	perRunAuditNeverEndByRun map[uuid.UUID]bool

	// #1147: counts per-run audit reads by category so the children-status
	// cost-gate test can assert a non-decomposed run issues no
	// plan_decomposed read.
	perRunAuditCategoryReads map[string]int

	// #2712 fixtures: GET /healthz and POST /v0/runs/{run_id}/reviews/reconcile.
	//
	// healthzProcessStart is the process_start value served, read under fb.mu on
	// every probe so a test can CHANGE the restart boundary MID-WAIT (the
	// binding-condition-1 case: the daemon restarts while the await is already
	// polling). Empty omits the field (a daemon predating #2712).
	// healthzStatus / healthzBody override the status and raw body for the
	// undecidable branches. healthzReads counts probes so the TTL cache can be
	// asserted.
	healthzProcessStart string
	healthzStatus       int
	healthzBody         string
	healthzReads        int

	// reconcileRespByRun is the reconcile endpoint's 200 body per run;
	// reconcileStatus overrides the status; reconcileCallsByRun counts calls;
	// reconcileHook, when non-nil, runs under fb.mu on each call so a test can
	// mutate the seeded audit (appending the synthesized terminal entry the
	// real backend would write).
	reconcileRespByRun   map[uuid.UUID]ReconcileReviewsResult
	reconcileStatus      int
	reconcileCallsByRun  map[uuid.UUID]int
	reconcileHook        func(runID uuid.UUID)
	reconcileLastRunID   uuid.UUID
	reconcileAuthHeaders []string

	// reviewFlip, when non-nil, is invoked under fb.mu on every per-run
	// audit request with the requested category. The fishhawk_await_review
	// poll-resolve test uses it to flip a pending review to complete
	// mid-poll without wall-clock sleeps — it mutates perRunAuditByRun
	// directly (the caller already holds fb.mu, so it must not re-lock).
	reviewFlip func(category string)

	// #2491 fixtures: GET /v0/runs/{run_id}/stages/{stage_id} wait envelope
	// (the #1252 endpoint fishhawk_await_stage polls). stageWaitByStageID is
	// the RunStageWait envelope returned for a stage id. stageWaitReadsByStageID
	// counts reads per stage id so a test can assert the poll path was taken
	// (> 1 read). stageWaitWaitsByStageID records every ?wait value observed ON
	// THE WIRE so the per-call clamp counterfactual asserts <= 15.
	// stageWaitStatusByStageID overrides the HTTP status for a stage id (the
	// transport-error test). stageWaitFlip, when non-nil, is invoked under fb.mu
	// on every stage-wait read with (stage id, running read count) so a test can
	// settle the stage mid-poll without wall-clock sleeps — it mutates
	// stageWaitByStageID directly (the caller already holds fb.mu).
	stageWaitByStageID       map[uuid.UUID]RunStageWait
	stageWaitReadsByStageID  map[uuid.UUID]int
	stageWaitWaitsByStageID  map[uuid.UUID][]int
	stageWaitStatusByStageID map[uuid.UUID]int
	stageWaitFlip            func(stageID uuid.UUID, reads int)

	// E22.1 fixtures: POST /v0/runs.
	// createRunBody captures the last decoded request body so tests
	// can assert what fields were sent.
	// createRunIdempKey captures the last Idempotency-Key header.
	// createRunResp drives the response Run when set; the fake
	// allocates a fresh UUID when CreateRunResp.ID is empty so the
	// dominant test pattern doesn't have to seed it.
	// createRunStatus drives the HTTP status code returned (default
	// 201 Created; tests overriding to 200 simulate the idempotent-
	// replay path).
	// createRunErrBody, when set, is written verbatim as the
	// response body — used to drive 4xx error-envelope tests.
	createRunBody     createRunRequest
	createRunIdempKey string
	createRunResp     Run
	createRunStatus   int
	createRunErrBody  string

	// #978 fixtures: POST /v0/runs/{run_id}/recover. Same shape as the
	// createRun fixtures; recoverParentID captures the path run_id.
	recoverBody     recoverRunRequest
	recoverParentID uuid.UUID
	recoverIdempKey string
	recoverResp     Run
	recoverStatus   int
	recoverErrBody  string

	// E22.2 fixtures: POST /v0/runs/{id}/cancel.
	// cancelResp lets a test seed the post-cancel Run body. When
	// empty the fake echoes the run from getRunByID (if seeded) or
	// builds a minimal Run with State="cancelled".
	// cancelStatus drives the HTTP status code (default 200).
	// cancelErrBody, when set, is written verbatim as the response
	// body so tests can drive the 404 / 409 paths.
	// cancelCalledByID counts cancel calls per run id for idempotency
	// / dedup tests.
	cancelResp       map[uuid.UUID]Run
	cancelStatus     int
	cancelErrBody    string
	cancelCalledByID map[uuid.UUID]int

	// E22.3 fixtures: POST /v0/stages/{id}/retry.
	// retryResp seeds the post-retry Stage body keyed by stage id;
	// when not seeded the fake builds a minimal Stage with
	// State="pending" (the dominant category-A/C outcome).
	// retryStatus drives the HTTP status code (default 200).
	// retryErrBody, when set, is written verbatim — used for the
	// 404 / 422 error-path tests.
	// retryCalledByID counts retry calls per stage id so tests can
	// verify short-circuits.
	retryResp       map[uuid.UUID]Stage
	retryStatus     int
	retryErrBody    string
	retryCalledByID map[uuid.UUID]int

	// #1928 fixtures: POST /v0/stages/{id}/acceptance-admission.
	// admissionShortCircuit drives the short_circuited response; on a
	// short-circuit the route flips the matching seeded stage's State to
	// succeeded so a post-short-circuit stages read reflects the settle.
	// admissionStatus (0 -> 200) drives the fail-open error branch;
	// admissionCalledByID counts admission POSTs per stage id.
	// admissionErrBody, when set on a non-200 admissionStatus, is written
	// verbatim in place of the default internal_error envelope — used to model
	// the two 404 shapes (#1937): a route-absent bare "404 page not found\n"
	// (empty decoded Code -> fail-open version skew) vs a body-decoded
	// stage_not_found envelope (non-empty Code -> fail-closed). Default empty
	// keeps every existing admission test byte-identical.
	admissionShortCircuit bool
	admissionStatus       int
	admissionErrBody      string
	admissionCalledByID   map[uuid.UUID]int
	// #1953 fixtures: the needs_target augmentation on the short_circuited:false
	// path. admissionNeedsTarget drives needs_target:true; admissionTargetHosts /
	// admissionExpectedHeadSHA populate the hosts + head SHA the verb-side gate
	// probes against. Only served when admissionShortCircuit is false.
	admissionNeedsTarget     bool
	admissionTargetHosts     []string
	admissionExpectedHeadSHA string
	// admissionLeavesRunning, when true, flips the target stage's State to
	// "running" inside the admission route (before any error status is written) —
	// modelling a mid-walk 500 whose partial TryShortCircuitAcceptance left the
	// stage in a non-dispatchable intermediate state. The fail-open re-check then
	// observes it and halts instead of spawning (#1928).
	admissionLeavesRunning bool

	// #1912 fixtures: POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch.
	// hostDispatchStatus (0 -> 200) drives the fail-closed error branch a
	// run_stage/dispatch_stage marker call hits; hostDispatchErrBody, when set, is
	// written verbatim on the error status. On a 200 the route flips the matching
	// seeded stage pending|awaiting_host_dispatch -> dispatched (transitioned:true)
	// or reports the idempotent no-op (transitioned:false) for an already-dispatched
	// stage. hostDispatchCalledByID counts marker POSTs per stage id so tests can
	// assert the marker fired exactly once before a spawn.
	// hostDispatchForceNoop, when true, skips the CAS flip entirely and always
	// returns transitioned:false / state "dispatched" regardless of the seeded
	// stage state — modelling the concurrent-invocation loser whose partition
	// read 'pending' before the winner's marker landed, so run_children's
	// transitioned:false skip branch (#1945) can be pinned deterministically
	// instead of relying on goroutine interleaving.
	hostDispatchStatus     int
	hostDispatchErrBody    string
	hostDispatchForceNoop  bool
	hostDispatchCalledByID map[uuid.UUID]int
	// E50.13 / #2363 fixtures. hostDispatchBaseBranch is echoed on the 200 body
	// as base_branch (the per-wave re-base the SERVER is authoritative for).
	// reapFailureByStage records every POST
	// /v0/runs/{run_id}/stages/{stage_id}/reap-failure body per stage id — the
	// spawn-error compensation's channel — and, on a 200, flips the seeded stage
	// to "failed" so a committed-state read can tell a fired compensation from a
	// stranded 'dispatched'. reapFailureStatus (0 -> 200) drives the
	// report-itself-fails branch.
	hostDispatchBaseBranch string
	// hostDispatchWaveNotIntegrated models the SERVER's per-wave integration
	// refusal (E50.13 / #2363), keyed by stage id: a marked stage answers 409
	// wave_not_integrated WITHOUT transitioning, exactly as the real server
	// refuses a dependent child whose predecessors have succeeded but are not yet
	// merged onto the consolidated branch. The default handler flips any seeded
	// pending|awaiting_host_dispatch stage to dispatched, which would over-model
	// a premature dependent child as dispatched; this set lets a fan-out test
	// keep such a child parked at awaiting_host_dispatch until its wave integrates.
	hostDispatchWaveNotIntegrated map[string]bool
	reapFailureByStage            map[uuid.UUID][]reapFailureRequest
	reapFailureStatus             int
	// reapFailureErrBody, when set, is written verbatim on a non-200 instead of
	// the generic internal_error body, so a test can drive the endpoint's real
	// refusal CODES (404 stage_not_found, the protected-park refusal) and assert
	// they reach the operator unchanged (#2689).
	reapFailureErrBody string
	// reapFailureRespBody, when set, is written verbatim on the 200 instead of
	// the default {transitioned:true,stage_state:"failed"} — the seam for the
	// endpoint's IDEMPOTENT no-op shape against an already-terminal stage.
	reapFailureRespBody string
	// reapFailureAttempts counts EVERY reap-failure POST the fake received,
	// including the ones it answered non-200 (#2689). reapFailureByStage records
	// only the accepted bodies, so it cannot express "the detached reaper's
	// bounded-backoff retry loop exhausted" — the residual-strand path the
	// operator verb exists to recover. This counter can.
	reapFailureAttempts int
	// decideFlipsListStatus makes the amendment decision POST also update the
	// matching row in amendmentsByRun, so a modelled child polling
	// ListScopeAmendments observes the decision land. Opt-in: the existing
	// decision tests assert on decideAmendmentResp and must stay unchanged.
	decideFlipsListStatus bool

	// E22.X fixtures: POST /v0/stages/{id}/fixup (#762).
	// fixupBody captures the last decoded request body so tests can
	// assert the selected concern indices + reason threading.
	// fixupResp seeds the post-fixup Stage keyed by stage id; default is
	// a minimal Stage with State="pending" (the re-opened outcome).
	// fixupStatus drives the HTTP status (default 200).
	// fixupErrBody, when set, is written verbatim — drives the 400 / 403
	// / 404 / 422 error-path tests.
	// fixupCalledByID counts fixup calls per stage id.
	fixupBody       fixupRequest
	fixupResp       map[uuid.UUID]Stage
	fixupStatus     int
	fixupErrBody    string
	fixupCalledByID map[uuid.UUID]int

	// #1088 fixtures: POST /v0/stages/{id}/clarification.
	// clarificationBody captures the last decoded request body so tests
	// can assert the answers + comment threading.
	// clarificationResp seeds the re-opened Stage keyed by stage id; default
	// is a minimal Stage with Type="plan" State="pending" (the re-opened
	// outcome).
	// clarificationStatus drives the HTTP status (default 200).
	// clarificationErrBody, when set, is written verbatim — drives the 400 /
	// 404 / 409 error-path tests.
	// clarificationCalledByID counts calls per stage id.
	clarificationBody       clarificationAnswerRequest
	clarificationResp       map[uuid.UUID]Stage
	clarificationStatus     int
	clarificationErrBody    string
	clarificationCalledByID map[uuid.UUID]int

	// #1099 fixtures: POST /v0/stages/{id}/revise.
	// reviseBody captures the last decoded request body so tests can
	// assert the constraint + force threading.
	// reviseResp seeds the re-opened Stage keyed by stage id; default is a
	// minimal Stage with Type="plan" State="pending" (the re-opened outcome).
	// reviseStatus drives the HTTP status (default 200).
	// reviseErrBody, when set, is written verbatim — drives the 400 / 409
	// error-path tests.
	// reviseCalledByID counts revise calls per stage id.
	reviseBody       reviseRequest
	reviseResp       map[uuid.UUID]Stage
	reviseStatus     int
	reviseErrBody    string
	reviseCalledByID map[uuid.UUID]int

	// #984 fixtures: POST /v0/concerns/{id}/waive.
	// waiveBody captures the last decoded request body (the reason).
	// waiveResp seeds the waived-concern response keyed by concern id;
	// default is a minimal WaivedConcern with State="waived".
	// waiveStatus drives the HTTP status (default 200).
	// waiveErrBody, when set, is written verbatim — drives the 400 / 403
	// / 404 / 422 error-path tests.
	// waiveCalledByID counts waive calls per concern id.
	waiveBody       waiveConcernRequest
	waiveResp       map[uuid.UUID]WaivedConcern
	waiveStatus     int
	waiveErrBody    string
	waiveCalledByID map[uuid.UUID]int

	// #1202 fixtures: POST /v0/concerns/{id}/defer.
	// deferBody captures the last decoded request body (the title
	// coordinates + overrides). deferResp seeds the defer result keyed by
	// concern id; default is a minimal result with the concern in state
	// deferred and a stub filed issue. deferStatus drives the HTTP status
	// (default 200). deferErrBody, when set, is written verbatim — drives
	// the 4xx / 5xx error-path tests. deferCalledByID counts defer calls
	// per concern id.
	deferBody       deferConcernRequest
	deferResp       map[uuid.UUID]DeferredConcernResult
	deferStatus     int
	deferErrBody    string
	deferCalledByID map[uuid.UUID]int

	// #961 fixtures: GET /v0/runs/{id}/scope-amendments + the decision
	// POST. amendmentsByRun seeds the list response; decideResp seeds the
	// decided row keyed by amendment id; decideBody captures the last
	// decoded decision body; *Status / *ErrBody drive error paths.
	// amendmentsReadsByRun counts list reads per run id so a test can assert
	// the #2588 await_stage probe cadence. amendmentsFlip, when non-nil, is
	// invoked under fb.mu on every list read with (run id, read count) BEFORE
	// the items snapshot, so a test can make an amendment appear (or settle
	// the awaited stage, driving the settledness-wins interleaving) at the Nth
	// read without wall-clock sleeps — it mutates the fixtures directly (the
	// caller already holds fb.mu, so it must not re-lock).
	amendmentsByRun      map[uuid.UUID][]ScopeAmendmentItem
	amendmentsReadsByRun map[uuid.UUID]int
	amendmentsFlip       func(runID uuid.UUID, reads int)
	amendmentsStatus     int
	amendmentsErrBody    string
	decideAmendmentResp  map[uuid.UUID]ScopeAmendmentItem
	decideAmendmentBody  scopeAmendmentDecisionRequest
	decideAmendmentState int
	decideAmendmentErr   string
	decideCalledByID     map[uuid.UUID]int

	// #1231 fixtures: POST /v0/runs/{id}/scope-completeness/decision.
	// decideScopeCompletenessResp seeds the decided park record keyed by
	// run id; decideScopeCompletenessBody captures the last decoded body;
	// *Status / *ErrBody drive the error paths; decideScopeCompletenessCalled
	// counts decisions per run id.
	decideScopeCompletenessResp   map[uuid.UUID]ScopeCompletenessDecisionResult
	decideScopeCompletenessBody   scopeCompletenessDecisionRequest
	decideScopeCompletenessStatus int
	decideScopeCompletenessErr    string
	decideScopeCompletenessCalled map[uuid.UUID]int

	// E22.4 fixtures: POST /v0/stages/{id}/approvals.
	// approvalsBody captures the last decoded body so tests can
	// assert decision + comment threading.
	// approvalsResp seeds the post-approve Stage keyed by stage id;
	// default is a minimal Stage with State="succeeded".
	// approvalsStatus drives the HTTP status (default 200).
	// approvalsErrBody, when set, is written verbatim — drives the
	// 404 / 422 error-path tests.
	// approvalsCalledByID counts approval calls per stage id.
	// approvalsRespBody, when set, is written verbatim on the 200 —
	// used to serve the #986 duplicate-labeled shape with the literal
	// duplicate_submission/prior_decision/prior_submitted_at keys so
	// the client-decode seam is pinned from this side of the wire.
	approvalsBody       approvalRequest
	approvalsResp       map[uuid.UUID]Stage
	approvalsStatus     int
	approvalsErrBody    string
	approvalsRespBody   string
	approvalsCalledByID map[uuid.UUID]int

	// Calibration fixtures: GET /v0/calibration.
	// calibrationResp drives the response body.
	// calibrationStatus drives the HTTP status code (default 200).
	// lastCalibrationQuery records the raw query string for assertion.
	calibrationResp      CalibrationResult
	calibrationStatus    int
	lastCalibrationQuery string

	// Budget fixtures: GET /v0/runs/{run_id}/budget (#693).
	// budgetByRun seeds the status per run; an unseeded run returns the
	// empty object {} — mirroring the backend's no-budget 200.
	// budgetStatus drives the HTTP status code (default 200).
	// budgetCalledByID counts fetches per run id.
	budgetByRun      map[uuid.UUID]BudgetStatus
	budgetStatus     int
	budgetCalledByID map[uuid.UUID]int

	// Cache-efficiency fixtures: GET /v0/runs/{run_id}/cache-efficiency
	// (ADR-044 slice 3 / #1352). cacheEffByRun seeds the metric per run; an
	// unseeded run returns the empty object {} — mirroring the backend's
	// no-cost-data 200. cacheEffStatus drives the HTTP status code (default
	// 200).
	cacheEffByRun  map[uuid.UUID]CacheEfficiency
	cacheEffStatus int

	// Cost fixtures: GET /v0/runs/{run_id}/cost (#1372). costByRun seeds the
	// cost surface per run; an unseeded run returns the empty object {} —
	// mirroring the backend's no-cost-data 200. costStatus drives the HTTP
	// status code (default 200).
	costByRun  map[uuid.UUID]RunCost
	costStatus int

	// Latency fixtures: GET /v0/runs/{run_id}/latency (#1702). latencyByRun
	// seeds the gate-latency rollup per run; an unseeded run returns the empty
	// object {} — mirroring the backend's no-gate-data 200. latencyStatus drives
	// the HTTP status code (default 200).
	latencyByRun  map[uuid.UUID]RunLatency
	latencyStatus int

	// integrate-wave fixtures: POST /v0/runs/{run_id}/integrate-wave (#1278
	// slice B). The run_children wave loop calls this between waves; the tests
	// drive its response and assert the call counter. integrateWaveResp is the
	// per-run response (a default integrated response is returned when unkeyed);
	// integrateWaveStatus drives the status code (default 200);
	// integrateWaveCalledByID counts calls per parent run id so the back-compat
	// "never integrate-waves a single wave" test can assert == 0.
	integrateWaveResp     map[uuid.UUID]IntegrateWaveResult
	integrateWaveStatus   int
	integrateWaveCalledBy map[uuid.UUID]int

	// E25.8 fixtures: the campaign endpoints (#1447).
	// createCampaignBody captures the last decoded POST /v0/campaigns body so
	// tests can assert repo/epic_ref/pause_policy round-trip. createCampaignResp
	// drives the response (the fake fills ID/Repo/EpicRef/State/PausePolicy from
	// the body when unset). createCampaignStatus drives the HTTP status (default
	// 201). createCampaignErr, when set, is written verbatim for 4xx/5xx tests.
	createCampaignBody   campaignCreateRequest
	createCampaignResp   Campaign
	createCampaignStatus int
	createCampaignErr    string

	// getCampaignStatusID captures the last GET /v0/campaigns/{id}/status path
	// id so status tests can assert the path round-trip. campaignStatusByID
	// seeds the per-id response; an unkeyed id returns a minimal running-campaign
	// payload. campaignStatusStatus drives the HTTP status (default 200);
	// campaignStatusErr, when set, is written verbatim for 4xx/5xx tests.
	getCampaignStatusID  uuid.UUID
	campaignStatusByID   map[uuid.UUID]CampaignStatus
	campaignStatusStatus int
	campaignStatusErr    string

	// resumeCampaignID captures the last POST /v0/campaigns/{id}/resume path id.
	// resumeCampaignResp seeds the resumed campaign (the fake fills
	// ID/State=running when unset). resumeCampaignStatus drives the HTTP status
	// (default 200); resumeCampaignErr, when set, is written verbatim.
	resumeCampaignID     uuid.UUID
	resumeCampaignResp   Campaign
	resumeCampaignStatus int
	resumeCampaignErr    string

	// cancelCampaignID captures the last POST /v0/campaigns/{id}/cancel path id.
	// cancelCampaignResp seeds the cancelled campaign (the fake fills
	// ID/State=cancelled when unset). cancelCampaignStatus drives the HTTP status
	// (default 200); cancelCampaignErr, when set, is written verbatim (#2355).
	cancelCampaignID     uuid.UUID
	cancelCampaignResp   Campaign
	cancelCampaignStatus int
	cancelCampaignErr    string

	// E26.2 fixtures: POST /v0/campaigns/{id}/runs (#1481).
	// startCampaignItemRunBody captures the last decoded request body so tests
	// can assert issue_ref/workflow_id/workflow_ref/runner_kind round-trip;
	// startCampaignItemRunID captures the path id. startCampaignItemRunResp seeds
	// the {run,item} response; startCampaignItemRunStatus drives the HTTP status
	// (default 201); startCampaignItemRunErr, when set, is written verbatim.
	startCampaignItemRunBody   startCampaignItemRunRequest
	startCampaignItemRunID     uuid.UUID
	startCampaignItemRunResp   StartCampaignItemRunResult
	startCampaignItemRunStatus int
	startCampaignItemRunErr    string
	// startCampaignItemRunCalls counts how often the POST
	// /v0/campaigns/{id}/runs handler ran (E48.69 / #2498), so the working_dir
	// refusal tests can assert an explicit never-dialed seam rather than
	// inferring it from a zero-value response.
	startCampaignItemRunCalls int
}

// recordedReapExpectedState reads the expected_state precondition the fake
// recorded for the LAST accepted reap-failure POST against stageID (E67.51 /
// #2699). The second return distinguishes a body that carried NO expected_state
// key (an UNCONDITIONAL report — the detached reaper and run_children's
// spawn-error compensation) from one that pinned a state, which is the
// distinction the whole feature turns on: the fake decodes into the client's own
// reapFailureRequest, whose *string field is nil exactly when the key was absent.
func recordedReapExpectedState(fb *fakeBackend, stageID uuid.UUID) (string, bool) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	bodies := fb.reapFailureByStage[stageID]
	if len(bodies) == 0 {
		return "", false
	}
	last := bodies[len(bodies)-1]
	if last.ExpectedState == nil {
		return "", false
	}
	return *last.ExpectedState, true
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		listStatus:                    http.StatusOK,
		getStatus:                     http.StatusOK,
		stagesStatus:                  http.StatusOK,
		artifactsStatus:               http.StatusOK,
		auditStatus:                   http.StatusOK,
		perRunAuditStatus:             http.StatusOK,
		listByQuery:                   map[string]listRunsResult{},
		getRunByID:                    map[uuid.UUID]Run{},
		getRunExtraByID:               map[uuid.UUID]map[string]any{},
		getStatusByID:                 map[uuid.UUID]int{},
		stagesByRun:                   map[uuid.UUID][]Stage{},
		stagesStatusByRun:             map[uuid.UUID]int{},
		artifactsByStage:              map[uuid.UUID][]Artifact{},
		stagesCalledByID:              map[uuid.UUID]int{},
		artifactsCalledID:             map[uuid.UUID]int{},
		auditByRun:                    map[uuid.UUID][]AuditEntry{},
		auditCalledByID:               map[uuid.UUID]int{},
		perRunAuditByRun:              map[uuid.UUID][]AuditEntry{},
		perRunAuditNextByRun:          map[uuid.UUID]string{},
		perRunAuditNeverEndByRun:      map[uuid.UUID]bool{},
		perRunAuditLastQueryByID:      map[uuid.UUID]string{},
		perRunAuditCategoryReads:      map[string]int{},
		stageWaitByStageID:            map[uuid.UUID]RunStageWait{},
		stageWaitReadsByStageID:       map[uuid.UUID]int{},
		stageWaitWaitsByStageID:       map[uuid.UUID][]int{},
		stageWaitStatusByStageID:      map[uuid.UUID]int{},
		healthzStatus:                 http.StatusOK,
		reconcileRespByRun:            map[uuid.UUID]ReconcileReviewsResult{},
		reconcileStatus:               http.StatusOK,
		reconcileCallsByRun:           map[uuid.UUID]int{},
		createRunStatus:               http.StatusCreated,
		recoverStatus:                 http.StatusCreated,
		cancelResp:                    map[uuid.UUID]Run{},
		cancelStatus:                  http.StatusOK,
		cancelCalledByID:              map[uuid.UUID]int{},
		retryResp:                     map[uuid.UUID]Stage{},
		retryStatus:                   http.StatusOK,
		retryCalledByID:               map[uuid.UUID]int{},
		admissionCalledByID:           map[uuid.UUID]int{},
		hostDispatchCalledByID:        map[uuid.UUID]int{},
		hostDispatchWaveNotIntegrated: map[string]bool{},
		reapFailureByStage:            map[uuid.UUID][]reapFailureRequest{},
		fixupResp:                     map[uuid.UUID]Stage{},
		fixupStatus:                   http.StatusOK,
		fixupCalledByID:               map[uuid.UUID]int{},
		clarificationResp:             map[uuid.UUID]Stage{},
		clarificationStatus:           http.StatusOK,
		clarificationCalledByID:       map[uuid.UUID]int{},
		reviseResp:                    map[uuid.UUID]Stage{},
		reviseStatus:                  http.StatusOK,
		reviseCalledByID:              map[uuid.UUID]int{},
		waiveResp:                     map[uuid.UUID]WaivedConcern{},
		waiveStatus:                   http.StatusOK,
		waiveCalledByID:               map[uuid.UUID]int{},
		deferResp:                     map[uuid.UUID]DeferredConcernResult{},
		deferStatus:                   http.StatusOK,
		deferCalledByID:               map[uuid.UUID]int{},
		amendmentsByRun:               map[uuid.UUID][]ScopeAmendmentItem{},
		amendmentsReadsByRun:          map[uuid.UUID]int{},
		amendmentsStatus:              http.StatusOK,
		decideAmendmentResp:           map[uuid.UUID]ScopeAmendmentItem{},
		decideAmendmentState:          http.StatusOK,
		decideCalledByID:              map[uuid.UUID]int{},
		decideScopeCompletenessResp:   map[uuid.UUID]ScopeCompletenessDecisionResult{},
		decideScopeCompletenessStatus: http.StatusOK,
		decideScopeCompletenessCalled: map[uuid.UUID]int{},
		approvalsResp:                 map[uuid.UUID]Stage{},
		approvalsStatus:               http.StatusOK,
		approvalsCalledByID:           map[uuid.UUID]int{},
		calibrationStatus:             http.StatusOK,
		budgetByRun:                   map[uuid.UUID]BudgetStatus{},
		budgetStatus:                  http.StatusOK,
		budgetCalledByID:              map[uuid.UUID]int{},
		cacheEffByRun:                 map[uuid.UUID]CacheEfficiency{},
		cacheEffStatus:                http.StatusOK,
		costByRun:                     map[uuid.UUID]RunCost{},
		costStatus:                    http.StatusOK,
		latencyByRun:                  map[uuid.UUID]RunLatency{},
		latencyStatus:                 http.StatusOK,
		integrateWaveResp:             map[uuid.UUID]IntegrateWaveResult{},
		integrateWaveStatus:           http.StatusOK,
		integrateWaveCalledBy:         map[uuid.UUID]int{},
		createCampaignStatus:          http.StatusCreated,
		startCampaignItemRunStatus:    http.StatusCreated,
		campaignStatusByID:            map[uuid.UUID]CampaignStatus{},
		campaignStatusStatus:          http.StatusOK,
		resumeCampaignStatus:          http.StatusOK,
		cancelCampaignStatus:          http.StatusOK,
	}
	mux := http.NewServeMux()
	// GET /healthz — the #2712 restart-boundary probe. Unauthenticated by
	// contract; the handler records any Authorization header it DID receive so
	// a test can prove the client sends none.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.healthzReads++
		status := fb.healthzStatus
		raw := fb.healthzBody
		ps := fb.healthzProcessStart
		if auth := r.Header.Get("Authorization"); auth != "" {
			fb.reconcileAuthHeaders = append(fb.reconcileAuthHeaders, "healthz:"+auth)
		}
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if raw != "" {
			_, _ = w.Write([]byte(raw))
			return
		}
		body := map[string]any{"status": "ok", "version": "dev", "git_sha": "deadbeef"}
		if ps != "" {
			body["process_start"] = ps
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	// POST /v0/runs/{run_id}/reviews/reconcile — the #2712 on-demand recovery.
	mux.HandleFunc("POST /v0/runs/{run_id}/reviews/reconcile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.reconcileCallsByRun[id]++
		fb.reconcileLastRunID = id
		status := fb.reconcileStatus
		resp, ok := fb.reconcileRespByRun[id]
		if fb.reconcileHook != nil {
			fb.reconcileHook(id)
		}
		fb.mu.Unlock()
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"error":{"code":"run_not_found","message":"no run with that id"}}`))
			return
		}
		if !ok {
			resp = ReconcileReviewsResult{RunID: id.String()}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body approvalRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.approvalsCalledByID[id]++
		fb.approvalsBody = body
		status := fb.approvalsStatus
		errBody := fb.approvalsErrBody
		rawBody := fb.approvalsRespBody
		resp, ok := fb.approvalsResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if rawBody != "" {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		if !ok {
			defaultState := "succeeded"
			if body.Decision == "reject" {
				defaultState = "failed"
			}
			resp = Stage{ID: id.String(), Type: "plan", State: defaultState}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/integrate-wave", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.integrateWaveCalledBy[id]++
		status := fb.integrateWaveStatus
		resp, ok := fb.integrateWaveResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// Default: a clean integration onto a per-run consolidated branch.
			resp = IntegrateWaveResult{
				RunID:              id.String(),
				Outcome:            "integrated",
				ConsolidatedBranch: "fishhawk/run-" + id.String()[:8] + "-consolidated",
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.retryCalledByID[id]++
		status := fb.retryStatus
		errBody := fb.retryErrBody
		resp, ok := fb.retryResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/acceptance-admission", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.admissionCalledByID[id]++
		sc := fb.admissionShortCircuit
		status := fb.admissionStatus
		errBody := fb.admissionErrBody
		needsTarget := fb.admissionNeedsTarget
		targetHosts := fb.admissionTargetHosts
		expectedHeadSHA := fb.admissionExpectedHeadSHA
		newState := ""
		if sc {
			// Reflect the server-side settle so a post-short-circuit stages read
			// returns the succeeded stage.
			newState = "succeeded"
		} else if fb.admissionLeavesRunning {
			// Model a mid-walk 500 that left the stage 'running' (#1928): the
			// fail-open re-check must observe this and halt.
			newState = "running"
		}
		if newState != "" {
			for _, stages := range fb.stagesByRun {
				for i := range stages {
					if stages[i].ID == id.String() {
						stages[i].State = newState
					}
				}
			}
		}
		fb.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			if errBody != "" {
				_, _ = w.Write([]byte(errBody))
			} else {
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
			}
			return
		}
		res := AcceptanceAdmissionResult{ShortCircuited: sc}
		if sc {
			res.Kind = "all_skip_with_basis"
			res.Basis = "all-skip-with-basis"
			res.CriteriaTotal = 2
			res.Stage = &Stage{ID: id.String(), Type: "acceptance", State: "succeeded"}
		} else if needsTarget {
			// #1953: the needs_target augmentation on the no-short-circuit path.
			res.NeedsTarget = true
			res.TargetHosts = targetHosts
			res.ExpectedHeadSHA = expectedHeadSHA
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.hostDispatchCalledByID[id]++
		// The per-wave integration refusal (E50.13 / #2363): a marked stage
		// answers 409 wave_not_integrated and is NOT transitioned, modelling the
		// server refusing a dependent child whose wave has not integrated yet.
		if fb.hostDispatchWaveNotIntegrated[id.String()] {
			fb.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"wave_not_integrated","message":"dependency slices have succeeded but are not yet integrated onto the consolidated branch"}}`))
			return
		}
		status := fb.hostDispatchStatus
		errBody := fb.hostDispatchErrBody
		forceNoop := fb.hostDispatchForceNoop
		baseBranch := fb.hostDispatchBaseBranch
		// Model the CAS marker: flip a seeded pending|awaiting_host_dispatch stage
		// to dispatched (transitioned:true); an already-dispatched stage is the
		// idempotent no-op (transitioned:false). The resulting state is echoed back.
		// forceNoop models a concurrent WINNER having already flipped this stage to
		// dispatched: it sets the seeded stage straight to "dispatched" (regardless
		// of its prior state) and always answers the loser's no-op, so a downstream
		// live read (the post-run stage_state refresh) observes the same state the
		// marker echoed rather than a stale "pending".
		transitioned := false
		state := "dispatched"
		for _, stages := range fb.stagesByRun {
			for i := range stages {
				if stages[i].ID != id.String() {
					continue
				}
				if forceNoop {
					stages[i].State = "dispatched"
				} else {
					switch stages[i].State {
					case "pending", "awaiting_host_dispatch":
						stages[i].State = "dispatched"
						transitioned = true
					}
				}
				state = stages[i].State
			}
		}
		fb.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			if errBody != "" {
				_, _ = w.Write([]byte(errBody))
			} else {
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"host-dispatch boom"}}`))
			}
			return
		}
		_ = json.NewEncoder(w).Encode(HostDispatchResult{Transitioned: transitioned, StageState: state, BaseBranch: baseBranch})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body reapFailureRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.reapFailureAttempts++
		status := fb.reapFailureStatus
		errBody := fb.reapFailureErrBody
		respBody := fb.reapFailureRespBody
		if status == 0 || status == http.StatusOK {
			fb.reapFailureByStage[id] = append(fb.reapFailureByStage[id], body)
			// Model the reaper's authority over {dispatched, running}: the stage
			// lands 'failed' so a committed-state read tells a fired compensation
			// from a stranded 'dispatched'.
			for _, stages := range fb.stagesByRun {
				for i := range stages {
					if stages[i].ID == id.String() {
						stages[i].State = "failed"
					}
				}
			}
		}
		fb.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			if errBody != "" {
				_, _ = w.Write([]byte(errBody))
			} else {
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"reap-failure boom"}}`))
			}
			return
		}
		if respBody != "" {
			_, _ = w.Write([]byte(respBody))
			return
		}
		_ = json.NewEncoder(w).Encode(ReapFailureResult{Transitioned: true, StageState: "failed"})
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/fixup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body fixupRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.fixupCalledByID[id]++
		fb.fixupBody = body
		status := fb.fixupStatus
		errBody := fb.fixupErrBody
		resp, ok := fb.fixupResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/clarification", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body clarificationAnswerRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.clarificationCalledByID[id]++
		fb.clarificationBody = body
		status := fb.clarificationStatus
		errBody := fb.clarificationErrBody
		resp, ok := fb.clarificationResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), Type: "plan", State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/revise", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body reviseRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.reviseCalledByID[id]++
		fb.reviseBody = body
		status := fb.reviseStatus
		errBody := fb.reviseErrBody
		resp, ok := fb.reviseResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), Type: "plan", State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/concerns/{concern_id}/waive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("concern_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body waiveConcernRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.waiveCalledByID[id]++
		fb.waiveBody = body
		status := fb.waiveStatus
		errBody := fb.waiveErrBody
		resp, ok := fb.waiveResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = WaivedConcern{ID: id.String(), State: "waived", StateReason: body.Reason}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/concerns/{concern_id}/defer", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("concern_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body deferConcernRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.deferCalledByID[id]++
		fb.deferBody = body
		status := fb.deferStatus
		errBody := fb.deferErrBody
		resp, ok := fb.deferResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = DeferredConcernResult{
				Concern: DeferredConcern{ID: id.String(), State: "deferred", StateReason: "deferred to #4242"},
				Issue:   DeferFiledIssue{Type: "chore", Number: 4242, URL: "https://github.com/kuhlman-labs/fishhawk/issues/4242"},
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.amendmentsReadsByRun[id]++
		if fb.amendmentsFlip != nil {
			fb.amendmentsFlip(id, fb.amendmentsReadsByRun[id])
		}
		status := fb.amendmentsStatus
		errBody := fb.amendmentsErrBody
		// Copy the elements, not just the slice header: the decision handler
		// mutates items[i].Status in place under the same lock, so encoding the
		// shared backing array after Unlock races with a concurrent decision.
		items := append([]ScopeAmendmentItem(nil), fb.amendmentsByRun[id]...)
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if items == nil {
			items = []ScopeAmendmentItem{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		amendmentID, perr := uuid.Parse(r.PathValue("amendment_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body scopeAmendmentDecisionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.decideCalledByID[amendmentID]++
		fb.decideAmendmentBody = body
		status := fb.decideAmendmentState
		errBody := fb.decideAmendmentErr
		resp, ok := fb.decideAmendmentResp[amendmentID]
		if fb.decideFlipsListStatus && (status == 0 || status == http.StatusOK) && errBody == "" {
			decided := "approved"
			if body.Decision == "deny" {
				decided = "denied"
			}
			for rid := range fb.amendmentsByRun {
				items := fb.amendmentsByRun[rid]
				for i := range items {
					if items[i].ID == amendmentID.String() {
						items[i].Status = decided
					}
				}
			}
		}
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ScopeAmendmentItem{ID: amendmentID.String(), Status: "approved"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /v0/runs/{run_id}/scope-completeness/decision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		runID, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body scopeCompletenessDecisionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.decideScopeCompletenessCalled[runID]++
		fb.decideScopeCompletenessBody = body
		status := fb.decideScopeCompletenessStatus
		errBody := fb.decideScopeCompletenessErr
		resp, ok := fb.decideScopeCompletenessResp[runID]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ScopeCompletenessDecisionResult{RunID: runID.String(), Decision: body.Decision}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.cancelCalledByID[id]++
		status := fb.cancelStatus
		errBody := fb.cancelErrBody
		resp, ok := fb.cancelResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Run{ID: id.String(), State: "cancelled"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body createRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.createRunBody = body
		fb.createRunIdempKey = r.Header.Get("Idempotency-Key")
		status := fb.createRunStatus
		errBody := fb.createRunErrBody
		resp := fb.createRunResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = uuid.NewString()
		}
		if resp.Repo == "" {
			resp.Repo = body.Repo
		}
		if resp.WorkflowID == "" {
			resp.WorkflowID = body.WorkflowID
		}
		if resp.State == "" {
			resp.State = "pending"
		}
		// PERSIST the run (with the bound working_dir from the request body)
		// keyed by its id, so a subsequent GET /v0/runs/{id} serves the STORED
		// binding — a genuine store-and-reload the full-chain test reads back
		// through, distinct from this POST response (E66.42 / #2482). The POST
		// response deliberately does NOT echo working_dir, so a test asserting
		// the binding must go through the GET readback, not this response.
		if id, perr := uuid.Parse(resp.ID); perr == nil {
			stored := resp
			stored.WorkingDir = body.WorkingDir
			fb.mu.Lock()
			fb.getRunByID[id] = stored
			fb.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/recover", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body recoverRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.recoverBody = body
		fb.recoverParentID = id
		fb.recoverIdempKey = r.Header.Get("Idempotency-Key")
		status := fb.recoverStatus
		errBody := fb.recoverErrBody
		resp := fb.recoverResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = uuid.NewString()
		}
		if resp.ParentRunID == nil {
			pid := id.String()
			resp.ParentRunID = &pid
		}
		if resp.State == "" {
			resp.State = "pending"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/campaigns", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body campaignCreateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.createCampaignBody = body
		status := fb.createCampaignStatus
		errBody := fb.createCampaignErr
		resp := fb.createCampaignResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = uuid.NewString()
		}
		if resp.Repo == "" {
			resp.Repo = body.Repo
		}
		if resp.EpicRef == "" {
			resp.EpicRef = body.EpicRef
		}
		if resp.State == "" {
			resp.State = "pending"
		}
		if resp.PausePolicy == "" {
			if body.PausePolicy != "" {
				resp.PausePolicy = body.PausePolicy
			} else {
				resp.PausePolicy = "pause_campaign"
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v0/campaigns/{campaign_id}/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("campaign_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.getCampaignStatusID = id
		status := fb.campaignStatusStatus
		errBody := fb.campaignStatusErr
		resp, ok := fb.campaignStatusByID[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			// Default: a running campaign with one item waiting.
			resp = CampaignStatus{
				Campaign:   Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#10", State: "running", PausePolicy: "pause_campaign"},
				Items:      []CampaignItem{},
				Rollup:     CampaignRollup{Eligible: []string{}, Blocked: []string{}, Running: []string{"#11"}, Done: []string{}, Failed: []string{}, Cancelled: []string{}, Paused: []string{}},
				NextAction: CampaignNextAction{Action: "wait", Detail: "items are running or blocked on a dependency"},
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/resume", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("campaign_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.resumeCampaignID = id
		status := fb.resumeCampaignStatus
		errBody := fb.resumeCampaignErr
		resp := fb.resumeCampaignResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = id.String()
		}
		if resp.State == "" {
			resp.State = "running"
		}
		if resp.PausePolicy == "" {
			resp.PausePolicy = "pause_campaign"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("campaign_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.cancelCampaignID = id
		status := fb.cancelCampaignStatus
		errBody := fb.cancelCampaignErr
		resp := fb.cancelCampaignResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = id.String()
		}
		if resp.State == "" {
			resp.State = "cancelled"
		}
		if resp.PausePolicy == "" {
			resp.PausePolicy = "pause_campaign"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("campaign_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body startCampaignItemRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.startCampaignItemRunCalls++
		fb.startCampaignItemRunID = id
		fb.startCampaignItemRunBody = body
		status := fb.startCampaignItemRunStatus
		errBody := fb.startCampaignItemRunErr
		resp := fb.startCampaignItemRunResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.Run.ID == "" {
			// Echo the body's working_dir onto the minted run (E48.69 / #2498) so
			// the run the campaign path binds carries the operator's checkout —
			// this is the observable the binding test reads back.
			resp.Run = Run{ID: uuid.NewString(), Repo: "x/y", State: "pending", RunnerKind: body.RunnerKind, WorkingDir: body.WorkingDir}
		}
		if resp.Item.ID == "" {
			resp.Item = CampaignItem{ID: uuid.NewString(), IssueRef: body.IssueRef, State: "running", RunID: resp.Run.ID}
		}
		// Persist the minted run so a later GET /v0/runs/{id} — the read
		// resolveWorkingDirForRun performs to inherit the binding — serves it
		// back (E48.69 / #2498).
		if rid, perr := uuid.Parse(resp.Run.ID); perr == nil {
			fb.mu.Lock()
			if fb.getRunByID == nil {
				fb.getRunByID = map[uuid.UUID]Run{}
			}
			fb.getRunByID[rid] = resp.Run
			fb.mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.lastListQuery = r.URL.RawQuery
		resp, override := fb.listByQuery[r.URL.RawQuery]
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.listStatus)
		if override {
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.listResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		row, ok := fb.getRunByID[id]
		extra := fb.getRunExtraByID[id]
		status := fb.getStatus
		if override, has := fb.getStatusByID[id]; has {
			status = override
		}
		fb.mu.Unlock()
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"run_not_found","message":"not found"}`))
			return
		}
		w.WriteHeader(status)
		if ok {
			if len(extra) > 0 {
				// Overlay the extra top-level fields the typed Run
				// mirror can't express (drive read surfaces, #1023).
				b, _ := json.Marshal(row)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				for k, v := range extra {
					m[k] = v
				}
				_ = json.NewEncoder(w).Encode(m)
				return
			}
			_ = json.NewEncoder(w).Encode(row)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.getResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.stagesCalledByID[id]++
		callNum := fb.stagesCalledByID[id]
		// Snapshot the stage VALUES under the lock (not just the slice header):
		// the host-dispatch handler mutates stages[i].State in place on this same
		// backing array, so encoding the shared slice after unlocking would race
		// (#1912 fix-up made run_children fire concurrent host-dispatch markers
		// against these stages).
		src := fb.stagesByRun[id]
		items := make([]Stage, len(src))
		copy(items, src)
		status := fb.stagesStatus
		if fb.stagesFailOnCall > 0 && callNum == fb.stagesFailOnCall {
			status = http.StatusInternalServerError
		}
		// Per-run-id status override (#2503): fail exactly this run's stage read.
		if s, ok := fb.stagesStatusByRun[id]; ok {
			status = s
		}
		// The response is already snapshotted, so a flip here lands strictly
		// AFTER this read is answered — the concurrent-advance injection point.
		if fb.stagesFlip != nil {
			fb.stagesFlip(id, callNum)
		}
		fb.mu.Unlock()
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(listStagesResult{Items: items})
	})
	// #2491: the #1252 per-stage wait envelope endpoint fishhawk_await_stage's
	// GetRunStageWait hits. Records the read count + every ?wait value seen on
	// the wire, invokes stageWaitFlip so a test can settle the stage mid-poll,
	// and returns the RunStageWait envelope (top-level state/terminal shape).
	mux.HandleFunc("GET /v0/runs/{run_id}/stages/{stage_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stageID, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.stageWaitReadsByStageID[stageID]++
		reads := fb.stageWaitReadsByStageID[stageID]
		waitVal := 0
		if raw := r.URL.Query().Get("wait"); raw != "" {
			if n, aerr := strconv.Atoi(raw); aerr == nil {
				waitVal = n
			}
		}
		fb.stageWaitWaitsByStageID[stageID] = append(fb.stageWaitWaitsByStageID[stageID], waitVal)
		if fb.stageWaitFlip != nil {
			fb.stageWaitFlip(stageID, reads)
		}
		status := fb.stageWaitStatusByStageID[stageID]
		if status == 0 {
			status = http.StatusOK
		}
		env := fb.stageWaitByStageID[stageID]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if status >= 400 {
			return
		}
		_ = json.NewEncoder(w).Encode(env)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.perRunAuditLastQueryByID[id] = r.URL.RawQuery
		if cat := r.URL.Query().Get("category"); cat != "" {
			fb.perRunAuditCategoryReads[cat]++
		}
		if fb.reviewFlip != nil {
			fb.reviewFlip(r.URL.Query().Get("category"))
		}
		all := fb.perRunAuditByRun[id]
		literalNext, hasLiteralNext := fb.perRunAuditNextByRun[id]
		neverEnd := fb.perRunAuditNeverEndByRun[id]
		status := fb.perRunAuditStatus
		fb.mu.Unlock()
		// Mirror the backend's category filter: when a category query
		// param is set, return only entries of that category. The
		// production endpoint filters server-side; loadPlanReviews
		// relies on this to query plan_reviewed and plan_review_skipped
		// independently (#574).
		items := all
		if cat := r.URL.Query().Get("category"); cat != "" {
			items = nil
			for _, e := range all {
				if e.Category == cat {
					items = append(items, e)
				}
			}
		}
		// Mirror the backend's since_sequence filter (#962): entries
		// with Sequence strictly greater than the anchor, applied
		// before the limit — the contract fishhawk_await_audit's
		// sequence-anchored poll relies on.
		if rawSince := r.URL.Query().Get("since_sequence"); rawSince != "" {
			since, serr := strconv.ParseInt(rawSince, 10, 64)
			if serr != nil || since < 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			filtered := make([]AuditEntry, 0, len(items))
			for _, e := range items {
				if e.Sequence > since {
					filtered = append(filtered, e)
				}
			}
			items = filtered
		}
		// Mirror the backend's limit + cursor pagination (reads.go
		// handleListRunAudit), applied AFTER the category + since_sequence
		// filters to match handler order (#2695). The cursor is base64("offset:<n>"),
		// the exact encoding the server's decodeOffsetCursor accepts — hand-rolled
		// locally so the fake needs no server internals. This is what makes the
		// item-1 category-paginated fan-in read testable rather than vacuous (the
		// #2660 blind-spot class): without it a >500-entry regression would pass on
		// the unfixed single-page read.
		//
		// BACK-COMPAT: when perRunAuditNextByRun[id] is EXPLICITLY set (key
		// present), keep today's behavior — the full filtered slice plus that
		// literal cursor, ignoring limit/cursor — so the existing pagination-shape
		// tests that pre-seed a literal next cursor stay green.
		nextCursor := ""
		if hasLiteralNext {
			nextCursor = literalNext
		} else if limitStr, cursorStr := r.URL.Query().Get("limit"), r.URL.Query().Get("cursor"); limitStr != "" || cursorStr != "" {
			offset, oerr := decodeFakeOffsetCursor(cursorStr)
			if oerr != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			limit := len(items) // absent limit → whole (filtered) slice, as the old fake did
			if limitStr != "" {
				n, lerr := strconv.Atoi(limitStr)
				if lerr != nil || n < 1 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				limit = n
			}
			items, nextCursor = fakePageOffset(items, offset, limit)
			if neverEnd {
				// Pathological/unbounded history: always hand back an ADVANCING
				// non-empty cursor so a paginated walk never terminates (exercises
				// the fan-in page-cap exhaustion, #2695).
				pageLen := limit
				if pageLen < 1 {
					pageLen = 1
				}
				nextCursor = encodeFakeOffsetCursor(offset + pageLen)
			}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: items, NextCursor: nextCursor})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/budget", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.budgetCalledByID[id]++
		bs, ok := fb.budgetByRun[id]
		status := fb.budgetStatus
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// No budget configured — empty object, as the backend does.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(bs)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/cache-efficiency", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		ce, ok := fb.cacheEffByRun[id]
		status := fb.cacheEffStatus
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// No cost data — empty object, as the backend does.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(ce)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/cost", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		rc, ok := fb.costByRun[id]
		status := fb.costStatus
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// No cost data — empty object, as the backend does.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(rc)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/latency", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		rl, ok := fb.latencyByRun[id]
		status := fb.latencyStatus
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// No gate data — empty object, as the backend does.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(rl)
	})
	mux.HandleFunc("GET /v0/audit", func(w http.ResponseWriter, r *http.Request) {
		runIDQ := r.URL.Query().Get("run_id")
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(runIDQ)
		if perr != nil {
			// /v0/audit allows missing run_id (global feed); the
			// MCP tool always sets it, so a missing one in tests
			// is a programming error.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.lastAuditLimit = r.URL.Query().Get("limit")
		fb.auditCalledByID[id]++
		items := fb.auditByRun[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.auditStatus)
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: items})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.artifactsCalledID[id]++
		items := fb.artifactsByStage[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.artifactsStatus)
		_ = json.NewEncoder(w).Encode(listArtifactsResult{Items: items})
	})
	mux.HandleFunc("GET /v0/calibration", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.lastCalibrationQuery = r.URL.RawQuery
		status := fb.calibrationStatus
		resp := fb.calibrationResp
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func newResolver(srv *httptest.Server, env map[string]string) *runResolver {
	return &runResolver{
		api: newAPIClient(config{
			backendURL: srv.URL,
			apiToken:   "tok-test",
		}),
		getenv: envFuncFromMap(env),
	}
}

func envFuncFromMap(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestResolveWorkingDir pins the transport-conditional working_dir resolver
// (#2479), one case per named branch of resolveWorkingDir. The two HTTP refusal
// branches (i) and (ii) are the controls the counterfactual pass deletes; cases
// (iv) and (vii) pin the stdio absolute-cwd resolution and its getwd-failure
// propagation.
func TestResolveWorkingDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	absDir := t.TempDir() // t.TempDir() is always absolute

	// (i) http + empty -> error naming working_dir.
	t.Run("http_empty_refused", func(t *testing.T) {
		r := &runResolver{httpTransport: true}
		got, err := r.resolveWorkingDir("")
		if err == nil {
			t.Fatalf("expected an error, got %q", got)
		}
		if !strings.Contains(err.Error(), "working_dir") {
			t.Errorf("error should name working_dir; got %v", err)
		}
	})

	// (ii) http + relative -> error (a relative path resolves against the daemon
	// cwd, so it is refused too).
	t.Run("http_relative_refused", func(t *testing.T) {
		r := &runResolver{httpTransport: true}
		got, err := r.resolveWorkingDir("sub/dir")
		if err == nil {
			t.Fatalf("expected an error for a relative path, got %q", got)
		}
		if !strings.Contains(err.Error(), "working_dir") {
			t.Errorf("error should name working_dir; got %v", err)
		}
	})

	// (iii) http + absolute -> passed through unchanged.
	t.Run("http_absolute_passthrough", func(t *testing.T) {
		r := &runResolver{httpTransport: true}
		got, err := r.resolveWorkingDir(absDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != absDir {
			t.Errorf("http+absolute = %q, want %q (unchanged)", got, absDir)
		}
	})

	// (iv) stdio + empty -> the process cwd as an ABSOLUTE path, never ".".
	t.Run("stdio_empty_resolves_to_absolute_cwd", func(t *testing.T) {
		r := &runResolver{httpTransport: false}
		got, err := r.resolveWorkingDir("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == "." {
			t.Fatal("stdio+empty resolved to the literal \".\"; want the absolute cwd")
		}
		if !filepath.IsAbs(got) {
			t.Errorf("stdio+empty = %q, want an absolute path", got)
		}
		if got != cwd {
			t.Errorf("stdio+empty = %q, want os.Getwd() %q", got, cwd)
		}
	})

	// (v) stdio + relative -> filepath.Abs of it.
	t.Run("stdio_relative_resolves_to_abs", func(t *testing.T) {
		r := &runResolver{httpTransport: false}
		want, err := filepath.Abs("sub/dir")
		if err != nil {
			t.Fatalf("filepath.Abs: %v", err)
		}
		got, err := r.resolveWorkingDir("sub/dir")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Errorf("stdio+relative = %q, want %q", got, want)
		}
	})

	// (vi) stdio + absolute -> unchanged.
	t.Run("stdio_absolute_unchanged", func(t *testing.T) {
		r := &runResolver{httpTransport: false}
		got, err := r.resolveWorkingDir(absDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != absDir {
			t.Errorf("stdio+absolute = %q, want %q (unchanged)", got, absDir)
		}
	})

	// (vii) stdio + a failing getwd seam -> error propagated, no "." fallback.
	t.Run("stdio_getwd_failure_propagated", func(t *testing.T) {
		r := &runResolver{
			httpTransport: false,
			getwd:         func() (string, error) { return "", fmt.Errorf("boom") },
		}
		got, err := r.resolveWorkingDir("")
		if err == nil {
			t.Fatalf("expected the getwd error to propagate, got %q", got)
		}
		if got == "." {
			t.Error("getwd failure degraded to the literal \".\"; want the error propagated")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("error should wrap the getwd failure; got %v", err)
		}
	})
}

// --- E66.42 / #2482: run-aware working_dir resolution + inheritance ---

// minimalWorkflowSpecYAML is a schema-valid inline workflow spec (empty
// workflows map) that lets start_run's local specValidate pass without a disk
// walk — the tool's admission control and wire mapping are what these tests
// exercise, not spec content.
func minimalWorkflowSpecYAML(_ *testing.T) string {
	return "version: \"0.3\"\n" +
		"workflows:\n" +
		"  feature_change:\n" +
		"    stages:\n" +
		"      - id: implement\n" +
		"        type: implement\n" +
		"        executor:\n" +
		"          agent: claude-code\n" +
		"        produces:\n" +
		"          - artifact: pull_request\n"
}

// seedRunWorkingDir seeds a run served by GET /v0/runs/{id} carrying a bound
// working_dir, so resolveWorkingDirForRun and the inheriting verbs read it.
func seedRunWorkingDir(fb *fakeBackend, runID uuid.UUID, wd string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y", WorkingDir: wd}
}

// seedRunShape seeds a run served by GET /v0/runs/{id} carrying an arbitrary
// decomposed_from / runner_kind / working_dir combination, so the rung-3
// refusal (E48.100 / #2547) can be driven through the fakeBackend's real JSON
// round-trip. Bad state is written BY CONSTRUCTION (an empty WorkingDir with a
// non-nil DecomposedFrom), never produced by calling the control.
func seedRunShape(fb *fakeBackend, runID uuid.UUID, decomposedFrom *string, runnerKind, wd string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[runID] = Run{
		ID:             runID.String(),
		State:          "running",
		Repo:           "x/y",
		DecomposedFrom: decomposedFrom,
		RunnerKind:     runnerKind,
		WorkingDir:     wd,
	}
}

// TestResolveWorkingDirForRun_UnboundLocalDecompositionChild is the rung-3
// refusal table (E48.100 / #2547). Every mode asserts the observable outcome —
// the returned path or the error text — not merely error-vs-nil:
//
//	A REFUSE  unbound local decomposition child, omitted input, stdio.
//	B ACCEPT  the same child WITH a binding inherits it.
//	C ACCEPT  the same unbound child with an EXPLICIT absolute input.
//	D ACCEPT  a NON-child unbound run on stdio still resolves the process cwd.
//	E ACCEPT  an unbound github_actions decomposition child falls through.
//	F ACCEPT  an unbound EMPTY-runner_kind decomposition child falls through —
//	          the exact residual the mcpserver README singles out.
//
// Mode A is the counterfactual vehicle and runs on the STDIO transport, where
// deleting the guard changes the outcome from an error to a cwd resolution
// (over HTTP the #2479 rule would refuse anyway, making the deletion invisible).
// Modes B–F staying green under that deletion proves the test is not passing by
// refusing everything.
//
// Mode F is the counterfactual vehicle for the NARROWNESS of the key: the README
// states the residual is a legacy row that is genuinely local but carries an
// empty `runner_kind`, and mode E pins only the "github_actions" spelling.
// Widening the comparison so an empty runner_kind reads as local turns F red
// while A stays green.
func TestResolveWorkingDirForRun_UnboundLocalDecompositionChild(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	parentID := uuid.New().String()
	bound := t.TempDir()
	explicit := t.TempDir()

	t.Run("A_unbound_local_child_refused", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "local", "")

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err == nil {
			t.Fatalf("expected a refusal for an unbound local decomposition child, got %q", got)
		}
		if got == cwd {
			t.Errorf("resolved to the process cwd %q; the refusal must fire instead", cwd)
		}
		if !strings.Contains(err.Error(), parentID) {
			t.Errorf("error should name the parent run id %s; got %v", parentID, err)
		}
		if !strings.Contains(err.Error(), "working_dir") {
			t.Errorf("error should name working_dir; got %v", err)
		}
	})

	t.Run("B_bound_local_child_inherits", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "local", bound)

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("a bound child must inherit, got error %v", err)
		}
		if got != bound {
			t.Errorf("resolved = %q, want the inherited binding %q", got, bound)
		}
	})

	t.Run("C_unbound_local_child_explicit_input_accepted", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "local", "")

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, explicit)
		if err != nil {
			t.Fatalf("the explicit-path workaround on a legacy child must survive, got error %v", err)
		}
		if got != explicit {
			t.Errorf("resolved = %q, want the explicit input %q", got, explicit)
		}
	})

	t.Run("D_non_child_unbound_falls_through_to_cwd", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, nil, "local", "")

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("a non-child unbound run must still resolve the process cwd (#2479 untouched), got error %v", err)
		}
		if got != cwd {
			t.Errorf("resolved = %q, want the process cwd %q", got, cwd)
		}
	})

	t.Run("E_unbound_github_actions_child_falls_through", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "github_actions", "")

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("a github_actions child has no local checkout to bind; must fall through, got error %v", err)
		}
		if got != cwd {
			t.Errorf("resolved = %q, want the process cwd %q", got, cwd)
		}
	})

	t.Run("F_unbound_empty_runner_kind_child_falls_through", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = false
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "", "")

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("an empty runner_kind resolves to github_actions, which has no local checkout; the documented residual is that such a child falls through, got error %v", err)
		}
		if got != cwd {
			t.Errorf("resolved = %q, want the process cwd %q", got, cwd)
		}
	})
}

// TestResolveWorkingDirForRun_UnreadableDecompositionChild_TransportDivergence
// pins the two transports' divergence SIDE BY SIDE on one seeded fault
// (E48.100 / #2547 review): the run row IS an unbound local decomposition child
// — the exact shape rung 3 refuses — but GET /v0/runs/{id} answers 500, so
// resolveWorkingDirForRun cannot classify it. On stdio that degrades to the
// process cwd and does NOT refuse (you cannot classify a run you could not read,
// and refusing on any read error would be a broad tightening); over HTTP the
// Condition-1 fail-closed branch still refuses. Pinning both against the SAME
// seeded row makes the asymmetry deliberate on its face rather than an oversight.
//
// The stdio half is the counterfactual vehicle for the nil-runObj carve-out in
// refuseUnboundLocalDecompositionChild: making that guard refuse on a nil
// runObj turns this half red while the rung-3 table above stays green.
func TestResolveWorkingDirForRun_UnreadableDecompositionChild_TransportDivergence(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	parentID := uuid.New().String()

	// seed writes the unbound-local-child row AND the 500 override, so the row
	// is genuinely the refusable shape and is genuinely unreadable — bad state
	// by construction, never produced by calling the control.
	seed := func(t *testing.T, httpTransport bool) (*runResolver, uuid.UUID) {
		t.Helper()
		fb, srv := newFakeBackend(t) // a REACHABLE in-test server, not a dead address
		r := newResolver(srv, nil)
		r.httpTransport = httpTransport
		runID := uuid.New()
		seedRunShape(fb, runID, &parentID, "local", "")
		fb.mu.Lock()
		fb.getStatusByID[runID] = http.StatusInternalServerError
		fb.mu.Unlock()
		return r, runID
	}

	t.Run("stdio_degrades_to_cwd", func(t *testing.T) {
		r, runID := seed(t, false)

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err != nil {
			t.Fatalf("an unreadable run on stdio must degrade to the process cwd, not refuse; got error %v", err)
		}
		if got != cwd {
			t.Errorf("stdio degrade = %q, want the process cwd %q", got, cwd)
		}
	})

	t.Run("http_still_fails_closed", func(t *testing.T) {
		r, runID := seed(t, true)

		got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
		if err == nil {
			t.Fatalf("an unreadable run over HTTP must fail closed (Condition 1 / #2482), got %q", got)
		}
		if got == cwd {
			t.Errorf("returned the process cwd %q over HTTP; must fail closed", cwd)
		}
	})
}

// decomposedPlanForWorkingDirE2E renders a schema-valid standard_v1 plan whose
// decomposition block carries two scoped sub_plans, so a real Advance fans the
// seeded parent out into two child runs.
func decomposedPlanForWorkingDirE2E(t *testing.T) []byte {
	t.Helper()
	sub := func(title, file string) map[string]any {
		return map[string]any{
			"title":                        title,
			"scope_hint":                   "scope hint for " + title,
			"predicted_runtime_minutes":    10,
			"predicted_runtime_confidence": "high",
			"scope": map[string]any{
				"files": []map[string]any{{"path": file, "operation": "create"}},
			},
		}
	}
	body := map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue",
			"url":  "https://github.com/example/repo/issues/1",
			"id":   "example/repo#1",
		},
		"generated_by": map[string]any{
			"agent":     "claude-code",
			"model":     "claude-opus-4-7",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		},
		"summary":                      "test plan with decomposition",
		"scope":                        map[string]any{"files": []map[string]any{{"path": "x.go", "operation": "create"}}},
		"approach":                     []map[string]any{{"step": 1, "description": "do it"}},
		"verification":                 map[string]any{"test_strategy": "run tests", "rollback_plan": "revert"},
		"predicted_runtime_minutes":    100,
		"predicted_runtime_confidence": "medium",
		"decomposition": map[string]any{
			"rationale": "test decomposition rationale",
			"sub_plans": []map[string]any{sub("Part A", "a.go"), sub("Part B", "b.go")},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return b
}

// TestResolveWorkingDirForRun_DecomposedChild_E2E is the FULL cross-boundary
// span the operator's binding condition requires (E48.100 / #2547): the
// orchestrator mints decomposition children from a parent carrying a
// working_dir binding, the children land in a REAL Postgres, the REAL
// server.Handler() serves one of those persisted child rows over
// GET /v0/runs/{id}, and the REAL MCP api client decodes it into the resolver
// — which, with the explicit input OMITTED, must resolve the parent's exact
// working_dir. Neither half alone proves this: the orchestrator integration
// test stops at the persisted rows, and the resolver's other tests run against
// fake-seeded rows, so a decode mismatch between the persisted row and the
// resolver's view would pass both. The unbound parent is the paired control:
// the same span with no binding mints the legacy null-bound local child, and
// the rung-3 refusal fires on it — so the bound case cannot pass by resolving
// everything to the same path, and the refusal is proven against a REAL
// persisted row rather than only a fake-seeded one.
//
// It lives in package mcpserver because resolveWorkingDirForRun is unexported
// and backend/internal/integration/orchestrator is an EXTERNAL test package
// (orchestratore2e_test), so the resolver cannot be reached from there. Driving
// a real server.New over pgtest from this package is the established idiom
// (TestStartCampaign_NoEpic_E2E_ThroughRealServer).
func TestResolveWorkingDirForRun_DecomposedChild_E2E(t *testing.T) {
	bound := t.TempDir()
	cases := []struct {
		name        string
		binding     string
		wantRefusal bool
	}{
		{"bound_parent_child_resolves_parent_dir", bound, false},
		{"unbound_parent_child_refused", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			ctx := context.Background()

			runRepo := runpkg.NewPostgresRepository(pool)
			artifactRepo := artifact.NewPostgresRepository(pool)
			auditRepo := audit.NewPostgresRepository(pool)

			// (1) A parent run carrying the binding, with a succeeded plan
			// stage and a decomposed plan artifact.
			parent, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
				Repo:          "kuhlman-labs/fishhawk",
				WorkflowID:    "feature_change",
				WorkflowSHA:   "deadbeef",
				TriggerSource: runpkg.TriggerCLI,
				RunnerKind:    runpkg.RunnerKindLocal,
				WorkingDir:    tc.binding,
			})
			if err != nil {
				t.Fatalf("CreateRun parent: %v", err)
			}
			planStage, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
				RunID: parent.ID, Sequence: 0, Type: runpkg.StageTypePlan,
				ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
			})
			if err != nil {
				t.Fatalf("CreateStage plan: %v", err)
			}
			if _, err := runRepo.CreateStage(ctx, runpkg.CreateStageParams{
				RunID: parent.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
				ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
			}); err != nil {
				t.Fatalf("CreateStage implement: %v", err)
			}
			for _, to := range []runpkg.StageState{
				runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateSucceeded,
			} {
				if _, err := runRepo.TransitionStage(ctx, planStage.ID, to, nil); err != nil {
					t.Fatalf("TransitionStage plan to %s: %v", to, err)
				}
			}
			planBytes := decomposedPlanForWorkingDirE2E(t)
			sum := sha256.Sum256(planBytes)
			schemaV := "standard_v1"
			if _, err := artifactRepo.Create(ctx, artifact.CreateParams{
				StageID:       planStage.ID,
				Kind:          artifact.KindPlan,
				SchemaVersion: &schemaV,
				Content:       planBytes,
				ContentHash:   hex.EncodeToString(sum[:]),
			}); err != nil {
				t.Fatalf("Create plan artifact: %v", err)
			}

			// (2) Real mint through the orchestrator into real Postgres.
			o := &orchestrator.Orchestrator{
				Runs: runRepo, Artifacts: artifactRepo, Audit: auditRepo, Logger: slog.Default(),
			}
			outcome, err := o.Advance(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if outcome != orchestrator.OutcomeDecomposed {
				t.Fatalf("Advance outcome = %q, want %q", outcome, orchestrator.OutcomeDecomposed)
			}
			children, err := runRepo.ListRuns(ctx, runpkg.ListRunsFilter{DecomposedFrom: &parent.ID, Limit: 100})
			if err != nil {
				t.Fatalf("ListRuns children: %v", err)
			}
			if len(children) != 2 {
				t.Fatalf("children = %d, want 2", len(children))
			}
			for i, child := range children {
				if child.WorkingDir != tc.binding {
					t.Fatalf("child %d working_dir read back from Postgres = %q, want %q", i, child.WorkingDir, tc.binding)
				}
			}

			// (3) The REAL HTTP surface over those persisted rows, decoded by
			// the REAL MCP api client into the resolver.
			srv := server.New(server.Config{RunRepo: runRepo, AuditRepo: auditRepo})
			httpSrv := httptest.NewServer(srv.Handler())
			t.Cleanup(httpSrv.Close)
			r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL})}

			got, err := r.resolveWorkingDirForRun(ctx, children[0].ID, "")
			if tc.wantRefusal {
				if err == nil {
					t.Fatalf("expected the rung-3 refusal for a real persisted unbound local child, got %q", got)
				}
				if !strings.Contains(err.Error(), parent.ID.String()) {
					t.Errorf("refusal should name the parent run id %s; got %v", parent.ID, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWorkingDirForRun on a real persisted child: %v", err)
			}
			if got != tc.binding {
				t.Errorf("resolved working_dir = %q, want the parent's binding %q", got, tc.binding)
			}
		})
	}
}

// TestResolveWorkingDirForRun_RelativeBindingRefused (C2): a run bound to the
// literal relative path "./sub" is refused on an omitted-parameter call over
// HTTP with the same must-be-absolute error an explicit "./sub" produces —
// inheritance is validated identically to an explicit value. Deleting the
// feed-the-binding-through-resolveWorkingDir step (returning the binding
// verbatim) turns it red.
func TestResolveWorkingDirForRun_RelativeBindingRefused(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true

	runID := uuid.New()
	seedRunWorkingDir(fb, runID, "./sub") // a relative binding, seeded by construction

	got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
	if err == nil {
		t.Fatalf("expected a refusal for a relative binding, got %q", got)
	}
	if !strings.Contains(err.Error(), "working_dir") || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should be the must-be-absolute error naming working_dir; got %v", err)
	}
}

// TestResolveWorkingDirForRun_ConflictingOverrideRefused (C3): a run bound to
// absolute dir A refuses an explicit absolute dir B (a SECOND, independently
// created temp dir — definitionally different, not derived from A) naming both
// paths; sibling cases pair the binding WITH ITSELF and with its own un-cleaned
// `A/./` binding form and assert ACCEPT, so the test cannot pass by refusing
// everything and the filepath.Clean normalization is proven real. Deleting the
// conflict check turns the refuse case red; deleting the Clean turns the
// un-cleaned-binding accept case red.
func TestResolveWorkingDirForRun_ConflictingOverrideRefused(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir() // independently created — definitionally != a

	cases := []struct {
		name        string
		binding     string
		input       string
		wantRefused bool
	}{
		{"conflict_B_vs_A", a, b, true},
		{"self_paired_accept", a, a, false},
		{"uncleaned_binding_accept", a + "/./", a, false},
		{"trailing_slash_input_accept", a, a + "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			r.httpTransport = true
			runID := uuid.New()
			seedRunWorkingDir(fb, runID, tc.binding)

			got, err := r.resolveWorkingDirForRun(context.Background(), runID, tc.input)
			if tc.wantRefused {
				if err == nil {
					t.Fatalf("expected a conflict refusal, got %q", got)
				}
				// Names BOTH paths.
				if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), b) {
					t.Errorf("conflict error should name both paths (%q and %q); got %v", a, b, err)
				}
				// Names the symlink caveat (Condition 3b).
				if !strings.Contains(err.Error(), "symlink") {
					t.Errorf("conflict error should state paths are compared without resolving symlinks; got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected accept, got error %v", err)
				}
				if filepath.Clean(got) != filepath.Clean(a) {
					t.Errorf("accepted path = %q, want (cleaned) %q", got, a)
				}
			}
		})
	}
}

// TestResolveWorkingDirForRun_RunReadFailureFailsClosed (C4, strengthened by
// Condition 1 / #2482): the client points at a REACHABLE in-test server
// returning 500 for GET /v0/runs/{id} (so the deletion cannot fail for the same
// reason the control would), over HTTP, and asserts a refusal REGARDLESS of
// whether an explicit value was supplied. The EXPLICIT-input case is the
// non-vacuous counterfactual vehicle: an omitted value would be refused anyway
// by the downstream #2479 rule, but an explicit absolute path is self-validating
// and — WITHOUT the fail-closed branch — would be accepted despite an unknown
// (possibly conflicting) binding. Deleting the fail-closed branch turns the
// explicit case red.
func TestResolveWorkingDirForRun_RunReadFailureFailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true
	runID := uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError // reachable server, 500
	fb.mu.Unlock()

	cwd, _ := os.Getwd()

	// Explicit absolute input — the Condition-1 case. Self-validating, so only
	// the fail-closed branch refuses it under an unreadable run.
	explicit := t.TempDir()
	if got, err := r.resolveWorkingDirForRun(context.Background(), runID, explicit); err == nil {
		t.Errorf("explicit input over HTTP with an unreadable run must fail closed, got %q", got)
	}

	// Omitted input also fails closed (and is NOT the process cwd).
	got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
	if err == nil {
		t.Fatalf("omitted input over HTTP with an unreadable run must fail closed, got %q", got)
	}
	if got == cwd {
		t.Errorf("returned the process cwd %q on a read failure over HTTP; must fail closed", cwd)
	}
}

// TestResolveWorkingDirForRun_RunReadFailureStdioDegradesToCwd is C4's stdio
// sibling: the SAME 500 on stdio degrades to the documented process-cwd
// carve-out, so the two branches are pinned independently.
func TestResolveWorkingDirForRun_RunReadFailureStdioDegradesToCwd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil) // stdio (httpTransport:false)
	runID := uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError
	fb.mu.Unlock()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	got, err := r.resolveWorkingDirForRun(context.Background(), runID, "")
	if err != nil {
		t.Fatalf("stdio should degrade to cwd on a read failure, got error %v", err)
	}
	if got != cwd {
		t.Errorf("stdio degrade = %q, want the process cwd %q", got, cwd)
	}
}

// TestResolveWorkingDirForRun_RunReadFailureStdioExplicitUsedVerbatim pins the
// OTHER stdio degrade branch the omitted-input sibling above does not cover
// (#2482, concern 5): when GetRun fails on stdio, an EXPLICIT absolute input is
// used verbatim (self-validating, no conflict check against the unreadable
// binding) rather than refused. This is the deliberate stdio carve-out that the
// HTTP transport does NOT get — over HTTP the same explicit case fails closed,
// pinned by TestResolveWorkingDirForRun_RunReadFailureFailsClosed.
func TestResolveWorkingDirForRun_RunReadFailureStdioExplicitUsedVerbatim(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil) // stdio (httpTransport:false)
	runID := uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError
	fb.mu.Unlock()

	explicit := t.TempDir() // absolute
	got, err := r.resolveWorkingDirForRun(context.Background(), runID, explicit)
	if err != nil {
		t.Fatalf("stdio explicit input should be used verbatim on a read failure, got error %v", err)
	}
	if got != explicit {
		t.Errorf("stdio explicit degrade = %q, want the explicit input %q verbatim", got, explicit)
	}
}

// TestStartRun_HTTPLocalRunnerRequiresWorkingDir (C5): an HTTP-transport
// start_run with runner_kind=local and no working_dir is refused AND NO POST
// /v0/runs reached the backend (a never-dialed seam — error identity alone
// would not distinguish a refusal from a backend rejection). A paired
// runner_kind=github_actions case asserts it still SUCCEEDS so the control
// cannot pass by refusing everything. Deleting the admission check turns the
// local case red.
func TestStartRun_HTTPLocalRunnerRequiresWorkingDir(t *testing.T) {
	inlineSpec := minimalWorkflowSpecYAML(t)

	t.Run("local_no_working_dir_refused_never_dialed", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = true

		_, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			RunnerKind: "local", WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
			// working_dir omitted.
		})
		if err == nil {
			t.Fatal("expected a refusal for a local HTTP start_run with no working_dir")
		}
		if !strings.Contains(err.Error(), "working_dir") {
			t.Errorf("error should name working_dir; got %v", err)
		}
		// Never-dialed seam: no POST /v0/runs reached the backend.
		fb.mu.Lock()
		body := fb.createRunBody
		fb.mu.Unlock()
		if body.Repo != "" {
			t.Errorf("POST /v0/runs was dialed despite the refusal (createRunBody=%+v)", body)
		}
	})

	t.Run("github_actions_no_working_dir_succeeds", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = true

		_, out, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			RunnerKind: "github_actions", WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
			// working_dir omitted — legitimate for a github_actions run.
		})
		if err != nil {
			t.Fatalf("a github_actions HTTP start_run with no working_dir must succeed, got %v", err)
		}
		if out.Run.ID == "" {
			t.Error("expected a created run id")
		}
	})
}

// TestStartRun_HTTPRelativeWorkingDirRefusedAnyRunnerKind is the security
// control for the daemon-cwd shape (#2479 / #2482): over the HTTP transport a
// RELATIVE working_dir is refused for a NON-local runner_kind too, because the
// start-time runner_kind is only a hint (#1346) and filepath.Abs would
// otherwise resolve the relative path against the fishhawkd daemon's own cwd
// and persist it as the run's absolute binding — inheritable by a later local
// dispatch. The refuse case asserts a never-dialed seam (no POST /v0/runs), so
// error identity alone cannot pass it; the paired absolute-input case asserts a
// SUCCESS with the value passed through unchanged (not daemon-cwd-joined), so
// the control cannot pass by refusing everything. Deleting the generalized
// relative-refusal turns the refuse case red (the relative value is
// absolutized and POSTed instead of refused).
func TestStartRun_HTTPRelativeWorkingDirRefusedAnyRunnerKind(t *testing.T) {
	inlineSpec := minimalWorkflowSpecYAML(t)

	t.Run("github_actions_relative_refused_never_dialed", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = true

		_, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			RunnerKind: "github_actions", WorkingDir: "./sub",
			WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
		})
		if err == nil {
			t.Fatal("expected a refusal for a relative working_dir over HTTP with runner_kind=github_actions")
		}
		if !strings.Contains(err.Error(), "working_dir") || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("error should name working_dir and require an absolute path; got %v", err)
		}
		// Never-dialed seam: no POST /v0/runs reached the backend, so the
		// relative value was never absolutized-against-daemon-cwd and persisted.
		fb.mu.Lock()
		body := fb.createRunBody
		fb.mu.Unlock()
		if body.Repo != "" {
			t.Errorf("POST /v0/runs was dialed despite the refusal (createRunBody=%+v)", body)
		}
	})

	t.Run("omitted_runner_kind_relative_refused", func(t *testing.T) {
		// runner_kind omitted defaults to github_actions on the backend, so an
		// HTTP relative value on the default path must be refused too.
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = true

		_, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			WorkingDir: "./sub", WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
		})
		if err == nil {
			t.Fatal("expected a refusal for a relative working_dir over HTTP with runner_kind omitted")
		}
		fb.mu.Lock()
		body := fb.createRunBody
		fb.mu.Unlock()
		if body.Repo != "" {
			t.Errorf("POST /v0/runs was dialed despite the refusal (createRunBody=%+v)", body)
		}
	})

	t.Run("github_actions_absolute_succeeds_passthrough", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		r.httpTransport = true

		wd := t.TempDir() // absolute
		_, out, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			RunnerKind: "github_actions", WorkingDir: wd,
			WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
		})
		if err != nil {
			t.Fatalf("an absolute working_dir over HTTP with runner_kind=github_actions must succeed, got %v", err)
		}
		if out.Run.ID == "" {
			t.Error("expected a created run id")
		}
		// The absolute value is passed through unchanged (cleaned, not
		// daemon-cwd-joined).
		fb.mu.Lock()
		sent := fb.createRunBody.WorkingDir
		fb.mu.Unlock()
		if sent != wd {
			t.Errorf("POST /v0/runs body working_dir = %q, want %q unchanged", sent, wd)
		}
	})
}

// TestStartRun_WorkingDirRoundTripsThroughToolSurface (Condition 2 / #2482) is
// the end-to-end SUCCESS test: it drives start_run over the tool surface with a
// bound absolute path, then reads the binding back through a DISTINCT GET
// /v0/runs/{id} round-trip — the fake persisted it on create, so the readback
// goes through store-and-reload, NOT this call's own POST response echo. That
// makes the assertion non-vacuous: it fails if persistence is disconnected (the
// create never stored the value) OR if readback is disconnected (the client GET
// mirror drops the json tag), the #371-class hand-maintained wire-mirror trap
// this run's step 5 names. The full persist→Postgres→read half of the chain is
// pinned independently by run.TestCreateRun_PersistsWorkingDir (real pgtest)
// and server.TestCreateRun_WorkingDirRoundTripsOverHTTP; this test covers the
// MCP tool → client JSON request → HTTP handler → readback seam above them.
func TestStartRun_WorkingDirRoundTripsThroughToolSurface(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	r.httpTransport = true
	inlineSpec := minimalWorkflowSpecYAML(t)

	wd := t.TempDir() // absolute
	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		RunnerKind: "local", WorkingDir: wd, WorkflowSpec: inlineSpec, WorkflowSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	// The client's JSON request carried working_dir to the backend (the
	// tool→client serialization half).
	fb.mu.Lock()
	sent := fb.createRunBody.WorkingDir
	fb.mu.Unlock()
	if sent != wd {
		t.Errorf("POST /v0/runs body working_dir = %q, want %q (tool→client mapping)", sent, wd)
	}

	// Read the binding back through a genuine GET /v0/runs/{id} — the persisted
	// value the fake stored on create, decoded by the client Run mirror. This
	// is NOT out.Run (the POST response, which deliberately omits working_dir):
	// a create that failed to persist, or a GET mirror that drops the tag,
	// fails here.
	runID, perr := uuid.Parse(out.Run.ID)
	if perr != nil {
		t.Fatalf("start_run returned a non-UUID run id %q: %v", out.Run.ID, perr)
	}
	got, err := r.api.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun readback: %v", err)
	}
	if got.WorkingDir != wd {
		t.Errorf("GetRun readback WorkingDir = %q, want %q (persist→readback through the client mirror)", got.WorkingDir, wd)
	}
}

func sampleRun(id uuid.UUID, repo string, age time.Duration) Run {
	pr := "https://github.com/" + repo + "/pull/42"
	tr := "issue:42"
	return Run{
		ID: id.String(), Repo: repo, WorkflowID: "feature_change",
		TriggerSource:  "github_issue",
		TriggerRef:     &tr,
		State:          "running",
		PullRequestURL: &pr,
		CreatedAt:      time.Now().UTC().Add(-age),
		UpdatedAt:      time.Now().UTC().Add(-age),
	}
}

func TestGetActiveRun_ByPRNumber_QueriesPullRequestURL(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	// Verify the filter actually hit the backend.
	for _, want := range []string{
		"repo=x%2Fy",
		"pull_request_url=https%3A%2F%2Fgithub.com%2Fx%2Fy%2Fpull%2F42",
	} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByPRNumber_RequiresRepo(t *testing.T) {
	// pr_number set, repo missing, GITHUB_REPOSITORY unset → the
	// tool can't build the canonical pull_request_url. Surface a
	// clean error rather than silently scoping the search to all
	// installations.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected error when repo and GITHUB_REPOSITORY are both unset")
	}
	if !strings.Contains(err.Error(), "repo required") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ByPRNumber_FallsBackToGitHubRepositoryEnv(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
}

func TestGetActiveRun_ByTriggerRef_QueriesTriggerRefFilter(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	for _, want := range []string{"repo=x%2Fy", "trigger_ref=issue%3A42"} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByEnvRunID_DirectFetch(t *testing.T) {
	// The runner case: FISHHAWK_RUN_ID stamped on the env →
	// fetch the run directly without a list scan.
	fb, srv := newFakeBackend(t)
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	fb.getResp = sampleRun(id, "x/y", time.Hour)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": id.String()})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
}

func TestGetActiveRun_ByEnvRunID_RejectsInvalidUUID(t *testing.T) {
	// Defensive: if the runner stamps a malformed env, surface a
	// clear error rather than a generic 4xx from the GET path.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": "not-a-uuid"})

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error on malformed FISHHAWK_RUN_ID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_NoResolutionPath_ReturnsStructuredError(t *testing.T) {
	// No pr_number, no trigger_ref, no FISHHAWK_RUN_ID. The error
	// message must list every input the caller could supply so an
	// agent reading it can ask the human for the missing piece.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error when no resolution path is available")
	}
	for _, want := range []string{"pr_number", "trigger_ref", "FISHHAWK_RUN_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q as an option: %v", want, err)
		}
	}
}

func TestGetActiveRun_PRNumber_NoMatchingRun(t *testing.T) {
	// Empty list response → friendly error naming the repo + PR
	// number so the caller knows the lookup itself worked but
	// nothing matched.
	fb, srv := newFakeBackend(t)
	fb.listResp = listRunsResult{Items: nil}
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "x/y") || !strings.Contains(err.Error(), "pull/42") {
		t.Errorf("error should name the repo + PR: %v", err)
	}
}

func TestGetActiveRun_PicksMostRecentByCreatedAt(t *testing.T) {
	// Two runs on the same PR (e.g., a retry chain). The resolver
	// returns the newer one. Defensive sort — even if the
	// backend ever stops ordering, we still pick correctly.
	fb, srv := newFakeBackend(t)
	older := uuid.New()
	newer := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{
		sampleRun(older, "x/y", 24*time.Hour),
		sampleRun(newer, "x/y", time.Hour),
	}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != newer.String() {
		t.Errorf("Run.ID = %s, want newer %s", out.Run.ID, newer.String())
	}
}

func TestGetActiveRun_BackendError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.listStatus = http.StatusInternalServerError
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected backend error")
	}
	// Both wrapped error and the underlying *apiError reach the
	// caller; just verify the surface message is helpful.
	if !strings.Contains(err.Error(), "list runs") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ResolutionOrder_PRNumberBeatsTriggerRef(t *testing.T) {
	// Both pr_number and trigger_ref provided — the spec's
	// resolution order says pr_number wins. Verify the trigger_ref
	// branch isn't even consulted (it would otherwise hit the
	// backend with a different query).
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:       "x/y",
		PRNumber:   42,
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	if !strings.Contains(fb.lastListQuery, "pull_request_url=") {
		t.Errorf("expected pull_request_url filter (pr_number wins); got %s", fb.lastListQuery)
	}
	if strings.Contains(fb.lastListQuery, "trigger_ref=") {
		t.Errorf("trigger_ref filter should not have been used: %s", fb.lastListQuery)
	}
}

func TestRegisterTools_RegistersGetActiveRun(t *testing.T) {
	// Smoke test: registerTools doesn't panic and the SDK accepts
	// the tool definition. Full handshake verification lives in
	// the SDK; we just assert the registration call sequence
	// completes for v0's tool set.
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{
		api:    newAPIClient(cfg),
		getenv: envFuncFromMap(nil),
	}
	registerTools(srv, resolver)
}

// TestToolDescriptions_ConformToHouseStyle is the #778 guardrail: it
// enumerates the FULL registered tool set over an in-memory MCP ListTools
// session (the same path a real client sees) and asserts every tool's
// description meets the structural bar — non-empty, above a minimum length
// FLOOR (a stub/empty-description catch, NOT a target to pad toward), and
// leading with a when/eligibility trigger token so the description tells the
// driving agent WHEN to reach for the tool. Adding a tool without a
// conformant description fails this test.
func TestToolDescriptions_ConformToHouseStyle(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{
		api:    newAPIClient(cfg),
		getenv: envFuncFromMap(nil),
	}
	registerTools(srv, resolver)

	// Drive the server's tool list through a real ListTools round-trip over
	// an in-memory transport, so the assertions run against the wire-visible
	// descriptions rather than the in-process registration structs.
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// when/eligibility trigger tokens (case-insensitive). A conformant
	// description leads with one so the agent reads WHEN to use the tool.
	triggerTokens := []string{"use this when", "when ", "after ", "once ", "before "}
	// Minimum description length is a FLOOR that catches an empty/stub
	// description, not a target — the #778 density guard wants dense, not
	// padded, prose.
	const minDescriptionLen = 80
	// The registered tool set is the fishhawk_* tools swept in #778. Bump
	// this and give the new tool a conformant description when adding one.
	// This count is the surface-sweep invariant (#873/#867). E33.5 (#1590,
	// ADR-051) adds exactly ONE tool — fishhawk_release_notes, the release
	// prepare/preview pair — taking the total 40 -> 41; the release cut/publish
	// verbs are CLI-only (next_actions names them at the release-loop states),
	// so the MCP surface grows by AT MOST one per the binding approval condition.
	//
	// E22.X (#1700, ADR-040/ADR-047) adds exactly ONE tool —
	// fishhawk_drive_run, the local auto-driver — taking the total 41 -> 42.
	//
	// #1915 adds exactly ONE tool — fishhawk_revive_run, the one-verb
	// failed-run revive — taking the total 42 -> 43.
	//
	// E48.7 (#1954) adds exactly ONE tool — fishhawk_merge_run, the one-verb
	// operator verdict + queue-merge + await-terminal verb that replaces the
	// bare merge_pr + post_merge hand ceremony — taking the total 43 -> 44.
	//
	// E48.13 (#1960) adds exactly ONE tool — fishhawk_get_gate_view, the
	// one-call review-gate decision read (open concerns with full notes, round
	// history, fix-up claims, re-review confirmations) — taking the total
	// 44 -> 45.
	//
	// E66.37 (#2474) adds exactly ONE tool — fishhawk_arbitrate_acceptance, the
	// operator-only discharge of a PAGED acceptance triage that un-wedges the
	// merge gate — taking the total 45 -> 46.
	//
	// E48.64 (#2491) adds exactly ONE tool — fishhawk_await_stage, the terminal
	// wait on the durable (run_id, stage_id) handle fishhawk_dispatch_stage
	// returns — taking the total 46 -> 47.
	//
	// E25.20 (#2355) adds exactly ONE tool — fishhawk_cancel_campaign, the
	// operator clean-shutdown of an abandoned/rebuilt campaign that marks it and
	// its unfinished items cancelled — taking the total 47 -> 48.
	//
	// E67.47 (#2689) adds exactly ONE tool — fishhawk_reap_stage, the
	// operator-reachable reap of a stage stranded in dispatched/running that
	// retry_stage / revive_run / fixup_stage / dispatch_stage all refuse —
	// taking the total 49 -> 50.
	//
	// E67.55 (#2712) adds exactly ONE tool — fishhawk_reconcile_reviews, the
	// on-demand recovery for a review round orphaned by a fishhawkd restart
	// (the boot sweep's operator-reachable twin) — taking the total 50 -> 51.
	// E54.30 (#2843) adds exactly ONE tool —
	// fishhawk_record_grooming_dispositions, the operator-only CAPTURE of
	// per-entry grooming verdicts as auditable facts (consumed by nothing until
	// #2991) — taking the total 51 -> 52.
	const wantToolCount = 52

	if len(res.Tools) != wantToolCount {
		t.Errorf("registered tool count = %d, want %d (a new tool must be added here with a when/eligibility-leading description)",
			len(res.Tools), wantToolCount)
	}

	// fishhawk_consolidate_slices (#1238) must be wire-visible — a registration
	// regression would otherwise drop it from the tool list without failing the
	// count if another tool were added in the same change.
	var sawConsolidate bool
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_consolidate_slices" {
			sawConsolidate = true
			break
		}
	}
	if !sawConsolidate {
		t.Error("fishhawk_consolidate_slices is not registered/visible over ListTools")
	}

	// fishhawk_get_gate_view (#1960) must be wire-visible — a registration
	// regression would otherwise drop the one-call gate decision read without
	// tripping the count if another tool were added in the same change.
	var sawGateView bool
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_get_gate_view" {
			sawGateView = true
			break
		}
	}
	if !sawGateView {
		t.Error("fishhawk_get_gate_view is not registered/visible over ListTools")
	}

	// fishhawk_record_grooming_dispositions (#2843) must be wire-visible — a
	// registration regression would otherwise drop the operator-only grooming
	// capture verb without tripping the count if another tool were added in the
	// same change.
	var sawGroomingDispositions bool
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_record_grooming_dispositions" {
			sawGroomingDispositions = true
			break
		}
	}
	if !sawGroomingDispositions {
		t.Error("fishhawk_record_grooming_dispositions is not registered/visible over ListTools")
	}

	// fishhawk_reconcile_reviews (#2712) must be wire-visible: it is the only
	// recovery for a restart-orphaned review round, and a registration
	// regression would leave an operator whose gate degraded from two
	// reviewers to one with no verb to clear it.
	var sawReconcileReviews bool
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_reconcile_reviews" {
			sawReconcileReviews = true
			break
		}
	}
	if !sawReconcileReviews {
		t.Error("fishhawk_reconcile_reviews is not registered/visible over ListTools")
	}

	// fishhawk_arbitrate_acceptance (#2474) must be wire-visible AND its
	// description must state what it is NOT — a registration regression would
	// leave a paged operator with no blessed discharge, and a description that
	// omitted the auto-routed carve-out would invite an operator to try to skip a
	// fix-up the loop already dispatched.
	var arbitrateDesc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_arbitrate_acceptance" {
			arbitrateDesc = tool.Description
			break
		}
	}
	if arbitrateDesc == "" {
		t.Error("fishhawk_arbitrate_acceptance is not registered/visible over ListTools")
	}
	for _, want := range []string{"AUTO-ROUTED", "NOT a re-run", "NOT a pass", "acceptance_arbitrated"} {
		if !strings.Contains(arbitrateDesc, want) {
			t.Errorf("fishhawk_arbitrate_acceptance description must mention %q; got:\n%s", want, arbitrateDesc)
		}
	}

	// fishhawk_reap_stage (#2689) must be wire-visible AND its description must
	// carry the load-bearing contract: the endpoint it wraps, the verb that
	// becomes applicable afterwards, and the server-side protected-park refusal
	// that makes it safe. A description regression would ship the wrong contract
	// to the one operator who reaches for this verb — the one holding a strand.
	var reapDesc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_reap_stage" {
			reapDesc = tool.Description
			break
		}
	}
	if reapDesc == "" {
		t.Fatal("fishhawk_reap_stage is not registered/visible over ListTools")
	}
	for _, want := range []string{"fishhawk_retry_stage", "reap-failure", "protected PARK states", "fishhawk_cancel_run", "runner_kind"} {
		if !strings.Contains(reapDesc, want) {
			t.Errorf("fishhawk_reap_stage description must mention %q (#2689):\n%s", want, reapDesc)
		}
	}

	// fishhawk_revive_run (#1915) must be wire-visible AND its description must
	// document the load-bearing no-dispatch re-park semantics that distinguish
	// it from fishhawk_retry_stage — a registration or description regression
	// would otherwise ship the wrong contract.
	var reviveDesc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_revive_run" {
			reviveDesc = tool.Description
			break
		}
	}
	if reviveDesc == "" {
		t.Fatal("fishhawk_revive_run is not registered/visible over ListTools")
	}
	lowerRevive := strings.ToLower(reviveDesc)
	for _, want := range []string{"re-park", "never dispatches", "fishhawk_retry_stage"} {
		if !strings.Contains(lowerRevive, strings.ToLower(want)) {
			t.Errorf("fishhawk_revive_run description missing %q (no-dispatch re-park contract, #1915):\n%s", want, reviveDesc)
		}
	}

	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "fishhawk_") {
			t.Errorf("tool %q: name does not start with fishhawk_", tool.Name)
		}
		desc := strings.TrimSpace(tool.Description)
		if desc == "" {
			t.Errorf("tool %q: empty description", tool.Name)
			continue
		}
		if len(desc) < minDescriptionLen {
			t.Errorf("tool %q: description length %d is below the %d floor; it must state WHEN to use the tool and name sibling tools",
				tool.Name, len(desc), minDescriptionLen)
		}
		lower := strings.ToLower(desc)
		hasTrigger := false
		for _, tok := range triggerTokens {
			if strings.Contains(lower, tok) {
				hasTrigger = true
				break
			}
		}
		if !hasTrigger {
			t.Errorf("tool %q: description has no when/eligibility trigger token (one of %v); lead with WHEN to reach for the tool",
				tool.Name, triggerTokens)
		}
	}
}

// TestRetryStageDescription_DocumentsAcceptanceReopenArm pins the #1567
// description change: the fishhawk_retry_stage tool must document the second
// admitted shape — a SUCCEEDED acceptance stage with no recorded verdict
// re-opens for a re-run (operator token only) — so a driving agent knows the
// verb covers the settled-outcome-unknown recovery, not only failed stages.
func TestRetryStageDescription_DocumentsAcceptanceReopenArm(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	var desc string
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_retry_stage" {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_retry_stage not registered/visible over ListTools")
	}
	lower := strings.ToLower(desc)
	for _, want := range []string{"acceptance", "operator", "no acceptance_outcome_recorded verdict"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("fishhawk_retry_stage description missing %q (acceptance-reopen arm, #1567):\n%s", want, desc)
		}
	}
}

// TestStageWaitDescriptions_NameCurrentTasksBlocker is the #2495 done-means
// test: it asserts the WIRE-VISIBLE descriptions of fishhawk_run_stage,
// fishhawk_dispatch_stage, and fishhawk_get_run_status no longer cite the
// stale native-MCP-Tasks deferral gate (invocationMode, ADR-033, "MCP Tasks
// GA"/"leaving experimental") and instead name the real blocker — the
// experimental io.modelcontextprotocol/tasks extension (SEP-2663) is
// unimplemented in the pinned go-sdk (go-sdk#626). A comment-only or no-op
// touch of any of the three description sites leaves the stale strings in
// place and fails this test, which the scope-completeness presence gate
// cannot catch on its own (#1169).
func TestStageWaitDescriptions_NameCurrentTasksBlocker(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	descByName := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		descByName[tool.Name] = tool.Description
	}

	tasksTools := []string{"fishhawk_run_stage", "fishhawk_dispatch_stage", "fishhawk_get_run_status"}
	for _, name := range tasksTools {
		desc, ok := descByName[name]
		if !ok || desc == "" {
			t.Errorf("%s: not registered/visible over ListTools", name)
			continue
		}
		lower := strings.ToLower(desc)

		if strings.Contains(lower, "invocationmode") {
			t.Errorf("%s: description still contains %q (stale field, deleted from the current Tasks extension design):\n%s", name, "invocationMode", desc)
		}
		if strings.Contains(desc, "ADR-033") {
			t.Errorf("%s: description still contains %q (the ADR-033 transport gate is satisfied — E66/#2388 shipped MCP over HTTP):\n%s", name, "ADR-033", desc)
		}
		if strings.Contains(desc, "MCP Tasks GA") {
			t.Errorf("%s: description still contains %q (no such core-spec milestone exists — Tasks moved to a standalone extension):\n%s", name, "MCP Tasks GA", desc)
		}
		if strings.Contains(lower, "leaving experimental") {
			t.Errorf("%s: description still contains %q (stale gate phrasing):\n%s", name, "leaving experimental", desc)
		}
		if !strings.Contains(desc, "io.modelcontextprotocol/tasks") {
			t.Errorf("%s: description missing %q (the real blocker — the extension Tasks now lives in):\n%s", name, "io.modelcontextprotocol/tasks", desc)
		}
		if !strings.Contains(desc, "go-sdk#626") {
			t.Errorf("%s: description missing %q (the upstream tracking issue for Tasks support in the pinned go-sdk):\n%s", name, "go-sdk#626", desc)
		}
		if !strings.Contains(lower, "available today") {
			t.Errorf("%s: description missing an \"available today\" phrasing (operator-relevant unavailability fact, binding approval condition 1):\n%s", name, desc)
		}
	}

	// fishhawk_run_stage's negotiated-fallback framing must survive the
	// rewrite verbatim — it is the load-bearing distinction between the
	// blessed poll-fishhawk_get_run_status path and this synchronous opt-in.
	runStageDesc := descByName["fishhawk_run_stage"]
	if !strings.Contains(strings.ToLower(runStageDesc), "negotiated fallback") {
		t.Errorf("fishhawk_run_stage: description missing %q (must be preserved by the rewrite):\n%s", "negotiated fallback", runStageDesc)
	}

	// fishhawk_dispatch_stage's note must remain exactly ONE sentence
	// (binding approval condition 2). Sentence-splitting the whole
	// description is brittle; a single "MCP Tasks" occurrence is a stable
	// proxy because the note is the description's only mention of Tasks.
	dispatchDesc := descByName["fishhawk_dispatch_stage"]
	if got := strings.Count(dispatchDesc, "MCP Tasks"); got != 1 {
		t.Errorf("fishhawk_dispatch_stage: description contains %d occurrences of %q, want exactly 1 (the Tasks note must stay one sentence):\n%s", got, "MCP Tasks", dispatchDesc)
	}
}

// cursorDescriptionClaims are the SUBSTANTIVE claims the `cursor` guidance has
// to make on the wire, asserted POSITIVELY rather than as the tokens they are
// worded with. A bare `strings.Contains(desc, "opaque")` stays green when the
// description is reworded to say the cursor is NOT opaque, which makes it a
// control that cannot fail for the reason it exists (#2494).
//
// So each claim carries an `assert` pattern for its AFFIRMATIVE form — the
// terms must stand in the right relation, not merely appear — plus, where an
// affirmative form survives a negation intact ("need not be copied verbatim"
// still contains "copied verbatim"), a `reject` pattern for the claim's
// negation that must NOT match. The reviewer's reversal — "next_cursor is not
// accepted; the cursor is not opaque and need not be copied verbatim; it is
// not an offset or an index" — fails four of these six claims.
//
// Every gap is bounded and clause-bounded (`[^.;:]`), leaving the wording
// between the terms free so an accurate rewording still passes. Mirrors
// assertCursorMessageNamesAcceptedInput in the server package, which pins the
// same claims on the REST 400 message.
var cursorDescriptionClaims = []struct {
	name   string
	assert *regexp.Regexp
	reject *regexp.Regexp
}{
	{
		name:   "say that next_cursor is the ONLY accepted value",
		assert: regexp.MustCompile(`\b(only|sole|solely|exclusively)\b[^.;:]{0,60}\bnext_cursor\b`),
		reject: regexp.MustCompile(`\b(not|never)\b[^.;:]{0,30}\bnext_cursor\b`),
	},
	{
		name:   "attribute that next_cursor to a PRIOR response",
		assert: regexp.MustCompile(`\bprior\b[^.;:]{0,40}\bnext_cursor\b|\bnext_cursor\b[^.;:]{0,40}\bprior\b`),
	},
	{
		name:   "say the token IS opaque",
		assert: regexp.MustCompile(`\bopaque\b[^.;:]{0,40}\b(token|cursor|value|string)\b|\bis\s+opaque\b`),
		reject: regexp.MustCompile(`\b(not|never)\b[^.;:]{0,30}\bopaque\b|\bopaque\b[^.;:]{0,30}\b(not|never)\b`),
	},
	{
		// "verbatim" is the one claim with no stable affirmative verb to
		// anchor on — copied / taken / reused / pasted verbatim are all
		// accurate wordings — so requiring a particular one would fail an
		// honest rewrite. Here the reject half carries the weight: the term
		// must appear AND must not be negated, which is what rules out the
		// "need not be copied verbatim" reversal.
		name:   "require the value be reproduced VERBATIM",
		assert: regexp.MustCompile(`\bverbatim\b`),
		reject: regexp.MustCompile(`\b(not|never)\b[^.;:]{0,30}\bverbatim\b`),
	},
	{
		name:   "say it is NOT an offset",
		assert: regexp.MustCompile(`\bnot\b[^.;]{0,40}\boffset\b`),
	},
	{
		name:   "say it is NOT an index",
		assert: regexp.MustCompile(`\bnot\b[^.;]{0,40}\bindex\b`),
	},
}

// normalizeDescription lower-cases and collapses whitespace runs: the
// descriptions are hard-wrapped prose, so a claim spanning a line break would
// otherwise be pinned to the current wrap column rather than to the claim.
func normalizeDescription(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// TestToolDescriptions_CursorAndStageVocabulary is the #2494 wire-visibility
// test for the two documentation-shaped items: the audit-listing tool's
// `cursor` guidance and the get_run_status stage-vocabulary mapping must
// actually reach the agent through ListTools, not merely exist in a source
// comment. A comment-only touch of either site leaves the wire description
// unchanged and fails here.
//
// It adds NO tool, so the wantToolCount surface-sweep invariant above is
// deliberately untouched.
func TestToolDescriptions_CursorAndStageVocabulary(t *testing.T) {
	descByName := listToolDescriptions(t)

	// Item 2: the cursor guidance the operator actually hit, asserted on BOTH
	// wire surfaces — the tool description prose AND the `cursor` INPUT-SCHEMA
	// property, which is what an agent filling the argument reads. Asserting
	// only the prose leaves the schema free to keep the old vague text.
	auditDesc := descByName["fishhawk_list_audit"]
	if auditDesc == "" {
		t.Fatal("fishhawk_list_audit is not registered/visible over ListTools")
	}
	schemaCursorDesc := toolInputPropertyDescription(t, "fishhawk_list_audit", "cursor")
	for _, surface := range []struct{ where, text string }{
		{"fishhawk_list_audit cursor INPUT SCHEMA", schemaCursorDesc},
		{"fishhawk_list_audit description", auditDesc},
	} {
		norm := normalizeDescription(surface.text)
		for _, claim := range cursorDescriptionClaims {
			if !claim.assert.MatchString(norm) {
				t.Errorf("%s does not %s (no match for %s) (#2494):\n%s",
					surface.where, claim.name, claim.assert, surface.text)
			}
			if claim.reject != nil && claim.reject.MatchString(norm) {
				t.Errorf("%s REVERSES the claim it must %s (matched the negation %s) (#2494):\n%s",
					surface.where, claim.name, claim.reject, surface.text)
			}
		}
	}

	// Item 4: the two-vocabulary mapping on the surface that reports both.
	// EVERY documented state -> bucket pair must be on the wire, not a sample
	// of three: a spot-check of awaiting_host_dispatch/awaiting_children stays
	// green while the rest of the mapping disappears from the description. The
	// pairs come from the same documentedStageWaitMapping the classification
	// test pins the code against, so code and documentation cannot disagree.
	statusDesc := descByName["fishhawk_get_run_status"]
	if statusDesc == "" {
		t.Fatal("fishhawk_get_run_status is not registered/visible over ListTools")
	}
	statusNorm := normalizeDescription(statusDesc)
	for _, m := range documentedStageWaitMapping {
		pair := string(m.State) + " -> " + m.Bucket
		if !strings.Contains(statusNorm, pair) {
			t.Errorf("fishhawk_get_run_status description must document the stage-vocabulary mapping %q (#2494):\n%s",
				pair, statusDesc)
		}
	}
	if !strings.Contains(statusNorm, "bucket") {
		t.Errorf("fishhawk_get_run_status description must name the wait status a BUCKET (#2494):\n%s", statusDesc)
	}
	// The reviews[] one-shape contract rides the same description.
	if !strings.Contains(statusNorm, "always present") {
		t.Errorf("fishhawk_get_run_status description must state reviews[] is always present (#2494):\n%s", statusDesc)
	}
}

// toolInputPropertyDescription returns one tool input property's wire-visible
// jsonschema description, the surface an agent reads when filling an argument
// (distinct from the tool's own prose description).
func toolInputPropertyDescription(t *testing.T, toolName, property string) string {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != toolName {
			continue
		}
		if tool.InputSchema == nil {
			t.Fatalf("%s: no input schema on the wire", toolName)
		}
		// InputSchema is `any` on the wire type, so round-trip it through JSON
		// rather than type-asserting a concrete schema implementation.
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: marshal input schema: %v", toolName, err)
		}
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: decode input schema: %v", toolName, err)
		}
		prop, ok := schema.Properties[property]
		if !ok {
			t.Fatalf("%s: input schema has no %q property", toolName, property)
		}
		return prop.Description
	}
	t.Fatalf("%s is not registered/visible over ListTools", toolName)
	return ""
}

// listToolDescriptions connects an in-memory MCP session, registers the full
// tool set, and returns each tool's wire-visible ListTools description keyed by
// name. It is the shared setup the description-guidance assertions reuse.
func listToolDescriptions(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	descByName := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		descByName[tool.Name] = tool.Description
	}
	return descByName
}

// TestAwaitStage_RegisteredAndAdvertised is the #2491 done-means test: over a
// real in-memory MCP session, ListTools carries fishhawk_await_stage with a
// non-empty description and the documented input properties (run_id, stage,
// stage_id, timeout_seconds, long_wait). A comment-only or no-op touch that
// failed to register the tool fails here (the wantToolCount 46->47 bump in
// TestRegisteredToolsHaveWhenLeadingDescriptions catches the count).
func TestAwaitStage_RegisteredAndAdvertised(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "fishhawk_await_stage" {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatal("fishhawk_await_stage is not registered/visible over ListTools")
	}
	if strings.TrimSpace(tool.Description) == "" {
		t.Error("fishhawk_await_stage has an empty description")
	}
	// Over the wire the SDK decodes Tool.InputSchema into a JSON object map, so
	// mirror the existing schema-advertisement pattern (line ~2763).
	schemaMap, ok := any(tool.InputSchema).(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_await_stage InputSchema is %T, want a JSON object map", tool.InputSchema)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_await_stage schema has no properties object; got %v", schemaMap["properties"])
	}
	for _, prop := range []string{"run_id", "stage", "stage_id", "timeout_seconds", "long_wait"} {
		if _, ok := props[prop]; !ok {
			t.Errorf("fishhawk_await_stage input schema missing property %q", prop)
		}
	}
}

// TestStageWaitDescriptions_StateBackgroundingTradeoff is the #2491 done-means
// test: the WIRE-VISIBLE ListTools descriptions of fishhawk_run_stage,
// fishhawk_dispatch_stage, and fishhawk_get_run_status must each (a) name
// fishhawk_await_stage as the terminal wait to call on the handle and (b) state
// the real trade-off — a client that BACKGROUNDS long tool calls prefers the
// blocking verbs, and dispatch_stage's advantage is its in-band amendment
// channel — and none of them may still advertise dispatch_stage as the better
// long-wait path. A comment-only or no-op touch of any of the three sites
// leaves the stale framing and fails here.
func TestStageWaitDescriptions_StateBackgroundingTradeoff(t *testing.T) {
	descByName := listToolDescriptions(t)
	for _, name := range []string{"fishhawk_run_stage", "fishhawk_dispatch_stage", "fishhawk_get_run_status"} {
		desc, ok := descByName[name]
		if !ok || desc == "" {
			t.Errorf("%s: not registered/visible over ListTools", name)
			continue
		}
		lower := strings.ToLower(desc)
		if !strings.Contains(desc, "fishhawk_await_stage") {
			t.Errorf("%s: description must name fishhawk_await_stage as the wait to call on the handle:\n%s", name, desc)
		}
		if !strings.Contains(lower, "backgrounds long") {
			t.Errorf("%s: description must state the backgrounding-client trade-off (a client that BACKGROUNDS long tool calls):\n%s", name, desc)
		}
		if !strings.Contains(lower, "blocking verbs") {
			t.Errorf("%s: description must name the blocking verbs as the backgrounding client's clean async path:\n%s", name, desc)
		}
		if !strings.Contains(lower, "in-band") {
			t.Errorf("%s: description must state dispatch_stage's advantage is its in-band amendment channel:\n%s", name, desc)
		}
		// The stale framing — dispatch as the better long-wait path — must be
		// gone from all three. The rewrite phrases the trade-off without this
		// substring, so its presence would be a regression.
		if strings.Contains(lower, "better long-wait") {
			t.Errorf("%s: description still advertises dispatch_stage as the better long-wait path (stale framing):\n%s", name, desc)
		}
	}
}

// TestProgressTokenDescriptions_DisclaimClientSupplied is the #2490 done-means
// test: it asserts the WIRE-VISIBLE descriptions of the four progressToken-
// citing tools no longer use the caller-imperative "supply a progressToken"
// phrasing (which sent the reporting operator hunting for a parameter that does
// not exist) and instead carry the client-supplied disclaimer. For the two
// await tools it additionally asserts (a) the description names the reachable
// long_wait knob and the raised 7200 cap, and (b) — binding condition 1 — the
// WIRE-VISIBLE ListTools INPUT SCHEMA advertises long_wait as a boolean
// property with a non-empty description, so a tool-calling client can actually
// discover it. A comment-only or no-op touch of a description site, or a missing
// jsonschema tag on the input field, fails this test.
func TestProgressTokenDescriptions_DisclaimClientSupplied(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	descByName := make(map[string]string, len(res.Tools))
	schemaByName := make(map[string]any, len(res.Tools))
	for _, tool := range res.Tools {
		descByName[tool.Name] = tool.Description
		schemaByName[tool.Name] = tool.InputSchema
	}

	// All four progressToken-citing tools: no caller-imperative phrasing, plus
	// the client-supplied disclaimer marker.
	all := []string{"fishhawk_await_review", "fishhawk_await_audit", "fishhawk_run_stage", "fishhawk_drive_run"}
	for _, name := range all {
		desc, ok := descByName[name]
		if !ok || desc == "" {
			t.Errorf("%s: not registered/visible over ListTools", name)
			continue
		}
		lower := strings.ToLower(desc)
		if strings.Contains(lower, "supply a progresstoken") {
			t.Errorf("%s: description still uses the caller-imperative %q phrasing (progressToken is not a tool input):\n%s", name, "supply a progressToken", desc)
		}
		if !strings.Contains(lower, "supplied by your mcp client") {
			t.Errorf("%s: description missing the %q disclaimer marker:\n%s", name, "supplied by your MCP client", desc)
		}
		if !strings.Contains(lower, "not a tool input") {
			t.Errorf("%s: description missing the %q disclaimer marker:\n%s", name, "not a tool input", desc)
		}
	}

	// The two await tools: description names the reachable long_wait knob and
	// the raised 7200 cap.
	awaits := []string{"fishhawk_await_review", "fishhawk_await_audit"}
	for _, name := range awaits {
		desc := descByName[name]
		if !strings.Contains(desc, "long_wait") {
			t.Errorf("%s: description must name the reachable long_wait knob:\n%s", name, desc)
		}
		if !strings.Contains(desc, "7200") {
			t.Errorf("%s: description must name the reachable 7200 cap:\n%s", name, desc)
		}
	}

	// Binding condition 1: the WIRE-VISIBLE input schema advertises long_wait as
	// a boolean with a non-empty description, so a tool-calling client can
	// discover it — not merely deserialize it when already known.
	for _, name := range awaits {
		schema := schemaByName[name]
		schemaMap, ok := schema.(map[string]any)
		if !ok {
			t.Fatalf("%s: registered InputSchema is %T, want a JSON object map", name, schema)
		}
		props, ok := schemaMap["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: schema has no properties object; got %v", name, schemaMap["properties"])
		}
		lw, ok := props["long_wait"].(map[string]any)
		if !ok {
			t.Fatalf("%s: long_wait property missing from the advertised input schema — a tool-calling client cannot discover it", name)
		}
		if typ, _ := lw["type"].(string); typ != "boolean" {
			t.Errorf("%s: long_wait schema type = %q, want boolean", name, typ)
		}
		if desc, _ := lw["description"].(string); strings.TrimSpace(desc) == "" {
			t.Errorf("%s: long_wait schema description is empty; want a non-empty description", name)
		}
	}
}

// TestApprovePlanReasonDescription_NamesActualCap pins the fishhawk_approve_plan
// `reason` input description to the CONCRETE prompt.MaxApprovalConditionBytes
// value (#2583, operator binding condition 2). The cap is declared once in Go
// but restated in prose across several surfaces; anchoring one machine-checked
// surface to strconv.Itoa(prompt.MaxApprovalConditionBytes) means the number
// cannot drift silently — bump the constant without updating this jsonschema tag
// and this test goes RED. It also asserts the description names the refusal so a
// caller learns the reason is gate-refused, not truncated.
func TestApprovePlanReasonDescription_NamesActualCap(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema any
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_approve_plan" {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		t.Fatal("fishhawk_approve_plan not registered/visible over ListTools")
	}
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_approve_plan InputSchema is %T, want a JSON object map", schema)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_approve_plan schema has no properties object; got %v", schemaMap["properties"])
	}
	reasonProp, ok := props["reason"].(map[string]any)
	if !ok {
		t.Fatal("fishhawk_approve_plan schema has no 'reason' property")
	}
	desc, _ := reasonProp["description"].(string)
	wantCap := strconv.Itoa(prompt.MaxApprovalConditionBytes)
	if !strings.Contains(desc, wantCap) {
		t.Errorf("approve_plan 'reason' description must name the concrete cap %q so the number cannot drift silently; got:\n%s", wantCap, desc)
	}
	if !strings.Contains(desc, "validation_failed") {
		t.Errorf("approve_plan 'reason' description must name the refusal error code 'validation_failed'; got:\n%s", desc)
	}
}

// --- get_plan (E19.4 / #344) ---

// samplePlanContent returns a small but complete standard_v1
// fixture. Used as the inline content on the plan artifact rows
// the fake backend serves.
func samplePlanContent() PlanContent {
	return PlanContent{
		PlanVersion: "standard_v1",
		TicketReference: PlanTicketRef{
			Type: "github_issue",
			URL:  "https://github.com/x/y/issues/42",
			ID:   "x/y#42",
		},
		GeneratedBy: PlanGeneratedBy{
			Agent:     "claude-code",
			Model:     "claude-opus-4-7",
			Timestamp: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		},
		Summary: "Add a dryRun flag to the dispatcher.",
		Scope: PlanScope{
			Files: []PlanScopeFile{
				{Path: "backend/internal/webhook/dispatcher.go", Operation: "modify"},
			},
			EstimatedLinesChanged: 40,
		},
		Approach: []PlanApproachStep{
			{Step: 1, Description: "Plumb dryRun through Handle."},
			{Step: 2, Description: "Add a unit test."},
		},
		Verification: PlanVerification{
			TestStrategy: "Run the dispatcher tests.",
			RollbackPlan: "Revert the PR.",
		},
		RisksAndAssumptions: []string{
			"Operators set dryRun via a feature flag.",
		},
		PredictedRuntimeMinutes:    20,
		PredictedRuntimeConfidence: "high",
	}
}

// seedPlanArtifact attaches a plan artifact to a stage in the fake
// backend. createdAge sets the artifact's CreatedAt so tests can
// distinguish older vs newer when the most-recent-wins rule fires.
func seedPlanArtifact(fb *fakeBackend, stageID uuid.UUID, content PlanContent, createdAge time.Duration) Artifact {
	v := "standard_v1"
	// Round-trip through JSON so Content holds the same shape it
	// would on the wire (decoded objects/arrays, not the typed
	// struct). The Artifact.Content field is `any` to match the
	// MCP SDK's schema reflection.
	body, _ := json.Marshal(content)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	art := Artifact{
		ID:            uuid.New().String(),
		StageID:       stageID.String(),
		Kind:          "plan",
		SchemaVersion: &v,
		ContentHash:   "h",
		Content:       decoded,
		CreatedAt:     time.Now().UTC().Add(-createdAge),
	}
	fb.mu.Lock()
	fb.artifactsByStage[stageID] = append(fb.artifactsByStage[stageID], art)
	fb.mu.Unlock()
	return art
}

func TestGetPlan_RejectsInvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_FromCurrentRun_StatusAvailableResolvedViaSelf(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), RunID: runID.String(), Type: "implement", State: "pending"},
	}
	expectedSummary := "Add a dryRun flag to the dispatcher."
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "self" {
		t.Errorf("ResolvedVia = %q, want self", out.ResolvedVia)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil when Status=available")
	}
	if out.Plan.Summary != expectedSummary {
		t.Errorf("summary = %q", out.Plan.Summary)
	}
	if got := len(out.Plan.Scope.Files); got != 1 {
		t.Errorf("scope.files count = %d", got)
	}
}

func TestGetPlan_PicksMostRecentArtifactWhenMultipleExist(t *testing.T) {
	// Same plan stage carries two standard_v1 artifacts (a re-upload
	// after a plan edit). The resolver must pick the newer one.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"}}

	older := samplePlanContent()
	older.Summary = "stale plan"
	seedPlanArtifact(fb, planStageID, older, 24*time.Hour)

	newer := samplePlanContent()
	newer.Summary = "fresh plan"
	seedPlanArtifact(fb, planStageID, newer, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan == nil || out.Plan.Summary != "fresh plan" {
		t.Errorf("Plan.Summary = %v, want 'fresh plan'", out.Plan)
	}
}

func TestGetPlan_RetryChain_WalksParentRunID(t *testing.T) {
	// Child run has no plan stage (CI-retry shape per #279 / E16);
	// parent run has the plan. The walk should resolve the parent's
	// plan and stamp ResolvedVia=parent:<id>.
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.New()
	parentPlanStage := uuid.New()

	parentIDStr := parentID.String()
	fb.getRunByID[childID] = Run{
		ID:          childID.String(),
		ParentRunID: &parentIDStr,
		State:       "running",
		Repo:        "x/y",
	}
	fb.getRunByID[parentID] = Run{ID: parentID.String(), State: "running", Repo: "x/y"}
	// Child has only an implement stage (the retry's shape).
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New().String(), RunID: childID.String(), Type: "implement", State: "running"},
	}
	// Parent has the plan stage carrying the artifact.
	fb.stagesByRun[parentID] = []Stage{
		{ID: parentPlanStage.String(), RunID: parentID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, parentPlanStage, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: childID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "parent:"+parentID.String() {
		t.Errorf("ResolvedVia = %q, want parent:%s", out.ResolvedVia, parentID)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
}

func TestGetPlan_NoPlanYet_ChainRootReached(t *testing.T) {
	// Run has no plan stage AND no parent. The structured
	// no_plan_yet response names the chain depth searched (0,
	// since the root is the requested run itself).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}
	fb.stagesByRun[runID] = nil // no stages — plan stage absent

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if out.Plan != nil {
		t.Errorf("Plan should be nil on no_plan_yet; got %+v", out.Plan)
	}
	if !strings.Contains(out.Message, "chain root reached") {
		t.Errorf("Message should explain the chain shape: %q", out.Message)
	}
}

func TestGetPlan_NoPlanYet_PlanStagePending(t *testing.T) {
	// Plan stage exists but has no terminal plan artifact yet
	// (mid-upload race). Same no_plan_yet response shape so the
	// agent can branch without parsing prose.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "running"},
	}
	// Artifacts map: empty — no plan uploaded yet.

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
}

func TestGetPlan_RetryChain_DepthCap_NoPlanYet(t *testing.T) {
	// Build a chain of 10 runs, no plan stage on any of them. The
	// walk stops at retryPlanChainDepth (8) and returns
	// no_plan_yet with a "depth cap" message rather than looping
	// forever.
	fb, srv := newFakeBackend(t)
	const chainLen = 10
	ids := make([]uuid.UUID, chainLen)
	for i := range ids {
		ids[i] = uuid.New()
	}
	for i := 0; i < chainLen; i++ {
		row := Run{ID: ids[i].String(), Repo: "x/y", State: "running"}
		if i+1 < chainLen {
			parentStr := ids[i+1].String()
			row.ParentRunID = &parentStr
		}
		fb.getRunByID[ids[i]] = row
		fb.stagesByRun[ids[i]] = nil
	}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: ids[0].String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if !strings.Contains(out.Message, "chain depth cap") {
		t.Errorf("Message should mention chain depth cap: %q", out.Message)
	}
	// Defensive: the walk visited at most retryPlanChainDepth
	// stages-fetches, never the 9th id in the chain.
	if got := fb.stagesCalledByID[ids[retryPlanChainDepth]]; got != 0 {
		t.Errorf("walk visited id[%d] %d times; expected 0 (past the cap)",
			retryPlanChainDepth, got)
	}
}

func TestGetPlan_BackendError_StagesList_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error on stages 500")
	}
	if !strings.Contains(err.Error(), "list stages") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_WithDecomposition_FieldsSurfaced(t *testing.T) {
	// Plan artifact carries decomposition.sub_plans (ADR-025 D2 /
	// #476). The tool must surface Decomposition, its Rationale, the
	// sub-plans slice, and the runtime prediction fields.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}

	content := samplePlanContent()
	content.Decomposition = &PlanDecomposition{
		Rationale: "Two independent file areas allow parallel execution.",
		SubPlans: []PlanSubPlan{
			{Title: "Add dispatcher flag", ScopeHint: "backend/internal/webhook/", PredictedRuntimeMinutes: 10, PredictedRuntimeConfidence: "high"},
			{Title: "Add unit tests", ScopeHint: "backend/internal/webhook/dispatcher_test.go", PredictedRuntimeMinutes: 8, PredictedRuntimeConfidence: "medium"},
		},
	}
	seedPlanArtifact(fb, planStageID, content, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil when Status=available")
	}
	if out.Plan.Decomposition == nil {
		t.Fatal("Plan.Decomposition should be non-nil for a decomposed plan")
	}
	if out.Plan.Decomposition.Rationale == "" {
		t.Error("Plan.Decomposition.Rationale should be non-empty")
	}
	if got := len(out.Plan.Decomposition.SubPlans); got != 2 {
		t.Errorf("len(Plan.Decomposition.SubPlans) = %d, want 2", got)
	}
	if out.Plan.PredictedRuntimeMinutes <= 0 {
		t.Errorf("Plan.PredictedRuntimeMinutes = %d, want > 0", out.Plan.PredictedRuntimeMinutes)
	}
}

func TestGetPlan_WithoutDecomposition_RuntimeFieldsPresent(t *testing.T) {
	// Plan artifact has no decomposition (standalone plan). The D2
	// runtime-prediction fields must still be surfaced; Decomposition
	// must be nil so it is omitted from the JSON response.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
	if out.Plan.PredictedRuntimeMinutes <= 0 {
		t.Errorf("Plan.PredictedRuntimeMinutes = %d, want > 0", out.Plan.PredictedRuntimeMinutes)
	}
	if out.Plan.PredictedRuntimeConfidence == "" {
		t.Error("Plan.PredictedRuntimeConfidence should be non-empty")
	}
	if out.Plan.Decomposition != nil {
		t.Errorf("Plan.Decomposition should be nil for a non-decomposed plan; got %+v", out.Plan.Decomposition)
	}
}

// --- get_plan reviews field (ADR-027 / #560 sub-plan E) ---

// seedPlanReviewAudit adds a plan_reviewed audit entry to the fake's
// per-run audit map. The payload is round-tripped through JSON so the
// handler's re-marshal + unmarshal decodes to the same value.
func seedPlanReviewAudit(fb *fakeBackend, runID uuid.UUID, review PlanReview) {
	payload, _ := json.Marshal(review)
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_reviewed",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

// seedImplementReviewAudit adds an implement_reviewed audit entry to the
// fake's per-run audit map (ADR-027 impl 2/2). Mirrors
// seedPlanReviewAudit but for the implement-review category.
func seedImplementReviewAudit(fb *fakeBackend, runID uuid.UUID, review PlanReview) {
	payload, _ := json.Marshal(review)
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "implement_reviewed",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

// concernEvidenceWirePayload is a *_reviewed audit payload built from RAW WIRE
// KEYS rather than from a marshalled PlanReview (E60.8 / #2353). Seeding
// through the typed struct would make a json-tag typo self-cancelling — the
// same wrong key would be written and read — so the one seam under test, the
// decode of `new_evidence` / `settled_ref` off the real payload, would not be
// exercised at all.
func concernEvidenceWirePayload(verdict, evidence, settledRef string) map[string]any {
	return map[string]any{
		"reviewer_kind":  "agent",
		"reviewer_model": "gpt-5.6-sol",
		"authority":      "advisory",
		"verdict":        verdict,
		"concerns": []any{
			map[string]any{
				"severity":     "high",
				"category":     "correctness",
				"note":         "the control is deleted but the test stays green",
				"new_evidence": evidence,
				"settled_ref":  settledRef,
			},
			map[string]any{
				"severity": "low",
				"category": "style",
				"note":     "a concern the reviewer supplied no evidence for",
			},
		},
	}
}

// seedRawReviewAudit appends a *_reviewed audit entry carrying an arbitrary
// wire payload, so a decode test can drive real JSON keys.
func seedRawReviewAudit(fb *fakeBackend, runID uuid.UUID, category string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: category,
		Payload:  decoded,
	})
}

// assertConcernEvidenceDecoded checks the shared PlanReviewConcern decode
// carried both fields onto the first concern and left the second (which the
// payload gave neither) empty.
func assertConcernEvidenceDecoded(t *testing.T, surface string, reviews []PlanReview, evidence, settledRef string) {
	t.Helper()
	if len(reviews) != 1 || len(reviews[0].Concerns) != 2 {
		t.Fatalf("%s: reviews = %+v, want 1 review with 2 concerns", surface, reviews)
	}
	c := reviews[0].Concerns[0]
	if c.NewEvidence != evidence {
		t.Errorf("%s: concern.new_evidence = %q, want %q verbatim", surface, c.NewEvidence, evidence)
	}
	if c.SettledRef != settledRef {
		t.Errorf("%s: concern.settled_ref = %q, want %q", surface, c.SettledRef, settledRef)
	}
	if bare := reviews[0].Concerns[1]; bare.NewEvidence != "" || bare.SettledRef != "" {
		t.Errorf("%s: no-evidence concern = (%q, %q), want both empty",
			surface, bare.NewEvidence, bare.SettledRef)
	}
}

// TestGetPlan_ConcernEvidenceDecoded is condition 6's explicit criterion for
// fishhawk_get_plan. get_plan, get_run_status and await_review all render
// reviews[] from ONE decode (runResolver.decodeReviewVerdicts into
// PlanReviewConcern), but "covered transitively" is a claim worth an assertion
// on the surface the operator actually reads — get_plan's reviews[] is where a
// plan-gate rejection's evidence has to appear.
func TestGetPlan_ConcernEvidenceDecoded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	const evidence = "backend/internal/server/trace.go:4557 builds the RaisedConcern literal without either field"
	settledRef := uuid.New().String()
	planStageID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	seedRawReviewAudit(fb, runID, "plan_reviewed", concernEvidenceWirePayload("reject", evidence, settledRef))

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	assertConcernEvidenceDecoded(t, "get_plan", out.Reviews, evidence, settledRef)
}

// TestGetRunStatus_ConcernEvidenceDecoded is condition 6's criterion for the
// other shared-decode surface: fishhawk_get_run_status's implement review block.
func TestGetRunStatus_ConcernEvidenceDecoded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	const evidence = "the gate view rendered note alone; the substance sat in new_evidence"
	settledRef := uuid.New().String()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedRawReviewAudit(fb, runID, "implement_reviewed", concernEvidenceWirePayload("reject", evidence, settledRef))

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	assertConcernEvidenceDecoded(t, "get_run_status", out.ImplementReviews, evidence, settledRef)
}

// seedSecurityFindingsAudit adds an implement_security_findings audit entry
// (#1096) to the fake's per-run audit map, carrying the findings under the
// cross-slice "findings" key. Round-tripped through JSON so the handler's
// re-marshal + unmarshal decodes to the same value.
func seedSecurityFindingsAudit(fb *fakeBackend, runID uuid.UUID, findings []securityscan.Finding) {
	payload, _ := json.Marshal(struct {
		Findings []securityscan.Finding `json:"findings"`
	}{Findings: findings})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: securityscan.AuditCategorySecurityFindings,
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

// TestGetRunStatus_SecurityFindings_ClearsAfterFixupFloor reproduces the real
// post-fix-up-clean writer path (#1096): a dirty scan is recorded, then a
// fix-up is triggered, then the clean re-scan records NOTHING (the webhook
// writer omits a clean marker above the floor). The surface must still omit the
// resolved finding by flooring on the latest stage_fixup_triggered — exactly as
// the merge gate does — rather than surfacing the stale pre-fix-up dirty entry.
func TestGetRunStatus_SecurityFindings_ClearsAfterFixupFloor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	// Dirty scan, then a fix-up triggered after it. No clean entry follows —
	// the writer records none in the post-fix-up-clean path.
	seedSecurityFindingsAudit(fb, runID, []securityscan.Finding{
		{Number: 7, RuleID: "go/sql-injection", Severity: securityscan.SeverityHigh, Path: "pkg/bar/bar.go"},
	})
	seedFixupTriggeredAudit(fb, runID, uuid.New())

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.SecurityFindings) != 0 {
		t.Errorf("SecurityFindings = %+v, want empty (finding floored below the fix-up marker)", out.SecurityFindings)
	}
}

// TestGetRunStatus_SecurityFindings_DirtyReScanAboveFloorReblocks: a fresh dirty
// re-scan recorded ABOVE the fix-up floor must surface — the floor stales only
// pre-fix-up entries, not a genuine new finding after the fix-up.
func TestGetRunStatus_SecurityFindings_DirtyReScanAboveFloorReblocks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	seedSecurityFindingsAudit(fb, runID, []securityscan.Finding{
		{Number: 7, RuleID: "go/sql-injection", Severity: securityscan.SeverityHigh, Path: "pkg/bar/bar.go"},
	})
	seedFixupTriggeredAudit(fb, runID, uuid.New())
	// Fresh dirty re-scan after the fix-up — above the floor, must re-block.
	seedSecurityFindingsAudit(fb, runID, []securityscan.Finding{
		{Number: 9, RuleID: "go/path-injection", Severity: securityscan.SeverityHigh, Path: "pkg/baz/baz.go"},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.SecurityFindings) != 1 || out.SecurityFindings[0].Number != 9 {
		t.Errorf("SecurityFindings = %+v, want the post-fix-up finding #9", out.SecurityFindings)
	}
}

// TestGetRunStatus_SurfacesSecurityFindings (#1096): the run-status output
// distills the newest implement_security_findings audit entry's findings so
// a high-severity code-scanning finding surfaces at the review gate.
func TestGetRunStatus_SurfacesSecurityFindings(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	seedSecurityFindingsAudit(fb, runID, []securityscan.Finding{
		{
			Number:      7,
			RuleID:      "go/sql-injection",
			Description: "Database query built from user-controlled sources",
			Severity:    securityscan.SeverityHigh,
			State:       "open",
			Path:        "pkg/bar/bar.go",
			StartLine:   42,
			HTMLURL:     "https://github.com/x/y/security/code-scanning/7",
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.SecurityFindings) != 1 {
		t.Fatalf("SecurityFindings = %+v, want 1 entry", out.SecurityFindings)
	}
	f := out.SecurityFindings[0]
	if f.RuleID != "go/sql-injection" || f.Severity != securityscan.SeverityHigh ||
		f.Path != "pkg/bar/bar.go" || f.StartLine != 42 || f.Number != 7 {
		t.Errorf("SecurityFindings[0] = %+v, want the seeded high-severity finding", f)
	}
}

// TestGetRunStatus_SecurityFindings_NewestWins: a clean re-scan recorded
// after a finding (higher sequence, empty findings) clears the surface.
func TestGetRunStatus_SecurityFindings_NewestWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	seedSecurityFindingsAudit(fb, runID, []securityscan.Finding{
		{Number: 7, RuleID: "go/sql-injection", Severity: securityscan.SeverityHigh, Path: "pkg/bar/bar.go"},
	})
	seedSecurityFindingsAudit(fb, runID, nil) // clean re-scan after a fix-up

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.SecurityFindings) != 0 {
		t.Errorf("SecurityFindings = %+v, want empty (newest clean re-scan wins)", out.SecurityFindings)
	}
}

// TestGetRunStatus_NoSecurityFindings_NilField: a run with no scan entry
// carries no SecurityFindings (additive — omitted).
func TestGetRunStatus_NoSecurityFindings_NilField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.SecurityFindings != nil {
		t.Errorf("SecurityFindings = %+v, want nil when no scan landed", out.SecurityFindings)
	}
}

func TestGetRunStatus_WithImplementReviews_PopulatesField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns: []PlanReviewConcern{
			{Severity: "low", Category: "scope", Note: "touched a file outside scope.files"},
		},
		FreeForm: "diff implements the plan",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if got := len(out.ImplementReviews); got != 1 {
		t.Fatalf("len(ImplementReviews) = %d, want 1", got)
	}
	rev := out.ImplementReviews[0]
	if rev.Verdict != "approve_with_concerns" {
		t.Errorf("Verdict = %q, want approve_with_concerns", rev.Verdict)
	}
	if rev.Authority != "advisory" {
		t.Errorf("Authority = %q, want advisory", rev.Authority)
	}
	if len(rev.Concerns) != 1 || rev.Concerns[0].Category != "scope" {
		t.Errorf("Concerns = %+v, want one scope concern", rev.Concerns)
	}
}

// seedRecentAuditPayload appends one recent-audit entry (served from the
// /v0/audit feed, distinct from the per-run audit endpoint) carrying an
// arbitrary decoded-JSON payload. Used by the #1727 compact-projection
// tests to exercise the recent_audit body/comments/free_form strip.
func seedRecentAuditPayload(fb *fakeBackend, runID uuid.UUID, category string, payload map[string]any) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	// Stamp non-empty verifier-only hash-chain fields (64 hex chars each, as the
	// production /v0/audit feed carries) so the #1749 compact-projection tests can
	// assert they are dropped by default and restored under include_audit_hashes.
	prev := strings.Repeat("a", 64)
	fb.auditByRun[runID] = append(fb.auditByRun[runID], AuditEntry{
		ID:        uuid.New().String(),
		Sequence:  int64(len(fb.auditByRun[runID]) + 1),
		RunID:     runID.String(),
		Category:  category,
		Payload:   decoded,
		PrevHash:  &prev,
		EntryHash: strings.Repeat("b", 64),
	})
}

// seedCompactFixture builds a run that carries every heavy free-text field
// the #1727 projection targets — issue_context, implement-review prose,
// and a recent-audit payload with body/comments/free_form — plus the
// operator-playbook fields (stages for the wait statuses, open concerns)
// so the same fixture drives both the omission and the retention tests.
func seedCompactFixture(t *testing.T) (*fakeBackend, *httptest.Server, uuid.UUID) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", State: "running",
		IssueContext: &IssueContext{
			Title:    "an issue",
			Body:     "the big issue body text",
			URL:      "https://example/issues/1",
			Number:   1727,
			Comments: []IssueComment{{Author: "a", Body: "a comment body", CreatedAt: "t"}},
		},
		Concerns: &RunConcerns{
			Open:    1,
			ByState: map[string]int{"raised": 1},
			Items:   []RunConcernItem{{ID: uuid.NewString(), StageKind: "implement", Severity: "low", Category: "scope", State: "raised"}},
		},
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 3, Type: "acceptance", State: "pending"},
	}
	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-8",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns:      []PlanReviewConcern{{Severity: "high", Category: "security", Note: "unvalidated input note"}},
		FreeForm:      "reviewer free-text prose",
	})
	// A recent-audit review payload carrying nested prose + an issue-fetch
	// payload carrying body/comments — the major recent_audit token source.
	seedRecentAuditPayload(fb, runID, "implement_reviewed", map[string]any{
		"verdict":   "approve_with_concerns",
		"free_form": "recent payload prose",
	})
	seedRecentAuditPayload(fb, runID, "issue_context_fetched", map[string]any{
		"title":    "an issue",
		"body":     "recent payload issue body",
		"comments": []any{map[string]any{"body": "recent comment body"}},
	})
	// A recent-audit entry carrying an oversized non-free-text payload string
	// (survives the #1727 free-text strip) — the #1749 truncation target.
	seedRecentAuditPayload(fb, runID, "cost_recorded", map[string]any{
		"detail": strings.Repeat("x", 800),
	})
	// A cache_efficiency metric WITH a per-stage breakdown — the #1749
	// cache-stage collapse target.
	seedCacheEfficiency(fb, runID, CacheEfficiency{
		CacheReadRatio: 0.5, ReuseFactor: 2.0, NetSavingsUSD: 16.75,
		Stages: []CacheEfficiencyStage{
			{Source: "agent", FreshInputTokens: 2_000_000},
			{Source: "implement_review", NetSavingsUSD: 4.5},
			{Source: "plan_review", NetSavingsUSD: 12.25},
		},
	})
	return fb, srv, runID
}

// TestGetRunStatus_CompactDefault_OmitsHeavyFreeText is the #1727 default
// omission proof: issue_context, reviewer free_form, and per-concern notes
// are stripped from the wire bytes, not merely nil in Go.
func TestGetRunStatus_CompactDefault_OmitsHeavyFreeText(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.IssueContext != nil {
		t.Errorf("Run.IssueContext = %+v, want nil (stripped by default)", out.Run.IssueContext)
	}
	if len(out.ImplementReviews) != 1 {
		t.Fatalf("len(ImplementReviews) = %d, want 1", len(out.ImplementReviews))
	}
	// The heavy free-text is elided to the visible marker (#3043), not the
	// reviewer's prose — distinct from a genuinely-empty note.
	if out.ImplementReviews[0].FreeForm != elidedReviewProseMarker {
		t.Errorf("ImplementReviews[0].FreeForm = %q, want the elision marker", out.ImplementReviews[0].FreeForm)
	}
	if n := out.ImplementReviews[0].Concerns[0].Note; n != elidedReviewProseMarker {
		t.Errorf("concern Note = %q, want the elision marker", n)
	}
	// Wire-bytes proof: the heavy keys must not survive serialization.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	// "issue_context" as a bare substring would false-match the
	// "issue_context_fetched" audit category name, so assert on the JSON
	// key form ("issue_context":) plus the heavy values themselves. The
	// review free_form KEY now legitimately carries the short elision marker
	// (#3043) — so the reviewer's PROSE is what must be absent, not the key.
	for _, banned := range []string{`"issue_context":`, "the big issue body", "reviewer free-text prose", "recent payload prose", "unvalidated input note", "recent payload issue body"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("wire bytes must not contain %q (compact default), got it in payload", banned)
		}
	}
	// The elision is VISIBLE: the marker (distinctive prose that names the
	// full-note surface) survives where the reviewer's prose was.
	if !strings.Contains(string(raw), "elided; full text via fishhawk_get_gate_view") {
		t.Errorf("compact default should carry the visible elision marker naming fishhawk_get_gate_view:\n%s", raw)
	}
}

// TestGetRunStatus_CompactDefault_HintReadsPreCompactionStoreConcerns is
// approval condition 3's must-land ordering assertion (#3043): on the
// compact-BY-DEFAULT getRunStatus path, the review-action hint must read the
// PRE-compaction runRow.Concerns block — so an AUTHORITATIVE store block (a
// non-nil OpenImplement) drives the hint with source="store", NOT a fallback
// marker. If a compaction lever ever turned a present authoritative block into
// an absent one BEFORE the hint read it, the hint would degrade to the stale
// audit count under a fallback marker — reintroducing this issue's symptom
// through the new code. The fixture seeds an audit sum (2) that DIFFERS from the
// authoritative store count (1), so a hint reporting 1/store proves the store
// block reached the hint intact under compaction; a hint reporting 2 (or any
// audit_fallback_* source) would prove the block was lost first.
func TestGetRunStatus_CompactDefault_HintReadsPreCompactionStoreConcerns(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	openImplement := 1
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", State: "running",
		Concerns: &RunConcerns{
			Open:          1,
			ByState:       map[string]int{"raised": 1},
			OpenImplement: &openImplement, // AUTHORITATIVE scalar present
			Items:         []RunConcernItem{{ID: uuid.NewString(), StageKind: "implement", Severity: "low", Category: "scope", State: "raised"}},
		},
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}
	// Audit sum (2) deliberately differs from the store's authoritative count (1).
	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory",
		Verdict: "approve_with_concerns",
		Concerns: []PlanReviewConcern{
			{Severity: "high", Category: "security", Note: "one"},
			{Severity: "low", Category: "scope", Note: "two"},
		},
	})

	r := newResolver(srv, nil)
	// Compact by DEFAULT: no include_* flags.
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ReviewActionHint == nil {
		t.Fatalf("review_action_hint absent; want a populated hint from the store block")
	}
	if out.ReviewActionHint.Source != hintSourceStore {
		t.Errorf("hint.Source = %q, want store (the PRE-compaction authoritative block must reach the hint)", out.ReviewActionHint.Source)
	}
	if out.ReviewActionHint.Concerns != 1 {
		t.Errorf("hint.Concerns = %d, want 1 (the authoritative store count, NOT the audit sum of 2)", out.ReviewActionHint.Concerns)
	}
}

// TestGetRunStatus_HintLegacyPeerBlock is approval condition 1's "test all three"
// source states asserted THROUGH the real getRunStatus (tools.go) call site
// rather than only via direct reviewActionHintFor calls (#3043). A run row whose
// concerns block is PRESENT but predates the open_implement scalar (a legacy
// peer: OpenImplement nil) must decode to the audit fallback under the DISTINCT
// audit_fallback_legacy_peer marker — never suppress, and never claim the store
// was unavailable when the read in fact succeeded. The store block carries open
// implement items, so a plain-int decode of the absent scalar would read as an
// authoritative zero and wrongly suppress; the pointer is what keeps this legacy
// state distinguishable. The audit sum (2) is the fallback source.
func TestGetRunStatus_HintLegacyPeerBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	implementStageID := uuid.New()
	// A legacy concerns block: present, with open items, but NO open_implement
	// scalar (OpenImplement stays nil). The store read SUCCEEDED.
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", State: "running",
		Concerns: &RunConcerns{
			Open:          2,
			ByState:       map[string]int{"raised": 2},
			OpenImplement: nil, // legacy peer: scalar absent
			Items: []RunConcernItem{
				{ID: uuid.NewString(), StageKind: "implement", Severity: "medium", Category: "scope", State: "raised"},
				{ID: uuid.NewString(), StageKind: "implement", Severity: "low", Category: "style", State: "raised"},
			},
		},
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: implementStageID.String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}
	// The audit fallback source: two implement-stage concerns in the latest round.
	seedImplementReviewedAudit(fb, runID, implementStageID, 2)

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ReviewActionHint == nil {
		t.Fatalf("review_action_hint absent; want the audit fallback to fire (a legacy peer must never suppress)")
	}
	if out.ReviewActionHint.Source != hintSourceLegacyPeer {
		t.Errorf("hint.Source = %q, want audit_fallback_legacy_peer", out.ReviewActionHint.Source)
	}
	if out.ReviewActionHint.Concerns != 2 {
		t.Errorf("hint.Concerns = %d, want 2 (the audit fallback count)", out.ReviewActionHint.Concerns)
	}
	if !strings.Contains(out.ReviewActionHint.Message, "predates the authoritative implement-stage concern count") {
		t.Errorf("legacy-peer message should name the missing authoritative count; got %q", out.ReviewActionHint.Message)
	}
	if strings.Contains(out.ReviewActionHint.Message, "the concern store was unavailable") {
		t.Errorf("legacy-peer message must NOT claim the store was unavailable (the read succeeded); got %q", out.ReviewActionHint.Message)
	}
}

// TestGetRunStatus_HintStoreUnavailableBlock is the third of condition 1's
// source states asserted through the real getRunStatus (tools.go) call site
// (#3043): a run row whose concerns block is ABSENT (nil — the backend could not
// read the store, or the repo is unwired) must degrade to the audit fallback
// under the audit_fallback_store_unavailable marker, whose wording DOES say the
// store was unavailable (true here). This is the same degraded reviewActionHintFor
// path the run_stage.go call site reaches when its post-run run fetch fails
// (runView == nil -> storeConcerns nil); that run_stage.go arm is pinned directly
// by TestRunStage_ReviewActionHint_StoreUnavailableWhenRunFetchFails, so this
// case pins the getRunStatus (tools.go) call site for the same marker.
func TestGetRunStatus_HintStoreUnavailableBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	implementStageID := uuid.New()
	// No concerns block at all: the store was unavailable.
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running", Concerns: nil}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: implementStageID.String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}
	seedImplementReviewedAudit(fb, runID, implementStageID, 2)

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ReviewActionHint == nil {
		t.Fatalf("review_action_hint absent; want the audit fallback to fire when the store block is absent")
	}
	if out.ReviewActionHint.Source != hintSourceStoreUnavailable {
		t.Errorf("hint.Source = %q, want audit_fallback_store_unavailable", out.ReviewActionHint.Source)
	}
	if out.ReviewActionHint.Concerns != 2 {
		t.Errorf("hint.Concerns = %d, want 2 (the audit fallback count)", out.ReviewActionHint.Concerns)
	}
	if !strings.Contains(out.ReviewActionHint.Message, "the concern store was unavailable") {
		t.Errorf("store-unavailable message should say the store was unavailable; got %q", out.ReviewActionHint.Message)
	}
}

// TestGetRunStatus_IncludeIssueContext_RestoresIssuePayload asserts the
// opt-in restores IssueContext and the recent-audit body/comments.
func TestGetRunStatus_IncludeIssueContext_RestoresIssuePayload(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeIssueContext: true})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.IssueContext == nil || out.Run.IssueContext.Body != "the big issue body text" {
		t.Fatalf("IssueContext not restored: %+v", out.Run.IssueContext)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "recent payload issue body") {
		t.Errorf("recent_audit issue body should be restored under include_issue_context")
	}
	// Review prose still stripped to the elision marker (the other flag was
	// not set) — not the reviewer's original prose.
	if out.ImplementReviews[0].FreeForm != elidedReviewProseMarker {
		t.Errorf("review free_form should still be elided to the marker when only include_issue_context is set; got %q", out.ImplementReviews[0].FreeForm)
	}
}

// TestGetRunStatus_IncludeReviewProse_RestoresReviewText asserts the opt-in
// restores implement-review free_form + concern notes and the recent-audit
// review payload free_form.
func TestGetRunStatus_IncludeReviewProse_RestoresReviewText(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeReviewProse: true})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ImplementReviews[0].FreeForm != "reviewer free-text prose" {
		t.Errorf("FreeForm = %q, want restored", out.ImplementReviews[0].FreeForm)
	}
	if out.ImplementReviews[0].Concerns[0].Note != "unvalidated input note" {
		t.Errorf("concern Note = %q, want restored", out.ImplementReviews[0].Concerns[0].Note)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "recent payload prose") {
		t.Errorf("recent_audit review free_form should be restored under include_review_prose")
	}
	// Issue context still stripped (the other flag was not set).
	if out.Run.IssueContext != nil {
		t.Errorf("IssueContext should still be stripped when only include_review_prose is set")
	}
}

// TestGetRunStatus_CompactDefault_RetainsPlaybookFields is the retention
// half of #1727: the compact default keeps every operator-playbook field.
func TestGetRunStatus_CompactDefault_RetainsPlaybookFields(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil {
		t.Error("NextActions is nil; want the playbook block retained")
	}
	if out.PlanStageWaitStatus == nil || out.ImplementStageWaitStatus == nil || out.AcceptanceStageWaitStatus == nil {
		t.Errorf("wait statuses must be retained: plan=%v implement=%v acceptance=%v",
			out.PlanStageWaitStatus, out.ImplementStageWaitStatus, out.AcceptanceStageWaitStatus)
	}
	if out.Run.Concerns == nil || out.Run.Concerns.Open != 1 {
		t.Errorf("run.concerns must be retained, got %+v", out.Run.Concerns)
	}
	rev := out.ImplementReviews[0]
	if rev.Verdict != "approve_with_concerns" || rev.Authority != "advisory" {
		t.Errorf("review verdict/authority must be retained, got %+v", rev)
	}
	if len(rev.Concerns) != 1 || rev.Concerns[0].Severity != "high" || rev.Concerns[0].Category != "security" {
		t.Errorf("concern severity/category must be retained, got %+v", rev.Concerns)
	}
}

// findRecentAudit returns the first recent_audit entry of the given category.
func findRecentAudit(t *testing.T, entries []AuditEntry, category string) AuditEntry {
	t.Helper()
	for _, e := range entries {
		if e.Category == category {
			return e
		}
	}
	t.Fatalf("no recent_audit entry of category %q in %+v", category, entries)
	return AuditEntry{}
}

// TestGetRunStatus_CompactDefault_OmitsAuditHashes is the #1749 default proof:
// each recent_audit entry's verifier-only entry_hash/prev_hash are dropped from
// the Go values AND the wire bytes.
func TestGetRunStatus_CompactDefault_OmitsAuditHashes(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.RecentAudit) == 0 {
		t.Fatal("no recent audit entries")
	}
	for _, e := range out.RecentAudit {
		if e.EntryHash != "" {
			t.Errorf("entry %s: EntryHash = %q, want dropped", e.Category, e.EntryHash)
		}
		if e.PrevHash != nil {
			t.Errorf("entry %s: PrevHash = %v, want dropped", e.Category, *e.PrevHash)
		}
	}
	raw, _ := json.Marshal(out)
	for _, banned := range []string{`"entry_hash":`, `"prev_hash":`, strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("wire bytes must not contain %q (compact default)", banned)
		}
	}
}

// TestGetRunStatus_IncludeAuditHashes_RestoresHashesAndPayload asserts the
// single flag restores BOTH the hash-chain fields AND the untruncated payload
// values together (the ratified single-flag coupling).
func TestGetRunStatus_IncludeAuditHashes_RestoresHashesAndPayload(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeAuditHashes: true})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	cost := findRecentAudit(t, out.RecentAudit, "cost_recorded")
	if cost.EntryHash != strings.Repeat("b", 64) {
		t.Errorf("EntryHash = %q, want restored", cost.EntryHash)
	}
	if cost.PrevHash == nil || *cost.PrevHash != strings.Repeat("a", 64) {
		t.Errorf("PrevHash = %v, want restored", cost.PrevHash)
	}
	// Full untruncated payload restored together (not just the hashes).
	detail := cost.Payload.(map[string]any)["detail"].(string)
	if detail != strings.Repeat("x", 800) {
		t.Errorf("payload detail truncated under include_audit_hashes: len=%d", len(detail))
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "full value via fishhawk_list_audit") {
		t.Error("wire bytes must not carry the truncation marker under include_audit_hashes")
	}
}

// TestGetRunStatus_CompactDefault_TruncatesLargePayload asserts an oversized
// recent_audit payload string is truncated by default with the list_audit marker.
func TestGetRunStatus_CompactDefault_TruncatesLargePayload(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	cost := findRecentAudit(t, out.RecentAudit, "cost_recorded")
	detail := cost.Payload.(map[string]any)["detail"].(string)
	if len(detail) >= 800 {
		t.Errorf("payload detail not truncated: len=%d", len(detail))
	}
	if !strings.Contains(detail, "full value via fishhawk_list_audit") {
		t.Errorf("truncated payload missing list_audit marker: %q", detail)
	}
}

// TestGetRunStatus_CompactDefault_CollapsesCacheStages asserts the
// cache_efficiency per-stage breakdown collapses to nil by default while the
// run-level rollup scalars remain.
func TestGetRunStatus_CompactDefault_CollapsesCacheStages(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.CacheEfficiency == nil {
		t.Fatal("cache_efficiency block missing")
	}
	if out.CacheEfficiency.Stages != nil {
		t.Errorf("cache stages should collapse to nil by default, got %+v", out.CacheEfficiency.Stages)
	}
	// Rollup scalars are always present.
	if out.CacheEfficiency.NetSavingsUSD != 16.75 || out.CacheEfficiency.CacheReadRatio != 0.5 {
		t.Errorf("rollup scalars must survive collapse, got %+v", out.CacheEfficiency)
	}
	// The top-level Stages (ordered stage list) always serializes "stages:",
	// so assert on a cache-stage-only marker instead: the per-stage source
	// "plan_review" appears only in the cache breakdown (the stage list types
	// are plan/implement/acceptance).
	ceRaw, _ := json.Marshal(out.CacheEfficiency)
	if strings.Contains(string(ceRaw), `"source"`) || strings.Contains(string(ceRaw), "plan_review") {
		t.Errorf("cache_efficiency wire bytes must omit the per-stage breakdown by default: %s", ceRaw)
	}
}

// TestGetRunStatus_IncludeCacheStages_RestoresBreakdown asserts the opt-in
// restores the per-stage breakdown while the rollup scalars remain present.
func TestGetRunStatus_IncludeCacheStages_RestoresBreakdown(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeCacheStages: true})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.CacheEfficiency == nil || len(out.CacheEfficiency.Stages) != 3 {
		t.Fatalf("cache stages not restored: %+v", out.CacheEfficiency)
	}
	if out.CacheEfficiency.Stages[2].Source != "plan_review" || out.CacheEfficiency.Stages[2].NetSavingsUSD != 12.25 {
		t.Errorf("stage breakdown wrong: %+v", out.CacheEfficiency.Stages[2])
	}
	if out.CacheEfficiency.NetSavingsUSD != 16.75 {
		t.Errorf("rollup scalar altered: %+v", out.CacheEfficiency)
	}
}

// TestListAudit_RetainsAuditHashes is the omitempty regression guard: the
// EntryHash json:"entry_hash,omitempty" change must NOT leak into the verifier
// surface — fishhawk_list_audit entries always carry non-empty entry_hash/prev_hash.
func TestListAudit_RetainsAuditHashes(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	prev := strings.Repeat("a", 64)
	fb.perRunAuditByRun[runID] = []AuditEntry{
		{
			ID: uuid.New().String(), Sequence: 1, RunID: runID.String(),
			Category: "plan_generated", Payload: json.RawMessage(`{}`),
			PrevHash: &prev, EntryHash: strings.Repeat("b", 64),
		},
	}
	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("listAudit: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("want 1 item, got %d", len(out.Items))
	}
	if out.Items[0].EntryHash != strings.Repeat("b", 64) {
		t.Errorf("list_audit entry_hash dropped: %q", out.Items[0].EntryHash)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"entry_hash":`) || !strings.Contains(string(raw), `"prev_hash":`) {
		t.Errorf("list_audit wire bytes must retain entry_hash + prev_hash: %s", raw)
	}
}

// TestGetRunStatus_CompactDefault_UnderSizeBudget is the done-means byte-size
// backstop (#1749): a representative audit_limit=10 post-review run marshalled
// with no flags must fall under the ~7KB target. A comment-only / no-op
// implementation fails this even though the scope-presence gate passes.
func TestGetRunStatus_CompactDefault_UnderSizeBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running",
		IssueContext: &IssueContext{
			Title: "an issue", Body: strings.Repeat("issue body ", 40), URL: "https://example/issues/1", Number: 1749,
			Comments: []IssueComment{{Author: "a", Body: strings.Repeat("comment ", 30), CreatedAt: "t"}},
		},
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
	}
	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory",
		Verdict:  "approve_with_concerns",
		Concerns: []PlanReviewConcern{{Severity: "high", Category: "security", Note: strings.Repeat("concern note ", 20)}},
		FreeForm: strings.Repeat("reviewer prose ", 40),
	})
	// Ten realistic recent-audit entries with non-empty hashes + payloads,
	// enough to reach audit_limit=10.
	seedRecentAuditPayload(fb, runID, "plan_generated", map[string]any{"detail": strings.Repeat("p", 400)})
	seedRecentAuditPayload(fb, runID, "issue_context_fetched", map[string]any{
		"title": "an issue", "body": strings.Repeat("body ", 60),
		"comments": []any{map[string]any{"body": strings.Repeat("c ", 40)}},
	})
	seedRecentAuditPayload(fb, runID, "implement_reviewed", map[string]any{
		"verdict": "approve_with_concerns", "free_form": strings.Repeat("prose ", 60),
	})
	for i := 0; i < 7; i++ {
		seedRecentAuditPayload(fb, runID, "cost_recorded", map[string]any{"detail": strings.Repeat("x", 400)})
	}
	seedCacheEfficiency(fb, runID, CacheEfficiency{
		CacheReadRatio: 0.5, ReuseFactor: 2.0, NetSavingsUSD: 16.75,
		Stages: []CacheEfficiencyStage{
			{Source: "agent", FreshInputTokens: 2_000_000, CacheReadRatio: 0.5, NetSavingsUSD: 4.0},
			{Source: "implement_review", CacheReadRatio: 0.5, NetSavingsUSD: 4.5},
			{Source: "plan_review", CacheReadRatio: 0.75, ReuseFactor: 3.0, NetSavingsUSD: 12.25},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), AuditLimit: 10})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if len(out.RecentAudit) != 10 {
		t.Fatalf("want 10 recent audit entries at audit_limit=10, got %d", len(out.RecentAudit))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const target = 7 * 1024
	if len(raw) >= target {
		t.Errorf("compact default response = %d bytes, want < %d (lower auditPayloadStringCap)", len(raw), target)
	}
}

// TestGetRunStatus_Heterogeneous_BothReviewerRowsAcrossStages is the #1127
// cross-seam test: a full heterogeneous round (configured_agents=2) on BOTH
// the plan and implement stages must surface BOTH reviewer rows through the
// getRunStatus output's plan_review_status.reviews[] and
// implement_review_status.reviews[] — the end-to-end path from the audit store
// through reviewStatusFor to the tool payload. Before #1127 a poll could catch
// the partial-landing window and return 'complete' with only the first row.
func TestGetRunStatus_Heterogeneous_BothReviewerRowsAcrossStages(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	// Plan stage: a full 2-reviewer round.
	seedReviewStartedAudit(fb, runID, "plan_review_started", 2, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory", Verdict: "approve_with_concerns", Concerns: []PlanReviewConcern{{Severity: "medium", Category: "scope", Note: "x"}}})
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "gpt-5.5", Authority: "advisory", Verdict: "reject"})

	// Implement stage: a full 2-reviewer round.
	seedReviewStartedAudit(fb, runID, "implement_review_started", 2, "advisory")
	seedImplementReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory", Verdict: "approve"})
	seedImplementReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "gpt-5.5", Authority: "advisory", Verdict: "approve_with_concerns", Concerns: []PlanReviewConcern{{Severity: "low", Category: "security", Note: "y"}}})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	assertBothModels := func(label string, rs *ReviewStatus) {
		t.Helper()
		if rs == nil || rs.Status != "complete" {
			t.Fatalf("%s = %+v, want complete", label, rs)
		}
		if len(rs.Reviews) != 2 {
			t.Fatalf("%s.Reviews = %+v, want both reviewer rows", label, rs.Reviews)
		}
		var sawOpus, sawCodex bool
		for _, rev := range rs.Reviews {
			switch rev.ReviewerModel {
			case "claude-opus-4-8":
				sawOpus = true
			case "gpt-5.5":
				sawCodex = true
			}
		}
		if !sawOpus || !sawCodex {
			t.Errorf("%s.Reviews must carry both opus and gpt-5.5 rows; got %+v", label, rs.Reviews)
		}
	}
	assertBothModels("plan_review_status", out.PlanReviewStatus)
	assertBothModels("implement_review_status", out.ImplementReviewStatus)
}

func TestGetRunStatus_NoImplementReviews_NilField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ImplementReviews != nil {
		t.Errorf("ImplementReviews should be nil with no entries; got %+v", out.ImplementReviews)
	}
}

func TestGetRunStatus_ImplementReviewSkipped_SurfacesSkippedVerdict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	// implement_review_skipped entry (reviewer not wired).
	payload, _ := json.Marshal(map[string]any{
		"reason":            "reviewer_not_configured",
		"configured_agents": 1,
		"authority":         "gating",
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.perRunAuditByRun[runID] = []AuditEntry{{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "implement_review_skipped",
		Payload:  decoded,
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if got := len(out.ImplementReviews); got != 1 {
		t.Fatalf("len(ImplementReviews) = %d, want 1", got)
	}
	if out.ImplementReviews[0].Verdict != "skipped" {
		t.Errorf("Verdict = %q, want skipped", out.ImplementReviews[0].Verdict)
	}
	if out.ImplementReviews[0].Reason != "reviewer_not_configured" {
		t.Errorf("Reason = %q, want reviewer_not_configured", out.ImplementReviews[0].Reason)
	}
}

func TestGetPlan_WithReviews_PopulatesField(t *testing.T) {
	// Two plan-review agent verdicts recorded on the plan's run.
	// Both must appear in Reviews with correct fields.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve",
	})
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns: []PlanReviewConcern{
			{Severity: "medium", Category: "scope", Note: "touching too many files"},
		},
		FreeForm: "Consider narrowing scope.",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if got := len(out.Reviews); got != 2 {
		t.Fatalf("len(Reviews) = %d, want 2", got)
	}
	if out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve", out.Reviews[0].Verdict)
	}
	if out.Reviews[0].Authority != "advisory" {
		t.Errorf("Reviews[0].Authority = %q, want advisory", out.Reviews[0].Authority)
	}
	if out.Reviews[1].Verdict != "approve_with_concerns" {
		t.Errorf("Reviews[1].Verdict = %q, want approve_with_concerns", out.Reviews[1].Verdict)
	}
	if got := len(out.Reviews[1].Concerns); got != 1 {
		t.Fatalf("len(Reviews[1].Concerns) = %d, want 1", got)
	}
	if out.Reviews[1].Concerns[0].Severity != "medium" {
		t.Errorf("Concerns[0].Severity = %q, want medium", out.Reviews[1].Concerns[0].Severity)
	}
	if out.Reviews[1].FreeForm != "Consider narrowing scope." {
		t.Errorf("Reviews[1].FreeForm = %q", out.Reviews[1].FreeForm)
	}
}

func TestGetPlan_WithSkippedReview_SurfacesSkippedVerdict(t *testing.T) {
	// A plan_review_skipped audit entry (#574) surfaces as a synthesized
	// review with verdict "skipped" and the recorded reason/authority.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	payload, _ := json.Marshal(map[string]any{
		"reason":            "reviewer_not_configured",
		"configured_agents": 1,
		"authority":         "gating",
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_review_skipped",
		Payload:  decoded,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if len(out.Reviews) != 1 {
		t.Fatalf("len(Reviews) = %d, want 1", len(out.Reviews))
	}
	got := out.Reviews[0]
	if got.Verdict != "skipped" {
		t.Errorf("Verdict = %q, want skipped", got.Verdict)
	}
	if got.Reason != "reviewer_not_configured" {
		t.Errorf("Reason = %q, want reviewer_not_configured", got.Reason)
	}
	if got.Authority != "gating" {
		t.Errorf("Authority = %q, want gating", got.Authority)
	}
	if got.ReviewerKind != "agent" {
		t.Errorf("ReviewerKind = %q, want agent", got.ReviewerKind)
	}
}

func TestGetPlan_NoReviewAuditEntries_ReviewsAbsent(t *testing.T) {
	// No plan_reviewed audit entries — Reviews should be nil so
	// it is omitted from the JSON response (omitempty).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	// perRunAuditByRun[runID] left empty.

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if out.Reviews != nil {
		t.Errorf("Reviews should be nil when no plan_reviewed entries exist; got %+v", out.Reviews)
	}
}

// TestGetPlan_AfterRevise_SurfacesOnlyRound2Reviews is the #1201 end-to-end
// done-means test: after a fishhawk_revise_plan re-opens the plan gate and a
// fresh round-2 review lands, getPlan must surface ONLY the post-revision
// verdicts through BOTH out.Reviews (loadPlanReviews) and
// out.PlanReviewStatus.Reviews (reviewStatusFor) — the stale round-1 reject
// must never leak into either. It crosses the full audit-read -> loadPlanReviews
// + reviewStatusFor -> GetPlanOutput seam. Before #1201 both surfaces returned
// the pre-revision round.
func TestGetPlan_AfterRevise_SurfacesOnlyRound2Reviews(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Round 1: a full 2-reviewer round that landed an approve_with_concerns and
	// a reject — the verdicts that prompted the operator to revise.
	seedReviewStartedAudit(fb, runID, "plan_review_started", 2, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory", Verdict: "approve_with_concerns", Concerns: []PlanReviewConcern{{Severity: "medium", Category: "scope", Note: "x"}}})
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "gpt-5.5", Authority: "advisory", Verdict: "reject"})

	// The operator revises the plan, re-opening the gate.
	seedRunPlanRevisedAudit(fb, runID)

	// Round 2: a fresh 2-reviewer round on the revised plan, both clean approves.
	seedReviewStartedAudit(fb, runID, "plan_review_started", 2, "advisory")
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory", Verdict: "approve"})
	seedPlanReviewAudit(fb, runID, PlanReview{ReviewerKind: "agent", ReviewerModel: "gpt-5.5", Authority: "advisory", Verdict: "approve"})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}

	// noReject asserts a Reviews slice carries exactly the two round-2 approves
	// and never the round-1 reject.
	noReject := func(label string, reviews []PlanReview) {
		t.Helper()
		if len(reviews) != 2 {
			t.Fatalf("%s = %+v, want exactly the two round-2 verdicts", label, reviews)
		}
		for _, rev := range reviews {
			if rev.Verdict != "approve" {
				t.Errorf("%s carries verdict %q, want only round-2 approve (the round-1 %q must be floored out)", label, rev.Verdict, rev.Verdict)
			}
		}
	}
	noReject("out.Reviews", out.Reviews)

	if out.PlanReviewStatus == nil || out.PlanReviewStatus.Status != "complete" {
		t.Fatalf("PlanReviewStatus = %+v, want complete", out.PlanReviewStatus)
	}
	noReject("out.PlanReviewStatus.Reviews", out.PlanReviewStatus.Reviews)
}

// seedScopePrecheckAudit marshals a SERVER-side ScopePrecheckPayload and
// feeds it back through the fake backend as a plan_scope_precheck audit
// entry. Using the real server type is the point of the seam test: it
// exercises the backend-write -> mcp-read JSON contract end to end, so a
// drift in either side's struct tags fails here rather than silently in
// production.
func seedScopePrecheckAudit(fb *fakeBackend, runID uuid.UUID, payload server.ScopePrecheckPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_scope_precheck",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_ScopePrecheck_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     2,
		Violations: []policy.Violation{
			{
				Constraint: "forbidden_paths",
				Detail:     `pattern ".github/workflows/**" matched`,
				Files:      []string{".github/workflows/ci.yml"},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2", out.ScopePrecheck.ScannedFiles)
	}
	if got := len(out.ScopePrecheck.Violations); got != 1 {
		t.Fatalf("len(Violations) = %d, want 1", got)
	}
	v := out.ScopePrecheck.Violations[0]
	if v.Constraint != "forbidden_paths" {
		t.Errorf("Constraint = %q, want forbidden_paths", v.Constraint)
	}
	if len(v.Files) != 1 || v.Files[0] != ".github/workflows/ci.yml" {
		t.Errorf("Files = %v, want [.github/workflows/ci.yml]", v.Files)
	}
}

// TestGetPlan_ScopePrecheck_ExemptedCountReachesSurface is the #2054
// binding-condition-2 seam assertion (resolves f828ec9c): the
// post-exemption ScannedFiles — generated/vendored paths dropped by
// policy.CountedFileCount — must reach the operator-visible surface
// (getPlan -> ScopePrecheck.scanned_files) intact. The seeded value is
// derived by the REAL exemption function over a mixed diff, marshaled as
// a genuine server.ScopePrecheckPayload, and read back through the REAL
// resolver, so a serialization OR selection divergence across the
// backend-write -> mcp-read boundary fails here rather than in
// production.
func TestGetPlan_ScopePrecheck_ExemptedCountReachesSurface(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// 2 non-generated + 2 generated (sqlc db + vendored): the exempted
	// count is 2. Derive it via the real matcher so the seam genuinely
	// couples CountedFileCount to the surface.
	mixed := policy.Diff{ChangedFiles: []policy.ChangedFile{
		{Path: "backend/a.go"},
		{Path: "backend/b.go"},
		{Path: "backend/internal/audit/db/queries.sql.go"},
		{Path: "backend/vendor/github.com/x/y.go"},
	}}
	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     policy.CountedFileCount(mixed),
		MaxFilesChanged:  30,
		Violations:       []policy.Violation{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2 (2 non-generated, 2 exempt)", out.ScopePrecheck.ScannedFiles)
	}
}

// TestGetPlan_ScopePrecheck_MinChangedFilesReachesSurface is the #2415 seam
// assertion: min_changed_files, written on the server-side
// ScopePrecheckPayload, round-trips through the backend-write -> mcp-read JSON
// contract and reaches fishhawk_get_plan's scope_precheck. A seeded value that
// DIFFERS from scanned_files proves the field is carried independently and not
// aliased to the declared count.
func TestGetPlan_ScopePrecheck_MinChangedFilesReachesSurface(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     4,
		MaxFilesChanged:  3,
		MinChangedFiles:  2, // rename-shaped: differs from scanned_files
		Violations:       []policy.Violation{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.MinChangedFiles != 2 {
		t.Errorf("MinChangedFiles = %d, want 2 (carried across the boundary independent of scanned_files)", out.ScopePrecheck.MinChangedFiles)
	}
	if out.ScopePrecheck.ScannedFiles != 4 {
		t.Errorf("ScannedFiles = %d, want 4", out.ScopePrecheck.ScannedFiles)
	}
}

func TestGetPlan_ScopePrecheck_NewestEntryWins(t *testing.T) {
	// A schema-retry run writes two plan_scope_precheck entries; the
	// authoritative one is the newest (last, sequence-ascending). The
	// first carries a violation; the second (the re-upload) is clean.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:   "feature_change",
		ScannedFiles: 1,
		Violations: []policy.Violation{
			{Constraint: "forbidden_paths", Detail: "stale", Files: []string{".github/workflows/ci.yml"}},
		},
	})
	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:   "feature_change",
		ScannedFiles: 2,
		Violations:   []policy.Violation{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2 (newest entry)", out.ScopePrecheck.ScannedFiles)
	}
	if len(out.ScopePrecheck.Violations) != 0 {
		t.Errorf("newest entry is clean; want zero violations, got %+v", out.ScopePrecheck.Violations)
	}
}

func TestGetPlan_ScopePrecheck_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the pre-check has no plan_scope_precheck
	// entry — ScopePrecheck must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck != nil {
		t.Errorf("ScopePrecheck should be nil with no entry; got %+v", out.ScopePrecheck)
	}
}

// seedSurfaceSweepAudit marshals a SERVER-side SurfaceSweepPayload and
// feeds it back through the fake backend as a plan_surface_sweep audit
// entry. Using the real server type is the point of the seam test (#618):
// it exercises the backend-write -> mcp-read JSON contract end to end, so a
// drift in either side's struct tags fails here rather than silently in
// production.
func seedSurfaceSweepAudit(fb *fakeBackend, runID uuid.UUID, payload server.SurfaceSweepPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_surface_sweep",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_SurfaceSweep_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 1,
		Findings: []server.SurfaceSweepFinding{
			{
				Pattern:         "actor @-mention render surfaces",
				TriggerPath:     "backend/internal/issuecomment/status_template.go",
				MissingSiblings: []string{"backend/internal/issuecomment/notifier.go"},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated")
	}
	if out.SurfaceSweep.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", out.SurfaceSweep.ScannedFiles)
	}
	if got := len(out.SurfaceSweep.Findings); got != 1 {
		t.Fatalf("len(Findings) = %d, want 1", got)
	}
	f := out.SurfaceSweep.Findings[0]
	if f.Pattern != "actor @-mention render surfaces" {
		t.Errorf("Pattern = %q", f.Pattern)
	}
	if f.TriggerPath != "backend/internal/issuecomment/status_template.go" {
		t.Errorf("TriggerPath = %q", f.TriggerPath)
	}
	if len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != "backend/internal/issuecomment/notifier.go" {
		t.Errorf("MissingSiblings = %v, want [notifier.go]", f.MissingSiblings)
	}
}

func TestGetPlan_SurfaceSweep_NewestEntryWins(t *testing.T) {
	// A schema-retry run writes two plan_surface_sweep entries; the
	// authoritative one is the newest (last, sequence-ascending). The first
	// carries a finding; the second (the re-upload) is clean.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 1,
		Findings: []server.SurfaceSweepFinding{
			{Pattern: "stale", TriggerPath: "x", MissingSiblings: []string{"y"}},
		},
	})
	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 2,
		Findings:     []server.SurfaceSweepFinding{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated")
	}
	if out.SurfaceSweep.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2 (newest entry)", out.SurfaceSweep.ScannedFiles)
	}
	if len(out.SurfaceSweep.Findings) != 0 {
		t.Errorf("newest entry is clean; want zero findings, got %+v", out.SurfaceSweep.Findings)
	}
}

func TestGetPlan_SurfaceSweep_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the sweep has no plan_surface_sweep entry —
	// SurfaceSweep must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep != nil {
		t.Errorf("SurfaceSweep should be nil with no entry; got %+v", out.SurfaceSweep)
	}
}

// TestGetPlan_SurfaceSweep_CrossSliceFindings exercises the #1102 seam:
// a server-side SurfaceSweepPayload carrying cross_slice_findings decodes
// into the MCP SurfaceSweep struct via the real backend write -> mcp read
// JSON contract, so a struct-tag drift fails here rather than in production.
func TestGetPlan_SurfaceSweep_CrossSliceFindings(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 1,
		Findings:     []server.SurfaceSweepFinding{},
		CrossSliceFindings: []server.CrossSliceCouplingFinding{
			{
				Pattern: "work-management schema requires every mirror",
				Slices: []server.CrossSliceClaim{
					{SliceTitle: "schema slice", Files: []string{"docs/spec/work-management-v0.schema.json"}},
					{SliceTitle: "wiring slice", Files: []string{"backend/internal/workmgmt/schemas/work-management-v0.schema.json"}},
				},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated")
	}
	if got := len(out.SurfaceSweep.CrossSliceFindings); got != 1 {
		t.Fatalf("len(CrossSliceFindings) = %d, want 1", got)
	}
	f := out.SurfaceSweep.CrossSliceFindings[0]
	if f.Pattern != "work-management schema requires every mirror" {
		t.Errorf("Pattern = %q", f.Pattern)
	}
	if len(f.Slices) != 2 {
		t.Fatalf("len(Slices) = %d, want 2", len(f.Slices))
	}
	if f.Slices[0].SliceTitle != "schema slice" || len(f.Slices[0].Files) != 1 ||
		f.Slices[0].Files[0] != "docs/spec/work-management-v0.schema.json" {
		t.Errorf("Slices[0] = %+v", f.Slices[0])
	}
}

// TestGetPlan_SurfaceSweep_LegacyPayloadDecodesEmpty pins backward
// compatibility (#1102): a plan_surface_sweep entry written before the
// cross_slice_findings field existed decodes to an empty slice, never an
// error, so the omitempty field is simply absent.
func TestGetPlan_SurfaceSweep_LegacyPayloadDecodesEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// A legacy payload with no cross_slice_findings key at all.
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_surface_sweep",
		Payload:  map[string]any{"scanned_files": float64(3), "findings": []any{}},
	})
	fb.mu.Unlock()

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated from the legacy entry")
	}
	if out.SurfaceSweep.ScannedFiles != 3 {
		t.Errorf("ScannedFiles = %d, want 3", out.SurfaceSweep.ScannedFiles)
	}
	if len(out.SurfaceSweep.CrossSliceFindings) != 0 {
		t.Errorf("legacy payload must decode CrossSliceFindings to empty; got %+v", out.SurfaceSweep.CrossSliceFindings)
	}
}

// seedTestSweepAudit marshals a SERVER-side TestSweepPayload and feeds it
// back through the fake backend as a plan_test_sweep audit entry. Using
// the real server type is the point of the seam test (#618): it exercises
// the backend-write -> mcp-read JSON contract end to end, so a drift in
// either side's struct tags fails here rather than silently in production.
func seedTestSweepAudit(fb *fakeBackend, runID uuid.UUID, payload server.TestSweepPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_test_sweep",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_TestSweep_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedTestSweepAudit(fb, runID, server.TestSweepPayload{
		ScannedFiles: 3,
		ListedDirs:   2,
		Findings: []server.TestSweepFinding{
			{
				Rule:         "stem_sibling",
				TriggerPath:  "backend/internal/server/upload.go",
				MissingTests: []string{"backend/internal/server/upload_test.go"},
			},
			{
				Rule:         "new_test_in_tested_package",
				TriggerPath:  "backend/internal/server/feature_test.go",
				MissingTests: []string{"backend/internal/server/a_test.go"},
				OmittedCount: 3,
			},
			{
				// #1031 path-trigger rule: the rule string must round-trip
				// the server-payload -> audit-entry -> MCP-decode seam
				// opaquely, like any other rule.
				Rule:         "migration_walk",
				TriggerPath:  "backend/internal/postgres/migrations/0032_x.up.sql",
				MissingTests: []string{"backend/internal/postgres/postgres_test.go"},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.TestSweep == nil {
		t.Fatal("TestSweep is nil; want populated")
	}
	if out.TestSweep.ScannedFiles != 3 || out.TestSweep.ListedDirs != 2 {
		t.Errorf("ScannedFiles/ListedDirs = %d/%d, want 3/2", out.TestSweep.ScannedFiles, out.TestSweep.ListedDirs)
	}
	if got := len(out.TestSweep.Findings); got != 3 {
		t.Fatalf("len(Findings) = %d, want 3", got)
	}
	f := out.TestSweep.Findings[0]
	if f.Rule != "stem_sibling" || f.TriggerPath != "backend/internal/server/upload.go" {
		t.Errorf("Findings[0] = %+v", f)
	}
	if len(f.MissingTests) != 1 || f.MissingTests[0] != "backend/internal/server/upload_test.go" {
		t.Errorf("MissingTests = %v, want [upload_test.go]", f.MissingTests)
	}
	if f.OmittedCount != 0 {
		t.Errorf("Findings[0].OmittedCount = %d, want 0", f.OmittedCount)
	}
	if out.TestSweep.Findings[1].OmittedCount != 3 {
		t.Errorf("Findings[1].OmittedCount = %d, want 3", out.TestSweep.Findings[1].OmittedCount)
	}
	mw := out.TestSweep.Findings[2]
	if mw.Rule != "migration_walk" || mw.TriggerPath != "backend/internal/postgres/migrations/0032_x.up.sql" {
		t.Errorf("Findings[2] = %+v, want the migration_walk finding", mw)
	}
	if len(mw.MissingTests) != 1 || mw.MissingTests[0] != "backend/internal/postgres/postgres_test.go" {
		t.Errorf("Findings[2].MissingTests = %v, want [backend/internal/postgres/postgres_test.go]", mw.MissingTests)
	}
}

func TestGetPlan_TestSweep_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the test sweep (or a fail-open no-op: non-
	// GitHub trigger, no GitHub client) has no plan_test_sweep entry —
	// TestSweep must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.TestSweep != nil {
		t.Errorf("TestSweep should be nil with no entry; got %+v", out.TestSweep)
	}
}

// seedPlanWarningsAudit marshals a SERVER-side PlanWarningsPayload and
// feeds it back through the fake backend as a plan_warnings audit entry.
// Using the real server type is the point of the seam test (#1684): it
// exercises the backend-write -> mcp-read JSON contract end to end, so a
// drift in either side's struct tags fails here rather than silently in
// production.
func seedPlanWarningsAudit(fb *fakeBackend, runID uuid.UUID, payload server.PlanWarningsPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_warnings",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

// TestGetPlan_PlanWarnings_CrossBoundarySeam pins the server -> MCP
// plan_warnings seam (#1684, binding condition 4): a server-side
// PlanWarningsPayload {warnings:[...]} round-trips through the audit log
// and getPlan surfaces it in GetPlanOutput.PlanWarnings.
func TestGetPlan_PlanWarnings_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedPlanWarningsAudit(fb, runID, server.PlanWarningsPayload{
		Warnings: []string{
			"decomposition has 2 sub-plans and none declares depends_on; if any slice forms a producer->consumer chain",
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if len(out.PlanWarnings) != 1 {
		t.Fatalf("len(PlanWarnings) = %d, want 1: %v", len(out.PlanWarnings), out.PlanWarnings)
	}
	if !strings.Contains(out.PlanWarnings[0], "none declares depends_on") {
		t.Errorf("PlanWarnings[0] = %q, want it to mention depends_on", out.PlanWarnings[0])
	}
}

// TestGetPlan_OverCapAdvisory_ReachesPlanWarningsField is the condition-2
// end-to-end assertion (#2053) exercising the REAL cross-boundary resolver.
// The sibling TestShipPlan_OverCapAdvisory_ReachesGetPlanField in
// backend/internal/server proves handleShipPlan -> runPlanWarnings PRODUCES
// the count-derived over-cap advisory payload, but it can only REPLICATE the
// get_plan selection contract (the resolver loadPlanWarnings lives here in
// package main, unimportable from the server package). This test closes that
// vacuity: a server-side PlanWarningsPayload carrying the over-cap advisory
// (the exact string overCapWarning emits, naming the scanned count and the
// resolved cap) round-trips through the audit log and surfaces via the actual
// getPlan/loadPlanWarnings path in GetPlanOutput.PlanWarnings — so a divergence
// in the real selection/serialization resolver would fail HERE.
func TestGetPlan_OverCapAdvisory_ReachesPlanWarningsField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// The over-cap advisory string, byte-for-byte as
	// server.(*Server).overCapWarning emits it (plan_warnings.go): scanned
	// count 4 exceeding the resolved cap of 3.
	const count, capLimit = 4, 3
	overCapAdvisory := fmt.Sprintf(
		"plan scope declares %d files, exceeding the implement-stage max_files_changed cap of %d — "+
			"narrow the scope or split the work into a decomposition before approving.",
		count, capLimit,
	)
	seedPlanWarningsAudit(fb, runID, server.PlanWarningsPayload{Warnings: []string{overCapAdvisory}})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if len(out.PlanWarnings) != 1 {
		t.Fatalf("len(PlanWarnings) = %d, want 1: %v", len(out.PlanWarnings), out.PlanWarnings)
	}
	got := out.PlanWarnings[0]
	if !strings.Contains(got, fmt.Sprintf("declares %d files", count)) ||
		!strings.Contains(got, fmt.Sprintf("cap of %d", capLimit)) {
		t.Errorf("PlanWarnings[0] = %q, want it to name count=%d and cap=%d", got, count, capLimit)
	}
}

// TestGetPlan_NearCapAdvisory_ReachesPlanWarningsField is the #2492 per-layer
// selection assertion, mirroring TestGetPlan_OverCapAdvisory_ReachesPlanWarningsField:
// a server-side PlanWarningsPayload carrying the near-cap advisory (the exact
// string server.(*Server).nearCapWarning emits, naming the scanned count, the
// resolved cap AND the remaining headroom) round-trips through the audit log and
// surfaces via the real getPlan/loadPlanWarnings path in GetPlanOutput.PlanWarnings.
//
// It pins the DECODE + field wiring on this side of the boundary. The genuine
// producer->consumer span — that runPlanWarnings PRODUCES exactly the string
// getPlan DELIVERS — is asserted end to end by
// TestE2E_PlanWarnings_NearCapAdvisory_ShipToGetPlan in
// backend/internal/integration/mcp, which drives the real ship-plan path into
// the real get_plan surface (runPlanWarnings and getPlan are unexported in their
// own packages, so only the integration package can reach both).
func TestGetPlan_NearCapAdvisory_ReachesPlanWarningsField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// The near-cap advisory string, byte-for-byte as
	// server.(*Server).nearCapWarning emits it (plan_warnings.go): scanned count 9
	// against the resolved cap of 10, leaving 1 file of headroom.
	const count, capLimit, headroom = 9, 10, 1
	nearCapAdvisory := fmt.Sprintf(
		"plan scope declares %d files against the implement-stage max_files_changed cap of %d — only %d file(s) of headroom remain. "+
			"Once that headroom is spent, the plan-approval scope-cap gate and the mid-stage scope-amendment headroom check refuse any "+
			"further file, so a correct mid-stage fix that needs an un-scoped file has no path in without a re-plan or a governed cap raise.",
		count, capLimit, headroom,
	)
	seedPlanWarningsAudit(fb, runID, server.PlanWarningsPayload{Warnings: []string{nearCapAdvisory}})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if len(out.PlanWarnings) != 1 {
		t.Fatalf("len(PlanWarnings) = %d, want 1: %v", len(out.PlanWarnings), out.PlanWarnings)
	}
	got := out.PlanWarnings[0]
	if !strings.Contains(got, fmt.Sprintf("declares %d files against the implement-stage max_files_changed cap of %d", count, capLimit)) ||
		!strings.Contains(got, fmt.Sprintf("only %d file(s) of headroom remain", headroom)) {
		t.Errorf("PlanWarnings[0] = %q, want it to name count=%d cap=%d headroom=%d", got, count, capLimit, headroom)
	}
}

// TestGetPlan_PlanWarnings_NewestEntryWins mirrors the sibling sweeps: a
// schema-retry run writes two plan_warnings entries; the authoritative one
// is the newest (last, sequence-ascending).
func TestGetPlan_PlanWarnings_NewestEntryWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedPlanWarningsAudit(fb, runID, server.PlanWarningsPayload{Warnings: []string{"stale warning"}})
	seedPlanWarningsAudit(fb, runID, server.PlanWarningsPayload{Warnings: []string{"fresh warning"}})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if len(out.PlanWarnings) != 1 || out.PlanWarnings[0] != "fresh warning" {
		t.Errorf("PlanWarnings = %v, want [\"fresh warning\"] (newest entry)", out.PlanWarnings)
	}
}

// TestGetPlan_PlanWarnings_AbsentWhenNoEntry is the no-fire surfacing
// case (binding condition 4): a warning-free plan (or an older run
// predating this pass) writes no plan_warnings entry, so PlanWarnings
// must be nil/omitted.
func TestGetPlan_PlanWarnings_AbsentWhenNoEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.PlanWarnings != nil {
		t.Errorf("PlanWarnings should be nil with no entry; got %v", out.PlanWarnings)
	}
}

// TestLoadPlanWarnings_CorruptPayload_DecodesNil pins the corrupt-decode
// degradation: a plan_warnings entry whose payload doesn't decode to the
// {warnings:[...]} shape is treated as "not present" rather than an error.
func TestLoadPlanWarnings_CorruptPayload_DecodesNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Corrupt entry: payload is a bare string, not an object carrying
	// "warnings".
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_warnings",
		Payload:  "not an object",
	})
	fb.mu.Unlock()

	r := newResolver(srv, nil)
	got, err := r.loadPlanWarnings(context.Background(), runID)
	if err != nil {
		t.Fatalf("loadPlanWarnings: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for a corrupt payload", got)
	}
}

func TestGetPlan_ReviewAuditError_Surfaced(t *testing.T) {
	// The per-run audit endpoint returns 500 → the error propagates
	// through loadPlanReviews and surfaces as a getPlan error.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	fb.perRunAuditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error when plan_reviewed audit query fails")
	}
	if !strings.Contains(err.Error(), "load plan reviews") {
		t.Errorf("error should mention 'load plan reviews': %v", err)
	}
}

func TestGetPlan_MalformedReviewPayload_Skipped(t *testing.T) {
	// An audit entry whose payload doesn't decode to PlanReview shape
	// is silently skipped; a valid entry that follows still appears.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Malformed entry: payload is a string, not an object.
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_reviewed",
		Payload:  "not-a-review-object",
	})
	// Valid entry follows.
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind: "agent",
		Authority:    "gating",
		Verdict:      "reject",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	// The malformed entry is skipped; only the valid entry appears.
	if got := len(out.Reviews); got != 1 {
		t.Fatalf("len(Reviews) = %d, want 1 (malformed entry skipped)", got)
	}
	if out.Reviews[0].Verdict != "reject" {
		t.Errorf("Reviews[0].Verdict = %q, want reject", out.Reviews[0].Verdict)
	}
}

func TestGetPlan_ReviewsQueryUsesResolvedRunID(t *testing.T) {
	// CI-retry: child run has no plan stage; parent has the plan.
	// Reviews should be queried against the PARENT run (the one
	// where the plan artifact lives), not the child.
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.New()
	parentPlanStage := uuid.New()

	parentIDStr := parentID.String()
	fb.getRunByID[childID] = Run{
		ID:          childID.String(),
		ParentRunID: &parentIDStr,
		State:       "running",
		Repo:        "x/y",
	}
	fb.getRunByID[parentID] = Run{ID: parentID.String(), State: "running", Repo: "x/y"}
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New().String(), RunID: childID.String(), Type: "implement", State: "running"},
	}
	fb.stagesByRun[parentID] = []Stage{
		{ID: parentPlanStage.String(), RunID: parentID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, parentPlanStage, samplePlanContent(), time.Hour)

	// Seed a review on the PARENT run.
	seedPlanReviewAudit(fb, parentID, PlanReview{
		ReviewerKind: "agent",
		Authority:    "advisory",
		Verdict:      "approve",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: childID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "parent:"+parentID.String() {
		t.Errorf("ResolvedVia = %q", out.ResolvedVia)
	}
	// Reviews come from the parent (the resolved run), not the child.
	if got := len(out.Reviews); got != 1 {
		t.Fatalf("len(Reviews) = %d, want 1 (review seeded on parent)", got)
	}
	if out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve", out.Reviews[0].Verdict)
	}
}

// --- get_run_status (E19.5 / #345) ---

func auditFixture(seq int64, runID uuid.UUID, category, actor string, offset time.Duration) AuditEntry {
	body, _ := json.Marshal(map[string]any{"actor": actor})
	return AuditEntry{
		ID:           uuid.New().String(),
		Sequence:     seq,
		RunID:        runID.String(),
		Timestamp:    time.Now().UTC().Add(-offset),
		Category:     category,
		ActorSubject: &actor,
		Payload:      body,
		EntryHash:    "h",
	}
}

func TestGetRunStatus_HappyPath_BundlesThreeReads(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running",
	}
	planStageID := uuid.New()
	implStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded",
			Executor: StageExecutor{Kind: "agent", Ref: "claude-code"}},
		{ID: implStageID.String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running",
			Executor: StageExecutor{Kind: "agent", Ref: "claude-code"}},
	}
	fb.auditByRun[runID] = []AuditEntry{
		// Returned time-descending — the fake serves what's there
		// without re-sorting; the production /v0/audit endpoint
		// orders so. Tests load these in the expected order.
		auditFixture(3, runID, "approval_submitted", "alice", 1*time.Minute),
		auditFixture(2, runID, "plan_generated", "system", 10*time.Minute),
		auditFixture(1, runID, "run_dispatched", "github-webhook", 15*time.Minute),
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, runID)
	}
	if len(out.Stages) != 2 {
		t.Fatalf("expected 2 stages; got %d", len(out.Stages))
	}
	if out.Stages[0].Type != "plan" || out.Stages[1].Type != "implement" {
		t.Errorf("stages not in sequence order: %+v", out.Stages)
	}
	if len(out.RecentAudit) != 3 {
		t.Fatalf("expected 3 audit rows; got %d", len(out.RecentAudit))
	}
	if out.RecentAudit[0].Category != "approval_submitted" {
		t.Errorf("first audit row should be newest (approval_submitted); got %q", out.RecentAudit[0].Category)
	}
}

// TestGetRunStatus_SucceededMerged_NextActionsState drives the full
// getRunStatus handler to pin the #1370 cross-binary seam: a SUCCEEDED run
// with an open PR URL whose recent-audit slice carries a post_merge_observed
// entry (the server lifecycle event) reclassifies next_actions to
// succeeded_merged — the operator post_merge dev-host step survives while the
// now-completed approve_pr / merge_pr ritual steps drop away. The negative
// mirror (no post_merge_observed → unchanged succeeded_pr_open) is asserted in
// the second sub-run so the gate is proven from BOTH sides through the handler.
func TestGetRunStatus_SucceededMerged_NextActionsState(t *testing.T) {
	prURL := "https://github.com/x/y/pull/42"
	seedRun := func(fb *fakeBackend, runID uuid.UUID, withObserved bool) {
		pr := prURL
		fb.getRunByID[runID] = Run{
			ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
			State: "succeeded", PullRequestURL: &pr,
		}
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.New().String(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.New().String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
			{ID: uuid.New().String(), RunID: runID.String(), Sequence: 3, Type: "review", State: "succeeded"},
		}
		audit := []AuditEntry{auditFixture(2, runID, "pr_merged", "alice", 2*time.Minute)}
		if withObserved {
			audit = append([]AuditEntry{auditFixture(3, runID, "post_merge_observed", "system", 1*time.Minute)}, audit...)
		}
		fb.auditByRun[runID] = audit
	}

	t.Run("merge observed -> succeeded_merged", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedRun(fb, runID, true)

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.NextActions == nil || out.NextActions.State != "succeeded_merged" {
			t.Fatalf("next_actions = %+v, want state succeeded_merged", out.NextActions)
		}
		var hasPostMerge bool
		for _, a := range out.NextActions.Actions {
			if a.Action == "approve_pr" || a.Action == "merge_pr" {
				t.Errorf("merge ritual action %q surfaced on a merged run", a.Action)
			}
			if a.Action == "post_merge" {
				hasPostMerge = true
			}
		}
		if !hasPostMerge {
			t.Errorf("succeeded_merged should still surface the operator post_merge step; got %+v", out.NextActions.Actions)
		}
	})

	t.Run("merge not observed -> succeeded_pr_open", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		seedRun(fb, runID, false)

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.NextActions == nil || out.NextActions.State != "succeeded_pr_open" {
			t.Fatalf("next_actions = %+v, want state succeeded_pr_open (no post_merge_observed)", out.NextActions)
		}
	})
}

// TestGetRunStatus_StageWaitStatus_PropagatesEndToEnd drives the full
// getRunStatus handler against the fake backend to cover the cross-layer seam
// (#879/#880, cf. #618): backend Stage.State -> stageWaitStatusFor derivation
// -> tool output rendering, as one flow. A running implement stage propagates
// status=="running" + poll_interval_seconds==30; a terminal (succeeded) plan
// stage omits the interval.
func TestGetRunStatus_StageWaitStatus_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	if out.PlanStageWaitStatus == nil {
		t.Fatal("PlanStageWaitStatus is nil")
	}
	if out.PlanStageWaitStatus.Status != "succeeded" {
		t.Errorf("plan status = %q, want succeeded", out.PlanStageWaitStatus.Status)
	}
	if out.PlanStageWaitStatus.PollIntervalSeconds != 0 {
		t.Errorf("plan poll_interval_seconds = %d, want 0 (terminal omits it)", out.PlanStageWaitStatus.PollIntervalSeconds)
	}

	if out.ImplementStageWaitStatus == nil {
		t.Fatal("ImplementStageWaitStatus is nil")
	}
	if out.ImplementStageWaitStatus.Status != "running" {
		t.Errorf("implement status = %q, want running", out.ImplementStageWaitStatus.Status)
	}
	if out.ImplementStageWaitStatus.PollIntervalSeconds != 30 {
		t.Errorf("implement poll_interval_seconds = %d, want 30", out.ImplementStageWaitStatus.PollIntervalSeconds)
	}
}

// TestGetRunStatus_StageWaitStatus_RunTerminalBackstop asserts the ADR-036
// (#874) backstop propagates through the handler: a running stage under a
// terminal run keeps 'running' but drops the poll interval.
func TestGetRunStatus_StageWaitStatus_RunTerminalBackstop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 1, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ImplementStageWaitStatus == nil {
		t.Fatal("ImplementStageWaitStatus is nil")
	}
	if out.ImplementStageWaitStatus.Status != "running" {
		t.Errorf("status = %q, want running", out.ImplementStageWaitStatus.Status)
	}
	if out.ImplementStageWaitStatus.PollIntervalSeconds != 0 {
		t.Errorf("poll_interval_seconds = %d, want 0 (run terminal -> backstop drops it)", out.ImplementStageWaitStatus.PollIntervalSeconds)
	}
}

// TestGetRunStatus_DriveStatus_PropagatesEndToEnd drives the full
// getRunStatus handler against the fake backend to cover the
// cross-layer seam (#1023, cf. #618): backend drive read surfaces
// (drive / derived_status / next_action / auto_advanced on
// GET /v0/runs/{id}) -> runDriveView decode -> drive_status rendering,
// as one flow.
func TestGetRunStatus_DriveStatus_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	// The QUALIFIED detail shape the backend now stamps when a reviewer has
	// rejected (#2487) — the same literal shape the server-side tests pin
	// from the backend side, so a wording change the MCP layer would swallow
	// is visible on both halves of the seam.
	const qualifiedDetail = "all blocking gates resolved and required checks are green; 1 advisory reject (latest review round) outstanding — read them with fishhawk_get_gate_view, then merge, route a fix-up with fishhawk_fixup_stage, or waive with fishhawk_waive_concern"
	fb.getRunExtraByID[runID] = map[string]any{
		"drive":          true,
		"derived_status": "awaiting_merge",
		"next_action": map[string]any{
			"action": "merge_pr",
			"detail": qualifiedDetail,
			"pr_url": "https://github.com/x/y/pull/42",
		},
		"auto_advanced": []map[string]any{
			{"rule": "plan_approved_dispatch", "from": "plan:approved", "to": "implement:dispatched", "ts": "2026-06-12T10:00:00Z"},
			{"rule": "checks_green_awaiting_merge", "from": "review:awaiting_approval", "to": "awaiting_merge", "ts": "2026-06-12T10:30:00Z"},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	ds := out.DriveStatus
	if ds == nil {
		t.Fatal("DriveStatus is nil, want the drive read view")
	}
	if !ds.Drive {
		t.Error("DriveStatus.Drive = false, want true")
	}
	if ds.DerivedStatus != "awaiting_merge" {
		t.Errorf("derived_status = %q, want awaiting_merge", ds.DerivedStatus)
	}
	if ds.NextAction == nil || ds.NextAction.Action != "merge_pr" || ds.NextAction.PRURL != "https://github.com/x/y/pull/42" {
		t.Errorf("next_action = %+v, want merge_pr with the PR URL", ds.NextAction)
	}
	// #2487: the qualified detail survives the HTTP decode into
	// DriveStatus.NextAction.Detail byte-for-byte.
	if ds.NextAction.Detail != qualifiedDetail {
		t.Errorf("next_action.detail = %q, want the qualified detail decoded unchanged", ds.NextAction.Detail)
	}
	if len(ds.AutoAdvanced) != 2 {
		t.Fatalf("auto_advanced = %+v, want 2 entries", ds.AutoAdvanced)
	}
	if ds.AutoAdvanced[0].Rule != "plan_approved_dispatch" || ds.AutoAdvanced[1].Rule != "checks_green_awaiting_merge" {
		t.Errorf("auto_advanced rules = [%q %q], want oldest-first order preserved",
			ds.AutoAdvanced[0].Rule, ds.AutoAdvanced[1].Rule)
	}
	if ds.AutoAdvanced[0].Timestamp.IsZero() {
		t.Error("auto_advanced[0].ts is zero, want the backend timestamp decoded")
	}
	// The Run mirror still rides along untouched.
	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, runID)
	}
	// #1024: the drive next_action folds into next_actions as the FIRST
	// entry, so the two surfaces never point different ways. E48.7 / #1954:
	// the drive-folded merge_pr is TRANSLATED to fishhawk_merge_run at the
	// MCP layer, while the drive_status block above keeps the persisted
	// merge_pr vocabulary (asserted at ds.NextAction.Action).
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatalf("NextActions = %+v, want the drive action folded in", out.NextActions)
	}
	if out.NextActions.Actions[0].Action != "fishhawk_merge_run" {
		t.Errorf("next_actions.actions[0] = %q, want the drive merge_pr translated to fishhawk_merge_run", out.NextActions.Actions[0].Action)
	}
	// #2487: the qualified detail folds into the merge action's reason
	// unchanged — the passthrough is what carries the reject warning to the
	// operator, so a change to the backend wording is visible here too.
	if out.NextActions.Actions[0].Reason != qualifiedDetail {
		t.Errorf("next_actions.actions[0].reason = %q, want the qualified detail folded through unchanged", out.NextActions.Actions[0].Reason)
	}
}

// TestGetRunStatus_NextActions_PlanGateParked is the get_run_status half
// of the #1024 wiring: the snapshot carries the next_actions block
// computed from the same run/stage/review reads, here at the parked plan
// gate (no review entries → review status none → approve/reject offered).
func TestGetRunStatus_NextActions_PlanGateParked(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "awaiting_approval"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "pending"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil {
		t.Fatal("NextActions is nil; want the #1024 block on every run")
	}
	if out.NextActions.State != "plan_gate_parked" {
		t.Errorf("next_actions.state = %q, want plan_gate_parked", out.NextActions.State)
	}
	names := make([]string, 0, len(out.NextActions.Actions))
	for _, a := range out.NextActions.Actions {
		names = append(names, a.Action)
	}
	if len(names) != 3 || names[0] != "fishhawk_approve_plan" || names[1] != "fishhawk_revise_plan" || names[2] != "fishhawk_reject_plan" {
		t.Errorf("next_actions.actions = %v, want [fishhawk_approve_plan fishhawk_revise_plan fishhawk_reject_plan]", names)
	}
}

// TestGetRunStatus_NextActions_TerminalRunNamesState pins the terminal
// shape: the block is still present naming the state, with no actions.
func TestGetRunStatus_NextActions_TerminalRunNamesState(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "cancelled"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "cancelled" {
		t.Fatalf("NextActions = %+v, want a block naming the terminal state cancelled", out.NextActions)
	}
	// E32.11 / #1737: a terminal FAILED/CANCELLED run is no longer actionless.
	// It carries exactly the operator-gated filing suggestion — the only legal
	// move left on a run with no recovery arm — pre-populated with the run id.
	// (A terminal SUCCESS-shaped state still carries no actions; that is what
	// the NextActions.Actions contract now says.)
	if got := actionNames(out.NextActions); len(got) != 1 || got[0] != "fishhawk_report_product_issue" {
		t.Fatalf("terminal run actions = %v, want exactly [fishhawk_report_product_issue]", got)
	}
	filing := out.NextActions.Actions[0]
	if filing.Params["run_id"] != runID.String() || filing.Consumes != consumesNone {
		t.Errorf("filing action = %+v, want run_id=%s consumes=none", filing, runID)
	}
}

// TestGetRunStatus_NonDriveRun_OmitsDriveStatus is the control: a run
// without drive surfaces (legacy / drive:false) renders no
// drive_status block at all.
func TestGetRunStatus_NonDriveRun_OmitsDriveStatus(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.DriveStatus != nil {
		t.Errorf("DriveStatus = %+v, want nil on a non-drive run", out.DriveStatus)
	}
}

// TestGetRunStatus_DriveRun_NoAdvancesYet pins the early-run shape: a
// drive:true run with no recorded transitions still gets the block
// (drive:true) with the lists empty — the operator can tell drive is
// armed before anything has advanced.
func TestGetRunStatus_DriveRun_NoAdvancesYet(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.getRunExtraByID[runID] = map[string]any{"drive": true}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.DriveStatus == nil || !out.DriveStatus.Drive {
		t.Fatalf("DriveStatus = %+v, want drive:true with empty surfaces", out.DriveStatus)
	}
	if out.DriveStatus.NextAction != nil || len(out.DriveStatus.AutoAdvanced) != 0 || out.DriveStatus.DerivedStatus != "" {
		t.Errorf("DriveStatus = %+v, want empty surfaces before any advance", out.DriveStatus)
	}
}

func TestGetRunStatus_StagesReSortedBySequence(t *testing.T) {
	// Defensive sort: even if the backend ever stops ordering by
	// sequence, the agent still sees the pipeline in order.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), Sequence: 3, Type: "review", State: "pending"},
		{ID: uuid.New().String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), Sequence: 2, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	got := []int{out.Stages[0].Sequence, out.Stages[1].Sequence, out.Stages[2].Sequence}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("stage sequences = %v, want [1,2,3]", got)
	}
}

func TestGetRunStatus_AuditLimit_DefaultsToFive(t *testing.T) {
	// audit_limit unset → request goes out with limit=5.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "5" {
		t.Errorf("audit request limit = %q, want 5", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_AuditLimit_ClampedToFifty(t *testing.T) {
	// audit_limit > 50 → request goes out with limit=50.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
		RunID:      runID.String(),
		AuditLimit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "50" {
		t.Errorf("audit request limit = %q, want 50 (clamped)", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_AuditLimit_ExplicitValueForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
		RunID:      runID.String(),
		AuditLimit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "20" {
		t.Errorf("audit request limit = %q, want 20", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_RejectsInvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_MissingRun_404Surfaced(t *testing.T) {
	// GetRun returns 404 → the wrapped error reaches the caller.
	fb, srv := newFakeBackend(t)
	fb.getStatus = http.StatusNotFound

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected 404 to surface")
	}
	if !strings.Contains(err.Error(), "get run") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_StagesEndpointError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected stages 500 to surface")
	}
	if !strings.Contains(err.Error(), "list stages") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_AuditEndpointError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.auditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected audit 500 to surface")
	}
	if !strings.Contains(err.Error(), "list recent audit") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_EmptyStagesAndAudit_OK(t *testing.T) {
	// Brand-new run before any stages or audit rows landed —
	// still returns Status=ok with empty arrays rather than erroring.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "pending"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s", out.Run.ID)
	}
	if got := len(out.Stages); got != 0 {
		t.Errorf("Stages length = %d, want 0", got)
	}
	if got := len(out.RecentAudit); got != 0 {
		t.Errorf("RecentAudit length = %d, want 0", got)
	}
}

func TestGetPlan_IgnoresNonStandardV1PlanArtifacts(t *testing.T) {
	// A plan stage might carry future-schema artifacts. The
	// resolver only returns standard_v1 — anything else is invisible
	// to v0's MCP tools.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"}}

	v := "future_v2"
	body, _ := json.Marshal(map[string]any{"plan_version": "future_v2"})
	fb.artifactsByStage[planStageID] = []Artifact{{
		ID: uuid.New().String(), StageID: planStageID.String(), Kind: "plan",
		SchemaVersion: &v, Content: body,
		CreatedAt: time.Now().UTC(),
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet (future schema is invisible)", out.Status)
	}
}

// --- list_audit (E19.6 / #346) ---

func TestListAudit_HappyPath_DefaultsLimit(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = []AuditEntry{
		auditFixture(1, runID, "run_dispatched", "github-webhook", 30*time.Minute),
		auditFixture(2, runID, "plan_generated", "system", 15*time.Minute),
		auditFixture(3, runID, "approval_submitted", "alice", 5*time.Minute),
	}

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("listAudit: %v", err)
	}
	if got := len(out.Items); got != 3 {
		t.Errorf("Items length = %d, want 3", got)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "limit=50") {
		t.Errorf("expected default limit=50; got %q", q)
	}
	// No filters → no category / stage_id / cursor in the query.
	for _, unwanted := range []string{"category=", "stage_id=", "cursor="} {
		if strings.Contains(q, unwanted) {
			t.Errorf("unfiltered call should not carry %q; got %q", unwanted, q)
		}
	}
}

func TestListAudit_FiltersForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	// A VALID base64 offset cursor (the fake now decodes it faithfully, as the
	// backend does, and 400s on junk like the old "tok-abc" fixture, #2695).
	cursor := encodeFakeOffsetCursor(10)
	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:    runID.String(),
		Category: "approval_submitted",
		StageID:  stageID.String(),
		Limit:    25,
		Cursor:   cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	for _, want := range []string{
		"category=approval_submitted",
		"stage_id=" + stageID.String(),
		"limit=25",
		"cursor=" + cursor,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %s", want, q)
		}
	}
}

func TestListAudit_Limit_ClampedTo200(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID: runID.String(),
		Limit: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "limit=200") {
		t.Errorf("limit should clamp to 200; got %q", q)
	}
}

// decodeFakeOffsetCursor decodes the fake audit endpoint's cursor. It mirrors
// the server's decodeOffsetCursor (backend/internal/server/reads.go) byte for
// byte: base64("offset:<n>"), empty → 0, malformed → error. Hand-rolled locally
// so the fake never imports server internals (#2695).
func decodeFakeOffsetCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("cursor is not valid base64")
	}
	var offset int
	if _, err := fmt.Sscanf(string(raw), "offset:%d", &offset); err != nil {
		return 0, fmt.Errorf("cursor is not in expected shape")
	}
	if offset < 0 {
		return 0, fmt.Errorf("cursor offset must be non-negative")
	}
	return offset, nil
}

// encodeFakeOffsetCursor is the fake's mirror of the server's
// encodeOffsetCursor (#2695).
func encodeFakeOffsetCursor(offset int) string {
	return base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf("offset:%d", offset)))
}

// fakePageOffset mirrors the server's pageOffset generic: items[offset:offset+limit]
// plus the encoded next-offset cursor, empty at the end (#2695).
func fakePageOffset(items []AuditEntry, offset, limit int) ([]AuditEntry, string) {
	if offset >= len(items) {
		return nil, ""
	}
	end := offset + limit
	if end >= len(items) {
		return items[offset:], ""
	}
	return items[offset:end], encodeFakeOffsetCursor(end)
}

// TestFakeBackend_PerRunAuditHonorsLimitAndCursor pins the fake audit endpoint's
// ascending/limit/base64-offset-cursor semantics against the backend handler's
// contract (reads.go handleListRunAudit). Without this the item-1 counterfactual
// (a >500-entry history that hides the fan-in marker beyond page one) would be
// vacuous — the fixture, not the fix, would decide the result (#2660 / #2695).
func TestFakeBackend_PerRunAuditHonorsLimitAndCursor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()

	const total = 600
	entries := make([]AuditEntry, 0, total)
	for i := 0; i < total; i++ {
		entries = append(entries, AuditEntry{
			ID:       uuid.NewString(),
			Sequence: int64(i + 1),
			RunID:    runID.String(),
			Category: "stage_advanced",
		})
	}
	fb.perRunAuditByRun[runID] = entries

	// Page 1: limit=500 returns 500 entries + a non-empty cursor.
	page1, next1, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{Limit: 500})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 500 {
		t.Fatalf("page 1 len = %d, want 500 (the endpoint's cap honored)", len(page1))
	}
	if next1 == "" {
		t.Fatal("page 1 next_cursor is empty; want a cursor pointing at the remaining entries")
	}
	if page1[0].Sequence != 1 || page1[499].Sequence != 500 {
		t.Errorf("page 1 is not ascending [1..500]: first=%d last=%d", page1[0].Sequence, page1[499].Sequence)
	}

	// Page 2: following the cursor yields the remaining 100 entries + no cursor.
	page2, next2, err := r.api.ListRunAudit(context.Background(), runID, ListRunAuditFilter{Limit: 500, Cursor: next1})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 100 {
		t.Fatalf("page 2 len = %d, want 100 (the remainder past the cursor)", len(page2))
	}
	if page2[0].Sequence != 501 || page2[99].Sequence != 600 {
		t.Errorf("page 2 is not the [501..600] remainder: first=%d last=%d", page2[0].Sequence, page2[99].Sequence)
	}
	if next2 != "" {
		t.Errorf("page 2 next_cursor = %q, want empty (end of the history)", next2)
	}
}

func TestListAudit_NextCursorPropagated(t *testing.T) {
	// Page 1 returns a next_cursor; the tool surfaces it so the
	// agent can call again with cursor=<token>. Verify both the
	// outbound forwarding and the inbound round-trip.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = []AuditEntry{
		auditFixture(1, runID, "run_dispatched", "github-webhook", time.Hour),
	}
	fb.perRunAuditNextByRun[runID] = "tok-page2"

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.NextCursor != "tok-page2" {
		t.Errorf("NextCursor = %q, want tok-page2", out.NextCursor)
	}

	// Round-trip: feed the cursor back in.
	_, _, err = r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:  runID.String(),
		Cursor: "tok-page2",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "cursor=tok-page2") {
		t.Errorf("page-2 call should forward cursor; got %q", q)
	}
}

func TestListAudit_BadRunUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "run_id") || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestListAudit_BadStageUUID_RejectedBeforeAPICall(t *testing.T) {
	// stage_id parses locally so a malformed input surfaces as a
	// clean tool error rather than a confusing backend 400.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:   uuid.New().String(),
		StageID: "nope",
	})
	if err == nil {
		t.Fatal("expected error on malformed stage_id")
	}
	if !strings.Contains(err.Error(), "stage_id") {
		t.Errorf("error should name the stage_id field: %v", err)
	}
	// Defensive: the backend must NOT have been hit when local
	// validation failed.
	if len(fb.perRunAuditLastQueryByID) != 0 {
		t.Errorf("backend hit despite local validation failure: %v", fb.perRunAuditLastQueryByID)
	}
}

func TestListAudit_BackendError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected 500 to surface")
	}
	if !strings.Contains(err.Error(), "list audit") {
		t.Errorf("error wording: %v", err)
	}
}

func TestListAudit_MissingRun_404Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.perRunAuditStatus = http.StatusNotFound

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected 404 to surface")
	}
}

func TestListAudit_EmptyPage_OK(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// perRunAuditByRun left empty for this id.
	_ = fb

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(out.Items); got != 0 {
		t.Errorf("expected empty items; got %d", got)
	}
	if out.NextCursor != "" {
		t.Errorf("empty page should have empty cursor; got %q", out.NextCursor)
	}
}

func TestClampListAuditLimit(t *testing.T) {
	// Centralized clamp logic — test directly without the full
	// tool flow so future tweaks have a fast feedback loop.
	cases := []struct {
		in, want int
	}{
		{0, listAuditLimitDefault},
		{-1, listAuditLimitDefault},
		{1, 1},
		{50, 50},
		{200, 200},
		{201, listAuditLimitMax},
		{99999, listAuditLimitMax},
	}
	for _, tc := range cases {
		if got := clampListAuditLimit(tc.in); got != tc.want {
			t.Errorf("clampListAuditLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// --- fishhawk_start_run (E22.1 / #390) ---

func TestStartRun_HappyPath_PostsBodyReturnsRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: "cli",
		TriggerRef:    "issue:42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Errorf("Run.ID empty; expected the fake to allocate one")
	}
	if out.Run.Repo != "x/y" || out.Run.WorkflowID != "feature_change" || out.Run.State != "pending" {
		t.Errorf("Run = %+v, want repo=x/y workflow=feature_change state=pending", out.Run)
	}
	if out.Idempotent {
		t.Errorf("Idempotent = true, want false (fresh create returns 201)")
	}
	// Backend received the right body.
	if fb.createRunBody.Repo != "x/y" {
		t.Errorf("backend got Repo = %q", fb.createRunBody.Repo)
	}
	if fb.createRunBody.WorkflowSHA != "deadbeef" {
		t.Errorf("backend got WorkflowSHA = %q", fb.createRunBody.WorkflowSHA)
	}
	if fb.createRunBody.TriggerSource != "cli" {
		t.Errorf("backend got TriggerSource = %q", fb.createRunBody.TriggerSource)
	}
	if fb.createRunBody.TriggerRef == nil || *fb.createRunBody.TriggerRef != "issue:42" {
		t.Errorf("backend got TriggerRef = %+v, want pointer to 'issue:42'", fb.createRunBody.TriggerRef)
	}
	if fb.createRunIdempKey != "" {
		t.Errorf("Idempotency-Key set without input: %q", fb.createRunIdempKey)
	}
}

func TestStartRun_TriggerSourceDefault_CLI(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:        "x/y",
		WorkflowID:  "feature_change",
		WorkflowSHA: "deadbeef",
		// TriggerSource omitted — defaults to "cli"
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.TriggerSource != "cli" {
		t.Errorf("default TriggerSource = %q, want cli", fb.createRunBody.TriggerSource)
	}
}

func TestStartRun_IdempotencyKey_SetsHeader(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  "cli",
		IdempotencyKey: "abc-123",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunIdempKey != "abc-123" {
		t.Errorf("Idempotency-Key header = %q, want abc-123", fb.createRunIdempKey)
	}
}

func TestStartRun_IdempotentReplay_FlagsTrue(t *testing.T) {
	// Backend returns 200 (instead of 201) to signal idempotent
	// replay. The MCP tool surfaces this on the Idempotent output
	// field so callers can branch.
	fb, srv := newFakeBackend(t)
	fb.createRunStatus = http.StatusOK
	r := newResolver(srv, nil)

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  "cli",
		IdempotencyKey: "abc-123",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !out.Idempotent {
		t.Errorf("Idempotent = false, want true (backend served 200)")
	}
}

func TestStartRun_BackendValidationError_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createRunStatus = http.StatusBadRequest
	fb.createRunErrBody = `{"error":{"code":"validation_failed","message":"repo is required","details":{"field":"repo"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y", // input passes our local validation
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: "cli",
	})
	if err == nil {
		t.Fatal("expected error from backend 400; got nil")
	}
	// Backend's typed error code should bubble through the wrap.
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Errorf("err = %v, want it to mention validation_failed", err)
	}
}

func TestStartRun_LocalValidationCatchesBadInputs(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	cases := []struct {
		name string
		in   StartRunInput
		want string
	}{
		{
			name: "missing repo",
			in:   StartRunInput{WorkflowID: "x", WorkflowSHA: "y", TriggerSource: "cli"},
			want: "repo is required",
		},
		{
			name: "missing workflow_id",
			in:   StartRunInput{Repo: "x/y", WorkflowSHA: "y", TriggerSource: "cli"},
			want: "workflow_id is required",
		},
		{
			name: "missing workflow_sha",
			in:   StartRunInput{Repo: "x/y", WorkflowID: "x", TriggerSource: "cli"},
			want: "workflow_sha is required",
		},
		{
			name: "bad trigger_source",
			in:   StartRunInput{Repo: "x/y", WorkflowID: "x", WorkflowSHA: "y", TriggerSource: "bogus"},
			want: "trigger_source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.startRun(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// --- fishhawk_start_run field parity (#426) ---

// validTrivialSpec is a minimal workflow YAML that passes the
// backend/internal/spec parser. Used across the parity tests so
// the auto-discover and inline-spec branches have a real bytes
// payload to ship.
const validTrivialSpec = "version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n"

// specAtVersion renders the smallest schema-valid workflow body at the
// given version string, mirroring backend/internal/spec's
// minimalSpecAtVersion test helper. The workflows map is valid on its
// own, so the ONLY possible schema violation is the version token —
// the staleness-hint tests below depend on their fixtures violating
// the spec exactly once BY CONSTRUCTION (#2326).
func specAtVersion(version string) string {
	return "version: \"" + version + "\"\n" + `workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
}

// TestStartRun_AutoDiscoversSpecFromWorkingDir exercises the
// headline #426 flow: an agent passes working_dir, the MCP server
// walks for .fishhawk/workflows.yaml, ships the bytes inline, and
// pre-computes workflow_sha so the agent doesn't need to.
func TestStartRun_AutoDiscoversSpecFromWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"),
		[]byte(validTrivialSpec), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:       "x/y",
		WorkflowID: "trivial",
		WorkingDir: dir,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Error("Run.ID empty")
	}
	if fb.createRunBody.WorkflowSpec != validTrivialSpec {
		t.Errorf("WorkflowSpec not forwarded; got %q", fb.createRunBody.WorkflowSpec)
	}
	if fb.createRunBody.WorkflowSHA != gitBlobSHA([]byte(validTrivialSpec)) {
		t.Errorf("WorkflowSHA = %q, want auto-computed %q",
			fb.createRunBody.WorkflowSHA, gitBlobSHA([]byte(validTrivialSpec)))
	}
}

// TestStartRun_InlineWorkflowSpec_SkipsDiscovery covers the
// "agent already has the bytes" path — the MCP server still
// validates + computes the SHA but doesn't touch the disk.
func TestStartRun_InlineWorkflowSpec_SkipsDiscovery(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.WorkflowSpec != validTrivialSpec {
		t.Errorf("inline spec not forwarded")
	}
	if fb.createRunBody.WorkflowSHA == "" {
		t.Error("SHA should be auto-computed when not provided")
	}
}

// TestStartRun_InlineSpec_InvalidYAMLFailsLocally surfaces the
// schema error without a backend round-trip. Matches the CLI's
// fast-fail UX.
func TestStartRun_InlineSpec_InvalidYAMLFailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: "not: valid: yaml: ::\n",
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "workflow_spec") {
		t.Errorf("err should reference workflow_spec: %v", err)
	}
}

// TestStartRun_InlineSpec_UnsupportedVersion_GainsStaleHint exercises
// proposal 3 of #1422 at the startRun call site: an inline spec whose
// version major is GENUINELY unsupported by the live binary surfaces
// the schema error WITH the staleness/`/mcp` hint (the self-diagnosing
// path for a silently-stale fishhawk-mcp), while the outer
// `workflow_spec:` prefix from the call-site wrap is preserved.
//
// The fixture is anchored on major 3 because major 2 became routable
// with workflow-v2 (ADR-067 / #2213) — a "2.0" spec is a SUPPORTED
// major failing its version enum, not an unsupported version — and the
// pre-#2326 two-violation fixture (`version: "2.0"` + `workflows: {}`)
// left which of the two unordered schema causes surfaced to a coin
// flip. An unrouted major fails closed in schemaForVersion BEFORE any
// schema validation runs, and specAtVersion's workflows body is
// schema-valid on its own, so this fixture has exactly one violation
// by construction. The day a workflow-v3 schema joins the
// embeddedSchemas routing table this anchor must move to the next
// unrouted major — the same maintenance note carried by spec's
// TestParse_UnsupportedMajorFailsClosed.
func TestStartRun_InlineSpec_UnsupportedVersion_GainsStaleHint(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: specAtVersion("3.0"),
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "workflow_spec") {
		t.Errorf("err should retain the workflow_spec prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "/mcp") {
		t.Errorf("unsupported-version error should gain the /mcp staleness hint: %v", err)
	}
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err should unwrap to *spec.SchemaError through the hint wrap: %v", err)
	}
	if se.Path != "/version" {
		t.Errorf("SchemaError.Path = %q, want \"/version\": %v", se.Path, err)
	}
	// Generic on the majors list (not the literal "0, 1, 2") so only the
	// version token moves when major 3 lands.
	if !strings.Contains(err.Error(), "unsupported spec version") ||
		!strings.Contains(err.Error(), "major 3") {
		t.Errorf("err should name the unsupported major: %v", err)
	}
	if strings.Contains(err.Error(), "/workflows") {
		t.Errorf("err mentions /workflows — the fixture must violate the spec exactly once, on /version: %v", err)
	}
}

// TestStartRun_InlineSpec_DottedV2Minor_GainsStaleHint covers the branch
// the pre-#2326 fixture actually exercised: a SUPPORTED major whose
// dotted minor form ("2.0") fails the workflow-v2 single-token version
// enum. resolveV2Reuse reports this deterministically BEFORE schema
// validation (#2216) as a *spec.SchemaError at Path "/version" with
// message "value must be '2'" — NOT an unsupported-version message,
// because major 2 routes. annotateStaleSpecError keys on the "/version"
// PATH generally, not on unsupported-major specifically, which is why
// this branch gains the /mcp hint too. The workflows body is
// schema-valid on its own (construction guard below), so the fixture
// has exactly one violation regardless of which layer reports it.
func TestStartRun_InlineSpec_DottedV2Minor_GainsStaleHint(t *testing.T) {
	// Construction guard: the same body under the valid single-token v2
	// version parses green, proving the dotted version token is this
	// fixture's only violation.
	if _, err := spec.ParseBytes([]byte(specAtVersion("2"))); err != nil {
		t.Fatalf("specAtVersion's workflows body must be schema-valid on its own: %v", err)
	}

	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: specAtVersion("2.0"),
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "workflow_spec") {
		t.Errorf("err should retain the workflow_spec prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "/mcp") {
		t.Errorf("dotted-v2-minor error should gain the /mcp staleness hint: %v", err)
	}
	var se *spec.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err should unwrap to *spec.SchemaError through the hint wrap: %v", err)
	}
	if se.Path != "/version" {
		t.Errorf("SchemaError.Path = %q, want \"/version\": %v", se.Path, err)
	}
	if !strings.Contains(err.Error(), "value must be '2'") {
		t.Errorf("err should carry the v2 version-enum message: %v", err)
	}
	if strings.Contains(err.Error(), "/workflows") {
		t.Errorf("err mentions /workflows — the fixture must violate the spec exactly once, on /version: %v", err)
	}
}

// TestStartRun_RunnerKindLocal_ForwardedToBackend covers the
// ADR-022 dimension: an agent minting a local-runner run passes
// runner_kind=local, the MCP forwards it verbatim.
func TestStartRun_RunnerKindLocal_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		RunnerKind:   "local",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.RunnerKind != "local" {
		t.Errorf("RunnerKind = %q, want local", fb.createRunBody.RunnerKind)
	}
}

// TestStartRun_BudgetOverride_ForwardedToBackend covers the #688
// admission-override dimension: an agent forcing a run past a blocking
// periodic budget passes budget_override=true, the MCP forwards it
// verbatim into the createRun request body.
func TestStartRun_BudgetOverride_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "trivial",
		WorkflowSpec:   validTrivialSpec,
		BudgetOverride: true,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !fb.createRunBody.BudgetOverride {
		t.Errorf("BudgetOverride = false, want true")
	}
}

// TestStartRun_UpstreamRunID_ForwardedToBackend asserts that a valid
// upstream_run_id UUID set on StartRunInput reaches the fake backend's
// captured createRunBody, crossing the MCP input -> StartRunParams ->
// createRunRequest -> JSON request body seam end-to-end.
func TestStartRun_UpstreamRunID_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	upstreamID := uuid.New().String()
	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		UpstreamRunID: upstreamID,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.UpstreamRunID != upstreamID {
		t.Errorf("UpstreamRunID = %q, want %q", fb.createRunBody.UpstreamRunID, upstreamID)
	}
}

// TestStartRun_UpstreamRunID_InvalidUUID_Errors asserts that a
// malformed upstream_run_id returns an error before calling the backend
// (local validation catches it, so the backend is never reached).
func TestStartRun_UpstreamRunID_InvalidUUID_Errors(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	prevBody := fb.createRunBody
	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		UpstreamRunID: "not-a-uuid",
	})
	if err == nil {
		t.Fatal("startRun: expected error for invalid UUID, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error %q does not mention 'not a valid UUID'", err.Error())
	}
	// Backend must not have been called (createRunBody unchanged).
	if fb.createRunBody != prevBody {
		t.Error("backend was called despite invalid upstream_run_id")
	}
}

// TestStartRun_IssueFetch_AutoFlipsTriggerSource exercises the
// gh-fetch convenience: when the agent passes issue, the MCP
// server fetches via gh and ships the payload inline, AND flips
// trigger_source to github_issue.
func TestStartRun_IssueFetch_AutoFlipsTriggerSource(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGh(t, `{"title":"Add foo","body":"We need foo helpers.","url":"https://github.com/x/y/issues/42","number":42}`)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		Issue:        "42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil {
		t.Fatal("IssueContext not forwarded")
	}
	if fb.createRunBody.IssueContext.Body != "We need foo helpers." {
		t.Errorf("body mismatch: %q", fb.createRunBody.IssueContext.Body)
	}
	if fb.createRunBody.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.createRunBody.TriggerSource)
	}
	if fb.createRunBody.TriggerRef == nil || *fb.createRunBody.TriggerRef != "issue:42" {
		t.Errorf("TriggerRef = %v, want issue:42", fb.createRunBody.TriggerRef)
	}
}

// TestStartRun_IssueContextInline_NoFetch confirms that when the
// agent already has an IssueContext (e.g. fetched once and reused
// across replays), the MCP server doesn't re-shell to gh.
func TestStartRun_IssueContextInline_NoFetch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Wire a gh that would explode if called.
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		t.Fatal("gh should NOT have been called when IssueContext is inline")
		return nil
	}
	t.Cleanup(func() { ghIssueCommand = exec.Command })

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		TriggerSource: "github_issue",
		IssueContext: &IssueContext{
			Title:  "Pre-fetched",
			Body:   "Inline body.",
			URL:    "https://github.com/x/y/issues/99",
			Number: 99,
		},
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil || fb.createRunBody.IssueContext.Number != 99 {
		t.Errorf("IssueContext not forwarded: %+v", fb.createRunBody.IssueContext)
	}
}

// TestStartRun_IssueContextRequiresGithubIssueSource mirrors the
// backend validation: issue_context is only valid with an ISSUE-ANCHORED
// trigger_source. The MCP server fails locally with a clean message instead of
// round-tripping to a 422.
//
// This is the RETAINED control (counterfactual (b), E54.22 / #2826): widening
// the coupling from github_issue-only to the issue-anchored set must NOT make
// it accept cli or ui. Delete the step-(7) check in tools.go and this test
// goes RED — and the backend is never called, which is asserted so a "failed
// locally" claim is not confused with a round-trip refusal.
func TestStartRun_IssueContextRequiresGithubIssueSource(t *testing.T) {
	for _, src := range []runpkg.TriggerSource{runpkg.TriggerUI, runpkg.TriggerCLI} {
		t.Run(string(src), func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)

			_, _, err := r.startRun(context.Background(), nil, StartRunInput{
				Repo:          "x/y",
				WorkflowID:    "trivial",
				WorkflowSpec:  validTrivialSpec,
				TriggerSource: string(src),
				IssueContext: &IssueContext{
					Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1,
				},
			})
			if err == nil {
				t.Fatalf("expected error for issue_context on trigger_source=%s", src)
			}
			if !strings.Contains(err.Error(), "issue_context") {
				t.Errorf("err should mention issue_context: %v", err)
			}
			if fb.createRunBody.Repo != "" {
				t.Errorf("backend was called despite the local refusal: %+v", fb.createRunBody)
			}
		})
	}
}

// TestStartRun_InvalidTriggerSourceNamesEverySource is AC5 at the MCP mirror
// (E54.22 / #2826): the tool error must name EVERY member of
// runpkg.ValidTriggerSources(), asserted by iterating the accessor rather than a
// literal, so this mirror and the server's set cannot drift apart.
func TestStartRun_InvalidTriggerSourceNamesEverySource(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
		TriggerSource: "bogus",
	})
	if err == nil {
		t.Fatal("expected a validation error for trigger_source=bogus")
	}
	for _, ts := range runpkg.ValidTriggerSources() {
		if !strings.Contains(err.Error(), string(ts)) {
			t.Errorf("error %v does not name accepted trigger source %q", err, ts)
		}
	}
}

// TestStartRun_OnDemandWithIssueContext_PassesThrough pins two things at once
// (E54.22 / #2826):
//
//   - an EXPLICIT trigger_source survives the step-(5) auto-flip even when an
//     issue/issue_context is present — the flip only fills an OMITTED value,
//     which is exactly what lets an operator start an issue-anchored on-demand
//     grooming run;
//   - the widened step-(7) pairing check admits issue_context on on_demand.
func TestStartRun_OnDemandWithIssueContext_PassesThrough(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGh(t, `{"title":"Groom","body":"Groom the backlog.","url":"https://github.com/x/y/issues/2826","number":2826}`)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		TriggerSource: string(runpkg.TriggerOnDemand),
		Issue:         "2826",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.TriggerSource != string(runpkg.TriggerOnDemand) {
		t.Errorf("TriggerSource = %q, want on_demand — an explicit value must NOT be overridden by the issue auto-flip", fb.createRunBody.TriggerSource)
	}
	if fb.createRunBody.IssueContext == nil || fb.createRunBody.IssueContext.Number != 2826 {
		t.Errorf("IssueContext not forwarded: %+v", fb.createRunBody.IssueContext)
	}
	if fb.createRunBody.TriggerRef == nil || *fb.createRunBody.TriggerRef != "issue:2826" {
		t.Errorf("TriggerRef = %v, want issue:2826 — the on-demand grooming run is issue-anchored", fb.createRunBody.TriggerRef)
	}

	// Inline issue_context (no gh hop) takes the same path.
	fb2, srv2 := newFakeBackend(t)
	r2 := newResolver(srv2, nil)
	if _, _, err := r2.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
		TriggerSource: string(runpkg.TriggerOnDemand),
		IssueContext:  &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1},
	}); err != nil {
		t.Fatalf("startRun with inline issue_context on on_demand: %v", err)
	}
	if fb2.createRunBody.IssueContext == nil {
		t.Error("inline IssueContext not forwarded on an on_demand run")
	}
}

// TestStartRun_GhMissing_DoesNotFail keeps the pre-#415 behavior
// alive: a missing gh emits a warning on the tool result but the
// run still mints. trigger_source still flips to github_issue
// because the agent asked for an issue-triggered run.
func TestStartRun_GhMissing_DoesNotFail(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGhMissing(t)

	meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		Issue:        "42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext != nil {
		t.Errorf("IssueContext should be nil when gh missing")
	}
	if fb.createRunBody.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.createRunBody.TriggerSource)
	}
	if meta == nil {
		t.Fatal("expected warning metadata on the tool result")
	}
}

// TestStartRun_OverCapCommentWarning drives the #2946 over-cap comment scan on
// the RESOLVED issue_context. The scan runs on both the inline-context path and
// the gh-fetch path; each case seeds bodies by construction (strings.Repeat of a
// known length) so the over/under-cap state is definitional, not derived from
// the control under test. The gh-missing warning is asserted to still surface so
// the new entries are ADDITIVE.
func TestStartRun_OverCapCommentWarning(t *testing.T) {
	over := func(n int) string { return strings.Repeat("z", prompt.MaxIssueCommentBytes+n) }
	under := strings.Repeat("z", prompt.MaxIssueCommentBytes) // exactly at cap → NOT over

	t.Run("inline_no_comments_no_warning", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
			TriggerSource: string(runpkg.TriggerOnDemand),
			IssueContext:  &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1},
		})
		if err != nil {
			t.Fatalf("startRun: %v", err)
		}
		if strings.Contains(rejectResultText(t, meta), "exceed the") {
			t.Errorf("no over-cap warning expected with no comments:\n%s", rejectResultText(t, meta))
		}
	})

	t.Run("inline_all_under_cap_including_exactly_at_cap_no_warning", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
			TriggerSource: string(runpkg.TriggerOnDemand),
			IssueContext: &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1,
				Comments: []IssueComment{
					{Author: "a", Body: "small", CreatedAt: "2026-05-01T00:00:00Z"},
					{Author: "b", Body: under, CreatedAt: "2026-05-02T00:00:00Z"}, // boundary: == cap, NOT over
				}},
		})
		if err != nil {
			t.Fatalf("startRun: %v", err)
		}
		if strings.Contains(rejectResultText(t, meta), "exceed the") {
			t.Errorf("exactly-at-cap comment must NOT warn (> not >=):\n%s", rejectResultText(t, meta))
		}
	})

	t.Run("inline_one_over_cap_one_warning_names_author_and_bytes", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
			TriggerSource: string(runpkg.TriggerOnDemand),
			IssueContext: &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1,
				Comments: []IssueComment{
					{Author: "alice", Body: over(500), CreatedAt: "2026-05-01T00:00:00Z"},
				}},
		})
		if err != nil {
			t.Fatalf("startRun: %v", err)
		}
		text := rejectResultText(t, meta)
		if !strings.Contains(text, "1 issue comment(s) exceed the") {
			t.Errorf("expected a single over-cap warning:\n%s", text)
		}
		if !strings.Contains(text, "@alice") || !strings.Contains(text, strconv.Itoa(prompt.MaxIssueCommentBytes+500)) {
			t.Errorf("warning must name the author and the byte count:\n%s", text)
		}
	})

	t.Run("inline_many_over_cap_bounded_detail_plus_summary", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		var comments []IssueComment
		total := maxOverCapDetailLines + 3 // 8 over-cap comments
		for i := 0; i < total; i++ {
			comments = append(comments, IssueComment{
				Author: "u" + strconv.Itoa(i), Body: over(i + 1),
				CreatedAt: "2026-05-01T00:00:00Z",
			})
		}
		meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
			TriggerSource: string(runpkg.TriggerOnDemand),
			IssueContext:  &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1, Comments: comments},
		})
		if err != nil {
			t.Fatalf("startRun: %v", err)
		}
		text := rejectResultText(t, meta)
		if !strings.Contains(text, strconv.Itoa(total)+" issue comment(s) exceed the") {
			t.Errorf("header must state the TRUE total %d:\n%s", total, text)
		}
		// Exactly maxOverCapDetailLines detail lines are named individually.
		detailLines := strings.Count(text, " over cap")
		if detailLines != maxOverCapDetailLines {
			t.Errorf("expected %d named detail lines, got %d:\n%s", maxOverCapDetailLines, detailLines, text)
		}
		if !strings.Contains(text, strconv.Itoa(total-maxOverCapDetailLines)+" more over-cap comment(s) not listed") {
			t.Errorf("remainder must be summarised as a count:\n%s", text)
		}
	})

	t.Run("gh_fetch_path_over_cap_warns_and_gh_present", func(t *testing.T) {
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		body := over(10)
		// gh JSON with one over-cap comment (author.login + createdAt shape).
		jsonBody := `{"title":"T","body":"B","url":"https://github.com/x/y/issues/9","number":9,"comments":[{"author":{"login":"bob"},"body":"` + body + `","createdAt":"2026-05-01T00:00:00Z"}]}`
		withFakeGh(t, jsonBody)
		meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec, Issue: "9",
		})
		if err != nil {
			t.Fatalf("startRun: %v", err)
		}
		text := rejectResultText(t, meta)
		if !strings.Contains(text, "1 issue comment(s) exceed the") || !strings.Contains(text, "@bob") {
			t.Errorf("gh-fetched over-cap comment must warn naming the author:\n%s", text)
		}
	})

	t.Run("gh_missing_warning_fires_and_overcap_is_additive", func(t *testing.T) {
		// The prior version installed withFakeGhMissing but supplied inline
		// IssueContext, so gh was NEVER consulted (startRun only fetches when
		// issueNumber > 0 && issueContext == nil) — the gh-missing warning never
		// fired and the case would have stayed green with that warning deleted.
		//
		// In startRun the gh-missing warning (needs a fetch attempt: an issue
		// number and NO inline context) and the over-cap warning (needs a
		// non-nil issue context) arise from MUTUALLY EXCLUSIVE branches: a
		// successful fetch yields context but no gh warning, a failed fetch
		// yields a gh warning but nil context. They cannot ride one result, so
		// each branch is exercised where it actually fires.

		// Branch 1 — fetch attempted with gh absent: the gh-missing warning
		// must surface. This is the assertion the prior case lacked; it reddens
		// if the "gh CLI not on PATH" warning is removed.
		_, srv := newFakeBackend(t)
		r := newResolver(srv, nil)
		withFakeGhMissing(t)
		meta1, _, err := r.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec, Issue: "5",
		})
		if err != nil {
			t.Fatalf("startRun (fetch path): %v", err)
		}
		text1 := rejectResultText(t, meta1)
		if !strings.Contains(text1, "gh CLI not on PATH") {
			t.Errorf("gh-missing fetch path must surface the gh-missing warning:\n%s", text1)
		}
		if strings.Contains(text1, "issue comment(s) exceed the") {
			t.Errorf("no over-cap warning expected when the failed fetch produced no context:\n%s", text1)
		}

		// Branch 2 — inline over-cap context (gh irrelevant): the over-cap
		// warning is APPENDED to the warning slice (overCapCommentWarnings is
		// additive), so it surfaces on the inline path.
		_, srv2 := newFakeBackend(t)
		r2 := newResolver(srv2, nil)
		withFakeGhMissing(t)
		meta2, _, err := r2.startRun(context.Background(), nil, StartRunInput{
			Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
			TriggerSource: string(runpkg.TriggerOnDemand),
			IssueContext: &IssueContext{Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1,
				Comments: []IssueComment{{Author: "alice", Body: over(5), CreatedAt: "2026-05-01T00:00:00Z"}}},
		})
		if err != nil {
			t.Fatalf("startRun (inline path): %v", err)
		}
		text2 := rejectResultText(t, meta2)
		if !strings.Contains(text2, "1 issue comment(s) exceed the") {
			t.Errorf("over-cap warning must surface on the inline path:\n%s", text2)
		}
	})
}

// TestStartRun_TriggerRefIssue_AutoDerivesNumber: when the agent
// passes trigger_ref=issue:7 without issue, the MCP server still
// fetches via gh.
func TestStartRun_TriggerRefIssue_AutoDerivesNumber(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGh(t, `{"title":"Auto","body":"Auto-derived.","url":"https://github.com/x/y/issues/7","number":7}`)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		TriggerRef:   "issue:7",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil || fb.createRunBody.IssueContext.Number != 7 {
		t.Errorf("IssueContext.Number not 7: %+v", fb.createRunBody.IssueContext)
	}
}

// TestStartRun_NoSpecNoSHA_FailsWithRemediation echoes the CLI's
// dual-remediation error when neither the discovery path nor an
// explicit workflow_sha can produce a SHA.
func TestStartRun_NoSpecNoSHA_FailsWithRemediation(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:       "x/y",
		WorkflowID: "trivial",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "workflow_sha") {
		t.Errorf("err should mention workflow_sha: %v", err)
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("err should suggest working_dir: %v", err)
	}
}

// TestStartRun_LegacyStagelessSeed_StillWorks documents the
// "test fixture / no checkout" path: pass repo + workflow_id +
// workflow_sha, no spec, no issue — backend creates a stage-less
// row. This is the pre-#411 behavior the MCP tool MUST preserve
// so integration tests that seed rows directly don't break.
func TestStartRun_LegacyStagelessSeed_StillWorks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:        "x/y",
		WorkflowID:  "trivial",
		WorkflowSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.WorkflowSpec != "" {
		t.Errorf("WorkflowSpec set when none provided: %q", fb.createRunBody.WorkflowSpec)
	}
	if fb.createRunBody.WorkflowSHA != "deadbeef" {
		t.Errorf("WorkflowSHA = %q, want deadbeef", fb.createRunBody.WorkflowSHA)
	}
}

// --- fishhawk_cancel_run (E22.2 / #391) ---

func TestCancelRun_HappyPath_TransitionsToCancelled(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y"}

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("cancelRun: %v", err)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
	if out.Run.ID != runID.String() {
		t.Errorf("ID = %q, want %s", out.Run.ID, runID.String())
	}
	if fb.cancelCalledByID[runID] != 1 {
		t.Errorf("cancel called %d times, want 1", fb.cancelCalledByID[runID])
	}
}

func TestCancelRun_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// The backend was never called — invalid UUID short-circuits.
	if len(fb.cancelCalledByID) != 0 {
		t.Errorf("backend cancel called %d times, want 0 (local validation should short-circuit)", len(fb.cancelCalledByID))
	}
}

func TestCancelRun_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.cancelStatus = http.StatusNotFound
	fb.cancelErrBody = `{"error":{"code":"run_not_found","message":"no run with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "run_not_found") {
		t.Errorf("err = %v, want it to mention run_not_found", err)
	}
}

func TestCancelRun_AlreadyTerminal_PropagatesConflict(t *testing.T) {
	// Cancelling a run that's already terminal (succeeded / failed)
	// surfaces the backend's `invalid_state_transition` code as a
	// tool error. The state machine is the source of truth.
	fb, srv := newFakeBackend(t)
	fb.cancelStatus = http.StatusConflict
	fb.cancelErrBody = `{"error":{"code":"invalid_state_transition","message":"cannot transition succeeded → cancelled","details":{"from":"succeeded","to":"cancelled"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 409; got nil")
	}
	if !strings.Contains(err.Error(), "invalid_state_transition") {
		t.Errorf("err = %v, want invalid_state_transition", err)
	}
}

func TestCancelRun_Idempotent_ReCancelSucceeds(t *testing.T) {
	// The backend treats re-cancel as idempotent (200 with the
	// cancelled run). The MCP tool surfaces both calls' results
	// without error.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled"}

	_, out1, err1 := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err1 != nil {
		t.Fatalf("first cancel: %v", err1)
	}
	_, out2, err2 := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err2 != nil {
		t.Fatalf("second cancel: %v", err2)
	}
	if out1.Run.State != "cancelled" || out2.Run.State != "cancelled" {
		t.Errorf("states = %q/%q, want cancelled/cancelled", out1.Run.State, out2.Run.State)
	}
	if fb.cancelCalledByID[runID] != 2 {
		t.Errorf("cancel called %d times, want 2", fb.cancelCalledByID[runID])
	}
}

// --- fishhawk_retry_stage (E22.3 / #392) ---

func TestRetryStage_HappyPath_CategoryA_PendingTransition(t *testing.T) {
	// Category-A retry (agent failure): backend flips failed →
	// pending and the orchestrator advances it. Test fixture
	// returns a Stage in State="pending" (the orchestrator advance
	// is a backend-internal concern).
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	runID := uuid.New()
	fb.retryResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  "implement",
		State: "pending",
	}

	_, out, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: stageID.String()})
	if err != nil {
		t.Fatalf("retryStage: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending", out.Stage.State)
	}
	if out.Stage.ID != stageID.String() {
		t.Errorf("ID = %q, want %s", out.Stage.ID, stageID.String())
	}
	if fb.retryCalledByID[stageID] != 1 {
		t.Errorf("retry called %d times, want 1", fb.retryCalledByID[stageID])
	}
}

func TestRetryStage_HappyPath_CategoryD_SLATimeout_BackToAwaitingApproval(t *testing.T) {
	// Category-D SLA-timeout retry flips the stage back to
	// awaiting_approval (no workflow_dispatch, just re-opens the
	// gate). Backend returns the stage in that state.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	fb.retryResp[stageID] = Stage{
		ID:    stageID.String(),
		Type:  "plan",
		State: "awaiting_approval",
	}

	_, out, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: stageID.String()})
	if err != nil {
		t.Fatalf("retryStage: %v", err)
	}
	if out.Stage.State != "awaiting_approval" {
		t.Errorf("State = %q, want awaiting_approval", out.Stage.State)
	}
}

func TestRetryStage_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// Local validation short-circuits — backend never called.
	if len(fb.retryCalledByID) != 0 {
		t.Errorf("backend retry called %d times, want 0", len(fb.retryCalledByID))
	}
}

func TestRetryStage_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.retryStatus = http.StatusNotFound
	fb.retryErrBody = `{"error":{"code":"stage_not_found","message":"no stage with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "stage_not_found") {
		t.Errorf("err = %v, want it to mention stage_not_found", err)
	}
}

func TestRetryStage_NotApplicable_CategoryB_PropagatesAs422(t *testing.T) {
	// Category-B (constraint / policy) retries are explicitly NOT
	// applicable — the workflow or spec needs to change first. The
	// backend surfaces this as a 422 with code retry_not_applicable;
	// the MCP tool propagates the error envelope verbatim.
	fb, srv := newFakeBackend(t)
	fb.retryStatus = http.StatusUnprocessableEntity
	fb.retryErrBody = `{"error":{"code":"retry_not_applicable","message":"category B failures require a workflow change"}}`
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "retry_not_applicable") {
		t.Errorf("err = %v, want retry_not_applicable", err)
	}
}

// --- fishhawk_approve_plan + fishhawk_reject_plan (E22.4 / #393) ---

// seedPlanStage installs a plan stage on the fakeBackend's stages-
// for-run map so the resolver finds it. Returns the stage id for
// downstream assertions.
func seedPlanStage(fb *fakeBackend, runID uuid.UUID) uuid.UUID {
	stageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: stageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval", Sequence: 0},
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "pending", Sequence: 1},
	}
	return stageID
}

func TestApprovePlan_HappyPath_ResolvesAndPostsApprove(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	// gh resolves the operator's real login (#751); the approve tool
	// threads it through as approver_github_login.
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.Stage.State)
	}
	if fb.approvalsBody.Decision != "approve" {
		t.Errorf("decision = %q, want approve", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "looks good" {
		t.Errorf("comment = %q, want 'looks good'", fb.approvalsBody.Comment)
	}
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Errorf("approvals call count = %d, want 1", fb.approvalsCalledByID[stageID])
	}
}

// TestApprovePlan_AddScopeFiles_PlumbedToSubmitApproval pins the #824 wire
// seam: ApprovePlanInput.AddScopeFiles must reach the approvals request body
// the backend decodes (MCP input -> client approvalRequest -> HTTP body).
func TestApprovePlan_AddScopeFiles_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	paths := []string{"backend/internal/agenteval/testdata/corpus/newcase/", "go.work"}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:         runID.String(),
		Reason:        "fold the fixture dir",
		AddScopeFiles: paths,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.AddScopeFiles, paths) {
		t.Errorf("add_scope_files = %v, want %v", fb.approvalsBody.AddScopeFiles, paths)
	}
}

// TestApprovePlan_RemoveScopeFiles_PlumbedToSubmitApproval pins the #1726 wire
// seam: ApprovePlanInput.RemoveScopeFiles must reach the approvals request body
// the backend decodes (MCP input -> client approvalRequest -> HTTP body),
// including the replace idiom (remove + add in the same call).
func TestApprovePlan_RemoveScopeFiles_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	removePaths := []string{"backend/old.go"}
	addPaths := []string{"backend/new.go"}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:            runID.String(),
		Reason:           "replace old with new",
		AddScopeFiles:    addPaths,
		RemoveScopeFiles: removePaths,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.RemoveScopeFiles, removePaths) {
		t.Errorf("remove_scope_files = %v, want %v", fb.approvalsBody.RemoveScopeFiles, removePaths)
	}
	if !reflect.DeepEqual(fb.approvalsBody.AddScopeFiles, addPaths) {
		t.Errorf("add_scope_files = %v, want %v (replace idiom)", fb.approvalsBody.AddScopeFiles, addPaths)
	}
}

// TestApprovePlan_NoRemoveScopeFiles_OmitsFieldOnTheWire confirms the
// byte-identical no-removal path: an approve without remove_scope_files leaves
// the field nil on the request body the backend decodes (omitempty).
func TestApprovePlan_NoRemoveScopeFiles_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.RemoveScopeFiles != nil {
		t.Errorf("remove_scope_files = %v, want nil when no removal declared", fb.approvalsBody.RemoveScopeFiles)
	}
}

// TestApprovePlan_ImplementModel_PlumbedToSubmitApproval pins the #1013 wire
// seam: ApprovePlanInput.ImplementModel must reach the approvals request body
// the backend decodes (MCP input -> client approvalRequest -> HTTP body).
func TestApprovePlan_ImplementModel_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:          runID.String(),
		Reason:         "use opus for this hard change",
		ImplementModel: "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ImplementModel != "claude-opus-4-8" {
		t.Errorf("implement_model = %q, want claude-opus-4-8", fb.approvalsBody.ImplementModel)
	}
}

// TestApprovePlan_NoImplementModel_OmitsFieldOnTheWire confirms the
// byte-identical no-override path: an approve without implement_model leaves
// the field empty on the request body the backend decodes (omitempty).
func TestApprovePlan_NoImplementModel_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ImplementModel != "" {
		t.Errorf("implement_model = %q, want empty when no override declared", fb.approvalsBody.ImplementModel)
	}
}

// TestApprovePlan_BindingAssertions_PlumbedToSubmitApproval pins the #1171
// wire seam: ApprovePlanInput.BindingAssertions must reach the approvals
// request body the backend decodes (MCP input -> client approvalRequest ->
// HTTP body), with the type/path/literal tags preserved.
func TestApprovePlan_BindingAssertions_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	assertions := []BindingAssertion{
		{Type: "file_contains", Path: "backend/internal/yaml/pad.go", Literal: "pad: 3"},
		{Type: "test_asserts", Path: "backend/internal/yaml/pad_test.go", Literal: "TestPad"},
	}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:             runID.String(),
		Reason:            "enforce the pad invariant",
		BindingAssertions: assertions,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.BindingAssertions, assertions) {
		t.Errorf("binding_assertions = %v, want %v", fb.approvalsBody.BindingAssertions, assertions)
	}
}

// TestApprovePlan_NoBindingAssertions_OmitsFieldOnTheWire confirms the
// byte-identical no-declaration path: an approve without binding_assertions
// leaves the field nil on the request body the backend decodes.
func TestApprovePlan_NoBindingAssertions_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.BindingAssertions != nil {
		t.Errorf("binding_assertions = %v, want nil when none declared", fb.approvalsBody.BindingAssertions)
	}
}

// TestApprovePlan_ClaimsConcernIDs_PlumbedToSubmitApproval pins the #1956
// wire seam: ApprovePlanInput.ClaimsConcernIDs must reach the approvals
// request body the backend decodes (MCP input -> client approvalRequest ->
// HTTP body).
func TestApprovePlan_ClaimsConcernIDs_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	claims := []string{uuid.New().String(), uuid.New().String()}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:            runID.String(),
		Reason:           "the retry cap is now enforced by this condition",
		ClaimsConcernIDs: claims,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.ClaimsConcernIDs, claims) {
		t.Errorf("claims_concern_ids = %v, want %v", fb.approvalsBody.ClaimsConcernIDs, claims)
	}
}

// TestApprovePlan_NoClaimsConcernIDs_OmitsFieldOnTheWire confirms the
// byte-identical no-claim path: an approve without claims_concern_ids leaves
// the field nil on the request body the backend decodes.
func TestApprovePlan_NoClaimsConcernIDs_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ClaimsConcernIDs != nil {
		t.Errorf("claims_concern_ids = %v, want nil when none declared", fb.approvalsBody.ClaimsConcernIDs)
	}
}

// TestApprovePlan_AmendAcceptanceCriteria_PlumbedToSubmitApproval pins the
// #2581 MCP half: the tool's amend_acceptance_criteria input reaches the
// approvals request body with every field intact, so the operator's retirement
// lands on the same approval row as the reason that motivated it.
func TestApprovePlan_AmendAcceptanceCriteria_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	amendments := []AcceptanceCriteriaAmendment{
		{ID: "crit-2", Action: "retire", Reason: "condition 1 dropped the surface this criterion asserts"},
	}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:                   runID.String(),
		Reason:                  "narrow the design to the resolver seam",
		AmendAcceptanceCriteria: amendments,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.AmendAcceptanceCriteria, amendments) {
		t.Errorf("amend_acceptance_criteria = %+v, want %+v",
			fb.approvalsBody.AmendAcceptanceCriteria, amendments)
	}
}

// TestApprovePlan_NoAmendAcceptanceCriteria_OmitsFieldOnTheWire confirms the
// byte-identical unused path: an approve without amendments leaves the field
// nil on the request body the backend decodes (#2581).
func TestApprovePlan_NoAmendAcceptanceCriteria_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.AmendAcceptanceCriteria != nil {
		t.Errorf("amend_acceptance_criteria = %+v, want nil when none declared",
			fb.approvalsBody.AmendAcceptanceCriteria)
	}
}

// TestApprovePlan_AmendAcceptanceCriteria_Advertised pins the wire-visible half:
// over a real in-memory MCP session, ListTools carries fishhawk_approve_plan
// with an amend_acceptance_criteria input property (#2581). No new tool is
// registered, so the tool-count assertion elsewhere is unchanged — this is the
// assertion that a no-op touch of the input struct would fail.
func TestApprovePlan_AmendAcceptanceCriteria_Advertised(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "fishhawk_approve_plan" {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatal("fishhawk_approve_plan is not registered/visible over ListTools")
	}
	schemaMap, ok := any(tool.InputSchema).(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_approve_plan InputSchema is %T, want a JSON object map", tool.InputSchema)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_approve_plan schema has no properties object; got %v", schemaMap["properties"])
	}
	if _, ok := props["amend_acceptance_criteria"]; !ok {
		t.Errorf("fishhawk_approve_plan input schema missing property amend_acceptance_criteria; got %v", props)
	}
}

func TestRejectPlan_HappyPath_ResolvesAndPostsReject(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: "scope is too wide",
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
	if out.Stage.State != "failed" {
		t.Errorf("State = %q, want failed", out.Stage.State)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "scope is too wide" {
		t.Errorf("comment = %q, want 'scope is too wide'", fb.approvalsBody.Comment)
	}
}

// rejectResultText concatenates every TextContent item on a tool result so a
// test can assert warnings that ride the result content regardless of ordering.
func rejectResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestRejectPlan_OverBudgetReason_WarnsOnResult pins the #2680 advisory warning:
// an over-cap rejection reason yields a tool-result warning naming the reason's
// byte count and the budget, AND the reject is STILL submitted (the advisory
// channel must never become a refusal — unlike the approve-only over-cap 400).
// Deleting the warning composition in rejectPlan reddens this.
func TestRejectPlan_OverBudgetReason_WarnsOnResult(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs") // clean login isolates the truncation warning

	reason := strings.Repeat("x", prompt.MaxRejectionFeedbackBytes+321)
	res, out, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: reason,
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	// The reject STILL submitted (advisory channel never refuses).
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject (over-cap reject must still submit)", fb.approvalsBody.Decision)
	}
	if out.StageID != stageID.String() {
		t.Errorf("StageID = %q, want %s", out.StageID, stageID.String())
	}
	text := rejectResultText(t, res)
	for _, w := range []string{
		strconv.Itoa(len(reason)),                      // reason byte count
		strconv.Itoa(prompt.MaxRejectionFeedbackBytes), // the budget
		"truncation marker",                            // not a silent cut
		"still submitted",                              // the reject landed
	} {
		if !strings.Contains(text, w) {
			t.Errorf("over-budget warning missing %q; got:\n%s", w, text)
		}
	}
}

// TestRejectPlan_UnderBudgetReason_NoWarning is the counterfactual for the
// threshold comparison (#2680): a reason exactly AT the cap warrants NO
// truncation warning (the > not >= boundary). Deleting the threshold comparison
// (warning unconditionally) reddens this.
func TestRejectPlan_UnderBudgetReason_NoWarning(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	res, _, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: strings.Repeat("x", prompt.MaxRejectionFeedbackBytes), // exactly at cap
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if txt := rejectResultText(t, res); strings.Contains(txt, "prior-rejection-feedback budget") {
		t.Errorf("at-cap reason must NOT warn; got:\n%s", txt)
	}
	// And it still submitted.
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
}

// TestRejectPlan_WarningThreshold_IsRejectionFeedbackCap is the drift pin
// (#2680): the warning boundary is keyed to prompt.MaxRejectionFeedbackBytes,
// not a re-typed literal — exactly-at-cap does not warn, one byte over does.
func TestRejectPlan_WarningThreshold_IsRejectionFeedbackCap(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		wantWarn bool
	}{
		{"at-cap", prompt.MaxRejectionFeedbackBytes, false},
		{"one-over", prompt.MaxRejectionFeedbackBytes + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)
			runID := uuid.New()
			seedPlanStage(fb, runID)
			withFakeGh(t, "kuhlman-labs")

			res, _, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
				RunID:  runID.String(),
				Reason: strings.Repeat("x", tc.size),
			})
			if err != nil {
				t.Fatalf("rejectPlan: %v", err)
			}
			warned := strings.Contains(rejectResultText(t, res), "prior-rejection-feedback budget")
			if warned != tc.wantWarn {
				t.Errorf("size %d (cap %d): warned=%v, want %v", tc.size, prompt.MaxRejectionFeedbackBytes, warned, tc.wantWarn)
			}
		})
	}
}

// TestRejectPlan_OverBudgetAndLoginWarnings_Coexist pins that the truncation
// warning and the approver-login warning both ride ONE result rather than one
// clobbering the other (#2680): an over-cap reason with an absent gh binary must
// surface BOTH, and the reject still submits.
func TestRejectPlan_OverBudgetAndLoginWarnings_Coexist(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t) // login warning fires

	reason := strings.Repeat("x", prompt.MaxRejectionFeedbackBytes+10)
	res, _, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: reason,
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	text := rejectResultText(t, res)
	if !strings.Contains(text, "gh CLI not on PATH") {
		t.Errorf("login warning clobbered by the truncation warning; got:\n%s", text)
	}
	if !strings.Contains(text, "prior-rejection-feedback budget") {
		t.Errorf("truncation warning clobbered by the login warning; got:\n%s", text)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
}

// TestRejectPlanReasonDescription_NamesBudget mirrors the approve-side drift pin
// (#2680): the reject tool's reason description must name the concrete
// 12000-byte budget so a calling agent knows it before writing a long reason.
func TestRejectPlanReasonDescription_NamesBudget(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
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

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema any
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_reject_plan" {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		t.Fatal("fishhawk_reject_plan not registered/visible over ListTools")
	}
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_reject_plan InputSchema is %T, want a JSON object map", schema)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("fishhawk_reject_plan schema has no properties object; got %v", schemaMap["properties"])
	}
	reasonProp, ok := props["reason"].(map[string]any)
	if !ok {
		t.Fatal("fishhawk_reject_plan schema has no 'reason' property")
	}
	desc, _ := reasonProp["description"].(string)
	if want := strconv.Itoa(prompt.MaxRejectionFeedbackBytes); !strings.Contains(desc, want) {
		t.Errorf("reject_plan 'reason' description must name the concrete budget %q; got:\n%s", want, desc)
	}
}

func TestApprovePlan_DuplicateSubmission_LabeledOutputAndLeadText(t *testing.T) {
	// #986: a duplicate-labeled 200 must reach the tool output as
	// duplicate_submission/prior_decision AND lead the result text with
	// an explicit no-op banner — the operator loop must never mistake a
	// duplicate for an effective approval.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	fb.approvalsRespBody = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"type":"plan","state":"succeeded","duplicate_submission":true,"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		stageID.String(), runID.String())

	res, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "second try without override",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !out.DuplicateSubmission {
		t.Errorf("DuplicateSubmission = false, want true")
	}
	if out.PriorDecision != "approve" {
		t.Errorf("PriorDecision = %q, want approve", out.PriorDecision)
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded (unchanged)", out.Stage.State)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected a tool result carrying the duplicate banner; got nil/empty")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.HasPrefix(text.Text, "duplicate submission — your prior approve decision") {
		t.Errorf("result text must lead with the duplicate banner, got %q", text.Text)
	}
	if !strings.Contains(text.Text, "gates were NOT re-run") {
		t.Errorf("result text must state gates did not re-run, got %q", text.Text)
	}
}

func TestRejectPlan_DuplicateSubmission_LabeledOutputAndLeadText(t *testing.T) {
	// Same #986 labeling on the reject tool: the prior decision named
	// in the banner is the EXISTING row's (approve), not this call's.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	fb.approvalsRespBody = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"type":"plan","state":"succeeded","duplicate_submission":true,"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		stageID.String(), runID.String())

	res, out, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: "changed my mind",
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if !out.DuplicateSubmission || out.PriorDecision != "approve" {
		t.Errorf("duplicate labeling = (%v, %q), want (true, approve)", out.DuplicateSubmission, out.PriorDecision)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected a tool result carrying the duplicate banner; got nil/empty")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.HasPrefix(text.Text, "duplicate submission — your prior approve decision") {
		t.Errorf("result text must lead with the duplicate banner, got %q", text.Text)
	}
}

func TestApprovePlan_NonDuplicate_NoBannerNoFlags(t *testing.T) {
	// The non-duplicate path is unchanged: a bare Stage 200 (no #986
	// keys) decodes to zero-valued duplicate fields and, with gh
	// resolving cleanly, a nil tool result.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	res, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if out.DuplicateSubmission || out.PriorDecision != "" {
		t.Errorf("duplicate labeling = (%v, %q), want zero values on a first submission", out.DuplicateSubmission, out.PriorDecision)
	}
	if res != nil {
		t.Errorf("tool result = %+v, want nil (no banner, no warning)", res)
	}
}

func TestApprovePlan_NoReason_PassesEmptyComment(t *testing.T) {
	// Reason is optional on approve; absent comment threads through
	// as an empty string. Backend treats empty comment as "no
	// comment recorded" per the existing approval row's nullable
	// comment column.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.Comment != "" {
		t.Errorf("comment = %q, want empty", fb.approvalsBody.Comment)
	}
}

// TestApprovePlan_ForwardsResolvedGithubLogin pins the #751 thread:
// the resolved gh login lands in the approval body's
// approver_github_login field for issue-thread `@`-mention rendering.
func TestApprovePlan_ForwardsResolvedGithubLogin(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
}

// TestApprovePlan_GhMissing_StillApprovesWithoutLogin keeps the
// approval best-effort (#751): a missing gh yields an empty login and
// a warning on the tool result, never a blocked approval.
func TestApprovePlan_GhMissing_StillApprovesWithoutLogin(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	meta, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan should not fail when gh is missing: %v", err)
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.Stage.State)
	}
	if fb.approvalsBody.ApproverGithubLogin != "" {
		t.Errorf("approver_github_login = %q, want empty when gh missing", fb.approvalsBody.ApproverGithubLogin)
	}
	if meta == nil || len(meta.Content) == 0 {
		t.Error("expected a warning on the tool result when gh is missing")
	}
}

func TestApprovePlan_NoPlanStage_FailsWithCleanError(t *testing.T) {
	// A run with no plan stage (e.g. a routine_change workflow that
	// skips planning) surfaces a clean tool error rather than a
	// generic "not found" from the backend.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	// Seed only an implement stage — no plan.
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "pending"},
	}

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error for run without plan stage")
	}
	if !strings.Contains(err.Error(), "no plan stage") {
		t.Errorf("err = %v, want it to mention 'no plan stage'", err)
	}
	// No approvals call should have fired — short-circuit on resolver failure.
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after resolver failure, want 0", len(fb.approvalsCalledByID))
	}
}

func TestApprovePlan_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// No stages call should have fired either.
	if len(fb.stagesCalledByID) != 0 {
		t.Errorf("list-stages called %d times, want 0", len(fb.stagesCalledByID))
	}
}

func TestApprovePlan_BackendStateMachineRefusal_PropagatesAsToolError(t *testing.T) {
	// E.g. plan stage isn't in awaiting_approval anymore. The
	// backend's state-machine rejects the approve; we surface the
	// error envelope verbatim.
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	fb.approvalsErrBody = `{"error":{"code":"invalid_state_transition","message":"plan stage not in awaiting_approval"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409")
	}
	if !strings.Contains(err.Error(), "invalid_state_transition") {
		t.Errorf("err = %v, want invalid_state_transition", err)
	}
}

// TestApprovePlan_AgentReviewPending_SurfacesPollUntilLanded pins the ADR-036
// (#875) consumer boundary: when the backend refuses the approve with a 409
// agent_review_pending (a configured agent plan review still in-flight), the
// tool surfaces a typed, operator-actionable poll-until-landed message that
// carries the landed/configured counts from the error details — not a generic
// wrap — and does NOT auto-retry.
func TestApprovePlan_AgentReviewPending_SurfacesPollUntilLanded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	fb.approvalsErrBody = `{"error":{"code":"agent_review_pending","message":"a configured agent plan review is still in-flight","details":{"stage_id":"x","configured_agents":2,"landed_terminal":1}}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409 agent_review_pending")
	}
	msg := err.Error()
	if !strings.Contains(msg, "agent_review_pending") {
		t.Errorf("err = %v, want agent_review_pending code", err)
	}
	// Carries the landed/configured counts from the details.
	if !strings.Contains(msg, "1 of 2") {
		t.Errorf("err = %v, want '1 of 2' landed/configured counts", err)
	}
	// Operator-actionable: points at the poll-until-terminal verbs and retry.
	if !strings.Contains(msg, "fishhawk_get_plan") || !strings.Contains(msg, "retry") {
		t.Errorf("err = %v, want poll-until-landed guidance", err)
	}
	// No auto-retry inside the tool: exactly one approvals call.
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Errorf("approvals call count = %d, want 1 (no auto-retry)", fb.approvalsCalledByID[stageID])
	}
}

// TestApprovePlan_AgentReviewPending_DegradesWhenDetailsAbsent covers the
// display edge flagged in the #875 implement review: when the backend's 409
// agent_review_pending carries no details (or unparseable counts), the tool
// must NOT print a misleading "0 of 0 landed" — it drops the count phrase but
// keeps the poll-until-landed guidance and the typed code.
func TestApprovePlan_AgentReviewPending_DegradesWhenDetailsAbsent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	// No details object at all — the degraded path.
	fb.approvalsErrBody = `{"error":{"code":"agent_review_pending","message":"a configured agent plan review is still in-flight"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409 agent_review_pending")
	}
	msg := err.Error()
	if !strings.Contains(msg, "agent_review_pending") {
		t.Errorf("err = %v, want agent_review_pending code", err)
	}
	// Must NOT claim a bogus "0 of 0" count when details are missing.
	if strings.Contains(msg, "0 of 0") {
		t.Errorf("err = %v, must not print misleading '0 of 0' count", err)
	}
	// Still operator-actionable: poll verbs + retry guidance present.
	if !strings.Contains(msg, "fishhawk_get_plan") || !strings.Contains(msg, "retry") {
		t.Errorf("err = %v, want poll-until-landed guidance", err)
	}
}

func TestRejectPlan_NoReason_PassesEmptyComment(t *testing.T) {
	// Reason is recommended on reject (CLI warns when missing) but
	// the MCP tool doesn't enforce — the audit log records the
	// absence and that's the source of truth.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "" {
		t.Errorf("comment = %q, want empty", fb.approvalsBody.Comment)
	}
}

// --- fishhawk_approve_deploy / fishhawk_reject_deploy (E23.15 / #1432) ---

// seedDeployStage installs a deploy stage (plus an upstream implement
// stage so the run looks plausible) on the fakeBackend's stages-for-run
// map so resolveDeployStage finds it. Returns the deploy stage id.
func seedDeployStage(fb *fakeBackend, runID uuid.UUID) uuid.UUID {
	stageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded", Sequence: 0},
		{ID: stageID.String(), RunID: runID.String(), Type: "deploy", State: "awaiting_deploy_approval", Sequence: 1},
	}
	return stageID
}

// TestApproveDeploy_HappyPath_ComposesEnvironmentIntoComment pins the core
// seam (#1432): environment is threaded to the backend only through the
// approval comment as --environment=<env>, which the deploy pre-flight
// parses. No --override-freeze unless OverrideFreeze is set.
func TestApproveDeploy_HappyPath_ComposesEnvironmentIntoComment(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedDeployStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "production",
		Reason:      "change merged, CI green",
	})
	if err != nil {
		t.Fatalf("approveDeploy: %v", err)
	}
	if fb.approvalsBody.Decision != "approve" {
		t.Errorf("decision = %q, want approve", fb.approvalsBody.Decision)
	}
	if !strings.Contains(fb.approvalsBody.Comment, "--environment=production") {
		t.Errorf("comment = %q, want it to contain --environment=production", fb.approvalsBody.Comment)
	}
	// --override-freeze must NOT appear when OverrideFreeze is unset.
	if strings.Contains(fb.approvalsBody.Comment, "--override-freeze") {
		t.Errorf("comment = %q, must NOT contain --override-freeze when OverrideFreeze is unset", fb.approvalsBody.Comment)
	}
	if !strings.Contains(fb.approvalsBody.Comment, "change merged, CI green") {
		t.Errorf("comment = %q, want it to carry the trimmed reason", fb.approvalsBody.Comment)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Errorf("approvals call count = %d, want 1", fb.approvalsCalledByID[stageID])
	}
}

// TestApproveDeploy_OverrideFreeze_AppendsFlag asserts the override_freeze
// flag composes --override-freeze into the comment the backend's
// commentHasFlag matches (the binding-condition-1 wiring: the backend
// parses --override-freeze the same whitespace-delimited-token way as
// --environment=).
func TestApproveDeploy_OverrideFreeze_AppendsFlag(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:          runID.String(),
		Environment:    "production",
		OverrideFreeze: true,
	})
	if err != nil {
		t.Fatalf("approveDeploy: %v", err)
	}
	if !strings.Contains(fb.approvalsBody.Comment, "--override-freeze") {
		t.Errorf("comment = %q, want it to contain --override-freeze when OverrideFreeze is set", fb.approvalsBody.Comment)
	}
	// The flag must be a standalone, whitespace-delimited token (the
	// backend's commentHasFlag does an exact token match, not substring).
	var sawToken bool
	for _, tok := range strings.Fields(fb.approvalsBody.Comment) {
		if tok == "--override-freeze" {
			sawToken = true
			break
		}
	}
	if !sawToken {
		t.Errorf("comment = %q, --override-freeze must be a standalone whitespace-delimited token", fb.approvalsBody.Comment)
	}
}

// TestApproveDeploy_MissingEnvironment_FailsLocally pins the local
// validation guard: an empty environment fails before any HTTP call.
func TestApproveDeploy_MissingEnvironment_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "   ", // whitespace-only is still empty
	})
	if err == nil {
		t.Fatal("expected local validation error for empty environment")
	}
	if !strings.Contains(err.Error(), "environment is required") {
		t.Errorf("err = %v, want 'environment is required'", err)
	}
	// No stages list and no approvals call should have fired.
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after local validation failure, want 0", len(fb.approvalsCalledByID))
	}
	if len(fb.stagesCalledByID) != 0 {
		t.Errorf("list-stages called %d times after local validation failure, want 0", len(fb.stagesCalledByID))
	}
}

// TestApproveDeploy_FreezeFlagSmuggledViaEnvironment_FailsLocally pins the
// injection guard (#1432 review): because the backend pre-flight parses
// whitespace-delimited tokens from the WHOLE comment, an Environment such as
// "production --override-freeze" would otherwise compose a standalone
// --override-freeze token and bypass the explicit OverrideFreeze control.
// The guard rejects an environment carrying embedded whitespace locally,
// before any HTTP call.
func TestApproveDeploy_FreezeFlagSmuggledViaEnvironment_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "production --override-freeze", // smuggle attempt
		// OverrideFreeze deliberately left false.
	})
	if err == nil {
		t.Fatal("expected local validation error for whitespace-bearing environment")
	}
	if !strings.Contains(err.Error(), "single whitespace-free token") {
		t.Errorf("err = %v, want 'single whitespace-free token'", err)
	}
	// The smuggling attempt must never reach the backend.
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after smuggling guard, want 0", len(fb.approvalsCalledByID))
	}
}

// TestApproveDeploy_FreezeFlagSmuggledViaReason_FailsLocally pins that a
// standalone --override-freeze token slipped through the free-form Reason
// is rejected when OverrideFreeze is false — closing the second injection
// vector named in the #1432 review.
func TestApproveDeploy_FreezeFlagSmuggledViaReason_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "production",
		Reason:      "ship it --override-freeze now", // smuggle attempt
		// OverrideFreeze deliberately left false.
	})
	if err == nil {
		t.Fatal("expected local validation error for --override-freeze in reason")
	}
	if !strings.Contains(err.Error(), "override_freeze is set") {
		t.Errorf("err = %v, want 'override_freeze is set'", err)
	}
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after smuggling guard, want 0", len(fb.approvalsCalledByID))
	}
}

// TestApproveDeploy_ReasonMentionsFlagSubstring_WhenOverrideSet allows a
// reason that legitimately references the flag once OverrideFreeze is set:
// the operator HAS requested the override, so the standalone token in reason
// is harmless and the call proceeds. This guards against the smuggling check
// over-rejecting the sanctioned case.
func TestApproveDeploy_ReasonMentionsFlagSubstring_WhenOverrideSet(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:          runID.String(),
		Environment:    "production",
		Reason:         "using --override-freeze for the hotfix",
		OverrideFreeze: true,
	})
	if err != nil {
		t.Fatalf("approveDeploy: %v", err)
	}
	if len(fb.approvalsCalledByID) != 1 {
		t.Errorf("approvals called %d times, want 1", len(fb.approvalsCalledByID))
	}
}

// TestApproveDeploy_EnvironmentNotAllowed_SurfacesAsToolError pins that a
// backend deploy_environment_not_allowed 422 propagates as a typed,
// operator-readable tool error.
func TestApproveDeploy_EnvironmentNotAllowed_SurfacesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusUnprocessableEntity
	fb.approvalsErrBody = `{"error":{"code":"deploy_environment_not_allowed","message":"requested deploy environment \"staging\" is not in the deploy stage's allowed_environments [production]"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "staging",
	})
	if err == nil {
		t.Fatal("expected error from backend 422 deploy_environment_not_allowed")
	}
	if !strings.Contains(err.Error(), "deploy_environment_not_allowed") {
		t.Errorf("err = %v, want deploy_environment_not_allowed code", err)
	}
}

// TestApproveDeploy_WriteDeployScope403_SurfacesAsToolError pins that the
// write:deploy 403 scope refusal (ADR-038/#1390) propagates as a tool
// error rather than being swallowed.
func TestApproveDeploy_WriteDeployScope403_SurfacesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusForbidden
	fb.approvalsErrBody = `{"error":{"code":"insufficient_scope","message":"deploy approval requires the write:deploy scope"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedDeployStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "production",
	})
	if err == nil {
		t.Fatal("expected error from backend 403 write:deploy")
	}
	if !strings.Contains(err.Error(), "write:deploy") {
		t.Errorf("err = %v, want it to surface the write:deploy scope refusal", err)
	}
}

// TestApproveDeploy_NoDeployStage_FailsWithCleanError pins the typed
// missing-deploy-stage error from resolveDeployStage: a plan-only run
// (no deploy stage) surfaces an operator-readable message.
func TestApproveDeploy_NoDeployStage_FailsWithCleanError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	// Seed only plan + implement — no deploy stage.
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "succeeded"},
	}

	_, _, err := r.approveDeploy(context.Background(), nil, ApproveDeployInput{
		RunID:       runID.String(),
		Environment: "production",
	})
	if err == nil {
		t.Fatal("expected error for run without deploy stage")
	}
	if !strings.Contains(err.Error(), "no deploy stage") {
		t.Errorf("err = %v, want it to mention 'no deploy stage'", err)
	}
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after resolver failure, want 0", len(fb.approvalsCalledByID))
	}
}

// TestRejectDeploy_HappyPath_NoEnvironmentInComment pins that a deploy
// reject posts decision=reject with no --environment in the comment — it
// routes through advanceStage, not the approve-only deploy pre-flight, so
// it needs neither write:deploy nor an environment.
func TestRejectDeploy_HappyPath_NoEnvironmentInComment(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedDeployStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.rejectDeploy(context.Background(), nil, RejectDeployInput{
		RunID:  runID.String(),
		Reason: "deploy window closed",
	})
	if err != nil {
		t.Fatalf("rejectDeploy: %v", err)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
	if strings.Contains(fb.approvalsBody.Comment, "--environment") {
		t.Errorf("comment = %q, must NOT contain --environment on a reject", fb.approvalsBody.Comment)
	}
	if fb.approvalsBody.Comment != "deploy window closed" {
		t.Errorf("comment = %q, want the raw reason", fb.approvalsBody.Comment)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
}

// TestRejectDeploy_NoDeployStage_FailsWithCleanError pins the typed
// missing-deploy-stage error on the reject path too.
func TestRejectDeploy_NoDeployStage_FailsWithCleanError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}

	_, _, err := r.rejectDeploy(context.Background(), nil, RejectDeployInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error for run without deploy stage")
	}
	if !strings.Contains(err.Error(), "no deploy stage") {
		t.Errorf("err = %v, want it to mention 'no deploy stage'", err)
	}
}

// TestResolveDeployStage_PicksDeployType pins the resolver returns the
// type=deploy stage out of a mixed stage list.
func TestResolveDeployStage_PicksDeployType(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedDeployStage(fb, runID)

	got, err := r.resolveDeployStage(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("resolveDeployStage: %v", err)
	}
	if got.Type != "deploy" {
		t.Errorf("resolved stage type = %q, want deploy", got.Type)
	}
	if got.ID != stageID.String() {
		t.Errorf("resolved stage id = %q, want %s", got.ID, stageID.String())
	}
}

// TestResolveDeployStage_InvalidUUID_FailsLocally pins the fast-path
// UUID parse on the input before the HTTP hop.
func TestResolveDeployStage_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, err := r.resolveDeployStage(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	if len(fb.stagesCalledByID) != 0 {
		t.Errorf("list-stages called %d times, want 0", len(fb.stagesCalledByID))
	}
}

// --- fishhawk_list_runs (E22.5 / #394) ---

func TestListRuns_HappyPath_ReturnsItemsAndCursor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id1, id2 := uuid.New(), uuid.New()
	fb.listResp = listRunsResult{
		Items: []Run{
			sampleRun(id1, "x/y", time.Hour),
			sampleRun(id2, "x/y", 2*time.Hour),
		},
		NextCursor: "b2Zmc2V0OjEw",
	}

	_, out, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if len(out.Items) != 2 {
		t.Errorf("got %d items, want 2", len(out.Items))
	}
	if out.NextCursor != "b2Zmc2V0OjEw" {
		t.Errorf("NextCursor = %q, want passthrough", out.NextCursor)
	}
}

func TestListRuns_NoFilters_DefaultsLimit(t *testing.T) {
	// Limit=0 input should clamp to listRunsLimitDefault (50) so
	// the agent doesn't accidentally fetch the entire chain.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.listResp = listRunsResult{Items: []Run{}}

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if !strings.Contains(fb.lastListQuery, "limit=50") {
		t.Errorf("query missing default limit: %s", fb.lastListQuery)
	}
}

func TestListRuns_ForwardsAllFilters(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.listResp = listRunsResult{Items: []Run{}}

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{
		Repo:       "x/y",
		WorkflowID: "feature_change",
		State:      "running",
		Limit:      25,
		Cursor:     "abc",
	})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	for _, want := range []string{
		"repo=x%2Fy",
		"workflow_id=feature_change",
		"state=running",
		"limit=25",
		"cursor=abc",
	} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestListRuns_BadState_FailsLocallyWithoutHTTPCall(t *testing.T) {
	// Closed-set check before the wire hop; backend would 400
	// either way, but local validation gives the agent a clearer
	// error.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{State: "bogus"})
	if err == nil {
		t.Fatal("expected validation error for bad state")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("err = %v, want it to mention state", err)
	}
	if fb.lastListQuery != "" {
		t.Errorf("backend should not be called on bad state; query = %q", fb.lastListQuery)
	}
}

func TestListRuns_LimitClamp(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, listRunsLimitDefault},
		{1, 1},
		{50, 50},
		{200, 200},
		{500, listRunsLimitMax},
		{-1, listRunsLimitDefault},
	} {
		if got := clampListRunsLimit(tc.in); got != tc.want {
			t.Errorf("clampListRunsLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestListRuns_CursorRoundTrip_WalksPagination(t *testing.T) {
	// Two-call pagination loop: first call returns a cursor;
	// second call feeds that cursor back and gets the next page.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id1, id2 := uuid.New(), uuid.New()

	// Seed two distinct responses keyed by query string.
	page1 := listRunsResult{
		Items:      []Run{sampleRun(id1, "x/y", time.Hour)},
		NextCursor: "next-page-cursor",
	}
	page2 := listRunsResult{
		Items:      []Run{sampleRun(id2, "x/y", 2*time.Hour)},
		NextCursor: "",
	}
	fb.listByQuery[`limit=50`] = page1
	fb.listByQuery[`cursor=next-page-cursor&limit=50`] = page2

	_, first, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.NextCursor != "next-page-cursor" {
		t.Fatalf("page 1 cursor = %q, want next-page-cursor", first.NextCursor)
	}
	_, second, err := r.listRuns(context.Background(), nil, ListRunsInput{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if second.NextCursor != "" {
		t.Errorf("page 2 NextCursor = %q, want empty (last page)", second.NextCursor)
	}
	if len(second.Items) != 1 || second.Items[0].ID != id2.String() {
		t.Errorf("page 2 items = %+v, want a single run with id %s", second.Items, id2.String())
	}
}

func TestListRuns_DefaultOmitsIssueContext(t *testing.T) {
	// Compact by default (#1098): a run carrying a populated
	// issue_context comes back with IssueContext nil so the
	// omitempty tag drops the heavy field from the marshalled output.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id := uuid.New()
	run := sampleRun(id, "x/y", time.Hour)
	run.IssueContext = &IssueContext{
		Title:    "long issue",
		Body:     "a very long body that would overflow the tool-result token cap",
		Number:   42,
		Comments: []IssueComment{{Author: "alice", Body: "a comment"}},
	}
	fb.listResp = listRunsResult{Items: []Run{run}}

	_, out, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Items))
	}
	if out.Items[0].IssueContext != nil {
		t.Errorf("IssueContext = %+v, want nil (compact by default)", out.Items[0].IssueContext)
	}
}

func TestListRuns_IncludeIssueContext_Preserved(t *testing.T) {
	// Opt-in (#1098): include_issue_context=true preserves the full
	// payload — body and comments survive the handler.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id := uuid.New()
	run := sampleRun(id, "x/y", time.Hour)
	run.IssueContext = &IssueContext{
		Title:    "long issue",
		Body:     "a very long body",
		Number:   42,
		Comments: []IssueComment{{Author: "alice", Body: "a comment"}},
	}
	fb.listResp = listRunsResult{Items: []Run{run}}

	_, out, err := r.listRuns(context.Background(), nil, ListRunsInput{IncludeIssueContext: true})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Items))
	}
	ic := out.Items[0].IssueContext
	if ic == nil {
		t.Fatal("IssueContext = nil, want preserved when include_issue_context=true")
	}
	if ic.Body != "a very long body" || len(ic.Comments) != 1 {
		t.Errorf("IssueContext payload not preserved: %+v", ic)
	}
}

// ── runtime_calibration tool ──────────────────────────────────────────────────

func TestRuntimeCalibration_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	fb.calibrationResp = CalibrationResult{
		Samples:             10,
		PredictedP50Minutes: 15.0,
		ActualP50Minutes:    12.0,
		ActualP95Minutes:    20.0,
		CalibrationRatio:    0.8,
		ConfidenceBandAccuracy: map[string]any{
			"medium": map[string]any{"samples": float64(10), "within_1.5x": float64(8)},
		},
	}

	_, out, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Samples != 10 {
		t.Errorf("Samples = %d, want 10", out.Samples)
	}
	if out.CalibrationRatio != 0.8 {
		t.Errorf("CalibrationRatio = %f, want 0.8", out.CalibrationRatio)
	}
	if out.ActualP95Minutes != 20.0 {
		t.Errorf("ActualP95Minutes = %f, want 20.0", out.ActualP95Minutes)
	}
}

func TestRuntimeCalibration_FiltersForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{
		WorkflowID: "my-workflow",
		StageType:  "implement",
		Since:      "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	q := fb.lastCalibrationQuery
	if !strings.Contains(q, "workflow_id=my-workflow") {
		t.Errorf("query %q missing workflow_id", q)
	}
	if !strings.Contains(q, "stage_type=implement") {
		t.Errorf("query %q missing stage_type", q)
	}
	if !strings.Contains(q, "since=") {
		t.Errorf("query %q missing since", q)
	}
}

func TestRuntimeCalibration_BackendError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	fb.calibrationStatus = http.StatusInternalServerError

	_, _, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

// --- budget status (#693 / ADR-030) ---
//
// Cross-boundary wire-to-tool seam: a stub backend serves
// GET /v0/runs/{id}/budget and the three consuming tools surface the
// same block (and omit it when the backend returns the empty no-budget
// object). Per-layer unit tests alone would pass while the field
// silently dropped at the seam (cf. #618), so these drive the full
// apiClient.GetRunBudget -> tool-output path.

func seedBudget(fb *fakeBackend, runID uuid.UUID, bs BudgetStatus) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.budgetByRun[runID] = bs
}

func warnFloat(f float64) *float64 { return &f }

func TestStartRun_SurfacesBudgetBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.createRunResp = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "pending"}
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", PeriodStart: "2026-06-01T00:00:00Z",
		LimitUSD: 50, SpentUSD: 165.86, Fraction: 3.3172,
		WarnAt: warnFloat(0.8), Tier: "over", Enforcement: "advisory",
	})

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced from backend; got nil")
	}
	if out.Budget.Tier != "over" || out.Budget.Enforcement != "advisory" {
		t.Errorf("budget = %+v, want tier=over enforcement=advisory", out.Budget)
	}
	if out.Budget.SpentUSD != 165.86 || out.Budget.LimitUSD != 50 {
		t.Errorf("budget spend/limit = %g/%g, want 165.86/50", out.Budget.SpentUSD, out.Budget.LimitUSD)
	}
}

func TestStartRun_OmitsBudgetWhenNoBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.createRunResp = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "pending"}
	// No seedBudget → backend returns {} → GetRunBudget yields nil.

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("expected no budget block; got %+v", out.Budget)
	}
	// The no-budget block must omit the JSON key entirely (nil pointer
	// + omitempty), not serialize a null.
	raw, _ := json.Marshal(out)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["budget"]; ok {
		t.Errorf("marshaled output must omit the budget key when no budget; got %s", raw)
	}
}

func TestGetRunStatus_SurfacesBudgetBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", LimitUSD: 100, SpentUSD: 60, Fraction: 0.6,
		WarnAt: warnFloat(0.5), Tier: "warn", Enforcement: "advisory",
	})

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced; got nil")
	}
	if out.Budget.Tier != "warn" {
		t.Errorf("budget tier = %q, want warn", out.Budget.Tier)
	}
}

func TestGetRunStatus_OmitsBudgetWhenNoBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("expected no budget block; got %+v", out.Budget)
	}
}

// TestGetRunStatus_SurfacesEscalatedTierAndAckRequired is the #1371
// cross-boundary seam (binding condition 2): the new escalating tier
// values (ack_required, page) and the new ack_required boolean must decode
// off the wire (the backend handler's budgetStatusResponse round-trips
// through JSON via the stub backend's GET /v0/runs/{id}/budget) and surface
// through get_run_status. A per-layer unit on either side would pass while
// the new field silently dropped at the MCP-mapping seam (cf. #618).
func TestGetRunStatus_SurfacesEscalatedTierAndAckRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier string
	}{
		{"ack_required", "ack_required"},
		{"page", "page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			r := newResolver(srv, nil)

			runID := uuid.New()
			fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
			seedBudget(fb, runID, BudgetStatus{
				Period: "weekly", LimitUSD: 100, SpentUSD: 250, Fraction: 2.5,
				WarnAt: warnFloat(0.8), Tier: tc.tier, AckRequired: true,
				Enforcement: "advisory",
			})

			_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
			if err != nil {
				t.Fatalf("getRunStatus: %v", err)
			}
			if out.Budget == nil {
				t.Fatal("expected budget block surfaced; got nil")
			}
			if out.Budget.Tier != tc.tier {
				t.Errorf("budget tier = %q, want %q", out.Budget.Tier, tc.tier)
			}
			if !out.Budget.AckRequired {
				t.Errorf("budget ack_required = false, want true at tier %q", tc.tier)
			}
		})
	}
}

// TestGetRunStatus_BudgetAckRequiredOmittedBelowRung confirms the omitempty
// seam: an ok/warn/over budget surfaces ack_required=false and the field is
// dropped from the marshaled tool output (never serialized as a stray
// false), so existing consumers stay byte-identical.
func TestGetRunStatus_BudgetAckRequiredOmittedBelowRung(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", LimitUSD: 100, SpentUSD: 60, Fraction: 0.6,
		WarnAt: warnFloat(0.5), Tier: "warn", Enforcement: "advisory",
	})

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced; got nil")
	}
	if out.Budget.AckRequired {
		t.Error("ack_required must be false at the warn tier")
	}
	raw, _ := json.Marshal(out.Budget)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["ack_required"]; ok {
		t.Errorf("marshaled budget must omit ack_required when false; got %s", raw)
	}
}

// --- cache efficiency (ADR-044 slice 3 / #1352) ---
//
// Cross-boundary wire-to-tool seam: a stub backend serves
// GET /v0/runs/{id}/cache-efficiency and getRunStatus surfaces the same
// block (and omits it when the backend returns the empty no-data object).
// Per-layer unit tests alone would pass while the field silently dropped at
// the seam (cf. #618), so this drives the full apiClient.GetRunCacheEfficiency
// -> tool-output path. The compute → endpoint layers are covered by the cost
// package and server handler tests; this asserts the MCP-mapping seam.

func seedCacheEfficiency(fb *fakeBackend, runID uuid.UUID, ce CacheEfficiency) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.cacheEffByRun[runID] = ce
}

func TestGetRunStatus_SurfacesCacheEfficiencyBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	seedCacheEfficiency(fb, runID, CacheEfficiency{
		FreshInputTokens: 4_000_000, CacheReadTokens: 4_000_000, CacheWriteTokens: 2_000_000,
		CacheReadRatio: 0.5, ReuseFactor: 2.0,
		GrossReadSavingsUSD: 18.0, WritePenaltyUSD: 1.25, NetSavingsUSD: 16.75,
		Stages: []CacheEfficiencyStage{
			{Source: "agent", FreshInputTokens: 2_000_000},
			{Source: "implement_review", CacheReadRatio: 0.5, NetSavingsUSD: 4.5},
			{Source: "plan_review", CacheReadRatio: 0.75, ReuseFactor: 3.0, NetSavingsUSD: 12.25},
		},
	})

	// include_cache_stages restores the per-stage breakdown this test asserts;
	// the compact default collapses stages[] to the rollup (#1749, covered by
	// TestGetRunStatus_CompactDefault_CollapsesCacheStages).
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeCacheStages: true})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.CacheEfficiency == nil {
		t.Fatal("expected cache_efficiency block surfaced from backend; got nil")
	}
	if out.CacheEfficiency.CacheReadRatio != 0.5 || out.CacheEfficiency.ReuseFactor != 2.0 {
		t.Errorf("cache efficiency ratios = read %g reuse %g, want 0.5/2.0",
			out.CacheEfficiency.CacheReadRatio, out.CacheEfficiency.ReuseFactor)
	}
	if out.CacheEfficiency.NetSavingsUSD != 16.75 {
		t.Errorf("net savings = %g, want 16.75", out.CacheEfficiency.NetSavingsUSD)
	}
	if len(out.CacheEfficiency.Stages) != 3 {
		t.Fatalf("want 3 stage rows, got %d: %+v", len(out.CacheEfficiency.Stages), out.CacheEfficiency.Stages)
	}
	if out.CacheEfficiency.Stages[2].Source != "plan_review" || out.CacheEfficiency.Stages[2].NetSavingsUSD != 12.25 {
		t.Errorf("plan_review stage = %+v, want source plan_review net 12.25", out.CacheEfficiency.Stages[2])
	}
}

func TestGetRunStatus_OmitsCacheEfficiencyWhenNoData(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	// No seedCacheEfficiency → backend returns {} → GetRunCacheEfficiency yields nil.

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.CacheEfficiency != nil {
		t.Errorf("expected no cache_efficiency block; got %+v", out.CacheEfficiency)
	}
	// The no-data block must omit the JSON key entirely (nil pointer +
	// omitempty), not serialize a null.
	raw, _ := json.Marshal(out)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["cache_efficiency"]; ok {
		t.Errorf("marshaled output must omit the cache_efficiency key when no data; got %s", raw)
	}
}

// --- run cost (#1372) ---
//
// Cross-boundary wire-to-tool seam: a stub backend serves GET
// /v0/runs/{id}/cost and getRunStatus surfaces the same block (and omits it
// when the backend returns the empty no-data object). The compute → endpoint
// layers — the cost_recorded-ledger aggregation, the pr_merged + PR-URL merge
// detection, and the two-runs-on-one-PR rollup sum — are covered end-to-end by
// the cost package (AggregateRunCost) and the server handler tests
// (cost_test.go); this asserts the MCP-mapping seam so the per-stage breakdown
// and merged-PR rollup don't silently drop on the wire (cf. #618).

func seedRunCost(fb *fakeBackend, runID uuid.UUID, rc RunCost) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.costByRun[runID] = rc
}

func TestGetRunStatus_SurfacesCostBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	url := "https://github.com/x/y/pull/7"
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "succeeded"}
	// Mirrors the cost_test.go merged-PR case: a per-stage breakdown summing to
	// the total, plus a two-run merged-PR rollup (5.00 + 3.00 = 8.00).
	seedRunCost(fb, runID, RunCost{
		TotalCostUSD: 5.00,
		Stages: []RunCostStage{
			{Source: "agent", CostUSD: 4.00},
			{Source: "implement_review", CostUSD: 0.50},
			{Source: "plan_review", CostUSD: 0.50},
		},
		MergedPR: &RunMergedPRCost{
			PullRequestURL:     url,
			CostPerMergedPRUSD: 8.00,
			RunCount:           2,
		},
	})

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Cost == nil {
		t.Fatal("expected cost block surfaced from backend; got nil")
	}
	if out.Cost.TotalCostUSD != 5.00 {
		t.Errorf("total_cost_usd = %g, want 5.00", out.Cost.TotalCostUSD)
	}
	if len(out.Cost.Stages) != 3 {
		t.Fatalf("want 3 stage rows, got %d: %+v", len(out.Cost.Stages), out.Cost.Stages)
	}
	if out.Cost.Stages[0].Source != "agent" || out.Cost.Stages[0].CostUSD != 4.00 {
		t.Errorf("agent stage = %+v, want source agent cost 4.00", out.Cost.Stages[0])
	}
	if out.Cost.MergedPR == nil {
		t.Fatal("expected merged_pr rollup surfaced; got nil")
	}
	if out.Cost.MergedPR.PullRequestURL != url {
		t.Errorf("merged_pr.pull_request_url = %q, want %q", out.Cost.MergedPR.PullRequestURL, url)
	}
	if out.Cost.MergedPR.CostPerMergedPRUSD != 8.00 || out.Cost.MergedPR.RunCount != 2 {
		t.Errorf("merged_pr rollup = cost %g run_count %d, want 8.00 / 2",
			out.Cost.MergedPR.CostPerMergedPRUSD, out.Cost.MergedPR.RunCount)
	}
}

func TestGetRunStatus_OmitsCostWhenNoData(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	// No seedRunCost → backend returns {} → GetRunCost yields nil.

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Cost != nil {
		t.Errorf("expected no cost block; got %+v", out.Cost)
	}
	// The no-data block must omit the JSON key entirely (nil pointer +
	// omitempty), not serialize a null.
	raw, _ := json.Marshal(out)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["cost"]; ok {
		t.Errorf("marshaled output must omit the cost key when no data; got %s", raw)
	}
}

// --- run latency (#1702) ---
//
// Cross-boundary wire-to-tool seam: a stub backend serves GET
// /v0/runs/{id}/latency and getRunStatus surfaces the same block (and omits it
// when the backend returns the empty no-data object). The compute → endpoint
// layers — the audit-timestamp gate-latency aggregation — are covered end-to-end
// by the latency package (AggregateGateLatency) and the server handler tests
// (latency_test.go); this asserts the MCP-mapping seam so the per-gate breakdown
// and totals don't silently drop on the wire (cf. #618).

func seedRunLatency(fb *fakeBackend, runID uuid.UUID, rl RunLatency) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.latencyByRun[runID] = rl
}

func TestGetRunStatus_SurfacesLatencyBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "succeeded"}
	opened := time.Date(2026, 7, 8, 9, 1, 0, 0, time.UTC)
	closed := time.Date(2026, 7, 8, 9, 6, 0, 0, time.UTC)
	seedRunLatency(fb, runID, RunLatency{
		Gates: []LatencyGate{
			{Gate: "plan_approval", OpenedAt: opened, ClosedAt: closed, WaitSeconds: 300},
			{Gate: "checks_green_to_merge", OpenedAt: opened, ClosedAt: closed, WaitSeconds: 600},
		},
		TotalWaitOnHumanSeconds: 900,
		WallClockSeconds:        2400,
	})

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Latency == nil {
		t.Fatal("expected latency block surfaced from backend; got nil")
	}
	if out.Latency.TotalWaitOnHumanSeconds != 900 || out.Latency.WallClockSeconds != 2400 {
		t.Errorf("latency totals = wait %g wall %g, want 900 / 2400",
			out.Latency.TotalWaitOnHumanSeconds, out.Latency.WallClockSeconds)
	}
	if len(out.Latency.Gates) != 2 {
		t.Fatalf("want 2 gate rows, got %d: %+v", len(out.Latency.Gates), out.Latency.Gates)
	}
	if out.Latency.Gates[0].Gate != "plan_approval" || out.Latency.Gates[0].WaitSeconds != 300 {
		t.Errorf("gate[0] = %+v, want plan_approval wait 300", out.Latency.Gates[0])
	}
	// The gate timestamps must cross the wire intact.
	if !out.Latency.Gates[0].OpenedAt.Equal(opened) || !out.Latency.Gates[0].ClosedAt.Equal(closed) {
		t.Errorf("gate[0] timestamps = %v..%v, want %v..%v",
			out.Latency.Gates[0].OpenedAt, out.Latency.Gates[0].ClosedAt, opened, closed)
	}
}

func TestGetRunStatus_OmitsLatencyWhenNoData(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	// No seedRunLatency → backend returns {} → GetRunLatency yields nil.

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Latency != nil {
		t.Errorf("expected no latency block; got %+v", out.Latency)
	}
	// The no-data block must omit the JSON key entirely (nil pointer +
	// omitempty), not serialize a null.
	raw, _ := json.Marshal(out)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["latency"]; ok {
		t.Errorf("marshaled output must omit the latency key when no data; got %s", raw)
	}
}

// TestGetRunStatus_LatencyFetchErrorOmitsBlock proves the best-effort posture:
// a 500 from the latency endpoint omits the block and NEVER fails the snapshot.
func TestGetRunStatus_LatencyFetchErrorOmitsBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	fb.latencyStatus = http.StatusInternalServerError

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus must not fail when the latency fetch errors: %v", err)
	}
	if out.Latency != nil {
		t.Errorf("expected no latency block on a fetch error; got %+v", out.Latency)
	}
}

// TestGetRunStatus_ConcernsBlock_PropagatesEndToEnd (#964, cf. #618):
// the backend run row's concerns block — open count, by_state, and the
// stable concern IDs fixup's concern_ids addressing needs — must cross
// the real HTTP + JSON-decode path into the tool output, not just exist
// as a struct field.
func TestGetRunStatus_ConcernsBlock_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	concernID := uuid.NewString()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running",
		Concerns: &RunConcerns{
			Open:    2,
			ByState: map[string]int{"raised": 1, "addressed_pending": 1},
			Items: []RunConcernItem{
				{ID: concernID, StageKind: "implement", Severity: "medium", Category: "scope", State: "raised"},
				{ID: uuid.NewString(), StageKind: "plan", Severity: "low", Category: "verification", State: "addressed_pending"},
			},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	got := out.Run.Concerns
	if got == nil {
		t.Fatal("Run.Concerns = nil, want the decoded block")
	}
	if got.Open != 2 {
		t.Errorf("Open = %d, want 2", got.Open)
	}
	if got.ByState["raised"] != 1 || got.ByState["addressed_pending"] != 1 {
		t.Errorf("ByState = %v", got.ByState)
	}
	if len(got.Items) != 2 || got.Items[0].ID != concernID || got.Items[0].StageKind != "implement" {
		t.Errorf("Items = %+v, want the stable IDs decoded through", got.Items)
	}
}

// seedShortSummaryWireFixture seeds a run whose /v0/runs/{id} response carries
// a concern item with a literal short_summary WIRE key set to a distinctive
// sentinel — injected as a RAW map overlay, NOT a re-encoded RunConcernItem —
// so the fixture is ASYMMETRIC: the wire key is fixed regardless of the mirror
// struct's json tag. A typo'd or missing tag on RunConcernItem.ShortSummary
// then decodes to "" and the assertions go RED, which a symmetric (struct
// re-encoded) fixture could not detect. The sentinel is unequal to every other
// field on the item. Returns the run id and the sentinel value. (#2488)
func seedShortSummaryWireFixture(t *testing.T) (*fakeBackend, *httptest.Server, uuid.UUID, string) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	sentinel := "SENTINEL-short-summary-value-9f3a7c"
	// No typed Concerns on the row: the raw overlay is the sole source of the
	// concerns block, so the wire "short_summary" key comes from this literal
	// map, independent of the RunConcernItem json tag under test.
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.getRunExtraByID[runID] = map[string]any{
		"concerns": map[string]any{
			"open":     1,
			"by_state": map[string]any{"raised": 1},
			"items": []any{
				map[string]any{
					"id":            uuid.NewString(),
					"stage_kind":    "implement",
					"severity":      "low",
					"category":      "scope",
					"state":         "raised",
					"short_summary": sentinel,
				},
			},
		},
	}
	return fb, srv, runID, sentinel
}

// TestGetRunStatus_ShortSummaryCrossesWireMirror is the asymmetric json-tag /
// decode seam test (#2488): the fake backend serves a raw short_summary wire
// value that could only have come from the wire, and the decoded
// RunConcernItem.ShortSummary must equal it — pinning that the hand-maintained
// mirror tag byte-matches the server's `short_summary`.
func TestGetRunStatus_ShortSummaryCrossesWireMirror(t *testing.T) {
	_, srv, runID, sentinel := seedShortSummaryWireFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.Concerns == nil || len(out.Run.Concerns.Items) != 1 {
		t.Fatalf("Run.Concerns = %+v, want one item", out.Run.Concerns)
	}
	if got := out.Run.Concerns.Items[0].ShortSummary; got != sentinel {
		t.Errorf("ShortSummary = %q, want %q — a mismatched/missing json tag decodes to \"\"", got, sentinel)
	}
}

// TestGetRunStatus_ShortSummarySurvivesProseFlagOff drives the tool with
// include_review_prose:false (the default) against the same fixture and
// asserts the sentinel short_summary is still present — proving the label
// lives OUTSIDE the prose gate (#2488). No byte-bound assertion here: the
// fixture is literal JSON that never reaches the server truncation helper.
func TestGetRunStatus_ShortSummarySurvivesProseFlagOff(t *testing.T) {
	_, srv, runID, sentinel := seedShortSummaryWireFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), IncludeReviewProse: false})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.Concerns == nil || len(out.Run.Concerns.Items) != 1 {
		t.Fatalf("Run.Concerns = %+v, want one item", out.Run.Concerns)
	}
	if got := out.Run.Concerns.Items[0].ShortSummary; got != sentinel {
		t.Errorf("ShortSummary = %q under include_review_prose:false, want %q (field must survive the prose gate)", got, sentinel)
	}
}

// TestGetRunStatus_NoConcernsBlock_NilField: a run with no open concerns
// (backend omits the key) decodes to a nil pointer, never a zero block.
func TestGetRunStatus_NoConcernsBlock_NilField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.Concerns != nil {
		t.Errorf("Run.Concerns = %+v, want nil when the backend omits the block", out.Run.Concerns)
	}
}

// TestGetPlan_ScopePrecheck_MaxFilesChangedCrossesSeam asserts the #983
// cap field rides the backend-write -> mcp-read JSON contract: a
// server-side payload with MaxFilesChanged set surfaces on the tool
// output so the approver can read headroom.
func TestGetPlan_ScopePrecheck_MaxFilesChangedCrossesSeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     29,
		Violations:       []policy.Violation{},
		MaxFilesChanged:  30,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.MaxFilesChanged != 30 {
		t.Errorf("MaxFilesChanged = %d, want 30", out.ScopePrecheck.MaxFilesChanged)
	}
	if out.ScopePrecheck.ScannedFiles != 29 {
		t.Errorf("ScannedFiles = %d, want 29", out.ScopePrecheck.ScannedFiles)
	}
}

// TestGetPlan_ScopePrecheck_OlderBackendWithoutCapDecodes asserts
// forward/backward compat: a pre-#983 payload lacking the
// max_files_changed key decodes cleanly with the field at zero.
func TestGetPlan_ScopePrecheck_OlderBackendWithoutCapDecodes(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Raw map rather than the server type: older backends never wrote
	// the max_files_changed key at all.
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_scope_precheck",
		Payload: map[string]any{
			"workflow_id":        "feature_change",
			"implement_stage_id": "implement",
			"violations":         []any{},
			"scanned_files":      2,
		},
	})
	fb.mu.Unlock()

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.MaxFilesChanged != 0 {
		t.Errorf("MaxFilesChanged = %d, want 0 for an older-backend payload", out.ScopePrecheck.MaxFilesChanged)
	}
}

// TestGetRunStatus_ChildrenStatus_DecomposedParent drives the full
// get_run_status handler against a decomposed parent parked at
// awaiting_children (#1147): the cost gate fires off the awaiting_children
// implement stage, the per-child GetRun fan-out resolves each slice, and the
// snapshot carries a children_status block with the phase + per-child state.
func TestGetRunStatus_ChildrenStatus_DecomposedParent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	parent := uuid.New()
	fb.getRunByID[parent] = Run{ID: parent.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[parent] = []Stage{
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 2, Type: "implement", State: "awaiting_children"},
	}
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "running")
	seedPlanDecomposed(fb, parent, []string{c0.String(), c1.String()}, 2)

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: parent.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ChildrenStatus == nil {
		t.Fatal("ChildrenStatus is nil, want the decomposed-parent block")
	}
	if out.ChildrenStatus.IntegrationPhase != "running_children" {
		t.Errorf("phase = %q, want running_children", out.ChildrenStatus.IntegrationPhase)
	}
	if out.ChildrenStatus.Total != 2 || out.ChildrenStatus.Succeeded != 1 || out.ChildrenStatus.Running != 1 {
		t.Errorf("counts total=%d succeeded=%d running=%d, want 2/1/1",
			out.ChildrenStatus.Total, out.ChildrenStatus.Succeeded, out.ChildrenStatus.Running)
	}
	if out.ChildrenStatus.Children[0].SliceIndex != 0 || out.ChildrenStatus.Children[1].SliceIndex != 1 {
		t.Errorf("slice indices = [%d %d], want [0 1]",
			out.ChildrenStatus.Children[0].SliceIndex, out.ChildrenStatus.Children[1].SliceIndex)
	}
	// next_actions surfaces the dedicated implement_awaiting_children arm.
	if out.NextActions == nil || out.NextActions.State != "implement_awaiting_children" {
		t.Fatalf("next_actions state = %+v, want implement_awaiting_children", out.NextActions)
	}
}

// TestGetRunStatus_ChildrenStatus_NonDecomposed_NoRead asserts the cost gate:
// an ordinary run carries no children_status AND triggers no plan_decomposed
// audit read (#1147).
func TestGetRunStatus_ChildrenStatus_NonDecomposed_NoRead(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ChildrenStatus != nil {
		t.Errorf("ChildrenStatus = %+v, want nil for a non-decomposed run", out.ChildrenStatus)
	}
	fb.mu.Lock()
	reads := fb.perRunAuditCategoryReads["plan_decomposed"]
	fb.mu.Unlock()
	if reads != 0 {
		t.Errorf("plan_decomposed audit reads = %d, want 0 (cost gate must not read it for a non-decomposed run)", reads)
	}
}

// TestGetRunStatus_ChildrenStatus_IntegratedAfterAwaitingChildren asserts the
// integrated phase is still detected after a clean fan-in moved the parent's
// implement stage OUT of awaiting_children: the gate keys additionally on the
// slices_integrated marker in the recent-audit window (#1147).
func TestGetRunStatus_ChildrenStatus_IntegratedAfterAwaitingChildren(t *testing.T) {
	fb, srv := newFakeBackend(t)
	parent := uuid.New()
	fb.getRunByID[parent] = Run{ID: parent.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[parent] = []Stage{
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 2, Type: "implement", State: "succeeded"},
	}
	c0, c1 := uuid.New(), uuid.New()
	seedChildRun(fb, c0, "succeeded")
	seedChildRun(fb, c1, "succeeded")
	childIDs := []string{c0.String(), c1.String()}
	seedPlanDecomposed(fb, parent, childIDs, 2)
	// The slices_integrated marker lands in the cross-chain recent-audit feed.
	fb.auditByRun[parent] = []AuditEntry{slicesIntegratedEntry(parent, 5, "fishhawk/consolidated-x", childIDs)}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: parent.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ChildrenStatus == nil {
		t.Fatal("ChildrenStatus is nil, want it surfaced from the slices_integrated marker")
	}
	if out.ChildrenStatus.IntegrationPhase != "integrated" {
		t.Errorf("phase = %q, want integrated", out.ChildrenStatus.IntegrationPhase)
	}
	if out.ChildrenStatus.ConsolidatedBranch != "fishhawk/consolidated-x" {
		t.Errorf("consolidated_branch = %q, want fishhawk/consolidated-x", out.ChildrenStatus.ConsolidatedBranch)
	}
}

// TestGetRunStatus_ChildrenStatus_DecodeError_StillSnapshots asserts the
// best-effort swallow (#1147): when childrenStatusFor errors (a corrupt
// plan_decomposed payload), get_run_status leaves children_status nil and
// still returns the snapshot rather than failing.
func TestGetRunStatus_ChildrenStatus_DecodeError_StillSnapshots(t *testing.T) {
	fb, srv := newFakeBackend(t)
	parent := uuid.New()
	fb.getRunByID[parent] = Run{ID: parent.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[parent] = []Stage{
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: parent.String(), Sequence: 2, Type: "implement", State: "awaiting_children"},
	}
	// A plan_decomposed entry with a nil payload → LatestPlanDecomposed errors.
	fb.mu.Lock()
	fb.perRunAuditByRun[parent] = []AuditEntry{{
		ID: uuid.NewString(), Sequence: 1, RunID: parent.String(), Category: "plan_decomposed",
	}}
	fb.mu.Unlock()

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: parent.String()})
	if err != nil {
		t.Fatalf("getRunStatus must not fail when childrenStatusFor errors: %v", err)
	}
	if out.ChildrenStatus != nil {
		t.Errorf("ChildrenStatus = %+v, want nil on a decode error", out.ChildrenStatus)
	}
	// The rest of the snapshot still landed.
	if out.Run.ID != parent.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, parent)
	}
}

// TestGetRunStatus_AcceptanceStageWaitStatus_PresentAndOmitted pins the E31.9
// surface field: a run declaring an acceptance stage carries
// acceptance_stage_wait_status (via the generic stageWaitStatusFor helper), and
// a run WITHOUT one omits it (nil) so non-acceptance runs are byte-identical.
func TestGetRunStatus_AcceptanceStageWaitStatus_PresentAndOmitted(t *testing.T) {
	t.Run("present on an acceptance-stage run", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 3, Type: "acceptance", State: "running"},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.AcceptanceStageWaitStatus == nil {
			t.Fatal("AcceptanceStageWaitStatus is nil; expected it on a run declaring an acceptance stage")
		}
		if out.AcceptanceStageWaitStatus.Stage != "acceptance" {
			t.Errorf("Stage = %q, want acceptance", out.AcceptanceStageWaitStatus.Stage)
		}
		if out.AcceptanceStageWaitStatus.Status != "running" {
			t.Errorf("Status = %q, want running", out.AcceptanceStageWaitStatus.Status)
		}
		if out.AcceptanceStageWaitStatus.PollIntervalSeconds != suggestedStageWaitPollIntervalSeconds {
			t.Errorf("PollIntervalSeconds = %d, want %d", out.AcceptanceStageWaitStatus.PollIntervalSeconds, suggestedStageWaitPollIntervalSeconds)
		}
	})

	t.Run("omitted on a run without an acceptance stage", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.AcceptanceStageWaitStatus != nil {
			t.Errorf("AcceptanceStageWaitStatus = %+v, want nil on a run with no acceptance stage", out.AcceptanceStageWaitStatus)
		}
	})
}

// TestGetRunStatus_StageDeadlineFields is the tools-layer done-means test for
// the #2540 remaining-budget observability: get_run_status over a run whose
// implement stage read carries agent_timeout_seconds (3600) and started 600s ago
// must surface elapsed_seconds, agent_timeout_seconds and deadline_seconds_remaining
// on the wait status — internally consistent (elapsed + remaining == timeout) —
// while a terminal stage and an unknown-budget stage omit all three.
func TestGetRunStatus_StageDeadlineFields(t *testing.T) {
	t.Run("running stage with a known budget surfaces the deadline", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
		implStarted := time.Now().UTC().Add(-600 * time.Second)
		fb.stagesByRun[runID] = []Stage{
			// Terminal plan stage carrying a budget: must still omit the fields.
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded", AgentTimeoutSeconds: 1800},
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running", StartedAt: &implStarted, AgentTimeoutSeconds: 3600},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		ws := out.ImplementStageWaitStatus
		if ws == nil {
			t.Fatal("ImplementStageWaitStatus is nil")
		}
		if ws.AgentTimeoutSeconds != 3600 {
			t.Errorf("agent_timeout_seconds = %d, want 3600", ws.AgentTimeoutSeconds)
		}
		if ws.ElapsedSeconds < 600 || ws.ElapsedSeconds > 630 {
			t.Errorf("elapsed_seconds = %d, want ~600", ws.ElapsedSeconds)
		}
		if ws.DeadlineSecondsRemaining == nil {
			t.Fatal("deadline_seconds_remaining is nil, want present")
		}
		if ws.ElapsedSeconds+*ws.DeadlineSecondsRemaining != ws.AgentTimeoutSeconds {
			t.Errorf("elapsed + remaining != agent_timeout: %d + %d != %d",
				ws.ElapsedSeconds, *ws.DeadlineSecondsRemaining, ws.AgentTimeoutSeconds)
		}
		// Terminal plan stage omits all three despite carrying a budget.
		if out.PlanStageWaitStatus == nil {
			t.Fatal("PlanStageWaitStatus is nil")
		}
		if p := out.PlanStageWaitStatus; p.AgentTimeoutSeconds != 0 || p.ElapsedSeconds != 0 || p.DeadlineSecondsRemaining != nil {
			t.Errorf("terminal plan carries deadline fields: %+v", p)
		}
	})

	t.Run("running stage with an unknown budget omits the deadline", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
		implStarted := time.Now().UTC().Add(-600 * time.Second)
		fb.stagesByRun[runID] = []Stage{
			// agent_timeout_seconds 0 (legacy/older backend): fail open to omitted.
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "implement", State: "running", StartedAt: &implStarted},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		ws := out.ImplementStageWaitStatus
		if ws == nil {
			t.Fatal("ImplementStageWaitStatus is nil")
		}
		if ws.AgentTimeoutSeconds != 0 || ws.ElapsedSeconds != 0 || ws.DeadlineSecondsRemaining != nil {
			t.Errorf("unknown-budget stage carries deadline fields: %+v", ws)
		}
	})
}

// TestGetRunStatus_DerivedStageWaitPollInterval is the DONE-MEANS behavioral
// test for E48.62 / #2489: get_run_status driven end to end over a run carrying
// predicted_runtime_minutes: 115 and an implement stage started 5400s ago must
// advertise the DERIVED cadence — (115*60 - 5400) / 4 = 375 — not the flat 30s
// constant. A no-op or comment-only touch of stage_wait.go fails here even
// though the scope-completeness presence gate would pass.
//
// The plan stage on the same snapshot is the companion assertion: it is
// terminal, so its interval is omitted entirely regardless of the prediction.
func TestGetRunStatus_DerivedStageWaitPollInterval(t *testing.T) {
	t.Run("prediction drives the implement cadence", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		fb.getRunByID[runID] = Run{
			ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
			State: "running", PredictedRuntimeMinutes: 115,
		}
		implStarted := time.Now().UTC().Add(-5400 * time.Second)
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running", StartedAt: &implStarted},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.ImplementStageWaitStatus == nil {
			t.Fatal("ImplementStageWaitStatus is nil")
		}
		// The snapshot reads time.Now() a few microseconds after the fixture
		// stamps started_at, so elapsed is 5400s plus a sliver; a whole second
		// of drift moves the quarter by at most 1. Assert the exact 375 with a
		// one-second tolerance band rather than a range wide enough to swallow
		// the floor or the ceiling — 30 and 900 both fail this.
		if got := out.ImplementStageWaitStatus.PollIntervalSeconds; got < 374 || got > 375 {
			t.Errorf("implement PollIntervalSeconds = %d, want 375 ((115*60 - 5400) / 4)", got)
		}
		if out.PlanStageWaitStatus == nil {
			t.Fatal("PlanStageWaitStatus is nil")
		}
		if out.PlanStageWaitStatus.PollIntervalSeconds != 0 {
			t.Errorf("plan PollIntervalSeconds = %d, want 0 (terminal omits it even under a prediction)",
				out.PlanStageWaitStatus.PollIntervalSeconds)
		}
	})

	t.Run("unstamped run falls back to the elapsed branch", func(t *testing.T) {
		fb, srv := newFakeBackend(t)
		runID := uuid.New()
		// No PredictedRuntimeMinutes — the plan-stage case by construction, and
		// the mixed-version degrade when an older backend omits the key.
		fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
		planStarted := time.Now().UTC().Add(-1304 * time.Second)
		fb.stagesByRun[runID] = []Stage{
			{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "running", StartedAt: &planStarted},
		}

		r := newResolver(srv, nil)
		_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("getRunStatus: %v", err)
		}
		if out.PlanStageWaitStatus == nil {
			t.Fatal("PlanStageWaitStatus is nil")
		}
		// 1304 / 4 = 326 — the observed long plan stage from #2489, which used
		// to sit at 30.
		if got := out.PlanStageWaitStatus.PollIntervalSeconds; got < 326 || got > 327 {
			t.Errorf("plan PollIntervalSeconds = %d, want 326 (1304 / 4)", got)
		}
	})
}

// TestGetRunStatus_AcceptancePassed_ThreadsRecentAudit exercises the REAL
// getRunStatus path end-to-end (wire JSON -> payload parse -> classifier ->
// next_actions) for the cross-boundary seam (#875): the backend writes the
// acceptance_outcome_recorded verdict, the MCP reads it off the recent-audit
// slice, and the classifier yields acceptance_passed with the merge ritual. A
// FAILED verdict leaves the STAGE succeeded, so this proves the classifier reads
// the audit payload, not the stage state.
func TestGetRunStatus_AcceptancePassed_ThreadsRecentAudit(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/77"
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running", PullRequestURL: &prURL,
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 3, Type: "acceptance", State: "succeeded"},
	}
	// The acceptance_outcome_recorded verdict=passed entry, as a map payload so
	// it round-trips through the fake's JSON encode -> client decode as an object
	// (not base64 bytes), exactly like the production /v0/audit endpoint.
	fb.auditByRun[runID] = []AuditEntry{
		{
			ID: uuid.New().String(), Sequence: 2, RunID: runID.String(),
			Category:  "acceptance_outcome_recorded",
			Payload:   map[string]any{"verdict": "passed", "outcome": "accepted"},
			EntryHash: "h",
		},
		{
			ID: uuid.New().String(), Sequence: 1, RunID: runID.String(),
			Category: "acceptance_dispatched", Payload: map[string]any{}, EntryHash: "g",
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "acceptance_passed" {
		t.Fatalf("next_actions = %+v, want state acceptance_passed", out.NextActions)
	}
	names := make([]string, 0, len(out.NextActions.Actions))
	for _, a := range out.NextActions.Actions {
		names = append(names, a.Action)
	}
	if len(names) != 2 || names[0] != "approve_pr" || names[1] != "fishhawk_merge_run" {
		t.Errorf("acceptance_passed actions = %v, want [approve_pr fishhawk_merge_run]", names)
	}
	// And the stage-execution surface tracks execution (succeeded), distinct
	// from the verdict.
	if out.AcceptanceStageWaitStatus == nil || out.AcceptanceStageWaitStatus.Status != "succeeded" {
		t.Errorf("AcceptanceStageWaitStatus = %+v, want status succeeded", out.AcceptanceStageWaitStatus)
	}
}

// TestGetRunStatus_AcceptanceNotValidated_ThreadsRecentAudit is the #2347 twin
// of the test above, over the SAME real getRunStatus path (wire JSON -> payload
// parse -> classifier -> next_actions). It proves the new server-internal
// verdict survives the whole MCP crossing — which is exactly where the #875
// mirrored-not-imported seam could silently drop it into the defensive
// acceptance_settled_outcome_unknown arm — and that the operator-facing
// acknowledgement prompt reaches the wire output, not just the classifier unit.
func TestGetRunStatus_AcceptanceNotValidated_ThreadsRecentAudit(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	prURL := "https://github.com/x/y/pull/78"
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running", PullRequestURL: &prURL,
	}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "succeeded"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 3, Type: "acceptance", State: "succeeded"},
	}
	fb.auditByRun[runID] = []AuditEntry{
		{
			ID: uuid.New().String(), Sequence: 2, RunID: runID.String(),
			Category: "acceptance_outcome_recorded",
			Payload: map[string]any{
				"verdict": "not_validated", "outcome": "not_validated",
				"criteria_passed": 0, "criteria_total": 3,
				"criteria_live_validation": 1,
				"basis":                    "all-skip-with-basis",
			},
			EntryHash: "h",
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "acceptance_not_validated" {
		t.Fatalf("next_actions = %+v, want state acceptance_not_validated", out.NextActions)
	}
	names := make([]string, 0, len(out.NextActions.Actions))
	for _, a := range out.NextActions.Actions {
		names = append(names, a.Action)
	}
	if len(names) != 2 || names[0] != "approve_pr" || names[1] != "fishhawk_merge_run" {
		t.Errorf("acceptance_not_validated actions = %v, want [approve_pr fishhawk_merge_run] (merge-eligible)", names)
	}
	// The acknowledgement prompt (binding condition 1) must survive to the wire.
	reason := out.NextActions.Actions[0].Reason
	if !strings.Contains(reason, "ZERO") || !strings.Contains(reason, "merge verdict") {
		t.Errorf("wire next_actions reason lost the zero-criteria / merge-verdict acknowledgement prompt:\n%s", reason)
	}
}

// releaseNotesFake is a minimal mux over the two release-notes endpoints the
// fishhawk_release_notes handler calls (E33.5 / #1590). It records the request
// coordinates and counts hits so a validation-branch test can assert the
// handler short-circuited BEFORE any HTTP hop.
type releaseNotesFake struct {
	previewHits int
	persistHits int
	lastPreview map[string]string
	lastPersist releaseNotesPersistRequest
	srv         *httptest.Server
}

func newReleaseNotesFake(t *testing.T) *releaseNotesFake {
	t.Helper()
	f := &releaseNotesFake{lastPreview: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/releases/notes/preview", func(w http.ResponseWriter, r *http.Request) {
		f.previewHits++
		f.lastPreview = map[string]string{
			"repo": r.URL.Query().Get("repo"),
			"from": r.URL.Query().Get("from"),
			"to":   r.URL.Query().Get("to"),
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, "# preview\n\nsuggested bump: minor (because ...)\n")
	})
	mux.HandleFunc("POST /v0/releases/notes", func(w http.ResponseWriter, r *http.Request) {
		f.persistHits++
		_ = json.NewDecoder(r.Body).Decode(&f.lastPersist)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ReleaseNotesPersistResult{
			ArtifactID:  "art-1",
			StageID:     f.lastPersist.StageID,
			Repo:        f.lastPersist.Repo,
			From:        f.lastPersist.From,
			To:          f.lastPersist.To,
			ContentHash: "hash-1",
			Markdown:    "# persisted",
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *releaseNotesFake) resolver(env map[string]string) *runResolver {
	return &runResolver{
		api:    newAPIClient(config{backendURL: f.srv.URL, apiToken: "tok-test"}),
		getenv: envFuncFromMap(env),
	}
}

// TestReleaseNotesTool_Handler exercises the fishhawk_release_notes tool handler
// (E33.5 / #1590): the preview and prepare happy paths plus EACH fail-closed
// validation branch, asserting the validation branches short-circuit before any
// HTTP hop (previewHits+persistHits stay zero).
func TestReleaseNotesTool_Handler(t *testing.T) {
	t.Run("preview renders the markdown without persisting", func(t *testing.T) {
		f := newReleaseNotesFake(t)
		_, out, err := f.resolver(nil).releaseNotes(context.Background(), nil,
			ReleaseNotesInput{Mode: "preview", Repo: "x/y", From: "v1.1.0", To: "HEAD"})
		if err != nil {
			t.Fatalf("releaseNotes preview: %v", err)
		}
		if out.Mode != "preview" || !strings.Contains(out.Markdown, "suggested bump") {
			t.Errorf("preview out = %+v, want the rendered markdown", out)
		}
		if out.ArtifactID != "" {
			t.Errorf("preview must not persist an artifact; got id %q", out.ArtifactID)
		}
		if f.persistHits != 0 {
			t.Errorf("preview hit the persist endpoint %d times, want 0", f.persistHits)
		}
		if f.lastPreview["repo"] != "x/y" || f.lastPreview["from"] != "v1.1.0" || f.lastPreview["to"] != "HEAD" {
			t.Errorf("preview coordinates = %+v", f.lastPreview)
		}
	})

	t.Run("empty mode defaults to preview", func(t *testing.T) {
		f := newReleaseNotesFake(t)
		_, out, err := f.resolver(nil).releaseNotes(context.Background(), nil,
			ReleaseNotesInput{Repo: "x/y", From: "v1.1.0", To: "HEAD"})
		if err != nil {
			t.Fatalf("releaseNotes default mode: %v", err)
		}
		if out.Mode != "preview" || f.previewHits != 1 || f.persistHits != 0 {
			t.Errorf("empty mode did not default to preview: out.Mode=%q previewHits=%d persistHits=%d", out.Mode, f.previewHits, f.persistHits)
		}
	})

	t.Run("repo falls back to GITHUB_REPOSITORY", func(t *testing.T) {
		f := newReleaseNotesFake(t)
		_, _, err := f.resolver(map[string]string{"GITHUB_REPOSITORY": "env/repo"}).releaseNotes(
			context.Background(), nil, ReleaseNotesInput{From: "v1.1.0", To: "HEAD"})
		if err != nil {
			t.Fatalf("releaseNotes env repo: %v", err)
		}
		if f.lastPreview["repo"] != "env/repo" {
			t.Errorf("repo = %q, want the GITHUB_REPOSITORY fallback env/repo", f.lastPreview["repo"])
		}
	})

	t.Run("prepare persists and returns the artifact id", func(t *testing.T) {
		f := newReleaseNotesFake(t)
		stageID := uuid.NewString()
		_, out, err := f.resolver(nil).releaseNotes(context.Background(), nil,
			ReleaseNotesInput{Mode: "prepare", Repo: "x/y", From: "v1.1.0", To: "HEAD", StageID: stageID})
		if err != nil {
			t.Fatalf("releaseNotes prepare: %v", err)
		}
		if out.Mode != "prepare" || out.ArtifactID != "art-1" || out.ContentHash != "hash-1" {
			t.Errorf("prepare out = %+v", out)
		}
		if f.lastPersist.StageID != stageID {
			t.Errorf("persist stage_id = %q, want %q", f.lastPersist.StageID, stageID)
		}
	})

	// Fail-closed validation branches — EACH must return an error BEFORE any HTTP
	// hop, so no request reaches the fake.
	validationCases := []struct {
		name string
		env  map[string]string
		in   ReleaseNotesInput
		want string
	}{
		{name: "repo missing", in: ReleaseNotesInput{From: "v1.1.0", To: "HEAD"}, want: "repo is required"},
		{name: "from/to missing", in: ReleaseNotesInput{Repo: "x/y"}, want: "from and to"},
		{name: "prepare without stage_id", in: ReleaseNotesInput{Mode: "prepare", Repo: "x/y", From: "v1.1.0", To: "HEAD"}, want: "stage_id is required"},
		{name: "prepare with non-UUID stage_id", in: ReleaseNotesInput{Mode: "prepare", Repo: "x/y", From: "v1.1.0", To: "HEAD", StageID: "not-a-uuid"}, want: "not a valid UUID"},
		{name: "unrecognized mode", in: ReleaseNotesInput{Mode: "cut", Repo: "x/y", From: "v1.1.0", To: "HEAD"}, want: "not recognized"},
	}
	for _, tc := range validationCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReleaseNotesFake(t)
			_, _, err := f.resolver(tc.env).releaseNotes(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("want a validation error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if f.previewHits != 0 || f.persistHits != 0 {
				t.Errorf("validation branch hit the backend: previewHits=%d persistHits=%d", f.previewHits, f.persistHits)
			}
		})
	}
}

// seedRawPlanArtifact attaches a plan artifact whose Content is a caller-built
// wire-shaped value (a map, not a typed PlanContent), so a test can exercise a
// wire shape the typed struct cannot express — e.g. split_proposal.phases[]
// nesting files under scope.files (#2055), which the resolver flattens.
func seedRawPlanArtifact(fb *fakeBackend, stageID uuid.UUID, content any, createdAge time.Duration) Artifact {
	v := "standard_v1"
	body, _ := json.Marshal(content)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	art := Artifact{
		ID:            uuid.New().String(),
		StageID:       stageID.String(),
		Kind:          "plan",
		SchemaVersion: &v,
		ContentHash:   "h",
		Content:       decoded,
		CreatedAt:     time.Now().UTC().Add(-createdAge),
	}
	fb.mu.Lock()
	fb.artifactsByStage[stageID] = append(fb.artifactsByStage[stageID], art)
	fb.mu.Unlock()
	return art
}

// TestGetPlan_WithSplitProposal_FieldsSurfaced covers the over-cap split render
// seam (#2055, E50.3): a plan artifact carrying a split_proposal (wire shape:
// each phase nests files under scope.files) is surfaced in the gate view as a
// PlanSplitProposal whose phases carry a flattened per-phase Files list, a
// derived FileCount, and the depends_on edges — visually distinct from a
// single-phase plan.
func TestGetPlan_WithSplitProposal_FieldsSurfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}

	// Build the artifact in WIRE shape so mapSplitProposal's flatten + FileCount
	// derivation is exercised exactly as in production.
	base, _ := json.Marshal(samplePlanContent())
	var content map[string]any
	if err := json.Unmarshal(base, &content); err != nil {
		t.Fatalf("unmarshal sample content: %v", err)
	}
	content["split_proposal"] = map[string]any{
		"rationale": "rename spans more files than the cap; split expand->migrate->contract",
		"phases": []any{
			map[string]any{
				"title":      "Expand",
				"scope_hint": "additive schema change",
				"scope": map[string]any{"files": []any{
					map[string]any{"path": "backend/internal/storage/run.go", "operation": "modify"},
				}},
			},
			map[string]any{
				"title":      "Migrate",
				"depends_on": []any{0},
				"scope": map[string]any{"files": []any{
					map[string]any{"path": "backend/internal/server/reads.go", "operation": "modify"},
					map[string]any{"path": "backend/internal/server/runs.go", "operation": "modify"},
				}},
			},
		},
	}
	seedRawPlanArtifact(fb, planStageID, content, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
	sp := out.Plan.SplitProposal
	if sp == nil {
		t.Fatal("Plan.SplitProposal should be non-nil for a plan carrying a split_proposal")
	}
	if sp.Rationale == "" {
		t.Error("SplitProposal.Rationale should be non-empty")
	}
	if got := len(sp.Phases); got != 2 {
		t.Fatalf("len(SplitProposal.Phases) = %d, want 2", got)
	}
	// Phase 0 (Expand): one file, derived count 1, no depends_on.
	if sp.Phases[0].Title != "Expand" {
		t.Errorf("Phases[0].Title = %q, want Expand", sp.Phases[0].Title)
	}
	if got := len(sp.Phases[0].Files); got != 1 {
		t.Errorf("Phases[0] Files len = %d, want 1", got)
	}
	if sp.Phases[0].FileCount != 1 {
		t.Errorf("Phases[0].FileCount = %d, want 1 (derived from len(Files))", sp.Phases[0].FileCount)
	}
	if len(sp.Phases[0].DependsOn) != 0 {
		t.Errorf("Phases[0].DependsOn = %v, want empty", sp.Phases[0].DependsOn)
	}
	// Phase 1 (Migrate): two files, derived count 2, depends_on [0].
	if sp.Phases[1].FileCount != 2 {
		t.Errorf("Phases[1].FileCount = %d, want 2", sp.Phases[1].FileCount)
	}
	if got := len(sp.Phases[1].Files); got != 2 {
		t.Errorf("Phases[1] Files len = %d, want 2", got)
	}
	if len(sp.Phases[1].DependsOn) != 1 || sp.Phases[1].DependsOn[0] != 0 {
		t.Errorf("Phases[1].DependsOn = %v, want [0]", sp.Phases[1].DependsOn)
	}
}

// TestGetPlan_WithoutSplitProposal_FieldNil confirms a plan with no
// split_proposal surfaces a nil SplitProposal (omitted from the JSON response),
// so single-phase rendering is unchanged (#2055).
func TestGetPlan_WithoutSplitProposal_FieldNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
	if out.Plan.SplitProposal != nil {
		t.Errorf("Plan.SplitProposal should be nil for a plan without a split_proposal; got %+v", out.Plan.SplitProposal)
	}
}

// --- get_plan reachability field (#2056, E50.4) ---

// seedReachabilityAudit adds a plan_reachability_sweep audit entry to the fake's
// per-run audit map, round-tripping the payload through JSON so the handler's
// re-marshal + unmarshal decodes to the same value (mirroring
// seedPlanReviewAudit).
func seedReachabilityAudit(fb *fakeBackend, runID uuid.UUID, payload any) {
	body, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_reachability_sweep",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

func seedReachabilityRun(t *testing.T, fb *fakeBackend, runID uuid.UUID) {
	t.Helper()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
}

// TestGetPlan_Reachability_DiscrepancyIndicatorOnMismatch asserts the distinct
// indicator: a phase whose declared/derived counts disagree carries mismatch=true
// and sets the top-level discrepancy flag, and the violations survive the render.
func TestGetPlan_Reachability_DiscrepancyIndicatorOnMismatch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedReachabilityRun(t, fb, runID)
	seedReachabilityAudit(fb, runID, map[string]any{
		"available": true,
		"phases": []map[string]any{
			{"index": 0, "title": "expand", "declared_count": 2, "derived_count": 3},
			{"index": 1, "title": "migrate", "declared_count": 1, "derived_count": 1},
		},
		"violations": []map[string]any{
			{"kind": "construction_site", "symbol": "Widget", "def_file": "a.go", "def_phase": 0, "use_file": "b.go", "use_phase": 1},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Reachability == nil {
		t.Fatal("Reachability should be populated")
	}
	if !out.Reachability.Available {
		t.Error("Available should be true")
	}
	if !out.Reachability.Discrepancy {
		t.Error("Discrepancy should be true when a phase's declared != derived")
	}
	if len(out.Reachability.Phases) != 2 {
		t.Fatalf("phases = %d, want 2", len(out.Reachability.Phases))
	}
	if !out.Reachability.Phases[0].Mismatch {
		t.Error("phase 0 (2 vs 3) should carry mismatch=true")
	}
	if out.Reachability.Phases[1].Mismatch {
		t.Error("phase 1 (1 vs 1) should NOT carry mismatch")
	}
	if len(out.Reachability.Violations) != 1 || out.Reachability.Violations[0].Symbol != "Widget" {
		t.Errorf("violations = %+v", out.Reachability.Violations)
	}
}

// TestGetPlan_Reachability_NoDiscrepancyOnCleanPartition asserts a clean
// partition (every phase declared==derived) leaves discrepancy false and no
// per-phase mismatch.
func TestGetPlan_Reachability_NoDiscrepancyOnCleanPartition(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedReachabilityRun(t, fb, runID)
	seedReachabilityAudit(fb, runID, map[string]any{
		"available": true,
		"phases": []map[string]any{
			{"index": 0, "title": "expand", "declared_count": 2, "derived_count": 2},
			{"index": 1, "title": "migrate", "declared_count": 1, "derived_count": 1},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Reachability == nil {
		t.Fatal("Reachability should be populated")
	}
	if out.Reachability.Discrepancy {
		t.Error("Discrepancy should be false for a clean partition")
	}
	for i, ph := range out.Reachability.Phases {
		if ph.Mismatch {
			t.Errorf("phase %d should not carry mismatch on a clean partition", i)
		}
	}
}

// TestGetPlan_Reachability_SkipRendersAvailableFalse asserts a fail-open skip
// (available=false with a skip_reason) surfaces without a discrepancy.
func TestGetPlan_Reachability_SkipRendersAvailableFalse(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedReachabilityRun(t, fb, runID)
	seedReachabilityAudit(fb, runID, map[string]any{
		"available":   false,
		"skip_reason": "loaded package has errors: broken.go:1: undefined X",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Reachability == nil {
		t.Fatal("Reachability should be populated")
	}
	if out.Reachability.Available {
		t.Error("Available should be false on a skip")
	}
	if out.Reachability.SkipReason == "" {
		t.Error("SkipReason should be surfaced")
	}
	if out.Reachability.Discrepancy {
		t.Error("Discrepancy should be false on a skip (no phases)")
	}
}

// TestGetPlan_Reachability_AbsentWhenNoEntry asserts the field is omitted when
// no plan_reachability_sweep entry exists (a non-split plan or older run).
func TestGetPlan_Reachability_AbsentWhenNoEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedReachabilityRun(t, fb, runID)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Reachability != nil {
		t.Errorf("Reachability should be nil when no sweep entry exists, got %+v", out.Reachability)
	}
}

// TestGetPlan_Reachability_EndToEndRoundTrip is the cross-layer transport test
// the operator required (SLICE 2): a RUNNER-produced JSON payload (exactly what
// reachability.Result marshals) is decoded by the SERVER's PlanReachabilityPayload
// (the server decode leg, imported here), re-marshaled as the audit payload the
// recorder stores, then rendered by get_plan — asserting per-phase counts AND
// violations survive every hop. A tag drift on any of the three structs breaks
// this test rather than silently failing the advisory open.
func TestGetPlan_Reachability_EndToEndRoundTrip(t *testing.T) {
	// The exact bytes a runner reachability.Result marshals to on the wire.
	runnerJSON := `{"available":true,` +
		`"phases":[{"index":0,"title":"expand","declared_count":2,"derived_count":3},` +
		`{"index":1,"title":"migrate","declared_count":1,"derived_count":1}],` +
		`"violations":[{"kind":"interface_implementer","symbol":"Store",` +
		`"def_file":"pkg/a/store.go","def_phase":0,"use_file":"pkg/b/impl.go","use_phase":1}]}`

	// Server decode leg: the backend owns a mirroring struct across the module
	// boundary. Decode the runner bytes into it, then re-marshal exactly as the
	// recorder stores the audit payload.
	var decoded server.PlanReachabilityPayload
	if err := json.Unmarshal([]byte(runnerJSON), &decoded); err != nil {
		t.Fatalf("server decode of runner payload failed: %v", err)
	}
	if decoded.Phases[0].DerivedCount != 3 || decoded.Violations[0].Kind != "interface_implementer" {
		t.Fatalf("server decode dropped a field: %+v", decoded)
	}
	recorded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal recorded payload: %v", err)
	}

	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedReachabilityRun(t, fb, runID)

	// Seed the audit entry with the exact bytes the server recorder would store.
	var asAny any
	if err := json.Unmarshal(recorded, &asAny); err != nil {
		t.Fatal(err)
	}
	seedReachabilityAudit(fb, runID, asAny)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Reachability == nil {
		t.Fatal("Reachability should survive the round-trip")
	}
	if len(out.Reachability.Phases) != 2 {
		t.Fatalf("phases = %d, want 2 after round-trip", len(out.Reachability.Phases))
	}
	if out.Reachability.Phases[0].DeclaredCount != 2 || out.Reachability.Phases[0].DerivedCount != 3 {
		t.Errorf("phase 0 counts did not survive: %+v", out.Reachability.Phases[0])
	}
	if !out.Reachability.Phases[0].Mismatch || !out.Reachability.Discrepancy {
		t.Error("mismatch/discrepancy indicator lost after round-trip")
	}
	if len(out.Reachability.Violations) != 1 ||
		out.Reachability.Violations[0].Kind != "interface_implementer" ||
		out.Reachability.Violations[0].Symbol != "Store" ||
		out.Reachability.Violations[0].UseFile != "pkg/b/impl.go" {
		t.Errorf("violation did not survive the round-trip: %+v", out.Reachability.Violations)
	}
}

// --- get_plan split_filing field (#2057, E50.5) ---

// seedSplitFilingAudit adds a split_children_filed audit entry to the fake's
// per-run audit map, round-tripping the payload through JSON so the handler's
// re-marshal + unmarshal decodes to the same value (mirroring
// seedReachabilityAudit).
func seedSplitFilingAudit(fb *fakeBackend, runID uuid.UUID, payload any) {
	body, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "split_children_filed",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

func seedSplitFilingRun(t *testing.T, fb *fakeBackend, runID uuid.UUID) {
	t.Helper()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
}

// TestGetPlan_SplitFiling_DeleteOnly asserts the delete-only completion marker
// surfaces the classification, the filed children (with the contract child
// flagged), the contract-child number and the #2062 deferral, and carries NO
// drafted cap-exception.
func TestGetPlan_SplitFiling_DeleteOnly(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	seedSplitFilingAudit(fb, runID, map[string]any{
		"contract_classification": "delete-only",
		"children": []map[string]any{
			{"phase_index": 0, "title": "expand: add NewFoo", "number": 3001, "url": "https://github.com/o/r/issues/3001"},
			{"phase_index": 1, "title": "migrate consumers", "number": 3002, "url": "https://github.com/o/r/issues/3002"},
			{"phase_index": 2, "title": "contract: delete Foo", "number": 3003, "url": "https://github.com/o/r/issues/3003", "is_contract": true},
		},
		"contract_child_number": 3003,
		"deferral_issue":        2062,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should be populated")
	}
	if out.SplitFiling.ContractClassification != "delete-only" {
		t.Errorf("classification = %q, want delete-only", out.SplitFiling.ContractClassification)
	}
	if len(out.SplitFiling.Children) != 3 {
		t.Fatalf("children = %d, want 3", len(out.SplitFiling.Children))
	}
	if !out.SplitFiling.Children[2].IsContract {
		t.Error("child 2 (contract phase) should carry is_contract=true")
	}
	if out.SplitFiling.Children[0].IsContract {
		t.Error("child 0 (expand phase) should NOT carry is_contract")
	}
	if out.SplitFiling.Children[1].Number != 3002 {
		t.Errorf("child 1 number = %d, want 3002 (sibling #N resolution)", out.SplitFiling.Children[1].Number)
	}
	if out.SplitFiling.ContractChildNumber != 3003 {
		t.Errorf("contract_child_number = %d, want 3003", out.SplitFiling.ContractChildNumber)
	}
	if out.SplitFiling.DeferralIssue != 2062 {
		t.Errorf("deferral_issue = %d, want 2062", out.SplitFiling.DeferralIssue)
	}
	if out.SplitFiling.CapException != nil {
		t.Errorf("delete-only should carry no cap_exception; got %+v", out.SplitFiling.CapException)
	}
}

// TestGetPlan_SplitFiling_GovernedException asserts a governed-exception marker
// surfaces the drafted cap-exception (spec_diff raising the cap + pr_body
// stating operator-authored + admin-merged) alongside the classification.
func TestGetPlan_SplitFiling_GovernedException(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	seedSplitFilingAudit(fb, runID, map[string]any{
		"contract_classification": "governed-exception",
		"children": []map[string]any{
			{"phase_index": 0, "title": "expand", "number": 3001, "url": "https://github.com/o/r/issues/3001"},
			{"phase_index": 1, "title": "contract", "number": 3002, "url": "https://github.com/o/r/issues/3002", "is_contract": true},
		},
		"contract_child_number": 3002,
		"deferral_issue":        2062,
		"cap_exception": map[string]any{
			"spec_diff": "-  max_files_changed: 3\n+  max_files_changed: 9",
			"pr_body":   "Atomic rename overflows the cap. This change must be operator-authored and admin-merged: .fishhawk/** is agent-forbidden.",
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should be populated")
	}
	if out.SplitFiling.ContractClassification != "governed-exception" {
		t.Errorf("classification = %q, want governed-exception", out.SplitFiling.ContractClassification)
	}
	if out.SplitFiling.CapException == nil {
		t.Fatal("governed-exception should carry a cap_exception draft")
	}
	if !strings.Contains(out.SplitFiling.CapException.SpecDiff, "max_files_changed: 9") {
		t.Errorf("spec_diff should raise the cap; got %q", out.SplitFiling.CapException.SpecDiff)
	}
	if !strings.Contains(out.SplitFiling.CapException.PRBody, "operator-authored") ||
		!strings.Contains(out.SplitFiling.CapException.PRBody, "admin-merged") {
		t.Errorf("pr_body should state operator-authored + admin-merged; got %q", out.SplitFiling.CapException.PRBody)
	}
}

// TestGetPlan_SplitFiling_AbsentWhenNoEntry asserts the field is omitted when
// no split_children_filed entry exists (a non-split plan, a hook that has not
// completed filing, or an older run).
func TestGetPlan_SplitFiling_AbsentWhenNoEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling != nil {
		t.Errorf("SplitFiling should be nil when no completion marker exists, got %+v", out.SplitFiling)
	}
}

// TestGetPlan_SplitFiling_NewestEntryWins mirrors the sibling sweeps: a
// re-approval after a partial-then-completed filing can write a second
// completion marker; the authoritative one is the newest (last,
// sequence-ascending) — reflecting the FULL child set (operator binding
// condition 1: the marker reflects the completed set).
func TestGetPlan_SplitFiling_NewestEntryWins(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	// A stale marker with a partial child set (the shape a superseded read
	// must not win with).
	seedSplitFilingAudit(fb, runID, map[string]any{
		"contract_classification": "delete-only",
		"children": []map[string]any{
			{"phase_index": 0, "title": "expand", "number": 3001, "url": "u1"},
		},
		"contract_child_number": 0,
		"deferral_issue":        2062,
	})
	// The authoritative completion marker: the full set.
	seedSplitFilingAudit(fb, runID, map[string]any{
		"contract_classification": "delete-only",
		"children": []map[string]any{
			{"phase_index": 0, "title": "expand", "number": 3001, "url": "u1"},
			{"phase_index": 1, "title": "migrate", "number": 3002, "url": "u2"},
			{"phase_index": 2, "title": "contract", "number": 3003, "url": "u3", "is_contract": true},
		},
		"contract_child_number": 3003,
		"deferral_issue":        2062,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should be populated")
	}
	if len(out.SplitFiling.Children) != 3 || out.SplitFiling.ContractChildNumber != 3003 {
		t.Errorf("newest (full-set) marker should win; got children=%d contract=%d",
			len(out.SplitFiling.Children), out.SplitFiling.ContractChildNumber)
	}
}

// TestLoadSplitFiling_CorruptPayload_DecodesNil pins the corrupt-decode
// degradation: a split_children_filed entry whose payload doesn't decode to the
// completion-marker shape is treated as "not present" rather than an error, so
// a corrupt marker never fails the whole plan fetch.
func TestLoadSplitFiling_CorruptPayload_DecodesNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	// Corrupt entry: payload is a bare string, not the completion-marker object.
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "split_children_filed",
		Payload:  "not-an-object",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan should not error on a corrupt split_children_filed payload: %v", err)
	}
	if out.SplitFiling != nil {
		t.Errorf("corrupt payload should decode to nil SplitFiling, got %+v", out.SplitFiling)
	}
}

// TestGetPlan_SplitFiling_EndToEndRoundTrip is the CONDITION 2 cross-layer
// assertion: it locks the approval-to-MCP-read contract by driving the REAL
// shared split-classification logic the approval hook itself runs
// (splitfiling.Classify + splitfiling.DraftCapException over a plan.SplitProposal
// and reachability evidence) — NOT a hand-fabricated payload of magic strings —
// then serializing the result through the server's OWN payload type
// (server.SplitChildrenFiledPayload, the exact struct writeSplitChildrenFiledAudit
// marshals — no hand-copied wire mirror that can drift from its tags) and reading
// it back through the ACTUAL getPlan/loadSplitFiling resolver. So the audit
// payload that crosses the boundary is byte-for-byte what the hook persists, and
// a divergence between the hook's write shape and the MCP read surface (a
// renamed/dropped json tag on either side) now fails to compile or fails the
// round-trip HERE — closing the integration-mismatch gap the two separate legs
// left open.
//
// The remaining leg — the server hook PRODUCING this payload from those same
// splitfiling outputs at run time — is locked in the sibling
// backend/internal/server/split_filing_test.go (its hook method + fakes are
// unexported package-internal, so a single in-process test that both drives the
// real hook AND calls this package-main resolver remains blocked by the
// package-main import boundary; the payload-type export above pins the shared
// wire contract these two legs meet on).
func TestGetPlan_SplitFiling_EndToEndRoundTrip(t *testing.T) {
	// The over-cap contract phase the hook classifies: a 3-phase rename whose
	// terminal (contract) phase's reachability DerivedCount (9) exceeds the
	// resolved implement cap (3), so an atomic rename cannot ship in one in-cap
	// PR — governed-exception.
	const cap, contractDerived = 3, 9
	proposal := plan.SplitProposal{
		Rationale: "over-cap rename",
		Phases: []plan.SplitPhase{
			{Title: "expand: add NewFoo", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpModify}}}},
			{Title: "migrate consumers", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "b.go", Operation: plan.FileOpModify}}}, DependsOn: []int{0}},
			{Title: "contract: delete Foo", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "a.go", Operation: plan.FileOpDelete}}}, DependsOn: []int{1}},
		},
	}
	evidence := []splitfiling.PhaseEvidence{
		{Index: 0, Title: "expand", DeclaredCount: 1, DerivedCount: 1},
		{Index: 1, Title: "migrate", DeclaredCount: 1, DerivedCount: 1},
		{Index: 2, Title: "contract", DeclaredCount: 1, DerivedCount: contractDerived},
	}

	// The REAL shared logic the approval hook runs — its outputs are what cross
	// the audit boundary.
	classification := splitfiling.Classify(proposal, evidence, cap)
	if classification != splitfiling.ClassificationGovernedException {
		t.Fatalf("precondition: Classify = %q, want governed-exception", classification)
	}
	draft := splitfiling.DraftCapException(proposal, evidence, cap)
	if draft == nil {
		t.Fatal("precondition: DraftCapException returned nil for a governed-exception")
	}

	// Serialize through the server's OWN payload type — the exact struct (and json
	// tags) writeSplitChildrenFiledAudit marshals, so a tag drift on either side
	// of the audit boundary is caught here at compile/round-trip time.
	wire := server.SplitChildrenFiledPayload{
		ContractClassification: string(classification),
		Children: []server.SplitFilingChild{
			{PhaseIndex: 0, Title: "expand: add NewFoo", Number: 3001, URL: "https://github.com/o/r/issues/3001"},
			{PhaseIndex: 1, Title: "migrate consumers", Number: 3002, URL: "https://github.com/o/r/issues/3002"},
			{PhaseIndex: 2, Title: "contract: delete Foo", Number: 3003, URL: "https://github.com/o/r/issues/3003", IsContract: true},
		},
		ContractChildNumber: 3003,
		DeferralIssue:       splitfiling.DeferralIssue,
		CapException:        &server.SplitCapExceptionDraft{SpecDiff: draft.SpecDiff, PRBody: draft.PRBody},
	}
	recorded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire payload: %v", err)
	}
	var asAny any
	if err := json.Unmarshal(recorded, &asAny); err != nil {
		t.Fatal(err)
	}

	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	seedSplitFilingAudit(fb, runID, asAny)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should survive the round-trip")
	}
	// The classification + deferral that crossed the boundary are the REAL
	// splitfiling values.
	if out.SplitFiling.ContractClassification != string(splitfiling.ClassificationGovernedException) {
		t.Errorf("classification did not survive: %q", out.SplitFiling.ContractClassification)
	}
	if out.SplitFiling.DeferralIssue != splitfiling.DeferralIssue {
		t.Errorf("deferral_issue = %d, want the shared splitfiling.DeferralIssue (%d)",
			out.SplitFiling.DeferralIssue, splitfiling.DeferralIssue)
	}
	// The drafted cap-exception decodes to the REAL DraftCapException output.
	if out.SplitFiling.CapException == nil {
		t.Fatal("cap_exception should survive the round-trip for a governed-exception")
	}
	if out.SplitFiling.CapException.SpecDiff != draft.SpecDiff {
		t.Errorf("spec_diff did not survive:\n got %q\nwant %q", out.SplitFiling.CapException.SpecDiff, draft.SpecDiff)
	}
	if out.SplitFiling.CapException.PRBody != draft.PRBody {
		t.Errorf("pr_body did not survive:\n got %q\nwant %q", out.SplitFiling.CapException.PRBody, draft.PRBody)
	}
	// The filed children + contract carrier survive with the contract flagged.
	if len(out.SplitFiling.Children) != 3 || !out.SplitFiling.Children[2].IsContract {
		t.Errorf("children did not survive with the contract child flagged: %+v", out.SplitFiling.Children)
	}
	if out.SplitFiling.ContractChildNumber != 3003 {
		t.Errorf("contract_child_number = %d, want 3003", out.SplitFiling.ContractChildNumber)
	}
	// The drafted spec diff literally raises the cap to the derived count.
	if !strings.Contains(out.SplitFiling.CapException.SpecDiff, strconv.Itoa(contractDerived)) {
		t.Errorf("spec_diff should raise the cap to the derived count %d; got %q", contractDerived, out.SplitFiling.CapException.SpecDiff)
	}
}

// seedSplitFilingRefusedAudit adds a split_filing_refused audit entry to the
// fake's per-run audit map, seeding under the server's EXPORTED category const
// (server.SplitFilingRefusedCategory) — the SAME symbol the on-approval hook
// writes under. The resolver reads its own literal "split_filing_refused"; if the
// hook's write-category const and the resolver's read-literal ever drift, this
// seed lands under a category the resolver never queries and the round-trip fails.
func seedSplitFilingRefusedAudit(fb *fakeBackend, runID uuid.UUID, payload any) {
	body, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: server.SplitFilingRefusedCategory,
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

// TestGetPlan_SplitFilingRefusedRoundTrip compile-locks the READ side of the
// refusal contract (#2412): it seeds the server's OWN exported payload type
// (server.SplitFilingRefusedPayload — the exact struct refuseSplitFilingOverCapPhase
// marshals, so a json-tag drift on either side fails to compile or fails the
// round-trip) under the server's exported category const
// (server.SplitFilingRefusedCategory — the same symbol the hook writes under), and
// reads it back through the ACTUAL getPlan/loadSplitFilingRefusal resolver (which
// queries its own "split_filing_refused" literal). It deliberately hand-seeds the
// audit entry rather than driving the hook, so it isolates the resolver decode +
// category-literal match; the FULL end-to-end — the REAL approval hook writing the
// marker and this same getPlan path reading it back across all three seams
// (category, run association, payload shape) — is condition 1's single test
// TestSplitFilingRefused_EndToEndThroughGetPlan in the sibling
// backend/internal/server/split_filing_test.go, which drives the production hook
// and needs no hand-seeded state.
func TestGetPlan_SplitFilingRefusedRoundTrip(t *testing.T) {
	wire := server.SplitFilingRefusedPayload{
		Reason: "refused to file split_proposal children: 1 phase(s) declare more files than the implement-stage max_files_changed cap of 3",
		Cap:    3,
		Phases: []server.SplitFilingRefusedPhase{
			{Index: 0, Title: "expand: over-cap lead", DeclaredCount: 4},
		},
	}
	recorded, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire payload: %v", err)
	}
	var asAny any
	if err := json.Unmarshal(recorded, &asAny); err != nil {
		t.Fatal(err)
	}

	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	seedSplitFilingRefusedAudit(fb, runID, asAny)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should be populated for a refusal")
	}
	if out.SplitFiling.Refused == nil {
		t.Fatal("SplitFiling.Refused should surface the refusal")
	}
	ref := out.SplitFiling.Refused
	if ref.Cap != 3 {
		t.Errorf("refused cap = %d, want 3", ref.Cap)
	}
	if len(ref.Phases) != 1 || ref.Phases[0].Index != 0 || ref.Phases[0].DeclaredCount != 4 {
		t.Errorf("refused phases = %+v, want one entry index=0 declared=4", ref.Phases)
	}
	if !strings.Contains(ref.Phases[0].Title, "over-cap lead") {
		t.Errorf("refused phase title = %q, want the offending phase title", ref.Phases[0].Title)
	}
	// A refusal writes no completion marker, so the filed set stays empty.
	if len(out.SplitFiling.Children) != 0 {
		t.Errorf("a refusal must carry no filed children, got %+v", out.SplitFiling.Children)
	}
}

// TestGetPlan_SplitFilingRefused_AbsentWhenNoEntry confirms the refused sub-object
// is omitted when neither a completion nor a refusal marker exists (#2412).
func TestGetPlan_SplitFilingRefused_AbsentWhenNoEntry(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling != nil {
		t.Errorf("SplitFiling should be nil when no marker exists, got %+v", out.SplitFiling)
	}
}

// TestGetPlan_SplitFiling_CompletionAndRefusalMerge pins loadSplitFiling's merge
// branch (#2412 fix-up, review concern): completion and refusal are DOCUMENTED as
// mutually exclusive outcomes, but the loader nonetheless merges both onto one
// PlanSplitFiling when both entries exist for a run (a defensive belt-and-braces
// path that was previously untested). With a split_children_filed AND a
// split_filing_refused entry both present, the single returned PlanSplitFiling
// carries the completion's filed children/classification AND the refusal's phases
// — neither outcome silently drops the other.
func TestGetPlan_SplitFiling_CompletionAndRefusalMerge(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	seedSplitFilingRun(t, fb, runID)
	// A completion marker (filed children + classification)...
	seedSplitFilingAudit(fb, runID, map[string]any{
		"contract_classification": "delete-only",
		"children": []map[string]any{
			{"phase_index": 0, "title": "expand: add NewFoo", "number": 3001, "url": "https://github.com/o/r/issues/3001"},
		},
		"contract_child_number": 3001,
		"deferral_issue":        2062,
	})
	// ...AND a refusal marker for the SAME run.
	refusalWire := server.SplitFilingRefusedPayload{
		Reason: "refused",
		Cap:    3,
		Phases: []server.SplitFilingRefusedPhase{{Index: 0, Title: "over-cap lead", DeclaredCount: 4}},
	}
	body, _ := json.Marshal(refusalWire)
	var refusalAny any
	if err := json.Unmarshal(body, &refusalAny); err != nil {
		t.Fatal(err)
	}
	seedSplitFilingRefusedAudit(fb, runID, refusalAny)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SplitFiling == nil {
		t.Fatal("SplitFiling should be populated when both markers exist")
	}
	// Completion side survived the merge.
	if out.SplitFiling.ContractClassification != "delete-only" {
		t.Errorf("classification = %q, want delete-only (completion side of the merge)", out.SplitFiling.ContractClassification)
	}
	if len(out.SplitFiling.Children) != 1 || out.SplitFiling.Children[0].Number != 3001 {
		t.Errorf("children = %+v, want the completion's one filed child", out.SplitFiling.Children)
	}
	// Refusal side survived the merge.
	if out.SplitFiling.Refused == nil {
		t.Fatal("Refused should be populated alongside the completion on the merged object")
	}
	if len(out.SplitFiling.Refused.Phases) != 1 || out.SplitFiling.Refused.Phases[0].DeclaredCount != 4 {
		t.Errorf("refused phases = %+v, want the over-cap phase (declared 4)", out.SplitFiling.Refused.Phases)
	}
}

// TestStartRun_AppliesToOverride_ForwardedToBackend is the registry-coupling
// half of E53.3 / #2226: the two new fishhawk_start_run parameters must cross
// the MCP input → StartRunParams → createRunRequest → JSON body seam. The
// escape hatch past a fail-closed routing control is only an escape hatch if
// it actually reaches the backend.
func TestStartRun_AppliesToOverride_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:                    "x/y",
		WorkflowID:              "trivial",
		WorkflowSpec:            validTrivialSpec,
		AppliesToOverride:       true,
		AppliesToOverrideReason: "one-off backport; widening tracked separately",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !fb.createRunBody.AppliesToOverride {
		t.Error("AppliesToOverride = false, want true")
	}
	if fb.createRunBody.AppliesToOverrideReason != "one-off backport; widening tracked separately" {
		t.Errorf("AppliesToOverrideReason = %q, want the operator's verbatim reason", fb.createRunBody.AppliesToOverrideReason)
	}
}

// TestStartRun_AppliesToOverride_DefaultsOff keeps an ordinary start_run byte-
// identical to a pre-#2226 one: an agent that never mentions the override must
// not silently send one.
func TestStartRun_AppliesToOverride_DefaultsOff(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	if _, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "trivial", WorkflowSpec: validTrivialSpec,
	}); err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.AppliesToOverride || fb.createRunBody.AppliesToOverrideReason != "" {
		t.Errorf("override defaulted on: %+v", fb.createRunBody)
	}
}

// ---------------------------------------------------------------------------
// ADR-077 / #2508 — response byte budget, driven through the REAL handler
// ---------------------------------------------------------------------------

// worstCase knobs one adversarial failed-run shape. EVERY knob is honoured by
// newWorstCaseFailedRun and asserted on: a knob the seeder silently ignores
// turns its row into a differently-named copy of another row.
//
// There is deliberately no action-COUNT knob. next_actions is a pure function of
// the run snapshot, computed by nextActionsFor from a fixed state table, so no
// fixture can make the handler emit 200 actions — the count dimension is driven
// at the ladder level instead, where maximalRunStatusOutput carries 20 actions
// and T9's cap is asserted directly. What a fixture CAN drive through the real
// handler is action SIZE: nextActionFieldLn below.
type worstCase struct {
	name              string
	stages            int
	failureReasonLen  int
	nextActionFieldLn int
	auditEntries      int
	auditArrayLen     int
	autoAdvanced      int
	concerns          int
	securityFindings  int
	freeTextPoison    bool
}

// newWorstCaseFailedRun seeds the observed failure shape on the shared
// fakeBackend seam. Every knob is independently adversarial.
func newWorstCaseFailedRun(t *testing.T, c worstCase) (*httptest.Server, uuid.UUID) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	poison := ""
	if c.freeTextPoison {
		poison = "<script>&</script>  \x00\x1f\xff\xc0\xaf " + strings.Repeat("日本 ", 200)
	}

	run := Run{
		ID: runID.String(), Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
		State: "failed", TriggerSource: "issue",
		IssueContext: &IssueContext{
			Title: "an issue" + poison, Body: strings.Repeat("issue body "+poison, 200),
			URL: "https://example/issues/1", Number: 2508,
			Comments: []IssueComment{{Author: "a", Body: strings.Repeat("comment "+poison, 200), CreatedAt: "t"}},
		},
		LiveValidation: &RunLiveValidation{PendingCriteriaCount: 2, WalkRef: "#2509"},
	}
	// next_actions bulk, driven through the REAL computation: working_dir is
	// folded onto the params of every runner-spawning action
	// (foldWorkingDirParams), and walk_ref lands in BOTH the live-validation
	// advisory's params and its rendered reason (foldLiveValidationAdvisory).
	// Both are run fields, so the fixture reaches next_actions without hand-
	// building the block the handler is supposed to compute.
	if c.nextActionFieldLn > 0 {
		run.WorkingDir = "/tmp/" + strings.Repeat("d", c.nextActionFieldLn)
		run.LiveValidation = &RunLiveValidation{
			PendingCriteriaCount: 2,
			WalkRef:              "#" + strings.Repeat("9", c.nextActionFieldLn),
		}
	}
	for i := 0; i < 4; i++ {
		run.ReviewAuthority = append(run.ReviewAuthority, RunReviewAuthority{
			Stage: fmt.Sprintf("s%d", i), StageType: "implement", Authority: "advisory", Source: "derived",
		})
	}
	if c.concerns > 0 {
		run.Concerns = &RunConcerns{Open: c.concerns, ByState: map[string]int{"raised": c.concerns}}
		for i := 0; i < c.concerns; i++ {
			run.Concerns.Items = append(run.Concerns.Items, RunConcernItem{
				ID: uuid.NewString(), StageKind: "implement", Severity: "high",
				Category: "correctness", State: "raised",
			})
		}
	}
	fb.getRunByID[runID] = run
	if c.autoAdvanced > 0 {
		var advances []any
		for i := 0; i < c.autoAdvanced; i++ {
			advances = append(advances, map[string]any{
				"rule": "reviews_settled_gate", "from": "implement_reviewed",
				"to": "awaiting_merge", "ts": "2026-08-01T00:00:00Z",
			})
		}
		fb.getRunExtraByID[runID] = map[string]any{"drive": true, "auto_advanced": advances}
	}

	reason := strings.Repeat("boom "+poison, 1+c.failureReasonLen/8)
	var stages []Stage
	for i := 0; i < c.stages; i++ {
		st := Stage{
			ID: uuid.NewString(), RunID: runID.String(), Sequence: i + 1,
			Type: "implement", Executor: StageExecutor{Kind: "agent", Ref: "claude"},
			State: "failed", FailureCategory: strptr("category_a"),
		}
		if c.failureReasonLen > 0 {
			r := reason
			st.FailureReason = &r
		}
		stages = append(stages, st)
	}
	if len(stages) > 0 {
		stages[0].Type = "plan"
		stages[0].State = "succeeded"
		stages[0].FailureCategory = nil
		stages[0].FailureReason = nil
	}
	fb.stagesByRun[runID] = stages

	for i := 0; i < c.auditEntries; i++ {
		payload := map[string]any{"detail": "d"}
		if c.auditArrayLen > 0 {
			// Purely STRUCTURAL bulk: a long array of short strings, which
			// truncateAuditPayloadStrings does not touch.
			arr := make([]any, c.auditArrayLen)
			for j := range arr {
				arr[j] = "s"
			}
			payload["rows"] = arr
		}
		seedRecentAuditPayload(fb, runID, "cost_recorded", payload)
	}
	for i := 0; i < 3; i++ {
		seedImplementReviewAudit(fb, runID, PlanReview{
			ReviewerKind: "agent", ReviewerModel: "claude-opus-4-8", Authority: "advisory",
			Verdict:  "approve_with_concerns",
			Concerns: []PlanReviewConcern{{Severity: "high", Category: "security", Note: strings.Repeat("note "+poison, 50)}},
			FreeForm: strings.Repeat("prose "+poison, 50),
		})
	}
	if c.securityFindings > 0 {
		var findings []securityscan.Finding
		for i := 0; i < c.securityFindings; i++ {
			findings = append(findings, securityscan.Finding{
				Number: i, RuleID: "go/sql-injection", Severity: "high",
				Description: strings.Repeat("desc "+poison, 20),
				Path:        "backend/internal/x.go", StartLine: i, State: "open", HTMLURL: "https://example/alert",
			})
		}
		seedSecurityFindingsAudit(fb, runID, findings)
	}
	seedCacheEfficiency(fb, runID, CacheEfficiency{
		CacheReadRatio: 0.5, ReuseFactor: 2, NetSavingsUSD: 16.75,
		Stages: []CacheEfficiencyStage{
			{Source: "agent", FreshInputTokens: 2_000_000, CacheReadRatio: 0.5, NetSavingsUSD: 4.0},
			{Source: "implement_review", CacheReadRatio: 0.5, NetSavingsUSD: 4.5},
			{Source: "plan_review", CacheReadRatio: 0.75, ReuseFactor: 3.0, NetSavingsUSD: 12.25},
		},
	})
	seedRunCost(fb, runID, RunCost{
		TotalCostUSD: 12.5,
		Stages: []RunCostStage{
			{Source: "agent", CostUSD: 10.0},
			{Source: "implement_review", CostUSD: 1.5},
			{Source: "plan_review", CostUSD: 1.0},
		},
	})
	seedRunLatency(fb, runID, RunLatency{
		TotalWaitOnHumanSeconds: 900, WallClockSeconds: 3600,
		Gates: []LatencyGate{{Gate: "plan_approval", WaitSeconds: 900}},
	})
	seedBudget(fb, runID, BudgetStatus{Tier: "ok", Period: "week"})
	return srv, runID
}

var worstCases = []worstCase{
	{name: "12 stages with 40KB failure reasons", stages: 12, failureReasonLen: 40 * 1024, auditEntries: 10},
	{name: "400 stages", stages: 400, failureReasonLen: 64, auditEntries: 10},
	{name: "next_actions carrying 8KB working_dir + walk_ref fields", stages: 4, failureReasonLen: 8 * 1024, nextActionFieldLn: 8 * 1024, auditEntries: 10},
	{name: "50 structurally-bulky audit payloads", stages: 4, auditEntries: 50, auditArrayLen: 400},
	{name: "300 auto_advanced transitions", stages: 4, autoAdvanced: 300, auditEntries: 10},
	{name: "200 open concerns", stages: 4, concerns: 200, auditEntries: 10},
	{name: "150 security findings", stages: 4, securityFindings: 150, auditEntries: 10},
	{name: "free text with invalid UTF-8, U+2028/9 and HTML bytes", stages: 6, failureReasonLen: 4096, auditEntries: 20, freeTextPoison: true},
	{name: "payload deliberately past the budget", stages: 30, failureReasonLen: 20 * 1024, auditEntries: 40, auditArrayLen: 200},
	{name: "everything maximal at once", stages: 40, failureReasonLen: 20 * 1024, auditEntries: 50, auditArrayLen: 400,
		autoAdvanced: 300, concerns: 200, securityFindings: 150, freeTextPoison: true},
}

// getRunStatusPreBound returns what the REAL handler assembles before any
// reduction: the same call with the budget override raised far above anything
// the handler can produce, so boundRunStatusOutput takes its `n <= budget` early
// return. That early return is proved to be a byte-identical no-op — with the
// baseline captured BEFORE the ladder runs, not by re-running it over its own
// output — by TestBound_UnderBudget_ReturnsTheInputBytesUnchanged.
func getRunStatusPreBound(t *testing.T, srv *httptest.Server, runID uuid.UUID, auditLimit int) GetRunStatusOutput {
	t.Helper()
	r := newResolver(srv, map[string]string{runStatusBudgetEnvVar: strconv.Itoa(1 << 30)})
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
		RunID: runID.String(), AuditLimit: auditLimit,
	})
	if err != nil {
		t.Fatalf("getRunStatus (unreduced): %v", err)
	}
	if out.Elisions != nil {
		t.Fatalf("the unreduced call still reduced the response: %+v", out.Elisions)
	}
	return out
}

// TestGetRunStatus_WorstCaseFailedRun_UnderBudget is the cross-boundary
// end-to-end proof: it drives the REAL fishhawk_get_run_status handler (never
// boundRunStatusOutput alone — a ladder-only test cannot fail when the ladder
// is correct but UNWIRED) across a table of independently adversarial
// failed-run fixtures.
func TestGetRunStatus_WorstCaseFailedRun_UnderBudget(t *testing.T) {
	for _, c := range worstCases {
		t.Run(c.name, func(t *testing.T) {
			srv, runID := newWorstCaseFailedRun(t, c)
			r := newResolver(srv, nil)
			_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
				RunID: runID.String(), AuditLimit: 200,
			})
			if err != nil {
				t.Fatalf("getRunStatus: %v", err)
			}
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			budget := mcpResponseByteBudget(envFuncFromMap(nil))
			if len(raw) > budget {
				t.Errorf("response = %d bytes, want <= the %d-byte budget", len(raw), budget)
			}

			// NON-VACUITY. A row that never exceeded the budget would assert
			// nothing about the bound, and a knob the seeder ignored would make
			// its row a differently-named copy of another. So drive the SAME
			// handler with the budget raised above anything it can assemble —
			// the ladder's `n <= budget` early return makes that the unreduced
			// response — and assert the row's claimed shape is really there.
			preBound := getRunStatusPreBound(t, srv, runID, 200)
			preRaw := mustMarshal(t, preBound)
			if len(preRaw) <= budget {
				t.Fatalf("the unreduced response is %d bytes, already within the %d-byte budget — this row exercises no tier", len(preRaw), budget)
			}
			if out.Elisions == nil {
				t.Fatalf("an over-budget row produced no elisions block")
			}
			if c.nextActionFieldLn > 0 {
				n := len(mustMarshal(t, preBound.NextActions))
				if n < c.nextActionFieldLn {
					t.Errorf("the unreduced next_actions block is %d bytes — this row claims a large next_actions shape but the fixture never produced one (want >= %d)", n, c.nextActionFieldLn)
				}
			}

			// The diagnosis core survives every tier including the floor.
			if out.Run.ID != runID.String() {
				t.Errorf("run.id = %q, want %q (never elided)", out.Run.ID, runID.String())
			}
			if out.Run.State != "failed" {
				t.Errorf("run.state = %q, want failed (never elided)", out.Run.State)
			}
			if len(out.Stages) == 0 {
				t.Fatal("every stage row was dropped — the failing stage's state and failure_category are never elided")
			}
			sawFailing := false
			for _, s := range out.Stages {
				if s.State == "failed" && s.FailureCategory != nil && *s.FailureCategory != "" {
					sawFailing = true
				}
			}
			if !sawFailing {
				t.Errorf("no stage row carries the failing state + failure_category: %+v", out.Stages)
			}
			if out.NextActions == nil || out.NextActions.State == "" {
				t.Errorf("next_actions presence is never elided, got %+v", out.NextActions)
			}

			// Every emitted entry satisfies its class/pointer invariant.
			if out.Elisions != nil {
				if err := validateWireElisions(out.Elisions); err != nil {
					t.Errorf("emitted elisions violate their own classification: %v", err)
				}
				if out.Elisions.Budget != budget {
					t.Errorf("elisions.budget = %d, want the effective %d", out.Elisions.Budget, budget)
				}
			}
		})
	}
}

// TestGetRunStatus_UnderBudget_ByteIdenticalNoElisionsBlock proves the bound is
// INERT on the happy path, through the real handler: an ordinary small run's
// wire carries no "elisions" key, and its bytes do not depend on the budget.
//
// It deliberately does NOT re-run the ladder over its own already-bounded
// output: that asserts IDEMPOTENCE, not the pre-bound/post-bound byte identity
// the design claims, and a first-pass mutation emitting no elisions would sit
// inside the baseline and stay green. The pre-bound baseline can only be
// captured where the input still exists — at the ladder level, in
// TestBound_UnderBudget_ReturnsTheInputBytesUnchanged.
func TestGetRunStatus_UnderBudget_ByteIdenticalNoElisionsBlock(t *testing.T) {
	_, srv, runID := seedCompactFixture(t)
	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String(), AuditLimit: 10})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Elisions != nil {
		t.Fatalf("an under-budget response must carry no elisions block, got %+v", out.Elisions)
	}
	raw := mustMarshal(t, out)
	if strings.Contains(string(raw), `"elisions"`) {
		t.Errorf("under-budget wire bytes carry an elisions key: %s", raw)
	}
	// Budget-independent: the same handler call with the budget raised far above
	// anything it can assemble returns the SAME bytes, so nothing on this path
	// is reduced as a function of the budget.
	if pre := mustMarshal(t, getRunStatusPreBound(t, srv, runID, 10)); string(pre) != string(raw) {
		t.Errorf("the under-budget response depends on the budget:\n at the default: %s\n unreduced:     %s", raw, pre)
	}
}

// TestListAudit_ForwardsSinceSequence asserts the anchor reaches the outbound
// query, so a get_run_status elision pointer is genuinely CALLABLE rather than
// decorative.
func TestListAudit_ForwardsSinceSequence(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = []AuditEntry{{ID: uuid.NewString(), Sequence: 8, RunID: runID.String(), Category: "cost_recorded"}}
	r := newResolver(srv, nil)

	if _, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String(), SinceSequence: 7}); err != nil {
		t.Fatalf("listAudit: %v", err)
	}
	fb.mu.Lock()
	q := fb.perRunAuditLastQueryByID[runID]
	fb.mu.Unlock()
	if !strings.Contains(q, "since_sequence=7") {
		t.Errorf("outbound query %q does not carry since_sequence=7 — the elision pointer would be decorative", q)
	}

	// Zero drops from the query string (the REST filter is strictly-greater-
	// than, so zero is a no-op that yields the whole chain).
	if _, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()}); err != nil {
		t.Fatalf("listAudit: %v", err)
	}
	fb.mu.Lock()
	q = fb.perRunAuditLastQueryByID[runID]
	fb.mu.Unlock()
	if strings.Contains(q, "since_sequence") {
		t.Errorf("outbound query %q carries since_sequence at the zero anchor", q)
	}
}

// TestGetRunStatus_PopulatedElisions_PassesSDKOutputSchemaValidation closes the
// boundary the fakeBackend seam can NEVER reach: go-sdk validates the
// marshalled tool output against the reflected output schema INSIDE the
// handler, and jsonschema-go forbids additional properties while skipping
// unexported fields. That constraint is what forced the exported wire DTO, so
// leaving it unverified would leave the design's own premise untested.
func TestGetRunStatus_PopulatedElisions_PassesSDKOutputSchemaValidation(t *testing.T) {
	rows := []struct {
		name    string
		fixture worstCase
		env     map[string]string
	}{
		// An over-budget fixture the ladder reduces WITHOUT reaching the floor,
		// so the block carries entries of all three classes with non-empty
		// Pointer / OmittedCount fields.
		{name: "default budget (tiered elisions, all three classes)", fixture: worstCases[0], env: nil},
		// The FLOOR budget: the Aggregate=true shape is the one most likely to
		// trip a schema mismatch and is never exercised by an under-budget call.
		{name: "floor budget (two aggregate elisions)", fixture: worstCases[len(worstCases)-1], env: map[string]string{runStatusBudgetEnvVar: "1"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			backend, runID := newWorstCaseFailedRun(t, row.fixture)
			cfg := config{backendURL: backend.URL, apiToken: "tok-test"}
			srv := buildServer(cfg)
			registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(row.env)})

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
				Name:      "fishhawk_get_run_status",
				Arguments: map[string]any{"run_id": runID.String(), "audit_limit": 200},
			})
			if err != nil {
				if strings.Contains(err.Error(), "validating tool output") {
					t.Fatalf("the elisions block failed the SDK's output-schema validation: %v", err)
				}
				t.Fatalf("CallTool: %v", err)
			}
			if res.IsError {
				t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
			}
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "validating tool output") {
					t.Fatalf("tool result carries an output-schema validation error: %s", tc.Text)
				}
			}

			structured, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshal structured content: %v", err)
			}
			var out GetRunStatusOutput
			if err := json.Unmarshal(structured, &out); err != nil {
				t.Fatalf("decode tool output %s: %v", structured, err)
			}
			if out.Elisions == nil || len(out.Elisions.Fields) == 0 {
				t.Fatalf("the round-tripped response carries no populated elisions block: %s", structured)
			}
			if err := validateWireElisions(out.Elisions); err != nil {
				t.Errorf("round-tripped elisions violate their own classification: %v", err)
			}
			classes := map[string]bool{}
			var sawPointer, sawCount bool
			for _, f := range out.Elisions.Fields {
				classes[f.Class] = true
				if f.Pointer != "" {
					sawPointer = true
				}
				if f.OmittedCount > 0 {
					sawCount = true
				}
			}
			if row.env == nil {
				for _, want := range []string{"stored", "oversized_capable", "computed"} {
					if !classes[want] {
						t.Errorf("round-tripped elisions carry no %q entry: %s", want, structured)
					}
				}
				if !sawPointer || !sawCount {
					t.Errorf("round-tripped elisions carry no non-empty Pointer / OmittedCount: %s", structured)
				}
			} else {
				if len(out.Elisions.Fields) != 2 {
					t.Errorf("floor budget produced %d entries, want the two aggregates", len(out.Elisions.Fields))
				}
				for _, f := range out.Elisions.Fields {
					if !f.Aggregate {
						t.Errorf("floor entry %q crossed the SDK boundary without its Aggregate flag", f.Field)
					}
				}
			}
		})
	}
}

// getRunStatusBudgetSourceOverSDK drives fishhawk_get_run_status end to end
// through the real MCP handshake with the given client capabilities and returns
// the decoded output. This crosses the seam no per-layer unit test can see:
// handshake -> ServerSession.InitializeParams -> handler -> r.responseBudget ->
// ladder -> reflected output-schema validation.
func getRunStatusBudgetSourceOverSDK(t *testing.T, caps *mcp.ClientCapabilities) GetRunStatusOutput {
	t.Helper()
	ctx := context.Background()
	backend, runID := newWorstCaseFailedRun(t, worstCases[0])
	cfg := config{backendURL: backend.URL, apiToken: "tok-test"}
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{Capabilities: caps})
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
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String(), "audit_limit": 200},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned IsError; content: %+v", res.Content)
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out GetRunStatusOutput
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("decode tool output %s: %v", structured, err)
	}
	return out
}

// TestGetRunStatus_AdvertisedLimit_BoundsToAdvertisedBudget is the CROSS-BOUNDARY
// proof (#2509): a real client advertises a tool-result limit BELOW the 32 KiB
// default, and the marshalled response is bounded to the ADVERTISED value (not
// the default), with elisions.budget_source reading "advertised". No per-layer
// unit can see this handshake -> ServerSession -> handler -> ladder seam.
func TestGetRunStatus_AdvertisedLimit_BoundsToAdvertisedBudget(t *testing.T) {
	const advertised = 8192 // below the 32 KiB default, above the 4 KiB floor
	if advertised >= mcpResponseByteBudgetDefault {
		t.Fatalf("the advertised limit %d must be below the default %d for this test to distinguish them", advertised, mcpResponseByteBudgetDefault)
	}
	caps := &mcp.ClientCapabilities{Extensions: map[string]any{advertisedLimitKey: map[string]any{advertisedBytesSettingsKey: advertised}}}
	out := getRunStatusBudgetSourceOverSDK(t, caps)

	raw := mustMarshal(t, out)
	if len(raw) > advertised {
		t.Errorf("response = %d bytes, want <= the advertised %d (not the %d default)", len(raw), advertised, mcpResponseByteBudgetDefault)
	}
	if out.Elisions == nil {
		t.Fatal("an advertised-limit-reduced response carries no elisions block")
	}
	if out.Elisions.BudgetSource != string(sourceAdvertised) {
		t.Errorf("budget_source = %q, want %q", out.Elisions.BudgetSource, sourceAdvertised)
	}
	if out.Elisions.Budget != advertised {
		t.Errorf("effective budget = %d, want the advertised %d", out.Elisions.Budget, advertised)
	}
}

// TestGetRunStatus_NoAdvertisement_BoundsToMeasuredDefault is the no-regression
// twin: a client advertising NOTHING gets the 32 KiB default, source "default".
func TestGetRunStatus_NoAdvertisement_BoundsToMeasuredDefault(t *testing.T) {
	out := getRunStatusBudgetSourceOverSDK(t, nil)
	raw := mustMarshal(t, out)
	if len(raw) > mcpResponseByteBudgetDefault {
		t.Errorf("response = %d bytes, want <= the %d default", len(raw), mcpResponseByteBudgetDefault)
	}
	if out.Elisions == nil {
		t.Fatal("the worst-case fixture exceeds the default budget, so an elisions block must be present")
	}
	if out.Elisions.BudgetSource != string(sourceDefault) {
		t.Errorf("budget_source = %q, want %q", out.Elisions.BudgetSource, sourceDefault)
	}
}

// ---------------------------------------------------------------------------
// #2510 — the bounded surfaces, driven through the real tool HANDLER
// ---------------------------------------------------------------------------

// callBoundedToolOverSDK drives ONE registered tool end to end through the MCP
// SDK's in-memory transport and returns its marshalled structured content.
//
// This is the seam per-layer unit tests cannot reach: go-sdk validates the
// MARSHALLED tool output against the REFLECTED output schema INSIDE the handler
// (and jsonschema-go forbids additional properties while skipping unexported
// fields), so a mis-tagged or unexported elisions field fails ONLY here. Every
// #2510 surface gained such a field, so every one is driven through it.
func callBoundedToolOverSDK(t *testing.T, srv *httptest.Server, env map[string]string,
	register func(*mcp.Server, *runResolver), name string, args map[string]any) []byte {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	register(server, newResolver(srv, env))

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		if strings.Contains(err.Error(), "validating tool output") {
			t.Fatalf("%s: the elisions block failed the SDK's output-schema validation: %v", name, err)
		}
		t.Fatalf("%s CallTool: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned IsError; content: %+v", name, res.Content)
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "validating tool output") {
			t.Fatalf("%s result carries an output-schema validation error: %s", name, tc.Text)
		}
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal structured content: %v", name, err)
	}
	return raw
}

// assertBoundedWithElisions is the shared assertion for a bounded surface's
// round-tripped response: under the effective budget, carrying a VALID elisions
// block.
func assertBoundedWithElisions(t *testing.T, name string, raw []byte, el *Elisions, budget int) {
	t.Helper()
	if len(raw) > budget {
		t.Errorf("%s round-tripped at %d bytes, want <= %d", name, len(raw), budget)
	}
	if el == nil {
		t.Fatalf("%s: an oversized fixture returned no elisions block: %s", name, raw)
	}
	if err := validateWireElisions(el); err != nil {
		t.Errorf("%s: round-tripped elisions violate their own classification: %v", name, err)
	}
	if el.Budget != budget {
		t.Errorf("%s: elisions.budget = %d, want the effective %d", name, el.Budget, budget)
	}
}

// TestListAudit_OversizedPage_BoundedThroughHandler drives fishhawk_list_audit
// at its advertised maximum through the real handler: the limit=200 page that
// hard-failed the client now returns partial, bounded, and cursor-blanked.
func TestListAudit_OversizedPage_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = auditPage(runID.String(), listAuditLimitMax, strings.Repeat("reviewer prose. ", 400))
	fb.perRunAuditNextByRun[runID] = "cursor-after-the-FULL-page"

	raw := callBoundedToolOverSDK(t, srv, nil, registerListAudit, "fishhawk_list_audit",
		map[string]any{"run_id": runID.String(), "limit": listAuditLimitMax})

	var out ListAuditOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_list_audit", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if len(out.Items) == 0 || len(out.Items) >= listAuditLimitMax {
		t.Fatalf("want a PARTIAL page, got %d of %d entries", len(out.Items), listAuditLimitMax)
	}
	if out.NextCursor != "" {
		t.Errorf("next_cursor = %q survived a truncated page through the handler", out.NextCursor)
	}
	for i, e := range out.Items {
		if e.EntryHash == "" || e.PrevHash == nil {
			t.Errorf("item %d lost its hash chain through the handler", i)
		}
	}
}

// TestListRuns_OversizedPage_BoundedThroughHandler drives the enumeration with
// include_issue_context=true — the opt-in that the bound must cover too.
func TestListRuns_OversizedPage_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	items := make([]Run, 0, listRunsLimitMax)
	for i := 0; i < listRunsLimitMax; i++ {
		r := worstCaseIssueRun(uuid.NewString())
		r.Repo = "kuhlman-labs/" + strings.Repeat("long-repo-segment-", 12)
		items = append(items, r)
	}
	fb.listResp = listRunsResult{Items: items, NextCursor: "cursor-after-the-FULL-page"}

	raw := callBoundedToolOverSDK(t, srv, nil, registerListRuns, "fishhawk_list_runs",
		map[string]any{"limit": listRunsLimitMax, "include_issue_context": true})

	var out ListRunsOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_list_runs", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if len(out.Items) >= listRunsLimitMax && out.NextCursor != "" {
		t.Errorf("a truncated page kept its cursor: %q", out.NextCursor)
	}
	if len(out.Items) < listRunsLimitMax && out.NextCursor != "" {
		t.Errorf("next_cursor = %q survived a row-dropping truncation through the handler", out.NextCursor)
	}
}

// TestGetActiveRun_OversizedRow_BoundedThroughHandler covers the FISHHAWK_RUN_ID
// resolution path — one of three, all of which route through boundActiveRun.
func TestGetActiveRun_OversizedRow_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = worstCaseIssueRun(runID.String())

	raw := callBoundedToolOverSDK(t, srv, map[string]string{"FISHHAWK_RUN_ID": runID.String()},
		registerGetActiveRun, "fishhawk_get_active_run", map[string]any{})

	var out GetActiveRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_get_active_run", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if out.Run.ID != runID.String() {
		t.Errorf("the diagnosis core did not survive: %+v", out.Run)
	}
}

// TestCancelRun_OversizedRow_BoundedThroughHandler is the mutating-verb case
// the issue names: the server-side cancel has ALREADY succeeded when this
// response renders, so the row is reduced rather than the response rejected.
func TestCancelRun_OversizedRow_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	cancelled := worstCaseIssueRun(runID.String())
	cancelled.State = "cancelled"
	fb.cancelResp[runID] = cancelled

	raw := callBoundedToolOverSDK(t, srv, nil, registerCancelRun, "fishhawk_cancel_run",
		map[string]any{"run_id": runID.String()})

	var out CancelRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_cancel_run", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if out.Run.State != "cancelled" {
		t.Errorf("the cancel success signal was lost to the bound: state=%q", out.Run.State)
	}
}

// TestStartRun_OversizedRow_BoundedThroughHandler covers the create verb: the
// run EXISTS by the time the response renders.
func TestStartRun_OversizedRow_BoundedThroughHandler(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.createRunResp = worstCaseIssueRun(runID.String())

	raw := callBoundedToolOverSDK(t, srv, nil, registerStartRun, "fishhawk_start_run", map[string]any{
		"repo":          "kuhlman-labs/fishhawk",
		"workflow_id":   "feature_change",
		"workflow_spec": minimalWorkflowSpecYAML(t),
		"workflow_sha":  "deadbeef",
		"runner_kind":   "github_actions",
	})

	var out StartRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertBoundedWithElisions(t, "fishhawk_start_run", raw, out.Elisions, mcpResponseByteBudgetDefault)
	if out.Run.ID != runID.String() {
		t.Errorf("the created run id was lost to the bound: %+v", out.Run)
	}
}

// --- #2545: cancel_run reaps the detached runners this process spawned ---

// TestCancelRun_TerminatesDetachedRunner is the MCP half of the #2545
// cross-layer contract: a REAL child spawned through the REAL
// spawnRunnerStageDetached is dead after fishhawk_cancel_run, the registry
// entry is gone, and the verb reports reaped_runners == 1. The runner half —
// that a dead holder is sufficient for the NEXT same-lineage acquire to be
// admitted — is TestLineageLock_CancelledRunHolder_NextDispatchAdmitted in the
// runner module (separate Go modules, no dependency edge either way).
func TestCancelRun_TerminatesDetachedRunner(t *testing.T) {
	reg := freshRegistry(t)
	// Every wall-clock bound in the #2545 tests derives from D(base) so one
	// factor scales them together on a loaded runner (AGENTS.md / #1984).
	detachedTerminateGrace = timescale.D(5 * time.Second)

	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y"}

	logPath, err := spawnRunnerStageDetached("/bin/sh", []string{"-c", "sleep 120"},
		os.Environ(), runID.String(), "stage-1", nil, nil)
	if err != nil {
		t.Fatalf("spawnRunnerStageDetached: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(logPath) })

	reg.mu.Lock()
	handles := reg.byRun[runID.String()]
	reg.mu.Unlock()
	if len(handles) != 1 {
		t.Fatalf("registered handles = %d, want 1", len(handles))
	}
	pid := handles[0].pid

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("cancelRun: %v", err)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
	if out.ReapedRunners != 1 {
		t.Errorf("ReapedRunners = %d, want 1", out.ReapedRunners)
	}
	if len(out.ReapWarnings) != 0 {
		t.Errorf("ReapWarnings = %v, want none", out.ReapWarnings)
	}
	if processStillAlive(pid) {
		t.Errorf("detached runner pid %d survived the cancel", pid)
	}
	reg.mu.Lock()
	_, stillRegistered := reg.byRun[runID.String()]
	reg.mu.Unlock()
	if stillRegistered {
		t.Errorf("registry entry survived the cancel")
	}
}

// TestCancelRun_ReapWarningDoesNotFailTheVerb asserts a runner that survives
// TERM and KILL yields a warning but the cancel still SUCCEEDS — the state
// change already landed, and turning a landed mutation into a tool error is
// the failure #2510 removed from this type.
func TestCancelRun_ReapWarningDoesNotFailTheVerb(t *testing.T) {
	reg := freshRegistry(t)
	// A want-TIMEOUT bound (the survivor must NOT report done inside it), so
	// short is safe — but it still derives from D(base) like every other bound
	// in this set rather than being a raw literal.
	detachedTerminateGrace = timescale.D(40 * time.Millisecond)
	origSignal := runStageSignalGroup
	runStageSignalGroup = func(*exec.Cmd, syscall.Signal) {}
	t.Cleanup(func() { runStageSignalGroup = origSignal })

	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y"}

	cmd := exec.Command("sh", "-c", "sleep 120")
	runStageSetProcessGroup(cmd)
	h, err := reg.startAndRegister(cmd, runID.String(), "stage-stuck")
	if err != nil {
		t.Fatalf("startAndRegister: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("cancelRun failed for a landed cancel with a stuck runner: %v", err)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
	if out.ReapedRunners != 0 {
		t.Errorf("ReapedRunners = %d, want 0", out.ReapedRunners)
	}
	if len(out.ReapWarnings) != 1 {
		t.Fatalf("ReapWarnings = %v, want exactly one", out.ReapWarnings)
	}
	if !strings.Contains(out.ReapWarnings[0], strconv.Itoa(h.pid)) {
		t.Errorf("warning %q does not name the surviving pid %d", out.ReapWarnings[0], h.pid)
	}
}

// --- #2689: the FAIL-CLOSED decomposition-child cancel guard ---
//
// Every arm asserts COMMITTED STATE (the fake's recorded cancel count), not
// error identity alone: a guard that fired and was then rolled back would
// return a byte-identical error, so the cancel count is what proves the
// refusal short-circuited the mutation.

func cancelCalls(fb *fakeBackend, runID uuid.UUID) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.cancelCalledByID[runID]
}

// seedChildRunRow seeds a run row whose decomposed_from names a parent — the
// shape that permanently wedges that parent when cancelled.
func seedChildRunRow(fb *fakeBackend, runID, parentID uuid.UUID) {
	parent := parentID.String()
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y", DecomposedFrom: &parent}
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y", DecomposedFrom: &parent}
}

// TestCancelRun_DecomposedChildRefusedWithoutOverride is the counterfactual
// vehicle for the child refusal.
func TestCancelRun_DecomposedChildRefusedWithoutOverride(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	child, parent := uuid.New(), uuid.New()
	seedChildRunRow(fb, child, parent)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: child.String()})
	if err == nil {
		t.Fatal("cancelling a decomposition child must be refused without the override")
	}
	if !strings.Contains(err.Error(), parent.String()) {
		t.Errorf("refusal must name the parent run id: %v", err)
	}
	if !strings.Contains(err.Error(), "orphan_parent_ok") {
		t.Errorf("refusal must name the override: %v", err)
	}
	if got := cancelCalls(fb, child); got != 0 {
		t.Errorf("backend cancel called %d times, want 0 — the refusal must short-circuit the mutation", got)
	}
}

// TestCancelRun_DecomposedChildProceedsWithOverride pins the escape hatch:
// exactly ONE cancel, plus a warning naming the orphaned parent.
func TestCancelRun_DecomposedChildProceedsWithOverride(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	child, parent := uuid.New(), uuid.New()
	seedChildRunRow(fb, child, parent)

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{
		RunID: child.String(), OrphanParentOK: true,
	})
	if err != nil {
		t.Fatalf("the override must let the cancel proceed: %v", err)
	}
	if got := cancelCalls(fb, child); got != 1 {
		t.Errorf("backend cancel called %d times, want exactly 1", got)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("an overridden guard must disclose the orphaned parent")
	}
	if !strings.Contains(out.Warnings[0], parent.String()) {
		t.Errorf("warning must name the parent run id: %q", out.Warnings[0])
	}
}

// TestCancelRun_GetRunErrorFailsClosed is the counterfactual vehicle for the
// FAIL-CLOSED read-error branch — the deliberate inversion of the fail-open
// posture the dispatch guards take, because this mutation is irreversible.
func TestCancelRun_GetRunErrorFailsClosed(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled"}
	fb.mu.Unlock()

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("an unreadable run row must REFUSE the cancel, not proceed")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("refusal must name the unreadable state: %v", err)
	}
	if !strings.Contains(err.Error(), "orphan_parent_ok") {
		t.Errorf("refusal must name the override: %v", err)
	}
	if got := cancelCalls(fb, runID); got != 0 {
		t.Errorf("backend cancel called %d times, want 0", got)
	}
}

// TestCancelRun_GetRunErrorProceedsWithOverride pins that the escape hatch
// clears the read-error refusal too — the availability cost of the fail-closed
// posture is one flag, not a wedged operator.
func TestCancelRun_GetRunErrorProceedsWithOverride(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.mu.Lock()
	fb.getStatusByID[runID] = http.StatusInternalServerError
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled"}
	fb.mu.Unlock()

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{
		RunID: runID.String(), OrphanParentOK: true,
	})
	if err != nil {
		t.Fatalf("the override must let the cancel proceed: %v", err)
	}
	if got := cancelCalls(fb, runID); got != 1 {
		t.Errorf("backend cancel called %d times, want exactly 1", got)
	}
	if len(out.Warnings) == 0 || !strings.Contains(out.Warnings[0], runID.String()) {
		t.Errorf("warnings %v must name the run whose parentage could not be read", out.Warnings)
	}
}

// TestCancelRun_NonChildUnaffected is the control that stops the guard passing
// by refusing everything: an ordinary non-child run cancels unchanged, with no
// warning.
func TestCancelRun_NonChildUnaffected(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y"}
	fb.mu.Unlock()

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("an ordinary non-child cancel must be unaffected: %v", err)
	}
	if got := cancelCalls(fb, runID); got != 1 {
		t.Errorf("backend cancel called %d times, want exactly 1", got)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("the non-child path carries no warnings; got %v", out.Warnings)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
}

// --- #2689: both new operator-facing fields proven across the MCP WIRE ---

// TestReapStage_WireContract invokes fishhawk_reap_stage BY NAME over a real
// in-memory MCP session and unmarshals the raw structured result, so
// runner_liveness and warnings are proven to survive the wire — not just the
// handler's return value.
func TestReapStage_WireContract(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID, stageID := uuid.New(), uuid.New()
	fb.mu.Lock()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", RunnerKind: "github_actions"}
	fb.mu.Unlock()

	raw := callBoundedToolOverSDK(t, srv, nil, registerReapStage, "fishhawk_reap_stage",
		map[string]any{"run_id": runID.String(), "stage_id": stageID.String()})

	var out ReapStageOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RunnerLiveness != "unknown" {
		t.Errorf("runner_liveness did not survive the wire: %q", out.RunnerLiveness)
	}
	if len(out.Warnings) == 0 {
		t.Fatalf("warnings did not survive the wire: %s", raw)
	}
	if !strings.Contains(out.Warnings[0], "github_actions") {
		t.Errorf("warning %q must name the runner kind", out.Warnings[0])
	}
	if !out.Transitioned || out.StageState != "failed" {
		t.Errorf("transitioned=%v stage_state=%q, want true/failed", out.Transitioned, out.StageState)
	}
	if !strings.Contains(out.NextStep, "fishhawk_retry_stage") {
		t.Errorf("next_step must name fishhawk_retry_stage: %q", out.NextStep)
	}
}

// TestCancelRun_OrphanParentOKCrossesTheWire proves the new INPUT field is
// advertised on the tool's schema and is honoured when sent over the wire, and
// that the resulting warning comes back on the wire too.
func TestCancelRun_OrphanParentOKCrossesTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	child, parent := uuid.New(), uuid.New()
	seedChildRunRow(fb, child, parent)

	raw := callBoundedToolOverSDK(t, srv, nil, registerCancelRun, "fishhawk_cancel_run",
		map[string]any{"run_id": child.String(), "orphan_parent_ok": true})

	var out CancelRunOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
	if len(out.Warnings) == 0 {
		t.Fatalf("warnings did not survive the wire: %s", raw)
	}
	if !strings.Contains(out.Warnings[0], parent.String()) {
		t.Errorf("warning %q must name the orphaned parent", out.Warnings[0])
	}

	// The input field must be ADVERTISED, else no client can send it.
	descs := toolInputSchemaProperties(t, "fishhawk_cancel_run")
	if _, ok := descs["orphan_parent_ok"]; !ok {
		t.Errorf("fishhawk_cancel_run's input schema does not advertise orphan_parent_ok; properties: %v", descs)
	}
}

// toolInputSchemaProperties returns the named registered tool's input-schema
// property set over a real ListTools round-trip.
func toolInputSchemaProperties(t *testing.T, name string) map[string]bool {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	server := buildServer(cfg)
	registerTools(server, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	props := map[string]bool{}
	for _, tool := range res.Tools {
		if tool.Name != name {
			continue
		}
		// Over the wire the SDK decodes Tool.InputSchema into a JSON object map.
		schemaMap, ok := any(tool.InputSchema).(map[string]any)
		if !ok {
			t.Fatalf("%s InputSchema is %T, want a JSON object map", name, tool.InputSchema)
		}
		raw, ok := schemaMap["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s schema has no properties object; got %v", name, schemaMap["properties"])
		}
		for prop := range raw {
			props[prop] = true
		}
		return props
	}
	t.Fatalf("tool %s is not registered/visible over ListTools", name)
	return nil
}

// TestRunStateProbe_ReadsLiveState covers the probe cancelRun hands to the
// tombstone: it returns the run's live state, and fails closed on a bad UUID
// and on a backend read error.
func TestRunStateProbe_ReadsLiveState(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}

	state, err := r.runStateProbe(context.Background(), runID.String())
	if err != nil {
		t.Fatalf("runStateProbe: %v", err)
	}
	if state != "running" {
		t.Errorf("state = %q, want running", state)
	}
	if _, err := r.runStateProbe(context.Background(), "not-a-uuid"); err == nil {
		t.Error("runStateProbe accepted a non-UUID run id")
	}
	other := uuid.New()
	fb.getStatusByID[other] = http.StatusInternalServerError
	if _, err := r.runStateProbe(context.Background(), other.String()); err == nil {
		t.Error("runStateProbe returned no error on a backend read failure")
	}
}

// TestApprovePlan_AddScopeFilesToSlice_PlumbedToSubmitApproval pins the #2515
// wire seam: ApprovePlanInput.AddScopeFilesToSlice must reach the approvals
// request body the backend decodes (MCP input -> client approvalRequest -> HTTP
// body) with keys and paths intact, so the gate resolves the operator's chosen
// slice rather than a mangled one.
func TestApprovePlan_AddScopeFilesToSlice_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	perSlice := map[string][]string{
		"tools slice": {"backend/internal/mcpserver/tools.go"},
		"1":           {"docs/extra.md"},
	}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:                runID.String(),
		Reason:               "restore the dropped file into the tools slice",
		AddScopeFilesToSlice: perSlice,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.AddScopeFilesToSlice, perSlice) {
		t.Errorf("add_scope_files_to_slice = %v, want %v", fb.approvalsBody.AddScopeFilesToSlice, perSlice)
	}
}

// TestApprovePlan_NoAddScopeFilesToSlice_OmitsFieldOnTheWire confirms the
// byte-identical no-targeting path: an approve without add_scope_files_to_slice
// leaves the field nil on the request body the backend decodes (omitempty), so
// a flat-plan approve is unchanged by #2515.
func TestApprovePlan_NoAddScopeFilesToSlice_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.AddScopeFilesToSlice != nil {
		t.Errorf("add_scope_files_to_slice = %v, want nil when no slice targeting declared", fb.approvalsBody.AddScopeFilesToSlice)
	}
}

// TestApprovePlan_MoveScopeFilesToSlice_PlumbedToSubmitApproval pins the #2596
// plumbing: fishhawk_approve_plan's MoveScopeFilesToSlice input reaches the
// backend request body verbatim through SubmitApproval.
func TestApprovePlan_MoveScopeFilesToSlice_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	move := map[string][]string{
		"tools slice": {"backend/internal/mcpserver/tools.go"},
		"1":           {"docs/extra.md"},
	}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:                 runID.String(),
		Reason:                "relocate the file into the tools slice",
		MoveScopeFilesToSlice: move,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.MoveScopeFilesToSlice, move) {
		t.Errorf("move_scope_files_to_slice = %v, want %v", fb.approvalsBody.MoveScopeFilesToSlice, move)
	}
}

// TestApprovePlan_NoMoveScopeFilesToSlice_OmitsFieldOnTheWire confirms the
// byte-identical no-move path: an approve without move_scope_files_to_slice
// leaves the field nil on the decoded request body (omitempty).
func TestApprovePlan_NoMoveScopeFilesToSlice_OmitsFieldOnTheWire(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.MoveScopeFilesToSlice != nil {
		t.Errorf("move_scope_files_to_slice = %v, want nil when no move declared", fb.approvalsBody.MoveScopeFilesToSlice)
	}
}

// TestApprovePlan_ReasonLint_WarnsOnTheResultAndStillApproves is the LAYER 1
// (#2512) wiring pin. reason_lint_test.go proves the lint's vocabulary and its
// never-refuses property in isolation; this proves it is actually CALLED — a
// lint that is written, tested, and never invoked passes every unit test and
// does nothing, which is the failure mode the plan's counterfactual (h) names.
//
// Both halves are asserted in ONE call, because the whole design is that they
// happen together:
//
//	(a) the warning surfaces on the tool result, and
//	(b) the approval STILL SUBMITS, with the reason byte-unmodified on the wire.
//
// (b) is the half that keeps this a warning rather than a gate. If the lint
// ever gained a refusal path, fb.approvalsBody.Comment would not match and this
// test would fail — so the never-refuses property is pinned at the seam where
// it could actually be violated, not only in the pure function.
func TestApprovePlan_ReasonLint_WarnsOnTheResultAndStillApproves(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	const reason = "approved, but retire criterion AC-4 — it needs a live MCP client"
	res, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: reason,
	})
	if err != nil {
		t.Fatalf("approvePlan returned an error — the lint must WARN, never refuse: %v", err)
	}

	// (b) the approval submitted, with the reason byte-unmodified.
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Fatalf("approvals call count = %d, want 1 — the lint must not block the submission", fb.approvalsCalledByID[stageID])
	}
	if fb.approvalsBody.Decision != "approve" {
		t.Errorf("decision = %q, want approve", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != reason {
		t.Errorf("comment = %q, want the reason byte-unmodified %q — the lint must not rewrite the operator's words",
			fb.approvalsBody.Comment, reason)
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.Stage.State)
	}

	// (a) the warning reached the tool result.
	if res == nil {
		t.Fatal("tool result is nil — the approve-reason lint warning never reached the operator (#2512)")
	}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "approve-reason lint (#2512)") {
		t.Fatalf("tool result carries no approve-reason lint warning:\n%s", joined)
	}
	if !strings.Contains(joined, "amend_acceptance_criteria") {
		t.Errorf("lint warning on the result does not name amend_acceptance_criteria:\n%s", joined)
	}
}

// TestApprovePlan_ReasonLint_SilentOnOrdinaryReason is the negative control for
// the test above. Without it, a lint that fired on EVERY reason would satisfy
// the warning assertion and nobody would notice until operators started
// ignoring the channel. An ordinary approval reason must leave the tool result
// exactly as it is today — here, nil, since no other warning applies.
func TestApprovePlan_ReasonLint_SilentOnOrdinaryReason(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	res, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "approved; keep the change under 200 lines and add a table test",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.Decision != "approve" {
		t.Errorf("decision = %q, want approve", fb.approvalsBody.Decision)
	}
	if res != nil {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "approve-reason lint") {
				t.Errorf("lint fired on an ordinary approval reason — a lint that fires on everything is a lint that gets ignored:\n%s", tc.Text)
			}
		}
	}
}
