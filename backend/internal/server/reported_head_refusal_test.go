package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file holds the ADR-141 (#5053, #4870) refusal tests beyond the core
// verdicts in reported_head_test.go: replay uncertainty, the shared forge
// deadline, reported-branch distrust, settled-stage refusal, evidence WARN
// content, recovery ordering, recovery costs and neutral text. As in that file, a
// failure line starting `setup:` is a fixture failure and one starting `assert:`
// is the behaviour under test (the ledger's RED rule).

// rhReportedBranch is a branch a runner names and the server never derived.
const rhReportedBranch = "fishhawk/reported-branch"

// forgeAt puts the PR head and the PR branch's tip where a test needs them. H2,
// the head these tests report, always has the reported base as its first parent,
// so only the head check can refuse it.
func (f *rhFixture) forgeAt(prHead, tip string) {
	f.t.Helper()
	f.forge.set(func(g *rhForge) {
		g.prHead = prHead
		g.tips[rhPRBranch] = tip
		g.parents[rhH2] = []string{rhBaseSHA}
	})
}

// shrinkReportedHeadForgeTimeout swaps the shared deadline for one test. No test
// in this package that reaches the check runs in parallel with a serial one.
func shrinkReportedHeadForgeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := reportedHeadForgeTimeout
	reportedHeadForgeTimeout = d
	t.Cleanup(func() { reportedHeadForgeTimeout = prev })
}

// postTo sends a signed report for an explicit stage, for the pairings post does
// not choose by itself.
func (f *rhFixture) postTo(stage *run.Stage, kc rhKindCase, head, base, branch string) *httptest.ResponseRecorder {
	f.t.Helper()
	body, err := json.Marshal(map[string]any{
		"outcome": kc.outcome, "branch": branch, "head_sha": head, "base_sha": base, "files_changed_count": 1,
	})
	if err != nil {
		f.t.Fatalf("setup: marshal report: %v", err)
	}
	priv, _ := f.sf.issue(f.t, f.runRow.ID)
	w := shipPRRequest(f.t, f.s, f.runRow.ID, stage.ID, priv, body, "")
	f.s.waitBackgroundReviews()
	return w
}

// recordRefReads logs each branch whose ref the check reads.
func (f *rhFixture) recordRefReads() func() []string {
	f.t.Helper()
	var mu sync.Mutex
	var seen []string
	f.forge.set(func(g *rhForge) {
		g.hook = func(route string, r *http.Request) {
			if route != "ref" {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, r.PathValue("branch"))
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// warnRecord returns the first WARN record carrying msg, or nil.
func (f *rhFixture) warnRecord(msg string) map[string]any {
	f.t.Helper()
	for _, line := range strings.Split(f.logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["level"] == "WARN" && rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// assertRowAt pins the single accepted row of a kind.
func (f *rhFixture) assertRowAt(kc rhKindCase, w *httptest.ResponseRecorder, head, branch string) {
	f.t.Helper()
	if w.Code != http.StatusOK {
		f.t.Errorf("assert: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if rows := f.rowPayloads(kc.category); len(rows) != 1 || rows[0]["head_sha"] != head || rows[0]["branch"] != branch {
		f.t.Errorf("assert: %s rows = %+v, want one at head %s on branch %s", kc.category, rows, head, branch)
	}
	if rows := f.mismatchRows(); len(rows) != 0 {
		f.t.Errorf("assert: reported_head_mismatch rows = %d, want 0", len(rows))
	}
}

// TestReportedHead_FixupPushed_NoCommitRead: the fix-up base stays outside ADR-141
// D1, so the fix-up kind reads no commit, on either head arm, even with a base that
// is no parent of the head.
//
// COUNTERFACTUAL (M45, the kind guard forced true): the fix-up reads the commit
// and refuses the non-parent base, so the status, the row and the read count go RED.
func TestReportedHead_FixupPushed_NoCommitRead(t *testing.T) {
	arms := map[string]struct{ prHead, tip string }{
		"pr_head_arm": {rhH2, rhOther},
		"tip_arm":     {rhH1, rhH2},
	}
	for name, arm := range arms {
		t.Run(name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(arm.prHead, arm.tip)
			f.forge.set(func(g *rhForge) { g.parents[rhH2] = []string{rhOther} })
			w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRowAt(rhFixupKind, w, rhH2, rhPRBranch)
			if n := f.forge.count("commit"); n != 0 {
				t.Errorf("assert: commit reads = %d, want 0 on the fix-up kind", n)
			}
		})
	}

	// The route log does see a commit read, so zero above is not a dead counter.
	t.Run("conflict_kind_control_reads_the_commit", func(t *testing.T) {
		f := newRHFixture(t)
		f.forgeAt(rhH2, rhH2)
		w := f.post(rhConflictKind, rhH2, rhBaseSHA, rhPRBranch)
		f.assertRowAt(rhConflictKind, w, rhH2, rhPRBranch)
		if n := f.forge.count("commit"); n != 1 {
			t.Errorf("assert: commit reads = %d, want 1 on the conflict kind", n)
		}
	})
}

// TestReportedHead_RefNameHeadRefused: a ref name or a short prefix never resolves
// to the commit it names. The fixture's ref and commit reads would both answer for
// those strings; exact comparison, not a lookup, decides.
//
// COUNTERFACTUAL (M46, the tip comparison neutered): the string reaches the row on
// every kind, whose commit read answers with the reported base, so zero-rows goes
// RED.
func TestReportedHead_RefNameHeadRefused(t *testing.T) {
	for name, head := range map[string]string{"ref_name": rhBaseRef, "short_prefix": rhH1[:7]} {
		for _, kc := range rhAllKinds {
			t.Run(name+"/"+kc.name, func(t *testing.T) {
				f := newRHFixture(t)
				f.forgeAt(rhH1, rhH1)
				f.forge.set(func(g *rhForge) {
					g.tips[rhBaseRef] = rhH1
					g.parents[head] = []string{rhBaseSHA}
					g.parents[rhH1] = []string{rhBaseSHA}
					g.compare[rhBaseRef] = []string{head}
				})
				w := f.post(kc, head, rhBaseSHA, rhPRBranch)
				f.assertRefused(kc, w, "C", reportedHeadReasonHead)
				if n := f.forge.count("commit"); n != 0 {
					t.Errorf("assert: commit reads = %d, want 0: a refused head reaches no lookup", n)
				}
			})
		}
	}
}

// TestReportedHead_ReportedBranchNotTrusted: on every run shape the branch comes
// from the forge or server state. The reported branch's tip equals the head while
// the server-resolved branch's tip differs, so reading it would verify the report.
//
// COUNTERFACTUALS: M50 (the tracked PR's branch read from the report) and M47 (the
// derived branch read from the report) verify against the reported branch, so the
// row lands and the tracked_pr or the other subtests go RED on zero-rows.
func TestReportedHead_ReportedBranchNotTrusted(t *testing.T) {
	cases := []struct {
		name    string
		kind    rhKindCase
		arrange func(f *rhFixture) string
	}{
		{"tracked_pr", rhFixupKind, func(f *rhFixture) string { return rhPRBranch }},
		{"decomposed_child_slice_branch", rhFixupKind, func(f *rhFixture) string {
			f.untrack()
			parent := uuid.New()
			idx := 2
			f.runRow.DecomposedFrom = &parent
			f.runRow.SliceIndex = &idx
			return fixupBranchFor(f.runRow, f.impl)
		}},
		{"decomposed_parent_consolidated_branch", rhFixupKind, func(f *rhFixture) string {
			f.untrack()
			child := f.rr.seedRun()
			child.DecomposedFrom = &f.runRow.ID
			return orchestrator.ConsolidatedBranch(f.runRow.ID)
		}},
		{"no_pr_acceptance", rhAcceptanceKind, func(f *rhFixture) string {
			f.untrack()
			return fixupBranchFor(f.runRow, f.impl)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRHFixture(t)
			resolved := c.arrange(f)
			refReads := f.recordRefReads()
			f.forgeAt(rhH1, rhH1)
			f.forge.set(func(g *rhForge) {
				g.tips[resolved] = rhH1
				g.tips[rhReportedBranch] = rhH2
			})

			w := f.post(c.kind, rhH2, rhBaseSHA, rhReportedBranch)

			f.assertRefused(c.kind, w, "C", reportedHeadReasonHead)
			reads := refReads()
			if len(reads) != 1 || reads[0] != resolved {
				t.Errorf("assert: branch refs read = %v, want only %s: the reported branch is never read", reads, resolved)
			}
			if rows := f.mismatchRows(); len(rows) == 1 {
				if rows[0]["forge_branch"] != resolved || rows[0]["forge_tip"] != rhH1 || rows[0]["reported_branch"] != rhReportedBranch {
					t.Errorf("assert: evidence branches = %+v, want forge %s at %s, reported %s", rows[0], resolved, rhH1, rhReportedBranch)
				}
			}
		})
	}
}

// TestReportedHead_RowBranchIsForgeResolved: an accepted report's row and response
// carry the forge-resolved branch, not the one the report named.
//
// COUNTERFACTUAL (M10, the assignment dropped): the row keeps the reported branch →
// RED.
func TestReportedHead_RowBranchIsForgeResolved(t *testing.T) {
	assertResolved := func(t *testing.T, f *rhFixture, kc rhKindCase, w *httptest.ResponseRecorder, want string) {
		t.Helper()
		f.assertRowAt(kc, w, rhH2, want)
		var resp struct {
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Branch != want {
			t.Errorf("assert: response branch = %q (%v), want %s", resp.Branch, err, want)
		}
	}
	for _, kc := range rhAllKinds {
		t.Run("tracked_pr/"+kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH2, rhH2)
			w := f.post(kc, rhH2, rhBaseSHA, rhReportedBranch)
			assertResolved(t, f, kc, w, rhPRBranch)
		})
		t.Run("derived/"+kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.untrack()
			derived := fixupBranchFor(f.runRow, f.impl)
			f.forgeAt("", rhOther)
			f.forge.set(func(g *rhForge) { g.tips[derived] = rhH2 })
			w := f.post(kc, rhH2, rhBaseSHA, rhReportedBranch)
			assertResolved(t, f, kc, w, derived)
		})
	}
}

// TestReportedHead_MismatchOnSettledStage_NoTransition: a mismatch fails only a
// running implement stage. A settled implement stage or a non-implement stage
// answers 409 and keeps its state, even with a fix-up trigger that could restore it.
//
// COUNTERFACTUAL (M12, the running check dropped from the failStage line): the
// settled implement stages take the 200 failure path → RED. The non-implement
// subtests pin the stage-type half, which M12 leaves in place.
func TestReportedHead_MismatchOnSettledStage_NoTransition(t *testing.T) {
	cases := []struct {
		name  string
		state run.StageState
		stage func(f *rhFixture) *run.Stage
	}{
		{"succeeded_implement", run.StageStateSucceeded, func(f *rhFixture) *run.Stage { return f.impl }},
		{"awaiting_approval_implement", run.StageStateAwaitingApproval, func(f *rhFixture) *run.Stage { return f.impl }},
		{"failed_implement", run.StageStateFailed, func(f *rhFixture) *run.Stage { return f.impl }},
		{"running_review_stage", run.StageStateRunning, func(f *rhFixture) *run.Stage { return f.review }},
		{"pending_review_stage", run.StageStatePending, func(f *rhFixture) *run.Stage { return f.review }},
	}
	for _, c := range cases {
		for _, kc := range []rhKindCase{rhFixupKind, rhConflictKind} {
			t.Run(c.name+"/"+kc.name, func(t *testing.T) {
				f := newRHFixture(t)
				f.seedFixupTrigger(10)
				f.forgeAt(rhH1, rhH1)
				stage := c.stage(f)
				stage.State = c.state

				w := f.postTo(stage, kc, rhH2, rhBaseSHA, rhPRBranch)

				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"reported_head_mismatch"`) {
					t.Errorf("assert: status = %d, want 409 reported_head_mismatch:\n%s", w.Code, w.Body.String())
				}
				if got := f.stageState(stage.ID).State; got != c.state {
					t.Errorf("assert: stage state = %s, want %s (unchanged)", got, c.state)
				}
				for _, category := range []string{kc.category, "pull_request_failed", CategoryStageFixupRecovered} {
					if rows := f.rowPayloads(category); len(rows) != 0 {
						t.Errorf("assert: %s rows = %d, want 0", category, len(rows))
					}
				}
				if rows := f.mismatchRows(); len(rows) != 1 {
					t.Errorf("assert: reported_head_mismatch rows = %d, want 1", len(rows))
				}
			})
		}
	}
}

// TestReportedHead_HungForgeIsBounded: a forge read that never answers cannot hold
// the report past the shared deadline, on any of the three reads.
//
// COUNTERFACTUAL (M13, the deadline replaced with a plain cancel): the fake holds
// its answer until its context ends and the stub client's own timeout is 5s, so
// the call returns after the 3s bound and the elapsed assertion goes RED.
func TestReportedHead_HungForgeIsBounded(t *testing.T) {
	const bound = 3 * time.Second
	cases := []struct {
		name  string
		route string
		kind  rhKindCase
		set   func(f *rhFixture)
	}{
		{"pull_read", "pull", rhFixupKind, func(f *rhFixture) {}},
		{"ref_read", "ref", rhFixupKind, func(f *rhFixture) { f.forgeAt(rhH1, rhH2) }},
		{"commit_read", "commit", rhConflictKind, func(f *rhFixture) { f.forgeAt(rhH2, rhH2) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRHFixture(t)
			shrinkReportedHeadForgeTimeout(t, 100*time.Millisecond)
			c.set(f)
			f.forge.set(func(g *rhForge) {
				g.hook = func(route string, r *http.Request) {
					if route != c.route {
						return
					}
					// Held until the client gives up. The timer only keeps a broken
					// disconnect from outliving this test.
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
				}
			})

			start := time.Now()
			w := f.post(c.kind, rhH2, rhBaseSHA, rhPRBranch)
			elapsed := time.Since(start)

			if n := f.forge.count(c.route); n != 1 {
				t.Fatalf("setup: %s reads = %d, want 1: the hung read was not reached", c.route, n)
			}
			f.assertUnverifiable(c.kind, w, f.impl, run.StageStateRunning)
			if elapsed >= bound {
				t.Errorf("assert: report answered after %s, want under %s", elapsed, bound)
			}
		})
	}
}

// TestReportedHead_ForgeReadsShareOneDeadline: the PR read and the branch-ref read
// spend one deadline. Each takes 300ms and answers values that would verify, which
// the generous-deadline control proves.
//
// COUNTERFACTUAL (M20, each read given its own timeout): both reads finish inside
// 400ms apiece, so the row lands and zero-rows goes RED.
func TestReportedHead_ForgeReadsShareOneDeadline(t *testing.T) {
	arrange := func(t *testing.T, timeout time.Duration) *rhFixture {
		t.Helper()
		f := newRHFixture(t)
		shrinkReportedHeadForgeTimeout(t, timeout)
		f.forgeAt(rhH1, rhH2)
		f.forge.set(func(g *rhForge) {
			g.hook = func(route string, r *http.Request) {
				if route != "pull" && route != "ref" {
					return
				}
				select {
				case <-time.After(300 * time.Millisecond):
				case <-r.Context().Done():
				}
			}
		})
		return f
	}
	t.Run("one_deadline_expires_during_the_second_read", func(t *testing.T) {
		f := arrange(t, 400*time.Millisecond)
		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		f.assertUnverifiable(rhFixupKind, w, f.impl, run.StageStateRunning)
	})
	t.Run("generous_deadline_control_verifies", func(t *testing.T) {
		f := arrange(t, 5*time.Second)
		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		f.assertRowAt(rhFixupKind, w, rhH2, rhPRBranch)
	})
}

// TestReportedHead_DedupReadErrorMismatchIsRetryable: a mismatch whose dedup read
// failed cannot be told from a replay after the tip moved, so it is unknown. So is
// a mismatch whose re-read finds the row a concurrent delivery just wrote.
//
// COUNTERFACTUAL (M09, the replay guard neutered): the verdict stays mismatch. The
// fix-up and acceptance kinds take the refusal path, so 503 and the invariant-row
// assertions go RED. The conflict kind's trigger-state read fails on the same
// category and still answers 503, so its logged cause is what goes RED there.
func TestReportedHead_DedupReadErrorMismatchIsRetryable(t *testing.T) {
	for _, kc := range rhAllKinds {
		t.Run("read_error/"+kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH3, rhH3)
			f.au.listByCategoryErrCategory = kc.category
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertUnverifiable(kc, w, f.reportingStage(kc), run.StageStateRunning)
			f.assertLoggedCause("replay_unknown")
		})
		t.Run("row_visible_on_reread/"+kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH3, rhH3)
			stage := f.reportingStage(kc)
			// The handler's dedup has already read nothing by the time the check
			// reaches the forge, so a row seeded here is visible only to the re-read.
			f.forge.set(func(g *rhForge) {
				g.hook = func(route string, _ *http.Request) {
					if route == "pull" {
						f.seed(kc.category, stage.ID, 5, map[string]any{"head_sha": rhH2})
					}
				}
			})
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertUnverifiable(kc, w, stage, run.StageStateRunning)
			f.assertLoggedCause("replay_unknown")
		})
	}
}

// TestReportedHead_DedupReadErrorVerifiedHeadStillWrites pins the existing fail-open
// dedup: a verified head proceeds and writes its row despite the failed read.
func TestReportedHead_DedupReadErrorVerifiedHeadStillWrites(t *testing.T) {
	for _, kc := range rhAllKinds {
		t.Run(kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH2, rhH2)
			f.au.listByCategoryErrCategory = kc.category
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRowAt(kc, w, rhH2, rhPRBranch)
			if !strings.Contains(f.logs.String(), "proceeding without idempotency guard") {
				t.Errorf("assert: the failed dedup read was not logged:\n%s", f.logs.String())
			}
		})
	}
}

// TestReportedHead_ReplayAfterTipMoved_DedupFirst: a signed report the server already
// admitted stays an idempotent 200 after the tip moves, with no forge read. Keying
// is unchanged: head-keyed for fix-up and acceptance, stage-keyed for conflict.
//
// COUNTERFACTUAL (M14a/b/c, each hook moved above its dedup): the replay reaches the
// check, whose moved tip no longer matches, so the 200 and the read count go RED.
func TestReportedHead_ReplayAfterTipMoved_DedupFirst(t *testing.T) {
	forgeReads := func(f *rhFixture) int {
		return f.forge.count("pull") + f.forge.count("ref") + f.forge.count("commit")
	}
	admit := func(t *testing.T, kc rhKindCase) *rhFixture {
		t.Helper()
		f := newRHFixture(t)
		f.forgeAt(rhH2, rhH2)
		w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
		f.assertRowAt(kc, w, rhH2, rhPRBranch)
		f.forgeAt(rhH3, rhH3)
		return f
	}
	for _, kc := range rhAllKinds {
		t.Run("replay/"+kc.name, func(t *testing.T) {
			f := admit(t, kc)
			before := forgeReads(f)
			w := f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			f.assertRowAt(kc, w, rhH2, rhPRBranch)
			if after := forgeReads(f); after != before {
				t.Errorf("assert: forge reads went from %d to %d, want no read on a replay", before, after)
			}
		})
	}
	t.Run("conflict_is_stage_keyed", func(t *testing.T) {
		f := admit(t, rhConflictKind)
		w := f.post(rhConflictKind, rhOther, rhBaseSHA, rhPRBranch)
		f.assertRowAt(rhConflictKind, w, rhH2, rhPRBranch)
	})
	for _, kc := range []rhKindCase{rhFixupKind, rhAcceptanceKind} {
		t.Run("new_head_is_checked/"+kc.name, func(t *testing.T) {
			f := admit(t, kc)
			w := f.post(kc, rhOther, rhBaseSHA, rhPRBranch)
			if w.Code != http.StatusConflict {
				t.Errorf("assert: status = %d, want 409: a new head is not a replay:\n%s", w.Code, w.Body.String())
			}
			if rows := f.rowPayloads(kc.category); len(rows) != 1 || rows[0]["head_sha"] != rhH2 {
				t.Errorf("assert: %s rows = %+v, want only the admitted head", kc.category, rows)
			}
		})
	}
}

// TestReportedHead_InvariantAppendFailureStillRefuses: evidence is best-effort. The
// refusal stands when either evidence append fails.
//
// COUNTERFACTUAL (M23, a failed invariant append returning false): the handler then
// continues past the hook and the row lands, so zero-rows goes RED.
func TestReportedHead_InvariantAppendFailureStillRefuses(t *testing.T) {
	cases := []struct {
		name          string
		appendErr     string
		kind          rhKindCase
		wantStage     run.StageState
		wantEvidence  int
		wantFailedRow int
	}{
		{"invariant_violation/fixup", invariantmonitor.CategoryInvariantViolation, rhFixupKind, run.StageStateFailed, 0, 1},
		{"invariant_violation/conflict", invariantmonitor.CategoryInvariantViolation, rhConflictKind, run.StageStateFailed, 0, 1},
		{"invariant_violation/acceptance", invariantmonitor.CategoryInvariantViolation, rhAcceptanceKind, run.StageStateRunning, 0, 0},
		{"pull_request_failed/fixup", "pull_request_failed", rhFixupKind, run.StageStateFailed, 1, 0},
		{"pull_request_failed/conflict", "pull_request_failed", rhConflictKind, run.StageStateFailed, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH1, rhH1)
			f.au.appendErrCategory = c.appendErr

			w := f.post(c.kind, rhH2, rhBaseSHA, rhPRBranch)

			if c.kind == rhAcceptanceKind {
				if w.Code != http.StatusConflict {
					t.Errorf("assert: status = %d, want 409:\n%s", w.Code, w.Body.String())
				}
			} else {
				var resp pullRequestFailureResponse
				_ = json.Unmarshal(w.Body.Bytes(), &resp)
				if w.Code != http.StatusOK || resp.Outcome != "failed" || resp.Category != "C" {
					t.Errorf("assert: status = %d outcome = %q category = %q, want 200 failed C:\n%s",
						w.Code, resp.Outcome, resp.Category, w.Body.String())
				}
			}
			if rows := f.rowPayloads(c.kind.category); len(rows) != 0 {
				t.Errorf("assert: %s rows = %d, want 0", c.kind.category, len(rows))
			}
			if got := f.stageState(f.reportingStage(c.kind).ID).State; got != c.wantStage {
				t.Errorf("assert: stage state = %s, want %s", got, c.wantStage)
			}
			if rows := f.mismatchRows(); len(rows) != c.wantEvidence {
				t.Errorf("assert: reported_head_mismatch rows = %d, want %d", len(rows), c.wantEvidence)
			}
			if rows := f.rowPayloads("pull_request_failed"); len(rows) != c.wantFailedRow {
				t.Errorf("assert: pull_request_failed rows = %d, want %d", len(rows), c.wantFailedRow)
			}
		})
	}
}

// TestReportedHead_EvidenceAppendWarnNamesRunStageAndHeads: when an evidence append
// fails, its WARN is the fallback record, so it names the run, stage and both heads.
//
// COUNTERFACTUALS: M39 (evidenceAttrs dropped from the evidence WARN) reddens the
// invariant_violation subtests. M40 (appendFailureAttrs nil) leaves the failure
// path's WARN with only the run and stage, reddening the pull_request_failed one.
func TestReportedHead_EvidenceAppendWarnNamesRunStageAndHeads(t *testing.T) {
	cases := []struct {
		name      string
		appendErr string
		kind      rhKindCase
		msg       string
	}{
		{"invariant_violation/fixup", invariantmonitor.CategoryInvariantViolation, rhFixupKind, "reported head: evidence append failed"},
		{"invariant_violation/acceptance", invariantmonitor.CategoryInvariantViolation, rhAcceptanceKind, "reported head: evidence append failed"},
		{"pull_request_failed/fixup", "pull_request_failed", rhFixupKind, "pull-request failure report: append audit entry failed"},
		{"pull_request_failed/conflict", "pull_request_failed", rhConflictKind, "pull-request failure report: append audit entry failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH1, rhH3)
			f.au.appendErrCategory = c.appendErr

			f.post(c.kind, rhH2, rhBaseSHA, rhPRBranch)

			rec := f.warnRecord(c.msg)
			if rec == nil {
				t.Fatalf("assert: no WARN %q in:\n%s", c.msg, f.logs.String())
			}
			want := map[string]string{
				"run_id":        f.runRow.ID.String(),
				"stage_id":      f.reportingStage(c.kind).ID.String(),
				"reported_head": rhH2,
				"forge_pr_head": rhH1,
				"forge_tip":     rhH3,
			}
			for key, value := range want {
				if rec[key] != value {
					t.Errorf("assert: WARN %s = %v, want %s", key, rec[key], value)
				}
			}
		})
	}
}

// seedReview adds a parked review stage to tell a recovery's anchors apart.
func (f *rhFixture) seedReview(seq int) *run.Stage {
	f.t.Helper()
	st := f.rr.seedStage(f.runRow.ID, seq, run.StageStatePending)
	st.Type = run.StageTypeReview
	return st
}

// seedFixupTriggerAnchored records a fix-up trigger whose anchors differ from every
// other recovery's: awaiting_approval and its own review stage.
func (f *rhFixture) seedFixupTriggerAnchored(seq int64, review *run.Stage) {
	f.t.Helper()
	f.seed(CategoryStageFixupTriggered, f.impl.ID, seq, map[string]any{
		"stage_id": f.impl.ID.String(), "prior_state": string(run.StageStateAwaitingApproval),
		"reparked_review_stage_id": review.ID.String(), "concern_ids": []string{}, "pass_ordinal": 1,
	})
}

// seedConflictTriggerAnchored records a live conflict trigger naming a review stage.
func (f *rhFixture) seedConflictTriggerAnchored(seq int64, review *run.Stage) {
	f.t.Helper()
	f.seed(CategoryStageConflictResolutionTriggered, f.impl.ID, seq, conflictResolutionTrigger{
		Branch: rhPRBranch, BaseRef: rhBaseRef, ExpectedHeadSHA: rhH1, Pass: 1,
		PriorState: string(run.StageStateSucceeded), ReparkedReviewStageID: review.ID.String(),
	})
}

// TestReportedHead_RecoveryOrdering: a refusal keeps the existing order of the
// recovery arms. Each subtest's triggers restore to different stages, so a wrong
// arm leaves a visible mark.
//
// COUNTERFACTUAL (M38, the conflict and fix-up arms swapped): in
// live_conflict_live_fixup the fix-up anchors restore first, so the implement state,
// both review stages and the row counts go RED.
func TestReportedHead_RecoveryOrdering(t *testing.T) {
	assertConflictRecovered := func(t *testing.T, f *rhFixture, stale *run.Stage) {
		t.Helper()
		if rows := f.rowPayloads(CategoryStageConflictResolutionFailed); len(rows) != 1 {
			t.Errorf("assert: stage_conflict_resolution_failed rows = %d, want 1", len(rows))
		}
		if rows := f.rowPayloads(CategoryStageFixupRecovered); len(rows) != 0 {
			t.Errorf("assert: stage_fixup_recovered rows = %d, want 0", len(rows))
		}
		f.assertRestored() // the conflict anchors: implement succeeded, f.review parked
		if got := f.stageState(stale.ID).State; got != run.StageStatePending {
			t.Errorf("assert: the fix-up anchor's review stage = %s, want pending (unchanged)", got)
		}
	}
	t.Run("live_conflict_spent_fixup", func(t *testing.T) {
		f := newRHFixture(t)
		stale := f.seedReview(8)
		f.seedFixupTriggerAnchored(10, stale)
		f.seed(CategoryFixupPushed, f.impl.ID, 11, map[string]any{"head_sha": rhOther})
		f.seedConflictTriggerAnchored(12, f.review)
		f.forgeAt(rhH1, rhH1)

		w := f.post(rhConflictKind, rhH2, rhBaseSHA, rhPRBranch)

		f.assertRefused(rhConflictKind, w, "C", reportedHeadReasonHead)
		if got := f.failedRow()["category"]; got != "C" {
			t.Errorf("assert: pull_request_failed category = %v, want C", got)
		}
		assertConflictRecovered(t, f, stale)
	})
	t.Run("live_conflict_live_fixup", func(t *testing.T) {
		f := newRHFixture(t)
		stale := f.seedReview(8)
		f.seedFixupTriggerAnchored(10, stale)
		f.seedConflictTriggerAnchored(11, f.review)
		f.forgeAt(rhH1, rhH1)

		w := f.post(rhConflictKind, rhH2, rhBaseSHA, rhPRBranch)

		f.assertRefused(rhConflictKind, w, "B", reportedHeadReasonHead)
		if got := f.failedRow()["category"]; got != "B" {
			t.Errorf("assert: pull_request_failed category = %v, want B", got)
		}
		assertConflictRecovered(t, f, stale)
	})
	t.Run("live_verify_pass", func(t *testing.T) {
		f := newRHFixture(t)
		staleFixup, staleConflict := f.seedReview(8), f.seedReview(9)
		f.seedFixupTriggerAnchored(10, staleFixup)
		f.seedConflictTriggerAnchored(11, staleConflict)
		f.seedVerifyTrigger(12, 0)
		f.forgeAt(rhH1, rhH1)

		w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)

		f.assertRefused(rhFixupKind, w, "B", reportedHeadReasonHead)
		rows := f.rowPayloads(CategoryMergeCandidateVerified)
		if len(rows) != 1 || rows[0]["result"] != mergeCandidateResultNotExecuted {
			t.Errorf("assert: merge_candidate_verified rows = %+v, want one not_executed", rows)
		}
		for _, category := range []string{CategoryStageConflictResolutionFailed, CategoryStageFixupRecovered} {
			if rows := f.rowPayloads(category); len(rows) != 0 {
				t.Errorf("assert: %s rows = %d, want 0: the verify recovery settles first", category, len(rows))
			}
		}
		f.assertRestored() // the verify anchors
		for _, stale := range []*run.Stage{staleFixup, staleConflict} {
			if got := f.stageState(stale.ID).State; got != run.StageStatePending {
				t.Errorf("assert: another trigger's review stage = %s, want pending (unchanged)", got)
			}
		}
	})
}

// TestReportedHead_FixupPushed_503ThenFailedC_Recovers: the cost of an outage on a
// fix-up. The forge cannot confirm the head, so nothing is recorded; the runner then
// reports failed category C after exhaustion and the run returns to its gate, still
// with no row, the pass delivering nothing.
func TestReportedHead_FixupPushed_503ThenFailedC_Recovers(t *testing.T) {
	f := newRHFixture(t)
	f.seedFixupTrigger(10)
	f.forge.set(func(g *rhForge) { g.status["pull"] = http.StatusInternalServerError })

	w := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
	f.assertUnverifiable(rhFixupKind, w, f.impl, run.StageStateRunning)

	body, err := json.Marshal(map[string]any{
		"outcome": "failed", "category": "C", "reason": "push report: retries exhausted",
	})
	if err != nil {
		t.Fatalf("setup: marshal failed report: %v", err)
	}
	priv, _ := f.sf.issue(t, f.runRow.ID)
	failed := shipPRRequest(t, f.s, f.runRow.ID, f.impl.ID, priv, body, "")
	f.s.waitBackgroundReviews()

	if failed.Code != http.StatusOK {
		t.Errorf("assert: failed report status = %d, want 200:\n%s", failed.Code, failed.Body.String())
	}
	f.assertRestored()
	if rows := f.rowPayloads(CategoryFixupPushed); len(rows) != 0 {
		t.Errorf("assert: fixup_pushed rows = %d, want 0: the pushed commit stays unrecorded", len(rows))
	}
	recovered := f.rowPayloads(CategoryStageFixupRecovered)
	if len(recovered) != 1 || recovered[0]["source_failure_category"] != "C" || recovered[0]["delivered_nothing"] != true {
		t.Errorf("assert: recovery rows = %+v, want one with source C and delivered_nothing", recovered)
	}
	if rows := f.mismatchRows(); len(rows) != 0 {
		t.Errorf("assert: reported_head_mismatch rows = %d, want 0: an outage is not a mismatch", len(rows))
	}
}

// TestReportedHead_RefusedPush_NoAutoVouch_ReadsForeignLater: the cost of a refusal
// (ADR-141 D5). The refused commit stays on the branch with no row and nothing vouches
// it, so a later fix-up whose own head passes the check still has the lineage guard
// flag it. The remedy is an operator's vouch or reset.
//
// COUNTERFACTUAL (M49, the refusal appending a vouch row): the ledger then admits
// H2, the guard passes and the later row lands, so the vouch and guard assertions go
// RED.
func TestReportedHead_RefusedPush_NoAutoVouch_ReadsForeignLater(t *testing.T) {
	f := newRHFixture(t)
	f.seedFixupTrigger(10)
	f.forgeAt(rhH1, rhH1)

	refused := f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
	f.assertRefused(rhFixupKind, refused, "B", reportedHeadReasonHead)
	f.assertRestored()

	// A later pass pushes H3 on top of the commit the refusal left on the branch.
	f.seedFixupTrigger(20)
	f.impl.State = run.StageStateRunning
	f.forgeAt(rhH3, rhH3)
	f.forge.set(func(g *rhForge) {
		g.compare[rhBaseRef] = []string{rhH2, rhH3}
		g.compare[rhBaseSHA] = []string{rhH2, rhH3}
	})

	later := f.post(rhFixupKind, rhH3, rhBaseSHA, rhPRBranch)

	if rows := f.rowPayloads(CategoryOperatorCommitVouched); len(rows) != 0 {
		t.Errorf("assert: operator_commit_vouched rows = %d, want 0: nothing vouches automatically", len(rows))
	}
	if rows := f.rowPayloads(CategoryFixupPushed); len(rows) != 0 {
		t.Errorf("assert: fixup_pushed rows = %+v, want 0: the lineage guard fails the later pass\n%s", rows, later.Body.String())
	}
	st := f.stageState(f.impl.ID)
	if st.State != run.StageStateFailed || st.FailureCategory == nil || *st.FailureCategory != run.FailureB {
		t.Errorf("assert: implement = %s (%v), want failed B from the lineage guard", st.State, st.FailureCategory)
	}
	if st.FailureReason == nil || !strings.Contains(*st.FailureReason, rhH2) {
		t.Errorf("assert: failure reason = %v, want it to name %s", st.FailureReason, rhH2)
	}
	var foreign []map[string]any
	for _, row := range f.rowPayloads(invariantmonitor.CategoryInvariantViolation) {
		if row["kind"] == invariantmonitor.KindForeignCommitOnBranch {
			foreign = append(foreign, row)
		}
	}
	if len(foreign) != 1 || foreign[0]["offending_sha"] != rhH2 {
		t.Errorf("assert: foreign_commit_on_branch rows = %+v, want one naming %s", foreign, rhH2)
	}
}

// assertNeutral pins one refusal surface: it names both heads, states the
// disagreement and calls neither the commit nor the report false.
func assertNeutral(t *testing.T, surface, text string) {
	t.Helper()
	if !strings.Contains(text, rhH2) || !strings.Contains(text, rhH1) {
		t.Errorf("assert: %s does not name both heads: %q", surface, text)
	}
	if !strings.Contains(text, "disagreed with forge state") {
		t.Errorf("assert: %s does not state the disagreement: %q", surface, text)
	}
	lower := strings.ToLower(text)
	for _, word := range []string{"foreign", "forged", "tamper", "misconduct"} {
		if strings.Contains(lower, word) {
			t.Errorf("assert: %s contains %q: %q", surface, word, text)
		}
	}
}

// TestReportedHead_RefusalTextIsNeutral: a refusal can be a benign head-movement
// race, so no surface states more than a disagreement.
//
// COUNTERFACTUAL (M48, the text changed to call the commit foreign): every surface
// goes RED on its statement of the disagreement and on the word itself.
func TestReportedHead_RefusalTextIsNeutral(t *testing.T) {
	for _, kc := range []rhKindCase{rhFixupKind, rhConflictKind} {
		t.Run("failed_stage/"+kc.name, func(t *testing.T) {
			f := newRHFixture(t)
			f.forgeAt(rhH1, rhH1)
			f.post(kc, rhH2, rhBaseSHA, rhPRBranch)
			st := f.stageState(f.impl.ID)
			if st.FailureReason == nil {
				t.Fatalf("assert: stage %s carries no failure reason", st.State)
			}
			assertNeutral(t, "stage failure reason", *st.FailureReason)
			reason, _ := f.failedRow()["reason"].(string)
			assertNeutral(t, "pull_request_failed reason", reason)
		})
	}
	t.Run("recovered_stage/fixup", func(t *testing.T) {
		f := newRHFixture(t)
		f.seedFixupTrigger(10)
		f.forgeAt(rhH1, rhH1)
		f.post(rhFixupKind, rhH2, rhBaseSHA, rhPRBranch)
		reason, _ := f.failedRow()["reason"].(string)
		assertNeutral(t, "pull_request_failed reason", reason)
	})
	t.Run("conflict_409/acceptance", func(t *testing.T) {
		f := newRHFixture(t)
		f.forgeAt(rhH1, rhH1)
		w := f.post(rhAcceptanceKind, rhH2, rhBaseSHA, rhPRBranch)
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || w.Code != http.StatusConflict {
			t.Fatalf("assert: status = %d (%v), want a 409 envelope:\n%s", w.Code, err, w.Body.String())
		}
		assertNeutral(t, "409 message", env.Error.Message)
	})
}
