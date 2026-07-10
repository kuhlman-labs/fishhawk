package issuecomment

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The sticky PR status comment (E42.1 / #1784) is the PR-locus sibling of the
// living issue anchor (#1054): one system-owned comment per PR, edited in
// place, projecting the subset a human deciding the merge needs to see on the
// PR page itself — current-round review verdicts, the acceptance outcome with
// a per-criterion table, and fix-up history. It deliberately carries NO plan /
// scope / approach dump — that material is issue-locus and lives on the anchor.
//
// RenderPRStatusBody is the pure renderer; the Notifier (notifier.go) loads
// the inputs (including the acceptance artifact body) and posts/edits the
// comment via maybeUpdatePRStatusComment. It reuses the anchor's in-package
// section helpers (currentRoundReviewVerdicts / renderStageReviews) and the
// status-comment helpers (runStateIcon, decodeAcceptanceActivity) rather than
// duplicating them.

// PRStatusInput bundles everything RenderPRStatusBody projects. AcceptanceArtifact
// is the raw KindAcceptance artifact body the Notifier loaded for the run's
// acceptance stage (nil when no acceptance verdict has landed or the artifact
// is unretrievable) — the renderer decodes it tolerantly for the per-criterion
// table and degrades to the audit-payload tally line when it is nil or
// undecodable.
type PRStatusInput struct {
	Run                *run.Run
	Stages             []*run.Stage
	Audit              []*audit.Entry
	AcceptanceArtifact []byte
	ExternalURL        string
	Now                time.Time
}

// prStatusSections is the assembled, still-mutable form of the PR-status body
// before the degradation ladder collapses it to fit GitHub's comment cap. The
// acceptance section is rendered at two fidelities — acceptanceFull (with the
// per-criterion table) and acceptanceTally (the collapsed tally line) — so the
// ladder can shed the table detail first while keeping the outcome visible.
type prStatusSections struct {
	header          string
	whatNow         string
	reviews         string
	acceptanceFull  string
	acceptanceTally string
	fixups          string
	footer          string
}

// RenderPRStatusBody assembles the sticky PR status comment body from the run's
// audit-chain projection + the loaded acceptance artifact. Pure — no IO, no
// time.Now — so callers control exactly what is surfaced. The body is capped at
// MaxIssueCommentBodyBytes via a degradation ladder that drops the acceptance
// per-criterion table first (collapsing to the tally line), then the fix-up
// history, always preserving the header, the current-round reviews, the
// acceptance outcome, and the footer. A still-oversized floor falls through to
// truncateForGitHubComment.
func RenderPRStatusBody(in PRStatusInput) string {
	if in.Run == nil {
		return ""
	}
	externalURL := strings.TrimRight(in.ExternalURL, "/")
	runURL := externalURL + "/runs/" + in.Run.ID.String()

	acc := buildPRAcceptance(in.Audit, in.AcceptanceArtifact)
	s := prStatusSections{
		header:          renderPRStatusHeader(in.Run, runURL),
		whatNow:         renderPRWhatNow(in.Run, in.Stages, acc),
		reviews:         renderStageReviews("implement", in.Audit),
		acceptanceFull:  renderPRAcceptance(acc, true),
		acceptanceTally: renderPRAcceptance(acc, false),
		fixups:          renderPRFixupHistory(in.Audit),
		footer:          renderPRStatusFooter(in.Run, runURL),
	}

	// Degradation ladder: level 0 is everything; level 1 collapses the
	// acceptance criteria table to the tally line; level 2 also drops the
	// fix-up history. Header / reviews / acceptance-outcome / footer are never
	// dropped.
	for level := 0; level <= 2; level++ {
		body := assemblePRStatus(s, level)
		if len(body) <= MaxIssueCommentBodyBytes {
			return body
		}
	}
	floor := assemblePRStatus(s, 2)
	return truncateForGitHubComment(floor, runURL, "", externalURL, in.Run.ID.String())
}

// assemblePRStatus joins the sections at the given degradation level, mirroring
// assembleAnchor's blank-line-separated join.
//
//	level 0 — full (acceptance with per-criterion table)
//	level 1 — acceptance collapsed to the tally line
//	level 2 — also drop the fix-up history
func assemblePRStatus(s prStatusSections, level int) string {
	parts := []string{s.header, s.whatNow}
	if s.reviews != "" {
		parts = append(parts, s.reviews)
	}
	if level < 1 {
		if s.acceptanceFull != "" {
			parts = append(parts, s.acceptanceFull)
		}
	} else if s.acceptanceTally != "" {
		parts = append(parts, s.acceptanceTally)
	}
	if level < 2 && s.fixups != "" {
		parts = append(parts, s.fixups)
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

func renderPRStatusHeader(r *run.Run, runURL string) string {
	return fmt.Sprintf("**Fishhawk run [`%s`](%s)** — `%s` · %s %s",
		shortID(r.ID), runURL, r.WorkflowID, runStateIcon(r.State), string(r.State))
}

// renderPRWhatNow is the merge-scoped "what now" line — a single sentence
// telling the reader what the PR is waiting on. Derived from run + stage state,
// enriched by the settled acceptance outcome when present.
func renderPRWhatNow(r *run.Run, stages []*run.Stage, acc *prAcceptanceView) string {
	switch r.State {
	case run.StateSucceeded:
		return "_What now: run complete — the PR is ready to merge._"
	case run.StateCancelled:
		return "_What now: run cancelled — the PR will not merge from this run._"
	case run.StateFailed:
		return "_What now: a stage failed — review the failure, then retry or replan before merging._"
	}
	if acc != nil {
		switch acc.outcome {
		case "accepted":
			return "_What now: acceptance passed — review the verdicts below and merge when ready._"
		case "rejected":
			return "_What now: acceptance failed — see the criteria table below and triage before merging._"
		}
	}
	for _, st := range stages {
		if st.Type != run.StageTypeReview && st.Type != run.StageTypeImplement {
			continue
		}
		if st.State == run.StageStateRunning || st.State == run.StageStateDispatched {
			return "_What now: the change is being implemented and reviewed._"
		}
	}
	return "_What now: run in progress._"
}

func renderPRStatusFooter(r *run.Run, runURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[View run →](%s)", runURL)
	if issueURL := issueURLFor(r); issueURL != "" {
		fmt.Fprintf(&b, " · [Issue thread →](%s)", issueURL)
	}
	return b.String()
}

// issueURLFor rebuilds the triggering issue's GitHub URL from the run's repo +
// TriggerRef ("issue:42"). Empty when the run is not issue-triggered or the
// coordinates don't parse — the footer then omits the issue link.
func issueURLFor(r *run.Run) string {
	if r.TriggerRef == nil {
		return ""
	}
	number, ok := parseIssueRef(*r.TriggerRef)
	if !ok {
		return ""
	}
	repo, err := parseRepo(r.Repo)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", repo.String(), number)
}

// ---------------------------------------------------------------------
// Acceptance projection.
// ---------------------------------------------------------------------

// prAcceptanceView is the acceptance state the PR comment renders, folded from
// the acceptance_outcome_recorded + acceptance_triage_decided audit payloads
// (the aggregate verdict, target URL, validated head, triage disposition) and
// the KindAcceptance artifact body (the per-criterion detail, which the audit
// payload does NOT carry — confirmed at server/acceptance.go buildOutcomePayload).
type prAcceptanceView struct {
	outcome           string
	criteriaPassed    int
	criteriaTotal     int
	targetURL         string
	headSHA           string
	triageClass       string
	triageDisposition string
	criteria          []prAcceptanceCriterion
}

// prAcceptanceCriterion mirrors the server package's acceptanceCriterionResult
// JSON tags (server/acceptance.go). The server type is unexported so it cannot
// be imported; this is a tolerant local decode target like the package's other
// decoders (decodeAnchorVerdict etc.).
type prAcceptanceCriterion struct {
	id               string
	result           string
	observed         string
	expected         string
	expectationBasis string
	reproHandle      string
}

// buildPRAcceptance folds the run's acceptance audit rows + the loaded artifact
// body into a prAcceptanceView, or nil when no acceptance_outcome_recorded row
// exists yet (the acceptance section is then omitted entirely). The latest
// outcome / triage rows win (newest by Sequence).
func buildPRAcceptance(entries []*audit.Entry, artifactBody []byte) *prAcceptanceView {
	var outcome, triage *audit.Entry
	for _, e := range entries {
		switch e.Category {
		case "acceptance_outcome_recorded":
			if outcome == nil || e.Sequence > outcome.Sequence {
				outcome = e
			}
		case "acceptance_triage_decided":
			if triage == nil || e.Sequence > triage.Sequence {
				triage = e
			}
		}
	}
	if outcome == nil {
		return nil
	}
	a := decodeAcceptanceActivity(outcome.Payload)
	v := &prAcceptanceView{
		outcome:        a.outcome,
		criteriaPassed: a.criteriaPassed,
		criteriaTotal:  a.criteriaTotal,
	}
	v.targetURL, v.headSHA = decodePRAcceptanceOutcome(outcome.Payload)
	if triage != nil {
		t := decodeAcceptanceActivity(triage.Payload)
		v.triageClass = t.class
		v.triageDisposition = t.disposition
	}
	v.criteria = decodePRAcceptanceCriteria(artifactBody)
	return v
}

// renderPRAcceptance renders the acceptance section. When withTable is true and
// per-criterion detail is available, the per-criterion table is included;
// otherwise only the headline + metadata lines render (the degradation-ladder
// collapse, and the natural shape when the artifact carried no criteria).
func renderPRAcceptance(v *prAcceptanceView, withTable bool) string {
	if v == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(renderPRAcceptanceHeadline(v))
	b.WriteString("\n")
	if withTable {
		if table := renderPRCriteriaTable(v.criteria); table != "" {
			b.WriteString("\n")
			b.WriteString(table)
			b.WriteString("\n")
		}
	}
	if meta := renderPRAcceptanceMeta(v); meta != "" {
		b.WriteString("\n")
		b.WriteString(meta)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderPRAcceptanceHeadline(v *prAcceptanceView) string {
	icon := "❓"
	switch v.outcome {
	case "accepted":
		icon = "✅"
	case "rejected":
		icon = "❌"
	}
	outcome := v.outcome
	if outcome == "" {
		outcome = "unknown"
	}
	if v.criteriaTotal > 0 {
		return fmt.Sprintf("**Acceptance** — %s %s (%d/%d criteria passed)", icon, outcome, v.criteriaPassed, v.criteriaTotal)
	}
	return fmt.Sprintf("**Acceptance** — %s %s", icon, outcome)
}

// renderPRAcceptanceMeta renders the validated target URL, head SHA, and triage
// disposition as bullet lines. Each is omitted when absent.
func renderPRAcceptanceMeta(v *prAcceptanceView) string {
	var b strings.Builder
	if v.targetURL != "" {
		fmt.Fprintf(&b, "- Target: %s\n", v.targetURL)
	}
	if v.headSHA != "" {
		fmt.Fprintf(&b, "- Validated head: `%s`\n", shortSHA(v.headSHA))
	}
	switch {
	case v.triageClass != "" && v.triageDisposition != "":
		fmt.Fprintf(&b, "- Triage — class-%s: %s\n", v.triageClass, v.triageDisposition)
	case v.triageDisposition != "":
		fmt.Fprintf(&b, "- Triage — %s\n", v.triageDisposition)
	case v.triageClass != "":
		fmt.Fprintf(&b, "- Triage — class-%s\n", v.triageClass)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderPRCriteriaTable renders the per-criterion evidence as a markdown table
// (id, result, basis). The basis column carries the expectation_basis, falling
// back to the observed note; it is left empty when neither is present. Empty
// when no criteria were itemized.
func renderPRCriteriaTable(criteria []prAcceptanceCriterion) string {
	if len(criteria) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Criterion | Result | Basis |\n|---|---|---|\n")
	for _, c := range criteria {
		basis := c.expectationBasis
		if basis == "" {
			basis = c.observed
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", prTableCell(c.id), criterionResultCell(c.result), prTableCell(basis))
	}
	return strings.TrimRight(b.String(), "\n")
}

func criterionResultCell(result string) string {
	switch result {
	case "passed":
		return "✅ pass"
	case "failed":
		return "❌ fail"
	case "skipped":
		return "⏭ skip"
	case "":
		return "—"
	}
	return result
}

// prTableCell sanitizes a value for a single markdown table cell: collapse to
// one line (capped by oneLine) and escape the pipe so it cannot break the
// column boundary.
func prTableCell(s string) string {
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}

// decodePRAcceptanceOutcome reads target_url + head_sha out of an
// acceptance_outcome_recorded payload (server/acceptance.go buildOutcomePayload).
// These are NOT in the artifact body. Empty strings on any decode failure.
func decodePRAcceptanceOutcome(payload []byte) (targetURL, headSHA string) {
	if len(payload) == 0 {
		return "", ""
	}
	var p struct {
		TargetURL string `json:"target_url"`
		HeadSHA   string `json:"head_sha"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", ""
	}
	return p.TargetURL, p.HeadSHA
}

// decodePRAcceptanceCriteria tolerantly decodes the per-criterion results from
// the KindAcceptance artifact body's flat `criteria` array. Returns nil on any
// decode failure or a non-array (e.g. the historical object-keyed variant the
// server package coerces) — the table then degrades to the tally line.
func decodePRAcceptanceCriteria(body []byte) []prAcceptanceCriterion {
	if len(body) == 0 {
		return nil
	}
	var p struct {
		Criteria []struct {
			ID               string `json:"id"`
			Result           string `json:"result"`
			Observed         string `json:"observed"`
			Expected         string `json:"expected"`
			ExpectationBasis string `json:"expectation_basis"`
			ReproHandle      string `json:"repro_handle"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	out := make([]prAcceptanceCriterion, 0, len(p.Criteria))
	for _, c := range p.Criteria {
		out = append(out, prAcceptanceCriterion{
			id:               c.ID,
			result:           c.Result,
			observed:         c.Observed,
			expected:         c.Expected,
			expectationBasis: c.ExpectationBasis,
			reproHandle:      c.ReproHandle,
		})
	}
	return out
}

// ---------------------------------------------------------------------
// Fix-up history.
// ---------------------------------------------------------------------

type prFixupPass struct {
	sequence     int64
	headSHA      string
	filesChanged int
	applyPath    string
}

// renderPRFixupHistory renders one line per fixup_pushed audit pass with the
// short head SHA, files-changed count, and the apply_path discriminator when
// present. Empty when the run has had no fix-up passes.
func renderPRFixupHistory(entries []*audit.Entry) string {
	var passes []prFixupPass
	for _, e := range entries {
		if e.Category != "fixup_pushed" {
			continue
		}
		p := decodeFixupPush(e.Payload)
		p.sequence = e.Sequence
		passes = append(passes, p)
	}
	if len(passes) == 0 {
		return ""
	}
	sort.SliceStable(passes, func(i, j int) bool { return passes[i].sequence < passes[j].sequence })
	var b strings.Builder
	b.WriteString("**Fix-up history**\n\n")
	for i, p := range passes {
		line := fmt.Sprintf("%d. `%s` · %d file%s changed", i+1, shortSHA(p.headSHA), p.filesChanged, plural(p.filesChanged))
		if p.applyPath != "" {
			line += fmt.Sprintf(" · %s", p.applyPath)
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// decodeFixupPush reads head_sha + files_changed_count + apply_path out of a
// fixup_pushed payload (server/pullrequest.go). Zero value on decode failure.
func decodeFixupPush(payload []byte) prFixupPass {
	if len(payload) == 0 {
		return prFixupPass{}
	}
	var p struct {
		HeadSHA           string `json:"head_sha"`
		FilesChangedCount int    `json:"files_changed_count"`
		ApplyPath         string `json:"apply_path"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return prFixupPass{}
	}
	return prFixupPass{headSHA: p.HeadSHA, filesChanged: p.FilesChangedCount, applyPath: p.ApplyPath}
}

// shortSHA truncates a git SHA to 12 chars for compact display; returns the
// input unchanged when shorter (or an empty string, which renders as the empty
// backtick span the caller wraps).
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
