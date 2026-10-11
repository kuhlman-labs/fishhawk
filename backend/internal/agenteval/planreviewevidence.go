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
//
// THE TRIAL SIDECAR. Every recording also writes evidence.trials.json beside
// the record (CatchRateTrialsPath; schema CatchRateTrialsSchema): per arm and
// case, one entry per trial in sample order — its outcome, the matched probe
// and attribution kind of a catch, every concern of a miss with its near-miss
// tag, a response excerpt of an undecodable trial. Every stored string passes
// through redaction.RedactDefault and then a rune-safe truncation at
// catchTrialNoteMaxRunes; a trial keeps at most catchTrialMaxConcerns concerns.
// The two files are written as a pair, sidecar first (a crash between the
// renames leaves a sidecar the old record does not match, which the gate
// refuses), and CheckCatchRateEvidence refuses a record whose sidecar is
// absent, malformed, from another recording or count-inconsistent with it.

package agenteval

import (
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
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// CatchRateEvidenceSchema is the evidence record's schema identifier.
const CatchRateEvidenceSchema = "planreview-catchrate-evidence-v1"

// catchRateRunbook is named in every refusal so the reader knows the remedy.
const catchRateRunbook = "docs/compliance/planreview-catchrate-evidence.md"

// CatchRateTrialsSchema is the trial sidecar's schema identifier.
const CatchRateTrialsSchema = "planreview-catchrate-trials-v1"

const (
	// catchTrialNoteMaxRunes caps every string the sidecar stores (severity,
	// category, note, matched probe, response excerpt), marker included.
	catchTrialNoteMaxRunes = 1000
	// catchTrialMaxConcerns caps the concerns one trial stores, in emitted
	// order; concerns_total keeps the uncapped count.
	catchTrialMaxConcerns = 12
	// catchTrialTruncationMarker ends a truncated string.
	catchTrialTruncationMarker = " [truncated]"
)

// CatchRateTrialsPath is the trial sidecar's path for an evidence record: the
// evidence path's stem plus ".trials.json"
// (testdata/planreview-catchrate/evidence.trials.json for the committed one).
func CatchRateTrialsPath(evidencePath string) string {
	return strings.TrimSuffix(evidencePath, filepath.Ext(evidencePath)) + ".trials.json"
}

// CatchRateTrialConcern is one stored reviewer concern of a trial.
type CatchRateTrialConcern struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Note     string `json:"note"`
	// NearMiss is set on a missed trial's concern only:
	// NearMissProbeWithoutAttribution or NearMissAttributionWithoutProbe.
	NearMiss string `json:"near_miss,omitempty"`
}

// CatchRateTrialRecord is one stored trial.
type CatchRateTrialRecord struct {
	// Trial is the 1-based sample index within its arm and case.
	Trial   int          `json:"trial"`
	Outcome CatchOutcome `json:"outcome"`
	// MatchedProbe and Attribution ("id" or "phrase") are set on a caught
	// trial only.
	MatchedProbe string `json:"matched_probe,omitempty"`
	Attribution  string `json:"attribution,omitempty"`
	// Concerns is the matching concern of a caught trial and every concern
	// (capped at max_concerns_per_trial) of a missed trial.
	Concerns      []CatchRateTrialConcern `json:"concerns"`
	ConcernsTotal int                     `json:"concerns_total"`
	// ResponseExcerpt is set on an undecodable trial only.
	ResponseExcerpt string `json:"response_excerpt,omitempty"`
}

// CatchRateTrialsArm is one arm's trials, per case in sample order.
type CatchRateTrialsArm struct {
	PerCase map[string][]CatchRateTrialRecord `json:"per_case"`
}

// CatchRateTrialsArms is both arms' trials.
type CatchRateTrialsArms struct {
	WithoutConventions CatchRateTrialsArm `json:"without_conventions"`
	WithConventions    CatchRateTrialsArm `json:"with_conventions"`
}

// CatchRateTrials is the committed trial sidecar
// (testdata/planreview-catchrate/evidence.trials.json). RecordedAt and
// PromptFingerprint equal the evidence record's, which is how the gate binds
// the pair.
type CatchRateTrials struct {
	Schema              string              `json:"schema"`
	RecordedAt          string              `json:"recorded_at"`
	PromptFingerprint   string              `json:"prompt_fingerprint"`
	NoteMaxRunes        int                 `json:"note_max_runes"`
	MaxConcernsPerTrial int                 `json:"max_concerns_per_trial"`
	Arms                CatchRateTrialsArms `json:"arms"`
}

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
// samples per case), judges the measurement, and — only when it passes and
// DryRun is off — writes the evidence record AND its trial sidecar
// (CatchRateTrialsPath) as a pair (writeCatchRateEvidencePair). It returns the
// rendered report (also on a gate refusal, so the caller can show the numbers)
// and an error.
//
// It REFUSES, leaving evidencePath and its sidecar byte-identical, on: PinBaseline with a
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
	trials := buildCatchRateTrials(rec, without, with)
	if err := writeCatchRateEvidencePair(evidencePath, rec, trials); err != nil {
		return report, err
	}
	return report + fmt.Sprintf("\nTrial sidecar written: %s\n", CatchRateTrialsPath(evidencePath)), nil
}

// buildCatchRateTrials is the sidecar for a judged measurement: the arm
// reports' trials, redacted and truncated (sanitizeCatchTrialText), concerns
// capped at catchTrialMaxConcerns.
func buildCatchRateTrials(rec CatchRateEvidence, without, with CatchRateArmReport) CatchRateTrials {
	arm := func(r CatchRateArmReport) CatchRateTrialsArm {
		out := CatchRateTrialsArm{PerCase: make(map[string][]CatchRateTrialRecord, len(r.Trials))}
		for name, trials := range r.Trials {
			recs := make([]CatchRateTrialRecord, 0, len(trials))
			for i, tr := range trials {
				recs = append(recs, catchRateTrialRecord(i+1, tr))
			}
			out.PerCase[name] = recs
		}
		return out
	}
	return CatchRateTrials{
		Schema:              CatchRateTrialsSchema,
		RecordedAt:          rec.RecordedAt,
		PromptFingerprint:   rec.PromptFingerprint,
		NoteMaxRunes:        catchTrialNoteMaxRunes,
		MaxConcernsPerTrial: catchTrialMaxConcerns,
		Arms:                CatchRateTrialsArms{WithoutConventions: arm(without), WithConventions: arm(with)},
	}
}

func catchRateTrialRecord(index int, tr CatchTrial) CatchRateTrialRecord {
	out := CatchRateTrialRecord{
		Trial:         index,
		Outcome:       tr.Outcome,
		MatchedProbe:  sanitizeCatchTrialText(tr.MatchedProbe),
		Attribution:   tr.Attribution,
		Concerns:      make([]CatchRateTrialConcern, 0, min(len(tr.Concerns), catchTrialMaxConcerns)),
		ConcernsTotal: tr.ConcernsTotal,
	}
	for i, c := range tr.Concerns {
		if i == catchTrialMaxConcerns {
			break
		}
		out.Concerns = append(out.Concerns, CatchRateTrialConcern{
			Severity: sanitizeCatchTrialText(c.Severity),
			Category: sanitizeCatchTrialText(c.Category),
			Note:     sanitizeCatchTrialText(c.Note),
			NearMiss: c.NearMiss,
		})
	}
	if tr.Outcome == CatchUndecodable {
		out.ResponseExcerpt = sanitizeCatchTrialText(tr.Response)
	}
	return out
}

// sanitizeCatchTrialText is what every model-derived string goes through
// before it is committed: redaction.RedactDefault (the notes are model output
// and land in the repository), then truncateRunes at catchTrialNoteMaxRunes.
func sanitizeCatchTrialText(s string) string {
	redacted, _ := redaction.RedactDefault([]byte(s))
	return truncateRunes(string(redacted), catchTrialNoteMaxRunes)
}

// truncateRunes returns s when it has at most maxRunes runes; otherwise the
// first runes of s followed by catchTrialTruncationMarker, maxRunes runes in
// all. It cuts on rune boundaries (invalid UTF-8 is first replaced with
// U+FFFD), so the result is always valid UTF-8.
func truncateRunes(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	keep := maxRunes - utf8.RuneCountInString(catchTrialTruncationMarker)
	if keep < 0 {
		keep = 0
	}
	return string([]rune(s)[:keep]) + catchTrialTruncationMarker
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
	if err := decodeStrictJSON(raw, &rec); err != nil {
		return nil, fmt.Errorf("malformed JSON, unknown field or trailing content: %w", err)
	}
	if rec.Schema != CatchRateEvidenceSchema {
		return nil, fmt.Errorf("schema %q is not %q", rec.Schema, CatchRateEvidenceSchema)
	}
	return &rec, nil
}

// writeCatchRateEvidencePair writes the record and its trial sidecar: both are
// marshalled and staged to temp files in the evidence directory first, then
// the sidecar is renamed into place, then the record. A failure before the
// first rename leaves both paths untouched; a crash between the renames leaves
// a new sidecar beside the OLD record, whose recorded_at and fingerprint it
// does not match, so CheckCatchRateEvidence refuses the pair (mode 15) — the
// fail-closed direction.
func writeCatchRateEvidencePair(path string, rec CatchRateEvidence, trials CatchRateTrials) error {
	wrap := func(err error) error { return fmt.Errorf("agenteval: write catch-rate evidence: %w", err) }
	recBytes, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("agenteval: marshal catch-rate evidence: %w", err)
	}
	trialBytes, err := json.MarshalIndent(trials, "", "  ")
	if err != nil {
		return fmt.Errorf("agenteval: marshal catch-rate trial sidecar: %w", err)
	}
	trialsTmp, err := stageCatchRateFile(filepath.Dir(path), ".catchrate-trials-*.json", append(trialBytes, '\n'))
	if err != nil {
		return wrap(err)
	}
	recTmp, err := stageCatchRateFile(filepath.Dir(path), ".catchrate-evidence-*.json", append(recBytes, '\n'))
	if err != nil {
		_ = os.Remove(trialsTmp)
		return wrap(err)
	}
	if err := os.Rename(trialsTmp, CatchRateTrialsPath(path)); err != nil {
		_ = os.Remove(trialsTmp)
		_ = os.Remove(recTmp)
		return wrap(err)
	}
	if err := os.Rename(recTmp, path); err != nil {
		_ = os.Remove(recTmp)
		return wrap(err)
	}
	return nil
}

// stageCatchRateFile writes b to a new temp file in dir and returns its name;
// on any failure it removes the temp file.
func stageCatchRateFile(dir, pattern string, b []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// CheckCatchRateEvidence is the offline gate: it decides from the committed
// record alone (no model call, no network) and RECOMPUTES every verdict from
// the counts. It returns a report (rates, deltas, both rules) on success and
// fails closed, each with a message naming the run-book, on:
//
//	(1) an absent evidence file
//	(2) malformed JSON, an unknown field, or trailing content after the record
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
//
// and then, only once the record itself passes every mode above, on its trial
// sidecar (CatchRateTrialsPath):
//
//	(13) an absent sidecar
//	(14) a malformed sidecar: malformed JSON, an unknown field, trailing
//	     content, or a wrong schema
//	(15) a sidecar prompt_fingerprint or recorded_at differing from the
//	     record's — a sidecar from another recording
//	(16) per arm, a sidecar case set differing from the record's; per arm and
//	     case, a trial list whose length differs from the recorded trials,
//	     whose trial indices are not exactly 1..n, or whose outcome tallies
//	     differ from the recorded caught / undecodable counts (an outcome
//	     outside caught|missed|undecodable is a tally mismatch)
//
// The success report names the sidecar path.
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
	trialsPath := CatchRateTrialsPath(evidencePath)
	if err := checkCatchRateTrials(trialsPath, rec); err != nil {
		return fail("trial sidecar %q: %v", trialsPath, err) // (13)-(16)
	}
	report += fmt.Sprintf("\nTrial sidecar: %s (per-trial concern notes; a missed trial's concerns carry near_miss tags)\n", trialsPath)
	return report, nil
}

// checkCatchRateTrials is modes (13)-(16): the sidecar exists, decodes
// strictly under its schema, belongs to this recording, and agrees with the
// record's counts. Each mode-16 sub-check is independent of the others on
// its own fixture: the case set is compared first (an extra sidecar case
// fails only it — the later checks walk the RECORD's cases), the indices are
// checked against the list's own length (a dropped trial fails only the
// length check) and the tallies count all three outcomes against the list's
// own length (a duplicated index fails only the index check).
func checkCatchRateTrials(path string, rec *CatchRateEvidence) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("absent: every recording writes the trial sidecar with the record — commit both files (git add evidence.json evidence.trials.json)") // (13)
		}
		return fmt.Errorf("read: %w", err)
	}
	var trials CatchRateTrials
	if err := decodeStrictJSON(raw, &trials); err != nil {
		return fmt.Errorf("malformed JSON, unknown field or trailing content: %w", err) // (14)
	}
	if trials.Schema != CatchRateTrialsSchema {
		return fmt.Errorf("schema %q is not %q", trials.Schema, CatchRateTrialsSchema) // (14)
	}
	if trials.PromptFingerprint != rec.PromptFingerprint {
		return fmt.Errorf("prompt_fingerprint %s differs from the record's %s: the sidecar is from another recording", trials.PromptFingerprint, rec.PromptFingerprint) // (15)
	}
	if trials.RecordedAt != rec.RecordedAt {
		return fmt.Errorf("recorded_at %q differs from the record's %q: the sidecar is from another recording", trials.RecordedAt, rec.RecordedAt) // (15)
	}
	for _, a := range []struct {
		arm    string
		counts map[string]CaseCatchCounts
		trials map[string][]CatchRateTrialRecord
	}{
		{ArmWithoutConventions, rec.Arms.WithoutConventions.PerCase, trials.Arms.WithoutConventions.PerCase},
		{ArmWithConventions, rec.Arms.WithConventions.PerCase, trials.Arms.WithConventions.PerCase},
	} {
		if !sameTrialCaseSet(a.counts, a.trials) {
			return fmt.Errorf("arm %q case set %v differs from the record's %v", a.arm, sortedTrialCaseNames(a.trials), sortedCaseNames(a.counts)) // (16) case set
		}
		for _, name := range sortedCaseNames(a.counts) {
			counts, list := a.counts[name], a.trials[name]
			if len(list) != counts.Trials {
				return fmt.Errorf("arm %q case %q lists %d trials, the record %d", a.arm, name, len(list), counts.Trials) // (16) length
			}
			for i, tr := range list {
				if tr.Trial != i+1 {
					return fmt.Errorf("arm %q case %q entry %d has trial index %d, want %d (indices must be exactly 1..%d in order)", a.arm, name, i, tr.Trial, i+1, len(list)) // (16) indices
				}
			}
			var caught, missed, undecodable int
			for _, tr := range list {
				switch tr.Outcome {
				case CatchCaught:
					caught++
				case CatchMissed:
					missed++
				case CatchUndecodable:
					undecodable++
				}
			}
			if caught != counts.Caught || undecodable != counts.Undecodable || caught+missed+undecodable != len(list) {
				return fmt.Errorf("arm %q case %q outcome tallies (caught %d, missed %d, undecodable %d, of %d) differ from the record (caught %d, undecodable %d)", a.arm, name, caught, missed, undecodable, len(list), counts.Caught, counts.Undecodable) // (16) tallies
			}
		}
	}
	return nil
}

func sameTrialCaseSet(counts map[string]CaseCatchCounts, trials map[string][]CatchRateTrialRecord) bool {
	if len(counts) != len(trials) {
		return false
	}
	for name := range trials {
		if _, ok := counts[name]; !ok {
			return false
		}
	}
	return true
}

func sortedTrialCaseNames(m map[string][]CatchRateTrialRecord) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
