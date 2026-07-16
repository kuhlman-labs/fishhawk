package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fixupServer wires the run + audit fakes the fix-up handler needs.
// auditFake (from trace_test.go) supports seeded + appended
// ListForRunByCategory, which the handler uses to resolve concerns and
// count prior fix-up passes.
func fixupServer(t *testing.T) (*Server, *approvalRunRepo, *auditFake) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   repo,
		AuditRepo: au,
	})
	return s, repo, au
}

// seedImplementGateStage seeds an implement stage parked at the review
// gate (awaiting_approval) — the precondition for a fix-up — plus its
// run row in `running` (FixupStage refuses terminal runs, #968).
func seedImplementGateStage(repo *approvalRunRepo) *run.Stage {
	stage := repo.seedGatelessStage(run.StageStateAwaitingApproval)
	repo.seedRun(&run.Run{ID: stage.RunID, State: run.StateRunning})
	return stage
}

// seedConcernsReview records an implement_reviewed audit entry carrying
// an approve_with_concerns verdict with the given concerns, so the
// handler resolves them as the addressable concern set.
func seedConcernsReview(au *auditFake, stage *run.Stage, concerns ...planreview.Concern) {
	payload, _ := json.Marshal(planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApproveWithConcerns,
		Concerns:     concerns,
	})
	rid := stage.RunID
	sid := stage.ID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &rid,
		StageID:  &sid,
		Category: "implement_reviewed",
		Payload:  payload,
	})
}

// seedPushOpenPRStages seeds the push_and_open_pr shape (#780): an
// implement stage that has SUCCEEDED (PR opened) plus a review stage in
// the given state, both sharing one RunID. Returns (implement, review).
func seedPushOpenPRStages(repo *approvalRunRepo, reviewState run.StageState) (*run.Stage, *run.Stage) {
	impl := repo.seedGatelessStage(run.StageStateSucceeded)
	repo.seedRun(&run.Run{ID: impl.RunID, State: run.StateRunning})
	review := repo.seedStage(reviewState)
	repo.mu.Lock()
	review.RunID = impl.RunID
	review.Type = run.StageTypeReview
	review.Sequence = 2
	repo.mu.Unlock()
	return impl, review
}

// failNextRunRepo wraps approvalRunRepo to fail the next N GetRun calls
// (then fall through to the embedded repo), so a test can simulate a
// TRANSIENT run-read failure on the fix-up model gate's GetRun (#1164)
// while FixupStage's own subsequent GetRun succeeds. Kept here in the
// fix-up test scope rather than on the shared approvalRunRepo.
type failNextRunRepo struct {
	*approvalRunRepo
	getRunFailNext int
}

func (r *failNextRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if r.getRunFailNext > 0 {
		r.getRunFailNext--
		return nil, errors.New("transient get-run failure")
	}
	return r.approvalRunRepo.GetRun(ctx, id)
}

func postFixup(t *testing.T, s *Server, stageID uuid.UUID, body fixupRequest) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	url := "/v0/stages/" + stageID.String() + "/fixup"
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.SetPathValue("stage_id", stageID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, withAuth(req))
	return w
}

func TestFixupStage_HappyPath(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "edited an out-of-scope file"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "address the scope drift"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No orchestrator wired → stage stays in pending after the re-open.
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}

	// One stage_fixup_triggered audit entry with the selected concern.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	got := au.appended[0]
	if got.Category != CategoryStageFixupTriggered {
		t.Errorf("audit category = %q, want %s", got.Category, CategoryStageFixupTriggered)
	}
	var payload map[string]any
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["pass_ordinal"].(float64) != 1 {
		t.Errorf("pass_ordinal = %v, want 1", payload["pass_ordinal"])
	}
	if payload["remaining_budget"].(float64) != 0 {
		t.Errorf("remaining_budget = %v, want 0", payload["remaining_budget"])
	}
	// The resolved selected concern must be persisted for the prompt
	// renderer to read back.
	concerns, ok := payload["concerns"].([]any)
	if !ok || len(concerns) != 1 {
		t.Fatalf("payload.concerns = %v, want one resolved concern", payload["concerns"])
	}
	c0 := concerns[0].(map[string]any)
	if c0["category"] != "scope" {
		t.Errorf("selected concern category = %v, want scope", c0["category"])
	}
}

// latestFixupTriggeredPayload decodes the newest stage_fixup_triggered audit
// payload appended during a test, failing when none exists.
func latestFixupTriggeredPayload(t *testing.T, au *auditFake) map[string]any {
	t.Helper()
	var raw []byte
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			raw = e.Payload
		}
	}
	if raw == nil {
		t.Fatal("no stage_fixup_triggered entry appended")
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	return payload
}

// TestFixupStage_ImplementModelOverrideAllowed covers the #1164 fix-up model
// gate happy path: an allow-listed implement_model override is accepted (200)
// and pinned on the stage_fixup_triggered payload as fixup_model=override with
// fixup_model_source=operator.
func TestFixupStage_ImplementModelOverrideAllowed(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ImplementAllowedModels = AllowedModels{"claudecode": {"claude-haiku-4-5-20251001": true}}
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "claude-haiku-4-5-20251001"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	if payload["fixup_model"] != "claude-haiku-4-5-20251001" {
		t.Errorf("fixup_model = %v, want the override", payload["fixup_model"])
	}
	if payload["fixup_model_source"] != string(ModelSourceOperator) {
		t.Errorf("fixup_model_source = %v, want operator", payload["fixup_model_source"])
	}
}

// TestFixupStage_ImplementModelOverrideDisallowed covers the #1164 reject
// branch: a model absent from a configured ImplementAllowedModels set returns
// 422 fixup_invalid_model with NO stage transition and NO audit entry.
func TestFixupStage_ImplementModelOverrideDisallowed(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ImplementAllowedModels = AllowedModels{"claudecode": {"claude-opus-4-8": true}}
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "some-unlisted-model"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_invalid_model") {
		t.Errorf("body missing fixup_invalid_model code: %s", w.Body.String())
	}
	// No transition: the stage must still be parked at the gate.
	got, err := repo.GetStage(context.Background(), stage.ID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (no transition on reject)", got.State)
	}
	// No audit entry was written.
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("stage_fixup_triggered appended despite the 422 reject")
		}
	}
}

// TestFixupStage_ImplementModelFailOpenAllowList covers the IsAllowed fail-open
// contract (#1164): with NO configured allow-list, any override model is
// accepted (byte-identical to today's no-allow-list deployments).
func TestFixupStage_ImplementModelFailOpenAllowList(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Default fixupServer leaves ImplementAllowedModels nil/empty → fail-open.
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "any-model-at-all"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open allow-list):\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	if payload["fixup_model"] != "any-model-at-all" {
		t.Errorf("fixup_model = %v, want the override accepted fail-open", payload["fixup_model"])
	}
}

// TestFixupStage_ImplementModelDefaultInherited covers the byte-identical
// default path (#1164): with no implement_model supplied the fix-up inherits
// the run's resolved implement model (here the deployment default) and pins it
// with the inherited source.
func TestFixupStage_ImplementModelDefaultInherited(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ImplementModelDefault = "claude-opus-4-8"
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	if payload["fixup_model"] != "claude-opus-4-8" {
		t.Errorf("fixup_model = %v, want the inherited deployment default", payload["fixup_model"])
	}
	if payload["fixup_model_source"] != string(ModelSourceDefault) {
		t.Errorf("fixup_model_source = %v, want default", payload["fixup_model_source"])
	}
}

// TestFixupStage_ImplementModelEmptyLadderPinned covers the empty-ladder
// default spawn (#1164): no override and a ModelSourceNone resolution (no
// deployment default, no spec/plan model) pins an EMPTY fixup_model with an
// empty source — a deliberate "use the adapter default spawn" pin.
func TestFixupStage_ImplementModelEmptyLadderPinned(t *testing.T) {
	s, repo, au := fixupServer(t)
	// No ImplementModelDefault, empty spec → resolveImplementModelForRun
	// returns ModelSourceNone.
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	// The keys are PRESENT (so the read-back honors the empty pin) but empty.
	v, ok := payload["fixup_model"]
	if !ok {
		t.Fatal("fixup_model key absent; the empty-ladder pin must still be present")
	}
	if v != "" {
		t.Errorf("fixup_model = %v, want empty (empty-ladder default spawn)", v)
	}
	if payload["fixup_model_source"] != string(ModelSourceNone) {
		t.Errorf("fixup_model_source = %v, want empty (none)", payload["fixup_model_source"])
	}
}

// TestFixupStage_ImplementModelGetRunFailOpenNoOverride covers the #1164
// run-read-failure fail-open path with NO operator override: when the model
// gate's GetRun fails transiently and no implement_model was supplied, the
// fix-up must NOT pin an empty model. Pinning an empty value would be honored
// by the presence-based read-back (fixupResolvedModelFromAudit) as a deliberate
// empty-ladder pin and force the fix-up to spawn with an EMPTY model instead of
// the run's already-resolved implement model. So the fixup_model / source keys
// must be ABSENT — the read-back then returns ok=false and falls through to
// live resolution. The transition still lands (the gate is best-effort).
func TestFixupStage_ImplementModelGetRunFailOpenNoOverride(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ImplementModelDefault = "claude-opus-4-8"
	// Fail ONLY the model gate's GetRun; FixupStage's own subsequent GetRun
	// succeeds so the transition still commits.
	fr := &failNextRunRepo{approvalRunRepo: repo, getRunFailNext: 1}
	s.cfg.RunRepo = fr
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open):\n%s", w.Code, w.Body.String())
	}
	// The transition still landed despite the model-gate read failure.
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}
	payload := latestFixupTriggeredPayload(t, au)
	// No pin: the keys must be ABSENT so the read-back falls through to live
	// resolution rather than honoring a present-but-empty empty-ladder pin.
	if _, ok := payload["fixup_model"]; ok {
		t.Errorf("fixup_model key present (=%v); want absent on the no-override run-read-failure path", payload["fixup_model"])
	}
	if _, ok := payload["fixup_model_source"]; ok {
		t.Errorf("fixup_model_source key present (=%v); want absent on the no-override run-read-failure path", payload["fixup_model_source"])
	}
}

// TestFixupStage_ImplementModelGetRunFailOpenWithOverride covers the #1164
// run-read-failure fail-open path WITH an operator override: the gate cannot
// validate against the allow-list (no run → no adapter), so it skips
// checkFixupModelAllowed and pins the override verbatim as {value, operator}.
// This is the one place a normally-rejected model slips through — an override
// absent from the configured allow-list is NOT 422-rejected here. Assert the
// pin records the operator intent so the prompt-fetch read-back honors it.
func TestFixupStage_ImplementModelGetRunFailOpenWithOverride(t *testing.T) {
	s, repo, au := fixupServer(t)
	// A configured allow-list that OMITS the override — on the normal path
	// this is the 422 reject covered by TestFixupStage_ImplementModelOverrideDisallowed.
	s.cfg.ImplementAllowedModels = AllowedModels{"claudecode": {"claude-opus-4-8": true}}
	fr := &failNextRunRepo{approvalRunRepo: repo, getRunFailNext: 1}
	s.cfg.RunRepo = fr
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "some-unlisted-model"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open skips allow-list validation):\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	if payload["fixup_model"] != "some-unlisted-model" {
		t.Errorf("fixup_model = %v, want the override pinned verbatim (validation skipped)", payload["fixup_model"])
	}
	if payload["fixup_model_source"] != string(ModelSourceOperator) {
		t.Errorf("fixup_model_source = %v, want operator", payload["fixup_model_source"])
	}
}

// TestFixupStage_AllowCreatePersisted asserts the validated allow_create
// paths land on the stage_fixup_triggered audit payload (#823) so the
// prompt renderer can fold them into the effective scope.files.
func TestFixupStage_AllowCreatePersisted(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "needs a new helper file"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{
		Concerns:    []int{0},
		Reason:      "add the helper",
		AllowCreate: []string{"  backend/internal/server/helper.go  ", "docs/api/v0.md"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload struct {
		AllowCreate []string `json:"allow_create"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	// Entries are trimmed; order preserved.
	want := []string{"backend/internal/server/helper.go", "docs/api/v0.md"}
	if len(payload.AllowCreate) != len(want) {
		t.Fatalf("allow_create = %v, want %v", payload.AllowCreate, want)
	}
	for i := range want {
		if payload.AllowCreate[i] != want[i] {
			t.Errorf("allow_create[%d] = %q, want %q", i, payload.AllowCreate[i], want[i])
		}
	}
}

// TestFixupStage_AllowCreateInvalidRejected asserts an absolute,
// ".."-containing, or empty allow_create entry returns 400
// validation_failed with field=allow_create, before any state change.
func TestFixupStage_AllowCreateInvalidRejected(t *testing.T) {
	cases := map[string][]string{
		"absolute":  {"/etc/passwd"},
		"traversal": {"../../etc/passwd"},
		"empty":     {"   "},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			s, repo, au := fixupServer(t)
			stage := seedImplementGateStage(repo)
			seedConcernsReview(au, stage,
				planreview.Concern{Severity: planreview.SeverityLow, Category: "scope", Note: "x"},
			)

			w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, AllowCreate: paths})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			var env struct {
				Error struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if env.Error.Code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", env.Error.Code)
			}
			if env.Error.Details["field"] != "allow_create" {
				t.Errorf("field = %v, want allow_create", env.Error.Details["field"])
			}
			// No state change: the transition is never reached, so no audit
			// entry is appended.
			if len(au.appended) != 0 {
				t.Errorf("audit entries = %d, want 0 (rejected before transition)", len(au.appended))
			}
		})
	}
}

func TestFixupStage_PushOpenPRReopensAndReparks(t *testing.T) {
	s, repo, au := fixupServer(t)
	impl, review := seedPushOpenPRStages(repo, run.StageStateAwaitingApproval)
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "out-of-scope file"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}, Reason: "address scope drift on the PR branch"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Implement re-opened to pending (no orchestrator wired).
	if body.State != string(run.StageStatePending) {
		t.Errorf("implement state = %q, want pending", body.State)
	}
	// Review re-parked to pending.
	curReview, err := repo.GetStage(context.Background(), review.ID)
	if err != nil {
		t.Fatalf("GetStage(review): %v", err)
	}
	if curReview.State != run.StageStatePending {
		t.Errorf("review state = %q, want pending (re-parked)", curReview.State)
	}

	// One audit entry carrying the re-parked review stage id.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["prior_state"] != string(run.StageStateSucceeded) {
		t.Errorf("prior_state = %v, want succeeded", payload["prior_state"])
	}
	if payload["reparked_review_stage_id"] != review.ID.String() {
		t.Errorf("reparked_review_stage_id = %v, want %s", payload["reparked_review_stage_id"], review.ID)
	}
}

func TestFixupStage_PushOpenPRReviewResolvedReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Review gate already closed (merged/succeeded): no longer a fix-up
	// candidate even though the implement stage succeeded.
	impl, _ := seedPushOpenPRStages(repo, run.StageStateSucceeded)
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
	// No fix-up audit entry written on the refusal.
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("unexpected stage_fixup_triggered entry on refused fix-up")
		}
	}
}

func TestFixupStage_SecondPassRefused(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// First pass succeeds (lands in pending, no orchestrator).
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("first fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Re-park the stage at the gate to model the re-review landing on
	// awaiting_approval again, so the only thing blocking the 2nd pass
	// is the bound — not the state machine.
	repo.mu.Lock()
	repo.stages[stage.ID].State = run.StageStateAwaitingApproval
	repo.mu.Unlock()

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second fixup status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
	// Still exactly one fix-up audit entry — the refused pass wrote none.
	n := 0
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			n++
		}
	}
	if n != 1 {
		t.Errorf("stage_fixup_triggered entries = %d, want 1", n)
	}
}

// reparkFixupStage models the re-review landing the stage on
// awaiting_approval again, so the only thing gating the next pass is the
// budget/ceiling decision — not the state machine.
func reparkFixupStage(repo *approvalRunRepo, stageID uuid.UUID) {
	repo.mu.Lock()
	repo.stages[stageID].State = run.StageStateAwaitingApproval
	repo.mu.Unlock()
}

// seedFixupNoChanges records a fixup_no_changes audit entry for the stage,
// modeling the #856 no-change report path (succeedFixupNoChangesStage in
// pullrequest.go) — the durable signal the budget refund counts (#967).
func seedFixupNoChanges(au *auditFake, stage *run.Stage) {
	payload, _ := json.Marshal(map[string]any{
		"run_id":   stage.RunID.String(),
		"stage_id": stage.ID.String(),
		"branch":   "fishhawk/run-x/stage-y",
		"base_sha": "deadbeef",
	})
	rid := stage.RunID
	sid := stage.ID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: "fixup_no_changes", Payload: payload})
}

// seedFixupTriggeredSeq seeds a stage_fixup_triggered audit entry directly into
// the fake's history with an explicit Sequence (#1957) so the infra-refund
// window logic — which pairs category-C death signals to per-pass trigger
// windows by Sequence — has a real, ordered trigger to pair against.
func seedFixupTriggeredSeq(au *auditFake, runID, stageID uuid.UUID, seq int64) {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":    stageID.String(),
		"prior_state": string(run.StageStateAwaitingApproval),
	})
	rid := runID
	sid := stageID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryStageFixupTriggered, Sequence: seq, Payload: payload})
	au.mu.Unlock()
}

// seedDispatchReaperFailed seeds a dispatch_reaper_failed audit entry with the
// given failure_category at an explicit Sequence (#1957) — the #1747
// spawn-phase reaper death signal the infra refund keys on when the category is
// "C".
func seedDispatchReaperFailed(au *auditFake, runID, stageID uuid.UUID, cat run.FailureCategory, seq int64) {
	payload, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"failure_category": string(cat),
	})
	rid := runID
	sid := stageID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryDispatchReaperFailed, Sequence: seq, Payload: payload})
	au.mu.Unlock()
}

// seedFixupRecoveredC seeds a stage_fixup_recovered audit entry with the given
// source_failure_category at an explicit Sequence (#1957) — the #788 recovery
// death signal the infra refund keys on when the category is "C". This is the
// post-agent-work shape: a FAILED fix-up re-dispatch recovered back to the
// review gate.
func seedFixupRecoveredC(au *auditFake, runID, stageID uuid.UUID, cat run.FailureCategory, seq int64) {
	payload, _ := json.Marshal(map[string]any{
		"stage_id":                stageID.String(),
		"restored_state":          string(run.StageStateSucceeded),
		"source_failure_category": string(cat),
	})
	rid := runID
	sid := stageID
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryStageFixupRecovered, Sequence: seq, Payload: payload})
	au.mu.Unlock()
}

// lastFixupTriggeredPayload returns the decoded payload of the most-recent
// appended stage_fixup_triggered entry, the receipt the refund tests assert on.
func lastFixupTriggeredPayload(t *testing.T, au *auditFake) map[string]any {
	t.Helper()
	var last *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryStageFixupTriggered {
			last = &au.appended[i]
		}
	}
	if last == nil {
		t.Fatal("no stage_fixup_triggered entry appended")
	}
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	return payload
}

// TestFixupStage_NoChangeRefundAdmitsSecondPass: a fix-up pass whose
// re-dispatch produced no commit (fixup_no_changes audit entry, #856) is
// refunded against the NORMAL budget (#967), so a second trigger is
// admitted WITHOUT force_additional_pass and the refund is recorded on the
// audit payload's refunded_passes field.
func TestFixupStage_NoChangeRefundAdmitsSecondPass(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Pass 1: normal, spends the budget...
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("first fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// ...but the re-dispatch produced no commit: the #856 report path
	// recorded a fixup_no_changes entry for the stage.
	seedFixupNoChanges(au, stage)
	reparkFixupStage(repo, stage.ID)

	// Pass 2: admitted without force — the no-change pass was refunded.
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "retry after no-op pass"})
	if w.Code != http.StatusOK {
		t.Fatalf("refunded second fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// The new stage_fixup_triggered entry records the refund.
	var last *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryStageFixupTriggered {
			last = &au.appended[i]
		}
	}
	if last == nil {
		t.Fatal("no stage_fixup_triggered entry appended")
	}
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["refunded_passes"].(float64) != 1 {
		t.Errorf("refunded_passes = %v, want 1", payload["refunded_passes"])
	}
	if payload["forced"] != false {
		t.Errorf("forced = %v, want false — the refunded pass is within the normal budget", payload["forced"])
	}

	// Pass 3 without force: the single refund is consumed (raw=2,
	// refunded=1, effective=1 >= max=1) — refused with the normal
	// budget-exhausted code, and its details carry the refund count.
	reparkFixupStage(repo, stage.ID)
	w = postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("third fixup status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "refunded_passes") {
		t.Errorf("budget-exhausted details missing refunded_passes: %s", w.Body.String())
	}
}

// TestFixupStage_NoChangeRefundNeverExtendsCeiling: the refund applies to
// the NORMAL budget only — the absolute hard ceiling keeps counting RAW
// stage_fixup_triggered entries, so 3 triggered passes hard-stop the stage
// even when one of them was a refunded no-change pass (#967).
func TestFixupStage_NoChangeRefundNeverExtendsCeiling(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Pass 1 (normal), then a no-change report refunds it.
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("pass 1 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	seedFixupNoChanges(au, stage)
	// Pass 2 (refund-admitted) and pass 3 (forced) reach the raw ceiling.
	reparkFixupStage(repo, stage.ID)
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("pass 2 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	reparkFixupStage(repo, stage.ID)
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true}); w.Code != http.StatusOK {
		t.Fatalf("pass 3 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// 4th attempt: 3 RAW passes triggered — refused with the distinct
	// ceiling code despite the refund, even when forced.
	reparkFixupStage(repo, stage.ID)
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ceiling status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_ceiling_reached") {
		t.Errorf("body missing fixup_ceiling_reached code: %s", w.Body.String())
	}
}

// TestFixupStage_InfraRefundAdmitsSecondPass: a fix-up pass that died
// category-C on a spawn-phase reaper (dispatch_reaper_failed, failure_category
// "C", #1747) before delivering anything to the PR branch is refunded against
// the NORMAL budget (#1957), so a second trigger is admitted WITHOUT
// force_additional_pass and the refund is recorded on the audit payload.
func TestFixupStage_InfraRefundAdmitsSecondPass(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Pass 1 was triggered (seq 10) then its runner died category-C on a
	// spawn-phase reaper (seq 12, inside the trigger's open-ended window).
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)
	seedDispatchReaperFailed(au, stage.RunID, stage.ID, run.FailureC, 12)

	// Pass 2: admitted without force — the infra-killed pass was refunded.
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "retry after infra death"})
	if w.Code != http.StatusOK {
		t.Fatalf("refunded second fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := lastFixupTriggeredPayload(t, au)
	if payload["refunded_passes"].(float64) != 1 {
		t.Errorf("refunded_passes = %v, want 1", payload["refunded_passes"])
	}
	if payload["forced"] != false {
		t.Errorf("forced = %v, want false — the refunded pass is within the normal budget", payload["forced"])
	}
}

// TestFixupStage_InfraRefund_RecoveredCategoryC pins the post-agent-work
// category-C recovery as REFUNDABLE (#1957, the operator's DELIBERATE-ACCEPTANCE
// of the delivered-nothing invariant): a fix-up pass whose re-dispatch ran the
// agent but FAILED category-C on the push/report and was recovered back to the
// review gate (stage_fixup_recovered, source_failure_category "C", #788) landed
// NOTHING on the PR branch. Under the delivered-nothing invariant it refunds
// exactly like a spawn-phase reaper death — the RAW-trigger hard ceiling, not a
// forced override, is the loop bound — so the second pass is admitted WITHOUT
// force_additional_pass and the refund is recorded on the audit payload.
func TestFixupStage_InfraRefund_RecoveredCategoryC(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)
	seedFixupRecoveredC(au, stage.RunID, stage.ID, run.FailureC, 12)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "retry after recovered category-C death"})
	if w.Code != http.StatusOK {
		t.Fatalf("second fixup status = %d, want 200 (post-agent-work recovery delivered nothing, so it refunds):\n%s", w.Code, w.Body.String())
	}
	payload := lastFixupTriggeredPayload(t, au)
	if payload["refunded_passes"].(float64) != 1 {
		t.Errorf("refunded_passes = %v, want 1", payload["refunded_passes"])
	}
	if payload["forced"] != false {
		t.Errorf("forced = %v, want false — the refunded pass is within the normal budget", payload["forced"])
	}
}

// TestFixupStage_InfraRefund_CategoryBNotRefunded: a recovered pass whose
// source failure was category B (policy) does NOT refund — only category C
// (infrastructure) qualifies, so the second pass is refused with the normal
// budget-exhausted code (#1957).
func TestFixupStage_InfraRefund_CategoryBNotRefunded(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)
	seedFixupRecoveredC(au, stage.RunID, stage.ID, run.FailureB, 12) // policy, not infra

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second fixup status = %d, want 422 (category B does not refund):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
}

// TestFixupStage_InfraRefund_PreTriggerReaperNotRefunded: a category-C reaper
// death sequenced BEFORE the first (only) trigger — an original-dispatch spawn
// death, not a fix-up pass — falls in no trigger window and never refunds
// (#1957). The second pass is refused, budget-exhausted.
func TestFixupStage_InfraRefund_PreTriggerReaperNotRefunded(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// The reaper (seq 5) precedes the fix-up trigger (seq 10): the death belongs
	// to the original implement dispatch, not this fix-up pass — window (10,+inf)
	// excludes it.
	seedDispatchReaperFailed(au, stage.RunID, stage.ID, run.FailureC, 5)
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second fixup status = %d, want 422 (pre-trigger reaper must not refund):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
}

// TestFixupStage_InfraRefund_BothSignalsOneWindowRefundOnce: a reaper AND a
// recovered entry, both category C, landing in ONE trigger window refund that
// pass exactly ONCE, not twice (#1957 per-window pairing). Two RAW triggers are
// seeded so the refunded>prior clamp cannot mask a double-count: the correct
// single refund leaves the effective budget at 2 (raw 2 >= max 2 → 422), while
// a double-count would widen it to 3 and wrongly ADMIT the pass.
func TestFixupStage_InfraRefund_BothSignalsOneWindowRefundOnce(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 20)
	// Both C-death signals fall inside the FIRST trigger's window (10,20).
	seedDispatchReaperFailed(au, stage.RunID, stage.ID, run.FailureC, 12)
	seedFixupRecoveredC(au, stage.RunID, stage.ID, run.FailureC, 13)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("third fixup status = %d, want 422 — both signals in one window must refund only once (a double-count would admit):\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"refunded_passes":1`) {
		t.Errorf("budget-exhausted details must carry refunded_passes:1 (single refund), got: %s", w.Body.String())
	}
}

// TestFixupStage_InfraRefundNeverExtendsCeiling: the infra refund applies to the
// NORMAL budget only — the absolute hard ceiling keeps counting RAW
// stage_fixup_triggered entries, so 3 triggered passes hard-stop the stage even
// with an infra refund in play, even when forced (#1957, mirroring the #967
// ceiling guarantee).
func TestFixupStage_InfraRefundNeverExtendsCeiling(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Three RAW triggered passes plus a category-C death inside the first
	// window: the refund would widen the NORMAL budget but never the ceiling.
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 20)
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 30)
	seedDispatchReaperFailed(au, stage.RunID, stage.ID, run.FailureC, 12)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 at the ceiling:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_ceiling_reached") {
		t.Errorf("body missing fixup_ceiling_reached code (refund must not extend the ceiling): %s", w.Body.String())
	}
}

// TestFixupStage_InfraRefundCountReadErrorFails500: the infra refund reads the
// stage's dispatch_reaper_failed audit entries; a read failure there must
// refuse the trigger with a 500 rather than silently treating the refund count
// as 0. Only the dispatch_reaper_failed read errors — the earlier reads
// (implement_reviewed, stage_fixup_triggered, fixup_no_changes) succeed,
// pinning the failure to countFixupInfraRefunds.
func TestFixupStage_InfraRefundCountReadErrorFails500(t *testing.T) {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:    "127.0.0.1:0",
		RunRepo: repo,
		AuditRepo: &categoryErrAuditRepo{
			auditFake:   au,
			errCategory: CategoryDispatchReaperFailed,
			err:         errors.New("audit store unavailable"),
		},
	})
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)
	// A prior trigger so countFixupInfraRefunds proceeds past its empty-trigger
	// early return into the erroring dispatch_reaper_failed read.
	seedFixupTriggeredSeq(au, stage.RunID, stage.ID, 10)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "count infra-refunded fix-up passes failed") {
		t.Errorf("body missing the infra-refund-count error message: %s", w.Body.String())
	}
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("stage_fixup_triggered appended despite the 500")
		}
	}
}

// categoryErrAuditRepo wraps auditFake to fail ListForRunByCategory for
// ONE category only. Handlers that read several categories in sequence
// (the fix-up handler: implement_reviewed → stage_fixup_triggered →
// fixup_no_changes) need a later read to fail while earlier ones succeed,
// which the fake's blanket listByCategoryErr knob can't do.
type categoryErrAuditRepo struct {
	*auditFake
	errCategory string
	err         error
}

func (c *categoryErrAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == c.errCategory {
		return nil, c.err
	}
	return c.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestFixupStage_RefundCountReadErrorFails500: the budget refund depends on
// reading the stage's fixup_no_changes audit entries; a read failure there
// must refuse the trigger with a 500 rather than silently treating the
// refund count as 0 (which could wrongly reject an admissible pass). Only
// the fixup_no_changes read errors — the earlier implement_reviewed and
// stage_fixup_triggered reads succeed, pinning the failure to
// countFixupNoChangeRefunds.
func TestFixupStage_RefundCountReadErrorFails500(t *testing.T) {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	s := New(Config{
		Addr:    "127.0.0.1:0",
		RunRepo: repo,
		AuditRepo: &categoryErrAuditRepo{
			auditFake:   au,
			errCategory: "fixup_no_changes",
			err:         errors.New("audit store unavailable"),
		},
	})
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "count refunded fix-up passes failed") {
		t.Errorf("body missing the refund-count error message: %s", w.Body.String())
	}
	// No stage_fixup_triggered entry was recorded — the trigger was refused
	// before the transition.
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("stage_fixup_triggered appended despite the 500")
		}
	}
}

// TestFixupStage_RefundClampedToPriorPasses: a fixup_no_changes entry with
// NO prior stage_fixup_triggered pass (impossible in normal flow — the
// report path only records it after a triggered pass — but reachable via
// manual audit writes or partial history) must not widen the budget past
// the configured max: the refund is clamped to the prior-pass count (0
// here) and audited as such.
func TestFixupStage_RefundClampedToPriorPasses(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)
	// A no-change entry with no triggered pass behind it.
	seedFixupNoChanges(au, stage)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var last *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryStageFixupTriggered {
			last = &au.appended[i]
		}
	}
	if last == nil {
		t.Fatal("no stage_fixup_triggered entry appended")
	}
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	// Without the clamp this would read 1 (and MaxPasses would have been
	// widened to 2 with zero passes actually triggered).
	if payload["refunded_passes"].(float64) != 0 {
		t.Errorf("refunded_passes = %v, want 0 (clamped to the prior-pass count)", payload["refunded_passes"])
	}
}

func TestFixupStage_ForceAdditionalPassGrantsForcedPass(t *testing.T) {
	// The normal budget (1) is spent after one pass; force_additional_pass
	// grants one more, audited as forced (#860).
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Pass 1: normal, spends the budget.
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("first fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	reparkFixupStage(repo, stage.ID)

	// Pass 2: budget spent, but the override grants it.
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "one more pass", ForceAdditionalPass: true})
	if w.Code != http.StatusOK {
		t.Fatalf("forced fixup status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// The latest stage_fixup_triggered entry records the forced override.
	var last *audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryStageFixupTriggered {
			last = &au.appended[i]
		}
	}
	if last == nil {
		t.Fatal("no stage_fixup_triggered entry appended")
	}
	var payload map[string]any
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload["forced"] != true {
		t.Errorf("forced = %v, want true", payload["forced"])
	}
	if payload["hard_ceiling"].(float64) != float64(defaultFixupCeiling) {
		t.Errorf("hard_ceiling = %v, want %d", payload["hard_ceiling"], defaultFixupCeiling)
	}
	if reason, _ := payload["admissibility_reason"].(string); !strings.Contains(reason, "operator-forced override") {
		t.Errorf("admissibility_reason = %q, want it to note the operator-forced override", reason)
	}
}

func TestFixupStage_CeilingReachedReturns422(t *testing.T) {
	// Drive the stage to the hard ceiling (3 total passes) and assert the
	// next attempt is refused with the DISTINCT fixup_ceiling_reached code,
	// not fixup_budget_exhausted (#860).
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	// Pass 1 (normal) + passes 2 and 3 (forced) consume the ceiling.
	if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}}); w.Code != http.StatusOK {
		t.Fatalf("pass 1 status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	for i := 2; i <= 3; i++ {
		reparkFixupStage(repo, stage.ID)
		if w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true}); w.Code != http.StatusOK {
			t.Fatalf("pass %d status = %d, want 200:\n%s", i, w.Code, w.Body.String())
		}
	}

	// 4th attempt: at the ceiling, even forced, refused with the distinct code.
	reparkFixupStage(repo, stage.ID)
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("ceiling status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_ceiling_reached") {
		t.Errorf("body missing fixup_ceiling_reached code: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body should carry the distinct ceiling code, not budget_exhausted: %s", w.Body.String())
	}
	// #1097: the 422 must surface the vouch-commit remediation pointer so a
	// late CI/SAST finding has a discoverable in-loop path at the ceiling.
	if !strings.Contains(w.Body.String(), "remediation") || !strings.Contains(w.Body.String(), "fishhawk_vouch_commit") {
		t.Errorf("body missing the vouch-commit remediation pointer: %s", w.Body.String())
	}
	// Exactly three fix-up entries — the refused pass wrote none.
	n := 0
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			n++
		}
	}
	if n != 3 {
		t.Errorf("stage_fixup_triggered entries = %d, want 3", n)
	}
}

func TestFixupStage_NoConcernsSelectedReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: nil})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_OutOfRangeIndexReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{5}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_DuplicateIndexReturns400(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "verification", Note: "untested"},
	)

	// Both indices are in range, but the duplicate must be rejected by
	// selectConcerns (mapped to 400) — not silently deduplicated.
	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0, 0}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a duplicate concern index:\n%s", w.Code, w.Body.String())
	}
}

func TestFixupStage_NoRecordedConcernsReturns422(t *testing.T) {
	s, repo, _ := fixupServer(t)
	stage := seedImplementGateStage(repo)
	// No implement_reviewed entry seeded.

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_WrongStateReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Implement stage that is running, not parked at the gate.
	stage := repo.seedGatelessStage(run.StageStateRunning)
	repo.seedRun(&run.Run{ID: stage.RunID, State: run.StateRunning})
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_TerminalRunReturns422(t *testing.T) {
	// #968: a run that already rolled up terminal has no live gate to flow
	// a fix-up back into — the handler surfaces run.FixupStage's terminal-
	// run refusal as fixup_not_applicable, even for a forced pass.
	s, repo, au := fixupServer(t)
	impl, review := seedPushOpenPRStages(repo, run.StageStateAwaitingApproval)
	// Overwrite the run row terminal — the incident shape: the run rolled
	// up succeeded while the review gate stayed open.
	repo.seedRun(&run.Run{ID: impl.RunID, State: run.StateSucceeded})
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}, ForceAdditionalPass: true})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
	// Neither stage may be touched.
	if cur, _ := repo.GetStage(context.Background(), impl.ID); cur.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want unchanged (succeeded)", cur.State)
	}
	if cur, _ := repo.GetStage(context.Background(), review.ID); cur.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want unchanged (awaiting_approval)", cur.State)
	}
	// No stage_fixup_triggered entry may have been written.
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("unexpected %s audit entry on a refused fix-up", CategoryStageFixupTriggered)
		}
	}
}

func TestFixupStage_NonImplementStageReturns422(t *testing.T) {
	s, repo, au := fixupServer(t)
	// Plan stage parked at the gate is not a fix-up candidate.
	stage := repo.seedStage(run.StageStateAwaitingApproval)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("body missing fixup_not_applicable code: %s", w.Body.String())
	}
}

func TestFixupStage_UnauthenticatedReturns401(t *testing.T) {
	s, repo, _ := fixupServer(t)
	stage := seedImplementGateStage(repo)

	raw, _ := json.Marshal(fixupRequest{Concerns: []int{0}})
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/fixup", bytes.NewReader(raw))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, req) // no identity injected → anonymous

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

// --- Subject-binding guard ---

func withMCPFixupAuth(req *http.Request, runID uuid.UUID) *http.Request {
	id := Identity{
		Subject: "mcp:run:" + runID.String(),
		TokenID: "tok-test",
		Scopes:  []string{"mcp:read", "write:fixups"},
	}
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
}

// --- Fix-up recovery (maybeRecoverFixupFailure, #788) ---

// fixupRecoveryRepo extends orchestratorRepo to admit the fix-up RECOVERY
// edges (failed → succeeded/awaiting_approval, review pending →
// awaiting_approval, #788) and to clear failure metadata on a nil
// completion — exactly what the production postgresRepo does via
// ValidStageFixupRecoveryTransition + UpdateStageState. The base
// orchestratorRepo only admits the normal transitions, so
// RestoreFixupStage's recovery transitions would be refused without this.
type fixupRecoveryRepo struct {
	*orchestratorRepo
}

func (r *fixupRecoveryRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stagesByID[id]
	if !ok {
		return nil, run.ErrNotFound
	}
	if s.State == to {
		return s, nil
	}
	if !run.ValidStageTransition(s.State, to) &&
		!run.ValidStageFixupTransition(s.State, to) &&
		!run.ValidStageFixupRecoveryTransition(s.State, to) {
		return nil, run.InvalidTransitionError{Kind: "stage", From: string(s.State), To: string(to)}
	}
	s.State = to
	if c != nil {
		s.FailureCategory = c.FailureCategory
		s.FailureReason = c.FailureReason
	} else {
		s.FailureCategory = nil
		s.FailureReason = nil
	}
	return s, nil
}

// seedFixupTriggered appends the durable stage_fixup_triggered audit
// entry the recovery detector keys off — recording the implement stage's
// prior gate state and (on the push_and_open_pr flow) the re-parked
// review stage id.
func seedFixupTriggered(t *testing.T, au *storingAuditFake, runID, stageID uuid.UUID, priorState run.StageState, reviewID *uuid.UUID) {
	t.Helper()
	fields := map[string]any{
		"stage_id":    stageID.String(),
		"prior_state": string(priorState),
	}
	if reviewID != nil {
		fields["reparked_review_stage_id"] = reviewID.String()
	}
	payload, _ := json.Marshal(fields)
	kind := audit.ActorKind("user")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Category:  CategoryStageFixupTriggered,
		ActorKind: &kind,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed stage_fixup_triggered: %v", err)
	}
}

// seedFailedFixupRun stands up a run (running) with a failed implement
// stage and a re-parked (pending) review stage — the state a
// push_and_open_pr fix-up re-dispatch failure lands in. Returns the run,
// implement, and review rows.
func seedFailedFixupRun(rr *fixupRecoveryRepo) (*run.Run, *run.Stage, *run.Stage) {
	runRow := rr.seedRun() // StateRunning
	impl := rr.seedStage(runRow.ID, 1, run.StageStateFailed)
	review := rr.seedStage(runRow.ID, 2, run.StageStatePending)
	rr.mu.Lock()
	impl.Type = run.StageTypeImplement
	cat := run.FailureA
	reason := "agent crashed mid fix-up"
	impl.FailureCategory = &cat
	impl.FailureReason = &reason
	review.Type = run.StageTypeReview
	rr.mu.Unlock()
	return runRow, impl, review
}

func TestMaybeRecoverFixupFailure_RecoversAndEmits(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newStoringAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	ctx := context.Background()

	runRow, impl, review := seedFailedFixupRun(rr)
	seedFixupTriggered(t, au, runRow.ID, impl.ID, run.StageStateSucceeded, &review.ID)

	if !s.maybeRecoverFixupFailure(ctx, runRow.ID, impl.ID) {
		t.Fatal("maybeRecoverFixupFailure = false, want true (recovered)")
	}

	// Implement restored to succeeded with cleared failure metadata.
	curImpl, _ := rr.GetStage(ctx, impl.ID)
	if curImpl.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded", curImpl.State)
	}
	if curImpl.FailureCategory != nil || curImpl.FailureReason != nil {
		t.Errorf("implement failure metadata = (%v, %v), want both nil", curImpl.FailureCategory, curImpl.FailureReason)
	}
	// Review re-parked back to its gate.
	curReview, _ := rr.GetStage(ctx, review.ID)
	if curReview.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval", curReview.State)
	}

	// A stage_fixup_recovered audit entry landed carrying the source failure.
	entries, err := au.ListForRunByCategory(ctx, runRow.ID, CategoryStageFixupRecovered)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stage_fixup_recovered entries = %d, want 1", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal stage_fixup_recovered payload: %v", err)
	}
	if payload["restored_state"] != string(run.StageStateSucceeded) {
		t.Errorf("restored_state = %v, want succeeded", payload["restored_state"])
	}
	if payload["source_failure_category"] != string(run.FailureA) {
		t.Errorf("source_failure_category = %v, want A", payload["source_failure_category"])
	}
	if payload["restored_review_stage_id"] != review.ID.String() {
		t.Errorf("restored_review_stage_id = %v, want %s", payload["restored_review_stage_id"], review.ID)
	}
}

func TestMaybeRecoverFixupFailure_NoPriorEntryReturnsFalse(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newStoringAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au})
	ctx := context.Background()

	runRow, impl, _ := seedFailedFixupRun(rr)
	// No stage_fixup_triggered entry seeded: this is an ordinary failure.

	if s.maybeRecoverFixupFailure(ctx, runRow.ID, impl.ID) {
		t.Fatal("maybeRecoverFixupFailure = true, want false (no fix-up entry → normal failure path)")
	}
	// The implement stage was not touched.
	if curImpl, _ := rr.GetStage(ctx, impl.ID); curImpl.State != run.StageStateFailed {
		t.Errorf("implement state = %q, want unchanged (failed)", curImpl.State)
	}
	// No recovery audit entry written.
	entries, _ := au.ListForRunByCategory(ctx, runRow.ID, CategoryStageFixupRecovered)
	if len(entries) != 0 {
		t.Errorf("stage_fixup_recovered entries = %d, want 0", len(entries))
	}
}

func TestAdvanceAfterFailure_SkipsAdvanceOnRecovery(t *testing.T) {
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newStoringAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	runRow, impl, review := seedFailedFixupRun(rr)
	seedFixupTriggered(t, au, runRow.ID, impl.ID, run.StageStateSucceeded, &review.ID)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	s.advanceAfterFailure(req, runRow.ID, impl.ID)

	// Recovery ran, so the run-failing Advance was skipped: the run stays
	// running and the stages are restored to their pre-fix-up gate.
	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running (Advance skipped on recovery)", curRun.State)
	}
	if curImpl, _ := rr.GetStage(context.Background(), impl.ID); curImpl.State != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (restored)", curImpl.State)
	}
	if curReview, _ := rr.GetStage(context.Background(), review.ID); curReview.State != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (restored)", curReview.State)
	}
}

func TestAdvanceAfterFailure_NoFixupEntryFailsRun(t *testing.T) {
	// The differential control for the skip test: with NO fix-up entry the
	// same failed implement stage drives the orchestrator to complete the
	// run as failed — the behavior recovery overrides.
	rr := &fixupRecoveryRepo{orchestratorRepo: newOrchestratorRepo()}
	au := newStoringAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		AuditRepo:    au,
		Orchestrator: &orchestrator.Orchestrator{Runs: rr},
	})

	runRow, impl, _ := seedFailedFixupRun(rr)
	// No stage_fixup_triggered entry: ordinary failure path.

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	s.advanceAfterFailure(req, runRow.ID, impl.ID)

	curRun, _ := rr.GetRun(context.Background(), runRow.ID)
	if curRun.State != run.StateFailed {
		t.Errorf("run state = %q, want failed (no recovery → normal Advance-to-failed)", curRun.State)
	}
	if curImpl, _ := rr.GetStage(context.Background(), impl.ID); curImpl.State != run.StageStateFailed {
		t.Errorf("implement state = %q, want unchanged (failed)", curImpl.State)
	}
}

func TestFixupStage_MCPTokenMismatchedRunReturns403(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)
	otherRunID := uuid.New() // does not match stage.RunID

	raw, _ := json.Marshal(fixupRequest{Concerns: []int{0}})
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/fixup", bytes.NewReader(raw))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, withMCPFixupAuth(req, otherRunID))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cross_run_fixup") {
		t.Errorf("body missing cross_run_fixup code: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Stable concern-ID addressing (#964)
// ---------------------------------------------------------------------------

// seqAuditFake wraps auditFake to stamp a monotonically increasing
// sequence on the entry AppendChained returns, mirroring the Postgres
// repository's RETURNING contract. The review loops persist concern
// rows with origin_review_sequence taken from that returned entry, so
// tests asserting the sequence threading need real values here.
type seqAuditFake struct {
	*auditFake
	nextSeq int64
	entries []*audit.Entry
}

func newSeqAuditFake() *seqAuditFake {
	return &seqAuditFake{auditFake: newAuditFake(), nextSeq: 100}
}

func (a *seqAuditFake) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	e, err := a.auditFake.AppendChained(ctx, p)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextSeq++
	e.Sequence = a.nextSeq
	e.Category = p.Category
	e.StageID = p.StageID
	e.Payload = p.Payload
	a.entries = append(a.entries, e)
	return e, nil
}

// entriesByCategory returns the sequence-stamped entries of one category
// in append order.
func (a *seqAuditFake) entriesByCategory(cat string) []*audit.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.entries {
		if e.Category == cat {
			out = append(out, e)
		}
	}
	return out
}

// fakeConcernRepo is an in-memory concern.Repository for handler tests.
// State transitions run through the real concern.Transition machine so
// the fake cannot drift from the production lifecycle rules. The
// Postgres adapter is exercised separately in
// backend/internal/concern/postgres_test.go via testcontainers.
type fakeConcernRepo struct {
	mu        sync.Mutex
	rows      []*concern.Concern
	insertErr error
	listErr   error
}

func newFakeConcernRepo() *fakeConcernRepo { return &fakeConcernRepo{} }

func (f *fakeConcernRepo) InsertRaised(_ context.Context, p concern.InsertRaisedParams) ([]*concern.Concern, error) {
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var rm *string
	if p.ReviewerModel != "" {
		m := p.ReviewerModel
		rm = &m
	}
	out := make([]*concern.Concern, 0, len(p.Concerns))
	for _, c := range p.Concerns {
		row := &concern.Concern{
			ID:                   uuid.New(),
			RunID:                p.RunID,
			StageID:              p.StageID,
			StageKind:            p.StageKind,
			OriginReviewSequence: p.OriginReviewSequence,
			ReviewerModel:        rm,
			Severity:             c.Severity,
			Category:             c.Category,
			Note:                 c.Note,
			SuggestedPatch:       c.SuggestedPatch,
			State:                concern.StateRaised,
		}
		f.rows = append(f.rows, row)
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeConcernRepo) GetByIDs(_ context.Context, ids []uuid.UUID) ([]*concern.Concern, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*concern.Concern, 0, len(ids))
	for _, id := range ids {
		var found *concern.Concern
		for _, row := range f.rows {
			if row.ID == id {
				found = row
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%w: %s", concern.ErrNotFound, id)
		}
		out = append(out, found)
	}
	return out, nil
}

func (f *fakeConcernRepo) ListByRun(_ context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*concern.Concern
	for _, row := range f.rows {
		if row.RunID == runID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeConcernRepo) ListOpenByRun(_ context.Context, runID uuid.UUID) ([]*concern.Concern, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*concern.Concern
	for _, row := range f.rows {
		if row.RunID == runID && row.State.IsOpen() {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeConcernRepo) MarkAddressedPending(ctx context.Context, ids []uuid.UUID, reason string) error {
	rows, err := f.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range rows {
		if row.State == concern.StateAddressedPending {
			continue
		}
		if err := concern.Transition(row.State, concern.StateAddressedPending); err != nil {
			return err
		}
		row.State = concern.StateAddressedPending
		row.StateReason = reason
	}
	return nil
}

func (f *fakeConcernRepo) ApplyResolution(ctx context.Context, id uuid.UUID, to concern.State, reason string) (*concern.Concern, error) {
	rows, err := f.GetByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row := rows[0]
	if err := concern.Transition(row.State, to); err != nil {
		return nil, err
	}
	row.State = to
	row.StateReason = reason
	return row, nil
}

// seedConcernRow inserts one concern row directly, returning it.
func seedConcernRow(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID, stageKind string, seq int64, note string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            stageKind,
		ReviewerModel:        "claude-opus-4-8",
		OriginReviewSequence: seq,
		Concerns:             []concern.RaisedConcern{{Severity: "medium", Category: "scope", Note: note}},
	})
	if err != nil {
		t.Fatalf("seed concern: %v", err)
	}
	return rows[0]
}

// fixupServerWithConcerns is fixupServer plus a wired concern store, for
// the stable-ID addressing path (#964).
func fixupServerWithConcerns(t *testing.T) (*Server, *approvalRunRepo, *auditFake, *fakeConcernRepo) {
	t.Helper()
	repo := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   au,
		ConcernRepo: cr,
	})
	return s, repo, au, cr
}

// TestFixupStage_ConcernIDs_RoutesExactConcern is the run-73456dc8
// mis-route regression: with concerns persisted from TWO review entries,
// addressing one from the SECOND entry by stable ID flips exactly that
// concern to addressed_pending — a flattened positional index 0 would
// have resolved into the first entry's set.
func TestFixupStage_ConcernIDs_RoutesExactConcern(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	first := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 101, "first reviewer's concern")
	second := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 102, "second reviewer's concern")

	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs: []string{second.ID.String()},
		Reason:     "address the second entry's concern",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	// Exactly the addressed concern flips addressed_pending with the
	// operator reason; the other stays raised.
	if second.State != concern.StateAddressedPending {
		t.Errorf("second.State = %q, want addressed_pending", second.State)
	}
	if second.StateReason != "address the second entry's concern" {
		t.Errorf("second.StateReason = %q", second.StateReason)
	}
	if first.State != concern.StateRaised {
		t.Errorf("first.State = %q, want raised (untouched)", first.State)
	}

	// The audit payload records the routed stable ID alongside the
	// embedded concern copy the prompt renderer reads back.
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	ids, ok := payload["concern_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != second.ID.String() {
		t.Errorf("payload.concern_ids = %v, want [%s]", payload["concern_ids"], second.ID)
	}
	concerns, ok := payload["concerns"].([]any)
	if !ok || len(concerns) != 1 {
		t.Fatalf("payload.concerns = %v, want one resolved concern", payload["concerns"])
	}
	if note := concerns[0].(map[string]any)["note"]; note != "second reviewer's concern" {
		t.Errorf("resolved concern note = %v, want the second entry's", note)
	}
}

// TestFixupStage_ConcernIDs_BothFormsRejected pins mixed addressing as
// validation_failed: silently preferring one form would reintroduce the
// ambiguity stable IDs remove.
func TestFixupStage_ConcernIDs_BothFormsRejected(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	c := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 101, "concern")

	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs: []string{c.ID.String()},
		Concerns:   []int{0},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed code: %s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit entries = %d, want 0 (validation precedes any state change)", len(au.appended))
	}
	if c.State != concern.StateRaised {
		t.Errorf("concern state = %q, want raised (untouched)", c.State)
	}
}

// TestFixupStage_ConcernIDs_PlanStageConcernRejected is the binding
// approval-condition case: a plan-stage concern UUID surfaced by the
// same run-status block must NEVER route into an implement fix-up — the
// stage_kind/stage_id mismatch is validation_failed, explicitly.
func TestFixupStage_ConcernIDs_PlanStageConcernRejected(t *testing.T) {
	s, repo, _, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	planStageID := uuid.New()
	planConcern := seedConcernRow(t, cr, stage.RunID, planStageID, concern.StageKindPlan, 50, "plan-stage concern")

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{planConcern.ID.String()}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "validation_failed") {
		t.Errorf("body missing validation_failed code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "plan-stage concern") {
		t.Errorf("body should name the plan-stage mismatch explicitly: %s", w.Body.String())
	}
	if planConcern.State != concern.StateRaised {
		t.Errorf("plan concern state = %q, want raised (untouched)", planConcern.State)
	}
}

// TestFixupStage_ConcernIDs_ForeignStageRejected covers an implement
// concern belonging to a DIFFERENT stage of the run.
func TestFixupStage_ConcernIDs_ForeignStageRejected(t *testing.T) {
	s, repo, _, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	otherStageID := uuid.New()
	foreign := seedConcernRow(t, cr, stage.RunID, otherStageID, concern.StageKindImplement, 60, "another stage's concern")

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{foreign.ID.String()}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "different run/stage") {
		t.Errorf("body should name the run/stage mismatch: %s", w.Body.String())
	}
}

func TestFixupStage_ConcernIDs_UnknownIDRejected(t *testing.T) {
	s, repo, _, _ := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{uuid.New().String()}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown concern_id") {
		t.Errorf("body should report the unknown id: %s", w.Body.String())
	}
}

func TestFixupStage_ConcernIDs_MalformedUUIDRejected(t *testing.T) {
	s, repo, _, _ := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{"not-a-uuid"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

// TestFixupStage_ConcernIDs_NonOpenRejected: a concern already resolved
// (addressed) cannot be routed again.
func TestFixupStage_ConcernIDs_NonOpenRejected(t *testing.T) {
	s, repo, _, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	c := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 70, "resolved concern")
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{c.ID}, "routed"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}
	if _, err := cr.ApplyResolution(context.Background(), c.ID, concern.StateAddressed, "confirmed"); err != nil {
		t.Fatalf("ApplyResolution: %v", err)
	}

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{c.ID.String()}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not open") {
		t.Errorf("body should report the non-open state: %s", w.Body.String())
	}
}

// TestFixupStage_ConcernIDs_NoRepo503: the ID path needs the concern
// store; without one the handler refuses rather than silently falling
// back to positional resolution.
func TestFixupStage_ConcernIDs_NoRepo503(t *testing.T) {
	s, repo, au := fixupServer(t) // no ConcernRepo wired
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{uuid.New().String()}})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_unconfigured") {
		t.Errorf("body missing fixup_unconfigured code: %s", w.Body.String())
	}
}

// TestFixupStage_LegacyIndices_StillWork pins the deprecated positional
// path: indices alone resolve against the audit-entry concern set even
// with a concern store wired, and no concern row is touched (positional
// routing predates stable IDs and has no ID to mark).
func TestFixupStage_LegacyIndices_StillWork(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)
	c := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 80, "stored concern")

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, Reason: "legacy path"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if c.State != concern.StateRaised {
		t.Errorf("stored concern state = %q, want raised (legacy path never marks rows)", c.State)
	}
}

// TestFixupStage_Drive_StampsReparkRule pins the fixup_rereview_repark
// drive stamp (#1023): on a drive-enabled run, the push_and_open_pr
// re-park lands a run_auto_advanced entry keyed to the re-parked
// REVIEW stage, alongside the existing stage_fixup_triggered entry.
func TestFixupStage_Drive_StampsReparkRule(t *testing.T) {
	s, repo, au := fixupServer(t)
	impl, review := seedPushOpenPRStages(repo, run.StageStateAwaitingApproval)
	repo.seedRun(&run.Run{ID: impl.RunID, State: run.StateRunning, Drive: true})
	seedConcernsReview(au, impl,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "out-of-scope file"},
	)

	w := postFixup(t, s, impl.ID, fixupRequest{Concerns: []int{0}, Reason: "address scope drift"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var advances []drive.Advance
	for _, e := range au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			t.Fatalf("run_auto_advanced payload unmarshal: %v", err)
		}
		if e.StageID == nil || *e.StageID != review.ID {
			t.Errorf("run_auto_advanced keyed to stage %v, want re-parked review %s", e.StageID, review.ID)
		}
		advances = append(advances, adv)
	}
	if len(advances) != 1 {
		t.Fatalf("run_auto_advanced entries = %d, want 1", len(advances))
	}
	if advances[0].Rule != drive.RuleFixupRereviewRepark {
		t.Errorf("Rule = %q, want fixup_rereview_repark", advances[0].Rule)
	}
	if advances[0].Parked {
		t.Error("Parked = true, want false: the re-park IS the mechanical advance into a fresh review round")
	}
}

// --- Delegated fix-up (ADR-040 / #1026) -------------------------------------

// delegatedActionSpecYAML is a version-0.5 spec whose workflow-level
// operator_agent block delegates the three action-handler knobs this
// package enforces (route_fixup / retry / waive). Shared by the
// delegated fix-up, retry, and waive tests.
const delegatedActionSpecYAML = `version: "0.5"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
workflows:
  feature_change:
    operator_agent:
      may_route_fixup: convergent_concerns
      may_retry: infra_flake
      may_waive: solo_low
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [tech_lead]
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// seedDelegatedRun stamps the delegated-action spec onto a stage's run
// row so checkDelegation can resolve the operator_agent block.
func seedDelegatedRun(repo *approvalRunRepo, stage *run.Stage) {
	repo.seedRun(&run.Run{
		ID:           stage.RunID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})
}

// TestFixupStage_Delegated_MetStampsAudit: with the implement review
// round settled (all verdicts in, none reject) and one open concern,
// a delegated fix-up proceeds and the stage_fixup_triggered payload
// records `delegated: "convergent_concerns"`.
func TestFixupStage_Delegated_MetStampsAudit(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := repo.seedGatelessStage(run.StageStateAwaitingApproval)
	seedDelegatedRun(repo, stage)
	seedReviewEntry(t, au, stage.RunID, 1, "implement_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: 1})
	seedReviewEntry(t, au, stage.RunID, 2, "implement_reviewed",
		planreview.ImplementReviewedPayload{
			ReviewerKind: "agent",
			Authority:    planreview.AuthorityAdvisory,
			Verdict:      planreview.VerdictApproveWithConcerns,
		})
	row := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 2, "tighten the test")

	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs: []string{row.ID.String()},
		Reason:     "route it back",
		Delegated:  true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if rule := delegatedAuditRule(t, au, CategoryStageFixupTriggered); rule != "convergent_concerns" {
		t.Errorf("audit delegated = %q, want convergent_concerns", rule)
	}
}

// TestFixupStage_Delegated_UnmetReturns403: with no implement review
// round recorded the condition is unmet — refused with the named
// predicate, no state change, no audit entry.
func TestFixupStage_Delegated_UnmetReturns403(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := repo.seedGatelessStage(run.StageStateAwaitingApproval)
	seedDelegatedRun(repo, stage)
	row := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 2, "tighten the test")

	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs: []string{row.ID.String()},
		Delegated:  true,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	errBody := decodeErrorEnvelope(t, w)
	reason, _ := errBody.Details["unmet_reason"].(string)
	if errBody.Code != "delegation_condition_unmet" ||
		!strings.Contains(reason, "no implement review round recorded") {
		t.Errorf("error = %+v, want delegation_condition_unmet naming the missing review round", errBody)
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (no state change on refusal)", stage.State)
	}
	if idx := auditEntriesByCategory(au, CategoryStageFixupTriggered); len(idx) != 0 {
		t.Errorf("stage_fixup_triggered entries = %d after refusal, want 0", len(idx))
	}
}

// TestFixupStage_Delegated_NotConfigured pins fail-closed: a run with
// no cached workflow spec (so no operator_agent block can govern it)
// refuses a delegated fix-up with delegation_not_configured.
func TestFixupStage_Delegated_NotConfigured(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	row := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 2, "tighten the test")

	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs: []string{row.ID.String()},
		Delegated:  true,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if errBody := decodeErrorEnvelope(t, w); errBody.Code != "delegation_not_configured" {
		t.Errorf("code = %q, want delegation_not_configured", errBody.Code)
	}
	if idx := auditEntriesByCategory(au, CategoryStageFixupTriggered); len(idx) != 0 {
		t.Errorf("stage_fixup_triggered entries = %d after refusal, want 0", len(idx))
	}
}

// TestFixupStage_OperatorAgentActorAttribution: a fix-up routed under
// an operator-agent token records actor_kind=agent with the full token
// subject on the stage_fixup_triggered entry (ADR-040 D4, #1027).
func TestFixupStage_OperatorAgentActorAttribution(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "drift"},
	)

	raw, _ := json.Marshal(fixupRequest{Concerns: []int{0}, Reason: "route the drift back"})
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/fixup", bytes.NewReader(raw))
	req.SetPathValue("stage_id", stage.ID.String())
	w := httptest.NewRecorder()
	s.handleFixupStage(w, withOperatorAgentAuth(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 || au.appended[0].Category != CategoryStageFixupTriggered {
		t.Fatalf("audit entries = %+v, want one stage_fixup_triggered", au.appended)
	}
	entry := au.appended[0]
	if entry.ActorKind == nil || *entry.ActorKind != audit.ActorAgent {
		t.Errorf("ActorKind = %v, want agent", entry.ActorKind)
	}
	if entry.ActorSubject == nil || *entry.ActorSubject != operatorAgentSubject {
		t.Errorf("ActorSubject = %v, want %q", entry.ActorSubject, operatorAgentSubject)
	}
}

// TestFixupStage_ApplyEligibleRecorded_PositionalPath asserts the #1165
// apply-eligibility provenance lands on the stage_fixup_triggered audit entry:
// true when EVERY routed concern carries a suggested_patch, and the patch
// rides the embedded concern copy so the prompt-serve resolver can read it back.
func TestFixupStage_ApplyEligibleRecorded_PositionalPath(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "fix import", SuggestedPatch: "diff-a"},
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "rename", SuggestedPatch: "diff-b"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0, 1}, Reason: "mechanical"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	var payload struct {
		ApplyEligible bool                 `json:"apply_eligible"`
		Concerns      []planreview.Concern `json:"concerns"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if !payload.ApplyEligible {
		t.Error("apply_eligible = false, want true when every routed concern carries a patch")
	}
	if len(payload.Concerns) != 2 || payload.Concerns[0].SuggestedPatch != "diff-a" || payload.Concerns[1].SuggestedPatch != "diff-b" {
		t.Errorf("embedded concerns must retain their suggested_patch, got %+v", payload.Concerns)
	}
}

// TestFixupStage_ApplyIneligibleWhenAPatchIsMissing covers the mixed /
// non-mechanical case (failure mode d): a single patch-less routed concern
// makes the whole pass apply-INELIGIBLE, so the runner must re-derive with the
// agent. apply_eligible is recorded false.
func TestFixupStage_ApplyIneligibleWhenAPatchIsMissing(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "has a patch", SuggestedPatch: "diff-a"},
		planreview.Concern{Severity: planreview.SeverityHigh, Category: "correctness", Note: "needs judgment"}, // no patch
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0, 1}, Reason: "mixed"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var payload struct {
		ApplyEligible bool `json:"apply_eligible"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if payload.ApplyEligible {
		t.Error("apply_eligible = true, want false when a routed concern lacks a patch")
	}
}

// TestFixupStage_ConcernIDs_CarriesSuggestedPatch pins the fixup.go
// resolveConcernsByID change: a stored concern's suggested_patch must flow onto
// the embedded concern copy in the trigger audit, otherwise the prompt-serve
// resolver could never engage the apply path for concern_ids-addressed fix-ups.
func TestFixupStage_ConcernIDs_CarriesSuggestedPatch(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	c := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 101, "mechanical fix")
	c.SuggestedPatch = "diff-xyz"

	w := postFixup(t, s, stage.ID, fixupRequest{ConcernIDs: []string{c.ID.String()}, Reason: "apply it"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var payload struct {
		ApplyEligible bool                 `json:"apply_eligible"`
		Concerns      []planreview.Concern `json:"concerns"`
	}
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal audit payload: %v", err)
	}
	if len(payload.Concerns) != 1 || payload.Concerns[0].SuggestedPatch != "diff-xyz" {
		t.Fatalf("embedded concern must carry the store's suggested_patch, got %+v", payload.Concerns)
	}
	if !payload.ApplyEligible {
		t.Error("apply_eligible = false, want true for a single patched concern")
	}
}

// TestNormalizeFixupApplyPath pins the #1165/#1213 apply-provenance normalizer:
// each of the four recognized discriminators round-trips, while an unknown string
// and the empty string both collapse to "" (the caller's signal to omit the
// apply_path key from the fixup_pushed audit entry).
func TestNormalizeFixupApplyPath(t *testing.T) {
	for _, valid := range []string{
		fixupApplyPathApplied,
		fixupApplyPathAgent,
		fixupApplyPathFailedFellback,
		fixupApplyPathFailedResetFailed,
	} {
		if got := normalizeFixupApplyPath(valid); got != valid {
			t.Errorf("normalizeFixupApplyPath(%q) = %q, want it preserved", valid, got)
		}
	}
	// Literal values guard against an accidental rename drifting from the wire
	// contract the runner ships.
	if fixupApplyPathApplied != "applied" ||
		fixupApplyPathAgent != "agent" ||
		fixupApplyPathFailedFellback != "apply_failed_fellback" ||
		fixupApplyPathFailedResetFailed != "apply_failed_reset_failed" {
		t.Fatal("apply_path constant values drifted from the runner wire contract")
	}
	for _, bad := range []string{"", "applied ", "APPLIED", "bogus", "fellback"} {
		if got := normalizeFixupApplyPath(bad); got != "" {
			t.Errorf("normalizeFixupApplyPath(%q) = %q, want \"\" (unrecognized → omit)", bad, got)
		}
	}
}

// --- free-text operator_concern (#1311) ---

// TestFixupStage_OperatorConcernOnly_ZeroConcernGate is the #1311 CodeQL
// case: an operator routes a free-text concern back on a gate-open implement
// stage that has NO recorded implement-review concern. It must re-open the
// stage to pending, fold the instruction into the routed set as a
// [high/operator] synthetic concern (the prompt renderer reads `concerns`
// back), and record the raw text under `operator_concern` — without 422
// fixup_not_applicable, which the deprecated positional path would raise.
func TestFixupStage_OperatorConcernOnly_ZeroConcernGate(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo) // NO seedConcernsReview — zero recorded concerns

	const text = "fix the CodeQL alert: sanitize the path before os.Open"
	w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: text, Reason: "required CI gate"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (operator_concern must work on a zero-concern gate):\n%s", w.Code, w.Body.String())
	}
	var body stageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.State != string(run.StageStatePending) {
		t.Errorf("body.State = %q, want pending", body.State)
	}

	payload := latestFixupTriggeredPayload(t, au)
	if payload["operator_concern"] != text {
		t.Errorf("payload.operator_concern = %v, want the verbatim text", payload["operator_concern"])
	}
	concerns, ok := payload["concerns"].([]any)
	if !ok || len(concerns) != 1 {
		t.Fatalf("payload.concerns = %v, want one synthetic concern", payload["concerns"])
	}
	c0 := concerns[0].(map[string]any)
	if c0["severity"] != string(operatorConcernSeverity) || c0["category"] != operatorConcernCategory || c0["note"] != text {
		t.Errorf("synthetic concern = %v, want {severity:%s category:%s note:%q}", c0, operatorConcernSeverity, operatorConcernCategory, text)
	}
}

// TestFixupStage_OperatorConcernOnly_NotFixupNotApplicable pins the negative:
// the operator-concern-only path must NOT 422 fixup_not_applicable on a
// zero-concern gate (the precondition only the deprecated positional path
// enforces). The positive re-open is covered above; this isolates the branch
// that the default switch arm skips resolveImplementConcerns.
func TestFixupStage_OperatorConcernOnly_NotFixupNotApplicable(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)

	w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: "address the missed edge case"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "fixup_not_applicable") {
		t.Errorf("operator-concern-only must NOT surface fixup_not_applicable: %s", w.Body.String())
	}
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
}

// TestFixupStage_OperatorConcernWithConcernIDs_FoldsBoth: an operator_concern
// supplied ALONGSIDE a valid concern_id routes BOTH — the resolved review
// concern and the synthetic [high/operator] concern — into `selected`.
func TestFixupStage_OperatorConcernWithConcernIDs_FoldsBoth(t *testing.T) {
	s, repo, au, cr := fixupServerWithConcerns(t)
	stage := seedImplementGateStage(repo)
	row := seedConcernRow(t, cr, stage.RunID, stage.ID, concern.StageKindImplement, 101, "reviewer's concern")

	const text = "also bump the timeout per the operator steer"
	w := postFixup(t, s, stage.ID, fixupRequest{
		ConcernIDs:      []string{row.ID.String()},
		OperatorConcern: text,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	payload := latestFixupTriggeredPayload(t, au)
	concerns, ok := payload["concerns"].([]any)
	if !ok || len(concerns) != 2 {
		t.Fatalf("payload.concerns = %v, want both the reviewer + synthetic concerns", payload["concerns"])
	}
	// The synthetic operator concern is appended last.
	last := concerns[1].(map[string]any)
	if last["category"] != operatorConcernCategory || last["note"] != text {
		t.Errorf("appended synthetic concern = %v, want {category:%s note:%q}", last, operatorConcernCategory, text)
	}
	if payload["operator_concern"] != text {
		t.Errorf("payload.operator_concern = %v, want the verbatim text", payload["operator_concern"])
	}
}

// TestFixupStage_OperatorConcernWhitespaceOnly_Rejected: a provided-but-
// whitespace-only operator_concern fails LOUD (400 field=operator_concern)
// rather than silently dropping a binding instruction. No state change, no
// audit entry.
func TestFixupStage_OperatorConcernWhitespaceOnly_Rejected(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)

	w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: "   \t\n  "})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "operator_concern") {
		t.Errorf("body should name operator_concern as the offending field: %s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit entries = %d, want 0 (rejected before any transition)", len(au.appended))
	}
}

// TestFixupStage_OperatorConcernOverLength_Rejected: an operator_concern
// exceeding maxOperatorConcernBytes fails LOUD (400 field=operator_concern)
// rather than being silently truncated by the renderer downstream.
func TestFixupStage_OperatorConcernOverLength_Rejected(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)

	oversized := strings.Repeat("a", maxOperatorConcernBytes+1)
	w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: oversized})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "operator_concern") {
		t.Errorf("body should name operator_concern as the offending field: %s", w.Body.String())
	}
	if len(au.appended) != 0 {
		t.Errorf("audit entries = %d, want 0 (rejected before any transition)", len(au.appended))
	}
}

// TestFixupStage_OperatorConcern_BudgetStillBounds: the synthetic operator
// concern rides the same `selected` slice AFTER the budget controls, so the
// bound is unaffected — a second operator-concern pass once the default
// budget is spent is refused with fixup_budget_exhausted.
func TestFixupStage_OperatorConcern_BudgetStillBounds(t *testing.T) {
	s, repo, au := fixupServer(t)
	stage := seedImplementGateStage(repo)

	if w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: "first operator steer"}); w.Code != http.StatusOK {
		t.Fatalf("first operator-concern pass status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	reparkFixupStage(repo, stage.ID)
	w := postFixup(t, s, stage.ID, fixupRequest{OperatorConcern: "second operator steer"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("second operator-concern pass status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fixup_budget_exhausted") {
		t.Errorf("body missing fixup_budget_exhausted code: %s", w.Body.String())
	}
	// Only the first pass wrote a trigger entry.
	n := 0
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			n++
		}
	}
	if n != 1 {
		t.Errorf("stage_fixup_triggered entries = %d, want 1", n)
	}
}

// --- Model validity gate (#1339) on the fix-up path --------------------------

// reject: an override model absent from a fresh+ok snapshot is refused 422
// model_invalid BEFORE the allow-list, with NO transition and NO audit entry.
func TestFixupStage_ModelValidity_RejectOnFreshAbsence(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ModelOracle = modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "claude-typo-9"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_invalid") {
		t.Errorf("body missing model_invalid code: %s", w.Body.String())
	}
	got, err := repo.GetStage(context.Background(), stage.ID)
	if err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if got.State != run.StageStateAwaitingApproval {
		t.Errorf("stage state = %q, want awaiting_approval (no transition on reject)", got.State)
	}
	for _, e := range au.appended {
		if e.Category == CategoryStageFixupTriggered {
			t.Errorf("stage_fixup_triggered appended despite the 422 reject")
		}
	}
}

// accept: an override present in a fresh+ok snapshot passes the validity layer
// (and the empty allow-list), triggering the fix-up 200.
func TestFixupStage_ModelValidity_AcceptOnFreshPresent(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ModelOracle = modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "claude-opus-4-8"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// fail-open-stale: a stale snapshot cannot reject — the fix-up proceeds 200.
func TestFixupStage_ModelValidity_FailOpenStale(t *testing.T) {
	s, repo, au := fixupServer(t)
	s.cfg.ModelOracle = modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  false,
	}
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "claude-typo-9"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (stale → fail open):\n%s", w.Code, w.Body.String())
	}
}

// fail-open-no-snapshot: with no oracle wired the validity layer is inert (the
// existing fix-up tests' posture) — an unknown override is not rejected by it.
func TestFixupStage_ModelValidity_FailOpenNoOracle(t *testing.T) {
	s, repo, au := fixupServer(t) // no ModelOracle wired
	stage := seedImplementGateStage(repo)
	seedConcernsReview(au, stage,
		planreview.Concern{Severity: planreview.SeverityLow, Category: "style", Note: "naming nit"},
	)

	w := postFixup(t, s, stage.ID, fixupRequest{Concerns: []int{0}, ImplementModel: "anything-goes"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil oracle → fail open):\n%s", w.Code, w.Body.String())
	}
}
