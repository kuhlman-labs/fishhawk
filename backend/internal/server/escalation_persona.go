package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Escalation-attached reviewer personas — the runtime half of ADR-084 D2(c) /
// rule 2 (E55.9 / #3754). A workflow-v2 escalation may declare
// `require.reviewers: [<persona>]`; when that escalation FIRES for the change
// under review, the named personas join the review round as extra reviewer
// invocations, exactly like a persona the reviewed stage attaches statically
// (reviewer_persona.go). The grammar and its three validation refusals live in
// backend/internal/spec; the pure firing walk and the fired-set → persona
// derivation (escalation.PersonaAttachments) in backend/internal/escalation.
//
// WHAT IS MATCHED, per review kind:
//
//   - plan review: the scope union of the plan UNDER REVIEW (no approved plan
//     exists yet) — path_source plan_scope.
//   - implement review: the APPROVED plan's scope union UNION every path the
//     reviewed diff changed (the pass diff AND the stage-cumulative evaluation
//     diff, rename/copy sources included) — path_source
//     approved_plan_scope_and_diff. The diff half is load-bearing: scope drift
//     into a sensitive path the plan never named still attaches the persona,
//     and an attachment reachable ONLY through the diff is recorded
//     via_diff_only.
//
// Labels and trigger are the run's admission change (escalationAdmissionChange
// — the SAME snapshot the approval and delegation seams match), so one rule
// cannot reach two answers about one run.
//
// COST ONLY WHERE ATTACHED. A workflow none of whose escalations declares
// require.reviewers short-circuits before any evaluation; an escalation that
// does not fire attaches nothing.
//
// NOT A REQUIREMENT. require.reviewers is deliberately absent from
// spec.ComposedRequirements, whose IsZero drives the approval gate's
// fail-closed and `escalated` branches: a reviewers-only escalation raises
// nothing at the approval or delegation seams (see README.md "Escalation
// enforcement").
//
// A Match error or a persona-selection refusal is reported to the caller as
// the escalation_unevaluable degrade detail, which the orchestrator in
// reviewer_persona.go turns into a terminal *_review_skipped entry — never a
// silent "nothing fired".

// CategoryEscalationPersonaAttached is the audit category recording that the
// escalations which fired for a review round attached reviewer personas to it.
// Registered in audit.KnownCategories (categories_completeness_test.go's AST
// sweep fails the build otherwise). INTERNAL like escalation_fired: it is not
// an issue-thread activity line.
const CategoryEscalationPersonaAttached = "escalation_persona_attached"

// Path sources an escalation_persona_attached entry names: which path set the
// escalations were matched against.
const (
	escalationPathSourcePlanScope       = "plan_scope"
	escalationPathSourceApprovedAndDiff = "approved_plan_scope_and_diff"
)

// reviewPaths is the path half of the change a review round's escalations are
// matched against.
type reviewPaths struct {
	// plan is the scope union of the plan the round measures against: the plan
	// under review at plan review, the approved plan at implement review.
	plan []string
	// diff is every path the reviewed change touched (diffReviewPaths); empty
	// at plan review.
	diff []string
	// source is the escalationPathSource* value recorded on the audit entry.
	source string
	// headSHA is the reviewed head, recorded when known (implement review).
	headSHA string
}

// diffReviewPaths returns every path the supplied diffs touch — each
// ChangedFile.Path AND each non-empty rename/copy OldPath, slash-normalized,
// de-duplicated and sorted. Several diffs are UNIONED: the implement-review
// call site passes the pass diff and the stage-cumulative evaluation diff, so
// a fix-up delta cannot hide a sensitive path an earlier pass of the same
// stage committed. The rename source is load-bearing: moving a file OUT of a
// sensitive path is a change to that path.
func diffReviewPaths(diffs ...policy.Diff) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, d := range diffs {
		for _, p := range implementReviewPaths(d) {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// escalationPersonaResolution is what resolveEscalationPersonas decided for
// one review round.
type escalationPersonaResolution struct {
	// selected are the escalation-attached personas, sorted by name.
	selected []spec.SelectedReviewerPersona
	// attachments carry each selected persona's fired provenance (same order).
	attachments []escalation.PersonaAttachment
	// viaDiffOnly names the personas that attach ONLY because of a diff path
	// (the plan scope alone would not have fired any rule naming them).
	viaDiffOnly map[string]bool
	// degraded is the escalation_unevaluable detail when the escalations could
	// not be evaluated or their personas could not be selected; non-empty
	// means selected is empty and a skip entry must be recorded.
	degraded string
}

// escalationsDeclareReviewers reports whether any declaration carries
// require.reviewers — the condition under which the review round evaluates
// escalations at all.
func escalationsDeclareReviewers(escalations []spec.Escalation) bool {
	for i := range escalations {
		if len(escalations[i].Require.Reviewers) > 0 {
			return true
		}
	}
	return false
}

// resolveEscalationPersonas evaluates wf's escalations against the run's
// admission change plus paths and returns the personas the FIRED ones attach.
// It is pure over its inputs (no repository read): the caller supplies the
// path sets, the labels and trigger come from the run row's snapshot.
func resolveEscalationPersonas(runRow *run.Run, parsed *spec.Spec, wf spec.Workflow, paths reviewPaths) escalationPersonaResolution {
	if runRow == nil || !escalationsDeclareReviewers(wf.Escalations) {
		return escalationPersonaResolution{}
	}
	change := escalationAdmissionChange(runRow)
	change.Paths = unionPaths(paths.plan, paths.diff)
	res, err := escalation.Evaluate(wf.Escalations, change)
	if err != nil {
		// A Match error is a declaration we cannot evaluate; treating it as
		// "nothing fired" would be indistinguishable from the control being
		// absent.
		return escalationPersonaResolution{degraded: personaDetailEscalationUnevaluable}
	}
	attachments := escalation.PersonaAttachments(res)
	if len(attachments) == 0 {
		return escalationPersonaResolution{}
	}
	viaDiffOnly := map[string]bool{}
	if len(paths.diff) > 0 {
		planOnly := escalationAdmissionChange(runRow)
		planOnly.Paths = unionPaths(paths.plan, nil)
		planRes, perr := escalation.Evaluate(wf.Escalations, planOnly)
		if perr != nil {
			return escalationPersonaResolution{degraded: personaDetailEscalationUnevaluable}
		}
		byPlan := map[string]bool{}
		for _, a := range escalation.PersonaAttachments(planRes) {
			byPlan[a.Persona] = true
		}
		for _, a := range attachments {
			if !byPlan[a.Persona] {
				viaDiffOnly[a.Persona] = true
			}
		}
	}
	names := make([]string, 0, len(attachments))
	for _, a := range attachments {
		names = append(names, a.Persona)
	}
	selected, err := parsed.SelectNamedReviewerPersonas(names)
	if err != nil {
		return escalationPersonaResolution{degraded: personaDetailEscalationUnevaluable}
	}
	return escalationPersonaResolution{selected: selected, attachments: attachments, viaDiffOnly: viaDiffOnly}
}

// unionPaths returns the de-duplicated union of a and b in first-seen order.
func unionPaths(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// escalationPersonaAttachedPayload is the escalation_persona_attached audit
// payload: one entry per attaching review round.
type escalationPersonaAttachedPayload struct {
	ReviewKind  string                             `json:"review_kind"`
	StageID     string                             `json:"stage_id"`
	PathSource  string                             `json:"path_source"`
	HeadSHA     string                             `json:"head_sha,omitempty"`
	Attachments []escalationPersonaAttachmentEntry `json:"attachments"`
}

// escalationPersonaAttachmentEntry is one attached persona. Fired and
// FiredKeys are index-aligned (built in one loop): the positional declaration
// index an operator finds the rule by, and the stable escalation.RuleKey a
// decision index joins on (the same two coordinates escalation_fired records).
type escalationPersonaAttachmentEntry struct {
	Persona     string   `json:"persona"`
	Fired       []int    `json:"fired"`
	FiredKeys   []string `json:"fired_keys"`
	ViaDiffOnly bool     `json:"via_diff_only"`
	// AlsoStatic is true when the reviewed stage ALSO attaches the persona
	// statically; it still runs exactly once.
	AlsoStatic bool `json:"also_static"`
}

// writeEscalationPersonaAttachedAudit records ONE escalation_persona_attached
// entry for a review round whose fired escalations attached personas, stamped
// on the reviewed stage. kind is the prompt kind ("plan_review" /
// "implement_review"); static names the personas the reviewed stage attaches
// statically.
//
// BEST-EFFORT, like escalation_fired: attaching a persona is a RAISE (one more
// reviewer), which has already happened by the time this is written, so an
// audit-store outage must not cancel the review. A missing repository or a
// failed append is WARN-logged NAMING the personas so the attachment is still
// recoverable from the application log.
func (s *Server) writeEscalationPersonaAttachedAudit(ctx context.Context, runRow *run.Run, stageID uuid.UUID, kind string, paths reviewPaths, res escalationPersonaResolution, static map[string]bool) {
	entries := make([]escalationPersonaAttachmentEntry, 0, len(res.attachments))
	names := make([]string, 0, len(res.attachments))
	for _, a := range res.attachments {
		e := escalationPersonaAttachmentEntry{
			Persona:     a.Persona,
			Fired:       make([]int, 0, len(a.Fired)),
			FiredKeys:   make([]string, 0, len(a.Fired)),
			ViaDiffOnly: res.viaDiffOnly[a.Persona],
			AlsoStatic:  static[a.Persona],
		}
		for _, f := range a.Fired {
			e.Fired = append(e.Fired, f.Index)
			e.FiredKeys = append(e.FiredKeys, escalation.RuleKey(f.Escalation))
		}
		entries = append(entries, e)
		names = append(names, a.Persona)
	}
	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "escalation persona: "+msg+"; the personas still run",
			slog.String("run_id", runRow.ID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("personas", strings.Join(names, ",")),
			slog.String("error", err.Error()))
	}
	if s.cfg.AuditRepo == nil {
		warn("no audit repository — "+CategoryEscalationPersonaAttached+" not recorded", fmt.Errorf("audit repository unconfigured"))
		return
	}
	payload, err := json.Marshal(escalationPersonaAttachedPayload{
		ReviewKind:  kind,
		StageID:     stageID.String(),
		PathSource:  paths.source,
		HeadSHA:     paths.headSHA,
		Attachments: entries,
	})
	if err != nil {
		warn("marshal "+CategoryEscalationPersonaAttached+" payload failed", err)
		return
	}
	actorKind := audit.ActorSystem
	sid := stageID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &sid,
		Timestamp: time.Now().UTC(),
		Category:  CategoryEscalationPersonaAttached,
		ActorKind: &actorKind,
		Payload:   payload,
	}); err != nil {
		warn("append "+CategoryEscalationPersonaAttached+" failed", err)
	}
}
