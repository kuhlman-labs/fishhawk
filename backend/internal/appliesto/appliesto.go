// Package appliesto is the shared, pure evaluation core for a workflow's
// `applies_to` routing declaration (E53.3 / #2226, E53.10 / #2361). It carries
// no I/O, no HTTP shapes and no persistence — only the two-phase predicate
// split, the satisfying-workflow enumeration, the ONE operator-facing rejection
// renderer, and the trigger/label Change builders. THREE seams import it
// symmetrically so the routing guarantee reads identically wherever it fires:
//
//   - server admission (POST /v0/runs) — the `labels` / `trigger` half;
//   - server plan gate (handleShipPlan) — the deferred `paths` half;
//   - webhook admission (Dispatcher.Handle) — the `labels` / `trigger` half,
//     evaluated against FORGE-authoritative labels.
//
// It was extracted verbatim out of backend/internal/server/applies_to.go so the
// webhook dispatcher can share the SAME renderer and phase split rather than
// re-implementing them a package away (the drift E53.10 exists to prevent). The
// dependency direction is verified by compilation: this package imports only
// `spec` and `run`, and neither imports it — a cycle fails `go build`.
//
// A workflow's `applies_to` is a spec.Predicate (E53.1 / #2224) declaring
// which changes may be routed through it. Enforcement is FAIL-CLOSED and runs
// in TWO PHASES, because the predicate's criteria do not all have a producer
// at the same moment:
//
//   - ADMISSION: `labels` and `trigger`. Both are known before the run row
//     exists — labels from the caller's issue-context snapshot (server) or the
//     forge webhook payload (dispatcher), trigger from trigger_source.
//   - PLAN GATE: `paths`. No path set exists at start_run; the first
//     authoritative one is the approved plan's scope.files, which is BINDING
//     rather than descriptive — the scope gate confines the implement stage to
//     it — so a run admitted under a `paths: [docs/**]` declaration is CONFINED
//     to docs-only, not merely claimed to be.
//
// All three seams render through the ONE renderer below (RenderRejection) so an
// operator refused at any of them sees the same shape: the workflow, the
// criterion that failed, the value observed, and which workflows the change
// WOULD satisfy. The trailing ways-forward sentence is the only per-seam
// variation and is selected by Seam.
//
// FAIL-CLOSED IS UNIFORM AND DELIBERATELY ASYMMETRIC TO THE HOST PACKAGES'
// ADVISORY NEIGHBOURS. spec.Predicate.Match returns (false, error) for an
// empty predicate or a malformed glob rather than silently not matching; every
// call site treats that error as a REJECTION. The server's runScopePrecheck /
// runSurfaceSweep / runTestSweep / overCapSplitRejection and the dispatcher's
// budget/fail-open sweeps all correctly fail OPEN because they are advisory —
// writing `if err != nil { admit }` here by analogy with them is the tempting
// bug this comment exists to name.
//
// TRUST BOUNDARY: on the server admission path labels are fetched by the
// CALLER's `gh` and shipped inline, so that seam prevents MISROUTING, not a
// determined authorized caller who attests whatever labels they like. The
// WEBHOOK path is stronger: the labels come from the forge's own issue payload,
// not a caller attestation. A server-side fetch for the API path is the named
// hardening follow-up.
package appliesto

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Phase names which half of the two-phase split an evaluation is running. The
// phase decides which criteria of the predicate are in play and how the
// rejection message describes the observed value.
type Phase int

const (
	// PhaseAdmission evaluates the criteria with a producer at start_run:
	// labels and trigger.
	PhaseAdmission Phase = iota
	// PhasePlanGate evaluates the criterion whose producer is the approved
	// plan: paths. Consumed by the server's plan-gate slice.
	PhasePlanGate
)

// Seam names which of the three enforcement points produced a rejection. It is
// ORTHOGONAL to Phase (which criteria are in play): Seam selects ONLY the
// trailing ways-forward sentence, leaving the binding message shape (workflow,
// failed criterion, observed value, satisfying workflows) identical everywhere.
// A webhook trigger carries no operator request, so its tail must NOT advertise
// applies_to_override — there is nowhere to pass one.
type Seam int

const (
	// SeamStartRun is the POST /v0/runs admission gate. Zero value, so an
	// unset Seam renders the start_run tail. Advertises applies_to_override.
	SeamStartRun Seam = iota
	// SeamWebhook is the webhook Dispatcher.Handle admission gate. Its tail
	// names amending the declaration or re-starting through POST /v0/runs /
	// the CLI, and deliberately does NOT advertise applies_to_override.
	SeamWebhook
	// SeamPlanGate is the handleShipPlan deferred `paths` gate. Advertises
	// applies_to_override (the audited admission-time force-past carries
	// forward to it).
	SeamPlanGate
)

// PhasePredicate splits p into the sub-predicate belonging to phase. The second
// return reports whether that phase has ANY criterion to evaluate; false means
// the phase does not constrain and the caller must ADMIT without calling Match
// (calling Match on an empty predicate would fail closed, which is right for a
// caller that skipped Validate and wrong for a deliberately deferred phase).
//
// The split is exhaustive over the predicate grammar: paths belong to the plan
// gate, labels and trigger to admission. change_kind is rejected at spec-parse
// time inside applies_to (no producer, no ratified token set), so it can never
// reach here — but it is carried into the admission half rather than dropped
// so a future producer landing the criterion cannot silently go unenforced.
func PhasePredicate(p spec.Predicate, phase Phase) (spec.Predicate, bool) {
	var sub spec.Predicate
	switch phase {
	case PhaseAdmission:
		sub.Labels = p.Labels
		sub.Triggers = p.Triggers
		sub.ChangeKinds = p.ChangeKinds
	case PhasePlanGate:
		sub.Paths = p.Paths
	}
	constrains := len(sub.Paths) > 0 || len(sub.Labels) > 0 || len(sub.ChangeKinds) > 0 || len(sub.Triggers) > 0
	return sub, constrains
}

// Evaluate reports whether change c satisfies workflow wf's applies_to
// declaration for the given phase.
//
// Returns (matched, evaluated, err):
//   - a workflow declaring NO applies_to matches everything (true, false, nil)
//     — the back-compat reading that keeps every existing workflow selectable;
//   - a phase the predicate does not constrain matches (true, false, nil);
//   - otherwise the phase sub-predicate's Match decides, and a Match ERROR is
//     returned rather than swallowed so the caller can fail closed.
func Evaluate(wf spec.Workflow, c spec.Change, phase Phase) (bool, bool, error) {
	if wf.AppliesTo == nil {
		return true, false, nil
	}
	sub, constrains := PhasePredicate(*wf.AppliesTo, phase)
	if !constrains {
		return true, false, nil
	}
	ok, err := sub.Match(c)
	if err != nil {
		return false, true, err
	}
	return ok, true, nil
}

// SatisfyingWorkflows enumerates, in name order, every workflow in parsed that
// WOULD accept change c at this phase — the "where can I take this instead?"
// half of the rejection message. A workflow declaring no applies_to accepts
// any change and is included. A workflow whose predicate ERRORS is excluded:
// a declaration we cannot evaluate must never be recommended.
func SatisfyingWorkflows(parsed *spec.Spec, c spec.Change, phase Phase) []string {
	if parsed == nil {
		return nil
	}
	var out []string
	for name, wf := range parsed.Workflows {
		ok, _, err := Evaluate(wf, c, phase)
		if err != nil || !ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Rejection is everything the ONE renderer needs. Every rejection point
// (server admission, server plan gate, webhook admission) builds this and calls
// RenderRejection, so the three messages cannot drift apart.
type Rejection struct {
	// WorkflowID is the workflow the operator NAMED. applies_to filters an
	// operator-named workflow; it never selects one, which is why two
	// matching workflows is benign rather than a coin flip.
	WorkflowID string
	// Criterion is the predicate criterion that failed: labels, trigger or
	// paths. Empty when MatchErr is set (the declaration could not be
	// evaluated at all).
	Criterion string
	// Required is the criterion's declared values; Observed is what the
	// change carried, described by ObservedLabel (e.g. "scope.files").
	Required      []string
	Observed      []string
	ObservedLabel string
	// AbsentIssueContext marks the EMPTY-label-set run: the change's label
	// set is empty rather than merely non-matching. This gets its own
	// sentence — "your labels don't match" is actively misleading when
	// there are no labels to match with.
	AbsentIssueContext bool
	// IssueContextPresent discriminates the TWO ways a label set comes out
	// empty, which have different fixes and must not be described with one
	// sentence: an issue-LESS run (false — trigger_source=cli/ui, no issue
	// context at all) versus an UNLABELLED issue (true — issue context is
	// present and carries zero labels). Telling the operator of an
	// issue-triggered run that it "carries NO issue context" sends them
	// looking for a trigger problem they do not have.
	IssueContextPresent bool
	TriggerSource       string
	// Satisfying is the workflows that WOULD accept this change.
	Satisfying []string
	// MatchErr is the fail-closed-on-error branch: the predicate could not be
	// evaluated (empty predicate, malformed glob). Rendered as a rejection,
	// never degraded into an admit.
	MatchErr error
	// Phase names which criteria were in play, so the messages are
	// distinguishable in a log without being differently shaped.
	Phase Phase
	// Seam names which enforcement point refused, selecting ONLY the trailing
	// ways-forward sentence (Phase-orthogonal).
	Seam Seam
}

// RenderRejection is THE message renderer every rejection point uses. Shape
// (binding, asserted by test): the workflow, the criterion that failed, the
// value observed, and which workflows the change WOULD satisfy — plus the
// per-seam ways forward. An operator refused at the plan gate is further into a
// run than one refused at admission and needs more help, not less, so they
// carry the same content; only the trailing sentence keys off Seam.
func RenderRejection(rj Rejection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "workflow %q does not accept this change", rj.WorkflowID)
	label := rj.ObservedLabel
	if label == "" {
		label = rj.Criterion
	}
	observed := "[]"
	if len(rj.Observed) > 0 {
		observed = "[" + strings.Join(rj.Observed, ", ") + "]"
	}
	switch {
	case rj.MatchErr != nil:
		fmt.Fprintf(&b, ": its applies_to declaration could not be evaluated (%s), so the run is refused rather than admitted — a routing declaration that cannot be evaluated is never treated as match-all", rj.MatchErr.Error())
	case rj.AbsentIssueContext && rj.IssueContextPresent:
		// The issue exists and carries no labels. The observed value is
		// still reported — the shape is binding on EVERY rejection path,
		// and "[]" is the actionable fact here, not a detail to elide.
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s — the run's issue context carries no labels at all; an unlabelled issue cannot satisfy a labels criterion",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed)
	case rj.AbsentIssueContext:
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s because this run carries NO issue context (trigger_source=%s); an issue-less run cannot satisfy a labels criterion",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed, rj.TriggerSource)
	default:
		fmt.Fprintf(&b, ": its applies_to %s criterion requires one of [%s], but the change's %s is %s",
			rj.Criterion, strings.Join(rj.Required, ", "), label, observed)
	}
	if len(rj.Satisfying) > 0 {
		fmt.Fprintf(&b, ". Workflows that would accept this change: %s", strings.Join(rj.Satisfying, ", "))
	} else {
		b.WriteString(". No workflow in this spec accepts this change")
	}
	switch rj.Seam {
	case SeamPlanGate:
		b.WriteString(". Amend the workflow's applies_to declaration, narrow the plan's scope.files to satisfy it, or re-start the run with applies_to_override and a reason")
	case SeamWebhook:
		// A webhook trigger carries no operator request, so there is nowhere
		// to pass applies_to_override — naming it here points the operator at
		// a flag they have no way to use (E53.10 condition 3). Name the paths
		// they DO have instead, and deliberately do NOT mention the override.
		b.WriteString(". Amend the workflow's applies_to declaration, or re-start the run under a workflow that accepts this change via POST /v0/runs or the fishhawk CLI")
	default: // SeamStartRun
		b.WriteString(". Amend the workflow's applies_to declaration, start the run under a workflow that accepts this change, or pass applies_to_override with a reason to force this run")
	}
	return b.String()
}

// FirstFailingCriterion re-walks sub's DECLARED criteria in evaluation order
// and reports the first one c does not satisfy, with its declared and observed
// values, so the message names a specific criterion instead of "the
// predicate". Returns ok=false only when every declared criterion is
// satisfied, which a caller reaching here after a non-match will not see.
func FirstFailingCriterion(sub spec.Predicate, c spec.Change) (criterion string, required, observed []string, ok bool) {
	if len(sub.Paths) > 0 {
		if m, err := (spec.Predicate{Paths: sub.Paths}).Match(spec.Change{Paths: c.Paths}); err != nil || !m {
			return "paths", sub.Paths, c.Paths, true
		}
	}
	if len(sub.Labels) > 0 {
		if m, err := (spec.Predicate{Labels: sub.Labels}).Match(spec.Change{Labels: c.Labels}); err != nil || !m {
			return "labels", sub.Labels, c.Labels, true
		}
	}
	if len(sub.ChangeKinds) > 0 {
		if m, err := (spec.Predicate{ChangeKinds: sub.ChangeKinds}).Match(spec.Change{ChangeKind: c.ChangeKind}); err != nil || !m {
			return "change_kind", sub.ChangeKinds, []string{c.ChangeKind}, true
		}
	}
	if len(sub.Triggers) > 0 {
		if m, err := (spec.Predicate{Triggers: sub.Triggers}).Match(spec.Change{Trigger: c.Trigger}); err != nil || !m {
			return "trigger", triggerFormStrings(sub.Triggers), []string{string(normalizeTriggerForm(c.Trigger))}, true
		}
	}
	return "", nil, nil, false
}

func triggerFormStrings(tf []spec.TriggerForm) []string {
	out := make([]string, len(tf))
	for i, t := range tf {
		out[i] = string(t)
	}
	return out
}

// normalizeTriggerForm mirrors Predicate.Match's back-compat seam so the
// rejection message reports the trigger form actually matched against rather
// than an empty string.
func normalizeTriggerForm(t spec.TriggerForm) spec.TriggerForm {
	if t == "" {
		return spec.TriggerDiff
	}
	return t
}

// TriggerFormForSource maps a run's trigger_source onto the predicate's
// TriggerForm vocabulary. Every trigger source v0 mints — github_issue, cli,
// ui — produces a code diff, so all three map to TriggerDiff. The non-diff
// forms (scheduled, on_demand) have NO producer today: ADR-065's groomer and
// ADR-053's incident intake will emit them. A predicate declaring only a
// non-diff form therefore matches nothing yet, which is documented rather than
// rejected because — unlike change_kind — `trigger: [diff, scheduled]` is
// partially usable against real runs today.
func TriggerFormForSource(triggerSource string) spec.TriggerForm {
	switch triggerSource {
	case string(run.TriggerGitHubIssue), string(run.TriggerCLI), string(run.TriggerUI):
		return spec.TriggerDiff
	default:
		return spec.TriggerDiff
	}
}

// AdmissionChange builds the spec.Change evaluated at admission from a trigger
// source and a label set. Labels come from the issue-context snapshot the
// server caller shipped inline, or the forge's own issue payload on the webhook
// path; a nil/empty label set is an EMPTY label set and fails closed against a
// labels criterion.
//
// Paths are deliberately absent: no authoritative path set exists at admission,
// and the plan gate — not a caller self-attestation — supplies one.
func AdmissionChange(triggerSource string, labels []string) spec.Change {
	c := spec.Change{Trigger: TriggerFormForSource(triggerSource)}
	if len(labels) > 0 {
		c.Labels = labels
	}
	return c
}
