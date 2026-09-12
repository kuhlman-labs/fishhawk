package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/scenario"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// scenarioYAML renders a corpus file the way scenario.Write does, so a seeded
// corpus round-trips through the real loader.
func scenarioYAML(id string, issue, pr int, runID string, recordedAt time.Time) string {
	return fmt.Sprintf("id: %s\nstatement: seeded %s\nsteps: GET /\nassertions:\n  expected: \"200\"\n  observed_at_record: \"200\"\norigin:\n  issue: %d\n  pr: %d\n  run_id: %s\n  head_sha: abc\n  recorded_at: %s\n",
		id, id, issue, pr, runID, recordedAt.UTC().Format(time.RFC3339))
}

// persistRepo builds a dispatch repo with a committed corpus (main), a bare
// origin the run branch is pushed to, and a detached acceptance tree at main's
// tip (the merge candidate). It returns the repo, the tree dir, the bare
// origin path and the merge-candidate SHA.
func persistRepo(t *testing.T, corpus map[string]string) (repo, tree, origin, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	var runGit func(args ...string)
	repo, runGit = compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	for rel, body := range corpus {
		p := filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p, body)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "seed corpus")
	head = gitHead(t, repo)
	origin = filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	runGit("push", origin, "main:main")
	tree = filepath.Join(t.TempDir(), "tree")
	runGit("worktree", "add", "--detach", tree, head)
	return repo, tree, origin, head
}

func passedVerdictFor(critID string) []byte {
	return []byte(`{"verdict":"passed","criteria":[{"id":"` + critID + `","result":"passed","observed":"200","expected":"200","steps_taken":"GET /","repro_handle":"curl"}]}`)
}

func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPersistAcceptanceScenarios_PushesScenarioOnlyCommit: a passed verdict on
// a drivable criterion records issue-101/crit-b.yaml with origin.pr 742 and
// origin.issue 101 (distinct), merges the served retirement with its reason,
// and pushes ONE commit touching only acceptance/scenarios/** to the run
// branch on the bare origin via the REAL gitops.Pusher.
func TestPersistAcceptanceScenarios_PushesScenarioOnlyCommit(t *testing.T) {
	_, tree, origin, head := persistRepo(t, nil)
	retired := []scenario.RetiredEntry{{ID: "scenario:issue-7/old", Reason: "behaviour replaced by #3327", RunID: "r0", PR: 700, RetiredAt: "2026-09-01T00:00:00Z"}}
	var log strings.Builder
	res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{
		treeDir: tree, runBranch: "fishhawk/run-r1", remoteURL: origin,
		issue: 101, prNumber: 742, headSHA: head, runID: "r1",
		criteria: []upload.AcceptanceCriterionEntry{
			{ID: "crit-b", Statement: "the run row lists the acceptance stage", Drivable: true},
			{ID: "crit-skip", Statement: "not drivable", Drivable: false},
		},
		retired: retired, verdict: []byte(`{"verdict":"passed","criteria":[{"id":"crit-b","result":"passed","observed":"listed","expected":"listed","steps_taken":"GET /runs/<id>"},{"id":"crit-skip","result":"passed"}]}`),
		now: func() time.Time { return time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC) },
	}, &gitops.Pusher{}, &log)
	if res.outcome != persistPushed {
		t.Fatalf("outcome = %s (%s), want persist_pushed\n%s", res.outcome, res.reason, log.String())
	}
	if res.baseSHA != head || res.headSHA == head || len(res.scenarioIDs) != 1 || res.scenarioIDs[0] != "scenario:issue-101/crit-b" {
		t.Errorf("result = %+v", res)
	}
	// The bare origin's run branch is the pushed head, one commit over the
	// merge candidate, touching only the corpus.
	if got := tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-r1"); got != res.headSHA {
		t.Errorf("origin run branch = %s, want %s", got, res.headSHA)
	}
	files := strings.Split(tgit(t, origin, "diff-tree", "--no-commit-id", "--name-only", "-r", res.headSHA), "\n")
	if len(files) != 2 {
		t.Fatalf("commit files = %v, want the scenario + retired.yaml only", files)
	}
	for _, f := range files {
		if !strings.HasPrefix(f, "acceptance/scenarios/") {
			t.Errorf("commit touches %s outside the corpus", f)
		}
	}
	yaml := tgit(t, origin, "show", res.headSHA+":acceptance/scenarios/issue-101/crit-b.yaml")
	for _, want := range []string{"id: scenario:issue-101/crit-b", "issue: 101", "pr: 742", "run_id: r1", "head_sha: " + head, "steps: GET /runs/<id>"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("recorded YAML missing %q:\n%s", want, yaml)
		}
	}
	ledger := tgit(t, origin, "show", res.headSHA+":acceptance/scenarios/retired.yaml")
	if !strings.Contains(ledger, "reason: 'behaviour replaced by #3327'") && !strings.Contains(ledger, `reason: "behaviour replaced by #3327"`) && !strings.Contains(ledger, "reason: behaviour replaced by #3327") {
		t.Errorf("retired.yaml lost the reason:\n%s", ledger)
	}
	// Author is the run's commit identity with a DCO sign-off.
	if msg := tgit(t, origin, "log", "-1", "--format=%an <%ae>%n%b", res.headSHA); !strings.Contains(msg, gitops.DefaultAuthorName) || !strings.Contains(msg, "Signed-off-by:") {
		t.Errorf("commit identity/sign-off = %q", msg)
	}
}

// TestPersist_RefusesDirtyOutsideCorpus: a stray file seeded BY CONSTRUCTION
// in the acceptance tree makes the commit persist_refused — nothing is
// committed, nothing pushed — even though a scenario WAS composed.
func TestPersist_RefusesDirtyOutsideCorpus(t *testing.T) {
	_, tree, origin, head := persistRepo(t, nil)
	mustWrite(t, filepath.Join(tree, "stray.txt"), "planted\n")
	res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{
		treeDir: tree, runBranch: "fishhawk/run-r1", remoteURL: origin, issue: 101, prNumber: 742, headSHA: head, runID: "r1",
		criteria: []upload.AcceptanceCriterionEntry{{ID: "crit-b", Statement: "s", Drivable: true}},
		verdict:  passedVerdictFor("crit-b"),
	}, &gitops.Pusher{}, &strings.Builder{})
	if res.outcome != persistRefused || !strings.Contains(res.reason, "stray.txt") {
		t.Fatalf("outcome = %s (%s), want persist_refused naming stray.txt", res.outcome, res.reason)
	}
	if out, err := exec.Command("git", "-C", origin, "rev-parse", "--verify", "refs/heads/fishhawk/run-r1").CombinedOutput(); err == nil {
		t.Errorf("refused persist must push nothing, but origin has the run branch: %s", out)
	}
	if tgit(t, tree, "rev-parse", "HEAD") != head {
		t.Error("refused persist must not commit")
	}
}

// TestPersist_WritesFullRetirementOnFailedVerdict: a FAILED verdict records no
// scenario but still merges the served retirement — reason intact — into
// retired.yaml and pushes it.
func TestPersist_WritesFullRetirementOnFailedVerdict(t *testing.T) {
	_, tree, origin, head := persistRepo(t, nil)
	res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{
		treeDir: tree, runBranch: "fishhawk/run-r1", remoteURL: origin, issue: 101, prNumber: 742, headSHA: head, runID: "r1",
		criteria: []upload.AcceptanceCriterionEntry{{ID: "crit-b", Statement: "s", Drivable: true}},
		retired:  []scenario.RetiredEntry{{ID: "scenario:issue-101/crit-b", Reason: "behaviour replaced by #3327", RunID: "r1", PR: 742, RetiredAt: "2026-09-12T00:00:00Z"}},
		verdict:  []byte(`{"verdict":"failed","failure_mode":"assertion_fail","criteria":[{"id":"crit-b","result":"failed"}]}`),
	}, &gitops.Pusher{}, &strings.Builder{})
	if res.outcome != persistPushed || len(res.scenarioIDs) != 0 {
		t.Fatalf("outcome = %s (%s) ids=%v, want persist_pushed with no scenario", res.outcome, res.reason, res.scenarioIDs)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, scenario.RetiredFile), tgit(t, origin, "show", res.headSHA+":acceptance/scenarios/retired.yaml")+"\n")
	got, err := scenario.LoadRetired(dir)
	if err != nil || len(got) != 1 || got[0].Reason != "behaviour replaced by #3327" || got[0].PR != 742 || got[0].RunID != "r1" {
		t.Fatalf("pushed ledger = %+v (%v), want the full entry with its reason", got, err)
	}
}

// TestPersist_UnknownPRRecordsZeroNotIssue: prNumber 0 is recorded as pr: 0,
// never substituted with the issue number.
func TestPersist_UnknownPRRecordsZeroNotIssue(t *testing.T) {
	_, tree, origin, head := persistRepo(t, nil)
	res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{
		treeDir: tree, runBranch: "fishhawk/run-r1", remoteURL: origin, issue: 101, prNumber: 0, headSHA: head, runID: "r1",
		criteria: []upload.AcceptanceCriterionEntry{{ID: "crit-b", Statement: "s", Drivable: true}},
		verdict:  passedVerdictFor("crit-b"),
	}, &gitops.Pusher{}, &strings.Builder{})
	if res.outcome != persistPushed {
		t.Fatalf("outcome = %s (%s)", res.outcome, res.reason)
	}
	yaml := tgit(t, origin, "show", res.headSHA+":acceptance/scenarios/issue-101/crit-b.yaml")
	if !strings.Contains(yaml, "pr: 0\n") || strings.Contains(yaml, "pr: 101") {
		t.Errorf("origin.pr must be 0 (unknown), never the issue:\n%s", yaml)
	}
}

// TestPersist_BestEffort: one case per non-pushed exit — each resolves to a
// named outcome/reason and never panics or returns an error.
func TestPersist_BestEffort(t *testing.T) {
	crit := []upload.AcceptanceCriterionEntry{{ID: "crit-b", Statement: "s", Drivable: true}}
	t.Run("no_acceptance_tree", func(t *testing.T) {
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: filepath.Join(t.TempDir(), "absent"), runBranch: "b", verdict: passedVerdictFor("crit-b")}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistSkipped || res.reason != "no_acceptance_tree" {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("no_run_branch", func(t *testing.T) {
		_, tree, _, _ := persistRepo(t, nil)
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, verdict: passedVerdictFor("crit-b")}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistSkipped || res.reason != "no_run_branch" {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("unchanged_nothing_to_write", func(t *testing.T) {
		_, tree, origin, head := persistRepo(t, nil)
		// Failed verdict, no retirements: nothing composed, nothing merged.
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", remoteURL: origin, issue: 101, headSHA: head, criteria: crit,
			verdict: []byte(`{"verdict":"failed","failure_mode":"error"}`)}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistSkipped || res.reason != "unchanged" {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("no_issue_number_records_nothing", func(t *testing.T) {
		_, tree, origin, head := persistRepo(t, nil)
		var log strings.Builder
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", remoteURL: origin, issue: 0, prNumber: 742, headSHA: head, criteria: crit, verdict: passedVerdictFor("crit-b")}, &gitops.Pusher{}, &log)
		if res.outcome != persistSkipped || res.reason != "unchanged" || !strings.Contains(log.String(), `"reason":"no_issue_number"`) {
			t.Errorf("got %+v log=%s", res, log.String())
		}
	})
	t.Run("push_error", func(t *testing.T) {
		_, tree, _, head := persistRepo(t, nil)
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", remoteURL: filepath.Join(t.TempDir(), "missing.git"), issue: 101, headSHA: head, criteria: crit, verdict: passedVerdictFor("crit-b")}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistFailed || !strings.HasPrefix(res.reason, "push: ") {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("no_remote", func(t *testing.T) {
		_, tree, _, head := persistRepo(t, nil)
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", issue: 101, headSHA: head, criteria: crit, verdict: passedVerdictFor("crit-b")}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistSkipped || res.reason != "no_remote" {
			t.Errorf("got %+v", res)
		}
	})
	t.Run("verdict_undecodable", func(t *testing.T) {
		_, tree, _, _ := persistRepo(t, nil)
		res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", verdict: []byte(`{`)}, &gitops.Pusher{}, &strings.Builder{})
		if res.outcome != persistFailed || !strings.HasPrefix(res.reason, "verdict_undecodable") {
			t.Errorf("got %+v", res)
		}
	})
}

// TestPersist_AlreadyLedgeredDisarms: every served retirement already in
// retired.yaml at HEAD → persist_skipped unchanged with retirementsLedgered
// true (a re-run after a prior persist is not a drop).
func TestPersist_AlreadyLedgeredDisarms(t *testing.T) {
	entry := scenario.RetiredEntry{ID: "scenario:issue-101/crit-b", Reason: "behaviour replaced by #3327", RunID: "r1", PR: 742, RetiredAt: "2026-09-12T00:00:00Z"}
	_, tree, origin, head := persistRepo(t, map[string]string{
		"retired.yaml": "retired:\n  - id: scenario:issue-101/crit-b\n    reason: behaviour replaced by #3327\n    run_id: r1\n    pr: 742\n    retired_at: \"2026-09-12T00:00:00Z\"\n",
	})
	res := persistAcceptanceScenarios(context.Background(), acceptancePersistInputs{treeDir: tree, runBranch: "b", remoteURL: origin, issue: 101, headSHA: head,
		retired: []scenario.RetiredEntry{entry}, verdict: []byte(`{"verdict":"failed","failure_mode":"error"}`)}, &gitops.Pusher{}, &strings.Builder{})
	if res.outcome != persistSkipped || !res.retirementsLedgered {
		t.Errorf("got %+v, want persist_skipped with retirementsLedgered", res)
	}
}

// TestRetirementDropReporter: nil-safe no-ops, the note/disarm contract, and
// the report itself (shipped through the uploadClient seam; nil client or key
// logs the drop unreported).
func TestRetirementDropReporter(t *testing.T) {
	var nilR *retirementDropReporter
	nilR.note("x")
	nilR.disarm("y")
	nilR.report(context.Background(), nil, config{}, nil, &strings.Builder{})
	if newRetirementDropReporter(nil) != nil {
		t.Fatal("no served entries must not arm")
	}
	entries := []scenario.RetiredEntry{{ID: "scenario:issue-101/crit-b", Reason: "behaviour replaced by #3327", RunID: "r1", PR: 742, RetiredAt: "2026-09-12T00:00:00Z"}}
	cfg := config{runID: "r", stageID: "s"}

	t.Run("ships dropped with the noted reason", func(t *testing.T) {
		fu := newFakeUploader(t)
		key := &upload.IssuedKey{PrivateKey: fu.priv}
		r := newRetirementDropReporter(entries)
		r.note("persist_refused:dirty_outside_corpus")
		var log strings.Builder
		r.report(context.Background(), fu, cfg, key, &log)
		if fu.gotPRArgs == nil || fu.gotPRArgs.Outcome != upload.OutcomeAcceptanceScenarioRetirementDropped ||
			fu.gotPRArgs.Reason != "persist_refused:dirty_outside_corpus" || len(fu.gotPRArgs.RetiredScenarios) != 1 ||
			fu.gotPRArgs.RetiredScenarios[0].Reason != "behaviour replaced by #3327" {
			t.Fatalf("drop report = %+v", fu.gotPRArgs)
		}
		if !strings.Contains(log.String(), `"event":"acceptance_scenario_retirement_dropped"`) || !strings.Contains(log.String(), `"event":"acceptance_scenario_retirement_drop_reported"`) {
			t.Errorf("log = %s", log.String())
		}
	})
	t.Run("default reason when nothing noted", func(t *testing.T) {
		fu := newFakeUploader(t)
		newRetirementDropReporter(entries).report(context.Background(), fu, cfg, &upload.IssuedKey{PrivateKey: fu.priv}, &strings.Builder{})
		if fu.gotPRArgs == nil || fu.gotPRArgs.Reason != retirementDropDefaultReason {
			t.Fatalf("drop report = %+v", fu.gotPRArgs)
		}
	})
	t.Run("disarmed ships nothing", func(t *testing.T) {
		fu := newFakeUploader(t)
		r := newRetirementDropReporter(entries)
		r.disarm("pushed_and_reported")
		var log strings.Builder
		r.report(context.Background(), fu, cfg, &upload.IssuedKey{PrivateKey: fu.priv}, &log)
		if fu.gotPRArgs != nil {
			t.Fatalf("disarmed reporter shipped %+v", fu.gotPRArgs)
		}
		if !strings.Contains(log.String(), `"event":"acceptance_scenario_retirement_persisted"`) || !strings.Contains(log.String(), `"how":"pushed_and_reported"`) {
			t.Errorf("log = %s", log.String())
		}
	})
	t.Run("nil key logs unreported", func(t *testing.T) {
		fu := newFakeUploader(t)
		var log strings.Builder
		newRetirementDropReporter(entries).report(context.Background(), fu, cfg, nil, &log)
		if fu.gotPRArgs != nil || !strings.Contains(log.String(), `"event":"acceptance_scenario_retirement_drop_unreported"`) {
			t.Errorf("gotPR=%+v log=%s", fu.gotPRArgs, log.String())
		}
	})
	t.Run("report failure is logged not fatal", func(t *testing.T) {
		fu := newFakeUploader(t)
		fu.prErr = errors.New("backend down")
		var log strings.Builder
		newRetirementDropReporter(entries).report(context.Background(), fu, cfg, &upload.IssuedKey{PrivateKey: fu.priv}, &log)
		if !strings.Contains(log.String(), `"event":"acceptance_scenario_retirement_drop_report_failed"`) {
			t.Errorf("log = %s", log.String())
		}
	})
}

// guardRepo seeds main with a corpus (crit-b + crit-c, optional ledger),
// then applies mutate on a run branch and returns a detached tree at its tip.
func guardRepo(t *testing.T, ledger string, mutate func(repo string, runGit func(args ...string))) (tree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	corpus := filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), "issue-101")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	mustWrite(t, filepath.Join(corpus, "crit-b.yaml"), scenarioYAML("scenario:issue-101/crit-b", 101, 742, "r0", old))
	mustWrite(t, filepath.Join(corpus, "crit-c.yaml"), scenarioYAML("scenario:issue-101/crit-c", 101, 742, "r0", old))
	if ledger != "" {
		mustWrite(t, filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), scenario.RetiredFile), ledger)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "seed")
	runGit("checkout", "-q", "-b", "fishhawk/run-x")
	mutate(repo, runGit)
	runGit("add", "-A")
	runGit("commit", "-q", "--allow-empty", "-m", "run branch edit")
	tree = filepath.Join(t.TempDir(), "tree")
	runGit("worktree", "add", "--detach", tree, gitHead(t, repo))
	return tree
}

const ledgerCritB = "retired:\n  - id: scenario:issue-101/crit-b\n    reason: behaviour replaced by #3327\n    run_id: r1\n    pr: 742\n    retired_at: \"2026-09-12T00:00:00Z\"\n"

// TestScenarioRemovalGuard: one case per branch.
func TestScenarioRemovalGuard(t *testing.T) {
	servedB := []scenario.RetiredEntry{{ID: "scenario:issue-101/crit-b", Reason: "r", RunID: "r1", PR: 742, RetiredAt: "2026-09-12T00:00:00Z"}}
	deleteB := func(repo string, runGit func(...string)) {
		runGit("rm", "-q", "acceptance/scenarios/issue-101/crit-b.yaml")
	}
	cases := []struct {
		name       string
		ledger     string
		served     []scenario.RetiredEntry
		mutate     func(string, func(...string))
		wantReason string
		wantDetail string
	}{
		{"deleted without retirement → B", "", nil, deleteB, guardRemovedWithoutRetirement, "crit-b.yaml"},
		{"deleted, in retired.yaml@HEAD → proceeds", ledgerCritB, nil, deleteB, "", ""},
		{"deleted, only in served → proceeds", "", servedB, deleteB, "", ""},
		{"modified without retirement → B", "", nil, func(repo string, _ func(...string)) {
			mustWrite(t, filepath.Join(repo, "acceptance/scenarios/issue-101/crit-b.yaml"), scenarioYAML("scenario:issue-101/crit-b", 101, 742, "r0", time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)))
		}, guardRemovedWithoutRetirement, "M acceptance/scenarios/issue-101/crit-b.yaml"},
		{"ledger entry new at HEAD, not served → B", "", nil, func(repo string, _ func(...string)) {
			mustWrite(t, filepath.Join(repo, "acceptance/scenarios/retired.yaml"), ledgerCritB)
		}, guardRetirementUnledgered, "scenario:issue-101/crit-b"},
		{"ledger entry new at HEAD and served → proceeds", "", servedB, func(repo string, _ func(...string)) {
			mustWrite(t, filepath.Join(repo, "acceptance/scenarios/retired.yaml"), ledgerCritB)
		}, "", ""},
		{"untouched corpus → proceeds", "", nil, func(string, func(...string)) {}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := guardRepo(t, tc.ledger, tc.mutate)
			var log strings.Builder
			reason, detail := scenarioRemovalGuard(context.Background(), tree, "main", tc.served, "r1", &log)
			if reason != tc.wantReason || !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("guard = %q / %q, want %q containing %q\n%s", reason, detail, tc.wantReason, tc.wantDetail, log.String())
			}
			if reason == "" && !strings.Contains(log.String(), `"event":"acceptance_scenario_guard_passed"`) {
				t.Errorf("missing guard_passed event: %s", log.String())
			}
		})
	}
	t.Run("unresolvable merge base → proceeds", func(t *testing.T) {
		tree := guardRepo(t, "", deleteB)
		var log strings.Builder
		reason, _ := scenarioRemovalGuard(context.Background(), tree, "no-such-base", nil, "r1", &log)
		if reason != "" || !strings.Contains(log.String(), `"event":"acceptance_scenario_guard_unresolved"`) {
			t.Fatalf("guard = %q log=%s", reason, log.String())
		}
	})
	t.Run("no tree → proceeds", func(t *testing.T) {
		var log strings.Builder
		if reason, _ := scenarioRemovalGuard(context.Background(), filepath.Join(t.TempDir(), "absent"), "main", nil, "r1", &log); reason != "" {
			t.Fatal(reason)
		}
	})
}

func TestAcceptanceServedVerdictIDs_Union(t *testing.T) {
	got := acceptanceServedVerdictIDs([]string{"AC1"}, scenario.ReplaySet{Scenarios: []scenario.ReplayedScenario{{ScenarioID: "scenario:issue-101/crit-b"}}})
	if len(got) != 2 || got[0] != "AC1" || got[1] != "scenario:issue-101/crit-b" {
		t.Fatalf("got %v", got)
	}
	if !replaySetHasCorpus(scenario.ReplaySet{RetiredExcluded: 1}) || replaySetHasCorpus(scenario.ReplaySet{Cap: 25}) {
		t.Fatal("replaySetHasCorpus: retired-excluded counts as a corpus, cap alone does not")
	}
}

// --- run()-level: the deferred drop reporter on every exit path -----------

// acceptanceReplayStageSetup extends acceptanceStageSetup with a served
// retirement, a corpus committed on main + a run branch the acceptance tree
// is provisioned from, the run-branch push target (bare origin) and the
// prompt-served issue/PR numbers.
func acceptanceReplayStageSetup(t *testing.T) (repo, origin string, fu *fakeUploader, args []string) {
	t.Helper()
	var runArgs []string
	repo, fu, runArgs = acceptanceStageSetup(t)
	origin = filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}
	tgit(t, repo, "push", "-q", origin, "main:main")
	tgit(t, repo, "checkout", "-q", "-b", "fishhawk/run-x")
	corpus := filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), "issue-7")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(corpus, "crit-c.yaml"), scenarioYAML("scenario:issue-7/crit-c", 7, 700, "r0", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "run branch corpus")
	tgit(t, repo, "push", "-q", origin, "fishhawk/run-x:fishhawk/run-x")
	head := gitHead(t, repo)
	fu.promptResp.AcceptanceExpectedHeadSHA = head
	fu.promptResp.AcceptanceRunBranch = "fishhawk/run-x"
	fu.promptResp.AcceptancePullRequestNumber = 742
	fu.promptResp.AcceptanceIssueNumber = 101
	fu.promptResp.AcceptanceCriteria = []upload.AcceptanceCriterionEntry{{ID: "AC1", Statement: "AC1 statement", Drivable: true}, {ID: "AC2", Statement: "AC2", Drivable: true}}
	fu.promptResp.AcceptanceRetiredScenarios = []scenario.RetiredEntry{{ID: "scenario:issue-7/crit-c", Reason: "behaviour replaced by #3327", RunID: acceptanceTestRunID, PR: 742, RetiredAt: "2026-09-12T00:00:00Z"}}
	origTreeDir := acceptanceTreeDir
	acceptanceTreeDir = t.TempDir()
	origRemote := acceptancePersistRemoteURL
	acceptancePersistRemoteURL = func(config) string { return origin }
	t.Cleanup(func() { acceptanceTreeDir = origTreeDir; acceptancePersistRemoteURL = origRemote })
	return repo, origin, fu, runArgs
}

func dropReport(fu *fakeUploader) *upload.ShipPullRequestArgs {
	for i := range fu.gotPRCalls {
		if fu.gotPRCalls[i].Outcome == upload.OutcomeAcceptanceScenarioRetirementDropped {
			return &fu.gotPRCalls[i]
		}
	}
	return nil
}

func pushedReport(fu *fakeUploader) *upload.ShipPullRequestArgs {
	for i := range fu.gotPRCalls {
		if fu.gotPRCalls[i].Outcome == upload.OutcomeAcceptanceScenariosPushed {
			return &fu.gotPRCalls[i]
		}
	}
	return nil
}

const okVerdict = `{"verdict":"passed","criteria":[{"id":"AC1","result":"passed","observed":"200","expected":"200","steps_taken":"GET /"},{"id":"AC2","result":"skipped"}]}`

// TestReportRetirementDrop_TargetGateFailsPreSpawn (binding condition 1): the
// reporter is armed BEFORE the target gate and provisionAcceptanceTree, so a
// stale-target category-C exit — before any provisioning — still ships the
// drop with the served entry and its reason.
func TestReportRetirementDrop_TargetGateFailsPreSpawn(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	healthz, _ := healthzServer(t, 200, `{"git_sha":"1234567"}`)
	fu.promptResp.EgressTargetHosts = []string{hostOf(healthz)}
	invoker := &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}}
	withFakeInvoker(t, invoker)
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if invoker.callIdx != 0 || !strings.Contains(stderr.String(), `"reason":"acceptance_target_stale"`) {
		t.Fatalf("expected a pre-spawn stale-target failure:\n%s", stderr.String())
	}
	dr := dropReport(fu)
	if dr == nil || dr.Reason != retirementDropDefaultReason || len(dr.RetiredScenarios) != 1 || dr.RetiredScenarios[0].Reason != "behaviour replaced by #3327" {
		t.Fatalf("drop report = %+v, want the served entry with reason %q\n%s", dr, retirementDropDefaultReason, stderr.String())
	}
	// The arm precedes provisioning in the log.
	out := stderr.String()
	if armed := strings.Index(out, "acceptance_scenario_retirement_reporter_armed"); armed < 0 || armed > strings.Index(out, "acceptance_target_stale") {
		t.Errorf("reporter must be armed before the target gate:\n%s", out)
	}
}

// TestReportRetirementDrop_ProvisionFails: an unprovisionable merge-candidate
// tree (a SHA the dispatch checkout cannot resolve) degrades provisioning;
// with retirements in hand the persist step finds no tree and the drop IS
// reported (binding condition 1).
func TestReportRetirementDrop_ProvisionFails(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	fu.promptResp.AcceptanceExpectedHeadSHA = "feedf00dfeedf00dfeedf00dfeedf00dfeedf00d"
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"acceptance_tree_failed"`) {
		t.Fatalf("provisioning must have failed:\n%s", stderr.String())
	}
	dr := dropReport(fu)
	if dr == nil || dr.Reason != "persist_skipped:no_acceptance_tree" {
		t.Fatalf("drop report = %+v, want persist_skipped:no_acceptance_tree\n%s", dr, stderr.String())
	}
}

// TestReportRetirementDrop_GuardCategoryB: a scenario deleted on the run
// branch without a ledger entry fails category-B pre-spawn and the drop
// report names the guard.
func TestReportRetirementDrop_GuardCategoryB(t *testing.T) {
	repo, origin, fu, args := acceptanceReplayStageSetup(t)
	// crit-d lands on MAIN (so it exists at the merge base), the run branch
	// merges main and then deletes it without a ledger entry.
	tgit(t, repo, "checkout", "-q", "main")
	corpus := filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), "issue-7")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(corpus, "crit-d.yaml"), scenarioYAML("scenario:issue-7/crit-d", 7, 700, "r0", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "add crit-d on main")
	tgit(t, repo, "push", "-q", origin, "main:main")
	tgit(t, repo, "checkout", "-q", "fishhawk/run-x")
	tgit(t, repo, "merge", "-q", "--no-edit", "main")
	tgit(t, repo, "rm", "-q", "acceptance/scenarios/issue-7/crit-d.yaml")
	tgit(t, repo, "commit", "-q", "-m", "delete crit-d without retirement")
	fu.promptResp.AcceptanceExpectedHeadSHA = gitHead(t, repo)
	invoker := &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}}
	withFakeInvoker(t, invoker)
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if invoker.callIdx != 0 || !strings.Contains(stderr.String(), `"reason":"acceptance_scenario_removed_without_retirement","category":"B"`) {
		t.Fatalf("expected pre-spawn category-B guard failure:\n%s", stderr.String())
	}
	if dr := dropReport(fu); dr == nil || dr.Reason != guardRemovedWithoutRetirement {
		t.Fatalf("drop report = %+v, want reason %s", dr, guardRemovedWithoutRetirement)
	}
}

// TestReportRetirementDrop_VerdictInvalid: an invalid verdict (category-B)
// never reaches persist; the drop names acceptance_verdict_invalid.
func TestReportRetirementDrop_VerdictInvalid(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(`{"verdict":"maybe"}`)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr == nil || dr.Reason != "acceptance_verdict_invalid" {
		t.Fatalf("drop report = %+v, want acceptance_verdict_invalid\n%s", dr, stderr.String())
	}
}

// TestReportRetirementDrop_NoRunBranch: no acceptance_run_branch served →
// persist_skipped no_run_branch → drop reported with that reason.
func TestReportRetirementDrop_NoRunBranch(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	fu.promptResp.AcceptanceRunBranch = ""
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr == nil || dr.Reason != "persist_skipped:no_run_branch" {
		t.Fatalf("drop report = %+v, want persist_skipped:no_run_branch\n%s", dr, stderr.String())
	}
	if pushedReport(fu) != nil {
		t.Error("nothing must be pushed without a run branch")
	}
}

// TestReportRetirementDrop_PersistRefused: a stray file planted in the
// provisioned tree (via the fake agent, which runs after provisioning) makes
// the commit persist_refused; the drop names it and nothing is pushed.
func TestReportRetirementDrop_PersistRefused(t *testing.T) {
	_, origin, fu, args := acceptanceReplayStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)},
		onInvoke: func(int, agent.Invocation) {
			mustWrite(t, filepath.Join(acceptanceTreePath(acceptanceTestRunID, acceptanceTestStageID), "stray.txt"), "planted\n")
		}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr == nil || !strings.HasPrefix(dr.Reason, "persist_refused:dirty_outside_corpus") {
		t.Fatalf("drop report = %+v, want persist_refused\n%s", dr, stderr.String())
	}
	if pushedReport(fu) != nil || tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-x") != fu.promptResp.AcceptanceExpectedHeadSHA {
		t.Error("a refused persist must push nothing")
	}
}

// TestReportRetirementDrop_PushFailure: the run branch on origin moved past
// the merge candidate, so the pinned push is non-fast-forward and fails; the
// drop names the push failure.
func TestReportRetirementDrop_PushFailure(t *testing.T) {
	repo, origin, fu, args := acceptanceReplayStageSetup(t)
	tgit(t, repo, "commit", "-q", "--allow-empty", "-m", "branch advanced after dispatch")
	tgit(t, repo, "push", "-q", origin, "fishhawk/run-x:fishhawk/run-x")
	tgit(t, repo, "reset", "-q", "--hard", "HEAD~1") // dispatch checkout stays at the merge candidate
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (best-effort):\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr == nil || !strings.HasPrefix(dr.Reason, "persist_failed:push: ") {
		t.Fatalf("drop report = %+v, want persist_failed:push\n%s", dr, stderr.String())
	}
}

// TestReportRetirementDrop_NotShippedAfterPush: a successful
// acceptance_scenarios_pushed report disarms the reporter — no drop report,
// and the pushed report carries the scenario id + the full retirement.
func TestReportRetirementDrop_NotShippedAfterPush(t *testing.T) {
	_, origin, fu, args := acceptanceReplayStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr != nil {
		t.Fatalf("drop must NOT be reported after a successful push, got %+v\n%s", dr, stderr.String())
	}
	pr := pushedReport(fu)
	if pr == nil || pr.Branch != "fishhawk/run-x" || len(pr.ScenarioIDs) != 1 || pr.ScenarioIDs[0] != "scenario:issue-101/AC1" ||
		len(pr.RetiredScenarios) != 1 || pr.RetiredScenarios[0].Reason != "behaviour replaced by #3327" || pr.HeadSHA == "" || pr.BaseSHA != fu.promptResp.AcceptanceExpectedHeadSHA {
		t.Fatalf("pushed report = %+v\n%s", pr, stderr.String())
	}
	if tgit(t, origin, "rev-parse", "refs/heads/fishhawk/run-x") != pr.HeadSHA {
		t.Error("origin run branch must be the reported head")
	}
	if !strings.Contains(stderr.String(), `"event":"acceptance_scenario_retirement_persisted"`) || !strings.Contains(stderr.String(), `"how":"pushed_and_reported"`) {
		t.Errorf("missing persisted/disarm trail:\n%s", stderr.String())
	}
}

// TestReportRetirementDrop_PushReportFails: the commit pushed but the
// acceptance_scenarios_pushed report failed → the reporter stays armed and
// names push_report_failed (disarm is bound to the REPORT, not the push).
func TestReportRetirementDrop_PushReportFails(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	fu.prErrSeq = []error{errors.New("backend 503"), nil}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if dr := dropReport(fu); dr == nil || !strings.HasPrefix(dr.Reason, "push_report_failed") {
		t.Fatalf("drop report = %+v, want push_report_failed\n%s", dr, stderr.String())
	}
}

// TestAcceptanceStage_ServedIDsUnionAdmitsScenarioRows: the served-id set the
// verdict is validated against is criteria ∪ replayed scenario ids, so a
// scenario row validates and ships (with the injected replay object).
func TestAcceptanceStage_ServedIDsUnionAdmitsScenarioRows(t *testing.T) {
	_, _, fu, args := acceptanceReplayStageSetup(t)
	fu.promptResp.AcceptanceRetiredScenarios = nil // crit-c is served for replay
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(
		`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"},{"id":"AC2","result":"passed"},{"id":"scenario:issue-7/crit-c","result":"passed","observed":"200","expected":"200"}]}`)}})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil || !strings.Contains(string(fu.gotAcceptanceArgs.Body), `"replay":{"cap":25,"corpus_size":1,"served":1,"sampled_out":0,"retired_excluded":0,"seed":"`+acceptanceTestRunID+`","scenarios":[{"scenario_id":"scenario:issue-7/crit-c","origin_pr":700,"origin_issue":7,"origin_run_id":"r0","path":"acceptance/scenarios/issue-7/crit-c.yaml"}]}`) {
		t.Fatalf("shipped body lacks the injected replay set: %s\n%s", fu.gotAcceptanceArgs, stderr.String())
	}
}

// TestRun_AcceptanceStage_ReplayCapBoundsServed: 8 scenarios, cap 5 → exactly
// 5 listed in the prompt file and the shipped replay header is
// {cap 5, corpus_size 8, served 5, sampled_out 3}.
func TestRun_AcceptanceStage_ReplayCapBoundsServed(t *testing.T) {
	repo, origin, fu, args := acceptanceReplayStageSetup(t)
	fu.promptResp.AcceptanceRetiredScenarios = nil
	corpus := filepath.Join(repo, filepath.FromSlash(scenario.CorpusDir), "issue-9")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		mustWrite(t, filepath.Join(corpus, fmt.Sprintf("c%d.yaml", i)), scenarioYAML(fmt.Sprintf("scenario:issue-9/c%d", i), 9, 900, "r0", time.Date(2021, 1, 1+i, 0, 0, 0, 0, time.UTC)))
	}
	tgit(t, repo, "add", "-A")
	tgit(t, repo, "commit", "-q", "-m", "more corpus")
	tgit(t, repo, "push", "-q", origin, "fishhawk/run-x:fishhawk/run-x")
	fu.promptResp.AcceptanceExpectedHeadSHA = gitHead(t, repo)
	t.Setenv("FISHHAWK_ACCEPTANCE_REPLAY_MAX_SCENARIOS", "5")
	var promptText string
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true, StructuredOutput: []byte(okVerdict)},
		onInvoke: func(_ int, inv agent.Invocation) { promptText = inv.Prompt }})
	var stderr strings.Builder
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if n := strings.Count(promptText, "- scenario: scenario:"); n != 5 {
		t.Errorf("prompt lists %d scenarios, want 5:\n%s", n, promptText)
	}
	if !strings.Contains(string(fu.gotAcceptanceArgs.Body), `"replay":{"cap":5,"corpus_size":8,"served":5,"sampled_out":3,`) {
		t.Errorf("shipped replay header wrong: %s", fu.gotAcceptanceArgs.Body)
	}
}
