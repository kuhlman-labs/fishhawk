package agenteval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// severityCalibrationCorpusDir is the committed labelled corpus.
const severityCalibrationCorpusDir = "testdata/severity-calibration-corpus"

// fakeCalibrationSender is this file's OWN MessageSender fake (the existing
// injection / envelope-quality / judge fakes live in unscoped test files).
// It replays a fixed queue of verdict JSON responses.
type fakeCalibrationSender struct {
	responses []string
	calls     int
	err       error
}

func (f *fakeCalibrationSender) Messages(_ context.Context, _, _ string) (string, string, int, int, int, int, error) {
	if f.err != nil {
		return "", "", 0, 0, 0, 0, f.err
	}
	i := f.calls
	f.calls++
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	return f.responses[i], "fake-model", 0, 0, 0, 0, nil
}

// ---------------------------------------------------------------------------
// Treatment-set enumeration.
// ---------------------------------------------------------------------------

// TestTreatmentLiteralCount_IsFiveAndEnumerated pins the #2119 treatment set
// at exactly FIVE literals with distinct names, probes and anchors, and pins
// that exactly one of them is the guard-conditional re-read bullet. Deleting
// a literal from the table — the HIGH-1 regression — goes red here
// independently of the render tests.
func TestTreatmentLiteralCount_IsFiveAndEnumerated(t *testing.T) {
	if got := len(treatmentLiterals); got != 5 {
		t.Fatalf("treatment set must have exactly 5 literals, got %d (%v)", got, TreatmentLiteralNames())
	}
	wantNames := []string{
		"standing-criterion-9-lead",
		"standing-criterion-10-lead",
		"adversarial-carve-out",
		"severity-calibration-rubric",
		"re-read-before-reopen-bullet",
	}
	got := TreatmentLiteralNames()
	for i, want := range wantNames {
		if got[i] != want {
			t.Errorf("treatment literal %d: got %q, want %q", i, got[i], want)
		}
	}
	seenProbe, seenAnchor, guarded := map[string]bool{}, map[string]bool{}, 0
	for _, l := range treatmentLiterals {
		if l.Probe == "" || l.Anchor == "" {
			t.Errorf("treatment literal %q has an empty probe or anchor", l.Name)
		}
		if !strings.Contains(l.Anchor, l.Probe) {
			t.Errorf("treatment literal %q: probe is not a substring of its anchor", l.Name)
		}
		if seenProbe[l.Probe] || seenAnchor[l.Anchor] {
			t.Errorf("treatment literal %q duplicates another entry's probe or anchor", l.Name)
		}
		seenProbe[l.Probe], seenAnchor[l.Anchor] = true, true
		if l.GuardConditional {
			guarded++
		}
	}
	if guarded != 1 {
		t.Errorf("exactly one treatment literal must be guard-conditional, got %d", guarded)
	}
}

// ---------------------------------------------------------------------------
// The strip against a REAL built prompt — the drift detector.
// ---------------------------------------------------------------------------

func calibrationTestTrigger(reviewTreeCommit string, priorConcerns bool) prompt.Trigger {
	c := SeverityCalibrationCase{
		Name:        "drift-detector",
		IssueNumber: 3309,
		Diff:        "diff --git a/a.go b/a.go\n+// noop\n",
		PlanSummary: "A small change.",
		ScopeFiles:  []string{"a.go"},
	}
	if priorConcerns {
		c.PriorConcerns = []PriorConcernFixture{{
			ID: "c-1", State: "addressed_pending", Severity: "medium",
			Category: "correctness", Note: "The earlier round left the handle open.",
		}}
	}
	tr := ToCalibrationTrigger(c)
	tr.ReviewTreeCommit = reviewTreeCommit
	return tr
}

// TestStripCalibrationCriteria_AcceptsRealPromptOutput is the DRIFT DETECTOR:
// it runs the strip over a genuinely built implement-review prompt in ALL
// FOUR postures (ReviewTreeCommit empty/non-empty x PriorConcerns
// empty/non-empty), because criteria 9 and 10 render a different second half
// in each grounding posture and the re-read bullet renders in only one
// prior-concerns posture. An edit to any of the five literals in prompt.go
// reddens this test until the copies here are updated in lockstep.
func TestStripCalibrationCriteria_AcceptsRealPromptOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit string
		prior  bool
	}{
		{"ungrounded/no-prior-concerns", "", false},
		{"ungrounded/prior-concerns", "", true},
		{"grounded/no-prior-concerns", "0123456789abcdef", false},
		{"grounded/prior-concerns", "0123456789abcdef", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built, err := prompt.Build("implement_review", calibrationTestTrigger(tc.commit, tc.prior))
			if err != nil {
				t.Fatalf("build implement_review prompt: %v", err)
			}
			stripped, err := StripCalibrationCriteria(built, tc.prior)
			if err != nil {
				t.Fatalf("strip over a real prompt must succeed: %v", err)
			}
			if len(stripped) >= len(built) {
				t.Fatalf("stripped arm (%d bytes) must be shorter than the built prompt (%d bytes)", len(stripped), len(built))
			}
			for _, l := range treatmentLiterals {
				if l.GuardConditional && !tc.prior {
					continue
				}
				if !strings.Contains(built, l.Probe) {
					t.Errorf("post arm must contain treatment literal %q", l.Name)
				}
				if strings.Contains(stripped, l.Probe) {
					t.Errorf("pre arm must NOT contain treatment literal %q", l.Name)
				}
			}
		})
	}
}

// TestCalibrationArms_DifferOnlyInCalibrationWording asserts on SHIPPED
// RENDERED OUTPUT that each of the five treatment literals is wholly absent
// from the PRE arm and present in the POST arm, and that the diff and
// verdict-schema sections are byte-identical between the arms.
func TestCalibrationArms_DifferOnlyInCalibrationWording(t *testing.T) {
	c := SeverityCalibrationCase{
		Name: "arms", IssueNumber: 3309,
		Diff:        "diff --git a/svc.go b/svc.go\n+func Handle() {}\n",
		PlanSummary: "Add a handler.",
		ScopeFiles:  []string{"svc.go"},
		PriorConcerns: []PriorConcernFixture{{
			ID: "c-9", State: "addressed_pending", Note: "Round one left the guard off.",
		}},
	}
	post, err := CalibrationArmPrompt(c, ArmPostCalibration)
	if err != nil {
		t.Fatalf("post arm: %v", err)
	}
	pre, err := CalibrationArmPrompt(c, ArmPreCalibration)
	if err != nil {
		t.Fatalf("pre arm: %v", err)
	}
	for _, l := range treatmentLiterals {
		if !strings.Contains(post, l.Anchor) {
			t.Errorf("post arm must contain the byte-exact anchor for %q", l.Name)
		}
		if strings.Contains(pre, l.Probe) {
			t.Errorf("pre arm must NOT contain treatment literal %q", l.Name)
		}
	}
	// The measured surfaces must be identical between arms.
	if !strings.Contains(pre, c.Diff) || !strings.Contains(post, c.Diff) {
		t.Error("both arms must carry the diff verbatim")
	}
	for _, section := range []string{"### Verdict schema", "### Verdict decision rule"} {
		if !strings.Contains(pre, section) || !strings.Contains(post, section) {
			t.Errorf("both arms must carry %q", section)
		}
	}
	if sectionAfter(pre, "### Verdict schema") != sectionAfter(post, "### Verdict schema") {
		t.Error("the verdict-schema section must be byte-identical between arms")
	}
}

// sectionAfter returns the text from heading up to the next "\n### ".
func sectionAfter(s, heading string) string {
	i := strings.Index(s, heading)
	if i < 0 {
		return ""
	}
	rest := s[i+len(heading):]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestCalibrationArms_ReReadBulletStrippedUnderPriorConcerns is the HIGH-1
// pin. The re-read-before-reopen bullet renders inside the
// len(PriorConcerns) > 0 guard, so a single-posture test would pass on a
// corpus that never populates the guard while the pre arm silently carried
// #2119 treatment.
func TestCalibrationArms_ReReadBulletStrippedUnderPriorConcerns(t *testing.T) {
	base := SeverityCalibrationCase{
		Name: "re-read", IssueNumber: 3309,
		Diff: "diff --git a/a.go b/a.go\n+// x\n", PlanSummary: "x", ScopeFiles: []string{"a.go"},
	}
	withPrior := base
	withPrior.PriorConcerns = []PriorConcernFixture{{
		ID: "c-1", State: "addressed_pending", Note: "Prior round finding.",
	}}

	post, err := CalibrationArmPrompt(withPrior, ArmPostCalibration)
	if err != nil {
		t.Fatalf("post arm with prior concerns: %v", err)
	}
	if !strings.Contains(post, reReadBlock) {
		t.Fatal("the post arm with prior concerns must carry the byte-exact re-read-before-reopen bullet")
	}
	pre, err := CalibrationArmPrompt(withPrior, ArmPreCalibration)
	if err != nil {
		t.Fatalf("pre arm with prior concerns: %v", err)
	}
	if strings.Contains(pre, reReadProbe) {
		t.Error("the pre arm must NOT carry the re-read-before-reopen bullet — it is #2119 treatment")
	}

	// The other posture: the bullet renders in NEITHER arm and the strip
	// must not error.
	postNo, err := CalibrationArmPrompt(base, ArmPostCalibration)
	if err != nil {
		t.Fatalf("post arm without prior concerns: %v", err)
	}
	preNo, err := CalibrationArmPrompt(base, ArmPreCalibration)
	if err != nil {
		t.Fatalf("pre arm without prior concerns must not error: %v", err)
	}
	if strings.Contains(postNo, reReadProbe) || strings.Contains(preNo, reReadProbe) {
		t.Error("without prior concerns the re-read bullet must render in neither arm")
	}
}

// ---------------------------------------------------------------------------
// StripCalibrationCriteria: eight named fail-closed modes.
// ---------------------------------------------------------------------------

// syntheticPrompt assembles a minimal prompt carrying the five treatment
// literals in render order. Each mode test mutates it BY CONSTRUCTION so the
// failure lands on the strip's own branch, never on fixture setup.
func syntheticPrompt(withReRead bool) string {
	var b strings.Builder
	b.WriteString("### Standing criteria\n\n")
	b.WriteString(criterion9Lead)
	b.WriteString("Grounding tail nine.\n")
	b.WriteString(criterion10Lead)
	b.WriteString("Grounding tail ten.\n")
	b.WriteString(carveOutBlock)
	b.WriteString("\n")
	b.WriteString(severityRubricBlock)
	if withReRead {
		b.WriteString("### Prior concerns (delta verification)\n\n")
		b.WriteString(reReadBlock)
	}
	b.WriteString("### Verdict schema\n\n{}\n")
	return b.String()
}

func TestStripCalibrationCriteria_FailClosedModes(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		name   string
		prompt string
		prior  bool
		want   string
	}{
		{
			mode: "a", name: "criterion-9-absent",
			prompt: strings.Replace(syntheticPrompt(false), criterion9Lead, "", 1), prior: false,
			want: `"standing-criterion-9-lead" is absent`,
		},
		{
			mode: "b", name: "criterion-10-absent",
			prompt: strings.Replace(syntheticPrompt(false), criterion10Lead, "", 1), prior: false,
			want: `"standing-criterion-10-lead" is absent`,
		},
		{
			mode: "c", name: "carve-out-absent",
			prompt: strings.Replace(syntheticPrompt(false), carveOutBlock, "", 1), prior: false,
			want: `"adversarial-carve-out" is absent`,
		},
		{
			mode: "d", name: "severity-rubric-absent",
			prompt: strings.Replace(syntheticPrompt(false), severityRubricBlock, "", 1), prior: false,
			want: `"severity-calibration-rubric" is absent`,
		},
		{
			mode: "e", name: "partial-drift-rubric-body",
			// The heading (the probe) SURVIVES; one word of the body is
			// changed. The mutated string is compared against ITSELF as the
			// input, so the byte-exact branch cannot reject it for an
			// unrelated length or ordering reason.
			prompt: strings.Replace(syntheticPrompt(false),
				"- `medium`: reaching it requires a misconfiguration or an unusual wiring.",
				"- `medium`: reaching it requires a misconfiguration or an atypical wiring.", 1),
			prior: false,
			want:  `"severity-calibration-rubric" is present but its body does not match`,
		},
		{
			mode: "f", name: "criterion-10-before-criterion-9",
			prompt: "### Standing criteria\n\n" + criterion10Lead + "Tail ten.\n" +
				criterion9Lead + "Tail nine.\n" + carveOutBlock + "\n" + severityRubricBlock,
			prior: false,
			want:  "standing criterion 10 occurs before standing criterion 9",
		},
		{
			mode: "g", name: "re-read-bullet-absent-while-expected",
			prompt: syntheticPrompt(false), prior: true,
			want: `"re-read-before-reopen-bullet" is ABSENT but the trigger carried prior concerns`,
		},
		{
			mode: "h", name: "re-read-bullet-present-while-not-expected",
			prompt: syntheticPrompt(true), prior: false,
			want: `"re-read-before-reopen-bullet" is PRESENT but the trigger carried no prior concerns`,
		},
	} {
		t.Run("mode-"+tc.mode+"-"+tc.name, func(t *testing.T) {
			out, err := StripCalibrationCriteria(tc.prompt, tc.prior)
			if err == nil {
				t.Fatalf("mode (%s) must fail closed; got a %d-byte stripped arm and no error", tc.mode, len(out))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mode (%s): error %q does not name the mode (want substring %q)", tc.mode, err.Error(), tc.want)
			}
			if out != "" {
				t.Errorf("mode (%s) must return the empty string on failure", tc.mode)
			}
		})
	}
}

// TestStripCalibrationCriteria_SyntheticHappyPath is the control for the
// mode table above: the unmutated synthetic prompt strips cleanly in both
// postures, so each mode's RED is attributable to its own mutation.
func TestStripCalibrationCriteria_SyntheticHappyPath(t *testing.T) {
	for _, prior := range []bool{false, true} {
		out, err := StripCalibrationCriteria(syntheticPrompt(prior), prior)
		if err != nil {
			t.Fatalf("prior=%t: unmutated synthetic prompt must strip cleanly: %v", prior, err)
		}
		for _, l := range treatmentLiterals {
			if l.GuardConditional && !prior {
				continue
			}
			if strings.Contains(out, l.Probe) {
				t.Errorf("prior=%t: %q survived the strip", prior, l.Name)
			}
		}
		if !strings.Contains(out, "### Verdict schema") {
			t.Errorf("prior=%t: the strip must leave untreated sections intact", prior)
		}
	}
}

// ---------------------------------------------------------------------------
// LoadSeverityCalibrationCorpus: ten named shape modes.
// ---------------------------------------------------------------------------

// validCase is the well-formed baseline every loader-mode fixture mutates.
func validCase() SeverityCalibrationCase {
	return SeverityCalibrationCase{
		Name: "valid", Diff: "diff --git a/a.go b/a.go\n+x\n", Synthetic: true,
		Concerns: []LabelledConcern{{
			ConcernID: "c-1", Severity: "high", Category: "security",
			Note: "A reachable cross-tenant read.", Disposition: "addressed", OperatorSeverity: "high",
		}},
	}
}

func writeCalibrationCase(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	caseDir := filepath.Join(dir, name)
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "case.json"), body, 0o644); err != nil {
		t.Fatalf("write case.json: %v", err)
	}
	return caseDir
}

func writeValidCase(t *testing.T, dir, name string, mutate func(*SeverityCalibrationCase)) {
	t.Helper()
	c := validCase()
	if mutate != nil {
		mutate(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeCalibrationCase(t, dir, name, raw)
}

func TestLoadSeverityCalibrationCorpus_FailClosedModes(t *testing.T) {
	t.Run("mode-a-absent-corpus-dir", func(t *testing.T) {
		_, err := LoadSeverityCalibrationCorpus(filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Fatal("an absent corpus dir must be an error, never an empty corpus")
		}
		if !strings.Contains(err.Error(), "read severity-calibration corpus dir") {
			t.Fatalf("error does not name the mode: %v", err)
		}
	})
	t.Run("mode-a-unreadable-case-json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "empty-case"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := LoadSeverityCalibrationCorpus(dir)
		if err == nil || !strings.Contains(err.Error(), "read case.json") {
			t.Fatalf("a case dir with no case.json must fail closed, got %v", err)
		}
	})
	t.Run("mode-b-malformed-json", func(t *testing.T) {
		dir := t.TempDir()
		writeCalibrationCase(t, dir, "bad", []byte("{not json"))
		_, err := LoadSeverityCalibrationCorpus(dir)
		if err == nil || !strings.Contains(err.Error(), "parse case.json") {
			t.Fatalf("malformed JSON must fail closed, got %v", err)
		}
	})

	for _, tc := range []struct {
		mode   string
		name   string
		mutate func(*SeverityCalibrationCase)
		want   string
	}{
		{"c", "empty-diff", func(c *SeverityCalibrationCase) { c.Diff = "" }, "diff must be non-empty"},
		{"d", "zero-concerns", func(c *SeverityCalibrationCase) { c.Concerns = nil }, "concerns must be non-empty"},
		{"e", "empty-concern-id", func(c *SeverityCalibrationCase) { c.Concerns[0].ConcernID = "" }, "concern_id must be non-empty"},
		{"f", "unrecognised-severity", func(c *SeverityCalibrationCase) { c.Concerns[0].Severity = "critical" }, `severity "critical" is not one of high|medium|low`},
		{"f", "empty-severity", func(c *SeverityCalibrationCase) { c.Concerns[0].Severity = "" }, `severity "" is not one of high|medium|low`},
		{"g", "empty-operator-severity", func(c *SeverityCalibrationCase) { c.Concerns[0].OperatorSeverity = "" }, "an unlabelled candidate is not a corpus case"},
		{"g", "unrecognised-operator-severity", func(c *SeverityCalibrationCase) { c.Concerns[0].OperatorSeverity = "moderate" }, "an unlabelled candidate is not a corpus case"},
		{"h", "unrecognised-disposition", func(c *SeverityCalibrationCase) { c.Concerns[0].Disposition = "ignored" }, `disposition "ignored" is not one of`},
		{"i", "empty-note", func(c *SeverityCalibrationCase) { c.Concerns[0].Note = "   " }, "note must be non-empty"},
		{"j", "prior-concern-empty-id", func(c *SeverityCalibrationCase) {
			c.PriorConcerns = []PriorConcernFixture{{ID: "", State: "raised"}}
		}, "prior_concerns[0]: id must be non-empty"},
		{"j", "prior-concern-unknown-state", func(c *SeverityCalibrationCase) {
			c.PriorConcerns = []PriorConcernFixture{{ID: "c-p", State: "pending"}}
		}, `state "pending" is not a known concern state`},
	} {
		t.Run("mode-"+tc.mode+"-"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeValidCase(t, dir, "case-under-test", tc.mutate)
			_, err := LoadSeverityCalibrationCorpus(dir)
			if err == nil {
				t.Fatalf("mode (%s) must fail closed", tc.mode)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mode (%s): error %q does not name the mode (want %q)", tc.mode, err.Error(), tc.want)
			}
		})
	}

	t.Run("duplicate-concern-id", func(t *testing.T) {
		dir := t.TempDir()
		writeValidCase(t, dir, "dup", func(c *SeverityCalibrationCase) {
			second := c.Concerns[0]
			c.Concerns = append(c.Concerns, second)
		})
		_, err := LoadSeverityCalibrationCorpus(dir)
		if err == nil || !strings.Contains(err.Error(), "is declared twice") {
			t.Fatalf("a duplicated concern_id must fail closed — the id is the comparison key, and two rows under one key silently collapse one of them: got %v", err)
		}
	})

	t.Run("control-valid-case-loads", func(t *testing.T) {
		dir := t.TempDir()
		writeValidCase(t, dir, "ok", nil)
		got, err := LoadSeverityCalibrationCorpus(dir)
		if err != nil {
			t.Fatalf("the unmutated baseline must load: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 case, got %d", len(got))
		}
	})
}

// TestLoadSeverityCalibrationCorpus_UnlabelledCaseRefused is the property
// that makes "labelled" mean something: a candidate the operator has not yet
// labelled does not load.
func TestLoadSeverityCalibrationCorpus_UnlabelledCaseRefused(t *testing.T) {
	dir := t.TempDir()
	writeValidCase(t, dir, "unlabelled", func(c *SeverityCalibrationCase) { c.Concerns[0].OperatorSeverity = "" })
	got, err := LoadSeverityCalibrationCorpus(dir)
	if err == nil {
		t.Fatalf("an unlabelled candidate must be refused; loaded %d cases", len(got))
	}
	if !strings.Contains(err.Error(), "an unlabelled candidate is not a corpus case") {
		t.Fatalf("error does not name the label requirement: %v", err)
	}
}

// TestSeverityCalibrationCorpus_CommittedFixturesLoad + posture coverage.
func TestSeverityCalibrationCorpus_CommittedFixturesLoad(t *testing.T) {
	cases, err := LoadSeverityCalibrationCorpus(severityCalibrationCorpusDir)
	if err != nil {
		t.Fatalf("the committed corpus must load: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("want 3 committed seed fixtures, got %d", len(cases))
	}
	for _, nc := range cases {
		if !nc.Case.Synthetic {
			t.Errorf("committed seed fixture %q must set synthetic:true", nc.Name)
		}
	}
}

// TestSeverityCalibrationCorpus_ExercisesPriorConcernsPosture asserts the
// COMMITTED corpus genuinely populates the len(PriorConcerns) > 0 guard the
// fifth treatment literal renders under — otherwise the guard-conditional
// contamination could hide behind a corpus that never exercises it.
func TestSeverityCalibrationCorpus_ExercisesPriorConcernsPosture(t *testing.T) {
	cases, err := LoadSeverityCalibrationCorpus(severityCalibrationCorpusDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	withPrior, withoutPrior := 0, 0
	for _, nc := range cases {
		if len(nc.Case.PriorConcerns) > 0 {
			withPrior++
		} else {
			withoutPrior++
		}
	}
	if withPrior == 0 {
		t.Error("at least one committed fixture must carry prior_concerns so the guard-conditional literal is exercised")
	}
	if withoutPrior == 0 {
		t.Error("at least one committed fixture must carry NO prior_concerns so the other posture is exercised")
	}
	// And both postures must actually render and strip.
	for _, nc := range cases {
		if _, err := CalibrationArmPrompt(nc.Case, ArmPreCalibration); err != nil {
			t.Errorf("committed fixture %q: pre arm: %v", nc.Name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// SeverityTier.
// ---------------------------------------------------------------------------

// TestSeverityTier_UnknownIsNotZero: an unknown severity must report
// not-found, never tier 0. Tier 0 against a labelled tier 1 would read as a
// distance of 1 — and against nothing at all as perfect agreement — a
// control failing OPEN.
func TestSeverityTier_UnknownIsNotZero(t *testing.T) {
	for _, s := range []string{"", "critical", "HIGH", "info", "0"} {
		if tier, ok := SeverityTier(s); ok {
			t.Errorf("SeverityTier(%q) reported found with tier %d; unknown severities must not resolve", s, tier)
		}
	}
	for s, want := range map[string]int{"low": 1, "medium": 2, "high": 3} {
		tier, ok := SeverityTier(s)
		if !ok || tier != want {
			t.Errorf("SeverityTier(%q) = (%d, %t), want (%d, true)", s, tier, ok, want)
		}
	}
	if MaxSeverityTierDistance != 2 {
		t.Errorf("MaxSeverityTierDistance must be the largest distance the scale admits (high vs low = 2), got %d", MaxSeverityTierDistance)
	}
}

// ---------------------------------------------------------------------------
// The comparison.
// ---------------------------------------------------------------------------

// armWith builds a hand-seeded arm report BY CONSTRUCTION: the labelled
// population, and the distances this arm produced. A labelled id absent from
// distances is a MISS.
func armWith(arm, caseName string, labelled []string, distances map[string]float64) SeverityArmReport {
	cov := ArmCoverage{LabelledConcernIDs: labelled, Distances: map[string]float64{}}
	for _, id := range labelled {
		if d, ok := distances[id]; ok {
			cov.Distances[id] = d
			continue
		}
		cov.MissedConcernIDs = append(cov.MissedConcernIDs, id)
	}
	return SeverityArmReport{Arm: arm, Samples: 1, PerCase: map[string]ArmCoverage{caseName: cov}}
}

// TestCompareSeverityArms_OmissionCannotManufactureImprovement is the
// BINDING-CONDITION pin, and it is the operator's exact counterexample, not
// a paraphrase: two labelled concerns A and B with pre/post distances 1/2
// and 2/1. With both arms complete the delta is zero. With the post arm
// OMITTING A — no severity has moved — the comparison must still report NO
// improvement.
//
// Pairwise-complete FAILS this: dropping A from both arms leaves B alone at
// pre 2 / post 1 and reports +1. The miss penalty passes it: A scores at
// MaxSeverityTierDistance in the post arm, so post is (2 + 1)/2 = 1.5 and
// the delta stays 0.
func TestCompareSeverityArms_OmissionCannotManufactureImprovement(t *testing.T) {
	labelled := []string{"A", "B"}
	pre := armWith(ArmPreCalibration, "astra", labelled, map[string]float64{"A": 1, "B": 2})

	t.Run("both-arms-complete-delta-is-zero", func(t *testing.T) {
		post := armWith(ArmPostCalibration, "astra", labelled, map[string]float64{"A": 2, "B": 1})
		got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if got.Overall != 0 {
			t.Fatalf("both arms complete: overall delta = %+.4f, want exactly 0\n%s", got.Overall, got.Render())
		}
		if got.Improved {
			t.Fatalf("a zero delta must not be reported as an improvement\n%s", got.Render())
		}
	})

	t.Run("post-arm-omits-A-reports-no-improvement", func(t *testing.T) {
		// Identical to the complete arm except A is OMITTED. No severity moved.
		post := armWith(ArmPostCalibration, "astra", labelled, map[string]float64{"B": 1})
		got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
		if err != nil {
			t.Fatalf("compare: %v", err)
		}
		if got.Improved {
			t.Fatalf("omitting labelled concern A must NOT be reported as an improvement\n%s", got.Render())
		}
		if got.Overall > 0 {
			t.Fatalf("omitting labelled concern A raised the delta to %+.4f; the comparison is not omission-monotone\n%s", got.Overall, got.Render())
		}
		if len(got.PerCase) != 1 || len(got.PerCase[0].PostMissed) != 1 || got.PerCase[0].PostMissed[0] != "A" {
			t.Errorf("the omission must be reported by concern_id in the post arm's coverage: %+v", got.PerCase)
		}
	})
}

// TestCompareSeverityArms_OmittedHighDistanceConcernIsNotAnImprovement is
// the WEAKER counterfactual the operator kept: the post arm omits the
// badly-calibrated concern (distance 2) and emits only the well-calibrated
// one (distance 0), with no severity having moved.
func TestCompareSeverityArms_OmittedHighDistanceConcernIsNotAnImprovement(t *testing.T) {
	labelled := []string{"bad", "good"}
	pre := armWith(ArmPreCalibration, "omission", labelled, map[string]float64{"bad": 2, "good": 0})
	post := armWith(ArmPostCalibration, "omission", labelled, map[string]float64{"good": 0})

	got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if got.Improved || got.Overall > 0 {
		t.Fatalf("an arm that merely omits a badly-calibrated labelled concern must not report an improvement; got overall %+.4f\n%s", got.Overall, got.Render())
	}
	if len(got.PerCase[0].PostMissed) != 1 || got.PerCase[0].PostMissed[0] != "bad" {
		t.Errorf("the missed concern_id must be reported: %+v", got.PerCase[0])
	}
}

// TestCompareSeverityArms_RealImprovementIsStillReported is the control for
// the two omission tests: when severities genuinely move closer to the
// operator label and NOTHING is omitted, the comparison does report it. A
// mechanism that reported "no improvement" unconditionally would satisfy the
// omission tests and be useless.
func TestCompareSeverityArms_RealImprovementIsStillReported(t *testing.T) {
	labelled := []string{"A", "B"}
	pre := armWith(ArmPreCalibration, "real", labelled, map[string]float64{"A": 2, "B": 2})
	post := armWith(ArmPostCalibration, "real", labelled, map[string]float64{"A": 0, "B": 1})

	got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !got.Improved {
		t.Fatalf("a genuine severity movement must be reported as an improvement; got %+.4f\n%s", got.Overall, got.Render())
	}
	if got.Overall != 1.5 {
		t.Errorf("overall delta = %+.4f, want +1.5", got.Overall)
	}
}

// TestCompareSeverityArms_NoComparableConcernsIsNotAZeroDelta: a case with
// an empty labelled population contributes NO score and is reported as
// skipped — never a zero distance, which reads as perfect agreement.
func TestCompareSeverityArms_NoComparableConcernsIsNotAZeroDelta(t *testing.T) {
	pre := armWith(ArmPreCalibration, "empty", nil, nil)
	post := armWith(ArmPostCalibration, "empty", nil, nil)

	got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if got.ScoredCases != 0 || got.SkippedCases != 1 {
		t.Fatalf("scored=%d skipped=%d, want scored=0 skipped=1", got.ScoredCases, got.SkippedCases)
	}
	if len(got.PerCase) != 1 || got.PerCase[0].Scored {
		t.Fatalf("the case must be reported unscored: %+v", got.PerCase)
	}
	if got.PerCase[0].SkipReason != "no_labelled_concerns" {
		t.Errorf("skip reason = %q, want %q", got.PerCase[0].SkipReason, "no_labelled_concerns")
	}
	if !strings.Contains(got.Render(), "SKIPPED (no_labelled_concerns)") {
		t.Errorf("the rendered report must name the skip:\n%s", got.Render())
	}
}

// TestCompareSeverityArms_FailsClosedOnCaseMismatch, in BOTH directions.
func TestCompareSeverityArms_FailsClosedOnCaseMismatch(t *testing.T) {
	labelled := []string{"A"}
	t.Run("case-in-pre-only", func(t *testing.T) {
		// The post arm is a strict SUBSET of the pre arm, so the
		// post-side sweep passes and the PRE-side guard is the only
		// thing standing between this input and a phantom comparison.
		pre := armWith(ArmPreCalibration, "shared", labelled, map[string]float64{"A": 1})
		pre.PerCase["only-in-pre"] = ArmCoverage{LabelledConcernIDs: labelled, Distances: map[string]float64{"A": 2}}
		post := armWith(ArmPostCalibration, "shared", labelled, map[string]float64{"A": 1})
		_, err := CompareSeverityArms(pre, post, 0)
		if err == nil {
			t.Fatal("a case present only in the pre arm must fail closed")
		}
		if !strings.Contains(err.Error(), `"only-in-pre"`) || !strings.Contains(err.Error(), "not comparable") {
			t.Fatalf("error does not name the mismatch: %v", err)
		}
	})
	t.Run("case-in-post-only", func(t *testing.T) {
		pre := armWith(ArmPreCalibration, "shared", labelled, map[string]float64{"A": 1})
		post := armWith(ArmPostCalibration, "shared", labelled, map[string]float64{"A": 1})
		post.PerCase["only-in-post"] = ArmCoverage{LabelledConcernIDs: labelled, Distances: map[string]float64{"A": 0}}
		_, err := CompareSeverityArms(pre, post, 0)
		if err == nil {
			t.Fatal("a case present only in the post arm must fail closed")
		}
		if !strings.Contains(err.Error(), `"only-in-post"`) {
			t.Fatalf("error does not name the case: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// RunSeverityArm.
// ---------------------------------------------------------------------------

// TestRunSeverityArm_ReportsLabelledMatchedAndMissedPerArm: the arm result
// is per-arm coverage, not a bare mean — an omission asymmetry buried in a
// mean is invisible, and the comparison needs the labelled population and
// the misses to be omission-monotone at all.
func TestRunSeverityArm_ReportsLabelledMatchedAndMissedPerArm(t *testing.T) {
	c := SeverityCalibrationCase{
		Name: "coverage", Diff: "diff --git a/a.go b/a.go\n+x\n", Synthetic: true,
		PlanSummary: "x", ScopeFiles: []string{"a.go"},
		Concerns: []LabelledConcern{
			{ConcernID: "c-1", Severity: "high", Category: "security",
				Note:        "Unscoped artifact lookup permits a cross-tenant read.",
				Disposition: "addressed", OperatorSeverity: "high"},
			{ConcernID: "c-2", Severity: "high", Category: "resource",
				Note:        "Unbounded manifest read without a size cap.",
				Disposition: "waived", OperatorSeverity: "low"},
		},
	}
	// The reviewer emits c-1's finding at `medium` (distance 1 from the
	// `high` label), misses c-2 entirely, and raises one novel concern.
	verdict := `{"verdict":"approve_with_concerns","concerns":[
	  {"severity":"medium","category":"security","note":"The artifact lookup is unscoped and permits a cross-tenant read."},
	  {"severity":"low","category":"style","note":"Naming here is inconsistent with the surrounding package."}]}`
	sender := &fakeCalibrationSender{responses: []string{verdict}}

	got, err := RunSeverityArm(context.Background(), sender,
		[]NamedSeverityCalibrationCase{{Name: c.Name, Case: c}}, ArmPostCalibration, 1)
	if err != nil {
		t.Fatalf("run arm: %v", err)
	}
	cov := got.PerCase["coverage"]
	if cov.Labelled() != 2 {
		t.Errorf("labelled = %d, want 2", cov.Labelled())
	}
	if cov.Matched() != 1 {
		t.Errorf("matched = %d, want 1", cov.Matched())
	}
	if len(cov.MissedConcernIDs) != 1 || cov.MissedConcernIDs[0] != "c-2" {
		t.Errorf("missed = %v, want [c-2]", cov.MissedConcernIDs)
	}
	if d, ok := cov.Distances["c-1"]; !ok || d != 1 {
		t.Errorf("c-1 distance = (%v, %t), want (1, true)", d, ok)
	}
	if cov.UnmatchedEmitted != 1 {
		t.Errorf("unmatched emitted = %d, want 1", cov.UnmatchedEmitted)
	}
}

func TestRunSeverityArm_FailsClosed(t *testing.T) {
	c := validCase()
	cases := []NamedSeverityCalibrationCase{{Name: c.Name, Case: c}}
	ok := `{"verdict":"approve","concerns":[]}`

	t.Run("no-fixtures", func(t *testing.T) {
		if _, err := RunSeverityArm(context.Background(), &fakeCalibrationSender{responses: []string{ok}}, nil, ArmPostCalibration, 1); err == nil {
			t.Fatal("an empty case set must fail closed")
		}
	})
	t.Run("zero-samples", func(t *testing.T) {
		if _, err := RunSeverityArm(context.Background(), &fakeCalibrationSender{responses: []string{ok}}, cases, ArmPostCalibration, 0); err == nil {
			t.Fatal("samples < 1 must fail closed")
		}
	})
	t.Run("nil-sender", func(t *testing.T) {
		if _, err := RunSeverityArm(context.Background(), nil, cases, ArmPostCalibration, 1); err == nil {
			t.Fatal("a nil sender must fail closed")
		}
	})
	t.Run("generate-error", func(t *testing.T) {
		s := &fakeCalibrationSender{err: fmt.Errorf("boom")}
		if _, err := RunSeverityArm(context.Background(), s, cases, ArmPostCalibration, 1); err == nil {
			t.Fatal("a generate error must abort the arm, never yield a partial one")
		}
	})
	t.Run("undecodable-verdict", func(t *testing.T) {
		s := &fakeCalibrationSender{responses: []string{"the model replied in prose"}}
		_, err := RunSeverityArm(context.Background(), s, cases, ArmPostCalibration, 1)
		if err == nil || !strings.Contains(err.Error(), "decode verdict") {
			t.Fatalf("an undecodable verdict must fail closed rather than read as zero concerns, got %v", err)
		}
	})
	t.Run("unknown-emitted-severity", func(t *testing.T) {
		s := &fakeCalibrationSender{responses: []string{`{"verdict":"reject","concerns":[{"severity":"critical","category":"security","note":"x"}]}`}}
		_, err := RunSeverityArm(context.Background(), s, cases, ArmPostCalibration, 1)
		if err == nil || !strings.Contains(err.Error(), "is not one of high|medium|low") {
			t.Fatalf("an unknown emitted severity must fail closed, got %v", err)
		}
	})
	t.Run("unknown-arm", func(t *testing.T) {
		s := &fakeCalibrationSender{responses: []string{ok}}
		if _, err := RunSeverityArm(context.Background(), s, cases, "sideways", 1); err == nil {
			t.Fatal("an unknown arm must fail closed")
		}
	})
}

// ---------------------------------------------------------------------------
// Omission monotonicity ACROSS SAMPLE AGGREGATION (the multi-sample half).
// ---------------------------------------------------------------------------

// multiSampleCase is astra's counterexample expressed as a real fixture that
// RunSeverityArm can drive: two labelled concerns with DISTINCT categories,
// so the emitted-to-labelled match is unambiguous, and notes carrying
// substantive words the emitted notes echo (the matcher needs category
// equality plus two shared words longer than three characters).
func multiSampleCase() []NamedSeverityCalibrationCase {
	return []NamedSeverityCalibrationCase{{
		Name: "astra-multisample",
		Case: SeverityCalibrationCase{
			Name:      "astra-multisample",
			Diff:      "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n",
			Synthetic: true,
			Concerns: []LabelledConcern{
				{
					ConcernID: "A", Category: "correctness", Severity: "low",
					Note:        "the authorization check reads an asynchronous projection while the mutation applies against the authoritative store",
					Disposition: "addressed", OperatorSeverity: "high",
				},
				{
					ConcernID: "B", Category: "efficiency", Severity: "medium",
					Note:        "the decode path allocates without an upper bound on the declared length",
					Disposition: "waived", OperatorSeverity: "high",
				},
			},
		},
	}}
}

// emittedConcernJSON renders one emitted concern for the fake sender.
func emittedConcernJSON(category, severity, note string) string {
	return fmt.Sprintf(`{"severity":%q,"category":%q,"note":%q}`, severity, category, note)
}

// verdictJSON assembles a reviewer verdict from already-rendered concerns.
func verdictJSON(concerns ...string) string {
	return `{"verdict":"changes_requested","concerns":[` + strings.Join(concerns, ",") + `]}`
}

const (
	// noteA / noteB echo enough of each labelled note to bind the emitted
	// concern to it. Kept as constants so every sample in both arms uses
	// byte-identical prose and the ONLY thing varying across the arms is
	// which samples emit A at all.
	noteA = "the projection read and the authoritative store disagree, so the authorization check passes stale"
	noteB = "the decode allocates without an upper bound on the declared length"
)

// TestRunSeverityArm_PartialSampleOmissionIsNotAnImprovement is astra's
// counterexample driven through the SUPPORTED MULTI-SAMPLE configuration,
// which is the shape the single-sample comparison tests structurally cannot
// reach.
//
// Both arms run 5 samples. In BOTH arms concern A is emitted at tier
// distance 2 in samples 1-4 and distance 0 in sample 5 WHEN IT IS EMITTED AT
// ALL; concern B is emitted identically in every sample of both arms, so B
// contributes nothing to the delta. The arms differ ONLY in that the POST
// arm OMITS A from samples 1-4 — no emitted severity differs between the
// arms, so an honest comparison must report a delta of exactly zero.
//
// Averaging each concern over its MATCHED samples only fails this: pre A is
// (2+2+2+2+0)/5 = 1.6 while post A is 0/1 = 0, and the case reports a +0.8
// improvement manufactured entirely by partial omission. The concern-level
// miss penalty in penalizedMeanDistance cannot catch it, because A WAS
// matched — once. The fix is the per-SAMPLE penalty inside RunSeverityArm.
func TestRunSeverityArm_PartialSampleOmissionIsNotAnImprovement(t *testing.T) {
	ctx := context.Background()
	cases := multiSampleCase()
	const samples = 5

	// A at distance 2 from its "high" label is an emitted "low"; at distance
	// 0 it is an emitted "high". B is always emitted at distance 0.
	aFar := emittedConcernJSON("correctness", "low", noteA)
	aExact := emittedConcernJSON("correctness", "high", noteA)
	bExact := emittedConcernJSON("efficiency", "high", noteB)

	preSender := &fakeCalibrationSender{responses: []string{
		verdictJSON(aFar, bExact),
		verdictJSON(aFar, bExact),
		verdictJSON(aFar, bExact),
		verdictJSON(aFar, bExact),
		verdictJSON(aExact, bExact),
	}}
	// IDENTICAL except A is simply absent from the first four samples.
	postSender := &fakeCalibrationSender{responses: []string{
		verdictJSON(bExact),
		verdictJSON(bExact),
		verdictJSON(bExact),
		verdictJSON(bExact),
		verdictJSON(aExact, bExact),
	}}

	pre, err := RunSeverityArm(ctx, preSender, cases, ArmPreCalibration, samples)
	if err != nil {
		t.Fatalf("pre arm: %v", err)
	}
	post, err := RunSeverityArm(ctx, postSender, cases, ArmPostCalibration, samples)
	if err != nil {
		t.Fatalf("post arm: %v", err)
	}

	preCov := pre.PerCase["astra-multisample"]
	postCov := post.PerCase["astra-multisample"]
	if got := preCov.MatchedSamples["A"]; got != samples {
		t.Fatalf("pre arm matched A in %d/%d samples; the fixture did not drive the intended shape (matcher failure)", got, samples)
	}
	if got := postCov.MatchedSamples["A"]; got != 1 {
		t.Fatalf("post arm matched A in %d/%d samples, want exactly 1; the fixture did not drive the intended shape", got, samples)
	}
	if len(postCov.MissedConcernIDs) != 0 {
		t.Fatalf("A was emitted once, so it is a PARTIAL omission and must not be reported as a whole-arm miss: %v", postCov.MissedConcernIDs)
	}

	got, err := CompareSeverityArms(pre, post, DefaultCalibrationImprovementThreshold)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if got.Improved {
		t.Fatalf("partial-sample omission of concern A was reported as an IMPROVEMENT; the comparison is not omission-monotone across sample aggregation\n%s", got.Render())
	}
	if got.Overall > 0 {
		t.Fatalf("partial-sample omission of concern A raised the delta to %+.4f; no emitted severity differs between the arms, so the delta must be exactly 0\n%s", got.Overall, got.Render())
	}
	if got.Overall != 0 {
		t.Fatalf("delta = %+.4f, want exactly 0\n%s", got.Overall, got.Render())
	}
}

// TestRunSeverityArm_PerSampleMissPenaltyIsApplied pins the arithmetic the
// test above depends on, so a regression that merely SHIFTS the numbers
// while keeping the delta at zero still goes red.
func TestRunSeverityArm_PerSampleMissPenaltyIsApplied(t *testing.T) {
	ctx := context.Background()
	cases := multiSampleCase()
	const samples = 4

	aExact := emittedConcernJSON("correctness", "high", noteA)
	bExact := emittedConcernJSON("efficiency", "high", noteB)

	// A is emitted at distance 0 in ONE of four samples and omitted from the
	// other three: (0 + 2 + 2 + 2)/4 = 1.5, NOT 0.
	sender := &fakeCalibrationSender{responses: []string{
		verdictJSON(aExact, bExact),
		verdictJSON(bExact),
		verdictJSON(bExact),
		verdictJSON(bExact),
	}}
	rep, err := RunSeverityArm(ctx, sender, cases, ArmPostCalibration, samples)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	cov := rep.PerCase["astra-multisample"]
	if got, want := cov.Distances["A"], 1.5; got != want {
		t.Errorf("A distance = %.4f, want %.4f (one exact sample plus three sample-level miss penalties of %d)", got, want, MaxSeverityTierDistance)
	}
	if got, want := cov.Distances["B"], 0.0; got != want {
		t.Errorf("B distance = %.4f, want %.4f (matched in every sample at distance 0)", got, want)
	}
	// A concern missed in EVERY sample lands exactly on the whole-arm
	// penalty, so the sample-level and concern-level rules agree at the
	// boundary rather than double-counting.
	missSender := &fakeCalibrationSender{responses: []string{verdictJSON(bExact)}}
	missRep, err := RunSeverityArm(ctx, missSender, cases, ArmPostCalibration, samples)
	if err != nil {
		t.Fatalf("miss arm: %v", err)
	}
	missCov := missRep.PerCase["astra-multisample"]
	if got, want := missCov.Distances["A"], float64(MaxSeverityTierDistance); got != want {
		t.Errorf("A missed in every sample: distance = %.4f, want %.4f", got, want)
	}
	if len(missCov.MissedConcernIDs) != 1 || missCov.MissedConcernIDs[0] != "A" {
		t.Errorf("A missed in every sample must still be reported as a whole-arm miss, got %v", missCov.MissedConcernIDs)
	}
	if got := penalizedMeanDistance(missCov); got != (float64(MaxSeverityTierDistance)+0)/2 {
		t.Errorf("penalizedMeanDistance = %.4f, want %.4f", got, (float64(MaxSeverityTierDistance)+0)/2)
	}
}
