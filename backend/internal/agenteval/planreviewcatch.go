// Plan-review catch-rate harness (E55.4 / #2245): does attaching the E55.3 /
// #2244 repository review-conventions section DILUTE the plan reviewer's
// catch rate on the plan-review-miss corpus?
//
// Each corpus case (testdata/planreview-miss-corpus/<case>/) carries, beside
// its miss.json, a hand-curated review_input.json: the issue text, a COMPLETE
// standard_v1 plan whose acceptance criteria contain the missed criterion, and
// the catch probes that decide whether a reviewer verdict caught it. Every case
// renders through the REAL prompt.Build("plan_review") twice — without
// conventions and with one representative conventions file rendered through
// the REAL repodoc path — so the two arms differ ONLY by the conventions
// section (TestCatchRateArms_DifferOnlyInConventionsSection pins that).
//
// A trial is CAUGHT only when ONE emitted concern satisfies BOTH halves of the
// rule (matchCatchConcern): the ATTRIBUTION half — the concern's note or
// category names a planted criterion id from the case's miss.json, or one of
// the case's declared attribution_phrases (each occurring in the planted
// criterion's statement and in no other criterion's, loader mode p) — AND the
// PROBE half (matchCatchProbe: some catch probe as a case-folded substring of
// the same note or category). A probe never occurs in the issue or plan the
// reviewer is shown (loader mode m), so a concern that merely ECHOES that
// wording scores missed; the attribution half additionally keeps a probe that
// is the plan-review prompt's OWN vocabulary (vague, untestable, ...) from
// scoring a concern about a DIFFERENT criterion as a catch. Neither half alone
// catches, and the two must meet in one concern. An UNDECODABLE verdict counts
// as a MISS in the rate and is reported separately — excluding it from the
// denominator would let an arm that stops emitting parseable verdicts look
// better, not worse.
//
// RunCatchRateArm also keeps one CatchTrial per sample (the decoded concerns,
// the matched probe and attribution of a catch, a near-miss tag per concern of
// a miss, the raw response of an undecodable trial) so the recorder can commit
// the per-trial sidecar beside the evidence (planreviewevidence.go).
//
// The regression rule (CompareCatchRateArms): the with-conventions catch rate
// may be at most DefaultCatchRateRegressionTolerance below the
// without-conventions rate (strictly greater FAILS), and a measurement with
// fewer than MinCatchRateTrialsPerArm trials in EITHER arm is REFUSED, never
// passed. The floor is DERIVED from the tolerance (minTrialsForTolerance), so
// an under-powered measurement is answered by raising the samples, not by
// widening the bar.
//
// READ THE FLOOR HONESTLY: the trial floor bounds MODEL-SAMPLING noise only.
// The corpus — the committed synthetic planted-defect shapes — bounds what the
// gate can detect; a dilution that spares every committed shape is invisible
// to it.
//
// The live measurement and the committed evidence record are NOT in this file
// (planreviewevidence.go + the double-gated live test own them); everything
// here is offline and tested with fake senders.

package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// The two catch-rate arms.
const (
	// ArmWithoutConventions renders the plan-review prompt with
	// Trigger.ReviewConventions nil — no conventions section at all.
	ArmWithoutConventions = "without_conventions"
	// ArmWithConventions renders the same prompt with the representative
	// conventions file attached as the only selected convention.
	ArmWithConventions = "with_conventions"
)

// CatchOutcome is the three-state classification of one reviewer response.
type CatchOutcome string

const (
	// CatchCaught means the verdict decoded and one concern satisfied both the
	// attribution half and the probe half of the rule.
	CatchCaught CatchOutcome = "caught"
	// CatchMissed means the verdict decoded and no single concern satisfied
	// both halves.
	CatchMissed CatchOutcome = "missed"
	// CatchUndecodable means the response carried no decodable verdict. Scored
	// as a MISS; reported separately.
	CatchUndecodable CatchOutcome = "undecodable"
)

const (
	// DefaultCatchRateRegressionTolerance is the largest drop of the
	// with-conventions catch rate below the without-conventions rate that
	// still PASSES. A JUDGEMENT CALL, stated as one: ten percentage points
	// is the smallest dilution this corpus can be asked to resolve at an
	// affordable trial count (see MinCatchRateTrialsPerArm).
	DefaultCatchRateRegressionTolerance = 0.10

	// catchRateOneSidedZ is the standard normal 0.95 quantile (one-sided
	// 95%; NIST/SEMATECH e-Handbook of Statistical Methods §1.3.6.7.1). The
	// 95% level is the second judgement call.
	catchRateOneSidedZ = 1.645

	// MinCatchRateTrialsPerArm is the power floor: the smallest per-arm trial
	// count n at which the one-sided 95% worst-case noise bound on the
	// DIFFERENCE of two arm rates, z*sqrt(0.5/n) (binomial variance
	// p(1-p) <= 0.25 per arm, two arms, normal approximation), is at most
	// the tolerance — n >= 0.5*(z/tolerance)^2 = 135.3, so 136. Derived,
	// not chosen: TestMinCatchRateTrialsPerArm_DerivedFromTolerance fails if
	// the constant and the tolerance drift apart. It bounds MODEL-SAMPLING
	// noise only; the corpus size bounds what the gate can detect.
	MinCatchRateTrialsPerArm = 136

	// catchRuleVersion names the catch-classification semantics. Bump it
	// whenever ClassifyCatch, matchCatchConcern, matchCatchAttribution or
	// matchCatchProbe changes meaning: it is folded into the evidence
	// fingerprint, so a bump makes a recorded measurement stale instead of
	// silently re-scoring it under new rules.
	catchRuleVersion = "planreview-catch-v4: a trial is caught when the response decodes (extract first '{'..last '}', " +
		"verdict one of approve|approve_with_concerns|reject) and ONE concern satisfies both halves: its note OR category " +
		"contains a planted criterion id (miss.json criterion_id) or a declared attribution phrase, AND its note OR " +
		"category contains some catch probe, each as a case-folded (strings.ToLower) substring; an undecodable response " +
		"is a miss counted in trials; no probe occurs case-folded in the case's issue title, issue body or plan text " +
		"(loader mode m); each attribution phrase occurs case-folded in a planted criterion's statement and in no other " +
		"criterion's statement (loader mode p); no planted id is a case-folded substring of another criterion id (loader mode q); " +
		"each case declares at least one non-blank defect phrase occurring case-folded in its issue or plan text (loader mode r); " +
		"v4 widens the per-case probes with the plan-review prompt's own vocabulary (#4355)"
)

// The two attribution kinds a caught trial records: which half of the
// attribution rule named the planted criterion.
const (
	// CatchAttributionID means the matching concern named a planted
	// criterion id. It is checked first, so a concern naming both the id and
	// a phrase counts as id-attributed.
	CatchAttributionID = "id"
	// CatchAttributionPhrase means the matching concern named no planted id
	// but carried one of the case's declared attribution_phrases.
	CatchAttributionPhrase = "phrase"
)

// The near-miss tags a missed trial's concerns carry.
const (
	// NearMissProbeWithoutAttribution tags a concern that matched a catch
	// probe but named neither a planted criterion id nor an attribution
	// phrase — a reviewer that may have flagged the defect without saying
	// WHICH criterion it meant.
	NearMissProbeWithoutAttribution = "probe_without_attribution"
	// NearMissAttributionWithoutProbe tags a concern that named the planted
	// criterion (by id or phrase) but matched no catch probe.
	NearMissAttributionWithoutProbe = "attribution_without_probe"
)

// representativeConventionName / representativeConventionPath /
// representativeConventionSeverityCap are the declaration the representative
// conventions fixture is rendered under. They are fixed literals — never the
// on-disk fixture path — so the rendered with-arm bytes do not depend on
// where the checkout lives.
const (
	representativeConventionName        = "repository-review-conventions"
	representativeConventionPath        = "docs/review-conventions.md"
	representativeConventionSeverityCap = "medium"
)

// planReviewCatchRepo is the repository the synthetic cases' plans belong to.
// It renders in the reviewer's role line.
const planReviewCatchRepo = "example-org/widgets-service"

// planReviewCatchGeneratorSystemPrompt is the fixed harness instruction both
// arms send to the generator, naming the plan-review verdict JSON contract so
// the emitted verdict is comparable across arms.
const planReviewCatchGeneratorSystemPrompt = `You are the Fishhawk plan-review agent. Read the stage prompt the user message contains and produce the review verdict it asks for as a JSON object carrying verdict, concerns (each with severity, category and note), and optional free_form. Respond with the verdict JSON only.`

// CatchExampleConcern is one hand-written reviewer concern in a case's
// catching_examples / non_catching_examples. The loader classifies each one
// through the SAME matcher the measurement uses, so a probe that cannot
// discriminate is refused at load time rather than discovered as noise.
type CatchExampleConcern struct {
	Severity string `json:"severity,omitempty"`
	Category string `json:"category"`
	Note     string `json:"note"`
}

// PlanReviewCatchInput is the review_input.json shape of one catch-rate case.
type PlanReviewCatchInput struct {
	IssueTitle string `json:"issue_title"`
	IssueBody  string `json:"issue_body"`
	// Plan is a COMPLETE standard_v1 plan object (validated by plan.Parse)
	// whose verification.acceptance_criteria carries the miss criterion.
	Plan json.RawMessage `json:"plan"`
	// CatchProbes are case-folded substrings: the PROBE half of the rule. A
	// concern whose note or category contains one of them AND names the
	// planted criterion (AttributionPhrases or a miss criterion id) catches
	// the planted defect.
	CatchProbes []string `json:"catch_probes"`
	// AttributionPhrases are case-folded substrings, each occurring in a
	// planted criterion's statement and in no other criterion's statement
	// (loader mode p): the id-free way a concern can name the planted
	// criterion for the ATTRIBUTION half of the rule.
	AttributionPhrases []string `json:"attribution_phrases"`
	// DefectPhrases are the case-SPECIFIC content of the planted defect (its
	// domain nouns, values and wording), each occurring case-folded in the
	// case's own issue or plan (loader mode r). They are never matched against
	// a concern: the prompt-leak self-check fails when one occurs in the
	// static plan-review prompt, which would show every reviewer the planted
	// answer (#4355). Class-level vocabulary the prompt legitimately teaches
	// ("was written", "robust", "vague") is not a defect phrase.
	DefectPhrases       []string              `json:"defect_phrases"`
	CatchingExamples    []CatchExampleConcern `json:"catching_examples"`
	NonCatchingExamples []CatchExampleConcern `json:"non_catching_examples"`
}

// PlanReviewCatchCase is one fully-loaded catch-rate case.
type PlanReviewCatchCase struct {
	// Name is the corpus directory name — the per-case key everywhere.
	Name string
	// Miss is the case's miss.json.
	Miss PlanReviewMissCase
	// Input is the decoded review_input.json.
	Input PlanReviewCatchInput
	// Plan is Input.Plan parsed by the real plan.Parse.
	Plan *plan.Plan
	// ReviewInputRaw is the exact committed review_input.json bytes (an
	// evidence-fingerprint input).
	ReviewInputRaw []byte
}

// CatchRule is the per-case input to the catch classifier: the probe half
// (Probes) and the attribution half (CriterionIDs, AttributionPhrases).
type CatchRule struct {
	Probes             []string
	CriterionIDs       []string
	AttributionPhrases []string
}

// CatchRule returns the case's catch rule: its probes, its planted criterion
// ids (every miss.json criterion_id) and its declared attribution phrases.
func (c PlanReviewCatchCase) CatchRule() CatchRule {
	return catchRuleFor(c.Miss, c.Input)
}

func catchRuleFor(m PlanReviewMissCase, in PlanReviewCatchInput) CatchRule {
	ids := make([]string, 0, len(m.Misses))
	for _, miss := range m.Misses {
		ids = append(ids, miss.CriterionID)
	}
	return CatchRule{Probes: in.CatchProbes, CriterionIDs: ids, AttributionPhrases: in.AttributionPhrases}
}

// LoadPlanReviewCatchCorpus loads every case under dir. It reuses
// LoadPlanReviewMissCorpus for miss.json and then reads each case's
// review_input.json, FAILING CLOSED with an error naming the case on:
//
//	(a) an absent corpus dir — unlike LoadPlanReviewMissCorpus: a missing
//	    corpus means the measurement silently is not running
//	(b) zero cases
//	(c) a case with no review_input.json
//	(d) malformed review_input.json or an unknown field in it
//	(e) an empty issue_title or issue_body
//	(f) a plan plan.Parse rejects
//	(g) a plan whose acceptance_criteria lack a miss criterion_id — the
//	    planted defect must be IN the reviewed plan (anti-vacuity)
//	(h) a plan criterion statement differing from the miss statement
//	(i) empty catch_probes, or an empty/whitespace probe (which would match
//	    every concern)
//	(j) a probe matching any non_catching_example — checked with
//	    matchCatchProbe, the runtime probe half itself, regardless of
//	    attribution, so the probe set discriminates on its own
//	(k) a catching_example that does not classify CAUGHT under the full rule
//	    (matchCatchConcern): it names neither a planted criterion id nor an
//	    attribution phrase, or it matches no probe
//	(l) empty catching_examples or non_catching_examples
//	(m) a probe occurring (case-folded) in the issue title, the issue body or
//	    any key or value of the plan — text the reviewer is SHOWN, so a
//	    concern echoing it about an unrelated aspect would score as a catch
//	(n) no non_catching_example carrying the attribution half (a planted id
//	    or an attribution phrase) — without one, (j) passes on concerns the
//	    attribution half already rejects and proves nothing about the probes
//	    on concerns that DO name the planted criterion
//	(o) empty attribution_phrases, or an empty/whitespace phrase (which would
//	    attribute every concern)
//	(p) an attribution phrase absent (case-folded) from every planted
//	    criterion's statement, or occurring in a non-planted criterion's
//	    statement — it would attribute a concern about another criterion
//	(q) a planted criterion id occurring (case-folded) inside another
//	    criterion id of the plan — a concern naming that other id would carry
//	    the planted id as a substring and be attributed to it
//	(r) empty defect_phrases, an empty/whitespace defect phrase, or a defect
//	    phrase absent (case-folded) from the issue title, the issue body and
//	    every key and value of the plan — a declared phrase must really be
//	    this case's content, or the prompt-leak self-check that reads it
//	    proves nothing (#4355). It applies to every case, including cases
//	    added after it (#4313)
//
// Mode (d) includes trailing content after the JSON value: a second object
// or garbage appended to a valid review_input.json is refused, not ignored.
//
// A miss.json failure (malformed, no misses, empty criterion id) surfaces
// through LoadPlanReviewMissCorpus with its own case-naming error. Synthetic
// is NOT required: a curated production case (synthetic:false) is legal.
func LoadPlanReviewCatchCorpus(dir string) ([]PlanReviewCatchCase, error) {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("agenteval: plan-review catch corpus dir %q is absent: the catch-rate measurement cannot run without it", dir) // (a)
		}
		return nil, fmt.Errorf("agenteval: plan-review catch corpus dir %q: %w", dir, err)
	}
	misses, err := LoadPlanReviewMissCorpus(dir)
	if err != nil {
		return nil, err
	}
	if len(misses) == 0 {
		return nil, fmt.Errorf("agenteval: plan-review catch corpus dir %q holds zero cases", dir) // (b)
	}
	out := make([]PlanReviewCatchCase, 0, len(misses))
	for _, m := range misses {
		c, err := loadPlanReviewCatchCase(dir, m)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func loadPlanReviewCatchCase(dir string, m NamedPlanReviewMissCase) (PlanReviewCatchCase, error) {
	name := m.Name
	fail := func(format string, args ...any) (PlanReviewCatchCase, error) {
		return PlanReviewCatchCase{}, fmt.Errorf("agenteval: plan-review catch case %q: "+format, append([]any{name}, args...)...)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name, "review_input.json"))
	if err != nil {
		return fail("read review_input.json (every catch-rate case needs a hand-curated review input): %w", err) // (c)
	}
	var in PlanReviewCatchInput
	if err := decodeStrictJSON(raw, &in); err != nil {
		return fail("parse review_input.json: %w", err) // (d)
	}
	if strings.TrimSpace(in.IssueTitle) == "" {
		return fail("review_input.json: issue_title must be non-empty") // (e)
	}
	if strings.TrimSpace(in.IssueBody) == "" {
		return fail("review_input.json: issue_body must be non-empty") // (e)
	}
	p, err := plan.Parse(in.Plan)
	if err != nil {
		return fail("review_input.json: plan is not a valid standard_v1 plan: %w", err) // (f)
	}
	for _, miss := range m.Case.Misses {
		var found *plan.AcceptanceCriterion
		for i := range p.Verification.AcceptanceCriteria {
			if p.Verification.AcceptanceCriteria[i].ID == miss.CriterionID {
				found = &p.Verification.AcceptanceCriteria[i]
				break
			}
		}
		if found == nil {
			return fail("plan acceptance_criteria lack miss criterion %q: the planted defect must be in the reviewed plan", miss.CriterionID) // (g)
		}
		if found.Statement != miss.Statement {
			return fail("plan criterion %q statement %q differs from the miss statement %q", miss.CriterionID, found.Statement, miss.Statement) // (h)
		}
	}
	if len(in.CatchProbes) == 0 {
		return fail("review_input.json: catch_probes must be non-empty") // (i)
	}
	for i, probe := range in.CatchProbes {
		if strings.TrimSpace(probe) == "" {
			return fail("review_input.json: catch_probes[%d] is empty — an empty probe matches every concern", i) // (i)
		}
	}
	if len(in.AttributionPhrases) == 0 {
		return fail("review_input.json: attribution_phrases must be non-empty — an id-free concern naming the planted criterion could never be attributed") // (o)
	}
	for i, phrase := range in.AttributionPhrases {
		if strings.TrimSpace(phrase) == "" {
			return fail("review_input.json: attribution_phrases[%d] is empty — an empty phrase attributes every concern", i) // (o)
		}
	}
	planted := make(map[string]bool, len(m.Case.Misses))
	for _, miss := range m.Case.Misses {
		planted[miss.CriterionID] = true
	}
	for _, phrase := range in.AttributionPhrases {
		lp := strings.ToLower(strings.TrimSpace(phrase))
		inPlanted := false
		for _, ac := range p.Verification.AcceptanceCriteria {
			if !strings.Contains(strings.ToLower(ac.Statement), lp) {
				continue
			}
			if !planted[ac.ID] {
				return fail("attribution phrase %q also occurs in criterion %q's statement: it would attribute a concern about that criterion to the planted one", phrase, ac.ID) // (p)
			}
			inPlanted = true
		}
		if !inPlanted {
			return fail("attribution phrase %q occurs in no planted criterion's statement: it does not name the planted criterion", phrase) // (p)
		}
	}
	for id := range planted {
		lid := strings.ToLower(id)
		for _, ac := range p.Verification.AcceptanceCriteria {
			if ac.ID != id && strings.Contains(strings.ToLower(ac.ID), lid) {
				return fail("planted criterion id %q occurs inside criterion id %q: a concern naming %q would be attributed to the planted criterion", id, ac.ID, ac.ID) // (q)
			}
		}
	}
	if len(in.CatchingExamples) == 0 || len(in.NonCatchingExamples) == 0 {
		return fail("review_input.json: catching_examples and non_catching_examples must both be non-empty — the probes' discrimination is unproven otherwise") // (l)
	}
	rule := catchRuleFor(m.Case, in)
	attributedNonCatching := false
	for i, ex := range in.NonCatchingExamples {
		if probe, ok := matchCatchProbe(ex.Category, ex.Note, in.CatchProbes); ok {
			return fail("catch probe %q matches non_catching_examples[%d] (%q): the probe does not discriminate", probe, i, ex.Note) // (j)
		}
		if _, ok := matchCatchAttribution(ex.Category, ex.Note, rule); ok {
			attributedNonCatching = true
		}
	}
	for i, ex := range in.CatchingExamples {
		if _, _, ok := matchCatchConcern(ex.Category, ex.Note, rule); !ok {
			return fail("catching_examples[%d] (%q) is not a catch under the attribution-AND-probe rule: one concern must name a planted criterion id or an attribution phrase AND match a catch probe", i, ex.Note) // (k)
		}
	}
	if !attributedNonCatching {
		return fail("no non_catching_example names a planted criterion id or an attribution phrase: the probes' discrimination on concerns that DO name the planted criterion is unproven") // (n)
	}
	shown, err := catchShownText(in)
	if err != nil {
		return fail("review_input.json: plan: %w", err)
	}
	if len(in.DefectPhrases) == 0 {
		return fail("review_input.json: defect_phrases must be non-empty — the prompt-leak self-check cannot tell whether the plan-review prompt shows this case's planted defect") // (r)
	}
	for i, phrase := range in.DefectPhrases {
		lp := strings.ToLower(strings.TrimSpace(phrase))
		if lp == "" {
			return fail("review_input.json: defect_phrases[%d] is empty — an empty phrase occurs in every prompt", i) // (r)
		}
		if !strings.Contains(shown, lp) {
			return fail("defect phrase %q occurs in neither the issue nor the plan: a defect phrase must be this case's own content", phrase) // (r)
		}
	}
	for _, probe := range in.CatchProbes {
		if strings.Contains(shown, strings.ToLower(strings.TrimSpace(probe))) {
			return fail("catch probe %q occurs in the issue or plan the reviewer is shown: a concern echoing that wording about an unrelated aspect would score as a catch — use a phrase only a concern flagging the planted defect would use", probe) // (m)
		}
	}
	return PlanReviewCatchCase{Name: name, Miss: m.Case, Input: in, Plan: p, ReviewInputRaw: raw}, nil
}

// decodeStrictJSON decodes exactly ONE JSON value from raw into v: an unknown
// field is refused, and so is any trailing non-whitespace content after the
// value. json.Decoder stops after the first value, so without the EOF check a
// second object or garbage appended to a valid document would be silently
// ignored — a malformed input that decodes as if it were well-formed.
func decodeStrictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing content after the JSON value (offset %d)", dec.InputOffset())
	}
	return nil
}

// catchShownText is the case-folded text a reviewer of this case is shown
// verbatim: the issue title, the issue body, and every key and value of the
// plan. Loader mode (m) refuses a probe occurring in it.
func catchShownText(in PlanReviewCatchInput) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(in.Plan))
	dec.UseNumber()
	var planValue any
	if err := dec.Decode(&planValue); err != nil {
		return "", err
	}
	parts := []string{in.IssueTitle, in.IssueBody}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				parts = append(parts, k)
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case string:
			parts = append(parts, t)
		case json.Number:
			parts = append(parts, t.String())
		}
	}
	walk(planValue)
	return strings.ToLower(strings.Join(parts, "\n")), nil
}

// matchCatchConcern is THE catch matcher, shared by ClassifyCatchTrial and the
// loader's mode (k): ONE concern catches only when it satisfies the
// attribution half (matchCatchAttribution) AND the probe half
// (matchCatchProbe). It returns the matched probe and the attribution kind.
// Changing its meaning requires a catchRuleVersion bump.
func matchCatchConcern(category, note string, rule CatchRule) (probe, attribution string, ok bool) {
	attribution, attributed := matchCatchAttribution(category, note, rule)
	if !attributed {
		return "", "", false
	}
	probe, probed := matchCatchProbe(category, note, rule.Probes)
	if !probed {
		return "", "", false
	}
	return probe, attribution, true
}

// matchCatchAttribution is the attribution half of the rule: CatchAttributionID
// when some planted criterion id occurs as a case-folded substring of the
// concern's note or category, else CatchAttributionPhrase when some declared
// attribution phrase does. The id is checked first, so the recorded share of
// id-attributed catches counts every catch that named the id.
func matchCatchAttribution(category, note string, rule CatchRule) (string, bool) {
	if _, ok := matchCatchProbe(category, note, rule.CriterionIDs); ok {
		return CatchAttributionID, true
	}
	if _, ok := matchCatchProbe(category, note, rule.AttributionPhrases); ok {
		return CatchAttributionPhrase, true
	}
	return "", false
}

// matchCatchProbe is the case-folded substring test both halves use, and the
// probe half on its own (loader mode j): it returns the first term that occurs
// as a case-folded (strings.ToLower) substring of the concern's note or
// category. It mirrors severitycalibration.go's matchLabelledConcern in
// matching over BOTH fields, but is a plain substring test rather than a
// token-overlap score: a probe names the planted defect, not a labelled
// concern. Changing its meaning requires a catchRuleVersion bump.
func matchCatchProbe(category, note string, probes []string) (string, bool) {
	cat := strings.ToLower(category)
	n := strings.ToLower(note)
	for _, p := range probes {
		lp := strings.ToLower(strings.TrimSpace(p))
		if lp == "" {
			continue
		}
		if strings.Contains(n, lp) || strings.Contains(cat, lp) {
			return p, true
		}
	}
	return "", false
}

// LoadRepresentativeConventions reads the committed representative conventions
// file and renders it exactly as the server's resolution path does: the bytes
// are shaped by the REAL repodoc.Fetched.Document (content hash, the size cap,
// and delimiter-line neutralization — the same call repodoc.Resolve makes) at
// the pinned reviewConventionFixtureCommit, then mapped through the REAL
// repodoc.ToPromptDocument under the server's "Review convention <name>"
// framing. It FAILS CLOSED on an absent file, an empty (whitespace-only) file,
// and a file over repodoc.DefaultMaxBytes (the real path would truncate it, so
// the with arm would not be what production renders).
func LoadRepresentativeConventions(path string) (prompt.ReviewConvention, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return prompt.ReviewConvention{}, fmt.Errorf("agenteval: representative conventions fixture %q is absent: the with-conventions arm cannot be rendered", path)
		}
		return prompt.ReviewConvention{}, fmt.Errorf("agenteval: read representative conventions fixture %q: %w", path, err)
	}
	content := string(raw)
	if strings.TrimSpace(content) == "" {
		return prompt.ReviewConvention{}, fmt.Errorf("agenteval: representative conventions fixture %q is empty: the with-conventions arm would carry no conventions", path)
	}
	if len(raw) > repodoc.DefaultMaxBytes {
		return prompt.ReviewConvention{}, fmt.Errorf("agenteval: representative conventions fixture %q is %d bytes, over repodoc.DefaultMaxBytes (%d): the server would truncate it", path, len(raw), repodoc.DefaultMaxBytes)
	}
	fetched := &repodoc.Fetched{Path: representativeConventionPath, Commit: reviewConventionFixtureCommit, Content: raw}
	doc := fetched.Document(repodoc.DefaultMaxBytes)
	return prompt.ReviewConvention{
		Name:        representativeConventionName,
		SeverityCap: representativeConventionSeverityCap,
		Document:    repodoc.ToPromptDocument(doc, repodoc.Framing{Heading: "Review convention " + representativeConventionName}),
	}, nil
}

// ToPlanReviewCatchTrigger builds the plan_review trigger for one case.
// convs is nil for the without arm and the one representative convention for
// the with arm; nothing else differs.
func ToPlanReviewCatchTrigger(c PlanReviewCatchCase, convs []prompt.ReviewConvention) prompt.Trigger {
	return prompt.Trigger{
		Source:            "github_issue",
		IssueTitle:        c.Input.IssueTitle,
		IssueBody:         c.Input.IssueBody,
		Repo:              planReviewCatchRepo,
		ApprovedPlan:      c.Plan,
		ReviewConventions: convs,
	}
}

// CatchRateArmPrompt renders one case's plan-review prompt for one arm. Both
// arms start from the SAME trigger; the with arm only adds the convention.
// It errors on an unknown arm, and on a with arm whose convention is empty (a
// zero-value convention would render a section with no rules — an arm that
// measures nothing).
func CatchRateArmPrompt(c PlanReviewCatchCase, conv prompt.ReviewConvention, arm string) (string, error) {
	var convs []prompt.ReviewConvention
	switch arm {
	case ArmWithoutConventions:
	case ArmWithConventions:
		if strings.TrimSpace(conv.Name) == "" || strings.TrimSpace(conv.Document.Body) == "" {
			return "", fmt.Errorf("agenteval: catch arm %q for case %q: the conventions document is empty", arm, c.Name)
		}
		convs = []prompt.ReviewConvention{conv}
	default:
		return "", fmt.Errorf("agenteval: unknown catch-rate arm %q (want %q or %q)", arm, ArmWithoutConventions, ArmWithConventions)
	}
	built, err := prompt.Build("plan_review", ToPlanReviewCatchTrigger(c, convs))
	if err != nil {
		return "", fmt.Errorf("agenteval: build plan_review prompt for case %q: %w", c.Name, err)
	}
	return built, nil
}

// planReviewEmittedVerdict is the harness-side decode of a plan-review
// verdict. A LOCAL, LENIENT wire shape on purpose: it decodes model output,
// and the conventions framing tells the reviewer to add a `convention` field
// to convention-derived concerns, so a strict decoder would score the with
// arm's extra field as undecodable — a dilution manufactured by the harness.
type planReviewEmittedVerdict struct {
	Verdict  string `json:"verdict"`
	Concerns []struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Note     string `json:"note"`
	} `json:"concerns"`
}

// CatchTrialConcern is one decoded reviewer concern of a trial. NearMiss is
// set only on a MISSED trial's concerns (NearMissProbeWithoutAttribution,
// NearMissAttributionWithoutProbe, or empty when the concern matched neither
// half).
type CatchTrialConcern struct {
	Severity string
	Category string
	Note     string
	NearMiss string
}

// CatchTrial is the classification of one reviewer response, with what the
// trial sidecar records about it.
type CatchTrial struct {
	Outcome CatchOutcome
	// MatchedProbe and Attribution are set only on a CAUGHT trial: the probe
	// the matching concern carried and which attribution half named the
	// planted criterion (CatchAttributionID or CatchAttributionPhrase).
	MatchedProbe string
	Attribution  string
	// Concerns is the matching concern of a CAUGHT trial, every decoded
	// concern (near-miss tagged) of a MISSED trial, and empty for an
	// UNDECODABLE trial.
	Concerns []CatchTrialConcern
	// ConcernsTotal is the number of concerns the verdict carried.
	ConcernsTotal int
	// Response is the raw response text of an UNDECODABLE trial only.
	Response string
}

// ClassifyCatch classifies one reviewer response against a case's catch rule;
// it is ClassifyCatchTrial's outcome.
func ClassifyCatch(responseText string, rule CatchRule) CatchOutcome {
	return ClassifyCatchTrial(responseText, rule).Outcome
}

// ClassifyCatchTrial classifies one reviewer response against a case's catch
// rule: CatchUndecodable when no verdict decodes (no JSON object, malformed
// JSON, or a verdict outside approve|approve_with_concerns|reject);
// CatchCaught when ONE concern satisfies both the attribution half and the
// probe half (matchCatchConcern) — the first such concern is the trial's
// matching concern; CatchMissed otherwise, including a decoded verdict whose
// concerns match only one half each, with every concern near-miss tagged.
func ClassifyCatchTrial(responseText string, rule CatchRule) CatchTrial {
	var v planReviewEmittedVerdict
	if err := json.Unmarshal([]byte(extractJSONObject(responseText)), &v); err != nil {
		return CatchTrial{Outcome: CatchUndecodable, Response: responseText}
	}
	switch v.Verdict {
	case "approve", "approve_with_concerns", "reject":
	default:
		return CatchTrial{Outcome: CatchUndecodable, Response: responseText}
	}
	for _, c := range v.Concerns {
		if probe, attribution, ok := matchCatchConcern(c.Category, c.Note, rule); ok {
			return CatchTrial{
				Outcome:       CatchCaught,
				MatchedProbe:  probe,
				Attribution:   attribution,
				Concerns:      []CatchTrialConcern{{Severity: c.Severity, Category: c.Category, Note: c.Note}},
				ConcernsTotal: len(v.Concerns),
			}
		}
	}
	trial := CatchTrial{Outcome: CatchMissed, Concerns: make([]CatchTrialConcern, 0, len(v.Concerns)), ConcernsTotal: len(v.Concerns)}
	for _, c := range v.Concerns {
		_, probed := matchCatchProbe(c.Category, c.Note, rule.Probes)
		_, attributed := matchCatchAttribution(c.Category, c.Note, rule)
		var tag string
		switch {
		case probed && !attributed:
			tag = NearMissProbeWithoutAttribution
		case attributed && !probed:
			tag = NearMissAttributionWithoutProbe
		}
		trial.Concerns = append(trial.Concerns, CatchTrialConcern{Severity: c.Severity, Category: c.Category, Note: c.Note, NearMiss: tag})
	}
	return trial
}

// CaseCatchCounts is one case's counts in one arm. Undecodable trials are
// counted in Trials and are never Caught, so Caught+Undecodable <= Trials.
type CaseCatchCounts struct {
	Trials      int `json:"trials"`
	Caught      int `json:"caught"`
	Undecodable int `json:"undecodable"`
}

// validate is the per-case count invariant.
func (c CaseCatchCounts) validate() error {
	if c.Trials < 0 || c.Caught < 0 || c.Undecodable < 0 {
		return fmt.Errorf("negative count (trials %d, caught %d, undecodable %d)", c.Trials, c.Caught, c.Undecodable)
	}
	if c.Caught+c.Undecodable > c.Trials {
		return fmt.Errorf("caught %d + undecodable %d exceeds trials %d", c.Caught, c.Undecodable, c.Trials)
	}
	return nil
}

// CatchRateArmReport is one arm's measurement.
type CatchRateArmReport struct {
	Arm            string
	SamplesPerCase int
	// PerCase maps case name to that case's counts.
	PerCase map[string]CaseCatchCounts
	// Trials maps case name to that case's classified trials, in sample
	// order; its outcome tallies equal PerCase. Nil on a report built from
	// recorded counts alone (judgeCatchRate), which never reads it.
	Trials map[string][]CatchTrial
}

// Totals sums the per-case counts.
func (r CatchRateArmReport) Totals() (trials, caught, undecodable int) {
	for _, c := range r.PerCase {
		trials += c.Trials
		caught += c.Caught
		undecodable += c.Undecodable
	}
	return trials, caught, undecodable
}

// Rate is caught / trials over the pooled trials (undecodable included in the
// denominator as misses); 0 when there are no trials.
func (r CatchRateArmReport) Rate() float64 {
	trials, caught, _ := r.Totals()
	if trials == 0 {
		return 0
	}
	return float64(caught) / float64(trials)
}

// SamplesPerCaseForPower returns the per-case sample count that clears the
// power floor for nCases cases: ceil(MinCatchRateTrialsPerArm / nCases). It
// returns 0 for nCases < 1, which RunCatchRateArm refuses (fail closed).
func SamplesPerCaseForPower(nCases int) int {
	if nCases < 1 {
		return 0
	}
	return (MinCatchRateTrialsPerArm + nCases - 1) / nCases
}

// RunCatchRateArm drives render → generate → classify for one arm. Fail
// closed: no cases, samples < 1, a nil sender, an unknown arm, a render error
// or a transport error returns the ZERO report and an error — a partial arm
// would be a silently-biased comparison. An undecodable response is NOT an
// error: it is a trial, scored as a miss and counted in Undecodable. Every
// trial's classification is kept in Trials, in sample order.
func RunCatchRateArm(ctx context.Context, sender MessageSender, cases []PlanReviewCatchCase, conv prompt.ReviewConvention, arm string, samples int) (CatchRateArmReport, error) {
	if len(cases) == 0 {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: catch arm %q: no cases", arm)
	}
	if samples < 1 {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: catch arm %q: samples must be >= 1, got %d", arm, samples)
	}
	if sender == nil {
		return CatchRateArmReport{}, fmt.Errorf("agenteval: catch arm %q: sender is required", arm)
	}
	report := CatchRateArmReport{
		Arm: arm, SamplesPerCase: samples,
		PerCase: make(map[string]CaseCatchCounts, len(cases)),
		Trials:  make(map[string][]CatchTrial, len(cases)),
	}
	for _, c := range cases {
		promptText, err := CatchRateArmPrompt(c, conv, arm)
		if err != nil {
			return CatchRateArmReport{}, err
		}
		rule := c.CatchRule()
		var counts CaseCatchCounts
		trials := make([]CatchTrial, 0, samples)
		for i := 0; i < samples; i++ {
			responseText, _, _, _, _, _, err := sender.Messages(ctx, planReviewCatchGeneratorSystemPrompt, promptText)
			if err != nil {
				return CatchRateArmReport{}, fmt.Errorf("agenteval: catch arm %q case %q sample %d: generate: %w", arm, c.Name, i+1, err)
			}
			counts.Trials++
			trial := ClassifyCatchTrial(responseText, rule)
			switch trial.Outcome {
			case CatchCaught:
				counts.Caught++
			case CatchUndecodable:
				counts.Undecodable++
			}
			trials = append(trials, trial)
		}
		report.PerCase[c.Name] = counts
		report.Trials[c.Name] = trials
	}
	return report, nil
}

// minTrialsForTolerance is the power floor DERIVED from a tolerance:
// ceil(0.5*(z/tolerance)^2). CompareCatchRateArms refuses a minTrials below
// it, so neither the tolerance nor the floor can be loosened independently.
func minTrialsForTolerance(tolerance float64) int {
	r := catchRateOneSidedZ / tolerance
	return int(math.Ceil(0.5 * r * r))
}

// CaseCatchComparison is one case's counts in both arms.
type CaseCatchComparison struct {
	Case    string
	Without CaseCatchCounts
	With    CaseCatchCounts
}

// CatchRateComparison is the verdict of CompareCatchRateArms.
type CatchRateComparison struct {
	WithoutTrials, WithoutCaught, WithoutUndecodable int
	WithTrials, WithCaught, WithUndecodable          int
	WithoutRate, WithRate                            float64
	// Delta is WithoutRate - WithRate: positive means the conventions arm
	// caught LESS.
	Delta     float64
	Tolerance float64
	MinTrials int
	// Regressed is true when Delta is STRICTLY greater than Tolerance,
	// decided in exact rational arithmetic over the counts.
	Regressed bool
	// PerCase is in case-name order.
	PerCase []CaseCatchComparison
}

// CompareCatchRateArms applies the regression rule. It errors (fails closed,
// never a pass) on: a tolerance outside (0,1); a minTrials below the floor
// derived from that tolerance (minTrialsForTolerance); a mislabelled or
// swapped arm; an arm whose per-case counts are inconsistent (negative, or
// caught+undecodable > trials); a case-set mismatch in EITHER direction; a
// case whose trial count differs between the arms (unequal case weights
// confound the pooled rates with case mix); and fewer than minTrials trials
// in EITHER arm. Otherwise Regressed = without rate - with rate > tolerance,
// strictly, compared exactly.
func CompareCatchRateArms(without, with CatchRateArmReport, tolerance float64, minTrials int) (CatchRateComparison, error) {
	if !(tolerance > 0 && tolerance < 1) {
		return CatchRateComparison{}, fmt.Errorf("agenteval: catch-rate tolerance %v must be in (0,1)", tolerance)
	}
	if floor := minTrialsForTolerance(tolerance); minTrials < floor {
		return CatchRateComparison{}, fmt.Errorf("agenteval: minimum trials %d is below the floor %d derived from tolerance %v: lowering the floor widens the bar", minTrials, floor, tolerance)
	}
	if without.Arm != ArmWithoutConventions || with.Arm != ArmWithConventions {
		return CatchRateComparison{}, fmt.Errorf("agenteval: catch-rate arms are mislabelled: got (%q, %q), want (%q, %q)", without.Arm, with.Arm, ArmWithoutConventions, ArmWithConventions)
	}
	for _, r := range []CatchRateArmReport{without, with} {
		for name, c := range r.PerCase {
			if err := c.validate(); err != nil {
				return CatchRateComparison{}, fmt.Errorf("agenteval: catch arm %q case %q: inconsistent counts: %w", r.Arm, name, err)
			}
		}
	}
	for name := range without.PerCase {
		if _, ok := with.PerCase[name]; !ok {
			return CatchRateComparison{}, fmt.Errorf("agenteval: catch-rate case set mismatch: case %q is in the %q arm but not the %q arm", name, without.Arm, with.Arm)
		}
	}
	for name := range with.PerCase {
		if _, ok := without.PerCase[name]; !ok {
			return CatchRateComparison{}, fmt.Errorf("agenteval: catch-rate case set mismatch: case %q is in the %q arm but not the %q arm", name, with.Arm, without.Arm)
		}
	}
	names := make([]string, 0, len(without.PerCase))
	for name := range without.PerCase {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if w, h := without.PerCase[name].Trials, with.PerCase[name].Trials; w != h {
			return CatchRateComparison{}, fmt.Errorf("agenteval: catch-rate trial weight mismatch: case %q has %d trials without conventions and %d with", name, w, h)
		}
	}
	cmp := CatchRateComparison{Tolerance: tolerance, MinTrials: minTrials}
	cmp.WithoutTrials, cmp.WithoutCaught, cmp.WithoutUndecodable = without.Totals()
	cmp.WithTrials, cmp.WithCaught, cmp.WithUndecodable = with.Totals()
	for _, arm := range []struct {
		name   string
		trials int
	}{{without.Arm, cmp.WithoutTrials}, {with.Arm, cmp.WithTrials}} {
		if arm.trials < minTrials {
			return CatchRateComparison{}, fmt.Errorf("agenteval: catch arm %q is under-powered: %d trials < %d: raise samples; do not widen the tolerance", arm.name, arm.trials, minTrials)
		}
	}
	cmp.WithoutRate = without.Rate()
	cmp.WithRate = with.Rate()
	cmp.Delta = cmp.WithoutRate - cmp.WithRate
	// Exact: (wc/wt - hc/ht) > tolerance in rationals. The float delta is for
	// display only — 0.8-0.7 is 0.10000000000000009 in float64, which would
	// fail a measurement sitting exactly on the boundary.
	delta := new(big.Rat).Sub(
		big.NewRat(int64(cmp.WithoutCaught), int64(cmp.WithoutTrials)),
		big.NewRat(int64(cmp.WithCaught), int64(cmp.WithTrials)),
	)
	tol := new(big.Rat).SetFloat64(tolerance)
	cmp.Regressed = delta.Cmp(tol) > 0
	for _, name := range names {
		cmp.PerCase = append(cmp.PerCase, CaseCatchComparison{Case: name, Without: without.PerCase[name], With: with.PerCase[name]})
	}
	return cmp, nil
}

// CatchRateRule states, on one line, the rule the offline gate applies: the
// within-run tolerance, the pinned-baseline rule, the per-arm trial floor and
// the derivation of that floor. catchrategate prints it on EVERY outcome —
// pass, regression, absent, stale or malformed evidence, and infrastructure
// failure — so the bar a record must clear is never missing from the output.
func CatchRateRule() string {
	return fmt.Sprintf("rule: FAIL when the with-conventions catch rate is more than %.2f below the without-conventions rate, "+
		"or either arm is more than %.2f below the same arm of the pinned baseline; each arm needs at least %d trials "+
		"(power floor n >= 0.5*(%.3f/%.2f)^2, one-sided 95%% sampling noise; derivation: backend/internal/agenteval/README.md "+
		"§ \"The rule, the tolerance and the power floor\")",
		DefaultCatchRateRegressionTolerance, DefaultCatchRateRegressionTolerance, MinCatchRateTrialsPerArm,
		catchRateOneSidedZ, DefaultCatchRateRegressionTolerance)
}

// Render states both rates, the delta, the per-case and undecodable counts,
// and the rule in words.
func (c CatchRateComparison) Render() string {
	var b strings.Builder
	b.WriteString("Plan-review catch rate, with vs without repository review conventions (E55.4 / #2245)\n\n")
	fmt.Fprintf(&b, "  without conventions: %d/%d caught (%.3f), %d undecodable\n", c.WithoutCaught, c.WithoutTrials, c.WithoutRate, c.WithoutUndecodable)
	fmt.Fprintf(&b, "  with conventions:    %d/%d caught (%.3f), %d undecodable\n", c.WithCaught, c.WithTrials, c.WithRate, c.WithUndecodable)
	fmt.Fprintf(&b, "  delta (without - with): %+.3f\n\n", c.Delta)
	fmt.Fprintf(&b, "Rule: FAIL when the with-conventions catch rate is MORE than %.2f below the without-conventions rate "+
		"(exactly %.2f below passes). Each arm needs at least %d trials — the one-sided 95%% worst-case sampling-noise "+
		"bound %.3f*sqrt(0.5/n) on the difference is then at most the tolerance; an under-powered measurement is refused, "+
		"never passed. An undecodable verdict counts as a miss. The floor bounds model-sampling noise only: the corpus "+
		"bounds what the gate can detect.\n\n", c.Tolerance, c.Tolerance, c.MinTrials, catchRateOneSidedZ)
	if c.Regressed {
		b.WriteString("Verdict: REGRESSED\n\n")
	} else {
		b.WriteString("Verdict: PASS\n\n")
	}
	b.WriteString("Per case (caught/trials, undecodable):\n")
	for _, pc := range c.PerCase {
		fmt.Fprintf(&b, "  %s: without %d/%d (%d undecodable), with %d/%d (%d undecodable)\n",
			pc.Case, pc.Without.Caught, pc.Without.Trials, pc.Without.Undecodable,
			pc.With.Caught, pc.With.Trials, pc.With.Undecodable)
	}
	return b.String()
}
