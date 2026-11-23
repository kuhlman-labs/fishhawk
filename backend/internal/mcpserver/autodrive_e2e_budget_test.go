package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// autodrive_e2e_budget_test.go is the cross-boundary leg of ADR-030
// Amendment A1 / #5473: the real server.New over pgtest Postgres with a
// githubclient.Client pointed at an in-file GitHub stub that serves the
// repository's LIVE default-branch spec, the real POST /v0/runs, a
// host-dispatcher token, the real tick loop and the real MCP client. It
// crosses the REST json tags, the MCP mirror, the dispatcher decision and
// status.json. Only the spawn seam is faked. Like every pgtest test these skip
// without Docker.

// adE2EBudgetGitHub is the in-file GitHub stub: the installation lookup and
// the contents read of .fishhawk/workflows.yaml. The live spec can be swapped
// between ticks; specStatus non-200 fails the contents read.
type adE2EBudgetGitHub struct {
	mu         sync.Mutex
	spec       string
	specStatus int
}

func (g *adE2EBudgetGitHub) set(spec string, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spec, g.specStatus = spec, status
}

func (g *adE2EBudgetGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":12345,"app_id":1}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		spec, status := g.spec, g.specStatus
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"stub failure"}`)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"path": ".fishhawk/workflows.yaml", "sha": "spec_sha", "type": "file",
			"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(spec)),
		})
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type adE2EBudgetTokens struct{}

func (adE2EBudgetTokens) Token(context.Context, int64) (string, error) { return "ghs_e2e", nil }

// adE2EBudgetSpec is a plan + implement workflow carrying one weekly budget.
func adE2EBudgetSpec(limit float64, enforcement string) string {
	return fmt.Sprintf(`version: "2"
workflows:
  feature_change:
    budgets:
      - period: weekly
        limit_usd: %g
        enforcement: %s
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
`, limit, enforcement)
}

// adE2EBudgetImplementOnlySpec is an implement-only workflow, so a created
// run's first dispatchable stage is the implement stage the verify-pass leg
// re-opens as a merge-candidate verify-only pass.
func adE2EBudgetImplementOnlySpec(limit float64) string {
	return fmt.Sprintf(`version: "2"
workflows:
  feature_change:
    budgets:
      - period: weekly
        limit_usd: %g
        enforcement: blocking
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`, limit)
}

// newAdE2EBudgetHarness matches newAdE2EHarness except that the server holds
// a GitHub client pointed at gh, and no campaign or driver is built: runs are
// created through the real POST /v0/runs. The spawner records spawns and
// emulates no end state.
func newAdE2EBudgetHarness(t *testing.T, gh *adE2EBudgetGitHub) *adE2EHarness {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	h := &adE2EHarness{
		t: t, ctx: ctx,
		runs:       runpkg.NewPostgresRepository(pool),
		audit:      audit.NewPostgresRepository(pool),
		artifacts:  artifact.NewPostgresRepository(pool),
		campaigns:  campaign.NewPostgresRepository(pool),
		approvals:  approval.NewPostgresRepository(pool),
		amendments: scopeamendment.NewPostgresRepository(pool),
		tokens:     apitoken.NewPostgresRepository(pool),
		concerns:   concern.NewPostgresRepository(pool),
		merger:     &adE2EMerger{},
	}
	h.root = t.TempDir()
	h.workDir = filepath.Join(h.root, "checkout")
	if err := os.MkdirAll(h.workDir, 0o750); err != nil {
		t.Fatalf("mkdir working dir: %v", err)
	}
	ghSrv := gh.server(t)
	h.orch = &orchestrator.Orchestrator{Runs: h.runs, Audit: h.audit, Artifacts: h.artifacts}
	h.srv = server.New(server.Config{
		RunRepo:             h.runs,
		AuditRepo:           h.audit,
		ArtifactRepo:        h.artifacts,
		CampaignRepo:        h.campaigns,
		ApprovalRepo:        h.approvals,
		ConcernRepo:         h.concerns,
		ScopeAmendmentRepo:  h.amendments,
		APITokenRepo:        h.tokens,
		Orchestrator:        h.orch,
		DefaultBranch:       defaultbranch.Static("main"),
		GitHubWebhookSecret: []byte(adE2EWebhookSecret),
		GitHub: &githubclient.Client{
			BaseURL: ghSrv.URL,
			Tokens:  adE2EBudgetTokens{},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
			AppJWT:  func() (string, error) { return "gha_app_jwt_e2e", nil },
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.ts.Close)

	operatorScopes := append([]string{"read:campaigns"}, oauthas.SupportedScopes...)
	h.dispatcherBearer = h.mint(adE2EDispatcherSub, hostdispatcher.Scopes())
	h.operatorBearer = h.mint(adE2EHumanSubject, operatorScopes)
	h.spawner = &adE2ESpawner{h: h, emulate: map[string]func(runID, stageID uuid.UUID){}}
	h.loop = h.newDispatchLoop()
	return h
}

// createBudgetRun creates a run through the real POST /v0/runs with the
// inline admission spec, marked for the host dispatcher and bound to the
// local checkout. It returns the 201 body.
func (h *adE2EHarness) createBudgetRun(admissionSpec string, autoDispatch, override bool) Run {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"repo": adE2ERepo, "workflow_id": "feature_change", "workflow_sha": "e2e",
		"trigger_source": "cli", "runner_kind": "local", "working_dir": h.workDir,
		"workflow_spec": admissionSpec, "auto_dispatch": autoDispatch, "budget_override": override,
	})
	req, _ := http.NewRequestWithContext(h.ctx, http.MethodPost, h.ts.URL+"/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.operatorBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /v0/runs: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("POST /v0/runs = %d; want 201:\n%s", resp.StatusCode, raw)
	}
	var out Run
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode 201: %v", err)
	}
	return out
}

// seedSpend records usd of spend this period on a separate, unmarked run of
// the same repo and workflow.
func (h *adE2EHarness) seedSpend(admissionSpec string, usd float64) {
	h.t.Helper()
	other := h.createBudgetRun(admissionSpec, false, false)
	coster, ok := h.runs.(interface {
		AddRunCost(ctx context.Context, id uuid.UUID, deltaUSD float64, resolvedModel string) (*runpkg.Run, error)
	})
	if !ok {
		h.t.Fatal("run repository cannot record run cost")
	}
	if _, err := coster.AddRunCost(h.ctx, uuid.MustParse(other.ID), usd, "claude-e2e"); err != nil {
		h.t.Fatalf("add run cost: %v", err)
	}
}

// budgetRefusal reads the dispatcher's status.json run_refusals entry.
func (h *adE2EHarness) budgetRefusal(runID string) (hostDispatchRunRefusal, bool) {
	h.t.Helper()
	st, err := ReadHostDispatcherStatus(h.loop.cfg.StateDir)
	if err != nil {
		h.t.Fatalf("read status.json: %v", err)
	}
	ref, ok := st.RunRefusals[runID]
	return ref, ok
}

// spawnsOf is the recorded spawns of runID.
func (h *adE2EHarness) spawnsOf(runID string) int {
	n := 0
	for _, s := range h.spawner.recorded() {
		if s.RunID == runID {
			n++
		}
	}
	return n
}

// G1 + G3: a run admitted under the limit is refused budget_blocked once the
// period's spend is over a LIVE blocking budget, with no spawn; raising
// limit_usd in the live spec (the run's admission spec is unchanged) spawns
// it on the next tick with no other change.
func TestAutoDriveE2E_BlockingBudgetHoldsSpawnUntilLiveLimitRaise(t *testing.T) {
	gh := &adE2EBudgetGitHub{}
	gh.set(adE2EBudgetSpec(10, "blocking"), http.StatusOK)
	h := newAdE2EBudgetHarness(t, gh)
	admission := adE2EBudgetSpec(10, "blocking")
	r := h.createBudgetRun(admission, true, false)
	h.seedSpend(admission, 15)

	h.dispatcherTick()
	if n := h.spawnsOf(r.ID); n != 0 {
		t.Fatalf("spawns under an exhausted blocking budget = %d; want 0", n)
	}
	ref, ok := h.budgetRefusal(r.ID)
	if !ok || ref.Code != hdRefuseBudgetBlocked {
		t.Fatalf("run_refusals[%s] = %+v (present %v); want budget_blocked", r.ID, ref, ok)
	}
	for _, want := range []string{"spent $15.00 of $10.00", "budget_source live_spec", "weekly"} {
		if !bytes.Contains([]byte(ref.Detail), []byte(want)) {
			t.Errorf("detail %q does not name %q", ref.Detail, want)
		}
	}
	verdict, err := h.resolver(h.dispatcherBearer).api.GetRunDispatchBudget(h.ctx, uuid.MustParse(r.ID))
	if err != nil || verdict.Status != dispatchBudgetStatusBlocked || verdict.Budget == nil || verdict.Budget.LimitUSD != 10 || verdict.Budget.SpentUSD != 15 {
		t.Fatalf("real client verdict = %+v, %v; want blocked at $15 of $10", verdict, err)
	}

	gh.set(adE2EBudgetSpec(20, "blocking"), http.StatusOK)
	h.dispatcherTick()
	if n := h.spawnsOf(r.ID); n != 1 {
		t.Fatalf("spawns after the live limit raise = %d; want 1", n)
	}
	if ref, ok := h.budgetRefusal(r.ID); ok {
		t.Fatalf("run_refusals still carries %+v after the spawn", ref)
	}
}

// G2: an advisory budget over its limit never holds a spawn.
func TestAutoDriveE2E_AdvisoryBudgetStillSpawns(t *testing.T) {
	gh := &adE2EBudgetGitHub{}
	gh.set(adE2EBudgetSpec(10, "advisory"), http.StatusOK)
	h := newAdE2EBudgetHarness(t, gh)
	admission := adE2EBudgetSpec(10, "advisory")
	r := h.createBudgetRun(admission, true, false)
	h.seedSpend(admission, 15)
	h.dispatcherTick()
	if n := h.spawnsOf(r.ID); n != 1 {
		ref, _ := h.budgetRefusal(r.ID)
		t.Fatalf("advisory-over-limit spawns = %d (refusal %+v); want 1", n, ref)
	}
}

// G4 (C3) + approval condition D1: a run created through the real POST
// /v0/runs with budget_override AFTER exhaustion carries the admission-written
// run_budget_override_bound row (read back from the real audit repository)
// and spawns under the exhausted budget, while a same-period run admitted
// earlier without an override is refused budget_blocked. With the live read
// then failing (the admission spec still declares a blocking budget), the
// bound run is still admitted, because the exemption is decided before the
// budget read; the unbound run is refused budget_check_failed.
func TestAutoDriveE2E_OverrideBoundRunSpawnsUnderExhaustedBudget(t *testing.T) {
	gh := &adE2EBudgetGitHub{}
	gh.set(adE2EBudgetSpec(10, "blocking"), http.StatusOK)
	h := newAdE2EBudgetHarness(t, gh)
	admission := adE2EBudgetSpec(10, "blocking")
	plain := h.createBudgetRun(admission, true, false)
	h.seedSpend(admission, 15)
	bound := h.createBudgetRun(admission, true, true)
	if bound.BudgetOverrideBinding == nil || !bound.BudgetOverrideBinding.Recorded {
		t.Fatalf("override create 201 budget_override_binding = %+v; want recorded true", bound.BudgetOverrideBinding)
	}
	if rows := h.auditRows(uuid.MustParse(bound.ID), "run_budget_override_bound"); len(rows) != 1 {
		t.Fatalf("bound run chain carries %d run_budget_override_bound row(s); want 1", len(rows))
	}

	h.dispatcherTick()
	if n := h.spawnsOf(bound.ID); n != 1 {
		ref, _ := h.budgetRefusal(bound.ID)
		t.Fatalf("override-bound spawns = %d (refusal %+v); want 1", n, ref)
	}
	if n := h.spawnsOf(plain.ID); n != 0 {
		t.Fatalf("plain run spawns = %d; want 0", n)
	}
	if ref, _ := h.budgetRefusal(plain.ID); ref.Code != hdRefuseBudgetBlocked {
		t.Fatalf("plain run refusal = %+v; want budget_blocked", ref)
	}

	// D1: the live read fails. A second override-bound run is admitted (its
	// first stage spawns); the plain run fails closed.
	gh.set("", http.StatusInternalServerError)
	bound2 := h.createBudgetRun(admission, true, true)
	h.dispatcherTick()
	if n := h.spawnsOf(bound2.ID); n != 1 {
		ref, _ := h.budgetRefusal(bound2.ID)
		t.Fatalf("override-bound spawns under a failed live read = %d (refusal %+v); want 1", n, ref)
	}
	if ref, _ := h.budgetRefusal(plain.ID); ref.Code != hdRefuseBudgetCheckFailed || h.spawnsOf(plain.ID) != 0 {
		t.Fatalf("plain run under a failed live read = %+v (spawns %d); want budget_check_failed, no spawn", ref, h.spawnsOf(plain.ID))
	}
}

// G5 (C3): an admitted run whose implement stage is pending with a LIVE
// stage_merge_candidate_verify_triggered row is served with that stage in
// verify_pass_stage_ids through the real client, and the real tick spawns it
// under the exhausted budget.
func TestAutoDriveE2E_VerifyPassSpawnsUnderExhaustedBudget(t *testing.T) {
	gh := &adE2EBudgetGitHub{}
	admission := adE2EBudgetImplementOnlySpec(10)
	gh.set(admission, http.StatusOK)
	h := newAdE2EBudgetHarness(t, gh)
	r := h.createBudgetRun(admission, true, false)
	h.seedSpend(admission, 15)
	runID := uuid.MustParse(r.ID)
	st := h.stageOf(runID, runpkg.StageTypeImplement)
	if st == nil {
		t.Fatal("run has no implement stage")
	}
	h.appendRunAudit(runID, &st.ID, "stage_merge_candidate_verify_triggered", map[string]any{
		"branch": "fishhawk/run-e2e", "base_ref": "main", "expected_head_sha": "abc123",
		"cause": "base_advance", "verify_command": "scripts/test verify",
	})

	verdict, err := h.resolver(h.dispatcherBearer).api.GetRunDispatchBudget(h.ctx, runID)
	if err != nil || verdict.Status != dispatchBudgetStatusBlocked || len(verdict.VerifyPassStageIDs) != 1 || verdict.VerifyPassStageIDs[0] != st.ID.String() {
		t.Fatalf("real client verdict = %+v, %v; want blocked naming verify pass %s", verdict, err, st.ID)
	}
	h.dispatcherTick()
	if n := h.spawnsOf(r.ID); n != 1 {
		ref, _ := h.budgetRefusal(r.ID)
		t.Fatalf("verify-pass spawns under an exhausted budget = %d (refusal %+v); want 1", n, ref)
	}
}

// Approval condition D1 (amendments 0f18ebd4 / a09939e2): with the live read
// FAILING and a blocking admission spec, the verdict is undecided (503
// budget_check_failed), yet the merge-candidate verify-only pass the server
// decided BEFORE the read rides the 503's details.verify_pass_stage_ids
// through the real server's 5xx redactor and the real client, and the real
// tick spawns it. A same-period run with no verify pass is refused
// budget_check_failed with no spawn.
func TestAutoDriveE2E_VerifyPassSpawnsUnderFailedLiveRead(t *testing.T) {
	gh := &adE2EBudgetGitHub{}
	admission := adE2EBudgetImplementOnlySpec(10)
	gh.set(admission, http.StatusOK)
	h := newAdE2EBudgetHarness(t, gh)
	r := h.createBudgetRun(admission, true, false)
	plain := h.createBudgetRun(admission, true, false)
	h.seedSpend(admission, 15)
	runID := uuid.MustParse(r.ID)
	st := h.stageOf(runID, runpkg.StageTypeImplement)
	if st == nil {
		t.Fatal("run has no implement stage")
	}
	h.appendRunAudit(runID, &st.ID, "stage_merge_candidate_verify_triggered", map[string]any{
		"branch": "fishhawk/run-e2e", "base_ref": "main", "expected_head_sha": "abc123",
		"cause": "base_advance", "verify_command": "scripts/test verify",
	})
	gh.set("", http.StatusInternalServerError)

	_, err := h.resolver(h.dispatcherBearer).api.GetRunDispatchBudget(h.ctx, runID)
	var ue *dispatchBudgetUndecidedError
	if !errors.As(err, &ue) || ue.Reason != "live_spec_unavailable" || len(ue.VerifyPassStageIDs) != 1 || ue.VerifyPassStageIDs[0] != st.ID.String() {
		t.Fatalf("real client verdict err = %v (%+v); want 503 budget_check_failed live_spec_unavailable naming verify pass %s", err, ue, st.ID)
	}
	h.dispatcherTick()
	if n := h.spawnsOf(r.ID); n != 1 {
		ref, _ := h.budgetRefusal(r.ID)
		t.Fatalf("verify-pass spawns under a failed live read = %d (refusal %+v); want 1", n, ref)
	}
	if ref, _ := h.budgetRefusal(plain.ID); ref.Code != hdRefuseBudgetCheckFailed || h.spawnsOf(plain.ID) != 0 {
		t.Fatalf("plain run under a failed live read = %+v (spawns %d); want budget_check_failed, no spawn", ref, h.spawnsOf(plain.ID))
	}
}
