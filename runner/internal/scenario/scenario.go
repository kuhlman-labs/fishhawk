// Package scenario is the replayable acceptance-scenario corpus (E72.4 /
// #3328): the YAML shape a passing, drivable acceptance criterion is
// persisted as under acceptance/scenarios/, the retirement ledger beside it
// (acceptance/scenarios/retired.yaml), the deterministic sampler that bounds
// how many prior scenarios a new acceptance pass replays, and the prompt
// section that asks the acceptance agent to replay them FIRST.
//
// Every corpus write (Write, WriteRetired) refuses a symlinked path component
// under the corpus dir before creating anything (RefuseSymlinks, #3396), so a
// committed symlink cannot redirect a write outside the tree.
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
	Seed  string `yaml:"seed,omitempty" json:"seed,omitempty"`
	Steps string `yaml:"steps" json:"steps"`
	// StepsCarriedFrom discloses that Steps predate the Origin this file
	// carries: a re-record (Amend) kept the richer PRIOR steps over genuine
	// new ones, so the drive Steps describes is NOT the drive Origin names. It
	// names the origin the steps were carried forward from (head + recorded_at)
	// so a reader of the file ALONE can tell (#3412). Empty when Steps and
	// Origin agree — a first record, or a re-record that took the new steps, or
	// one whose new steps were the StepsNotRecorded fallback (nothing was
	// displaced, so nothing to disclose).
	StepsCarriedFrom string     `yaml:"steps_carried_from,omitempty" json:"steps_carried_from,omitempty"`
	Assertions       Assertions `yaml:"assertions" json:"assertions"`
	Origin           Origin     `yaml:"origin" json:"origin"`

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

// AmendReport tells the caller which STEPS an Amend chose so the log can say
// so. StepsKept is "prior" when the richer prior steps were retained, "new"
// otherwise.
type AmendReport struct {
	StepsKept string
}

// Existing loads the corpus file for id under dir, if present. It is the
// re-record read: PathFor(id) resolves the corpus-relative path, RefuseSymlinks
// refuses a symlinked component (a committed symlink cannot redirect the read),
// an absent file is (zero, false, nil), a decodable file is (s, true, nil) with
// Path set, and a malformed file is (zero, false, named-error).
//
// The CALLER must have proven the tree clean before calling: Existing reads the
// file on disk, which equals HEAD's content only because persist refused a
// dirty tree first. A planted file never reaches this call.
func Existing(dir, id string) (Scenario, bool, error) {
	rel, err := PathFor(id)
	if err != nil {
		return Scenario{}, false, err
	}
	if err := RefuseSymlinks(dir, rel); err != nil {
		return Scenario{}, false, err
	}
	path := filepath.Join(dir, filepath.FromSlash(rel))
	s, err := loadOne(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Scenario{}, false, nil
		}
		return Scenario{}, false, fmt.Errorf("scenario %s: %w", filepath.ToSlash(rel), err)
	}
	s.Path = filepath.ToSlash(rel)
	return s, true, nil
}

// Amend merges a re-recorded scenario (next) onto the prior corpus file (prev),
// so a re-record does not blindly REPLACE the prior recording and lose the
// expensive reproduction recipe (#3412). The volatile, plan-authoritative
// fields always come from next — ID, Statement, VerifyHint, Seed and the WHOLE
// Origin (issue, pr, run_id, head_sha, recorded_at) — so the file attributes
// itself to THIS pass. The durable evidence is kept when next did not improve
// it:
//
//   - Steps: the richer text wins, richness approximated as longer after a
//     whitespace trim (ties go to next). TWO fallback guards make that safe:
//     a prior StepsNotRecorded fallback NEVER wins (else the fallback string
//     could beat genuine-but-shorter new steps — the bug inverted, #3412
//     condition 2), and a new StepsNotRecorded fallback never displaces genuine
//     prior steps.
//   - Assertions (ReproHandle / Expected / ObservedAtRecord): next's value when
//     non-empty after a trim, else prev's.
//
// When the richer PRIOR steps are kept AND next carried GENUINE (non-fallback)
// steps that were displaced, StepsCarriedFrom is set to disclose it — so a
// reader of the file alone can tell the steps predate the Origin it now carries
// (#3412 condition 1). A displaced-nothing keep (next was the fallback) adds no
// NEW disclosure — but a chain preserves the DEEPEST source, so prev's EXISTING
// disclosure is carried forward whether or not next was the fallback: clearing
// it on a fallback keep would strand prev's still-kept steps beside next's fresh
// origin with no disclosure, the exact defect condition 1 targets.
func Amend(prev, next Scenario) (Scenario, AmendReport) {
	out := next
	rep := AmendReport{StepsKept: "new"}

	newIsFallback := next.Steps == StepsNotRecorded
	priorIsFallback := prev.Steps == StepsNotRecorded
	priorRicher := len(strings.TrimSpace(next.Steps)) < len(strings.TrimSpace(prev.Steps))
	if !priorIsFallback && (newIsFallback || priorRicher) {
		out.Steps = prev.Steps
		rep.StepsKept = "prior"
		// A chain preserves the DEEPEST source, so prev's own disclosure is
		// carried forward REGARDLESS of whether next was the fallback —
		// clearing it on a fallback keep would strand prev's still-kept steps
		// beside next's fresh origin with no disclosure (#3412 condition 1).
		out.StepsCarriedFrom = prev.StepsCarriedFrom
		if prev.StepsCarriedFrom == "" {
			// The kept steps are prev's while the Origin written is next's, so
			// they predate it — disclose, whatever next carried. Gating this on
			// next being genuine loses the TWO-pass case (genuine steps at head
			// A, then a fallback re-record): nothing is "displaced", prev has no
			// disclosure to preserve, and the file would assert head B's origin
			// beside head A's steps in silence (#3412 condition 1).
			out.StepsCarriedFrom = carriedFromLabel(prev.Origin)
		}
	}

	out.Assertions.ReproHandle = coalesceTrimmed(next.Assertions.ReproHandle, prev.Assertions.ReproHandle)
	out.Assertions.Expected = coalesceTrimmed(next.Assertions.Expected, prev.Assertions.Expected)
	out.Assertions.ObservedAtRecord = coalesceTrimmed(next.Assertions.ObservedAtRecord, prev.Assertions.ObservedAtRecord)

	return out, rep
}

// coalesceTrimmed returns a when it is non-empty after a whitespace trim, else b.
func coalesceTrimmed(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// carriedFromLabel names the origin some carried-forward steps were recorded
// against, for the StepsCarriedFrom disclosure.
func carriedFromLabel(o Origin) string {
	var parts []string
	if o.HeadSHA != "" {
		parts = append(parts, "head "+o.HeadSHA)
	}
	if !o.RecordedAt.IsZero() {
		parts = append(parts, "recorded_at "+o.RecordedAt.UTC().Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return "an earlier recording"
	}
	return strings.Join(parts, " ")
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
// the same directory), creating dir when needed. It shares writeAtomic with
// Write, so a symlinked retired.yaml (or a symlinked dir component) is
// refused by RefuseSymlinks before anything is created.
func WriteRetired(dir string, entries []RetiredEntry) error {
	b, err := yaml.Marshal(retiredLedger{Retired: entries})
	if err != nil {
		return fmt.Errorf("retired ledger: marshal: %w", err)
	}
	return writeAtomic(dir, RetiredFile, b)
}

// Write persists s under dir at PathFor(s.ID), atomically. A symlinked
// component of that path under dir (the issue-<N> directory or the leaf) is
// refused by RefuseSymlinks BEFORE the directory is created, so a committed
// symlink cannot redirect the write outside dir (#3396).
func Write(dir string, s Scenario) (string, error) {
	rel, err := PathFor(s.ID)
	if err != nil {
		return "", err
	}
	b, err := yaml.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("scenario %s: marshal: %w", s.ID, err)
	}
	if err := writeAtomic(dir, rel, b); err != nil {
		return "", err
	}
	return rel, nil
}

// RefuseSymlinks walks every component of rel (slash form) under root and
// refuses — naming the component — when an EXISTING component is a symlink
// or an existing intermediate is not a directory. The walk stops at the
// first component that does not exist: MkdirAll will create the remainder,
// and nothing that does not exist can redirect it. The LEAF is checked too
// (rename(2) onto a symlink replaces the link entry rather than following
// it, but a symlinked leaf is still not a path the corpus owns). root itself
// is deliberately NOT checked: a symlinked temp root (macOS t.TempDir(),
// a TMPDIR under /var → /private/var) is legitimate and is not
// attacker-committed content. Any other Lstat error propagates. rel is
// also refused when it carries a `..` component or is absolute: the walk
// below joins each component with filepath.Join, which would normalize a
// `..` UP and out of root before the lstat ever ran, so the guard must not
// depend on every caller having pre-validated rel the way PathFor does.
func RefuseSymlinks(root, rel string) error {
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("%s: refusing absolute path %q", root, rel)
	}
	comps := strings.Split(rel, "/")
	// Scanned BEFORE the walk: the walk returns nil at the first absent
	// component, so a `..` behind a not-yet-created prefix would otherwise
	// never be reached.
	for _, comp := range comps {
		if comp == ".." {
			return fmt.Errorf("%s: refusing parent-directory path component in %q", root, rel)
		}
	}
	cur := root
	for _, comp := range comps {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("%s: lstat %q: %w", root, comp, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: refusing to write through symlinked path component %q", root, comp)
		}
		if !fi.IsDir() && cur != filepath.Join(root, filepath.FromSlash(rel)) {
			return fmt.Errorf("%s: refusing to write through non-directory path component %q", root, comp)
		}
	}
	return nil
}

// writeAtomic writes b to root/rel via temp file + rename in the target
// directory. RefuseSymlinks runs FIRST — before MkdirAll, which would
// otherwise already have followed a symlinked component out of root.
func writeAtomic(root, rel string, b []byte) error {
	if err := RefuseSymlinks(root, rel); err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
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
	b.WriteString("Write every `steps_taken` you report — for a replayed scenario above AND for a criterion — as a COMPLETE, standalone reproduction recipe. Never write it by reference to another scenario listed here (e.g. \"same as the replayed scenario\"): a passing criterion is re-recorded as a scenario file, and a back-reference points at a recording that later re-records rewrite, so the referent disappears.\n\n")
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
