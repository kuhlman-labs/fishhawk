package devfixtures_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures/catalog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spendalert"
)

// TestEmbeddedScenarios_AllParseAndValidate loads every catalog scenario
// through the real Load path (embed → Parse → Validate) and additionally
// checks that every plan / grooming_report artifact the scenarios carry is
// a document the plan package's kind-routed validator accepts — a fixture
// the product itself would refuse is not a useful fixture.
func TestEmbeddedScenarios_AllParseAndValidate(t *testing.T) {
	for _, name := range catalog.Names() {
		t.Run(name, func(t *testing.T) {
			s, err := devfixtures.Load(name)
			if err != nil {
				t.Fatalf("Load(%q): %v", name, err)
			}
			if s.Name != name {
				t.Fatalf("Name = %q, want %q", s.Name, name)
			}
			if catalog.Description(name) == "" {
				t.Fatalf("catalog.Description(%q) is empty", name)
			}
			for _, a := range s.Artifacts {
				switch artifact.Kind(a.Kind) {
				case artifact.KindPlan, artifact.KindGroomingReport:
					if err := plan.ValidateArtifact([]byte(a.Content)); err != nil {
						t.Errorf("artifact %q (%s) rejected by plan.ValidateArtifact: %v", a.Key, a.Kind, err)
					}
				}
			}
		})
	}
}

// TestEmbeddedScenarios_DeclaredShapes pins the shape each scenario
// promises to its consumers (the acceptance prompt and the classifier
// describe them by these properties), so a YAML edit that changes the
// done-means shape fails here rather than in a preview.
func TestEmbeddedScenarios_DeclaredShapes(t *testing.T) {
	t.Run("grooming-confirm-gate", func(t *testing.T) {
		s := mustLoad(t, "grooming-confirm-gate")
		if len(s.Runs) != 1 || s.Runs[0].WorkflowID != "backlog_grooming" {
			t.Fatalf("want one backlog_grooming run, got %+v", s.Runs)
		}
		var sawPlanSucceeded, sawHumanReviewParked bool
		for _, st := range s.Stages {
			if st.Type == "implement" {
				t.Fatalf("grooming-confirm-gate must carry no implement stage, got %+v", st)
			}
			if st.Type == "plan" && st.ExecutorKind == "agent" && st.State == "succeeded" {
				sawPlanSucceeded = true
			}
			if st.Type == "review" && st.ExecutorKind == "human" && st.RequiresApproval && st.State == "awaiting_approval" {
				sawHumanReviewParked = true
			}
		}
		if !sawPlanSucceeded || !sawHumanReviewParked {
			t.Fatalf("want plan/agent succeeded + review/human awaiting_approval, got %+v", s.Stages)
		}
		if len(s.Artifacts) != 1 || s.Artifacts[0].Kind != "grooming_report" {
			t.Fatalf("want exactly one grooming_report artifact, got %+v", s.Artifacts)
		}
		if len(s.Approvals) != 1 || s.Approvals[0].Decision != "approve" {
			t.Fatalf("want exactly one approve approval, got %+v", s.Approvals)
		}
	})
	t.Run("plan-gate-parked", func(t *testing.T) {
		s := mustLoad(t, "plan-gate-parked")
		if len(s.Stages) != 1 || s.Stages[0].Type != "plan" || s.Stages[0].State != "awaiting_approval" {
			t.Fatalf("want a single plan stage at awaiting_approval, got %+v", s.Stages)
		}
		if len(s.Artifacts) != 1 || s.Artifacts[0].Kind != "plan" || s.Artifacts[0].SchemaVersion != "standard_v1" {
			t.Fatalf("want one standard_v1 plan artifact, got %+v", s.Artifacts)
		}
	})
	t.Run("trace-upload-target", func(t *testing.T) {
		s := mustLoad(t, "trace-upload-target")
		if len(s.Stages) != 1 || s.Stages[0].Type != "plan" || s.Stages[0].State != "dispatched" {
			t.Fatalf("want a single plan stage at dispatched, got %+v", s.Stages)
		}
		var ages []string
		for _, row := range s.Audit {
			if row.Category != "cost_recorded" {
				t.Fatalf("every audit row must be cost_recorded, got %q", row.Category)
			}
			ages = append(ages, row.Age)
		}
		if want := []string{"1h5m", "2h", "3h", "4h"}; !slices.Equal(ages, want) {
			t.Fatalf("ages = %v, want %v", ages, want)
		}
	})
}

// TestNames_MatchesEmbeddedScenarioSet is the TWO-WAY binding between the
// stdlib-only catalog and the embedded YAML set: every catalog name has a
// file and every file has a catalog name. Counterfactuals: append a
// phantom name to catalog → RED (missing file); add a YAML the catalog
// does not name → RED (extra file).
func TestNames_MatchesEmbeddedScenarioSet(t *testing.T) {
	names := devfixtures.Names()
	if !slices.Equal(names, catalog.Names()) {
		t.Fatalf("devfixtures.Names() = %v, catalog.Names() = %v", names, catalog.Names())
	}
	if !slices.IsSorted(names) {
		t.Fatalf("Names() not sorted: %v", names)
	}
	embedded, err := devfixtures.EmbeddedNames()
	if err != nil {
		t.Fatalf("EmbeddedNames: %v", err)
	}
	for _, n := range names {
		if !slices.Contains(embedded, n) {
			t.Errorf("catalog names %q but no scenarios/%s.yaml is embedded", n, n)
		}
		if !catalog.Known(n) {
			t.Errorf("catalog.Known(%q) = false for a catalog name", n)
		}
	}
	for _, n := range embedded {
		if !catalog.Known(n) {
			t.Errorf("scenarios/%s.yaml is embedded but the catalog does not name it", n)
		}
	}
	if catalog.Known("not-a-scenario") || catalog.Description("not-a-scenario") != "" {
		t.Fatalf("catalog admits an unknown name")
	}
	// Names() must hand back a fresh slice — a caller must not be able to
	// mutate the catalog.
	names[0] = "mutated"
	if catalog.Names()[0] == "mutated" {
		t.Fatalf("catalog.Names() shares its backing array with callers")
	}
}

func TestLoad_UnknownScenarioNamesKnownSet(t *testing.T) {
	_, err := devfixtures.Load("not-a-scenario")
	if !errors.Is(err, devfixtures.ErrUnknownScenario) {
		t.Fatalf("err = %v, want ErrUnknownScenario", err)
	}
	for _, n := range catalog.Names() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error %q does not name known scenario %q", err, n)
		}
	}
	if !strings.Contains(err.Error(), `"not-a-scenario"`) {
		t.Errorf("error %q does not name the requested scenario", err)
	}
}

func TestParse_RefusesUnknownYAMLKeys(t *testing.T) {
	_, err := devfixtures.Parse([]byte("runs: []\nbogus: 1\n"))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want an unknown-field refusal naming bogus", err)
	}
	if _, err := devfixtures.Parse([]byte("runs: [")); err == nil {
		t.Fatalf("malformed YAML accepted")
	}
}

func TestAuditRow_AgeDuration(t *testing.T) {
	d, err := devfixtures.AuditRow{}.AgeDuration()
	if err != nil || d != 0 {
		t.Fatalf("absent age: d=%v err=%v, want 0,nil", d, err)
	}
	d, err = devfixtures.AuditRow{Age: "1h5m"}.AgeDuration()
	if err != nil || d != 65*time.Minute {
		t.Fatalf("1h5m: d=%v err=%v", d, err)
	}
	if _, err := (devfixtures.AuditRow{Age: "soon"}).AgeDuration(); err == nil {
		t.Fatalf("unparseable age accepted")
	}
}

// goodScenario is the known-good base every refusal row mutates: the
// grooming-confirm-gate scenario plus one valid backdated audit row so
// audit-side mutations have something to corrupt. It is asserted valid
// (the control) before any row runs.
func goodScenario(t *testing.T) *devfixtures.Scenario {
	t.Helper()
	s := mustLoad(t, "grooming-confirm-gate")
	s.Audit = append(s.Audit, devfixtures.AuditRow{
		Run: "groom-run", Stage: "groom", Category: "cost_recorded",
		ActorKind: "system", ActorSubject: "devfixtures",
		Payload: `{"usd":0.001}`, Age: "1h",
	})
	if err := s.Validate(); err != nil {
		t.Fatalf("control: goodScenario does not validate: %v", err)
	}
	return s
}

// TestValidate_Refusals carries one row per refusal branch in Validate.
// Each row mutates the known-good base by construction and asserts the
// error names the offending handle (or index) and the defect.
func TestValidate_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *devfixtures.Scenario)
		want   []string // every substring must appear in the error
	}{
		{"no runs", func(s *devfixtures.Scenario) { s.Runs = nil }, []string{"no runs"}},
		{"run empty key", func(s *devfixtures.Scenario) { s.Runs[0].Key = "" }, []string{"runs[0]", "key required"}},
		{"run duplicate key", func(s *devfixtures.Scenario) { s.Runs = append(s.Runs, s.Runs[0]) }, []string{`run "groom-run"`, "duplicate key"}},
		{"unknown run state", func(s *devfixtures.Scenario) { s.Runs[0].State = "parked" }, []string{`run "groom-run"`, `unknown run state "parked"`}},
		{"unparseable workflow_spec", func(s *devfixtures.Scenario) { s.Runs[0].WorkflowSpec = "version: \"2\"\nworkflows: 7\n" }, []string{`run "groom-run"`, "workflow_spec"}},
		{"empty workflow_spec", func(s *devfixtures.Scenario) { s.Runs[0].WorkflowSpec = "" }, []string{`run "groom-run"`, "workflow_spec"}},
		{"workflow_id not declared by spec", func(s *devfixtures.Scenario) { s.Runs[0].WorkflowID = "release" }, []string{`run "groom-run"`, `does not declare workflow_id "release"`}},
		{"stage empty key", func(s *devfixtures.Scenario) { s.Stages[0].Key = "" }, []string{"stages[0]", "key required"}},
		{"stage duplicate key", func(s *devfixtures.Scenario) { s.Stages[1].Key = s.Stages[0].Key }, []string{`stage "groom"`, "duplicate key"}},
		{"stage unknown run handle", func(s *devfixtures.Scenario) { s.Stages[1].Run = "ghost-run" }, []string{`stage "confirm"`, `unknown run handle "ghost-run"`}},
		{"unknown stage type", func(s *devfixtures.Scenario) { s.Stages[0].Type = "groom" }, []string{`stage "groom"`, `unknown stage type "groom"`}},
		{"unknown executor kind", func(s *devfixtures.Scenario) { s.Stages[0].ExecutorKind = "robot" }, []string{`stage "groom"`, `unknown executor kind "robot"`}},
		{"unreachable stage state awaiting_input", func(s *devfixtures.Scenario) { s.Stages[1].State = "awaiting_input" }, []string{`stage "confirm"`, `"awaiting_input"`, "not reachable"}},
		{"unreachable stage state cancelled", func(s *devfixtures.Scenario) { s.Stages[1].State = "cancelled" }, []string{`stage "confirm"`, `"cancelled"`, "not reachable"}},
		{"unknown stage state", func(s *devfixtures.Scenario) { s.Stages[1].State = "done" }, []string{`stage "confirm"`, `"done"`, "not reachable"}},
		{"failed without category", func(s *devfixtures.Scenario) { s.Stages[1].State = "failed" }, []string{`stage "confirm"`, "failure_category"}},
		{"failed with bogus category", func(s *devfixtures.Scenario) { s.Stages[1].State = "failed"; s.Stages[1].FailureCategory = "Z" }, []string{`stage "confirm"`, "failure_category", `"Z"`}},
		{"artifact empty key", func(s *devfixtures.Scenario) { s.Artifacts[0].Key = "" }, []string{"artifacts[0]", "key required"}},
		{"artifact duplicate key", func(s *devfixtures.Scenario) { s.Artifacts = append(s.Artifacts, s.Artifacts[0]) }, []string{`artifact "report"`, "duplicate key"}},
		{"artifact unknown stage handle", func(s *devfixtures.Scenario) { s.Artifacts[0].Stage = "ghost" }, []string{`artifact "report"`, `unknown stage handle "ghost"`}},
		{"unknown artifact kind", func(s *devfixtures.Scenario) { s.Artifacts[0].Kind = "diagram" }, []string{`artifact "report"`, `unknown artifact kind "diagram"`}},
		{"artifact invalid JSON", func(s *devfixtures.Scenario) { s.Artifacts[0].Content = "{not json" }, []string{`artifact "report"`, "not valid JSON"}},
		{"approval unknown stage handle", func(s *devfixtures.Scenario) { s.Approvals[0].Stage = "ghost" }, []string{"approvals[0]", `unknown stage handle "ghost"`}},
		{"approval unknown decision", func(s *devfixtures.Scenario) { s.Approvals[0].Decision = "maybe" }, []string{"approvals[0]", `stage "groom"`, `unknown decision "maybe"`}},
		{"approval unknown surface", func(s *devfixtures.Scenario) { s.Approvals[0].Surface = "carrier-pigeon" }, []string{"approvals[0]", `stage "groom"`, `unknown surface "carrier-pigeon"`}},
		{"audit unknown run handle", func(s *devfixtures.Scenario) { s.Audit[0].Run = "ghost-run" }, []string{"audit[0]", `unknown run handle "ghost-run"`}},
		{"audit unknown stage handle", func(s *devfixtures.Scenario) { s.Audit[0].Stage = "ghost" }, []string{"audit[0]", `unknown stage handle "ghost"`}},
		{"audit unknown category", func(s *devfixtures.Scenario) { s.Audit[0].Category = "money_spent" }, []string{"audit[0]", `unknown audit category "money_spent"`}},
		{"audit unknown actor kind", func(s *devfixtures.Scenario) { s.Audit[0].ActorKind = "bot" }, []string{"audit[0]", `unknown actor kind "bot"`}},
		{"audit invalid payload JSON", func(s *devfixtures.Scenario) { s.Audit[0].Payload = "usd=1" }, []string{"audit[0]", "cost_recorded", "not valid JSON"}},
		{"audit bad age", func(s *devfixtures.Scenario) { s.Audit[0].Age = "an hour" }, []string{"audit[0]", "cost_recorded", `age "an hour"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodScenario(t)
			tc.mutate(s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate accepted the %s mutation", tc.name)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// TestValidate_AcceptsReachableShapes is the positive edge of the
// refusal table: the shapes Validate must ADMIT so a refusal row cannot
// be green merely because the branch rejects everything.
func TestValidate_AcceptsReachableShapes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *devfixtures.Scenario)
	}{
		{"failed stage with category", func(s *devfixtures.Scenario) {
			s.Stages[1].State = "failed"
			s.Stages[1].FailureCategory = "A"
			s.Stages[1].FailureReason = "seeded"
		}},
		{"audit row without stage", func(s *devfixtures.Scenario) { s.Audit[0].Stage = "" }},
		{"audit row without age", func(s *devfixtures.Scenario) { s.Audit[0].Age = "" }},
		{"every reachable stage state", func(s *devfixtures.Scenario) {
			for i, st := range []string{"pending", "dispatched", "running", "succeeded", "awaiting_approval"} {
				s.Stages = append(s.Stages, devfixtures.Stage{
					Key: "extra-" + st, Run: "groom-run", Sequence: 10 + i,
					Type: "review", ExecutorKind: "human", State: st,
				})
			}
		}},
		{"every run state", func(s *devfixtures.Scenario) {
			for _, st := range []string{"pending", "succeeded", "failed", "cancelled"} {
				r := s.Runs[0]
				r.Key = "run-" + st
				r.State = st
				s.Runs = append(s.Runs, r)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodScenario(t)
			tc.mutate(s)
			if err := s.Validate(); err != nil {
				t.Fatalf("Validate refused an admissible shape: %v", err)
			}
		})
	}
}

// TestScenario_TraceUploadTarget_BaselineSurvivesHourBoundary is binding
// condition 1 from the 4a7fa6bc approval, verbatim: with the seed clock
// pinned at 10:59:59Z the four backdated cost_recorded rows land in
// buckets 09/08/07/06 UTC; for BOTH the same-hour upload clock (10:59:59Z)
// and the post-crossing one (11:00:01Z) a $5 sample trips spend_alert,
// the YOUNGEST POPULATED PRIOR bucket is seed-hour minus one (09:00Z),
// carries USD > 0, and lies inside spendalert's 24h Window. It does NOT
// assert a bucket adjacent to the upload hour — that is unachievable by
// seeding once the upload slips an hour.
//
// Counterfactuals (run by hand against the YAML, reported in the PR):
// drop the 1h5m row → youngest becomes 08:00Z → RED; drop all four rows →
// PriorHours 0 / Tripped false → RED.
func TestScenario_TraceUploadTarget_BaselineSurvivesHourBoundary(t *testing.T) {
	s := mustLoad(t, "trace-upload-target")
	seedClock := time.Date(2026, 1, 1, 10, 59, 59, 0, time.UTC)
	wantYoungest := seedClock.Truncate(time.Hour).Add(-time.Hour) // 09:00Z

	var baseline []spendalert.Sample
	wantBuckets := []int{9, 8, 7, 6}
	var gotBuckets []int
	for _, row := range s.Audit {
		age, err := row.AgeDuration()
		if err != nil {
			t.Fatalf("age %q: %v", row.Age, err)
		}
		usd := costUSD(t, row.Payload)
		at := seedClock.Add(-age)
		gotBuckets = append(gotBuckets, at.Hour())
		t.Logf("row age=%s → %s (bucket %02d:00Z) usd=%g", row.Age, at.Format(time.RFC3339), at.Hour(), usd)
		baseline = append(baseline, spendalert.Sample{Time: at, USD: usd})
	}
	if !slices.Equal(gotBuckets, wantBuckets) {
		t.Fatalf("rows landed in buckets %v, want %v", gotBuckets, wantBuckets)
	}

	for _, clock := range []time.Time{
		seedClock, // same hour as the seed
		time.Date(2026, 1, 1, 11, 0, 1, 0, time.UTC), // hour boundary crossed
	} {
		t.Run(clock.Format("15:04:05"), func(t *testing.T) {
			samples := append(slices.Clone(baseline), spendalert.Sample{Time: clock, USD: 5})
			d := spendalert.Evaluate(samples, clock, 0)
			t.Logf("clock=%s tripped=%v prior=%d avg=%g ratio=%g", clock.Format(time.RFC3339), d.Tripped, d.PriorHours, d.RollingAvgUSD, d.Ratio)
			if !d.Tripped {
				t.Fatalf("spend alert did not trip at %s: %+v", clock.Format(time.RFC3339), d)
			}
			if d.PriorHours < 1 {
				t.Fatalf("PriorHours = %d, want ≥ 1", d.PriorHours)
			}
			if d.PriorHours != len(baseline) {
				t.Fatalf("PriorHours = %d, want %d (every row in its own prior bucket)", d.PriorHours, len(baseline))
			}

			// Youngest POPULATED prior bucket: the latest hour bucket
			// strictly before the upload hour carrying spend.
			uploadHour := clock.Truncate(time.Hour)
			var youngest time.Time
			var youngestUSD float64
			byBucket := map[time.Time]float64{}
			for _, smp := range baseline {
				b := smp.Time.Truncate(time.Hour)
				if !b.Before(uploadHour) {
					continue
				}
				byBucket[b] += smp.USD
				if b.After(youngest) {
					youngest = b
				}
			}
			youngestUSD = byBucket[youngest]
			if !youngest.Equal(wantYoungest) {
				t.Fatalf("youngest populated prior bucket = %s, want %s (seed hour − 1h)", youngest.Format(time.RFC3339), wantYoungest.Format(time.RFC3339))
			}
			if youngestUSD <= 0 {
				t.Fatalf("youngest prior bucket %s carries usd %g, want > 0", youngest.Format(time.RFC3339), youngestUSD)
			}
			if gap := uploadHour.Sub(youngest); gap >= spendalert.Window {
				t.Fatalf("youngest prior bucket is %s before the upload hour, outside the %s window", gap, spendalert.Window)
			}
		})
	}
}

// TestScenario_TraceUploadTarget_EarlyHourSeedSharesBucket is the early-hour
// sibling of the boundary test above (#3326 fix-up, review concern 53854d82):
// with the seed clock pinned at 10:02:00Z the 1h5m row lands at 08:57Z and
// SHARES bucket 08 with the 2h row (08:02Z), so the four rows populate only
// THREE distinct prior buckets (08/07/06), the youngest populated prior
// bucket is seed-hour MINUS TWO, and that bucket sums both rows' spend. This
// pins what the YAML header and docs/acceptance-preview.md now state as the
// clock-independent guarantee: three distinct prior buckets inside the 24h
// Window and a tripped alert at both the same-hour and post-crossing upload
// clocks — NOT four distinct buckets, which holds only for a seed at or past
// :05. The boundary test's 10:59:59 pin is unchanged.
//
// COUNTERFACTUAL (run against the YAML, observed): drop the 2h row → RED
// ("rows landed in buckets [8 7 6], want [8 8 7 6]"); had the bucket list
// matched, the shared-bucket sum and PriorHours == 3 assertions each fail
// independently on a missing 1h5m / 2h row.
func TestScenario_TraceUploadTarget_EarlyHourSeedSharesBucket(t *testing.T) {
	s := mustLoad(t, "trace-upload-target")
	seedClock := time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC)
	wantYoungest := seedClock.Truncate(time.Hour).Add(-2 * time.Hour) // 08:00Z

	var baseline []spendalert.Sample
	wantBuckets := []int{8, 8, 7, 6}
	var gotBuckets []int
	var sharedBucketUSD float64
	for _, row := range s.Audit {
		age, err := row.AgeDuration()
		if err != nil {
			t.Fatalf("age %q: %v", row.Age, err)
		}
		usd := costUSD(t, row.Payload)
		at := seedClock.Add(-age)
		gotBuckets = append(gotBuckets, at.Hour())
		if at.Truncate(time.Hour).Equal(wantYoungest) {
			sharedBucketUSD += usd
		}
		t.Logf("row age=%s → %s (bucket %02d:00Z) usd=%g", row.Age, at.Format(time.RFC3339), at.Hour(), usd)
		baseline = append(baseline, spendalert.Sample{Time: at, USD: usd})
	}
	if !slices.Equal(gotBuckets, wantBuckets) {
		t.Fatalf("rows landed in buckets %v, want %v (1h5m and 2h share bucket 08 at a :02 seed)", gotBuckets, wantBuckets)
	}
	if sharedBucketUSD <= costUSD(t, s.Audit[0].Payload) {
		t.Fatalf("shared bucket %s sums usd %g, want the 1h5m AND 2h rows folded together", wantYoungest.Format(time.RFC3339), sharedBucketUSD)
	}

	for _, clock := range []time.Time{
		seedClock, // same hour as the seed
		time.Date(2026, 1, 1, 11, 0, 1, 0, time.UTC), // hour boundary crossed
	} {
		t.Run(clock.Format("15:04:05"), func(t *testing.T) {
			samples := append(slices.Clone(baseline), spendalert.Sample{Time: clock, USD: 5})
			d := spendalert.Evaluate(samples, clock, 0)
			t.Logf("clock=%s tripped=%v prior=%d avg=%g ratio=%g", clock.Format(time.RFC3339), d.Tripped, d.PriorHours, d.RollingAvgUSD, d.Ratio)
			if !d.Tripped {
				t.Fatalf("spend alert did not trip at %s: %+v", clock.Format(time.RFC3339), d)
			}
			if d.PriorHours != 3 {
				t.Fatalf("PriorHours = %d, want 3 (four rows in three distinct prior buckets)", d.PriorHours)
			}
			uploadHour := clock.Truncate(time.Hour)
			var youngest time.Time
			for _, smp := range baseline {
				if b := smp.Time.Truncate(time.Hour); b.Before(uploadHour) && b.After(youngest) {
					youngest = b
				}
			}
			if !youngest.Equal(wantYoungest) {
				t.Fatalf("youngest populated prior bucket = %s, want %s (seed hour − 2h at an early-hour seed)", youngest.Format(time.RFC3339), wantYoungest.Format(time.RFC3339))
			}
			if gap := uploadHour.Sub(youngest); gap >= spendalert.Window {
				t.Fatalf("youngest prior bucket is %s before the upload hour, outside the %s window", gap, spendalert.Window)
			}
		})
	}
}

func mustLoad(t *testing.T, name string) *devfixtures.Scenario {
	t.Helper()
	s, err := devfixtures.Load(name)
	if err != nil {
		t.Fatalf("Load(%q): %v", name, err)
	}
	return s
}

func costUSD(t *testing.T, payload string) float64 {
	t.Helper()
	var p struct {
		USD float64 `json:"usd"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("payload %q: %v", payload, err)
	}
	return p.USD
}
