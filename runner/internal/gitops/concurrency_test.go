package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestCommitAndPush_ConcurrentSiblingWorktrees is the ADR-115 D2 (#4648)
// cross-layer test: two CommitAndPush calls run CONCURRENTLY in two linked
// sibling worktrees of one repository, each on the unpinned FreshFetchBase arm
// (fetch -> snapshot -> checkout -B -> restore -> commit -> push), with
// distinct branches, snapshot keys, identities, tracked edits and untracked
// files. The primary repository carries NO user identity, an operator stash
// entry is seeded first, and the shared .git/config bytes are captured.
//
// It asserts each pushed branch carries its OWN author and committer and its
// OWN edits (untracked file included) and nothing of the sibling's, that the
// shared .git/config is byte-identical afterwards (no `git config user.*`
// leaked into $GIT_COMMON_DIR/config), that refs/stash still holds exactly the
// operator's entry, and that no refs/fishhawk/stash/ ref is left behind.
func TestCommitAndPush_ConcurrentSiblingWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	primary := filepath.Join(dir, "primary")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(primary, 0o755); err != nil {
		t.Fatal(err)
	}
	ident := []string{"-c", "user.name=seed", "-c", "user.email=seed@example.com", "-c", "commit.gpgsign=false"}
	mustGit(t, primary, "init", "-q", "--initial-branch=main")
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(primary, f), []byte("base "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, primary, "add", "-A")
	mustGit(t, primary, append(ident, "commit", "-q", "-m", "base")...)
	mustGit(t, primary, "init", "-q", "--bare", "--initial-branch=main", bare)
	mustGit(t, primary, "push", "-q", bare, "main")

	// The operator's own stash entry, on the shared stack before either stage.
	if err := os.WriteFile(filepath.Join(primary, "a.txt"), []byte("operator wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, primary, append(ident, "stash", "push", "-q", "-m", "operator wip")...)
	operatorStash := mustGitOut(t, primary, "rev-parse", "refs/stash")

	type side struct {
		name, wt, branch, key, author, email, tracked, untracked string
	}
	sides := []side{
		{name: "a", branch: "fishhawk/run-4648/slice-a", key: "stage-a", author: "Stage A", email: "a@example.com", tracked: "a.txt", untracked: "a-new.txt"},
		{name: "b", branch: "fishhawk/run-4648/slice-b", key: "stage-b", author: "Stage B", email: "b@example.com", tracked: "b.txt", untracked: "b-new.txt"},
	}
	for i := range sides {
		sides[i].wt = filepath.Join(dir, "wt-"+sides[i].name)
		mustGit(t, primary, "worktree", "add", "-q", "--detach", sides[i].wt, "main")
		if err := os.WriteFile(filepath.Join(sides[i].wt, sides[i].tracked), []byte(sides[i].name+" edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sides[i].wt, sides[i].untracked), []byte(sides[i].name+" new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(primary, ".git", "config")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make([]error, len(sides))
	var wg sync.WaitGroup
	for i, s := range sides {
		wg.Add(1)
		go func(i int, s side) {
			defer wg.Done()
			<-start
			_, errs[i] = (&Pusher{}).CommitAndPush(context.Background(), CommitAndPushArgs{
				RepoDir:        s.wt,
				Branch:         s.branch,
				CommitMessage:  "slice " + s.name,
				AuthorName:     s.author,
				AuthorEmail:    s.email,
				RemoteURL:      bare,
				FreshFetchBase: "main",
				SnapshotKey:    s.key,
			})
		}(i, s)
	}
	close(start)
	wg.Wait()

	for i, s := range sides {
		if errs[i] != nil {
			t.Fatalf("CommitAndPush(%s): %v", s.name, errs[i])
		}
		other := sides[1-i]
		ref := "refs/heads/" + s.branch
		if got := mustGitOut(t, bare, "log", "-1", "--format=%an <%ae> | %cn <%ce>", ref); got != s.author+" <"+s.email+"> | "+s.author+" <"+s.email+">" {
			t.Errorf("%s commit identity = %q, want its own author AND committer %s <%s>", s.name, got, s.author, s.email)
		}
		if got := mustGitOut(t, bare, "show", ref+":"+s.tracked); got != s.name+" edit" {
			t.Errorf("%s pushed %s = %q, want its own edit", s.name, s.tracked, got)
		}
		if got := mustGitOut(t, bare, "show", ref+":"+s.untracked); got != s.name+" new" {
			t.Errorf("%s pushed %s = %q, want its own untracked file", s.name, s.untracked, got)
		}
		if got := mustGitOut(t, bare, "show", ref+":"+other.tracked); got != "base "+other.tracked {
			t.Errorf("%s pushed %s = %q, want the base content (none of the sibling's edit)", s.name, other.tracked, got)
		}
		if files := mustGitOut(t, bare, "ls-tree", "-r", "--name-only", ref); strings.Contains(files, other.untracked) {
			t.Errorf("%s pushed tree carries the sibling's untracked %s: %q", s.name, other.untracked, files)
		}
	}

	configAfter, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Errorf("shared .git/config changed under concurrent CommitAndPush (identity must ride per command):\nbefore:\n%s\nafter:\n%s", configBefore, configAfter)
	}
	if got := mustGitOut(t, primary, "rev-parse", "refs/stash"); got != operatorStash {
		t.Errorf("refs/stash = %s, want the operator's entry %s", got, operatorStash)
	}
	if n := mustGitOut(t, primary, "rev-list", "--walk-reflogs", "--count", "refs/stash"); n != "1" {
		t.Errorf("stash stack holds %s entries, want exactly the operator's 1", n)
	}
	assertNoSnapshotRefs(t, primary)
}

// trackingRaceRepo is a client clone whose remote-tracking ref
// refs/remotes/origin/<branch> is STALE (at oldSHA) while the bare origin's
// <branch> holds newSHA, a commit the client does not have. raceSHA is a
// local-only commit from commit-tree: the race injected by installRaceHook
// moves the tracking ref to it mid-fetch (#4835).
type trackingRaceRepo struct {
	dir, seed, bare, client, branch string
	oldSHA, newSHA, raceSHA         string
	marker                          string
}

// newTrackingRaceRepo builds the fixture. The remote branch MUST hold a commit
// the client lacks: otherwise the fetch has nothing to transfer, upload-pack
// never runs pack-objects, and the injected hook never fires.
func newTrackingRaceRepo(t *testing.T) *trackingRaceRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	f := &trackingRaceRepo{dir: t.TempDir(), branch: "feat"}
	f.seed = filepath.Join(f.dir, "seed")
	f.bare = filepath.Join(f.dir, "origin.git")
	f.client = filepath.Join(f.dir, "client")
	f.marker = filepath.Join(f.dir, "hook-fired")
	if err := os.Mkdir(f.seed, 0o755); err != nil {
		t.Fatal(err)
	}
	ident := []string{"-c", "user.name=seed", "-c", "user.email=seed@example.com", "-c", "commit.gpgsign=false"}
	commit := func(msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.seed, "f.txt"), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, f.seed, "add", "-A")
		mustGit(t, f.seed, append(ident, "commit", "-q", "-m", msg)...)
	}
	mustGit(t, f.seed, "init", "-q", "--initial-branch=main")
	commit("base")
	mustGit(t, f.seed, "checkout", "-q", "-b", f.branch)
	commit("old")
	f.oldSHA = mustGitOut(t, f.seed, "rev-parse", "HEAD")
	mustGit(t, f.seed, "init", "-q", "--bare", "--initial-branch=main", f.bare)
	mustGit(t, f.seed, "push", "-q", f.bare, "main", f.branch)
	mustGit(t, f.dir, "clone", "-q", f.bare, f.client)

	// The remote advances; the client's tracking ref stays at oldSHA.
	commit("new")
	f.newSHA = mustGitOut(t, f.seed, "rev-parse", "HEAD")
	mustGit(t, f.seed, "push", "-q", f.bare, f.branch)

	// A commit that exists only in the client (never on the remote).
	f.raceSHA = mustGitOut(t, f.client, append(ident, "commit-tree", f.oldSHA+"^{tree}", "-p", f.oldSHA, "-m", "racer")...)
	if got := mustGitOut(t, f.client, "rev-parse", f.trackingRef()); got != f.oldSHA {
		t.Fatalf("fixture tracking ref = %s, want the stale %s", got, f.oldSHA)
	}
	return f
}

func (f *trackingRaceRepo) trackingRef() string { return "refs/remotes/origin/" + f.branch }

// installRaceHook makes every upload-pack in this test move the client's
// tracking ref to raceSHA after the client read the ref's old value and before
// it stores refs, which is exactly the window a concurrent operator fetch hits.
// It rides uploadpack.packObjectsHook, which git honours from GIT_CONFIG_GLOBAL
// (not from repository config) on any git version; a reference-transaction
// hook did not reproduce the race on git 2.39.5.
//
// The override keeps maintenance.auto=false (the runTestMain pin this env var
// would otherwise replace, #3503) and carries no signing or credential keys.
// It is installed AFTER the fixture's own clone and pushes, so none of those
// runs under a hook-only config or fires the hook. t.Setenv forbids
// t.Parallel on callers.
func (f *trackingRaceRepo) installRaceHook(t *testing.T) {
	t.Helper()
	script := filepath.Join(f.dir, "race-hook.sh")
	body := fmt.Sprintf("#!/bin/sh\necho fired >> '%s'\nGIT_DIR='%s' git update-ref '%s' '%s'\nexec \"$@\"\n",
		f.marker, filepath.Join(f.client, ".git"), f.trackingRef(), f.raceSHA)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(f.dir, "race-gitconfig")
	cfgBody := fmt.Sprintf("[maintenance]\n\tauto = false\n[uploadpack]\n\tpackObjectsHook = %s\n", script)
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

// hookFired reports whether the race hook ran (the race really fired).
func (f *trackingRaceRepo) hookFired(t *testing.T) bool {
	t.Helper()
	b, err := os.ReadFile(f.marker)
	return err == nil && len(bytes.TrimSpace(b)) > 0
}

func assertNoPrivateFetchRefs(t *testing.T, repo string) {
	t.Helper()
	if refs := mustGitOut(t, repo, "for-each-ref", "--format=%(refname)", privateFetchRefPrefix); refs != "" {
		t.Errorf("private fetch refs left behind: %q", refs)
	}
}

// refSnapshot maps every ref in repo to its object name.
func refSnapshot(t *testing.T, repo string) map[string]string {
	t.Helper()
	out := mustGitOut(t, repo, "for-each-ref", "--format=%(refname) %(objectname)")
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			m[name] = sha
		}
	}
	return m
}

// TestFetchRemoteBranchTip_SurvivesTrackingRefRace is the #4835 regression: a
// concurrent writer moves refs/remotes/origin/<branch> between the fetch's read
// of its old value and its store. The pre-fix fetch named that shared ref and
// died with `cannot lock ref ... is at X but expected Y`; the private-ref fetch
// never names it, so every caller of the shared prologue survives and still
// leaves the tracking ref at the fetched tip.
func TestFetchRemoteBranchTip_SurvivesTrackingRefRace(t *testing.T) {
	cases := []struct {
		name     string
		run      func(f *trackingRaceRepo) (string, error)
		checkout bool
	}{
		{"FetchBaseTip", func(f *trackingRaceRepo) (string, error) {
			return FetchBaseTip(context.Background(), f.client, "origin", f.branch, "")
		}, false},
		{"CheckoutRemoteBranch", func(f *trackingRaceRepo) (string, error) {
			return CheckoutRemoteBranch(context.Background(), f.client, "origin", f.branch, "", nil)
		}, true},
		{"CheckoutRemoteBranchDetached", func(f *trackingRaceRepo) (string, error) {
			return CheckoutRemoteBranchDetached(context.Background(), f.client, "origin", f.branch, "")
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTrackingRaceRepo(t)
			f.installRaceHook(t)

			tip, err := tc.run(f)
			if err != nil {
				t.Fatalf("%s lost the tracking-ref race: %v", tc.name, err)
			}
			if !f.hookFired(t) {
				t.Fatal("race hook never fired: the fixture did not exercise the race")
			}
			if tip != f.newSHA {
				t.Errorf("returned tip = %s, want the remote tip %s", tip, f.newSHA)
			}
			if got := mustGitOut(t, f.client, "rev-parse", f.trackingRef()); got != f.newSHA {
				t.Errorf("tracking ref = %s, want the remote tip %s (not the racer %s)", got, f.newSHA, f.raceSHA)
			}
			if tc.checkout {
				if got := mustGitOut(t, f.client, "rev-parse", "HEAD"); got != tip {
					t.Errorf("HEAD = %s, want the tip %s", got, tip)
				}
			}
			assertNoPrivateFetchRefs(t, f.client)
		})
	}
}

// TestFetchBaseTip_WritesOnlyTheTrackingRef pins the private-ref fetch's
// footprint: with a remote tag at the fetched tip, the only ref the call may
// change in the client is the tracking ref (--no-tags keeps the auto-followed
// tag out, the deferred drop keeps the private ref out).
func TestFetchBaseTip_WritesOnlyTheTrackingRef(t *testing.T) {
	f := newTrackingRaceRepo(t)
	mustGit(t, f.seed, "tag", "v1", f.newSHA)
	mustGit(t, f.seed, "push", "-q", f.bare, "v1")
	before := refSnapshot(t, f.client)

	tip, err := FetchBaseTip(context.Background(), f.client, "origin", f.branch, "")
	if err != nil {
		t.Fatalf("FetchBaseTip: %v", err)
	}
	if tip != f.newSHA {
		t.Fatalf("tip = %s, want %s", tip, f.newSHA)
	}
	after := refSnapshot(t, f.client)
	for name, sha := range after {
		old, existed := before[name]
		switch {
		case name == f.trackingRef():
			if sha != f.newSHA {
				t.Errorf("tracking ref = %s, want %s", sha, f.newSHA)
			}
		case !existed:
			t.Errorf("ref %s (%s) appeared; the fetch may write only the tracking ref", name, sha)
		case old != sha:
			t.Errorf("ref %s moved %s -> %s; the fetch may write only the tracking ref", name, old, sha)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("ref %s disappeared", name)
		}
	}
}

// TestFetchBaseTip_PublishFailureIsReturned pins that a failed publish is a
// real failure, not swallowed: a directory/file conflict makes the update-ref
// of the tracking ref fail AFTER a successful private fetch, and the private
// ref is still dropped.
func TestFetchBaseTip_PublishFailureIsReturned(t *testing.T) {
	f := newTrackingRaceRepo(t)
	mustGit(t, f.client, "update-ref", "-d", f.trackingRef())
	mustGit(t, f.client, "update-ref", f.trackingRef()+"/sub", f.oldSHA)

	tip, err := FetchBaseTip(context.Background(), f.client, "origin", f.branch, "")
	if err == nil {
		t.Fatalf("FetchBaseTip = %s, nil; want the publish failure", tip)
	}
	if !strings.Contains(err.Error(), "update-ref") {
		t.Errorf("error = %v, want it to name update-ref", err)
	}
	if tip != "" {
		t.Errorf("tip = %q on failure, want empty", tip)
	}
	assertNoPrivateFetchRefs(t, f.client)
}

// TestFetchRemoteBranchTip_CancelLeavesNoPrivateRef pins the cleanup contract:
// a stage cancelled after the fetch created the private ref still deletes it,
// because the drop runs on a context detached from the caller's cancellation.
func TestFetchRemoteBranchTip_CancelLeavesNoPrivateRef(t *testing.T) {
	f := newTrackingRaceRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &Pusher{Cmd: func(c context.Context, name string, args ...string) *exec.Cmd {
		for _, a := range args {
			if a == "rev-parse" {
				cancel() // the fetch has already stored the private ref
			}
		}
		return exec.CommandContext(c, name, args...)
	}}

	_, _, err := fetchRemoteBranchTip(ctx, p, f.client, "origin", f.branch, nil)
	if err == nil {
		t.Fatal("fetchRemoteBranchTip succeeded under a cancelled context; want an error")
	}
	assertNoPrivateFetchRefs(t, f.client)
}
