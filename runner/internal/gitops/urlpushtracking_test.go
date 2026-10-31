package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// URL-push tracking-ref guard (#4767). git 2.56 makes `git push <url>` create or
// move refs/remotes/<r>/<b> whenever exactly one configured remote has that push
// URL; urlPushNoTrackingArgs defeats the unique match. Two tests, split by what
// each can discriminate:
//
//   - TestURLPush_CarriesNoTrackingGuard pins the ARGV, so it reddens on any git.
//   - TestURLPush_LeavesNoTrackingRef_OnInstalledGit pins the BEHAVIOUR, so it
//     reddens on git >= 2.56 only (older git never writes the ref).

const urlPushTestBranch = "fishhawk/run-4767/stage-x"

// Origin layouts relative to the URL the runner pushes to (the bare path).
const (
	originURLEqualsPushURL     = "origin_url_equals_push_url"
	originPushURLEqualsPushURL = "origin_pushurl_equals_push_url"
	originURLDiffers           = "origin_url_differs"
)

// newURLPushFixture builds a repo with one commit plus an empty bare remote and
// an `origin` laid out per mode. It returns (repo, bare); the runner pushes to
// the bare path.
func newURLPushFixture(t *testing.T, mode string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	bare := filepath.Join(filepath.Dir(repo), "origin.git")
	mustGit(t, repo, "init", "--bare", bare)
	switch mode {
	case originURLEqualsPushURL:
		mustGit(t, repo, "remote", "add", "origin", bare)
	case originPushURLEqualsPushURL:
		mustGit(t, repo, "remote", "add", "origin", "file://"+bare)
		mustGit(t, repo, "config", "remote.origin.pushurl", bare)
	case originURLDiffers:
		mustGit(t, repo, "remote", "add", "origin", "file://"+bare)
	default:
		t.Fatalf("unknown origin mode %q", mode)
	}
	return repo, bare
}

func writeAgentEdit(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "agent.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// recordingPusher passes every git invocation through to real git and records
// the argv of the push.
func recordingPusher(pushArgv *[]string) *Pusher {
	return &Pusher{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			if gitSubcommand(args) == "push" {
				*pushArgv = append([]string(nil), args...)
			}
			return exec.CommandContext(ctx, name, args...)
		},
	}
}

// trackingRefResolves reports whether refs/remotes/origin/<branch> resolves.
func trackingRefResolves(repo, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

func TestURLPush_CarriesNoTrackingGuard(t *testing.T) {
	// assertGuard requires the guard pair exactly once, ahead of the push
	// subcommand (a `-c` after the subcommand is a push argument, not git config).
	assertGuard := func(t *testing.T, argv []string, url string) {
		t.Helper()
		if argv == nil {
			t.Fatal("push was never invoked")
		}
		want := "remote." + urlPushNoTrackingRemote + ".pushurl=" + url
		subIdx := -1
		for i := 0; i < len(argv); i++ {
			if argv[i] == "-c" {
				i++
				continue
			}
			subIdx = i
			break
		}
		if subIdx < 0 || argv[subIdx] != "push" {
			t.Fatalf("push argv has no push subcommand: %v", argv)
		}
		pairs := 0
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-c" && argv[i+1] == want {
				pairs++
				if i >= subIdx {
					t.Errorf("guard pair at index %d is not before the push subcommand (index %d): %v", i, subIdx, argv)
				}
			}
		}
		if pairs != 1 {
			t.Errorf("push argv carries the URL-push guard pair `-c %s` %d times, want exactly 1: %v", want, pairs, argv)
		}
	}

	for _, updateTracking := range []bool{false, true} {
		name := "CommitAndPush"
		if updateTracking {
			name = "CommitAndPush_UpdateTrackingRef"
		}
		t.Run(name, func(t *testing.T) {
			repo, bare := newURLPushFixture(t, originURLEqualsPushURL)
			writeAgentEdit(t, repo)
			var pushArgv []string
			_, err := recordingPusher(&pushArgv).CommitAndPush(context.Background(), CommitAndPushArgs{
				RepoDir:           repo,
				Branch:            urlPushTestBranch,
				CommitMessage:     "agent change",
				RemoteURL:         bare,
				UpdateTrackingRef: updateTracking,
			})
			if err != nil {
				t.Fatalf("CommitAndPush: %v", err)
			}
			assertGuard(t, pushArgv, bare)
		})
	}

	t.Run("PushCommittedBranch", func(t *testing.T) {
		repo, bare, head := pushCommittedFixture(t)
		var pushArgv []string
		if _, err := recordingPusher(&pushArgv).PushCommittedBranch(context.Background(), PushCommittedBranchArgs{
			RepoDir:   repo,
			Branch:    urlPushTestBranch,
			RemoteURL: bare,
			HeadSHA:   head,
		}); err != nil {
			t.Fatalf("PushCommittedBranch: %v", err)
		}
		assertGuard(t, pushArgv, bare)
	})
}

func TestURLPush_LeavesNoTrackingRef_OnInstalledGit(t *testing.T) {
	// Probe: does THIS git write the tracking ref for a raw, unguarded URL push?
	// Diagnostic only — never an assertion. A true line means this run
	// discriminates the guard; a false one means the git predates 2.56 and the
	// subtests below pass with or without it (the argv test carries that case).
	{
		repo, bare := newURLPushFixture(t, originURLEqualsPushURL)
		mustGit(t, repo, "push", bare, "HEAD:refs/heads/probe")
		t.Logf("installed git writes a tracking ref for an unguarded URL push: %v (true means this run discriminates the guard)",
			trackingRefResolves(repo, "probe"))
	}

	tests := []struct {
		name string
		kind string // "commit_and_push" | "push_committed_branch"
		mode string
	}{
		{"commit_and_push/origin_url_equals_push_url", "commit_and_push", originURLEqualsPushURL},
		{"commit_and_push/origin_pushurl_equals_push_url", "commit_and_push", originPushURLEqualsPushURL},
		{"push_committed_branch/origin_url_equals_push_url", "push_committed_branch", originURLEqualsPushURL},
		{"commit_and_push/origin_url_differs", "commit_and_push", originURLDiffers},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo, bare := newURLPushFixture(t, tc.mode)
			before := mustGitOut(t, repo, "for-each-ref", "refs/remotes/")

			var headSHA string
			switch tc.kind {
			case "commit_and_push":
				writeAgentEdit(t, repo)
				res, err := (&Pusher{}).CommitAndPush(context.Background(), CommitAndPushArgs{
					RepoDir:       repo,
					Branch:        urlPushTestBranch,
					CommitMessage: "agent change",
					RemoteURL:     bare,
				})
				if err != nil {
					t.Fatalf("CommitAndPush: %v", err)
				}
				headSHA = res.HeadSHA
			case "push_committed_branch":
				headSHA = mustGitOut(t, repo, "rev-parse", "HEAD")
				if _, err := (&Pusher{}).PushCommittedBranch(context.Background(), PushCommittedBranchArgs{
					RepoDir:   repo,
					Branch:    urlPushTestBranch,
					RemoteURL: bare,
					HeadSHA:   headSHA,
				}); err != nil {
					t.Fatalf("PushCommittedBranch: %v", err)
				}
			default:
				t.Fatalf("unknown kind %q", tc.kind)
			}

			if got := mustGitOut(t, repo, "--git-dir="+bare, "rev-parse", urlPushTestBranch); got != headSHA {
				t.Errorf("bare branch sha = %q, want pushed HEAD %q", got, headSHA)
			}
			if trackingRefResolves(repo, urlPushTestBranch) {
				t.Errorf("refs/remotes/origin/%s resolves after a URL push; the push must leave no tracking ref (git >= 2.56 writes one unless the push carries the URL-push guard, #4767)", urlPushTestBranch)
			}
			if after := mustGitOut(t, repo, "for-each-ref", "refs/remotes/"); after != before {
				t.Errorf("refs/remotes changed across a URL push:\nbefore:\n%s\nafter:\n%s", strings.TrimSpace(before), strings.TrimSpace(after))
			}
		})
	}
}
