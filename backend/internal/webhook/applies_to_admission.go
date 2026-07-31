package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/appliesto"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// refusedByAppliesTo reports whether a NEW webhook-triggered run must be refused
// because the dispatched workflow's `applies_to` routing declaration does not
// accept this change (E53.10 / #2361). It is the dispatcher-side counterpart to
// the server's checkAppliesTo — the dispatcher creates runs directly (bypassing
// handleCreateRun), so an operator's routing declaration would otherwise
// silently not hold for webhook-triggered runs. Structurally the sibling of
// refusedByBlockingBudget.
//
// It evaluates ONLY the admission-phase criteria (`labels`, `trigger`) through
// the shared appliesto core, against the FORGE-authoritative labels captured on
// m.IssueRef — better provenance than the caller-attested labels the API path
// receives. The deferred `paths` criterion is NOT evaluated here: a
// webhook-created run reaches the plan gate at handleShipPlan like any other
// run, and carries no run-scoped override entry, so `paths` is already enforced
// for it there. A workflow with no `applies_to`, or a paths-only declaration
// that does not constrain admission, returns false (admit).
//
// FAIL-CLOSED: a Match error refuses (never `if err != nil { admit }` by
// analogy with the package's advisory fail-open sweeps), and an unsatisfied
// criterion refuses.
//
// The audit append and the issue comment are BOTH BEST-EFFORT and must NEVER
// soften the refusal — the decision already went the SAFE way (no run row), so
// an audit problem or a nil/failing notifier cannot invert it. This is the
// deliberate asymmetry to the server's OVERRIDE grant, which fails CLOSED on the
// same failure because it records an EXCEPTION being made rather than the safe
// outcome. Both fail-safe, opposite failure directions.
func (d *Dispatcher) refusedByAppliesTo(ctx context.Context, ev Event, m Match, workflow spec.Workflow, parsed *spec.Spec, scope forge.CredentialScope, specSHA string, now time.Time) bool {
	if workflow.AppliesTo == nil {
		// No declaration at all: nothing to enforce.
		return false
	}
	sub, constrains := appliesto.PhasePredicate(*workflow.AppliesTo, appliesto.PhaseAdmission)
	if !constrains {
		// The declaration constrains only the deferred `paths` criterion,
		// which does not gate admission — the server plan gate enforces it once
		// a plan exists. Admit here.
		return false
	}

	var labels []string
	if m.IssueRef != nil {
		labels = m.IssueRef.IssueLabels
	}
	c := appliesto.AdmissionChange(string(m.TriggerSource), labels)
	matched, matchErr := sub.Match(c)
	if matchErr == nil && matched {
		return false
	}

	// Refuse. Build the rejection through the ONE renderer with SeamWebhook so
	// the message reads identically to the server seam but names amending the
	// declaration / re-starting through the API rather than an override the
	// webhook path cannot carry.
	rj := appliesto.Rejection{
		WorkflowID:    m.WorkflowID,
		TriggerSource: string(m.TriggerSource),
		Satisfying:    appliesto.SatisfyingWorkflows(parsed, c, appliesto.PhaseAdmission),
		MatchErr:      matchErr,
		Phase:         appliesto.PhaseAdmission,
		Seam:          appliesto.SeamWebhook,
		// Both webhook entry points (matchIssue, matchIssueComment) are
		// issue-triggered, so an empty label set is an UNLABELLED ISSUE, not an
		// issue-less run — render the "carries no labels at all" sentence.
		IssueContextPresent: true,
	}
	if matchErr == nil {
		crit, required, observed, ok := appliesto.FirstFailingCriterion(sub, c)
		if ok {
			rj.Criterion = crit
			rj.Required = required
			rj.Observed = observed
			rj.ObservedLabel = crit
			rj.AbsentIssueContext = crit == "labels" && len(labels) == 0
		}
	}
	message := appliesto.RenderRejection(rj)

	d.logger().LogAttrs(ctx, slog.LevelWarn,
		"webhook dispatch refused: applies_to routing declaration not satisfied",
		slog.String("delivery_id", ev.DeliveryID),
		slog.String("repo", ev.Repo),
		slog.String("workflow_id", m.WorkflowID),
		slog.String("workflow_sha", specSHA),
		slog.String("criterion", rj.Criterion),
		slog.String("message", message),
	)

	// Best-effort audit on the SAME category the server seam writes
	// (run_rejected_applies_to) so one audit query covers both seams.
	d.appendAppliesToRejectedAudit(ctx, ev, m, rj, message, specSHA, now)

	// Best-effort issue comment. Both issue-trigger entry points populate
	// m.IssueRef; a nil notifier or a post failure logs and never softens the
	// refusal.
	if d.IssueNotifier != nil && m.TriggerSource == run.TriggerGitHubIssue &&
		m.IssueRef != nil && m.IssueRef.Number > 0 {
		if err := d.IssueNotifier.NotifyRunNotApplicable(ctx, ev.Repo, scope,
			m.IssueRef.Number, m.WorkflowID, message); err != nil {
			d.logger().LogAttrs(ctx, slog.LevelWarn,
				"run-not-applicable comment failed",
				slog.String("delivery_id", ev.DeliveryID),
				slog.String("repo", ev.Repo),
				slog.String("workflow_id", m.WorkflowID),
				slog.String("error", err.Error()),
			)
		}
	}
	return true
}

// appendAppliesToRejectedAudit records the applies_to refusal on the GLOBAL
// chain: no run row exists at the gate, so there is nothing to scope the entry
// to (system actor, github-webhook subject — the same shape as the budget /
// misconfigured refusal audits). It writes the SAME run_rejected_applies_to
// category the server admission seam uses so a single audit query covers both
// seams.
//
// BEST-EFFORT: a nil audit repository or an append failure is warn-logged and
// does NOT soften the refusal — the decision already went the safe way. (The
// override GRANT is the deliberate opposite: it fails closed on the same
// failure because it records an exception, not the safe outcome.)
func (d *Dispatcher) appendAppliesToRejectedAudit(ctx context.Context, ev Event, m Match, rj appliesto.Rejection, message, specSHA string, now time.Time) {
	if d.Audit == nil {
		return
	}
	systemKind := audit.ActorKind("system")
	fields := map[string]any{
		"reason":               "workflow_not_applicable",
		"workflow_id":          m.WorkflowID,
		"repo":                 ev.Repo,
		"workflow_sha":         specSHA,
		"trigger_source":       string(m.TriggerSource),
		"criterion":            rj.Criterion,
		"required":             rj.Required,
		"observed":             rj.Observed,
		"absent_issue_context": rj.AbsentIssueContext,
		"satisfying_workflows": rj.Satisfying,
		"message":              message,
		"delivery_id":          ev.DeliveryID,
		"event":                ev.Type,
	}
	if rj.MatchErr != nil {
		fields["evaluation_error"] = rj.MatchErr.Error()
	}
	payload, _ := json.Marshal(fields)
	if _, err := d.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    now,
		Category:     "run_rejected_applies_to",
		ActorKind:    &systemKind,
		ActorSubject: stringPtr("github-webhook"),
		Payload:      payload,
		AccountID:    d.eventAccountID(ctx, ev),
	}); err != nil {
		d.logger().LogAttrs(ctx, slog.LevelWarn,
			"append run_rejected_applies_to audit entry failed",
			slog.String("delivery_id", ev.DeliveryID),
			slog.String("repo", ev.Repo),
			slog.String("workflow_id", m.WorkflowID),
			slog.String("error", err.Error()),
		)
	}
}
