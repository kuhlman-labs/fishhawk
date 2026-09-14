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
	// Positive control for the #3396 guard: a nested NON-symlink dir that
	// does not exist yet is still created and written (MkdirAll unchanged).
	nested := want
	nested.ID = "scenario:issue-303/crit-z"
	if _, err := Write(dir, nested); err != nil {
		t.Fatalf("plain nested write must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "issue-303", "crit-z.yaml")); err != nil {
		t.Errorf("nested write missing: %v", err)
	}
}

// symlinkFixture returns a corpus root and an OUTSIDE directory (a sibling
// under the same t.TempDir so the two are never nested) plus a scenario to
// write; the caller plants the hostile symlink under root.
func symlinkFixture(t *testing.T) (root, outside string, s Scenario) {
	t.Helper()
	base := t.TempDir()
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s = Scenario{
		ID: "scenario:issue-101/crit-b", Statement: "st", Steps: "steps",
		Origin: Origin{Issue: 101, RunID: "run", RecordedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}
	return root, outside, s
}

// assertOutsideUntouched: the outside dir holds exactly want entries (no
// scenario yaml, no retired.yaml, no leaked .*.tmp) and every named path
// under root is STILL a symlink (the guard neither followed nor replaced it).
func assertOutsideUntouched(t *testing.T, outside string, want []string, stillLinks ...string) {
	t.Helper()
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("outside dir entries = %v, want %v (a write escaped root)", got, want)
	}
	for _, p := range stillLinks {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Errorf("%s must still be a symlink after the refusal: lstat: %v", p, err)
		} else if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s must still be a symlink after the refusal, mode %v (the write replaced it)", p, fi.Mode())
		}
	}
}

// TestWrite_RefusesSymlinkedDirComponent (#3396): root/issue-101 is a REAL
// symlink to a directory outside root. Write must refuse, naming the
// component, and nothing — not the yaml, not a temp file — may land outside.
func TestWrite_RefusesSymlinkedDirComponent(t *testing.T) {
	root, outside, s := symlinkFixture(t)
	link := filepath.Join(root, "issue-101")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := Write(root, s)
	if err == nil || !strings.Contains(err.Error(), `symlinked path component "issue-101"`) {
		t.Errorf("Write err = %v, want a refusal naming issue-101", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "crit-b.yaml")); err == nil {
		t.Error("crit-b.yaml landed outside root through the symlink")
	}
	assertOutsideUntouched(t, outside, nil, link)
}

// TestWrite_RefusesSymlinkedLeaf (#3396): the LEAF crit-b.yaml is a symlink
// to a file outside root with known bytes. Write refuses naming the leaf and
// the target's bytes are unchanged (rename onto a symlink would replace the
// link entry, not the target — the refusal is belt-and-braces).
func TestWrite_RefusesSymlinkedLeaf(t *testing.T) {
	root, outside, s := symlinkFixture(t)
	target := filepath.Join(outside, "target.yaml")
	if err := os.WriteFile(target, []byte("known\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "issue-101"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "issue-101", "crit-b.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := Write(root, s)
	if err == nil || !strings.Contains(err.Error(), `symlinked path component "crit-b.yaml"`) {
		t.Errorf("Write err = %v, want a refusal naming crit-b.yaml", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "known\n" {
		t.Errorf("target bytes changed: %q", b)
	}
	assertOutsideUntouched(t, outside, []string{"target.yaml"}, link)
}

// TestWriteRetired_RefusesSymlinkedLedger (#3396): WriteRetired shares
// writeAtomic, so a symlinked retired.yaml is refused the same way.
func TestWriteRetired_RefusesSymlinkedLedger(t *testing.T) {
	root, outside, _ := symlinkFixture(t)
	target := filepath.Join(outside, "x.yaml")
	if err := os.WriteFile(target, []byte("known\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, RetiredFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := WriteRetired(root, []RetiredEntry{{ID: "scenario:issue-7/old", Reason: "r"}})
	if err == nil || !strings.Contains(err.Error(), `symlinked path component "retired.yaml"`) {
		t.Errorf("WriteRetired err = %v, want a refusal naming retired.yaml", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "known\n" {
		t.Errorf("target bytes changed: %q", b)
	}
	assertOutsideUntouched(t, outside, []string{"x.yaml"}, link)
}

// TestRefuseSymlinks_NonDirectoryComponentAndMissingTail: an existing
// intermediate that is a regular FILE is refused by name; a path whose
// components do not exist yet passes (MkdirAll creates them).
func TestRefuseSymlinks_NonDirectoryComponentAndMissingTail(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "issue-101"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RefuseSymlinks(root, "issue-101/crit-b.yaml")
	if err == nil || !strings.Contains(err.Error(), `non-directory path component "issue-101"`) {
		t.Fatalf("err = %v, want a non-directory refusal naming issue-101", err)
	}
	if err := RefuseSymlinks(root, "issue-202/deep/crit-b.yaml"); err != nil {
		t.Fatalf("absent components must pass: %v", err)
	}
	if err := RefuseSymlinks(root, "issue-101"); err != nil {
		t.Fatalf("a regular-file LEAF is overwritable, not refused: %v", err)
	}
}

// TestRefuseSymlinks_RejectsParentAndAbsoluteRel: RefuseSymlinks is
// exported, so it cannot rely on every caller feeding it PathFor output —
// a `..` component (which filepath.Join would normalize UP and out of root
// before any lstat) and an absolute rel are refused by the function itself,
// and nothing outside root is lstat-walked or created.
func TestRefuseSymlinks_RejectsParentAndAbsoluteRel(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a/../../x.yaml", "../x.yaml", "..", "a/.."} {
		err := RefuseSymlinks(root, rel)
		if err == nil || !strings.Contains(err.Error(), "parent-directory path component") {
			t.Errorf("RefuseSymlinks(%q) err = %v, want a parent-directory refusal", rel, err)
		}
	}
	err := RefuseSymlinks(root, "/etc/x.yaml")
	if err == nil || !strings.Contains(err.Error(), "refusing absolute path") {
		t.Errorf("absolute rel err = %v, want an absolute-path refusal", err)
	}
	if err := RefuseSymlinks(root, "a/./b.yaml"); err != nil {
		t.Errorf("a `.` component is inert and must pass: %v", err)
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
		"COMPLETE, standalone reproduction recipe",
		"Never write it by reference to another scenario",
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

func TestAmend(t *testing.T) {
	// A prior genuine recording and a fresh re-record; the two differ in every
	// field so a wrong pick is visible.
	priorOrigin := Origin{Issue: 101, PR: 700, RunID: "r0", HeadSHA: "abc", RecordedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	nextOrigin := Origin{Issue: 101, PR: 742, RunID: "r1", HeadSHA: "def", RecordedAt: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)}
	longSteps := strings.Repeat("drive the two-run replay and assert the corpus. ", 6) // >200 chars
	shortGenuine := "GET /runs/<id>"

	prev := func(steps string) Scenario {
		return Scenario{ID: "scenario:issue-101/crit-b", Statement: "OLD statement", VerifyHint: "old hint", Seed: "old-seed",
			Steps: steps, Assertions: Assertions{Expected: "old exp", ObservedAtRecord: "old obs", ReproHandle: "old-handle"}, Origin: priorOrigin}
	}
	next := func(steps string, a Assertions) Scenario {
		return Scenario{ID: "scenario:issue-101/crit-b", Statement: "NEW statement", VerifyHint: "new hint", Seed: "new-seed",
			Steps: steps, Assertions: a, Origin: nextOrigin}
	}

	t.Run("shorter new keeps prior steps and discloses", func(t *testing.T) {
		out, rep := Amend(prev(longSteps), next(shortGenuine, Assertions{Expected: "new exp", ObservedAtRecord: "new obs", ReproHandle: "new-handle"}))
		if rep.StepsKept != "prior" || out.Steps != longSteps {
			t.Fatalf("want prior steps kept, got kept=%s steps=%q", rep.StepsKept, out.Steps)
		}
		if out.StepsCarriedFrom == "" || !strings.Contains(out.StepsCarriedFrom, "abc") {
			t.Errorf("displaced genuine steps must be disclosed naming the prior origin, got %q", out.StepsCarriedFrom)
		}
	})
	t.Run("longer new takes new steps and discloses nothing", func(t *testing.T) {
		out, rep := Amend(prev(shortGenuine), next(longSteps, Assertions{Expected: "e"}))
		if rep.StepsKept != "new" || out.Steps != longSteps || out.StepsCarriedFrom != "" {
			t.Fatalf("want new steps, no disclosure: kept=%s steps=%q carried=%q", rep.StepsKept, out.Steps, out.StepsCarriedFrom)
		}
	})
	t.Run("equal length ties to new", func(t *testing.T) {
		out, rep := Amend(prev("aaaa"), next("bbbb", Assertions{Expected: "e"}))
		if rep.StepsKept != "new" || out.Steps != "bbbb" {
			t.Errorf("tie must take new: kept=%s steps=%q", rep.StepsKept, out.Steps)
		}
	})
	t.Run("new fallback keeps prior steps without disclosure", func(t *testing.T) {
		out, rep := Amend(prev(shortGenuine), next(StepsNotRecorded, Assertions{Expected: "e"}))
		if rep.StepsKept != "prior" || out.Steps != shortGenuine {
			t.Fatalf("a fallback re-record must keep genuine prior steps: kept=%s steps=%q", rep.StepsKept, out.Steps)
		}
		if out.StepsCarriedFrom != "" {
			t.Errorf("a fallback displaced nothing, so no disclosure: %q", out.StepsCarriedFrom)
		}
	})
	t.Run("prior fallback never wins (condition 2)", func(t *testing.T) {
		// prev is the fallback (28 chars); next is genuine but SHORTER (14).
		out, rep := Amend(prev(StepsNotRecorded), next(shortGenuine, Assertions{Expected: "e"}))
		if rep.StepsKept != "new" || out.Steps != shortGenuine {
			t.Fatalf("a prior fallback must not beat genuine new steps: kept=%s steps=%q", rep.StepsKept, out.Steps)
		}
		if out.StepsCarriedFrom != "" {
			t.Errorf("no genuine prior steps were kept, so no disclosure: %q", out.StepsCarriedFrom)
		}
	})
	t.Run("both fallback takes new", func(t *testing.T) {
		out, rep := Amend(prev(StepsNotRecorded), next(StepsNotRecorded, Assertions{Expected: "e"}))
		if rep.StepsKept != "new" || out.Steps != StepsNotRecorded {
			t.Errorf("both fallback: kept=%s steps=%q", rep.StepsKept, out.Steps)
		}
	})
	t.Run("empty new assertions keep prior, non-empty take new", func(t *testing.T) {
		out, _ := Amend(prev(longSteps), next(shortGenuine, Assertions{Expected: "  ", ObservedAtRecord: "", ReproHandle: "new-handle"}))
		if out.Assertions.Expected != "old exp" || out.Assertions.ObservedAtRecord != "old obs" {
			t.Errorf("blank new assertions must keep prior: %+v", out.Assertions)
		}
		if out.Assertions.ReproHandle != "new-handle" {
			t.Errorf("non-empty new repro_handle must take new: %q", out.Assertions.ReproHandle)
		}
	})
	t.Run("origin statement verify_hint seed always new", func(t *testing.T) {
		out, _ := Amend(prev(longSteps), next(shortGenuine, Assertions{Expected: "e"}))
		if out.Origin != nextOrigin || out.Statement != "NEW statement" || out.VerifyHint != "new hint" || out.Seed != "new-seed" {
			t.Errorf("plan-authoritative fields must come from next: %+v", out)
		}
	})
	t.Run("chained disclosure preserves the original source", func(t *testing.T) {
		p := prev(longSteps)
		p.StepsCarriedFrom = "head ORIGINAL recorded_at 2025-01-01T00:00:00Z"
		out, _ := Amend(p, next(shortGenuine, Assertions{Expected: "e"}))
		if out.StepsCarriedFrom != p.StepsCarriedFrom {
			t.Errorf("a chained re-record must keep the deepest disclosure: %q", out.StepsCarriedFrom)
		}
	})
}

func TestExisting(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := Existing(dir, "scenario:issue-101/crit-b"); err != nil || found {
		t.Fatalf("absent → (_, false, nil), got found=%v err=%v", found, err)
	}
	want := Scenario{ID: "scenario:issue-101/crit-b", Statement: "st", Steps: "steps",
		Assertions: Assertions{Expected: "e"}, Origin: Origin{Issue: 101, RunID: "r", RecordedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}}
	if _, err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := Existing(dir, "scenario:issue-101/crit-b")
	if err != nil || !found || got.ID != want.ID || got.Path != "issue-101/crit-b.yaml" {
		t.Fatalf("valid → loaded, got %+v found=%v err=%v", got, found, err)
	}
	// Malformed → named error.
	writeFile(t, filepath.Join(dir, "issue-9", "bad.yaml"), "id: scenario:issue-9/bad\nbogus_field: 1\n")
	if _, _, err := Existing(dir, "scenario:issue-9/bad"); err == nil || !strings.Contains(err.Error(), "issue-9/bad.yaml") {
		t.Errorf("malformed → named error, got %v", err)
	}
	// Symlinked leaf → named refusal.
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "issue-101"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "target.yaml"), filepath.Join(root, "issue-101", "crit-b.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Existing(root, "scenario:issue-101/crit-b"); err == nil || !strings.Contains(err.Error(), `symlinked path component "crit-b.yaml"`) {
		t.Errorf("symlinked leaf → named refusal, got %v", err)
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
