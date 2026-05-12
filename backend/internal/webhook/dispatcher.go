package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// FishhawkLabel is the issue / PR label that triggers a default
// run. Customers add this label to their issue when they want
// Fishhawk to start a workflow.
const FishhawkLabel = "fishhawk"

// CommentTrigger is the chat-style command that triggers a run
// from an issue or PR comment.
const CommentTrigger = "/fishhawk run"

// CommentApprove and CommentReject are the chat-style commands that
// submit a gate decision against an issue's currently-awaiting-
// approval stage (#238). The reviewer can leave an optional comment
// on the line(s) following the command — captured into the
// approval row's `comment` column.
const (
	CommentApprove = "/fishhawk approve"
	CommentReject  = "/fishhawk reject"
)

// MatchAction tags how a matched event should be handled. Run is
// the historical default (create + workflow_dispatch); approve and
// reject act on an existing run's gate state without dispatching
// new work; runner_action_failed flips a stuck dispatched stage to
// failed-C when the customer-side GitHub Actions run errored out.
type MatchAction string

// MatchAction values.
const (
	MatchActionRun                MatchAction = "run"
	MatchActionApprove            MatchAction = "approve"
	MatchActionReject             MatchAction = "reject"
	MatchActionRunnerActionFailed MatchAction = "runner_action_failed"
	// MatchActionCIFailureRetry tags a check_run.completed event
	// whose conclusion is in the fail bucket (per
	// stagecheck.DeriveState) and whose target is a Fishhawk-
	// managed PR (#276 / #278). The dispatcher's handler in #279
	// looks up the run by PR URL, counts retries against the
	// workflow's on_ci_failure.max_retries (#277), and either fires
	// a fresh implement workflow_dispatch or audits a
	// `ci_retry_exhausted`. Matching stays pure here; the real I/O
	// lives in handleCIFailureRetry.
	MatchActionCIFailureRetry MatchAction = "ci_failure_retry"
)

// DefaultWorkflowID is the workflow_id (a key under `workflows:`
// in `.fishhawk/workflows.yaml`) the dispatcher selects when the
// trigger doesn't specify one. Per MVP_SPEC §4.2's example.
const DefaultWorkflowID = "feature_change"

// DefaultActionsWorkflowFile is the customer-side GitHub Actions
// workflow file that calls `kuhlman-labs/fishhawk/runner@vX.Y`.
// Customers commit this at .github/workflows/<file>.yml; v0
// hardcodes the convention.
const DefaultActionsWorkflowFile = "fishhawk.yml"

// Match describes what to do with a webhook event after the
// receiver has accepted it. Skip=true means "no action; record the
// reason in the audit log and return 202." Skip=false means
// "perform Action against this run." For Action=run that's "create
// a Run with these inputs and fire workflow_dispatch." For
// Action=approve / reject it's "submit a gate decision against the
// existing run for this issue."
type Match struct {
	Skip   bool
	Reason string

	// Action tags what kind of side effect Skip=false implies. Empty
	// is treated as MatchActionRun for backwards-compatibility with
	// the existing dispatcher path.
	Action MatchAction

	WorkflowID    string
	TriggerSource run.TriggerSource
	TriggerRef    string

	// IssueRef is the parsed (number, body) tuple for issue-style
	// triggers; empty for non-issue triggers.
	IssueRef *IssueRef

	// CommentBody is the trailing text of a slash command, when the
	// comment carries a reason after the command word. Captured for
	// approve / reject so the approval row's `comment` column gets
	// the reviewer's rationale.
	CommentBody string

	// WorkflowRunID is the GitHub Actions run id from a
	// `workflow_run.completed` event. Set for
	// MatchActionRunnerActionFailed so the dispatcher can fetch
	// the run's inputs (run_id / stage_id) via the actions API.
	WorkflowRunID int64

	// WorkflowRunConclusion is the GitHub Actions terminal status
	// — `failure`, `timed_out`, `cancelled`, etc. Captured into
	// the audit row's failure_reason so operators can correlate.
	WorkflowRunConclusion string

	// CheckRunRef carries the bits of a check_run.completed payload
	// the CI-retry handler needs (#278). Set when Action is
	// MatchActionCIFailureRetry. The handler in #279 uses
	// (PRNumber, HeadSHA) to look up the parent Fishhawk run via
	// runs.pull_request_url and uses CheckName + Conclusion for
	// the audit-row payload.
	CheckRunRef *CheckRunRef
}

// CheckRunRef is the subset of a check_run payload the CI-retry
// dispatcher path needs (#278). All fields are required for
// MatchActionCIFailureRetry — matchCheckRun fills them in lock-
// step before tagging the action.
type CheckRunRef struct {
	PRNumber   int
	HeadSHA    string
	CheckName  string
	Conclusion string
}

// IssueRef captures the bits of an issue payload the dispatcher
// surfaces. Body lets the comment-trigger detector pattern-match
// without re-decoding the raw event.
type IssueRef struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

// MatchEvent classifies an event into a Match. Pure: no I/O, no
// side effects. Tests drive it with fixture event payloads to
// pin the v0 dispatch rules.
//
// Rules per #109:
//   - Bot-authored events skip (avoid feedback loops between Fishhawk
//     itself and other bots running in the customer's workflow).
//   - issues.labeled with the `fishhawk` label → dispatch.
//   - issue_comment.created containing "/fishhawk run" → dispatch.
//   - Everything else is acknowledged but skipped.
func MatchEvent(ev Event) Match {
	if ev.SenderType == "Bot" {
		return Match{Skip: true, Reason: "sender is a bot"}
	}
	if ev.InstallationID == 0 {
		return Match{Skip: true, Reason: "no installation id in payload"}
	}

	switch ev.Type {
	case "issues":
		return matchIssue(ev)
	case "issue_comment":
		return matchIssueComment(ev)
	case "workflow_run":
		return matchWorkflowRun(ev)
	case "check_run":
		return matchCheckRun(ev)
	case "branch_protection_rule", "repository_ruleset":
		// Recognized for #251 / ADR-017: an upstream protection
		// edit invalidates any cached snapshot for the repo. v0
		// reads protection on every run-create (no cache to bust),
		// so the receiver acknowledges + skips. Future caching
		// adds work here without changing the webhook contract or
		// requiring a re-install.
		return Match{Skip: true,
			Reason: fmt.Sprintf("%s event acknowledged; v0 reads protection per-run", ev.Type)}
	default:
		return Match{Skip: true,
			Reason: fmt.Sprintf("unrecognized event type %q", ev.Type)}
	}
}

func matchIssue(ev Event) Match {
	if ev.Action != "labeled" {
		return Match{Skip: true,
			Reason: fmt.Sprintf("issues.%s is not a trigger action", ev.Action)}
	}
	var payload struct {
		Issue struct {
			Number int `json:"number"`
		} `json:"issue"`
		Label struct {
			Name string `json:"name"`
		} `json:"label"`
	}
	if err := json.Unmarshal(ev.RawBody, &payload); err != nil {
		return Match{Skip: true, Reason: "issues payload parse failed"}
	}
	if !strings.EqualFold(payload.Label.Name, FishhawkLabel) {
		return Match{Skip: true,
			Reason: fmt.Sprintf("label %q is not fishhawk", payload.Label.Name)}
	}
	if payload.Issue.Number == 0 {
		return Match{Skip: true, Reason: "issue payload missing number"}
	}
	return Match{
		Action:        MatchActionRun,
		WorkflowID:    DefaultWorkflowID,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    fmt.Sprintf("issue:%d", payload.Issue.Number),
		IssueRef: &IssueRef{
			Number: payload.Issue.Number,
		},
	}
}

func matchIssueComment(ev Event) Match {
	if ev.Action != "created" {
		return Match{Skip: true,
			Reason: fmt.Sprintf("issue_comment.%s is not a trigger action", ev.Action)}
	}
	var payload struct {
		Comment struct {
			Body string `json:"body"`
		} `json:"comment"`
		Issue struct {
			Number int `json:"number"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(ev.RawBody, &payload); err != nil {
		return Match{Skip: true, Reason: "issue_comment payload parse failed"}
	}
	body := strings.TrimSpace(payload.Comment.Body)
	if payload.Issue.Number == 0 {
		return Match{Skip: true, Reason: "issue_comment payload missing issue number"}
	}

	// Pick the most-specific command first so /fishhawk approve
	// doesn't accidentally classify as /fishhawk run when the
	// "/fishhawk" prefix coincides. Each branch leaves the trailing
	// text (after the command) in CommentBody so approve / reject
	// can capture an optional reason. The match is anchored at the
	// start of the body — comments that begin with prose followed
	// by the command are intentionally not honored (avoids quoted-
	// reply false positives like "Should I run `/fishhawk run`?").
	switch {
	case isCommand(body, CommentApprove):
		return Match{
			Action:        MatchActionApprove,
			TriggerSource: run.TriggerGitHubIssue,
			TriggerRef:    fmt.Sprintf("issue:%d", payload.Issue.Number),
			IssueRef:      &IssueRef{Number: payload.Issue.Number, Body: payload.Comment.Body},
			CommentBody:   trailingComment(body, CommentApprove),
		}
	case isCommand(body, CommentReject):
		return Match{
			Action:        MatchActionReject,
			TriggerSource: run.TriggerGitHubIssue,
			TriggerRef:    fmt.Sprintf("issue:%d", payload.Issue.Number),
			IssueRef:      &IssueRef{Number: payload.Issue.Number, Body: payload.Comment.Body},
			CommentBody:   trailingComment(body, CommentReject),
		}
	case isCommand(body, CommentTrigger):
		return Match{
			Action:        MatchActionRun,
			WorkflowID:    DefaultWorkflowID,
			TriggerSource: run.TriggerGitHubIssue,
			TriggerRef:    fmt.Sprintf("issue:%d", payload.Issue.Number),
			IssueRef:      &IssueRef{Number: payload.Issue.Number, Body: payload.Comment.Body},
		}
	}
	return Match{Skip: true,
		Reason: fmt.Sprintf("comment does not start with a Fishhawk command (recognized: %q, %q, %q)",
			CommentTrigger, CommentApprove, CommentReject)}
}

// isCommand returns true when body starts with command followed by
// either end-of-string, whitespace, or a newline. Matches
// "/fishhawk run", "/fishhawk run\n…", "/fishhawk run because reason"
// — but not "/fishhawk runner" (no false-prefix match against a
// longer-but-similar command name).
func isCommand(body, command string) bool {
	if !strings.HasPrefix(body, command) {
		return false
	}
	if len(body) == len(command) {
		return true
	}
	next := body[len(command)]
	return next == ' ' || next == '\t' || next == '\n' || next == '\r'
}

// trailingComment returns the trimmed text after a command word,
// or "" when the command is the entire body. Used to capture the
// reviewer's rationale on approve / reject. Multi-line bodies keep
// internal newlines; only leading and trailing whitespace is
// trimmed.
func trailingComment(body, command string) string {
	if len(body) <= len(command) {
		return ""
	}
	return strings.TrimSpace(body[len(command):])
}

// failedRunnerConclusions enumerates the GitHub Actions terminal
// statuses that indicate the runner action failed before reporting
// in (#243). `success` / `neutral` / `skipped` are excluded — the
// trace upload is the canonical success signal, and a skipped run
// (e.g., a workflow that was a no-op) is by definition fine.
//
// Closed set so a future GitHub-side conclusion (`stale`,
// `startup_failure`, etc.) lands as "not a failure we recognize" by
// default. Adding a new conclusion to this map is a deliberate
// decision after confirming the operator wants Fishhawk to flip the
// stage to failed-C on it.
var failedRunnerConclusions = map[string]struct{}{
	"failure":         {},
	"timed_out":       {},
	"cancelled":       {},
	"action_required": {},
	"startup_failure": {},
	"stale":           {},
}

// matchWorkflowRun classifies a `workflow_run.completed` event for
// the customer's runner workflow file (#243). Fishhawk uses
// workflow_dispatch to fire `fishhawk.yml` on the customer's repo;
// when that run errors out before uploading a trace, the matched
// stage stays in `dispatched` until the watchdog times out. This
// matcher routes the failure signal so the stage flips to failed-C
// immediately.
//
// Skip rules:
//   - action != "completed" — only the terminal event matters.
//   - workflow path != fishhawk's actions file — ignore other
//     workflows in the customer's repo.
//   - conclusion not in failedRunnerConclusions — success / neutral
//     / skipped don't need our intervention.
//   - workflow_run.id zero / parse failure — bad payload, skip.
func matchWorkflowRun(ev Event) Match {
	if ev.Action != "completed" {
		return Match{Skip: true,
			Reason: fmt.Sprintf("workflow_run.%s is not a terminal action", ev.Action)}
	}
	var payload struct {
		WorkflowRun struct {
			ID         int64  `json:"id"`
			Path       string `json:"path"`
			Conclusion string `json:"conclusion"`
			Status     string `json:"status"`
			Event      string `json:"event"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(ev.RawBody, &payload); err != nil {
		return Match{Skip: true, Reason: "workflow_run payload parse failed"}
	}
	if payload.WorkflowRun.ID == 0 {
		return Match{Skip: true, Reason: "workflow_run payload missing id"}
	}
	if !isFishhawkWorkflowPath(payload.WorkflowRun.Path) {
		return Match{Skip: true,
			Reason: fmt.Sprintf("workflow %q is not the runner action", payload.WorkflowRun.Path)}
	}
	if payload.WorkflowRun.Event != "workflow_dispatch" {
		// We only fired workflow_dispatch invocations of this
		// workflow; a manual / scheduled run on the same file is
		// not Fishhawk's concern.
		return Match{Skip: true,
			Reason: fmt.Sprintf("workflow_run.event %q is not workflow_dispatch", payload.WorkflowRun.Event)}
	}
	if _, ok := failedRunnerConclusions[payload.WorkflowRun.Conclusion]; !ok {
		return Match{Skip: true,
			Reason: fmt.Sprintf("workflow_run.conclusion %q is not a failure",
				payload.WorkflowRun.Conclusion)}
	}
	return Match{
		Action:                MatchActionRunnerActionFailed,
		WorkflowRunID:         payload.WorkflowRun.ID,
		WorkflowRunConclusion: payload.WorkflowRun.Conclusion,
	}
}

// isFishhawkWorkflowPath returns true when the workflow_run's
// `path` matches the runner action file (default `fishhawk.yml`).
// GitHub reports `path` as `.github/workflows/<file>` — strip the
// directory before comparing.
func isFishhawkWorkflowPath(path string) bool {
	const prefix = ".github/workflows/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	file := path[len(prefix):]
	return file == DefaultActionsWorkflowFile
}

// fishhawkAuditCompleteCheckName is the check name Fishhawk
// publishes for its own audit-complete signal (#231). Excluded from
// the CI-retry trigger predicate (#278 / E16) — a failing audit-
// complete means Fishhawk's own audit story is broken (missing
// plan, foreign commit, etc.), and re-running the agent won't fix
// it. Hardcoded here (not imported from auditcheckpublisher) to
// keep the matcher pure + free of upstream-package coupling.
const fishhawkAuditCompleteCheckName = "fishhawk_audit_complete"

// failedCheckRunConclusions enumerates the GitHub check_run terminal
// conclusions that map to the "fail" bucket in
// stagecheck.DeriveState. Sharing the same closed set keeps the
// retry trigger consistent with what the SPA renders as a failing
// check, so a customer can predict from the SPA which failures
// will fire a retry.
//
// Closed set on purpose: a future GitHub-side conclusion (or one
// we just haven't catalogued yet) lands as "not a failure we
// recognize" by default — we don't want to retry on every unknown
// signal.
var failedCheckRunConclusions = map[string]struct{}{
	"failure":         {},
	"timed_out":       {},
	"cancelled":       {},
	"action_required": {},
	"stale":           {},
	"startup_failure": {},
}

// matchCheckRun classifies a `check_run.completed` event for the
// CI-failure retry path (#278 / E16). Pure — the handler in #279
// does the run lookup, retry counting, and workflow_dispatch.
//
// The match is narrow on purpose: `check_run.completed` fires for
// every check on every PR. We only want to retry when:
//
//   - action == "completed" (the terminal signal).
//   - conclusion is in `failedCheckRunConclusions`. success /
//     neutral / skipped don't need our intervention; pending
//     conclusions haven't decided yet.
//   - check name != fishhawk_audit_complete. Retrying won't fix
//     Fishhawk's own audit gaps; that's #229's job.
//   - pull_requests[] is non-empty. Org-level / standalone checks
//     don't have a Fishhawk run to retry against.
//
// Whether the PR is actually Fishhawk-managed (lookup by
// pull_request_url, etc.) is the handler's responsibility — keeps
// matching pure + table-test-friendly.
func matchCheckRun(ev Event) Match {
	if ev.Action != "completed" {
		return Match{Skip: true,
			Reason: fmt.Sprintf("check_run.%s is not the terminal action", ev.Action)}
	}
	var payload struct {
		CheckRun struct {
			Name         string `json:"name"`
			HeadSHA      string `json:"head_sha"`
			Conclusion   string `json:"conclusion"`
			Status       string `json:"status"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_run"`
	}
	if err := json.Unmarshal(ev.RawBody, &payload); err != nil {
		return Match{Skip: true, Reason: "check_run payload parse failed"}
	}
	if _, ok := failedCheckRunConclusions[payload.CheckRun.Conclusion]; !ok {
		return Match{Skip: true,
			Reason: fmt.Sprintf("check_run.conclusion %q is not a failure",
				payload.CheckRun.Conclusion)}
	}
	if payload.CheckRun.Name == fishhawkAuditCompleteCheckName {
		return Match{Skip: true,
			Reason: fmt.Sprintf("check_run %q is fishhawk_audit_complete; not retrying",
				payload.CheckRun.Name)}
	}
	if len(payload.CheckRun.PullRequests) == 0 {
		return Match{Skip: true,
			Reason: "check_run has no pull_requests[]; nothing to retry against"}
	}
	// First-listed PR is the canonical one in GitHub's payload.
	// Multi-PR check_runs (shared branches across forks) are out
	// of scope for v0; the handler can revisit when a customer
	// surfaces the need.
	pr := payload.CheckRun.PullRequests[0]
	if pr.Number <= 0 {
		return Match{Skip: true, Reason: "check_run.pull_requests[0].number is zero"}
	}
	if payload.CheckRun.HeadSHA == "" {
		return Match{Skip: true, Reason: "check_run.head_sha is empty"}
	}
	return Match{
		Action: MatchActionCIFailureRetry,
		CheckRunRef: &CheckRunRef{
			PRNumber:   pr.Number,
			HeadSHA:    payload.CheckRun.HeadSHA,
			CheckName:  payload.CheckRun.Name,
			Conclusion: payload.CheckRun.Conclusion,
		},
	}
}

// GitHubAPI is the slice of githubclient.Client the dispatcher
// uses. Defining it as an interface lets tests substitute a stub
// without standing up an httptest.Server alongside the existing
// dispatcher tests.
type GitHubAPI interface {
	GetWorkflowSpec(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, ref string) (*githubclient.FileContent, error)
	DispatchWorkflow(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, workflowFile, ref string,
		inputs githubclient.DispatchInputs) error
	GetWorkflowRun(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, runID int64) (*githubclient.WorkflowRun, error)
	GetBranchProtection(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, branch string) (*githubclient.BranchProtection, error)
	ListRulesetRequiredChecks(ctx context.Context, installationID int64,
		repo githubclient.RepoRef, branch string) ([]githubclient.RulesetRequiredCheck, error)
}

// IssueNotifier is the slice of issuecomment.Notifier the dispatcher
// uses for the pickup-acknowledgment hook. Defining it as an
// interface keeps the import boundary clean and lets tests substitute
// a recording stub. Nil at the dispatcher means no comment is posted
// (the demo loop pre-#234 posture).
type IssueNotifier interface {
	NotifyPickup(ctx context.Context, runID uuid.UUID, senderLogin string) error
	// NotifyCIRetry posts a comment on the originating issue when
	// the dispatcher fires a CI-failure auto-retry (#279 / E16).
	// Per-attempt dedup via the audit log; failures log but don't
	// unwind the dispatch.
	NotifyCIRetry(ctx context.Context, runID uuid.UUID, parentRunID uuid.UUID, checkName string, attempt, max int) error
}

// ApprovalCommandHandler executes a slash-command approval / reject
// against the run currently associated with an issue (#238). The
// concrete implementation lives in the server package where the
// approval, role, and stage-check repos all live; the dispatcher
// just routes to it.
//
// Implementations are responsible for: finding the awaiting-approval
// stage, authorizing the sender, enforcing blocking checks,
// submitting the approval, advancing the run, and replying on the
// issue with the outcome. Any error returned is best-effort logged
// and not surfaced as a webhook 5xx — slash-command handling is a
// best-effort companion to the SPA flow, not a failure-blocking
// path.
type ApprovalCommandHandler interface {
	HandleApprovalCommand(ctx context.Context, params ApprovalCommandParams) error
}

// ApprovalCommandParams bundles what the handler needs to act on a
// slash-command approval. The dispatcher fills these from the Match
// + Event before calling the handler.
type ApprovalCommandParams struct {
	Repo           string
	IssueNumber    int
	InstallationID int64
	SenderLogin    string
	Decision       MatchAction // approve | reject
	Comment        string      // optional reviewer rationale (the trailing line on the slash command)
}

// Dispatcher orchestrates the I/O side: it consumes a Match,
// fetches the workflow spec, validates it, creates the Run record,
// fires workflow_dispatch, and writes audit entries. The webhook
// HTTP handler calls Handle once dedup has passed.
type Dispatcher struct {
	GitHub GitHubAPI
	Runs   run.Repository
	Audit  audit.Repository
	// Artifacts is consulted by the CI-failure retry handler (#279
	// / E16) to look up the implement-stage pull_request artifact
	// for a run, so the dedup guard can compare head_sha against
	// every Fishhawk run on the PR. Nil leaves the retry path's
	// dedup guard at "no, this head_sha isn't recorded yet" — the
	// audit cap still bounds runaway retries.
	Artifacts artifact.Repository
	Logger    *slog.Logger

	// IssueNotifier posts the pickup-acknowledgment comment back
	// to the triggering issue (#234). Best-effort: failures log
	// but don't unwind the dispatch. Nil leaves the comment-back
	// path off; the run still dispatches.
	IssueNotifier IssueNotifier

	// ApprovalHandler routes /fishhawk approve and /fishhawk
	// reject slash commands (#238). Nil leaves these commands
	// silently skipped — useful in early dev or when the role
	// resolver / approval repo aren't wired yet.
	ApprovalHandler ApprovalCommandHandler

	// DefaultRef is the git ref to dispatch against when the
	// event doesn't carry one (e.g., issues events). Defaults to
	// "main" when empty.
	DefaultRef string

	// ActionsWorkflowFile is the customer's .github/workflows/<file>
	// that runs `fishhawk/runner`. Defaults to "fishhawk.yml".
	ActionsWorkflowFile string

	// Now is the clock used for audit timestamps; defaults to
	// time.Now. Overridable for deterministic tests.
	Now func() time.Time
}

// Handle takes a webhook event after receipt + dedup and runs the
// dispatcher pipeline. Returns nil on every path that shouldn't
// trigger a webhook retry (skip-with-audit-log, dispatch success,
// or upstream-validation failure recorded in the audit log).
// Returns non-nil only on transient infrastructure failures the
// caller should surface as 5xx so GitHub redelivers.
func (d *Dispatcher) Handle(ctx context.Context, ev Event) error {
	now := d.now()

	m := MatchEvent(ev)
	if m.Skip {
		// Skips don't write audit entries — they're noise that
		// would dwarf real audit content. The receiver's
		// structured log line already records every delivery.
		d.logger().LogAttrs(ctx, slog.LevelInfo, "webhook skipped",
			slog.String("event", ev.Type),
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("reason", m.Reason),
		)
		return nil
	}

	// Route by Match.Action. Approve / reject act on an existing
	// run rather than creating a new one — they take a separate
	// path that doesn't fetch the workflow spec or fire
	// workflow_dispatch. The approval handler validates its own
	// repo / installation inputs against what's already persisted.
	switch m.Action {
	case MatchActionApprove, MatchActionReject:
		return d.handleApprovalCommand(ctx, ev, m)
	case MatchActionRunnerActionFailed:
		return d.handleRunnerActionFailed(ctx, ev, m)
	case MatchActionCIFailureRetry:
		return d.handleCIFailureRetry(ctx, ev, m)
	}

	repo, err := parseRepo(ev.Repo)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn, "webhook repo malformed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
		)
		return nil
	}

	ref := d.DefaultRef
	if ref == "" {
		ref = "main"
	}

	// Step 1: fetch the workflow spec at the ref. Failures here
	// are typically "App lacks access" or "file not yet committed";
	// neither is transient.
	specFile, err := d.GitHub.GetWorkflowSpec(ctx, ev.InstallationID, repo, ref)
	if err != nil {
		// If the App can't read the file, we can't dispatch;
		// record the outcome and return nil so GitHub doesn't
		// retry. ErrForbidden / ErrNotFound aren't transient.
		if errors.Is(err, githubclient.ErrForbidden) || errors.Is(err, githubclient.ErrNotFound) {
			d.logSkipFromGitHub(ctx, ev, err)
			return nil
		}
		return fmt.Errorf("dispatcher: get workflow spec: %w", err)
	}

	// Step 2: parse + semantic-validate the spec. A malformed
	// spec is a category-B (constraint/policy) failure: we know
	// the customer's config is bad and we're refusing to run.
	parsed, err := spec.ParseBytes(specFile.Content)
	if err != nil {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA, err, now)
		return nil
	}

	// Step 3: confirm the requested workflow_id exists.
	workflow, ok := parsed.Workflows[m.WorkflowID]
	if !ok {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA,
			fmt.Errorf("workflow_id %q not defined in .fishhawk/workflows.yaml",
				m.WorkflowID), now)
		return nil
	}
	if len(workflow.Stages) == 0 {
		d.writeSpecRejectionAudit(ctx, ev, m, specFile.SHA,
			fmt.Errorf("workflow_id %q has no stages", m.WorkflowID), now)
		return nil
	}

	// Step 3.5: snapshot required-status-check contexts from
	// branch protection + rulesets (ADR-017 / #251). The list is
	// the source of truth for "which CI checks must pass before
	// merge" and is the SPA's "required checks" surface (#256).
	// No protection covering the target ref → refuse the run with
	// a category-B audit; v0 won't dispatch into an ungated repo
	// because the gate-state derivation later in the flow has
	// nothing to derive from.
	snapshot, snapshotErr := d.resolveRequiredChecks(ctx, ev.InstallationID, repo, ref)
	if snapshotErr != nil {
		d.writeProtectionRefusalAudit(ctx, ev, m, specFile.SHA, snapshotErr, now)
		return nil
	}

	// Step 4: create the Run record. workflow_sha is the blob SHA
	// — stable per content, so two refs at the same content hash
	// resolve to the same row's foreign key target.
	//
	// Thread the new run as a follow-up when a prior run on the
	// same (repo, trigger_ref) is still active (#216). The most-
	// recent active run is the parent so reviewers see "follow-up
	// to <short-id>" pointing at the relevant predecessor.
	// Best-effort: a lookup failure logs but doesn't unwind —
	// threading is convenience, not correctness, and we'd rather
	// dispatch unthreaded than refuse the run on a query flap.
	triggerRef := m.TriggerRef
	installationID := ev.InstallationID
	parentRunID := d.findParentRunID(ctx, ev.Repo, triggerRef)
	created, err := d.Runs.CreateRun(ctx, run.CreateRunParams{
		Repo:                   ev.Repo,
		WorkflowID:             m.WorkflowID,
		WorkflowSHA:            specFile.SHA,
		TriggerSource:          m.TriggerSource,
		TriggerRef:             &triggerRef,
		InstallationID:         &installationID,
		ParentRunID:            parentRunID,
		RequiredChecksSnapshot: snapshot,
		// Cache the validated spec bytes so the trace handler's
		// policy re-evaluation reads constraints from storage
		// instead of refetching from GitHub (the refetch path was
		// broken — passed the blob SHA as a ref; see #283).
		WorkflowSpec: specFile.Content,
		// Snapshot the CI-retry cap so the SPA can render
		// "Retry N/M" without re-parsing the spec (#280).
		MaxRetriesSnapshot: workflowMaxRetries(workflow),
	})
	if err != nil {
		return fmt.Errorf("dispatcher: create run: %w", err)
	}

	// Step 5: create one Stage row per stage definition in the
	// spec. All stages start in pending; the first transitions to
	// dispatched when we fire workflow_dispatch below. Subsequent
	// stages move forward as the runner reports completion through
	// the trace upload + state-machine endpoints.
	stages, err := d.createStages(ctx, created.ID, workflow.Stages)
	if err != nil {
		return fmt.Errorf("dispatcher: create stages: %w", err)
	}

	// Step 6: fire workflow_dispatch on the customer-side Actions
	// workflow. Inputs carry run_id, stage_id, and workflow_id so
	// the runner action can call /v0/runs/{run_id}/signing-key with
	// a known identity AND the trace endpoint with a stage UUID.
	actionsFile := d.ActionsWorkflowFile
	if actionsFile == "" {
		actionsFile = DefaultActionsWorkflowFile
	}
	firstStage := stages[0]
	dispatchErr := d.GitHub.DispatchWorkflow(ctx, ev.InstallationID, repo,
		actionsFile, ref, githubclient.DispatchInputs{
			"run_id":      created.ID.String(),
			"stage_id":    firstStage.ID.String(),
			"workflow_id": m.WorkflowID,
			"stage":       firstStage.ExecutorRef, // workflow-spec stage name vs stage UUID
		})

	// Step 7: transition the first stage to dispatched once the
	// dispatch call returned (regardless of success — we tried to
	// move it, the audit row records the outcome). Skip on failure
	// so the next dispatch attempt sees the stage in pending.
	if dispatchErr == nil {
		if _, err := d.Runs.TransitionStage(ctx, firstStage.ID,
			run.StageStateDispatched, nil); err != nil {
			// Don't fail the request — the stage is already
			// associated with the run, the runner will
			// eventually pick it up.
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"transition stage to dispatched failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("stage_id", firstStage.ID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Step 8: audit. Whether dispatch succeeded or not, this
	// delivery produced a Run row, so the audit log gets an entry
	// pinning it to the trigger.
	d.writeDispatchAudit(ctx, ev, m, created, specFile.SHA, dispatchErr, now)

	// Step 8.5: comment back on the triggering issue (#234) so the
	// labeler sees that Fishhawk picked it up. Only fires for
	// issue-triggered runs; the notifier itself is the source of
	// truth on whether to skip (see issuecomment.Notifier).
	// Best-effort: a failure logs at WARN but doesn't unwind the
	// dispatch — the run is in the DB regardless of the comment.
	if d.IssueNotifier != nil && dispatchErr == nil && m.TriggerSource == run.TriggerGitHubIssue {
		if err := d.IssueNotifier.NotifyPickup(ctx, created.ID, ev.Sender); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"pickup comment-back failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("run_id", created.ID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	// Step 9: log the outcome. Without these lines, operators tailing
	// stdout see only `webhook received` + the request log and can't
	// tell whether dispatch actually happened — the audit row is
	// invisible without a query (#186).
	if dispatchErr != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn, "webhook dispatch failed",
			slog.String("event", ev.Type),
			slog.String("action", ev.Action),
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
			slog.String("workflow_id", m.WorkflowID),
			slog.String("run_id", created.ID.String()),
			slog.String("stage_id", firstStage.ID.String()),
			slog.String("error", dispatchErr.Error()),
		)
		// Dispatch failures aren't transient (validation, missing
		// workflow file, etc.), so don't retry — the audit entry
		// is the record.
		return nil
	}
	d.logger().LogAttrs(ctx, slog.LevelInfo, "webhook dispatched",
		slog.String("event", ev.Type),
		slog.String("action", ev.Action),
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("run_id", created.ID.String()),
		slog.String("stage_id", firstStage.ID.String()),
	)
	return nil
}

// findParentRunID returns the most-recent non-terminal run for
// (repo, trigger_ref), or nil when there's no active predecessor
// (#216). Best-effort: a query error logs but returns nil so the
// new run dispatches as a fresh root rather than failing.
//
// "Active" here is "not in a terminal state." A run that finished
// (succeeded / failed / cancelled) doesn't get follow-up children;
// once the lifecycle is closed, the next /fishhawk run on the
// same issue is treated as a fresh root. Open question for v0.x:
// whether a succeeded run should still have follow-ups (revision
// requests on a merged PR). Punt for now.
func (d *Dispatcher) findParentRunID(ctx context.Context, repo, triggerRef string) *uuid.UUID {
	if d.Runs == nil || repo == "" || triggerRef == "" {
		return nil
	}
	tr := triggerRef
	prior, err := d.Runs.ListRuns(ctx, run.ListRunsFilter{
		Repo:       repo,
		TriggerRef: &tr,
		Limit:      10,
	})
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"parent-run lookup failed",
			slog.String("repo", repo),
			slog.String("trigger_ref", triggerRef),
			slog.String("error", err.Error()),
		)
		return nil
	}
	for _, r := range prior {
		if !r.State.IsTerminal() {
			id := r.ID
			return &id
		}
	}
	return nil
}

// handleApprovalCommand routes /fishhawk approve and /fishhawk
// reject slash commands (#238). Best-effort throughout: a missing
// ApprovalHandler logs and returns nil (the comment is silently
// dropped, same posture as a missing IssueNotifier on the pickup
// path). A handler error logs but doesn't surface as a webhook 5xx
// — slash-command approval is a companion to the SPA flow, not the
// only path. The reviewer can still go to the dashboard.
func (d *Dispatcher) handleApprovalCommand(ctx context.Context, ev Event, m Match) error {
	if d.ApprovalHandler == nil {
		d.logger().LogAttrs(ctx, slog.LevelInfo,
			"slash-command approval skipped: no handler wired",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("action", string(m.Action)),
		)
		return nil
	}
	if m.IssueRef == nil || m.IssueRef.Number == 0 {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval skipped: missing issue number",
			slog.String("delivery_id", ev.DeliveryID),
		)
		return nil
	}
	if err := d.ApprovalHandler.HandleApprovalCommand(ctx, ApprovalCommandParams{
		Repo:           ev.Repo,
		IssueNumber:    m.IssueRef.Number,
		InstallationID: ev.InstallationID,
		SenderLogin:    ev.Sender,
		Decision:       m.Action,
		Comment:        m.CommentBody,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("action", string(m.Action)),
			slog.String("repo", ev.Repo),
			slog.Int("issue", m.IssueRef.Number),
			slog.String("error", err.Error()),
		)
	}
	return nil
}

// handleRunnerActionFailed flips the matched stage to failed-C
// when the customer-side runner action errors out (#243). Best-
// effort: errors log but don't surface as webhook 5xx — the
// dispatch watchdog (E8.4 #158) is the slow-but-eventual fallback,
// so a flap here just delays the transition rather than losing it.
//
// The matching strategy uses the workflow_run's
// `workflow_dispatch.inputs` echoed back by the actions API. We
// fired the original dispatch with `run_id` and `stage_id`
// inputs (per `Dispatcher.Handle` step 6); GitHub stores them
// verbatim, and `GetWorkflowRun` returns them so we can recover
// the Fishhawk stage without a separate matching scheme.
//
// Idempotency: re-deliveries of the same workflow_run.completed
// hit the same stage. `run.FailStage` is itself idempotent on
// already-failed stages (same-state transition is a no-op), so
// the second handle is harmless.
func (d *Dispatcher) handleRunnerActionFailed(ctx context.Context, ev Event, m Match) error {
	repo, err := parseRepo(ev.Repo)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"runner_action_failed: repo parse failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
		)
		return nil
	}

	wfRun, err := d.GitHub.GetWorkflowRun(ctx, ev.InstallationID, repo, m.WorkflowRunID)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"runner_action_failed: get workflow run failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.Int64("workflow_run_id", m.WorkflowRunID),
			slog.String("error", err.Error()),
		)
		return nil
	}

	stageIDStr, ok := wfRun.Inputs["stage_id"]
	if !ok || stageIDStr == "" {
		d.logger().LogAttrs(ctx, slog.LevelInfo,
			"runner_action_failed: workflow_run has no stage_id input — not a Fishhawk dispatch",
			slog.String("delivery_id", ev.DeliveryID),
			slog.Int64("workflow_run_id", m.WorkflowRunID),
		)
		return nil
	}
	stageID, err := uuid.Parse(stageIDStr)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"runner_action_failed: stage_id input is not a UUID",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("stage_id", stageIDStr),
			slog.String("error", err.Error()),
		)
		return nil
	}

	reason := fmt.Sprintf("runner action workflow_run #%d concluded as %s",
		m.WorkflowRunID, m.WorkflowRunConclusion)
	if _, err := run.FailStage(ctx, d.Runs, stageID, run.FailureC, reason); err != nil {
		// Stage may have already advanced (trace upload landed
		// before this webhook arrived) — that's fine, fail-stage
		// is idempotent on already-terminal stages. Log other
		// errors at warn but don't surface as 5xx.
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"runner_action_failed: fail-stage failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	d.logger().LogAttrs(ctx, slog.LevelInfo,
		"runner_action_failed: stage transitioned to failed-C",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.Int64("workflow_run_id", m.WorkflowRunID),
		slog.String("conclusion", m.WorkflowRunConclusion),
		slog.String("stage_id", stageID.String()),
		slog.String("workflow_run_url", wfRun.HTMLURL),
	)
	return nil
}

// handleCIFailureRetry creates a follow-up implement run when a
// required CI check fails on a Fishhawk-managed PR (#279 / E16).
// Best-effort throughout — every skip path emits a structured log
// line so an operator can trace why the auto-retry didn't fire.
//
// Algorithm:
//
//  1. Find the most-recent run on the PR URL via runs.pull_request_url
//     (#216). Skip if no Fishhawk run touches this PR.
//
//  2. Skip if the failing check isn't in the parent's
//     required_checks_snapshot (#251). A non-required check failing
//     isn't a merge blocker, so it isn't a retry trigger.
//
//  3. Skip if a Fishhawk run already has this head_sha. The runner's
//     fresh commit produces a new head_sha each retry; an event for
//     a SHA we already wrote a run against is a redelivery / racing
//     event.
//
//  4. Read on_ci_failure.max_retries from the parent's cached
//     workflow spec (#283 / #277), defaulting to DefaultMaxRetries
//     when the block is absent. Explicit max_retries: 0 disables
//     auto-retry — emit ci_retry_exhausted with the
//     "opt-out" reason.
//
//  5. Cap check: when parent.RetryAttempt >= maxRetries, emit
//     ci_retry_exhausted with the "cap" reason and stop.
//
//  6. Create the follow-up run with ParentRunID = parent.ID,
//     RetryAttempt = parent.RetryAttempt + 1, reusing the parent's
//     workflow_id / workflow_sha / installation_id / required-checks
//     snapshot / cached spec.
//
//  7. Create stages for the retry — variant A from the issue body:
//     skip plan stages. The implement stage's prompt builder
//     (server/prompt.go::loadApprovedPlanForRun) walks ParentRunID
//     to find the most-recent approved plan, so the retry runs
//     against the parent's plan without re-prompting.
//
//  8. Fire workflow_dispatch on the implement stage.
//
//  9. Audit (ci_failure_retry_dispatched) + best-effort notify the
//     originating issue.
func (d *Dispatcher) handleCIFailureRetry(ctx context.Context, ev Event, m Match) error {
	if m.CheckRunRef == nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: missing CheckRunRef",
			slog.String("delivery_id", ev.DeliveryID))
		return nil
	}
	ref := m.CheckRunRef
	now := d.now()

	// Step 1: find the parent run on this PR.
	parent, ok := d.findRunForCIRetry(ctx, ev.Repo, ref.PRNumber)
	if !ok {
		d.logger().LogAttrs(ctx, slog.LevelDebug,
			"ci_failure_retry: PR not Fishhawk-managed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
			slog.Int("pr_number", ref.PRNumber))
		return nil
	}

	// Step 2: only fire when the failing check is one the run's
	// branch-protection snapshot says is required.
	if !checkInSnapshot(parent, ref.CheckName) {
		d.logger().LogAttrs(ctx, slog.LevelDebug,
			"ci_failure_retry: check is not in required snapshot",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("run_id", parent.ID.String()),
			slog.String("check_name", ref.CheckName))
		return nil
	}

	// Step 3: dedup against existing runs on this head_sha.
	dup, err := d.runOnHeadSHAExists(ctx, ev.Repo, ref.PRNumber, ref.HeadSHA)
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: dedup lookup failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
		return nil
	}
	if dup {
		d.logger().LogAttrs(ctx, slog.LevelInfo,
			"ci_failure_retry: a Fishhawk run already exists on this head_sha",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("head_sha", ref.HeadSHA))
		return nil
	}

	// Step 4: resolve the retry cap from the cached spec.
	workflow, maxRetries, ok := d.resolveRetryPolicy(ctx, parent)
	if !ok {
		// The dispatch path here is best-effort. If we can't read
		// the spec, refuse the retry to avoid a runaway loop —
		// logged loudly so the operator sees it.
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: cannot resolve retry policy from cached spec",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("run_id", parent.ID.String()))
		return nil
	}

	// Step 5: cap check (covers both the max-hit and explicit
	// max_retries:0 opt-out cases; max_retries:0 means cap is 0,
	// any RetryAttempt >= 0 trips the check on the original).
	if parent.RetryAttempt >= maxRetries {
		d.writeCIRetryExhaustedAudit(ctx, ev, parent, ref, maxRetries, now)
		return nil
	}

	// Step 6: create the follow-up run.
	triggerRef := ""
	if parent.TriggerRef != nil {
		triggerRef = *parent.TriggerRef
	}
	installationID := int64(0)
	if parent.InstallationID != nil {
		installationID = *parent.InstallationID
	}
	parentID := parent.ID
	params := run.CreateRunParams{
		Repo:                   parent.Repo,
		WorkflowID:             parent.WorkflowID,
		WorkflowSHA:            parent.WorkflowSHA,
		TriggerSource:          parent.TriggerSource,
		ParentRunID:            &parentID,
		RequiredChecksSnapshot: parent.RequiredChecksSnapshot,
		WorkflowSpec:           parent.WorkflowSpec,
		RetryAttempt:           parent.RetryAttempt + 1,
		// Carry the parent's snapshotted cap forward so a chained
		// retry chain sees the same N/M values on every row (#280).
		MaxRetriesSnapshot: parent.MaxRetriesSnapshot,
	}
	if triggerRef != "" {
		params.TriggerRef = &triggerRef
	}
	if installationID != 0 {
		params.InstallationID = &installationID
	}
	child, err := d.Runs.CreateRun(ctx, params)
	if err != nil {
		return fmt.Errorf("dispatcher: create retry run: %w", err)
	}

	// Step 7: create stages — skip plan. The retry's implement
	// stage prompt walks ParentRunID to find the original plan.
	retryStages := filterOutPlanStages(workflow.Stages)
	if len(retryStages) == 0 {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: no non-plan stages to retry against",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("run_id", child.ID.String()))
		return nil
	}
	stages, err := d.createStages(ctx, child.ID, retryStages)
	if err != nil {
		return fmt.Errorf("dispatcher: create retry stages: %w", err)
	}

	// Step 8: fire workflow_dispatch on the first retry stage.
	repo, err := parseRepo(ev.Repo)
	if err != nil {
		return fmt.Errorf("dispatcher: parse repo: %w", err)
	}
	dispatchRef := d.DefaultRef
	if dispatchRef == "" {
		dispatchRef = "main"
	}
	actionsFile := d.ActionsWorkflowFile
	if actionsFile == "" {
		actionsFile = DefaultActionsWorkflowFile
	}
	firstStage := stages[0]
	dispatchErr := d.GitHub.DispatchWorkflow(ctx, installationID, repo,
		actionsFile, dispatchRef, githubclient.DispatchInputs{
			"run_id":      child.ID.String(),
			"stage_id":    firstStage.ID.String(),
			"workflow_id": parent.WorkflowID,
			"stage":       firstStage.ExecutorRef,
		})
	if dispatchErr == nil {
		if _, err := d.Runs.TransitionStage(ctx, firstStage.ID,
			run.StageStateDispatched, nil); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"ci_failure_retry: transition stage to dispatched failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("stage_id", firstStage.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	// Step 9: audit.
	d.writeCIRetryDispatchedAudit(ctx, ev, parent, child, ref, maxRetries, dispatchErr, now)

	// Notify the originating issue. Best-effort — a comment failure
	// logs but doesn't unwind the dispatch.
	if d.IssueNotifier != nil && dispatchErr == nil {
		if err := d.IssueNotifier.NotifyCIRetry(ctx, child.ID, parent.ID,
			ref.CheckName, child.RetryAttempt, maxRetries); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"ci_failure_retry: comment-back failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("run_id", child.ID.String()),
				slog.String("error", err.Error()))
		}
	}

	if dispatchErr != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: workflow_dispatch failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("run_id", child.ID.String()),
			slog.String("error", dispatchErr.Error()))
	} else {
		d.logger().LogAttrs(ctx, slog.LevelInfo,
			"ci_failure_retry: dispatched retry run",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("parent_run_id", parent.ID.String()),
			slog.String("run_id", child.ID.String()),
			slog.Int("retry_attempt", child.RetryAttempt),
			slog.Int("max_retries", maxRetries),
			slog.String("check_name", ref.CheckName),
			slog.String("head_sha", ref.HeadSHA),
		)
	}
	return nil
}

// pullRequestURLFor builds the canonical github.com PR URL for a
// (repo, number) tuple. The runner stamps this exact shape when it
// opens the PR (#216 / #206); using the same builder keeps the
// ListRuns lookup byte-equal.
func pullRequestURLFor(repo string, prNumber int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, prNumber)
}

// findRunForCIRetry returns the most-recent run on the given PR. The
// list is ordered by created_at desc + id desc (per the SQL); a
// non-cancelled run at index 0 is the canonical parent for a retry.
// When the run is cancelled, the chain is "closed" — refuse to
// retry on top of a manually-stopped lineage.
func (d *Dispatcher) findRunForCIRetry(ctx context.Context, repo string, prNumber int) (*run.Run, bool) {
	if d.Runs == nil || repo == "" || prNumber <= 0 {
		return nil, false
	}
	prURL := pullRequestURLFor(repo, prNumber)
	runs, err := d.Runs.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          25,
	})
	if err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"ci_failure_retry: list runs failed",
			slog.String("repo", repo),
			slog.Int("pr_number", prNumber),
			slog.String("error", err.Error()))
		return nil, false
	}
	if len(runs) == 0 {
		return nil, false
	}
	parent := runs[0]
	if parent.State == run.StateCancelled {
		// Lineage was manually stopped; don't restart it.
		return nil, false
	}
	return parent, true
}

// checkInSnapshot reports whether the run's required-checks
// snapshot includes `name`. Empty snapshot (legacy run pre-#251 or
// CLI / UI flow) returns false — without protection metadata we
// don't know what's required, so we refuse to retry rather than
// trigger on every check.
func checkInSnapshot(r *run.Run, name string) bool {
	if r.RequiredChecksSnapshot == nil {
		return false
	}
	for _, c := range r.RequiredChecksSnapshot.Contexts {
		if c == name {
			return true
		}
	}
	return false
}

// runOnHeadSHAExists reports whether some Fishhawk run on this PR
// already records the given head_sha — either as a direct PR
// artifact head or as an ancestor in the chain. Used by the dedup
// guard so a redelivery on the same head_sha doesn't spawn a second
// retry.
func (d *Dispatcher) runOnHeadSHAExists(ctx context.Context, repo string, prNumber int, headSHA string) (bool, error) {
	if d.Runs == nil || d.Artifacts == nil || headSHA == "" {
		return false, nil
	}
	prURL := pullRequestURLFor(repo, prNumber)
	runs, err := d.Runs.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          25,
	})
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		stages, err := d.Runs.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return false, err
		}
		for _, s := range stages {
			if s.Type != run.StageTypeImplement {
				continue
			}
			arts, err := d.Artifacts.ListForStage(ctx, s.ID)
			if err != nil {
				return false, err
			}
			for _, a := range arts {
				if a.Kind != artifact.KindPullRequest {
					continue
				}
				if sha := decodeArtifactHeadSHA(a.Content); sha != "" && sha == headSHA {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// decodeArtifactHeadSHA pulls head_sha out of a pull_request
// artifact's JSON content. Mirrors the helper in
// auditcheckpublisher; duplicated here to keep the dispatcher
// import-free of that package.
func decodeArtifactHeadSHA(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	var body struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return ""
	}
	return body.HeadSHA
}

// resolveRetryPolicy reads on_ci_failure.max_retries from the
// parent's cached spec. Returns the workflow definition so the
// caller can use its stages list. ok=false signals "couldn't
// resolve" — the caller refuses the retry rather than guessing.
func (*Dispatcher) resolveRetryPolicy(_ context.Context, parent *run.Run) (spec.Workflow, int, bool) {
	if len(parent.WorkflowSpec) == 0 {
		return spec.Workflow{}, 0, false
	}
	parsed, err := spec.ParseBytes(parent.WorkflowSpec)
	if err != nil {
		return spec.Workflow{}, 0, false
	}
	wf, ok := parsed.Workflows[parent.WorkflowID]
	if !ok {
		return spec.Workflow{}, 0, false
	}
	max := spec.DefaultMaxRetries
	if wf.OnCIFailure != nil {
		max = wf.OnCIFailure.MaxRetries
	}
	return wf, max, true
}

// workflowMaxRetries returns the spec's on_ci_failure.max_retries
// value, defaulting to spec.DefaultMaxRetries when the block is
// absent. Used by the run-create path to snapshot the cap on the
// runs row (#280) so the SPA can render "Retry N/M" without
// re-parsing the spec.
func workflowMaxRetries(wf spec.Workflow) int {
	if wf.OnCIFailure != nil {
		return wf.OnCIFailure.MaxRetries
	}
	return spec.DefaultMaxRetries
}

// filterOutPlanStages returns the stages list with all `plan` types
// removed. Retry runs inherit the parent's plan via parent_run_id
// (resolved in server/prompt.go::loadApprovedPlanForRun) so the
// retry doesn't need its own plan stage row.
func filterOutPlanStages(in []spec.Stage) []spec.Stage {
	out := make([]spec.Stage, 0, len(in))
	for _, s := range in {
		if s.Type == spec.StageTypePlan {
			continue
		}
		out = append(out, s)
	}
	return out
}

// writeCIRetryDispatchedAudit appends a chained audit entry tying
// the retry dispatch to the parent run + the failing check.
func (d *Dispatcher) writeCIRetryDispatchedAudit(ctx context.Context, ev Event, parent, child *run.Run,
	ref *CheckRunRef, maxRetries int, dispatchErr error, now time.Time) {
	systemKind := audit.ActorKind("system")
	outcome := "dispatched"
	if dispatchErr != nil {
		outcome = "dispatch_failed"
	}
	payload := map[string]any{
		"event":         ev.Type,
		"delivery_id":   ev.DeliveryID,
		"repo":          ev.Repo,
		"parent_run_id": parent.ID.String(),
		"child_run_id":  child.ID.String(),
		"check_name":    ref.CheckName,
		"conclusion":    ref.Conclusion,
		"head_sha":      ref.HeadSHA,
		"pr_number":     ref.PRNumber,
		"retry_attempt": child.RetryAttempt,
		"max_retries":   maxRetries,
		"outcome":       outcome,
	}
	if dispatchErr != nil {
		payload["error"] = dispatchErr.Error()
	}
	body, _ := json.Marshal(payload)
	if _, err := d.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        child.ID,
		Timestamp:    now,
		Category:     "ci_failure_retry_dispatched",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("github-webhook"),
		Payload:      body,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "ci_failure_retry: audit append failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
	}
}

// writeCIRetryExhaustedAudit records the cap-hit case so an
// operator can see "we tried, this is why we stopped." Chains
// against the parent run since there's no child to attribute to.
func (d *Dispatcher) writeCIRetryExhaustedAudit(ctx context.Context, ev Event, parent *run.Run,
	ref *CheckRunRef, maxRetries int, now time.Time) {
	systemKind := audit.ActorKind("system")
	payload, _ := json.Marshal(map[string]any{
		"event":         ev.Type,
		"delivery_id":   ev.DeliveryID,
		"repo":          ev.Repo,
		"run_id":        parent.ID.String(),
		"check_name":    ref.CheckName,
		"conclusion":    ref.Conclusion,
		"head_sha":      ref.HeadSHA,
		"pr_number":     ref.PRNumber,
		"retry_attempt": parent.RetryAttempt,
		"max_retries":   maxRetries,
	})
	if _, err := d.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        parent.ID,
		Timestamp:    now,
		Category:     "ci_retry_exhausted",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("github-webhook"),
		Payload:      payload,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "ci_failure_retry: exhausted audit failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()))
	}
	d.logger().LogAttrs(ctx, slog.LevelInfo, "ci_failure_retry: retry cap reached; not dispatching",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("run_id", parent.ID.String()),
		slog.Int("retry_attempt", parent.RetryAttempt),
		slog.Int("max_retries", maxRetries),
	)
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// logSkipFromGitHub writes a structured log line and an audit
// entry when GitHub refuses our access for a delivery. Distinct
// from MatchEvent's "skip" path (which doesn't audit) because an
// access failure represents a real configuration problem we want
// surfaced in the audit log.
func (d *Dispatcher) logSkipFromGitHub(ctx context.Context, ev Event, err error) {
	d.logger().LogAttrs(ctx, slog.LevelWarn, "webhook dispatch refused by github",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("error", err.Error()),
	)
	// No Run row created → no run_id to associate the audit entry
	// with → we don't write one. The structured log line is the
	// trace of record for these.
}

// writeSpecRejectionAudit logs a rejection event tied to the trigger.
// We don't have a Run row (we refused to create one), so we use the
// AppendChained variant scoped to a synthetic run UUID derived from
// the delivery ID. v0.x will introduce a "rejected dispatches"
// table that doesn't pretend to be a run.
//
// For now: log only; no audit row. Skip-with-reason at this layer
// belongs in a separate ledger from the per-run audit log.
func (d *Dispatcher) writeSpecRejectionAudit(ctx context.Context, ev Event, m Match,
	specSHA string, validationErr error, _ time.Time) {
	d.logger().LogAttrs(ctx, slog.LevelWarn, "webhook dispatch rejected",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("workflow_sha", specSHA),
		slog.String("error", validationErr.Error()),
	)
}

// writeDispatchAudit appends a chained audit entry tying the
// trigger to the created run. dispatchErr is non-nil when GitHub
// rejected the workflow_dispatch — the entry records the failure
// so a future re-dispatch can be authorized against the audit log.
func (d *Dispatcher) writeDispatchAudit(ctx context.Context, ev Event, m Match,
	created *run.Run, specSHA string, dispatchErr error, now time.Time) {
	systemKind := audit.ActorKind("system")

	outcome := "dispatched"
	if dispatchErr != nil {
		outcome = "dispatch_failed"
	}
	payload := map[string]any{
		"event":          ev.Type,
		"delivery_id":    ev.DeliveryID,
		"action":         ev.Action,
		"sender":         ev.Sender,
		"workflow_id":    m.WorkflowID,
		"workflow_sha":   specSHA,
		"trigger_ref":    m.TriggerRef,
		"trigger_source": string(m.TriggerSource),
		"outcome":        outcome,
	}
	if dispatchErr != nil {
		payload["error"] = dispatchErr.Error()
	}
	body, _ := json.Marshal(payload)

	if _, err := d.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        created.ID,
		Timestamp:    now,
		Category:     "run_dispatched",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("github-webhook"),
		Payload:      body,
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelError, "audit append failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("error", err.Error()),
		)
	}
}

// errNoBranchProtection signals the target branch has neither
// classic protection nor a ruleset that contributes required-status
// checks. The dispatcher refuses to create a run in that case
// (ADR-017 / #251) — the gate-state derivation later in the flow
// has nothing to derive from. Customer-facing fix: configure branch
// protection on the default branch, then re-trigger.
var errNoBranchProtection = errors.New("no branch protection or ruleset covers the target ref")

// errProtectionScopeMissing signals the App's installation lacks
// the `administration: read` scope (ADR-017 / #252). Distinct from
// "no protection" so audit logs can name the operator-side fix
// (re-install the App to accept the new scope) precisely.
var errProtectionScopeMissing = errors.New("app installation missing administration:read; re-install to accept the new scope")

// resolveRequiredChecks queries classic branch protection +
// rulesets and returns the union of required-status-check contexts
// as a snapshot ready to persist on the run row (#251). Returns
// errNoBranchProtection when neither surface contributes a context;
// errProtectionScopeMissing when the App lacks the new permission;
// any other error is a transport / GitHub-side issue and surfaces
// to the caller as-is so step 3.5 can audit and refuse.
func (d *Dispatcher) resolveRequiredChecks(ctx context.Context, installationID int64,
	repo githubclient.RepoRef, branch string) (*run.RequiredChecksSnapshot, error) {
	var contexts []string
	var sources []string
	seen := make(map[string]struct{})
	add := func(c string) {
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		contexts = append(contexts, c)
	}

	classic, classicErr := d.GitHub.GetBranchProtection(ctx, installationID, repo, branch)
	switch {
	case classicErr == nil:
		if len(classic.RequiredStatusCheckContexts) > 0 {
			sources = append(sources, "branch_protection")
			for _, c := range classic.RequiredStatusCheckContexts {
				add(c)
			}
		}
	case errors.Is(classicErr, githubclient.ErrNotFound):
		// Branch isn't protected by the classic API — fall through
		// to rulesets.
	case errors.Is(classicErr, githubclient.ErrForbidden):
		return nil, errProtectionScopeMissing
	default:
		return nil, fmt.Errorf("get branch protection: %w", classicErr)
	}

	rulesets, rulesetsErr := d.GitHub.ListRulesetRequiredChecks(ctx, installationID, repo, branch)
	switch {
	case rulesetsErr == nil:
		for _, r := range rulesets {
			if len(r.Contexts) == 0 {
				continue
			}
			sources = append(sources, fmt.Sprintf("ruleset:%d", r.RulesetID))
			for _, c := range r.Contexts {
				add(c)
			}
		}
	case errors.Is(rulesetsErr, githubclient.ErrForbidden):
		return nil, errProtectionScopeMissing
	default:
		// 404 from the rulesets endpoint is unusual but not fatal —
		// some self-hosted GHES versions don't expose it. Fall
		// through with whatever classic protection contributed.
		if !errors.Is(rulesetsErr, githubclient.ErrNotFound) {
			return nil, fmt.Errorf("list rulesets: %w", rulesetsErr)
		}
	}

	if len(contexts) == 0 {
		return nil, errNoBranchProtection
	}
	return &run.RequiredChecksSnapshot{
		Contexts: contexts,
		Sources:  sources,
	}, nil
}

// writeProtectionRefusalAudit logs the dispatcher's refusal to
// create a run when no branch protection covers the target ref
// (#251). No Run row exists, so we can only log — the v0.x rejected-
// dispatches table referenced in writeSpecRejectionAudit will pick
// this up too.
func (d *Dispatcher) writeProtectionRefusalAudit(ctx context.Context, ev Event, m Match,
	specSHA string, refusalErr error, _ time.Time) {
	d.logger().LogAttrs(ctx, slog.LevelWarn, "webhook dispatch refused: branch protection",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("workflow_sha", specSHA),
		slog.String("error", refusalErr.Error()),
	)
}

// createStages translates the workflow spec's stage definitions
// into Stage rows (in StagePending). Returns the created stages
// in spec order so callers can address the first one.
//
// Mapping decisions:
//   - sequence is the position in the spec's stages array (0-based).
//   - executorKind comes from spec.Executor: agent → ExecutorAgent,
//     human → ExecutorHuman.
//   - executorRef is the agent name for agent stages and a
//     conventional "human" string for human stages — the field is
//     non-nullable in the DB schema, and we never read it for human
//     stages. v0.x can swap to using the role name once approvals
//     are wired (E3.5 / #45).
func (d *Dispatcher) createStages(ctx context.Context, runID uuid.UUID, defs []spec.Stage) ([]*run.Stage, error) {
	out := make([]*run.Stage, 0, len(defs))
	for i, def := range defs {
		execKind, execRef := mapExecutor(def)
		params := run.CreateStageParams{
			RunID:        runID,
			Sequence:     i,
			Type:         run.StageType(def.Type),
			ExecutorKind: execKind,
			ExecutorRef:  execRef,
		}
		// Persist the first approval gate's SLA string verbatim so
		// the SLA ticker (E3.11) can scan for timeouts without
		// re-parsing the spec at every tick. v0 stages typically
		// carry one approval gate; if multiple are configured we
		// take the first non-empty SLA. Empty SLA → leave nil
		// (means "no timeout").
		if sla := firstApprovalSLA(def.Gates); sla != "" {
			params.GateSLA = &sla
		}
		// Persist whether the stage's spec demands an approval gate
		// so the trace upload handler can pick the right
		// post-upload transition (#207). Plan + review have one;
		// implement does not.
		params.RequiresApproval = hasApprovalGate(def.Gates)
		// Persist the primary gate's full shape so the review-stage
		// UI (and future check-state ingestion) can read
		// blocking_checks + approvers without re-parsing the spec
		// (#213). Primary = first approval gate, else first check
		// gate, else nil.
		params.Gate = primaryGate(def.Gates)
		stage, err := d.Runs.CreateStage(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("create stage %d (%s): %w", i, def.ID, err)
		}
		out = append(out, stage)
	}
	return out, nil
}

// firstApprovalSLA returns the first non-empty SLA from any
// approval gate in the stage's Gates list. Returns "" when no gate
// has an SLA (or no approval gate exists).
func firstApprovalSLA(gates []spec.Gate) string {
	for _, g := range gates {
		if g.Type == spec.GateTypeApproval && g.SLA != "" {
			return g.SLA
		}
	}
	return ""
}

// hasApprovalGate reports whether any of the stage's gates is the
// approval type. The trace upload handler reads this through
// stages.requires_approval to pick the right post-upload state
// (gated → awaiting_approval, gateless → succeeded). #207.
func hasApprovalGate(gates []spec.Gate) bool {
	for _, g := range gates {
		if g.Type == spec.GateTypeApproval {
			return true
		}
	}
	return false
}

// primaryGate picks the gate to persist on the stages row (#213).
// Approval gates win over check gates so the review-stage UI can
// always reach the approvers when they're declared. Returns nil for
// stages with no gate.
func primaryGate(gates []spec.Gate) *run.Gate {
	if len(gates) == 0 {
		return nil
	}
	g := pickPrimaryGate(gates)
	if g == nil {
		return nil
	}
	out := &run.Gate{
		Kind: run.GateKind(g.Type),
	}
	if g.Approvers != nil {
		out.Approvers = &run.GateApprovers{
			AnyOf: g.Approvers.AnyOf,
			AllOf: g.Approvers.AllOf,
		}
	}
	return out
}

// pickPrimaryGate is the inner choice — first approval gate if any,
// else first check gate. Split out from primaryGate so the policy
// is unit-testable without a run.Gate round-trip.
func pickPrimaryGate(gates []spec.Gate) *spec.Gate {
	for i := range gates {
		if gates[i].Type == spec.GateTypeApproval {
			return &gates[i]
		}
	}
	for i := range gates {
		if gates[i].Type == spec.GateTypeCheck {
			return &gates[i]
		}
	}
	return nil
}

// mapExecutor projects a spec.Executor onto the run-package
// executor enum. Per the schema, exactly one of Agent / Human is
// set; we trust that here rather than reasserting it.
func mapExecutor(s spec.Stage) (run.ExecutorKind, string) {
	if s.Executor.Human {
		// Stage is human-driven. ExecutorRef is informational
		// only for human stages; the run state machine doesn't
		// dispatch them to a runner.
		return run.ExecutorHuman, "human"
	}
	return run.ExecutorAgent, s.Executor.Agent
}

// parseRepo splits "owner/name" into a githubclient.RepoRef. Empty
// or malformed inputs return an error so the caller can skip with a
// useful reason rather than firing a request at api.github.com that
// will 404.
func parseRepo(s string) (githubclient.RepoRef, error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("malformed repo %q", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

func stringPtr(s string) *string { return &s }
