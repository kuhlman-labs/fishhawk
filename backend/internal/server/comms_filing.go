package server

// The comms FILING RENDERER and the ingest PREVIEW (E81.5 / #3775, phase 5
// #4015). commsFilingRequest is the ONE renderer the comms_report ingest
// preview and the phase-7 filing (#4017) share, so the captain approves the
// bytes that are filed: it neutralizes the agent's title and body, appends a
// provenance section built only from server data, ends the body with the comms
// draft marker, and filters labels. commsPreviewDrafts runs every draft through
// previewWorkItem (#3774) under one total budget and records what the captain
// reviews. Contract: docs/spec/comms-report-v1.md § "Filing renderer" and
// § "comms_report_recorded".

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// commsPreviewTotalBudget bounds ALL of one ingest's previews together. A
// package var so a NON-PARALLEL test can shrink it.
var commsPreviewTotalBudget = 30 * time.Second

// commsProvenanceHeading opens the server-rendered provenance section. An agent
// line carrying the same text is demoted, so this section is the only one.
const commsProvenanceHeading = "### Comms provenance (server-rendered)"

// commsProvenanceHeadingText is the heading's text without its markers, the
// form an agent line is compared against (case-insensitively).
const commsProvenanceHeadingText = "Comms provenance (server-rendered)"

// commsDemotedPrefix is prepended to a demoted agent line.
const commsDemotedPrefix = "(agent-written) "

// commsCharterLineMaxBytes caps one rubric line's charter text in the
// provenance section.
const commsCharterLineMaxBytes = 300

// The neutralization rules, applied in this order by neutralizeCommsProse.
// Every replacement produces text no rule matches again, so the function is
// idempotent.
var (
	// A character reference (&#35; &#x40; &num; &commat;) would decode to a
	// `#`, `@` or `<` the later rules never see.
	commsCharRefRe  = regexp.MustCompile(`&(#|[A-Za-z][A-Za-z0-9]{0,31};)`)
	commsImageRe    = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\n]*)\)`)
	commsLinkRe     = regexp.MustCompile(`\[([^\]]*)\]\(([^)\n]*)\)`)
	commsLinkDefRe  = regexp.MustCompile(`(?m)^[ \t]{0,3}\[([^\]\n]+)\]:.*$`)
	commsHTTPRe     = regexp.MustCompile(`(?i)https?://`)
	commsFTPRe      = regexp.MustCompile(`(?i)ftp://`)
	commsWWWRe      = regexp.MustCompile(`(?i)(www)\.`)
	commsIssueRefRe = regexp.MustCompile(`#([0-9])`)
	commsGHRefRe    = regexp.MustCompile(`(?i)(gh)-([0-9])`)
	commsMentionRe  = regexp.MustCompile(`@([A-Za-z0-9])`)
)

// neutralizeCommsProse makes agent-authored text inert when it is filed under
// the bot identity: nothing in it can render raw HTML, forge a marker, link,
// embed an image, autolink an issue or URL, or notify a user. In order:
//
//  1. `<` and `>` become U+FF1C and U+FF1E (raw HTML, HTML comments — so a
//     forged `<!-- fishhawk-comms:v1` marker — and `<url>` autolinks).
//  2. `&` opening a character reference becomes U+FF06.
//  3. `![alt](url)` becomes `alt (image removed)`.
//  4. `[text](url)` becomes `text (link removed)`.
//  5. a reference definition line `[x]: url` becomes `[x] (link removed)`.
//  6. any remaining `](` or `]:` gains a space, so no link form survives.
//  7. http/https/ftp schemes become hxxp/hxxps/fxp; `www.` becomes `www[.]`.
//  8. `#` before a digit (`#12`, `owner/repo#12`) becomes U+FF03.
//  9. `GH-` before a digit becomes `GH` + U+2011 + the digit.
//  10. `@` before a username character (mentions, team mentions and email
//     autolinks alike) becomes U+FF20.
//  11. a line whose text is the provenance heading is demoted.
func neutralizeCommsProse(s string) string {
	s = strings.NewReplacer("<", "\uFF1C", ">", "\uFF1E").Replace(s)
	s = commsCharRefRe.ReplaceAllString(s, "\uFF06$1")
	s = commsImageRe.ReplaceAllString(s, "$1 (image removed)")
	s = commsLinkRe.ReplaceAllString(s, "$1 (link removed)")
	s = commsLinkDefRe.ReplaceAllString(s, "[$1] (link removed)")
	s = strings.NewReplacer("](", "] (", "]:", "] :").Replace(s)
	s = commsHTTPRe.ReplaceAllStringFunc(s, commsDefangScheme)
	s = commsFTPRe.ReplaceAllStringFunc(s, commsDefangScheme)
	s = commsWWWRe.ReplaceAllString(s, "${1}[.]")
	s = commsIssueRefRe.ReplaceAllString(s, "\uFF03$1")
	s = commsGHRefRe.ReplaceAllString(s, "${1}\u2011${2}")
	s = commsMentionRe.ReplaceAllString(s, "\uFF20$1")
	return demoteCommsProvenanceHeading(s)
}

// commsDefangScheme replaces the `t`s of a matched http/https/ftp scheme with
// `x`s of the same case (https:// -> hxxps://, FTP:// -> FXP://).
func commsDefangScheme(m string) string {
	return strings.NewReplacer("t", "x", "T", "X").Replace(m)
}

// demoteCommsProvenanceHeading prefixes every line whose text, stripped of
// heading markers and surrounding space, equals the provenance heading text.
func demoteCommsProvenanceHeading(s string) string {
	if !strings.Contains(strings.ToLower(s), strings.ToLower(commsProvenanceHeadingText)) {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		text := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if strings.EqualFold(text, commsProvenanceHeadingText) {
			lines[i] = commsDemotedPrefix + text
		}
	}
	return strings.Join(lines, "\n")
}

// neutralizeCommsTitle renders an agent title as one neutralized line of at
// most plan.CommsMaxTitleBytes bytes.
func neutralizeCommsTitle(s string) string {
	return commsOneLine(s, plan.CommsMaxTitleBytes)
}

// commsOneLine collapses s to one line (control and line-separator runes
// become spaces, whitespace runs collapse), neutralizes it, and caps it at
// maxBytes on a rune boundary. Idempotent.
func commsOneLine(s string, maxBytes int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = neutralizeCommsProse(s)
	if len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut])
	}
	return s
}

// commsFilingInput is one draft and the server data its filing renders from.
// Gathered is the comms_scan_gathered row the report was validated against;
// RubricText maps a rubric id to its charter line text and is nil when the
// charter text is not to be rendered (ids render alone).
type commsFilingInput struct {
	Draft      plan.CommsDraft
	Gathered   *commsScanGatheredPayload
	RubricText map[string]string
}

// commsFilingRequest renders the filing for one draft (contract § "Filing
// renderer"). The body is the neutralized agent body, a `---` rule, the
// server-rendered provenance section and, as its FINAL line, the comms draft
// marker over the cited reports' (id, content hash) from the gather. Nothing
// agent-written and no gathered report text enters the server section.
func commsFilingRequest(in commsFilingInput) workmgmt.FilingRequest {
	d := in.Draft
	shown := map[string]commsShownReport{}
	if in.Gathered != nil {
		for _, r := range in.Gathered.Shown {
			shown[r.ID] = r
		}
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(neutralizeCommsProse(d.ProposedIssue.Body)))
	b.WriteString("\n\n---\n")
	b.WriteString(commsProvenanceHeading)
	b.WriteString("\n\nReports:\n")
	var marked []userreport.MarkedReport
	issueNums := map[int]bool{}
	for _, id := range d.SourceReportIDs {
		r, ok := shown[id]
		if !ok {
			// Unreachable after the ingest's report_ref check; the id renders
			// alone and carries no marker entry or source ref.
			fmt.Fprintf(&b, "- %s\n", id)
			continue
		}
		if r.Kind == string(workmgmt.UserReportKindComment) {
			fmt.Fprintf(&b, "- %s (comment on #%d)\n", id, r.IssueNumber)
		} else {
			fmt.Fprintf(&b, "- %s (#%d)\n", id, r.IssueNumber)
		}
		marked = append(marked, userreport.MarkedReport{ID: id, ContentHash: r.ContentHash})
		if r.IssueNumber > 0 {
			issueNums[r.IssueNumber] = true
		}
	}
	b.WriteString("\nCharter rubric:\n")
	for _, c := range d.RubricCitations {
		if text := commsOneLine(in.RubricText[c.RubricID], commsCharterLineMaxBytes); text != "" {
			fmt.Fprintf(&b, "- %s: %s\n", c.RubricID, text)
		} else {
			fmt.Fprintf(&b, "- %s\n", c.RubricID)
		}
	}
	if marker := userreport.DraftMarker(marked); marker != "" {
		b.WriteString("\n")
		b.WriteString(marker)
	}

	nums := make([]int, 0, len(issueNums))
	for n := range issueNums {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	refs := make([]string, len(nums))
	for i, n := range nums {
		refs[i] = "#" + strconv.Itoa(n)
	}

	req := workmgmt.FilingRequest{
		Type:       d.ProposedIssue.Type,
		Summary:    neutralizeCommsTitle(d.ProposedIssue.Title),
		Body:       strings.TrimRight(b.String(), "\n"),
		Labels:     commsFilingLabels(d.ProposedIssue.Labels),
		SourceRefs: refs,
	}
	if d.ProposedIssue.ParentEpic != nil {
		if n, ok := plan.CommsParentEpicNumber(*d.ProposedIssue.ParentEpic); ok {
			req.Relations.ParentEpic = "#" + strconv.Itoa(n)
		}
	}
	return req
}

// commsFilingLabels keeps only labels in plan.CommsLabelNamespaces
// (case-insensitive, trimmed), first occurrence wins. autonomy:* and every
// other prefix are dropped even if a stored draft carries one.
func commsFilingLabels(labels []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range labels {
		ns := strings.ToLower(strings.TrimSpace(l))
		allowed := false
		for _, p := range plan.CommsLabelNamespaces() {
			if strings.HasPrefix(ns, p) {
				allowed = true
				break
			}
		}
		if !allowed || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, l)
	}
	return out
}

// commsFilingBodyDigest is the lowercase-hex sha256 of a commsFilingRequest
// body: what phase 7 recomputes and compares before filing.
func commsFilingBodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// The run-wide preview degrade reasons (preview_degrade_reason), the per-draft
// skip reasons, and the charter_text states.
const (
	commsPreviewDegradeRepoMalformed          = "repo_malformed"
	commsPreviewDegradeConventionsUnavailable = "conventions_unavailable"

	commsPreviewSkipBudgetExhausted = "budget_exhausted"
	commsPreviewSkipTimeout         = "preview_timeout"

	commsPreviewErrorPanicked = "preview_panicked"

	commsCharterTextRendered    = "rendered"
	commsCharterTextUnavailable = "unavailable"
	commsCharterTextChanged     = "changed"
)

// commsPreviewSet is every draft's preview plus the run-wide facts, shaped as
// the comms_report_recorded keys it fills (embed it in that payload).
type commsPreviewSet struct {
	Previews      []commsDraftPreview `json:"previews"`
	Degraded      bool                `json:"preview_degraded"`
	DegradeReason string              `json:"preview_degrade_reason,omitempty"`
	CharterText   string              `json:"charter_text"`
}

// commsDraftPreview is one draft's recorded preview: always the draft id and
// filing body digest, plus EXACTLY ONE of the rendered preview (its fields
// flattened in), Error, or Skipped (a skip reason, or the run-wide degrade
// reason when Degraded).
type commsDraftPreview struct {
	DraftID          string `json:"draft_id"`
	FilingBodyDigest string `json:"filing_body_digest"`
	*CommsRenderedPreview
	Error   *commsPreviewError `json:"error,omitempty"`
	Skipped string             `json:"skipped,omitempty"`
}

// CommsRenderedPreview is what previewWorkItem rendered for a draft. Body is
// the FULL rendered preview, intake advisory section included: the exact text
// the captain reviews. It is exported only so encoding/json can allocate it
// when a recorded row is decoded (an unexported embedded pointer cannot be).
type CommsRenderedPreview struct {
	Title                  string              `json:"title"`
	Body                   string              `json:"body"`
	Labels                 []string            `json:"labels"`
	DefaultedLabels        []string            `json:"defaulted_labels"`
	MissingLabelNamespaces []string            `json:"missing_label_namespaces"`
	Number                 int                 `json:"number,omitempty"`
	ParentEpic             string              `json:"parent_epic,omitempty"`
	SourceRefs             []string            `json:"source_refs"`
	Intake                 intakegroom.Signals `json:"intake"`
}

// commsPreviewError is a preview previewWorkItem refused.
type commsPreviewError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// commsPreviewEnv is the run-wide preview input.
type commsPreviewEnv struct {
	conv        workmgmt.Conventions
	target      workmgmt.Target
	owner, name string
}

// commsPreviewDrafts previews every draft of report, in report order, under
// ONE commsPreviewTotalBudget derived from ctx. It never returns an error: a
// degrade, a refused preview, a panic and an exhausted budget are all recorded.
// The caller runs it OUTSIDE any lock.
//
// Each preview runs on its own goroutine and the loop selects on the budget,
// so it returns within the budget even when a preview is blocked on an
// uncancellable number-allocation lock (keyedLocks.lock): that draft records
// skipped preview_timeout, the drafts after it skipped budget_exhausted, and
// the abandoned goroutine's result is discarded. The run-wide reads before the
// loop (conventions, the work target, the charter) are context-cooperative and
// run under the same budget.
func (s *Server) commsPreviewDrafts(ctx context.Context, runRow *run.Run, report *plan.CommsReport, gathered *commsScanGatheredPayload) commsPreviewSet {
	ctx, cancel := context.WithTimeout(ctx, commsPreviewTotalBudget)
	defer cancel()

	set := commsPreviewSet{Previews: []commsDraftPreview{}, CharterText: commsCharterTextUnavailable}
	if report == nil {
		return set
	}
	env, rubricText, degrade, charterText := s.commsPreviewSetup(ctx, runRow, gathered)
	set.CharterText = charterText
	if degrade != "" {
		set.Degraded, set.DegradeReason = true, degrade
	}
	for _, d := range report.Drafts {
		req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: gathered, RubricText: rubricText})
		entry := commsDraftPreview{DraftID: d.ID, FilingBodyDigest: commsFilingBodyDigest(req.Body)}
		switch {
		case degrade != "":
			entry.Skipped = degrade
		case ctx.Err() != nil:
			entry.Skipped = commsPreviewSkipBudgetExhausted
		default:
			s.commsPreviewOne(ctx, &entry, req, env)
		}
		set.Previews = append(set.Previews, entry)
	}
	return set
}

// commsPreviewSetup resolves the run-wide preview input: the repo
// coordinates, the conventions, the run-scoped target (a scope failure is
// tolerated, as in the upkeep apply: the provider fails closed per draft) and
// the charter rubric text, used only when the charter read now has the
// gathered content hash.
func (s *Server) commsPreviewSetup(ctx context.Context, runRow *run.Run, gathered *commsScanGatheredPayload) (env commsPreviewEnv, rubricText map[string]string, degrade, charterText string) {
	charterText = commsCharterTextUnavailable
	repo := ""
	if runRow != nil {
		repo = runRow.Repo
	}
	owner, name, ok := splitRepoFullName(repo)
	if !ok {
		return env, nil, commsPreviewDegradeRepoMalformed, charterText
	}
	conv, err := conventionsLoader(ctx, repo)
	if err != nil {
		return env, nil, commsPreviewDegradeConventionsUnavailable, charterText
	}
	target, _ := s.runScopedWorkTarget(ctx, runRow, owner, name, conv)
	env = commsPreviewEnv{conv: conv, target: target, owner: owner, name: name}

	charter, reason, _ := s.resolveCharterDocument(ctx, conv, target)
	switch {
	case reason != "" || gathered == nil || charter.ContentHash == "":
		return env, nil, "", commsCharterTextUnavailable
	case charter.ContentHash != gathered.Charter.ContentHash:
		return env, nil, "", commsCharterTextChanged
	}
	rubricText = map[string]string{}
	for _, id := range charter.RubricIDs.IDs() {
		rubricText[id] = charter.RubricIDs.Quote(id)
	}
	return env, rubricText, "", commsCharterTextRendered
}

// commsPreviewResult is one preview goroutine's outcome.
type commsPreviewResult struct {
	pv   *workItemPreview
	werr *workItemError
}

// commsPreviewOne runs one previewWorkItem on a goroutine and fills entry from
// its result, or records preview_timeout when the budget expires first. The
// result channel is buffered, so an abandoned goroutine never blocks; a panic
// in it is recovered and recorded (no request frame recovers a goroutine).
func (s *Server) commsPreviewOne(ctx context.Context, entry *commsDraftPreview, req workmgmt.FilingRequest, env commsPreviewEnv) {
	done := make(chan commsPreviewResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- commsPreviewResult{werr: &workItemError{code: commsPreviewErrorPanicked, msg: fmt.Sprintf("preview panicked: %v", r)}}
			}
		}()
		pv, werr := s.previewWorkItem(ctx, req, env.conv, env.target, env.owner, env.name)
		done <- commsPreviewResult{pv: pv, werr: werr}
	}()

	var res commsPreviewResult
	select {
	case res = <-done:
	case <-ctx.Done():
		select {
		case res = <-done: // finished as the budget expired: keep it
		default:
			entry.Skipped = commsPreviewSkipTimeout
			return
		}
	}
	if res.werr != nil {
		entry.Error = &commsPreviewError{Code: res.werr.code, Message: res.werr.msg}
		return
	}
	pv := res.pv
	entry.CommsRenderedPreview = &CommsRenderedPreview{
		Title:                  pv.Title,
		Body:                   pv.Body,
		Labels:                 nonNilStrings(pv.Labels),
		DefaultedLabels:        nonNilStrings(pv.DefaultedLabels),
		MissingLabelNamespaces: nonNilStrings(pv.MissingLabelNamespaces),
		Number:                 pv.Number,
		ParentEpic:             pv.Relations.ParentEpic,
		SourceRefs:             nonNilStrings(req.SourceRefs),
		Intake:                 pv.Intake,
	}
}

// nonNilStrings returns xs, or an empty slice when xs is nil, so a recorded
// list is always a JSON array.
func nonNilStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
