package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// snapIdentArgs is the identity a fixture's OWN setup commits use. The
// fixture repositories carry no user.name/user.email and set
// user.useConfigOnly=true, so git cannot guess an identity: a snapshot commit
// succeeds only because SnapshotWorkTree carries IdentityEnv itself.
var snapIdentArgs = []string{"-c", "user.name=init", "-c", "user.email=init@example.com"}

func snapCommitAll(t *testing.T, repo, msg string) {
	t.Helper()
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, append(append([]string{}, snapIdentArgs...), "commit", "-q", "-m", msg)...)
}

// newSnapshotRepo returns a repository with README.md, a.txt, b.txt, f.txt
// and a .gitignore (ignoring ignored.txt) committed, and no local identity.
func newSnapshotRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q", "--initial-branch=main")
	mustGit(t, repo, "config", "user.useConfigOnly", "true")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	for name, body := range map[string]string{
		"README.md":  "# initial\n",
		"a.txt":      "a\n",
		"b.txt":      "b\n",
		"f.txt":      "f\n",
		".gitignore": "ignored.txt\n",
	} {
		snapWriteFile(t, repo, name, body)
	}
	snapCommitAll(t, repo, "initial")
	return repo
}

func snapWriteFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapReadFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func snapFileExists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// snapRefSHA resolves ref, returning "" when it does not exist.
func snapRefSHA(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestSnapshot_UntrackedSurvivesIgnoredExcluded(t *testing.T) {
	repo := newSnapshotRepo(t)
	ctx := context.Background()
	p := &Pusher{}

	snapWriteFile(t, repo, "README.md", "# agent edit\n")
	snapWriteFile(t, repo, "new.txt", "agent new file\n")
	snapWriteFile(t, repo, "ignored.txt", "ignored content\n")

	snap, created, err := p.SnapshotWorkTree(ctx, repo, "stage-1", nil)
	if err != nil || !created {
		t.Fatalf("SnapshotWorkTree = (%+v, %v, %v), want a created snapshot", snap, created, err)
	}
	if snap.Ref != SnapshotRef("stage-1") || snap.Ref != "refs/fishhawk/stash/stage-1" {
		t.Errorf("snapshot ref = %q, want refs/fishhawk/stash/stage-1", snap.Ref)
	}
	if got := snapRefSHA(t, repo, snap.Ref); got != snap.SHA {
		t.Errorf("%s = %q, want the snapshot SHA %q", snap.Ref, got, snap.SHA)
	}
	if got := snapRefSHA(t, repo, "refs/stash"); got != "" {
		t.Errorf("refs/stash = %q after SnapshotWorkTree, want untouched (absent)", got)
	}
	if got := mustGitOut(t, repo, "log", "-1", "--format=%an|%ae|%cn|%ce", snap.SHA); got != DefaultAuthorName+"|"+DefaultAuthorEmail+"|"+DefaultAuthorName+"|"+DefaultAuthorEmail {
		t.Errorf("snapshot identity = %q, want the default runner identity", got)
	}

	// Reverted: clean except the ignored file, which is byte-identical.
	if status := mustGitOut(t, repo, "status", "--porcelain"); status != "" {
		t.Errorf("status after snapshot = %q, want clean", status)
	}
	if got := snapReadFile(t, repo, "README.md"); got != "# initial\n" {
		t.Errorf("README.md after snapshot = %q, want HEAD content", got)
	}
	if snapFileExists(repo, "new.txt") {
		t.Error("untracked new.txt still present after snapshot, want cleaned")
	}
	if got := snapReadFile(t, repo, "ignored.txt"); got != "ignored content\n" {
		t.Errorf("ignored.txt after snapshot = %q, want untouched", got)
	}

	// U carries the untracked file and not the ignored one.
	uFiles := mustGitOut(t, repo, "ls-tree", "-r", "--name-only", snap.SHA+"^3")
	if uFiles != "new.txt" {
		t.Errorf("snapshot untracked tree = %q, want exactly new.txt (ignored.txt excluded)", uFiles)
	}

	if err := p.RestoreSnapshot(ctx, repo, snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if got := snapReadFile(t, repo, "README.md"); got != "# agent edit\n" {
		t.Errorf("README.md after restore = %q, want the agent edit", got)
	}
	if staged := mustGitOut(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("staged after restore = %q, want the edit unstaged", staged)
	}
	if got := mustGitOut(t, repo, "diff", "--name-only"); got != "README.md" {
		t.Errorf("unstaged after restore = %q, want README.md", got)
	}
	if !snapFileExists(repo, "new.txt") || snapReadFile(t, repo, "new.txt") != "agent new file\n" {
		t.Error("untracked new.txt not restored with its content")
	}
	if got := snapReadFile(t, repo, "ignored.txt"); got != "ignored content\n" {
		t.Errorf("ignored.txt after restore = %q, want untouched", got)
	}
}

func TestSnapshot_SiblingWorktreesRestoreOwnSnapshot(t *testing.T) {
	repo := newSnapshotRepo(t)
	ctx := context.Background()
	p := &Pusher{}

	// The operator's own stash entry, seeded first on the shared stack.
	snapWriteFile(t, repo, "README.md", "# operator wip\n")
	mustGit(t, repo, append(append([]string{}, snapIdentArgs...), "stash", "push", "-q", "-m", "operator")...)
	operatorStash := snapRefSHA(t, repo, "refs/stash")
	if operatorStash == "" {
		t.Fatal("seeding the operator stash entry failed")
	}

	wtA := filepath.Join(t.TempDir(), "A")
	wtB := filepath.Join(t.TempDir(), "B")
	mustGit(t, repo, "worktree", "add", "-q", "--detach", wtA, "HEAD")
	mustGit(t, repo, "worktree", "add", "-q", "--detach", wtB, "HEAD")

	snapWriteFile(t, wtA, "a.txt", "a from A\n")
	snapWriteFile(t, wtA, "a-new.txt", "A untracked\n")
	snapWriteFile(t, wtB, "b.txt", "b from B\n")
	snapWriteFile(t, wtB, "b-new.txt", "B untracked\n")

	snapA, createdA, err := p.SnapshotWorkTree(ctx, wtA, "stage-A", nil)
	if err != nil || !createdA {
		t.Fatalf("snapshot A = (%+v, %v, %v)", snapA, createdA, err)
	}
	snapB, createdB, err := p.SnapshotWorkTree(ctx, wtB, "stage-B", nil)
	if err != nil || !createdB {
		t.Fatalf("snapshot B = (%+v, %v, %v)", snapB, createdB, err)
	}
	if err := p.RestoreSnapshot(ctx, wtA, snapA); err != nil {
		t.Fatalf("restore A: %v", err)
	}
	if err := p.RestoreSnapshot(ctx, wtB, snapB); err != nil {
		t.Fatalf("restore B: %v", err)
	}

	for _, tc := range []struct {
		dir, edited, editBody, untracked, untrackedBody, otherEdited, otherUntracked string
	}{
		{wtA, "a.txt", "a from A\n", "a-new.txt", "A untracked\n", "b.txt", "b-new.txt"},
		{wtB, "b.txt", "b from B\n", "b-new.txt", "B untracked\n", "a.txt", "a-new.txt"},
	} {
		if got := snapReadFile(t, tc.dir, tc.edited); got != tc.editBody {
			t.Errorf("%s/%s = %q, want its own edit %q", filepath.Base(tc.dir), tc.edited, got, tc.editBody)
		}
		if !snapFileExists(tc.dir, tc.untracked) || snapReadFile(t, tc.dir, tc.untracked) != tc.untrackedBody {
			t.Errorf("%s/%s not restored with its own content", filepath.Base(tc.dir), tc.untracked)
		}
		if got := snapReadFile(t, tc.dir, "README.md"); got != "# initial\n" {
			t.Errorf("%s/README.md = %q: the operator's stash entry was applied", filepath.Base(tc.dir), got)
		}
		if got := mustGitOut(t, tc.dir, "diff", "--name-only", "--", tc.otherEdited); got != "" {
			t.Errorf("%s carries the sibling's edit to %s", filepath.Base(tc.dir), tc.otherEdited)
		}
		if snapFileExists(tc.dir, tc.otherUntracked) {
			t.Errorf("%s carries the sibling's untracked %s", filepath.Base(tc.dir), tc.otherUntracked)
		}
	}

	if got := snapRefSHA(t, repo, "refs/stash"); got != operatorStash {
		t.Errorf("refs/stash = %q, want the operator entry %q", got, operatorStash)
	}
	if n := len(strings.Split(mustGitOut(t, repo, "stash", "list"), "\n")); n != 1 {
		t.Errorf("stash list has %d entries, want exactly the operator's one", n)
	}
	refA, refB := snapRefSHA(t, repo, SnapshotRef("stage-A")), snapRefSHA(t, repo, SnapshotRef("stage-B"))
	if refA != snapA.SHA || refB != snapB.SHA || refA == refB {
		t.Errorf("snapshot refs A=%q B=%q, want distinct %q / %q", refA, refB, snapA.SHA, snapB.SHA)
	}
}

func TestSnapshot_NothingToSave(t *testing.T) {
	repo := newSnapshotRepo(t)
	p := &Pusher{}

	// An ignored file and a touched-but-unchanged tracked file are not
	// changes: the private index is refreshed before the probe.
	snapWriteFile(t, repo, "ignored.txt", "ignored\n")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(repo, "a.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	snap, created, err := p.SnapshotWorkTree(context.Background(), repo, "stage-1", nil)
	if err != nil {
		t.Fatalf("SnapshotWorkTree: %v", err)
	}
	if created || snap != (Snapshot{}) {
		t.Errorf("SnapshotWorkTree = (%+v, %v), want (zero, false) on a clean tree", snap, created)
	}
	if got := snapRefSHA(t, repo, SnapshotRef("stage-1")); got != "" {
		t.Errorf("%s = %q, want no ref written", SnapshotRef("stage-1"), got)
	}
	if !snapFileExists(repo, "ignored.txt") {
		t.Error("ignored.txt removed by a no-op snapshot")
	}

	// A pathspec naming only clean paths is also nothing to save.
	snapWriteFile(t, repo, "b.txt", "dirty b\n")
	_, created, err = p.SnapshotWorkTree(context.Background(), repo, "stage-2", []string{"a.txt"})
	if err != nil || created {
		t.Errorf("pathspec snapshot of clean a.txt = (created %v, %v), want not created", created, err)
	}
	if got := snapReadFile(t, repo, "b.txt"); got != "dirty b\n" {
		t.Errorf("b.txt = %q, want the unnamed edit untouched", got)
	}
}

// snapConflicted snapshots a divergent edit to a.txt and then advances
// HEAD over the same line, so restoring the snapshot conflicts.
func snapConflicted(t *testing.T, p *Pusher) (repo string, snap Snapshot) {
	t.Helper()
	repo = newSnapshotRepo(t)
	snapWriteFile(t, repo, "a.txt", "agent line\n")
	snap, created, err := p.SnapshotWorkTree(context.Background(), repo, "stage-1", nil)
	if err != nil || !created {
		t.Fatalf("SnapshotWorkTree = (%+v, %v, %v)", snap, created, err)
	}
	snapWriteFile(t, repo, "a.txt", "base advanced line\n")
	snapCommitAll(t, repo, "base advances a.txt")
	return repo, snap
}

func snapAssertTreeAtHEAD(t *testing.T, repo string) {
	t.Helper()
	if unmerged := mustGitOut(t, repo, "ls-files", "--unmerged"); unmerged != "" {
		t.Errorf("unmerged entries left after the abort: %q", unmerged)
	}
	if diff := mustGitOut(t, repo, "diff", "HEAD", "--name-only"); diff != "" {
		t.Errorf("tracked tree differs from HEAD after the abort: %q", diff)
	}
}

func TestSnapshot_RestoreConflictRetainsSnapshot(t *testing.T) {
	p := &Pusher{}
	repo, snap := snapConflicted(t, p)

	err := p.RestoreSnapshot(context.Background(), repo, snap)
	var conflict *BaseRebaseConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("RestoreSnapshot = %v, want *BaseRebaseConflictError", err)
	}
	if len(conflict.ConflictPaths) != 1 || conflict.ConflictPaths[0] != "a.txt" {
		t.Errorf("ConflictPaths = %q, want [a.txt]", conflict.ConflictPaths)
	}
	if !strings.Contains(conflict.ConflictHunks, "<<<<<<<") || !strings.Contains(conflict.ConflictHunks, "agent line") {
		t.Errorf("ConflictHunks = %q, want the conflict markers around the agent line", conflict.ConflictHunks)
	}
	if !strings.Contains(conflict.StashPatch, "+agent line") {
		t.Errorf("StashPatch = %q, want the snapshot's patch", conflict.StashPatch)
	}
	snapAssertTreeAtHEAD(t, repo)
	if got := snapRefSHA(t, repo, snap.Ref); got != snap.SHA {
		t.Errorf("%s = %q after a conflict, want the retained snapshot %q", snap.Ref, got, snap.SHA)
	}
}

// snapSeamPusher delegates to real git except where rewrite returns a
// replacement argv for an invocation.
func snapSeamPusher(rewrite func(args []string) []string) *Pusher {
	return &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if r := rewrite(args); r != nil {
				return exec.CommandContext(ctx, name, r...)
			}
			return exec.CommandContext(ctx, name, args...)
		},
	}
}

var snapFailingGit = []string{"-C", "/nonexistent/fishhawk-seam", "status"}

func TestSnapshot_RestoreProbeFailure(t *testing.T) {
	isUnmergedProbe := func(args []string) bool {
		return len(args) >= 2 && args[0] == "ls-files" && args[1] == "--unmerged"
	}
	isHardReset := func(args []string) bool {
		return len(args) == 2 && args[0] == "reset" && args[1] == "--hard"
	}

	t.Run("probe fails, reset runs", func(t *testing.T) {
		p := snapSeamPusher(func(args []string) []string {
			if isUnmergedProbe(args) {
				return snapFailingGit
			}
			return nil
		})
		repo, snap := snapConflicted(t, &Pusher{})
		err := p.RestoreSnapshot(context.Background(), repo, snap)
		if err == nil || errors.Is(err, ErrBaseRebaseConflict) {
			t.Fatalf("RestoreSnapshot = %v, want a non-conflict error (conflict unconfirmed)", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "ls-files") || !strings.Contains(msg, "working tree reset to HEAD") || !strings.Contains(msg, snap.SHA) {
			t.Errorf("error %q must name the snapshot, the probe failure and the reset", msg)
		}
		snapAssertTreeAtHEAD(t, repo)
	})

	t.Run("probe and reset both fail", func(t *testing.T) {
		p := snapSeamPusher(func(args []string) []string {
			if isUnmergedProbe(args) || isHardReset(args) {
				return snapFailingGit
			}
			return nil
		})
		repo, snap := snapConflicted(t, &Pusher{})
		err := p.RestoreSnapshot(context.Background(), repo, snap)
		if err == nil || errors.Is(err, ErrBaseRebaseConflict) {
			t.Fatalf("RestoreSnapshot = %v, want a non-conflict error", err)
		}
		if !strings.Contains(err.Error(), "best-effort reset --hard also failed") {
			t.Errorf("error %q must annotate the failed best-effort reset", err)
		}
	})

	t.Run("conflict confirmed, abort reset fails", func(t *testing.T) {
		p := snapSeamPusher(func(args []string) []string {
			if isHardReset(args) {
				return snapFailingGit
			}
			return nil
		})
		repo, snap := snapConflicted(t, &Pusher{})
		err := p.RestoreSnapshot(context.Background(), repo, snap)
		if err == nil || errors.Is(err, ErrBaseRebaseConflict) || !strings.Contains(err.Error(), "reset --hard after conflicted snapshot restore") {
			t.Fatalf("RestoreSnapshot = %v, want the failed-abort error", err)
		}
	})
}

func TestSnapshot_RestoreNonConflictFailure(t *testing.T) {
	repo := newSnapshotRepo(t)
	p := &Pusher{}
	snapWriteFile(t, repo, "new.txt", "snapshotted\n")
	snap, created, err := p.SnapshotWorkTree(context.Background(), repo, "stage-1", nil)
	if err != nil || !created {
		t.Fatalf("SnapshotWorkTree = (%+v, %v, %v)", snap, created, err)
	}
	// A file now in the way of the untracked restore: the apply fails with no
	// unmerged entries.
	snapWriteFile(t, repo, "new.txt", "in the way\n")
	err = p.RestoreSnapshot(context.Background(), repo, snap)
	if err == nil || errors.Is(err, ErrBaseRebaseConflict) {
		t.Fatalf("RestoreSnapshot = %v, want a generic non-conflict error", err)
	}
	if !strings.Contains(err.Error(), "gitops: restore snapshot "+snap.SHA+": ") {
		t.Errorf("error %q, want the generic restore wrap naming the snapshot", err)
	}
	if got := snapReadFile(t, repo, "new.txt"); got != "in the way\n" {
		t.Errorf("new.txt = %q, want the in-the-way file untouched", got)
	}

	if err := p.RestoreSnapshot(context.Background(), repo, Snapshot{Ref: snap.Ref}); err == nil || !strings.Contains(err.Error(), "empty snapshot SHA") {
		t.Errorf("RestoreSnapshot(no SHA) = %v, want a refusal", err)
	}
}

func TestSnapshot_PathspecLimited(t *testing.T) {
	repo := newSnapshotRepo(t)
	ctx := context.Background()
	p := &Pusher{}

	snapWriteFile(t, repo, "a.txt", "a named edit\n")
	snapWriteFile(t, repo, "b.txt", "b unnamed edit\n")
	snapWriteFile(t, repo, "c-new.txt", "c named untracked\n")
	snapWriteFile(t, repo, "d-new.txt", "d unnamed untracked\n")
	if err := os.Remove(filepath.Join(repo, "f.txt")); err != nil {
		t.Fatal(err)
	}
	named := []string{"a.txt", "c-new.txt", "f.txt"}

	snap, created, err := p.SnapshotWorkTree(ctx, repo, "drift", named)
	if err != nil || !created {
		t.Fatalf("SnapshotWorkTree = (%+v, %v, %v)", snap, created, err)
	}
	if got := snapReadFile(t, repo, "a.txt"); got != "a\n" {
		t.Errorf("named a.txt = %q after snapshot, want reverted", got)
	}
	if snapFileExists(repo, "c-new.txt") {
		t.Error("named untracked c-new.txt still present after snapshot")
	}
	if !snapFileExists(repo, "f.txt") || snapReadFile(t, repo, "f.txt") != "f\n" {
		t.Error("named deleted f.txt not checked back out after snapshot")
	}
	if got := snapReadFile(t, repo, "b.txt"); got != "b unnamed edit\n" {
		t.Errorf("unnamed b.txt = %q after snapshot, want untouched", got)
	}
	if got := snapReadFile(t, repo, "d-new.txt"); got != "d unnamed untracked\n" {
		t.Errorf("unnamed d-new.txt = %q after snapshot, want untouched", got)
	}
	if got := mustGitOut(t, repo, "ls-tree", "-r", "--name-only", snap.SHA+"^3"); got != "c-new.txt" {
		t.Errorf("snapshot untracked tree = %q, want exactly c-new.txt", got)
	}
	if got := mustGitOut(t, repo, "show", snap.SHA+":b.txt"); got != "b" {
		t.Errorf("snapshot b.txt = %q, want HEAD content (unnamed edit not captured)", got)
	}

	if err := p.RestoreSnapshot(ctx, repo, snap); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if got := snapReadFile(t, repo, "a.txt"); got != "a named edit\n" {
		t.Errorf("a.txt after restore = %q, want the named edit", got)
	}
	if !snapFileExists(repo, "c-new.txt") {
		t.Error("c-new.txt not restored")
	}
	if snapFileExists(repo, "f.txt") {
		t.Error("f.txt deletion not restored")
	}
	if got := snapReadFile(t, repo, "b.txt"); got != "b unnamed edit\n" {
		t.Errorf("unnamed b.txt = %q after restore, want untouched", got)
	}
}

func TestSnapshot_DropDeletesOwnRefOnly(t *testing.T) {
	repo := newSnapshotRepo(t)
	ctx := context.Background()
	p := &Pusher{}
	head := mustGitOut(t, repo, "rev-parse", "HEAD")

	snapWriteFile(t, repo, "a.txt", "edit\n")
	snap, _, err := p.SnapshotWorkTree(ctx, repo, "stage-1", nil)
	if err != nil {
		t.Fatalf("SnapshotWorkTree: %v", err)
	}

	// Re-pointed ref: the old-value guard refuses, and the ref survives.
	mustGit(t, repo, "update-ref", snap.Ref, head)
	if err := p.DropSnapshot(ctx, repo, snap); err == nil {
		t.Fatal("DropSnapshot of a re-pointed ref succeeded, want the old-value guard to refuse")
	}
	if got := snapRefSHA(t, repo, snap.Ref); got != head {
		t.Errorf("%s = %q after the refused drop, want the re-pointed %q", snap.Ref, got, head)
	}

	mustGit(t, repo, "update-ref", snap.Ref, snap.SHA)
	if err := p.DropSnapshot(ctx, repo, snap); err != nil {
		t.Fatalf("DropSnapshot: %v", err)
	}
	if got := snapRefSHA(t, repo, snap.Ref); got != "" {
		t.Errorf("%s = %q after DropSnapshot, want deleted", snap.Ref, got)
	}

	// Refusals and no-ops.
	if err := p.DropSnapshot(ctx, repo, Snapshot{}); err != nil {
		t.Errorf("DropSnapshot(zero) = %v, want no-op", err)
	}
	if err := p.DropSnapshot(ctx, repo, Snapshot{Ref: "refs/heads/main", SHA: head}); err == nil {
		t.Error("DropSnapshot of refs/heads/main succeeded, want refused as foreign")
	}
	if got := snapRefSHA(t, repo, "refs/heads/main"); got != head {
		t.Errorf("refs/heads/main = %q, want untouched %q", got, head)
	}
	if err := p.DropSnapshot(ctx, repo, Snapshot{Ref: snapshotRefPrefix, SHA: head}); err == nil {
		t.Error("DropSnapshot of the bare namespace prefix succeeded, want refused")
	}
	if err := p.DropSnapshot(ctx, repo, Snapshot{Ref: SnapshotRef("x")}); err == nil {
		t.Error("DropSnapshot with no SHA succeeded, want refused")
	}

	// DropSnapshotRef: absent is success; present is deleted whatever it holds.
	if err := p.DropSnapshotRef(ctx, repo, "never-written"); err != nil {
		t.Errorf("DropSnapshotRef(absent) = %v, want success", err)
	}
	mustGit(t, repo, "update-ref", SnapshotRef("stage-2"), head)
	if err := p.DropSnapshotRef(ctx, repo, "stage-2"); err != nil {
		t.Fatalf("DropSnapshotRef: %v", err)
	}
	if got := snapRefSHA(t, repo, SnapshotRef("stage-2")); got != "" {
		t.Errorf("%s = %q after DropSnapshotRef, want deleted", SnapshotRef("stage-2"), got)
	}
	if err := p.DropSnapshotRef(ctx, repo, ""); err == nil {
		t.Error("DropSnapshotRef(\"\") succeeded, want refused")
	}

	// A resolve failure that is not "absent" is an error, not success.
	failResolve := snapSeamPusher(func(args []string) []string {
		if len(args) > 0 && args[0] == "rev-parse" {
			return snapFailingGit
		}
		return nil
	})
	if err := failResolve.DropSnapshotRef(ctx, repo, "stage-3"); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Errorf("DropSnapshotRef with a failing resolve = %v, want a resolve error", err)
	}
}

// TestSnapshot_RefWriteRetriesLockContention pins approval condition C1 for
// the snapshot's own ref write: it waits out a held ref lock within the
// RetryLockContention bound, and every attempt runs under the C locale (read
// from the child's own environment) so the classifier sees English text.
func TestSnapshot_RefWriteRetriesLockContention(t *testing.T) {
	repo := newSnapshotRepo(t)
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")

	lockDir := filepath.Join(repo, ".git", "refs", "fishhawk", "stash")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "stage-1.lock")
	holdRefLock(t, lockPath)
	envLog := filepath.Join(t.TempDir(), "env.log")

	updateRefs := 0
	p := &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "update-ref" {
				updateRefs++
				if updateRefs == 2 {
					_ = os.Remove(lockPath) // the sibling releases its lock
				}
				// Record the child's locale, then exec the real git.
				script := `printf '%s|%s\n' "$LC_ALL" "$LANG" >> "$0"; exec git "$@"`
				return exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", script, envLog}, args...)...)
			}
			return exec.CommandContext(ctx, name, args...)
		},
	}

	snapWriteFile(t, repo, "a.txt", "edit\n")
	snap, created, err := p.SnapshotWorkTree(context.Background(), repo, "stage-1", nil)
	if err != nil || !created {
		t.Fatalf("SnapshotWorkTree under a briefly-held ref lock = (%+v, %v, %v), want success", snap, created, err)
	}
	if updateRefs != 2 {
		t.Errorf("update-ref attempts = %d, want 2 (one contended, one success)", updateRefs)
	}
	if got := snapRefSHA(t, repo, snap.Ref); got != snap.SHA {
		t.Errorf("%s = %q, want %q", snap.Ref, got, snap.SHA)
	}
	lines := strings.Split(strings.TrimSpace(snapReadFile(t, filepath.Dir(envLog), filepath.Base(envLog))), "\n")
	if len(lines) != 2 {
		t.Fatalf("recorded %d update-ref child envs, want 2: %q", len(lines), lines)
	}
	for i, line := range lines {
		if line != "C|C" {
			t.Errorf("update-ref attempt %d ran with LC_ALL|LANG = %q, want C|C", i+1, line)
		}
	}
}

func TestSnapshot_FailureModes(t *testing.T) {
	ctx := context.Background()

	t.Run("empty key", func(t *testing.T) {
		repo := newSnapshotRepo(t)
		// On a clean tree the guard is the ONLY refusal (no ref write is
		// ever attempted, so git's own ref-name check cannot mask it).
		if _, created, err := (&Pusher{}).SnapshotWorkTree(ctx, repo, "", nil); err == nil || created {
			t.Fatalf("SnapshotWorkTree(empty key, clean tree) = (created %v, %v), want refused", created, err)
		}
		snapWriteFile(t, repo, "a.txt", "edit\n")
		if _, created, err := (&Pusher{}).SnapshotWorkTree(ctx, repo, "", nil); err == nil || created {
			t.Fatalf("SnapshotWorkTree(empty key) = (created %v, %v), want refused", created, err)
		}
		if got := snapReadFile(t, repo, "a.txt"); got != "edit\n" {
			t.Errorf("a.txt = %q, want the edit untouched by a refused snapshot", got)
		}
	})

	t.Run("unborn HEAD", func(t *testing.T) {
		repo := t.TempDir()
		mustGit(t, repo, "init", "-q", "--initial-branch=main")
		snapWriteFile(t, repo, "a.txt", "a\n")
		if _, created, err := (&Pusher{}).SnapshotWorkTree(ctx, repo, "k", nil); err == nil || created || !strings.Contains(err.Error(), "resolve HEAD") {
			t.Fatalf("SnapshotWorkTree(unborn) = (created %v, %v), want a resolve-HEAD error", created, err)
		}
	})

	t.Run("ref write fails: tree untouched, no ref", func(t *testing.T) {
		repo := newSnapshotRepo(t)
		snapWriteFile(t, repo, "a.txt", "edit\n")
		snapWriteFile(t, repo, "new.txt", "new\n")
		p := snapSeamPusher(func(args []string) []string {
			if len(args) > 0 && args[0] == "update-ref" {
				return snapFailingGit
			}
			return nil
		})
		snap, created, err := p.SnapshotWorkTree(ctx, repo, "k", nil)
		if err == nil || created || snap != (Snapshot{}) {
			t.Fatalf("SnapshotWorkTree = (%+v, %v, %v), want a not-created error", snap, created, err)
		}
		if snapReadFile(t, repo, "a.txt") != "edit\n" || !snapFileExists(repo, "new.txt") {
			t.Error("working tree reverted although the snapshot ref was never written")
		}
		if got := snapRefSHA(t, repo, SnapshotRef("k")); got != "" {
			t.Errorf("%s = %q, want absent", SnapshotRef("k"), got)
		}
	})

	t.Run("revert fails after the ref is written", func(t *testing.T) {
		repo := newSnapshotRepo(t)
		snapWriteFile(t, repo, "a.txt", "edit\n")
		p := snapSeamPusher(func(args []string) []string {
			if len(args) > 0 && args[0] == "reset" {
				return snapFailingGit
			}
			return nil
		})
		snap, created, err := p.SnapshotWorkTree(ctx, repo, "k", nil)
		if err == nil || !created || snap.SHA == "" {
			t.Fatalf("SnapshotWorkTree = (%+v, %v, %v), want the created snapshot plus the revert error", snap, created, err)
		}
		if !strings.Contains(err.Error(), snap.SHA) {
			t.Errorf("error %q does not name the saved snapshot commit", err)
		}
		if got := snapRefSHA(t, repo, snap.Ref); got != snap.SHA {
			t.Errorf("%s = %q, want the retained snapshot %q", snap.Ref, got, snap.SHA)
		}
	})

	t.Run("tracked-change probe error", func(t *testing.T) {
		repo := newSnapshotRepo(t)
		snapWriteFile(t, repo, "a.txt", "edit\n")
		p := snapSeamPusher(func(args []string) []string {
			if len(args) > 1 && args[0] == "diff-index" && args[1] == "--quiet" {
				return snapFailingGit
			}
			return nil
		})
		if _, created, err := p.SnapshotWorkTree(ctx, repo, "k", nil); err == nil || created || !strings.Contains(err.Error(), "probe tracked changes") {
			t.Fatalf("SnapshotWorkTree = (created %v, %v), want the probe error", created, err)
		}
	})
}
