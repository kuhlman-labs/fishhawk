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
// A trial is CAUGHT when any emitted concern matches any case probe
// (matchCatchProbe: case-folded substring over the concern's note AND
// category). A probe never occurs in the issue or plan the reviewer is shown
// (loader mode m), so a concern that merely ECHOES that wording about an
// unrelated aspect scores missed rather than inflating both arms toward a
// ceiling that would hide a dilution. An UNDECODABLE verdict counts as a MISS
// in the rate and is reported separately — excluding it from the denominator
// would let an arm that stops emitting parseable verdicts look better, not
// worse.
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
// The corpus — six synthetic planted-defect shapes — bounds what the gate can
// detect; a dilution that spares these six shapes is invisible to it.
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
	// CatchCaught means the verdict decoded and some concern matched a probe.
	CatchCaught CatchOutcome = "caught"
	// CatchMissed means the verdict decoded and no concern matched any probe.
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
	// whenever ClassifyCatch or matchCatchProbe changes meaning: it is
	// folded into the evidence fingerprint, so a bump makes a recorded
	// measurement stale instead of silently re-scoring it under new rules.
	catchRuleVersion = "planreview-catch-v2: a trial is caught when the response decodes (extract first '{'..last '}', " +
		"verdict one of approve|approve_with_concerns|reject) and some concern's note OR category contains some " +
		"catch probe as a case-folded (strings.ToLower) substring; an undecodable response is a miss counted in trials; " +
		"no probe occurs case-folded in the case's issue title, issue body or plan text (loader mode m)"
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
	// CatchProbes are case-folded substrings; a concern whose note or
	// category contains any of them catches the planted defect.
	CatchProbes         []string              `json:"catch_probes"`
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
//	    matchCatchProbe, the runtime matcher itself
//	(k) a catching_example matching no probe
//	(l) empty catching_examples or non_catching_examples
//	(m) a probe occurring (case-folded) in the issue title, the issue body or
//	    any key or value of the plan — text the reviewer is SHOWN, so a
//	    concern echoing it about an unrelated aspect would score as a catch
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
	if len(in.CatchingExamples) == 0 || len(in.NonCatchingExamples) == 0 {
		return fail("review_input.json: catching_examples and non_catching_examples must both be non-empty — the probes' discrimination is unproven otherwise") // (l)
	}
	for i, ex := range in.NonCatchingExamples {
		if probe, ok := matchCatchProbe(ex.Category, ex.Note, in.CatchProbes); ok {
			return fail("catch probe %q matches non_catching_examples[%d] (%q): the probe does not discriminate", probe, i, ex.Note) // (j)
		}
	}
	for i, ex := range in.CatchingExamples {
		if _, ok := matchCatchProbe(ex.Category, ex.Note, in.CatchProbes); !ok {
			return fail("catching_examples[%d] (%q) matches no catch probe", i, ex.Note) // (k)
		}
	}
	shown, err := catchShownText(in)
	if err != nil {
		return fail("review_input.json: plan: %w", err)
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

// matchCatchProbe is THE catch matcher, shared by ClassifyCatch and the
// loader's discrimination checks (modes j/k): it returns the first probe that
// occurs as a case-folded (strings.ToLower) substring of the concern's note or
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
		Category string `json:"category"`
		Note     string `json:"note"`
	} `json:"concerns"`
}

// ClassifyCatch classifies one reviewer response against a case's probes:
// CatchUndecodable when no verdict decodes (no JSON object, malformed JSON,
// or a verdict outside approve|approve_with_concerns|reject); CatchCaught when
// some concern matches some probe (matchCatchProbe); CatchMissed otherwise —
// including a decoded verdict whose concerns match nothing.
func ClassifyCatch(responseText string, probes []string) CatchOutcome {
	var v planReviewEmittedVerdict
	if err := json.Unmarshal([]byte(extractJSONObject(responseText)), &v); err != nil {
		return CatchUndecodable
	}
	switch v.Verdict {
	case "approve", "approve_with_concerns", "reject":
	default:
		return CatchUndecodable
	}
	for _, c := range v.Concerns {
		if _, ok := matchCatchProbe(c.Category, c.Note, probes); ok {
			return CatchCaught
		}
	}
	return CatchMissed
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
// error: it is a trial, scored as a miss and counted in Undecodable.
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
	report := CatchRateArmReport{Arm: arm, SamplesPerCase: samples, PerCase: make(map[string]CaseCatchCounts, len(cases))}
	for _, c := range cases {
		promptText, err := CatchRateArmPrompt(c, conv, arm)
		if err != nil {
			return CatchRateArmReport{}, err
		}
		var counts CaseCatchCounts
		for i := 0; i < samples; i++ {
			responseText, _, _, _, _, _, err := sender.Messages(ctx, planReviewCatchGeneratorSystemPrompt, promptText)
			if err != nil {
				return CatchRateArmReport{}, fmt.Errorf("agenteval: catch arm %q case %q sample %d: generate: %w", arm, c.Name, i+1, err)
			}
			counts.Trials++
			switch ClassifyCatch(responseText, c.Input.CatchProbes) {
			case CatchCaught:
				counts.Caught++
			case CatchUndecodable:
				counts.Undecodable++
			}
		}
		report.PerCase[c.Name] = counts
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
