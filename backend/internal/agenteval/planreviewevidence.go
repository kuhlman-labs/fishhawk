// Plan-review catch-rate EVIDENCE (E55.4 / #2245; arm-keyed v2, E78.11 /
// #4313): the committed record of a live multi-arm measurement, and the
// offline check that decides from it.
//
// planreviewcatch.go and planreviewpersona.go own the measurement (corpora,
// arms, the ONE classifier classifyCatchConcerns, CompareCatchRateArms). This
// file owns what survives a measurement: a committed JSON record (schema
// CatchRateEvidenceSchema) carrying, PER ARM, its kind, samples_per_case,
// prompt fingerprint and per-case counts; the tolerance and power floor; the
// generator model; a record-level SHA-256 fingerprint over every arm's; and a
// PINNED BASELINE carrying the same arm map.
//
// THE ARM TABLE. catchRateArmTable is the one place an arm is declared: its
// name, kind, cases (name, expectation class, catch rule, raw bytes), its
// fingerprint and its runner. Today it holds without_conventions and
// with_conventions over the miss corpus and architect over the persona
// corpus; #4314 registers its security arms as further rows. The record, the
// sidecar, the gate and the re-score all iterate it.
//
// THE RULE, PER ARM. catchRateComparisonPairs keeps the within-run rule for
// (without_conventions, with_conventions). Then every arm, per expectation
// class, must clear the 136-trial floor and is judged against the SAME arm of
// the pinned baseline: FAIL on a catch-rate drop, or a false-alarm-rate rise
// ((false_alarms + undecodable) / trials over the clean cases), strictly
// above the tolerance, in exact rationals.
//
// LEGACY v1. A planreview-catchrate-evidence-v1 record is recognised by a
// version-dispatching reader: the gate refuses it naming the shared
// #4313/#4314 re-pin recording, and the recorder treats it as a
// non-comparable baseline (only a pin replaces it).
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
// the record (CatchRateTrialsPath; schema CatchRateTrialsSchema, arm-keyed
// like the record): per arm and case, one entry per trial in sample order —
// its outcome, the matched probe and attribution kind of a catch, every
// concern of a miss with its near-miss tag, the alarming concerns of a clean
// case's false alarm (every concern of a no_false_alarm), a response excerpt
// of an undecodable trial. A persona concern also stores its post-control
// severity, severity_clamped_from, quote_unverified and document_ref. Every stored string passes
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
	"hash"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// CatchRateEvidenceSchema is the evidence record's schema identifier.
const CatchRateEvidenceSchema = "planreview-catchrate-evidence-v2"

// catchRateEvidenceSchemaV1 is the LEGACY two-arm record's schema: recognised
// (never accepted) by readCatchRateEvidence.
const catchRateEvidenceSchemaV1 = "planreview-catchrate-evidence-v1"

// catchRateSharedRecording names the remedy for a legacy v1 record.
const catchRateSharedRecording = "the shared #4313/#4314 re-pin recording (one recording of every arm, pinning a v2 baseline)"

// errLegacyCatchRateEvidence marks a v1 record.
var errLegacyCatchRateEvidence = errors.New("legacy planreview-catchrate-evidence-v1 record")

// catchRateRunbook is named in every refusal so the reader knows the remedy.
const catchRateRunbook = "docs/compliance/planreview-catchrate-evidence.md"

// CatchRateTrialsSchema is the trial sidecar's schema identifier.
const CatchRateTrialsSchema = "planreview-catchrate-trials-v2"

// catchRateTrialsSchemaV1 is the legacy two-arm sidecar's schema; the
// re-score still reads it (its arms decode into the same arm map).
const catchRateTrialsSchemaV1 = "planreview-catchrate-trials-v1"

// The arm kinds.
const (
	CatchRateArmKindConventions = "conventions"
	CatchRateArmKindPersona     = "persona"
)

// CatchRateInputs names everything the arm table is built from: the miss
// corpus and the conventions fixture (the conventions arms), and the persona
// corpus plus the checkout root its documents are read from (the architect
// arm).
type CatchRateInputs struct {
	CorpusDir        string
	ConventionsPath  string
	PersonaCorpusDir string
	RepoRoot         string
}

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

// CatchRateTrialConcern is one stored reviewer concern of a trial. The
// persona fields (SeverityClampedFrom, QuoteUnverified, DocumentRef) are set
// only by a persona arm and omitted otherwise, so a v1 sidecar decodes.
type CatchRateTrialConcern struct {
	Severity            string `json:"severity"`
	SeverityClampedFrom string `json:"severity_clamped_from,omitempty"`
	QuoteUnverified     bool   `json:"quote_unverified,omitempty"`
	DocumentRef         string `json:"document_ref,omitempty"`
	Category            string `json:"category"`
	Note                string `json:"note"`
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

// CatchRateTrialsArms is every arm's trials, keyed by arm name.
type CatchRateTrialsArms map[string]CatchRateTrialsArm

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

// CatchRateEvidenceArm is one arm of a measurement: its kind
// (CatchRateArmKindConventions or CatchRateArmKindPersona), its per-case
// sample count, the fingerprint of everything that arm's model was shown, and
// its per-case counts.
type CatchRateEvidenceArm struct {
	Kind              string                     `json:"kind"`
	SamplesPerCase    int                        `json:"samples_per_case"`
	PromptFingerprint string                     `json:"prompt_fingerprint"`
	PerCase           map[string]CaseCatchCounts `json:"per_case"`
}

// CatchRateEvidenceArms is every arm of one measurement, keyed by arm name.
type CatchRateEvidenceArms map[string]CatchRateEvidenceArm

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
	Arms              CatchRateEvidenceArms `json:"arms"`
	// Baseline is the pinned reference; absent or null is refused by
	// CheckCatchRateEvidence.
	Baseline *CatchRateBaseline `json:"baseline"`
}

// catchRateArmCase is one case of an arm-table row.
type catchRateArmCase struct {
	name string
	// expect is CatchExpectCatch or CatchExpectClean.
	expect string
	rule   CatchRule
}

// catchRateArmRow is ONE arm of the arm table: everything the recorder, the
// gate and the re-score need to know about it.
type catchRateArmRow struct {
	name string
	kind string
	// cases is in case-name order.
	cases []catchRateArmCase
	// samples is the per-case sample count that clears the floor in every
	// expectation class the arm has.
	samples int
	// fingerprint is "sha256:<hex>" over everything this arm's model is
	// shown and how its trials are scored.
	fingerprint string
	// run measures the arm.
	run func(ctx context.Context, sender MessageSender, samples int) (CatchRateArmReport, error)
}

// catchRateComparisonPair is a (reference, treatment) arm pair the
// within-run rule compares.
type catchRateComparisonPair struct{ without, with string }

// catchRateComparisonPairs is the within-run rule's pairs: the conventions
// arm against its own same-run no-conventions arm. A persona arm has no
// within-run pair; it is judged per arm against the pinned baseline.
var catchRateComparisonPairs = []catchRateComparisonPair{{ArmWithoutConventions, ArmWithConventions}}

// fingerprintHasher is the length-prefixed SHA-256 every fingerprint uses.
type fingerprintHasher struct{ h hash.Hash }

func newFingerprintHasher() *fingerprintHasher { return &fingerprintHasher{h: sha256.New()} }

func (f *fingerprintHasher) put(b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	f.h.Write(n[:])
	f.h.Write(b)
}

func (f *fingerprintHasher) sum() string { return "sha256:" + hex.EncodeToString(f.h.Sum(nil)) }

// conventionsArmFingerprint is one conventions arm's fingerprint: the arm
// name, catchRuleVersion, the generator system prompt, and per case in name
// order its name, its exact review_input.json bytes and this arm's rendered
// prompt (which covers the conventions fixture and every prompt.Build change).
func conventionsArmFingerprint(cases []PlanReviewCatchCase, conv prompt.ReviewConvention, arm string) (string, error) {
	f := newFingerprintHasher()
	f.put([]byte(arm))
	f.put([]byte(catchRuleVersion))
	f.put([]byte(planReviewCatchGeneratorSystemPrompt))
	sorted := append([]PlanReviewCatchCase(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		f.put([]byte(c.Name))
		f.put(c.ReviewInputRaw)
		p, err := CatchRateArmPrompt(c, conv, arm)
		if err != nil {
			return "", err
		}
		f.put([]byte(p))
	}
	return f.sum(), nil
}

// personaArmFingerprint is the architect arm's fingerprint: the arm name,
// catchRuleVersion, the persona generator system prompt, the fixed
// declaration (remit, severity cap, index, category predicate, repository,
// fixture commit), and per case in name order its name, its exact
// persona_case.json bytes and its rendered persona prompt — so a remit, ADR,
// builder or corpus edit stales THIS arm, by name.
func personaArmFingerprint(cases []PersonaCatchCase, repoRoot string, render personaArmRenderer) (string, error) {
	f := newFingerprintHasher()
	f.put([]byte(ArmArchitect))
	f.put([]byte(catchRuleVersion))
	f.put([]byte(personaCatchGeneratorSystemPrompt))
	for _, d := range []string{personaArchitectRemitPath, personaArchitectSeverityCap, personaArchitectIndexPath,
		personaArchitectConcernCategory, personaCatchRepoOwner + "/" + personaCatchRepoName, personaFixtureCommit} {
		f.put([]byte(d))
	}
	sorted := append([]PersonaCatchCase(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		f.put([]byte(c.Name))
		f.put(c.Raw)
		r, err := render(c, repoRoot)
		if err != nil {
			return "", err
		}
		f.put([]byte(r.Prompt))
	}
	return f.sum(), nil
}

// catchRateArmTable builds THE arm table from inputs, in arm-name order. It
// fails closed on an unavailable corpus, conventions fixture, persona corpus
// or document checkout, and on a render error.
func catchRateArmTable(in CatchRateInputs) ([]catchRateArmRow, error) {
	return catchRateArmTableWith(in, PersonaArmPrompt)
}

func catchRateArmTableWith(in CatchRateInputs, render personaArmRenderer) ([]catchRateArmRow, error) {
	cases, err := LoadPlanReviewCatchCorpus(in.CorpusDir)
	if err != nil {
		return nil, fmt.Errorf("corpus unavailable: %w", err)
	}
	conv, err := LoadRepresentativeConventions(in.ConventionsPath)
	if err != nil {
		return nil, fmt.Errorf("conventions fixture unavailable: %w", err)
	}
	personaCases, err := LoadPersonaCatchCorpus(in.PersonaCorpusDir, in.RepoRoot, ArmArchitect)
	if err != nil {
		return nil, fmt.Errorf("persona corpus unavailable: %w", err)
	}
	var rows []catchRateArmRow
	convCases := make([]catchRateArmCase, 0, len(cases))
	for _, c := range cases {
		convCases = append(convCases, catchRateArmCase{name: c.Name, expect: CatchExpectCatch, rule: c.CatchRule()})
	}
	sort.Slice(convCases, func(i, j int) bool { return convCases[i].name < convCases[j].name })
	for _, arm := range []string{ArmWithoutConventions, ArmWithConventions} {
		fp, err := conventionsArmFingerprint(cases, conv, arm)
		if err != nil {
			return nil, fmt.Errorf("arm %q fingerprint: %w", arm, err)
		}
		rows = append(rows, catchRateArmRow{
			name: arm, kind: CatchRateArmKindConventions, cases: convCases,
			samples: SamplesPerCaseForPower(len(cases)), fingerprint: fp,
			run: func(ctx context.Context, sender MessageSender, samples int) (CatchRateArmReport, error) {
				return RunCatchRateArm(ctx, sender, cases, conv, arm, samples)
			},
		})
	}
	pCases := make([]catchRateArmCase, 0, len(personaCases))
	var nCatch, nClean int
	for _, c := range personaCases {
		pCases = append(pCases, catchRateArmCase{name: c.Name, expect: c.Expect(), rule: c.CatchRule()})
		if c.Expect() == CatchExpectClean {
			nClean++
		} else {
			nCatch++
		}
	}
	fp, err := personaArmFingerprint(personaCases, in.RepoRoot, render)
	if err != nil {
		return nil, fmt.Errorf("arm %q fingerprint: %w", ArmArchitect, err)
	}
	rows = append(rows, catchRateArmRow{
		name: ArmArchitect, kind: CatchRateArmKindPersona, cases: pCases,
		samples: SamplesPerCaseForPersona(nCatch, nClean), fingerprint: fp,
		run: func(ctx context.Context, sender MessageSender, samples int) (CatchRateArmReport, error) {
			return runPersonaCatchArm(ctx, sender, personaCases, in.RepoRoot, samples, render)
		},
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	return rows, nil
}

// catchRateRecordFingerprint is the record-level fingerprint: the schema id,
// the generator model, the tolerance, the power floor, and each arm's name and
// fingerprint in arm-name order.
func catchRateRecordFingerprint(arms map[string]string, generatorModel string) string {
	f := newFingerprintHasher()
	f.put([]byte(CatchRateEvidenceSchema))
	f.put([]byte(generatorModel))
	f.put([]byte(strconv.FormatFloat(DefaultCatchRateRegressionTolerance, 'g', -1, 64)))
	f.put([]byte(strconv.Itoa(MinCatchRateTrialsPerArm)))
	names := make([]string, 0, len(arms))
	for name := range arms {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f.put([]byte(name))
		f.put([]byte(arms[name]))
	}
	return f.sum()
}

func rowFingerprints(rows []catchRateArmRow) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.name] = r.fingerprint
	}
	return out
}

// CatchRatePromptFingerprint is the CURRENT record-level fingerprint for
// inputs: "sha256:<hex>" over a length-prefixed encoding of the schema id, the
// generator model, the tolerance, the power floor, and every arm's name and
// fingerprint in name order (conventionsArmFingerprint,
// personaArmFingerprint). It does NOT cover the Go source of the classifier;
// a matcher change must bump catchRuleVersion.
func CatchRatePromptFingerprint(in CatchRateInputs, generatorModel string) (string, error) {
	rows, err := catchRateArmTable(in)
	if err != nil {
		return "", err
	}
	return catchRateRecordFingerprint(rowFingerprints(rows), generatorModel), nil
}

// RecordCatchRateOptions selects how RecordCatchRateEvidence treats the
// measurement it takes.
type RecordCatchRateOptions struct {
	// PinBaseline makes this measurement the new pinned baseline — the
	// explicit operator action that moves the reference. It still requires a
	// passing measurement (the within-run rule, and the CURRENT baseline when
	// that baseline is comparable). The first recording must pin, and so must
	// the first recording over a legacy v1 record.
	PinBaseline bool
	// Reason is recorded on the baseline; required (non-blank) with
	// PinBaseline.
	Reason string
	// DryRun measures and judges but never writes the evidence file.
	DryRun bool
}

// RecordCatchRateEvidence runs EVERY arm of the arm table against sender
// (each at its own floor-clearing samples per case), judges the measurement,
// and — only when it passes and DryRun is off — writes the evidence record
// AND its trial sidecar (CatchRateTrialsPath) as a pair
// (writeCatchRateEvidencePair). It returns the rendered report (also on a
// gate refusal, so the caller can show the numbers) and an error.
//
// It REFUSES, leaving evidencePath and its sidecar byte-identical, on:
// PinBaseline with a blank Reason; an existing record that is malformed
// (never silently dropped); no pinned baseline yet — including a legacy v1
// record, which is a non-comparable baseline — without PinBaseline (checked
// BEFORE any model call); an unavailable input, a transport or render error;
// a within-run regression; a regression of any arm against the pinned
// baseline; and, without PinBaseline, a baseline that is not comparable to
// this measurement (arm set, case set, model or samples changed — re-pin).
func RecordCatchRateEvidence(ctx context.Context, sender MessageSender, generatorModel string, in CatchRateInputs, evidencePath string, now time.Time, opts RecordCatchRateOptions) (string, error) {
	if opts.PinBaseline && strings.TrimSpace(opts.Reason) == "" {
		return "", fmt.Errorf("agenteval: pinning a catch-rate baseline requires a reason (it is recorded in the evidence); see %s", catchRateRunbook)
	}
	if strings.TrimSpace(generatorModel) == "" {
		return "", fmt.Errorf("agenteval: catch-rate recording requires a generator model")
	}
	rows, err := catchRateArmTable(in)
	if err != nil {
		return "", fmt.Errorf("agenteval: catch-rate recording: %w", err)
	}
	existing, legacy, err := readExistingCatchRateEvidence(evidencePath)
	if err != nil {
		return "", err
	}
	var ref *CatchRateBaseline
	if existing != nil {
		ref = existing.Baseline
	}
	if legacy && !opts.PinBaseline && !opts.DryRun {
		return "", fmt.Errorf("agenteval: the existing catch-rate evidence %q is a legacy %s record: its baseline is not comparable to an arm-keyed measurement, so this recording must pin a new one (PinBaseline with a reason) — %s; see %s", evidencePath, catchRateEvidenceSchemaV1, catchRateSharedRecording, catchRateRunbook)
	}
	if ref == nil && !legacy && !opts.PinBaseline && !opts.DryRun {
		return "", fmt.Errorf("agenteval: no pinned catch-rate baseline exists in %q: the first recording must pin one (PinBaseline with a reason); see %s", evidencePath, catchRateRunbook)
	}
	reports := make(map[string]CatchRateArmReport, len(rows))
	rec := CatchRateEvidence{
		Schema:            CatchRateEvidenceSchema,
		RecordedAt:        now.UTC().Format(time.RFC3339),
		GeneratorModel:    generatorModel,
		PromptFingerprint: catchRateRecordFingerprint(rowFingerprints(rows), generatorModel),
		Tolerance:         DefaultCatchRateRegressionTolerance,
		MinTrialsPerArm:   MinCatchRateTrialsPerArm,
		Arms:              make(CatchRateEvidenceArms, len(rows)),
		Baseline:          ref,
	}
	for _, row := range rows {
		r, err := row.run(ctx, sender, row.samples)
		if err != nil {
			return "", err
		}
		reports[row.name] = r
		rec.Arms[row.name] = CatchRateEvidenceArm{Kind: row.kind, SamplesPerCase: row.samples, PromptFingerprint: row.fingerprint, PerCase: r.PerCase}
	}
	report := ""
	if legacy {
		report = fmt.Sprintf("The existing record is a legacy %s record: not comparable; the pin replaces it.\n\n", catchRateEvidenceSchemaV1)
	}
	// The pin is judged against the CURRENT baseline too: a measurement that
	// regresses against the reference cannot become the reference.
	judged, err := judgeCatchRate(rec, ref, opts.PinBaseline)
	report += judged
	if err != nil {
		return report, fmt.Errorf("agenteval: catch-rate measurement REFUSED, evidence not written: %w", err)
	}
	if opts.PinBaseline {
		rec.Baseline = &CatchRateBaseline{
			PinnedAt:          rec.RecordedAt,
			Reason:            strings.TrimSpace(opts.Reason),
			GeneratorModel:    rec.GeneratorModel,
			PromptFingerprint: rec.PromptFingerprint,
			Arms:              cloneEvidenceArms(rec.Arms),
		}
		report += fmt.Sprintf("\nPinned this measurement as the new baseline (reason: %s).\n", rec.Baseline.Reason)
	}
	if opts.DryRun {
		return report + "\nDry run: evidence not written.\n", nil
	}
	trials := buildCatchRateTrials(rec, reports)
	if err := writeCatchRateEvidencePair(evidencePath, rec, trials); err != nil {
		return report, err
	}
	return report + fmt.Sprintf("\nTrial sidecar written: %s\n", CatchRateTrialsPath(evidencePath)), nil
}

// cloneEvidenceArms deep-copies an arm map, so the baseline never aliases the
// measurement's per-case maps.
func cloneEvidenceArms(arms CatchRateEvidenceArms) CatchRateEvidenceArms {
	out := make(CatchRateEvidenceArms, len(arms))
	for name, a := range arms {
		pc := make(map[string]CaseCatchCounts, len(a.PerCase))
		for k, v := range a.PerCase {
			pc[k] = v
		}
		a.PerCase = pc
		out[name] = a
	}
	return out
}

// buildCatchRateTrials is the sidecar for a judged measurement: every arm
// report's trials, redacted and truncated (sanitizeCatchTrialText), concerns
// capped at catchTrialMaxConcerns.
func buildCatchRateTrials(rec CatchRateEvidence, reports map[string]CatchRateArmReport) CatchRateTrials {
	arms := make(CatchRateTrialsArms, len(reports))
	for armName, r := range reports {
		out := CatchRateTrialsArm{PerCase: make(map[string][]CatchRateTrialRecord, len(r.Trials))}
		for name, trials := range r.Trials {
			recs := make([]CatchRateTrialRecord, 0, len(trials))
			for i, tr := range trials {
				recs = append(recs, catchRateTrialRecord(i+1, tr))
			}
			out.PerCase[name] = recs
		}
		arms[armName] = out
	}
	return CatchRateTrials{
		Schema:              CatchRateTrialsSchema,
		RecordedAt:          rec.RecordedAt,
		PromptFingerprint:   rec.PromptFingerprint,
		NoteMaxRunes:        catchTrialNoteMaxRunes,
		MaxConcernsPerTrial: catchTrialMaxConcerns,
		Arms:                arms,
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
			Severity:            sanitizeCatchTrialText(c.Severity),
			SeverityClampedFrom: sanitizeCatchTrialText(c.SeverityClampedFrom),
			QuoteUnverified:     c.QuoteUnverified,
			DocumentRef:         sanitizeCatchTrialText(c.DocumentRef),
			Category:            sanitizeCatchTrialText(c.Category),
			Note:                sanitizeCatchTrialText(c.Note),
			NearMiss:            c.NearMiss,
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

// readExistingCatchRateEvidence returns nil for an absent file, (nil, true)
// for a legacy v1 record (a non-comparable baseline: only a pin replaces it,
// and a non-pinning recording is refused), and an error for a malformed one
// (strict decode, schema, baseline shape) — a malformed existing record is
// never silently dropped, or its pinned baseline would be.
func readExistingCatchRateEvidence(path string) (*CatchRateEvidence, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("agenteval: read existing catch-rate evidence %q: %w", path, err)
	}
	rec, err := decodeCatchRateEvidence(raw)
	if errors.Is(err, errLegacyCatchRateEvidence) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("agenteval: existing catch-rate evidence %q is malformed (refusing to overwrite it — its pinned baseline would be lost): %w", path, err)
	}
	if rec.Baseline != nil {
		if err := validateCatchRateBaseline(rec.Baseline); err != nil {
			return nil, false, fmt.Errorf("agenteval: existing catch-rate evidence %q is malformed (refusing to overwrite it): %w", path, err)
		}
	}
	return rec, false, nil
}

// decodeCatchRateEvidence is the VERSION-DISPATCHING reader: it reads the
// schema field first, returns errLegacyCatchRateEvidence for a v1 record, and
// strictly decodes a v2 one.
func decodeCatchRateEvidence(raw []byte) (*CatchRateEvidence, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err == nil && head.Schema == catchRateEvidenceSchemaV1 {
		return nil, fmt.Errorf("%w: the arm-keyed gate does not read it — re-record every arm in %s", errLegacyCatchRateEvidence, catchRateSharedRecording)
	}
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
// the counts. It returns a report (rates, deltas, every rule) on success and
// fails closed, each with a message naming the run-book, on:
//
//	(1) an absent evidence file
//	(2) malformed JSON, an unknown field, or trailing content after the record
//	(3) a wrong schema; (3b) a legacy v1 record, refused naming the shared
//	    #4313/#4314 re-pin recording
//	(4) a stale prompt fingerprint, or a generator model other than
//	    generatorModel; (4b) a stale ARM fingerprint, naming every stale arm
//	(5) a recorded tolerance / min_trials_per_arm differing from the current constants
//	(6) inconsistent counts in any arm (negative, caught+undecodable > trials,
//	    false_alarms on a catch case, caught on a clean case), samples_per_case
//	    < 1, per-case trials != the arm's samples_per_case, an unknown arm kind,
//	    or a comparison pair disagreeing on the case set or the per-case trial
//	    weights; (6b) a case's expect differing from the corpus
//	(7) a case set differing from the current corpus; (7b) a missing or
//	    unknown arm, or an arm kind differing from the arm table's
//	(8) an under-powered arm or expectation class
//	(9) a within-run regression above the tolerance
//	(10) a missing, malformed or non-comparable pinned baseline, or a catch-rate
//	     drop or false-alarm-rate rise of any arm against it
//	(11) an unavailable corpus
//	(12) an unavailable conventions fixture (or persona corpus / checkout)
//
// and then, only once the record itself passes every mode above, on its trial
// sidecar (CatchRateTrialsPath):
//
//	(13) an absent sidecar
//	(14) a malformed sidecar: malformed JSON, an unknown field, trailing
//	     content, or a wrong schema
//	(15) a sidecar prompt_fingerprint or recorded_at differing from the
//	     record's — a sidecar from another recording
//	(16) a sidecar arm set or, per arm, case set differing from the record's;
//	     per arm and case, a trial list whose length differs from the
//	     recorded trials, whose trial indices are not exactly 1..n, or whose
//	     outcome tallies differ from the recorded caught / false_alarms /
//	     undecodable counts (an outcome outside the case's expectation class
//	     is a tally mismatch)
//
// The success report names the sidecar path.
func CheckCatchRateEvidence(evidencePath string, in CatchRateInputs, generatorModel string) (string, error) {
	fail := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("agenteval: catch-rate evidence %q: "+format+"; see %s", append(append([]any{evidencePath}, args...), catchRateRunbook)...)
	}
	rows, err := catchRateArmTable(in)
	if err != nil {
		return fail("%v", err) // (11), (12)
	}
	raw, err := os.ReadFile(evidencePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("absent: no arm-keyed measurement has been recorded") // (1)
		}
		return fail("read: %v", err)
	}
	rec, err := decodeCatchRateEvidence(raw) // (2), (3), (3b)
	if err != nil {
		return fail("%v", err)
	}
	if rec.Tolerance != DefaultCatchRateRegressionTolerance || rec.MinTrialsPerArm != MinCatchRateTrialsPerArm {
		return fail("recorded tolerance %v / min_trials_per_arm %d differ from the current rule (%v / %d): re-measure", rec.Tolerance, rec.MinTrialsPerArm, DefaultCatchRateRegressionTolerance, MinCatchRateTrialsPerArm) // (5)
	}
	if rec.GeneratorModel != generatorModel {
		return fail("recorded generator model %q is not the gate's model %q: re-measure", rec.GeneratorModel, generatorModel) // (4)
	}
	if err := checkArmSet(rec.Arms, rows); err != nil {
		return fail("%v", err) // (7b)
	}
	var stale []string
	for _, row := range rows {
		if rec.Arms[row.name].PromptFingerprint != row.fingerprint {
			stale = append(stale, row.name)
		}
	}
	if len(stale) > 0 {
		return fail("STALE arm(s) %s: the recorded arm fingerprint differs from the current one — that arm's corpus, documents, rendered prompt or the catch rule changed since the measurement: re-measure", strings.Join(stale, ", ")) // (4b)
	}
	if want := catchRateRecordFingerprint(rowFingerprints(rows), generatorModel); rec.PromptFingerprint != want {
		return fail("STALE: recorded prompt fingerprint %s != current %s — the schema, model, tolerance, floor or an arm fingerprint changed since the measurement: re-measure", rec.PromptFingerprint, want) // (4)
	}
	if err := validateEvidenceArms(rec.Arms); err != nil {
		return fail("measurement: %v", err) // (6)
	}
	for _, row := range rows {
		if err := checkArmCases(rec.Arms[row.name].PerCase, row); err != nil {
			return fail("%v", err) // (6b), (7)
		}
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

// CatchRateArmRule states, on one line, the per-arm half of the rule the
// offline gate applies to the arm-keyed record: every arm, per expectation
// class, needs the trial floor and is judged against the SAME arm of the
// pinned baseline on both its catch rate and its false-alarm rate.
// catchrategate prints it beside CatchRateRule on every outcome.
func CatchRateArmRule() string {
	return fmt.Sprintf("per-arm rule: every arm (%s, %s, %s and any later arm-table row) needs at least %d trials in EACH expectation class "+
		"(catch, clean) and FAILS when its catch rate is more than %.2f below, or its false-alarm rate ((false_alarms + undecodable) / clean trials) "+
		"more than %.2f above, the same arm of the pinned baseline",
		ArmWithoutConventions, ArmWithConventions, ArmArchitect, MinCatchRateTrialsPerArm, DefaultCatchRateRegressionTolerance, DefaultCatchRateRegressionTolerance)
}

// checkArmSet is mode (7b): the record's arms are exactly the arm table's,
// each with the table's kind.
func checkArmSet(arms CatchRateEvidenceArms, rows []catchRateArmRow) error {
	known := make(map[string]bool, len(rows))
	for _, row := range rows {
		known[row.name] = true
		a, ok := arms[row.name]
		if !ok {
			return fmt.Errorf("arm %q is missing from the record: every arm of the arm table is recorded together — re-measure", row.name)
		}
		if a.Kind != row.kind {
			return fmt.Errorf("arm %q kind %q is not the arm table's %q", row.name, a.Kind, row.kind)
		}
	}
	for _, name := range sortedArmNames(arms) {
		if !known[name] {
			return fmt.Errorf("unknown arm %q: the arm table has no such row", name)
		}
	}
	return nil
}

// checkArmCases is modes (6b) and (7) for one arm: the recorded case set
// equals the arm table's, and each case's expect equals the corpus's.
func checkArmCases(perCase map[string]CaseCatchCounts, row catchRateArmRow) error {
	inCorpus := make(map[string]string, len(row.cases))
	var missing, extra []string
	for _, c := range row.cases {
		inCorpus[c.name] = c.expect
		if _, ok := perCase[c.name]; !ok {
			missing = append(missing, c.name)
		}
	}
	for _, name := range sortedCaseNames(perCase) {
		if _, ok := inCorpus[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("arm %q case set differs from the current corpus (missing %v, unknown %v): re-measure", row.name, missing, extra) // (7)
	}
	for _, name := range sortedCaseNames(perCase) {
		if got, want := countsExpect(perCase[name]), inCorpus[name]; got != want {
			return fmt.Errorf("arm %q case %q records expect %q, the corpus %q", row.name, name, got, want) // (6b)
		}
	}
	return nil
}

// countsExpect is a case's expectation class, with the v4 empty = catch.
func countsExpect(c CaseCatchCounts) string {
	if c.Expect == "" {
		return CatchExpectCatch
	}
	return c.Expect
}

// checkCatchRateTrials is modes (13)-(16): the sidecar exists, decodes
// strictly under its schema, belongs to this recording, and agrees with the
// record's counts. Each mode-16 sub-check is independent of the others on
// its own fixture: the arm and case sets are compared first (an extra
// sidecar case fails only that — the later checks walk the RECORD's cases),
// the indices are checked against the list's own length (a dropped trial
// fails only the length check) and the tallies count all five outcomes
// against the list's own length (a duplicated index fails only the index
// check).
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
	if len(trials.Arms) != len(rec.Arms) {
		return fmt.Errorf("arm set %v differs from the record's %v", sortedTrialArmNames(trials.Arms), sortedArmNames(rec.Arms)) // (16) arm set
	}
	for _, armName := range sortedArmNames(rec.Arms) {
		ta, ok := trials.Arms[armName]
		if !ok {
			return fmt.Errorf("arm set %v differs from the record's %v", sortedTrialArmNames(trials.Arms), sortedArmNames(rec.Arms)) // (16) arm set
		}
		counts := rec.Arms[armName].PerCase
		if !sameTrialCaseSet(counts, ta.PerCase) {
			return fmt.Errorf("arm %q case set %v differs from the record's %v", armName, sortedTrialCaseNames(ta.PerCase), sortedCaseNames(counts)) // (16) case set
		}
		for _, name := range sortedCaseNames(counts) {
			c, list := counts[name], ta.PerCase[name]
			if len(list) != c.Trials {
				return fmt.Errorf("arm %q case %q lists %d trials, the record %d", armName, name, len(list), c.Trials) // (16) length
			}
			for i, tr := range list {
				if tr.Trial != i+1 {
					return fmt.Errorf("arm %q case %q entry %d has trial index %d, want %d (indices must be exactly 1..%d in order)", armName, name, i, tr.Trial, i+1, len(list)) // (16) indices
				}
			}
			tally := map[CatchOutcome]int{}
			for _, tr := range list {
				tally[tr.Outcome]++
			}
			inClass := tally[CatchCaught] + tally[CatchMissed] + tally[CatchUndecodable]
			if c.IsClean() {
				inClass = tally[CatchFalseAlarm] + tally[CatchNoFalseAlarm] + tally[CatchUndecodable]
			}
			if tally[CatchCaught] != c.Caught || tally[CatchFalseAlarm] != c.FalseAlarms || tally[CatchUndecodable] != c.Undecodable || inClass != len(list) {
				return fmt.Errorf("arm %q case %q outcome tallies (caught %d, missed %d, false_alarm %d, no_false_alarm %d, undecodable %d, of %d) differ from the record (expect %s, caught %d, false_alarms %d, undecodable %d)",
					armName, name, tally[CatchCaught], tally[CatchMissed], tally[CatchFalseAlarm], tally[CatchNoFalseAlarm], tally[CatchUndecodable], len(list),
					countsExpect(c), c.Caught, c.FalseAlarms, c.Undecodable) // (16) tallies
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

func sortedArmNames(m CatchRateEvidenceArms) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedTrialArmNames(m CatchRateTrialsArms) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateEvidenceArms is the count-invariant check (mode 6), in an order that
// lets each rule fire on its own fixture: per arm the samples and kind, then
// per-case counts, then each comparison pair's agreement on the case set and
// the per-case trial weights, then trials == the arm's samples.
func validateEvidenceArms(arms CatchRateEvidenceArms) error {
	if len(arms) == 0 {
		return fmt.Errorf("no arms recorded")
	}
	names := sortedArmNames(arms)
	for _, name := range names {
		a := arms[name]
		if a.SamplesPerCase < 1 {
			return fmt.Errorf("arm %q samples_per_case %d must be >= 1", name, a.SamplesPerCase)
		}
		if a.Kind != CatchRateArmKindConventions && a.Kind != CatchRateArmKindPersona {
			return fmt.Errorf("arm %q kind %q is not %q or %q", name, a.Kind, CatchRateArmKindConventions, CatchRateArmKindPersona)
		}
		for _, c := range sortedCaseNames(a.PerCase) {
			if err := a.PerCase[c].validate(); err != nil {
				return fmt.Errorf("inconsistent counts in arm %q case %q: %w", name, c, err)
			}
		}
	}
	for _, pair := range catchRateComparisonPairs {
		w, wok := arms[pair.without]
		h, hok := arms[pair.with]
		if !wok || !hok {
			return fmt.Errorf("the comparison pair (%q, %q) is incomplete: both arms are required", pair.without, pair.with)
		}
		for _, name := range sortedCaseNames(w.PerCase) {
			if _, ok := h.PerCase[name]; !ok {
				return fmt.Errorf("the arms disagree on the case set: %q is in %q but not %q", name, pair.without, pair.with)
			}
		}
		for _, name := range sortedCaseNames(h.PerCase) {
			if _, ok := w.PerCase[name]; !ok {
				return fmt.Errorf("the arms disagree on the case set: %q is in %q but not %q", name, pair.with, pair.without)
			}
		}
		for _, name := range sortedCaseNames(w.PerCase) {
			if w.PerCase[name].Trials != h.PerCase[name].Trials {
				return fmt.Errorf("the arms disagree on the trial weight of case %q (%d without, %d with)", name, w.PerCase[name].Trials, h.PerCase[name].Trials)
			}
		}
	}
	for _, name := range names {
		a := arms[name]
		for _, c := range sortedCaseNames(a.PerCase) {
			if t := a.PerCase[c].Trials; t != a.SamplesPerCase {
				return fmt.Errorf("arm %q case %q has %d trials, not samples_per_case %d", name, c, t, a.SamplesPerCase)
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
	if err := validateEvidenceArms(b.Arms); err != nil {
		return fmt.Errorf("pinned baseline: %w", err)
	}
	return nil
}

// baselineComparable reports why a measurement cannot be judged against a
// baseline: a different generator model, arm set, or — per arm — case set
// (or expectation class) or samples_per_case makes the pooled rates a
// different instrument.
func baselineComparable(rec CatchRateEvidence, b *CatchRateBaseline) (bool, string) {
	if rec.GeneratorModel != b.GeneratorModel {
		return false, fmt.Sprintf("generator model changed (%q pinned, %q measured)", b.GeneratorModel, rec.GeneratorModel)
	}
	if got, want := sortedArmNames(rec.Arms), sortedArmNames(b.Arms); !slices.Equal(got, want) {
		return false, fmt.Sprintf("arm set changed (%v pinned, %v measured)", want, got)
	}
	for _, name := range sortedArmNames(rec.Arms) {
		m, p := rec.Arms[name], b.Arms[name]
		if !sameCaseNames(m.PerCase, p.PerCase) {
			return false, fmt.Sprintf("arm %q corpus changed (the case sets differ)", name)
		}
		for c, counts := range m.PerCase {
			if countsExpect(counts) != countsExpect(p.PerCase[c]) {
				return false, fmt.Sprintf("arm %q case %q expectation class changed", name, c)
			}
		}
		if m.SamplesPerCase != p.SamplesPerCase {
			return false, fmt.Sprintf("arm %q samples_per_case changed (%d pinned, %d measured)", name, p.SamplesPerCase, m.SamplesPerCase)
		}
	}
	return true, ""
}

// classCounts pools one arm's counts per expectation class: catch trials and
// caught, and clean trials and alarms (false_alarms + undecodable — an
// undecodable clean trial counts as a false alarm, the fail-safe direction).
type classCounts struct {
	catchTrials, caught int
	cleanTrials, alarms int
}

func pooledClassCounts(perCase map[string]CaseCatchCounts) classCounts {
	var out classCounts
	for _, c := range perCase {
		if c.IsClean() {
			out.cleanTrials += c.Trials
			out.alarms += c.FalseAlarms + c.Undecodable
			continue
		}
		out.catchTrials += c.Trials
		out.caught += c.Caught
	}
	return out
}

// judgeCatchRate is THE gate rule, shared by the recorder and the checker. It
// applies the within-run rule to every comparison pair (CompareCatchRateArms:
// power floor, then the tolerance), then the per-arm, per-expectation-class
// power floor, then — when ref is non-nil — the pinned-baseline rule: FAIL
// when any arm's catch rate is more than the tolerance below, or its
// false-alarm rate more than the tolerance above, the SAME arm of the pinned
// baseline, decided in exact rationals. A non-comparable baseline is an error
// unless pinning (the pin replaces it). Regressions wrap
// ErrCatchRateRegressed.
func judgeCatchRate(rec CatchRateEvidence, ref *CatchRateBaseline, pinning bool) (string, error) {
	var b strings.Builder
	for _, pair := range catchRateComparisonPairs {
		wa, ha := rec.Arms[pair.without], rec.Arms[pair.with]
		without := CatchRateArmReport{Arm: pair.without, SamplesPerCase: wa.SamplesPerCase, PerCase: wa.PerCase}
		with := CatchRateArmReport{Arm: pair.with, SamplesPerCase: ha.SamplesPerCase, PerCase: ha.PerCase}
		cmp, err := CompareCatchRateArms(without, with, DefaultCatchRateRegressionTolerance, MinCatchRateTrialsPerArm)
		if err != nil {
			return b.String(), err
		}
		b.WriteString(cmp.Render())
		if cmp.Regressed {
			return b.String(), fmt.Errorf("%w: the with-conventions arm is %+.3f below the without-conventions arm, more than the %.2f tolerance", ErrCatchRateRegressed, cmp.Delta, DefaultCatchRateRegressionTolerance)
		}
	}
	b.WriteString("\nPer arm and expectation class (catch rate = caught / trials; false-alarm rate = (false_alarms + undecodable) / trials):\n")
	for _, name := range sortedArmNames(rec.Arms) {
		cc := pooledClassCounts(rec.Arms[name].PerCase)
		for _, class := range []struct {
			label  string
			trials int
		}{{CatchExpectCatch, cc.catchTrials}, {CatchExpectClean, cc.cleanTrials}} {
			if class.trials > 0 && class.trials < MinCatchRateTrialsPerArm {
				return b.String(), fmt.Errorf("agenteval: catch arm %q %s class is under-powered: %d trials < %d: raise samples; do not widen the tolerance", name, class.label, class.trials, MinCatchRateTrialsPerArm)
			}
		}
		if cc.catchTrials > 0 {
			fmt.Fprintf(&b, "  %s: catch %d/%d (%.3f)", name, cc.caught, cc.catchTrials, float64(cc.caught)/float64(cc.catchTrials))
		} else {
			fmt.Fprintf(&b, "  %s: catch n/a", name)
		}
		if cc.cleanTrials > 0 {
			fmt.Fprintf(&b, ", false alarm %d/%d (%.3f)", cc.alarms, cc.cleanTrials, float64(cc.alarms)/float64(cc.cleanTrials))
		}
		b.WriteString("\n")
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
	fmt.Fprintf(&b, "\nPinned baseline (pinned %s, reason: %s). Rule: FAIL when any arm's catch rate is MORE than %.2f below, or its false-alarm rate MORE than %.2f above, the SAME arm of the pinned baseline — the reference does not roll forward with each recording.\n",
		ref.PinnedAt, ref.Reason, DefaultCatchRateRegressionTolerance, DefaultCatchRateRegressionTolerance)
	tol := new(big.Rat).SetFloat64(DefaultCatchRateRegressionTolerance)
	var regressed []string
	for _, name := range sortedArmNames(rec.Arms) {
		m, p := pooledClassCounts(rec.Arms[name].PerCase), pooledClassCounts(ref.Arms[name].PerCase)
		if m.catchTrials > 0 && p.catchTrials > 0 {
			drop := new(big.Rat).Sub(big.NewRat(int64(p.caught), int64(p.catchTrials)), big.NewRat(int64(m.caught), int64(m.catchTrials)))
			dropF, _ := drop.Float64()
			fmt.Fprintf(&b, "  %s: pinned %d/%d (%.3f), measured %d/%d (%.3f), drop %+.3f\n",
				name, p.caught, p.catchTrials, float64(p.caught)/float64(p.catchTrials), m.caught, m.catchTrials, float64(m.caught)/float64(m.catchTrials), dropF)
			if drop.Cmp(tol) > 0 {
				regressed = append(regressed, fmt.Sprintf("%s catch rate %+.3f", name, -dropF))
			}
		}
		if m.cleanTrials > 0 && p.cleanTrials > 0 {
			rise := new(big.Rat).Sub(big.NewRat(int64(m.alarms), int64(m.cleanTrials)), big.NewRat(int64(p.alarms), int64(p.cleanTrials)))
			riseF, _ := rise.Float64()
			fmt.Fprintf(&b, "  %s false alarms: pinned %d/%d (%.3f), measured %d/%d (%.3f), rise %+.3f\n",
				name, p.alarms, p.cleanTrials, float64(p.alarms)/float64(p.cleanTrials), m.alarms, m.cleanTrials, float64(m.alarms)/float64(m.cleanTrials), riseF)
			if rise.Cmp(tol) > 0 {
				regressed = append(regressed, fmt.Sprintf("%s false-alarm rate %+.3f", name, riseF))
			}
		}
	}
	if len(regressed) > 0 {
		return b.String(), fmt.Errorf("%w against the pinned baseline (%s): %s, more than the %.2f tolerance", ErrCatchRateRegressed, ref.PinnedAt, strings.Join(regressed, ", "), DefaultCatchRateRegressionTolerance)
	}
	return b.String(), nil
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
