package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/scenario"
)

// seedCorpus writes n scenarios (issue-<i>/c, newest = highest i) under
// treeDir/acceptance/scenarios via the real writer and returns the corpus dir.
func seedCorpus(t *testing.T, treeDir string, n int) string {
	t.Helper()
	dir := filepath.Join(treeDir, "acceptance", "scenarios")
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		s := scenario.Scenario{
			ID:         fmt.Sprintf("scenario:issue-%d/c", i),
			Statement:  fmt.Sprintf("statement %d", i),
			Steps:      "steps",
			Assertions: scenario.Assertions{Expected: "expected"},
			Origin:     scenario.Origin{Issue: i, PR: 700 + i, RunID: "rec-run", HeadSHA: "sha", RecordedAt: base.Add(time.Duration(i) * time.Hour)},
		}
		if _, err := scenario.Write(dir, s); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func corpusEvents(t *testing.T, log *bytes.Buffer, event string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if m["event"] == event {
			out = append(out, m)
		}
	}
	return out
}

func scenarioIDs(set scenario.ReplaySet) []string {
	out := make([]string, 0, len(set.Scenarios))
	for _, e := range set.Scenarios {
		out = append(out, e.ScenarioID)
	}
	return out
}

func TestLoadReplayCorpus_ExcludesRetiredFromFileAndServed(t *testing.T) {
	tree := t.TempDir()
	dir := seedCorpus(t, tree, 4)
	if err := scenario.WriteRetired(dir, []scenario.RetiredEntry{{ID: "scenario:issue-0/c", Reason: "in file ledger", RunID: "r", RetiredAt: "t"}}); err != nil {
		t.Fatal(err)
	}
	served := []scenario.RetiredEntry{{ID: "scenario:issue-1/c", Reason: "served this run", RunID: "r2", PR: 742, RetiredAt: "t2"}}
	var log bytes.Buffer
	set, section, err := loadReplayCorpus(tree, served, replayCorpusConfig{maxScenarios: 25, timeCap: time.Minute}, "run-1", &log)
	if err != nil {
		t.Fatal(err)
	}
	got := scenarioIDs(set)
	for _, excluded := range []string{"scenario:issue-0/c", "scenario:issue-1/c"} {
		for _, id := range got {
			if id == excluded {
				t.Errorf("%s must be excluded (file ledger UNION served): %v", excluded, got)
			}
		}
		if strings.Contains(section, excluded) {
			t.Errorf("%s must not be rendered", excluded)
		}
	}
	if len(got) != 2 || set.RetiredExcluded != 2 || set.CorpusSize != 2 || set.Served != 2 {
		t.Errorf("set: %+v", set)
	}
	if !strings.Contains(section, "scenario:issue-3/c") || !strings.Contains(section, "scenario:issue-2/c") {
		t.Errorf("live scenarios must be rendered:\n%s", section)
	}
	ev := corpusEvents(t, &log, "acceptance_replay_corpus_loaded")
	if len(ev) != 1 || ev[0]["retired_excluded"] != "2" || ev[0]["corpus_size"] != "2" || ev[0]["served"] != "2" || ev[0]["seed"] != "run-1" {
		t.Errorf("loaded event: %v", ev)
	}
}

func TestLoadReplayCorpus_ReplaySetCarriesCapCorpusSampledOut(t *testing.T) {
	tree := t.TempDir()
	seedCorpus(t, tree, 8)
	var log bytes.Buffer
	set, section, err := loadReplayCorpus(tree, nil, replayCorpusConfig{maxScenarios: 5, timeCap: 10 * time.Minute}, "run-seed", &log)
	if err != nil {
		t.Fatal(err)
	}
	if set.Cap != 5 || set.CorpusSize != 8 || set.Served != 5 || set.SampledOut != 3 || set.RetiredExcluded != 0 || set.Seed != "run-seed" {
		t.Fatalf("header: %+v", set)
	}
	if len(set.Scenarios) != set.Served {
		t.Fatalf("one entry per served scenario: %d vs %d", len(set.Scenarios), set.Served)
	}
	if n := strings.Count(section, "- scenario: "); n != 5 {
		t.Errorf("prompt lists exactly the served set, got %d:\n%s", n, section)
	}
	if !strings.Contains(section, "10m0s") {
		t.Errorf("time cap missing:\n%s", section)
	}
	ev := corpusEvents(t, &log, "acceptance_replay_corpus_loaded")
	if len(ev) != 1 || ev[0]["cap"] != "5" || ev[0]["corpus_size"] != "8" || ev[0]["served"] != "5" || ev[0]["sampled_out"] != "3" {
		t.Errorf("loaded event: %v", ev)
	}
	// Deterministic on the run id: the same seed serves the same set.
	again, _, _ := loadReplayCorpus(tree, nil, replayCorpusConfig{maxScenarios: 5, timeCap: 10 * time.Minute}, "run-seed", &bytes.Buffer{})
	if strings.Join(scenarioIDs(again), ",") != strings.Join(scenarioIDs(set), ",") {
		t.Errorf("same run id must serve the same set")
	}
}

func TestLoadReplayCorpus_ScenariosFromFileOriginOnly(t *testing.T) {
	tree := t.TempDir()
	dir := filepath.Join(tree, "acceptance", "scenarios")
	for _, s := range []scenario.Scenario{
		{ID: "scenario:issue-101/crit-b", Statement: "s", Steps: "st", Origin: scenario.Origin{Issue: 101, PR: 742, RunID: "run-a", RecordedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}},
		{ID: "scenario:issue-102/crit-c", Statement: "s", Steps: "st", Origin: scenario.Origin{Issue: 102, PR: 0, RunID: "run-b", RecordedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}},
	} {
		if _, err := scenario.Write(dir, s); err != nil {
			t.Fatal(err)
		}
	}
	set, section, err := loadReplayCorpus(tree, nil, replayCorpusConfig{maxScenarios: 25, timeCap: time.Minute}, "run-x", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]scenario.ReplayedScenario{}
	for _, e := range set.Scenarios {
		byID[e.ScenarioID] = e
	}
	a := byID["scenario:issue-101/crit-b"]
	if a.OriginPR != 742 || a.OriginIssue != 101 || a.OriginRunID != "run-a" || a.Path != "acceptance/scenarios/issue-101/crit-b.yaml" {
		t.Errorf("attribution must come from the loaded file: %+v", a)
	}
	b := byID["scenario:issue-102/crit-c"]
	if b.OriginPR != 0 || b.OriginIssue != 102 {
		t.Errorf("unknown pr stays 0, never the issue: %+v", b)
	}
	if !strings.Contains(section, "origin PR #742") || !strings.Contains(section, "origin PR unknown") {
		t.Errorf("section:\n%s", section)
	}
	// The wire form omits origin_pr for the unknown entry.
	raw, _ := json.Marshal(b)
	if strings.Contains(string(raw), "origin_pr") {
		t.Errorf("origin_pr 0 must be absent on the wire: %s", raw)
	}
}

func TestLoadReplayCorpus_EnvKnobs(t *testing.T) {
	cases := []struct {
		max, secs string
		wantMax   int
		wantCap   time.Duration
	}{
		{"", "", 25, 600 * time.Second},
		{"0", "30", 0, 30 * time.Second},
		{"7", "0", 7, 600 * time.Second},
		{"-3", "-1", 25, 600 * time.Second},
		{"abc", "xyz", 25, 600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.max+"/"+tc.secs, func(t *testing.T) {
			t.Setenv(acceptanceReplayMaxScenariosEnv, tc.max)
			t.Setenv(acceptanceReplayTimeCapSecsEnv, tc.secs)
			got := replayCorpusConfigFromEnv()
			if got.maxScenarios != tc.wantMax || got.timeCap != tc.wantCap {
				t.Errorf("got %+v want max %d cap %s", got, tc.wantMax, tc.wantCap)
			}
		})
	}
	// An explicit 0 DISABLES replay end to end: nothing served, no section.
	tree := t.TempDir()
	seedCorpus(t, tree, 3)
	var log bytes.Buffer
	set, section, err := loadReplayCorpus(tree, nil, replayCorpusConfig{maxScenarios: 0, timeCap: time.Minute}, "run-x", &log)
	if err != nil || section != "" || set.Served != 0 || len(set.Scenarios) != 0 || set.CorpusSize != 3 || set.SampledOut != 3 {
		t.Errorf("cap 0: %+v section=%q err=%v", set, section, err)
	}
}

func TestLoadReplayCorpus_UnreadableSkipsReplayNotStage(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, dir string)
	}{
		{"undecodable scenario", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("id: [unterminated"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown scenario field", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte("id: scenario:x\nbogus: 1\norigin:\n  run_id: r\n  recorded_at: 2026-09-01T00:00:00Z\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed ledger", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, scenario.RetiredFile), []byte("retired: [\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			dir := seedCorpus(t, tree, 2)
			tc.seed(t, dir)
			var log bytes.Buffer
			set, section, err := loadReplayCorpus(tree, nil, replayCorpusConfig{maxScenarios: 25, timeCap: time.Minute}, "run-x", &log)
			if err == nil {
				t.Fatal("unreadable corpus must return the named error")
			}
			if section != "" || set.Served != 0 || len(set.Scenarios) != 0 || set.CorpusSize != 0 {
				t.Errorf("replay must be skipped wholesale: %+v %q", set, section)
			}
			ev := corpusEvents(t, &log, "acceptance_replay_corpus_unreadable")
			if len(ev) != 1 || ev[0]["outcome"] != "replay_skipped" || ev[0]["detail"] == "" {
				t.Errorf("unreadable event: %v", ev)
			}
			if len(corpusEvents(t, &log, "acceptance_replay_corpus_loaded")) != 0 {
				t.Error("no loaded event on the unreadable path")
			}
		})
	}
	// Absent corpus directory: an empty corpus, NOT unreadable.
	var log bytes.Buffer
	set, section, err := loadReplayCorpus(t.TempDir(), nil, replayCorpusConfig{maxScenarios: 25, timeCap: time.Minute}, "run-x", &log)
	if err != nil || section != "" || set.CorpusSize != 0 {
		t.Errorf("absent corpus: %+v %q %v", set, section, err)
	}
	if len(corpusEvents(t, &log, "acceptance_replay_corpus_loaded")) != 1 {
		t.Error("absent corpus still emits the loaded event with zero counts")
	}
}

func TestAppendPromptSection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(p, []byte("PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendPromptSection(p, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "PROMPT" {
		t.Errorf("empty section must be a no-op: %q", b)
	}
	if err := appendPromptSection(p, "### Regression corpus\n"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "PROMPT\n\n### Regression corpus\n" {
		t.Errorf("got %q", b)
	}
	if err := appendPromptSection(filepath.Join(t.TempDir(), "missing"), "x"); err == nil {
		t.Error("missing prompt file must error")
	}
}
