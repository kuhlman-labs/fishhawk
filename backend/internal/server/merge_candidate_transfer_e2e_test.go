package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E87.2 / #4293 CROSS-BOUNDARY END-TO-END: classifier → git → audit chain →
// rebase handler → merge gate. The REAL rebase handler over pgtest Postgres
// and the mcqGitHub forge, whose merges endpoint creates a REAL `git merge
// --no-ff` commit of main into the PR branch of a REAL repository bound as the
// run's working dir; the REAL default inputs resolver (basedelta.GitDelta +
// MaterializeTree) and the REAL basedelta.Classifier with GoListLoader over a
// tiny module (packages a and b, no deps, GOPROXY=off). The PR head H edits
// a/a.go and carries a RECORDED passed verdict.

// mcteWorld is one e2e repository plus its seeded run.
type mcteWorld struct {
	f        *mcqFixture
	r        *mcqRun
	repo     string
	head     string // H, the PR head with the recorded passed verdict
	fromBase string // B, the PR's merge base
	toBase   string // B', main after the advance
	merged   string // H', the merge commit the forge created
}

// newMCTEWorld builds the repository and the run, advances main by advance
// (path → content), and wires mode with the real classifier.
func newMCTEWorld(t *testing.T, mode MergeCandidateTransferMode, advance map[string]string) *mcteWorld {
	t.Helper()
	for _, bin := range []string{"git", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	w := &mcteWorld{f: newMCQFixture(t), repo: t.TempDir()}
	mctGit(t, w.repo, "init", "-q")
	mctWrite(t, w.repo, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n",
		"a/a.go": "package a\n\nconst A = 1\n",
		"b/b.go": "package b\n\nconst B = 1\n",
	})
	mctGit(t, w.repo, "add", "-A")
	mctGit(t, w.repo, "commit", "-q", "-m", "base")
	w.fromBase = mctGit(t, w.repo, "rev-parse", "HEAD")
	mctGit(t, w.repo, "checkout", "-q", "-b", "pr")
	mctWrite(t, w.repo, map[string]string{"a/a.go": "package a\n\nconst A = 2\n"})
	mctGit(t, w.repo, "commit", "-q", "-am", "pr")
	w.head = mctGit(t, w.repo, "rev-parse", "HEAD")
	mctGit(t, w.repo, "checkout", "-q", "main")
	mctWrite(t, w.repo, advance)
	mctGit(t, w.repo, "add", "-A")
	mctGit(t, w.repo, "commit", "-q", "-m", "advance")
	w.toBase = mctGit(t, w.repo, "rev-parse", "HEAD")
	mctGit(t, w.repo, "checkout", "-q", "pr")

	f := w.f
	w.r = f.seed("e2e", w.head, []byte(mcvUndelegatedSpecYAML))
	f.appendSystemRow(w.r, CategoryBranchRebased, map[string]any{
		"new_head_sha": w.head, "merge_commit_sha": w.head, "already_up_to_date": false})
	f.passedAt(w.r, w.head)
	f.exec(`UPDATE runs SET working_dir = $1 WHERE id = $2`, w.repo, w.r.id)

	f.gh.set(func() {
		f.gh.behind[w.head] = []string{w.toBase}
		f.gh.mergeFn = func(base string) string {
			if base != w.r.branch {
				return ""
			}
			cmd := exec.Command("git", "-c", "user.email=t@example.invalid", "-c", "user.name=t",
				"-c", "commit.gpgsign=false", "-C", w.repo, "merge", "-q", "--no-ff", "-m", "merge main", "main")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("forge merge: %v\n%s", err, out)
				return ""
			}
			out, err := exec.Command("git", "-C", w.repo, "rev-parse", "HEAD").Output()
			if err != nil {
				t.Errorf("forge merge rev-parse: %v", err)
				return ""
			}
			w.merged = strings.TrimSpace(string(out))
			return w.merged
		}
	})
	f.srv.cfg.MergeCandidateTransfer = MergeCandidateTransferConfig{
		Mode: mode, Classifier: &basedelta.Classifier{Loader: &basedelta.GoListLoader{}}}
	return w
}

func (w *mcteWorld) runRow(t *testing.T) *run.Run {
	t.Helper()
	r, err := w.f.repo.GetRun(context.Background(), w.r.id)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return r
}

func TestMergeCandidateTransfer_E2E_DisjointAdvanceTransfers(t *testing.T) {
	w := newMCTEWorld(t, MergeCandidateTransferEnforce, map[string]string{"b/b.go": "package b\n\nconst B = 2\n"})
	f := w.f

	resp := f.rebase(w.r)
	if w.merged == "" || resp.NewHeadSHA != w.merged {
		t.Fatalf("new head = %q, want the real merge commit %q", resp.NewHeadSHA, w.merged)
	}
	rows := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferred)
	if len(rows) != 1 {
		refused := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferRefused)
		t.Fatalf("transferred rows = %+v (refused %+v), want one", rows, refused)
	}
	got := rows[0]
	if got.FromHead != w.head || got.ToHead != w.merged || got.FromBase != w.fromBase || got.ToBase != w.toBase ||
		got.DeltaPathsCount != 1 || got.ReasonCode != transferReasonIndependent || got.Reason != transferReasonIndependent {
		t.Fatalf("transferred = %+v, want H %s → H' %s, B %s → B' %s, 1 delta path, independent",
			got, w.head, w.merged, w.fromBase, w.toBase)
	}
	if tr := f.triggers(w.r); len(tr) != 0 {
		t.Fatalf("triggers = %+v, want none for H'", tr)
	}
	if st := f.stageState(w.r.impl.ID); st != run.StageStateSucceeded {
		t.Errorf("implement = %s, want succeeded (not re-opened)", st)
	}
	if g := f.srv.mergeCandidateGate(context.Background(), w.runRow(t)); !g.admits() {
		t.Errorf("merge gate = %+v, want admit on the transferred verdict", g)
	}
}

func TestMergeCandidateTransfer_E2E_SamePackageAdvanceRefusedReachOverlap(t *testing.T) {
	w := newMCTEWorld(t, MergeCandidateTransferEnforce, map[string]string{"a/other.go": "package a\n\nconst O = 1\n"})
	f := w.f

	f.rebase(w.r)
	rows := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferRefused)
	if len(rows) != 1 || rows[0].ReasonCode != transferReasonReachOverlap || rows[0].ReasonDetail != "a" || rows[0].ToHead != w.merged {
		t.Fatalf("refused rows = %+v, want one reach_overlap detail `a` for %s", rows, w.merged)
	}
	if n := len(f.rows(w.r, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Fatalf("transferred rows = %d, want 0", n)
	}
	if tr := f.triggers(w.r); len(tr) != 1 || tr[0].ExpectedHeadSHA != w.merged {
		t.Fatalf("triggers = %+v, want one for H' %s", tr, w.merged)
	}
	if g := f.srv.mergeCandidateGate(context.Background(), w.runRow(t)); g.Code != mergeCodeCandidateUnverified {
		t.Errorf("merge gate code = %q, want merge_candidate_unverified", g.Code)
	}
}

func TestMergeCandidateTransfer_E2E_ShadowDisjointStillRunsPass(t *testing.T) {
	w := newMCTEWorld(t, MergeCandidateTransferShadow, map[string]string{"b/b.go": "package b\n\nconst B = 2\n"})
	f := w.f

	f.rebase(w.r)
	if tr := f.triggers(w.r); len(tr) != 1 || tr[0].ExpectedHeadSHA != w.merged {
		t.Fatalf("triggers = %+v, want one for H' %s", tr, w.merged)
	}
	f.srv.waitMergeCandidateTransferShadows()
	sh := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferShadow)
	if len(sh) != 1 || sh[0].WouldTransfer == nil || !*sh[0].WouldTransfer || sh[0].ToHead != w.merged ||
		sh[0].ReasonCode != transferReasonIndependent {
		t.Fatalf("shadow rows = %+v, want one would_transfer independent for %s", sh, w.merged)
	}
	if n := len(f.rows(w.r, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Fatalf("transferred rows = %d in shadow, want 0", n)
	}
}
