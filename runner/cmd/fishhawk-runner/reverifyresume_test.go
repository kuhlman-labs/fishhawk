package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// #4190: the reverify resume's precheck, pre-push gates, re-verify classifier
// and commit synthesis, each over real temp repositories.

// commitOnParent builds, BY CONSTRUCTION, a commit whose tree is parent's tree
// with files written, parented on parent — through a throwaway index, so the
// work tree, the real index and every ref are untouched. The result is
// reachable from NO ref, like a reset-away verify WIP commit.
func commitOnParent(t *testing.T, repo, parent string, files map[string]string) (commit, tree string) {
	t.Helper()
	idx := filepath.Join(t.TempDir(), "index")
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx,
			"GIT_AUTHOR_NAME=wip", "GIT_AUTHOR_EMAIL=wip@example.com",
			"GIT_COMMITTER_NAME=wip", "GIT_COMMITTER_EMAIL=wip@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "read-tree", parent)
	for p, body := range files {
		blob := run(body, "hash-object", "-w", "--stdin")
		run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+p)
	}
	tree = run("", "write-tree")
	return run("fishhawk verify wip\n", "commit-tree", tree, "-p", parent), tree
}

// reverifyFixture: a work repo whose bare origin carries main at base, plus a
// held WIP commit editing a.txt parented on base and reachable from no ref.
type reverifyFixture struct {
	repo, bare, base, held, heldTree string
}

func newReverifyFixture(t *testing.T) reverifyFixture {
	t.Helper()
	repo, bare, _, _, _ := pushResumeRepo(t)
	base := cprGit(t, repo, "rev-parse", "main")
	held, heldTree := commitOnParent(t, repo, base, map[string]string{"a.txt": "the held agent edit\n"})
	return reverifyFixture{repo: repo, bare: bare, base: base, held: held, heldTree: heldTree}
}

func (f reverifyFixture) input() reverifyHeld {
	return reverifyHeld{
		repoDir:    f.repo,
		verifyCmd:  "scripts/test verify",
		heldSHA:    f.held,
		baseSHA:    f.base,
		baseBranch: "main",
		scopeFiles: []upload.ScopeFile{{Path: "a.txt", Operation: "modify"}},
	}
}

// TestReverifyPrecheck asserts each row's token by IDENTITY, so a deleted guard
// surfaces as the NEXT row's token (or "") rather than passing.
func TestReverifyPrecheck(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, f reverifyFixture, h *reverifyHeld)
		want  string
		// wantDetail, when set, pins WHICH guard produced the token: the fetch
		// guard and the ancestry guard share the unverifiable token.
		wantDetail string
	}{
		{name: "all_good", setup: func(*testing.T, reverifyFixture, *reverifyHeld) {}, want: ""},
		{name: "verify_command_absent", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.verifyCmd = "  " }, want: reverifyFallbackVerifyCommandAbsent},
		{name: "scope_unavailable", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.scopeFiles = nil }, want: reverifyFallbackScopeUnavailable},
		{name: "held_sha_empty", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.heldSHA = "" }, want: reverifyFallbackHeldCommitAbsent},
		// A 40-hex id never written: absent by construction.
		{name: "held_commit_absent", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.heldSHA = strings.Repeat("ab", 20) }, want: reverifyFallbackHeldCommitAbsent},
		{name: "held_commit_base_mismatch", setup: func(t *testing.T, f reverifyFixture, h *reverifyHeld) {
			// An unrelated root commit: present, but not the held commit's parent.
			h.baseSHA = cprGit(t, f.repo, "commit-tree", f.heldTree, "-m", "unrelated root")
		}, want: reverifyFallbackBaseMismatch},
		{name: "base_branch_absent", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.baseBranch = "" }, want: reverifyFallbackBaseUnverifiable, wantDetail: "no declared base branch"},
		{name: "base_fetch_fails", setup: func(_ *testing.T, _ reverifyFixture, h *reverifyHeld) { h.baseBranch = "no-such-base" }, want: reverifyFallbackBaseUnverifiable, wantDetail: "fresh fetch of origin/no-such-base"},
		{name: "foreign_parent_not_on_base", setup: func(t *testing.T, f reverifyFixture, h *reverifyHeld) {
			// The held commit is parented on a LOCAL-ONLY commit origin never
			// saw, and served base == that parent, so the mismatch row passes.
			// The stale tracking ref is planted to CLAIM the foreign commit is
			// origin/main: only a FRESH fetch (which rewrites it to the real tip)
			// refuses it.
			foreign, _ := commitOnParent(t, f.repo, f.base, map[string]string{"foreign.txt": "never pushed\n"})
			held, _ := commitOnParent(t, f.repo, foreign, map[string]string{"a.txt": "the held agent edit\n"})
			cprGit(t, f.repo, "update-ref", "refs/remotes/origin/main", foreign)
			h.heldSHA, h.baseSHA = held, foreign
		}, want: reverifyFallbackBaseNotOnBase},
		{name: "base_rewritten_not_on_base", setup: func(t *testing.T, f reverifyFixture, _ *reverifyHeld) {
			// origin/main force-moved to an unrelated history: the held parent
			// is no longer an ancestor of the declared base.
			rewritten := cprGit(t, f.repo, "commit-tree", f.heldTree, "-m", "rewritten main")
			cprGit(t, f.repo, "push", "--force", "origin", rewritten+":refs/heads/main")
		}, want: reverifyFallbackBaseNotOnBase},
		{name: "base_advanced_is_ancestor", setup: func(t *testing.T, f reverifyFixture, _ *reverifyHeld) {
			// origin/main moved FORWARD: the held parent is a proper ancestor of
			// the fresh tip, which is the ordinary state of a retry.
			next, _ := commitOnParent(t, f.repo, f.base, map[string]string{"other.txt": "main advanced\n"})
			cprGit(t, f.repo, "push", "origin", next+":refs/heads/main")
		}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReverifyFixture(t)
			h := f.input()
			tc.setup(t, f, &h)
			got, detail := reverifyPrecheck(ctx, h)
			if got != tc.want {
				t.Fatalf("token = %q (%s), want %q", got, detail, tc.want)
			}
			if got != "" && detail == "" {
				t.Errorf("token %q carries no detail", got)
			}
			if !strings.Contains(detail, tc.wantDetail) {
				t.Errorf("detail %q does not name %q", detail, tc.wantDetail)
			}
		})
	}
}

// TestReverifyPrePushGates: each gate refuses a held tree that violates it,
// PERMANENT category B, and each passing row isolates the gate before it.
func TestReverifyPrePushGates(t *testing.T) {
	ctx := context.Background()
	scope := func(paths ...string) []upload.ScopeFile {
		var out []upload.ScopeFile
		for _, p := range paths {
			out = append(out, upload.ScopeFile{Path: p, Operation: "modify"})
		}
		return out
	}
	assertion := []upload.BindingAssertion{{Type: "file_contains", Path: "a.txt", Literal: "MUST-SHIP-4190"}}
	cases := []struct {
		name          string
		files         map[string]string
		scope         []upload.ScopeFile
		assertions    []upload.BindingAssertion
		exemptions    []scopeExemption
		absentHeld    bool
		wantToken     string
		wantCategory  string
		wantInDetail  string
		wantPermanent bool
	}{
		{name: "all_gates_pass", files: map[string]string{"a.txt": "MUST-SHIP-4190\n"}, scope: scope("a.txt"), assertions: assertion},
		{name: "out_of_scope", files: map[string]string{"a.txt": "x\n", "stray.txt": "agent-created\n"}, scope: scope("a.txt"),
			wantToken: reverifyGateOutOfScope, wantCategory: "B", wantPermanent: true, wantInDetail: "stray.txt"},
		{name: "scope_files_missing", files: map[string]string{"a.txt": "x\n"}, scope: scope("a.txt", "b.txt"),
			wantToken: reverifyGateScopeFilesMissing, wantCategory: "B", wantPermanent: true, wantInDetail: "b.txt (modify)"},
		{name: "missing_but_operator_exempted", files: map[string]string{"a.txt": "x\n"}, scope: scope("a.txt", "b.txt"),
			exemptions: []scopeExemption{{Path: "b.txt", Reason: "operator exempt"}}},
		{name: "binding_assertion_unsatisfied", files: map[string]string{"a.txt": "no literal here\n"}, scope: scope("a.txt"), assertions: assertion,
			wantToken: reverifyGateBindingAssertionUnsatisfied, wantCategory: "B", wantPermanent: true, wantInDetail: "MUST-SHIP-4190"},
		{name: "gate_unevaluable", absentHeld: true, scope: scope("a.txt"),
			wantToken: reverifyGateUnevaluable, wantCategory: "C", wantPermanent: true, wantInDetail: "scope assertion gate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReverifyFixture(t)
			h := f.input()
			if tc.absentHeld {
				h.heldSHA = strings.Repeat("ab", 20)
			} else {
				h.heldSHA, _ = commitOnParent(t, f.repo, f.base, tc.files)
			}
			h.scopeFiles, h.bindingAssertions, h.scopeExemptions = tc.scope, tc.assertions, tc.exemptions
			v := reverifyPrePushGates(ctx, h)
			if v.token != tc.wantToken {
				t.Fatalf("token = %q (%s), want %q", v.token, v.detail, tc.wantToken)
			}
			if v.ok() != (tc.wantToken == "") {
				t.Errorf("ok() = %v for token %q", v.ok(), v.token)
			}
			if v.category != tc.wantCategory || v.permanent != tc.wantPermanent {
				t.Errorf("category %q permanent %v, want %q %v", v.category, v.permanent, tc.wantCategory, tc.wantPermanent)
			}
			if !strings.Contains(v.detail, tc.wantInDetail) {
				t.Errorf("detail %q does not name %q", v.detail, tc.wantInDetail)
			}
		})
	}
}

// scriptedVerify is one scripted runVerifyCommittedTree result.
type scriptedVerify struct {
	out     string
	outcome string
	disp    gateDisposition
}

// withScriptedReverify swaps reverifyVerifyFn for a script, recording each
// call's package set (nil = the FULL form) and head.
func withScriptedReverify(t *testing.T, script []scriptedVerify) (pkgSets *[][]string, heads *[]string) {
	t.Helper()
	orig := reverifyVerifyFn
	t.Cleanup(func() { reverifyVerifyFn = orig })
	var sets [][]string
	var hs []string
	reverifyVerifyFn = func(_ context.Context, cmd, _ string, head string, _ time.Duration, scopePkgs []string) (agent.Event, string, string, gateDisposition) {
		i := len(sets)
		sets = append(sets, scopePkgs)
		hs = append(hs, head)
		if i >= len(script) {
			t.Fatalf("unexpected verify call %d (script has %d)", i+1, len(script))
		}
		s := script[i]
		return verifyRunEvent(cmd, head, "", 1, s.out, s.outcome), s.out, s.outcome, s.disp
	}
	return &sets, &hs
}

// TestReverifyHeldCommit drives the classifier with a scripted verify. Every
// call must be the FULL form (nil package set); the call COUNT pins the
// one-shot infra re-run.
func TestReverifyHeldCommit(t *testing.T) {
	const infra = "lint: " + golangciLintLockSignature + "\n"
	const lockRefusal = "scripts/test: verify lock held: another RUNNER verify still holds the lock\n"
	inScope := "FAIL\tgithub.com/kuhlman-labs/fishhawk/runner/cmd/widget\t0.1s\n"
	outside := "--- FAIL: TestFlaky (0.01s)\nFAIL\tgithub.com/kuhlman-labs/fishhawk/backend/internal/other\t0.2s\n"
	pass := scriptedVerify{out: "ok", outcome: "passed", disp: gateExecuted}
	failed := func(out string) scriptedVerify {
		return scriptedVerify{out: out, outcome: "failed", disp: gateExecuted}
	}
	cases := []struct {
		name          string
		script        []scriptedVerify
		wantCalls     int
		wantToken     string
		wantPermanent bool
	}{
		{name: "pass", script: []scriptedVerify{pass}, wantCalls: 1},
		{name: "infra_then_pass", script: []scriptedVerify{failed(infra), pass}, wantCalls: 2},
		{name: "infra_twice", script: []scriptedVerify{failed(infra), failed(infra)}, wantCalls: 2, wantToken: reverifyGateNotExecuted},
		{name: "refused", script: []scriptedVerify{{out: "refused", outcome: "failed", disp: gateRefused}}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		{name: "unavailable", script: []scriptedVerify{{out: "no runtime", outcome: "failed", disp: gateUnavailable}}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		{name: "timed_out", script: []scriptedVerify{{out: "killed at the deadline", outcome: "failed", disp: gateTimedOut}}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		// An infra signature in a TIMED-OUT gate's output must not buy a re-run:
		// only an EXECUTED failure is re-run (a re-run costs another timeout).
		{name: "timed_out_infra_output_no_rerun", script: []scriptedVerify{{out: infra, outcome: "failed", disp: gateTimedOut}}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		{name: "skipped", script: []scriptedVerify{{out: "clone: boom", outcome: "skipped", disp: gateExecuted}}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		{name: "lock_contended", script: []scriptedVerify{failed(lockRefusal)}, wantCalls: 1, wantToken: reverifyGateNotExecuted},
		{name: "failed_outside_change", script: []scriptedVerify{failed(outside)}, wantCalls: 1, wantToken: reverifyFailedOutsideChange},
		{name: "failed_in_change", script: []scriptedVerify{failed(inScope)}, wantCalls: 1, wantToken: reverifyFailedInChange, wantPermanent: true},
		{name: "undecidable_output", script: []scriptedVerify{failed("exit status 2, no test lines")}, wantCalls: 1, wantToken: reverifyFailedInChange, wantPermanent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sets, heads := withScriptedReverify(t, tc.script)
			h := reverifyHeld{repoDir: t.TempDir(), verifyCmd: "scripts/test verify", heldSHA: "held-4190",
				scopeFiles: []upload.ScopeFile{{Path: "runner/cmd/widget/w.go", Operation: "modify"}}}
			res := reverifyHeldCommit(context.Background(), h, time.Minute)
			if len(*sets) != tc.wantCalls || res.runs != tc.wantCalls || len(res.events) != tc.wantCalls {
				t.Fatalf("verify calls = %d (runs %d, events %d), want %d", len(*sets), res.runs, len(res.events), tc.wantCalls)
			}
			for i, s := range *sets {
				if s != nil {
					t.Errorf("call %d package set = %v, want nil (the FULL form)", i+1, s)
				}
				if (*heads)[i] != "held-4190" {
					t.Errorf("call %d verified %q, want the held commit", i+1, (*heads)[i])
				}
			}
			v := res.verdict
			if v.token != tc.wantToken {
				t.Fatalf("token = %q (%s), want %q", v.token, v.detail, tc.wantToken)
			}
			if v.permanent != tc.wantPermanent {
				t.Errorf("permanent = %v, want %v", v.permanent, tc.wantPermanent)
			}
			if !v.ok() && v.category != "C" {
				t.Errorf("category = %q, want C", v.category)
			}
			if res.output != tc.script[tc.wantCalls-1].out {
				t.Errorf("output = %q, want the LAST run's", res.output)
			}
		})
	}
}

// TestSynthesizeCommitOnParent: the commit carries the input tree, the EXPLICIT
// parent (HEAD is deliberately elsewhere, so a HEAD-parented body is
// observable), a Signed-off-by trailer and the given identity — and keeps the
// held commit's patch-id.
func TestSynthesizeCommitOnParent(t *testing.T) {
	f := newReverifyFixture(t)
	ctx := context.Background()
	// Move HEAD off the parent: the pushResumeRepo branch head is a different
	// commit from base.
	elsewhere, _ := commitOnParent(t, f.repo, f.base, map[string]string{"elsewhere.txt": "HEAD lives here\n"})
	cprGit(t, f.repo, "checkout", "--detach", elsewhere)
	const name, email = "fishhawk-dev[bot]", "42+fishhawk-dev[bot]@users.noreply.github.com"
	sha, err := synthesizeCommitOnParent(ctx, f.repo, f.heldTree, f.base, "feat: the agent's change\n\nbody\n", name, email)
	if err != nil {
		t.Fatal(err)
	}
	if got := cprGit(t, f.repo, "rev-parse", sha+"^{tree}"); got != f.heldTree {
		t.Errorf("tree = %s, want %s", got, f.heldTree)
	}
	if got := cprGit(t, f.repo, "rev-parse", sha+"^"); got != f.base {
		t.Errorf("parent = %s, want the explicit parent %s (HEAD is %s)", got, f.base, elsewhere)
	}
	if msg := cprGit(t, f.repo, "log", "-1", "--format=%B", sha); !strings.Contains(msg, "Signed-off-by: "+name+" <"+email+">") {
		t.Errorf("message lacks the Signed-off-by trailer:\n%s", msg)
	}
	if got := cprGit(t, f.repo, "log", "-1", "--format=%an <%ae>|%cn <%ce>", sha); got != name+" <"+email+">|"+name+" <"+email+">" {
		t.Errorf("identity = %q", got)
	}
	if a, b := gitPatchIDForCommit(ctx, f.repo, sha), gitPatchIDForCommit(ctx, f.repo, f.held); a == "" || a != b {
		t.Errorf("patch-id %q != held commit's %q: same tree and parent must keep the change identity", a, b)
	}

	// Refusals, each asserted by CAUSE.
	for _, tc := range []struct {
		name, tree, parent, msg, want string
	}{
		{name: "no_tree", parent: f.base, msg: "m", want: "no tree"},
		{name: "no_parent", tree: f.heldTree, msg: "m", want: "no parent"},
		{name: "empty_message", tree: f.heldTree, parent: f.base, msg: " \n", want: "empty commit message"},
		{name: "missing_tree", tree: strings.Repeat("cd", 20), parent: f.base, msg: "m", want: "commit-tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := synthesizeCommitOnParent(ctx, f.repo, tc.tree, tc.parent, tc.msg, "", ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	// Defaults: an empty identity falls back to the gitops bot identity.
	def, err := synthesizeCommitOnParent(ctx, f.repo, f.heldTree, f.base, "m", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := cprGit(t, f.repo, "log", "-1", "--format=%an <%ae>", def); got != gitops.DefaultAuthorName+" <"+gitops.DefaultAuthorEmail+">" {
		t.Errorf("default identity = %q", got)
	}
}
