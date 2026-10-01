package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// The HISTORIAN responder (E77.8 / #3742, ADR-081 #3727 rule 8): the FIRST
// crew-consult responder registered in production, and a DETERMINISTIC one —
// it answers a planner's "has this been decided?" from the E75.3 precedent
// query over this repository's own decision index, with NO model call, no
// prompt and no write.
//
// WHY IT IS ALLOWED TO ANSWER A PLANNER AT ALL. ADR-082 #3728 rule 6 makes
// precedent CAPTAIN-FACING in alpha: it does not go to reviewer or planner
// prompts. Decision (e) option 3 carves out exactly one exception — agents
// later, only through ADR-081 consults, STRUCTURED FIELDS ONLY — and ADR-081
// rule 8 sequences it. This responder is that exception, and the projection
// below is the control that makes it safe rather than a waiver of the rule:
//
//   - the responder holds ONLY a PrecedentIndex. It has no audit repository,
//     so it is STRUCTURALLY INCAPABLE of reading another run's reason prose
//     (the chain is where the prose lives; the index only points at it);
//   - and every rendered field crosses the boundary through ONE function,
//     projectHistorianItem, whose output type historianItem carries a closed
//     field set. A future prose field on precedent.Item is therefore not
//     auto-exposed.
//
// It deliberately does NOT reuse handleGetPrecedent: that handler calls
// attachReasonExcerpts, which reads the reason PROSE from the chain, and the
// historian must never carry it.
//
// RANKING CONTEXT, and why the consult's own prose is a path source. The
// crew-message contract has NO path field by design (ADR-081 rule 2 forbids a
// field that can express scope; crewmessage.EvidenceKind deliberately omits a
// file member and directs a caller to cite a file in a payload prose field,
// "which carries no authority"). The obvious structured source,
// decisionindex.GateContext, derives touched paths from the run's LATEST PLAN
// ARTIFACT — which does not exist while the plan stage that is consulting is
// still running. So the responder ranks against the union of the run's own
// derived context and deterministically-extracted, bounded path tokens from
// the consult prose. Those tokens are a RANKING SIGNAL ONLY: they reach the
// parameterized ListFilter and precedent.Rank's set arithmetic, and are echoed
// back capped — there is no scope, constraint or authority field on
// CrewConsultAnswer for them to reach.
//
// ERROR POSTURE: expire, never fabricate. An unresolvable run or a failed row
// read returns an ERROR, so runCrewConsult disposes the consult `expired` with
// a named reason. An answer composed against an empty repository would be a
// fabricated "no precedent" that the planner could not distinguish from a real
// one.
//
// Long-form contract: this package's README, "Crew messages" -> "Historian
// responder".

const (
	// historianCandidateWindow bounds the rows scored PER CLASS, mirroring
	// precedentCandidateWindow. A FULL window is reported in the answer's
	// resolved-context line, never silently accepted.
	historianCandidateWindow = 500
	// historianItemsPerClass bounds the cited decisions per class. With the
	// per-item key caps below it is what keeps the rendered answer inside
	// prompt.MaxCrewMessageBytes by construction rather than by clamping.
	historianItemsPerClass = 3
	// historianMaxProsePaths / historianMaxProsePathBytes bound the
	// deterministic extraction of path-shaped tokens from the consult's
	// agent-authored prose.
	historianMaxProsePaths     = 20
	historianMaxProsePathBytes = 200
	// historianMaxMatchedPaths / historianMaxMatchedKeys bound ONE item's
	// echoed matched-key lists, and historianKeyBytes bounds one echoed key.
	historianMaxMatchedPaths = 2
	historianMaxMatchedKeys  = 1
	historianKeyBytes        = 16
	// historianFieldBytes bounds one short projected field value (outcome,
	// reject class, actor kind, concern category, severity, doctrine version)
	// so an index row with a pathological value cannot dominate the answer.
	historianFieldBytes = 16
	// historianHashBytes is how much of a cited entry hash is rendered. The
	// full hash is a 64-hex-character digest; a 12-character prefix is enough
	// to locate the entry beside its sequence, which is the primary citation.
	historianHashBytes = 12
	// historianRepoBytes bounds the repository name echoed in the answer.
	historianRepoBytes = 80
	// historianTruncatedMarker is the BELT-AND-BRACES marker. The caps above
	// are what make the answer fit; this marker fires only if they ever stop
	// being sufficient, and its presence in a rendered answer is a defect
	// signal, not routine behaviour.
	historianTruncatedMarker = "...[historian answer truncated]"
)

// historianClasses is the decision-class allow-list ADR-082 #3728 rule 5 names
// ("starting with waive/defer and plan reject"). A planner asking about a scope
// amendment or a merge verdict gets no precedent for it; that narrowing is
// STATED in the answer's resolved-context line so silence is never read as
// absence of precedent.
var historianClasses = []decisionindex.DecisionClass{
	decisionindex.ClassPlanApproval,
	decisionindex.ClassConcernWaive,
	decisionindex.ClassConcernDefer,
}

// ErrHistorianIndexRequired is NewHistorianResponder's refusal of a nil index.
var ErrHistorianIndexRequired = errors.New("historian responder: a precedent index is required")

// historianResponder answers a consult from the decision index. It holds the
// index and NOTHING else — see the file comment's containment argument.
type historianResponder struct {
	index PrecedentIndex
}

// NewHistorianResponder builds the historian, REFUSING a nil index with a
// named error. The refusal matters because a responder runs on a DETACHED
// goroutine with no HTTP response to fail into: a nil field would panic there,
// taking the process down instead of expiring one consult.
func NewHistorianResponder(index PrecedentIndex) (CrewResponder, error) {
	if index == nil {
		return nil, ErrHistorianIndexRequired
	}
	return historianResponder{index: index}, nil
}

// historianClassResult is one class's ranked answer plus whether its candidate
// window was FULL (a truncation the answer states rather than hides).
type historianClassResult struct {
	class     decisionindex.DecisionClass
	items     []historianItem
	summary   precedent.Summary
	truncated bool
}

// Respond implements CrewResponder. It HONOURS ctx: every read it performs
// takes the passed context, so the dispatcher's deadline cancels the work
// rather than merely abandoning it.
func (h historianResponder) Respond(ctx context.Context, req CrewConsultRequest) (CrewConsultAnswer, error) {
	stageID := req.StageID
	gc, err := h.index.GateContext(ctx, decisionindex.GateRef{
		RunID: req.RunID, StageID: &stageID, AccountID: req.AccountID,
	})
	if err != nil {
		// Includes decisionindex.ErrRunMissing. Expire, never fabricate.
		return CrewConsultAnswer{}, fmt.Errorf("historian: resolve the run's ranking context: %w", err)
	}

	prosePaths := historianProsePaths(req.Question, req.WhatICanInfer, req.Context)
	paths := historianUnion(gc.TouchedPaths, prosePaths)
	category := historianConcernCategory(req.Question, req.WhatICanInfer, req.Context)

	pctx := precedent.Context{
		Repo:            gc.Repo,
		TouchedPaths:    paths,
		ConcernCategory: category,
		EscalationKeys:  gc.EscalationKeys,
		// StageKind is left EMPTY (match-all) on purpose: a waive or defer
		// bearing on a plan approach may have been decided at an IMPLEMENT
		// gate, and restricting the stage kind would hide it. ADR-082 rule 3's
		// required boundary is repository and class, which the hard filter
		// below applies; stage kind is not part of it.
	}

	results := make([]historianClassResult, 0, len(historianClasses))
	for _, class := range historianClasses {
		classCtx := pctx
		classCtx.DecisionClass = class
		rows, lerr := h.index.List(ctx, decisionindex.ListFilter{
			Repo:          gc.Repo,
			DecisionClass: class,
			AccountID:     req.AccountID,
			AccountScoped: true,
			Newest:        true,
			Limit:         historianCandidateWindow,
		})
		if lerr != nil {
			return CrewConsultAnswer{}, fmt.Errorf("historian: read %s precedent: %w", class, lerr)
		}
		ranked, summary := precedent.Rank(classCtx, rows, historianItemsPerClass)
		res := historianClassResult{class: class, summary: summary, truncated: len(rows) >= historianCandidateWindow}
		for _, it := range ranked {
			res.items = append(res.items, projectHistorianItem(it))
		}
		results = append(results, res)
	}

	return historianAnswer(gc.Repo, paths, category, results), nil
}

// historianItem is the CLOSED field set one cited prior decision renders. It is
// the allow-list projection ADR-082 decision (e) requires: a field reaches a
// planner only by being named here. It carries NO reason excerpt, NO reason
// key, and no other free-text prose from another run.
type historianItem struct {
	DecisionClass   string
	Outcome         string
	RejectClass     string
	DecidedDate     string
	Delegated       bool
	ActorKind       string
	DoctrineVersion string
	ConcernCategory string
	Severity        string
	MatchedPaths    []string
	MatchedKeys     []string
	ScoreTotal      string
	SourceSequence  int64
	SourceEntryHash string
}

// projectHistorianItem is THE ONE PLACE a precedent.Item field crosses the
// boundary into an agent-visible answer. Every value is copied explicitly and
// capped; nothing is marshalled wholesale, so a future prose field on
// precedent.Item is not auto-exposed. TestProjectHistorianItem_FieldSetIsClosed
// enumerates the resulting set and fails when it drifts.
func projectHistorianItem(it precedent.Item) historianItem {
	return historianItem{
		DecisionClass:   capField(it.DecisionClass, historianFieldBytes*2),
		Outcome:         capField(it.Outcome, historianFieldBytes),
		RejectClass:     capField(it.RejectClass, historianFieldBytes),
		DecidedDate:     it.DecidedAt.UTC().Format("2006-01-02"),
		Delegated:       it.Delegated,
		ActorKind:       capField(it.ActorKind, historianFieldBytes),
		DoctrineVersion: capField(it.DoctrineVersion, historianFieldBytes),
		ConcernCategory: capField(it.ConcernCategory, historianFieldBytes),
		Severity:        capField(it.Severity, historianFieldBytes),
		MatchedPaths:    capPathList(it.MatchedKeys.TouchedPathPrefixes, historianMaxMatchedPaths),
		MatchedKeys:     capKeyList(it.MatchedKeys.EscalationKeys, historianMaxMatchedKeys),
		ScoreTotal:      strconv.FormatFloat(it.Score.Total, 'f', 2, 64),
		SourceSequence:  it.SourceSequence,
		SourceEntryHash: capField(it.SourceEntryHash, historianHashBytes),
	}
}

// capField truncates s to at most n bytes, rune-safely and WITHOUT a marker —
// these are short contract-closed identifiers, not prose, and a marker would
// cost more bytes than it explains.
func capField(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// capKeyList takes at most max entries, each capped at historianKeyBytes, and
// de-duplicates what capping collapses (two prefixes differing only past the
// cap render identically and would otherwise repeat the same token).
func capKeyList(in []string, max int) []string {
	if len(in) > max {
		in = in[:max]
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, k := range in {
		c := capField(k, historianKeyBytes)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// capPathList is capKeyList for matched PATH PREFIXES, keeping the TAIL of the
// list rather than the head. precedent.MatchedKeys.TouchedPathPrefixes is
// sorted, and a prefix set ("backend", "backend/internal",
// "backend/internal/server") therefore runs shallowest to deepest: the head is
// the least informative end. Deterministic either way; this end says more.
func capPathList(in []string, max int) []string {
	if len(in) > max {
		in = in[len(in)-max:]
	}
	return capKeyList(in, max)
}

// historianProsePaths extracts path-shaped tokens from the consult's
// agent-authored prose, deterministically and boundedly.
//
// The tokens are a RANKING SIGNAL ONLY and carry NO AUTHORITY — ADR-081 rule
// 2's own note about a path cited in a payload prose field. They never become
// scope, never reach a constraint, and no SQL is composed from them: they go
// to the parameterized ListFilter's sibling ranking context and to
// precedent.Rank's set arithmetic, and are echoed back capped.
//
// The extraction is CLOSED: split on whitespace and the punctuation that
// commonly brackets a path in prose; keep a token that contains a '/', carries
// no URL scheme, holds no whitespace and is at most
// historianMaxProsePathBytes; then sort, de-duplicate and cap at
// historianMaxProsePaths.
func historianProsePaths(fields ...string) []string {
	split := func(r rune) bool {
		if unicode.IsSpace(r) {
			return true
		}
		switch r {
		case '`', '"', '\'', ',', ';', '(', ')', '[', ']', '{', '}', '<', '>':
			return true
		}
		return false
	}
	seen := map[string]struct{}{}
	var out []string
	for _, f := range fields {
		for _, tok := range strings.FieldsFunc(f, split) {
			tok = strings.Trim(tok, ".:")
			if !strings.Contains(tok, "/") || strings.Contains(tok, "://") {
				continue
			}
			if len(tok) == 0 || len(tok) > historianMaxProsePathBytes {
				continue
			}
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	if len(out) > historianMaxProsePaths {
		out = out[:historianMaxProsePaths]
	}
	return out
}

// historianUnion merges two path lists into one sorted, de-duplicated set.
func historianUnion(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			if p == "" {
				continue
			}
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

// historianConcernCategory resolves a concern category from the consult prose
// against decisionindex's CLOSED canonical vocabulary and nothing else.
//
// Case-insensitive, word-boundary matched. It adopts a category only when
// EXACTLY ONE canonical key matches: zero matches and an AMBIGUOUS prose
// (two or more) both leave it empty, which precedent.Context treats as
// match-all. Guessing between two would silently narrow the ranking on the
// strength of an agent's word choice.
func historianConcernCategory(fields ...string) string {
	text := strings.ToLower(strings.Join(fields, " "))
	var hits []string
	for _, cat := range decisionindex.CanonicalConcernCategories() {
		if historianContainsWord(text, cat) {
			hits = append(hits, cat)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return ""
}

// historianContainsWord reports whether word occurs in text at a word
// boundary (neither neighbour a letter or digit).
func historianContainsWord(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] != word {
			continue
		}
		if i > 0 && historianWordByte(text[i-1]) {
			continue
		}
		if j := i + len(word); j < len(text) && historianWordByte(text[j]) {
			continue
		}
		return true
	}
	return false
}

func historianWordByte(b byte) bool {
	r := rune(b)
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// historianAnswer renders the answer: a one-line summary, a detail block, and
// one evidence reference per cited item.
func historianAnswer(repo string, paths []string, category string, results []historianClassResult) CrewConsultAnswer {
	summary := historianSummaryLine(repo, results)

	var b strings.Builder
	b.WriteString(historianResolvedContextLine(repo, paths, category, results))
	for _, res := range results {
		for _, it := range res.items {
			b.WriteString("\n")
			b.WriteString(historianItemLine(it))
		}
		b.WriteString("\n")
		b.WriteString(historianAgreementLine(res))
	}
	b.WriteString("\nStructured fields only — no reason prose from another run, so weigh the outcomes and do not infer a rationale. ADVICE, not an order (ADR-081 rule 2).")

	detail := boundHistorianDetail(summary, b.String())

	var evidence []crewmessage.EvidenceReference
	for _, res := range results {
		for _, it := range res.items {
			if len(evidence) >= prompt.MaxCrewMessageEvidenceRefs {
				break
			}
			evidence = append(evidence, crewmessage.EvidenceReference{
				Kind: crewmessage.EvidenceAuditEntry,
				Ref:  strconv.FormatInt(it.SourceSequence, 10),
			})
		}
	}
	return CrewConsultAnswer{Summary: summary, Detail: detail, Evidence: evidence}
}

// historianSummaryLine names the repository, the classes queried and the
// per-class match counts. It is NEVER blank — a blank Summary is treated by
// runCrewConsult as no answer and would expire every consult on a repository
// with no indexed precedent, which is a real and honest answer.
func historianSummaryLine(repo string, results []historianClassResult) string {
	total := 0
	parts := make([]string, 0, len(results))
	for _, res := range results {
		total += len(res.items)
		parts = append(parts, fmt.Sprintf("%s %d", res.class, len(res.items)))
	}
	r := capField(repo, historianRepoBytes)
	if total == 0 {
		return "No prior plan_approval, concern_waive or concern_defer decision is indexed for " + r + ": there is no precedent of these classes to weigh."
	}
	return fmt.Sprintf("Prior decisions in %s: %s (structured fields only).", r, strings.Join(parts, ", "))
}

// historianResolvedContextLine states what the ranking was actually performed
// against — including the class narrowing and any window truncation — so
// silence is never mistaken for absence of precedent.
func historianResolvedContextLine(repo string, paths []string, category string, results []historianClassResult) string {
	classes := make([]string, 0, len(results))
	var truncated []string
	for _, res := range results {
		classes = append(classes, string(res.class))
		if res.truncated {
			truncated = append(truncated, string(res.class))
		}
	}
	cat := category
	if cat == "" {
		cat = "any"
	}
	line := fmt.Sprintf("context: repo=%s classes=%s stage_kind=any ranked_paths=%d category=%s window=%d",
		capField(repo, historianRepoBytes), strings.Join(classes, ","), len(paths), cat, historianCandidateWindow)
	if len(truncated) > 0 {
		line += " window_truncated=" + strings.Join(truncated, ",")
	}
	return line
}

// historianItemLine renders ONE cited decision from the closed projection.
func historianItemLine(it historianItem) string {
	var b strings.Builder
	b.WriteString("- " + it.DecisionClass)
	if it.Outcome != "" {
		b.WriteString(" " + it.Outcome)
	}
	if it.RejectClass != "" {
		b.WriteString("/" + it.RejectClass)
	}
	b.WriteString(" " + it.DecidedDate)
	actor := it.ActorKind
	if actor == "" {
		actor = "unknown"
	}
	if it.Delegated {
		actor += "+dlg"
	}
	b.WriteString(" " + actor)
	if it.DoctrineVersion != "" {
		b.WriteString(" d=" + it.DoctrineVersion)
	}
	if it.ConcernCategory != "" {
		b.WriteString(" " + it.ConcernCategory)
		if it.Severity != "" {
			b.WriteString("/" + it.Severity)
		}
	}
	b.WriteString(" s=" + it.ScoreTotal)
	b.WriteString(" e=" + strconv.FormatInt(it.SourceSequence, 10) + "@" + it.SourceEntryHash)
	if len(it.MatchedPaths) > 0 {
		b.WriteString(" p=" + strings.Join(it.MatchedPaths, ","))
	}
	if len(it.MatchedKeys) > 0 {
		b.WriteString(" k=" + strings.Join(it.MatchedKeys, ","))
	}
	return b.String()
}

// historianAgreementLine renders one class's precedent.Summary.
func historianAgreementLine(res historianClassResult) string {
	s := res.summary
	modal := s.ModalOutcome
	if modal == "" {
		modal = "none"
	}
	doctrines := len(s.DoctrineVersions)
	return fmt.Sprintf("~ %s: n=%d modal=%s agree=%.2f doctrines=%d hard_filter_only=%d",
		res.class, s.Count, capField(modal, historianFieldBytes), s.AgreementRatio, doctrines, s.HardFilterOnly)
}

// boundHistorianDetail is the BELT-AND-BRACES bound. The per-class and
// per-item caps above are what make the rendered answer fit
// prompt.MaxCrewMessageBytes BY CONSTRUCTION; this is the last resort if they
// ever stop being sufficient, and it MARKS the cut rather than silently
// dropping bytes (writeUntrustedCrewMessages would otherwise truncate the
// whole message with no signal the historian could be blamed for).
//
// The budget accounts for the way crewMessageForPrompt flattens the payload —
// "summary: <s>\ndetail: <d>" — and for the per-line "| " prefix
// sanitizeUntrustedComment adds INSIDE the cap. The line count is taken from
// the UNTRUNCATED detail, so truncating can only ever leave more headroom.
func boundHistorianDetail(summary, detail string) string {
	lines := strings.Count(summary, "\n") + strings.Count(detail, "\n") + 2
	budget := prompt.MaxCrewMessageBytes -
		len("summary: ") - len(summary) - 1 - len("detail: ") - 2*lines
	if budget < len(historianTruncatedMarker) {
		budget = len(historianTruncatedMarker)
	}
	if len(detail) <= budget {
		return detail
	}
	cut := budget - len(historianTruncatedMarker)
	if cut < 0 {
		cut = 0
	}
	return strings.ToValidUTF8(detail[:cut], "") + historianTruncatedMarker
}
