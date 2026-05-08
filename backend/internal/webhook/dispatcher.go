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
// new work.
type MatchAction string

// MatchAction values.
const (
	MatchActionRun     MatchAction = "run"
	MatchActionApprove MatchAction = "approve"
	MatchActionReject  MatchAction = "reject"
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
}

// IssueNotifier is the slice of issuecomment.Notifier the dispatcher
// uses for the pickup-acknowledgment hook. Defining it as an
// interface keeps the import boundary clean and lets tests substitute
// a recording stub. Nil at the dispatcher means no comment is posted
// (the demo loop pre-#234 posture).
type IssueNotifier interface {
	NotifyPickup(ctx context.Context, runID uuid.UUID, senderLogin string) error
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
	Logger *slog.Logger

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

	// Step 4: create the Run record. workflow_sha is the blob SHA
	// — stable per content, so two refs at the same content hash
	// resolve to the same row's foreign key target.
	triggerRef := m.TriggerRef
	installationID := ev.InstallationID
	created, err := d.Runs.CreateRun(ctx, run.CreateRunParams{
		Repo:           ev.Repo,
		WorkflowID:     m.WorkflowID,
		WorkflowSHA:    specFile.SHA,
		TriggerSource:  m.TriggerSource,
		TriggerRef:     &triggerRef,
		InstallationID: &installationID,
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
		Kind:           run.GateKind(g.Type),
		BlockingChecks: g.BlockingChecks,
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
