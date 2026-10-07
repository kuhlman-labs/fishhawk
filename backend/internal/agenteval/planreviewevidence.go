// Plan-review catch-rate EVIDENCE (E55.4 / #2245): the committed record of a
// live two-arm measurement, and the offline check that decides from it.
//
// planreviewcatch.go owns the measurement (corpus, arms, ClassifyCatch,
// CompareCatchRateArms). This file owns what survives a measurement: a
// committed JSON record (schema CatchRateEvidenceSchema) carrying both arms'
// per-case counts, the tolerance and power floor, the generator model, a
// SHA-256 fingerprint of everything the model was shown, and a PINNED
// BASELINE.
//
// THE BASELINE IS PINNED, NOT ROLLING. Every recording is judged against two
// references: its own same-run without-conventions arm (the within-run rule)
// AND the pinned baseline — never only against the immediately preceding
// recording, which would let a series of individually-tolerable drops erode
// the reference without bound. RecordCatchRateEvidence REFUSES to write a
// measurement that fails either rule, so a failed run can never become a
// baseline. Moving the baseline is a separate, explicit operator action
// (RecordCatchRateOptions.PinBaseline) that still requires a passing
// measurement and records a reason and timestamp in the evidence.
//
// CheckCatchRateEvidence RECOMPUTES every verdict from the stored counts: the
// record carries no verdict field to trust.

package agenteval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// CatchRateEvidenceSchema is the evidence record's schema identifier.
const CatchRateEvidenceSchema = "planreview-catchrate-evidence-v1"

// catchRateRunbook is named in every refusal so the reader knows the remedy.
const catchRateRunbook = "docs/compliance/planreview-catchrate-evidence.md"

// ErrCatchRateRegressed marks a refusal whose cause is the REGRESSION rule
// (the within-run rule or the pinned-baseline rule), as opposed to a shape,
// staleness or availability failure. Tests use it to prove a fixture fails
// for the regression reason, not a shape reason.
var ErrCatchRateRegressed = errors.New("plan-review catch rate regressed")

// CatchRateEvidenceArm is one arm's per-case counts.
type CatchRateEvidenceArm struct {
	PerCase map[string]CaseCatchCounts `json:"per_case"`
}

// CatchRateEvidenceArms is both arms of one measurement.
type CatchRateEvidenceArms struct {
	WithoutConventions CatchRateEvidenceArm `json:"without_conventions"`
	WithConventions    CatchRateEvidenceArm `json:"with_conventions"`
}

// CatchRateBaseline is the PINNED reference measurement. It is set only by an
// explicit pin (RecordCatchRateOptions.PinBaseline) and carried forward
// byte-for-byte by every ordinary recording.
type CatchRateBaseline struct {
	// PinnedAt is the RFC 3339 time the operator pinned this baseline.
	PinnedAt string `json:"pinned_at"`
	// Reason is the operator's stated reason for pinning (never empty).
	Reason            string                `json:"reason"`
	GeneratorModel    string                `json:"generator_model"`
	PromptFingerprint string                `json:"prompt_fingerprint"`
	SamplesPerCase    int                   `json:"samples_per_case"`
	Arms              CatchRateEvidenceArms `json:"arms"`
}

// CatchRateEvidence is the committed evidence record
// (testdata/planreview-catchrate/evidence.json).
type CatchRateEvidence struct {
	Schema            string                `json:"schema"`
	RecordedAt        string                `json:"recorded_at"`
	GeneratorModel    string                `json:"generator_model"`
	PromptFingerprint string                `json:"prompt_fingerprint"`
	Tolerance         float64               `json:"tolerance"`
	MinTrialsPerArm   int                   `json:"min_trials_per_arm"`
	SamplesPerCase    int                   `json:"samples_per_case"`
	Arms              CatchRateEvidenceArms `json:"arms"`
	// Baseline is the pinned reference; absent or null is refused by
	// CheckCatchRateEvidence.
	Baseline *CatchRateBaseline `json:"baseline"`
}

// CatchRatePromptFingerprint is "sha256:<hex>" over a length-prefixed
// canonical encoding of: the schema id, the generator model, the tolerance,
// the power floor, catchRuleVersion, the generator system prompt, and — per
// case in name order — the case name, its exact review_input.json bytes and
// BOTH rendered arm prompts (so the conventions file and every prompt.Build
// change are covered). It does NOT cover the Go source of ClassifyCatch; a
// matcher change must bump catchRuleVersion.
func CatchRatePromptFingerprint(cases []PlanReviewCatchCase, conv prompt.ReviewConvention, generatorModel string) (string, error) {
	h := sha256.New()
	put := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	put([]byte(CatchRateEvidenceSchema))
	put([]byte(generatorModel))
	put([]byte(strconv.FormatFloat(DefaultCatchRateRegressionTolerance, 'g', -1, 64)))
	put([]byte(strconv.Itoa(MinCatchRateTrialsPerArm)))
	put([]byte(catchRuleVersion))
	put([]byte(planReviewCatchGeneratorSystemPrompt))
	sorted := append([]PlanReviewCatchCase(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		put([]byte(c.Name))
		put(c.ReviewInputRaw)
		for _, arm := range []string{ArmWithoutConventions, ArmWithConventions} {
			p, err := CatchRateArmPrompt(c, conv, arm)
			if err != nil {
				return "", err
			}
			put([]byte(p))
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// RecordCatchRateOptions selects how RecordCatchRateEvidence treats the
// measurement it takes.
type RecordCatchRateOptions struct {
	// PinBaseline makes this measurement the new pinned baseline — the
	// explicit operator action that moves the reference. It still requires a
	// passing measurement (the within-run rule, and the CURRENT baseline when
	// that baseline is comparable). The first recording must pin.
	PinBaseline bool
	// Reason is recorded on the baseline; required (non-blank) with
	// PinBaseline.
	Reason string
	// DryRun measures and judges but never writes the evidence file.
	DryRun bool
}

// RecordCatchRateEvidence runs both arms against sender (SamplesPerCaseForPower
// samples per case), judges the measurement, and — only when it passes —
// writes the evidence record atomically. It returns the rendered report (also
// on a gate refusal, so the caller can show the numbers) and an error.
//
// It REFUSES, leaving evidencePath byte-identical, on: PinBaseline with a
// blank Reason; an existing record that is malformed (never silently
// dropped); no pinned baseline yet without PinBaseline (the first recording
// must pin — checked BEFORE any model call); a transport or render error; a
// within-run regression; a regression of either arm against the pinned
// baseline; and, without PinBaseline, a baseline that is not comparable to
// this measurement (corpus, model or samples changed — re-pin).
func RecordCatchRateEvidence(ctx context.Context, sender MessageSender, generatorModel, corpusDir, conventionsPath, evidencePath string, now time.Time, opts RecordCatchRateOptions) (string, error) {
	if opts.PinBaseline && strings.TrimSpace(opts.Reason) == "" {
		return "", fmt.Errorf("agenteval: pinning a catch-rate baseline requires a reason (it is recorded in the evidence); see %s", catchRateRunbook)
	}
	if strings.TrimSpace(generatorModel) == "" {
		return "", fmt.Errorf("agenteval: catch-rate recording requires a generator model")
	}
	cases, err := LoadPlanReviewCatchCorpus(corpusDir)
	if err != nil {
		return "", err
	}
	conv, err := LoadRepresentativeConventions(conventionsPath)
	if err != nil {
		return "", err
	}
	existing, err := readExistingCatchRateEvidence(evidencePath)
	if err != nil {
		return "", err
	}
	var ref *CatchRateBaseline
	if existing != nil {
		ref = existing.Baseline
	}
	if ref == nil && !opts.PinBaseline && !opts.DryRun {
		return "", fmt.Errorf("agenteval: no pinned catch-rate baseline exists in %q: the first recording must pin one (PinBaseline with a reason); see %s", evidencePath, catchRateRunbook)
	}
	fingerprint, err := CatchRatePromptFingerprint(cases, conv, generatorModel)
	if err != nil {
		return "", err
	}
	samples := SamplesPerCaseForPower(len(cases))
	without, err := RunCatchRateArm(ctx, sender, cases, conv, ArmWithoutConventions, samples)
	if err != nil {
		return "", err
	}
	with, err := RunCatchRateArm(ctx, sender, cases, conv, ArmWithConventions, samples)
	if err != nil {
		return "", err
	}
	rec := CatchRateEvidence{
		Schema:            CatchRateEvidenceSchema,
		RecordedAt:        now.UTC().Format(time.RFC3339),
		GeneratorModel:    generatorModel,
		PromptFingerprint: fingerprint,
		Tolerance:         DefaultCatchRateRegressionTolerance,
		MinTrialsPerArm:   MinCatchRateTrialsPerArm,
		SamplesPerCase:    samples,
		Arms: CatchRateEvidenceArms{
			WithoutConventions: CatchRateEvidenceArm{PerCase: without.PerCase},
			WithConventions:    CatchRateEvidenceArm{PerCase: with.PerCase},
		},
		Baseline: ref,
	}
	// The pin is judged against the CURRENT baseline too: a measurement that
	// regresses against the reference cannot become the reference.
	report, err := judgeCatchRate(rec, ref, opts.PinBaseline)
	if err != nil {
		return report, fmt.Errorf("agenteval: catch-rate measurement REFUSED, evidence not written: %w", err)
	}
	if opts.PinBaseline {
		rec.Baseline = &CatchRateBaseline{
			PinnedAt:          rec.RecordedAt,
			Reason:            strings.TrimSpace(opts.Reason),
			GeneratorModel:    rec.GeneratorModel,
			PromptFingerprint: rec.PromptFingerprint,
			SamplesPerCase:    rec.SamplesPerCase,
			Arms:              rec.Arms,
		}
		report += fmt.Sprintf("\nPinned this measurement as the new baseline (reason: %s).\n", rec.Baseline.Reason)
	}
	if opts.DryRun {
		return report + "\nDry run: evidence not written.\n", nil
	}
	if err := writeCatchRateEvidence(evidencePath, rec); err != nil {
		return report, err
	}
	return report, nil
}

// readExistingCatchRateEvidence returns nil for an absent file and an error
// for a malformed one (strict decode, schema, baseline shape) — a malformed
// existing record is never silently dropped, or its pinned baseline would be.
func readExistingCatchRateEvidence(path string) (*CatchRateEvidence, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("agenteval: read existing catch-rate evidence %q: %w", path, err)
	}
	rec, err := decodeCatchRateEvidence(raw)
	if err != nil {
		return nil, fmt.Errorf("agenteval: existing catch-rate evidence %q is malformed (refusing to overwrite it — its pinned baseline would be lost): %w", path, err)
	}
	if rec.Baseline != nil {
		if err := validateCatchRateBaseline(rec.Baseline); err != nil {
			return nil, fmt.Errorf("agenteval: existing catch-rate evidence %q is malformed (refusing to overwrite it): %w", path, err)
		}
	}
	return rec, nil
}

func decodeCatchRateEvidence(raw []byte) (*CatchRateEvidence, error) {
	var rec CatchRateEvidence
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("malformed JSON or unknown field: %w", err)
	}
	if rec.Schema != CatchRateEvidenceSchema {
		return nil, fmt.Errorf("schema %q is not %q", rec.Schema, CatchRateEvidenceSchema)
	}
	return &rec, nil
}

func writeCatchRateEvidence(path string, rec CatchRateEvidence) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("agenteval: marshal catch-rate evidence: %w", err)
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".catchrate-evidence-*.json")
	if err != nil {
		return fmt.Errorf("agenteval: write catch-rate evidence: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("agenteval: write catch-rate evidence: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("agenteval: write catch-rate evidence: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("agenteval: write catch-rate evidence: %w", err)
	}
	return nil
}

// CheckCatchRateEvidence is the offline gate: it decides from the committed
// record alone (no model call, no network) and RECOMPUTES every verdict from
// the counts. It returns a report (rates, deltas, both rules) on success and
// fails closed, each with a message naming the run-book, on:
//
//	(1) an absent evidence file
//	(2) malformed JSON or an unknown field
//	(3) a wrong schema
//	(4) a stale prompt fingerprint, or a generator model other than generatorModel
//	(5) a recorded tolerance / min_trials_per_arm differing from the current constants
//	(6) inconsistent counts in either arm (negative, caught+undecodable > trials),
//	    per-case trials != samples_per_case, or the two arms disagreeing on the
//	    case set or the per-case trial weights
//	(7) a case set differing from the current corpus
//	(8) an under-powered arm
//	(9) a within-run regression above the tolerance
//	(10) a missing, malformed or non-comparable pinned baseline, or a
//	     regression of either arm against it
//	(11) an unavailable corpus
//	(12) an unavailable conventions fixture
func CheckCatchRateEvidence(evidencePath, corpusDir, conventionsPath, generatorModel string) (string, error) {
	fail := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("agenteval: catch-rate evidence %q: "+format+"; see %s", append(append([]any{evidencePath}, args...), catchRateRunbook)...)
	}
	cases, err := LoadPlanReviewCatchCorpus(corpusDir)
	if err != nil {
		return fail("corpus unavailable: %v", err) // (11)
	}
	conv, err := LoadRepresentativeConventions(conventionsPath)
	if err != nil {
		return fail("conventions fixture unavailable: %v", err) // (12)
	}
	raw, err := os.ReadFile(evidencePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("absent: no two-arm measurement has been recorded") // (1)
		}
		return fail("read: %v", err)
	}
	rec, err := decodeCatchRateEvidence(raw) // (2), (3)
	if err != nil {
		return fail("%v", err)
	}
	if rec.Tolerance != DefaultCatchRateRegressionTolerance || rec.MinTrialsPerArm != MinCatchRateTrialsPerArm {
		return fail("recorded tolerance %v / min_trials_per_arm %d differ from the current rule (%v / %d): re-measure", rec.Tolerance, rec.MinTrialsPerArm, DefaultCatchRateRegressionTolerance, MinCatchRateTrialsPerArm) // (5)
	}
	if rec.GeneratorModel != generatorModel {
		return fail("recorded generator model %q is not the gate's model %q: re-measure", rec.GeneratorModel, generatorModel) // (4)
	}
	want, err := CatchRatePromptFingerprint(cases, conv, generatorModel)
	if err != nil {
		return fail("fingerprint: %v", err)
	}
	if rec.PromptFingerprint != want {
		return fail("STALE: recorded prompt fingerprint %s != current %s — the corpus, the conventions fixture, the rendered plan-review prompt or the catch rule changed since the measurement: re-measure", rec.PromptFingerprint, want) // (4)
	}
	if err := validateEvidenceArms(rec.SamplesPerCase, rec.Arms); err != nil {
		return fail("measurement: %v", err) // (6)
	}
	if missing, extra := caseSetDiff(rec.Arms.WithoutConventions.PerCase, cases); len(missing) > 0 || len(extra) > 0 {
		return fail("case set differs from the current corpus (missing %v, unknown %v): re-measure", missing, extra) // (7)
	}
	if rec.Baseline == nil {
		return fail("no pinned baseline: every record must carry one") // (10)
	}
	if err := validateCatchRateBaseline(rec.Baseline); err != nil {
		return fail("%v", err) // (10)
	}
	report, err := judgeCatchRate(*rec, rec.Baseline, false) // (8), (9), (10)
	if err != nil {
		return report, fmt.Errorf("agenteval: catch-rate evidence %q: %w; see %s", evidencePath, err, catchRateRunbook)
	}
	return report, nil
}

// validateEvidenceArms is the count-invariant check (mode 6), in an order that
// lets each rule fire on its own fixture: per-case counts, then arm agreement
// on the case set and the per-case trial weights, then trials == samples.
func validateEvidenceArms(samples int, arms CatchRateEvidenceArms) error {
	if samples < 1 {
		return fmt.Errorf("samples_per_case %d must be >= 1", samples)
	}
	named := []struct {
		arm     string
		perCase map[string]CaseCatchCounts
	}{
		{ArmWithoutConventions, arms.WithoutConventions.PerCase},
		{ArmWithConventions, arms.WithConventions.PerCase},
	}
	for _, a := range named {
		for _, name := range sortedCaseNames(a.perCase) {
			if err := a.perCase[name].validate(); err != nil {
				return fmt.Errorf("inconsistent counts in arm %q case %q: %w", a.arm, name, err)
			}
		}
	}
	w, h := arms.WithoutConventions.PerCase, arms.WithConventions.PerCase
	for _, name := range sortedCaseNames(w) {
		if _, ok := h[name]; !ok {
			return fmt.Errorf("the arms disagree on the case set: %q is in %q but not %q", name, ArmWithoutConventions, ArmWithConventions)
		}
	}
	for _, name := range sortedCaseNames(h) {
		if _, ok := w[name]; !ok {
			return fmt.Errorf("the arms disagree on the case set: %q is in %q but not %q", name, ArmWithConventions, ArmWithoutConventions)
		}
	}
	for _, name := range sortedCaseNames(w) {
		if w[name].Trials != h[name].Trials {
			return fmt.Errorf("the arms disagree on the trial weight of case %q (%d without, %d with)", name, w[name].Trials, h[name].Trials)
		}
	}
	for _, a := range named {
		for _, name := range sortedCaseNames(a.perCase) {
			if t := a.perCase[name].Trials; t != samples {
				return fmt.Errorf("arm %q case %q has %d trials, not samples_per_case %d", a.arm, name, t, samples)
			}
		}
	}
	return nil
}

func validateCatchRateBaseline(b *CatchRateBaseline) error {
	if strings.TrimSpace(b.Reason) == "" {
		return fmt.Errorf("pinned baseline carries no reason")
	}
	if _, err := time.Parse(time.RFC3339, b.PinnedAt); err != nil {
		return fmt.Errorf("pinned baseline pinned_at %q is not an RFC 3339 timestamp", b.PinnedAt)
	}
	if err := validateEvidenceArms(b.SamplesPerCase, b.Arms); err != nil {
		return fmt.Errorf("pinned baseline: %w", err)
	}
	return nil
}

// baselineComparable reports why a measurement cannot be judged against a
// baseline: a different generator model, samples_per_case or case set makes
// the pooled rates a different instrument.
func baselineComparable(rec CatchRateEvidence, b *CatchRateBaseline) (bool, string) {
	if rec.GeneratorModel != b.GeneratorModel {
		return false, fmt.Sprintf("generator model changed (%q pinned, %q measured)", b.GeneratorModel, rec.GeneratorModel)
	}
	if !sameCaseNames(rec.Arms.WithoutConventions.PerCase, b.Arms.WithoutConventions.PerCase) {
		return false, "corpus changed (the case sets differ)"
	}
	if rec.SamplesPerCase != b.SamplesPerCase {
		return false, fmt.Sprintf("samples_per_case changed (%d pinned, %d measured)", b.SamplesPerCase, rec.SamplesPerCase)
	}
	return true, ""
}

// judgeCatchRate is THE gate rule, shared by the recorder and the checker. It
// applies the within-run rule (CompareCatchRateArms: power floor, then the
// tolerance), then — when ref is non-nil — the pinned-baseline rule: FAIL when
// either arm's rate is more than the tolerance below the SAME arm of the
// pinned baseline, decided in exact rationals. A non-comparable baseline is an
// error unless pinning (the pin replaces it). Regressions wrap
// ErrCatchRateRegressed.
func judgeCatchRate(rec CatchRateEvidence, ref *CatchRateBaseline, pinning bool) (string, error) {
	without := CatchRateArmReport{Arm: ArmWithoutConventions, SamplesPerCase: rec.SamplesPerCase, PerCase: rec.Arms.WithoutConventions.PerCase}
	with := CatchRateArmReport{Arm: ArmWithConventions, SamplesPerCase: rec.SamplesPerCase, PerCase: rec.Arms.WithConventions.PerCase}
	cmp, err := CompareCatchRateArms(without, with, DefaultCatchRateRegressionTolerance, MinCatchRateTrialsPerArm)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(cmp.Render())
	if cmp.Regressed {
		return b.String(), fmt.Errorf("%w: the with-conventions arm is %+.3f below the without-conventions arm, more than the %.2f tolerance", ErrCatchRateRegressed, cmp.Delta, DefaultCatchRateRegressionTolerance)
	}
	if ref == nil {
		b.WriteString("\nNo pinned baseline: judged on the within-run rule only.\n")
		return b.String(), nil
	}
	if ok, why := baselineComparable(rec, ref); !ok {
		if pinning {
			fmt.Fprintf(&b, "\nThe current pinned baseline is not comparable (%s); the pin replaces it.\n", why)
			return b.String(), nil
		}
		return b.String(), fmt.Errorf("the pinned baseline is not comparable to this measurement: %s — re-pin it (an explicit, reasoned operator action)", why)
	}
	fmt.Fprintf(&b, "\nPinned baseline (pinned %s, reason: %s). Rule: FAIL when either arm's catch rate is MORE than %.2f below the SAME arm of the pinned baseline — the reference does not roll forward with each recording.\n",
		ref.PinnedAt, ref.Reason, DefaultCatchRateRegressionTolerance)
	var regressed []string
	for _, arm := range []struct {
		name     string
		measured map[string]CaseCatchCounts
		pinned   map[string]CaseCatchCounts
	}{
		{ArmWithoutConventions, rec.Arms.WithoutConventions.PerCase, ref.Arms.WithoutConventions.PerCase},
		{ArmWithConventions, rec.Arms.WithConventions.PerCase, ref.Arms.WithConventions.PerCase},
	} {
		mt, mc := pooledCounts(arm.measured)
		pt, pc := pooledCounts(arm.pinned)
		drop := new(big.Rat).Sub(big.NewRat(int64(pc), int64(pt)), big.NewRat(int64(mc), int64(mt)))
		dropF, _ := drop.Float64()
		fmt.Fprintf(&b, "  %s: pinned %d/%d (%.3f), measured %d/%d (%.3f), drop %+.3f\n",
			arm.name, pc, pt, float64(pc)/float64(pt), mc, mt, float64(mc)/float64(mt), dropF)
		if drop.Cmp(new(big.Rat).SetFloat64(DefaultCatchRateRegressionTolerance)) > 0 {
			regressed = append(regressed, fmt.Sprintf("%s %+.3f", arm.name, dropF))
		}
	}
	if len(regressed) > 0 {
		return b.String(), fmt.Errorf("%w against the pinned baseline (%s): %s, more than the %.2f tolerance", ErrCatchRateRegressed, ref.PinnedAt, strings.Join(regressed, ", "), DefaultCatchRateRegressionTolerance)
	}
	return b.String(), nil
}

// pooledCounts sums trials and caught. Callers have already validated the
// counts, so trials > 0 for a non-empty, powered arm.
func pooledCounts(perCase map[string]CaseCatchCounts) (trials, caught int) {
	for _, c := range perCase {
		trials += c.Trials
		caught += c.Caught
	}
	return trials, caught
}

func sortedCaseNames(m map[string]CaseCatchCounts) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sameCaseNames(a, b map[string]CaseCatchCounts) bool {
	if len(a) != len(b) {
		return false
	}
	for name := range a {
		if _, ok := b[name]; !ok {
			return false
		}
	}
	return true
}

// caseSetDiff returns the corpus cases absent from the record and the record
// cases absent from the corpus.
func caseSetDiff(recorded map[string]CaseCatchCounts, cases []PlanReviewCatchCase) (missing, extra []string) {
	inCorpus := make(map[string]bool, len(cases))
	for _, c := range cases {
		inCorpus[c.Name] = true
		if _, ok := recorded[c.Name]; !ok {
			missing = append(missing, c.Name)
		}
	}
	for _, name := range sortedCaseNames(recorded) {
		if !inCorpus[name] {
			extra = append(extra, name)
		}
	}
	return missing, extra
}
