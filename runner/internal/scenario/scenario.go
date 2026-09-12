// Package scenario is the replayable acceptance-scenario corpus (E72.4 /
// #3328): the YAML shape a passing, drivable acceptance criterion is
// persisted as under acceptance/scenarios/, the retirement ledger beside it
// (acceptance/scenarios/retired.yaml), the deterministic sampler that bounds
// how many prior scenarios a new acceptance pass replays, and the prompt
// section that asks the acceptance agent to replay them FIRST.
//
// The package imports only the standard library and gopkg.in/yaml.v3. It
// deliberately references NO runner/internal/upload symbol and NO backend
// symbol: it is the slice that PRODUCES the replay wire types, so it must
// build alone. The three wire types (ReplaySet, ReplayedScenario,
// RetiredEntry) carry the CROSS-MODULE WIRE CONTRACT marker; their manifest
// Pair rows are registered in backend/internal/wirecontract by the slice
// that introduces the backend twins.
package scenario

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// IDPrefix distinguishes a replayed-scenario verdict row from a criterion
// row: every scenario id is `scenario:issue-<N>/<criterion-id>`, so the
// backend can partition rows on the prefix alone.
const IDPrefix = "scenario:"

// CorpusDir is the repo-relative directory the corpus and its retirement
// ledger live under.
const CorpusDir = "acceptance/scenarios"

// RetiredFile is the retirement ledger's basename inside CorpusDir.
const RetiredFile = "retired.yaml"

// StepsNotRecorded is the literal recorded as a scenario's steps when the
// validator's passing row carried no steps_taken — a scenario always has a
// non-empty steps field so a replay never reads an empty instruction.
const StepsNotRecorded = "not recorded by the validator"

// Origin attributes a scenario to the run that recorded it. PR is an int
// with 0 meaning UNKNOWN (the originating PR could not be resolved at record
// time); it is never substituted with the issue number.
type Origin struct {
	Issue      int       `yaml:"issue" json:"issue"`
	PR         int       `yaml:"pr" json:"pr"`
	RunID      string    `yaml:"run_id" json:"run_id"`
	HeadSHA    string    `yaml:"head_sha" json:"head_sha"`
	RecordedAt time.Time `yaml:"recorded_at" json:"recorded_at"`
}

// Assertions is what the recording pass observed and what a replay must
// re-observe.
type Assertions struct {
	Expected         string `yaml:"expected" json:"expected"`
	ObservedAtRecord string `yaml:"observed_at_record" json:"observed_at_record"`
	ReproHandle      string `yaml:"repro_handle,omitempty" json:"repro_handle,omitempty"`
}

// Scenario is one persisted corpus entry — the YAML document at
// acceptance/scenarios/issue-<N>/<criterion-id>.yaml.
type Scenario struct {
	ID         string `yaml:"id" json:"id"`
	Statement  string `yaml:"statement" json:"statement"`
	VerifyHint string `yaml:"verify_hint,omitempty" json:"verify_hint,omitempty"`
	// Seed names the E72.2 seeded-fixture scenario the criterion needs
	// materialized before it can be driven; empty when none.
	Seed       string     `yaml:"seed,omitempty" json:"seed,omitempty"`
	Steps      string     `yaml:"steps" json:"steps"`
	Assertions Assertions `yaml:"assertions" json:"assertions"`
	Origin     Origin     `yaml:"origin" json:"origin"`

	// Path is the corpus-relative file the scenario was loaded from (set by
	// Load, never serialized).
	Path string `yaml:"-" json:"-"`
}

// RetiredEntry is ONE type for three surfaces: the retired.yaml row (yaml
// tags), the served prompt-field element and the push-report element (json
// tags) — so a retirement's reason cannot be lost at a conversion boundary.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to the
// backend's retired-scenario entry (served as acceptance_retired_scenarios
// and carried on the acceptance_scenarios_pushed report); the wirecontract
// parity test pins them once the backend twin is registered.
type RetiredEntry struct {
	ID        string `yaml:"id" json:"id"`
	Reason    string `yaml:"reason" json:"reason"`
	RunID     string `yaml:"run_id" json:"run_id"`
	PR        int    `yaml:"pr" json:"pr"`
	RetiredAt string `yaml:"retired_at" json:"retired_at"`
}

// ReplayedScenario is the runner-injected attribution for one served
// scenario, built from the LOADED FILE (never from agent prose).
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to the
// backend's acceptanceReplayedScenario; the wirecontract parity test pins
// them once the backend twin is registered.
type ReplayedScenario struct {
	ScenarioID  string `json:"scenario_id"`
	OriginPR    int    `json:"origin_pr,omitempty"`
	OriginIssue int    `json:"origin_issue"`
	OriginRunID string `json:"origin_run_id"`
	Path        string `json:"path"`
}

// ReplaySet is the top-level `replay` object the runner injects into the
// validated verdict body: the cap evidence (cap, corpus_size, served,
// sampled_out, retired_excluded, seed) computed ONCE at the sampling site
// plus the per-scenario attribution.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to the
// backend's acceptanceReplay; the wirecontract parity test pins them once
// the backend twin is registered.
type ReplaySet struct {
	Cap             int                `json:"cap"`
	CorpusSize      int                `json:"corpus_size"`
	Served          int                `json:"served"`
	SampledOut      int                `json:"sampled_out"`
	RetiredExcluded int                `json:"retired_excluded"`
	Seed            string             `json:"seed"`
	Scenarios       []ReplayedScenario `json:"scenarios"`
}

// Criterion is the plan-side input Compose reads: the approved criterion's
// text plus whether it is drivable (not skip_expected). Seed names the
// seeded-fixture scenario, empty when none.
type Criterion struct {
	ID            string
	Statement     string
	VerifyHint    string
	Preconditions []string
	Seed          string
	Drivable      bool
}

// CriterionResult is the verdict-side input Compose reads: the acceptance
// agent's row for one criterion.
type CriterionResult struct {
	ID          string
	Result      string
	StepsTaken  string
	Expected    string
	Observed    string
	ReproHandle string
}

// PathFor returns the corpus-relative file for a scenario id:
// `scenario:issue-101/crit-b` → `issue-101/crit-b.yaml`. An id without the
// prefix is refused.
func PathFor(id string) (string, error) {
	rest, ok := strings.CutPrefix(id, IDPrefix)
	if !ok || rest == "" {
		return "", fmt.Errorf("scenario id %q: missing %q prefix", id, IDPrefix)
	}
	if strings.Contains(rest, "..") || strings.HasPrefix(rest, "/") {
		return "", fmt.Errorf("scenario id %q: path component refused", id)
	}
	return rest + ".yaml", nil
}

// Load reads every *.yaml under dir (recursively, excluding RetiredFile)
// with a STRICT decode. An unknown field, an id without IDPrefix, a missing
// origin.run_id or origin.recorded_at, or undecodable YAML is a NAMED error
// carrying the file path — never a silent skip. A missing dir is an empty
// corpus, not an error. Origin.PR == 0 is unknown, not an error. Results
// are sorted by corpus-relative path.
func Load(dir string) ([]Scenario, error) {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scenario corpus %s: %w", dir, err)
	}
	var out []Scenario
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") || d.Name() == RetiredFile {
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		s, loadErr := loadOne(p)
		if loadErr != nil {
			return fmt.Errorf("scenario %s: %w", filepath.ToSlash(rel), loadErr)
		}
		s.Path = filepath.ToSlash(rel)
		out = append(out, s)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func loadOne(path string) (Scenario, error) {
	f, err := os.Open(path)
	if err != nil {
		return Scenario{}, err
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("decode: %w", err)
	}
	if !strings.HasPrefix(s.ID, IDPrefix) {
		return Scenario{}, fmt.Errorf("id %q: missing %q prefix", s.ID, IDPrefix)
	}
	if s.Origin.RunID == "" {
		return Scenario{}, errors.New("origin.run_id: missing")
	}
	if s.Origin.RecordedAt.IsZero() {
		return Scenario{}, errors.New("origin.recorded_at: missing")
	}
	return s, nil
}

// retiredLedger is the retired.yaml document shape.
type retiredLedger struct {
	Retired []RetiredEntry `yaml:"retired"`
}

// LoadRetired reads dir/retired.yaml. An absent file is an empty ledger; a
// malformed one (undecodable, unknown field, entry without id) is a named
// error.
func LoadRetired(dir string) ([]RetiredEntry, error) {
	path := filepath.Join(dir, RetiredFile)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("retired ledger %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var l retiredLedger
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("retired ledger %s: decode: %w", path, err)
	}
	for i, e := range l.Retired {
		if e.ID == "" {
			return nil, fmt.Errorf("retired ledger %s: entry %d: missing id", path, i)
		}
	}
	return l.Retired, nil
}

// MergeRetired folds incoming into existing, idempotent on id: a duplicate
// id KEEPS the existing entry whole (its reason, run_id, pr, retired_at),
// and new ids are appended in incoming order. The result is a new slice.
func MergeRetired(existing, incoming []RetiredEntry) []RetiredEntry {
	out := make([]RetiredEntry, 0, len(existing)+len(incoming))
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, e := range existing {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	for _, e := range incoming {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out
}

// WriteRetired writes dir/retired.yaml atomically (temp file + rename in
// the same directory), creating dir when needed.
func WriteRetired(dir string, entries []RetiredEntry) error {
	b, err := yaml.Marshal(retiredLedger{Retired: entries})
	if err != nil {
		return fmt.Errorf("retired ledger: marshal: %w", err)
	}
	return writeAtomic(filepath.Join(dir, RetiredFile), b)
}

// Write persists s under dir at PathFor(s.ID), atomically.
func Write(dir string, s Scenario) (string, error) {
	rel, err := PathFor(s.ID)
	if err != nil {
		return "", err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("scenario %s: marshal: %w", s.ID, err)
	}
	if err := writeAtomic(filepath.Join(dir, filepath.FromSlash(rel)), b); err != nil {
		return "", err
	}
	return rel, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%s: mkdir: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("%s: temp: %w", path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s: write: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s: close: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s: rename: %w", path, err)
	}
	return nil
}

// Sample bounds list to cap: the ceil(cap/2) NEWEST by origin.recorded_at
// (ties broken by id) are always kept, and the remainder is drawn from the
// OLDER pool by a math/rand/v2 PCG seeded from the FNV-64 of seed — so the
// same seed (the run id) always serves the same set and a different seed
// rotates the older half. cap <= 0 disables replay (nothing served). The
// returned ReplaySet header carries Cap, CorpusSize, Served, SampledOut and
// Seed computed here, once; Scenarios and RetiredExcluded are the caller's
// (see Entries). The chosen list is returned newest-first.
func Sample(list []Scenario, cap int, seed string) ([]Scenario, ReplaySet) {
	set := ReplaySet{Cap: cap, CorpusSize: len(list), Seed: seed}
	if cap <= 0 || len(list) == 0 {
		set.SampledOut = len(list)
		return nil, set
	}
	sorted := make([]Scenario, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Origin.RecordedAt.Equal(sorted[j].Origin.RecordedAt) {
			return sorted[i].Origin.RecordedAt.After(sorted[j].Origin.RecordedAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	if len(sorted) <= cap {
		set.Served = len(sorted)
		return sorted, set
	}
	keep := (cap + 1) / 2
	chosen := append([]Scenario(nil), sorted[:keep]...)
	older := sorted[keep:]
	draw := cap - keep
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	r := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	perm := r.Perm(len(older))
	picked := append([]int(nil), perm[:draw]...)
	sort.Ints(picked) // preserve newest-first order among the drawn
	for _, idx := range picked {
		chosen = append(chosen, older[idx])
	}
	set.Served = len(chosen)
	set.SampledOut = len(list) - len(chosen)
	return chosen, set
}

// Entries builds the replay attribution for the chosen scenarios from the
// loaded files — the ONLY attribution source. Path is repo-relative
// (CorpusDir/<Path>).
func Entries(chosen []Scenario) []ReplayedScenario {
	out := make([]ReplayedScenario, 0, len(chosen))
	for _, s := range chosen {
		out = append(out, ReplayedScenario{
			ScenarioID:  s.ID,
			OriginPR:    s.Origin.PR,
			OriginIssue: s.Origin.Issue,
			OriginRunID: s.Origin.RunID,
			Path:        CorpusDir + "/" + s.Path,
		})
	}
	return out
}

// Compose emits one Scenario per DRIVABLE criterion whose verdict row PASSED,
// keyed `scenario:issue-<origin.Issue>/<criterion-id>`. A skip_expected
// criterion, a criterion with no row, and a row that did not pass are all
// excluded. An empty steps_taken records StepsNotRecorded.
func Compose(criteria []Criterion, results []CriterionResult, origin Origin) []Scenario {
	byID := make(map[string]CriterionResult, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}
	var out []Scenario
	for _, c := range criteria {
		if !c.Drivable {
			continue
		}
		r, ok := byID[c.ID]
		if !ok || r.Result != "passed" {
			continue
		}
		steps := r.StepsTaken
		if strings.TrimSpace(steps) == "" {
			steps = StepsNotRecorded
		}
		out = append(out, Scenario{
			ID:         fmt.Sprintf("%sissue-%d/%s", IDPrefix, origin.Issue, c.ID),
			Statement:  c.Statement,
			VerifyHint: c.VerifyHint,
			Seed:       c.Seed,
			Steps:      steps,
			Assertions: Assertions{
				Expected:         r.Expected,
				ObservedAtRecord: r.Observed,
				ReproHandle:      r.ReproHandle,
			},
			Origin: origin,
		})
	}
	return out
}

// BudgetExhaustedBasis is the expectation_basis a replay row carries when
// the time cap ran out before the scenario was reached.
const BudgetExhaustedBasis = "replay_budget_exhausted"

// RenderPromptSection renders the `### Regression corpus` markdown the
// runner appends to the acceptance prompt: a replay-first instruction, one
// block per scenario (id, statement, seed, steps, expected, origin PR or
// "origin PR unknown"), the time cap, and the rule that an unreached
// scenario is reported skipped with expectation_basis
// BudgetExhaustedBasis. Empty when no scenario is served.
func RenderPromptSection(chosen []Scenario, timeCap time.Duration) string {
	if len(chosen) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Regression corpus\n\n")
	fmt.Fprintf(&b, "Replay the %d scenario(s) below FIRST, before the criteria above. Each was recorded by a prior passing acceptance pass; report every one as its own verdict row using the scenario id verbatim (the `%s` prefix included) with result `passed` or `failed` and `observed`/`expected`.\n\n",
		len(chosen), IDPrefix)
	fmt.Fprintf(&b, "Replay time cap: %s in total. A scenario you do not reach before the cap is reported `skipped` with `expectation_basis: %s` — never `failed`, never omitted.\n\n",
		timeCap.String(), BudgetExhaustedBasis)
	for _, s := range chosen {
		fmt.Fprintf(&b, "- scenario: %s\n", s.ID)
		fmt.Fprintf(&b, "  statement: %s\n", s.Statement)
		if s.Seed != "" {
			fmt.Fprintf(&b, "  seed: %s\n", s.Seed)
		}
		fmt.Fprintf(&b, "  steps: %s\n", s.Steps)
		fmt.Fprintf(&b, "  expected: %s\n", s.Assertions.Expected)
		if s.Origin.PR > 0 {
			fmt.Fprintf(&b, "  origin PR #%d\n", s.Origin.PR)
		} else {
			b.WriteString("  origin PR unknown\n")
		}
	}
	return b.String()
}
