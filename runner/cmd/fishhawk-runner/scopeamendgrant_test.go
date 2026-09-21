package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- #3390 approved-grant delivery into the verify-fix loop + unused-grant fail-loud ---

func grantScope() []upload.ScopeFile {
	return []upload.ScopeFile{
		{Path: "mod/reg.go", Operation: "modify"},
		{Path: "mod/reg_test.go", Operation: "create"},
		{Path: "", Operation: "modify"}, // empty path: skipped
	}
}

func grantRow(id, stageID, decisionReason string) upload.ScopeAmendment {
	return upload.ScopeAmendment{
		ID:             id,
		RunID:          verifyFixRunID,
		StageID:        stageID,
		Status:         "approved",
		Paths:          []upload.ScopeAmendmentPath{{Path: "mod/other.go", Operation: "modify"}},
		Reason:         "AGENT-AUTHORED-REASON-MUST-NOT-RENDER",
		DecisionReason: decisionReason,
	}
}

// TestVerifyFixPrompt_RendersEffectiveScope: every non-empty scope path + op
// appears under the effective-scope header in scope order; an empty scope
// (the `git add -A` fallback) omits the section and keeps the legacy
// "approved scope" wording; the Command/Output blocks are unchanged.
func TestVerifyFixPrompt_RendersEffectiveScope(t *testing.T) {
	prompt, elided := verifyFixPrompt("go test ./...", "--- FAIL: TestGet", grantScope(), nil, nil)
	if elided != 0 {
		t.Fatalf("elided = %d, want 0", elided)
	}
	if !strings.Contains(prompt, "Command:\ngo test ./...\n\nOutput:\n--- FAIL: TestGet\n") {
		t.Errorf("Command/Output blocks changed:\n%s", prompt)
	}
	if !strings.Contains(prompt, verifyFixEffectiveScopeHeader) {
		t.Errorf("missing the effective-scope header:\n%s", prompt)
	}
	i := strings.Index(prompt, "- mod/reg.go (modify)\n")
	j := strings.Index(prompt, "- mod/reg_test.go (create)\n")
	if i < 0 || j < 0 || j < i {
		t.Errorf("scope lines missing or out of order (%d, %d):\n%s", i, j, prompt)
	}
	if strings.Contains(prompt, "- \n") || strings.Contains(prompt, "-  (modify)") {
		t.Errorf("empty scope path must be skipped:\n%s", prompt)
	}
	if !strings.Contains(prompt, `the files listed under
"Effective scope" above`) {
		t.Errorf("closing paragraph must point at the effective-scope list:\n%s", prompt)
	}
	if strings.Contains(prompt, verifyFixGrantedMarker) {
		t.Errorf("nil granted must render no GRANTED block:\n%s", prompt)
	}
	if strings.Contains(prompt, verifyFixOutOfScopeMarker) {
		t.Errorf("nil outOfScope must render no OUT-OF-SCOPE block:\n%s", prompt)
	}

	// Empty scope: section omitted, legacy wording kept.
	empty, _ := verifyFixPrompt("go test ./...", "out", nil, nil, nil)
	if strings.Contains(empty, verifyFixEffectiveScopeHeader) {
		t.Errorf("empty scope must omit the effective-scope section:\n%s", empty)
	}
	if !strings.Contains(empty, "already allowed to change (the approved scope)") {
		t.Errorf("empty scope must keep the legacy wording:\n%s", empty)
	}
}

// TestVerifyFixPrompt_RendersGrantedMidStage: the GRANTED MID-STAGE block
// names the amendment id, `path (op)`, the operator decision reason and the
// in-force instruction; an empty DecisionReason omits the reason line; the
// agent-authored `reason` NEVER renders; nil granted omits the block.
func TestVerifyFixPrompt_RendersGrantedMidStage(t *testing.T) {
	granted := []upload.ScopeAmendment{
		grantRow("amd-1", verifyFixStageID, "edit only the init func"),
		{ID: "amd-2", Status: "approved", StageID: verifyFixStageID, Reason: "AGENT-AUTHORED-REASON-MUST-NOT-RENDER",
			Paths: []upload.ScopeAmendmentPath{{Path: "docs/x.md", Operation: "create"}, {Path: "", Operation: "modify"}, {Path: "pkg/noop.go"}}},
		{ID: "amd-empty", Status: "approved", StageID: verifyFixStageID},
	}
	prompt, _ := verifyFixPrompt("go test ./...", "out", grantScope(), granted, nil)
	for _, want := range []string{
		verifyFixGrantedMarker,
		"- amendment amd-1 granted mod/other.go (modify)\n  Operator decision reason: edit only the init func\n",
		"- amendment amd-2 granted docs/x.md (create), pkg/noop.go\n",
		"- amendment amd-empty\n",
		"are in force for this fix",
		"do not treat the grant as\npending or denied",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "AGENT-AUTHORED-REASON-MUST-NOT-RENDER") {
		t.Errorf("agent-authored amendment reason must never render:\n%s", prompt)
	}
	// amd-2 has no DecisionReason: exactly ONE reason line in the whole prompt.
	if n := strings.Count(prompt, "Operator decision reason:"); n != 1 {
		t.Errorf("Operator decision reason lines = %d, want 1 (empty DecisionReason omits the line):\n%s", n, prompt)
	}
	// Order: Output → Effective scope → GRANTED → closing paragraph.
	o, s, g, c := strings.Index(prompt, "Output:"), strings.Index(prompt, verifyFixEffectiveScopeHeader),
		strings.Index(prompt, verifyFixGrantedMarker), strings.Index(prompt, "Edit the code so this command passes")
	if o >= s || s >= g || g >= c {
		t.Errorf("section order = output %d, scope %d, granted %d, closing %d:\n%s", o, s, g, c, prompt)
	}

	none, _ := verifyFixPrompt("go test ./...", "out", grantScope(), nil, nil)
	if strings.Contains(none, verifyFixGrantedMarker) || strings.Contains(none, "- amendment") {
		t.Errorf("nil granted must omit the GRANTED block entirely:\n%s", none)
	}
	if strings.Contains(prompt, verifyFixOutOfScopeMarker) || strings.Contains(none, verifyFixOutOfScopeMarker) {
		t.Errorf("nil outOfScope must render no OUT-OF-SCOPE block")
	}
}

// --- #3410 OUT-OF-SCOPE PATHS detector + always-on amendment recipe ---

// TestOutOfScopePathsInVerifyOutput is the pure-detector table with an
// injected exists func: the verbatim #3400 verify excerpt yields the registry
// path; in-scope paths, non-existent paths, module-relative paths exists()
// rejects, bare basenames, absolute and parent-relative tokens (rejected as
// WHOLE tokens even when the suffix exists — binding condition 1) are dropped;
// duplicates collapse; 12 paths cap to 10 sorted.
func TestOutOfScopePathsInVerifyOutput(t *testing.T) {
	scope := []upload.ScopeFile{{Path: "backend/internal/server/foo.go", Operation: "modify"}, {Path: `backend\internal\server\bar.go`}}
	existsSet := func(paths ...string) func(string) bool {
		set := map[string]bool{}
		for _, p := range paths {
			set[p] = true
		}
		return func(rel string) bool { return set[rel] }
	}
	const registry = "backend/internal/audit/categories.go"
	all := func(string) bool { return true }

	cases := []struct {
		name   string
		output string
		exists func(string) bool
		want   []string
	}{
		{"verbatim #3400 excerpt", "    categories_completeness_test.go:98: emitted audit categories not registered in KnownCategories (add them to backend/internal/audit/categories.go):\n        zz_cat", existsSet(registry), []string{registry}},
		{"in-scope path dropped", "fix backend/internal/server/foo.go and backend/internal/server/bar.go", all, nil},
		{"non-existent dropped", "see backend/internal/audit/categories.go", existsSet("other/file.go"), nil},
		{"module-relative exists() rejects", "internal/server/trace.go:12: undefined: x", existsSet(registry), nil},
		{"basename without slash dropped", "categories_completeness_test.go:98: boom", all, nil},
		{"parent-relative rejected whole", "open ../foo/bar.go: no such file", existsSet("foo/bar.go"), nil},
		{"absolute rejected whole", "open /abs/path.go: permission denied", existsSet("abs/path.go"), nil},
		{"dot-dot segment mid-token dropped", "see a/../b/c.go", existsSet("b/c.go"), nil},
		{"duplicates collapse", registry + " and again " + registry + ", then (" + registry + ")", existsSet(registry), []string{registry}},
		{"trailing punctuation trimmed", "add them to " + registry + "):", existsSet(registry), []string{registry}},
		{"empty output", "", all, nil},
		{"nil exists", "x " + registry, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := outOfScopePathsInVerifyOutput(tc.output, scope, tc.exists)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// 12 distinct paths cap to 10, sorted.
	var sb strings.Builder
	for i := 11; i >= 0; i-- {
		fmt.Fprintf(&sb, "err in pkg/f%02d.go; ", i)
	}
	got := outOfScopePathsInVerifyOutput(sb.String(), scope, all)
	if len(got) != verifyFixMaxOutOfScopePaths {
		t.Fatalf("len = %d, want %d: %v", len(got), verifyFixMaxOutOfScopePaths, got)
	}
	for i, p := range got {
		if want := fmt.Sprintf("pkg/f%02d.go", i); p != want {
			t.Errorf("got[%d] = %q, want %q", i, p, want)
		}
	}
}

// TestFileExistsUnder pins the production predicate: a regular file is true;
// a directory and a missing path are false.
func TestFileExistsUnder(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "a", "b.go"), "package a\n")
	exists := fileExistsUnder(dir)
	if !exists("a/b.go") {
		t.Error("regular file must exist")
	}
	if exists("a") {
		t.Error("a directory must not count")
	}
	if exists("a/missing.go") {
		t.Error("a missing file must not count")
	}
}

// TestVerifyFixPrompt_RendersOutOfScopeAndRecipe: non-empty outOfScope renders
// the marker, each path and the instruction; the recipe with both endpoint
// strings, the bearer env and the ?wait=30 poll is rendered whenever the scope
// is non-empty (even with no out-of-scope path); an empty scope renders
// neither.
func TestVerifyFixPrompt_RendersOutOfScopeAndRecipe(t *testing.T) {
	recipe := []string{
		verifyFixAmendmentRecipeHeader,
		"POST $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments",
		"GET $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments?wait=30",
		"Authorization: Bearer $FISHHAWK_API_TOKEN",
		"file a\nscope amendment as described above rather than retrying",
	}

	withPaths, _ := verifyFixPrompt("go test ./...", "out", grantScope(), nil, []string{"backend/internal/audit/categories.go", "docs/x.md"})
	for _, want := range append([]string{
		verifyFixOutOfScopeMarker,
		"- backend/internal/audit/categories.go\n- docs/x.md\n",
		"do NOT retry an in-scope\nworkaround and do NOT edit it",
	}, recipe...) {
		if !strings.Contains(withPaths, want) {
			t.Errorf("prompt missing %q:\n%s", want, withPaths)
		}
	}
	// Order: Effective scope → OUT-OF-SCOPE → recipe → closing paragraph.
	s, o, r, c := strings.Index(withPaths, verifyFixEffectiveScopeHeader), strings.Index(withPaths, verifyFixOutOfScopeMarker),
		strings.Index(withPaths, verifyFixAmendmentRecipeHeader), strings.Index(withPaths, "Edit the code so this command passes")
	if s >= o || o >= r || r >= c {
		t.Errorf("section order = scope %d, out-of-scope %d, recipe %d, closing %d:\n%s", s, o, r, c, withPaths)
	}

	noPaths, _ := verifyFixPrompt("go test ./...", "out", grantScope(), nil, nil)
	if strings.Contains(noPaths, verifyFixOutOfScopeMarker) {
		t.Errorf("empty outOfScope must render no marker:\n%s", noPaths)
	}
	for _, want := range recipe {
		if !strings.Contains(noPaths, want) {
			t.Errorf("recipe must render with a non-empty scope even with no out-of-scope path; missing %q:\n%s", want, noPaths)
		}
	}

	noScope, _ := verifyFixPrompt("go test ./...", "out", nil, nil, []string{"backend/internal/audit/categories.go"})
	for _, absent := range []string{verifyFixAmendmentRecipeHeader, "scope-amendments", "as described above"} {
		if strings.Contains(noScope, absent) {
			t.Errorf("empty scope must render no recipe; found %q:\n%s", absent, noScope)
		}
	}
	if !strings.Contains(noScope, "already allowed to change (the approved scope)") {
		t.Errorf("empty scope must keep the legacy wording:\n%s", noScope)
	}
}

// TestRun_VerifyFixLoop_OutOfScopePathNamed_FixPromptCarriesIt is the
// cross-boundary e2e over the TestRun_NoAmendment_FixPromptScopeOnly harness:
// the base repo additionally commits mod/registry.go (out of scope) and the
// verify command names it; the captured iteration-1 fix prompt carries the
// OUT-OF-SCOPE marker with the path and the runner log carries
// verify_fix_out_of_scope_named. The control names only the in-scope
// mod/reg.go: no marker, no log line — but the amendment recipe is STILL
// rendered (binding condition 3: always-on rendering pinned end to end).
func TestRun_VerifyFixLoop_OutOfScopePathNamed_FixPromptCarriesIt(t *testing.T) {
	const recipePOST = "POST $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments"
	drive := func(t *testing.T, verifyCmd string) (fixPrompt, stderr string) {
		t.Helper()
		pinAmendmentWatchInterval(t)
		repo := verifyFixBaseRepo(t)
		mustWrite(t, filepath.Join(repo, "mod", "registry.go"), "package mod\n\n// out-of-scope registry\n")
		runGit := func(args ...string) {
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		runGit("add", "-A")
		runGit("commit", "-q", "-m", "base: out-of-scope registry")
		mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
		mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

		invoker := &fakeInvoker{
			mirrorWorkingTreeFrom: repo,
			canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			onInvoke: func(idx int, inv agent.Invocation) {
				if idx == 1 {
					fixPrompt = inv.Prompt
				}
			},
		}
		withFakeInvoker(t, invoker)
		implementEnv(t, "kuhlman-labs/fishhawk", "main")
		fu := newFakeUploader(t)
		fp := undecidedVerifyFixPrompt()
		fp.VerifyCommand = verifyCmd
		fu.promptResp = fp
		withFakeUploader(t, fu)
		withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

		bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
		var sb strings.Builder
		if got := run(verifyFixRunArgs(repo, bundlePath), &sb); got != exitFailure {
			t.Errorf("run = %d, want exitFailure:\n%s", got, sb.String())
		}
		if fixPrompt == "" {
			t.Fatal("iteration-1 fix prompt was never captured")
		}
		return fixPrompt, sb.String()
	}

	t.Run("out-of-scope path named", func(t *testing.T) {
		fixPrompt, stderr := drive(t, `sh -c 'echo "registry miss: add them to mod/registry.go"; exit 1'`)
		for _, want := range []string{verifyFixOutOfScopeMarker, "- mod/registry.go\n", recipePOST} {
			if !strings.Contains(fixPrompt, want) {
				t.Errorf("fix prompt missing %q:\n%s", want, fixPrompt)
			}
		}
		if !strings.Contains(stderr, `"event":"verify_fix_out_of_scope_named"`) || !strings.Contains(stderr, `"paths":"mod/registry.go"`) {
			t.Errorf("runner log missing verify_fix_out_of_scope_named for mod/registry.go:\n%s", stderr)
		}
	})
	t.Run("control: in-scope path only", func(t *testing.T) {
		fixPrompt, stderr := drive(t, `sh -c 'echo "registry miss: add them to mod/reg.go"; exit 1'`)
		if strings.Contains(fixPrompt, verifyFixOutOfScopeMarker) {
			t.Errorf("in-scope-only output must render no OUT-OF-SCOPE marker:\n%s", fixPrompt)
		}
		if strings.Contains(stderr, "verify_fix_out_of_scope_named") {
			t.Errorf("in-scope-only output must emit no verify_fix_out_of_scope_named line:\n%s", stderr)
		}
		if !strings.Contains(fixPrompt, recipePOST) {
			t.Errorf("amendment recipe must render regardless (always-on):\n%s", fixPrompt)
		}
	})
}

// TestUnusedScopeAmendmentGrants is the pure-predicate table: which granted
// paths count as unused against a dirty set, which rows are ignored, and which
// paths are skipped by the MissingScopeFiles-mirroring rules.
func TestUnusedScopeAmendmentGrants(t *testing.T) {
	const stage = verifyFixStageID
	row := func(id string, paths ...upload.ScopeAmendmentPath) upload.ScopeAmendment {
		return upload.ScopeAmendment{ID: id, Status: "approved", StageID: stage, Paths: paths}
	}
	mp := func(p, op string) upload.ScopeAmendmentPath { return upload.ScopeAmendmentPath{Path: p, Operation: op} }

	nm := grantNotModified
	cases := []struct {
		name        string
		approved    []upload.ScopeAmendment
		stageID     string
		dirty       []string
		staged      []string
		stagedKnown bool
		want        []unusedScopeAmendmentGrant
	}{
		{"untouched modify -> unused (not_modified)", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage, []string{"mod/reg.go"}, nil, false,
			[]unusedScopeAmendmentGrant{{"a", "mod/other.go", "modify", nm}}},
		{"modified path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage, []string{"mod/other.go"}, nil, false, nil},
		{"created path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/new.go", "create"))}, stage, []string{"mod/new.go"}, nil, false, nil},
		{"deleted path in dirty set -> used", []upload.ScopeAmendment{row("a", mp("mod/old.go", "delete"))}, stage, []string{"mod/old.go"}, nil, false, nil},
		{"other stage's row ignored", []upload.ScopeAmendment{{ID: "a", Status: "approved", StageID: undecidedOtherStageID, Paths: []upload.ScopeAmendmentPath{mp("mod/other.go", "modify")}}}, stage, nil, nil, false, nil},
		{"non-approved row ignored", []upload.ScopeAmendment{{ID: "a", Status: "pending", StageID: stage, Paths: []upload.ScopeAmendmentPath{mp("mod/other.go", "modify")}}}, stage, nil, nil, false, nil},
		{"empty stageID -> nil", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, "", nil, nil, false, nil},
		{"skip rules: trailing-slash, empty, absolute, dotdot", []upload.ScopeAmendment{row("a",
			mp("corpus/new/", "create"), mp("", "modify"), mp("/etc/passwd", "modify"), mp("../up.go", "modify"), mp("keep.go", "modify"))},
			stage, nil, nil, false, []unusedScopeAmendmentGrant{{"a", "keep.go", "modify", nm}}},
		{"order preserved across rows and paths", []upload.ScopeAmendment{row("a", mp("z.go", "modify"), mp("y.go", "create")), row("b", mp("x.go", ""))},
			stage, nil, nil, false, []unusedScopeAmendmentGrant{{"a", "z.go", "modify", nm}, {"a", "y.go", "create", nm}, {"b", "x.go", "", nm}}},
		{"no approved rows -> nil", nil, stage, []string{"a"}, nil, false, nil},
		// #3434 dispositions.
		{"dirty+staged, stagedKnown -> used", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage,
			[]string{"mod/other.go"}, []string{"mod/other.go"}, true, nil},
		{"dirty+unstaged, stagedKnown -> not_staged", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage,
			[]string{"mod/other.go"}, []string{"mod/reg.go"}, true, []unusedScopeAmendmentGrant{{"a", "mod/other.go", "modify", grantNotStaged}}},
		{"dirty+unstaged, !stagedKnown -> used (refinement withheld)", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage,
			[]string{"mod/other.go"}, nil, false, nil},
		{"clean, stagedKnown -> not_modified", []upload.ScopeAmendment{row("a", mp("mod/other.go", "modify"))}, stage,
			nil, nil, true, []unusedScopeAmendmentGrant{{"a", "mod/other.go", "modify", nm}}},
		{"mixed: one not_modified, one not_staged, one used", []upload.ScopeAmendment{row("a", mp("a.go", "modify"), mp("b.go", "create"), mp("c.go", "modify"))}, stage,
			[]string{"b.go", "c.go"}, []string{"c.go"}, true,
			[]unusedScopeAmendmentGrant{{"a", "a.go", "modify", nm}, {"a", "b.go", "create", grantNotStaged}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unusedScopeAmendmentGrants(tc.approved, tc.stageID, tc.dirty, tc.staged, tc.stagedKnown)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestAnnotateUnusedAmendmentFailure_ExactLine pins the byte-exact attribution
// line, the preserved reason prefix, the empty-operation form, and the
// empty-list identity.
func TestAnnotateUnusedAmendmentFailure_ExactLine(t *testing.T) {
	const reason = "verify command \"go test\" still failing after 2 iteration(s):\n--- FAIL: TestGet"
	got := annotateUnusedAmendmentFailure(reason, []unusedScopeAmendmentGrant{
		{"amd-e2e", "mod/other.go", "modify", grantNotModified},
		{"amd-2", "pkg/noop.go", "", grantNotModified},
	})
	if !strings.HasPrefix(got, reason+"\n\n") {
		t.Errorf("reason prefix not preserved:\n%s", got)
	}
	for _, want := range []string{
		"amendment amd-e2e granted mod/other.go (modify); file not modified\n",
		"amendment amd-2 granted pkg/noop.go; file not modified\n",
		"fishhawk_retry_stage",
		"never edited",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation missing %q:\n%s", want, got)
		}
	}
	if same := annotateUnusedAmendmentFailure(reason, nil); same != reason {
		t.Errorf("empty list must return reason unchanged, got:\n%s", same)
	}
	// A not_modified-only list renders NO not_staged sentence: the closing
	// explanation is disposition-specific.
	if strings.Contains(got, "not staged into the verified commit") || strings.Contains(got, "WAS edited") {
		t.Errorf("not_modified-only annotation must carry no not_staged text:\n%s", got)
	}
}

// TestAnnotateUnusedAmendmentFailure_NotStagedLine pins the exact #3434
// not_staged line, its disposition-specific closing sentence, and that a
// mixed list renders BOTH closing sentences (not_modified first).
func TestAnnotateUnusedAmendmentFailure_NotStagedLine(t *testing.T) {
	const reason = "verify command \"go test\" still failing after 2 iteration(s)"
	got := annotateUnusedAmendmentFailure(reason, []unusedScopeAmendmentGrant{
		{"amd-e2e", "mod/other.go", "modify", grantNotStaged},
		{"amd-2", "pkg/noop.go", "", grantNotStaged},
	})
	if !strings.HasPrefix(got, reason+"\n\n") {
		t.Errorf("reason prefix not preserved:\n%s", got)
	}
	for _, want := range []string{
		"amendment amd-e2e granted mod/other.go (modify); file modified but not staged into the verified commit\n",
		"amendment amd-2 granted pkg/noop.go; file modified but not staged into the verified commit\n",
		"the granted file WAS edited",
		"re-stage unstaged it",
		"fishhawk_retry_stage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "file not modified") || strings.Contains(got, "never edited") {
		t.Errorf("not_staged-only annotation must carry no not_modified text:\n%s", got)
	}
	mixed := annotateUnusedAmendmentFailure(reason, []unusedScopeAmendmentGrant{
		{"amd-1", "a.go", "modify", grantNotModified},
		{"amd-2", "b.go", "create", grantNotStaged},
	})
	i, j := strings.Index(mixed, "never edited"), strings.Index(mixed, "WAS edited")
	if i < 0 || j < 0 || j < i {
		t.Errorf("mixed list must render both closing sentences, not_modified first (%d, %d):\n%s", i, j, mixed)
	}
}

// TestUnusedGrantStagedKnown pins the run()-level predicate (#3434, approval
// condition 2): the not_staged refinement is live exactly on the
// committed-gate path (a verify command configured AND not the deterministic
// fix-up apply path); the verifyCmd=="" and appliedFixup paths withhold it.
func TestUnusedGrantStagedKnown(t *testing.T) {
	cases := []struct {
		name         string
		appliedFixup bool
		verifyCmd    string
		want         bool
	}{
		{"committed gate ran (verifyCmd set, agent path)", false, "cd mod && go test ./...", true},
		{"verifyCmd empty -> withheld", false, "", false},
		{"appliedFixup -> withheld", true, "cd mod && go test ./...", false},
		{"appliedFixup and verifyCmd empty -> withheld", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unusedGrantStagedKnown(tc.appliedFixup, tc.verifyCmd); got != tc.want {
				t.Errorf("unusedGrantStagedKnown(%v, %q) = %v, want %v", tc.appliedFixup, tc.verifyCmd, got, tc.want)
			}
		})
	}
}

// TestDetectUnusedScopeAmendmentGrants_GitErrorFailsOpen: a non-git workingDir
// makes `git status` fail — the check logs scope_amendment_grant_check_failed
// and returns nil, nil (no grants, no events) instead of failing the stage.
func TestDetectUnusedScopeAmendmentGrants_GitErrorFailsOpen(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cfg := config{
		runID:              verifyFixRunID,
		stageID:            verifyFixStageID,
		workingDir:         t.TempDir(), // not a git repository
		approvedAmendments: []upload.ScopeAmendment{grantRow("amd-e2e", verifyFixStageID, "")},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log)
	if unused != nil || events != nil {
		t.Fatalf("got unused=%+v events=%+v, want nil,nil (fail-open)", unused, events)
	}
	if !strings.Contains(log.String(), `"event":"scope_amendment_grant_check_failed"`) {
		t.Errorf("missing scope_amendment_grant_check_failed:\n%s", log.String())
	}
	if strings.Contains(log.String(), "scope_amendment_grant_unused") {
		t.Errorf("a failed check must not emit the unused signal:\n%s", log.String())
	}
}

// TestDetectUnusedScopeAmendmentGrants_OtherStageOnly_NoOp (approval condition
// 3): approved rows exist ONLY for a different stage id — the run() gate
// (len(cfg.approvedAmendments) > 0) would admit them, so the inner stage
// filter is what must return nil,nil with no git call and no line.
func TestDetectUnusedScopeAmendmentGrants_OtherStageOnly_NoOp(t *testing.T) {
	cfg := config{
		runID:      verifyFixRunID,
		stageID:    verifyFixStageID,
		workingDir: t.TempDir(), // NOT a git repo: a git call here would log check_failed
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-other", undecidedOtherStageID, ""),
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log)
	if unused != nil || events != nil {
		t.Fatalf("got unused=%+v events=%+v, want nil,nil", unused, events)
	}
	if log.Len() != 0 {
		t.Errorf("other-stage-only rows must emit nothing (no git call, no line), got:\n%s", log.String())
	}
	// Empty stageID: same no-op, same silence.
	cfg.stageID = ""
	cfg.approvedAmendments = []upload.ScopeAmendment{grantRow("amd-e2e", "", "")}
	if u, e := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log); u != nil || e != nil || log.Len() != 0 {
		t.Errorf("empty stageID must be a silent no-op, got unused=%+v events=%+v log=%q", u, e, log.String())
	}
}

// TestEmitScopeAmendmentGrantUnused_SeamContract pins the JSONL field set
// {event, run_id, stage_id, grants:[{amendment_id, path, operation,
// disposition}]} and the
// policy_event payload {check, grants} against a REAL git repo whose dirty set
// excludes the granted path.
func TestEmitScopeAmendmentGrantUnused_SeamContract(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy) // dirty AND staged: a used grant under stagedKnown
	addCmd := exec.Command("git", "add", "mod/reg.go")
	addCmd.Dir = repo
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cfg := config{
		runID:      "run-abc",
		stageID:    verifyFixStageID,
		workingDir: repo,
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-7", verifyFixStageID, "why"),
			// A used grant and another stage's grant must not reach the wire.
			{ID: "amd-8", Status: "approved", StageID: verifyFixStageID, Paths: []upload.ScopeAmendmentPath{{Path: "mod/reg.go", Operation: "modify"}}},
			grantRow("amd-9", undecidedOtherStageID, ""),
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log)
	if len(unused) != 1 || unused[0] != (unusedScopeAmendmentGrant{"amd-7", "mod/other.go", "modify", grantNotModified}) {
		t.Fatalf("unused = %+v, want exactly amd-7 mod/other.go not_modified", unused)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines, want exactly 1: %q", len(lines), log.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("emitted line is not one JSON object: %v (%q)", err, lines[0])
	}
	if got["event"] != "scope_amendment_grant_unused" || got["run_id"] != "run-abc" || got["stage_id"] != verifyFixStageID {
		t.Errorf("event/run_id/stage_id = %v/%v/%v", got["event"], got["run_id"], got["stage_id"])
	}
	rows, ok := got["grants"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("grants = %v, want a 1-element array", got["grants"])
	}
	row, _ := rows[0].(map[string]any)
	if row["amendment_id"] != "amd-7" || row["path"] != "mod/other.go" || row["operation"] != "modify" || row["disposition"] != grantNotModified {
		t.Errorf("grant row = %v", row)
	}
	if len(row) != 4 {
		t.Errorf("grant row has %d fields, want exactly 4 {amendment_id, path, operation, disposition}: %v", len(row), row)
	}
	if len(got) != 4 {
		t.Errorf("line has %d fields, want exactly 4 {event, run_id, stage_id, grants}: %v", len(got), got)
	}
	if len(events) != 1 || events[0].Kind != "policy_event" {
		t.Fatalf("events = %+v, want one policy_event", events)
	}
	var payload struct {
		Check  string           `json:"check"`
		Grants []map[string]any `json:"grants"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Check != "scope_amendment_grant_unused" || len(payload.Grants) != 1 || payload.Grants[0]["amendment_id"] != "amd-7" {
		t.Errorf("policy_event payload = %s", events[0].Payload)
	}
}

// TestDetectUnusedScopeAmendmentGrants_ModifiedButUnstaged_NotStagedDisposition
// is the #3434 incident shape on a REAL repo: the granted file is written and
// `git add`ed, then `git reset -q` (StageScoped's leading mixed reset) leaves
// it ` M` — modified, unstaged. A second granted path is written AND left
// staged. With stagedKnown the check reports exactly ONE not_staged row for
// the first and none for the second; with stagedKnown=false (the withheld
// paths) it reports nothing at all. The counterfactual for the staged-set
// comparison: delete it and the first row disappears (RED).
func TestDetectUnusedScopeAmendmentGrants_ModifiedButUnstaged_NotStagedDisposition(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	// Seed both granted files as TRACKED so the unstaged one shows as ` M`,
	// not `??` (the incident's post-mortem column).
	mustWrite(t, filepath.Join(repo, "mod", "other.go"), "package mod\n")
	mustWrite(t, filepath.Join(repo, "mod", "third.go"), "package mod\n")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "seed granted files")
	// The fix agent edits + stages other.go; the loop's re-stage unstages it.
	mustWrite(t, filepath.Join(repo, "mod", "other.go"), "package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")
	runGit("add", "mod/other.go")
	runGit("reset", "-q")
	// third.go is edited AND left staged (a folded path the commit carried).
	mustWrite(t, filepath.Join(repo, "mod", "third.go"), "package mod\n\nvar third = 3\n")
	runGit("add", "mod/third.go")

	cfg := config{
		runID:      verifyFixRunID,
		stageID:    verifyFixStageID,
		workingDir: repo,
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-e2e", verifyFixStageID, ""),
			{ID: "amd-3", Status: "approved", StageID: verifyFixStageID, Paths: []upload.ScopeAmendmentPath{{Path: "mod/third.go", Operation: "modify"}}},
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log)
	want := unusedScopeAmendmentGrant{"amd-e2e", "mod/other.go", "modify", grantNotStaged}
	if len(unused) != 1 || unused[0] != want {
		t.Fatalf("unused = %+v, want exactly %+v", unused, want)
	}
	if len(events) != 1 || !strings.Contains(string(events[0].Payload), `"disposition":"not_staged"`) {
		t.Errorf("policy_event must carry the not_staged disposition: %+v", events)
	}
	if !strings.Contains(log.String(), `"disposition":"not_staged"`) || strings.Contains(log.String(), "mod/third.go") {
		t.Errorf("JSONL line must name only the not_staged grant:\n%s", log.String())
	}
	if strings.Contains(log.String(), "scope_amendment_grant_check_failed") {
		t.Errorf("no check_failed line expected on a healthy repo:\n%s", log.String())
	}
	// Withheld paths (stagedKnown=false): the dirty grant counts as used.
	log.Reset()
	if u, e := detectUnusedScopeAmendmentGrants(context.Background(), cfg, false, &log); u != nil || e != nil || log.Len() != 0 {
		t.Errorf("stagedKnown=false must report nothing for a dirty-but-unstaged grant, got unused=%+v events=%+v log=%q", u, e, log.String())
	}
}

// TestDetectUnusedScopeAmendmentGrants_StagedPathsErrorFailsOpen: when the
// dirty set is readable but the staged set is not (stagedPathsFn seam — both
// run the same `git status`, so a filesystem fault cannot fail one and not
// the other), the check logs scope_amendment_grant_check_failed and DEGRADES
// to stagedKnown=false: the not_modified row is still reported and the
// dirty-but-unstaged grant counts as used — no not_staged row is ever
// invented from an unreadable index.
func TestDetectUnusedScopeAmendmentGrants_StagedPathsErrorFailsOpen(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "other.go"), "package mod\n") // dirty (untracked), never staged
	orig := stagedPathsFn
	stagedPathsFn = func(context.Context, string) ([]string, error) { return nil, errors.New("index locked") }
	t.Cleanup(func() { stagedPathsFn = orig })

	cfg := config{
		runID:      verifyFixRunID,
		stageID:    verifyFixStageID,
		workingDir: repo,
		approvedAmendments: []upload.ScopeAmendment{
			grantRow("amd-e2e", verifyFixStageID, ""), // mod/other.go: dirty, unstaged
			{ID: "amd-4", Status: "approved", StageID: verifyFixStageID, Paths: []upload.ScopeAmendmentPath{{Path: "mod/fourth.go", Operation: "create"}}}, // clean
		},
	}
	var log bytes.Buffer
	unused, events := detectUnusedScopeAmendmentGrants(context.Background(), cfg, true, &log)
	want := unusedScopeAmendmentGrant{"amd-4", "mod/fourth.go", "create", grantNotModified}
	if len(unused) != 1 || unused[0] != want {
		t.Fatalf("unused = %+v, want exactly the not_modified row %+v (degraded to stagedKnown=false)", unused, want)
	}
	if len(events) != 1 {
		t.Errorf("events = %+v, want the one unused policy_event", events)
	}
	if !strings.Contains(log.String(), `"event":"scope_amendment_grant_check_failed"`) || !strings.Contains(log.String(), "index locked") {
		t.Errorf("missing scope_amendment_grant_check_failed naming the StagedPaths error:\n%s", log.String())
	}
	if strings.Contains(log.String(), "not_staged") || strings.Contains(log.String(), "mod/other.go") {
		t.Errorf("an unreadable index must never yield a not_staged row:\n%s", log.String())
	}
}

// approvedGrantRow is the e2e fixture row: the #2601 undecided row, APPROVED,
// with an operator decision reason — run faf4cf76's shape.
func approvedGrantRow() upload.ScopeAmendment {
	a := undecidedAmendmentRow("approved")
	a.DecisionReason = "seed the registry from an init in mod/other.go"
	return a
}

// approvedSecondGrantRow is a SECOND approved row for THIS stage, granting a
// path the fake agent never writes — the OK+unused quadrant partner to
// approvedGrantRow's USED grant (#3433).
func approvedSecondGrantRow() upload.ScopeAmendment {
	return upload.ScopeAmendment{
		ID:             "amd-e2e-2",
		RunID:          verifyFixRunID,
		StageID:        verifyFixStageID,
		Status:         "approved",
		Paths:          []upload.ScopeAmendmentPath{{Path: "mod/second.go", Operation: "create"}},
		Reason:         "the fix also needs a helper file",
		DecisionReason: "add the helper in mod/second.go",
	}
}

// grantFixPromptAssertions checks a captured iteration-1 fix prompt carries
// the grant: amendment id, `path (op)`, the operator decision reason, the
// GRANTED MID-STAGE marker, and the effective-scope list (which includes the
// folded path).
func grantFixPromptAssertions(t *testing.T, prompt string) {
	t.Helper()
	if prompt == "" {
		t.Fatal("iteration-1 fix prompt was never captured (no reinvoke?)")
	}
	for _, want := range []string{
		verifyFixGrantedMarker,
		"- amendment amd-e2e granted mod/other.go (modify)\n",
		"Operator decision reason: seed the registry from an init in mod/other.go",
		verifyFixEffectiveScopeHeader,
		"- mod/reg.go (modify)\n",
		"- mod/other.go (modify)\n",
		"in force for this fix",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "the fix needs a file the plan did not declare") {
		t.Errorf("fix prompt must not render the agent-authored amendment reason:\n%s", prompt)
	}
}

const unusedGrantLine = "amendment amd-e2e granted mod/other.go (modify); file not modified"

// TestRun_ApprovedScopeAmendment_FixPromptCarriesGrant_UnusedGrantFailsLoud is
// the #3390 DONE-MEANS test, driven end to end through run(): an APPROVED
// amendment for THIS stage (folded before the verify-fix loop) whose path the
// fake agent never touches. The iteration-1 fix prompt must carry the grant,
// and the exhausted stage must fail category-A with the byte-exact
// `amendment <id> granted <path> (<op>); file not modified` line, the
// scope_amendment_grant_unused JSONL line, the policy_event, and no push.
//
// The bad state is seeded BY CONSTRUCTION (an approved row whose path the
// agent never writes), never by calling the control inside setup.
func TestRun_ApprovedScopeAmendment_FixPromptCarriesGrant_UnusedGrantFailsLoud(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		// The fix agent fixes NOTHING: it only lets the test read its prompt.
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt() // VerifyMaxIterations 1
	fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	// (i) the fix re-invocation prompt names the grant.
	grantFixPromptAssertions(t, fixPrompt)
	// (ii) category-A exhaustion with the byte-exact attribution line.
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected a category-A terminal demotion:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), unusedGrantLine) {
		t.Errorf("failure reason missing the exact line %q:\n%s", unusedGrantLine, stderr.String())
	}
	// (iii) the JSONL signal and the bundle policy_event.
	if !strings.Contains(stderr.String(), `"event":"scope_amendment_grant_unused"`) {
		t.Errorf("missing the scope_amendment_grant_unused JSONL signal:\n%s", stderr.String())
	}
	events := readBundleEvents(t, bundlePath)
	if !hasPolicyEvent(events, "scope_amendment_grant_unused") {
		t.Error("bundle missing the scope_amendment_grant_unused policy_event")
	}
	// (iv) no push, and the budget was spent (initial + 1 fix).
	if fp.gotArgs != nil || fpr.gotArgs != nil {
		t.Error("CommitAndPush/OpenPR must not run after a terminal verify-fix exhaustion")
	}
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix)", invoker.callIdx)
	}
}

// TestRun_ApprovedScopeAmendment_GrantUsed_NoUnusedSignal is the
// discriminating twin over the IDENTICAL fixture: the fix agent EDITS the
// granted file (the seed-init shape), iteration 2 passes, and NO unused
// line/event/annotation appears — so the check reads the tree, not the grant
// list. It ALSO asserts positively that the iteration-1 fix prompt carried the
// grant (approval condition 2), so deleting the fold record reddens this twin
// too, not only the fail-loud test.
func TestRun_ApprovedScopeAmendment_GrantUsed_NoUnusedSignal(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
				// The fix lands in the GRANTED file, exactly as the prompt asks.
				mustWrite(t, filepath.Join(repo, "mod", "other.go"),
					"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	fu.amendments = []upload.ScopeAmendment{approvedGrantRow()}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// POSITIVE grant delivery (condition 2).
	grantFixPromptAssertions(t, fixPrompt)
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix that converged)", invoker.callIdx)
	}
	// NO unused signal anywhere: log line, policy_event, or failure reason.
	if strings.Contains(stderr.String(), "scope_amendment_grant_unused") {
		t.Errorf("a USED grant must emit no scope_amendment_grant_unused line:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "file not modified") {
		t.Errorf("a USED grant must add no failure-reason annotation:\n%s", stderr.String())
	}
	if hasPolicyEvent(readBundleEvents(t, bundlePath), "scope_amendment_grant_unused") {
		t.Error("bundle must carry no scope_amendment_grant_unused policy_event for a used grant")
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush must run: the fix converged")
	}
}

// runnerCompletedLine finds the run's single runner_completed JSONL line and
// decodes its outcome + reason (approval condition 1, #3433): a direct read
// of the observable completion event, not an inference from the ABSENCE of a
// substring across the whole log. An absent key decodes to the zero value, so
// reason=="" covers both "no reason key" (the OK shape) and "reason key
// present but empty".
//
// VERIFIED LIMIT (#3433 fix-up review): this direct decode has the SAME
// blind spot as the stderr-negative substring it supplements, not a
// different one. logCompletion's res.OK branch never renders a "reason" key
// at all — regardless of res.FailureReason's value — so it cannot observe
// whether annotateUnusedAmendmentFailure ran on a passing result. Confirmed
// by counterfactual: removing the `!res.OK &&` guard at the run()-level call
// site (main.go) and re-running
// TestRun_ApprovedScopeAmendment_OnePathUsedOneUnused_EventOnPassingPath left
// it GREEN. What this decode DOES add over the substring check: it reads a
// structured field instead of grepping free text, so it would catch a
// regression that changed logCompletion to render "reason" on the OK path
// with a non-empty value. It does not, and cannot, catch the annotation
// itself running unconditionally while res.OK stays true — that would need a
// surface independent of logCompletion's OK/failure branch, which does not
// exist today.
func runnerCompletedLine(t *testing.T, log string) (outcome, reason string) {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"event":"runner_completed"`) {
			continue
		}
		var decoded struct {
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("runner_completed line is not valid JSON: %v (%q)", err, line)
		}
		return decoded.Outcome, decoded.Reason
	}
	t.Fatal("no runner_completed line in log")
	return "", ""
}

// TestRun_ApprovedScopeAmendment_OnePathUsedOneUnused_EventOnPassingPath is
// the missing OK+unused quadrant (#3390/#3434, #3433): TWO approved grants
// for THIS stage — amd-e2e (mod/other.go, USED by the fix, so iteration 2
// converges) and amd-e2e-2 (mod/second.go, create, NEVER touched, seeded bad
// by construction: the fake agent simply never writes it). The stage still
// reaches exitOK — the unused check never sets res.OK/res.FailureCategory on
// the passing path — yet the event fires: the JSONL line + policy_event name
// ONLY the untouched amd-e2e-2/mod/second.go, never the used
// amd-e2e/mod/other.go, and no failure-reason attribution appears anywhere
// (the annotation is failure-only). This proves the README's "emitted on
// BOTH the passing and failing paths" claim was not vacuously true just
// because the failing twin (FixPromptCarriesGrant_UnusedGrantFailsLoud) has
// it — that test never exercises a PASSING run with an unused grant at all.
func TestRun_ApprovedScopeAmendment_OnePathUsedOneUnused_EventOnPassingPath(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
				// The fix lands in the USED grant only, exactly as the seed-init
				// shape of the discriminating twin above; mod/second.go (the
				// second grant) is deliberately never written.
				mustWrite(t, filepath.Join(repo, "mod", "other.go"),
					"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	fu.amendments = []upload.ScopeAmendment{approvedGrantRow(), approvedSecondGrantRow()}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	// (a) the fix prompt names BOTH grants.
	grantFixPromptAssertions(t, fixPrompt)
	for _, want := range []string{
		"- amendment amd-e2e-2 granted mod/second.go (create)\n",
		"- mod/second.go (create)\n",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, fixPrompt)
		}
	}
	// (b) converged and pushed.
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix that converged)", invoker.callIdx)
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush must run: the fix converged")
	}
	// (c) the unused signal names ONLY the untouched grant.
	if !strings.Contains(stderr.String(), `"event":"scope_amendment_grant_unused"`) {
		t.Fatalf("missing the scope_amendment_grant_unused JSONL signal:\n%s", stderr.String())
	}
	var unusedLine string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, `"event":"scope_amendment_grant_unused"`) {
			unusedLine = line
			break
		}
	}
	for _, want := range []string{`"amendment_id":"amd-e2e-2"`, `"path":"mod/second.go"`, `"disposition":"not_modified"`} {
		if !strings.Contains(unusedLine, want) {
			t.Errorf("unused JSONL line missing %q:\n%s", want, unusedLine)
		}
	}
	if strings.Contains(unusedLine, `"path":"mod/other.go"`) {
		t.Errorf("unused JSONL line must not name the USED grant:\n%s", unusedLine)
	}
	// (d) the trace bundle carries the policy_event too.
	if !hasPolicyEvent(readBundleEvents(t, bundlePath), "scope_amendment_grant_unused") {
		t.Error("bundle missing the scope_amendment_grant_unused policy_event")
	}
	// (e) no failure-reason attribution anywhere in the log — a stderr
	// negative, kept as a second arm alongside the direct check below. Both
	// literals are copied from the failure-path annotation in
	// scopeamendgrant.go (annotateUnusedAmendmentFailure): "file not
	// modified" is the not_modified disposition's suffix, and "The scope
	// amendment above was APPROVED" opens the anyNotModified closing sentence.
	if strings.Contains(stderr.String(), "file not modified") || strings.Contains(stderr.String(), "The scope amendment above was APPROVED") {
		t.Errorf("a passing run must carry no unused-grant failure annotation:\n%s", stderr.String())
	}
	// Approval condition 1: assert res.FailureReason == "" DIRECTLY via the
	// runner_completed line's reason field, structured-field-read alongside
	// the stderr-negative substring check above rather than a replacement for
	// it. Neither arm can observe an unconditional annotation call on a
	// passing result (see runnerCompletedLine's VERIFIED LIMIT doc comment,
	// confirmed by counterfactual on #3433 fix-up review) because
	// logCompletion's res.OK branch renders no "reason" key at all,
	// independent of res.FailureReason's value. Kept for what it DOES catch:
	// a regression in logCompletion itself that starts rendering "reason" on
	// the OK path.
	outcome, reason := runnerCompletedLine(t, stderr.String())
	if outcome != "ok" || reason != "" {
		t.Errorf("runner_completed outcome/reason = %q/%q, want \"ok\"/\"\"", outcome, reason)
	}
}

// TestRun_NoAmendment_FixPromptScopeOnly is the no-grant control over the
// same harness: a stage with NO amendments produces a fix prompt with the
// effective-scope list but no GRANTED block, no unused signal, and no
// scope_amendment_grant_check_failed line (the check never ran).
func TestRun_NoAmendment_FixPromptScopeOnly(t *testing.T) {
	pinAmendmentWatchInterval(t)
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	var fixPrompt string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			if idx == 1 {
				fixPrompt = inv.Prompt
			}
		},
	}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = undecidedVerifyFixPrompt()
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitFailure {
		t.Errorf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if fixPrompt == "" {
		t.Fatal("iteration-1 fix prompt was never captured")
	}
	for _, want := range []string{verifyFixEffectiveScopeHeader, "- mod/reg.go (modify)\n", "- mod/reg_test.go (create)\n"} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, fixPrompt)
		}
	}
	if strings.Contains(fixPrompt, verifyFixGrantedMarker) {
		t.Errorf("no-amendment stage must render no GRANTED block:\n%s", fixPrompt)
	}
	// #3410 always-on half: the amendment recipe renders in the effective-scope
	// form even when nothing is granted and no out-of-scope path is named.
	if !strings.Contains(fixPrompt, "POST $FISHHAWK_BACKEND_URL/v0/runs/<run_id>/scope-amendments") {
		t.Errorf("no-amendment stage must still render the amendment recipe:\n%s", fixPrompt)
	}
	for _, absent := range []string{"scope_amendment_grant_unused", "scope_amendment_grant_check_failed", "file not modified"} {
		if strings.Contains(stderr.String(), absent) {
			t.Errorf("no-amendment stage must not emit %q:\n%s", absent, stderr.String())
		}
	}
}

func hasPolicyEvent(events []bundle.Line, check string) bool {
	for _, ev := range events {
		if ev.Kind == "policy_event" && strings.Contains(string(ev.Data), `"`+check+`"`) {
			return true
		}
	}
	return false
}
