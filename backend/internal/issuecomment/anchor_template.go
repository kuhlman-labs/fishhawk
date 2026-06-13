package issuecomment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// PlanVersion is one plan-artifact version surfaced in the anchor
// comment's plan sections (#1054). The caller (the notifier anchor
// projection) loads every plan version produced for a run, numbers
// them oldest-to-newest, and tags the newest as current
// (Superseded == false). The current version renders as an expandable
// <details> whose <summary> line stays visible when collapsed; each
// earlier version renders collapsed and labeled "Plan vN (superseded)"
// with the rejection reason that retired it (matched from the audit
// chain by StageID).
type PlanVersion struct {
	// Plan is the parsed standard_v1 artifact. A nil Plan is skipped
	// by the renderer rather than panicking — a malformed version row
	// must never strand the whole anchor.
	Plan *plan.Plan
	// Version is the 1-based display number ("Plan v1", "Plan v2").
	Version int
	// StageID is the plan stage that produced this version. Used both
	// for the "approve in the dashboard" deep-link and to match the
	// approval_submitted reject entry that carries the rejection reason.
	StageID uuid.UUID
	// RequiresApproval drives whether the current plan renders an
	// "approve in the dashboard" link (gated) or a plain "view run"
	// link (gateless). Mirrors run.Stage.RequiresApproval.
	RequiresApproval bool
	// Superseded marks every version but the current (newest). A
	// superseded version renders collapsed with its rejection reason.
	Superseded bool
}

// RenderAnchorBody projects a run, its stages, every plan version, and
// the full run audit chain into the living "anchor" issue comment
// (#1054). The body is rebuilt from scratch on every call — there is
// no text patching — so concurrent writers each emit a complete,
// at-least-as-fresh projection (last-writer-wins is safe).
//
// Composition, top to bottom:
//
//  1. Header — run short-id link, workflow, run-state icon + text.
//  2. "What now" line — the distilled operator next step derived from
//     the same pipeline-state inputs the run-status next_action block
//     reads (awaiting approval, reviews pending, failed, running).
//  3. Stage list — one row per stage with a state icon (the structural
//     transition view).
//  4. Event timeline — gate decisions with #1053 approver identity,
//     reviewer verdicts as "model: verdict (n high, m medium)" with
//     free_form notes in a nested <details>, dispatches, CI retries,
//     and failures with category — projected from the audit chain.
//  5. Current plan — an expandable <details> whose <summary> shows the
//     plan summary when collapsed.
//  6. Superseded plans — collapsed <details>, each with its rejection
//     reason.
//  7. Footer — the dashboard deep-link (and PR link when stamped).
//
// The assembled body is held under GitHub's MaxIssueCommentBodyBytes
// cap by a degradation ladder (see assembleAnchorWithinCap): drop the
// oldest timeline entries first, then the stage list, then superseded
// plans oldest-first, then collapse the current plan to its summary
// line — always retaining the header, the what-now line, the current
// plan summary, and the dashboard deep-link.
//
// `now` is the reference point for relative timestamps; passing the
// notifier's own clock keeps rendering deterministic under test. Pure:
// no IO, no time.Now().
func RenderAnchorBody(runRow *run.Run, stages []*run.Stage, planVersions []PlanVersion, chain []*audit.Entry, externalURL string, now time.Time) string {
	if runRow == nil {
		return ""
	}
	runURL := externalURL + "/runs/" + runRow.ID.String()

	current, superseded := splitPlanVersions(planVersions)

	parts := anchorParts{
		header:      renderAnchorHeader(runRow, runURL),
		whatNow:     renderWhatNow(runRow, stages, chain),
		stageList:   renderAnchorStageList(stages),
		timeline:    renderAnchorTimeline(chain, now),
		current:     renderCurrentPlanBlock(current, runRow, externalURL),
		superseded:  renderSupersededPlanBlocks(superseded, chain),
		footer:      renderAnchorFooter(runRow, runURL),
		runURL:      runURL,
		runID:       runRow.ID.String(),
		externalURL: externalURL,
	}
	if current != nil {
		parts.currentStageID = current.StageID.String()
	}
	return assembleAnchorWithinCap(parts)
}

// anchorParts holds the structured, pre-rendered sections the
// degradation ladder re-assembles at varying levels of detail. Keeping
// the sections as discrete strings (rather than one flat body) is what
// lets the ladder drop content while always emitting well-formed
// markdown.
type anchorParts struct {
	header    string
	whatNow   string
	stageList string
	footer    string

	// timeline blocks are ordered oldest-first; the ladder drops from
	// the front (oldest) under size pressure.
	timeline []string
	// superseded plan blocks are ordered oldest-first (lowest version
	// first); the ladder drops from the front under size pressure.
	superseded []planBlock
	// current is the newest plan version's block, nil when the run has
	// no plan yet.
	current *planBlock

	runURL         string
	runID          string
	externalURL    string
	currentStageID string
}

// planBlock is one plan version's rendered pieces. label is the bold
// heading ("Plan v2"); summary is the one-line plan summary shown in
// the collapsed <summary>; body is the full plan markdown; reason is
// the trailing rejection-reason line (superseded versions only, "" for
// the current plan).
type planBlock struct {
	label   string
	summary string
	body    string
	reason  string
}

// assembleAnchorWithinCap renders the anchor body, shedding detail in
// the documented order until it fits under MaxIssueCommentBodyBytes.
// The header, what-now line, current-plan summary, and footer deep-link
// are never dropped; the final fallback hard-truncates the minimal body
// via truncateForGitHubComment so the output is always valid markdown
// under the cap.
func assembleAnchorWithinCap(p anchorParts) string {
	cfg := anchorCfg{
		timelineKeep:   len(p.timeline),
		stageList:      true,
		supersededKeep: len(p.superseded),
		expandCurrent:  true,
	}
	if body := assembleAnchor(p, cfg); len(body) <= MaxIssueCommentBodyBytes {
		return body
	}
	// 1. Drop timeline entries oldest-first.
	for cfg.timelineKeep > 0 {
		cfg.timelineKeep--
		if body := assembleAnchor(p, cfg); len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	// 2. Drop the stage list.
	if cfg.stageList {
		cfg.stageList = false
		if body := assembleAnchor(p, cfg); len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	// 3. Drop superseded plans oldest-first.
	for cfg.supersededKeep > 0 {
		cfg.supersededKeep--
		if body := assembleAnchor(p, cfg); len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	// 4. Collapse the current plan to its summary line (drop the body).
	if cfg.expandCurrent {
		cfg.expandCurrent = false
		if body := assembleAnchor(p, cfg); len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	// 5. Last resort: even the minimal projection exceeds the cap
	// (a pathologically long plan summary). Hard-truncate at a rune
	// boundary with a "view full plan" tail so the output stays valid.
	return truncateForGitHubComment(assembleAnchor(p, cfg), p.runURL, p.currentStageID, p.externalURL, p.runID)
}

// anchorCfg are the knobs the degradation ladder turns. timelineKeep /
// supersededKeep are counts of the most-recent blocks to retain (older
// blocks dropped); stageList toggles the stage rows; expandCurrent
// toggles the current plan's expandable body (its summary line stays
// regardless).
type anchorCfg struct {
	timelineKeep   int
	stageList      bool
	supersededKeep int
	expandCurrent  bool
}

func assembleAnchor(p anchorParts, cfg anchorCfg) string {
	var b strings.Builder
	b.WriteString(p.header)
	if p.whatNow != "" {
		b.WriteString("\n")
		b.WriteString(p.whatNow)
		b.WriteString("\n")
	}
	if cfg.stageList && p.stageList != "" {
		b.WriteString("\n")
		b.WriteString(p.stageList)
	}
	if cfg.timelineKeep > 0 && len(p.timeline) > 0 {
		kept := p.timeline
		if cfg.timelineKeep < len(kept) {
			kept = kept[len(kept)-cfg.timelineKeep:]
		}
		b.WriteString("\n**Timeline**\n\n")
		if cfg.timelineKeep < len(p.timeline) {
			fmt.Fprintf(&b, "_…%d earlier events omitted._\n", len(p.timeline)-cfg.timelineKeep)
		}
		for _, t := range kept {
			b.WriteString(t)
		}
	}
	if p.current != nil {
		b.WriteString("\n")
		b.WriteString(renderPlanBlock(*p.current, cfg.expandCurrent))
	}
	if cfg.supersededKeep > 0 && len(p.superseded) > 0 {
		kept := p.superseded
		if cfg.supersededKeep < len(kept) {
			kept = kept[len(kept)-cfg.supersededKeep:]
		}
		b.WriteString("\n**Earlier plans**\n\n")
		for _, sp := range kept {
			b.WriteString(renderPlanBlock(sp, false))
		}
	}
	b.WriteString("\n")
	b.WriteString(p.footer)
	return b.String()
}

func renderAnchorHeader(r *run.Run, runURL string) string {
	return fmt.Sprintf("**Fishhawk run [`%s`](%s)** — `%s` · %s\n",
		shortID(r.ID), runURL, r.WorkflowID, runStateIcon(r.State)+" "+string(r.State))
}

func renderAnchorFooter(r *run.Run, runURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[View run →](%s)", runURL)
	if r.PullRequestURL != nil && *r.PullRequestURL != "" {
		fmt.Fprintf(&b, " · [Pull request →](%s)", *r.PullRequestURL)
	}
	b.WriteString("\n")
	return b.String()
}

func renderAnchorStageList(stages []*run.Stage) string {
	if len(stages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range stages {
		fmt.Fprintf(&b, "- %s `%s` · %s\n", stageStateIcon(s.State), s.Type, string(s.State))
	}
	return b.String()
}

// renderWhatNow distills the operator's next step into one line,
// mirroring the run-status next_action vocabulary. Order of precedence:
// terminal failure → awaiting approval → reviews pending → running →
// succeeded. Returns "" when no actionable state applies.
func renderWhatNow(r *run.Run, stages []*run.Stage, chain []*audit.Entry) string {
	if r.State == run.StateFailed {
		if s := latestFailedStage(stages); s != nil && s.FailureCategory != nil {
			cat := *s.FailureCategory
			return fmt.Sprintf("**What now:** %s failed — category %s (%s). %s",
				s.Type, cat, cat.Description(), retryabilityHint(cat))
		}
		return "**What now:** the run failed. Inspect the trace in the dashboard."
	}
	if s := awaitingApprovalStage(stages); s != nil {
		return "**What now:** awaiting approval — reply `+1` / `lgtm` to approve from this thread, or approve in the dashboard."
	}
	if n := pendingReviewerVerdicts(chain); n > 0 {
		return fmt.Sprintf("**What now:** waiting on %d reviewer %s.", n, plural(n, "verdict", "verdicts"))
	}
	switch r.State {
	case run.StateRunning, run.StatePending:
		return "**What now:** run in progress."
	case run.StateSucceeded:
		if r.PullRequestURL != nil && *r.PullRequestURL != "" {
			return "**What now:** run complete — review the pull request."
		}
		return "**What now:** run complete."
	case run.StateCancelled:
		return "**What now:** run cancelled."
	}
	return ""
}

func retryabilityHint(c run.FailureCategory) string {
	switch c {
	case run.FailureA:
		return "Retry the stage once the agent issue is understood."
	case run.FailureB:
		return "Address the constraint violation, then replan or re-run."
	case run.FailureC:
		return "Transient infrastructure failure — retry the stage."
	case run.FailureD:
		return "The gate timed out or was rejected; re-approve or start a new run."
	}
	return ""
}

func latestFailedStage(stages []*run.Stage) *run.Stage {
	var out *run.Stage
	for _, s := range stages {
		if s.State == run.StageStateFailed {
			out = s
		}
	}
	return out
}

func awaitingApprovalStage(stages []*run.Stage) *run.Stage {
	for _, s := range stages {
		if s.State == run.StageStateAwaitingApproval {
			return s
		}
	}
	return nil
}

// pendingReviewerVerdicts returns how many configured agent reviewers
// have not yet landed a terminal verdict for the most-recent review
// dispatch. Derived purely from the chain: a *_review_started payload
// carries configured_agents, and each terminal *_reviewed / *_failed /
// *_skipped entry for the same stage settles one. Returns 0 when no
// review is in flight (nothing started, or all settled).
func pendingReviewerVerdicts(chain []*audit.Entry) int {
	pending := 0
	// Walk newest-first so the most-recent review dispatch per stage
	// wins; track stages already accounted for.
	type tally struct {
		configured int
		terminal   int
	}
	tallies := map[uuid.UUID]*tally{}
	startedStage := map[uuid.UUID]bool{}
	for _, e := range chain {
		stageKey := uuid.Nil
		if e.StageID != nil {
			stageKey = *e.StageID
		}
		switch e.Category {
		case "plan_review_started", "implement_review_started":
			var p struct {
				ConfiguredAgents int `json:"configured_agents"`
			}
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				t := tallies[stageKey]
				if t == nil {
					t = &tally{}
					tallies[stageKey] = t
				}
				t.configured = p.ConfiguredAgents
				startedStage[stageKey] = true
			}
		case "plan_reviewed", "implement_reviewed",
			"plan_review_failed", "implement_review_failed",
			"plan_review_skipped", "implement_review_skipped":
			t := tallies[stageKey]
			if t == nil {
				t = &tally{}
				tallies[stageKey] = t
			}
			t.terminal++
		}
	}
	for stageKey, t := range tallies {
		if !startedStage[stageKey] {
			continue
		}
		if remaining := t.configured - t.terminal; remaining > 0 {
			pending += remaining
		}
	}
	return pending
}

// renderAnchorTimeline projects the interesting audit-chain events into
// ordered (oldest-first) markdown blocks. Each block is a self-contained
// chunk the degradation ladder can drop whole.
func renderAnchorTimeline(chain []*audit.Entry, now time.Time) []string {
	// Defensive copy + ascending-sequence sort so the timeline reads
	// chronologically regardless of caller ordering.
	sorted := make([]*audit.Entry, 0, len(chain))
	for _, e := range chain {
		if e == nil {
			continue
		}
		sorted = append(sorted, e)
	}
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Sequence < sorted[i].Sequence {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	out := make([]string, 0, len(sorted))
	for _, e := range sorted {
		if block := renderTimelineEntry(e, now); block != "" {
			out = append(out, block)
		}
	}
	return out
}

// renderTimelineEntry renders one audit entry into a timeline block, or
// "" when the category is system noise the anchor doesn't surface.
func renderTimelineEntry(e *audit.Entry, now time.Time) string {
	age := relativeAge(e.Timestamp, now)
	switch e.Category {
	case "run_dispatched":
		return fmt.Sprintf("- 🚀 Run dispatched · %s\n", age)
	case "plan_generated":
		return fmt.Sprintf("- 📋 Plan posted · %s\n", age)
	case "approval_submitted":
		line := fmt.Sprintf("- 👋 %s %s the plan", approverMention(e), approvalDecisionVerb(e.Payload))
		if reason := decodeRejectionReason(e.Payload); reason != "" {
			line += fmt.Sprintf(" — _%s_", truncate(reason, 200))
		}
		return line + fmt.Sprintf(" · %s\n", age)
	case "plan_reviewed", "implement_reviewed":
		return renderVerdictBlock(e, age)
	case "plan_review_failed", "implement_review_failed":
		return fmt.Sprintf("- ⚠️ %s review failed%s · %s\n",
			reviewerModelLabel(e.Payload), reviewFailureSuffix(e.Payload), age)
	case "plan_review_skipped", "implement_review_skipped":
		return fmt.Sprintf("- ⚠️ Review skipped (no reviewer configured) · %s\n", age)
	case "ci_failure_retry_dispatched":
		return fmt.Sprintf("- 🔁 CI failed; retry dispatched%s · %s\n",
			retryAttemptSuffix(e.Payload), age)
	case "ci_retry_exhausted":
		return fmt.Sprintf("- 🛑 CI retry cap reached · %s\n", age)
	case "stage_retried":
		return fmt.Sprintf("- 🔄 Stage retried · %s\n", age)
	}
	return ""
}

// renderVerdictBlock renders a plan_reviewed / implement_reviewed entry
// as "model: verdict (n high, m medium)" with any free_form notes in a
// nested collapsed <details>.
func renderVerdictBlock(e *audit.Entry, age string) string {
	var p struct {
		ReviewerModel string `json:"reviewer_model"`
		Verdict       string `json:"verdict"`
		Concerns      []struct {
			Severity string `json:"severity"`
		} `json:"concerns"`
		FreeForm string `json:"free_form"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return ""
	}
	model := p.ReviewerModel
	if model == "" {
		model = "reviewer"
	}
	var high, medium, low int
	for _, c := range p.Concerns {
		switch c.Severity {
		case "high":
			high++
		case "medium":
			medium++
		case "low":
			low++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- 🔍 `%s`: %s%s · %s\n",
		model, verdictLabel(p.Verdict), severityCounts(high, medium, low), age)
	if ff := strings.TrimSpace(p.FreeForm); ff != "" {
		// Blank line after <summary> is required for inner markdown to
		// render (GitHub collapsed-sections spec).
		fmt.Fprintf(&b, "  <details><summary>reviewer notes</summary>\n\n%s\n\n  </details>\n", ff)
	}
	return b.String()
}

func verdictLabel(v string) string {
	switch v {
	case "approve":
		return "approved"
	case "approve_with_concerns":
		return "approved with concerns"
	case "reject":
		return "rejected"
	}
	if v == "" {
		return "reviewed"
	}
	return v
}

// severityCounts renders the parenthetical "(n high, m medium, k low)",
// omitting zero buckets. Returns "" when there are no concerns.
func severityCounts(high, medium, low int) string {
	parts := make([]string, 0, 3)
	if high > 0 {
		parts = append(parts, fmt.Sprintf("%d high", high))
	}
	if medium > 0 {
		parts = append(parts, fmt.Sprintf("%d medium", medium))
	}
	if low > 0 {
		parts = append(parts, fmt.Sprintf("%d low", low))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func reviewerModelLabel(payload json.RawMessage) string {
	var p struct {
		ReviewerModel string `json:"reviewer_model"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.ReviewerModel == "" {
		return "Reviewer"
	}
	return "`" + p.ReviewerModel + "`"
}

func reviewFailureSuffix(payload json.RawMessage) string {
	var p struct {
		Timeout bool `json:"timeout"`
	}
	if err := json.Unmarshal(payload, &p); err == nil && p.Timeout {
		return " (timed out)"
	}
	return ""
}

// decodeRejectionReason pulls the operator-facing rejection reason from
// an approval_submitted reject payload: the free-form rejection_comment,
// falling back to the decompose sentinel. Returns "" for approvals or
// reason-less rejects.
func decodeRejectionReason(payload json.RawMessage) string {
	var p struct {
		Decision         string `json:"decision"`
		RejectionComment string `json:"rejection_comment"`
		RejectReason     string `json:"reject_reason"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if p.Decision != "reject" {
		return ""
	}
	if p.RejectionComment != "" {
		return p.RejectionComment
	}
	if p.RejectReason != "" {
		return p.RejectReason
	}
	return ""
}

// splitPlanVersions separates the current (newest, non-superseded)
// version from the earlier superseded ones. Superseded versions are
// returned ascending by Version (oldest first). When no version is
// flagged current, the highest-Version entry is treated as current so
// the anchor always shows a plan once one exists.
func splitPlanVersions(versions []PlanVersion) (*PlanVersion, []PlanVersion) {
	var current *PlanVersion
	superseded := make([]PlanVersion, 0, len(versions))
	for i := range versions {
		v := versions[i]
		if v.Plan == nil {
			continue
		}
		if !v.Superseded {
			if current == nil || v.Version >= current.Version {
				if current != nil {
					superseded = append(superseded, *current)
				}
				vv := v
				current = &vv
				continue
			}
		}
		superseded = append(superseded, v)
	}
	// Fall back to the highest-version entry as current when none was
	// explicitly flagged.
	if current == nil && len(superseded) > 0 {
		hi := 0
		for i := range superseded {
			if superseded[i].Version >= superseded[hi].Version {
				hi = i
			}
		}
		cur := superseded[hi]
		current = &cur
		superseded = append(superseded[:hi], superseded[hi+1:]...)
	}
	sortPlanVersionsAsc(superseded)
	return current, superseded
}

func sortPlanVersionsAsc(versions []PlanVersion) {
	for i := 0; i < len(versions); i++ {
		for j := i + 1; j < len(versions); j++ {
			if versions[j].Version < versions[i].Version {
				versions[i], versions[j] = versions[j], versions[i]
			}
		}
	}
}

func renderCurrentPlanBlock(v *PlanVersion, r *run.Run, externalURL string) *planBlock {
	if v == nil {
		return nil
	}
	body := renderPlanArtifactBody(v.Plan)
	// Append the approve / view-run deep link inside the plan body so it
	// travels with the expandable section.
	if v.RequiresApproval {
		body += fmt.Sprintf("\n[Approve in the dashboard →](%s/runs/%s/stages/%s)\n",
			externalURL, r.ID.String(), v.StageID.String())
		body += "\n_Or approve from this thread by replying `+1` / `lgtm`; reply `/fishhawk reject <reason>` to block with a rationale._\n"
	}
	return &planBlock{
		label:   fmt.Sprintf("Plan v%d", v.Version),
		summary: planSummaryLine(v.Plan),
		body:    body,
	}
}

func renderSupersededPlanBlocks(versions []PlanVersion, chain []*audit.Entry) []planBlock {
	if len(versions) == 0 {
		return nil
	}
	out := make([]planBlock, 0, len(versions))
	for i := range versions {
		v := versions[i]
		reason := rejectionReasonForStage(chain, v.StageID)
		summary := fmt.Sprintf("Plan v%d (superseded)", v.Version)
		if reason != "" {
			summary += " — rejected: " + truncate(reason, 160)
		}
		out = append(out, planBlock{
			label:   fmt.Sprintf("Plan v%d (superseded)", v.Version),
			summary: summary,
			body:    renderPlanArtifactBody(v.Plan),
		})
	}
	return out
}

// rejectionReasonForStage walks the chain for the approval_submitted
// reject entry scoped to stageID and returns its reason. Returns "" when
// the version was superseded by something other than an explicit reject
// (e.g. a re-plan).
func rejectionReasonForStage(chain []*audit.Entry, stageID uuid.UUID) string {
	for i := len(chain) - 1; i >= 0; i-- {
		e := chain[i]
		if e == nil || e.Category != "approval_submitted" {
			continue
		}
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if reason := decodeRejectionReason(e.Payload); reason != "" {
			return reason
		}
	}
	return ""
}

// renderPlanBlock renders a planBlock. When expand is true the body is
// wrapped in a collapsed <details> whose <summary> shows the summary
// line; when false only the bold label + summary line is emitted (the
// degradation-ladder collapse). The trailing reason line, when present,
// is always shown.
func renderPlanBlock(blk planBlock, expand bool) string {
	var b strings.Builder
	if expand && strings.TrimSpace(blk.body) != "" {
		// Blank line after <summary> is required for the inner markdown
		// body to render (GitHub collapsed-sections spec).
		fmt.Fprintf(&b, "<details><summary><b>%s</b> — %s</summary>\n\n%s\n</details>\n",
			blk.label, blk.summary, blk.body)
	} else {
		fmt.Fprintf(&b, "**%s** — %s\n", blk.label, blk.summary)
	}
	if blk.reason != "" {
		fmt.Fprintf(&b, "_%s_\n", blk.reason)
	}
	return b.String()
}

// planSummaryLine returns the one-line, length-capped plan summary used
// in the collapsed <summary>. Falls back to a placeholder when empty.
func planSummaryLine(p *plan.Plan) string {
	if p == nil || strings.TrimSpace(p.Summary) == "" {
		return "(no summary)"
	}
	// Collapse newlines so the summary stays on one line inside <summary>.
	s := strings.Join(strings.Fields(p.Summary), " ")
	return truncate(s, 200)
}

// renderPlanArtifactBody renders the standard_v1 plan as a markdown
// document for embedding inside the anchor's <details>. Mirrors the
// retired full-plan body's section order (summary, scope, approach,
// verification, risks) but omits the header/footer/truncation — the
// anchor owns those. Pure.
func renderPlanArtifactBody(p *plan.Plan) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	if p.Summary != "" {
		fmt.Fprintf(&b, "**Summary**\n\n%s\n\n", p.Summary)
	}
	if files := renderFileList(p.Scope.Files, len(p.Scope.Files)); files != "" {
		b.WriteString("**Scope**\n\n")
		b.WriteString(files)
		b.WriteString("\n")
	}
	if len(p.Approach) > 0 {
		b.WriteString("**Approach**\n\n")
		for _, s := range p.Approach {
			fmt.Fprintf(&b, "%d. %s\n", s.Step, s.Description)
		}
		b.WriteString("\n")
	}
	if p.Verification.TestStrategy != "" || p.Verification.RollbackPlan != "" {
		b.WriteString("**Verification**\n\n")
		if p.Verification.TestStrategy != "" {
			fmt.Fprintf(&b, "- **Test strategy**: %s\n", p.Verification.TestStrategy)
		}
		if p.Verification.RollbackPlan != "" {
			fmt.Fprintf(&b, "- **Rollback plan**: %s\n", p.Verification.RollbackPlan)
		}
		b.WriteString("\n")
	}
	if len(p.RisksAndAssumptions) > 0 {
		b.WriteString("**Risks & assumptions**\n\n")
		for _, r := range p.RisksAndAssumptions {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// plural returns singular when n == 1, else pluralForm.
func plural(n int, singular, pluralForm string) string {
	if n == 1 {
		return singular
	}
	return pluralForm
}
