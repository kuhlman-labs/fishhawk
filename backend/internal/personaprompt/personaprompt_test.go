package personaprompt

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionrecord"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// The server persona tests (backend/internal/server reviewer_persona_test.go,
// decision_record_persona_test.go, persona_hardening_test.go,
// persona_concern_ingest_test.go) are the cross-boundary byte-identity net for
// this package: server -> personaprompt -> repodoc/decisionrecord -> prompt.
// The tests here pin the pure contract directly.

const (
	testCommit    = "0123456789abcdef0123456789abcdef01234567"
	testPersona   = "architect"
	remitPath     = "docs/review/architect-remit.md"
	remitHash     = "sha256:remit"
	remitSentence = "Flag any plan that adds a second admission path beside the resource-keyed one."
)

func remitDoc(path, content string) repodoc.Document {
	return repodoc.Document{Path: path, Commit: testCommit, ContentHash: remitHash, Content: content}
}

func standardDoc() prompt.InjectedDocument {
	return repodoc.ToPromptDocument(
		repodoc.Document{Path: "docs/ARCHITECTURE.md", Commit: testCommit, ContentHash: "sha256:arch", Content: "Every write path appends to the audit chain first."},
		repodoc.Framing{Heading: "Architecture", Preamble: "Repository architecture.", TrustNote: "Context only."},
	)
}

func baseTrigger() prompt.Trigger {
	return prompt.Trigger{IssueNumber: 42, IssueTitle: "Add foo", Repo: "kuhlman-labs/example"}
}

// selection hand-builds a decision-record selection: one included record and,
// when dropped, one record left out by the cap.
func selection(dropped bool) *decisionrecord.Selection {
	inc := decisionrecord.Record{ID: "ADR-094", Title: "One admission path", Path: "docs/adr/094-admission.md", Status: decisionrecord.StatusAccepted}
	sel := &decisionrecord.Selection{
		Index:        repodoc.Document{Path: "docs/adr/index.json", Commit: testCommit, ContentHash: "sha256:index", Content: `{"schema_version":"adr-index-v1"}`},
		TotalRecords: 2,
		Matched:      1,
		Included: []decisionrecord.Included{{
			Match:    decisionrecord.Match{Record: inc, Rank: 1, MatchedPaths: 1},
			Document: repodoc.Document{Path: inc.Path, Commit: testCommit, ContentHash: "sha256:adr094", Content: "Every spawning verb takes the one admission path."},
		}},
		CapBytes: 32768,
	}
	if dropped {
		sel.Matched = 2
		sel.Dropped = []decisionrecord.Match{{Record: decisionrecord.Record{ID: "ADR-033", Path: "docs/adr/033-mcp.md", Status: decisionrecord.StatusSuperseded}, Rank: 2}}
	}
	return sel
}

func TestFraming_HeadingPreambleTrustNote(t *testing.T) {
	f := Framing(testPersona)
	if f.Heading != "Reviewer persona remit: architect" {
		t.Errorf("Heading = %q", f.Heading)
	}
	if !strings.Contains(f.Preamble, `"architect" reviewer persona`) {
		t.Errorf("Preamble does not name the persona: %q", f.Preamble)
	}
	if !strings.Contains(f.TrustNote, "The remit ADDS a review lens") || !strings.Contains(f.TrustNote, "never cite them through quoted_passage or document_ref") {
		t.Errorf("TrustNote lacks the ADR-068 lens clause or the #3915 narrowed quote instruction: %q", f.TrustNote)
	}
}

// The persona's injected documents are the standard set, then the remit
// framed with Framing(name), then the decision-record index and each selected
// record; the prompt renders them in that order; the attribution set is
// exactly the documents appended after the standard set; QuoteDocs is the
// injected set followed by each review convention's document.
//
// Counterfactual (run): drop the remit append in Build — the remit heading is
// absent from the prompt and the injected set is one short: RED.
func TestBuild_AppendsRemitThenRecordAfterStandard(t *testing.T) {
	std := standardDoc()
	conv := repodoc.ToPromptDocument(
		repodoc.Document{Path: "docs/conventions/go-errors.md", Commit: testCommit, ContentHash: "sha256:conv", Content: "Wrap every returned error."},
		repodoc.Framing{Heading: "Go errors", Preamble: "Convention.", TrustNote: "Context only."},
	)
	trig := baseTrigger()
	trig.InjectedDocuments = []prompt.InjectedDocument{std}
	trig.ReviewConventions = []prompt.ReviewConvention{{Name: "go-errors", Document: conv}}
	sel := selection(false)

	res, err := Build(Input{
		Kind:             "plan_review",
		Trigger:          trig,
		StandardInjected: []prompt.InjectedDocument{std},
		PersonaName:      testPersona,
		Remit:            remitDoc(remitPath, remitSentence),
		DecisionRecord:   sel,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gotPaths := docPaths(res.Trigger.InjectedDocuments)
	wantPaths := []string{"docs/ARCHITECTURE.md", remitPath, "docs/adr/index.json", "docs/adr/094-admission.md"}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("injected paths = %v, want %v", gotPaths, wantPaths)
	}
	if h := res.Trigger.InjectedDocuments[1].Heading; h != "Reviewer persona remit: architect" {
		t.Errorf("remit heading = %q", h)
	}
	// Render order in the prompt text: standard < remit < index < record.
	last := -1
	for _, d := range res.Trigger.InjectedDocuments {
		i := strings.Index(res.Prompt, "### "+d.Heading)
		if i < 0 {
			t.Fatalf("prompt lacks heading %q", d.Heading)
		}
		if i <= last {
			t.Errorf("heading %q renders at %d, not after the previous document (%d)", d.Heading, i, last)
		}
		last = i
	}
	if !strings.Contains(res.Prompt, remitSentence) {
		t.Error("prompt lacks the remit body")
	}
	// Attribution == the documents appended after the standard set.
	if got, want := attributionPaths(res.Attribution), wantPaths[1:]; !slices.Equal(got, want) {
		t.Errorf("attribution paths = %v, want %v (the injected set past the standard documents)", got, want)
	}
	if len(res.Attribution.Selections) != 0 {
		t.Errorf("attribution selections = %+v, want none (nothing dropped)", res.Attribution.Selections)
	}
	if got, want := docPaths(res.QuoteDocs), append(slices.Clone(wantPaths), "docs/conventions/go-errors.md"); !slices.Equal(got, want) {
		t.Errorf("QuoteDocs paths = %v, want %v", got, want)
	}
}

// A persona without a decision record attributes the remit alone (the
// pre-#3756 single-document set), and a selection that dropped a match carries
// its selection-level truncation into the ONE attribution set.
func TestBuild_AttributionSet(t *testing.T) {
	res, err := Build(Input{Kind: "plan_review", Trigger: baseTrigger(), PersonaName: testPersona, Remit: remitDoc(remitPath, remitSentence)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := attributionPaths(res.Attribution); !slices.Equal(got, []string{remitPath}) || res.Attribution.Selections != nil {
		t.Errorf("remit-only attribution = %v / %+v, want [%s] and no selections", got, res.Attribution.Selections, remitPath)
	}

	res, err = Build(Input{Kind: "plan_review", Trigger: baseTrigger(), PersonaName: testPersona, Remit: remitDoc(remitPath, remitSentence), DecisionRecord: selection(true)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(res.Attribution.Selections) != 1 || len(res.Attribution.Selections[0].Dropped) != 1 || res.Attribution.Selections[0].Dropped[0].ID != "ADR-033" {
		t.Errorf("attribution selections = %+v, want one truncation dropping ADR-033", res.Attribution.Selections)
	}
	if !strings.Contains(res.Prompt, "NOT INCLUDED because of the injection cap") {
		t.Error("index framing does not name the dropped record")
	}
}

// Grounding: tree access needs a grounded round AND a reviewer that can
// ground. A grounded round whose persona reviewer cannot ground clears the
// tree fields and is stamped reviewer_cannot_ground (#4066); an ungrounded
// round's reason carries over; the index framing follows TreeAccess.
//
// Counterfactuals (run): (A) drop the `if in.RoundGrounded` stamp — the
// grounded/cannot-ground row keeps the zero reason: RED; (B) drop the
// ReviewTreeCommit clear — that row still claims the tree: RED.
func TestBuild_UngroundedClause(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		roundGrounded, canGrd bool
		inReason              string
		wantAccess            bool
		wantCommit, wantReas  string
		wantIndexPhrase       string
	}{
		{name: "grounded round, grounding reviewer", roundGrounded: true, canGrd: true, wantAccess: true, wantCommit: testCommit, wantIndexPhrase: "you may read one from the review tree"},
		{name: "grounded round, reviewer cannot ground", roundGrounded: true, canGrd: false, wantReas: prompt.ReviewUngroundedReviewerCannotGround, wantIndexPhrase: "you cannot read it in this review"},
		{name: "ungrounded round keeps its reason", roundGrounded: false, canGrd: true, inReason: prompt.ReviewUngroundedNoRef, wantReas: prompt.ReviewUngroundedNoRef, wantIndexPhrase: "you cannot read it in this review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trig := baseTrigger()
			trig.ReviewUngroundedReason = tc.inReason
			if tc.roundGrounded {
				trig.ReviewTreeCommit = testCommit
				trig.ReviewTreeSkippedSymlinks = 2
				trig.ReviewTreeSkippedInstructions = 3
			}
			res, err := Build(Input{
				Kind: "plan_review", Trigger: trig, PersonaName: testPersona, Remit: remitDoc(remitPath, remitSentence),
				DecisionRecord: selection(false), RoundGrounded: tc.roundGrounded, ReviewerCanGround: tc.canGrd,
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if res.TreeAccess != tc.wantAccess {
				t.Errorf("TreeAccess = %v, want %v", res.TreeAccess, tc.wantAccess)
			}
			if res.Trigger.ReviewTreeCommit != tc.wantCommit || res.Trigger.ReviewUngroundedReason != tc.wantReas {
				t.Errorf("tree commit / reason = %q / %q, want %q / %q", res.Trigger.ReviewTreeCommit, res.Trigger.ReviewUngroundedReason, tc.wantCommit, tc.wantReas)
			}
			if !tc.wantAccess && (res.Trigger.ReviewTreeSkippedSymlinks != 0 || res.Trigger.ReviewTreeSkippedInstructions != 0) {
				t.Errorf("skipped counts = %d/%d, want 0/0 without tree access", res.Trigger.ReviewTreeSkippedSymlinks, res.Trigger.ReviewTreeSkippedInstructions)
			}
			if !strings.Contains(res.Prompt, tc.wantIndexPhrase) {
				t.Errorf("index framing lacks %q", tc.wantIndexPhrase)
			}
			if tc.roundGrounded && trig.ReviewTreeCommit != testCommit {
				t.Error("Build wrote through to the caller's Trigger")
			}
		})
	}
}

// The standard injected set is CLONED, never aliased: with spare capacity
// (len 1, cap 4) a plain append would write the remit into the caller's
// backing array, so a second persona build overwrites the first result's
// remit slot.
//
// Counterfactual (run): replace slices.Clone(in.StandardInjected) with
// in.StandardInjected — result A's remit slot reads remit B: RED.
func TestBuild_DoesNotAliasStandardInjected(t *testing.T) {
	standard := make([]prompt.InjectedDocument, 1, 4)
	standard[0] = standardDoc()
	trig := baseTrigger()
	trig.InjectedDocuments = standard
	build := func(path string) Result {
		t.Helper()
		res, err := Build(Input{Kind: "plan_review", Trigger: trig, StandardInjected: standard, PersonaName: testPersona, Remit: remitDoc(path, remitSentence)})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return res
	}
	a := build("docs/review/a.md")
	b := build("docs/review/b.md")
	if got := a.Trigger.InjectedDocuments[1].Path; got != "docs/review/a.md" {
		t.Errorf("result A remit path = %q after a second build, want docs/review/a.md (aliased standard slice)", got)
	}
	if got := b.Trigger.InjectedDocuments[1].Path; got != "docs/review/b.md" {
		t.Errorf("result B remit path = %q", got)
	}
	if spare := standard[:2][1]; spare != (prompt.InjectedDocument{}) {
		t.Errorf("caller's spare capacity was written: %+v", spare)
	}
	if len(trig.InjectedDocuments) != 1 {
		t.Errorf("caller Trigger InjectedDocuments len = %d, want 1", len(trig.InjectedDocuments))
	}
}

// A prompt.Build error is returned with a ZERO Result, so the caller has no
// prompt and nothing to attribute.
func TestBuild_PromptBuildErrorReturnsZeroResult(t *testing.T) {
	res, err := Build(Input{Kind: "not_a_stage", Trigger: baseTrigger(), PersonaName: testPersona, Remit: remitDoc(remitPath, remitSentence)})
	if !errors.Is(err, prompt.ErrUnsupportedStage) {
		t.Fatalf("err = %v, want prompt.ErrUnsupportedStage", err)
	}
	if !reflect.DeepEqual(res, Result{}) {
		t.Errorf("Result = %+v, want zero", res)
	}
}

func TestQuotedDocuments_TextIsInjectedContent(t *testing.T) {
	remit := repodoc.ToPromptDocument(remitDoc(remitPath, remitSentence), Framing(testPersona))
	withheld := prompt.InjectedDocument{Heading: "Withheld", Body: "This document was withheld.", Path: "docs/withheld.md", Commit: testCommit, ContentHash: "sha256:w"}
	got := QuotedDocuments([]prompt.InjectedDocument{remit, withheld})
	want := []planreview.QuotedDocument{
		{Path: remitPath, Commit: testCommit, ContentHash: remitHash, Text: remitSentence},
		{Path: "docs/withheld.md", Commit: testCommit, ContentHash: "sha256:w", Text: ""},
	}
	if len(got) != 2 || got[1] != want[1] || got[0].Path != want[0].Path || got[0].ContentHash != want[0].ContentHash || strings.TrimSpace(got[0].Text) != remitSentence {
		t.Errorf("QuotedDocuments = %+v, want %+v", got, want)
	}
	if strings.Contains(got[0].Text, "Reviewer persona remit") {
		t.Error("quoted text carries the server framing, not only the delimited document")
	}
}

func TestChangePaths_UnionDedupSorted(t *testing.T) {
	got := ChangePaths([]string{"b.go", "a.go", "b.go"}, []string{"c.go", "a.go"})
	if want := []string{"a.go", "b.go", "c.go"}; !slices.Equal(got, want) {
		t.Errorf("ChangePaths = %v, want %v", got, want)
	}
	if got := ChangePaths(nil, nil); got != nil {
		t.Errorf("empty ChangePaths = %v, want nil", got)
	}
	if got := ChangePaths(nil, []string{"d.go"}); !slices.Equal(got, []string{"d.go"}) {
		t.Errorf("diff-only ChangePaths = %v, want [d.go]", got)
	}
}

// randomToken is a fresh token absent from every injected document.
func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "token-" + hex.EncodeToString(b)
}

func remitQuoteDocs() []prompt.InjectedDocument {
	return []prompt.InjectedDocument{repodoc.ToPromptDocument(remitDoc(remitPath, remitSentence), Framing(testPersona))}
}

// Counterfactual (run): remove the VerifyQuotedPassages call — the concern
// stays medium with quote_unverified false: RED.
func TestApplyConcernControls_DemotesUnverifiedQuote(t *testing.T) {
	v := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{{
		Severity: planreview.SeverityMedium, Category: "decision_record", Note: "n", QuotedPassage: randomToken(t), DocumentRef: remitPath,
	}}}
	ctl := ApplyConcernControls(v, remitQuoteDocs(), "")
	c := v.Concerns[0]
	if c.Severity != planreview.SeverityLow || !c.QuoteUnverified || c.SeverityClampedFrom != planreview.SeverityMedium {
		t.Errorf("concern = %+v, want low, quote_unverified, clamped from medium", c)
	}
	if len(ctl.Quote.Demoted) != 1 || ctl.Quote.Demoted[0].Failure != planreview.QuoteFailurePassageNotFound {
		t.Errorf("Quote.Demoted = %+v, want one passage_not_found", ctl.Quote.Demoted)
	}
}

// The quote is copied verbatim from the remit, so verification PASSES and
// cannot mask the clamp.
//
// Counterfactual (run): remove the ClampPersonaSeverities call — the concern
// stays high: RED.
func TestApplyConcernControls_ClampsToCap(t *testing.T) {
	v := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{{
		Severity: planreview.SeverityHigh, Category: "decision_record", Note: "n", QuotedPassage: remitSentence, DocumentRef: remitPath,
	}}}
	ctl := ApplyConcernControls(v, remitQuoteDocs(), "medium")
	c := v.Concerns[0]
	if c.QuoteUnverified || c.QuoteVerifiedContentHash != remitHash {
		t.Fatalf("quote did not verify (fixture defect): %+v", c)
	}
	if c.Severity != planreview.SeverityMedium || c.PersonaSeverityCap != planreview.SeverityMedium || c.SeverityClampedFrom != planreview.SeverityHigh {
		t.Errorf("concern = %+v, want medium, persona_severity_cap medium, clamped from high", c)
	}
	if len(ctl.Clamp.Clamped) != 1 {
		t.Errorf("Clamp.Clamped = %+v, want one", ctl.Clamp.Clamped)
	}
}

// Order is load-bearing: an unverified high under a medium cap is demoted to
// low by quote verification FIRST, so the cap records no clamp.
//
// Counterfactual (run): swap the two calls — the cap clamps the high to
// medium first and Clamp.Clamped has one entry: RED.
func TestApplyConcernControls_QuoteBeforeCap(t *testing.T) {
	v := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{{
		Severity: planreview.SeverityHigh, Category: "decision_record", Note: "n", QuotedPassage: randomToken(t), DocumentRef: remitPath,
	}}}
	ctl := ApplyConcernControls(v, remitQuoteDocs(), "medium")
	if c := v.Concerns[0]; c.Severity != planreview.SeverityLow || c.SeverityClampedFrom != planreview.SeverityHigh {
		t.Errorf("concern = %+v, want low clamped from high", c)
	}
	if len(ctl.Quote.Demoted) != 1 || len(ctl.Clamp.Clamped) != 0 {
		t.Errorf("Demoted/Clamped = %d/%d, want 1/0 (quote verification runs before the cap)", len(ctl.Quote.Demoted), len(ctl.Clamp.Clamped))
	}
}

// VerdictClampedFrom is the first non-empty of the quote and clamp results.
//
// Counterfactual (run): drop the fallback to Clamp.VerdictClampedFrom — the
// "cap downgrades" row returns "": RED.
func TestApplyConcernControls_VerdictClampedFromMerged(t *testing.T) {
	for _, tc := range []struct {
		name, quote, capSev string
		want                planreview.Verdict
	}{
		{name: "quote demotion downgrades", quote: "unverified", want: planreview.VerdictReject},
		{name: "cap downgrades", quote: remitSentence, capSev: "medium", want: planreview.VerdictReject},
		{name: "nothing lowered", quote: remitSentence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote := tc.quote
			if quote == "unverified" {
				quote = randomToken(t)
			}
			v := &planreview.ReviewVerdict{Verdict: planreview.VerdictReject, Concerns: []planreview.Concern{{
				Severity: planreview.SeverityHigh, Category: "decision_record", Note: "n", QuotedPassage: quote, DocumentRef: remitPath,
			}}}
			ctl := ApplyConcernControls(v, remitQuoteDocs(), tc.capSev)
			if ctl.VerdictClampedFrom != tc.want {
				t.Errorf("VerdictClampedFrom = %q, want %q", ctl.VerdictClampedFrom, tc.want)
			}
			wantVerdict := planreview.VerdictReject
			if tc.want != "" {
				wantVerdict = planreview.VerdictApproveWithConcerns
			}
			if v.Verdict != wantVerdict {
				t.Errorf("verdict = %s, want %s", v.Verdict, wantVerdict)
			}
		})
	}
}

func TestApplyConcernControls_NilVerdict(t *testing.T) {
	if ctl := ApplyConcernControls(nil, remitQuoteDocs(), "medium"); !reflect.DeepEqual(ctl, ConcernControls{}) {
		t.Errorf("nil verdict controls = %+v, want zero", ctl)
	}
}

func docPaths(docs []prompt.InjectedDocument) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Path)
	}
	return out
}

func attributionPaths(set repodoc.InjectionSet) []string {
	out := make([]string, 0, len(set.Documents))
	for _, d := range set.Documents {
		out = append(out, d.Path)
	}
	return out
}
