// Severity-calibration corpus and the two-arm pre-/post-#2119 comparison
// for the implement-review prompt (E50.22 / #3309).
//
// #2119 added a severity rubric plus two standing calibration criteria to
// the implement-review prompt on the bet that they would move reviewer
// severities closer to the severity an operator actually assigned when
// dispositioning the concern. This file is the APPARATUS that tests the
// bet, in two halves:
//
//   - OFFLINE (runs in every `scripts/test verify`, no model call): the
//     arm-correctness gate. The PRE-#2119 arm is produced by a
//     HARNESS-side transform over the real shipped prompt, and every
//     treatment literal it removes is verified byte-exact against a
//     genuinely built prompt, so a prompt.go edit reddens this package
//     rather than silently weakening the arm.
//   - LIVE (opt-in, double env-gated, SKIPPED in the committed tree): the
//     measurement itself.
//
// READ THE OFFLINE GREEN HONESTLY: it proves the arms are correct and the
// comparison cannot be gamed. It does NOT prove #2119 calibrated anything.
// That question stays open until the live arms run against a real model.
//
// THE #2119 TREATMENT SET IS EXACTLY FIVE LITERALS (see treatmentLiterals).
// Four of them render together in the standing-criteria and severity-rubric
// sections; the fifth — the re-read-before-reopen bullet — renders in a
// COMPLETELY SEPARATE place, inside the `if len(t.PriorConcerns) > 0` guard
// of the "Prior concerns (delta verification)" section. Because that fifth
// literal is GUARD-CONDITIONAL, leaving it in the pre arm would be
// INVISIBLE on a fixture set that never populates the guard and would
// corrupt the comparison the moment one did. So the strip is POSTURE-AWARE
// and fails closed in BOTH directions.
//
// Adding a SIXTH #2119 surface to the implement-review prompt without
// registering it here silently weakens the pre arm. Nothing here can detect
// that; the residual is stated rather than papered over.
//
// This file imports backend/internal/prompt to render the real shipped
// prompt. prompt does NOT import agenteval, so the edge is acyclic.

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

// The two arms of the severity-calibration A/B. ArmPostCalibration is the
// prompt as shipped today; ArmPreCalibration is that same prompt with the
// five #2119 treatment literals removed by StripCalibrationCriteria.
const (
	ArmPreCalibration  = "pre_calibration"
	ArmPostCalibration = "post_calibration"
)

// DefaultCalibrationSamples is the sample count per fixture per arm.
//
// JUDGEMENT CALL, not a measured value — the same caveat
// DefaultQualitySamples carries. It is a parameter of RunSeverityArm, never
// a hardcoded gate, so the operator running the live arm can retune it
// against real samples without editing this file.
const DefaultCalibrationSamples = 5

// DefaultCalibrationImprovementThreshold is the signed pre-minus-post mean
// tier-distance delta at or above which CompareSeverityArms reports an
// improvement. Positive means the post-#2119 arm sits CLOSER to the
// operator's label.
//
// JUDGEMENT CALL, not a measured value, exactly as
// DefaultQualityRegressionThreshold is documented to be. 0.25 is a quarter
// of one severity tier: smaller deltas are not distinguishable from
// sampling noise at DefaultCalibrationSamples.
const DefaultCalibrationImprovementThreshold = 0.25

// ---------------------------------------------------------------------------
// The five #2119 treatment literals.
// ---------------------------------------------------------------------------

// These constants are SECOND COPIES of literals owned by
// backend/internal/prompt/prompt.go. They are DRIFT DETECTORS, not silent
// duplicates: StripCalibrationCriteria ERRORS rather than best-efforts when
// a literal it is asked to remove does not byte-match, and
// TestStripCalibrationCriteria_AcceptsRealPromptOutput runs the strip over
// a genuinely built prompt in all four postures. Editing any of these five
// in prompt.go reddens this package until the copy is updated in lockstep.
const (
	// criterion9Probe / criterion9Lead: standing criterion 9 (the baseline
	// check before severity). The criterion renders as ONE line whose second
	// half branches on Trigger.ReviewTreeCommit, so the LEAD SENTENCE is the
	// byte-exact detector and the removal span runs to end of line.
	criterion9Probe = "9. **Baseline check before severity (standing rule)**"
	criterion9Lead  = "9. **Baseline check before severity (standing rule)**: Before assigning a severity to a " +
		"PATTERN-based finding — an unbounded read or decode, a missing cap or limit, an absent guard or check — " +
		"first establish whether sibling or surrounding code already exhibits the same pattern. If it does, say so " +
		"explicitly and calibrate the severity DOWN: report it as pre-existing convention this diff MATCHES, not " +
		"as a regression this diff INTRODUCED. "

	// criterion10Probe / criterion10Lead: standing criterion 10 (trace
	// mechanical predictions). Same one-line, branching-tail shape.
	criterion10Probe = "10. **Trace mechanical predictions (standing rule)**"
	criterion10Lead  = "10. **Trace mechanical predictions (standing rule)**: Any claim about what a specific code " +
		"path, test, or handler WILL DO — a status code returned, an error surfaced, a branch taken — must be " +
		"traced to the actual definitions that govern it: the fake, the override, the wiring, the fixture. NEVER " +
		"infer that behavior from a type or function name; a test fake routinely overrides the base behavior its " +
		"name implies. "

	// carveOutProbe / carveOutBlock: the adversarial-reasoning carve-out that
	// bounds both standing rules. Posture-independent, so the WHOLE block is
	// the byte-exact literal.
	carveOutProbe = "These two standing rules apply to PATTERN-based and MECHANICAL-PREDICTION findings ONLY."
	carveOutBlock = "These two standing rules apply to PATTERN-based and MECHANICAL-PREDICTION findings ONLY. They " +
		"are NOT a requirement to cite a line for every claim. Adversarial reasoning about implications — a threat " +
		"model, a privilege-escalation path, a fail-open, a cross-tenant leak — is a claim about what COULD happen " +
		"and is not citable to a line: do NOT withhold such a finding for want of a citation, and do NOT downgrade " +
		"its severity on that ground.\n"

	// severityRubricProbe / severityRubricBlock: the "### Severity
	// calibration" subsection. Posture-independent (writeSeverityCalibration
	// takes no Trigger), so the WHOLE block is the byte-exact literal.
	severityRubricProbe = "### Severity calibration"
	severityRubricBlock = "### Severity calibration\n\n" +
		"Assign every concern's `severity` from this rubric, and state in the note WHICH tier you " +
		"applied and why — the operator reconciling two reviewers' verdicts must be able to read the basis of a " +
		"disagreement rather than re-derive it:\n\n" +
		"- `high`: the defect is REACHABLE in a supported configuration.\n" +
		"- `medium`: reaching it requires a misconfiguration or an unusual wiring.\n" +
		"- `low`: defense-in-depth hardening, documentation accuracy, or test hardening, with no " +
		"reachable defect behind it.\n\n" +
		"A standing-rule-9 baseline finding — a pattern already present in sibling or surrounding code, " +
		"which this diff merely matches — is a `low`, not a `high`.\n\n"

	// reReadProbe / reReadBlock: the #2119 re-read-before-reopen bullet. This
	// is the GUARD-CONDITIONAL literal: it renders ONLY inside the
	// `if len(t.PriorConcerns) > 0` guard of the "Prior concerns (delta
	// verification)" section, in a completely different part of the prompt
	// from the other four.
	reReadProbe = "- Before emitting a `reopened` resolution, READ the CURRENT diff state"
	reReadBlock = "- Before emitting a `reopened` resolution, READ the CURRENT diff state for that " +
		"concern's subject. If a prior resolution or the fix-up claims the fix landed, either CONFIRM it " +
		"against the diff in front of you or state SPECIFICALLY what remains missing and where. Reopening " +
		"on prior-round reasoning — restating the round-N finding without checking the round-N+1 diff — is " +
		"a defect in the review; on the delta path the diff shown is exactly the fix-up change the " +
		"resolution refers to.\n"
)

// treatmentLiteral is one member of the #2119 treatment set.
//
// Probe is a short marker whose PRESENCE means the block rendered at all;
// Anchor is the byte-exact literal the strip removes. Probe-present with
// Anchor-absent is PARTIAL DRIFT (mode (e)) — the block is there but its
// body is not the body this transform was written against. Probe-absent is
// a plain absence (modes (a)-(d), (g)).
type treatmentLiteral struct {
	// Name identifies the literal in error messages and in the README's
	// treatment-set enumeration.
	Name string
	// Probe is the presence marker.
	Probe string
	// Anchor is the byte-exact literal.
	Anchor string
	// WholeBlock is true when Anchor spans the entire removal region. When
	// false the removal runs from Anchor's start to the end of its line
	// (inclusive), because the block's tail branches on render posture and
	// only its lead sentence is byte-stable.
	WholeBlock bool
	// GuardConditional is true for the one literal that renders only when
	// the trigger carries prior concerns.
	GuardConditional bool
}

// treatmentLiterals is THE enumeration of the #2119 treatment set: exactly
// FIVE members. The count and the enumeration cannot disagree because there
// is only one list — TestTreatmentLiteralCount_IsFiveAndEnumerated pins the
// count, and the strip consults this table and nothing else.
var treatmentLiterals = []treatmentLiteral{
	{Name: "standing-criterion-9-lead", Probe: criterion9Probe, Anchor: criterion9Lead},
	{Name: "standing-criterion-10-lead", Probe: criterion10Probe, Anchor: criterion10Lead},
	{Name: "adversarial-carve-out", Probe: carveOutProbe, Anchor: carveOutBlock, WholeBlock: true},
	{Name: "severity-calibration-rubric", Probe: severityRubricProbe, Anchor: severityRubricBlock, WholeBlock: true},
	{Name: "re-read-before-reopen-bullet", Probe: reReadProbe, Anchor: reReadBlock, WholeBlock: true, GuardConditional: true},
}

// TreatmentLiteralNames returns the names of the #2119 treatment set, in
// table order. Exported so a sibling harness and a documentation check can
// enumerate the set without reaching into the unexported table.
func TreatmentLiteralNames() []string {
	out := make([]string, 0, len(treatmentLiterals))
	for _, l := range treatmentLiterals {
		out = append(out, l.Name)
	}
	return out
}

// StripCalibrationCriteria removes the five #2119 treatment literals from a
// rendered implement-review prompt and returns the PRE-#2119 arm.
//
// expectPriorConcerns declares the render posture: true when the trigger
// that produced p carried prior concerns (so the guard-conditional re-read
// bullet MUST be present), false when it did not (so the bullet MUST be
// absent). The caller owns the trigger and therefore owns this fact;
// inferring it from the prompt text would make the guard tautological.
//
// EIGHT named fail-closed modes, each an error rather than a best-effort
// strip — a partially-stripped arm would silently confound the measurement
// it feeds:
//
//	(a) standing criterion 9 absent
//	(b) standing criterion 10 absent
//	(c) the adversarial carve-out sentence absent
//	(d) the "### Severity calibration" block absent
//	(e) PARTIAL DRIFT: any of the five blocks present (its probe matches)
//	    but its body does not byte-match the expected literal
//	(f) criterion 10 occurring BEFORE criterion 9 (out-of-order render)
//	(g) the re-read bullet absent while expectPriorConcerns is true
//	(h) the re-read bullet present while expectPriorConcerns is false
//
// Modes (g) and (h) are the POSTURE GUARD, and both directions matter. (g)
// catches the bullet silently ceasing to render — the pre arm would then be
// stripped of four literals and labelled as if it were stripped of five.
// (h) catches the bullet rendering where the posture model says it cannot,
// which would mean the model is wrong and every no-prior-concerns pre arm
// has been carrying #2119 treatment.
func StripCalibrationCriteria(p string, expectPriorConcerns bool) (string, error) {
	type span struct{ start, end int }
	var spans []span

	for _, l := range treatmentLiterals {
		iProbe := strings.Index(p, l.Probe)
		if l.GuardConditional {
			switch {
			case expectPriorConcerns && iProbe < 0:
				// (g)
				return "", fmt.Errorf("agenteval: strip calibration criteria: treatment literal %q is ABSENT but the trigger carried prior concerns, so it must render (prompt.go guard drift)", l.Name)
			case !expectPriorConcerns && iProbe >= 0:
				// (h)
				return "", fmt.Errorf("agenteval: strip calibration criteria: treatment literal %q is PRESENT but the trigger carried no prior concerns, so it must not render (the posture model is wrong)", l.Name)
			case !expectPriorConcerns:
				continue
			}
		}
		if iProbe < 0 {
			// (a) / (b) / (c) / (d)
			return "", fmt.Errorf("agenteval: strip calibration criteria: treatment literal %q is absent from the rendered prompt", l.Name)
		}
		iAnchor := strings.Index(p, l.Anchor)
		if iAnchor < 0 {
			// (e) PARTIAL DRIFT.
			return "", fmt.Errorf("agenteval: strip calibration criteria: treatment literal %q is present but its body does not match the expected literal (prompt.go drift — update the copy in severitycalibration.go)", l.Name)
		}
		end := iAnchor + len(l.Anchor)
		if !l.WholeBlock {
			if nl := strings.IndexByte(p[end:], '\n'); nl >= 0 {
				end += nl + 1
			} else {
				end = len(p)
			}
		}
		spans = append(spans, span{start: iAnchor, end: end})
	}

	// (f) Out-of-order render. Checked on the ORIGINAL string, before any
	// splice, so the indices are the ones the prompt actually produced.
	if i9, i10 := strings.Index(p, criterion9Probe), strings.Index(p, criterion10Probe); i10 < i9 {
		return "", fmt.Errorf("agenteval: strip calibration criteria: standing criterion 10 occurs before standing criterion 9 (prompt.go render order drift)")
	}

	// Splice descending so each removal leaves the earlier indices valid.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := p
	for _, s := range spans {
		out = out[:s.start] + out[s.end:]
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The labelled corpus.
// ---------------------------------------------------------------------------

// SeverityTiers is the ordinal severity scale. high=3 / medium=2 / low=1.
//
// JUDGEMENT CALL: the tiers are equally spaced by fiat, so a
// high-versus-low disagreement is scored as exactly twice a
// high-versus-medium one. Nothing measured says that is the right ratio.
var SeverityTiers = map[string]int{"low": 1, "medium": 2, "high": 3}

// MaxSeverityTierDistance is the largest distance the scale admits (high
// against low). It is also the MISS PENALTY — see CompareSeverityArms.
const MaxSeverityTierDistance = 2

// SeverityTier maps a severity string to its ordinal tier with an EXPLICIT
// found flag. An unknown string returns (0, false) and callers must refuse
// it — never a zero tier, which would read as the strongest possible
// agreement and fail OPEN. Same fail-closed rule RubricCard.Score applies.
func SeverityTier(s string) (int, bool) {
	t, ok := SeverityTiers[s]
	return t, ok
}

// CalibrationDispositions is the closed set of operator dispositions a
// labelled concern may carry.
//
// Every member is a real concern.State (backend/internal/concern/concern.go
// declares StateWaived, StateDeferred, StateAddressed, StateSuperseded and
// StateAddressedByCondition). Two of them — `addressed` and `superseded` —
// have no dedicated audit CATEGORY and therefore no distill producer, so a
// case labelled with either is OPERATOR-CURATED rather than tool-scaffolded.
var CalibrationDispositions = []string{"waived", "deferred", "addressed", "addressed_by_condition", "superseded"}

// CalibrationPriorConcernStates is the closed set of lifecycle states a
// fixture's prior_concerns entry may declare. Mirrors concern.State.
var CalibrationPriorConcernStates = []string{
	"raised", "addressed_pending", "addressed", "reopened",
	"waived", "superseded", "deferred", "addressed_by_condition",
}

// PriorConcernFixture is one corpus-declared prior concern. It mirrors
// prompt.PriorConcern; the corpus carries its own type so a case.json is a
// stable committed artifact rather than a mirror of a production struct.
//
// A fixture carrying a non-empty prior_concerns array is what populates the
// `len(t.PriorConcerns) > 0` guard the fifth treatment literal renders
// under, so at least one committed case must carry one.
type PriorConcernFixture struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Severity    string `json:"severity,omitempty"`
	Category    string `json:"category,omitempty"`
	Note        string `json:"note,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
}

// LabelledConcern is one reviewer concern paired with the severity the
// OPERATOR assigned when dispositioning it. OperatorSeverity is the LABEL —
// it is what makes this corpus labelled, so a case carrying an empty one
// does not load.
type LabelledConcern struct {
	ConcernID         string `json:"concern_id"`
	ReviewerModel     string `json:"reviewer_model,omitempty"`
	Severity          string `json:"severity"`
	Category          string `json:"category"`
	Note              string `json:"note"`
	Disposition       string `json:"disposition"`
	DispositionReason string `json:"disposition_reason,omitempty"`
	OperatorSeverity  string `json:"operator_severity"`
}

// SeverityCalibrationCase is one committed labelled fixture.
type SeverityCalibrationCase struct {
	Name          string                `json:"name"`
	RunID         string                `json:"run_id,omitempty"`
	StageID       string                `json:"stage_id,omitempty"`
	IssueNumber   int                   `json:"issue_number,omitempty"`
	Diff          string                `json:"diff"`
	PlanSummary   string                `json:"plan_summary,omitempty"`
	ScopeFiles    []string              `json:"scope_files,omitempty"`
	PriorConcerns []PriorConcernFixture `json:"prior_concerns,omitempty"`
	Concerns      []LabelledConcern     `json:"concerns"`
	// Synthetic marks a HAND-AUTHORED fixture. The committed seed cases all
	// set it true so no reader mistakes them for distilled production cases.
	Synthetic bool `json:"synthetic"`
}

// NamedSeverityCalibrationCase pairs a loaded case with its directory name.
type NamedSeverityCalibrationCase struct {
	Name string
	Case SeverityCalibrationCase
}

// LoadSeverityCalibrationCorpus walks dir/<case>/case.json in directory
// order. Like LoadInjectionCorpus and LoadEnvelopeQualityCorpus — and
// unlike LoadPlanReviewMissCorpus — an ABSENT DIR IS AN ERROR: this corpus
// is committed, so not finding it means the measurement is silently not
// running.
//
// TEN named shape modes:
//
//	(a) absent corpus dir, or a case directory with no readable case.json
//	(b) malformed JSON
//	(c) empty diff
//	(d) zero concerns
//	(e) a concern with an empty concern_id
//	(f) an empty or unrecognised severity
//	(g) an empty or unrecognised operator_severity — the LABEL
//	(h) an unrecognised disposition
//	(i) an empty concern note
//	(j) a prior_concerns entry with an empty id or an unrecognised state
func LoadSeverityCalibrationCorpus(dir string) ([]NamedSeverityCalibrationCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("agenteval: read severity-calibration corpus dir %q: %w", dir, err) // (a)
	}
	var out []NamedSeverityCalibrationCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "case.json"))
		if err != nil {
			return nil, fmt.Errorf("agenteval: severity-calibration case %q: read case.json: %w", e.Name(), err) // (a)
		}
		var c SeverityCalibrationCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("agenteval: severity-calibration case %q: parse case.json: %w", e.Name(), err) // (b)
		}
		if err := c.validate(e.Name()); err != nil {
			return nil, err
		}
		out = append(out, NamedSeverityCalibrationCase{Name: e.Name(), Case: c})
	}
	return out, nil
}

func (c *SeverityCalibrationCase) validate(name string) error {
	if strings.TrimSpace(c.Diff) == "" {
		return fmt.Errorf("agenteval: severity-calibration case %q: diff must be non-empty", name) // (c)
	}
	if len(c.Concerns) == 0 {
		return fmt.Errorf("agenteval: severity-calibration case %q: concerns must be non-empty", name) // (d)
	}
	seen := make(map[string]bool, len(c.Concerns))
	for i, cc := range c.Concerns {
		if strings.TrimSpace(cc.ConcernID) == "" {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern %d: concern_id must be non-empty", name, i) // (e)
		}
		if seen[cc.ConcernID] {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern_id %q is declared twice", name, cc.ConcernID)
		}
		seen[cc.ConcernID] = true
		if _, ok := SeverityTier(cc.Severity); !ok {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern %q: severity %q is not one of high|medium|low", name, cc.ConcernID, cc.Severity) // (f)
		}
		if _, ok := SeverityTier(cc.OperatorSeverity); !ok {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern %q: operator_severity %q is not one of high|medium|low — an unlabelled candidate is not a corpus case", name, cc.ConcernID, cc.OperatorSeverity) // (g)
		}
		if !contains(CalibrationDispositions, cc.Disposition) {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern %q: disposition %q is not one of %s", name, cc.ConcernID, cc.Disposition, strings.Join(CalibrationDispositions, "|")) // (h)
		}
		if strings.TrimSpace(cc.Note) == "" {
			return fmt.Errorf("agenteval: severity-calibration case %q: concern %q: note must be non-empty", name, cc.ConcernID) // (i)
		}
	}
	for i, pc := range c.PriorConcerns {
		if strings.TrimSpace(pc.ID) == "" {
			return fmt.Errorf("agenteval: severity-calibration case %q: prior_concerns[%d]: id must be non-empty", name, i) // (j)
		}
		if !contains(CalibrationPriorConcernStates, pc.State) {
			return fmt.Errorf("agenteval: severity-calibration case %q: prior_concerns[%d]: state %q is not a known concern state", name, i, pc.State) // (j)
		}
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Arm rendering.
// ---------------------------------------------------------------------------

// ToPromptPriorConcerns projects corpus prior concerns onto the production
// type the prompt builder consumes.
func ToPromptPriorConcerns(in []PriorConcernFixture) []prompt.PriorConcern {
	if len(in) == 0 {
		return nil
	}
	out := make([]prompt.PriorConcern, 0, len(in))
	for _, p := range in {
		out = append(out, prompt.PriorConcern{
			ID:          p.ID,
			State:       p.State,
			Severity:    p.Severity,
			Category:    p.Category,
			Note:        p.Note,
			StateReason: p.StateReason,
		})
	}
	return out
}

// ToCalibrationTrigger builds the prompt.Trigger for a labelled fixture.
// The trigger's PriorConcerns carry the case's posture, which is what
// decides whether the guard-conditional fifth treatment literal renders.
func ToCalibrationTrigger(c SeverityCalibrationCase) prompt.Trigger {
	issue := c.IssueNumber
	if issue == 0 {
		issue = 3309
	}
	files := make([]plan.ScopeFile, 0, len(c.ScopeFiles))
	for _, f := range c.ScopeFiles {
		files = append(files, plan.ScopeFile{Path: f, Operation: plan.FileOperation("modify")})
	}
	return prompt.Trigger{
		Source:      "github_issue",
		IssueNumber: issue,
		IssueTitle:  c.Name,
		IssueURL:    fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/issues/%d", issue),
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

// CalibrationArmPrompt renders one fixture's implement-review prompt for
// one arm. Both arms start from the SAME prompt.Build output, so they
// differ ONLY in the five #2119 treatment literals.
func CalibrationArmPrompt(c SeverityCalibrationCase, arm string) (string, error) {
	built, err := prompt.Build("implement_review", ToCalibrationTrigger(c))
	if err != nil {
		return "", fmt.Errorf("agenteval: build implement_review prompt for %q: %w", c.Name, err)
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

// calibrationGeneratorSystemPrompt is the fixed harness instruction handed
// to the GENERATOR for both arms. It names the reviewer verdict JSON
// contract so the emitted verdict is comparable across arms.
const calibrationGeneratorSystemPrompt = `You are the Fishhawk implement-review agent. Read the stage prompt the user message contains and produce the review verdict it asks for as a JSON object carrying verdict, concerns (each with severity, category and note), and optional free_form. Respond with the verdict JSON only.`

// ---------------------------------------------------------------------------
// Running an arm.
// ---------------------------------------------------------------------------

// emittedVerdict is the harness-side decode of a reviewer verdict. It is
// deliberately a LOCAL wire shape, not a production struct: this decodes
// model output, and a strict production decoder would reject a verdict the
// measurement should still be able to read.
type emittedVerdict struct {
	Verdict  string `json:"verdict"`
	Concerns []struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Note     string `json:"note"`
	} `json:"concerns"`
}

// ArmCoverage is one arm's result for one case. It is deliberately NOT a
// bare mean: the per-arm mean is not the comparison unit, because an
// omission asymmetry buried in a mean is invisible.
type ArmCoverage struct {
	// LabelledConcernIDs is the FULL labelled population for this case, in
	// corpus order. It is the denominator — see CompareSeverityArms.
	LabelledConcernIDs []string
	// Distances maps a labelled concern_id to this arm's mean tier distance
	// from the operator's label, over the samples in which it was matched.
	// A concern absent from this map was MISSED in every sample.
	Distances map[string]float64
	// MissedConcernIDs are labelled concerns this arm never emitted, in
	// corpus order.
	MissedConcernIDs []string
	// UnmatchedEmitted counts emitted concerns that matched no labelled
	// concern, summed over samples. Reported, never scored: a novel finding
	// is not a calibration error.
	UnmatchedEmitted int
}

// Labelled returns the size of the labelled population.
func (c ArmCoverage) Labelled() int { return len(c.LabelledConcernIDs) }

// Matched returns the count of labelled concerns this arm emitted at least
// once.
func (c ArmCoverage) Matched() int { return len(c.LabelledConcernIDs) - len(c.MissedConcernIDs) }

// SeverityArmReport is one arm's aggregated result.
type SeverityArmReport struct {
	Arm     string
	Samples int
	// PerCase maps case name to that case's coverage.
	PerCase map[string]ArmCoverage
}

// RunSeverityArm drives the full render-generate-match path for one arm.
//
// Fail-closed: any render or generate error aborts and returns the zero
// report with a non-nil error. A partial arm would be a silently-biased
// comparison. A verdict that does not decode is likewise an error, not a
// zero-concern sample: silently reading it as "the reviewer raised nothing"
// would fabricate misses.
func RunSeverityArm(ctx context.Context, sender MessageSender, cases []NamedSeverityCalibrationCase, arm string, samples int) (SeverityArmReport, error) {
	if len(cases) == 0 {
		return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q: no fixtures", arm)
	}
	if samples < 1 {
		return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q: samples must be >= 1, got %d", arm, samples)
	}
	if sender == nil {
		return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q: sender is required", arm)
	}

	report := SeverityArmReport{Arm: arm, Samples: samples, PerCase: make(map[string]ArmCoverage, len(cases))}
	for _, nc := range cases {
		promptText, err := CalibrationArmPrompt(nc.Case, arm)
		if err != nil {
			return SeverityArmReport{}, err
		}
		cov := ArmCoverage{Distances: make(map[string]float64, len(nc.Case.Concerns))}
		for _, lc := range nc.Case.Concerns {
			cov.LabelledConcernIDs = append(cov.LabelledConcernIDs, lc.ConcernID)
		}
		distanceSum := make(map[string]float64, len(nc.Case.Concerns))
		matchedSamples := make(map[string]int, len(nc.Case.Concerns))

		for i := 0; i < samples; i++ {
			responseText, _, _, _, _, _, err := sender.Messages(ctx, calibrationGeneratorSystemPrompt, promptText)
			if err != nil {
				return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q case %q sample %d: generate: %w", arm, nc.Name, i+1, err)
			}
			var v emittedVerdict
			if err := json.Unmarshal([]byte(extractJSONObject(responseText)), &v); err != nil {
				return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q case %q sample %d: decode verdict: %w", arm, nc.Name, i+1, err)
			}
			taken := make(map[string]bool, len(nc.Case.Concerns))
			for _, ec := range v.Concerns {
				emitted, ok := SeverityTier(ec.Severity)
				if !ok {
					return SeverityArmReport{}, fmt.Errorf("agenteval: severity arm %q case %q sample %d: emitted severity %q is not one of high|medium|low", arm, nc.Name, i+1, ec.Severity)
				}
				id, ok := matchLabelledConcern(nc.Case.Concerns, ec.Category, ec.Note, taken)
				if !ok {
					cov.UnmatchedEmitted++
					continue
				}
				taken[id] = true
				label, _ := SeverityTier(labelledSeverity(nc.Case.Concerns, id))
				distanceSum[id] += float64(abs(emitted - label))
				matchedSamples[id]++
			}
		}

		for _, id := range cov.LabelledConcernIDs {
			if matchedSamples[id] == 0 {
				cov.MissedConcernIDs = append(cov.MissedConcernIDs, id)
				continue
			}
			cov.Distances[id] = distanceSum[id] / float64(matchedSamples[id])
		}
		report.PerCase[nc.Name] = cov
	}
	return report, nil
}

func labelledSeverity(cs []LabelledConcern, id string) string {
	for _, c := range cs {
		if c.ConcernID == id {
			return c.OperatorSeverity
		}
	}
	return ""
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// matchLabelledConcern maps an EMITTED concern back to a labelled one.
//
// HEURISTIC, and stated as one: a re-run reviewer writes its own prose, so
// the match is category equality plus the strongest note-word overlap above
// a minimum. Under the miss-penalty scoring rule a matching failure cannot
// manufacture a delta in either direction — it becomes a MISS, which scores
// at the maximum tier distance in that arm and is reported by concern_id.
// A weak heuristic therefore costs the measurement POWER, visibly, rather
// than correctness, silently.
func matchLabelledConcern(cs []LabelledConcern, category, note string, taken map[string]bool) (string, bool) {
	want := tokenize(note)
	best, bestScore := "", 0
	for _, c := range cs {
		if taken[c.ConcernID] {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(c.Category), strings.TrimSpace(category)) {
			continue
		}
		score := overlap(want, tokenize(c.Note))
		if score > bestScore {
			best, bestScore = c.ConcernID, score
		}
	}
	// At least two shared substantive words, so a category match alone does
	// not bind an emitted concern to an unrelated labelled one.
	if bestScore < 2 {
		return "", false
	}
	return best, true
}

func tokenize(s string) map[string]bool {
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(f) > 3 {
			out[f] = true
		}
	}
	return out
}

func overlap(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------

// CaseComparison is one case's pre-versus-post result.
type CaseComparison struct {
	Case string
	// PreScore / PostScore are mean tier distances over the FULL labelled
	// population — misses included at MaxSeverityTierDistance.
	PreScore  float64
	PostScore float64
	// Delta is PreScore - PostScore. POSITIVE means the post-#2119 arm sits
	// CLOSER to the operator's label.
	Delta float64
	// Scored is false when the case contributed no score.
	Scored bool
	// SkipReason names why an unscored case was skipped.
	SkipReason string
	// PreMissed / PostMissed are the labelled concern_ids each arm never
	// emitted. Reported alongside every score so an omission asymmetry is
	// visible rather than buried in a mean.
	PreMissed  []string
	PostMissed []string
}

// SeverityComparison is the whole-corpus comparison.
type SeverityComparison struct {
	PerCase []CaseComparison
	// Overall is the unweighted mean Delta over SCORED cases, so no case
	// dominates regardless of how many concerns it carries.
	Overall float64
	// ScoredCases / SkippedCases partition PerCase.
	ScoredCases  int
	SkippedCases int
	Threshold    float64
	// Improved is Overall >= Threshold.
	Improved bool
}

// CompareSeverityArms scores both arms over the LABELLED population and
// returns the signed pre-minus-post delta.
//
// OMISSION-MONOTONE BY CONSTRUCTION — this is the load-bearing property,
// and the mechanism is an EXPLICIT MISS PENALTY.
//
// An arm's score for a case is the mean tier distance over EVERY labelled
// concern in that case. A concern the arm emitted contributes its measured
// distance; a concern the arm MISSED contributes MaxSeverityTierDistance —
// the largest distance the scale admits. The denominator is the labelled
// count and is identical in both arms and independent of what either arm
// emitted.
//
// Therefore omitting a labelled concern can only ever make an arm's own
// score WORSE (weakly), and cannot change the other arm's score at all: the
// penalty is >= any distance an emitted answer could score, so replacing a
// measured distance with a miss never lowers the sum. An arm cannot improve
// its score, or its delta against the other arm, by omitting anything.
//
// WHY NOT PAIRWISE-COMPLETE (excluding a concern from BOTH arms whenever
// EITHER missed it): it is not omission-monotone. Two labelled concerns A
// and B with pre/post distances 1/2 and 2/1 score 1.5 against 1.5 — delta
// zero. Let the post arm omit A; pairwise-complete drops A from both arms
// and leaves B alone, giving pre 2 against post 1 and a reported +1
// IMPROVEMENT produced entirely by an omission, with no severity having
// moved. Dropping a concern on which the post arm did WORSE flatters the
// remainder. Reporting the omission makes it visible but does not stop the
// number being wrong. Under the miss penalty the same case scores 1.5
// against (2 + 1)/2 = 1.5 — delta zero, no improvement.
//
// The penalty VALUE is a JUDGEMENT CALL and is stated as one: pinning it at
// MaxSeverityTierDistance is the smallest value that guarantees
// monotonicity (any smaller value could be beaten by an emitted answer, and
// omitting that answer would then improve the score). It also means a
// heavily-omitting arm's score is dominated by penalties rather than by
// measured severities, which is why PreMissed / PostMissed are reported per
// case: a delta computed mostly from penalties measures coverage, not
// calibration, and the reader must be able to see that.
//
// A case whose labelled population is EMPTY contributes NO score and is
// reported with SkipReason "no_labelled_concerns" — never as a zero
// distance, which would read as perfect agreement and fail OPEN.
//
// FAILS CLOSED on a case name present in one arm and absent from the other,
// in EITHER direction: indexing an absent key would score that case against
// a coverage no arm produced.
func CompareSeverityArms(pre, post SeverityArmReport, threshold float64) (SeverityComparison, error) {
	for name := range post.PerCase {
		if _, ok := pre.PerCase[name]; !ok {
			return SeverityComparison{}, fmt.Errorf("agenteval: case %q is present in the post-calibration arm but absent from the pre-calibration arm; the two arms are not comparable", name)
		}
	}
	names := make([]string, 0, len(pre.PerCase))
	for name := range pre.PerCase {
		names = append(names, name)
	}
	sort.Strings(names)

	out := SeverityComparison{Threshold: threshold}
	deltaTotal := 0.0
	for _, name := range names {
		preCov := pre.PerCase[name]
		postCov, ok := post.PerCase[name]
		if !ok {
			return SeverityComparison{}, fmt.Errorf("agenteval: case %q is present in the pre-calibration arm but absent from the post-calibration arm; the two arms are not comparable", name)
		}
		cc := CaseComparison{Case: name, PreMissed: preCov.MissedConcernIDs, PostMissed: postCov.MissedConcernIDs}
		if preCov.Labelled() == 0 || postCov.Labelled() == 0 {
			cc.SkipReason = "no_labelled_concerns"
			out.SkippedCases++
			out.PerCase = append(out.PerCase, cc)
			continue
		}
		cc.PreScore = penalizedMeanDistance(preCov)
		cc.PostScore = penalizedMeanDistance(postCov)
		cc.Delta = cc.PreScore - cc.PostScore
		cc.Scored = true
		deltaTotal += cc.Delta
		out.ScoredCases++
		out.PerCase = append(out.PerCase, cc)
	}
	if out.ScoredCases > 0 {
		out.Overall = deltaTotal / float64(out.ScoredCases)
		out.Improved = out.Overall >= threshold
	}
	return out, nil
}

// penalizedMeanDistance is the OMISSION-MONOTONE score: the mean tier
// distance over the FULL labelled population, with a missed concern scored
// at MaxSeverityTierDistance rather than dropped.
//
// Dropping misses instead (a mean over each arm's own matched set) is
// exactly the defect this function exists to prevent — see
// CompareSeverityArms.
func penalizedMeanDistance(c ArmCoverage) float64 {
	total := 0.0
	for _, id := range c.LabelledConcernIDs {
		d, ok := c.Distances[id]
		if !ok {
			total += MaxSeverityTierDistance
			continue
		}
		total += d
	}
	return total / float64(len(c.LabelledConcernIDs))
}

// Render returns an operator-readable summary. The header states the
// scoring rule in words, because a mean tier distance read without knowing
// misses are penalized is a different number than it appears to be.
func (s SeverityComparison) Render() string {
	var b strings.Builder
	b.WriteString("severity calibration: pre-#2119 vs post-#2119\n")
	b.WriteString("score = mean tier distance from the operator label over the FULL labelled population;\n")
	fmt.Fprintf(&b, "a MISSED labelled concern is penalized at the maximum distance (%d), never dropped,\n", MaxSeverityTierDistance)
	b.WriteString("so omitting a concern can never improve an arm's score. positive delta = post arm is closer.\n\n")
	for _, c := range s.PerCase {
		if !c.Scored {
			fmt.Fprintf(&b, "  %-40s SKIPPED (%s)\n", c.Case, c.SkipReason)
			continue
		}
		fmt.Fprintf(&b, "  %-40s pre=%.2f post=%.2f delta=%+.2f pre_missed=%v post_missed=%v\n",
			c.Case, c.PreScore, c.PostScore, c.Delta, c.PreMissed, c.PostMissed)
	}
	fmt.Fprintf(&b, "\n  scored=%d skipped=%d overall_delta=%+.2f threshold=%+.2f improved=%t\n",
		s.ScoredCases, s.SkippedCases, s.Overall, s.Threshold, s.Improved)
	return b.String()
}
