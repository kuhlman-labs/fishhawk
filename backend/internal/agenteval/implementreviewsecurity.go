// Security persona catch arms (E80.17 / #4314): does the security reviewer
// persona — rendered through the SAME production persona builder the review
// loop uses (personaprompt.Build, kind implement_review), over the REAL
// docs/review/security-remit.md and, for the security arm, the REAL
// docs/review/security-conventions.md rendered through the production
// review-conventions path — catch a sensitive-path diff carrying a planted
// security defect, and does it stay quiet on a clean sensitive-path diff?
//
// Two arms share one corpus (testdata/planreview-persona-corpus/security/):
//
//   - ArmSecurity renders the remit AND the repository's security convention
//     and runs every case;
//   - ArmSecurityPortability renders the remit alone — the stand-in for a
//     repository that declares no convention — and runs the clean cases only.
//
// Each case's persona_case.json carries the issue text, a COMPLETE standard_v1
// plan, the unified diff under review, and either a catch rule (anchored at a
// changed file) or a clean expectation. Scoring is catch rule v5
// (planreviewcatch.go) through the shared classifyCatchConcerns, AFTER the
// review loop's own ingest order (ClassifyPersonaTrialForRound: convention
// clamp, marker scrub, quote verification, the persona's medium cap). The
// corpus markers.json then annotates every stored concern: the first
// repository-specific marker it names and whether it asserts a control is
// absent (SecurityMarkers.Annotate).
//
// The documents are read from a checkout (repoRoot) through the persona arm's
// filesystem fetcher inside a real repodoc.Resolver at the production
// DefaultMaxBytes cap, pinned to personaFixtureCommit. Everything here is
// offline and tested with fake senders; the live measurement belongs to the
// evidence recorder.

package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/personaprompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The two security persona arms.
const (
	// ArmSecurity renders the security remit plus the repository's security
	// review convention and runs every case.
	ArmSecurity = "security"
	// ArmSecurityPortability renders the security remit alone (a repository
	// with no convention) and runs the clean cases only.
	ArmSecurityPortability = "security_portability"
)

// The fixed security declaration the arms render under: the values a
// reviewer_personas.security entry and a review_conventions entry would
// declare at enablement. They are literals, never derived from the checkout,
// so the rendered bytes depend only on the documents' content.
const (
	personaSecurityName        = "security"
	personaSecurityRemitPath   = "docs/review/security-remit.md"
	personaSecuritySeverityCap = "medium"
	// The security review convention (the ArmSecurity arm only).
	personaSecurityConventionName        = "security-conventions"
	personaSecurityConventionPath        = "docs/review/security-conventions.md"
	personaSecurityConventionSeverityCap = "medium"
	// securityConventionHeadingPrefix is the repodoc framing heading the server
	// gives a review convention's declaration (server/review_conventions.go
	// reviewConventionHeadingPrefix).
	securityConventionHeadingPrefix = "Review convention "
	// securityMarkersFile is the markers file at the corpus root.
	securityMarkersFile = "markers.json"
)

// securityCatchGeneratorSystemPrompt is the fixed harness instruction the
// security arms send to the generator. It names the persona verdict contract,
// including quoted_passage and document_ref, so the quote-verification control
// has something to verify. It carries no case content (the leak test pins it).
const securityCatchGeneratorSystemPrompt = `You are a Fishhawk implement-review agent reviewing as a reviewer persona. Read the stage prompt the user message contains and produce the review verdict it asks for as a JSON object carrying verdict, concerns (each with severity, category and note, plus quoted_passage and document_ref when the concern rests on a passage of an injected repository document), and optional free_form. Respond with the verdict JSON only.`

// The security remit's lenses, in remit order. A defect case names exactly
// one of them.
const (
	SecurityLensTrustBoundaries = "trust_boundaries"
	SecurityLensAuthnAuthz      = "authn_authz"
	SecurityLensSecretCustody   = "secret_custody"
	SecurityLensPermissionScope = "permission_scope"
	SecurityLensFailClosed      = "fail_closed"
	SecurityLensRecordIntegrity = "record_integrity"
	SecurityLensSupplyChain     = "supply_chain"
)

// SecurityLenses is every remit lens, in remit order.
var SecurityLenses = []string{
	SecurityLensTrustBoundaries,
	SecurityLensAuthnAuthz,
	SecurityLensSecretCustody,
	SecurityLensPermissionScope,
	SecurityLensFailClosed,
	SecurityLensRecordIntegrity,
	SecurityLensSupplyChain,
}

// The security case kinds and the expectation each one implies.
const (
	SecurityKindDefect         = "defect"
	SecurityKindCleanSensitive = "clean_sensitive"
)

var securityKindExpect = map[string]string{
	SecurityKindDefect:         CatchExpectCatch,
	SecurityKindCleanSensitive: CatchExpectClean,
}

// SecurityAnchorDiffFile is the one anchor kind a security catch case
// declares: the changed file carrying the planted defect.
const SecurityAnchorDiffFile = "diff_file"

// SecurityCaseAnchor names WHICH changed file a catch case's defect sits in.
type SecurityCaseAnchor struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// SecurityCaseInput is the persona_case.json shape of one security case.
type SecurityCaseInput struct {
	Persona string `json:"persona"`
	Kind    string `json:"kind"`
	Expect  string `json:"expect"`
	// Lens is the remit lens a defect case's planted defect falls under; a
	// clean case carries none.
	Lens       string `json:"lens,omitempty"`
	IssueTitle string `json:"issue_title"`
	IssueBody  string `json:"issue_body"`
	// Plan is a COMPLETE standard_v1 plan object (validated by plan.Parse).
	Plan json.RawMessage `json:"plan"`
	// Diff is the unified patch under review: one or more `diff --git a/<p>
	// b/<p>` file sections (no renames), every path in the plan's scope.
	Diff          string   `json:"diff"`
	DefectPhrases []string `json:"defect_phrases"`

	// Catch-case fields.
	Anchor              *SecurityCaseAnchor   `json:"anchor,omitempty"`
	CatchProbes         []string              `json:"catch_probes,omitempty"`
	AttributionPhrases  []string              `json:"attribution_phrases,omitempty"`
	CatchingExamples    []CatchExampleConcern `json:"catching_examples,omitempty"`
	NonCatchingExamples []CatchExampleConcern `json:"non_catching_examples,omitempty"`

	// Clean-case field.
	FalseAlarmExamples []CatchExampleConcern `json:"false_alarm_examples,omitempty"`
}

// SecurityDiffFile is one changed file of a case's diff, with its git status
// (policy.Status letters: A added, M modified, D deleted).
type SecurityDiffFile struct {
	Path   string
	Status string
}

// SecurityCatchCase is one fully-loaded security case.
type SecurityCatchCase struct {
	// Name is the case directory name — the per-case key everywhere.
	Name string
	// Input is the decoded persona_case.json.
	Input SecurityCaseInput
	// Plan is Input.Plan parsed by the real plan.Parse.
	Plan *plan.Plan
	// Files is the diff's changed files, in diff order.
	Files []SecurityDiffFile
	// Raw is the exact committed persona_case.json bytes (an
	// evidence-fingerprint input).
	Raw []byte
}

// Expect is the case's expectation class (CatchExpectCatch or
// CatchExpectClean).
func (c SecurityCatchCase) Expect() string { return c.Input.Expect }

// Lens is a defect case's remit lens ("" for a clean case).
func (c SecurityCatchCase) Lens() string { return c.Input.Lens }

// CatchRule returns the case's catch rule v5: for a catch case the probes and
// the attribution phrases (the FILE half of "match by file and lens"; the lens
// half lives in the probes); for a clean case a clean rule. Neither carries a
// category predicate: the security remit reserves no concern category and the
// implement-review schema teaches none for it, so EVERY concern on a clean
// diff counts as a false alarm (fail-safe) and a catch is not lost to a
// mislabelled category.
func (c SecurityCatchCase) CatchRule() CatchRule {
	return securityCatchRuleFor(c.Input)
}

func securityCatchRuleFor(in SecurityCaseInput) CatchRule {
	if in.Expect == CatchExpectClean {
		return CatchRule{Clean: true}
	}
	return CatchRule{Probes: in.CatchProbes, AttributionPhrases: in.AttributionPhrases}
}

// SecurityMarkers is the corpus markers.json: the vocabulary the security arms
// annotate every stored concern with.
type SecurityMarkers struct {
	// RepoSpecificMarkers name this repository's own controls, files and
	// identifiers: each occurs in the security convention and never in the
	// generic remit, so a portability-arm concern naming one is about a
	// control that reviewer was never shown. Matched as case-folded
	// substrings.
	RepoSpecificMarkers []string `json:"repo_specific_markers"`
	// GenericControlPhrases name a security control in general terms. Matched
	// case-folded and WORD-BOUNDED, so "pin" never matches "pinned".
	GenericControlPhrases []string `json:"generic_control_phrases"`
	// AbsencePhrases assert that something is missing. Matched case-folded and
	// word-bounded.
	AbsencePhrases []string `json:"absence_phrases"`
	// Raw is the exact markers.json bytes (an evidence-fingerprint input).
	Raw []byte `json:"-"`
}

// RepoMarkerIn returns the first repository-specific marker (in markers.json
// order) occurring case-folded in text, or "".
func (m SecurityMarkers) RepoMarkerIn(text string) string {
	lt := strings.ToLower(text)
	for _, mk := range m.RepoSpecificMarkers {
		lm := strings.ToLower(strings.TrimSpace(mk))
		if lm != "" && strings.Contains(lt, lm) {
			return mk
		}
	}
	return ""
}

// Annotate sets c's RepoSpecificMarker (the first repository-specific marker
// its category or note names) and AbsentControl (an absence phrase
// co-occurring with a generic control phrase or a repository-specific marker).
func (m SecurityMarkers) Annotate(c *CatchTrialConcern) {
	text := c.Category + "\n" + c.Note
	c.RepoSpecificMarker = m.RepoMarkerIn(text)
	c.AbsentControl = anyWordBounded(text, m.AbsencePhrases) &&
		(c.RepoSpecificMarker != "" || anyWordBounded(text, m.GenericControlPhrases))
}

func (m SecurityMarkers) annotateTrial(t *CatchTrial) {
	for i := range t.Concerns {
		m.Annotate(&t.Concerns[i])
	}
}

// empty reports whether any marker list is empty — a zero SecurityMarkers
// would annotate nothing and silently disarm the portability refusal.
func (m SecurityMarkers) empty() bool {
	return len(m.RepoSpecificMarkers) == 0 || len(m.GenericControlPhrases) == 0 || len(m.AbsencePhrases) == 0
}

// anyWordBounded reports whether any phrase occurs word-bounded in text.
func anyWordBounded(text string, phrases []string) bool {
	for _, p := range phrases {
		if containsWordBounded(text, p) {
			return true
		}
	}
	return false
}

// containsWordBounded reports whether phrase occurs case-folded in text with
// no word character (letter, digit, underscore, or a non-ASCII byte)
// immediately before or after it.
func containsWordBounded(text, phrase string) bool {
	lt := strings.ToLower(text)
	lp := strings.ToLower(strings.TrimSpace(phrase))
	if lp == "" {
		return false
	}
	for off := 0; off <= len(lt); {
		i := strings.Index(lt[off:], lp)
		if i < 0 {
			return false
		}
		start := off + i
		end := start + len(lp)
		if !isWordByteAt(lt, start-1) && !isWordByteAt(lt, end) {
			return true
		}
		off = start + 1
	}
	return false
}

func isWordByteAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	b := s[i]
	return b == '_' || b >= 0x80 || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// securityLensAliases are category spellings a reviewer uses for a security
// finding without naming the remit lens id.
var securityLensAliases = []string{
	"authz", "authn", "authorization", "authentication", "auth",
	"secret", "secrets", "credential", "credentials", "credential_exposure",
	"permission", "permissions", "least_privilege",
	"trust_boundary", "untrusted_input", "prompt_injection", "injection",
	"fail_open", "audit", "audit_integrity", "supply-chain", "pinning", "pin",
}

// SecurityLensCategory reports whether a concern category names the security
// lens: after normalization (case-folded, trimmed, spaces and hyphens folded
// to underscores) it contains "security", or equals a remit lens id or a
// common alias (authz, secrets, permissions, supply_chain, ...). It is the
// definition the false-alarm category partition reads: a clean-case false
// alarm is a security-lens alarm when this holds, an other-category alarm
// (a standard-criteria concern) otherwise.
func SecurityLensCategory(category string) bool {
	n := normalizeSecurityCategory(category)
	if n == "" {
		return false
	}
	if strings.Contains(n, "security") || slices.Contains(SecurityLenses, n) {
		return true
	}
	for _, a := range securityLensAliases {
		if normalizeSecurityCategory(a) == n {
			return true
		}
	}
	return false
}

func normalizeSecurityCategory(category string) string {
	n := strings.ToLower(strings.TrimSpace(category))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(n)
}

// parseSecurityDiff splits a unified patch into its changed files. Each file
// section opens with `diff --git a/<p> b/<p>` (the same path on both sides —
// a rename is refused as malformed); `new file mode` marks it A, `deleted file
// mode` D, anything else M. It refuses an empty diff, a diff with no file
// header, a malformed header and a path changed twice.
func parseSecurityDiff(diff string) ([]SecurityDiffFile, error) {
	if strings.TrimSpace(diff) == "" {
		return nil, errors.New("diff is empty")
	}
	var files []SecurityDiffFile
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			p, ok := parseDiffGitHeader(strings.TrimPrefix(line, "diff --git "))
			if !ok {
				return nil, fmt.Errorf("malformed diff file header %q (want `diff --git a/<path> b/<path>`, the same path on both sides)", line)
			}
			if seen[p] {
				return nil, fmt.Errorf("diff changes %s twice", p)
			}
			seen[p] = true
			files = append(files, SecurityDiffFile{Path: p, Status: "M"})
		case len(files) > 0 && strings.HasPrefix(line, "new file mode"):
			files[len(files)-1].Status = "A"
		case len(files) > 0 && strings.HasPrefix(line, "deleted file mode"):
			files[len(files)-1].Status = "D"
		}
	}
	if len(files) == 0 {
		return nil, errors.New("diff has no `diff --git` file header")
	}
	return files, nil
}

// parseDiffGitHeader parses `a/<p> b/<p>` with the same non-empty path on
// both sides.
func parseDiffGitHeader(rest string) (string, bool) {
	if !strings.HasPrefix(rest, "a/") {
		return "", false
	}
	body := rest[len("a/"):]
	n := len(body) - len(" b/")
	if n < 2 || n%2 != 0 {
		return "", false
	}
	p := body[:n/2]
	if body[n/2:n/2+len(" b/")] != " b/" || body[n/2+len(" b/"):] != p || strings.TrimSpace(p) != p || strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// renderSecurityChangedFiles renders a case's changed files in the server's
// renderDiffForReview format ("- <status> <path>" per file).
func renderSecurityChangedFiles(files []SecurityDiffFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "- %s %s\n", f.Status, f.Path)
	}
	return b.String()
}

// securityShownText is the case-folded text a reviewer of this case is shown
// verbatim: the issue title and body, every key and value of the plan, and
// the WHOLE diff (removed lines included).
func securityShownText(in SecurityCaseInput) (string, error) {
	shown, err := catchShownText(PlanReviewCatchInput{IssueTitle: in.IssueTitle, IssueBody: in.IssueBody, Plan: in.Plan})
	if err != nil {
		return "", err
	}
	return shown + "\n" + strings.ToLower(in.Diff), nil
}

// LoadSecurityCatchCorpus loads every case under dir and the corpus
// markers.json, checking the markers' premises against the REAL remit and
// convention read from repoRoot. It FAILS CLOSED with an error naming the case
// (or markers.json) on:
//
//	(a) an absent corpus dir, or zero cases
//	(b) a case with no persona_case.json, malformed JSON, an unknown field or
//	    trailing content after the JSON value
//	(c) persona differing from "security"
//	(d) an unknown kind or expect, a kind/expect mismatch, a defect case
//	    without a known lens, or a clean case with a lens
//	(e) an empty issue_title or issue_body, or a plan plan.Parse rejects
//	(f) an empty diff, a diff with no `diff --git` file header or a malformed
//	    one, a changed path outside the plan's scope.files, or a diff touching
//	    the remit or the convention (production would then render the
//	    modified-conventions notice)
//	(g) empty or blank defect_phrases, or a defect phrase absent from the
//	    issue, the plan and the diff
//	(h) catch-only fields on a clean case, or clean-only fields on a catch case
//	(i) catch: an absent anchor, an anchor kind other than diff_file, or an
//	    anchored path not among the diff's files
//	(j) catch: empty or blank catch_probes or attribution_phrases
//	(k) catch: an attribution phrase absent from the anchored path, or present
//	    in another changed path
//	(l) catch: empty catching_examples or non_catching_examples; a
//	    catching_example not caught; a non_catching_example caught; a probe
//	    matching a non_catching_example; no non_catching_example naming the
//	    anchor
//	(m) catch: a probe occurring in the issue, the plan or the diff (removed
//	    lines included — the reviewer is shown the whole patch)
//	(n) clean: empty false_alarm_examples, or one not classified false_alarm
//	(o) markers.json absent or malformed; an empty list, a blank or duplicate
//	    entry; a generic control phrase occurring inside another; a
//	    repo-specific marker absent from the convention or present in the
//	    remit; or a repo-specific marker occurring in any clean case's shown
//	    text (so a portability concern naming one can only be about a control
//	    the reviewer was not shown and the clean change did not remove)
func LoadSecurityCatchCorpus(dir, repoRoot string) ([]SecurityCatchCase, SecurityMarkers, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, SecurityMarkers{}, fmt.Errorf("agenteval: security catch corpus dir %q is absent: the security arms cannot run without it", dir) // (a)
		}
		return nil, SecurityMarkers{}, fmt.Errorf("agenteval: security catch corpus dir %q: %w", dir, err)
	}
	var out []SecurityCatchCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := loadSecurityCatchCase(dir, e.Name())
		if err != nil {
			return nil, SecurityMarkers{}, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, SecurityMarkers{}, fmt.Errorf("agenteval: security catch corpus dir %q holds zero cases", dir) // (a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	markers, err := loadSecurityMarkers(dir, repoRoot, out)
	if err != nil {
		return nil, SecurityMarkers{}, err
	}
	return out, markers, nil
}

func loadSecurityCatchCase(dir, name string) (SecurityCatchCase, error) {
	fail := func(format string, args ...any) (SecurityCatchCase, error) {
		return SecurityCatchCase{}, fmt.Errorf("agenteval: security catch case %q: "+format, append([]any{name}, args...)...)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name, "persona_case.json"))
	if err != nil {
		return fail("read persona_case.json: %w", err) // (b)
	}
	var in SecurityCaseInput
	if err := decodeStrictJSON(raw, &in); err != nil {
		return fail("parse persona_case.json: %w", err) // (b)
	}
	if in.Persona != personaSecurityName {
		return fail("persona %q is not the security arms' persona %q", in.Persona, personaSecurityName) // (c)
	}
	wantExpect, ok := securityKindExpect[in.Kind]
	if !ok {
		return fail("unknown kind %q (want %q or %q)", in.Kind, SecurityKindDefect, SecurityKindCleanSensitive) // (d)
	}
	if in.Expect != CatchExpectCatch && in.Expect != CatchExpectClean {
		return fail("unknown expect %q (want %q or %q)", in.Expect, CatchExpectCatch, CatchExpectClean) // (d)
	}
	if in.Expect != wantExpect {
		return fail("kind %q expects %q, got expect %q", in.Kind, wantExpect, in.Expect) // (d)
	}
	if in.Kind == SecurityKindDefect && !slices.Contains(SecurityLenses, in.Lens) {
		return fail("a defect case needs a lens among %v, got %q", SecurityLenses, in.Lens) // (d)
	}
	if in.Kind == SecurityKindCleanSensitive && in.Lens != "" {
		return fail("a clean_sensitive case carries no lens, got %q", in.Lens) // (d)
	}
	if strings.TrimSpace(in.IssueTitle) == "" || strings.TrimSpace(in.IssueBody) == "" {
		return fail("issue_title and issue_body must be non-empty") // (e)
	}
	p, err := plan.Parse(in.Plan)
	if err != nil {
		return fail("plan is not a valid standard_v1 plan: %w", err) // (e)
	}
	files, err := parseSecurityDiff(in.Diff)
	if err != nil {
		return fail("%w", err) // (f)
	}
	scope := personaScopePaths(p)
	for _, f := range files {
		if !slices.Contains(scope, f.Path) {
			return fail("the diff changes %s, which is outside the plan's scope.files", f.Path) // (f)
		}
		if f.Path == personaSecurityRemitPath || f.Path == personaSecurityConventionPath {
			return fail("the diff changes %s: a diff touching the remit or the convention would render the modified-conventions notice production adds", f.Path) // (f)
		}
	}
	shown, err := securityShownText(in)
	if err != nil {
		return fail("plan: %w", err)
	}
	if len(in.DefectPhrases) == 0 {
		return fail("defect_phrases must be non-empty — the prompt-leak self-check cannot tell whether a static persona surface shows this case's planted defect") // (g)
	}
	for i, phrase := range in.DefectPhrases {
		lp := strings.ToLower(strings.TrimSpace(phrase))
		if lp == "" {
			return fail("defect_phrases[%d] is empty — an empty phrase occurs in every prompt", i) // (g)
		}
		if !strings.Contains(shown, lp) {
			return fail("defect phrase %q occurs in neither the issue, the plan nor the diff: a defect phrase must be this case's own content", phrase) // (g)
		}
	}
	c := SecurityCatchCase{Name: name, Input: in, Plan: p, Files: files, Raw: raw}
	if in.Expect == CatchExpectClean {
		err = checkSecurityCleanCase(in)
	} else {
		err = checkSecurityCatchCase(in, files, shown)
	}
	if err != nil {
		return fail("%w", err)
	}
	return c, nil
}

// checkSecurityCatchCase is loader modes (h)-(m) for a catch case.
func checkSecurityCatchCase(in SecurityCaseInput, files []SecurityDiffFile, shown string) error {
	if len(in.FalseAlarmExamples) > 0 {
		return errors.New("a catch case must not carry the clean-case field false_alarm_examples") // (h)
	}
	if in.Anchor == nil {
		return errors.New("a catch case needs an anchor (kind diff_file + path)") // (i)
	}
	if in.Anchor.Kind != SecurityAnchorDiffFile {
		return fmt.Errorf("unknown anchor kind %q (want %q)", in.Anchor.Kind, SecurityAnchorDiffFile) // (i)
	}
	var others []string
	found := false
	for _, f := range files {
		if f.Path == in.Anchor.Path {
			found = true
			continue
		}
		others = append(others, f.Path)
	}
	if !found {
		return fmt.Errorf("anchored path %q is not among the diff's changed files", in.Anchor.Path) // (i)
	}
	if len(in.CatchProbes) == 0 || len(in.AttributionPhrases) == 0 {
		return errors.New("catch_probes and attribution_phrases must be non-empty") // (j)
	}
	for i, probe := range in.CatchProbes {
		if strings.TrimSpace(probe) == "" {
			return fmt.Errorf("catch_probes[%d] is empty — an empty probe matches every concern", i) // (j)
		}
	}
	for i, phrase := range in.AttributionPhrases {
		if strings.TrimSpace(phrase) == "" {
			return fmt.Errorf("attribution_phrases[%d] is empty — an empty phrase attributes every concern", i) // (j)
		}
	}
	for _, phrase := range in.AttributionPhrases {
		lp := strings.ToLower(strings.TrimSpace(phrase))
		if !strings.Contains(strings.ToLower(in.Anchor.Path), lp) {
			return fmt.Errorf("attribution phrase %q is absent from the anchored path %s: it does not name the anchor", phrase, in.Anchor.Path) // (k)
		}
		for _, o := range others {
			if strings.Contains(strings.ToLower(o), lp) {
				return fmt.Errorf("attribution phrase %q also occurs in the changed path %s: it would attribute a concern about that file to the anchor", phrase, o) // (k)
			}
		}
	}
	if len(in.CatchingExamples) == 0 || len(in.NonCatchingExamples) == 0 {
		return errors.New("catching_examples and non_catching_examples must both be non-empty — the probes' discrimination is unproven otherwise") // (l)
	}
	rule := securityCatchRuleFor(in)
	for i, ex := range in.CatchingExamples {
		if _, _, ok := matchCatchConcern(ex.Category, ex.Note, rule); !ok {
			return fmt.Errorf("catching_examples[%d] (%q) is not a catch under catch rule v5: one concern must name the anchored file AND match a catch probe", i, ex.Note) // (l)
		}
	}
	attributed := false
	for i, ex := range in.NonCatchingExamples {
		if _, _, ok := matchCatchConcern(ex.Category, ex.Note, rule); ok {
			return fmt.Errorf("non_catching_examples[%d] (%q) is caught under catch rule v5", i, ex.Note) // (l)
		}
		if probe, ok := matchCatchProbe(ex.Category, ex.Note, in.CatchProbes); ok {
			return fmt.Errorf("catch probe %q matches non_catching_examples[%d] (%q): the probe does not discriminate", probe, i, ex.Note) // (l)
		}
		if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); ok {
			attributed = true
		}
	}
	if !attributed {
		return errors.New("no non_catching_example names the anchored file: the probes' discrimination on concerns that DO name it is unproven") // (l)
	}
	for _, probe := range in.CatchProbes {
		if strings.Contains(shown, strings.ToLower(strings.TrimSpace(probe))) {
			return fmt.Errorf("catch probe %q occurs in the issue, the plan or the diff the reviewer is shown: a concern echoing that wording would score as a catch", probe) // (m)
		}
	}
	return nil
}

// checkSecurityCleanCase is loader modes (h) and (n) for a clean case.
func checkSecurityCleanCase(in SecurityCaseInput) error {
	if in.Anchor != nil || len(in.CatchProbes) > 0 || len(in.AttributionPhrases) > 0 || len(in.CatchingExamples) > 0 || len(in.NonCatchingExamples) > 0 {
		return errors.New("a clean case must not carry catch-case fields (anchor, catch_probes, attribution_phrases, catching_examples, non_catching_examples)") // (h)
	}
	if len(in.FalseAlarmExamples) == 0 {
		return errors.New("false_alarm_examples must be non-empty — the clean rule's discrimination is unproven otherwise") // (n)
	}
	rule := securityCatchRuleFor(in)
	for i, ex := range in.FalseAlarmExamples {
		if classifyCatchConcerns(examplesAsConcerns(ex), 1, rule).Outcome != CatchFalseAlarm {
			return fmt.Errorf("false_alarm_examples[%d] (%q) does not classify %s", i, ex.Note, CatchFalseAlarm) // (n)
		}
	}
	return nil
}

// loadSecurityMarkers is loader mode (o).
func loadSecurityMarkers(dir, repoRoot string, cases []SecurityCatchCase) (SecurityMarkers, error) {
	fail := func(format string, args ...any) (SecurityMarkers, error) {
		return SecurityMarkers{}, fmt.Errorf("agenteval: security catch corpus %s: "+format, append([]any{securityMarkersFile}, args...)...)
	}
	raw, err := os.ReadFile(filepath.Join(dir, securityMarkersFile))
	if err != nil {
		return fail("read: %w", err)
	}
	var m SecurityMarkers
	if err := decodeStrictJSON(raw, &m); err != nil {
		return fail("parse: %w", err)
	}
	m.Raw = raw
	for _, list := range []struct {
		name  string
		items []string
	}{
		{"repo_specific_markers", m.RepoSpecificMarkers},
		{"generic_control_phrases", m.GenericControlPhrases},
		{"absence_phrases", m.AbsencePhrases},
	} {
		if len(list.items) == 0 {
			return fail("%s must be non-empty", list.name)
		}
		seen := map[string]bool{}
		for i, it := range list.items {
			l := strings.ToLower(strings.TrimSpace(it))
			if l == "" {
				return fail("%s[%d] is blank — a blank entry matches every concern", list.name, i)
			}
			if seen[l] {
				return fail("%s lists %q twice", list.name, it)
			}
			seen[l] = true
		}
	}
	for i, a := range m.GenericControlPhrases {
		for j, b := range m.GenericControlPhrases {
			if i != j && strings.Contains(strings.ToLower(strings.TrimSpace(b)), strings.ToLower(strings.TrimSpace(a))) {
				return fail("generic control phrase %q occurs inside %q: keep each phrase distinct so a match names one control", a, b)
			}
		}
	}
	readDoc := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("read %s under %q: %w", rel, repoRoot, err)
		}
		return strings.ToLower(string(b)), nil
	}
	conv, err := readDoc(personaSecurityConventionPath)
	if err != nil {
		return fail("%w", err)
	}
	remit, err := readDoc(personaSecurityRemitPath)
	if err != nil {
		return fail("%w", err)
	}
	for _, mk := range m.RepoSpecificMarkers {
		lm := strings.ToLower(strings.TrimSpace(mk))
		if !strings.Contains(conv, lm) {
			return fail("repo-specific marker %q is absent from %s: a marker must be one of this repository's documented controls", mk, personaSecurityConventionPath)
		}
		if strings.Contains(remit, lm) {
			return fail("repo-specific marker %q occurs in the generic remit %s: the portability arm is shown it", mk, personaSecurityRemitPath)
		}
	}
	for _, c := range cases {
		if c.Expect() != CatchExpectClean {
			continue
		}
		shown, err := securityShownText(c.Input)
		if err != nil {
			return fail("case %q: %w", c.Name, err)
		}
		if mk := m.RepoMarkerIn(shown); mk != "" {
			return fail("repo-specific marker %q occurs in clean case %q's issue, plan or diff: a portability concern naming it would not be about an unshown control", mk, c.Name)
		}
	}
	return m, nil
}

// SecurityArmCases returns the cases arm runs: every case for ArmSecurity, the
// clean cases only for ArmSecurityPortability, nil for an unknown arm.
func SecurityArmCases(cases []SecurityCatchCase, arm string) []SecurityCatchCase {
	switch arm {
	case ArmSecurity:
		return cases
	case ArmSecurityPortability:
		var out []SecurityCatchCase
		for _, c := range cases {
			if c.Expect() == CatchExpectClean {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

// SecurityStandardTrigger is the standard reviewers' implement_review trigger
// for one case: the issue, the approved plan, the changed-files list (the
// server's renderDiffForReview format), the patch, and convs — the rendered
// review conventions (the security convention for ArmSecurity, nil for
// ArmSecurityPortability). No injected documents.
func SecurityStandardTrigger(c SecurityCatchCase, convs []prompt.ReviewConvention) prompt.Trigger {
	return prompt.Trigger{
		Source:            "github_issue",
		IssueTitle:        c.Input.IssueTitle,
		IssueBody:         c.Input.IssueBody,
		Repo:              personaCatchRepoOwner + "/" + personaCatchRepoName,
		ApprovedPlan:      c.Plan,
		Diff:              renderSecurityChangedFiles(c.Files),
		DiffPatch:         c.Input.Diff,
		ReviewConventions: convs,
	}
}

// SecurityArmRender is one case's rendered security persona prompt.
type SecurityArmRender struct {
	// Prompt is the persona prompt.
	Prompt string
	// StandardTrigger is the standard reviewers' trigger the persona prompt
	// was built from.
	StandardTrigger prompt.Trigger
	// QuoteDocs is every document the prompt renders — the remit and, for
	// ArmSecurity, the convention — what a quoted_passage is verified against.
	QuoteDocs []prompt.InjectedDocument
	// Caps is the round's convention caps: the convention's cap for
	// ArmSecurity, nil for ArmSecurityPortability.
	Caps planreview.ConventionCaps
	// RemitTruncated / ConventionTruncated report whether the resolver cut a
	// document at its cap.
	RemitTruncated, ConventionTruncated bool
}

// securityArmRenderer renders one case's security persona prompt; a seam so
// the fail-closed render-error branch is testable.
type securityArmRenderer func(c SecurityCatchCase, repoRoot, arm string) (SecurityArmRender, error)

// SecurityConvention resolves the security review convention under repoRoot
// exactly as the server's document injection does: a real repodoc.Resolver
// read at the fixture commit, declared at the review_conventions declaration
// site with the "Review convention <name>" framing, rendered through
// repodoc.ToPromptDocument and wrapped as a prompt.ReviewConvention. It also
// returns the resolved document (for its Truncated flag).
func SecurityConvention(repoRoot string) (prompt.ReviewConvention, repodoc.Document, error) {
	return resolveSecurityConvention(context.Background(), personaResolver(repoRoot))
}

func resolveSecurityConvention(ctx context.Context, r *repodoc.Resolver) (prompt.ReviewConvention, repodoc.Document, error) {
	framing := repodoc.Framing{Heading: securityConventionHeadingPrefix + personaSecurityConventionName}
	doc, err := r.Resolve(ctx, repodoc.Request{
		Repo:    forge.RepoRef{Owner: personaCatchRepoOwner, Name: personaCatchRepoName},
		BaseRef: personaFixtureCommit,
		Declaration: repodoc.Declaration{
			Path:            personaSecurityConventionPath,
			DeclarationSite: fmt.Sprintf(spec.DeclarationSiteFmtReviewConvention, personaSecurityConventionName),
			Framing:         framing,
			Base:            repodoc.BaseSourceRunAdmission,
		},
	})
	if err != nil {
		return prompt.ReviewConvention{}, repodoc.Document{}, err
	}
	return prompt.ReviewConvention{
		Name:        personaSecurityConventionName,
		SeverityCap: personaSecurityConventionSeverityCap,
		Document:    repodoc.ToPromptDocument(*doc, framing),
	}, *doc, nil
}

// SecurityArmPrompt renders one case's security persona prompt for arm through
// the production path: the remit resolved with personaprompt.Framing, the
// convention (ArmSecurity only) resolved by SecurityConvention, and
// personaprompt.Build ("implement_review", the standard trigger, no decision
// record, an ungrounded round). It errors on an unknown arm and on a document
// that does not resolve.
func SecurityArmPrompt(c SecurityCatchCase, repoRoot, arm string) (SecurityArmRender, error) {
	if arm != ArmSecurity && arm != ArmSecurityPortability {
		return SecurityArmRender{}, fmt.Errorf("agenteval: unknown security arm %q (want %q or %q)", arm, ArmSecurity, ArmSecurityPortability)
	}
	ctx := context.Background()
	r := personaResolver(repoRoot)
	remit, err := r.Resolve(ctx, repodoc.Request{
		Repo:    forge.RepoRef{Owner: personaCatchRepoOwner, Name: personaCatchRepoName},
		BaseRef: personaFixtureCommit,
		Declaration: repodoc.Declaration{
			Path:            personaSecurityRemitPath,
			DeclarationSite: fmt.Sprintf(spec.DeclarationSiteFmtReviewerPersonaRemit, personaSecurityName),
			Framing:         personaprompt.Framing(personaSecurityName),
			Base:            repodoc.BaseSourceRunAdmission,
		},
	})
	if err != nil {
		return SecurityArmRender{}, fmt.Errorf("agenteval: security case %q: resolve remit: %w", c.Name, err)
	}
	out := SecurityArmRender{RemitTruncated: remit.Truncated}
	var convs []prompt.ReviewConvention
	if arm == ArmSecurity {
		conv, doc, err := resolveSecurityConvention(ctx, r)
		if err != nil {
			return SecurityArmRender{}, fmt.Errorf("agenteval: security case %q: resolve convention: %w", c.Name, err)
		}
		convs = []prompt.ReviewConvention{conv}
		out.Caps = planreview.ConventionCaps{conv.Name: planreview.ConcernSeverity(conv.SeverityCap)}
		out.ConventionTruncated = doc.Truncated
	}
	out.StandardTrigger = SecurityStandardTrigger(c, convs)
	res, err := personaprompt.Build(personaprompt.Input{
		Kind:        "implement_review",
		Trigger:     out.StandardTrigger,
		PersonaName: personaSecurityName,
		Remit:       *remit,
	})
	if err != nil {
		return SecurityArmRender{}, fmt.Errorf("agenteval: security case %q: build persona prompt: %w", c.Name, err)
	}
	out.Prompt = res.Prompt
	out.QuoteDocs = res.QuoteDocs
	return out, nil
}

// ClassifySecurityTrial classifies one security persona response the way the
// review loop would store it (ClassifyPersonaTrialForRound at the remit's
// medium cap, with the round's convention caps) and then annotates every
// stored concern from markers (SecurityMarkers.Annotate).
func ClassifySecurityTrial(responseText string, rule CatchRule, quoteDocs []prompt.InjectedDocument, caps planreview.ConventionCaps, markers SecurityMarkers) CatchTrial {
	trial := ClassifyPersonaTrialForRound(responseText, rule, quoteDocs, personaSecuritySeverityCap, caps)
	markers.annotateTrial(&trial)
	return trial
}

// RunSecurityCatchArm drives render → generate → classify for one security
// arm over SecurityArmCases(cases, arm). Fail closed: an unknown arm, empty
// markers, no cases to run, samples < 1, a nil sender, a render error or a
// transport error returns the ZERO report and an error. An undecodable
// response is NOT an error: it is a trial, counted in Undecodable.
func RunSecurityCatchArm(ctx context.Context, sender MessageSender, cases []SecurityCatchCase, markers SecurityMarkers, repoRoot, arm string, samples int) (CatchRateArmReport, error) {
	return runSecurityCatchArm(ctx, sender, cases, markers, repoRoot, arm, samples, SecurityArmPrompt)
}

func runSecurityCatchArm(ctx context.Context, sender MessageSender, cases []SecurityCatchCase, markers SecurityMarkers, repoRoot, arm string, samples int, render securityArmRenderer) (CatchRateArmReport, error) {
	if arm != ArmSecurity && arm != ArmSecurityPortability {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: unknown security arm %q (want %q or %q)", arm, ArmSecurity, ArmSecurityPortability)
	}
	if markers.empty() {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: persona arm %q: markers are required — without them no concern is annotated and the portability refusal cannot fire", arm)
	}
	return runPersonaTrialLoop(ctx, sender, arm, securityCatchGeneratorSystemPrompt, SecurityArmCases(cases, arm), samples, func(c SecurityCatchCase) (personaTrialCase, error) {
		r, err := render(c, repoRoot, arm)
		if err != nil {
			return personaTrialCase{}, err
		}
		rule := c.CatchRule()
		return personaTrialCase{
			Name:   c.Name,
			Expect: c.Expect(),
			Prompt: r.Prompt,
			Classify: func(responseText string) CatchTrial {
				return ClassifySecurityTrial(responseText, rule, r.QuoteDocs, r.Caps, markers)
			},
		}, nil
	})
}
