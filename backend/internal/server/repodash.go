package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/latency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodash"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Repo dashboard rollups (E40.3 / #1714): GET
// /v0/repos/{owner}/{name}/{throughput,health,economics,posture}. Read-only,
// additive, repo-visibility gated through requestRepoFilter +
// repoVisibleOr403 (the point-read DENY convention). The folds live in the
// pure backend/internal/repodash package; this file only gathers and maps.
// Long-form contract: backend/internal/server/README.md § "Repo dashboard".

// repoDashPageSize is the ListRuns page size the window scan pages with.
const repoDashPageSize = 200

// repoDashWindowFields is the window envelope every rollup carries.
type repoDashWindowFields struct {
	Repo        string    `json:"repo"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	WindowWeeks int       `json:"window_weeks"`
	RunsScanned int       `json:"runs_scanned"`
	// Truncated is true when repodash.MaxRunsScanned stopped the scan before
	// it reached the look-back boundary (approval condition 1).
	Truncated bool `json:"truncated"`
}

type repoThroughputResponse struct {
	repoDashWindowFields
	Weeks                  []repodash.WeekCount  `json:"weeks"`
	MergedChanges          int                   `json:"merged_changes"`
	MedianCycleTimeSeconds float64               `json:"median_cycle_time_seconds"`
	CycleTimeSamples       int                   `json:"cycle_time_samples"`
	CycleTimeExcluded      int                   `json:"cycle_time_excluded"`
	WaitOnHuman            *repodash.WaitOnHuman `json:"wait_on_human,omitempty"`
}

type repoHealthResponse struct {
	repoDashWindowFields
	RunsConsidered            int                 `json:"runs_considered"`
	PlanApprovalSamples       int                 `json:"plan_approval_samples"`
	PlanFirstShotApprovals    int                 `json:"plan_first_shot_approvals"`
	PlanFirstShotApprovalRate float64             `json:"plan_first_shot_approval_rate"`
	FixupRuns                 int                 `json:"fixup_runs"`
	FixupRate                 float64             `json:"fixup_rate"`
	AcceptanceSamples         int                 `json:"acceptance_samples"`
	AcceptancePassed          int                 `json:"acceptance_passed"`
	AcceptanceNotValidated    int                 `json:"acceptance_not_validated"`
	AcceptanceFailed          int                 `json:"acceptance_failed"`
	AcceptanceUndecidable     int                 `json:"acceptance_undecidable"`
	AcceptancePassRate        float64             `json:"acceptance_pass_rate"`
	FailureCategories         repodash.FailureMix `json:"failure_categories"`
}

type repoEconomicsResponse struct {
	repoDashWindowFields
	CostEntries            int                      `json:"cost_entries"`
	TotalCostUSD           float64                  `json:"total_cost_usd"`
	MergedChanges          int                      `json:"merged_changes"`
	CostPerMergedChangeUSD float64                  `json:"cost_per_merged_change_usd"`
	Weeks                  []repodash.WeekEconomics `json:"weeks"`
	Budgets                []repoBudgetBurn         `json:"budgets,omitempty"`
}

// repoEconomicsEmpty is the no-cost_recorded body: `{}` (or
// `{"truncated":true}` when the scan was cut short), matching the /cost and
// /cache-efficiency presence-not-status-code precedent.
type repoEconomicsEmpty struct {
	Truncated bool `json:"truncated,omitempty"`
}

// repoBudgetBurn is one workflow's ADR-030 periodic-budget burn, evaluated
// through the same evaluateWorkflowBudget + effectiveBudgetLimit chain
// GET /v0/runs/{id}/budget uses.
type repoBudgetBurn struct {
	WorkflowID  string  `json:"workflow_id"`
	Period      string  `json:"period"`
	PeriodStart string  `json:"period_start"`
	LimitUSD    float64 `json:"limit_usd"`
	SpentUSD    float64 `json:"spent_usd"`
	Fraction    float64 `json:"fraction"`
	Tier        string  `json:"tier"`
	Enforcement string  `json:"enforcement"`
}

type repoPostureResponse struct {
	Repo            string            `json:"repo"`
	RunID           string            `json:"run_id"`
	WorkflowID      string            `json:"workflow_id"`
	WorkflowSHA     string            `json:"workflow_sha"`
	Version         string            `json:"version"`
	SchemaMajor     int               `json:"schema_major"`
	SchemaHash      string            `json:"schema_hash,omitempty"`
	SchemaSupported bool              `json:"schema_supported"`
	SpecValid       bool              `json:"spec_valid"`
	SpecError       string            `json:"spec_error,omitempty"`
	Workflows       []postureWorkflow `json:"workflows"`
}

type postureWorkflow struct {
	ID       string                  `json:"id"`
	Autonomy string                  `json:"autonomy,omitempty"`
	Stages   []postureStage          `json:"stages"`
	Budgets  []posturePeriodicBudget `json:"budgets,omitempty"`
}

type postureStage struct {
	ID        string              `json:"id"`
	Type      string              `json:"type"`
	Executor  string              `json:"executor"`
	Model     string              `json:"model,omitempty"`
	Gates     []postureGate       `json:"gates,omitempty"`
	Reviewers *postureReviewers   `json:"reviewers,omitempty"`
	Budget    *postureStageBudget `json:"budget,omitempty"`
}

type postureGate struct {
	Type           string   `json:"type"`
	Autonomy       string   `json:"autonomy,omitempty"`
	ApproversAnyOf []string `json:"approvers_any_of,omitempty"`
	ApproversAllOf []string `json:"approvers_all_of,omitempty"`
}

type postureReviewers struct {
	Agents []postureAgentReviewer `json:"agents,omitempty"`
	Human  int                    `json:"human,omitempty"`
}

type postureAgentReviewer struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
}

type postureStageBudget struct {
	MaxTokens         int     `json:"max_tokens,omitempty"`
	MaxRuntimeSeconds float64 `json:"max_runtime_seconds,omitempty"`
	LimitUSD          float64 `json:"limit_usd,omitempty"`
}

type posturePeriodicBudget struct {
	Period      string   `json:"period"`
	LimitUSD    float64  `json:"limit_usd"`
	Enforcement string   `json:"enforcement,omitempty"`
	WarnAt      *float64 `json:"warn_at,omitempty"`
}

// embeddedWorkflowSchemaHashes maps a workflow major to the embedded schema
// hash — the same three accessors /healthz advertises under `schemas`. A
// major absent here is one this binary does not embed.
var embeddedWorkflowSchemaHashes = map[int]func() string{
	0: spec.EmbeddedSchemaHash,
	1: spec.EmbeddedSchemaHashV1,
	2: spec.EmbeddedSchemaHashV2,
}

// repoDashScan is one gathered window.
type repoDashScan struct {
	runs      []*run.Run
	newest    *run.Run
	scanned   int
	truncated bool
}

// repoDashPrelude runs the shared gate for a rollup: 503 on an unconfigured
// run/audit repository, repo visibility (403/503), then the bounded `weeks`
// parameter (400). Returns ok=false once it has written a response.
func (s *Server) repoDashPrelude(w http.ResponseWriter, r *http.Request, needAudit, needWeeks bool) (string, repodash.Window, bool) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"repo dashboard requires a configured run repository", nil)
		return "", repodash.Window{}, false
	}
	if needAudit && s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"repo dashboard requires a configured audit repository", nil)
		return "", repodash.Window{}, false
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	filter, ok := s.requestRepoFilter(w, r)
	if !ok {
		return "", repodash.Window{}, false
	}
	if !s.repoVisibleOr403(w, r, filter, repo) {
		return "", repodash.Window{}, false
	}
	if !needWeeks {
		return repo, repodash.Window{}, true
	}
	weeks := repodash.DefaultWeeks
	if raw := r.URL.Query().Get("weeks"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < repodash.MinWeeks || n > repodash.MaxWeeks {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				fmt.Sprintf("weeks must be an integer between %d and %d", repodash.MinWeeks, repodash.MaxWeeks),
				map[string]any{"field": "weeks", "got": raw})
			return "", repodash.Window{}, false
		}
		weeks = n
	}
	return repo, repodash.NewWindow(s.nowFunc(), weeks), true
}

// scanRepoRuns pages ListRuns newest-first until a page's oldest created_at
// is older than the window's scan boundary (windowStart - LookBack) or the
// listing is exhausted, under the repodash.MaxRunsScanned ceiling. When the
// ceiling stops the scan first, truncated is set — unless a one-row probe
// proves the listing ended exactly at the ceiling.
func (s *Server) scanRepoRuns(ctx context.Context, repo string, boundary time.Time) (repoDashScan, error) {
	var out repoDashScan
	filter := run.ListRunsFilter{Repo: repo, AccountID: IdentityFrom(ctx).AccountID}
	complete := false
	for out.scanned < repodash.MaxRunsScanned {
		filter.Limit = min(repoDashPageSize, repodash.MaxRunsScanned-out.scanned)
		filter.Offset = out.scanned
		page, err := s.cfg.RunRepo.ListRuns(ctx, filter)
		if err != nil {
			return out, err
		}
		out.scanned += len(page)
		for _, rn := range page {
			if out.newest == nil {
				out.newest = rn
			}
			if rn.CreatedAt.Before(boundary) {
				complete = true
				continue
			}
			out.runs = append(out.runs, rn)
		}
		if complete || len(page) < filter.Limit {
			complete = true
			break
		}
	}
	if !complete {
		filter.Limit, filter.Offset = 1, out.scanned
		more, err := s.cfg.RunRepo.ListRuns(ctx, filter)
		if err != nil {
			return out, err
		}
		out.truncated = len(more) > 0
	}
	return out, nil
}

// dashRun decodes one run's audit chain (and, for health, its stages) into
// the pure fold input.
func (s *Server) dashRun(ctx context.Context, rn *run.Run, withStages, withWait bool) (repodash.Run, error) {
	out := repodash.Run{ID: rn.ID.String(), CreatedAt: rn.CreatedAt}
	if rn.PullRequestURL != nil {
		out.PullRequestURL = *rn.PullRequestURL
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, rn.ID)
	if err != nil {
		return out, err
	}
	stageTypes := map[string]string{}
	if withStages {
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, rn.ID)
		if err != nil {
			return out, err
		}
		for _, st := range stages {
			stageTypes[st.ID.String()] = string(st.Type)
			if st.FailureCategory != nil {
				out.FailureCategories = append(out.FailureCategories, string(*st.FailureCategory))
			}
		}
	}
	for _, e := range entries {
		out.Events = append(out.Events, dashEvent(e, stageTypes))
		if e.Category == "cost_recorded" {
			if c, ok := dashCost(e); ok {
				out.Cost = append(out.Cost, c)
			}
		}
	}
	if withWait {
		out.Wait = dashWait(entries, rn.CreatedAt)
	}
	return out, nil
}

func dashEvent(e *audit.Entry, stageTypes map[string]string) repodash.Event {
	ev := repodash.Event{Category: e.Category, Timestamp: e.Timestamp}
	switch e.Category {
	case repodash.CategoryApprovalSubmitted:
		var p struct {
			StageID  string `json:"stage_id"`
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		ev.Decision = p.Decision
		stageID := p.StageID
		if e.StageID != nil {
			stageID = e.StageID.String()
		}
		ev.StageType = stageTypes[stageID]
	case repodash.CategoryAcceptanceOutcome:
		var p struct {
			Verdict string `json:"verdict"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		ev.Verdict = p.Verdict
	}
	return ev
}

func dashCost(e *audit.Entry) (repodash.CostEntry, bool) {
	var p struct {
		USD              float64 `json:"usd"`
		Model            string  `json:"model"`
		Source           string  `json:"source"`
		InputTokens      int     `json:"input_tokens"`
		OutputTokens     int     `json:"output_tokens"`
		CacheReadTokens  int     `json:"cache_read_input_tokens"`
		CacheWriteTokens int     `json:"cache_write_input_tokens"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return repodash.CostEntry{}, false
	}
	return repodash.CostEntry{
		Timestamp: e.Timestamp, Model: p.Model, Source: p.Source, USD: p.USD,
		FreshInput: p.InputTokens, CacheRead: p.CacheReadTokens,
		CacheWrite: p.CacheWriteTokens, Output: p.OutputTokens,
	}, true
}

// dashWait folds already-loaded entries through the SAME gate-event mapping
// and aggregator runLatencySummary uses (#1702), without re-reading the chain.
// Returns nil when no gate interval resolves.
func dashWait(entries []*audit.Entry, createdAt time.Time) *repodash.RunWait {
	events := make([]latency.GateEvent, 0, len(entries))
	runEnd := createdAt
	for _, e := range entries {
		if e.Timestamp.After(runEnd) {
			runEnd = e.Timestamp
		}
		if cat, ok := gateEventCategory(e.Category, e.Payload); ok {
			events = append(events, latency.GateEvent{Category: cat, Timestamp: e.Timestamp})
		}
	}
	if term, ok := latestTerminalTimestamp(entries); ok {
		runEnd = term
	}
	roll := latency.AggregateGateLatency(events, createdAt, runEnd)
	if len(roll.Gates) == 0 {
		return nil
	}
	out := &repodash.RunWait{TotalSeconds: roll.TotalWaitOnHumanSeconds}
	for _, g := range roll.Gates {
		out.Gates = append(out.Gates, repodash.GateWait{Gate: g.Gate, WaitSeconds: g.WaitSeconds})
	}
	return out
}

// gatherRepoDash is the shared prelude + scan + decode for the three rollups.
func (s *Server) gatherRepoDash(w http.ResponseWriter, r *http.Request, withStages, withWait bool) (repodash.Window, repoDashWindowFields, repoDashScan, []repodash.Run, bool) {
	repo, win, ok := s.repoDashPrelude(w, r, true, true)
	if !ok {
		return win, repoDashWindowFields{}, repoDashScan{}, nil, false
	}
	scan, err := s.scanRepoRuns(r.Context(), repo, win.ScanBoundary())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return win, repoDashWindowFields{}, scan, nil, false
	}
	runs := make([]repodash.Run, 0, len(scan.runs))
	for _, rn := range scan.runs {
		dr, err := s.dashRun(r.Context(), rn, withStages, withWait)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read run audit/stages failed", map[string]any{"error": err.Error(), "run_id": rn.ID.String()})
			return win, repoDashWindowFields{}, scan, nil, false
		}
		runs = append(runs, dr)
	}
	env := repoDashWindowFields{
		Repo: repo, WindowStart: win.Start, WindowEnd: win.End, WindowWeeks: win.Weeks,
		RunsScanned: scan.scanned, Truncated: scan.truncated,
	}
	return win, env, scan, runs, true
}

// handleGetRepoThroughput implements GET /v0/repos/{owner}/{name}/throughput.
func (s *Server) handleGetRepoThroughput(w http.ResponseWriter, r *http.Request) {
	win, env, _, runs, ok := s.gatherRepoDash(w, r, false, true)
	if !ok {
		return
	}
	th := repodash.FoldThroughput(win, runs)
	s.writeJSON(w, r, http.StatusOK, repoThroughputResponse{
		repoDashWindowFields:   env,
		Weeks:                  th.Weeks,
		MergedChanges:          th.MergedChanges,
		MedianCycleTimeSeconds: th.MedianCycleTimeSeconds,
		CycleTimeSamples:       th.CycleTimeSamples,
		CycleTimeExcluded:      th.CycleTimeExcluded,
		WaitOnHuman:            th.WaitOnHuman,
	})
}

// handleGetRepoHealth implements GET /v0/repos/{owner}/{name}/health.
func (s *Server) handleGetRepoHealth(w http.ResponseWriter, r *http.Request) {
	win, env, _, runs, ok := s.gatherRepoDash(w, r, true, false)
	if !ok {
		return
	}
	h := repodash.FoldHealth(win, runs)
	s.writeJSON(w, r, http.StatusOK, repoHealthResponse{
		repoDashWindowFields:      env,
		RunsConsidered:            h.RunsConsidered,
		PlanApprovalSamples:       h.PlanApprovalSamples,
		PlanFirstShotApprovals:    h.PlanFirstShotApprovals,
		PlanFirstShotApprovalRate: h.PlanFirstShotApprovalRate,
		FixupRuns:                 h.FixupRuns,
		FixupRate:                 h.FixupRate,
		AcceptanceSamples:         h.AcceptanceSamples,
		AcceptancePassed:          h.AcceptancePassed,
		AcceptanceNotValidated:    h.AcceptanceNotValidated,
		AcceptanceFailed:          h.AcceptanceFailed,
		AcceptanceUndecidable:     h.AcceptanceUndecidable,
		AcceptancePassRate:        h.AcceptancePassRate,
		FailureCategories:         h.FailureCategories,
	})
}

// handleGetRepoEconomics implements GET /v0/repos/{owner}/{name}/economics.
func (s *Server) handleGetRepoEconomics(w http.ResponseWriter, r *http.Request) {
	win, env, scan, runs, ok := s.gatherRepoDash(w, r, false, false)
	if !ok {
		return
	}
	e := repodash.FoldEconomics(win, runs)
	if e.CostEntries == 0 {
		s.writeJSON(w, r, http.StatusOK, repoEconomicsEmpty{Truncated: env.Truncated})
		return
	}
	budgets, err := s.repoBudgetBurn(r.Context(), env.Repo, scan.newest)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"evaluate workflow budget failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, repoEconomicsResponse{
		repoDashWindowFields:   env,
		CostEntries:            e.CostEntries,
		TotalCostUSD:           e.TotalCostUSD,
		MergedChanges:          e.MergedChanges,
		CostPerMergedChangeUSD: e.CostPerMergedChangeUSD,
		Weeks:                  e.Weeks,
		Budgets:                budgets,
	})
}

// repoBudgetBurn evaluates the FIRST periodic budget of every workflow in the
// newest run's cached spec (the /budget display convention), sorted by
// workflow id. Nil when the run repo cannot sum cost, there is no cached or
// parseable spec, or no workflow declares a budget.
func (s *Server) repoBudgetBurn(ctx context.Context, repo string, newest *run.Run) ([]repoBudgetBurn, error) {
	summer, ok := s.cfg.RunRepo.(runCostSummer)
	if !ok || newest == nil || len(newest.WorkflowSpec) == 0 {
		return nil, nil
	}
	parsed, err := spec.ParseBytes(newest.WorkflowSpec)
	if err != nil {
		return nil, nil
	}
	ids := make([]string, 0, len(parsed.Workflows))
	for id := range parsed.Workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	var out []repoBudgetBurn
	for _, id := range ids {
		wf := parsed.Workflows[id]
		if len(wf.Budgets) == 0 {
			continue
		}
		b := wf.Budgets[0]
		b.LimitUSD = s.effectiveBudgetLimit(b)
		d, ok, err := evaluateWorkflowBudget(ctx, summer, repo, id, b, s.nowFunc(), loc)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		enforcement := string(b.Enforcement)
		if enforcement == "" {
			enforcement = string(spec.EnforcementAdvisory)
		}
		out = append(out, repoBudgetBurn{
			WorkflowID:  id,
			Period:      b.Period,
			PeriodStart: d.PeriodStart.Format(time.RFC3339),
			LimitUSD:    b.LimitUSD,
			SpentUSD:    d.Spent,
			Fraction:    d.Fraction,
			Tier:        budget.Tier(d, s.cfg.BudgetAckMultiple, s.cfg.BudgetPageMultiple),
			Enforcement: enforcement,
		})
	}
	return out, nil
}

// handleGetRepoPosture implements GET /v0/repos/{owner}/{name}/posture: a
// projection of the workflow spec cached on the repo's NEWEST run, plus the
// declared version, its major, whether this binary embeds that major, and
// whether the cached bytes validate through spec.ParseBytes (the same
// parse/validate path the backend runs). 200 `{}` when there is no run or no
// cached spec.
func (s *Server) handleGetRepoPosture(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.repoDashPrelude(w, r, false, false)
	if !ok {
		return
	}
	rows, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		Repo: repo, AccountID: IdentityFrom(r.Context()).AccountID, Limit: 1,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return
	}
	if len(rows) == 0 || len(bytes.TrimSpace(rows[0].WorkflowSpec)) == 0 {
		s.writeJSON(w, r, http.StatusOK, struct{}{})
		return
	}
	s.writeJSON(w, r, http.StatusOK, projectPosture(repo, rows[0]))
}

// projectPosture builds the posture body. It never panics on a malformed or
// unsupported spec: the version is read from the raw YAML independently of
// validation, and the workflow projection is emitted only for a valid spec.
func projectPosture(repo string, rn *run.Run) repoPostureResponse {
	out := repoPostureResponse{
		Repo: repo, RunID: rn.ID.String(), WorkflowID: rn.WorkflowID,
		WorkflowSHA: rn.WorkflowSHA, Workflows: []postureWorkflow{},
	}
	var raw map[string]any
	if yaml.Unmarshal(rn.WorkflowSpec, &raw) == nil {
		if v, ok := raw["version"]; ok && v != nil {
			out.Version = fmt.Sprint(v)
		}
	}
	out.SchemaMajor = spec.VersionMajor(out.Version)
	if h, ok := embeddedWorkflowSchemaHashes[out.SchemaMajor]; ok {
		out.SchemaHash = h()
		out.SchemaSupported = true
	}
	parsed, err := spec.ParseBytes(rn.WorkflowSpec)
	if err != nil {
		out.SpecError = err.Error()
		return out
	}
	out.SpecValid = true
	ids := make([]string, 0, len(parsed.Workflows))
	for id := range parsed.Workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out.Workflows = append(out.Workflows, projectWorkflow(id, parsed.Workflows[id]))
	}
	return out
}

func projectWorkflow(id string, wf spec.Workflow) postureWorkflow {
	pw := postureWorkflow{ID: id, Autonomy: string(wf.Autonomy), Stages: []postureStage{}}
	for _, b := range wf.Budgets {
		pw.Budgets = append(pw.Budgets, posturePeriodicBudget{
			Period: b.Period, LimitUSD: b.LimitUSD, Enforcement: string(b.Enforcement), WarnAt: b.WarnAt,
		})
	}
	for _, st := range wf.Stages {
		ps := postureStage{ID: st.ID, Type: string(st.Type), Executor: st.Executor.Agent, Model: st.Executor.Model}
		if st.Executor.Human {
			ps.Executor = "human"
		}
		for _, g := range st.Gates {
			pg := postureGate{Type: string(g.Type), Autonomy: string(g.Autonomy)}
			if g.Approvers != nil {
				pg.ApproversAnyOf = g.Approvers.AnyOf
				pg.ApproversAllOf = g.Approvers.AllOf
			}
			ps.Gates = append(ps.Gates, pg)
		}
		if rv := st.Reviewers; rv != nil {
			pr := &postureReviewers{Human: rv.Human}
			for _, a := range rv.Agents {
				pr.Agents = append(pr.Agents, postureAgentReviewer{Provider: a.Provider, Model: a.Model})
			}
			ps.Reviewers = pr
		}
		if b := st.Budget; b != nil {
			ps.Budget = &postureStageBudget{MaxTokens: b.MaxTokens, MaxRuntimeSeconds: b.Runtime().Seconds(), LimitUSD: b.LimitUSD}
		}
		pw.Stages = append(pw.Stages, ps)
	}
	return pw
}
