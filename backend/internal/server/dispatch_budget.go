package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/budget"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Blocking budget at host dispatch (ADR-030 Amendment A1 / #5473). The host
// dispatcher (ADR-104) reads GET /v0/runs/{run_id}?include=dispatch_budget
// before every spawn and refuses budget_blocked or budget_check_failed. This
// file is the server half: the verdict, its live budget source, the
// override-binding exemption and the verify-pass exemption.

// runIncludeDispatchBudget is the GET /v0/runs/{run_id} include value that
// serves the dispatch_budget block.
const runIncludeDispatchBudget = "dispatch_budget"

// CategoryRunBudgetOverrideBound is the run-chain row binding an ADR-030
// budget_override grant to the run it admitted (create or recover). The
// dispatch-time evaluator reads it to exempt that run and its DecomposedFrom
// descendants. INTERNAL and OPERATOR-ONLY: it carries spend figures, so
// reads.go withholds it from run-bound audit reads (interim under ADR-106).
const CategoryRunBudgetOverrideBound = "run_budget_override_bound"

// dispatch_budget statuses.
const (
	dispatchBudgetClear   = "clear"
	dispatchBudgetBlocked = "blocked"
	dispatchBudgetExempt  = "exempt"
)

// dispatchBudgetExemptOverride is the exempt_reason of an override-bound run
// (or its DecomposedFrom descendant).
const dispatchBudgetExemptOverride = "budget_override"

// Budget sources (approval condition D2): which spec decided the verdict.
const (
	// dispatchBudgetSourceLive: the repository's CURRENT default-branch
	// .fishhawk/workflows.yaml, read through the GitHub client.
	dispatchBudgetSourceLive = "live_spec"
	// dispatchBudgetSourceAdmissionFallback: a configured live read FAILED and
	// the run's admission spec declares no blocking budget, so it clears.
	dispatchBudgetSourceAdmissionFallback = "admission_spec_fallback"
	// dispatchBudgetSourceAdmission: no server-side spec read exists for the
	// run (a GitLab run, or no GitHub client), so the cached admission spec
	// is evaluated.
	dispatchBudgetSourceAdmission = "admission_spec"
)

// budgetCheckFailedCode is the 503 error code of an undecided verdict.
const budgetCheckFailedCode = "budget_check_failed"

// budgetCheckError reasons. Each is a product-owned enum token; the raw cause
// is logged server-side with the run id and never reaches the response body.
const (
	budgetCheckLiveInstallationUnresolved = "live_spec_installation_unresolved"
	budgetCheckLiveUnavailable            = "live_spec_unavailable"
	budgetCheckLiveNotFound               = "live_spec_not_found"
	budgetCheckLiveInvalid                = "live_spec_invalid"
	budgetCheckWorkflowAbsent             = "workflow_absent_from_live_spec"
	budgetCheckSpendSumFailed             = "spend_sum_failed"
	budgetCheckBindingUnreadable          = "override_binding_unreadable"
)

// budgetCheckError is an undecided dispatch_budget verdict. The handler
// answers 503 budget_check_failed with details.reason = Reason.
type budgetCheckError struct {
	Reason string
	Err    error
	// VerifyPassStageIDs carries the stages the verify-pass exemption admits
	// even though the verdict is undecided (approval condition D1). It is
	// decided before any budget read.
	VerifyPassStageIDs []string
}

func (e *budgetCheckError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *budgetCheckError) Unwrap() error { return e.Err }

// dispatchBudgetPayload is the include=dispatch_budget block. The MCP client
// mirror decodes it; the json tags MUST byte-match its counterpart.
type dispatchBudgetPayload struct {
	// Status is clear | blocked | exempt.
	Status string `json:"status"`
	// BudgetSource names the spec that decided a clear or blocked verdict:
	// live_spec | admission_spec_fallback | admission_spec. Omitted on an
	// exempt verdict, which is decided before any budget read.
	BudgetSource string `json:"budget_source,omitempty"`
	// ExemptReason is budget_override on an exempt verdict.
	ExemptReason string `json:"exempt_reason,omitempty"`
	// OverrideRunID is the run whose run_budget_override_bound row exempts
	// this run: itself, or a DecomposedFrom ancestor.
	OverrideRunID string `json:"override_run_id,omitempty"`
	// Budget is the offending blocking budget on a blocked verdict.
	Budget *dispatchBudgetDetail `json:"budget,omitempty"`
	// VerifyPassStageIDs lists the run's implement stages that are
	// merge-candidate verify-only passes; the dispatcher spawns them even on a
	// blocked verdict.
	VerifyPassStageIDs []string `json:"verify_pass_stage_ids,omitempty"`
}

// dispatchBudgetDetail is the offending budget of a blocked verdict.
type dispatchBudgetDetail struct {
	WorkflowID  string `json:"workflow_id"`
	Period      string `json:"period"`
	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`
	// LimitUSD is the EFFECTIVE limit (FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD
	// when set), the one GET /v0/runs/{id}/budget reports.
	LimitUSD    float64 `json:"limit_usd"`
	SpentUSD    float64 `json:"spent_usd"`
	Enforcement string  `json:"enforcement"`
}

// budgetOverrideBindingStatus is the create / recover 201's
// budget_override_binding block: whether the override grant's dispatch
// exemption was recorded.
type budgetOverrideBindingStatus struct {
	Recorded bool   `json:"recorded"`
	Remedy   string `json:"remedy,omitempty"`
}

// budgetOverrideBindingRemedy is the recorded:false remedy text.
const budgetOverrideBindingRemedy = "run admitted past the exhausted blocking budget but its dispatch exemption was not recorded; " +
	"the host dispatcher will refuse its later stages budget_blocked until the period rolls over, limit_usd is raised " +
	"or enforcement is set to advisory; dispatch them by hand with fishhawk_dispatch_stage"

// budgetOverrideGrant is the override-admitted arm's grant, bound to the
// created run by bindBudgetOverride.
type budgetOverrideGrant struct {
	WorkflowID string
	Repo       string
	Period     string
	LimitUSD   float64
	Spent      float64
}

// budgetOverrideBoundPayload is the run_budget_override_bound row's payload.
type budgetOverrideBoundPayload struct {
	WorkflowID  string  `json:"workflow_id"`
	Repo        string  `json:"repo"`
	Period      string  `json:"period"`
	LimitUSD    float64 `json:"limit_usd"`
	Spent       float64 `json:"spent"`
	AdmittedVia string  `json:"admitted_via"`
}

// currentWorkflowBudgets returns the budgets the dispatch verdict evaluates,
// and the source they came from (ruling C1). A GitHub run on a server with a
// GitHub client reads the repository's CURRENT default-branch spec (an empty
// ref, the read StartRunForCampaignIssue and campaign_hotfix make); each
// failure is a *budgetCheckError with its own reason. A GitLab run, or a
// server with no GitHub client, has no server-side spec read, so the cached
// admission spec is returned (source admission_spec). Nothing else about the
// run reads the live spec.
func (s *Server) currentWorkflowBudgets(ctx context.Context, runRow *run.Run) ([]spec.PeriodicBudget, string, error) {
	if isGitLabRun(runRow) || s.cfg.GitHub == nil {
		return admissionSpecBudgets(runRow)
	}
	repoRef, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckLiveInstallationUnresolved, Err: err}
	}
	var instID int64
	if runRow.InstallationID != nil {
		instID = *runRow.InstallationID
	} else {
		id, ierr := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
		if ierr != nil {
			return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckLiveInstallationUnresolved, Err: ierr}
		}
		instID = id
	}
	fc, err := s.cfg.GitHub.GetWorkflowSpec(ctx, forge.FromGitHubInstallationID(instID), repoRef, "")
	if errors.Is(err, forge.ErrNotFound) {
		return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckLiveNotFound, Err: err}
	}
	if err != nil {
		return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckLiveUnavailable, Err: err}
	}
	parsed, err := spec.ParseBytes(fc.Content)
	if err != nil {
		return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckLiveInvalid, Err: err}
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, dispatchBudgetSourceLive, &budgetCheckError{Reason: budgetCheckWorkflowAbsent,
			Err: fmt.Errorf("workflow %q not defined in the default-branch spec", runRow.WorkflowID)}
	}
	return wf.Budgets, dispatchBudgetSourceLive, nil
}

// admissionSpecBudgets returns the budgets of the run's cached admission spec
// (runs.workflow_spec), source admission_spec. A missing spec, a parse failure
// or an absent workflow yields nil budgets, runBudgetStatus's historic
// posture. It never errors; the error return matches currentWorkflowBudgets'
// signature so either can stand in for the other.
func admissionSpecBudgets(runRow *run.Run) ([]spec.PeriodicBudget, string, error) {
	if len(runRow.WorkflowSpec) == 0 {
		return nil, dispatchBudgetSourceAdmission, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, dispatchBudgetSourceAdmission, nil
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, dispatchBudgetSourceAdmission, nil
	}
	return wf.Budgets, dispatchBudgetSourceAdmission, nil
}

// hasBlockingBudget reports whether any budget is enforcement: blocking.
func hasBlockingBudget(budgets []spec.PeriodicBudget) bool {
	for _, b := range budgets {
		if b.Enforcement == spec.EnforcementBlocking {
			return true
		}
	}
	return false
}

// withEffectiveLimits returns a copy of budgets whose LimitUSD is the
// effective limit (effectiveBudgetLimit, FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD).
func (s *Server) withEffectiveLimits(budgets []spec.PeriodicBudget) []spec.PeriodicBudget {
	out := make([]spec.PeriodicBudget, len(budgets))
	for i, b := range budgets {
		b.LimitUSD = s.effectiveBudgetLimit(b)
		out[i] = b
	}
	return out
}

// dispatchBudgetFor evaluates the include=dispatch_budget verdict for runRow
// at now. Order (approval conditions D1 and D2):
//
//  1. The exemptions are decided BEFORE any budget read. The verify-pass
//     stages are listed, then the override binding is walked; a bound run (or
//     its DecomposedFrom descendant) is exempt with no budget read, so it is
//     admitted even when the live read would fail. A binding read error is
//     held and returned only when the verdict would not be clear.
//  2. currentWorkflowBudgets resolves the live budgets. When a configured live
//     read FAILS, the run's admission spec is consulted: one that declares a
//     blocking budget fails closed (the read's *budgetCheckError); one that
//     declares none clears (admission_spec_fallback), so an advisory-only repo
//     never stalls on a read error.
//  3. No blocking budget, or a RunRepo without the cost-sum capability (the
//     admission gate's capability-absent skip), clears.
//  4. Every budget is evaluated at its EFFECTIVE limit through
//     webhook.CheckBlockingBudget; a sum error fails closed.
//  5. Over a blocking budget is blocked, with the offending budget's window
//     and spend.
func (s *Server) dispatchBudgetFor(ctx context.Context, runRow *run.Run, now time.Time) (*dispatchBudgetPayload, error) {
	verifyPass := s.verifyPassStageIDs(ctx, runRow)
	boundRun, bindErr := s.budgetOverrideBinding(ctx, runRow)
	if bindErr == nil && boundRun != nil {
		out := &dispatchBudgetPayload{ExemptReason: dispatchBudgetExemptOverride, OverrideRunID: boundRun.String(), VerifyPassStageIDs: verifyPass}
		out.Status = dispatchBudgetExempt
		return out, nil
	}
	clear := func(source string) *dispatchBudgetPayload {
		return &dispatchBudgetPayload{Status: dispatchBudgetClear, BudgetSource: source, VerifyPassStageIDs: verifyPass}
	}
	undecided := func(reason string, err error) error {
		return &budgetCheckError{Reason: reason, Err: err, VerifyPassStageIDs: verifyPass}
	}

	budgets, source, err := s.currentWorkflowBudgets(ctx, runRow)
	if err != nil {
		var bce *budgetCheckError
		if !errors.As(err, &bce) {
			bce = &budgetCheckError{Reason: budgetCheckLiveUnavailable, Err: err}
		}
		fallback, _, _ := admissionSpecBudgets(runRow)
		if hasBlockingBudget(fallback) {
			return nil, undecided(bce.Reason, bce.Err)
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"dispatch budget: live spec read failed; the admission spec declares no blocking budget, so the verdict clears",
			slog.String("run_id", runRow.ID.String()),
			slog.String("reason", bce.Reason),
			slog.String("error", errString(bce.Err)))
		return clear(dispatchBudgetSourceAdmissionFallback), nil
	}
	if !hasBlockingBudget(budgets) {
		return clear(source), nil
	}
	if s.cfg.RunRepo == nil {
		return clear(source), nil
	}
	summer, ok := s.cfg.RunRepo.(webhook.CostSummer)
	if !ok {
		return clear(source), nil
	}

	budgets = s.withEffectiveLimits(budgets)
	blocked, b, dec, err := webhook.CheckBlockingBudget(ctx, summer, runRow.Repo, runRow.WorkflowID, budgets, now, s.cfg.BudgetLocation)
	if err != nil {
		return nil, undecided(budgetCheckSpendSumFailed, err)
	}
	if !blocked {
		return clear(source), nil
	}
	if bindErr != nil {
		return nil, undecided(budgetCheckBindingUnreadable, bindErr)
	}

	loc := s.cfg.BudgetLocation
	if loc == nil {
		loc = time.UTC
	}
	start, end := budget.PeriodRange(b.Period, now, loc)
	out := &dispatchBudgetPayload{
		Status:       dispatchBudgetBlocked,
		BudgetSource: source,
		Budget: &dispatchBudgetDetail{
			WorkflowID:  runRow.WorkflowID,
			Period:      b.Period,
			PeriodStart: start.Format(time.RFC3339),
			PeriodEnd:   end.Format(time.RFC3339),
			LimitUSD:    b.LimitUSD,
			SpentUSD:    dec.Spent,
			Enforcement: string(b.Enforcement),
		},
		VerifyPassStageIDs: verifyPass,
	}
	return out, nil
}

// verifyPassStageIDs lists the run's implement stages in pending,
// awaiting_host_dispatch or dispatched for which isMergeCandidateVerifyPass
// holds: the live-trigger predicate the prompt endpoint and slot routing use,
// so an unreadable trigger is NOT listed (not exempt). A stage-list read error
// lists none (WARN-logged): the exemption fails closed, the verdict does not.
func (s *Server) verifyPassStageIDs(ctx context.Context, runRow *run.Run) []string {
	if s.cfg.RunRepo == nil {
		return nil
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"dispatch budget: list stages failed; no verify-pass stage is exempt",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return nil
	}
	var out []string
	for _, st := range stages {
		if st.Type != run.StageTypeImplement {
			continue
		}
		switch st.State {
		case run.StageStatePending, run.StageStateAwaitingHostDispatch, run.StageStateDispatched:
		default:
			continue
		}
		if s.isMergeCandidateVerifyPass(ctx, st) {
			out = append(out, st.ID.String())
		}
	}
	return out
}

// budgetExemptionParent is the ancestry link the override exemption inherits
// through: DecomposedFrom ONLY (ruling C2). A recovery, CI-retry or follow-up
// run (ParentRunID) needs its own override.
func budgetExemptionParent(r *run.Run) *uuid.UUID {
	return r.DecomposedFrom
}

// budgetOverrideBinding walks runRow and its DecomposedFrom ancestors (at
// most maxCampaignAncestorDepth hops) for a run_budget_override_bound row
// whose grant names the SAME repo and workflow_id as runRow; the first run
// carrying one wins and its id is returned. A missing parent, a cycle or a
// depth overrun stops the walk with a WARN and no exemption. A malformed row
// is skipped (WARN). A chain or ancestor read error is returned. A nil
// AuditRepo means no binding. A plain create cannot nominate a parent: only
// server-minted decomposition children carry DecomposedFrom.
func (s *Server) budgetOverrideBinding(ctx context.Context, runRow *run.Run) (*uuid.UUID, error) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return nil, nil
	}
	stop := func(why string) (*uuid.UUID, error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"dispatch budget: override-binding ancestry walk stopped; no exemption",
			slog.String("run_id", runRow.ID.String()),
			slog.String("reason", why))
		return nil, nil
	}
	cur := runRow
	seen := map[uuid.UUID]bool{cur.ID: true}
	for hops := 0; ; hops++ {
		rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, cur.ID, CategoryRunBudgetOverrideBound)
		if err != nil {
			return nil, fmt.Errorf("list %s for run %s: %w", CategoryRunBudgetOverrideBound, cur.ID, err)
		}
		for _, e := range rows {
			var p budgetOverrideBoundPayload
			if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
					"dispatch budget: malformed override-binding row skipped",
					slog.String("run_id", cur.ID.String()),
					slog.String("error", jerr.Error()))
				continue
			}
			if p.Repo == runRow.Repo && p.WorkflowID == runRow.WorkflowID {
				id := cur.ID
				return &id, nil
			}
		}
		parent := budgetExemptionParent(cur)
		if parent == nil {
			return nil, nil
		}
		if seen[*parent] {
			return stop(campaignAncestryCycle)
		}
		if hops >= maxCampaignAncestorDepth {
			return stop(campaignAncestryDepthExceeded)
		}
		next, err := s.cfg.RunRepo.GetRun(ctx, *parent)
		if errors.Is(err, run.ErrNotFound) {
			return stop(campaignAncestryMissingParent)
		}
		if err != nil {
			return nil, fmt.Errorf("read ancestor run %s: %w", *parent, err)
		}
		seen[next.ID] = true
		cur = next
	}
}

// bindBudgetOverride appends ONE run_budget_override_bound row to runID's
// chain for an override-admitted grant (via is create | recover), and reports
// the binding for the 201's budget_override_binding block. A nil grant (the
// run was not admitted past an exhausted blocking budget) binds nothing and
// returns nil. An append failure leaves the run admitted but unbound, which
// is fail-safe: it WARN-logs with the run id and returns recorded:false with
// the remedy. The run is NOT abandoned (unlike recordAppliesToOverride): the
// admitted override is already recorded globally (run_admitted_budget_override)
// and only the dispatch exemption is lost.
func (s *Server) bindBudgetOverride(ctx context.Context, runID uuid.UUID, grant *budgetOverrideGrant, via string) *budgetOverrideBindingStatus {
	if grant == nil {
		return nil
	}
	payload, _ := json.Marshal(budgetOverrideBoundPayload{
		WorkflowID:  grant.WorkflowID,
		Repo:        grant.Repo,
		Period:      grant.Period,
		LimitUSD:    grant.LimitUSD,
		Spent:       grant.Spent,
		AdmittedVia: via,
	})
	var err error
	if s.cfg.AuditRepo == nil {
		err = errors.New("audit repository unconfigured")
	} else {
		systemKind := audit.ActorKind("system")
		_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     runID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryRunBudgetOverrideBound,
			ActorKind: &systemKind,
			Payload:   payload,
		})
	}
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"budget override: append run_budget_override_bound failed; the run is admitted but its dispatch exemption is not recorded",
			slog.String("run_id", runID.String()),
			slog.String("workflow_id", grant.WorkflowID),
			slog.String("error", err.Error()))
		return &budgetOverrideBindingStatus{Recorded: false, Remedy: budgetOverrideBindingRemedy}
	}
	return &budgetOverrideBindingStatus{Recorded: true}
}
