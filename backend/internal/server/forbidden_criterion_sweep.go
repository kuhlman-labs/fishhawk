package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// The forbidden-criterion rule of the plan-gate surface sweep (#3620).
//
// Every preset's implement stage carries `forbidden_paths` for good reason —
// an implement agent must not rewrite the governance document that constrains
// it, so `.fishhawk/**`, `.github/workflows/**`, `.gitlab-ci.yml`, `LICENSE`
// and `NOTICE` are off limits. An ACCEPTANCE CRITERION demanding a change to
// one of those paths is therefore unsatisfiable from inside the loop: the
// constraint is enforced against the REAL diff, so the implement agent cannot
// touch the file and every review round re-raises the same gap (run f199dcf1:
// flagged six times across three rounds, unfixable every time). The defect is
// undetectable at authoring time and rediscovered by paid reviewers.
//
// Like the #3410 audit-category rule this is PROSE-triggered, not
// path-triggered: it reads each acceptance criterion's statement and
// verify_hint for a path-shaped token and matches it against the run's
// resolved implement-stage forbidden_paths using backend/internal/policy —
// the SAME matcher the post-implement gate runs, so the plan-time verdict is
// identical to the verdict the implement stage would produce for the same
// path. The finding rides the existing plan_surface_sweep entry, so the rule
// introduces no audit category and no issue-comment surface of its own.
//
// The heuristic bound, stated honestly: the gate MEASURES only that a
// criterion NAMES a forbidden path; it reads acceptance-criteria prose ONLY to
// find one. A criterion phrased "the verify gate enforces the new guard"
// requires a forbidden edit without naming any path and is NOT caught, and
// summary/risks prose is deliberately not scanned because a plan legitimately
// discussing the forbidden-paths constraint would self-flag.
//
// Whether the criterion REQUIRES AN EDIT to the path is a separate question
// (#5499). The finding's ForbiddenInScope bit asserts it — the review prompt
// then renders the UNENFORCEABLE CRITERION line — when ANY of these holds:
// (a) the plan scopes the path (top-level, sub-plan or split-phase
// scope.files; a directory or glob token such as `.fishhawk/**` matches the
// scoped files under it); (b) the criterion's own statement or verify_hint
// names the path as something to change (namesPathAsChange); (c) an approach
// step names it. Otherwise the bit is false and the prompt renders the
// FORBIDDEN PATH NAMED advisory, which asks the reviewer to judge. That
// not-asserted outcome is an INFERENCE, not a measurement: the two observed
// evidence_conflict runs named the forbidden path as an output value and as a
// runtime-state read target, so a criterion nothing in the plan frames as an
// edit is more likely naming the path as data — but the classifier is a
// keyword heuristic, so the advisory keeps the two-merge-request remedy in
// front of the reviewer rather than dropping the finding.
//
// A criterion that names a forbidden path for a reason OTHER than requiring an
// edit to it clears via a plan-declared surface_sweep_exemptions entry
// {pattern: surfacePatternForbiddenCriterion, sibling: <path>} with a
// reviewer-challengeable reason. Advisory in both directions: never a refusal.
const (
	// surfacePatternForbiddenCriterion is the finding's Pattern and the
	// exemption key a plan declares to suppress it.
	surfacePatternForbiddenCriterion = "acceptance criterion requires a forbidden path"
	// forbiddenCriterionSweepMaxFindings caps the findings per plan so a prose
	// blizzard cannot flood the gate evidence.
	forbiddenCriterionSweepMaxFindings = 10
)

// criterionPathMention is one path-shaped token and where in the plan's
// acceptance criteria it was named. Location is
// "acceptance_criteria[<id>].statement" or
// "acceptance_criteria[<id>].verify_hint".
type criterionPathMention struct {
	Path     string
	Location string
	// CriterionID is the sort key that makes the finding order deterministic
	// independently of the location string's shape.
	CriterionID string
	// NamedAsChange is J1 (b) of #5499: the criterion's statement or
	// verify_hint names the path as something to change (namesPathAsChange).
	// When the same path is named in both fields the mention collapses onto
	// the statement and this is the OR of both fields.
	NamedAsChange bool
}

// The two bounded capture shapes. A path-shaped token is captured ONLY when it
// is backticked/quoted (shape A) or carries a forward slash (shape B, the
// post-capture pathTokenHasSlash filter). A BARE unquoted word is deliberately
// NOT captured: prose mentioning the word LICENSE or NOTICE would otherwise
// match the same-named forbidden glob. Both shapes anchor the capture at a
// non-word left boundary so a longer or mixed-case identifier yields the WHOLE
// token rather than a matching suffix (`foo.fishhawk/bar` never yields
// `.fishhawk/bar`), exactly as the audit-category shapes do.
var (
	// Shape A, quoted: the token sits inside a backtick, or a straight or
	// curly single/double quote.
	criterionQuotedPathRE = regexp.MustCompile(
		"[`\"'‘“]([A-Za-z0-9._][A-Za-z0-9._/*-]*)[`\"'’”]")
	// Shape B, bare: a token at a non-word left boundary. The slash
	// requirement is the post-capture filter (Go regexp has no lookahead, and
	// requiring the slash inside the pattern would let a capture start mid-
	// identifier at the segment before the slash).
	criterionBarePathRE = regexp.MustCompile(
		"(?:^|[^A-Za-z0-9_./])([A-Za-z0-9._][A-Za-z0-9._/*-]*)")
)

// pathTokenHasSlash is shape B's post-capture filter: a bare token qualifies
// only when it carries a path separator.
func pathTokenHasSlash(tok string) bool { return strings.Contains(tok, "/") }

// extractCriterionPathTokens returns the path-shaped tokens in one prose
// field, deduplicated with first-seen order preserved. Quoted tokens are taken
// verbatim; a bare token has ONE trailing sentence-final period stripped
// (`see docs/spec/workflow-v2.md.` names the file, not `…md.`) and must carry
// a slash to qualify.
func extractCriterionPathTokens(text string) []string {
	if text == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(tok string) {
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for _, m := range criterionQuotedPathRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range criterionBarePathRE.FindAllStringSubmatch(text, -1) {
		tok := strings.TrimSuffix(m[1], ".")
		if !pathTokenHasSlash(tok) {
			continue
		}
		add(tok)
	}
	return out
}

// planCriterionPathMentions walks verification.acceptance_criteria in declared
// order and, per criterion, extracts tokens from statement then verify_hint.
// Duplicates WITHIN one criterion collapse (the same path named in both fields
// draws one finding, attributed to the statement); the same path in two
// criteria draws one finding each, because each criterion is separately
// unenforceable.
func planCriterionPathMentions(p *plan.Plan) []criterionPathMention {
	var out []criterionPathMention
	for _, c := range p.Verification.AcceptanceCriteria {
		seen := make(map[string]int)
		for _, field := range []struct{ suffix, text string }{
			{"statement", c.Statement},
			{"verify_hint", c.VerifyHint},
		} {
			for _, tok := range extractCriterionPathTokens(field.text) {
				asChange := namesPathAsChange(field.text, tok)
				if i, ok := seen[tok]; ok {
					out[i].NamedAsChange = out[i].NamedAsChange || asChange
					continue
				}
				seen[tok] = len(out)
				out = append(out, criterionPathMention{
					Path:          tok,
					Location:      fmt.Sprintf("acceptance_criteria[%s].%s", c.ID, field.suffix),
					CriterionID:   c.ID,
					NamedAsChange: asChange,
				})
			}
		}
	}
	return out
}

// pathChangeWords are the verbs that frame a named path as something the
// change edits ("the gate is wired in `x`", "add a step to `x`").
// pathReadWords frame it as data: an output value, a read target or a
// command argument ("prints `x`", "jq '.s' x"). Both are lowercase.
var (
	pathChangeWords = wordSet("add adds added adding edit edits edited editing modify modifies modified modifying " +
		"update updates updated updating change changes changed changing wire wires wired wiring rewrite rewrites " +
		"rewrote rewritten create creates created creating delete deletes deleted deleting remove removes removed " +
		"removing rename renames renamed append appends appended insert inserts inserted extend extends extended " +
		"register registers registered patch patches patched amend amends amended bump bumps bumped declare " +
		"declares declared configure configures configured gain gains gained")
	pathReadWords = wordSet("print prints printed printing report reports reported reporting list lists listed " +
		"listing output outputs read reads reading cat jq grep head tail ls stat show shows shown showing display " +
		"displays displayed emit emits emitted return returns returned log logs logged echo echoes parse parses " +
		"parsed load loads loaded inspect inspects inspected query queries queried mention mentions mentioned " +
		"name names named cite cites cited reference references referenced contain contains containing against " +
		"value values exists")
	// pathSubjectFillers may precede a path at the start of a clause without
	// moving it out of subject position ("The file `x` runs …").
	pathSubjectFillers = wordSet("the a an file its this that and then also")
)

// pathChangeLookback bounds how many words before a named path the change /
// read classification reads.
const pathChangeLookback = 6

func wordSet(words string) map[string]bool {
	out := make(map[string]bool)
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

// proseWords splits text into lowercase words with surrounding quote and
// bracket punctuation stripped, dropping words that strip to nothing.
func proseWords(text string) []string {
	var out []string
	for _, w := range strings.Fields(text) {
		w = strings.ToLower(strings.Trim(w, "`\"'‘’“”()[]{},:;!?"))
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

// clauseStart returns the offset just past the last clause boundary in
// prefix: a sentence-final punctuation mark followed by a space, a
// semicolon, a colon or a newline. A path's own dots never end a clause.
func clauseStart(prefix string) int {
	start := 0
	for _, sep := range []string{". ", "? ", "! ", "; ", ": ", "\n"} {
		if i := strings.LastIndex(prefix, sep); i >= 0 && i+len(sep) > start {
			start = i + len(sep)
		}
	}
	return start
}

// namesPathAsChange is J1 (b) of #5499: whether one prose field names tok as
// something to change. A keyword heuristic, read per occurrence of tok:
//
//   - a change verb right after the path ("the block in `x` declares …"), or
//     a passive one ("`x` is updated"), decides first as a change; a passive
//     read ("`x` is read by") decides as not a change;
//   - a path in SUBJECT position — only fillers such as "the" or "file"
//     before it in its clause — is a change: the criterion states what the
//     file itself does or holds ("deploy/x.yml runs make lint");
//   - otherwise the nearest change or read word in the pathChangeLookback
//     words before it, within its clause, decides ("is wired in `x`" is a
//     change; "prints `x`" and "jq '.s' x" are not);
//   - no deciding word means not a change.
func namesPathAsChange(text, tok string) bool {
	for off := 0; off < len(text); {
		i := strings.Index(text[off:], tok)
		if i < 0 {
			return false
		}
		at := off + i
		off = at + len(tok)
		if occurrenceNamesChange(text[:at], text[at+len(tok):]) {
			return true
		}
	}
	return false
}

func occurrenceNamesChange(before, after string) bool {
	next := proseWords(after)
	if len(next) >= 1 && pathChangeWords[next[0]] {
		return true
	}
	if len(next) >= 2 {
		switch next[0] {
		case "is", "are", "was", "were", "be", "been", "gets", "get":
			if pathChangeWords[next[1]] {
				return true
			}
			if pathReadWords[next[1]] {
				return false
			}
		}
	}
	prev := proseWords(before[clauseStart(before):])
	subject := true
	for _, w := range prev {
		if !pathSubjectFillers[w] {
			subject = false
			break
		}
	}
	if subject {
		return true
	}
	for n, i := 0, len(prev)-1; i >= 0 && n < pathChangeLookback; i, n = i-1, n+1 {
		switch {
		case pathChangeWords[prev[i]]:
			return true
		case pathReadWords[prev[i]]:
			return false
		}
	}
	return false
}

// pathCoveredBy is J1 (a)/(c) of #5499: whether path tok — a concrete path, a
// directory or a glob token — is covered by any entry of paths, which may
// themselves be concrete, directory or glob entries. Exact match, a doublestar
// glob on either side, or a directory prefix on either side all count, so a
// criterion naming `.fishhawk/**` is covered by a scoped
// `.fishhawk/workflows.yaml` and a criterion naming `.fishhawk/workflows.yaml`
// is covered by an approach step naming `.fishhawk/`.
func pathCoveredBy(tok string, paths []string) bool {
	tok = strings.TrimPrefix(toSlashPath(tok), "./")
	if tok == "" {
		return false
	}
	for _, p := range paths {
		p = strings.TrimPrefix(toSlashPath(p), "./")
		if p == "" {
			continue
		}
		if p == tok || pathUnder(p, tok) || pathUnder(tok, p) {
			return true
		}
		if ok, err := doublestar.Match(tok, p); err == nil && ok {
			return true
		}
		if ok, err := doublestar.Match(p, tok); err == nil && ok {
			return true
		}
	}
	return false
}

// pathUnder reports whether path sits under the directory dir.
func pathUnder(path, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return dir != "" && strings.HasPrefix(path, dir+"/")
}

// planApproachPathTokens returns every path-shaped token the plan's approach
// steps name, with the same extraction the criteria use.
func planApproachPathTokens(p *plan.Plan) []string {
	var out []string
	for _, st := range p.Approach {
		out = append(out, extractCriterionPathTokens(st.Description)...)
	}
	return out
}

// forbiddenGlobForPaths maps each candidate path to the FIRST forbidden glob
// that matches it, using policy.Evaluate — the same matcher the post-implement
// gate runs, so the plan-time verdict is identical for the same path. Grounded
// in the code rather than inferred: policy's checkForbidden delegates to
// matchAny, which calls doublestar.PathMatch(pattern, f.Path) and never reads
// f.Status, so feeding it a synthetic modified-status diff of prose-extracted
// tokens is sound (the same property mapFileOperation already documents).
//
// A MALFORMED glob in the workflow spec yields a policy Violation carrying an
// "invalid pattern" Detail and NO Files, so it contributes no mapping and
// cannot crash the rule. Iteration is over the violation slice policy.Evaluate
// returns in pattern order, so "first matching glob" is the first DECLARED
// forbidden path — deterministic.
func forbiddenGlobForPaths(paths []string, forbidden []string) map[string]string {
	if len(paths) == 0 || len(forbidden) == 0 {
		return nil
	}
	diff := policy.Diff{}
	for _, p := range paths {
		diff.ChangedFiles = append(diff.ChangedFiles, policy.ChangedFile{
			Path:   p,
			Status: policy.StatusModified,
		})
	}
	out := make(map[string]string)
	for _, v := range policy.Evaluate(diff, policy.Constraints{ForbiddenPaths: forbidden}) {
		if v.Constraint != "forbidden_paths" || len(v.Files) == 0 {
			continue
		}
		glob := forbiddenGlobFromDetail(v.Detail, forbidden)
		if glob == "" {
			continue
		}
		for _, f := range v.Files {
			if _, ok := out[f]; !ok {
				out[f] = glob
			}
		}
	}
	return out
}

// forbiddenGlobFromDetail recovers WHICH declared glob a forbidden_paths
// violation belongs to. policy.Violation carries the pattern only inside its
// human-readable Detail (`pattern "x" matched`), and keying on Detail TEXT is
// the brittleness the prompt renderer's comment warns against — so this
// resolves the glob by testing each DECLARED pattern for containment in the
// quoted form policy emits, and returns "" when none matches (the caller then
// records no mapping rather than a guess).
func forbiddenGlobFromDetail(detail string, forbidden []string) string {
	for _, pat := range forbidden {
		if strings.Contains(detail, fmt.Sprintf("%q", pat)) {
			return pat
		}
	}
	return ""
}

// evaluateForbiddenCriterionRule is the PURE evaluator (no receiver, no I/O),
// mirroring evaluateAuditCategoryRule. forbidden is the run's resolved
// implement-stage forbidden_paths (the caller reads it fail-open via
// resolveImplementConstraints). It returns nil, nil when p is nil, when
// forbidden is empty, when the plan declares no acceptance criteria, and when
// no criterion names a path any glob forbids.
//
// A matching exemption {surfacePatternForbiddenCriterion, <the token path>}
// suppresses EVERY finding for that token and yields ONE AppliedExemption
// carrying the plan's reason. Otherwise one finding per distinct (criterion,
// token) pair, sorted by criterion id then token and capped at
// forbiddenCriterionSweepMaxFindings, each with Pattern, TriggerPath naming
// the token and its location, MissingSiblings nil (nothing is missing from
// scope — the path is structurally unreachable), ForbiddenPath /
// ForbiddenPattern carrying the token and the glob that forbids it, and
// ForbiddenInScope carrying whether the plan puts that path in play as
// something to change (J1 of #5499: scoped, named as a change target by the
// criterion, or named by an approach step).
func evaluateForbiddenCriterionRule(p *plan.Plan, forbidden []string, exemptions []plan.SurfaceSweepExemption) ([]SurfaceSweepFinding, []AppliedExemption) {
	if p == nil || len(forbidden) == 0 || len(p.Verification.AcceptanceCriteria) == 0 {
		return nil, nil
	}

	mentions := planCriterionPathMentions(p)
	if len(mentions) == 0 {
		return nil, nil
	}
	candidates := make([]string, 0, len(mentions))
	seen := make(map[string]bool, len(mentions))
	for _, m := range mentions {
		if seen[m.Path] {
			continue
		}
		seen[m.Path] = true
		candidates = append(candidates, m.Path)
	}

	matched := forbiddenGlobForPaths(candidates, forbidden)
	if len(matched) == 0 {
		return nil, nil
	}

	// J1 of #5499, per slash-normalized forbidden token: (a) scoped is the
	// plan's own scope union, and approachNamed is (c), an approach step
	// naming it. (b) rides each mention as NamedAsChange.
	scopeUnion := scopedPaths(p)
	approachTokens := planApproachPathTokens(p)
	scoped := make(map[string]bool, len(matched))
	approachNamed := make(map[string]bool, len(matched))
	for path := range matched {
		key := toSlashPath(path)
		scoped[key] = pathCoveredBy(key, scopeUnion)
		approachNamed[key] = pathCoveredBy(key, approachTokens)
	}

	// Index the plan's exemptions for this rule by slash-normalized path.
	exByPath := make(map[string]plan.SurfaceSweepExemption)
	for _, e := range exemptions {
		if e.Pattern != surfacePatternForbiddenCriterion {
			continue
		}
		exByPath[toSlashPath(e.Sibling)] = e
	}

	var hits []criterionPathMention
	exemptedReason := make(map[string]string)
	for _, m := range mentions {
		if _, ok := matched[m.Path]; !ok {
			continue
		}
		if e, isEx := exByPath[toSlashPath(m.Path)]; isEx {
			// Suppressed: the reason is recorded once per token below, so a
			// reviewer sees each challengeable justification exactly once.
			exemptedReason[m.Path] = e.Reason
			continue
		}
		hits = append(hits, m)
	}

	// Applied exemptions in first-mention (candidate) order for determinism.
	var applied []AppliedExemption
	for _, path := range candidates {
		reason, ok := exemptedReason[path]
		if !ok {
			continue
		}
		applied = append(applied, AppliedExemption{
			Pattern: surfacePatternForbiddenCriterion,
			Sibling: toSlashPath(path),
			Reason:  reason,
		})
	}

	if len(hits) == 0 {
		return nil, applied
	}

	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].CriterionID != hits[b].CriterionID {
			return hits[a].CriterionID < hits[b].CriterionID
		}
		return hits[a].Path < hits[b].Path
	})
	if len(hits) > forbiddenCriterionSweepMaxFindings {
		hits = hits[:forbiddenCriterionSweepMaxFindings]
	}
	findings := make([]SurfaceSweepFinding, 0, len(hits))
	for _, m := range hits {
		f := SurfaceSweepFinding{
			Pattern:          surfacePatternForbiddenCriterion,
			TriggerPath:      fmt.Sprintf("path %q named at %s", m.Path, m.Location),
			ForbiddenPath:    toSlashPath(m.Path),
			ForbiddenPattern: matched[m.Path],
			ForbiddenInScope: scoped[toSlashPath(m.Path)],
		}
		if m.NamedAsChange {
			f.ForbiddenInScope = true
		}
		if approachNamed[toSlashPath(m.Path)] {
			f.ForbiddenInScope = true
		}
		findings = append(findings, f)
	}
	return findings, applied
}
