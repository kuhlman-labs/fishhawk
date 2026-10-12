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

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionrecord"
	"github.com/kuhlman-labs/fishhawk/backend/internal/personaprompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// committedPersonaCorpus is the committed architect persona corpus (E78.11 /
// #4313). The tests below run over it with the REAL docs/review/architect-remit.md
// and docs/adr/** read from the checkout (personaTestRepoRoot). Their
// assertions are invariants — envelopes, byte identity, premises, a
// min_matched_records floor — so a routine ADR landing does not redden verify,
// while a genuine premise break (ADR-033 losing superseded_by, ADR-094's index
// entry truncated away) does. Selection stats are logged, never pinned.
const committedPersonaCorpus = "testdata/planreview-persona-corpus/architect"

// committedPersonaCaseFloor is the number of cases the corpus has carried
// since #4313; it may grow, never shrink below it.
const committedPersonaCaseFloor = 6

// The repodoc envelope delimiters (repodoc/inject.go's unexported
// beginDelimiter / endDelimiter). A drift there leaves the envelope assertion
// finding no spans, which fails loudly rather than vacuously.
const (
	personaDocBegin = "----- BEGIN REPO-AUTHORED DOCUMENT -----"
	personaDocEnd   = "----- END REPO-AUTHORED DOCUMENT -----"
)

func loadCommittedPersona(t *testing.T) []PersonaCatchCase {
	t.Helper()
	cases, err := LoadPersonaCatchCorpus(committedPersonaCorpus, personaTestRepoRoot, ArmArchitect)
	if err != nil {
		t.Fatalf("LoadPersonaCatchCorpus(%s): %v", committedPersonaCorpus, err)
	}
	return cases
}

func TestPersonaCatchCorpus_Loads(t *testing.T) {
	cases := loadCommittedPersona(t)
	if len(cases) < committedPersonaCaseFloor {
		t.Fatalf("committed persona corpus loaded %d cases, want >= %d", len(cases), committedPersonaCaseFloor)
	}
	for _, c := range cases {
		md, err := os.ReadFile(filepath.Join(committedPersonaCorpus, c.Name, "case.md"))
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
}

// TestPersonaCatchCorpus_KindsPresent: every case kind is present, with at
// least three catch, three clean and two clean_area cases, so the arm measures
// both directions and a clean area governed by more than one record.
func TestPersonaCatchCorpus_KindsPresent(t *testing.T) {
	cases := loadCommittedPersona(t)
	kinds := map[string]int{}
	classes := map[string]int{}
	for _, c := range cases {
		kinds[c.Input.Kind]++
		classes[c.Expect()]++
	}
	for kind := range personaKindExpect {
		if kinds[kind] == 0 {
			t.Errorf("no committed case of kind %q", kind)
		}
	}
	if classes[CatchExpectCatch] < 3 || classes[CatchExpectClean] < 3 {
		t.Errorf("classes = %v, want >= 3 catch and >= 3 clean", classes)
	}
	if kinds[PersonaKindCleanArea] < 2 {
		t.Errorf("%d clean_area cases, want >= 2", kinds[PersonaKindCleanArea])
	}
}

// adrIDRe finds an ADR id in a concern note.
var adrIDRe = regexp.MustCompile(`ADR-\d{3}`)

// TestPersonaCatchCorpus_CatchExamplesShape pins #4253's and #4355's example
// validity rules on every committed catch case: >= 2 catching examples, one of
// them attributed by a phrase rather than the anchor; >= 4 non-catching
// examples, among them an anchored decision_record concern citing a record
// OTHER than the case's governing records (so the category predicate plus the
// anchor alone is shown not to catch).
func TestPersonaCatchCorpus_CatchExamplesShape(t *testing.T) {
	for _, c := range loadCommittedPersona(t) {
		if c.Expect() != CatchExpectCatch {
			continue
		}
		rule := c.CatchRule()
		if len(c.Input.CatchingExamples) < 2 {
			t.Errorf("case %q: %d catching examples, want >= 2", c.Name, len(c.Input.CatchingExamples))
		}
		phrase := false
		for _, ex := range c.Input.CatchingExamples {
			trial := classifyCatchConcerns(examplesAsConcerns(ex), 1, rule)
			if trial.Outcome == CatchCaught && trial.Attribution == CatchAttributionPhrase {
				phrase = true
			}
		}
		if !phrase {
			t.Errorf("case %q: no catching example is caught through an attribution phrase", c.Name)
		}
		if len(c.Input.NonCatchingExamples) < 4 {
			t.Errorf("case %q: %d non-catching examples, want >= 4", c.Name, len(c.Input.NonCatchingExamples))
		}
		governing := map[string]bool{}
		for _, g := range c.Input.GoverningRecords {
			governing[g.ID] = true
		}
		otherRecord := false
		for _, ex := range c.Input.NonCatchingExamples {
			if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); !ok || !matchCatchCategory(ex.Category, rule) {
				continue
			}
			for _, id := range adrIDRe.FindAllString(ex.Note, -1) {
				if !governing[id] {
					otherRecord = true
				}
			}
		}
		if !otherRecord {
			t.Errorf("case %q: no anchored decision_record non-catching example cites a record other than %v", c.Name, c.Input.GoverningRecords)
		}
	}
}

// personaDocSpan is one rendered repodoc envelope.
type personaDocSpan struct{ start, end int }

// checkPersonaEnvelopes is the offset-based containment check over one persona
// render: every document in docs (the remit, the index and each included
// record) renders strictly inside its OWN column-0 BEGIN/END envelope — the
// envelope's text is exactly that document's injected content, the document's
// heading sits between the previous envelope and this one, and the envelopes
// appear in document order — and every occurrence of remitLine (read
// independently from the remit file) lies inside the remit's envelope. It
// returns an error rather than failing the test so that
// TestPersonaEnvelopeAssertion_RejectsMissingRemit can prove it is not vacuous.
func checkPersonaEnvelopes(rendered string, docs []prompt.InjectedDocument, remitLine string) (map[string]personaDocSpan, error) {
	var spans []personaDocSpan
	for _, s := range spansOf(rendered, personaDocBegin, personaDocEnd) {
		spans = append(spans, personaDocSpan{s[0], s[1]})
	}
	if len(spans) == 0 {
		return nil, errors.New("the render carries no repo-authored document envelope")
	}
	out := map[string]personaDocSpan{}
	next := 0
	for _, d := range docs {
		content, ok := repodoc.InjectedContent(d)
		if !ok {
			return nil, fmt.Errorf("document %s carries no envelope of its own", d.Path)
		}
		found := -1
		for i := next; i < len(spans); i++ {
			if rendered[spans[i].start:spans[i].end] == "\n"+content+"\n" {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("document %s is not rendered strictly inside its own envelope (after envelope %d)", d.Path, next)
		}
		prevEnd := 0
		if found > 0 {
			prevEnd = spans[found-1].end
		}
		if !strings.Contains(rendered[prevEnd:spans[found].start], "### "+d.Heading+"\n") {
			return nil, fmt.Errorf("document %s: its heading %q does not precede its envelope", d.Path, d.Heading)
		}
		out[d.Path] = spans[found]
		next = found + 1
	}
	remit, ok := out[personaArchitectRemitPath]
	if !ok {
		return nil, fmt.Errorf("no %s document among the rendered documents", personaArchitectRemitPath)
	}
	if _, ok := out[personaArchitectIndexPath]; !ok {
		return nil, fmt.Errorf("no %s document among the rendered documents", personaArchitectIndexPath)
	}
	occurrences := 0
	for off := 0; ; {
		rel := strings.Index(rendered[off:], remitLine)
		if rel < 0 {
			break
		}
		abs := off + rel
		occurrences++
		if abs < remit.start || abs+len(remitLine) > remit.end {
			return nil, fmt.Errorf("remit line %q occurrence at offset %d is outside the remit envelope [%d,%d)", remitLine, abs, remit.start, remit.end)
		}
		off = abs + len(remitLine)
	}
	if occurrences == 0 {
		return nil, fmt.Errorf("remit line %q is absent from the render", remitLine)
	}
	return out, nil
}

// distinctiveRemitLine reads the REAL remit file independently of the
// resolver and returns its longest line — a line no other prompt surface
// carries.
func distinctiveRemitLine(t *testing.T) string {
	t.Helper()
	raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaArchitectRemitPath)))
	var best string
	for _, line := range strings.Split(string(raw), "\n") {
		if l := strings.TrimSpace(line); len(l) > len(best) {
			best = l
		}
	}
	if len(best) < 40 {
		t.Fatalf("fixture premise: the remit's longest line %q is too short to be distinctive", best)
	}
	return best
}

// realIndexEntry is one docs/adr/index.json record, decoded independently of
// decisionrecord.ParseIndex so the premise checks are not the loader's own.
type realIndexEntry struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Status       string   `json:"status"`
	Supersedes   []string `json:"supersedes"`
	SupersededBy []string `json:"superseded_by"`
	AppliesTo    []string `json:"applies_to"`
}

func readRealIndex(t *testing.T) map[string]realIndexEntry {
	t.Helper()
	var idx struct {
		Records []realIndexEntry `json:"records"`
	}
	raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaArchitectIndexPath)))
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("decode the real index: %v", err)
	}
	out := map[string]realIndexEntry{}
	for _, r := range idx.Records {
		out[r.ID] = r
	}
	if len(out) == 0 {
		t.Fatal("the real index lists no records")
	}
	return out
}

// TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes renders
// every committed case through the production persona path and asserts, by
// offset, that the remit, the index and every included record each sit
// strictly inside their own envelope, with a remit line read independently
// from docs/review/architect-remit.md inside the remit's envelope.
//
// It also carries approval condition C1: the real index is near the
// DefaultMaxBytes cap and decisionrecord.Assemble reshapes an over-cap index
// behind a truncation marker, so every governing record's index entry (its id
// AND its title, plus each successor's entry) must be PRESENT in the RENDERED,
// post-truncation index envelope. A future ADR landing that truncates them
// away reddens verify here. The selection (included / dropped ids, rendered
// index bytes) is logged, not pinned.
func TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes(t *testing.T) {
	cases := loadCommittedPersona(t)
	remitLine := distinctiveRemitLine(t)
	real := readRealIndex(t)
	for _, c := range cases {
		r, err := PersonaArmPrompt(c, personaTestRepoRoot)
		if err != nil {
			t.Fatalf("case %q: PersonaArmPrompt: %v", c.Name, err)
		}
		spans, err := checkPersonaEnvelopes(r.Prompt, r.QuoteDocs, remitLine)
		if err != nil {
			t.Errorf("case %q: %v", c.Name, err)
			continue
		}
		if want := 2 + len(r.IncludedIDs); len(r.QuoteDocs) != want {
			t.Errorf("case %q: %d rendered documents, want the remit, the index and %d included records", c.Name, len(r.QuoteDocs), len(r.IncludedIDs))
		}
		idxSpan := spans[personaArchitectIndexPath]
		indexText := r.Prompt[idxSpan.start:idxSpan.end]
		need := map[string]bool{}
		for _, g := range c.Input.GoverningRecords {
			need[g.ID] = true
			for _, s := range g.SupersededBy {
				need[s] = true
			}
		}
		for id := range need {
			if !strings.Contains(indexText, `"id": "`+id+`"`) {
				t.Errorf("case %q: the rendered index lacks %s's entry: an index truncation dropped a record the case depends on", c.Name, id)
				continue
			}
			if title := real[id].Title; title == "" || !strings.Contains(indexText, title) {
				t.Errorf("case %q: the rendered index lacks %s's title %q", c.Name, id, title)
			}
		}
		t.Logf("case %q: matched %v included %v dropped %v; index rendered %d bytes (truncated %v, cap %d)",
			c.Name, r.MatchedIDs, r.IncludedIDs, r.DroppedIDs, r.Selection.Index.RenderedBytes, r.Selection.Index.Truncated, r.Selection.CapBytes)
	}
}

// TestPersonaEnvelopeAssertion_RejectsMissingRemit proves checkPersonaEnvelopes
// is not vacuous: it errors on a render without the remit, on a document list
// without the remit, on a remit line placed outside the remit envelope, and on
// an envelope whose document heading does not precede it.
func TestPersonaEnvelopeAssertion_RejectsMissingRemit(t *testing.T) {
	cases := loadCommittedPersona(t)
	c := cases[0]
	remitLine := distinctiveRemitLine(t)
	r, err := PersonaArmPrompt(c, personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkPersonaEnvelopes(r.Prompt, r.QuoteDocs, remitLine); err != nil {
		t.Fatalf("fixture premise: the real persona render must pass: %v", err)
	}
	standard, err := prompt.Build("plan_review", PersonaStandardTrigger(c))
	if err != nil {
		t.Fatal(err)
	}
	withoutRemit := slices.DeleteFunc(slices.Clone(r.QuoteDocs), func(d prompt.InjectedDocument) bool { return d.Path == personaArchitectRemitPath })
	remitDoc := r.QuoteDocs[slices.IndexFunc(r.QuoteDocs, func(d prompt.InjectedDocument) bool { return d.Path == personaArchitectRemitPath })]
	renamed := slices.Clone(r.QuoteDocs)
	for i := range renamed {
		if renamed[i].Path == personaArchitectRemitPath {
			renamed[i].Heading = "Reviewer persona remit: someone-else"
		}
	}
	tests := []struct {
		name, rendered string
		docs           []prompt.InjectedDocument
		want           string
	}{
		{"the standard render (no remit envelope)", standard, r.QuoteDocs, "no repo-authored document envelope"},
		{"the remit dropped from the documents", r.Prompt, withoutRemit, "no " + personaArchitectRemitPath},
		{"the remit envelope removed from the render", strings.Replace(r.Prompt, remitDoc.Body, "", 1), r.QuoteDocs, "not rendered strictly inside its own envelope"},
		{"a remit line outside the remit envelope", r.Prompt + "\n" + remitLine + "\n", r.QuoteDocs, "outside the remit envelope"},
		{"a remit envelope under another heading", r.Prompt, renamed, "does not precede its envelope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkPersonaEnvelopes(tc.rendered, tc.docs, remitLine); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkPersonaEnvelopes = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestPersonaCatchCorpus_StandardRenderByteIdentical: building a case's
// persona prompt leaves the STANDARD reviewers' plan_review render untouched —
// the render of the case's standard trigger is byte-identical before and after
// the persona build (which shares the case's *plan.Plan), equals a direct
// prompt.Build of a trigger spelled out field by field over a re-parsed plan,
// and carries no remit heading and no document envelope. The persona render
// is the standard render with ONE contiguous insertion carrying the documents.
func TestPersonaCatchCorpus_StandardRenderByteIdentical(t *testing.T) {
	for _, c := range loadCommittedPersona(t) {
		before, err := prompt.Build("plan_review", PersonaStandardTrigger(c))
		if err != nil {
			t.Fatalf("case %q: standard render: %v", c.Name, err)
		}
		r, err := PersonaArmPrompt(c, personaTestRepoRoot)
		if err != nil {
			t.Fatalf("case %q: PersonaArmPrompt: %v", c.Name, err)
		}
		after, err := prompt.Build("plan_review", PersonaStandardTrigger(c))
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Errorf("case %q: the standard render changed across the persona build", c.Name)
		}
		p, err := plan.Parse(c.Input.Plan)
		if err != nil {
			t.Fatal(err)
		}
		direct, err := prompt.Build("plan_review", prompt.Trigger{
			Source:       "github_issue",
			IssueTitle:   c.Input.IssueTitle,
			IssueBody:    c.Input.IssueBody,
			Repo:         "kuhlman-labs/fishhawk",
			ApprovedPlan: p,
		})
		if err != nil {
			t.Fatal(err)
		}
		if before != direct {
			t.Errorf("case %q: the standard render differs from a direct prompt.Build of the standard trigger", c.Name)
		}
		if strings.Contains(before, "Reviewer persona remit:") || len(spansOf(before, personaDocBegin, personaDocEnd)) != 0 {
			t.Errorf("case %q: the standard render carries a persona remit or a document envelope", c.Name)
		}
		pre := commonPrefixLen(before, r.Prompt)
		suf := commonSuffixLen(before[pre:], r.Prompt[pre:])
		if pre+suf != len(before) {
			t.Errorf("case %q: the persona render is not the standard render plus one contiguous insertion (prefix %d + suffix %d != %d)", c.Name, pre, suf, len(before))
			continue
		}
		inserted := r.Prompt[pre : len(r.Prompt)-suf]
		for _, d := range r.QuoteDocs {
			if !strings.Contains(inserted, "### "+d.Heading+"\n") {
				t.Errorf("case %q: the inserted span lacks document %s's heading", c.Name, d.Path)
			}
		}
	}
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSuffixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

// TestPersonaCatchCorpus_GoverningPremisesHold re-checks each case's planted
// premise against the REAL index, decoded independently of the loader: every
// governing record is listed with the declared status and superseded_by; a
// superseded_reliance case's superseded record names an accepted successor that
// is itself governing and lists the supersession back; an accepted_conflict
// case's records are accepted, unsuperseded and never named in its issue or
// plan (the persona must find the conflict from the index); and every governing
// record of a clean_area case is one the plan scope actually selects.
func TestPersonaCatchCorpus_GoverningPremisesHold(t *testing.T) {
	real := readRealIndex(t)
	idxRaw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaArchitectIndexPath)))
	idx, err := decisionrecord.ParseIndex(idxRaw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range loadCommittedPersona(t) {
		governing := map[string]PersonaGoverningRecord{}
		for _, g := range c.Input.GoverningRecords {
			governing[g.ID] = g
			r, ok := real[g.ID]
			if !ok {
				t.Errorf("case %q: governing record %s is absent from the real index", c.Name, g.ID)
				continue
			}
			if r.Status != g.Status || !sameStringSet(r.SupersededBy, g.SupersededBy) {
				t.Errorf("case %q: %s declared %s/%v, the real index says %s/%v", c.Name, g.ID, g.Status, g.SupersededBy, r.Status, r.SupersededBy)
			}
		}
		shown, err := catchShownText(PlanReviewCatchInput{IssueTitle: c.Input.IssueTitle, IssueBody: c.Input.IssueBody, Plan: c.Input.Plan})
		if err != nil {
			t.Fatal(err)
		}
		switch c.Input.Kind {
		case PersonaKindSupersededReliance:
			superseded := 0
			for _, g := range c.Input.GoverningRecords {
				for _, s := range g.SupersededBy {
					superseded++
					succ, ok := real[s]
					if _, gov := governing[s]; !ok || !gov || succ.Status != "accepted" || !slices.Contains(succ.Supersedes, g.ID) {
						t.Errorf("case %q: %s's successor %s must be an accepted governing record listing supersedes %s", c.Name, g.ID, s, g.ID)
					}
				}
			}
			if superseded == 0 {
				t.Errorf("case %q: a superseded_reliance case needs a governing record with superseded_by", c.Name)
			}
		case PersonaKindAcceptedConflict:
			for _, g := range c.Input.GoverningRecords {
				if g.Status != "accepted" || len(g.SupersededBy) != 0 {
					t.Errorf("case %q: %s must be accepted and unsuperseded", c.Name, g.ID)
				}
				if strings.Contains(shown, strings.ToLower(g.ID)) {
					t.Errorf("case %q: the issue or plan names %s: the persona would be told the governing record", c.Name, g.ID)
				}
			}
		case PersonaKindCleanArea:
			matches, err := decisionrecord.Select(idx, personaprompt.ChangePaths(personaScopePaths(c.Plan), nil))
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range matches {
				ids = append(ids, m.Record.ID)
			}
			for _, g := range c.Input.GoverningRecords {
				if !slices.Contains(ids, g.ID) {
					t.Errorf("case %q: governing record %s is not selected by the plan scope (selected %v)", c.Name, g.ID, ids)
				}
			}
		}
	}
}

// personaAnchorRefs returns how a concern names a catch case's anchor: "step
// N" (or the criterion id) and every attribution phrase.
func personaAnchorRefs(c PersonaCatchCase) []string {
	var refs []string
	switch c.Input.Anchor.Kind {
	case PersonaAnchorApproachStep:
		refs = append(refs, fmt.Sprintf("Step %d", c.Input.Anchor.Step))
	case PersonaAnchorCriterion:
		refs = append(refs, c.Input.Anchor.CriterionID)
	}
	return append(refs, c.Input.AttributionPhrases...)
}

// personaVerdict renders a persona verdict carrying one concern.
func personaVerdict(t *testing.T, category, note string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "low", "category": category, "note": note},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPersonaCatchCorpus_GenericObjectionsScoreMissed: for every catch case,
// each way of naming the anchor joined with EVERY generic objection word in
// one decision_record concern scores MISSED through the production persona
// classifier, and the case declares a non-catching example naming the anchor
// with a generic objection word, which also scores MISSED. A generic objection
// is never, by that word, a catch.
func TestPersonaCatchCorpus_GenericObjectionsScoreMissed(t *testing.T) {
	objection := strings.Join(genericObjectionWords, "; ")
	for _, c := range loadCommittedPersona(t) {
		if c.Expect() != CatchExpectCatch {
			continue
		}
		rule := c.CatchRule()
		for _, ref := range personaAnchorRefs(c) {
			note := ref + ": " + objection
			if _, ok := matchCatchAttribution(personaArchitectConcernCategory, note, rule); !ok {
				t.Fatalf("case %q: fixture premise: a concern naming %q must satisfy the attribution half", c.Name, ref)
			}
			if trial := ClassifyPersonaTrial(personaVerdict(t, personaArchitectConcernCategory, note), rule, nil, personaArchitectSeverityCap); trial.Outcome != CatchMissed {
				t.Errorf("case %q: %q joined with the generic objection words classified %q (probe %q), want missed", c.Name, ref, trial.Outcome, trial.MatchedProbe)
			}
		}
		pinned := false
		for _, ex := range c.Input.NonCatchingExamples {
			if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); !ok || !matchCatchCategory(ex.Category, rule) {
				continue
			}
			if _, ok := matchCatchProbe(ex.Category, ex.Note, genericObjectionWords); !ok {
				continue
			}
			if trial := ClassifyPersonaTrial(personaVerdict(t, ex.Category, ex.Note), rule, nil, personaArchitectSeverityCap); trial.Outcome != CatchMissed {
				t.Errorf("case %q: generic-objection example %q classified %q, want missed", c.Name, ex.Note, trial.Outcome)
			}
			pinned = true
		}
		if !pinned {
			t.Errorf("case %q has no anchored decision_record non-catching example carrying a generic objection word", c.Name)
		}
	}
}

// TestPersonaCatchCorpus_EchoesScoreMissed: a decision_record concern that
// names the anchor and merely ECHOES the case's own text — the anchored step
// verbatim, or the issue plus every criterion and every approach step — scores
// MISSED. A probe matching any of it would score every reviewer that quotes
// the plan as a catch.
func TestPersonaCatchCorpus_EchoesScoreMissed(t *testing.T) {
	for _, c := range loadCommittedPersona(t) {
		if c.Expect() != CatchExpectCatch {
			continue
		}
		rule := c.CatchRule()
		ref := personaAnchorRefs(c)[0]
		var anchored string
		echo := []string{ref + ":", c.Input.IssueTitle, c.Input.IssueBody}
		for _, s := range c.Plan.Approach {
			echo = append(echo, s.Description)
			if c.Input.Anchor.Kind == PersonaAnchorApproachStep && s.Step == c.Input.Anchor.Step {
				anchored = s.Description
			}
		}
		for _, ac := range c.Plan.Verification.AcceptanceCriteria {
			echo = append(echo, ac.ID+": "+ac.Statement)
			if c.Input.Anchor.Kind == PersonaAnchorCriterion && ac.ID == c.Input.Anchor.CriterionID {
				anchored = ac.Statement
			}
		}
		for _, note := range []string{ref + ": " + anchored, strings.Join(echo, "\n")} {
			if trial := ClassifyPersonaTrial(personaVerdict(t, personaArchitectConcernCategory, note), rule, nil, personaArchitectSeverityCap); trial.Outcome != CatchMissed {
				t.Errorf("case %q: an echo concern classified %q (probe %q), want missed: %q", c.Name, trial.Outcome, trial.MatchedProbe, note)
			}
		}
	}
}

// neutralPersonaCase is the corpus-disjoint lighthouse trigger
// (neutralLeakPlan) as a persona case. Its scope matches no applies_to glob,
// so its persona render is the static persona surface: the plan-review
// prompt, the remit and the full index framing, with no record in full.
func neutralPersonaCase(t *testing.T) PersonaCatchCase {
	t.Helper()
	n := neutralLeakCase(t)
	return PersonaCatchCase{
		Name:  n.Name,
		Input: PersonaCaseInput{Persona: ArmArchitect, IssueTitle: n.Input.IssueTitle, IssueBody: n.Input.IssueBody, Plan: n.Input.Plan},
		Plan:  n.Plan,
	}
}

// TestPersonaCatchCorpus_NotLeakedIntoPersonaPrompt is the persona arm's
// prompt-leak self-check: no case's defect phrase occurs in a STATIC persona
// surface — the neutral persona render (the plan-review prompt, the real
// remit and the real index) or the persona generator system prompt. A hit
// would show every architect reviewer the planted defect in every render. The
// neutral trigger's own text provably carries no needle, so a hit comes from
// static bytes. Both sides are normalized (normalizeLeakText).
func TestPersonaCatchCorpus_NotLeakedIntoPersonaPrompt(t *testing.T) {
	cases := loadCommittedPersona(t)
	neutral := neutralPersonaCase(t)
	r, err := PersonaArmPrompt(neutral, personaTestRepoRoot)
	if err != nil {
		t.Fatalf("neutral persona render: %v", err)
	}
	for _, premise := range []string{"Reviewer persona remit: " + ArmArchitect, `"id": "ADR-094"`, "8a. **Issue fidelity**"} {
		if !strings.Contains(r.Prompt, premise) {
			t.Fatalf("fixture premise: the neutral persona render lacks %q, so the leak check would be vacuous", premise)
		}
	}
	surfaces := []struct{ name, text string }{
		{"neutral persona render", normalizeLeakText(r.Prompt)},
		{"persona generator system prompt", normalizeLeakText(personaCatchGeneratorSystemPrompt)},
	}
	shown, err := catchShownText(PlanReviewCatchInput{IssueTitle: neutral.Input.IssueTitle, IssueBody: neutral.Input.IssueBody, Plan: neutral.Input.Plan})
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
					t.Errorf("case %q: defect phrase %q occurs in the %s: the prompt shows every architect reviewer this case's planted defect", c.Name, phrase, s.name)
				}
			}
		}
	}
	if needles == 0 {
		t.Fatal("no needles: the leak check is vacuous")
	}
}
