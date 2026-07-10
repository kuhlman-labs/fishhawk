package issuecomment

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The living anchor (#1054) is one comment per run, edited in place,
// that projects the run's full audit chain: a distilled header with a
// next_actions-style "what now" line, an event timeline, surfaced
// reviewer verdicts, the current plan as a collapsed <details>, and any
// superseded plans (kept collapsed with their rejection reasons). It is
// rebuilt from the audit chain on every update — no text patching — so
// the projection is idempotent. RenderAnchorBody is the pure renderer;
// the Notifier (notifier.go) loads the inputs and posts/edits the
// comment, and ping.go decides when a page-class event needs a NEW ping
// comment (GitHub does not notify on comment edits).

// AnchorPlanView is the distilled plan the anchor renders inside a
// collapsed <details>. Used for both the current plan and each
// superseded plan version. The Notifier builds these from the plan
// artifacts; RenderAnchorBody never reaches the artifact store itself.
type AnchorPlanView struct {
	// Summary is the plan's one-paragraph summary, shown as the
	// <summary> line (visible while collapsed).
	Summary string
	// Files is the plan's scope.files, rendered as a bullet list inside
	// the expanded body.
	Files []plan.ScopeFile
	// Approach is the optional ordered step list.
	Approach []plan.ApproachStep
	// RejectionReason, when non-empty, marks this as a superseded plan
	// version and is rendered alongside the superseded label.
	RejectionReason string
	// RecommendedModel is the plan's model_recommendation.implement_model
	// (#1013) — the planner's complexity-informed implement-model
	// suggestion the operator ratifies or overrides at the gate. Empty when
	// the plan made no recommendation.
	RecommendedModel string
	// RecommendationRationale is the rationale paired with RecommendedModel
	// (model_recommendation.rationale). Empty when absent.
	RecommendationRationale string
}

// AnchorInput bundles everything RenderAnchorBody projects. CurrentPlan
// is nil until a plan exists (or when the artifact store is
// unavailable); SupersededPlans is oldest-first and empty in the common
// single-plan case.
type AnchorInput struct {
	Run             *run.Run
	Stages          []*run.Stage
	Audit           []*audit.Entry
	CurrentPlan     *AnchorPlanView
	SupersededPlans []AnchorPlanView
	// Economics, when non-nil, renders the per-change economics block
	// (#1702) — total cost, per-stage cost, wall clock, per-gate
	// wait-on-human breakdown, and cache net savings. The Notifier folds the
	// run's cost/latency rollups and populates this; nil (or an all-zero
	// rollup) omits the block. It is a DROPPABLE section — the degradation
	// ladder sheds it FIRST when the body exceeds the comment cap.
	Economics   *EconomicsInput
	ExternalURL string
	Now         time.Time
}

// anchorSections is the assembled, still-mutable form of the anchor body
// before the degradation ladder collapses it to fit GitHub's comment
// cap. Each field renders independently so the ladder can drop the
// optional ones (economics, then timeline, then superseded plans) while
// always keeping the header, the current plan summary, and the dashboard
// deep-link.
type anchorSections struct {
	header          string
	whatNow         string
	stages          string
	timeline        string
	reviews         string
	currentPlan     string
	modelResolved   string
	supersededPlans string
	economics       string
	footer          string
}

// RenderAnchorBody assembles the living-anchor body from the run's audit
// chain projection. Pure — no IO, no time.Now — so callers (the Notifier
// and the CLI status-comment endpoint) control exactly what is surfaced.
// The body is capped at MaxIssueCommentBodyBytes via a degradation ladder
// that drops the economics block first, then the timeline, then superseded
// plans, always preserving the header, current plan summary, and dashboard
// deep-link.
func RenderAnchorBody(in AnchorInput) string {
	if in.Run == nil {
		return ""
	}
	externalURL := strings.TrimRight(in.ExternalURL, "/")
	// runURL is the bare run-page URL used only by the oversize truncation
	// fallback; "" when the base URL is unset so the fallback degrades link-less
	// (#1787). The rendered surfaces thread externalURL through runShortLink /
	// viewRunLink instead, which branch on emptiness themselves.
	runURL := runURLFor(externalURL, in.Run.ID)

	s := anchorSections{
		header:          renderAnchorHeader(in.Run, externalURL),
		whatNow:         renderWhatNow(in.Run, in.Stages),
		stages:          renderAnchorStages(in.Stages),
		timeline:        renderAnchorTimeline(in.Audit, in.Now),
		reviews:         renderAnchorReviews(in.Stages, in.Audit),
		currentPlan:     renderCurrentPlan(in.CurrentPlan),
		modelResolved:   renderResolvedModel(in.Audit),
		supersededPlans: renderSupersededPlans(in.SupersededPlans),
		economics:       renderEconomicsSection(in.Economics),
		footer:          renderAnchorFooter(in.Run, externalURL),
	}

	// Degradation ladder: assemble at progressively reduced fidelity
	// until the body fits. Level 0 is everything; level 1 drops the
	// economics block first (display-only, least load-bearing); level 2
	// also drops the timeline; level 3 also drops superseded plans. A
	// still-oversized body at the floor falls through to
	// truncateForGitHubComment.
	for level := 0; level <= 3; level++ {
		body := assembleAnchor(s, level)
		if len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	floor := assembleAnchor(s, 3)
	return truncateForGitHubComment(floor, runURL, "", externalURL, in.Run.ID.String())
}

// assembleAnchor joins the sections at the given degradation level.
//
//	level 0 — full
//	level 1 — drop the economics block first (display-only, derived)
//	level 2 — also drop the timeline (oldest, least load-bearing context)
//	level 3 — also drop superseded plans
//
// The header, what-now line, current plan, and footer (dashboard link)
// are never dropped. The economics block sits just above the footer and is
// the FIRST section shed under the cap (#1702).
func assembleAnchor(s anchorSections, level int) string {
	parts := []string{s.header, s.whatNow, s.stages}
	if level < 2 && s.timeline != "" {
		parts = append(parts, s.timeline)
	}
	if s.reviews != "" {
		parts = append(parts, s.reviews)
	}
	if s.currentPlan != "" {
		parts = append(parts, s.currentPlan)
	}
	if s.modelResolved != "" {
		parts = append(parts, s.modelResolved)
	}
	if level < 3 && s.supersededPlans != "" {
		parts = append(parts, s.supersededPlans)
	}
	if level < 1 && s.economics != "" {
		parts = append(parts, s.economics)
	}
	parts = append(parts, s.footer)

	var b strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p)
		if !strings.HasSuffix(p, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func renderAnchorHeader(r *run.Run, externalURL string) string {
	return fmt.Sprintf("**Fishhawk run %s** — `%s` · %s %s",
		runShortLink(externalURL, r.ID), r.WorkflowID, runStateIcon(r.State), string(r.State))
}

// renderWhatNow is the next_actions-style "what now" line: a single
// sentence telling the reader what the run is waiting on. Derived from
// run + stage state rather than re-deriving the drive's NextAction
// (which is a request-time computation the renderer has no access to).
func renderWhatNow(r *run.Run, stages []*run.Stage) string {
	switch r.State {
	case run.StateSucceeded:
		return "_What now: run complete — nothing to do._"
	case run.StateCancelled:
		return "_What now: run cancelled._"
	case run.StateFailed:
		return "_What now: a stage failed — review the failure below, then retry or replan._"
	}
	// Running/pending: surface the most actionable stage state.
	for _, st := range stages {
		if st.State == run.StageStateAwaitingApproval {
			return fmt.Sprintf("_What now: the `%s` stage is awaiting approval — review the plan below and approve, or reply `/fishhawk reject <reason>`._", st.Type)
		}
	}
	for _, st := range stages {
		if st.State == run.StageStateRunning || st.State == run.StageStateDispatched {
			return fmt.Sprintf("_What now: the `%s` stage is running._", st.Type)
		}
	}
	return "_What now: run in progress._"
}

func renderAnchorStages(stages []*run.Stage) string {
	if len(stages) == 0 {
		return "_No stages yet._"
	}
	var b strings.Builder
	for _, s := range stages {
		fmt.Fprintf(&b, "- %s `%s` · %s\n", stageStateIcon(s.State), s.Type, string(s.State))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderAnchorTimeline projects the run's interesting audit rows as a
// collapsed event timeline (most-recent first). Reuses pickActivity's
// closed category set + renderActivityLine so the verb phrasing matches
// the rest of the surface. Empty when no interesting rows exist.
func renderAnchorTimeline(entries []*audit.Entry, now time.Time) string {
	activity := pickActivity(entries, anchorTimelineLimit)
	if len(activity) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<details><summary>Timeline</summary>\n\n")
	for _, e := range activity {
		if e.Category == "approval_submitted" {
			b.WriteString(renderGateDecisionTimelineEntry(e, entries, now))
			continue
		}
		fmt.Fprintf(&b, "- %s · %s\n", renderActivityLine(e), relativeAge(e.Timestamp, now))
	}
	b.WriteString("\n</details>")
	return b.String()
}

// renderGateDecisionTimelineEntry projects an `approval_submitted` row as
// a first-class gate-decision timeline entry: the approver identity
// (#1053), a precise decision phrase distinguishing approve /
// approve-with-conditions / reject, an explicit "(over N advisory
// reject(s))" marker when the operator decided over reviewer reject
// verdicts in the same round, and — for an approve carrying binding
// conditions — the verbatim conditions text in a nested collapsed
// <details>. The relative-age suffix stays on the parent bullet so the
// timeline reads uniformly. `entries` is the full chain (needed to bound
// the advisory-reject count to the arbitrated round).
func renderGateDecisionTimelineEntry(e *audit.Entry, entries []*audit.Entry, now time.Time) string {
	decision := approvalDecisionOf(e.Payload)
	comment := decodeApprovalComment(e.Payload)

	var phrase string
	switch decision {
	case "approve":
		if comment != "" {
			phrase = "approved the plan with conditions"
		} else {
			phrase = "approved the plan"
		}
	case "reject":
		phrase = "rejected the plan"
	default:
		phrase = "acted on the plan"
	}

	line := fmt.Sprintf("%s %s", approverMention(e), phrase)
	// The "over N advisory reject(s)" marker is an OVERRIDE signal: it
	// only makes sense on an approve that proceeded despite reviewer
	// rejects in the same round. A reject decision aligning with reviewer
	// rejects is not an override, so it carries no marker.
	if decision == "approve" {
		if n := advisoryRejectCountBefore("plan", entries, e.Sequence); n > 0 {
			line += fmt.Sprintf(" (over %d advisory %s)", n, advisoryRejectNoun(n))
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "- %s · %s\n", line, relativeAge(e.Timestamp, now))
	if decision == "approve" && comment != "" {
		// Block-level <details> nested under the bullet — GitHub Flavored
		// Markdown renders it inside the list item, matching how the anchor
		// already nests <details> for reviews and plans.
		b.WriteString("  <details><summary>Approval conditions</summary>\n\n")
		fmt.Fprintf(&b, "%s\n\n", comment)
		b.WriteString("  </details>\n")
	}
	return b.String()
}

// anchorTimelineLimit caps the timeline rows. Generous relative to the
// status comment's 3 because the anchor is the run's full projection,
// not a glance.
const anchorTimelineLimit = 12

// renderAnchorReviews renders the surfaced reviewer verdicts for each
// stage that has review activity, honoring the current-round isolation
// (binding condition 1). Empty when no stage has a current-round verdict.
func renderAnchorReviews(stages []*run.Stage, entries []*audit.Entry) string {
	var b strings.Builder
	for _, stageType := range []string{"plan", "implement"} {
		// Only render a stage's reviews when that stage exists on the run.
		if !hasStageType(stages, stageType) {
			continue
		}
		section := renderStageReviews(stageType, entries)
		if section == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(section)
	}
	return strings.TrimRight(b.String(), "\n")
}

func hasStageType(stages []*run.Stage, stageType string) bool {
	for _, s := range stages {
		if string(s.Type) == stageType {
			return true
		}
	}
	return false
}

// renderStageReviews renders one stage's current-round reviewer verdicts:
// an inline summary line ("claude-opus-4-8: approve · gpt-5.5: reject (1
// high)") plus the full free_form text per verdict in an expandable
// <details>. Empty when the current round has no landed verdicts.
func renderStageReviews(stageType string, entries []*audit.Entry) string {
	verdicts := currentRoundReviewVerdicts(stageType, entries)
	if len(verdicts) == 0 {
		return ""
	}
	var summaries []string
	for _, v := range verdicts {
		summaries = append(summaries, v.summaryToken())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s review** — %s\n", capitalize(stageType), strings.Join(summaries, " · "))
	// Every current-round reviewer gets its own per-reviewer <details> so a
	// two-reviewer round can never read as one (#1073). When a verdict
	// carries no concerns and no free_form, the body is "(no additional
	// notes)" — keeping the block non-empty and the expandable-block count
	// equal to the reviewer count.
	for _, v := range verdicts {
		fmt.Fprintf(&b, "<details><summary>%s</summary>\n\n", v.summaryToken())
		if v.freeForm == "" && len(v.concerns) == 0 {
			b.WriteString("(no additional notes)\n")
		} else {
			for _, c := range v.concerns {
				fmt.Fprintf(&b, "- **%s** (%s): %s\n", c.severity, c.category, c.note)
			}
			if v.freeForm != "" {
				fmt.Fprintf(&b, "\n%s\n", v.freeForm)
			}
		}
		b.WriteString("\n</details>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCurrentPlan renders the current plan with the summary VISIBLE as
// plain markdown under a **Plan** heading, and only scope+approach tucked
// into a `Plan details` <details> (#1073). The <summary> attribute holds
// the short label `Plan details`, never plan prose, and the summary text
// is never duplicated inside the details body.
func renderCurrentPlan(p *AnchorPlanView) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("**Plan**\n\n")
	if p.Summary != "" {
		fmt.Fprintf(&b, "%s\n", p.Summary)
	} else {
		b.WriteString("_No summary provided._\n")
	}
	if p.RecommendedModel != "" {
		// The planner's complexity-informed implement-model recommendation
		// (#1013), visible alongside the summary so the operator sees the
		// suggestion the gate will ratify or override.
		if p.RecommendationRationale != "" {
			fmt.Fprintf(&b, "\n_Model recommendation: `%s` — %s_\n", p.RecommendedModel, oneLine(p.RecommendationRationale))
		} else {
			fmt.Fprintf(&b, "\n_Model recommendation: `%s`_\n", p.RecommendedModel)
		}
	}
	if detail := renderPlanScopeApproach(p); detail != "" {
		b.WriteString("\n<details><summary>Plan details</summary>\n\n")
		b.WriteString(detail)
		b.WriteString("\n</details>")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderResolvedModel renders the gate's resolved implement model (#1013) as a
// compact block under the plan: "**Implement model** — `<model>` (source:
// <rung>)". It reads the most-recent model_resolved audit entry (the gate is
// the sole writer; newest by Sequence wins). Empty when no model_resolved entry
// exists yet (pre-approval). An entry recording an EMPTY model — the deliberate
// default spawn — renders "**Implement model** — adapter default" so the anchor
// states the resolution honestly rather than omitting it.
func renderResolvedModel(entries []*audit.Entry) string {
	var latest *audit.Entry
	for _, e := range entries {
		if e.Category != "model_resolved" {
			continue
		}
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return ""
	}
	model, source := decodeModelResolved(latest.Payload)
	if model == "" {
		return "**Implement model** — adapter default"
	}
	if source == "" {
		return fmt.Sprintf("**Implement model** — `%s`", model)
	}
	return fmt.Sprintf("**Implement model** — `%s` (source: %s)", model, source)
}

func renderSupersededPlans(plans []AnchorPlanView) string {
	if len(plans) == 0 {
		return ""
	}
	var b strings.Builder
	for i := range plans {
		p := plans[i]
		fmt.Fprintf(&b, "<details><summary>🗑 Superseded plan — %s</summary>\n\n", oneLine(p.Summary))
		if p.RejectionReason != "" {
			fmt.Fprintf(&b, "_Rejected: %s_\n\n", oneLine(p.RejectionReason))
		}
		b.WriteString(renderPlanDetailBody(&p))
		b.WriteString("\n</details>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPlanDetailBody renders the expanded plan body (summary, scope,
// approach) shared by the superseded plan section. The current plan no
// longer uses this — it renders the summary visibly and tucks only
// scope+approach (via renderPlanScopeApproach) into its `Plan details`
// block (#1073).
func renderPlanDetailBody(p *AnchorPlanView) string {
	var b strings.Builder
	if p.Summary != "" {
		fmt.Fprintf(&b, "%s\n\n", p.Summary)
	}
	if sa := renderPlanScopeApproach(p); sa != "" {
		b.WriteString(sa)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPlanScopeApproach renders just the **Scope** bullet list and
// **Approach** ordered list, WITHOUT the leading summary paragraph. Used
// by renderCurrentPlan's `Plan details` block and shared into
// renderPlanDetailBody for superseded plans. Empty when the plan has
// neither scope files nor approach steps.
func renderPlanScopeApproach(p *AnchorPlanView) string {
	var b strings.Builder
	if files := renderFileList(p.Files, len(p.Files)); files != "" {
		b.WriteString("**Scope**\n\n")
		b.WriteString(files)
		b.WriteString("\n")
	}
	if len(p.Approach) > 0 {
		b.WriteString("**Approach**\n\n")
		for _, s := range p.Approach {
			fmt.Fprintf(&b, "%d. %s\n", s.Step, s.Description)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderEconomicsSection renders the per-change economics block (#1702) for
// the living anchor, or "" when no economics rollup is wired (nil) or the
// rollup is all-zero (RenderEconomicsBlock returns ""). Placed just above the
// footer and shed FIRST by the degradation ladder.
func renderEconomicsSection(in *EconomicsInput) string {
	if in == nil {
		return ""
	}
	return RenderEconomicsBlock(*in)
}

// renderAnchorFooter builds the footer from the "view run" link (omitted when
// the base URL is unset, #1787) and the optional pull-request link, joining the
// non-empty parts with the middot so an omitted run link leaves no dangling
// separator.
func renderAnchorFooter(r *run.Run, externalURL string) string {
	var parts []string
	if link := viewRunLink("View run →", externalURL, r.ID); link != "" {
		parts = append(parts, link)
	}
	if r.PullRequestURL != nil && *r.PullRequestURL != "" {
		parts = append(parts, fmt.Sprintf("[Pull request →](%s)", *r.PullRequestURL))
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------------
// Reviewer-verdict projection — binding condition 1 (#1054 / #1060).
// ---------------------------------------------------------------------

type anchorReviewConcern struct {
	severity string
	category string
	note     string
}

type anchorReviewVerdict struct {
	reviewerModel string
	verdict       string
	concerns      []anchorReviewConcern
	freeForm      string
	sequence      int64
}

// summaryToken renders the inline summary form for one verdict, e.g.
// "gpt-5.5: reject (1 high)". Concern counts are severity-tagged.
func (v anchorReviewVerdict) summaryToken() string {
	model := v.reviewerModel
	if model == "" {
		model = "reviewer"
	}
	tok := fmt.Sprintf("%s: %s", model, v.verdict)
	if c := severityCountSuffix(v.concerns); c != "" {
		tok += " " + c
	}
	return tok
}

// severityCountSuffix renders "(1 high · 2 medium)" from a concern set.
// Empty when there are no concerns.
func severityCountSuffix(concerns []anchorReviewConcern) string {
	if len(concerns) == 0 {
		return ""
	}
	bySev := map[string]int{}
	var order []string
	for _, c := range concerns {
		sev := c.severity
		if sev == "" {
			sev = "concern"
		}
		if _, ok := bySev[sev]; !ok {
			order = append(order, sev)
		}
		bySev[sev]++
	}
	// Stable, severity-priority ordering so the line is deterministic.
	sort.SliceStable(order, func(i, j int) bool {
		return severityRank(order[i]) < severityRank(order[j])
	})
	var parts []string
	for _, sev := range order {
		parts = append(parts, fmt.Sprintf("%d %s", bySev[sev], sev))
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

func severityRank(sev string) int {
	switch strings.ToLower(sev) {
	case "high":
		return 0
	case "medium":
		return 1
	case "low":
		return 2
	default:
		return 3
	}
}

// currentRoundReviewVerdicts returns the verdicts for stageType that
// landed in the MOST-RECENT review dispatch — entries on
// `<stageType>_reviewed` whose audit Sequence is strictly greater than
// the latest `<stageType>_review_started` Sequence (the dispatch
// boundary). This mirrors decodeReviewVerdicts' sinceSeq floor in
// backend/cmd/fishhawk-mcp/review.go: a stale verdict from a prior round
// (Sequence <= the floor) is excluded so it can never read as the
// current round's state. When no `_review_started` entry exists the
// floor is 0 and every verdict counts (real audit sequences are >= 1).
func currentRoundReviewVerdicts(stageType string, entries []*audit.Entry) []anchorReviewVerdict {
	startedCat := stageType + "_review_started"
	reviewedCat := stageType + "_reviewed"

	var floor int64
	for _, e := range entries {
		if e.Category == startedCat && e.Sequence > floor {
			floor = e.Sequence
		}
	}

	var out []anchorReviewVerdict
	for _, e := range entries {
		if e.Category != reviewedCat {
			continue
		}
		if e.Sequence <= floor {
			// Stale: belongs to an earlier review round.
			continue
		}
		v := decodeAnchorVerdict(e.Payload)
		v.sequence = e.Sequence
		out = append(out, v)
	}
	// Deterministic order: by audit sequence ascending.
	sort.SliceStable(out, func(i, j int) bool { return out[i].sequence < out[j].sequence })
	return out
}

func decodeAnchorVerdict(payload []byte) anchorReviewVerdict {
	var p struct {
		ReviewerModel string `json:"reviewer_model"`
		Verdict       string `json:"verdict"`
		FreeForm      string `json:"free_form"`
		Concerns      []struct {
			Severity string `json:"severity"`
			Category string `json:"category"`
			Note     string `json:"note"`
		} `json:"concerns"`
	}
	v := anchorReviewVerdict{}
	if len(payload) == 0 {
		return v
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return v
	}
	v.reviewerModel = p.ReviewerModel
	v.verdict = p.Verdict
	if v.verdict == "" {
		v.verdict = "?"
	}
	v.freeForm = p.FreeForm
	for _, c := range p.Concerns {
		v.concerns = append(v.concerns, anchorReviewConcern{severity: c.Severity, category: c.Category, note: c.Note})
	}
	return v
}

// advisoryRejectCountBefore counts the current-round reviewer rejects on
// `<stageType>_reviewed` that precede a given approval (Sequence
// beforeSeq). The round is bounded below by the latest
// `<stageType>_review_started` Sequence that is itself below beforeSeq —
// the round boundary immediately preceding the approval — so the count
// reflects only the round the approval actually arbitrated and survives
// replan rounds keyed by their own approval Sequence. Mirrors the
// current-round isolation in currentRoundReviewVerdicts.
func advisoryRejectCountBefore(stageType string, entries []*audit.Entry, beforeSeq int64) int {
	startedCat := stageType + "_review_started"
	reviewedCat := stageType + "_reviewed"

	var floor int64
	for _, e := range entries {
		if e.Category == startedCat && e.Sequence < beforeSeq && e.Sequence > floor {
			floor = e.Sequence
		}
	}

	count := 0
	for _, e := range entries {
		if e.Category != reviewedCat {
			continue
		}
		if e.Sequence >= beforeSeq || e.Sequence <= floor {
			continue
		}
		if verdictOf(e.Payload) == "reject" {
			count++
		}
	}
	return count
}

// advisoryRejectNoun renders the singular/plural noun for an advisory
// reject count ("reject" / "rejects").
func advisoryRejectNoun(n int) string {
	if n == 1 {
		return "reject"
	}
	return "rejects"
}

// approvalDecisionOf reads the `decision` field from an
// `approval_submitted` audit payload. Empty when absent or unparseable.
func approvalDecisionOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Decision
}

// decodeApprovalComment reads the approve-path conditions/amendment text
// from an `approval_submitted` payload — the `comment` field that
// approvals.go stamps on decision=approve (mirroring decodeRejectionComment
// in notifier.go, which reads the reject-path `rejection_comment`). Empty
// when absent or unparseable.
func decodeApprovalComment(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Comment string `json:"comment"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Comment
}

// oneLine collapses a (possibly multi-line) string to a single line and
// caps it so a <summary> stays readable.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	return truncate(s, 200)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
