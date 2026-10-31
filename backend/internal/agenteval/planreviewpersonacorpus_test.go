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
// while a genuine premise break (ADR-091 losing amended_by, ADR-006's index
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
	Amends       []string `json:"amends"`
	AmendedBy    []string `json:"amended_by"`
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
// AND its title, plus each successor's and each amendment's entry) must be
// PRESENT in the RENDERED,
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
		if want := 2 + len(r.IncludedIDs) + len(r.Selection.Excerpted); len(r.QuoteDocs) != want {
			t.Errorf("case %q: %d rendered documents, want the remit, the index, %d included records and %d excerpts", c.Name, len(r.QuoteDocs), len(r.IncludedIDs), len(r.Selection.Excerpted))
		}
		idxSpan := spans[personaArchitectIndexPath]
		indexText := r.Prompt[idxSpan.start:idxSpan.end]
		need := map[string]bool{}
		for _, g := range c.Input.GoverningRecords {
			need[g.ID] = true
			for _, s := range g.SupersededBy {
				need[s] = true
			}
			for _, a := range g.AmendedBy {
				need[a] = true
			}
		}
		for id := range need {
			if !strings.Contains(indexText, `"id":"`+id+`"`) {
				t.Errorf("case %q: the rendered index lacks %s's entry: an index truncation dropped a record the case depends on", c.Name, id)
				continue
			}
			if title := real[id].Title; title == "" || !strings.Contains(indexText, jsonEncodedTitle(t, title)) {
				t.Errorf("case %q: the rendered index lacks %s's title %q", c.Name, id, title)
			}
		}
		t.Logf("case %q: matched %v included %v excerpted %d dropped %v; index rendered %d bytes (truncated %v, cap %d)",
			c.Name, r.MatchedIDs, r.IncludedIDs, len(r.Selection.Excerpted), r.DroppedIDs, r.Selection.Index.RenderedBytes, r.Selection.Index.Truncated, r.Selection.CapBytes)
	}
}

// TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona is a
// deliberate #4399 tripwire, separate from the logged-not-pinned selection in
// TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes: rendered
// through PersonaArmPrompt (the production path, the real docs/adr at the
// production cap), every case shows at least min(2, matched) record bodies in
// full, and a CATCH case shows every governing record the selection matched
// in FULL — a governing record shown only as its Decision-section excerpt
// (#4489) fails it too. Before #4399 the index alone filled the shared budget
// and every matched record was dropped, so adr006-production-migrate-down
// showed no ADR-006 body. A governing record the selection does not match
// (adr091 today) is vacuous here. The matched set is read from the Selection
// (included, excerpted and dropped), since PersonaArmRender.MatchedIDs does
// not carry excerpts.
func TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona(t *testing.T) {
	for _, c := range loadCommittedPersona(t) {
		r, err := PersonaArmPrompt(c, personaTestRepoRoot)
		if err != nil {
			t.Fatalf("case %q: PersonaArmPrompt: %v", c.Name, err)
		}
		var matched, excerpted []string
		for _, inc := range r.Selection.Included {
			matched = append(matched, inc.Record.ID)
		}
		for _, ex := range r.Selection.Excerpted {
			matched = append(matched, ex.Record.ID)
			excerpted = append(excerpted, ex.Record.ID)
		}
		for _, d := range r.Selection.Dropped {
			matched = append(matched, d.Record.ID)
		}
		if want := min(2, len(matched)); len(r.IncludedIDs) < want {
			t.Errorf("case %q: %d record bodies shown in full (included %v, excerpted %v, dropped %v), want >= %d", c.Name, len(r.IncludedIDs), r.IncludedIDs, excerpted, r.DroppedIDs, want)
		}
		if c.Expect() != CatchExpectCatch {
			continue
		}
		for _, g := range c.Input.GoverningRecords {
			if slices.Contains(matched, g.ID) && !slices.Contains(r.IncludedIDs, g.ID) {
				t.Errorf("case %q: governing record %s matched but its full body was not shown (included %v, excerpted %v, dropped %v)", c.Name, g.ID, r.IncludedIDs, excerpted, r.DroppedIDs)
			}
		}
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

// supersededReliancePremiseErrors is the superseded_reliance premise: some
// governing record has a REPLACING link, and every replacing link holds in the
// real index. A superseded_by successor must be an accepted GOVERNING record
// listing supersedes back. An amended_by amendment must be an accepted record
// in the real index listing amends back, whether or not the case also lists it
// as governing (E78.13 / #4471): a case may rest on an amended record whose
// amendment's full body the record budget cannot fit (adr040, whose ADR-101 is
// ~23 KB), and the persona then sees the amendment only through the index
// metadata and, when it has one that fits, its Decision-section excerpt
// (#4489). A non-governing amendment is CHECKED against the real index, never
// skipped.
func supersededReliancePremiseErrors(records []PersonaGoverningRecord, real map[string]realIndexEntry) []string {
	governing := map[string]bool{}
	for _, g := range records {
		governing[g.ID] = true
	}
	var errs []string
	replaced := 0
	for _, g := range records {
		for _, s := range g.SupersededBy {
			replaced++
			succ, ok := real[s]
			if !ok || !governing[s] || succ.Status != "accepted" || !slices.Contains(succ.Supersedes, g.ID) {
				errs = append(errs, fmt.Sprintf("%s's successor %s must be an accepted governing record listing supersedes %s", g.ID, s, g.ID))
			}
		}
		for _, a := range g.AmendedBy {
			replaced++
			am, ok := real[a]
			switch {
			case !ok:
				errs = append(errs, fmt.Sprintf("%s's amendment %s is absent from the real index", g.ID, a))
			case am.Status != "accepted" || !slices.Contains(am.Amends, g.ID):
				errs = append(errs, fmt.Sprintf("%s's amendment %s must be an accepted record listing amends %s (governing: %t)", g.ID, a, g.ID, governing[a]))
			}
		}
	}
	if replaced == 0 {
		errs = append(errs, "a superseded_reliance case needs a governing record with superseded_by or amended_by")
	}
	return errs
}

// TestSupersededReliancePremise_Rows pins supersededReliancePremiseErrors on
// hand-built real indexes: each refusal row differs from the accepted adr040
// shape (ADR-040 governing, amended_by [ADR-097, ADR-101], neither governing)
// in ONE fact about one non-governing amendment.
func TestSupersededReliancePremise_Rows(t *testing.T) {
	adr040 := []PersonaGoverningRecord{{ID: "ADR-040", Status: "accepted", SupersededBy: []string{}, AmendedBy: []string{"ADR-097", "ADR-101"}}}
	realWith := func(edit func(map[string]realIndexEntry)) map[string]realIndexEntry {
		r := map[string]realIndexEntry{
			"ADR-040": {ID: "ADR-040", Status: "accepted", AmendedBy: []string{"ADR-097", "ADR-101"}},
			"ADR-097": {ID: "ADR-097", Status: "accepted", Amends: []string{"ADR-040", "ADR-089"}, AmendedBy: []string{"ADR-101"}},
			"ADR-101": {ID: "ADR-101", Status: "accepted", Amends: []string{"ADR-040", "ADR-089", "ADR-097"}},
		}
		if edit != nil {
			edit(r)
		}
		return r
	}
	for _, tc := range []struct {
		name    string
		real    map[string]realIndexEntry
		records []PersonaGoverningRecord
		want    string // "" = accepted
	}{
		{"adr040 shape accepted", realWith(nil), adr040, ""},
		{"non-governing amendment missing from the index", realWith(func(r map[string]realIndexEntry) { delete(r, "ADR-101") }), adr040, "ADR-040's amendment ADR-101 is absent from the real index"},
		{"non-governing amendment not listing amends back", realWith(func(r map[string]realIndexEntry) {
			e := r["ADR-101"]
			e.Amends = []string{"ADR-089", "ADR-097"}
			r["ADR-101"] = e
		}), adr040, "ADR-040's amendment ADR-101 must be an accepted record listing amends ADR-040 (governing: false)"},
		{"non-governing amendment not accepted", realWith(func(r map[string]realIndexEntry) {
			e := r["ADR-097"]
			e.Status = "proposed"
			r["ADR-097"] = e
		}), adr040, "ADR-040's amendment ADR-097 must be an accepted record listing amends ADR-040 (governing: false)"},
		{"no replacing link", realWith(nil), []PersonaGoverningRecord{{ID: "ADR-040", Status: "accepted", SupersededBy: []string{}}}, "needs a governing record with superseded_by or amended_by"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := supersededReliancePremiseErrors(tc.records, tc.real)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("refused the accepted shape: %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
				t.Fatalf("errors = %v, want exactly one naming %q", errs, tc.want)
			}
		})
	}
}

// TestPersonaCatchCorpus_GoverningPremisesHold re-checks each case's planted
// premise against the REAL index, decoded independently of the loader: every
// governing record is listed with the declared status, superseded_by and
// amended_by; a superseded_reliance case rests on a governing record a later
// decision replaced — through superseded_by (each successor an accepted
// governing record listing the supersession back) or amended_by (at least one
// amendment an accepted governing record listing amends back; E78.13 / #4471:
// remit item 2 covers reliance on a decision an amendment changed); an
// accepted_conflict case's records are accepted, unsuperseded, unamended and
// never named in its issue or
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
		for _, g := range c.Input.GoverningRecords {
			r, ok := real[g.ID]
			if !ok {
				t.Errorf("case %q: governing record %s is absent from the real index", c.Name, g.ID)
				continue
			}
			if r.Status != g.Status || !sameStringSet(r.SupersededBy, g.SupersededBy) || !sameStringSet(r.AmendedBy, g.AmendedBy) {
				t.Errorf("case %q: %s declared %s/superseded_by %v/amended_by %v, the real index says %s/%v/%v",
					c.Name, g.ID, g.Status, g.SupersededBy, g.AmendedBy, r.Status, r.SupersededBy, r.AmendedBy)
			}
		}
		shown, err := catchShownText(PlanReviewCatchInput{IssueTitle: c.Input.IssueTitle, IssueBody: c.Input.IssueBody, Plan: c.Input.Plan})
		if err != nil {
			t.Fatal(err)
		}
		switch c.Input.Kind {
		case PersonaKindSupersededReliance:
			for _, msg := range supersededReliancePremiseErrors(c.Input.GoverningRecords, real) {
				t.Errorf("case %q: %s", c.Name, msg)
			}
		case PersonaKindAcceptedConflict:
			for _, g := range c.Input.GoverningRecords {
				if g.Status != "accepted" || len(g.SupersededBy) != 0 || len(g.AmendedBy) != 0 {
					t.Errorf("case %q: %s must be accepted, unsuperseded and unamended", c.Name, g.ID)
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
	for _, premise := range []string{"Reviewer persona remit: " + ArmArchitect, `"id":"ADR-094"`, "8a. **Issue fidelity**"} {
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

// adr040AmendmentEdges is the E78.13 / #4471 encoding of ADR-040 and the
// records amending it (docs/adr/README.md § "Amendment relation"): ADR-097
// and ADR-101 amend ADR-040 and ADR-089, ADR-101 also amends ADR-097, and none
// of the four supersedes another. A record not listed keeps its shipped links.
var adr040AmendmentEdges = map[string]struct{ amends, amendedBy []string }{
	"ADR-040": {nil, []string{"ADR-097", "ADR-101"}},
	"ADR-089": {nil, []string{"ADR-097", "ADR-101"}},
	"ADR-097": {[]string{"ADR-040", "ADR-089"}, []string{"ADR-101"}},
	"ADR-101": {[]string{"ADR-040", "ADR-085", "ADR-089", "ADR-097"}, []string{"ADR-121", "ADR-122"}},
}

// TestPersonaPrompt_AmendmentFramingCrossesTheBoundary is approval
// conditions 1-3: the SHIPPED docs/adr (index, record bodies, remit) driven
// through decisionrecord.ParseIndex -> Select -> Assemble -> the production
// persona builder (PersonaArmPrompt) on ADR-040's REAL applies_to path,
// asserting on the RENDERED prompt, not only on Match metadata: ADR-097 and
// ADR-101 are selected through amended_by (AmendsVia, never Via), ADR-040's
// body ranks ahead of both and is shown whenever either is, and the amendment
// count and framing sentences reach the prompt text. ADR-101, too large to fit
// whole, reaches the prompt as its Decision-section excerpt (#4489): its
// Decision-only Q4 answer is in the rendered prompt and its
// Recommendation-only Q4 question is not.
//
// The index is the shipped one with adr040AmendmentEdges applied. Once the
// records carry that encoding the overlay must change nothing, so from then on
// the test exercises the shipped index byte-for-byte in substance; before it,
// the overlay supplies the edges the reader side must follow.
func TestPersonaPrompt_AmendmentFramingCrossesTheBoundary(t *testing.T) {
	const operatorScope = ".fishhawk/operator.yaml"
	raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaArchitectIndexPath)))
	idx, err := decisionrecord.ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex(shipped index): %v", err)
	}
	shippedEncoded, changed := false, false
	for i := range idx.Records {
		r := &idx.Records[i]
		e, ok := adr040AmendmentEdges[r.ID]
		if !ok {
			continue
		}
		if r.ID == "ADR-040" {
			if !slices.Contains(r.AppliesTo, operatorScope) {
				t.Fatalf("ADR-040's shipped applies_to %v no longer names %s: re-anchor this test on ADR-040's real scope", r.AppliesTo, operatorScope)
			}
			shippedEncoded = len(r.AmendedBy) > 0
		}
		if len(r.Supersedes) != 0 || len(r.SupersededBy) != 0 || !sameStringSet(r.Amends, e.amends) || !sameStringSet(r.AmendedBy, e.amendedBy) {
			changed = true
		}
		r.Supersedes, r.SupersededBy, r.Amends, r.AmendedBy = nil, nil, e.amends, e.amendedBy
	}
	if shippedEncoded && changed {
		t.Fatal("the shipped index encodes ADR-040's amendments but differs from adr040AmendmentEdges: update the edges (or the records) so they agree")
	}
	t.Logf("shipped index already encodes the amendments: %v", shippedEncoded)
	overlaid, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decisionrecord.ParseIndex(overlaid); err != nil {
		t.Fatalf("the overlaid index does not parse: %v", err)
	}

	root := t.TempDir()
	writeFile := func(rel string, content []byte) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(personaArchitectRemitPath, mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(personaArchitectRemitPath))))
	writeFile(personaArchitectIndexPath, overlaid)
	for _, r := range idx.Records {
		writeFile(r.Path, mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(r.Path))))
	}

	c := loadCommittedPersona(t)[0]
	p := *c.Plan
	p.Scope.Files = []plan.ScopeFile{{Path: operatorScope, Operation: plan.FileOpModify}}
	c.Plan = &p
	r, err := PersonaArmPrompt(c, root)
	if err != nil {
		t.Fatalf("PersonaArmPrompt: %v", err)
	}

	ranked := map[string]decisionrecord.Match{}
	included, excerpted, dropped := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, inc := range r.Selection.Included {
		ranked[inc.Record.ID] = inc.Match
		included[inc.Record.ID] = true
	}
	for _, ex := range r.Selection.Excerpted {
		ranked[ex.Record.ID] = ex.Match
		excerpted[ex.Record.ID] = true
	}
	for _, d := range r.Selection.Dropped {
		ranked[d.Record.ID] = d
		dropped[d.Record.ID] = true
	}
	p040, ok40 := ranked["ADR-040"]
	if !ok40 || p040.MatchedPaths == 0 {
		t.Fatalf("ADR-040 not selected directly by %s (matched %v)", operatorScope, r.MatchedIDs)
	}
	for _, w := range []struct {
		id        string
		amendsVia []string
	}{{"ADR-097", []string{"ADR-040"}}, {"ADR-101", []string{"ADR-040", "ADR-097"}}} {
		m, ok := ranked[w.id]
		if !ok {
			t.Errorf("%s not selected (matched %v): ADR-040's amended_by was not followed", w.id, r.MatchedIDs)
			continue
		}
		if !slices.Equal(m.AmendsVia, w.amendsVia) || len(m.Via) != 0 || m.MatchedPaths != 0 {
			t.Errorf("%s = {amendsVia %v via %v matched %d}, want {amendsVia %v via [] matched 0}: an amendment, never a successor", w.id, m.AmendsVia, m.Via, m.MatchedPaths, w.amendsVia)
		}
		if m.Rank <= p040.Rank {
			t.Errorf("%s ranks %d, ahead of the record it amends (ADR-040 ranks %d)", w.id, m.Rank, p040.Rank)
		}
		if (included[w.id] || excerpted[w.id]) && !included["ADR-040"] && !excerpted["ADR-040"] {
			t.Errorf("%s is shown but ADR-040 is not (included %v)", w.id, r.IncludedIDs)
		}
	}
	if r.Selection.Amendments < 2 {
		t.Fatalf("selection counts %d amendments, want >= 2 (ADR-097, ADR-101)", r.Selection.Amendments)
	}

	wants := []string{
		fmt.Sprintf("%d of the selected records amend a selected record, and an amended record still governs, except where an amendment decides otherwise; ", r.Selection.Amendments),
		"an amendment ranks with the records it amends, after them",
	}
	if included["ADR-040"] {
		wants = append(wants, "Amended by ADR-097, ADR-101: it still governs, except where an amendment decides otherwise.")
	}
	for _, w := range []struct{ id, path, amends string }{
		{"ADR-097", "docs/adr/097-operator-as-orchestrator.md", "ADR-040"},
		{"ADR-101", "docs/adr/101-charter-authorization-mode.md", "ADR-040, ADR-097"},
	} {
		switch {
		case included[w.id] || excerpted[w.id]:
			wants = append(wants, "This record was selected because it amends "+w.amends+"; the amended record still governs except where this record decides otherwise.")
			if excerpted[w.id] {
				wants = append(wants, "Decision record "+w.id+" — status: accepted (Decision section only)")
			}
		case dropped[w.id]:
			wants = append(wants, "- "+w.id+" ("+w.path+", status: accepted, amendment of "+w.amends+")")
		}
	}
	for _, want := range wants {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("the rendered persona prompt lacks %q", want)
		}
	}
	// ADR-101's Decision text reaches the rendered prompt (#4489). The two Q4
	// anchors each occur once in ADR-101's file — the answer under Decision,
	// the question under Recommendation — and in no other shipped document
	// the prompt can show, so the answer proves the Decision section was shown
	// and the question's absence proves nothing outside it was.
	const (
		adr101Path          = "docs/adr/101-charter-authorization-mode.md"
		decisionAnchor      = "**Q4: yes.** Agent classification before issuance"
		recommendationProbe = "Should the charter subject be classified"
	)
	file := string(mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(adr101Path))))
	for _, anchor := range []string{decisionAnchor, recommendationProbe} {
		if n := strings.Count(file, anchor); n != 1 {
			t.Fatalf("premise: %q occurs %d times in %s, want exactly once", anchor, n, adr101Path)
		}
	}
	if dropped["ADR-101"] {
		t.Errorf("ADR-101 was dropped (included %v, dropped %v): its Decision section must be shown", r.IncludedIDs, r.DroppedIDs)
	}
	for _, want := range []string{decisionAnchor, "holding only the lease credential"} {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("the rendered persona prompt lacks ADR-101's Decision text %q", want)
		}
	}
	if strings.Contains(r.Prompt, recommendationProbe) {
		t.Errorf("the rendered persona prompt carries ADR-101's Recommendation-only %q: the excerpt leaked text outside the Decision section", recommendationProbe)
	}
	t.Logf("operator.yaml scope: matched %v included %v excerpted %d dropped %v; amendments %d; record budget %d, %d used",
		r.MatchedIDs, r.IncludedIDs, len(r.Selection.Excerpted), r.DroppedIDs, r.Selection.Amendments, r.Selection.CapBytes, r.Selection.IncludedBytes)
}
