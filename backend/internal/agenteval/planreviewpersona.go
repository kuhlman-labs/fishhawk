// Architect persona catch arm (E78.11 / #4313): does the architect reviewer
// persona — rendered through the SAME production persona builder the review
// loop uses (personaprompt.Build), over the REAL docs/review/architect-remit.md
// and the ADR-084 D4(b) decision-record selection from the REAL
// docs/adr/index.json — catch a plan that conflicts with a recorded decision,
// and does it stay quiet on a plan that conforms?
//
// Each persona case (testdata/planreview-persona-corpus/<persona>/<case>/)
// carries a persona_case.json: the issue text, a COMPLETE standard_v1 plan,
// the governing records the planted premise rests on (checked against the
// real index), and either a catch rule (anchored at an approach step or an
// acceptance criterion) or a clean expectation. Scoring is catch rule v5
// (planreviewcatch.go) through the shared classifyCatchConcerns, AFTER the
// review loop's ingest order has run on the decoded verdict — the convention
// clamp (nil caps: no convention renders for the architect), the marker scrub,
// then the production persona-ingest controls (quote verification against the
// injected documents, then the persona's severity cap —
// personaprompt.ApplyConcernControls) — so a trial is scored on what the
// review loop would store (ClassifyPersonaTrialForRound).
//
// The documents are read from a checkout (repoRoot) through a filesystem
// fetcher inside a real repodoc.Resolver at the production DefaultMaxBytes
// cap, pinned to a fixed fixture commit label, so the selection, truncation
// and framing are the production ones. Everything here is offline and tested
// with fake senders; the live measurement belongs to the evidence recorder.

package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionrecord"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/personaprompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ArmArchitect is the architect persona arm's name.
const ArmArchitect = "architect"

// The fixed architect declaration the persona arm renders under: the values a
// .fishhawk/workflows.yaml reviewer_personas.architect entry would declare.
// They are literals, never derived from the checkout, so the rendered bytes
// depend only on the documents' content.
const (
	personaArchitectRemitPath   = "docs/review/architect-remit.md"
	personaArchitectSeverityCap = "medium"
	personaArchitectIndexPath   = "docs/adr/index.json"
	// personaArchitectConcernCategory is the category predicate: a concern counts
	// (as a catch, or as a clean case's false alarm) only when it is a
	// decision_record concern.
	personaArchitectConcernCategory = "decision_record"
	// personaCatchRepoOwner / personaCatchRepoName is the repository the
	// persona cases' plans belong to; it renders in the reviewer's role line.
	personaCatchRepoOwner = "kuhlman-labs"
	personaCatchRepoName  = "fishhawk"
	// personaFixtureCommit is the fixed 40-hex commit label every persona
	// document read is pinned to. The filesystem fetcher refuses any other
	// ref, so a read can never silently resolve a branch.
	personaFixtureCommit = "4313000000000000000000000000000000004313"
)

// personaCatchGeneratorSystemPrompt is the fixed harness instruction the
// persona arm sends to the generator. It names the persona verdict contract,
// including quoted_passage and document_ref, so the quote-verification control
// has something to verify. It carries no case content (the leak test pins it).
const personaCatchGeneratorSystemPrompt = `You are a Fishhawk plan-review agent reviewing as a reviewer persona. Read the stage prompt the user message contains and produce the review verdict it asks for as a JSON object carrying verdict, concerns (each with severity, category and note, plus quoted_passage and document_ref when the concern rests on a passage of an injected repository document), and optional free_form. Respond with the verdict JSON only.`

// The persona case kinds and the expectation each one implies.
const (
	PersonaKindAcceptedConflict    = "accepted_conflict"
	PersonaKindSupersededReliance  = "superseded_reliance"
	PersonaKindUndeclaredDirection = "undeclared_direction"
	PersonaKindDeclaredDirection   = "declared_direction"
	PersonaKindCleanArea           = "clean_area"
)

var personaKindExpect = map[string]string{
	PersonaKindAcceptedConflict:    CatchExpectCatch,
	PersonaKindSupersededReliance:  CatchExpectCatch,
	PersonaKindUndeclaredDirection: CatchExpectCatch,
	PersonaKindDeclaredDirection:   CatchExpectClean,
	PersonaKindCleanArea:           CatchExpectClean,
}

// The two anchor kinds a persona catch case declares.
const (
	PersonaAnchorApproachStep = "approach_step"
	PersonaAnchorCriterion    = "criterion"
)

// PersonaGoverningRecord is one record a persona case's premise rests on, as
// the case expects the REAL index to list it. AmendedBy is optional (E78.13 /
// #4471): absent is empty, as in the index.
type PersonaGoverningRecord struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	SupersededBy []string `json:"superseded_by"`
	AmendedBy    []string `json:"amended_by,omitempty"`
}

// PersonaCaseAnchor names WHERE in the plan a catch case's defect sits.
type PersonaCaseAnchor struct {
	Kind        string `json:"kind"`
	Step        int    `json:"step,omitempty"`
	CriterionID string `json:"criterion_id,omitempty"`
}

// PersonaCaseInput is the persona_case.json shape of one persona case.
type PersonaCaseInput struct {
	Persona    string `json:"persona"`
	Kind       string `json:"kind"`
	Expect     string `json:"expect"`
	IssueTitle string `json:"issue_title"`
	IssueBody  string `json:"issue_body"`
	// Plan is a COMPLETE standard_v1 plan object (validated by plan.Parse).
	Plan             json.RawMessage          `json:"plan"`
	GoverningRecords []PersonaGoverningRecord `json:"governing_records"`
	DefectPhrases    []string                 `json:"defect_phrases"`

	// Catch-case fields.
	Anchor              *PersonaCaseAnchor    `json:"anchor,omitempty"`
	CatchProbes         []string              `json:"catch_probes,omitempty"`
	AttributionPhrases  []string              `json:"attribution_phrases,omitempty"`
	CatchingExamples    []CatchExampleConcern `json:"catching_examples,omitempty"`
	NonCatchingExamples []CatchExampleConcern `json:"non_catching_examples,omitempty"`

	// Clean-case fields.
	FalseAlarmExamples []CatchExampleConcern `json:"false_alarm_examples,omitempty"`
	BenignExamples     []CatchExampleConcern `json:"benign_examples,omitempty"`
	PairOf             string                `json:"pair_of,omitempty"`
	MinMatchedRecords  int                   `json:"min_matched_records,omitempty"`
}

// PersonaCatchCase is one fully-loaded persona case.
type PersonaCatchCase struct {
	// Name is the case directory name — the per-case key everywhere.
	Name string
	// Input is the decoded persona_case.json.
	Input PersonaCaseInput
	// Plan is Input.Plan parsed by the real plan.Parse.
	Plan *plan.Plan
	// Raw is the exact committed persona_case.json bytes (an
	// evidence-fingerprint input).
	Raw []byte
}

// Expect is the case's expectation class (CatchExpectCatch or
// CatchExpectClean).
func (c PersonaCatchCase) Expect() string { return c.Input.Expect }

// CatchRule returns the case's catch rule v5: a clean rule for a clean case,
// otherwise the probes, the anchor (step or criterion id) and the attribution
// phrases; both under the architect's decision_record category predicate.
func (c PersonaCatchCase) CatchRule() CatchRule {
	return personaCatchRuleFor(c.Input)
}

func personaCatchRuleFor(in PersonaCaseInput) CatchRule {
	if in.Expect == CatchExpectClean {
		return CatchRule{Clean: true, Category: personaArchitectConcernCategory}
	}
	rule := CatchRule{Probes: in.CatchProbes, AttributionPhrases: in.AttributionPhrases, Category: personaArchitectConcernCategory}
	if in.Anchor != nil {
		switch in.Anchor.Kind {
		case PersonaAnchorApproachStep:
			rule.AnchorStep = in.Anchor.Step
		case PersonaAnchorCriterion:
			rule.CriterionIDs = []string{in.Anchor.CriterionID}
		}
	}
	return rule
}

// personaFSFetcher reads repository documents from a checkout rooted at root.
// It refuses any ref other than personaFixtureCommit and maps a missing file
// to forge.ErrNotFound, which the resolver turns into
// repodoc.ErrMissingDocument.
type personaFSFetcher struct {
	root string
}

func (f personaFSFetcher) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, path, ref string) (*forge.FileContent, error) {
	if ref != personaFixtureCommit {
		return nil, fmt.Errorf("agenteval: persona fixture fetcher reads only at the fixture commit %s, got ref %q", personaFixtureCommit, ref)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(path)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("agenteval: persona fixture %s: %w", path, forge.ErrNotFound)
		}
		return nil, fmt.Errorf("agenteval: persona fixture %s: %w", path, err)
	}
	return &forge.FileContent{Path: path, Content: raw}, nil
}

// personaResolver is a real repodoc.Resolver over the checkout at repoRoot,
// at the production DefaultMaxBytes cap.
func personaResolver(repoRoot string) *repodoc.Resolver {
	return &repodoc.Resolver{Fetcher: personaFSFetcher{root: repoRoot}, MaxBytes: repodoc.DefaultMaxBytes}
}

// PersonaStandardTrigger is the standard reviewers' plan_review trigger for
// one persona case: no conventions and no injected documents.
func PersonaStandardTrigger(c PersonaCatchCase) prompt.Trigger {
	return prompt.Trigger{
		Source:       "github_issue",
		IssueTitle:   c.Input.IssueTitle,
		IssueBody:    c.Input.IssueBody,
		Repo:         personaCatchRepoOwner + "/" + personaCatchRepoName,
		ApprovedPlan: c.Plan,
	}
}

// PersonaArmRender is one case's rendered architect persona prompt.
type PersonaArmRender struct {
	// Prompt is the persona prompt.
	Prompt string
	// QuoteDocs is every document the prompt renders — what a quoted_passage
	// is verified against.
	QuoteDocs []prompt.InjectedDocument
	// Selection is the assembled decision record.
	Selection *decisionrecord.Selection
	// IncludedIDs / DroppedIDs are the selection's included and dropped
	// record ids, in rank order; MatchedIDs is both, included first.
	MatchedIDs, IncludedIDs, DroppedIDs []string
}

// PersonaArmPrompt renders one case's architect persona prompt through the
// production path: the remit resolved with personaprompt.Framing, the decision
// record assembled by decisionrecord.Assemble over
// personaprompt.ChangePaths(plan scope, nil), and personaprompt.Build
// ("plan_review", the standard trigger, an ungrounded round).
func PersonaArmPrompt(c PersonaCatchCase, repoRoot string) (PersonaArmRender, error) {
	ctx := context.Background()
	r := personaResolver(repoRoot)
	repo := forge.RepoRef{Owner: personaCatchRepoOwner, Name: personaCatchRepoName}
	remit, err := r.Resolve(ctx, repodoc.Request{
		Repo:    repo,
		BaseRef: personaFixtureCommit,
		Declaration: repodoc.Declaration{
			Path:            personaArchitectRemitPath,
			DeclarationSite: fmt.Sprintf(spec.DeclarationSiteFmtReviewerPersonaRemit, ArmArchitect),
			Framing:         personaprompt.Framing(ArmArchitect),
			Base:            repodoc.BaseSourceRunAdmission,
		},
	})
	if err != nil {
		return PersonaArmRender{}, fmt.Errorf("agenteval: persona case %q: resolve remit: %w", c.Name, err)
	}
	sel, err := decisionrecord.Assemble(ctx, r, decisionrecord.Request{
		Repo:            repo,
		Commit:          personaFixtureCommit,
		IndexPath:       personaArchitectIndexPath,
		DeclarationSite: fmt.Sprintf(spec.DeclarationSiteFmtReviewerPersonaDecisionRecord, ArmArchitect),
		ChangePaths:     personaprompt.ChangePaths(personaScopePaths(c.Plan), nil),
	})
	if err != nil {
		return PersonaArmRender{}, fmt.Errorf("agenteval: persona case %q: assemble decision record: %w", c.Name, err)
	}
	res, err := personaprompt.Build(personaprompt.Input{
		Kind:           "plan_review",
		Trigger:        PersonaStandardTrigger(c),
		PersonaName:    ArmArchitect,
		Remit:          *remit,
		DecisionRecord: sel,
	})
	if err != nil {
		return PersonaArmRender{}, fmt.Errorf("agenteval: persona case %q: build persona prompt: %w", c.Name, err)
	}
	out := PersonaArmRender{Prompt: res.Prompt, QuoteDocs: res.QuoteDocs, Selection: sel}
	for _, inc := range sel.Included {
		out.IncludedIDs = append(out.IncludedIDs, inc.Record.ID)
	}
	for _, d := range sel.Dropped {
		out.DroppedIDs = append(out.DroppedIDs, d.Record.ID)
	}
	out.MatchedIDs = append(append([]string{}, out.IncludedIDs...), out.DroppedIDs...)
	return out, nil
}

func personaScopePaths(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.Scope.Files))
	for _, f := range p.Scope.Files {
		out = append(out, f.Path)
	}
	return out
}

// LoadPersonaCatchCorpus loads every case under dir for the named persona arm,
// checking each case's governing premises against the REAL index read from
// repoRoot. It FAILS CLOSED with an error naming the case on:
//
//	(a) an absent corpus dir, or zero cases
//	(b) a case with no persona_case.json, malformed JSON, an unknown field or
//	    trailing content after the JSON value
//	(c) persona differing from the arm
//	(d) an unknown kind or expect, or a kind/expect mismatch
//	(e) an empty issue_title or issue_body, or a plan plan.Parse rejects
//	(f) empty, blank or absent defect_phrases, or a defect phrase absent from
//	    the issue and plan (v4 mode r)
//	(g) empty governing_records, or a governing record absent from the real
//	    index or listed there with a different status, superseded_by or
//	    amended_by
//	(h) catch-only fields on a clean case, or clean-only fields on a catch case
//	(i) catch: an absent or unknown anchor, or an anchored step or criterion
//	    absent from the plan
//	(j) catch: empty or blank catch_probes or attribution_phrases
//	(k) catch: an attribution phrase absent from the anchored step (or
//	    criterion), or present in another step's description or another
//	    criterion's statement
//	(l) catch: empty catching_examples or non_catching_examples; a
//	    catching_example not CAUGHT; a non_catching_example caught; a probe
//	    matching a non_catching_example (v4 mode j); no non_catching_example
//	    carrying the attribution half (v4 mode n)
//	(m) catch: a probe occurring in the issue or plan text (v4 mode m)
//	(n) clean: empty false_alarm_examples or benign_examples; a
//	    false_alarm_example not classified false_alarm, or a benign_example
//	    classified false_alarm
//	(o) clean: a declared_direction case without pair_of, or pair_of on any
//	    other kind; a pair_of naming an absent case, a pair that is not
//	    undeclared_direction (catch) vs declared_direction (clean), or a pair
//	    whose plans differ in any field other than new_architectural_decision
//	(p) clean: a clean_area case without min_matched_records >= 1, or one on
//	    any other kind; a clean_area case whose plan scope matches fewer than
//	    min_matched_records index records (decisionrecord.Select)
func LoadPersonaCatchCorpus(dir, repoRoot, persona string) ([]PersonaCatchCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agenteval: persona catch corpus dir %q is absent: the persona arm cannot run without it", dir) // (a)
		}
		return nil, fmt.Errorf("agenteval: persona catch corpus dir %q: %w", dir, err)
	}
	idxRaw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(personaArchitectIndexPath)))
	if err != nil {
		return nil, fmt.Errorf("agenteval: persona catch corpus: read the real decision-record index under %q: %w", repoRoot, err)
	}
	idx, err := decisionrecord.ParseIndex(idxRaw)
	if err != nil {
		return nil, fmt.Errorf("agenteval: persona catch corpus: parse the real decision-record index: %w", err)
	}
	var out []PersonaCatchCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := loadPersonaCatchCase(dir, e.Name(), persona, idx)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("agenteval: persona catch corpus dir %q holds zero cases", dir) // (a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if err := checkPersonaPairs(out); err != nil {
		return nil, err
	}
	return out, nil
}

func loadPersonaCatchCase(dir, name, persona string, idx *decisionrecord.Index) (PersonaCatchCase, error) {
	fail := func(format string, args ...any) (PersonaCatchCase, error) {
		return PersonaCatchCase{}, fmt.Errorf("agenteval: persona catch case %q: "+format, append([]any{name}, args...)...)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name, "persona_case.json"))
	if err != nil {
		return fail("read persona_case.json: %w", err) // (b)
	}
	var in PersonaCaseInput
	if err := decodeStrictJSON(raw, &in); err != nil {
		return fail("parse persona_case.json: %w", err) // (b)
	}
	if in.Persona != persona {
		return fail("persona %q is not the %q arm's persona", in.Persona, persona) // (c)
	}
	wantExpect, ok := personaKindExpect[in.Kind]
	if !ok {
		return fail("unknown kind %q", in.Kind) // (d)
	}
	if in.Expect != CatchExpectCatch && in.Expect != CatchExpectClean {
		return fail("unknown expect %q (want %q or %q)", in.Expect, CatchExpectCatch, CatchExpectClean) // (d)
	}
	if in.Expect != wantExpect {
		return fail("kind %q expects %q, got expect %q", in.Kind, wantExpect, in.Expect) // (d)
	}
	if strings.TrimSpace(in.IssueTitle) == "" || strings.TrimSpace(in.IssueBody) == "" {
		return fail("issue_title and issue_body must be non-empty") // (e)
	}
	p, err := plan.Parse(in.Plan)
	if err != nil {
		return fail("plan is not a valid standard_v1 plan: %w", err) // (e)
	}
	shown, err := catchShownText(PlanReviewCatchInput{IssueTitle: in.IssueTitle, IssueBody: in.IssueBody, Plan: in.Plan})
	if err != nil {
		return fail("plan: %w", err)
	}
	if len(in.DefectPhrases) == 0 {
		return fail("defect_phrases must be non-empty — the prompt-leak self-check cannot tell whether a static persona surface shows this case's planted defect") // (f)
	}
	for i, phrase := range in.DefectPhrases {
		lp := strings.ToLower(strings.TrimSpace(phrase))
		if lp == "" {
			return fail("defect_phrases[%d] is empty — an empty phrase occurs in every prompt", i) // (f)
		}
		if !strings.Contains(shown, lp) {
			return fail("defect phrase %q occurs in neither the issue nor the plan: a defect phrase must be this case's own content", phrase) // (f)
		}
	}
	if err := checkPersonaPremises(in.GoverningRecords, idx); err != nil {
		return fail("%w", err) // (g)
	}
	c := PersonaCatchCase{Name: name, Input: in, Plan: p, Raw: raw}
	if in.Expect == CatchExpectClean {
		err = checkPersonaCleanCase(in, p, idx)
	} else {
		err = checkPersonaCatchCase(in, p, shown)
	}
	if err != nil {
		return fail("%w", err)
	}
	return c, nil
}

// checkPersonaPremises is loader mode (g): every governing record is listed in
// the real index with the declared status, superseded_by and amended_by (each
// list compared as a set, an absent amended_by equal to an empty one).
func checkPersonaPremises(recs []PersonaGoverningRecord, idx *decisionrecord.Index) error {
	if len(recs) == 0 {
		return errors.New("governing_records must be non-empty — the case's premise rests on no recorded decision")
	}
	byID := make(map[string]decisionrecord.Record, len(idx.Records))
	for _, r := range idx.Records {
		byID[r.ID] = r
	}
	for _, g := range recs {
		r, ok := byID[g.ID]
		if !ok {
			return fmt.Errorf("governing record %s is absent from the real index: the case's premise does not hold", g.ID)
		}
		if string(r.Status) != g.Status {
			return fmt.Errorf("governing record %s: the case declares status %q, the real index says %q: the case's premise does not hold", g.ID, g.Status, r.Status)
		}
		if !sameStringSet(r.SupersededBy, g.SupersededBy) {
			return fmt.Errorf("governing record %s: the case declares superseded_by %v, the real index says %v: the case's premise does not hold", g.ID, g.SupersededBy, r.SupersededBy)
		}
		if !sameStringSet(r.AmendedBy, g.AmendedBy) {
			return fmt.Errorf("governing record %s: the case declares amended_by %v, the real index says %v: the case's premise does not hold", g.ID, g.AmendedBy, r.AmendedBy)
		}
	}
	return nil
}

func sameStringSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(x, y)
}

// checkPersonaCatchCase is loader modes (h)-(m) for a catch case.
func checkPersonaCatchCase(in PersonaCaseInput, p *plan.Plan, shown string) error {
	if len(in.FalseAlarmExamples) > 0 || len(in.BenignExamples) > 0 || in.PairOf != "" || in.MinMatchedRecords != 0 {
		return errors.New("a catch case must not carry clean-case fields (false_alarm_examples, benign_examples, pair_of, min_matched_records)") // (h)
	}
	if in.Anchor == nil {
		return errors.New("a catch case needs an anchor (approach_step + step, or criterion + criterion_id)") // (i)
	}
	anchorText, others, err := personaAnchorText(*in.Anchor, p)
	if err != nil {
		return err // (i)
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
		if !strings.Contains(strings.ToLower(anchorText), lp) {
			return fmt.Errorf("attribution phrase %q is absent from the anchored %s: it does not name the anchor", phrase, in.Anchor.Kind) // (k)
		}
		for _, o := range others {
			if strings.Contains(strings.ToLower(o.text), lp) {
				return fmt.Errorf("attribution phrase %q also occurs in %s: it would attribute a concern about that to the anchor", phrase, o.where) // (k)
			}
		}
	}
	if len(in.CatchingExamples) == 0 || len(in.NonCatchingExamples) == 0 {
		return errors.New("catching_examples and non_catching_examples must both be non-empty — the probes' discrimination is unproven otherwise") // (l)
	}
	rule := personaCatchRuleFor(in)
	for i, ex := range in.CatchingExamples {
		if _, _, ok := matchCatchConcern(ex.Category, ex.Note, rule); !ok {
			return fmt.Errorf("catching_examples[%d] (%q) is not a catch under catch rule v5: one %s concern must name the anchor AND match a catch probe", i, ex.Note, personaArchitectConcernCategory) // (l)
		}
	}
	attributed := false
	for i, ex := range in.NonCatchingExamples {
		if _, _, ok := matchCatchConcern(ex.Category, ex.Note, rule); ok {
			return fmt.Errorf("non_catching_examples[%d] (%q) is caught under catch rule v5", i, ex.Note) // (l)
		}
		if probe, ok := matchCatchProbe(ex.Category, ex.Note, in.CatchProbes); ok {
			return fmt.Errorf("catch probe %q matches non_catching_examples[%d] (%q): the probe does not discriminate", probe, i, ex.Note) // (l) v4 mode j
		}
		if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); ok {
			attributed = true
		}
	}
	if !attributed {
		return errors.New("no non_catching_example names the anchor: the probes' discrimination on concerns that DO name it is unproven") // (l) v4 mode n
	}
	for _, probe := range in.CatchProbes {
		if strings.Contains(shown, strings.ToLower(strings.TrimSpace(probe))) {
			return fmt.Errorf("catch probe %q occurs in the issue or plan the reviewer is shown: a concern echoing that wording would score as a catch", probe) // (m)
		}
	}
	return nil
}

type personaOtherText struct {
	where, text string
}

// personaAnchorText returns the anchored step's description (or criterion's
// statement) and every OTHER step description and criterion statement.
func personaAnchorText(a PersonaCaseAnchor, p *plan.Plan) (string, []personaOtherText, error) {
	var anchor string
	found := false
	var others []personaOtherText
	switch a.Kind {
	case PersonaAnchorApproachStep:
		if a.Step < 1 || a.CriterionID != "" {
			return "", nil, fmt.Errorf("an approach_step anchor needs step >= 1 and no criterion_id, got step %d criterion_id %q", a.Step, a.CriterionID)
		}
	case PersonaAnchorCriterion:
		if strings.TrimSpace(a.CriterionID) == "" || a.Step != 0 {
			return "", nil, fmt.Errorf("a criterion anchor needs a criterion_id and no step, got criterion_id %q step %d", a.CriterionID, a.Step)
		}
	default:
		return "", nil, fmt.Errorf("unknown anchor kind %q (want %q or %q)", a.Kind, PersonaAnchorApproachStep, PersonaAnchorCriterion)
	}
	for _, s := range p.Approach {
		if a.Kind == PersonaAnchorApproachStep && s.Step == a.Step {
			anchor, found = s.Description, true
			continue
		}
		others = append(others, personaOtherText{where: fmt.Sprintf("approach step %d", s.Step), text: s.Description})
	}
	for _, ac := range p.Verification.AcceptanceCriteria {
		if a.Kind == PersonaAnchorCriterion && ac.ID == a.CriterionID {
			anchor, found = ac.Statement, true
			continue
		}
		others = append(others, personaOtherText{where: fmt.Sprintf("criterion %q", ac.ID), text: ac.Statement})
	}
	if !found {
		if a.Kind == PersonaAnchorApproachStep {
			return "", nil, fmt.Errorf("anchored approach step %d is absent from the plan", a.Step)
		}
		return "", nil, fmt.Errorf("anchored criterion %q is absent from the plan", a.CriterionID)
	}
	return anchor, others, nil
}

// checkPersonaCleanCase is loader modes (h), (n) and (p) for a clean case;
// pair_of's cross-case half (o) runs after every case loads.
func checkPersonaCleanCase(in PersonaCaseInput, p *plan.Plan, idx *decisionrecord.Index) error {
	if in.Anchor != nil || len(in.CatchProbes) > 0 || len(in.AttributionPhrases) > 0 || len(in.CatchingExamples) > 0 || len(in.NonCatchingExamples) > 0 {
		return errors.New("a clean case must not carry catch-case fields (anchor, catch_probes, attribution_phrases, catching_examples, non_catching_examples)") // (h)
	}
	if len(in.FalseAlarmExamples) == 0 || len(in.BenignExamples) == 0 {
		return errors.New("false_alarm_examples and benign_examples must both be non-empty — the clean rule's discrimination is unproven otherwise") // (n)
	}
	rule := personaCatchRuleFor(in)
	for i, ex := range in.FalseAlarmExamples {
		if classifyCatchConcerns(examplesAsConcerns(ex), 1, rule).Outcome != CatchFalseAlarm {
			return fmt.Errorf("false_alarm_examples[%d] (%q) does not classify %s", i, ex.Note, CatchFalseAlarm) // (n)
		}
	}
	for i, ex := range in.BenignExamples {
		if classifyCatchConcerns(examplesAsConcerns(ex), 1, rule).Outcome == CatchFalseAlarm {
			return fmt.Errorf("benign_examples[%d] (%q) classifies %s", i, ex.Note, CatchFalseAlarm) // (n)
		}
	}
	switch in.Kind {
	case PersonaKindDeclaredDirection:
		if strings.TrimSpace(in.PairOf) == "" {
			return errors.New("a declared_direction case needs pair_of naming its undeclared_direction twin") // (o)
		}
		if in.MinMatchedRecords != 0 {
			return errors.New("min_matched_records applies only to a clean_area case") // (p)
		}
	case PersonaKindCleanArea:
		if in.PairOf != "" {
			return errors.New("pair_of applies only to a declared_direction case") // (o)
		}
		if in.MinMatchedRecords < 1 {
			return errors.New("a clean_area case needs min_matched_records >= 1: a clean area no record governs measures nothing about the decision record") // (p)
		}
		matches, err := decisionrecord.Select(idx, personaprompt.ChangePaths(personaScopePaths(p), nil))
		if err != nil {
			return fmt.Errorf("select records for the plan scope: %w", err)
		}
		if len(matches) < in.MinMatchedRecords {
			return fmt.Errorf("the plan scope matches %d index records, fewer than min_matched_records %d: the clean area is not governed as declared", len(matches), in.MinMatchedRecords) // (p)
		}
	}
	return nil
}

func examplesAsConcerns(ex CatchExampleConcern) []CatchTrialConcern {
	return []CatchTrialConcern{{Severity: ex.Severity, Category: ex.Category, Note: ex.Note}}
}

// checkPersonaPairs is loader mode (o)'s cross-case half: each pair_of names a
// loaded undeclared_direction case whose plan equals this declared_direction
// case's plan in every field but new_architectural_decision.
func checkPersonaPairs(cases []PersonaCatchCase) error {
	byName := make(map[string]PersonaCatchCase, len(cases))
	for _, c := range cases {
		byName[c.Name] = c
	}
	for _, c := range cases {
		if c.Input.PairOf == "" {
			continue
		}
		fail := func(format string, args ...any) error {
			return fmt.Errorf("agenteval: persona catch case %q: "+format, append([]any{c.Name}, args...)...)
		}
		twin, ok := byName[c.Input.PairOf]
		if !ok {
			return fail("pair_of %q names no case in the corpus", c.Input.PairOf) // (o)
		}
		if twin.Input.Kind != PersonaKindUndeclaredDirection || twin.Input.Expect != CatchExpectCatch || c.Input.Kind != PersonaKindDeclaredDirection {
			return fail("pair_of %q: a pair must be undeclared_direction (catch) vs declared_direction (clean), got %s (%s) vs %s (%s)",
				c.Input.PairOf, twin.Input.Kind, twin.Input.Expect, c.Input.Kind, c.Input.Expect) // (o)
		}
		same, err := plansEqualExceptDeclaration(c.Input.Plan, twin.Input.Plan)
		if err != nil {
			return fail("pair_of %q: %w", c.Input.PairOf, err)
		}
		if !same {
			return fail("pair_of %q: the two plans differ in a field other than new_architectural_decision — the pair would not isolate the declaration", c.Input.PairOf) // (o)
		}
	}
	return nil
}

// plansEqualExceptDeclaration compares two plan JSON objects with their
// new_architectural_decision keys removed.
func plansEqualExceptDeclaration(a, b json.RawMessage) (bool, error) {
	decode := func(raw json.RawMessage) (map[string]any, error) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
		delete(m, "new_architectural_decision")
		return m, nil
	}
	ma, err := decode(a)
	if err != nil {
		return false, err
	}
	mb, err := decode(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(ma, mb), nil
}

// ClassifyPersonaTrial is ClassifyPersonaTrialForRound with NIL convention
// caps: a round that rendered no review convention, so the convention clamp
// still runs and lowers any convention-category concern to low (#4314).
func ClassifyPersonaTrial(responseText string, rule CatchRule, quoteDocs []prompt.InjectedDocument, severityCap string) CatchTrial {
	return ClassifyPersonaTrialForRound(responseText, rule, quoteDocs, severityCap, nil)
}

// ClassifyPersonaTrialForRound classifies one persona reviewer response the
// way the review loop would store it, in the loop's own ingest order
// (server/trace.go clampImplementReviewVerdict, then
// applyPersonaConcernControls): the production planreview.DecodeVerdict (a
// decode error or an invalid verdict is CatchUndecodable), then the
// convention clamp (planreview.ClampConventionSeveritiesForRound against caps,
// the conventions the round rendered — nil when none did), then
// planreview.ClearPersonaIngestMarkers, then the production persona-ingest
// controls (personaprompt.ApplyConcernControls: quote verification against
// quoteDocs, then the severityCap clamp). Each post-control concern is stored
// with its severity, severity_clamped_from, quote_unverified, document_ref,
// category and note, and the concerns go to classifyCatchConcerns.
func ClassifyPersonaTrialForRound(responseText string, rule CatchRule, quoteDocs []prompt.InjectedDocument, severityCap string, caps planreview.ConventionCaps) CatchTrial {
	v, err := planreview.DecodeVerdict([]byte(responseText))
	if err != nil || !v.Verdict.Valid() {
		return CatchTrial{Outcome: CatchUndecodable, Response: responseText}
	}
	// A persona case never modifies a declared conventions file (the security
	// loader refuses such a diff), so the round's flag is false.
	planreview.ClampConventionSeveritiesForRound(&v, caps, false)
	planreview.ClearPersonaIngestMarkers(&v)
	personaprompt.ApplyConcernControls(&v, quoteDocs, severityCap)
	concerns := make([]CatchTrialConcern, 0, len(v.Concerns))
	for _, c := range v.Concerns {
		concerns = append(concerns, CatchTrialConcern{
			Severity:            string(c.Severity),
			SeverityClampedFrom: string(c.SeverityClampedFrom),
			QuoteUnverified:     c.QuoteUnverified,
			DocumentRef:         c.DocumentRef,
			Category:            c.Category,
			Note:                c.Note,
		})
	}
	return classifyCatchConcerns(concerns, len(v.Concerns), rule)
}

// personaArmRenderer renders one case's persona prompt; a seam so the
// fail-closed render-error branch is testable.
type personaArmRenderer func(c PersonaCatchCase, repoRoot string) (PersonaArmRender, error)

// RunPersonaCatchArm drives render → generate → classify for the architect
// persona arm. Fail closed: no cases, samples < 1, a nil sender, a render
// error or a transport error returns the ZERO report and an error. An
// undecodable response is NOT an error: it is a trial, counted in
// Undecodable (a miss on a catch case, a false alarm on a clean case). PerCase
// carries each case's Expect.
func RunPersonaCatchArm(ctx context.Context, sender MessageSender, cases []PersonaCatchCase, repoRoot string, samples int) (CatchRateArmReport, error) {
	return runPersonaCatchArm(ctx, sender, cases, repoRoot, samples, PersonaArmPrompt)
}

func runPersonaCatchArm(ctx context.Context, sender MessageSender, cases []PersonaCatchCase, repoRoot string, samples int, render personaArmRenderer) (CatchRateArmReport, error) {
	return runPersonaTrialLoop(ctx, sender, ArmArchitect, personaCatchGeneratorSystemPrompt, cases, samples, func(c PersonaCatchCase) (personaTrialCase, error) {
		r, err := render(c, repoRoot)
		if err != nil {
			return personaTrialCase{}, err
		}
		rule := c.CatchRule()
		return personaTrialCase{
			Name:   c.Name,
			Expect: c.Expect(),
			Prompt: r.Prompt,
			Classify: func(responseText string) CatchTrial {
				return ClassifyPersonaTrial(responseText, rule, r.QuoteDocs, personaArchitectSeverityCap)
			},
		}, nil
	})
}

// personaTrialCase is one prepared case of a persona arm's trial loop: the
// rendered prompt sent every sample and the classifier each response goes to.
type personaTrialCase struct {
	Name     string
	Expect   string
	Prompt   string
	Classify func(responseText string) CatchTrial
}

// runPersonaTrialLoop is the per-case render → generate → classify → count
// loop every persona arm shares (the architect arm and the security arms,
// #4314). Fail closed: no cases, samples < 1, a nil sender, a prepare (render)
// error or a transport error returns the ZERO report and an error. Each case
// is prepared just before its samples are sent, so a render error on a later
// case comes after the earlier cases' calls. An undecodable response is NOT an
// error: it is a trial, counted in Undecodable.
func runPersonaTrialLoop[C any](ctx context.Context, sender MessageSender, arm, systemPrompt string, cases []C, samples int, prepare func(C) (personaTrialCase, error)) (CatchRateArmReport, error) {
	if len(cases) == 0 {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: persona arm %q: no cases", arm)
	}
	if samples < 1 {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: persona arm %q: samples must be >= 1, got %d", arm, samples)
	}
	if sender == nil {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: persona arm %q: sender is required", arm)
	}
	report := CatchRateArmReport{
		Arm: arm, SamplesPerCase: samples,
		PerCase: make(map[string]CaseCatchCounts, len(cases)),
		Trials:  make(map[string][]CatchTrial, len(cases)),
	}
	for _, raw := range cases {
		c, err := prepare(raw)
		if err != nil {
			return CatchRateArmReport{}, err
		}
		counts := CaseCatchCounts{Expect: c.Expect}
		trials := make([]CatchTrial, 0, samples)
		for i := 0; i < samples; i++ {
			responseText, _, _, _, _, _, err := sender.Messages(ctx, systemPrompt, c.Prompt)
			if err != nil {
				return CatchRateArmReport{}, fmt.Errorf("agenteval: persona arm %q case %q sample %d: generate: %w", arm, c.Name, i+1, err)
			}
			counts.Trials++
			trial := c.Classify(responseText)
			switch trial.Outcome {
			case CatchCaught:
				counts.Caught++
			case CatchFalseAlarm:
				counts.FalseAlarms++
			case CatchUndecodable:
				counts.Undecodable++
			}
			trials = append(trials, trial)
			observeCatchTrial(ctx, arm, c.Name, c.Expect, i+1, responseText, trial)
		}
		report.PerCase[c.Name] = counts
		report.Trials[c.Name] = trials
	}
	return report, nil
}

// SamplesPerCaseForPersona returns the per-case sample count that clears the
// power floor in EACH present expectation class: the max over the classes
// with at least one case of ceil(MinCatchRateTrialsPerArm / n). It returns 0
// when both classes are empty (or a count is negative), which
// RunPersonaCatchArm refuses.
func SamplesPerCaseForPersona(nCatch, nClean int) int {
	if nCatch < 0 || nClean < 0 {
		return 0
	}
	return max(SamplesPerCaseForPower(nCatch), SamplesPerCaseForPower(nClean))
}
