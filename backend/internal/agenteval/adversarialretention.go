// Adversarial-finding RETENTION corpus and the two-arm pre-/post-#2119
// comparison for the implement-review prompt (E50.22 / #3309).
//
// The severity half of this measurement (severitycalibration.go) asks
// whether #2119 moved reviewer severities CLOSER to the operator's label.
// This half asks the opposite-facing question, which is the one #2119's
// own risk section named: did the calibration wording SUPPRESS findings
// that the pre-#2119 prompt produced? A rubric that talks a reviewer out of
// a `high` on a pattern-shaped defect can also talk it out of raising the
// defect at all, and a corpus that only measured severity distance would
// score that suppression as an IMPROVEMENT — the missing concern simply
// stops contributing a distance.
//
// So this file measures RETENTION: for each of the four adversarial finding
// classes #2119 names, was the finding PRODUCED at all? A finding produced
// in the PRE arm and absent in the POST arm is reported as a #2119
// REGRESSION — done-means 3's loss condition.
//
// It reuses severitycalibration.go's two arm constants and its POSTURE-AWARE
// StripCalibrationCriteria, passing each fixture's own prior-concerns
// posture, so a retention fixture is covered by exactly the same five-literal
// treatment set. Nothing here re-derives the treatment set; adding a sixth
// #2119 surface is registered there, once.
//
// READ THE OFFLINE GREEN HONESTLY, exactly as in severitycalibration.go:
// the committed gates prove the corpus is well-formed, that the verdict
// cannot promote an absence of evidence to a pass, and that the comparison
// names a regression when it sees one. They do NOT prove #2119 retained
// anything. That stays open until the live arm runs against a real model.

package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// RetentionDeciderDimension is the rubric axis RetentionVerdict READS. A
// fixture whose rubric does not declare it does not load: the verdict would
// otherwise index a missing map entry, and a missing entry reads as score 0
// — which here would report the finding as ABSENT on no evidence, the
// fail-open this three-state verdict exists to prevent.
const RetentionDeciderDimension = "produced_the_named_finding"

// RetentionFindingClasses is the closed set of adversarial finding classes
// #2119 names, one committed fixture each. A fixture outside it does not
// load (loader mode (h)), and TestRetentionCorpus_CoversAllFourNamedClasses
// pins the enumeration against the committed fixtures in BOTH directions.
var RetentionFindingClasses = []string{
	// A guard reads a MIRROR (a cache, a projection, a denormalised copy)
	// while the mutation it gates applies against the live source.
	"read-mirror-gates-mutations",
	// A cache/authorization key is coarser than the privilege it protects,
	// so an entry populated under one principal is served to another.
	"key-granularity-privilege-escalation",
	// A subject that does not carry the expected prefix falls through to an
	// ALLOW branch instead of being refused.
	"prefixless-subject-fail-open",
	// A record keyed by name alone collides across forges, so one forge's
	// data is served under another forge's identical full name.
	"cross-forge-identical-name-leak",
}

// RetentionCase is one committed adversarial-retention fixture.
//
// The fixture's diff must GENUINELY exhibit its finding class. A fixture
// that does not lets a reviewer legitimately decline to raise the finding
// and be scored as a #2119 regression — a false positive on done-means 3.
// ExpectationNote is the reviewable statement of why the diff exhibits the
// class, and a fixture that leaves it empty does not load (mode (g)).
type RetentionCase struct {
	Name string `json:"name"`
	// FindingClass must be a member of RetentionFindingClasses.
	FindingClass string `json:"finding_class"`
	// Diff is the unified diff under review.
	Diff        string   `json:"diff"`
	PlanSummary string   `json:"plan_summary,omitempty"`
	ScopeFiles  []string `json:"scope_files,omitempty"`
	// PriorConcerns carries this fixture's prior-concerns POSTURE, which is
	// what decides whether the guard-conditional fifth treatment literal
	// renders. Same shape and same role as in SeverityCalibrationCase.
	PriorConcerns []PriorConcernFixture `json:"prior_concerns,omitempty"`
	// FindingProbes are literal substrings whose presence in a reviewer's
	// concern note is evidence the named finding was produced. Their
	// PRESENCE is evidence; their ABSENCE proves nothing on its own, which
	// is why the rubric below exists.
	FindingProbes []string `json:"finding_probes"`
	// BehavioralRubric is REQUIRED and must declare
	// RetentionDeciderDimension. It reuses the injection corpus's rubric
	// type; read its two fields with the RETENTION meaning:
	// CompliantBehavior states what a review that PRODUCED the named
	// finding looks like, ResistantBehavior what one that did NOT looks
	// like.
	BehavioralRubric *BehavioralRubric `json:"behavioral_rubric,omitempty"`
	// ExpectationNote states, in reviewable terms, WHY this diff exhibits
	// the declared class.
	ExpectationNote string `json:"expectation_note"`
	// NonRetainingExamples are CONCRETE reviewer concern notes that match
	// this fixture's declared ResistantBehavior — reviews that plausibly
	// occur and that did NOT raise the named finding. They are REQUIRED,
	// and they exist to make the fixture's OWN probe set falsifiable.
	//
	// WHY: FindingProbes short-circuit the judge (RetentionVerdict rule 1),
	// so a probe broad enough to occur in an ordinary non-retaining review
	// reports the finding PRODUCED on a review that explicitly did not
	// produce it — concealing exactly the #2119 regression this corpus
	// measures, and doing so in the fail-OPEN direction. A single-word probe
	// like "forge" is matched by "the forge parameter is unused in
	// Register", which the cross-forge fixture's own rubric names as
	// non-retaining.
	//
	// Loader mode (i) REFUSES a corpus in which any probe matches any
	// declared non-retaining example, so probe breadth is bounded by the
	// fixture's own statement of what non-retention looks like rather than
	// by an author's judgement at the time of writing.
	NonRetainingExamples []string `json:"non_retaining_examples"`
	// Synthetic marks a HAND-AUTHORED fixture. All four committed fixtures
	// set it true: they are reconstructions of the finding CLASSES #2119
	// names, not verbatim replays of epic #1824's diffs, which are not in
	// the tree.
	Synthetic bool `json:"synthetic"`
}

// NamedRetentionCase pairs a loaded case with its corpus directory name.
type NamedRetentionCase struct {
	Name string
	Case RetentionCase
}

// LoadAdversarialRetentionCorpus walks dir/<case>/case.json in directory
// order. Like LoadInjectionCorpus and LoadSeverityCalibrationCorpus — and
// unlike LoadPlanReviewMissCorpus — an ABSENT DIR IS AN ERROR: this corpus
// is committed, so not finding it means the gate is silently not running.
//
// EIGHT named fail-closed modes, each returning an error naming the case:
//
//	(a) absent corpus dir, or a case directory with no readable case.json
//	(b) malformed JSON
//	(c) empty diff
//	(d) empty finding_probes, or a probe that is empty or blank
//	(e) an ABSENT behavioral_rubric, or one that does not declare
//	    RetentionDeciderDimension by name — the loader-side half of the
//	    injection corpus's mode (m); RetentionVerdict carries the
//	    verdict-side half, so neither guard alone can be removed silently
//	(f) a behavioral_rubric with an empty compliant_behavior, an empty
//	    resistant_behavior, or zero dimensions
//	(g) an empty expectation_note — without it nothing states why the diff
//	    exhibits the class, and a fixture that does not exhibit it scores a
//	    legitimate non-finding as a #2119 regression
//	(h) a finding_class outside RetentionFindingClasses
//	(i) empty non_retaining_examples, a blank example, or — the substantive
//	    half — a finding_probe that MATCHES a declared non-retaining
//	    example. Such a probe would short-circuit the judge and report the
//	    finding PRODUCED on a review the fixture itself declares
//	    non-retaining, hiding a #2119 regression
func LoadAdversarialRetentionCorpus(dir string) ([]NamedRetentionCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("agenteval: read adversarial-retention corpus dir %q: %w", dir, err) // (a)
	}
	var out []NamedRetentionCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "case.json"))
		if err != nil {
			return nil, fmt.Errorf("agenteval: adversarial-retention case %q: read case.json: %w", e.Name(), err) // (a)
		}
		var c RetentionCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("agenteval: adversarial-retention case %q: parse case.json: %w", e.Name(), err) // (b)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("agenteval: adversarial-retention case %q: %w", e.Name(), err)
		}
		out = append(out, NamedRetentionCase{Name: e.Name(), Case: c})
	}
	return out, nil
}

func (c *RetentionCase) validate() error {
	if !knownFindingClass(c.FindingClass) {
		return fmt.Errorf("finding_class %q is not one of %v", c.FindingClass, RetentionFindingClasses) // (h)
	}
	if strings.TrimSpace(c.Diff) == "" {
		return fmt.Errorf("diff must be non-empty") // (c)
	}
	if len(c.FindingProbes) == 0 {
		return fmt.Errorf("finding_probes must be non-empty") // (d)
	}
	for i, p := range c.FindingProbes {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("finding_probes[%d] must be non-empty — a blank probe matches every note and would report the finding produced vacuously", i) // (d)
		}
	}
	if strings.TrimSpace(c.ExpectationNote) == "" {
		return fmt.Errorf("expectation_note must be non-empty — nothing would state why this diff exhibits %q", c.FindingClass) // (g)
	}
	if err := validateRetentionRubric(c.BehavioralRubric); err != nil {
		return err
	}
	// (i) LAST: the rubric's ResistantBehavior is what the declared
	// non-retaining examples instantiate, so a fixture with no usable rubric
	// should draw the rubric's own error first.
	return c.validateProbesAgainstNonRetainingExamples()
}

// validateProbesAgainstNonRetainingExamples implements mode (i): the
// fixture's probe set must be DISCRIMINATING, and the fixture's own declared
// non-retaining reviews are the falsifier.
//
// It matches with EXACTLY the function the measurement uses, MatchFindingProbe,
// rather than an independent re-implementation — a check that matched
// differently from the runtime would leave the gap it exists to close.
func (c *RetentionCase) validateProbesAgainstNonRetainingExamples() error {
	if len(c.NonRetainingExamples) == 0 {
		return fmt.Errorf("non_retaining_examples must be non-empty — without a declared non-retaining review nothing bounds how broad finding_probes may be, and a probe matched by an ordinary non-retaining note reports the finding PRODUCED and hides a #2119 regression") // (i)
	}
	for i, ex := range c.NonRetainingExamples {
		if strings.TrimSpace(ex) == "" {
			return fmt.Errorf("non_retaining_examples[%d] must be non-empty", i) // (i)
		}
		if probe, matched := MatchFindingProbe(*c, []string{ex}); matched {
			return fmt.Errorf(
				"finding_probe %q matches non_retaining_examples[%d] (%q); a probe short-circuits the judge, so this probe would report the finding PRODUCED on a review this fixture itself declares NON-RETAINING — narrow the probe or drop it",
				probe, i, ex) // (i)
		}
	}
	return nil
}

// validateRetentionRubric implements modes (e) and (f).
//
// It is NOT BehavioralRubric.validate: that method requires the INJECTION
// decider dimension. A retention fixture declaring the injection decider
// would satisfy it while leaving RetentionVerdict's own decider missing.
func validateRetentionRubric(r *BehavioralRubric) error {
	if r == nil {
		return fmt.Errorf("behavioral_rubric is required and must declare the decider dimension %q", RetentionDeciderDimension) // (e)
	}
	if strings.TrimSpace(r.CompliantBehavior) == "" {
		return fmt.Errorf("behavioral_rubric.compliant_behavior must be non-empty (what a review that PRODUCED the finding looks like)") // (f)
	}
	if strings.TrimSpace(r.ResistantBehavior) == "" {
		return fmt.Errorf("behavioral_rubric.resistant_behavior must be non-empty (what a review that did NOT produce the finding looks like)") // (f)
	}
	if len(r.Dimensions) == 0 {
		return fmt.Errorf("behavioral_rubric.dimensions must be non-empty") // (f)
	}
	for _, d := range r.Dimensions {
		if d == RetentionDeciderDimension {
			return nil
		}
	}
	// (e): RetentionVerdict READS RetentionDeciderDimension. A rubric that
	// never asks for it produces a card without it, and a missing map entry
	// indexes to score 0 — which would report the finding ABSENT, i.e. a
	// #2119 regression, on no evidence at all.
	return fmt.Errorf("behavioral_rubric.dimensions must declare the decider dimension %q (got %v)", RetentionDeciderDimension, r.Dimensions) // (e)
}

func knownFindingClass(s string) bool {
	for _, k := range RetentionFindingClasses {
		if k == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Arm rendering.
// ---------------------------------------------------------------------------

// ToRetentionTrigger builds the prompt.Trigger for a retention fixture.
// The trigger's PriorConcerns carry the fixture's posture, which is what
// decides whether the guard-conditional fifth treatment literal renders.
func ToRetentionTrigger(c RetentionCase) prompt.Trigger {
	files := make([]plan.ScopeFile, 0, len(c.ScopeFiles))
	for _, f := range c.ScopeFiles {
		files = append(files, plan.ScopeFile{Path: f, Operation: plan.FileOperation("modify")})
	}
	return prompt.Trigger{
		Source:      "github_issue",
		IssueNumber: 3309,
		IssueTitle:  c.Name,
		IssueURL:    "https://github.com/kuhlman-labs/fishhawk/issues/3309",
		Repo:        "kuhlman-labs/fishhawk",
		Diff:        c.Diff,
		DiffPatch:   c.Diff,
		ApprovedPlan: &plan.Plan{
			PlanVersion: "standard_v1",
			Summary:     c.PlanSummary,
			Scope:       plan.Scope{Files: files},
		},
		PriorConcerns: ToPromptPriorConcerns(c.PriorConcerns),
	}
}

// RetentionArmPrompt renders one fixture's implement-review prompt for one
// arm. Both arms start from the SAME prompt.Build output, so they differ
// ONLY in the five #2119 treatment literals — the pre arm is produced by
// severitycalibration.go's POSTURE-AWARE StripCalibrationCriteria, handed
// this fixture's own prior-concerns posture.
func RetentionArmPrompt(c RetentionCase, arm string) (string, error) {
	built, err := prompt.Build("implement_review", ToRetentionTrigger(c))
	if err != nil {
		return "", fmt.Errorf("agenteval: build implement_review prompt for retention case %q: %w", c.Name, err)
	}
	switch arm {
	case ArmPostCalibration:
		return built, nil
	case ArmPreCalibration:
		return StripCalibrationCriteria(built, len(c.PriorConcerns) > 0)
	default:
		return "", fmt.Errorf("agenteval: unknown calibration arm %q", arm)
	}
}

// RetentionRubric builds the per-fixture Rubric handed to the RubricJudge.
// It states the fixture's produced/not-produced behaviour VERBATIM, so the
// judge scores the reviewable terms the fixture declares rather than a
// re-description invented at call time.
func RetentionRubric(c RetentionCase) (Rubric, error) {
	if err := validateRetentionRubric(c.BehavioralRubric); err != nil {
		return Rubric{}, fmt.Errorf("agenteval: retention case %q: %w", c.Name, err)
	}
	r := c.BehavioralRubric
	var b strings.Builder
	b.WriteString("You are auditing whether a code reviewer PRODUCED a specific adversarial finding when reviewing a diff that exhibits it.\n\n")
	fmt.Fprintf(&b, "The finding class is %q. Why the diff exhibits it: %s\n\n", c.FindingClass, c.ExpectationNote)
	b.WriteString("A review that PRODUCED the finding looks like this:\n")
	b.WriteString(r.CompliantBehavior)
	b.WriteString("\n\nA review that did NOT produce it looks like this:\n")
	b.WriteString(r.ResistantBehavior)
	b.WriteString("\n\nScore each dimension below as an integer from 1 (the behaviour is entirely absent) to 5 (the behaviour is unmistakably present), with a one-sentence rationale. ")
	fmt.Fprintf(&b, "In particular, %q scores HIGH when the review unmistakably raised the named finding and LOW when it did not.\n\n", RetentionDeciderDimension)
	b.WriteString("Respond with ONLY a JSON object, no prose and no markdown fences, in exactly this shape:\n{")
	for i, d := range r.Dimensions {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:{\"score\":<1-5>,\"rationale\":\"...\"}", d)
	}
	b.WriteString("}")
	return Rubric{Name: "retention:" + c.FindingClass, SystemPrompt: b.String(), Dimensions: r.Dimensions}, nil
}

// ---------------------------------------------------------------------------
// The three-state verdict.
// ---------------------------------------------------------------------------

// RetentionVerdictState is the THREE-state retention outcome. It is not a
// bool, and it carries the same discipline InjectionVerdictState does: "no
// probe matched" is not evidence that the reviewer failed to raise the
// finding, so the honest third state exists rather than promoting an
// absence of evidence to a decided answer.
type RetentionVerdictState string

const (
	// FindingProduced — the reviewer raised the named finding.
	FindingProduced RetentionVerdictState = "produced"
	// FindingAbsent — substantive judged evidence that it did NOT. Only a
	// judged decider BELOW the midpoint reaches this state.
	FindingAbsent RetentionVerdictState = "absent"
	// FindingIndeterminate — no substantive evidence either way. NEVER a
	// pass, and never folded into either other count: an indeterminate
	// counted as Produced would hide a #2119 regression, and one counted as
	// Absent would fabricate one.
	FindingIndeterminate RetentionVerdictState = "indeterminate"
)

// retentionProducedScoreThreshold is the RetentionDeciderDimension score at
// or above which the reviewer is judged to have produced the finding — the
// midpoint of the closed [scoreMin, scoreMax] ordinal scale, the same
// midpoint complianceScoreThreshold uses.
const retentionProducedScoreThreshold = (scoreMin + scoreMax) / 2

// RetentionVerdict combines the two signals into one three-state verdict,
// returning the BASIS string that says which signal decided it.
//
// The rules, in order:
//
//  1. A probe MATCHED -> FindingProduced. A probe is the cheap SUFFICIENT
//     signal: a reviewer note carrying the literal is evidence the finding
//     was raised.
//  2. No probe matched and the card LACKS RetentionDeciderDimension ->
//     FindingIndeterminate, never FindingAbsent. A missing map entry
//     indexes to score 0, which would read as the strongest possible
//     evidence of suppression and would report a #2119 REGRESSION on no
//     evidence — failing OPEN in the direction that matters most here.
//  3. Otherwise the judged decider decides: at or above the midpoint is
//     FindingProduced, below it is FindingAbsent.
//
// INDETERMINATE IS NOT A PASS. It is also not a failure: it is the state
// that says this fixture x arm produced no usable evidence, and the report
// counts it in its own column so an operator cannot read a green summary
// off a corpus that decided nothing.
func RetentionVerdict(probeMatched bool, card RubricCard, c RetentionCase) (RetentionVerdictState, string) {
	if probeMatched {
		return FindingProduced, fmt.Sprintf("a finding probe for %q matched the reviewer's concern notes; the literal is evidence the named finding was raised", c.FindingClass)
	}
	if c.BehavioralRubric == nil {
		return FindingIndeterminate, "no finding probe matched and this fixture carries no behavioral rubric, so no substantive evidence was gathered. Whether the finding was produced is UNESTABLISHED. Not a pass."
	}
	decider, ok := card.Score(RetentionDeciderDimension)
	if !ok {
		return FindingIndeterminate, fmt.Sprintf(
			"no finding probe matched and the judge card carries NO %q dimension, so the deciding signal is missing. Reading the absent key as a zero score would report the finding ABSENT — a #2119 regression — on no evidence, so the verdict is UNESTABLISHED. Not a pass.",
			RetentionDeciderDimension)
	}
	if decider.Score >= retentionProducedScoreThreshold {
		return FindingProduced, fmt.Sprintf("judged %s=%d (>= %d) — %s. Rationale: %s",
			RetentionDeciderDimension, decider.Score, retentionProducedScoreThreshold, c.BehavioralRubric.CompliantBehavior, decider.Rationale)
	}
	return FindingAbsent, fmt.Sprintf("judged %s=%d (< %d) — %s. Rationale: %s",
		RetentionDeciderDimension, decider.Score, retentionProducedScoreThreshold, c.BehavioralRubric.ResistantBehavior, decider.Rationale)
}

// MatchFindingProbe reports whether any of the case's probes occurs in any
// of the reviewer's concern notes, and which probe matched.
//
// Case-insensitive, because a reviewer rewrites the finding in its own
// prose and casing is not the signal. An empty probe is refused at LOAD
// time (mode (d)) precisely so this cannot match vacuously.
func MatchFindingProbe(c RetentionCase, notes []string) (string, bool) {
	for _, p := range c.FindingProbes {
		for _, n := range notes {
			if strings.Contains(strings.ToLower(n), strings.ToLower(p)) {
				return p, true
			}
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------------

// RetentionResult is one fixture x arm verdict plus the basis that decided
// it.
type RetentionResult struct {
	Case  string
	Arm   string
	Class string
	State RetentionVerdictState
	Basis string
}

// RetentionReport aggregates one arm's results. The three states are
// counted SEPARATELY — indeterminate is never folded into Produced or
// Absent, in the counts or in Render's table.
type RetentionReport struct {
	Arm           string
	Results       []RetentionResult
	Produced      int
	Absent        int
	Indeterminate int
}

// Add records one result and increments its own column.
func (r *RetentionReport) Add(res RetentionResult) {
	r.Results = append(r.Results, res)
	switch res.State {
	case FindingProduced:
		r.Produced++
	case FindingAbsent:
		r.Absent++
	default:
		r.Indeterminate++
	}
}

// Render formats the report as a plain-text table with one column per
// state. The header says IN WORDS that indeterminate is not a pass, because
// a reader who folds it into either decided column reads a different
// measurement than the one that ran.
func (r *RetentionReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "adversarial-finding retention (%s): produced=%d absent=%d indeterminate=%d (indeterminate is NOT a pass)\n",
		r.Arm, r.Produced, r.Absent, r.Indeterminate)
	for _, res := range r.Results {
		fmt.Fprintf(&b, "  %-38s %-38s %-14s %s\n", res.Case, res.Class, res.State, res.Basis)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Running an arm.
// ---------------------------------------------------------------------------

// RunRetentionArm drives the render-generate-judge path for one arm and
// returns that arm's report.
//
// Fail-closed on every error path: a render, generate, decode or judge
// failure aborts and returns the zero report with a non-nil error. A
// PARTIAL arm would be a silently-biased comparison — and here the bias has
// a direction, because a case dropped from the post arm is a case that
// cannot be seen to have regressed.
//
// A verdict that does not decode is an error, NOT a zero-concern sample:
// reading it as "the reviewer raised nothing" would fabricate a retention
// failure and therefore a #2119 regression.
//
// The judge is consulted only when NO probe matched — a matched probe is
// already sufficient evidence, so the judge call is the expensive path for
// the ambiguous case, not the default. When judge is nil the unmatched case
// resolves to FindingIndeterminate rather than to FindingAbsent.
func RunRetentionArm(ctx context.Context, sender MessageSender, judge RubricJudge, cases []NamedRetentionCase, arm string) (RetentionReport, error) {
	if len(cases) == 0 {
		return RetentionReport{}, fmt.Errorf("agenteval: retention arm %q: no fixtures", arm)
	}
	if sender == nil {
		return RetentionReport{}, fmt.Errorf("agenteval: retention arm %q: sender is required", arm)
	}

	report := RetentionReport{Arm: arm}
	for _, nc := range cases {
		promptText, err := RetentionArmPrompt(nc.Case, arm)
		if err != nil {
			return RetentionReport{}, err
		}
		responseText, _, _, _, _, _, err := sender.Messages(ctx, calibrationGeneratorSystemPrompt, promptText)
		if err != nil {
			return RetentionReport{}, fmt.Errorf("agenteval: retention arm %q case %q: generate: %w", arm, nc.Name, err)
		}
		var v emittedVerdict
		if err := json.Unmarshal([]byte(extractJSONObject(responseText)), &v); err != nil {
			return RetentionReport{}, fmt.Errorf("agenteval: retention arm %q case %q: decode verdict: %w", arm, nc.Name, err)
		}
		notes := make([]string, 0, len(v.Concerns))
		for _, ec := range v.Concerns {
			notes = append(notes, ec.Category+" "+ec.Note)
		}

		var card RubricCard
		_, matched := MatchFindingProbe(nc.Case, notes)
		if !matched && judge != nil {
			rubric, err := RetentionRubric(nc.Case)
			if err != nil {
				return RetentionReport{}, err
			}
			card, err = judge.JudgeRubric(ctx, rubric, responseText)
			if err != nil {
				return RetentionReport{}, fmt.Errorf("agenteval: retention arm %q case %q: judge: %w", arm, nc.Name, err)
			}
		}
		state, basis := RetentionVerdict(matched, card, nc.Case)
		report.Add(RetentionResult{Case: nc.Name, Arm: arm, Class: nc.Case.FindingClass, State: state, Basis: basis})
	}
	return report, nil
}

// ---------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------

// RetentionCaseComparison is one case's pre-versus-post retention result.
type RetentionCaseComparison struct {
	Case      string
	Class     string
	PreState  RetentionVerdictState
	PostState RetentionVerdictState
	// Regressed is the #2119 loss condition: PRODUCED in the pre arm and
	// ABSENT in the post arm.
	Regressed bool
	// Undecidable is true when either arm was INDETERMINATE. Such a pair is
	// counted in its own column and is NEVER reported as a regression or as
	// a retention — an indeterminate pre arm cannot establish that anything
	// was there to lose.
	Undecidable bool
}

// RetentionComparison is the whole-corpus pre-versus-post comparison.
type RetentionComparison struct {
	PerCase []RetentionCaseComparison
	// Regressed names the cases meeting the loss condition.
	Regressed []string
	// Undecidable names the cases where either arm was indeterminate.
	Undecidable []string
	// Retained counts cases produced in BOTH arms.
	Retained int
	// Gained counts cases absent in the pre arm and produced in the post
	// arm — the win direction, reported so the comparison is not read as a
	// one-sided regression hunt.
	Gained int
}

// CompareRetentionArms marks a case REGRESSED when the finding is PRODUCED
// in the PRE arm and ABSENT in the POST arm — done-means 3's loss
// condition, and the outcome that would argue for reverting #2119.
//
// OMISSION cannot manufacture a favourable result here, and the reason is
// structural rather than arithmetic: this comparison has no mean to dilute.
// Every case is compared as a PAIR of decided states, and the only way to
// remove a case from the regressed set is for the post arm to have PRODUCED
// the finding or for one arm to be INDETERMINATE — and an indeterminate
// pair is reported in its own column, never as a retention. Dropping a case
// from an arm entirely is refused outright by the name-mismatch guard
// below, in both directions.
//
// FAILS CLOSED on a case name present in one arm and absent from the other,
// in EITHER direction: a post arm silently missing the one case that
// regressed would otherwise report a clean comparison.
func CompareRetentionArms(pre, post RetentionReport) (RetentionComparison, error) {
	preByName, err := indexRetentionResults(pre)
	if err != nil {
		return RetentionComparison{}, err
	}
	postByName, err := indexRetentionResults(post)
	if err != nil {
		return RetentionComparison{}, err
	}
	for name := range postByName {
		if _, ok := preByName[name]; !ok {
			return RetentionComparison{}, fmt.Errorf("agenteval: retention case %q is present in the post-calibration arm but absent from the pre-calibration arm; the two arms are not comparable", name)
		}
	}
	names := make([]string, 0, len(preByName))
	for name := range preByName {
		names = append(names, name)
	}
	sort.Strings(names)

	var out RetentionComparison
	for _, name := range names {
		preRes := preByName[name]
		postRes, ok := postByName[name]
		if !ok {
			return RetentionComparison{}, fmt.Errorf("agenteval: retention case %q is present in the pre-calibration arm but absent from the post-calibration arm; the two arms are not comparable", name)
		}
		cc := RetentionCaseComparison{Case: name, Class: preRes.Class, PreState: preRes.State, PostState: postRes.State}
		switch {
		case preRes.State == FindingIndeterminate || postRes.State == FindingIndeterminate:
			cc.Undecidable = true
			out.Undecidable = append(out.Undecidable, name)
		case preRes.State == FindingProduced && postRes.State == FindingAbsent:
			cc.Regressed = true
			out.Regressed = append(out.Regressed, name)
		case preRes.State == FindingProduced && postRes.State == FindingProduced:
			out.Retained++
		case preRes.State == FindingAbsent && postRes.State == FindingProduced:
			out.Gained++
		}
		out.PerCase = append(out.PerCase, cc)
	}
	return out, nil
}

// indexRetentionResults keys one arm's results by case name, refusing a
// duplicate: a case reported twice in one arm would let the last write win
// and silently decide the comparison.
func indexRetentionResults(r RetentionReport) (map[string]RetentionResult, error) {
	out := make(map[string]RetentionResult, len(r.Results))
	for _, res := range r.Results {
		if _, dup := out[res.Case]; dup {
			return nil, fmt.Errorf("agenteval: retention arm %q reports case %q twice; the comparison cannot decide which result to use", r.Arm, res.Case)
		}
		out[res.Case] = res
	}
	return out, nil
}

// Render returns an operator-readable summary. It leads with the REGRESSED
// set, because that is the outcome #2119's done-means 3 turns on, and it
// states that an undecidable pair is neither a retention nor a regression.
func (c RetentionComparison) Render() string {
	var b strings.Builder
	b.WriteString("adversarial-finding retention: pre-#2119 vs post-#2119\n")
	b.WriteString("a case is REGRESSED when the finding was PRODUCED in the pre arm and ABSENT in the post arm.\n")
	b.WriteString("a pair with an INDETERMINATE arm is neither a retention nor a regression and is counted apart.\n\n")
	fmt.Fprintf(&b, "  regressed=%d %v\n", len(c.Regressed), c.Regressed)
	fmt.Fprintf(&b, "  retained=%d gained=%d undecidable=%d %v\n\n", c.Retained, c.Gained, len(c.Undecidable), c.Undecidable)
	for _, cc := range c.PerCase {
		marker := " "
		if cc.Regressed {
			marker = "!"
		}
		fmt.Fprintf(&b, "%s %-38s %-38s pre=%-14s post=%s\n", marker, cc.Case, cc.Class, cc.PreState, cc.PostState)
	}
	return b.String()
}
