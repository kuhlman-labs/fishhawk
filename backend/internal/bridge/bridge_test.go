package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readFile is a test helper that reads a file under dir, failing the test on error.
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// countManagedBlocks returns how many managed blocks (BeginMarker occurrences)
// appear in s.
func countManagedBlocks(s string) int {
	return strings.Count(s, BeginMarker)
}

// TestEnsureAgentDocs_FreshDir covers mode (a): a fresh directory gets both
// files created, AGENTS.md carries the block between the markers, and CLAUDE.md
// imports AGENTS.md.
func TestEnsureAgentDocs_FreshDir(t *testing.T) {
	dir := t.TempDir()

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusCreated {
		t.Errorf("AgentsMD status = %q, want %q", res.AgentsMD, StatusCreated)
	}
	if res.ClaudeMD != StatusCreated {
		t.Errorf("ClaudeMD status = %q, want %q", res.ClaudeMD, StatusCreated)
	}
	if !res.Changed() {
		t.Error("Result.Changed() = false, want true on fresh dir")
	}

	agents := readFile(t, dir, "AGENTS.md")
	if !strings.Contains(agents, BeginMarker) || !strings.Contains(agents, EndMarker) {
		t.Errorf("AGENTS.md missing markers:\n%s", agents)
	}
	begin := strings.Index(agents, BeginMarker)
	end := strings.Index(agents, EndMarker)
	if begin == -1 || end == -1 || end <= begin {
		t.Fatalf("markers not ordered in AGENTS.md: begin=%d end=%d", begin, end)
	}
	between := agents[begin+len(BeginMarker) : end]
	if !strings.Contains(between, "Fishhawk loop") {
		t.Errorf("managed block body not between markers:\n%s", between)
	}

	claude := readFile(t, dir, "CLAUDE.md")
	if !strings.Contains(claude, ImportLine) {
		t.Errorf("CLAUDE.md missing import line %q:\n%s", ImportLine, claude)
	}
}

// TestEnsureAgentDocs_ExistingAgentsUserProse covers mode (b): an AGENTS.md
// with user prose has the prose preserved verbatim and exactly one managed
// block inserted.
func TestEnsureAgentDocs_ExistingAgentsUserProse(t *testing.T) {
	dir := t.TempDir()
	userProse := "# My repo\n\nSome hand-written guidance for agents.\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(userProse), 0o644); err != nil {
		t.Fatalf("seed AGENTS.md: %v", err)
	}

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusUpdated {
		t.Errorf("AgentsMD status = %q, want %q", res.AgentsMD, StatusUpdated)
	}

	agents := readFile(t, dir, "AGENTS.md")
	if !strings.Contains(agents, "Some hand-written guidance for agents.") {
		t.Errorf("user prose not preserved:\n%s", agents)
	}
	if n := countManagedBlocks(agents); n != 1 {
		t.Errorf("managed block count = %d, want 1:\n%s", n, agents)
	}
}

// TestEnsureAgentDocs_StaleBlockReplaced covers mode (c): an AGENTS.md whose
// managed block is stale has only the between-markers region replaced, leaving
// surrounding user content untouched.
func TestEnsureAgentDocs_StaleBlockReplaced(t *testing.T) {
	dir := t.TempDir()
	stale := "# Header prose\n\n" +
		BeginMarker + "\nOLD STALE MANAGED CONTENT\n" + EndMarker + "\n\n" +
		"## Footer prose kept by the user\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(stale), 0o644); err != nil {
		t.Fatalf("seed AGENTS.md: %v", err)
	}

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusUpdated {
		t.Errorf("AgentsMD status = %q, want %q", res.AgentsMD, StatusUpdated)
	}

	agents := readFile(t, dir, "AGENTS.md")
	if strings.Contains(agents, "OLD STALE MANAGED CONTENT") {
		t.Errorf("stale managed content not replaced:\n%s", agents)
	}
	if !strings.Contains(agents, "# Header prose") {
		t.Errorf("header prose not preserved:\n%s", agents)
	}
	if !strings.Contains(agents, "## Footer prose kept by the user") {
		t.Errorf("footer prose not preserved:\n%s", agents)
	}
	if !strings.Contains(agents, "Fishhawk loop") {
		t.Errorf("fresh managed body not inserted:\n%s", agents)
	}
	if n := countManagedBlocks(agents); n != 1 {
		t.Errorf("managed block count = %d, want 1:\n%s", n, agents)
	}
}

// TestEnsureAgentDocs_ClaudeMissingImport covers mode (d): an existing CLAUDE.md
// without the import gains the import line and preserves its content.
func TestEnsureAgentDocs_ClaudeMissingImport(t *testing.T) {
	dir := t.TempDir()
	existing := "# CLAUDE.md\n\nProject-specific notes.\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.ClaudeMD != StatusUpdated {
		t.Errorf("ClaudeMD status = %q, want %q", res.ClaudeMD, StatusUpdated)
	}

	claude := readFile(t, dir, "CLAUDE.md")
	if !strings.Contains(claude, "Project-specific notes.") {
		t.Errorf("existing CLAUDE.md content not preserved:\n%s", claude)
	}
	if !strings.Contains(claude, ImportLine) {
		t.Errorf("import line not added:\n%s", claude)
	}
}

// TestEnsureAgentDocs_ClaudeAlreadyImporting covers mode (e): a CLAUDE.md that
// already imports AGENTS.md is left byte-identical and reported unchanged.
func TestEnsureAgentDocs_ClaudeAlreadyImporting(t *testing.T) {
	dir := t.TempDir()
	existing := "# CLAUDE.md\n\n" + ImportLine + "\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.ClaudeMD != StatusUnchanged {
		t.Errorf("ClaudeMD status = %q, want %q", res.ClaudeMD, StatusUnchanged)
	}

	claude := readFile(t, dir, "CLAUDE.md")
	if claude != existing {
		t.Errorf("CLAUDE.md changed; got:\n%s\nwant:\n%s", claude, existing)
	}
}

// TestEnsureAgentDocs_Idempotent covers mode (f): a second EnsureAgentDocs run
// on the same tree reports all-unchanged and leaves the bytes identical to
// after the first run (the clean-diff guarantee).
func TestEnsureAgentDocs_Idempotent(t *testing.T) {
	dir := t.TempDir()

	if _, err := EnsureAgentDocs(dir); err != nil {
		t.Fatalf("first EnsureAgentDocs: %v", err)
	}
	agents1 := readFile(t, dir, "AGENTS.md")
	claude1 := readFile(t, dir, "CLAUDE.md")

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("second EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusUnchanged {
		t.Errorf("second run AgentsMD status = %q, want %q", res.AgentsMD, StatusUnchanged)
	}
	if res.ClaudeMD != StatusUnchanged {
		t.Errorf("second run ClaudeMD status = %q, want %q", res.ClaudeMD, StatusUnchanged)
	}
	if res.Changed() {
		t.Error("second run Result.Changed() = true, want false")
	}

	if got := readFile(t, dir, "AGENTS.md"); got != agents1 {
		t.Errorf("AGENTS.md bytes changed on re-run:\nfirst:\n%s\nsecond:\n%s", agents1, got)
	}
	if got := readFile(t, dir, "CLAUDE.md"); got != claude1 {
		t.Errorf("CLAUDE.md bytes changed on re-run:\nfirst:\n%s\nsecond:\n%s", claude1, got)
	}
}

// TestEnsureAgentDocs_StatusMatrix covers mode (g): the created / updated /
// unchanged status is correct across a fresh, a stale-block, and an
// already-current file.
func TestEnsureAgentDocs_StatusMatrix(t *testing.T) {
	dir := t.TempDir()

	// AGENTS.md pre-seeded with a stale block -> updated; CLAUDE.md absent -> created.
	stale := BeginMarker + "\nold\n" + EndMarker + "\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(stale), 0o644); err != nil {
		t.Fatalf("seed AGENTS.md: %v", err)
	}

	res, err := EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusUpdated {
		t.Errorf("AgentsMD status = %q, want %q", res.AgentsMD, StatusUpdated)
	}
	if res.ClaudeMD != StatusCreated {
		t.Errorf("ClaudeMD status = %q, want %q", res.ClaudeMD, StatusCreated)
	}

	// Re-run: both now unchanged.
	res, err = EnsureAgentDocs(dir)
	if err != nil {
		t.Fatalf("second EnsureAgentDocs: %v", err)
	}
	if res.AgentsMD != StatusUnchanged || res.ClaudeMD != StatusUnchanged {
		t.Errorf("re-run statuses = (%q, %q), want both unchanged", res.AgentsMD, res.ClaudeMD)
	}
}

// TestMergeManagedBlock exercises the merge helper's branches directly,
// including the empty-input, append, and replace cases plus idempotence.
func TestMergeManagedBlock(t *testing.T) {
	// Empty input -> just the block, changed.
	got, changed := mergeManagedBlock("")
	if !changed || got != ManagedBlock() {
		t.Errorf("empty input: got changed=%v result=%q", changed, got)
	}

	// Whitespace-only input -> just the block, changed.
	got, changed = mergeManagedBlock("   \n\n")
	if !changed || got != ManagedBlock() {
		t.Errorf("whitespace input: got changed=%v result=%q", changed, got)
	}

	// Append case: prose with no markers.
	got, changed = mergeManagedBlock("# Prose\n")
	if !changed {
		t.Error("append case: changed = false, want true")
	}
	if !strings.HasPrefix(got, "# Prose\n\n") {
		t.Errorf("append case: prose not preserved with blank-line separation:\n%s", got)
	}
	if countManagedBlocks(got) != 1 {
		t.Errorf("append case: block count = %d, want 1", countManagedBlocks(got))
	}

	// Idempotence: merging the append result again is a no-op.
	got2, changed2 := mergeManagedBlock(got)
	if changed2 {
		t.Errorf("second merge reported changed; result:\n%s", got2)
	}
	if got2 != got {
		t.Errorf("second merge altered bytes:\nfirst:\n%s\nsecond:\n%s", got, got2)
	}

	// Replace case with a mismatched/absent EndMarker ordering is treated as
	// append (no valid region), not a panic.
	got, changed = mergeManagedBlock(EndMarker + "\nx\n" + BeginMarker + "\n")
	if !changed {
		t.Error("reversed markers: changed = false, want true (append)")
	}
	if countManagedBlocks(got) != 2 {
		// One from the original reversed text, one freshly appended — the
		// helper only recognizes a well-ordered begin<end region.
		t.Errorf("reversed markers: block count = %d, want 2", countManagedBlocks(got))
	}
}

// TestEnsureClaudeImport exercises the import helper's branches directly.
func TestEnsureClaudeImport(t *testing.T) {
	// Empty -> minimal file with import.
	got, changed := ensureClaudeImport("")
	if !changed || !strings.Contains(got, ImportLine) {
		t.Errorf("empty input: changed=%v result=%q", changed, got)
	}

	// Already importing -> unchanged, even with surrounding whitespace on the line.
	existing := "# CLAUDE.md\n\n  " + ImportLine + "  \n"
	got, changed = ensureClaudeImport(existing)
	if changed || got != existing {
		t.Errorf("already importing (trimmed): changed=%v result=%q", changed, got)
	}

	// Missing import -> appended.
	got, changed = ensureClaudeImport("# Notes\n")
	if !changed {
		t.Error("missing import: changed = false, want true")
	}
	if !strings.Contains(got, "# Notes") || !strings.Contains(got, ImportLine) {
		t.Errorf("missing import: result missing content or import:\n%s", got)
	}

	// Idempotence on the appended result.
	got2, changed2 := ensureClaudeImport(got)
	if changed2 || got2 != got {
		t.Errorf("second ensureClaudeImport altered content: changed=%v", changed2)
	}
}
