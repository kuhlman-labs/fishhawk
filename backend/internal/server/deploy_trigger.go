package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// categoryDeploymentDispatchFailed records that the delegating deploy stage
// could NOT fire its external pipeline (DispatchWorkflow / webhook POST
// errored, or the delegate config was unusable). Paired with a category-C
// FailStage so a deploy that cannot trigger fails loudly rather than silently
// parking (#1386 / E23.6). Defined here (slice 1) rather than in deployment.go
// — it is an audit category, not an issue-comment surface, so it needs no
// docs/issue-comment-surfaces.md entry.
const categoryDeploymentDispatchFailed = "deployment_dispatch_failed"

// deployHTTPClient is the outbound client for the webhook delegate target. A
// dedicated client (not http.DefaultClient) bounds the POST and keeps the
// trigger's outbound surface explicit.
//
// CheckRedirect returns http.ErrUseLastResponse so NEITHER webhook POST (the
// forward trigger or the rollback re-dispatch) ever follows a redirect (E45.57
// / #3497). The POST may carry the resolved secret in a header or the body: a
// target that reflects the credential into a Location header, or a 307/308
// that would re-send the header/body to another host, must never produce a
// second request — Go's default policy would follow a 302 with GET and a
// 307/308 re-sending the body, and strips only Authorization /
// WWW-Authenticate / Cookie on a cross-host hop, so a PRIVATE-TOKEN header
// would be forwarded. A 3xx is returned as-is and lands in the non-2xx arm,
// whose reason/details carry ONLY the status code (never Location, never the
// body). CheckRedirect alone is NOT sufficient: net/http parses the Location
// header BEFORE consulting CheckRedirect and embeds the RAW header in the
// `failed to parse Location header %q` error, so postWebhookDeploy classifies
// every Do error into a fixed vocabulary and redacts the secret from every
// sink instead of propagating err.Error() verbatim.
var deployHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Fixed vocabulary for a webhook deploy POST transport failure (E45.57 /
// #3497, operator condition 1a). Only the CLASS — never the underlying error
// string, which can embed a raw Location header carrying the secret — reaches
// the failure reason, audit details and log line.
const (
	webhookErrClassTimeout           = "timeout"
	webhookErrClassDNS               = "dns"
	webhookErrClassConnectionRefused = "connection_refused"
	webhookErrClassTLS               = "tls"
	webhookErrClassRedirectParse     = "redirect_parse"
	webhookErrClassOther             = "other"
)

// webhookLocationParsePrefix is the prefix net/http (client.go) gives the
// error it returns when a 3xx Location header does not parse; that error is
// built BEFORE CheckRedirect runs and embeds the raw header verbatim.
const webhookLocationParsePrefix = "failed to parse Location header"

// classifyWebhookDoError maps a deployHTTPClient.Do error onto the fixed
// vocabulary above. It inspects the error only through errors.As / prefix
// checks and returns a constant — the input string is never returned.
func classifyWebhookDoError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil && strings.HasPrefix(ue.Err.Error(), webhookLocationParsePrefix) {
		return webhookErrClassRedirectParse
	}
	if strings.HasPrefix(err.Error(), webhookLocationParsePrefix) {
		return webhookErrClassRedirectParse
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return webhookErrClassTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return webhookErrClassDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return webhookErrClassConnectionRefused
	}
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if errors.As(err, &certErr) || errors.As(err, &recErr) || errors.As(err, &alertErr) {
		return webhookErrClassTLS
	}
	return webhookErrClassOther
}

// redactSecret replaces every occurrence of the resolved secret value in s
// with "[redacted]". Belt-and-braces (E45.57 / #3497, operator condition 1b):
// applied to EVERY string that reaches a sink for a webhook dispatch — the
// failure reason, each string detail value, the error returned upward and the
// log attrs — so even a sink the classifier does not cover cannot carry the
// value. An empty value is a no-op (nothing to redact).
func redactSecret(s, value string) string {
	if value == "" {
		return s
	}
	return strings.ReplaceAll(s, value, "[redacted]")
}

// redactSecretDetails applies redactSecret to every string value of details
// (nested maps are not produced by the webhook path; only the top level is
// walked) and returns the same map.
func redactSecretDetails(details map[string]any, value string) map[string]any {
	for k, v := range details {
		if str, ok := v.(string); ok {
			details[k] = redactSecret(str, value)
		}
	}
	return details
}

// webhookSecretPlacement names where the resolved secret rode for the audit
// payloads (names only — never the value).
const (
	webhookSecretPlacementHeader = "header"
	webhookSecretPlacementBody   = "body"
)

// webhookDeployResult is the outcome of a successful (2xx) webhook deploy
// POST: the NAMES the audit payload records — the variable the secret was
// read from and where it was placed — never the value.
type webhookDeployResult struct {
	SecretEnv       string
	SecretPlacement string
	DispatchedAt    time.Time
}

// webhookDeployError is a webhook deploy POST failure with the secret already
// redacted from every field. Reason is a caller-prefixable sentence; Details
// carries only committed/spec-derived or classified values (url, secret_env,
// status, error_class); Class is set for a transport failure.
type webhookDeployError struct {
	Reason  string
	Details map[string]any
	Class   string
}

// webhookTriggerBody builds the JSON trigger body both webhook POST sites
// send. The flat correlation keys are the ones the reconciler / callback
// contract already documents; the `variables` object repeats them in
// CI-variable form so a GitLab pipeline trigger targeted DIRECTLY receives
// them as `$FISHHAWK_RUN_ID` etc. (E45.57 / #3497). rollback adds the
// fishhawk_rollback marker in both forms. Every top-level key written here
// MUST be in spec.WebhookReservedBodyKeys — postWebhookDeploy asserts it.
func webhookTriggerBody(stage *run.Stage, runRow *run.Run, rollback bool) map[string]any {
	variables := map[string]string{
		"FISHHAWK_RUN_ID":      stage.RunID.String(),
		"FISHHAWK_STAGE_ID":    stage.ID.String(),
		"FISHHAWK_REPO":        runRow.Repo,
		"FISHHAWK_WORKFLOW_ID": runRow.WorkflowID,
	}
	body := map[string]any{
		"fishhawk_run_id":   stage.RunID.String(),
		"fishhawk_stage_id": stage.ID.String(),
		"repo":              runRow.Repo,
		"workflow_id":       runRow.WorkflowID,
	}
	if rollback {
		body[rollbackDispatchInput] = true
		variables["FISHHAWK_ROLLBACK"] = "true"
	}
	body["variables"] = variables
	return body
}

// lookupDeploySecret resolves the configured secret lookup seam: cfg's
// injected function, or os.LookupEnv over fishhawkd's own process environment.
func (s *Server) lookupDeploySecret(name string) (string, bool) {
	if s.cfg.DeploySecretLookup != nil {
		return s.cfg.DeploySecretLookup(name)
	}
	return os.LookupEnv(name)
}

// postWebhookDeploy is the ONE shared request builder + dispatcher behind both
// webhook POST sites (forward trigger and rollback; E45.57 / #3497). It:
//
//  1. asserts at runtime that every key in body is in
//     spec.WebhookReservedBodyKeys — a violation is a programming error that
//     names the offending KEY, so the validator's reserved set and the
//     writers can never silently diverge (checked BEFORE the secret field is
//     inserted; the field itself was validated non-reserved at admission);
//  2. resolves delegate.secret_env through the lookup seam; unset OR empty
//     fails naming ONLY the variable name;
//  3. places the value per SecretPlacement — header → req.Header.Set,
//     field → a top-level body key — and POSTs through deployHTTPClient,
//     which never follows a redirect;
//  4. maps a Do error onto the fixed class vocabulary (never err.Error())
//     and a non-2xx onto {status, url} ONLY — neither resp.Header (Location)
//     nor resp.Body is ever read into a sink;
//  5. redacts the resolved value from every string that leaves this function.
//
// The caller has already refused an empty delegate.URL with its own message.
func (s *Server) postWebhookDeploy(ctx context.Context, stage *run.Stage, delegate *spec.DelegateConfig, body map[string]any) (*webhookDeployResult, *webhookDeployError) {
	for k := range body {
		if !spec.IsWebhookReservedBodyKey(k) {
			return nil, &webhookDeployError{
				Reason:  fmt.Sprintf("webhook trigger body key %q is not in spec.WebhookReservedBodyKeys (programming error: extend the reserved set)", k),
				Details: map[string]any{"body_key": k, "url": delegate.URL},
			}
		}
	}

	var secret, placement string
	header, field := delegate.SecretPlacement()
	if delegate.SecretEnv != "" {
		value, ok := s.lookupDeploySecret(delegate.SecretEnv)
		if !ok || value == "" {
			state := "unset"
			if ok {
				state = "empty"
			}
			return nil, &webhookDeployError{
				Reason:  fmt.Sprintf("webhook secret env var %s is %s in fishhawkd's environment", delegate.SecretEnv, state),
				Details: map[string]any{"secret_env": delegate.SecretEnv, "url": delegate.URL},
			}
		}
		secret = value
		if field != "" {
			body[field] = secret
			placement = webhookSecretPlacementBody
		} else {
			placement = webhookSecretPlacementHeader
		}
	}
	fail := func(reason string, details map[string]any, class string) (*webhookDeployResult, *webhookDeployError) {
		return nil, &webhookDeployError{
			Reason:  redactSecret(reason, secret),
			Details: redactSecretDetails(details, secret),
			Class:   class,
		}
	}

	dispatchedAt := time.Now().UTC()
	triggerBody, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delegate.URL, bytes.NewReader(triggerBody))
	if err != nil {
		return fail("building webhook request failed", map[string]any{"error": err.Error(), "url": delegate.URL}, "")
	}
	req.Header.Set("Content-Type", "application/json")
	if header != "" && secret != "" {
		req.Header.Set(header, secret)
	}

	resp, err := deployHTTPClient.Do(req)
	if err != nil {
		class := classifyWebhookDoError(err)
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "deploy webhook POST failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error_class", class),
			slog.String("url", redactSecret(delegate.URL, secret)))
		return fail("webhook POST failed: "+class, map[string]any{"error_class": class, "url": delegate.URL}, class)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Status code ONLY: never Location, never the body.
		return fail(fmt.Sprintf("webhook POST returned a non-2xx status (%d)", resp.StatusCode),
			map[string]any{"status": resp.StatusCode, "url": delegate.URL}, "")
	}
	return &webhookDeployResult{
		SecretEnv:       delegate.SecretEnv,
		SecretPlacement: placement,
		DispatchedAt:    dispatchedAt,
	}, nil
}

// triggerDeploy fires the external delegating pipeline for an approved+dispatched
// deploy stage and parks it at awaiting_deployment (#1386 / E23.6, ADR-038).
//
// Called from advanceForDecision once an approved deploy stage has transitioned
// awaiting_deploy_approval → dispatched. It reads the stage's executor.delegate
// config from the run's cached workflow spec and, by target:
//
//   - github_actions — DispatchWorkflow (workflow_dispatch) carrying the
//     Fishhawk correlation token (fishhawk_run_id / fishhawk_stage_id) as
//     inputs, then best-effort resolves the resulting run id (the dispatch
//     endpoint returns 204 with no body) via ResolveDispatchedRun.
//   - webhook — POST a trigger payload to delegate.url.
//
// On a successful trigger it records the external run handle into the
// deployment_dispatched audit payload (so the slice-2 reconciler can read it
// back) and transitions dispatched → running → awaiting_deployment. On a trigger
// ERROR it writes a deployment_dispatch_failed audit and fails the stage
// category C — never a silent park.
//
// Returns the resulting stage (awaiting_deployment on success, failed on a
// trigger error) and an error ONLY for an internal repository failure the HTTP
// layer should surface as 500. The dispatch itself happens on the approval
// request path; the deploy gate already performs network I/O there, so this adds
// no new blocking posture.
//
// NOT-WIRED POSTURE: a github_actions target with no GitHub client configured
// (cfg.GitHub == nil) is the demo/un-wired backend, mirroring
// orchestrator.dispatchViaWorkflow — it WARN-logs and leaves the stage at
// dispatched rather than failing it. A genuine dispatch error (GitHub returned
// non-204) is distinct and DOES fail the stage.
func (s *Server) triggerDeploy(ctx context.Context, stage *run.Stage) (*run.Stage, error) {
	if s.cfg.RunRepo == nil {
		return stage, errors.New("deploy trigger requires a run repository")
	}

	delegate, runRow, failed, err := s.resolveDeployDelegate(ctx, stage)
	if delegate == nil {
		// resolveDeployDelegate already failed the stage + audited; propagate
		// its (failed-stage, err) verbatim.
		return failed, err
	}

	switch delegate.Target {
	case spec.DelegateTargetGitHubActions:
		return s.triggerDeployGitHubActions(ctx, stage, runRow, delegate)
	case spec.DelegateTargetWebhook:
		return s.triggerDeployWebhook(ctx, stage, runRow, delegate)
	default:
		return s.failDeployTrigger(ctx, stage,
			fmt.Sprintf("deploy delegate target %q is not supported", delegate.Target),
			map[string]any{"target": delegate.Target})
	}
}

// resolveDeployDelegate reads the deploy stage's executor.delegate config from
// the run's cached workflow spec. On any can't-resolve branch it fails the stage
// category C (the spec was already parsed at the pre-flight gate, so a failure
// here is an infrastructure-class surprise) and returns a nil delegate alongside
// the failed stage + error for the caller to propagate. On success it returns
// the delegate + run with a nil stage/error.
//
// It routes the stage lookup through the shared resolveDeploySpecStage chokepoint
// (E23.19 / #2642) so the trigger fires THIS stage's workflow_ref: on a
// multi-deploy-stage workflow, first-match would fire the FIRST deploy stage's
// ref while the gate admitted a LATER stage for its own environment — a worse
// end state than today's consistently-wrong behavior. Because the resolver's
// typed reason is a diagnostic the trigger does not surface distinctly (every
// spec-resolution failure here is the same category-C surprise), the two
// distinct trigger messages are "the deploy stage could not be resolved" and
// "the gated deploy stage declares no executor.delegate".
func (s *Server) resolveDeployDelegate(ctx context.Context, stage *run.Stage) (*spec.DelegateConfig, *run.Run, *run.Stage, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		failed, ferr := s.failDeployTrigger(ctx, stage, "deploy trigger: run lookup failed",
			map[string]any{"error": err.Error()})
		return nil, nil, failed, ferr
	}
	st, reason, rerr := s.resolveDeploySpecStage(ctx, runRow, stage)
	if reason != deployStageResolveOK {
		details := map[string]any{"resolve_reason": int(reason)}
		if rerr != nil {
			details["error"] = rerr.Error()
		}
		failed, ferr := s.failDeployTrigger(ctx, stage,
			"deploy trigger: the deploy stage could not be resolved from the run's cached spec", details)
		return nil, nil, failed, ferr
	}
	if st.Executor.Delegate == nil {
		failed, ferr := s.failDeployTrigger(ctx, stage,
			"deploy trigger: the gated deploy stage declares no executor.delegate", nil)
		return nil, nil, failed, ferr
	}
	return st.Executor.Delegate, runRow, nil, nil
}

// triggerDeployGitHubActions dispatches the customer's deploy workflow via
// workflow_dispatch and parks the stage at awaiting_deployment. The correlation
// token rides the dispatch INPUTS (the deploy workflow must declare
// fishhawk_run_id / fishhawk_stage_id inputs) so the reconciler can match the
// resulting run unambiguously (#1386 binding condition 1).
func (s *Server) triggerDeployGitHubActions(ctx context.Context, stage *run.Stage, runRow *run.Run, delegate *spec.DelegateConfig) (*run.Stage, error) {
	if delegate.WorkflowRef == "" {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: github_actions delegate is missing workflow_ref", nil)
	}
	// A GitLab-created run has no GitHub App installation, so the
	// github_actions delegate can never dispatch for it (#3465): fail the
	// stage NAMING the forge-neutral remedy rather than falling into the
	// generic no-installation_id message. Placed BEFORE the cfg.GitHub ==
	// nil guard deliberately — on a GitLab-only deployment that guard would
	// leave the stage parked at `dispatched` forever with no message at
	// all, and the InstallationID branch below would never be reached.
	if isGitLabRun(runRow) {
		installationRef := ""
		if runRow.InstallationRef != nil {
			installationRef = *runRow.InstallationRef
		}
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: the github_actions delegate dispatches workflow_dispatch through a GitHub App installation, and this run was created for GitLab (runner_kind gitlab_ci / installation_ref gitlab:<id>) so it has none; declare executor.delegate.target: webhook — the forge-neutral deploy path — for this workflow's deploy stage (see docs/deploy/gitlab.md 'Deploy stages on GitLab')",
			map[string]any{
				"runner_kind":      runRow.RunnerKind,
				"installation_ref": installationRef,
				"remedy":           "executor.delegate.target: webhook",
			})
	}
	if s.cfg.GitHub == nil {
		// Un-wired/demo backend — mirror orchestrator.dispatchViaWorkflow: WARN
		// and leave the stage at dispatched rather than failing it. A wired
		// production backend always has a GitHub client.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"deploy trigger: GitHub not configured; leaving deploy stage at dispatched",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()))
		return stage, nil
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: run has no installation_id; cannot dispatch the deploy workflow", nil)
	}
	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return s.failDeployTrigger(ctx, stage,
			fmt.Sprintf("deploy trigger: %v", err), map[string]any{"repo": runRow.Repo})
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	branch := delegate.GitRef
	if branch == "" {
		// The deploy targets the merged change; default to the repo's default
		// branch when the spec pins no explicit git_ref.
		branch = "main"
	}
	correlation := map[string]string{
		"fishhawk_run_id":   stage.RunID.String(),
		"fishhawk_stage_id": stage.ID.String(),
	}
	dispatchedAt := time.Now().UTC()

	if err := s.cfg.GitHub.DispatchWorkflow(ctx, scope, repo,
		delegate.WorkflowRef, branch, githubclient.DispatchInputs(correlation)); err != nil {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: workflow_dispatch failed",
			map[string]any{"error": err.Error(), "workflow_ref": delegate.WorkflowRef, "git_ref": branch})
	}

	// Best-effort run-id resolution. A failure or an indeterminate result is NOT
	// a trigger failure — the pipeline IS running; the reconciler re-resolves by
	// the correlation token + dispatched_at window stored in the audit payload.
	var ghaRunID int64
	var externalURL string
	resolved, rerr := s.cfg.GitHub.ResolveDispatchedRun(ctx, scope, repo, branch, correlation, dispatchedAt.Add(-1*time.Minute))
	switch {
	case rerr != nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"deploy trigger: dispatched-run resolution errored; reconciler will re-resolve",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", rerr.Error()))
	case resolved == nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"deploy trigger: dispatched run not yet resolvable; reconciler will re-resolve",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()))
	default:
		ghaRunID = resolved.ID
		externalURL = resolved.HTMLURL
	}

	payload := map[string]any{
		"run_id":           stage.RunID.String(),
		"stage_id":         stage.ID.String(),
		"target":           spec.DelegateTargetGitHubActions,
		"workflow_ref":     delegate.WorkflowRef,
		"git_ref":          branch,
		"dispatched_at":    dispatchedAt.Format(time.RFC3339),
		"gha_run_id":       ghaRunID,
		"external_run_url": externalURL,
	}
	return s.recordDispatchAndPark(ctx, stage, payload)
}

// triggerDeployWebhook POSTs the deploy trigger to the delegate's URL and parks
// the stage at awaiting_deployment. The external webhook-driven pipeline reports
// its terminal outcome by calling back into POST /v0/runs/{run_id}/deployment
// (#1395) — the reconciler does NOT poll webhook targets (slice 2). The POST
// goes through postWebhookDeploy, which carries the secret channel
// (secret_env / secret_header / secret_field), refuses redirects, classifies
// transport errors and redacts the secret (E45.57 / #3497); every failure
// fails the stage category C via failDeployTrigger with the NAME of the secret
// variable at most, never its value.
func (s *Server) triggerDeployWebhook(ctx context.Context, stage *run.Stage, runRow *run.Run, delegate *spec.DelegateConfig) (*run.Stage, error) {
	if delegate.URL == "" {
		return s.failDeployTrigger(ctx, stage,
			"deploy trigger: webhook delegate is missing url", nil)
	}
	res, werr := s.postWebhookDeploy(ctx, stage, delegate, webhookTriggerBody(stage, runRow, false))
	if werr != nil {
		return s.failDeployTrigger(ctx, stage, "deploy trigger: "+werr.Reason, werr.Details)
	}

	payload := map[string]any{
		"run_id":        stage.RunID.String(),
		"stage_id":      stage.ID.String(),
		"target":        spec.DelegateTargetWebhook,
		"url":           delegate.URL,
		"dispatched_at": res.DispatchedAt.Format(time.RFC3339),
	}
	if res.SecretEnv != "" {
		// Names only — the variable read and where its value rode.
		payload["secret_env"] = res.SecretEnv
		payload["secret_placement"] = res.SecretPlacement
	}
	return s.recordDispatchAndPark(ctx, stage, payload)
}

// recordDispatchAndPark writes the deployment_dispatched audit entry carrying
// the external run handle, then transitions the stage dispatched → running →
// awaiting_deployment. A failure to write the audit is fatal to the trigger (the
// reconciler reads the handle from that entry — a dispatched-but-unrecorded run
// would be unresolvable), so it fails the stage rather than parking blind.
func (s *Server) recordDispatchAndPark(ctx context.Context, stage *run.Stage, payload map[string]any) (*run.Stage, error) {
	raw, _ := json.Marshal(payload)
	if s.cfg.AuditRepo != nil {
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  CategoryDeploymentDispatched,
			ActorKind: &systemKind,
			Payload:   raw,
		}); err != nil {
			return s.failDeployTrigger(ctx, stage,
				"deploy trigger: recording the deployment_dispatched handle failed",
				map[string]any{"error": err.Error()})
		}
	}

	if _, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateRunning, nil); err != nil {
		return stage, fmt.Errorf("deploy trigger: dispatched → running: %w", err)
	}
	parked, err := s.cfg.RunRepo.TransitionStage(ctx, stage.ID, run.StageStateAwaitingDeployment, nil)
	if err != nil {
		return stage, fmt.Errorf("deploy trigger: running → awaiting_deployment: %w", err)
	}
	return parked, nil
}

// isGitLabRun reports whether the run was created for GitLab (#3465). It is a
// thin wrapper over the existing runForge classifier (issue_approval.go:
// RunnerKind gitlab_ci wins, else the installation_ref scheme — a `gitlab:`
// ref is GitLab; nil, empty and bare-decimal refs are GitHub) so the deploy
// trigger and the issue-approval path classify a run's forge IDENTICALLY
// rather than each re-deriving it. A nil run is not a GitLab run.
func isGitLabRun(r *run.Run) bool {
	return r != nil && runForge(r) == webhook.ForgeGitLab
}

// failDeployTrigger writes a deployment_dispatch_failed audit (system actor) and
// fails the stage category C. Best-effort audit: a logged append failure never
// suppresses the FailStage. Returns the failed stage (or, if FailStage itself
// errors, the original stage + a wrapped error for the 500 path).
func (s *Server) failDeployTrigger(ctx context.Context, stage *run.Stage, reason string, details map[string]any) (*run.Stage, error) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "deploy trigger failed",
		slog.String("run_id", stage.RunID.String()),
		slog.String("stage_id", stage.ID.String()),
		slog.String("reason", reason))

	if s.cfg.AuditRepo != nil {
		if details == nil {
			details = map[string]any{}
		}
		details["stage_id"] = stage.ID.String()
		details["reason"] = reason
		payload, _ := json.Marshal(details)
		systemKind := audit.ActorKind("system")
		if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID:     stage.RunID,
			StageID:   &stage.ID,
			Timestamp: time.Now().UTC(),
			Category:  categoryDeploymentDispatchFailed,
			ActorKind: &systemKind,
			Payload:   payload,
		}); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"deploy trigger: append deployment_dispatch_failed audit failed",
				slog.String("run_id", stage.RunID.String()),
				slog.String("stage_id", stage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	failed, err := run.FailStage(ctx, s.cfg.RunRepo, stage.ID, run.FailureC, reason)
	if err != nil {
		return stage, fmt.Errorf("deploy trigger: failing stage: %w", err)
	}
	return failed, nil
}

// parseRepoRef splits "owner/name" into a forge.RepoRef. Local to the
// server package (orchestrator.parseRepo is unexported); a shared helper is a
// v0.x cleanup.
func parseRepoRef(s string) (forge.RepoRef, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			if i == 0 || i == len(s)-1 {
				return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
			}
			return forge.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
		}
	}
	return forge.RepoRef{}, fmt.Errorf("malformed repo %q", s)
}
