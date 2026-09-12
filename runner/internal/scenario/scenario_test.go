package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoad_ValidFixture(t *testing.T) {
	got, err := Load("testdata")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 scenario (retired.yaml excluded), got %d: %+v", len(got), got)
	}
	s := got[0]
	if s.ID != "scenario:issue-101/crit-b" || s.Path != "valid.yaml" || s.Seed != "split-parent-linked" {
		t.Errorf("unexpected scenario: %+v", s)
	}
	if s.Origin.Issue != 101 || s.Origin.RunID == "" || s.Origin.RecordedAt.IsZero() {
		t.Errorf("origin not decoded: %+v", s.Origin)
	}
	if s.Assertions.ReproHandle == "" {
		t.Errorf("assertions not decoded: %+v", s.Assertions)
	}
}

func TestLoad_MissingDirIsEmptyCorpus(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(got) != 0 {
		t.Fatalf("want empty/nil, got %v / %v", got, err)
	}
}

func TestLoad_StrictDecode(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(readFixture(t, "valid.yaml"), "seed: split-parent-linked", "seed: split-parent-linked\nbogus_field: 1", 1)
	writeFile(t, filepath.Join(dir, "issue-101", "crit-b.yaml"), body)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("unknown field must be a named error, not a silent skip")
	}
	if !strings.Contains(err.Error(), "issue-101/crit-b.yaml") || !strings.Contains(err.Error(), "bogus_field") {
		t.Errorf("error must name the file and the field: %v", err)
	}
}

func TestLoad_NamedErrors(t *testing.T) {
	valid := readFixture(t, "valid.yaml")
	cases := []struct {
		name, body, want string
	}{
		{"undecodable yaml", "id: [unterminated", "decode"},
		{"missing prefix", strings.Replace(valid, "id: scenario:issue-101/crit-b", "id: issue-101/crit-b", 1), "missing \"scenario:\" prefix"},
		{"missing run_id", strings.Replace(valid, "  run_id: 11111111-1111-1111-1111-111111111111\n", "", 1), "origin.run_id: missing"},
		{"missing recorded_at", strings.Replace(valid, "  recorded_at: 2026-09-01T10:00:00Z\n", "", 1), "origin.recorded_at: missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "x.yaml"), tc.body)
			_, err := Load(dir)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "x.yaml") {
				t.Errorf("want error naming x.yaml and %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoad_OriginPRZeroIsUnknownNotError(t *testing.T) {
	got, err := Load("testdata")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got[0].Origin.PR != 0 {
		t.Fatalf("fixture pins pr: 0, got %d", got[0].Origin.PR)
	}
	sec := RenderPromptSection(got, time.Minute)
	if !strings.Contains(sec, "origin PR unknown") {
		t.Errorf("pr 0 must render as unknown:\n%s", sec)
	}
}

func TestLoadRetired_RoundTripsFullEntry(t *testing.T) {
	want := []RetiredEntry{{
		ID: "scenario:issue-100/crit-a", Reason: "behaviour replaced by #3327",
		RunID: "22222222-2222-2222-2222-222222222222", PR: 742, RetiredAt: "2026-09-02T09:30:00Z",
	}}
	got, err := LoadRetired("testdata")
	if err != nil {
		t.Fatalf("LoadRetired fixture: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture load: got %+v want %+v", got, want)
	}
	dir := t.TempDir()
	if err := WriteRetired(dir, want); err != nil {
		t.Fatal(err)
	}
	back, err := LoadRetired(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("Write->Load: got %+v want %+v", back, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("atomic write must leave no temp file: %v", entries)
	}
}

func TestLoadRetired_AbsentIsEmptyMalformedIsNamed(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadRetired(dir)
	if err != nil || got != nil {
		t.Fatalf("absent ledger: got %v / %v", got, err)
	}
	writeFile(t, filepath.Join(dir, RetiredFile), "retired:\n  - id: x\n    bogus: 1\n")
	if _, err := LoadRetired(dir); err == nil || !strings.Contains(err.Error(), "retired.yaml") {
		t.Errorf("unknown field must be a named error: %v", err)
	}
	writeFile(t, filepath.Join(dir, RetiredFile), "retired:\n  - reason: no id\n")
	if _, err := LoadRetired(dir); err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Errorf("entry without id must be a named error: %v", err)
	}
	writeFile(t, filepath.Join(dir, RetiredFile), "retired: [\n")
	if _, err := LoadRetired(dir); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("undecodable must be a named error: %v", err)
	}
}

func TestMergeRetired_IdempotentOnIDKeepsReason(t *testing.T) {
	existing := []RetiredEntry{{ID: "scenario:a", Reason: "original reason", RunID: "r1", PR: 1, RetiredAt: "t1"}}
	incoming := []RetiredEntry{
		{ID: "scenario:a", Reason: "REPLACED", RunID: "r2", PR: 2, RetiredAt: "t2"},
		{ID: "scenario:b", Reason: "new", RunID: "r2", PR: 2, RetiredAt: "t2"},
		{ID: "scenario:b", Reason: "dup within incoming"},
		{ID: "", Reason: "blank id dropped"},
	}
	got := MergeRetired(existing, incoming)
	want := []RetiredEntry{existing[0], incoming[1]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	again := MergeRetired(got, incoming)
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("not idempotent: %+v", again)
	}
	if existing[0].Reason != "original reason" {
		t.Error("input mutated")
	}
}

func corpus(n int) []Scenario {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Scenario, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Scenario{
			ID:     fmt.Sprintf("scenario:issue-%d/c", i),
			Path:   fmt.Sprintf("issue-%d/c.yaml", i),
			Origin: Origin{Issue: i, PR: 700 + i, RunID: "r", RecordedAt: base.Add(time.Duration(i) * time.Hour)},
		})
	}
	return out
}

func ids(list []Scenario) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

func TestSample_KeepsNewestHalfAndSamplesOlderDeterministically(t *testing.T) {
	list := corpus(12) // issue-11 is newest
	chosen, set := Sample(list, 6, "run-seed-a")
	if len(chosen) != 6 || set.Served != 6 || set.SampledOut != 6 || set.CorpusSize != 12 || set.Cap != 6 || set.Seed != "run-seed-a" {
		t.Fatalf("header/count: %+v (%d chosen)", set, len(chosen))
	}
	got := ids(chosen)
	for _, want := range []string{"scenario:issue-11/c", "scenario:issue-10/c", "scenario:issue-9/c"} {
		if got[0] != "scenario:issue-11/c" || !contains(got, want) {
			t.Errorf("newest three must always be present, newest first: %v", got)
		}
	}
	older := 0
	for _, id := range got[3:] {
		if id == "scenario:issue-11/c" || id == "scenario:issue-10/c" || id == "scenario:issue-9/c" {
			t.Errorf("drawn half must come from the older pool: %v", got)
		}
		older++
	}
	if older != 3 {
		t.Errorf("want 3 drawn from the older 9, got %d", older)
	}
	same, _ := Sample(list, 6, "run-seed-a")
	if !reflect.DeepEqual(ids(same), got) {
		t.Errorf("same seed must draw the same set: %v vs %v", ids(same), got)
	}
	// Any fixed seed pair could coincide by chance; require that SOME other
	// seed rotates the older half, which a seed-ignoring sampler cannot do.
	rotated := false
	for i := 0; i < 32 && !rotated; i++ {
		other, _ := Sample(list, 6, fmt.Sprintf("run-seed-%d", i))
		rotated = !reflect.DeepEqual(ids(other)[3:], got[3:])
	}
	if !rotated {
		t.Errorf("a different seed must rotate the older half; every seed drew %v", got[3:])
	}
	// Input order must not matter: a shuffled input yields the same set.
	shuffled := append([]Scenario(nil), list...)
	sort.Slice(shuffled, func(i, j int) bool { return shuffled[i].ID > shuffled[j].ID })
	fromShuffled, _ := Sample(shuffled, 6, "run-seed-a")
	if !reflect.DeepEqual(ids(fromShuffled), got) {
		t.Errorf("input order leaked into the sample: %v vs %v", ids(fromShuffled), got)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestSample_ReturnsHeaderCounts(t *testing.T) {
	chosen, set := Sample(corpus(8), 5, "run-x")
	if set.CorpusSize != 8 || set.Served != 5 || set.SampledOut != 3 || set.Cap != 5 || len(chosen) != 5 {
		t.Fatalf("cap 5 over 8: %+v (%d chosen)", set, len(chosen))
	}
	// Under the cap: everything served, nothing sampled out, newest first.
	all, set := Sample(corpus(3), 5, "run-x")
	if set.Served != 3 || set.SampledOut != 0 || set.CorpusSize != 3 || ids(all)[0] != "scenario:issue-2/c" {
		t.Fatalf("under cap: %+v %v", set, ids(all))
	}
}

func TestSample_ZeroCapDisables(t *testing.T) {
	for _, cap := range []int{0, -1} {
		chosen, set := Sample(corpus(4), cap, "run-x")
		if len(chosen) != 0 || set.Served != 0 || set.SampledOut != 4 || set.CorpusSize != 4 || set.Cap != cap {
			t.Errorf("cap %d: %+v %v", cap, set, chosen)
		}
	}
}

func TestEntries_FromLoadedFilesOnly(t *testing.T) {
	got := Entries(corpus(2))
	want := []ReplayedScenario{
		{ScenarioID: "scenario:issue-0/c", OriginPR: 700, OriginIssue: 0, OriginRunID: "r", Path: "acceptance/scenarios/issue-0/c.yaml"},
		{ScenarioID: "scenario:issue-1/c", OriginPR: 701, OriginIssue: 1, OriginRunID: "r", Path: "acceptance/scenarios/issue-1/c.yaml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCompose_OnlyDrivablePassedCriteria(t *testing.T) {
	criteria := []Criterion{
		{ID: "crit-a", Statement: "A", Drivable: true},
		{ID: "crit-skip", Statement: "S", Drivable: false},
		{ID: "crit-failed", Statement: "F", Drivable: true},
		{ID: "crit-norow", Statement: "N", Drivable: true},
		{ID: "crit-undecidable", Statement: "U", Drivable: true},
	}
	results := []CriterionResult{
		{ID: "crit-a", Result: "passed", StepsTaken: "did A", Expected: "A ok", Observed: "A was ok", ReproHandle: "h"},
		{ID: "crit-skip", Result: "passed"},
		{ID: "crit-failed", Result: "failed"},
		{ID: "crit-undecidable", Result: "undecidable"},
	}
	origin := Origin{Issue: 101, PR: 742, RunID: "r", HeadSHA: "h", RecordedAt: time.Unix(0, 0).UTC()}
	got := Compose(criteria, results, origin)
	if len(got) != 1 {
		t.Fatalf("want exactly the drivable passed criterion, got %v", ids(got))
	}
	s := got[0]
	if s.ID != "scenario:issue-101/crit-a" || s.Steps != "did A" || s.Assertions.Expected != "A ok" ||
		s.Assertions.ObservedAtRecord != "A was ok" || s.Assertions.ReproHandle != "h" || s.Origin != origin {
		t.Errorf("composed: %+v", s)
	}
	for _, excluded := range []string{"crit-skip", "crit-failed", "crit-norow", "crit-undecidable"} {
		if contains(ids(got), IDPrefix+"issue-101/"+excluded) {
			t.Errorf("%s must be excluded", excluded)
		}
	}
}

func TestCompose_EmptyStepsTakenRecordsFallbackNotEmpty(t *testing.T) {
	got := Compose(
		[]Criterion{{ID: "c", Drivable: true}},
		[]CriterionResult{{ID: "c", Result: "passed", StepsTaken: "   "}},
		Origin{Issue: 1},
	)
	if len(got) != 1 || got[0].Steps != StepsNotRecorded {
		t.Fatalf("got %+v", got)
	}
}

func TestWrite_RoundTripsLoad(t *testing.T) {
	dir := t.TempDir()
	want := Scenario{
		ID: "scenario:issue-101/crit-b", Statement: "st", VerifyHint: "vh", Seed: "sd", Steps: "steps",
		Assertions: Assertions{Expected: "e", ObservedAtRecord: "o", ReproHandle: "r"},
		Origin:     Origin{Issue: 101, PR: 742, RunID: "run", HeadSHA: "sha", RecordedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}
	rel, err := Write(dir, want)
	if err != nil {
		t.Fatal(err)
	}
	if rel != "issue-101/crit-b.yaml" {
		t.Errorf("rel path: %s", rel)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want.Path = rel
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if _, err := Write(dir, Scenario{ID: "no-prefix"}); err == nil {
		t.Error("id without prefix must be refused")
	}
	if _, err := Write(dir, Scenario{ID: "scenario:../escape"}); err == nil {
		t.Error("path traversal must be refused")
	}
}

func TestWireTypes_ExactJSONKeys(t *testing.T) {
	keys := func(v any) []string {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	set := ReplaySet{Cap: 5, CorpusSize: 8, Served: 5, SampledOut: 3, Seed: "s", Scenarios: []ReplayedScenario{{ScenarioID: "x", OriginPR: 742, OriginIssue: 101}}}
	if got, want := keys(set), []string{"cap", "corpus_size", "retired_excluded", "sampled_out", "scenarios", "seed", "served"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReplaySet keys %v want %v", got, want)
	}
	if got, want := keys(set.Scenarios[0]), []string{"origin_issue", "origin_pr", "origin_run_id", "path", "scenario_id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReplayedScenario keys %v want %v", got, want)
	}
	if got, want := keys(ReplayedScenario{}), []string{"origin_issue", "origin_run_id", "path", "scenario_id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("origin_pr 0 must be OMITTED (absent, never an issue number): %v", got)
	}
	if got, want := keys(RetiredEntry{}), []string{"id", "pr", "reason", "retired_at", "run_id"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RetiredEntry keys %v want %v", got, want)
	}
	// The zero ReplaySet keeps every count on the wire so the backend can
	// copy cap/corpus_size/sampled_out verbatim even when they are 0.
	if got := keys(ReplaySet{}); len(got) != 7 {
		t.Errorf("ReplaySet counts must not be omitempty: %v", got)
	}
}

func TestRenderPromptSection(t *testing.T) {
	known := Scenario{ID: "scenario:issue-101/crit-b", Statement: "the statement", Seed: "split-parent-linked",
		Steps: "the steps", Assertions: Assertions{Expected: "the expected"}, Origin: Origin{Issue: 101, PR: 742}}
	unknown := Scenario{ID: "scenario:issue-102/crit-c", Statement: "s2", Steps: "st2", Assertions: Assertions{Expected: "e2"}, Origin: Origin{Issue: 102, PR: 0}}
	sec := RenderPromptSection([]Scenario{known, unknown}, 10*time.Minute)
	for _, want := range []string{
		"### Regression corpus",
		"FIRST, before the criteria",
		"scenario: scenario:issue-101/crit-b",
		"statement: the statement",
		"seed: split-parent-linked",
		"steps: the steps",
		"expected: the expected",
		"origin PR #742",
		"scenario: scenario:issue-102/crit-c",
		"origin PR unknown",
		"10m0s",
		"expectation_basis: replay_budget_exhausted",
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("missing %q in:\n%s", want, sec)
		}
	}
	if strings.Contains(sec, "origin PR #101") || strings.Contains(sec, "origin PR #102") {
		t.Errorf("issue number must never be rendered as a PR:\n%s", sec)
	}
	if strings.Count(sec, "seed:") != 1 {
		t.Errorf("seed line only when set:\n%s", sec)
	}
	if RenderPromptSection(nil, time.Minute) != "" {
		t.Error("no scenarios → empty section")
	}
}

func TestPathFor(t *testing.T) {
	if p, err := PathFor("scenario:issue-1/x"); err != nil || p != "issue-1/x.yaml" {
		t.Errorf("%s %v", p, err)
	}
	for _, bad := range []string{"", "scenario:", "issue-1/x", "scenario:/abs", "scenario:a/../b"} {
		if _, err := PathFor(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
