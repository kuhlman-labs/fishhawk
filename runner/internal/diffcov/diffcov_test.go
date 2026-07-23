package diffcov

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// lines is a readable literal for an added-line set.
func lines(ns ...int) map[int]bool {
	out := map[int]bool{}
	for _, n := range ns {
		out[n] = true
	}
	return out
}

// TestParseUnifiedDiffHunkShapes pins every hunk-header and file-header
// shape the added-line extraction must handle. The omitted-count form is
// the one most likely to silently drop or miscount added lines.
func TestParseUnifiedDiffHunkShapes(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want ChangedFiles
	}{
		{
			name: "single-line hunk with the count omitted",
			diff: "--- a/src/app.go\n+++ b/src/app.go\n@@ -0,0 +7 @@\n+x := 1\n",
			want: ChangedFiles{"src/app.go": lines(7)},
		},
		{
			name: "multi-line hunk",
			diff: "--- a/src/app.go\n+++ b/src/app.go\n@@ -10,0 +11,3 @@\n+a\n+b\n+c\n",
			want: ChangedFiles{"src/app.go": lines(11, 12, 13)},
		},
		{
			name: "pure-deletion hunk adds nothing",
			diff: "--- a/src/app.go\n+++ b/src/app.go\n@@ -5,3 +4,0 @@\n-a\n-b\n-c\n",
			want: ChangedFiles{},
		},
		{
			name: "new file (--- /dev/null) still attributes added lines",
			diff: "--- /dev/null\n+++ b/src/new.go\n@@ -0,0 +1,2 @@\n+a\n+b\n",
			want: ChangedFiles{"src/new.go": lines(1, 2)},
		},
		{
			name: "deleted file (+++ /dev/null) attributes nothing",
			diff: "--- a/src/gone.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n",
			want: ChangedFiles{},
		},
		{
			name: "path containing a space",
			diff: "--- a/src/my file.go\n+++ b/src/my file.go\n@@ -0,0 +3 @@\n+x\n",
			want: ChangedFiles{"src/my file.go": lines(3)},
		},
		{
			name: "C-quoted non-ASCII path is decoded",
			diff: "--- a/x\n+++ b/\"src/caf\\303\\251.go\"\n@@ -0,0 +1 @@\n+x\n",
			want: ChangedFiles{"src/café.go": lines(1)},
		},
		{
			name: "two files in one diff",
			diff: "--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+x\n--- a/b.go\n+++ b/b.go\n@@ -0,0 +2,2 @@\n+y\n+z\n",
			want: ChangedFiles{"a.go": lines(1), "b.go": lines(2, 3)},
		},
		{
			name: "multiple hunks in one file",
			diff: "--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+x\n@@ -9,0 +11,2 @@\n+y\n+z\n",
			want: ChangedFiles{"a.go": lines(1, 11, 12)},
		},
		{
			name: "empty diff",
			diff: "",
			want: ChangedFiles{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUnifiedDiff(tc.diff)
			if err != nil {
				t.Fatalf("ParseUnifiedDiff: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseUnifiedDiffMalformedHeaders(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want string
	}{
		{"non-numeric start", "+++ b/a.go\n@@ -0,0 +xx,2 @@\n", "non-numeric start"},
		{"non-numeric count", "+++ b/a.go\n@@ -0,0 +1,zz @@\n", "non-numeric count"},
		{"dangling path escape", "+++ b/\"a\\\"\n", "dangling escape"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUnifiedDiff(tc.diff)
			if err == nil {
				t.Fatalf("ParseUnifiedDiff succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

// fakeGit records the git argv it was handed and replays canned output,
// so ChangedLines is exercised without a real repository.
type fakeGit struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (f *fakeGit) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	key := args[0]
	if e, ok := f.err[key]; ok {
		return "", e
	}
	return f.out[key], nil
}

func TestChangedLinesResolvesMergeBaseThenDiffsWorkTree(t *testing.T) {
	f := &fakeGit{out: map[string]string{
		"merge-base": "abc123\n",
		"diff":       "--- a/a.go\n+++ b/a.go\n@@ -0,0 +5,2 @@\n+x\n+y\n",
	}}
	got, err := ChangedLines(context.Background(), f.run, "main")
	if err != nil {
		t.Fatalf("ChangedLines: %v", err)
	}
	if !reflect.DeepEqual(got, ChangedFiles{"a.go": lines(5, 6)}) {
		t.Errorf("changed = %v", got)
	}
	if len(f.calls) != 3 {
		t.Fatalf("git calls = %v, want merge-base, diff, then the untracked sweep", f.calls)
	}
	if got, want := strings.Join(f.calls[0], " "), "merge-base main HEAD"; got != want {
		t.Errorf("call 1 = %q, want %q", got, want)
	}
	// The diff must target the resolved merge base and NOT carry "HEAD":
	// the coverage report describes the WORK TREE, so diffing HEAD would
	// put coverage and lines on two different snapshots.
	call2 := strings.Join(f.calls[1], " ")
	if want := "diff -U0 --no-color abc123"; call2 != want {
		t.Errorf("call 2 = %q, want %q", call2, want)
	}
	// `git diff` sees only TRACKED files, so the untracked sweep is what
	// stops a never-`git add`ed new file bypassing the gate outright.
	if want := "ls-files --others --exclude-standard -z"; strings.Join(f.calls[2], " ") != want {
		t.Errorf("call 3 = %q, want %q", strings.Join(f.calls[2], " "), want)
	}
}

// TestChangedLinesFoldsUntrackedFiles pins the untracked-file blind spot:
// a new file that was never `git add`ed is invisible to `git diff`, and
// without this sweep every one of its lines would bypass the gate.
func TestChangedLinesFoldsUntrackedFiles(t *testing.T) {
	f := &fakeGit{out: map[string]string{
		"merge-base": "abc123\n",
		"ls-files":   "src/brand new.go\x00",
	}}
	// Two diff calls: the tracked one, then the --no-index untracked one.
	var diffCall int
	git := func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "diff" {
			diffCall++
			if diffCall == 1 {
				return "--- a/tracked.go\n+++ b/tracked.go\n@@ -0,0 +1 @@\n+x\n", nil
			}
			return "--- /dev/null\n+++ b/src/brand new.go\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n", nil
		}
		return f.run(ctx, args...)
	}
	got, err := ChangedLines(context.Background(), git, "main")
	if err != nil {
		t.Fatalf("ChangedLines: %v", err)
	}
	want := ChangedFiles{
		"tracked.go":       lines(1),
		"src/brand new.go": lines(1, 2, 3),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed = %v, want %v", got, want)
	}
}

// TestChangedLinesUntrackedSweepFailurePropagates pins that a failure of
// the untracked enumeration is an ERROR, not a silent partial denominator
// — a partial answer would under-report the gate.
func TestChangedLinesUntrackedSweepFailurePropagates(t *testing.T) {
	f := &fakeGit{
		out: map[string]string{"merge-base": "abc\n", "diff": ""},
		err: map[string]error{"ls-files": errors.New("ls-files exploded")},
	}
	if _, err := ChangedLines(context.Background(), f.run, "main"); err == nil ||
		!strings.Contains(err.Error(), "ls-files exploded") {
		t.Errorf("err = %v, want the ls-files error", err)
	}
}

// TestChangedLinesEmptyBaseRefNeverShellsOut is condition 5's fail-closed
// backstop: an empty ref must be reported as a named failure, never handed
// to git as an empty argument.
func TestChangedLinesEmptyBaseRefNeverShellsOut(t *testing.T) {
	for _, ref := range []string{"", "   "} {
		f := &fakeGit{}
		_, err := ChangedLines(context.Background(), f.run, ref)
		if !errors.Is(err, ErrEmptyBaseRef) {
			t.Errorf("base %q: error = %v, want ErrEmptyBaseRef", ref, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("base %q: git was invoked %v, want no invocation", ref, f.calls)
		}
	}
}

func TestChangedLinesGitFailures(t *testing.T) {
	t.Run("merge-base fails", func(t *testing.T) {
		f := &fakeGit{err: map[string]error{"merge-base": errors.New("bad revision")}}
		if _, err := ChangedLines(context.Background(), f.run, "nope"); err == nil ||
			!strings.Contains(err.Error(), "bad revision") {
			t.Errorf("err = %v, want the git error", err)
		}
	})
	t.Run("no merge base", func(t *testing.T) {
		f := &fakeGit{out: map[string]string{"merge-base": "\n"}}
		_, err := ChangedLines(context.Background(), f.run, "orphan")
		if err == nil || !strings.Contains(err.Error(), "no merge base") {
			t.Errorf("err = %v, want a no-merge-base error", err)
		}
	})
	t.Run("diff fails", func(t *testing.T) {
		f := &fakeGit{
			out: map[string]string{"merge-base": "abc\n"},
			err: map[string]error{"diff": errors.New("diff exploded")},
		}
		if _, err := ChangedLines(context.Background(), f.run, "main"); err == nil ||
			!strings.Contains(err.Error(), "diff exploded") {
			t.Errorf("err = %v, want the git error", err)
		}
	})
}

// TestNormalizePath is condition 2's core: the spellings coverage
// producers actually emit must all resolve to the same repo-relative key
// the diff parser yields, and a path that cannot be placed must ERROR
// rather than being guessed at.
func TestNormalizePath(t *testing.T) {
	const repo = "/build/repo"
	ok := []struct {
		name string
		in   string
		want string
	}{
		{"already relative", "src/app.go", "src/app.go"},
		{"dot-slash prefixed", "./src/app.go", "src/app.go"},
		{"redundant separator", "src//app.go", "src/app.go"},
		{"redundant dot segment", "src/./app.go", "src/app.go"},
		{"absolute under the repo", "/build/repo/src/app.go", "src/app.go"},
		{"absolute with redundant separators", "/build/repo//src/./app.go", "src/app.go"},
		{"interior dot-dot that stays inside", "src/sub/../app.go", "src/app.go"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePath(repo, tc.in)
			if err != nil {
				t.Fatalf("NormalizePath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("NormalizePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	bad := []struct {
		name    string
		repoDir string
		in      string
	}{
		{"empty", repo, ""},
		{"escapes upward", repo, "../outside.go"},
		{"resolves to the repo root itself", repo, "."},
		{"absolute outside the repo", repo, "/elsewhere/app.go"},
		{"absolute with no repo root", "", "/build/repo/src/app.go"},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if got, err := NormalizePath(tc.repoDir, tc.in); err == nil {
				t.Errorf("NormalizePath(%q, %q) = %q, want an error", tc.repoDir, tc.in, got)
			}
		})
	}
}

func TestMeasureThreeOfFourCovered(t *testing.T) {
	changed := ChangedFiles{
		"src/app.go":  lines(10, 11, 14),
		"src/util.go": lines(2),
	}
	cov := Coverage{
		"src/app.go":  FileCoverage{10: 1, 11: 0, 14: 3},
		"src/util.go": FileCoverage{2: 7},
	}
	res := Measure("/repo", changed, cov)
	if res.NewLines != 4 || res.CoveredNewLines != 3 {
		t.Fatalf("covered/total = %d/%d, want 3/4", res.CoveredNewLines, res.NewLines)
	}
	if res.Percent != 75 {
		t.Errorf("percent = %v, want 75", res.Percent)
	}
	if !reflect.DeepEqual(res.UncoveredFiles, []string{"src/app.go"}) {
		t.Errorf("uncovered = %v, want [src/app.go]", res.UncoveredFiles)
	}
}

// TestMeasureNormalizesBothSides is the false-RED regression: an
// exact map-key intersection over these producer spellings would report
// 0% and fail every opted-in run.
func TestMeasureNormalizesBothSides(t *testing.T) {
	const repo = "/build/repo"
	changed := ChangedFiles{"src/app.go": lines(1, 2)}
	for _, spelling := range []string{
		"/build/repo/src/app.go", // absolute
		"./src/app.go",           // dot-slash prefixed
		"src//app.go",            // redundant separator
		"src/./app.go",           // redundant dot segment
	} {
		t.Run(spelling, func(t *testing.T) {
			res := Measure(repo, changed, Coverage{spelling: FileCoverage{1: 1, 2: 1}})
			if res.NewLines != 2 || res.CoveredNewLines != 2 {
				t.Fatalf("covered/total = %d/%d, want 2/2 — the report path did not intersect",
					res.CoveredNewLines, res.NewLines)
			}
			if res.Percent != 100 {
				t.Errorf("percent = %v, want 100", res.Percent)
			}
			if len(res.UnnormalizablePaths) != 0 {
				t.Errorf("unnormalizable = %v, want none", res.UnnormalizablePaths)
			}
		})
	}
}

// TestMeasureUnnormalizablePathIsReportedNotCountedUncovered pins the
// condition-2 tail: a report path outside the repo is named explicitly
// rather than silently dragging the percentage down.
func TestMeasureUnnormalizablePathIsReportedNotCountedUncovered(t *testing.T) {
	changed := ChangedFiles{"src/app.go": lines(1)}
	cov := Coverage{
		"src/app.go":             FileCoverage{1: 1},
		"/elsewhere/vendored.go": FileCoverage{1: 0},
		"../outside/escaped.go":  FileCoverage{1: 0},
	}
	res := Measure("/build/repo", changed, cov)
	if res.NewLines != 1 || res.CoveredNewLines != 1 || res.Percent != 100 {
		t.Errorf("covered/total = %d/%d (%.0f%%), want 1/1 100%% — an unplaceable report path must not be counted as uncovered",
			res.CoveredNewLines, res.NewLines, res.Percent)
	}
	want := []string{"../outside/escaped.go", "/elsewhere/vendored.go"}
	got := append([]string(nil), res.UnnormalizablePaths...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unnormalizable = %v, want %v", got, want)
	}
}

// TestMeasureDenominatorExclusions pins the two ways an added line stays
// out of the denominator: a file the report never measured, and a line
// the report measured no statement on.
func TestMeasureDenominatorExclusions(t *testing.T) {
	t.Run("file the report never mentions", func(t *testing.T) {
		res := Measure("/repo",
			ChangedFiles{"README.md": lines(1, 2, 3), "src/app.go": lines(1)},
			Coverage{"src/app.go": FileCoverage{1: 1}})
		if res.NewLines != 1 || res.CoveredNewLines != 1 {
			t.Errorf("covered/total = %d/%d, want 1/1 (README.md excluded)",
				res.CoveredNewLines, res.NewLines)
		}
	})
	t.Run("line with no DA record", func(t *testing.T) {
		// Lines 2 and 3 are a blank line and a comment: measured file,
		// but not coverable statements.
		res := Measure("/repo",
			ChangedFiles{"src/app.go": lines(1, 2, 3)},
			Coverage{"src/app.go": FileCoverage{1: 1}})
		if res.NewLines != 1 || res.CoveredNewLines != 1 {
			t.Errorf("covered/total = %d/%d, want 1/1", res.CoveredNewLines, res.NewLines)
		}
	})
}

func TestMeasureZeroNewLines(t *testing.T) {
	res := Measure("/repo", ChangedFiles{}, Coverage{"a.go": FileCoverage{1: 1}})
	if res.NewLines != 0 || res.CoveredNewLines != 0 {
		t.Fatalf("covered/total = %d/%d, want 0/0", res.CoveredNewLines, res.NewLines)
	}
	if res.Percent != 0 {
		t.Errorf("percent = %v, want 0 (no division by zero)", res.Percent)
	}
	if len(res.UncoveredFiles) != 0 {
		t.Errorf("uncovered = %v, want none", res.UncoveredFiles)
	}
}

func TestMeasureFoldsCollidingSpellings(t *testing.T) {
	// Two SF records spelling one file differently must fold into one
	// entry with summed hits, not shadow each other.
	res := Measure("/repo",
		ChangedFiles{"a.go": lines(1)},
		Coverage{"./a.go": FileCoverage{1: 0}, "a.go": FileCoverage{1: 2}})
	if res.NewLines != 1 || res.CoveredNewLines != 1 {
		t.Errorf("covered/total = %d/%d, want 1/1 (hits summed across spellings)",
			res.CoveredNewLines, res.NewLines)
	}
}

func TestMeasureUncoveredFilesAreSorted(t *testing.T) {
	changed := ChangedFiles{}
	cov := Coverage{}
	for _, p := range []string{"z.go", "a.go", "m.go"} {
		changed[p] = lines(1)
		cov[p] = FileCoverage{1: 0}
	}
	res := Measure("/repo", changed, cov)
	if !sort.StringsAreSorted(res.UncoveredFiles) {
		t.Errorf("uncovered = %v, want sorted for determinism", res.UncoveredFiles)
	}
	if len(res.UncoveredFiles) != 3 {
		t.Errorf("uncovered = %v, want 3 entries", res.UncoveredFiles)
	}
}

// TestChangedLinesExcludesPostForkBaseCommits documents the merge-base
// framing: lines a post-fork base-branch commit added are not in this
// stage's denominator, because the diff is taken from the merge base.
func TestChangedLinesExcludesPostForkBaseCommits(t *testing.T) {
	// The fake resolves `merge-base main HEAD` to the FORK POINT, and the
	// canned diff is what git would emit from there — it contains only the
	// stage's own lines. Asserting the merge-base argv is what proves the
	// post-fork commits were excluded by construction rather than by the
	// fixture's choice of text.
	f := &fakeGit{out: map[string]string{
		"merge-base": "forkpoint\n",
		"diff":       "--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+stage line\n",
	}}
	got, err := ChangedLines(context.Background(), f.run, "main")
	if err != nil {
		t.Fatalf("ChangedLines: %v", err)
	}
	if !reflect.DeepEqual(got, ChangedFiles{"a.go": lines(1)}) {
		t.Errorf("changed = %v", got)
	}
	if diffArgs := fmt.Sprint(f.calls[1]); !strings.Contains(diffArgs, "forkpoint") {
		t.Errorf("diff argv %s does not target the merge base", diffArgs)
	}
}
