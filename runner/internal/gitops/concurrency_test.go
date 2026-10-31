package gitops

import (
	"bytes"
	"context"
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
