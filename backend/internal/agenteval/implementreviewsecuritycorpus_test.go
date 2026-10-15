package agenteval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// committedSecurityCorpus is the committed security persona corpus (E80.17 /
// #4314). The tests below run over it with the REAL docs/review/security-remit.md
// and docs/review/security-conventions.md read from the checkout
// (personaTestRepoRoot). Their assertions are invariants — envelopes, byte
// identity, the document budget, the leak self-check, the marker premises —
// so a routine remit or convention edit reddens verify only when it breaks
// one of them. None of them calls a model or needs a credential.
const committedSecurityCorpus = "testdata/planreview-persona-corpus/security"

// The corpus floors since #4314: six defect cases (one per issue-named lens)
// and four clean sensitive-path cases. They may grow, never shrink.
const (
	committedSecurityDefectFloor = 6
	committedSecurityCleanFloor  = 4
)

// securityIssueLenses are the six remit lenses #4314 names a defect case for
// (record_integrity is a remit lens the issue does not ask the corpus to plant).
var securityIssueLenses = []string{
	SecurityLensTrustBoundaries,
	SecurityLensAuthnAuthz,
	SecurityLensSecretCustody,
	SecurityLensPermissionScope,
	SecurityLensFailClosed,
	SecurityLensSupplyChain,
}

// securitySensitivePrefixes are the repository paths a security escalation
// would attach the persona for. Every committed case changes at least one
// file under one of them.
var securitySensitivePrefixes = []string{
	"backend/internal/auth/",
	"backend/internal/policy/",
	"backend/internal/prompt/",
	"backend/internal/server/",
	".github/workflows/",
	"runner/cmd/fishhawk-runner/",
	"runner/internal/upload/",
	"deploy/gate-image/",
}

// securityGenericObjectionWords are words a security reviewer uses to object
// to ANY sensitive-path change. None may be a catch probe: a concern naming
// the anchored file with one of them is not, by that word, a catch.
var securityGenericObjectionWords = []string{
	"security risk", "insecure", "vulnerable", "vulnerability", "attack surface",
	"harden", "defence in depth", "defense in depth", "needs a security review",
	"security implications", "sensitive path", "could be exploited",
	"weakens security", "risky", "dangerous", "unsafe",
}

func loadCommittedSecurity(t *testing.T) ([]SecurityCatchCase, SecurityMarkers) {
	t.Helper()
	cases, markers, err := LoadSecurityCatchCorpus(committedSecurityCorpus, personaTestRepoRoot)
	if err != nil {
		t.Fatalf("LoadSecurityCatchCorpus(%s): %v", committedSecurityCorpus, err)
	}
	return cases, markers
}

func TestSecurityCatchCorpus_Loads(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	classes := map[string]int{}
	for _, c := range cases {
		classes[c.Expect()]++
		md, err := os.ReadFile(filepath.Join(committedSecurityCorpus, c.Name, "case.md"))
		if err != nil {
			t.Errorf("case %q: case.md: %v", c.Name, err)
			continue
		}
		if !strings.Contains(string(md), "**Provenance: SYNTHETIC.**") {
			t.Errorf("case %q: case.md does not declare SYNTHETIC provenance", c.Name)
		}
		if !strings.Contains(string(md), "# Case: "+c.Name) {
			t.Errorf("case %q: case.md heading does not name the case", c.Name)
		}
	}
	if classes[CatchExpectCatch] < committedSecurityDefectFloor || classes[CatchExpectClean] < committedSecurityCleanFloor {
		t.Errorf("classes = %v, want >= %d catch and >= %d clean", classes, committedSecurityDefectFloor, committedSecurityCleanFloor)
	}
}

// TestSecurityCatchCorpus_LensesPresent: each lens #4314 names has at least one
// defect case, there are at least three clean cases, and every case changes a
// file under a sensitive prefix.
func TestSecurityCatchCorpus_LensesPresent(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	lenses := map[string]int{}
	clean := 0
	for _, c := range cases {
		if c.Expect() == CatchExpectClean {
			clean++
		} else {
			lenses[c.Lens()]++
		}
		sensitive := false
		for _, f := range c.Files {
			for _, p := range securitySensitivePrefixes {
				if strings.HasPrefix(f.Path, p) {
					sensitive = true
				}
			}
		}
		if !sensitive {
			t.Errorf("case %q changes no file under a sensitive prefix %v", c.Name, securitySensitivePrefixes)
		}
	}
	for _, l := range securityIssueLenses {
		if lenses[l] == 0 {
			t.Errorf("no committed defect case for lens %q", l)
		}
	}
	if clean < 3 {
		t.Errorf("%d clean cases, want >= 3", clean)
	}
}

// securityDocLines reads a REAL document independently of the resolver and
// returns its longest line (a line no other prompt surface carries) and its
// last non-empty line (what a truncation cuts first).
func securityDocLines(t *testing.T, rel string) (longest, last string) {
	t.Helper()
	raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(rel)))
	for _, line := range strings.Split(string(raw), "\n") {
		l := strings.TrimSpace(line)
		if len(l) > len(longest) {
			longest = l
		}
		if l != "" {
			last = l
		}
	}
	if len(longest) < 40 {
		t.Fatalf("fixture premise: %s's longest line %q is too short to be distinctive", rel, longest)
	}
	return longest, last
}

// lastH3Before returns the last column-0 "### " heading line that starts
// before offset off, or "".
func lastH3Before(rendered string, off int) string {
	i := strings.LastIndex(rendered[:off], "\n### ")
	if i < 0 {
		return ""
	}
	line := rendered[i+1:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	return line
}

// checkSecurityEnvelopes is the offset-based containment check over one
// security persona render: the render carries exactly one repo-authored
// envelope per document in docs, each document's injected content is exactly
// one envelope's text, the remit's envelope sits under the "### Reviewer
// persona remit: security" heading and the convention's (when wantConvention)
// under the review-conventions section heading AND its "#### Convention
// security-conventions" line, and every occurrence of remitLine and convLine
// (read independently from the files) lies inside its own document's envelope.
// Without wantConvention the render must carry no conventions section and no
// convention line. It returns an error rather than failing the test so
// TestSecurityEnvelopeAssertion_RejectsMissingRemit can prove it is not vacuous.
func checkSecurityEnvelopes(rendered string, docs []prompt.InjectedDocument, remitLine, convLine string, wantConvention bool) error {
	var spans []personaDocSpan
	for _, s := range spansOf(rendered, personaDocBegin, personaDocEnd) {
		spans = append(spans, personaDocSpan{s[0], s[1]})
	}
	if len(spans) == 0 {
		return errors.New("the render carries no repo-authored document envelope")
	}
	if len(spans) != len(docs) {
		return fmt.Errorf("the render carries %d envelopes for %d documents", len(spans), len(docs))
	}
	byPath := map[string]personaDocSpan{}
	for _, d := range docs {
		content, ok := repodoc.InjectedContent(d)
		if !ok {
			return fmt.Errorf("document %s carries no envelope of its own", d.Path)
		}
		found := -1
		for i, s := range spans {
			if rendered[s.start:s.end] == "\n"+content+"\n" {
				found = i
				break
			}
		}
		if found < 0 {
			return fmt.Errorf("document %s is not rendered strictly inside its own envelope", d.Path)
		}
		byPath[d.Path] = spans[found]
	}
	remit, ok := byPath[personaSecurityRemitPath]
	if !ok {
		return fmt.Errorf("no %s document among the rendered documents", personaSecurityRemitPath)
	}
	if h := lastH3Before(rendered, remit.start); h != "### Reviewer persona remit: "+personaSecurityName {
		return fmt.Errorf("the remit envelope sits under %q, not the security remit heading", h)
	}
	if err := linesInside(rendered, remitLine, remit, "remit"); err != nil {
		return err
	}
	conv, hasConv := byPath[personaSecurityConventionPath]
	if !wantConvention {
		if hasConv || strings.Contains(rendered, prompt.ReviewConventionsHeading) || strings.Contains(rendered, convLine) {
			return errors.New("the portability render carries the security convention or a conventions section")
		}
		return nil
	}
	if !hasConv {
		return fmt.Errorf("no %s document among the rendered documents", personaSecurityConventionPath)
	}
	if h := lastH3Before(rendered, conv.start); h != "### "+prompt.ReviewConventionsHeading {
		return fmt.Errorf("the convention envelope sits under %q, not the review-conventions section", h)
	}
	convHeading := "#### Convention " + personaSecurityConventionName + " (severity cap: " + personaSecurityConventionSeverityCap + ")\n"
	if i := strings.LastIndex(rendered[:conv.start], "\n#### "); i < 0 || !strings.HasPrefix(rendered[i+1:], convHeading) {
		return fmt.Errorf("the convention envelope is not under its %q line", strings.TrimSpace(convHeading))
	}
	return linesInside(rendered, convLine, conv, "convention")
}

// linesInside asserts line occurs in rendered and every occurrence lies inside
// span.
func linesInside(rendered, line string, span personaDocSpan, what string) error {
	n := 0
	for off := 0; ; {
		rel := strings.Index(rendered[off:], line)
		if rel < 0 {
			break
		}
		abs := off + rel
		n++
		if abs < span.start || abs+len(line) > span.end {
			return fmt.Errorf("%s line %q occurrence at offset %d is outside the %s envelope [%d,%d)", what, line, abs, what, span.start, span.end)
		}
		off = abs + len(line)
	}
	if n == 0 {
		return fmt.Errorf("%s line %q is absent from the render", what, line)
	}
	return nil
}

// TestSecurityCatchCorpus_RenderContainsRemitAndConventionInEnvelopes renders
// every committed case through the production persona path in both arms and
// asserts, by offset, that the remit (and, for the security arm, the
// convention) sits strictly inside its own envelope under its own heading,
// with a line read independently from each file inside that envelope; the
// portability render carries the remit and no conventions section.
func TestSecurityCatchCorpus_RenderContainsRemitAndConventionInEnvelopes(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	remitLine, _ := securityDocLines(t, personaSecurityRemitPath)
	convLine, _ := securityDocLines(t, personaSecurityConventionPath)
	for _, c := range cases {
		for _, arm := range []string{ArmSecurity, ArmSecurityPortability} {
			r, err := SecurityArmPrompt(c, personaTestRepoRoot, arm)
			if err != nil {
				t.Fatalf("case %q %s: SecurityArmPrompt: %v", c.Name, arm, err)
			}
			if err := checkSecurityEnvelopes(r.Prompt, r.QuoteDocs, remitLine, convLine, arm == ArmSecurity); err != nil {
				t.Errorf("case %q %s: %v", c.Name, arm, err)
			}
		}
	}
}

// TestSecurityEnvelopeAssertion_RejectsMissingRemit proves
// checkSecurityEnvelopes is not vacuous: it errors on a render without any
// envelope, a document list without the remit, a render with the remit
// envelope removed, a remit line outside the remit envelope, a remit envelope
// under another heading, a convention line outside its envelope, and a
// portability check over a render that carries the convention.
func TestSecurityEnvelopeAssertion_RejectsMissingRemit(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	c := cases[0]
	remitLine, _ := securityDocLines(t, personaSecurityRemitPath)
	convLine, _ := securityDocLines(t, personaSecurityConventionPath)
	r, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSecurityEnvelopes(r.Prompt, r.QuoteDocs, remitLine, convLine, true); err != nil {
		t.Fatalf("fixture premise: the real security render must pass: %v", err)
	}
	standard, err := prompt.Build("implement_review", SecurityStandardTrigger(c, nil))
	if err != nil {
		t.Fatal(err)
	}
	isRemit := func(d prompt.InjectedDocument) bool { return d.Path == personaSecurityRemitPath }
	withoutRemit := slices.DeleteFunc(slices.Clone(r.QuoteDocs), isRemit)
	remitDoc := r.QuoteDocs[slices.IndexFunc(r.QuoteDocs, isRemit)]
	renamed := strings.Replace(r.Prompt, "### Reviewer persona remit: "+personaSecurityName+"\n", "### Reviewer persona remit: someone-else\n", 1)
	tests := []struct {
		name, rendered string
		docs           []prompt.InjectedDocument
		conv           bool
		want           string
	}{
		{"the standard render (no envelope)", standard, r.QuoteDocs, true, "no repo-authored document envelope"},
		{"the remit dropped from the documents", r.Prompt, withoutRemit, true, "envelopes for"},
		{"the remit dropped from documents and render", strings.Replace(r.Prompt, remitDoc.Body, "", 1), withoutRemit, true, "no " + personaSecurityRemitPath},
		{"the remit envelope removed from the render", strings.Replace(r.Prompt, remitDoc.Body, "", 1), r.QuoteDocs, true, "envelopes for"},
		{"a remit line outside the remit envelope", r.Prompt + "\n" + remitLine + "\n", r.QuoteDocs, true, "outside the remit envelope"},
		{"a remit envelope under another heading", renamed, r.QuoteDocs, true, "not the security remit heading"},
		{"a convention line outside its envelope", r.Prompt + "\n" + convLine + "\n", r.QuoteDocs, true, "outside the convention envelope"},
		{"a portability check over the security render", r.Prompt, r.QuoteDocs, false, "portability render carries"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkSecurityEnvelopes(tc.rendered, tc.docs, remitLine, convLine, tc.conv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkSecurityEnvelopes = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// diffGitHeaderRe reads a diff's changed paths independently of
// parseSecurityDiff.
var diffGitHeaderRe = regexp.MustCompile(`(?m)^diff --git a/(\S+) b/(\S+)$`)

// independentChangedFiles renders a case's changed-files list in the server's
// renderDiffForReview format without the harness's parser.
func independentChangedFiles(diff string) string {
	var b strings.Builder
	sections := diffGitHeaderRe.FindAllStringSubmatchIndex(diff, -1)
	for i, m := range sections {
		end := len(diff)
		if i+1 < len(sections) {
			end = sections[i+1][0]
		}
		section := diff[m[0]:end]
		status := "M"
		switch {
		case strings.Contains(section, "\nnew file mode"):
			status = "A"
		case strings.Contains(section, "\ndeleted file mode"):
			status = "D"
		}
		fmt.Fprintf(&b, "- %s %s\n", status, diff[m[2]:m[3]])
	}
	return b.String()
}

// TestSecurityCatchCorpus_StandardRenderByteIdentical: per case and arm, the
// STANDARD implement_review render (with the convention for the security arm,
// without for portability) is byte-identical before and after the persona
// build, equals a direct prompt.Build of a trigger spelled out field by field
// over a re-parsed plan and an independently derived changed-files list, and
// carries no persona remit. The persona render is the standard render plus ONE
// contiguous insertion carrying the remit, and the convention stays in the
// standard part.
func TestSecurityCatchCorpus_StandardRenderByteIdentical(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	conv, _, err := SecurityConvention(personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	remitLine, _ := securityDocLines(t, personaSecurityRemitPath)
	for _, c := range cases {
		for _, tc := range []struct {
			arm   string
			convs []prompt.ReviewConvention
		}{{ArmSecurity, []prompt.ReviewConvention{conv}}, {ArmSecurityPortability, nil}} {
			before, err := prompt.Build("implement_review", SecurityStandardTrigger(c, tc.convs))
			if err != nil {
				t.Fatalf("case %q %s: standard render: %v", c.Name, tc.arm, err)
			}
			r, err := SecurityArmPrompt(c, personaTestRepoRoot, tc.arm)
			if err != nil {
				t.Fatalf("case %q %s: SecurityArmPrompt: %v", c.Name, tc.arm, err)
			}
			after, err := prompt.Build("implement_review", SecurityStandardTrigger(c, tc.convs))
			if err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Errorf("case %q %s: the standard render changed across the persona build", c.Name, tc.arm)
			}
			p, err := plan.Parse(c.Input.Plan)
			if err != nil {
				t.Fatal(err)
			}
			direct, err := prompt.Build("implement_review", prompt.Trigger{
				Source:            "github_issue",
				IssueTitle:        c.Input.IssueTitle,
				IssueBody:         c.Input.IssueBody,
				Repo:              "kuhlman-labs/fishhawk",
				ApprovedPlan:      p,
				Diff:              independentChangedFiles(c.Input.Diff),
				DiffPatch:         c.Input.Diff,
				ReviewConventions: tc.convs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if before != direct {
				t.Errorf("case %q %s: the standard render differs from a direct prompt.Build of the standard trigger", c.Name, tc.arm)
			}
			if strings.Contains(before, "Reviewer persona remit:") || strings.Contains(before, remitLine) {
				t.Errorf("case %q %s: the standard render carries the persona remit", c.Name, tc.arm)
			}
			if got := strings.Contains(before, prompt.ReviewConventionsHeading); got != (tc.arm == ArmSecurity) {
				t.Errorf("case %q %s: the standard render carries the conventions section = %v", c.Name, tc.arm, got)
			}
			pre := commonPrefixLen(before, r.Prompt)
			suf := commonSuffixLen(before[pre:], r.Prompt[pre:])
			if pre+suf != len(before) {
				t.Errorf("case %q %s: the persona render is not the standard render plus one contiguous insertion (prefix %d + suffix %d != %d)", c.Name, tc.arm, pre, suf, len(before))
				continue
			}
			inserted := r.Prompt[pre : len(r.Prompt)-suf]
			if !strings.Contains(inserted, "### Reviewer persona remit: "+personaSecurityName+"\n") || !strings.Contains(inserted, remitLine) {
				t.Errorf("case %q %s: the inserted span lacks the remit", c.Name, tc.arm)
			}
			if strings.Contains(inserted, prompt.ReviewConventionsHeading) {
				t.Errorf("case %q %s: the inserted span carries the conventions section, which belongs to the standard render", c.Name, tc.arm)
			}
		}
	}
}

// TestSecurityCatchCorpus_DocumentsWithinBudget (the #4399 lesson): neither the
// remit nor the convention is truncated at repodoc.DefaultMaxBytes in any
// render — the resolver reports no truncation AND each document's last line,
// read independently from the file, renders inside the prompt.
func TestSecurityCatchCorpus_DocumentsWithinBudget(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	_, remitLast := securityDocLines(t, personaSecurityRemitPath)
	_, convLast := securityDocLines(t, personaSecurityConventionPath)
	for _, rel := range []string{personaSecurityRemitPath, personaSecurityConventionPath} {
		size := len(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(rel))))
		t.Logf("%s: %d bytes against the %d-byte cap", rel, size, repodoc.DefaultMaxBytes)
	}
	for _, c := range cases {
		for _, arm := range []string{ArmSecurity, ArmSecurityPortability} {
			r, err := SecurityArmPrompt(c, personaTestRepoRoot, arm)
			if err != nil {
				t.Fatalf("case %q %s: %v", c.Name, arm, err)
			}
			if r.RemitTruncated || r.ConventionTruncated {
				t.Errorf("case %q %s: truncated (remit %v, convention %v)", c.Name, arm, r.RemitTruncated, r.ConventionTruncated)
			}
			if !strings.Contains(r.Prompt, remitLast) {
				t.Errorf("case %q %s: the remit's last line %q is not rendered", c.Name, arm, remitLast)
			}
			if arm == ArmSecurity && !strings.Contains(r.Prompt, convLast) {
				t.Errorf("case %q %s: the convention's last line %q is not rendered", c.Name, arm, convLast)
			}
		}
	}
}

// neutralSecurityCase is the corpus-disjoint lighthouse trigger
// (neutralLeakPlan) as a security case with a one-line diff, so its render is
// the static security persona surface: the implement-review prompt, the remit
// and the convention.
func neutralSecurityCase(t *testing.T) SecurityCatchCase {
	t.Helper()
	n := neutralLeakCase(t)
	diff := "diff --git a/lighthouse/lamp.go b/lighthouse/lamp.go\n--- a/lighthouse/lamp.go\n+++ b/lighthouse/lamp.go\n@@ -1 +1,2 @@\n package lighthouse\n+// Lamp turns once per minute.\n"
	files, err := parseSecurityDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	return SecurityCatchCase{
		Name:  n.Name,
		Input: SecurityCaseInput{Persona: personaSecurityName, IssueTitle: n.Input.IssueTitle, IssueBody: n.Input.IssueBody, Plan: n.Input.Plan, Diff: diff},
		Plan:  n.Plan,
		Files: files,
	}
}

// TestSecurityCatchCorpus_NotLeakedIntoPersonaPrompt is the security arms'
// prompt-leak self-check: no case's defect phrase occurs in a STATIC persona
// surface — the real remit, the real convention, the neutral security render
// (the implement-review prompt plus both documents) or the generator system
// prompt. Both sides are normalized (normalizeLeakText).
func TestSecurityCatchCorpus_NotLeakedIntoPersonaPrompt(t *testing.T) {
	cases, _ := loadCommittedSecurity(t)
	neutral := neutralSecurityCase(t)
	r, err := SecurityArmPrompt(neutral, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatalf("neutral security render: %v", err)
	}
	for _, premise := range []string{"Reviewer persona remit: " + personaSecurityName, prompt.ReviewConventionsHeading, securityRemitLine, securityConventionLine} {
		if !strings.Contains(r.Prompt, premise) {
			t.Fatalf("fixture premise: the neutral security render lacks %q, so the leak check would be vacuous", premise)
		}
	}
	surfaces := []struct{ name, text string }{
		{"security remit", normalizeLeakText(string(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaSecurityRemitPath)))))},
		{"security convention", normalizeLeakText(string(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaSecurityConventionPath)))))},
		{"neutral security render", normalizeLeakText(r.Prompt)},
		{"security generator system prompt", normalizeLeakText(securityCatchGeneratorSystemPrompt)},
	}
	shown, err := securityShownText(neutral.Input)
	if err != nil {
		t.Fatal(err)
	}
	shown = normalizeLeakText(shown)
	needles := 0
	for _, c := range cases {
		for _, phrase := range c.Input.DefectPhrases {
			norm := normalizeLeakText(phrase)
			if norm == "" {
				t.Fatalf("case %q: defect phrase %q normalizes to empty", c.Name, phrase)
			}
			needles++
			if strings.Contains(shown, norm) {
				t.Fatalf("fixture premise: the neutral trigger's own text carries case %q's defect phrase %q", c.Name, phrase)
			}
			for _, s := range surfaces {
				if strings.Contains(s.text, norm) {
					t.Errorf("case %q: defect phrase %q occurs in the %s: every security reviewer would be shown this case's planted defect", c.Name, phrase, s.name)
				}
			}
		}
	}
	if needles == 0 {
		t.Fatal("no needles: the leak check is vacuous")
	}
}

// TestSecurityCatchCorpus_GenericObjectionsScoreMissed: for every catch case,
// each attribution phrase joined with EVERY generic security objection word in
// one security concern scores MISSED through the production security
// classifier, and the case declares a non-catching example naming the anchor
// with a generic objection word, which also scores MISSED.
func TestSecurityCatchCorpus_GenericObjectionsScoreMissed(t *testing.T) {
	cases, markers := loadCommittedSecurity(t)
	objection := strings.Join(securityGenericObjectionWords, "; ")
	for _, c := range cases {
		if c.Expect() != CatchExpectCatch {
			continue
		}
		rule := c.CatchRule()
		for _, ref := range c.Input.AttributionPhrases {
			note := ref + ": " + objection
			if _, ok := matchCatchAttribution("security", note, rule); !ok {
				t.Fatalf("case %q: fixture premise: a concern naming %q must satisfy the attribution half", c.Name, ref)
			}
			if trial := ClassifySecurityTrial(personaVerdict(t, "security", note), rule, nil, nil, markers); trial.Outcome != CatchMissed {
				t.Errorf("case %q: %q joined with the generic objection words classified %q (probe %q), want missed", c.Name, ref, trial.Outcome, trial.MatchedProbe)
			}
		}
		pinned := false
		for _, ex := range c.Input.NonCatchingExamples {
			if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); !ok {
				continue
			}
			if _, ok := matchCatchProbe(ex.Category, ex.Note, securityGenericObjectionWords); !ok {
				continue
			}
			if trial := ClassifySecurityTrial(personaVerdict(t, ex.Category, ex.Note), rule, nil, nil, markers); trial.Outcome != CatchMissed {
				t.Errorf("case %q: generic-objection example %q classified %q, want missed", c.Name, ex.Note, trial.Outcome)
			}
			pinned = true
		}
		if !pinned {
			t.Errorf("case %q has no anchored non-catching example carrying a generic security objection word", c.Name)
		}
	}
}

// TestSecurityCatchCorpus_EchoesScoreMissed: a security concern that names the
// anchored file and merely ECHOES the case's own text — the issue, every
// approach step and criterion, and the WHOLE diff — scores MISSED. A probe
// matching any of it would score every reviewer that quotes the change as a
// catch.
func TestSecurityCatchCorpus_EchoesScoreMissed(t *testing.T) {
	cases, markers := loadCommittedSecurity(t)
	for _, c := range cases {
		if c.Expect() != CatchExpectCatch {
			continue
		}
		rule := c.CatchRule()
		echo := []string{c.Input.AttributionPhrases[0] + ":", c.Input.IssueTitle, c.Input.IssueBody}
		for _, s := range c.Plan.Approach {
			echo = append(echo, s.Description)
		}
		for _, ac := range c.Plan.Verification.AcceptanceCriteria {
			echo = append(echo, ac.ID+": "+ac.Statement)
		}
		for _, note := range []string{strings.Join(echo, "\n"), c.Input.AttributionPhrases[0] + ": " + c.Input.Diff} {
			if trial := ClassifySecurityTrial(personaVerdict(t, "security", note), rule, nil, nil, markers); trial.Outcome != CatchMissed {
				t.Errorf("case %q: an echo concern classified %q (probe %q), want missed", c.Name, trial.Outcome, trial.MatchedProbe)
			}
		}
	}
}

// TestSecurityCatchCorpus_ExamplesScoreThroughTheProductionClassifier: every
// committed example scores as the loader says through the full security
// ingest path over the case's REAL render (quote set, convention caps,
// markers): each catching example is caught in the security arm, each
// false-alarm example is a false alarm in both arms.
func TestSecurityCatchCorpus_ExamplesScoreThroughTheProductionClassifier(t *testing.T) {
	cases, markers := loadCommittedSecurity(t)
	for _, c := range cases {
		for _, arm := range []string{ArmSecurity, ArmSecurityPortability} {
			if !slices.ContainsFunc(SecurityArmCases(cases, arm), func(x SecurityCatchCase) bool { return x.Name == c.Name }) {
				continue
			}
			r, err := SecurityArmPrompt(c, personaTestRepoRoot, arm)
			if err != nil {
				t.Fatal(err)
			}
			want, examples := CatchCaught, c.Input.CatchingExamples
			if c.Expect() == CatchExpectClean {
				want, examples = CatchFalseAlarm, c.Input.FalseAlarmExamples
			}
			for _, ex := range examples {
				if trial := ClassifySecurityTrial(personaVerdict(t, ex.Category, ex.Note), c.CatchRule(), r.QuoteDocs, r.Caps, markers); trial.Outcome != want {
					t.Errorf("case %q %s: example %q classified %q, want %q", c.Name, arm, ex.Note, trial.Outcome, want)
				}
			}
		}
	}
}

// TestSecurityCatchCorpus_MarkerPremisesHold re-checks the committed
// markers.json premises independently of the loader: every list is non-empty
// with no blank or duplicate entry; no generic control phrase occurs inside
// another (approval condition C4's loader premise); every repo-specific marker
// is in the REAL convention and absent from the REAL remit; and no marker
// occurs in any clean case's issue, plan or diff. It then pins the annotation
// rows over the COMMITTED markers, including C4's correctly-pinned-bump note,
// which must NOT set AbsentControl.
func TestSecurityCatchCorpus_MarkerPremisesHold(t *testing.T) {
	cases, markers := loadCommittedSecurity(t)
	var raw struct {
		RepoSpecificMarkers   []string `json:"repo_specific_markers"`
		GenericControlPhrases []string `json:"generic_control_phrases"`
		AbsencePhrases        []string `json:"absence_phrases"`
	}
	if err := json.Unmarshal(mustReadCatch(t, filepath.Join(committedSecurityCorpus, securityMarkersFile)), &raw); err != nil {
		t.Fatalf("decode markers.json: %v", err)
	}
	for name, list := range map[string][]string{"repo_specific_markers": raw.RepoSpecificMarkers, "generic_control_phrases": raw.GenericControlPhrases, "absence_phrases": raw.AbsencePhrases} {
		if len(list) == 0 {
			t.Errorf("%s is empty", name)
		}
		seen := map[string]bool{}
		for _, it := range list {
			l := strings.ToLower(strings.TrimSpace(it))
			if l == "" || seen[l] {
				t.Errorf("%s: blank or duplicate entry %q", name, it)
			}
			seen[l] = true
		}
	}
	for i, a := range raw.GenericControlPhrases {
		for j, b := range raw.GenericControlPhrases {
			if i != j && strings.Contains(strings.ToLower(b), strings.ToLower(a)) {
				t.Errorf("generic control phrase %q is a substring of %q", a, b)
			}
		}
	}
	conv := strings.ToLower(string(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaSecurityConventionPath)))))
	remit := strings.ToLower(string(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaSecurityRemitPath)))))
	for _, mk := range raw.RepoSpecificMarkers {
		lm := strings.ToLower(mk)
		if !strings.Contains(conv, lm) {
			t.Errorf("repo-specific marker %q is absent from the real convention", mk)
		}
		if strings.Contains(remit, lm) {
			t.Errorf("repo-specific marker %q occurs in the real remit", mk)
		}
		for _, c := range cases {
			if c.Expect() != CatchExpectClean {
				continue
			}
			shown := strings.ToLower(c.Input.IssueTitle + "\n" + c.Input.IssueBody + "\n" + string(c.Input.Plan) + "\n" + c.Input.Diff)
			if strings.Contains(shown, lm) {
				t.Errorf("repo-specific marker %q occurs in clean case %q's shown text", mk, c.Name)
			}
		}
	}
	rows := []struct {
		name, category, note string
		marker               string
		absent               bool
	}{
		{"correctly pinned bump (C4)", "supply_chain", "ci.yml: golangci-lint is pinned to the v2.9.0 tag and no longer references v2.8.0", "", false},
		{"pinned bump naming the pin noun", "supply_chain", "ci.yml keeps the pin on the tagged install script", "", false},
		{"portability-shaped repo-marker absence", "security", "the change does not add the new variable to the gateenv.go allow-list", "gateenv.go", true},
		{"generic absent control", "security", "the new route is missing a scope check", "", true},
		{"repo marker without absence", "security", "the emitter's category is registered in categories.go", "categories.go", false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			c := CatchTrialConcern{Category: tc.category, Note: tc.note}
			markers.Annotate(&c)
			if c.RepoSpecificMarker != tc.marker || c.AbsentControl != tc.absent {
				t.Errorf("Annotate(%q) = marker %q absent %v, want %q %v", tc.note, c.RepoSpecificMarker, c.AbsentControl, tc.marker, tc.absent)
			}
		})
	}
}
