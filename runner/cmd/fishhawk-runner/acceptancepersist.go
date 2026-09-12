package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/scenario"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

/*
 * Acceptance scenario-corpus persistence (E72.4 / #3328, slice C).
 *
 * After the acceptance verdict ships, the runner records the pass as
 * replayable scenarios and folds the run's approved retirements into the
 * ledger, then commits ONLY acceptance/scenarios/** onto the run branch:
 *
 *   persistAcceptanceScenarios — Compose+Write on a passed verdict, MergeRetired
 *       on every verdict, then a scenario-only commit pushed to
 *       fetched.AcceptanceRunBranch. Every failure is BEST-EFFORT (the verdict
 *       outcome never changes); the result names the exit path so the drop
 *       reporter can carry it.
 *   retirementDropReporter — the single deferred reporter armed the moment the
 *       prompt fetch returns served retirements (BEFORE the target gate and
 *       provisionAcceptanceTree — binding condition 1) and disarmed ONLY by a
 *       successful acceptance_scenarios_pushed report (or by proving every
 *       served id already sits in the ledger at HEAD). On every other exit it
 *       ships outcome acceptance_scenario_retirement_dropped so an approved
 *       retirement can never vanish silently.
 *   scenarioRemovalGuard — pre-spawn category-B refusal of a scenario file
 *       deleted or modified without a ledger entry, and of a ledger entry not
 *       backed by this run's approval.
 */

// Persist outcomes (acceptance_scenarios_persisted event `outcome`).
const (
	persistPushed  = "persist_pushed"
	persistSkipped = "persist_skipped"
	persistRefused = "persist_refused"
	persistFailed  = "persist_failed"
)

// retirementDropDefaultReason is the reason the deferred reporter ships when
// no specific exit path noted one — the stage returned between the prompt
// fetch and the persist step (a pre-spawn guard, an agent failure, a trace or
// verdict upload failure).
const retirementDropDefaultReason = "stage_exited_before_persist"

// retirementDropReportTimeout bounds the best-effort drop report: it runs from
// a deferred call on a context that may already be cancelled.
const retirementDropReportTimeout = 30 * time.Second

// retirementDropReporter is the deferred acceptance_scenario_retirement_dropped
// reporter (fix 4). nil-safe: every method is a no-op on a nil receiver so the
// unarmed (no served retirements) path costs nothing.
type retirementDropReporter struct {
	entries  []scenario.RetiredEntry
	reason   string
	disarmed bool
	// disarmReason is logged with the disarm so the trail says WHY nothing was
	// reported (pushed_and_reported | already_ledgered).
	disarmReason string
}

// newRetirementDropReporter arms the reporter for the served entries; nil when
// nothing was served (so every method is a no-op).
func newRetirementDropReporter(entries []scenario.RetiredEntry) *retirementDropReporter {
	if len(entries) == 0 {
		return nil
	}
	return &retirementDropReporter{entries: entries, reason: retirementDropDefaultReason}
}

// note records the exit path the drop report will carry. The last note wins;
// call sites are the guard, the verdict capture/validation branches and the
// persist outcomes, none of which follow one another on the same exit.
func (r *retirementDropReporter) note(reason string) {
	if r == nil || reason == "" {
		return
	}
	r.reason = reason
}

// disarm marks the retirements as persisted so report ships nothing.
func (r *retirementDropReporter) disarm(reason string) {
	if r == nil {
		return
	}
	r.disarmed = true
	r.disarmReason = reason
}

// report is the deferred call: ships outcome
// acceptance_scenario_retirement_dropped {retired, reason} best-effort and
// logs acceptance_scenario_retirement_dropped. It uses a fresh bounded
// context detached from ctx's cancellation because it runs on the way out of
// run(). A nil client or unissued key (the fetch never completed) logs the
// drop without a report — nothing to sign with.
func (r *retirementDropReporter) report(ctx context.Context, client uploadClient, cfg config, key *upload.IssuedKey, logSink io.Writer) {
	if r == nil || r.disarmed || len(r.entries) == 0 {
		if r != nil && r.disarmed {
			logEvent(logSink, "acceptance_scenario_retirement_persisted", map[string]string{
				"run_id": cfg.runID, "stage_id": cfg.stageID, "how": r.disarmReason,
				"retired_ids": strings.Join(retiredIDs(r.entries), ","),
			})
		}
		return
	}
	ids := strings.Join(retiredIDs(r.entries), ",")
	logEvent(logSink, "acceptance_scenario_retirement_dropped", map[string]string{
		"run_id": cfg.runID, "stage_id": cfg.stageID, "reason": r.reason, "retired_ids": ids,
	})
	if client == nil || key == nil {
		logEvent(logSink, "acceptance_scenario_retirement_drop_unreported", map[string]string{
			"run_id": cfg.runID, "stage_id": cfg.stageID, "detail": "no upload client or unissued signing key",
		})
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retirementDropReportTimeout)
	defer cancel()
	if _, err := client.ShipPullRequest(rctx, upload.ShipPullRequestArgs{
		RunID:            cfg.runID,
		StageID:          cfg.stageID,
		PrivateKey:       key.PrivateKey,
		Outcome:          upload.OutcomeAcceptanceScenarioRetirementDropped,
		RetiredScenarios: r.entries,
		Reason:           r.reason,
	}); err != nil {
		logEvent(logSink, "acceptance_scenario_retirement_drop_report_failed", map[string]string{
			"run_id": cfg.runID, "stage_id": cfg.stageID, "detail": err.Error(),
		})
		return
	}
	logEvent(logSink, "acceptance_scenario_retirement_drop_reported", map[string]string{
		"run_id": cfg.runID, "stage_id": cfg.stageID, "reason": r.reason, "retired_ids": ids,
	})
}

func retiredIDs(entries []scenario.RetiredEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// acceptancePersistInputs is everything persistAcceptanceScenarios reads.
type acceptancePersistInputs struct {
	// treeDir is the provisioned acceptance tree (acceptanceTreePath) — the
	// detached merge-candidate checkout the scenario commit is made in.
	treeDir string
	// runBranch / remoteURL / pushToken address the push. An empty runBranch
	// (older backend, ledger gap) or remoteURL (no owner/name) skips.
	runBranch string
	remoteURL string
	pushToken string
	// issue is the trigger issue number (scenario id key); 0 skips recording.
	issue int
	// prNumber is the run's PR (0 = unknown, recorded as-is, NEVER the issue).
	prNumber int
	headSHA  string
	runID    string
	criteria []upload.AcceptanceCriterionEntry
	retired  []scenario.RetiredEntry
	// verdict is the VALIDATED (coerced), REDACTED verdict bytes — the same
	// bytes that shipped, so no unredacted prose lands in the repository.
	verdict     []byte
	authorName  string
	authorEmail string
	now         func() time.Time
}

// acceptancePersistResult is the persist step's outcome for the caller's
// report + drop-reporter decisions.
type acceptancePersistResult struct {
	outcome string
	reason  string
	// headSHA / baseSHA are the pushed commit coordinates (persistPushed only).
	headSHA string
	baseSHA string
	// scenarioIDs are the ids written this pass (persistPushed only).
	scenarioIDs []string
	// retirementsLedgered is true when every served retirement is already in
	// retired.yaml at HEAD — a re-run after a prior successful persist — so
	// the drop reporter may disarm even though nothing was pushed this pass.
	retirementsLedgered bool
}

// persistAcceptanceScenarios records the pass and merges the retirements,
// then commits and pushes acceptance/scenarios/** to the run branch. It never
// returns an error: every branch resolves to an acceptancePersistResult
// naming the exit path, and the caller keeps the verdict outcome unchanged.
func persistAcceptanceScenarios(ctx context.Context, in acceptancePersistInputs, p pusher, logSink io.Writer) acceptancePersistResult {
	skip := func(reason string) acceptancePersistResult {
		return acceptancePersistResult{outcome: persistSkipped, reason: reason}
	}
	fail := func(reason string) acceptancePersistResult {
		return acceptancePersistResult{outcome: persistFailed, reason: reason}
	}
	if in.now == nil {
		in.now = time.Now
	}
	if in.treeDir == "" || !isGitWorkTree(ctx, in.treeDir) {
		return skip("no_acceptance_tree")
	}
	if in.runBranch == "" {
		return skip("no_run_branch")
	}
	corpusDir := filepath.Join(in.treeDir, filepath.FromSlash(scenario.CorpusDir))

	// (a) Verdict passed → Compose + Write one scenario per drivable criterion
	// whose row passed. A missing issue number cannot key the id, so it records
	// nothing (never the PR number) — the retirement merge below still runs.
	var v acceptanceVerdict
	if err := json.Unmarshal(in.verdict, &v); err != nil {
		return fail("verdict_undecodable: " + err.Error())
	}
	var written []string
	if v.Verdict == "passed" && in.issue > 0 {
		rows, _, err := coerceAcceptanceCriteria(v.Criteria)
		if err != nil {
			return fail("verdict_criteria_undecodable: " + err.Error())
		}
		results := make([]scenario.CriterionResult, 0, len(rows))
		for _, r := range rows {
			results = append(results, scenario.CriterionResult{
				ID: r.ID, Result: r.Result, StepsTaken: r.StepsTaken,
				Expected: r.Expected, Observed: r.Observed, ReproHandle: r.ReproHandle,
			})
		}
		crits := make([]scenario.Criterion, 0, len(in.criteria))
		for _, c := range in.criteria {
			crits = append(crits, scenario.Criterion{
				ID: c.ID, Statement: c.Statement, VerifyHint: c.VerifyHint,
				Preconditions: c.Preconditions, Drivable: c.Drivable,
			})
		}
		origin := scenario.Origin{
			Issue: in.issue, PR: in.prNumber, RunID: in.runID, HeadSHA: in.headSHA,
			RecordedAt: in.now().UTC().Truncate(time.Second),
		}
		for _, s := range scenario.Compose(crits, results, origin) {
			if _, err := scenario.Write(corpusDir, s); err != nil {
				return fail("scenario_write: " + err.Error())
			}
			written = append(written, s.ID)
		}
	} else if v.Verdict == "passed" && in.issue == 0 {
		logEvent(logSink, "acceptance_scenario_record_skipped", map[string]string{
			"run_id": in.runID, "reason": "no_issue_number",
		})
	}

	// (b) Regardless of verdict, merge the served FULL entries into the ledger,
	// reason preserved (MergeRetired keeps an existing entry whole on a
	// duplicate id). Write only when the merge grew the ledger.
	ledgered := false
	if len(in.retired) > 0 {
		existing, err := scenario.LoadRetired(corpusDir)
		if err != nil {
			return fail("retired_ledger_unreadable: " + err.Error())
		}
		merged := scenario.MergeRetired(existing, in.retired)
		if len(merged) > len(existing) {
			if err := scenario.WriteRetired(corpusDir, merged); err != nil {
				return fail("retired_ledger_write: " + err.Error())
			}
		} else {
			ledgered = true
		}
	}

	// (c)/(d) The dirty set must be confined to acceptance/scenarios/**: the
	// tree is a pristine detached checkout, so anything else dirty means a
	// foreign write landed in it and the commit is REFUSED rather than
	// carrying it onto the run branch. Then stage the corpus dir only and
	// re-assert on the staged set.
	dirty, err := gitPorcelainPaths(ctx, in.treeDir)
	if err != nil {
		return fail("status: " + err.Error())
	}
	if stray := outsideCorpus(dirty); len(stray) > 0 {
		return acceptancePersistResult{outcome: persistRefused,
			reason: "dirty_outside_corpus: " + strings.Join(stray, ","), retirementsLedgered: ledgered}
	}
	if len(dirty) == 0 {
		return acceptancePersistResult{outcome: persistSkipped, reason: "unchanged", retirementsLedgered: ledgered}
	}
	if out, err := gitTree(ctx, in.treeDir, "add", "-A", "--", scenario.CorpusDir); err != nil {
		return fail("add: " + strings.TrimSpace(out))
	}
	stagedOut, err := gitTree(ctx, in.treeDir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return fail("diff --cached: " + strings.TrimSpace(stagedOut))
	}
	staged := splitNUL([]byte(stagedOut))
	if stray := outsideCorpus(staged); len(stray) > 0 {
		return acceptancePersistResult{outcome: persistRefused,
			reason: "staged_outside_corpus: " + strings.Join(stray, ","), retirementsLedgered: ledgered}
	}
	if len(staged) == 0 {
		return acceptancePersistResult{outcome: persistSkipped, reason: "unchanged", retirementsLedgered: ledgered}
	}

	// (e) Commit with the run's author (per-invocation -c, never a config
	// write into the shared admin dir) under the hardened git config, then
	// push PINNED to the new head. PushCommittedBranch is a plain (no-force)
	// push: the commit's parent is the merge-candidate head the branch tip is
	// expected to be, so a tip that moved makes the push non-fast-forward and
	// it fails here rather than overwriting a foreign commit.
	baseOut, err := gitTree(ctx, in.treeDir, "rev-parse", "HEAD")
	if err != nil {
		return fail("rev-parse HEAD: " + strings.TrimSpace(baseOut))
	}
	baseSHA := strings.TrimSpace(baseOut)
	authorName, authorEmail := in.authorName, in.authorEmail
	if authorName == "" {
		authorName = gitops.DefaultAuthorName
	}
	if authorEmail == "" {
		authorEmail = gitops.DefaultAuthorEmail
	}
	msg := fmt.Sprintf("chore(acceptance): record scenario corpus for run %s\n\nScenarios recorded: %d. Retirements merged: %d.\n",
		in.runID, len(written), len(in.retired))
	commitArgs := append(gitops.HardeningArgs(),
		"-c", "user.name="+authorName, "-c", "user.email="+authorEmail,
		"commit", "--signoff", "-m", msg)
	if out, err := gitTree(ctx, in.treeDir, commitArgs...); err != nil {
		return fail("commit: " + strings.TrimSpace(out))
	}
	headOut, err := gitTree(ctx, in.treeDir, "rev-parse", "HEAD")
	if err != nil {
		return fail("rev-parse new HEAD: " + strings.TrimSpace(headOut))
	}
	headSHA := strings.TrimSpace(headOut)
	if in.remoteURL == "" {
		return acceptancePersistResult{outcome: persistSkipped, reason: "no_remote", baseSHA: baseSHA, headSHA: headSHA}
	}
	if _, err := p.PushCommittedBranch(ctx, gitops.PushCommittedBranchArgs{
		RepoDir: in.treeDir, Branch: in.runBranch, RemoteURL: in.remoteURL,
		PushToken: in.pushToken, HeadSHA: headSHA,
	}); err != nil {
		return acceptancePersistResult{outcome: persistFailed, reason: "push: " + err.Error(), baseSHA: baseSHA, headSHA: headSHA}
	}
	sort.Strings(written)
	return acceptancePersistResult{outcome: persistPushed, headSHA: headSHA, baseSHA: baseSHA, scenarioIDs: written, retirementsLedgered: true}
}

// gitTree runs git in dir and returns its combined output.
func gitTree(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

// gitPorcelainPaths returns every dirty path (`status --porcelain -uall -z`),
// rename records reduced to their new path.
func gitPorcelainPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := gitTree(ctx, dir, "status", "--porcelain", "-uall", "-z")
	if err != nil {
		return nil, errors.New(strings.TrimSpace(out))
	}
	recs := splitNUL([]byte(out))
	var paths []string
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if len(rec) < 4 {
			continue
		}
		paths = append(paths, rec[3:])
		if rec[0] == 'R' || rec[0] == 'C' {
			i++ // the origin path follows as its own NUL record
		}
	}
	return paths, nil
}

// outsideCorpus returns the paths not under acceptance/scenarios/.
func outsideCorpus(paths []string) []string {
	var out []string
	for _, p := range paths {
		if !strings.HasPrefix(p, scenario.CorpusDir+"/") {
			out = append(out, p)
		}
	}
	return out
}

// Removal-guard categories (pre-spawn category-B runner_failed reasons).
const (
	guardRemovedWithoutRetirement = "acceptance_scenario_removed_without_retirement"
	guardRetirementUnledgered     = "acceptance_scenario_retirement_unledgered"
)

// scenarioRemovalGuard inspects `git diff --name-status <merge-base>..HEAD --
// acceptance/scenarios/` in treeDir and returns a non-empty category-B reason
// (plus detail) when:
//
//   - a scenario file was DELETED or MODIFIED and its id is in neither
//     retired.yaml@HEAD nor the served retirement set
//     (acceptance_scenario_removed_without_retirement) — a retirement is the
//     only sanctioned way a scenario leaves the corpus;
//   - retired.yaml gained an entry at HEAD whose id is not in the served set
//     (acceptance_scenario_retirement_unledgered) — a hand-edited ledger row
//     not backed by this run's approval.
//
// An unresolvable merge base (no base ref, unrelated histories) emits
// acceptance_scenario_guard_unresolved and returns "" — the stage proceeds.
// Scenario ids are derived from the corpus path (the inverse of
// scenario.PathFor, which is how every recorded file is named).
func scenarioRemovalGuard(ctx context.Context, treeDir, baseRef string, served []scenario.RetiredEntry, runID string, logSink io.Writer) (reason, detail string) {
	unresolved := func(why string) (string, string) {
		logEvent(logSink, "acceptance_scenario_guard_unresolved", map[string]string{
			"run_id": runID, "detail": why, "outcome": "guard_skipped",
		})
		return "", ""
	}
	if treeDir == "" || !isGitWorkTree(ctx, treeDir) {
		return unresolved("no acceptance tree")
	}
	mergeBase := ""
	for _, ref := range []string{"refs/remotes/origin/" + baseRef, "refs/heads/" + baseRef, baseRef} {
		out, err := gitTree(ctx, treeDir, "merge-base", "HEAD", ref)
		if err == nil && strings.TrimSpace(out) != "" {
			mergeBase = strings.TrimSpace(out)
			break
		}
	}
	if mergeBase == "" {
		return unresolved("merge base with " + baseRef + " unresolvable")
	}
	// --no-renames is load-bearing: with rename detection on, a deleted
	// scenario paired with a similar new file surfaces as R0xx and the
	// deletion would slip past the D check.
	diffOut, err := gitTree(ctx, treeDir, "diff", "--name-status", "--no-renames", "-z", mergeBase+"..HEAD", "--", scenario.CorpusDir+"/")
	if err != nil {
		return unresolved("diff: " + strings.TrimSpace(diffOut))
	}
	recs := splitNUL([]byte(diffOut))

	allowed := make(map[string]bool, len(served))
	for _, e := range served {
		allowed[e.ID] = true
	}
	ledgerPath := scenario.CorpusDir + "/" + scenario.RetiredFile
	headLedger, err := ledgerAt(ctx, treeDir, "HEAD", ledgerPath)
	if err != nil {
		return guardRetirementUnledgered, "retired.yaml at HEAD unreadable: " + err.Error()
	}
	for _, e := range headLedger {
		allowed[e.ID] = true
	}

	ledgerChanged := false
	for i := 0; i < len(recs); i++ {
		status := recs[i]
		if i+1 >= len(recs) {
			break
		}
		i++
		path := recs[i]
		if path == ledgerPath {
			ledgerChanged = true
			continue
		}
		if status == "" || (status[0] != 'D' && status[0] != 'M') {
			continue
		}
		id := scenarioIDForPath(path)
		if !allowed[id] {
			return guardRemovedWithoutRetirement, fmt.Sprintf("%s %s (id %s) is not in retired.yaml@HEAD or the served retirements", status[:1], path, id)
		}
	}
	if ledgerChanged {
		baseLedger, err := ledgerAt(ctx, treeDir, mergeBase, ledgerPath)
		if err != nil {
			return guardRetirementUnledgered, "retired.yaml at merge base unreadable: " + err.Error()
		}
		known := make(map[string]bool, len(baseLedger))
		for _, e := range baseLedger {
			known[e.ID] = true
		}
		servedIDs := make(map[string]bool, len(served))
		for _, e := range served {
			servedIDs[e.ID] = true
		}
		for _, e := range headLedger {
			if !known[e.ID] && !servedIDs[e.ID] {
				return guardRetirementUnledgered, fmt.Sprintf("retired.yaml entry %s is new at HEAD and not among this run's approved retirements", e.ID)
			}
		}
	}
	logEvent(logSink, "acceptance_scenario_guard_passed", map[string]string{
		"run_id": runID, "merge_base": mergeBase, "ledgered_ids": strings.Join(retiredIDs(headLedger), ","),
	})
	return "", ""
}

// ledgerAt decodes retired.yaml at rev (absent = empty ledger).
func ledgerAt(ctx context.Context, treeDir, rev, ledgerPath string) ([]scenario.RetiredEntry, error) {
	if _, err := gitTree(ctx, treeDir, "cat-file", "-e", rev+":"+ledgerPath); err != nil {
		return nil, nil
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", treeDir, "show", rev+":"+ledgerPath).Output()
	if err != nil {
		return nil, fmt.Errorf("show %s:%s: %w", rev, ledgerPath, err)
	}
	dir, err := os.MkdirTemp("", "fishhawk-ledger-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, scenario.RetiredFile), raw, 0o600); err != nil {
		return nil, err
	}
	return scenario.LoadRetired(dir)
}

// scenarioIDForPath inverts scenario.PathFor: acceptance/scenarios/issue-101/
// crit-b.yaml → scenario:issue-101/crit-b.
func scenarioIDForPath(path string) string {
	rel := strings.TrimPrefix(path, scenario.CorpusDir+"/")
	return scenario.IDPrefix + strings.TrimSuffix(rel, ".yaml")
}

// acceptancePersistRemoteURL resolves the push URL for the scenario commit —
// the same https://github.com/<owner>/<repo> form every other runner push
// uses. Empty when no owner/name is configured (persist_skipped no_remote).
// Test seam: overridden to a file-path bare remote.
var acceptancePersistRemoteURL = func(cfg config) string {
	repoSlug := cfg.githubRepo
	if repoSlug == "" {
		repoSlug = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repoName, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || repoName == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s", owner, repoName)
}

// acceptanceReplayInputs is the prompt-served replay input bundle
// (FetchedPrompt.Acceptance{RunBranch,PullRequestNumber,IssueNumber,Criteria,
// RetiredScenarios}), threaded out of fetchPromptToFile as ONE tuple element.
type acceptanceReplayInputs struct {
	runBranch string
	prNumber  int
	issue     int
	criteria  []upload.AcceptanceCriterionEntry
	retired   []scenario.RetiredEntry
}

func acceptanceReplayInputsFromPrompt(got *upload.FetchedPrompt) acceptanceReplayInputs {
	if got == nil {
		return acceptanceReplayInputs{}
	}
	return acceptanceReplayInputs{
		runBranch: got.AcceptanceRunBranch,
		prNumber:  got.AcceptancePullRequestNumber,
		issue:     got.AcceptanceIssueNumber,
		criteria:  got.AcceptanceCriteria,
		retired:   got.AcceptanceRetiredScenarios,
	}
}

// acceptanceServedVerdictIDs is the join-key set validateAcceptanceVerdict
// admits: the served criterion ids UNION the served replay scenario ids, so a
// scenario row is a member (and an unknown id still fails closed).
func acceptanceServedVerdictIDs(criteriaIDs []string, set scenario.ReplaySet) []string {
	out := append([]string(nil), criteriaIDs...)
	for _, sc := range set.Scenarios {
		out = append(out, sc.ScenarioID)
	}
	return out
}

// persistAndReportAcceptanceScenarios is the run() glue around
// persistAcceptanceScenarios: it resolves the push remote + token, runs the
// persist step against the provisioned acceptance tree, emits
// acceptance_scenarios_persisted, ships acceptance_scenarios_pushed on a push
// and settles the deferred drop reporter — disarmed ONLY by a successful
// report (or an already-ledgered retirement set); every other outcome is
// noted as the drop reason. Never changes the stage outcome.
func persistAndReportAcceptanceScenarios(ctx context.Context, cfg config, client uploadClient, issued *upload.IssuedKey,
	in acceptanceReplayInputs, headSHA string, verdict []byte, drop *retirementDropReporter, logSink io.Writer) {
	pin := acceptancePersistInputs{
		treeDir:     acceptanceTreePath(cfg.runID, cfg.stageID),
		runBranch:   in.runBranch,
		issue:       in.issue,
		prNumber:    in.prNumber,
		headSHA:     headSHA,
		runID:       cfg.runID,
		criteria:    in.criteria,
		retired:     in.retired,
		verdict:     verdict,
		authorName:  cfg.commitAuthorName,
		authorEmail: cfg.commitAuthorEmail,
	}
	// Resolve the remote + credential only when a push can happen: the token
	// mint is best-effort (mintBaseAuthToken degrades to ambient auth).
	if in.runBranch != "" {
		pin.remoteURL = acceptancePersistRemoteURL(cfg)
		if pin.remoteURL != "" {
			pin.pushToken = mintBaseAuthToken(ctx, cfg, client, issued, logSink)
		}
	}
	res := persistAcceptanceScenarios(ctx, pin, newPusher(), logSink)
	logEvent(logSink, "acceptance_scenarios_persisted", map[string]string{
		"run_id": cfg.runID, "stage_id": cfg.stageID, "outcome": res.outcome, "reason": res.reason,
		"head_sha": res.headSHA, "base_sha": res.baseSHA, "branch": in.runBranch,
		"scenario_ids": strings.Join(res.scenarioIDs, ","), "retired_ids": strings.Join(retiredIDs(in.retired), ","),
	})
	if res.outcome != persistPushed {
		if res.retirementsLedgered {
			drop.disarm("already_ledgered")
		} else {
			drop.note(res.outcome + ":" + res.reason)
		}
		return
	}
	if client == nil || issued == nil {
		drop.note("push_report_unsendable")
		return
	}
	if _, err := client.ShipPullRequest(ctx, upload.ShipPullRequestArgs{
		RunID:            cfg.runID,
		StageID:          cfg.stageID,
		PrivateKey:       issued.PrivateKey,
		Outcome:          upload.OutcomeAcceptanceScenariosPushed,
		Branch:           in.runBranch,
		HeadSHA:          res.headSHA,
		BaseSHA:          res.baseSHA,
		ScenarioIDs:      res.scenarioIDs,
		RetiredScenarios: in.retired,
	}); err != nil {
		drop.note("push_report_failed: " + err.Error())
		logEvent(logSink, "acceptance_scenarios_push_report_failed", map[string]string{
			"run_id": cfg.runID, "stage_id": cfg.stageID, "detail": err.Error(),
		})
		return
	}
	drop.disarm("pushed_and_reported")
	logEvent(logSink, "acceptance_scenarios_pushed", map[string]string{
		"run_id": cfg.runID, "stage_id": cfg.stageID, "branch": in.runBranch,
		"head_sha": res.headSHA, "base_sha": res.baseSHA,
		"scenario_ids": strings.Join(res.scenarioIDs, ","), "retired_ids": strings.Join(retiredIDs(in.retired), ","),
	})
}

// replaySetHasCorpus reports whether the loaded corpus held ANY scenario —
// served, sampled out, or excluded by a retirement. Only then is the `replay`
// object injected into the verdict; an empty/absent corpus leaves the body
// unchanged (backend records replay:null).
func replaySetHasCorpus(set scenario.ReplaySet) bool {
	return set.CorpusSize > 0 || set.RetiredExcluded > 0 || len(set.Scenarios) > 0
}
