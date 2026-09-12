package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/scenario"
)

// Replay-corpus knobs (E72.4 / #3328). Runner-deployment config read off
// the runner-process env, never agent env — acceptenv's default-deny
// allow-list excludes them, the same class as the FISHHAWK_ACCEPTANCE_PREVIEW_*
// knobs in previewprobe.go.
const (
	// acceptanceReplayMaxScenariosEnv caps how many prior scenarios one
	// acceptance pass replays. Unset / unparsable / negative → the default;
	// an explicit 0 DISABLES replay (nothing served, no prompt section).
	acceptanceReplayMaxScenariosEnv = "FISHHAWK_ACCEPTANCE_REPLAY_MAX_SCENARIOS"
	// acceptanceReplayTimeCapSecsEnv is the total replay time budget the
	// prompt states; positive-int-or-default (envSeconds).
	acceptanceReplayTimeCapSecsEnv = "FISHHAWK_ACCEPTANCE_REPLAY_TIME_CAP_SECS"

	defaultAcceptanceReplayMaxScenarios = 25
	defaultAcceptanceReplayTimeCap      = 600 * time.Second
)

// replayCorpusConfig is the resolved knob set loadReplayCorpus consumes.
type replayCorpusConfig struct {
	maxScenarios int
	timeCap      time.Duration
}

// replayCorpusConfigFromEnv reads both knobs off the runner-process env.
func replayCorpusConfigFromEnv() replayCorpusConfig {
	return replayCorpusConfig{
		maxScenarios: envNonNegativeInt(acceptanceReplayMaxScenariosEnv, defaultAcceptanceReplayMaxScenarios),
		timeCap:      envSeconds(acceptanceReplayTimeCapSecsEnv, defaultAcceptanceReplayTimeCap),
	}
}

// envNonNegativeInt parses an integer env var, falling back to def on unset,
// unparsable, or NEGATIVE values. Unlike envSeconds it honors an explicit 0,
// which is the replay-disable switch.
func envNonNegativeInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// loadReplayCorpus reads the scenario corpus under treeDir (the acceptance
// tree, i.e. the merge candidate's checkout) and returns the replay set plus
// the rendered prompt section. It:
//
//   - loads acceptance/scenarios/**.yaml and acceptance/scenarios/retired.yaml;
//   - excludes every id in the FILE ledger UNION the servedRetired entries
//     (retirements approved on this run that have not been merged into the
//     ledger yet), counting them into ReplaySet.RetiredExcluded;
//   - samples with the run id as the seed (scenario.Sample), so cap,
//     corpus_size, served and sampled_out are computed once, here;
//   - fills ReplaySet.Scenarios from the LOADED FILES — the only attribution
//     source; agent prose is never consulted;
//   - emits acceptance_replay_corpus_loaded {corpus_size, retired_excluded,
//     served, sampled_out, cap, seed}.
//
// An unreadable corpus or ledger (undecodable YAML, unknown field, missing
// origin) emits acceptance_replay_corpus_unreadable and returns the error
// with a ZERO set and an empty section: replay is skipped, the stage
// proceeds — callers log nothing further and never fail the stage on it.
// An absent corpus directory is an empty corpus (counts all 0), not an error.
func loadReplayCorpus(treeDir string, servedRetired []scenario.RetiredEntry, cfg replayCorpusConfig, runID string, logSink io.Writer) (scenario.ReplaySet, string, error) {
	corpusDir := filepath.Join(treeDir, filepath.FromSlash(scenario.CorpusDir))
	all, err := scenario.Load(corpusDir)
	if err != nil {
		logEvent(logSink, "acceptance_replay_corpus_unreadable", map[string]string{
			"run_id": runID, "detail": err.Error(), "outcome": "replay_skipped",
		})
		return scenario.ReplaySet{}, "", fmt.Errorf("replay corpus: %w", err)
	}
	ledger, err := scenario.LoadRetired(corpusDir)
	if err != nil {
		logEvent(logSink, "acceptance_replay_corpus_unreadable", map[string]string{
			"run_id": runID, "detail": err.Error(), "outcome": "replay_skipped",
		})
		return scenario.ReplaySet{}, "", fmt.Errorf("replay corpus: %w", err)
	}
	retired := make(map[string]bool, len(ledger)+len(servedRetired))
	for _, e := range ledger {
		retired[e.ID] = true
	}
	for _, e := range servedRetired {
		retired[e.ID] = true
	}
	live := make([]scenario.Scenario, 0, len(all))
	excluded := 0
	for _, s := range all {
		if retired[s.ID] {
			excluded++
			continue
		}
		live = append(live, s)
	}
	chosen, set := scenario.Sample(live, cfg.maxScenarios, runID)
	set.RetiredExcluded = excluded
	set.Scenarios = scenario.Entries(chosen)
	logEvent(logSink, "acceptance_replay_corpus_loaded", map[string]string{
		"run_id":           runID,
		"corpus_size":      strconv.Itoa(set.CorpusSize),
		"retired_excluded": strconv.Itoa(set.RetiredExcluded),
		"served":           strconv.Itoa(set.Served),
		"sampled_out":      strconv.Itoa(set.SampledOut),
		"cap":              strconv.Itoa(set.Cap),
		"seed":             set.Seed,
	})
	return set, scenario.RenderPromptSection(chosen, cfg.timeCap), nil
}

// appendPromptSection appends section to the fetched prompt file, separated
// by a blank line. An empty section is a no-op (nothing served).
func appendPromptSection(promptFile, section string) error {
	if section == "" {
		return nil
	}
	f, err := os.OpenFile(promptFile, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("append prompt section: %w", err)
	}
	if _, err := f.WriteString("\n\n" + section); err != nil {
		_ = f.Close()
		return fmt.Errorf("append prompt section: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("append prompt section: %w", err)
	}
	return nil
}
