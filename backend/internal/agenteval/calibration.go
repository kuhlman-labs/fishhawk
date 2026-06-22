package agenteval

// Calibration harness for the Tier-B judge (#820). It scores the judge's
// output against a human-labeled subset and gates a trusted/not-trusted
// verdict on a configurable agreement threshold, mirroring the Tier-A
// bootstrap discipline. No real labeled corpus exists yet, so the seed
// human labels are SYNTHETIC (HumanLabels.Synthetic) — the harness
// proves the mechanism + discrimination; replacing the synthetic labels
// with captured, labeled production traces is the operator-triaged
// deferred follow-up (docs/architecture/agent-eval.md).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// HumanLabels is one trajectory's human-assigned dimension scores — the
// ground truth the judge is calibrated against. The dimension fields are
// bare ordinal scores (1-5); unlike a JudgeCard they carry no rationale.
type HumanLabels struct {
	// MeaningfulEvidence is the human score for the evidence-inspection
	// dimension (1-5). Same axis as JudgeCard.MeaningfulEvidence.Score.
	MeaningfulEvidence int `json:"meaningful_evidence"`
	// HonestUncertainty is the human score for the honesty dimension.
	HonestUncertainty int `json:"honest_uncertainty"`
	// ReasoningQuality is the human score for the reasoning dimension.
	ReasoningQuality int `json:"reasoning_quality"`
	// Labeler identifies who (or what) assigned the labels.
	Labeler string `json:"labeler"`
	// Notes records labeling context — for the seed set, that the labels
	// are synthetic and why each score was chosen.
	Notes string `json:"notes"`
	// Synthetic is true for hand-authored bootstrap labels (no real
	// labeled corpus exists yet). Real captured labels set this false.
	Synthetic bool `json:"synthetic"`
}

// CalibrationCase pairs a parsed trajectory with its human labels.
type CalibrationCase struct {
	// Name identifies the case (the corpus directory name).
	Name string
	// Lines is the parsed trajectory the judge scores.
	Lines []bundle.Line
	// Labels is the human ground truth for this case.
	Labels HumanLabels
}

// DimensionAgreement is one dimension's agreement between the judge and
// the human labels across all calibration cases.
type DimensionAgreement struct {
	// ExactMatchRate is the fraction of cases where the judge's score
	// equals the human score exactly.
	ExactMatchRate float64 `json:"exact_match_rate"`
	// Within1Rate is the fraction of cases where the judge's score is
	// within 1 of the human score (|judge - human| <= 1).
	Within1Rate float64 `json:"within1_rate"`
}

// CalibrationReport is the outcome of a calibration run: per-dimension
// agreement, the overall within-1 agreement the trust gate reads, the
// sample count, the threshold applied, and the trusted/not-trusted
// verdict.
type CalibrationReport struct {
	// MeaningfulEvidence, HonestUncertainty, ReasoningQuality are the
	// per-dimension agreement rates.
	MeaningfulEvidence DimensionAgreement `json:"meaningful_evidence"`
	HonestUncertainty  DimensionAgreement `json:"honest_uncertainty"`
	ReasoningQuality   DimensionAgreement `json:"reasoning_quality"`
	// OverallWithin1 is the within-1 agreement across all
	// dimension-comparisons (SampleCount * 3). The trust gate reads this.
	OverallWithin1 float64 `json:"overall_within1"`
	// SampleCount is the number of calibration cases scored.
	SampleCount int `json:"sample_count"`
	// Threshold is the OverallWithin1 value at/above which Trusted is set.
	// A configurable parameter, NOT a hardcoded CI gate.
	Threshold float64 `json:"threshold"`
	// Trusted is OverallWithin1 >= Threshold.
	Trusted bool `json:"trusted"`
}

// loadHumanLabels reads a testdata/corpus/<case>/human_labels.json file.
func loadHumanLabels(path string) (HumanLabels, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return HumanLabels{}, fmt.Errorf("read human labels %s: %w", path, err)
	}
	var hl HumanLabels
	if err := json.Unmarshal(b, &hl); err != nil {
		return HumanLabels{}, fmt.Errorf("parse human labels %s: %w", path, err)
	}
	return hl, nil
}

// Calibrate scores judge over cases and reports per-dimension and
// overall within-1 agreement against the human labels. Trusted is set
// when OverallWithin1 >= threshold.
//
// Fail-closed on a per-case judge error: the erroring case is NOT
// silently dropped — it stays in the denominator and counts as a
// disagreement on every dimension, so a flaky judge cannot pass
// calibration by attrition. The judge error does not abort Calibrate;
// only an empty case set is a harness-level error.
func Calibrate(ctx context.Context, judge Judge, cases []CalibrationCase, threshold float64) (CalibrationReport, error) {
	if len(cases) == 0 {
		return CalibrationReport{}, fmt.Errorf("agenteval: calibrate needs at least one case")
	}

	var meExact, meW1, huExact, huW1, rqExact, rqW1 int
	for _, c := range cases {
		card, err := judge.Judge(ctx, c.Lines)
		if err != nil {
			// Fail-closed: an errored case counts as a full disagreement
			// (no exact, no within-1) on every dimension. Do not drop it.
			continue
		}
		me := agree(card.MeaningfulEvidence.Score, c.Labels.MeaningfulEvidence)
		hu := agree(card.HonestUncertainty.Score, c.Labels.HonestUncertainty)
		rq := agree(card.ReasoningQuality.Score, c.Labels.ReasoningQuality)
		meExact += me.exact
		meW1 += me.within1
		huExact += hu.exact
		huW1 += hu.within1
		rqExact += rq.exact
		rqW1 += rq.within1
	}

	n := len(cases)
	report := CalibrationReport{
		MeaningfulEvidence: DimensionAgreement{ExactMatchRate: rate(meExact, n), Within1Rate: rate(meW1, n)},
		HonestUncertainty:  DimensionAgreement{ExactMatchRate: rate(huExact, n), Within1Rate: rate(huW1, n)},
		ReasoningQuality:   DimensionAgreement{ExactMatchRate: rate(rqExact, n), Within1Rate: rate(rqW1, n)},
		OverallWithin1:     rate(meW1+huW1+rqW1, n*3),
		SampleCount:        n,
		Threshold:          threshold,
	}
	report.Trusted = report.OverallWithin1 >= threshold
	return report, nil
}

// agreement is the per-comparison exact/within-1 tally (0 or 1 each).
type agreement struct {
	exact   int
	within1 int
}

// agree compares a judge score to a human score, reporting exact-match
// and within-1 (|judge - human| <= 1) as 0/1 counters.
func agree(judge, human int) agreement {
	d := judge - human
	if d < 0 {
		d = -d
	}
	a := agreement{}
	if d == 0 {
		a.exact = 1
	}
	if d <= 1 {
		a.within1 = 1
	}
	return a
}

// rate is num/den as a float64; 0 when den is 0 (never the live path —
// Calibrate rejects an empty case set).
func rate(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}
