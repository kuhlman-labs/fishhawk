package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/runnerbackend"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// GitLab run creation — the go-live half of E45.22 / #2043.
//
// #1861 landed the gitlab_ci plumbing DORMANT: a runner_kind, a
// runnerbackend.GitLabCI pipeline trigger, and an auditcheckpublisher route
// that all existed but that nothing ever minted a run for, because the
// dispatcher parked every GitLab MatchActionRun before the spec fetch. This
// file unparks it.
//
// It is a SEPARATE path from Handle's GitHub create rather than a shared one
// for three reasons the shared code cannot absorb: the spec read goes through
// the forge-neutral FileFetcher (the GitHub client's scope parse rejects a
// "gitlab:<project_id>" ref outright), there is no branch-protection snapshot
// to take (GitLab's protected-branches API contributes no required-status
// contexts, so the snapshot would be empty and the ADR-017 refusal would
// reject every GitLab run), and the comment-back / board-sync notifiers are
// GitHub-only. Everything that IS shared — applies_to admission, the blocking
// budget gate, the coarse plan-reviewer gate, stage creation, the
// run_dispatched audit — is called here, not reimplemented.

// gitLabWorkflowSpecPath is the repo-relative path of the workflow spec.
// Mirrors githubclient.WorkflowSpecPath; named locally so this package does
// not import the GitHub client for a constant.
const gitLabWorkflowSpecPath = ".fishhawk/workflows.yaml"

// GitLabProjectAuthorizer answers whether the GitLab project a webhook payload
// names is one this deployment is REGISTERED to act on. It is the
// authorization binding between untrusted payload identity and an operator
// decision, and it gates GitLab run creation.
//
// WHY IT EXISTS. The GitLab receiver authenticates a delivery by comparing
// X-Gitlab-Token against ONE shared deployment secret, and GitLab signs no HMAC
// over the body (see VerifyGitLabToken). A valid token therefore proves the
// sender knows the deployment secret; it proves NOTHING about the project named
// in the payload. Both project selectors on the create path are payload-
// derived: ev.CredentialRef ("gitlab:<project_id>") selects the credential AND
// the project the pipeline is created in, while ev.Repo
// (path_with_namespace) selects the project the executable workflow spec is
// READ from. Left unbound, a token holder could aim either at any project the
// deployment credential can reach — untrusted input steering sensitive
// credentials, network egress and code execution. That is a confused deputy,
// and it is why the check runs BEFORE the spec fetch and before the pipeline
// trigger rather than between them.
//
// The GitHub path needs no analogue: its identity (ev.InstallationID) arrives
// inside an HMAC-signed payload, and Dispatcher.Accounts resolves it to an
// owning tenant.
type GitLabProjectAuthorizer interface {
	// AuthorizedGitLabProject reports whether credentialRef names a
	// registered GitLab installation AND projectPath belongs to that
	// installation's account. BOTH halves are required because the two
	// selectors address different projects: registering only the id would
	// still let a payload name any readable path for the spec read.
	//
	// A false answer is a REFUSAL, not an error. An error means the
	// registry could not be consulted; the caller fails closed on both.
	AuthorizedGitLabProject(ctx context.Context, credentialRef, projectPath string) (bool, error)
}

// gitLabAuthzRefusal names WHY a GitLab trigger was refused before any forge
// call. They are audit-payload `reason` values on the existing run-less
// rejection category, not new categories — the run row the refusal prevents
// does not exist, so there is nothing to chain a per-run category to.
const (
	// gitLabAuthzRegistryUnwired is the FAIL-CLOSED default: no project
	// registry is configured, so no project can be shown to be authorized.
	// GitLab run creation is off until the operator wires one.
	gitLabAuthzRegistryUnwired = "gitlab_project_registry_unwired"
	// gitLabAuthzRefUnknown means the payload named no project id at all.
	gitLabAuthzRefUnknown = "gitlab_project_ref_absent"
	// gitLabAuthzLookupFailed means the registry could not be read. A
	// transient fault must not open the gate, so it refuses.
	gitLabAuthzLookupFailed = "gitlab_project_authorization_lookup_failed"
	// gitLabAuthzNotRegistered is the refusal proper: the project is not one
	// this deployment was authorized to act on.
	gitLabAuthzNotRegistered = "gitlab_project_not_registered"
)

// gitLabAuthzPath names WHICH gate refused. Both GitLab entry points — run
// creation and CI-failure retry — share one authorizer and one refusal
// category, so without this discriminator the two refusals are byte-identical
// in the audit and an operator cannot tell a rejected trigger from a rejected
// retry. Same #2860 principle as the ci_retry_skipped reasons.
const (
	gitLabAuthzPathCreate  = "create_run"
	gitLabAuthzPathCIRetry = "ci_failure_retry"
)

// authorizeGitLabProject is the authorization gate for EVERY GitLab entry
// point that acts on a payload-named project — run creation and CI-failure
// retry alike. It returns true only when the registry positively vouches for
// the payload's project; every other outcome — unwired registry, absent ref,
// lookup fault, unknown project — refuses, writes the run-less rejection audit
// naming which (and via `path`, which gate), and leaves ZERO forge calls
// behind.
//
// It is deliberately shared rather than duplicated per path: an authorizer
// applied to only one of two entry points is the asymmetry that made the retry
// path a forgeable pipeline-dispatch surface in the first place.
func (d *Dispatcher) authorizeGitLabProject(ctx context.Context, ev Event, m Match,
	path string, now time.Time) bool {
	reason := ""
	switch {
	case d.GitLabProjects == nil:
		reason = gitLabAuthzRegistryUnwired
	case ev.CredentialRef == "":
		reason = gitLabAuthzRefUnknown
	default:
		ok, err := d.GitLabProjects.AuthorizedGitLabProject(ctx, ev.CredentialRef, ev.Repo)
		switch {
		case err != nil:
			reason = gitLabAuthzLookupFailed
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"gitlab dispatch: project authorization lookup failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("repo", ev.Repo),
				slog.String("error", err.Error()))
		case !ok:
			reason = gitLabAuthzNotRegistered
		}
	}
	if reason == "" {
		return true
	}
	d.writeGitLabUnauthorizedProjectAudit(ctx, ev, m, path, reason, now)
	d.logger().LogAttrs(ctx, slog.LevelWarn,
		"gitlab dispatch rejected: project not authorized",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("credential_ref", ev.CredentialRef),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("path", path),
		slog.String("reason", reason))
	return false
}

// writeGitLabUnauthorizedProjectAudit records the refusal on the SAME run-less
// category the plan-reviewer guard uses (run_rejected_misconfigured), keyed
// apart by its `reason`. No run row exists at this point — the whole purpose of
// the guard is that none is minted — so the entry is global-chained.
func (d *Dispatcher) writeGitLabUnauthorizedProjectAudit(ctx context.Context, ev Event, m Match,
	path, reason string, now time.Time) {
	if d.Audit == nil {
		return
	}
	systemKind := audit.ActorKind("system")
	payload, _ := json.Marshal(map[string]any{
		"reason":         reason,
		"path":           path,
		"repo":           ev.Repo,
		"credential_ref": ev.CredentialRef,
		"workflow_id":    m.WorkflowID,
		"delivery_id":    ev.DeliveryID,
		"event":          ev.Type,
		"forge":          string(ForgeGitLab),
	})
	if _, err := d.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    now,
		Category:     "run_rejected_misconfigured",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("gitlab-webhook"),
		Payload:      payload,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"append gitlab run_rejected_misconfigured audit entry failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
	}
}

// handleGitLabCreateRun creates a run from an admitted GitLab trigger.
//
// The run is stamped runner_kind=gitlab_ci as a CREATION-TIME HINT with
// runner_kind_resolved left false (the column's default): the ADR-045 runner
// self-report lock stays authoritative, so this never pre-empts it. It is also
// stamped installation_ref = the event's "gitlab:<project_id>" credential ref,
// which is what lets orchestrator.runCredentialScope resolve a non-zero scope
// for the run's LATER stages instead of warn-skipping every one of them
// (E45.22 HIGH 1).
//
// Returns non-nil only for a transient fault the receiver should surface as a
// 5xx; every refusal returns nil with an audit row and/or a structured log.
func (d *Dispatcher) handleGitLabCreateRun(ctx context.Context, ev Event, m Match, now time.Time) error {
	// Step 0: AUTHORIZATION. First statement in the function on purpose —
	// every later step either reads with deployment credentials or executes
	// code in the named project, so an unauthorized payload must fall out
	// here having caused zero forge calls. See GitLabProjectAuthorizer.
	if !d.authorizeGitLabProject(ctx, ev, m, gitLabAuthzPathCreate, now) {
		return nil
	}
	repo, err := parseRepo(ev.Repo)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn, "gitlab dispatch: repo malformed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo))
		return nil
	}
	scope := ev.credentialScope()
	ref := d.DefaultRef
	if ref == "" {
		ref = "main"
	}

	// Step 1: read the spec through the forge-neutral FileFetcher. An
	// unwired GitLab file reader is a DEPLOYMENT misconfiguration, not a
	// transient fault — log and skip rather than 5xx into a redelivery loop.
	if d.GitLabFiles == nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"gitlab dispatch: no GitLab file reader configured; cannot fetch workflow spec",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo))
		return nil
	}
	specFile, err := d.GitLabFiles.FetchFile(ctx, scope, repo, gitLabWorkflowSpecPath, ref)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) || errors.Is(err, forge.ErrNotFound) {
			d.logSkipFromGitHub(ctx, ev, err)
			return nil
		}
		return fmt.Errorf("dispatcher: get gitlab workflow spec: %w", err)
	}

	// Step 2 / 3: parse + resolve the requested workflow.
	parsed, err := spec.ParseBytes(specFile.Content)
	if err != nil {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA, err, now)
		return nil
	}
	workflow, ok := parsed.Workflows[m.WorkflowID]
	if !ok {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA,
			fmt.Errorf("workflow_id %q not defined in %s", m.WorkflowID, gitLabWorkflowSpecPath), now)
		return nil
	}
	if len(workflow.Stages) == 0 {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA,
			fmt.Errorf("workflow_id %q has no stages", m.WorkflowID), now)
		return nil
	}

	// Step 3.4: the COARSE plan-reviewer gate, identical to the GitHub path —
	// a gating plan stage on a deployment with no review backend at all can
	// never satisfy its gate, so refuse rather than mint an unsatisfiable run.
	// The comment-back is GitHub-only, so the audit + WARN are the whole
	// refusal surface here.
	if !d.PlanReviewerConfigured {
		for _, st := range workflow.Stages {
			if st.Type != spec.StageTypePlan || st.Reviewers == nil {
				continue
			}
			if planreview.ResolveAuthority(*st.Reviewers) != planreview.AuthorityGating {
				continue
			}
			d.writeReviewerMisconfiguredAudit(ctx, ev, m, st, now)
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"gitlab dispatch rejected: plan reviewer unconfigured",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("repo", ev.Repo),
				slog.String("workflow_id", m.WorkflowID),
				slog.String("stage", st.ID))
			return nil
		}
	}

	// Step 3.45 / 3.6: the SAME admission gates the GitHub path runs, in the
	// same order — routing (`applies_to`) answers whether the run should exist
	// at all, then the blocking periodic budget. A GitLab trigger is admitted
	// exactly as a GitHub one; neither gate is forge-aware.
	if d.refusedByAppliesTo(ctx, ev, m, workflow, parsed, scope, specFile.SHA, now) {
		return nil
	}
	if d.refusedByBlockingBudget(ctx, ev, m, workflow, specFile.SHA, now) {
		return nil
	}

	// Step 4: create the run. installation_ref carries the forge-neutral
	// credential reference; InstallationID stays nil because a GitLab project
	// has no GitHub App installation id.
	triggerRef := m.TriggerRef
	credentialRef := ev.CredentialRef
	created, err := d.Runs.CreateRun(ctx, run.CreateRunParams{
		Repo:               ev.Repo,
		WorkflowID:         m.WorkflowID,
		WorkflowSHA:        specFile.SHA,
		TriggerSource:      m.TriggerSource,
		TriggerRef:         &triggerRef,
		InstallationRef:    &credentialRef,
		ParentRunID:        d.findParentRunID(ctx, ev.Repo, triggerRef),
		WorkflowSpec:       specFile.Content,
		MaxRetriesSnapshot: WorkflowMaxRetries(workflow),
		// The ADR-045 creation-time HINT. runner_kind_resolved stays at the
		// column default (false) — CreateRunParams carries no field for it —
		// so the runner's signed self-report remains authoritative and can
		// still contradict this hint without a lock conflict.
		RunnerKind: run.RunnerKindGitLabCI,
	})
	if err != nil {
		return fmt.Errorf("dispatcher: create gitlab run: %w", err)
	}

	// Step 5: one stage row per spec stage.
	stages, err := CreateStagesFromSpec(ctx, d.Runs, created.ID, workflow.Stages)
	if err != nil {
		return fmt.Errorf("dispatcher: create gitlab stages: %w", err)
	}

	// Step 6: trigger the first stage through the gitlab_ci backend. The
	// FIRST stage's pipeline necessarily runs on the default ref: the run's
	// ADR-035 sole-writer branch does not exist until the runner creates it.
	// That is the same premise the CI-retry correlation contract rests on —
	// see gitlab_ciretry.go's first-stage deferral.
	firstStage := stages[0]
	backend, _ := d.backends().Backend(run.RunnerKindGitLabCI)
	dispatchErr := backend.TriggerStage(ctx, runnerbackend.TriggerParams{
		RunID:            created.ID,
		StageID:          firstStage.ID,
		WorkflowID:       m.WorkflowID,
		StageExecutorRef: firstStage.ExecutorRef,
		Repo:             ev.Repo,
		Scope:            scope,
		Ref:              ref,
	})
	if dispatchErr == nil {
		if _, err := d.Runs.TransitionStage(ctx, firstStage.ID,
			run.StageStateDispatched, nil); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"gitlab dispatch: transition stage to dispatched failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("stage_id", firstStage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	// Step 7: audit. Same category and payload shape as the GitHub path so a
	// run_dispatched consumer needs no forge branch.
	d.writeDispatchAudit(ctx, ev, m, created, specFile.SHA, dispatchErr, now)

	if dispatchErr != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn, "gitlab webhook dispatch failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
			slog.String("run_id", created.ID.String()),
			slog.String("error", dispatchErr.Error()))
		return nil
	}
	d.logger().LogAttrs(ctx, slog.LevelInfo, "gitlab webhook dispatched",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("run_id", created.ID.String()),
		slog.String("stage_id", firstStage.ID.String()),
		slog.String("installation_ref", credentialRef),
	)
	return nil
}
