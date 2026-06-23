package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// stubJudge is a programmable Judge for the calibration tests: fn maps a
// trajectory to its (card, error) so a test can drive perfect agreement,
// a deliberate offset, or a per-case error.
type stubJudge struct {
	fn func([]bundle.Line) (JudgeCard, error)
}

func (s stubJudge) Judge(_ context.Context, lines []bundle.Line) (JudgeCard, error) {
	return s.fn(lines)
}

// cardFromLabels builds a JudgeCard whose scores equal the human labels
// (rationale text is irrelevant to the agreement metric).
func cardFromLabels(hl HumanLabels) JudgeCard {
	return JudgeCard{
		MeaningfulEvidence: DimensionScore{Score: hl.MeaningfulEvidence},
		HonestUncertainty:  DimensionScore{Score: hl.HonestUncertainty},
		ReasoningQuality:   DimensionScore{Score: hl.ReasoningQuality},
		Model:              "stub",
	}
}

func mkCase(name string, me, hu, rq int) CalibrationCase {
	return CalibrationCase{
		Name:   name,
		Lines:  []bundle.Line{{Seq: 1, Kind: bundle.EventKindManifest, Data: json.RawMessage(`{"run_id":"` + name + `"}`)}},
		Labels: HumanLabels{MeaningfulEvidence: me, HonestUncertainty: hu, ReasoningQuality: rq, Synthetic: true},
	}
}

// TestCalibratePerfectAgreement: a judge that exactly echoes the labels
// yields OverallWithin1 == 1.0 and Trusted true above the threshold.
func TestCalibratePerfectAgreement(t *testing.T) {
	cases := []CalibrationCase{mkCase("a", 5, 4, 5), mkCase("b", 1, 2, 2)}
	judge := stubJudge{fn: func(lines []bundle.Line) (JudgeCard, error) {
		for _, c := range cases {
			if string(c.Lines[0].Data) == string(lines[0].Data) {
				return cardFromLabels(c.Labels), nil
			}
		}
		return JudgeCard{}, errors.New("unknown case")
	}}
	report, err := Calibrate(context.Background(), judge, cases, 0.8)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if !report.Trusted {
		t.Errorf("want Trusted=true, report=%+v", report)
	}
	if report.OverallWithin1 != 1.0 {
		t.Errorf("OverallWithin1 = %v, want 1.0", report.OverallWithin1)
	}
	if report.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2", report.SampleCount)
	}
}

// TestCalibrateBelowThreshold: a judge offset by 3 (beyond within-1)
// drives agreement to 0 and Trusted false.
func TestCalibrateBelowThreshold(t *testing.T) {
	cases := []CalibrationCase{mkCase("a", 5, 5, 5), mkCase("b", 1, 1, 1)}
	judge := stubJudge{fn: func(lines []bundle.Line) (JudgeCard, error) {
		// Offset every dimension toward the middle so |judge-human| >= 3.
		return JudgeCard{
			MeaningfulEvidence: DimensionScore{Score: 2},
			HonestUncertainty:  DimensionScore{Score: 2},
			ReasoningQuality:   DimensionScore{Score: 2},
		}, nil
	}}
	report, err := Calibrate(context.Background(), judge, cases, 0.8)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	// case a: |2-5|=3 (no), case b: |2-1|=1 (within1). 3 of 6 within-1.
	if report.Trusted {
		t.Errorf("want Trusted=false, report=%+v", report)
	}
	if math.Abs(report.OverallWithin1-0.5) > 1e-9 {
		t.Errorf("OverallWithin1 = %v, want 0.5", report.OverallWithin1)
	}
}

// TestCalibrateAgreementMath pins within-1 vs exact-match on a single
// hand-built case: one exact dimension, one within-1-not-exact, one
// neither.
func TestCalibrateAgreementMath(t *testing.T) {
	cases := []CalibrationCase{mkCase("a", 3, 3, 3)}
	judge := stubJudge{fn: func(lines []bundle.Line) (JudgeCard, error) {
		return JudgeCard{
			MeaningfulEvidence: DimensionScore{Score: 3}, // exact
			HonestUncertainty:  DimensionScore{Score: 4}, // within-1, not exact
			ReasoningQuality:   DimensionScore{Score: 5}, // neither (|5-3|=2)
		}, nil
	}}
	report, err := Calibrate(context.Background(), judge, cases, 0.5)
	if err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	checks := []struct {
		name       string
		got        DimensionAgreement
		wantExact  float64
		wantWithin float64
	}{
		{"meaningful_evidence", report.MeaningfulEvidence, 1.0, 1.0},
		{"honest_uncertainty", report.HonestUncertainty, 0.0, 1.0},
		{"reasoning_quality", report.ReasoningQuality, 0.0, 0.0},
	}
	for _, c := range checks {
		if c.got.ExactMatchRate != c.wantExact || c.got.Within1Rate != c.wantWithin {
			t.Errorf("%s = %+v, want exact=%v within1=%v", c.name, c.got, c.wantExact, c.wantWithin)
		}
	}
	// Overall within-1 = (1+1+0)/3.
	if math.Abs(report.OverallWithin1-2.0/3.0) > 1e-9 {
		t.Errorf("OverallWithin1 = %v, want 0.6667", report.OverallWithin1)
	}
}

// TestCalibrateFailsClosedOnJudgeError: a case whose judge errors is NOT
// dropped — it stays in the denominator and counts as full disagreement,
// so the run cannot pass by attrition.
func TestCalibrateFailsClosedOnJudgeError(t *testing.T) {
	good := mkCase("good", 5, 5, 5)
	bad := mkCase("bad", 5, 5, 5)
	cases := []CalibrationCase{good, bad}
	judge := stubJudge{fn: func(lines []bundle.Line) (JudgeCard, error) {
		if string(lines[0].Data) == string(bad.Lines[0].Data) {
			return JudgeCard{}, errors.New("judge transport boom")
		}
		return cardFromLabels(good.Labels), nil
	}}
	report, err := Calibrate(context.Background(), judge, cases, 0.8)
	if err != nil {
		t.Fatalf("Calibrate must not error on a per-case judge error: %v", err)
	}
	if report.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2 (errored case NOT dropped)", report.SampleCount)
	}
	// good case: 3/3 within-1; bad case: 0/3. Overall 3/6 = 0.5 < 0.8.
	if report.Trusted {
		t.Errorf("want Trusted=false (fail-closed), report=%+v", report)
	}
	if math.Abs(report.OverallWithin1-0.5) > 1e-9 {
		t.Errorf("OverallWithin1 = %v, want 0.5", report.OverallWithin1)
	}
}

// TestCalibrateEmptyCases: an empty case set is a harness error.
func TestCalibrateEmptyCases(t *testing.T) {
	if _, err := Calibrate(context.Background(), stubJudge{}, nil, 0.8); err == nil {
		t.Fatal("want error on empty case set, got nil")
	}
}

// TestCalibrateCorpusReplay is the cross-boundary test: it loads all
// four testdata/corpus/<case>/human_labels.json fixtures from disk
// alongside their trace.jsonl, runs a stub Judge through Calibrate, and
// asserts the end-to-end report — proving the fixture-load -> judge ->
// calibrate -> report seam holds together (not just the per-layer
// units). The seam crosses the corpus-fixture persistence layer, the
// JudgeCard domain type, and the agreement-scoring consumer.
func TestCalibrateCorpusReplay(t *testing.T) {
	cases := loadCorpusCases(t)
	if len(cases) < 4 {
		t.Fatalf("want at least the 4 labeled seed corpus cases, got %d", len(cases))
	}
	for _, c := range cases {
		if !c.Labels.Synthetic {
			t.Errorf("corpus case %q labels must be marked Synthetic", c.Name)
		}
	}

	// A judge that echoes each case's labels (matched by trajectory) →
	// perfect agreement → Trusted true. The point is the seam runs, not
	// discrimination (covered by the offset/math tests above).
	judge := stubJudge{fn: func(lines []bundle.Line) (JudgeCard, error) {
		for _, c := range cases {
			if traceRunID(c.Lines) == traceRunID(lines) {
				return cardFromLabels(c.Labels), nil
			}
		}
		return JudgeCard{}, errors.New("no matching corpus case")
	}}

	report, err := Calibrate(context.Background(), judge, cases, 0.8)
	if err != nil {
		t.Fatalf("Calibrate corpus replay: %v", err)
	}
	if report.SampleCount != 4 {
		t.Errorf("SampleCount = %d, want 4", report.SampleCount)
	}
	if !report.Trusted || report.OverallWithin1 != 1.0 {
		t.Errorf("want Trusted with OverallWithin1=1.0, report=%+v", report)
	}
}

// TestCalibrateLive is the opt-in live-model calibration. It is SKIPPED
// unless BOTH FISHHAWK_AGENTEVAL_JUDGE_LIVE and FISHHAWKD_ANTHROPIC_API_KEY
// are set, so the committed-tree verify / CI never makes a live model
// call. Passing *anthropic.Client where MessageSender is expected is
// also the compile-time proof the SDK adapter's signature has not
// drifted from the interface.
func TestCalibrateLive(t *testing.T) {
	if os.Getenv("FISHHAWK_AGENTEVAL_JUDGE_LIVE") == "" {
		t.Skip("set FISHHAWK_AGENTEVAL_JUDGE_LIVE=1 to run the live judge calibration")
	}
	apiKey := os.Getenv("FISHHAWKD_ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("FISHHAWKD_ANTHROPIC_API_KEY unset; skipping live judge calibration")
	}

	client := anthropic.NewClient(anthropic.Config{
		APIKey:    apiKey,
		Model:     DefaultJudgeModel,
		MaxTokens: 1024,
		Timeout:   60 * time.Second,
		// Pin JudgeCardSchema so the live judge issues the schema-constrained
		// request (#1326), exercising the same output_config.format path the
		// committed-tree integration test asserts.
		Schema: JudgeCardSchema(),
	})
	// Passing *anthropic.Client to NewLLMJudge's MessageSender parameter
	// is the compile-time proof the SDK adapter's signature still matches.
	judge := NewLLMJudge(client, DefaultJudgeModel, 2)

	cases := loadCorpusCases(t)
	report, err := Calibrate(context.Background(), judge, cases, 0.6)
	if err != nil {
		t.Fatalf("live Calibrate: %v", err)
	}
	t.Logf("live calibration report (synthetic labels): %+v", report)
}

// traceRunID extracts the manifest run_id from a trajectory; "" if absent.
func traceRunID(lines []bundle.Line) string {
	for _, l := range lines {
		if l.Kind != bundle.EventKindManifest {
			continue
		}
		var m struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal(l.Data, &m)
		return m.RunID
	}
	return ""
}

// loadCorpusCases reads every testdata/corpus/<case>/ directory into a
// CalibrationCase (trace.jsonl + human_labels.json).
func loadCorpusCases(t *testing.T) []CalibrationCase {
	t.Helper()
	const corpusDir = "testdata/corpus"
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("read corpus dir: %v", err)
	}
	var cases []CalibrationCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(corpusDir, e.Name())
		labelsPath := filepath.Join(dir, "human_labels.json")
		if _, statErr := os.Stat(labelsPath); errors.Is(statErr, os.ErrNotExist) {
			// Tier-A-only corpus case: it carries expected.json (exercised by
			// the scorer's TestScore) but no Tier-B human_labels.json yet, so
			// it is not part of the judge-calibration subset. Skip it rather
			// than forcing every corpus case to be Tier-B-labeled — this
			// decouples Tier-A corpus growth (#819) from Tier-B labeling and
			// is why a labels-free case (e.g. a freshly distilled real trace)
			// must not break calibration replay (#1174 / #1298).
			continue
		}
		labels, err := loadHumanLabels(labelsPath)
		if err != nil {
			t.Fatalf("load labels for %s: %v", e.Name(), err)
		}
		cases = append(cases, CalibrationCase{
			Name:   e.Name(),
			Lines:  readTraceLines(t, filepath.Join(dir, "trace.jsonl")),
			Labels: labels,
		})
	}
	return cases
}
