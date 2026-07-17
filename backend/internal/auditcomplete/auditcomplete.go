// Package auditcomplete derives the state of the
// `fishhawk_audit_complete` blocking check (#229). The check fails
// when the audit story for a run isn't intact: a missing plan
// artifact, a missing trace bundle, a missing pull_request
// artifact, or a tampered/missing audit-chain link. The reviewer
// can't approve until everything Fishhawk claims to record actually
// landed.
//
// Scope:
//   - Read-only. Compute pulls from the run, artifact, and audit
//     repos, runs the same hash algorithm the verifier uses, and
//     returns a state without writing anything.
//   - Compute-on-read (per #229's recommendation). The review-stage
//     read endpoint and the approval-handler enforcement both call
//     Compute directly. No persistence layer; verification cost
//     is bounded (single-digit ms on a normal run's chain) and the
//     freshest state is always what the reviewer sees.
//
// Failure categorization:
//   - State=fail with a non-empty `missing` list → audit story is
//     broken, gate refuses approval, reviewer sees what to fix.
//   - State=pending → some non-review stages haven't terminated
//     yet, OR the only gaps are pending-flavored: a live PR-HEAD
//     fetch failure (head_fetch_failed), a dispatched agent
//     implement-review that hasn't landed yet (review_pending, the
//     #947 pre-merge presence gate), or a code-scanning signal we
//     couldn't read/decode (security_findings_unverified, the #1096
//     fail-open gate). We can't say "done"; the reviewer waits and
//     branch protection re-evaluates on the next publish.
//   - State=pass → every load-bearing artifact + audit entry is
//     present and the chain verifies.
package auditcomplete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// MissingKind names a category of audit-incompleteness. Stable,
// machine-readable; the SPA can localize / branch on it.
type MissingKind string

// MissingKind values.
const (
	MissingPlan          MissingKind = "plan_missing"        // plan stage didn't produce a standard_v1 artifact
	MissingTrace         MissingKind = "trace_missing"       // a non-review stage hasn't shipped both bundle variants
	MissingPullRequest   MissingKind = "pr_missing"          // implement stage didn't produce a pull_request artifact
	MissingChain         MissingKind = "chain_invalid"       // audit chain prev_hash → entry_hash links don't verify
	MissingChainBroken   MissingKind = "chain_unrecoverable" // chain read or hash recomputation errored
	MissingForeignCommit MissingKind = "foreign_commit"      // PR HEAD on GitHub isn't a Fishhawk-recorded head_sha (#282)
	MissingHeadFetchFail MissingKind = "head_fetch_failed"   // couldn't read PR HEAD from GitHub; result is pending, not fail (#282)
	// MissingReviewPending marks a configured agent implement-review (ADR-027)
	// that has not yet reached a terminal verdict (#947). Pending-flavored
	// like head_fetch_failed: a not-yet-landed review is "wait", not "broken",
	// so when it is the ONLY missing item the overall state is pending, not
	// fail. The pre-merge presence gate — fishhawk_audit_complete cannot go
	// green while a dispatched agent review is still in-flight. ADR-027's
	// advisory verdict stays non-blocking: ANY terminal kind clears this.
	MissingReviewPending MissingKind = "review_pending"
	// MissingSecurityFindings marks an unresolved high-severity code-scanning
	// (CodeQL/SAST) finding on the implement diff (#1096). A HARD gate: like
	// the review verdict and foreign_commit, its presence flips the overall
	// state to fail and holds the merge until the finding is resolved (routable
	// via fixup_stage) and a clean re-scan lands. The string value is bound to
	// securityscan.GateMissingKind so the cross-slice contract token lives ONCE
	// in the leaf package and is imported here unchanged.
	MissingSecurityFindings MissingKind = MissingKind(securityscan.GateMissingKind) // "security_findings_unresolved"
	// MissingSecurityScanUnverified marks a securityscan read/decode failure
	// (#1096). Pending-flavored like head_fetch_failed: we could not READ the
	// code-scanning signal, so the gate fails OPEN — never a hard Compute error
	// and never a silently-open pass. When it is the ONLY missing item the
	// overall state is pending, so branch protection re-evaluates once the read
	// recovers. Mirrors Rule 5's I/O posture.
	MissingSecurityScanUnverified MissingKind = "security_findings_unverified"
)

// TerminalImplementReviewCategories is the set of audit categories that count
// as a settled agent implement-review verdict (#947 / ADR-027). ANY of them
// clears the review_pending presence gate — a budget-killed reviewer
// (implement_review_failed) or an unwired layer (implement_review_skipped)
// is as terminal as a landed implement_reviewed. Shared single-source by the
// audit-complete rule (Compute) and the ADR-036 merge-resolution hold
// (server.checkImplementReviewSettled) so the two cannot diverge.
var TerminalImplementReviewCategories = []string{
	"implement_reviewed",
	"implement_review_failed",
	"implement_review_skipped",
}

// MissingItem points at a specific gap. Detail is human-readable;
// callers that want to render structured info (per-stage breakdown,
// etc.) should branch on Kind.
type MissingItem struct {
	Kind   MissingKind `json:"kind"`
	Detail string      `json:"detail"`
}

// Deps groups the repository handles Compute needs. Production
// wires the postgres-backed repos; tests wire fakes. Defining the
// dependencies here lets Compute stay a pure function over data.
type Deps struct {
	Runs      run.Repository
	Artifacts artifact.Repository
	Audit     audit.Repository
	// PRHead resolves a PR number to its live HEAD sha on GitHub
	// (#282). Used by rule 5 (foreign-commit detection) to compare
	// what's on GitHub right now against the Fishhawk-recorded
	// head_shas across the run + parent chain. Nil disables the
	// rule — Compute treats that as "no live data, skip drift
	// check" rather than failing it. Production wires a closure
	// around `githubclient.Client.GetPullRequest`.
	PRHead PRHeadFetcher
	// ImplementReviewers resolves the run's IMPLEMENT-stage
	// reviewers.agent count from its workflow spec (#947 rule 6).
	// Production wires server.resolveStageReviewers so spec parsing
	// stays single-sourced and auditcomplete never imports server
	// logic. Nil (dev / test posture) skips the review-pending rule
	// cleanly, mirroring the nil-PRHead pattern. Returning nil for a
	// run with no implement stage / no reviewers also skips.
	ImplementReviewers func(runRow *run.Run) *spec.ReviewersConfig
	// ReviewBackstop returns the hard max-wait for the configured
	// agent count, after which a stuck (never-terminal) review is
	// treated as present so the gate cannot wedge forever (#947).
	// Production wires server.planReviewBackstop, the same bound the
	// ADR-036 merge-resolution hold uses. Nil skips the rule.
	ReviewBackstop func(agentCount int) time.Duration
	// Now is the clock the review-pending rule reads for the backstop
	// comparison. Nil defaults to time.Now; tests inject a fixed time
	// to drive the backstop-elapsed branch deterministically.
	Now func() time.Time
}

// ReviewPresenceInputs is the already-fetched data ReviewPresent decides
// over. Both the audit-complete review-pending rule and the ADR-036 merge-
// resolution hold pass this so the "is the agent implement-review present?"
// decision has exactly one implementation.
type ReviewPresenceInputs struct {
	// ReviewersAgent is the configured implement-stage reviewers.agent count.
	ReviewersAgent int
	// Started is the run's implement_review_started audit entries (dispatch
	// markers). Empty means the review was never dispatched.
	Started []*audit.Entry
	// TerminalCount is the number of settled review entries across
	// TerminalImplementReviewCategories.
	TerminalCount int
	// Backstop is the hard max-wait anchored on the earliest Started entry.
	Backstop time.Duration
	// Now is the comparison clock for the backstop.
	Now time.Time
}

// ReviewPresent decides whether a configured agent implement-review is
// "present" — i.e. settled enough that a merge / audit-complete must not be
// held on it (#947, single source of truth for the presence gate). It
// returns present=true when:
//
//   - no agent reviewer is configured (ReviewersAgent==0), OR
//   - the review was never dispatched (len(Started)==0), OR
//   - every configured agent review reached a terminal verdict
//     (TerminalCount>=ReviewersAgent), OR
//   - the earliest dispatch is older than Backstop — a reviewer that died
//     emitting no terminal entry must not wedge the gate forever. Only this
//     last case reports backstopElapsed=true, so the caller emits the
//     degrade audit exactly once.
//
// present=false (review still genuinely in-flight) is the only case that
// holds the gate. backstopElapsed is meaningful only alongside present=true.
func ReviewPresent(in ReviewPresenceInputs) (present, backstopElapsed bool) {
	if in.ReviewersAgent == 0 {
		return true, false
	}
	if len(in.Started) == 0 {
		return true, false
	}
	if in.TerminalCount >= in.ReviewersAgent {
		return true, false
	}
	earliest := in.Started[0].Timestamp
	for _, e := range in.Started {
		if e.Timestamp.Before(earliest) {
			earliest = e.Timestamp
		}
	}
	if in.Now.Sub(earliest) > in.Backstop {
		return true, true
	}
	return false, false
}

// PRHeadFetcher is the signature for the live-HEAD callback. Errors
// flow into a `head_fetch_failed` MissingItem rather than failing
// Compute outright (GitHub flap shouldn't break the audit signal).
type PRHeadFetcher func(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, prNumber int) (headSHA string, err error)

// HeadReportCategoriesByPrecedence lists a run's own-chain head-reporting
// audit categories in DESCENDING precedence (#1682): the newest fixup_pushed
// head (the most recently pushed commit) wins over a child_pushed head, which
// wins over the pull_request_opened (PR-open) head. Within a category the
// winner is the highest-sequence entry.
//
// This single ordering is the shared enabler consumed by BOTH the server-side
// resolver (server.latestRunHeadSHA — acceptance head binding + Option C's
// head-aware retry) AND the audit-check publisher (auditcheckpublisher's
// findHeadSHA — the fishhawk_audit_complete Check Run target). Divergent head
// resolution between the acceptance/retry path and audit_complete publishing is
// exactly the failure this centralization prevents: both import this package so
// they cannot drift.
var HeadReportCategoriesByPrecedence = []string{"fixup_pushed", "child_pushed", "pull_request_opened"}

// LatestReportedHeadSHA applies HeadReportCategoriesByPrecedence to a run's
// chained audit entries: for the highest-precedence category that carries at
// least one entry with a non-empty head_sha payload field, it returns the
// head_sha of that category's highest-sequence entry. Returns ("", false) when
// no entry carries a head_sha.
//
// Pure over the passed entries — the caller fetches them (the entry set may
// mix categories; only the head-report categories are considered). Sequence is
// the monotonic per-table INSERT position, so "highest sequence" is
// unambiguously "most recently recorded".
func LatestReportedHeadSHA(entries []*audit.Entry) (string, bool) {
	type winner struct {
		seq int64
		sha string
		set bool
	}
	byCategory := map[string]winner{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		var p struct {
			HeadSHA string `json:"head_sha"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.HeadSHA == "" {
			continue
		}
		w := byCategory[e.Category]
		if !w.set || e.Sequence > w.seq {
			byCategory[e.Category] = winner{seq: e.Sequence, sha: p.HeadSHA, set: true}
		}
	}
	for _, cat := range HeadReportCategoriesByPrecedence {
		if w, ok := byCategory[cat]; ok && w.set {
			return w.sha, true
		}
	}
	return "", false
}

// Compute returns the audit-completeness state for the run plus a
// list of structured missing items. Both are returned together so
// the SPA can render "fail because: plan_missing, trace_missing
// (implement stage)" rather than just "fail."
//
// Errors are returned for transient I/O failures the caller should
// retry (DB unreachable, etc.). Logical gaps (missing artifact,
// failed chain) are encoded in the (state, missing) pair, never as
// errors.
func Compute(ctx context.Context, runID uuid.UUID, deps Deps) (stagecheck.State, []MissingItem, error) {
	if deps.Runs == nil || deps.Artifacts == nil || deps.Audit == nil {
		return stagecheck.StatePending, nil, errors.New("auditcomplete: incomplete deps")
	}

	stages, err := deps.Runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: list stages: %w", err)
	}

	// E38.3 (#1657): an acceptance stage the orchestrator auto-terminated for an
	// out_of_scope / zero-acceptance_criteria plan ships NO trace bundle yet is
	// legitimately succeeded. Read the skip markers once and build the set of
	// skipped stage ids so the partition below can exempt those stages from the
	// trace-required rule the same way review stages are exempt. A read failure is
	// transient (matching Rule 2's / Rule 6's I/O posture): return it so the caller
	// retries rather than silently under- or over-gating. A normally-dispatched
	// acceptance stage carries no marker and still requires its trace (the
	// exemption is marker-gated, not blanket).
	skipMarkers, err := deps.Audit.ListForRunByCategory(ctx, runID, "acceptance_skipped_out_of_scope")
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: acceptance skip markers: %w", err)
	}
	skippedStageIDs := make(map[uuid.UUID]struct{}, len(skipMarkers))
	for _, e := range skipMarkers {
		if e.StageID != nil {
			skippedStageIDs[*e.StageID] = struct{}{}
		}
	}

	// #1728 (E41.5) / #1748 (E41.6): an acceptance stage the orchestrator
	// SHORT-CIRCUITED records a verdict=passed acceptance_outcome_recorded entry
	// at dispatch time and ships NO trace bundle — yet is legitimately succeeded.
	// Exempt it from the trace-required rule the same way the E38.3 skip marker
	// is exempted. The exemption accepts ONLY the two known basis values —
	// "empty-criteria" (zero acceptance_criteria / zero out_of_scope) and
	// "all-skip-with-basis" (every criterion skip_expected with basis), the
	// shared plan.AcceptanceBasisEmptyCriteria / plan.AcceptanceBasisAllSkipWithBasis
	// constants: any OTHER basis — or a normal validator-recorded verdict, which
	// never sets basis at all — is NOT exempted and still requires its trace, so a
	// normally-dispatched acceptance stage is unaffected. A read failure is
	// transient (matching the skip-marker read above): return it so the caller
	// retries rather than silently under- or over-gating.
	outcomeEntries, err := deps.Audit.ListForRunByCategory(ctx, runID, "acceptance_outcome_recorded")
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: acceptance outcome entries: %w", err)
	}
	for _, e := range outcomeEntries {
		if e.StageID == nil {
			continue
		}
		var p map[string]any
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if b, ok := p[plan.AcceptanceBasisKey].(string); ok && (b == plan.AcceptanceBasisEmptyCriteria || b == plan.AcceptanceBasisAllSkipWithBasis) {
			skippedStageIDs[*e.StageID] = struct{}{}
		}
	}

	// Sort the stages we care about by type. Review stages don't
	// produce traces or artifacts of their own — they consume the
	// implement stage's pull_request — so they're excluded from
	// the "every non-review stage must have shipped a trace" rule. A
	// skipped acceptance stage (E38.3) is excluded on the same footing:
	// it is succeeded but ships no trace by design.
	var (
		planStage      *run.Stage
		implementStage *run.Stage
		nonReview      []*run.Stage
	)
	for _, s := range stages {
		_, isSkippedAcceptance := skippedStageIDs[s.ID]
		isSkippedAcceptance = isSkippedAcceptance && s.Type == run.StageTypeAcceptance
		if s.Type != run.StageTypeReview && !isSkippedAcceptance {
			nonReview = append(nonReview, s)
		}
		switch s.Type {
		case run.StageTypePlan:
			planStage = s
		case run.StageTypeImplement:
			implementStage = s
		}
	}

	// Mid-flight: if any non-review stage hasn't terminated, the
	// run isn't "done" — so neither is the audit. Pending rather
	// than fail; the reviewer waits.
	for _, s := range nonReview {
		if !s.State.IsTerminal() {
			return stagecheck.StatePending, nil, nil
		}
	}

	var missing []MissingItem

	// Rule 1: every plan stage in the run must have produced a
	// standard_v1 plan artifact. Workflows without a plan stage
	// (e.g. routine_change) skip this rule cleanly.
	if planStage != nil {
		ok, err := hasStandardV1Plan(ctx, deps.Artifacts, planStage.ID)
		if err != nil {
			return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: plan artifacts: %w", err)
		}
		if !ok {
			missing = append(missing, MissingItem{
				Kind:   MissingPlan,
				Detail: fmt.Sprintf("plan stage %s has no kind=plan, schema_version=standard_v1 artifact", shortID(planStage.ID)),
			})
		}
	}

	// Rule 2: every non-review stage that completed must have a
	// trace_uploaded audit entry. The runner ships both raw and
	// redacted variants per stage (E2.4); both must land for the
	// chain to be considered complete.
	traceMisses, err := missingTraces(ctx, deps.Audit, runID, nonReview)
	if err != nil {
		return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: trace audit: %w", err)
	}
	missing = append(missing, traceMisses...)

	// Rule 3: implement stages must produce a pull_request
	// artifact. Workflows without an implement stage skip cleanly.
	if implementStage != nil {
		ok, err := hasPullRequest(ctx, deps.Artifacts, implementStage.ID)
		if err != nil {
			return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: pr artifacts: %w", err)
		}
		if !ok {
			missing = append(missing, MissingItem{
				Kind:   MissingPullRequest,
				Detail: fmt.Sprintf("implement stage %s has no kind=pull_request artifact", shortID(implementStage.ID)),
			})
		}
	}

	// Rule 4: the audit chain must verify. Recompute every
	// entry's hash from its inputs and check the link to the prior
	// entry. A single mismatch invalidates the run — the
	// integrity story doesn't tolerate "mostly correct."
	if chainErr := verifyChain(ctx, deps.Audit, runID); chainErr != nil {
		var kind MissingKind
		if errors.Is(chainErr, errChainInvalid) {
			kind = MissingChain
		} else {
			kind = MissingChainBroken
		}
		missing = append(missing, MissingItem{
			Kind:   kind,
			Detail: chainErr.Error(),
		})
	}

	// Rule 5: the PR's live HEAD on GitHub must match a Fishhawk-
	// recorded head_sha across the run + its parent_run_id chain
	// (#282 / ADR-017 spirit — Fishhawk's audit story is "every
	// commit on this branch was produced by Fishhawk").
	//
	// Skipped when:
	//   - PRHead callback isn't wired (dev / test posture without
	//     a GitHub client).
	//   - The implement stage hasn't produced a pull_request
	//     artifact yet (caught by Rule 3 above; rule 5 has nothing
	//     to compare against).
	//   - The PR couldn't be read from GitHub (head_fetch_failed).
	//     Surfaces as a "pending"-flavored missing item; doesn't
	//     flip the overall state to fail. A flapping GitHub signal
	//     mustn't break the audit gate.
	rule5(ctx, deps, runID, &missing)

	// Rule 6: a configured agent implement-review (ADR-027) must have
	// reached a terminal verdict before the audit can go green (#947).
	// This is the PRE-merge presence gate — fishhawk_audit_complete is a
	// required check, so holding it pending blocks the merge until the
	// dispatched review lands (then auto-republishes green). Pending-
	// flavored like head_fetch_failed: a not-yet-landed review is "wait",
	// not "broken". Skipped cleanly when the resolver closures aren't wired
	// (dev / test) or no agent reviewer is configured.
	if implementStage != nil {
		item, err := reviewPendingRule(ctx, deps, runID, implementStage)
		if err != nil {
			return stagecheck.StatePending, nil, fmt.Errorf("auditcomplete: review pending: %w", err)
		}
		if item != nil {
			missing = append(missing, *item)
		}
	}

	// Rule 7: hold the merge while a high-severity code-scanning finding on
	// the implement diff is unresolved (#1096). Reads the latest securityscan
	// audit entry FLOORED to the most recent stage_fixup_triggered, so a
	// finding recorded before the latest fixup is stale and a clean re-scan
	// recorded after it clears the gate. Findings are a SEPARATE signal from
	// the review verdict (their own MissingKind), so a security finding routes
	// its own fixup without consuming a design-concern pass. Like rule5 the
	// helper never errors: a read/decode failure surfaces as a pending-flavored
	// security_findings_unverified item (fail OPEN), never a hard Compute error
	// and never a silently-open pass.
	securityFindingsRule(ctx, deps, runID, &missing)

	// Decide overall state. A `head_fetch_failed`, `review_pending`, or
	// `security_findings_unverified` item is pending-flavored — if the missing
	// list holds ONLY such items, the audit isn't broken: we either couldn't
	// verify the drift rule against a live source, a dispatched agent review
	// simply hasn't landed yet, or we couldn't read the code-scanning signal.
	// State stays pending so branch protection re-evaluates on a successful
	// follow-up publish rather than tripping a misleading red.
	switch {
	case len(missing) == 0:
		return stagecheck.StatePass, nil, nil
	case onlyPendingFlavored(missing):
		return stagecheck.StatePending, missing, nil
	default:
		return stagecheck.StateFail, missing, nil
	}
}

// reviewPendingRule implements Compute's rule 6 (#947): hold the audit
// pending while a configured agent implement-review is dispatched but not
// yet terminal. Returns a non-nil MissingItem when the review is still
// in-flight, nil when present (none configured, never dispatched, all
// terminal, or backstop elapsed). The decision delegates to ReviewPresent
// so it cannot diverge from the ADR-036 merge-resolution hold. Read failures
// surface as a transient error the caller retries — matching the other rules'
// I/O posture (the merge gate fails OPEN; this read endpoint fails to a retry).
func reviewPendingRule(ctx context.Context, deps Deps, runID uuid.UUID, implementStage *run.Stage) (*MissingItem, error) {
	if deps.ImplementReviewers == nil || deps.ReviewBackstop == nil {
		return nil, nil
	}
	runRow, err := deps.Runs.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", shortID(runID), err)
	}
	cfg := deps.ImplementReviewers(runRow)
	if cfg == nil || cfg.Agent == 0 {
		return nil, nil
	}

	started, err := deps.Audit.ListForRunByCategory(ctx, runID, "implement_review_started")
	if err != nil {
		return nil, fmt.Errorf("list implement_review_started: %w", err)
	}
	if len(started) == 0 {
		// Configured but never dispatched — nothing to wait on.
		return nil, nil
	}

	terminalCount := 0
	for _, cat := range TerminalImplementReviewCategories {
		entries, err := deps.Audit.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", cat, err)
		}
		terminalCount += len(entries)
	}

	now := time.Now().UTC()
	if deps.Now != nil {
		now = deps.Now()
	}
	present, _ := ReviewPresent(ReviewPresenceInputs{
		ReviewersAgent: cfg.Agent,
		Started:        started,
		TerminalCount:  terminalCount,
		Backstop:       deps.ReviewBackstop(cfg.Agent),
		Now:            now,
	})
	if present {
		return nil, nil
	}
	return &MissingItem{
		Kind: MissingReviewPending,
		Detail: fmt.Sprintf(
			"implement stage %s: %d/%d configured agent implement-review(s) settled; review has not landed yet",
			shortID(implementStage.ID), terminalCount, cfg.Agent),
	}, nil
}

// fixupTriggeredCategory is the audit category the runner records when a
// fixup pass is triggered (#762). Rule 7 floors the securityscan signal on
// the latest one. Mirrors server.CategoryStageFixupTriggered as a literal —
// auditcomplete sits below server, so the value is duplicated here exactly
// as the "trace_uploaded" / "implement_review_started" literals already are
// rather than importing a higher layer.
const fixupTriggeredCategory = "stage_fixup_triggered"

// securityFindingsPayload is the shape rule 7 decodes out of an
// implement_security_findings audit entry (#1096). The webhook ingest records
// the high-severity findings intersecting the implement diff under "findings";
// an entry with an EMPTY list is a clean scan that clears the gate. This is
// the cross-slice payload contract — the webhook writer, this gate reader, and
// the run-status surface all agree on it, pinned end-to-end by the integration
// test. The contract Finding type is imported from securityscan unchanged.
type securityFindingsPayload struct {
	Findings []securityscan.Finding `json:"findings"`
}

// securityFindingsRule implements Compute's rule 7 (#1096): hold the merge
// while a high-severity code-scanning finding on the implement diff is
// unresolved. It reads the latest implement_security_findings audit entry
// FLOORED to the most recent stage_fixup_triggered — a finding recorded before
// the latest fixup is stale (the fixup may have resolved it), so a clean
// re-scan recorded AFTER the fixup clears the gate while a fresh dirty re-scan
// re-blocks it.
//
// Mirrors rule5's I/O posture: it never returns an error. Any read failure
// (securityscan entries, fixup markers) or payload-decode failure appends a
// pending-flavored security_findings_unverified item (fail OPEN), so a flapping
// audit read or a malformed payload can neither hard-error Compute nor silently
// let an unscanned merge through. Absence of any scan entry is NOT a gap: CodeQL
// completes after the PR opens, so a window with no entry is normal and the
// alert surfaces in run-status once it lands.
func securityFindingsRule(ctx context.Context, deps Deps, runID uuid.UUID, out *[]MissingItem) {
	scans, err := deps.Audit.ListForRunByCategory(ctx, runID, securityscan.AuditCategorySecurityFindings)
	if err != nil {
		*out = append(*out, MissingItem{
			Kind:   MissingSecurityScanUnverified,
			Detail: fmt.Sprintf("could not read code-scanning audit entries: %v", err),
		})
		return
	}
	if len(scans) == 0 {
		// No scan recorded yet — not blocking (async CodeQL window).
		return
	}

	floor, err := latestFixupSequence(ctx, deps.Audit, runID)
	if err != nil {
		*out = append(*out, MissingItem{
			Kind:   MissingSecurityScanUnverified,
			Detail: fmt.Sprintf("could not read fixup markers to floor code-scanning signal: %v", err),
		})
		return
	}

	// Entries come back sequence-ascending; the last one ABOVE the floor is
	// the current scan. A scan at or below the floor predates the latest fixup
	// and is stale — we wait for the re-scan rather than gating on it.
	var current *audit.Entry
	for _, e := range scans {
		if e.Sequence > floor {
			current = e
		}
	}
	if current == nil {
		// Every recorded scan predates the latest fixup. The re-scan hasn't
		// landed yet — treat as not-blocking (re-scan-clears-after-fixup).
		return
	}

	var payload securityFindingsPayload
	if err := json.Unmarshal(current.Payload, &payload); err != nil {
		*out = append(*out, MissingItem{
			Kind:   MissingSecurityScanUnverified,
			Detail: fmt.Sprintf("could not decode code-scanning findings payload: %v", err),
		})
		return
	}
	if len(payload.Findings) == 0 {
		// Clean scan above the floor — gate clears.
		return
	}
	*out = append(*out, MissingItem{
		Kind:   MissingSecurityFindings,
		Detail: securityFindingsDetail(payload.Findings),
	})
}

// latestFixupSequence returns the sequence of the most recent
// stage_fixup_triggered entry for the run, or 0 when none has been recorded
// (no fixup yet → floor is 0, so every securityscan entry is current).
func latestFixupSequence(ctx context.Context, repo audit.Repository, runID uuid.UUID) (int64, error) {
	fixups, err := repo.ListForRunByCategory(ctx, runID, fixupTriggeredCategory)
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range fixups {
		if e.Sequence > latest {
			latest = e.Sequence
		}
	}
	return latest, nil
}

// securityFindingsDetail renders the human-readable missing-item detail: the
// finding count plus the first finding's rule + location, so a reviewer sees
// what to route to fixup without opening the run. Callers guarantee a non-empty
// slice.
func securityFindingsDetail(findings []securityscan.Finding) string {
	first := findings[0]
	loc := first.Path
	if first.Path != "" && first.StartLine > 0 {
		loc = fmt.Sprintf("%s:%d", first.Path, first.StartLine)
	}
	return fmt.Sprintf(
		"%d unresolved high-severity code-scanning finding(s) on the implement diff; e.g. %s at %s",
		len(findings), first.RuleID, loc)
}

// rule5 implements the foreign-commit detection. Appends missing
// items to `out` and never returns an error — fetch failures land
// as `head_fetch_failed` so the state-decision logic can treat them
// as pending rather than fail. Pure data-flow helper; the I/O is
// guarded by the PRHead callback being non-nil.
func rule5(ctx context.Context, deps Deps, runID uuid.UUID, out *[]MissingItem) {
	if deps.PRHead == nil {
		return
	}
	gather, ok, err := gatherForeignCommitInputs(ctx, deps, runID)
	if err != nil {
		// Walking the parent chain or reading artifacts failed —
		// log-equivalent as a fetch-failure missing item. The
		// caller treats it as pending for state decision.
		*out = append(*out, MissingItem{
			Kind:   MissingHeadFetchFail,
			Detail: fmt.Sprintf("could not gather Fishhawk-recorded head_shas: %v", err),
		})
		return
	}
	if !ok {
		// Either no implement stage yet, no installation, no PR
		// artifact, or no parseable PR number — Rule 3 covers the
		// missing artifact case; for the others rule 5 has nothing
		// to compare against.
		return
	}

	liveSHA, err := deps.PRHead(ctx, gather.scope, gather.repo, gather.prNumber)
	if err != nil {
		*out = append(*out, MissingItem{
			Kind:   MissingHeadFetchFail,
			Detail: fmt.Sprintf("could not read PR HEAD from GitHub: %v", err),
		})
		return
	}
	if _, hit := gather.knownSHAs[liveSHA]; hit {
		return
	}

	known := make([]string, 0, len(gather.knownSHAs))
	for sha := range gather.knownSHAs {
		known = append(known, shortSHA(sha))
	}
	*out = append(*out, MissingItem{
		Kind: MissingForeignCommit,
		Detail: fmt.Sprintf(
			"PR HEAD %s is not a Fishhawk-recorded commit (known: %s)",
			shortSHA(liveSHA),
			strings.Join(known, ", ")),
	})
}

// foreignCommitInputs bundles the values rule5 needs to make the
// PRHead call + compose the missing-item detail.
type foreignCommitInputs struct {
	scope     forge.CredentialScope
	repo      forge.RepoRef
	prNumber  int
	knownSHAs map[string]struct{}
}

// gatherForeignCommitInputs walks runID upward via parent_run_id
// (#216) and collects every implement-stage `pull_request`
// artifact's head_sha + the PR's number. Returns (inputs, true, nil)
// when there's enough to call PRHead; (_, false, nil) when there's
// no implement stage / no installation / no PR yet; error only on
// transient I/O.
func gatherForeignCommitInputs(ctx context.Context, deps Deps, runID uuid.UUID) (foreignCommitInputs, bool, error) {
	known := make(map[string]struct{})
	var (
		scope    forge.CredentialScope
		repoRef  forge.RepoRef
		prNumber int
	)

	cursor := runID
	visited := map[uuid.UUID]struct{}{}
	for {
		if _, seen := visited[cursor]; seen {
			break // defensive against a corrupted parent loop
		}
		visited[cursor] = struct{}{}

		r, err := deps.Runs.GetRun(ctx, cursor)
		if err != nil {
			return foreignCommitInputs{}, false, fmt.Errorf("get run %s: %w", shortID(cursor), err)
		}

		// The original (head) run anchors installation + repo;
		// every ancestor shares them. Capture once.
		if scope.IsZero() && r.InstallationID != nil {
			scope = forge.FromGitHubInstallationID(*r.InstallationID)
			parsed, perr := parseRepo(r.Repo)
			if perr == nil {
				repoRef = parsed
			}
		}

		// Pull head_sha + pr_number from this run's implement stage.
		stages, err := deps.Runs.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return foreignCommitInputs{}, false, fmt.Errorf("list stages for %s: %w", shortID(r.ID), err)
		}
		var impl *run.Stage
		for _, s := range stages {
			if s.Type == run.StageTypeImplement {
				impl = s
				break
			}
		}
		if impl != nil {
			arts, err := deps.Artifacts.ListForStage(ctx, impl.ID)
			if err != nil {
				return foreignCommitInputs{}, false, fmt.Errorf("list artifacts for %s: %w", shortID(impl.ID), err)
			}
			for _, a := range arts {
				if a.Kind != artifact.KindPullRequest {
					continue
				}
				sha, num := decodePRArtifact(a.Content)
				if sha != "" {
					known[sha] = struct{}{}
				}
				// The newest run on the chain (we visit it first)
				// is authoritative for the PR number.
				if prNumber == 0 && num > 0 {
					prNumber = num
				}
			}
		}

		// #1682: a fix-up push places a NEW head on the PR branch that
		// the stale PR-open artifact does not record. Union in this run's
		// own head-report audit entries (fixup_pushed / child_pushed /
		// pull_request_opened head_shas) so a post-fixup live HEAD is
		// recognized as Fishhawk-authored rather than flagged
		// foreign_commit by rule 5. Additive to the artifact-derived set;
		// a read failure is transient I/O (matching the artifact-read
		// posture above), so it aborts the gather rather than silently
		// under-populating known and false-flagging a legitimate head.
		for _, cat := range HeadReportCategoriesByPrecedence {
			entries, err := deps.Audit.ListForRunByCategory(ctx, r.ID, cat)
			if err != nil {
				return foreignCommitInputs{}, false, fmt.Errorf("list %s heads for %s: %w", cat, shortID(r.ID), err)
			}
			for _, e := range entries {
				var p struct {
					HeadSHA string `json:"head_sha"`
				}
				if json.Unmarshal(e.Payload, &p) == nil && p.HeadSHA != "" {
					known[p.HeadSHA] = struct{}{}
				}
			}
		}

		if r.ParentRunID == nil {
			break
		}
		cursor = *r.ParentRunID
	}

	if scope.IsZero() || repoRef.Owner == "" || prNumber == 0 || len(known) == 0 {
		return foreignCommitInputs{}, false, nil
	}
	return foreignCommitInputs{
		scope:     scope,
		repo:      repoRef,
		prNumber:  prNumber,
		knownSHAs: known,
	}, true, nil
}

// decodePRArtifact pulls (head_sha, pr_number) out of a
// pull_request artifact's content. Mirrors the publisher's
// `decodeHeadSHA` but returns the number too. Empty / unparseable
// values come back as zero — the caller skips silently rather than
// surfacing the parse failure (the artifact validator is the right
// place for that complaint).
func decodePRArtifact(content []byte) (string, int) {
	if len(content) == 0 {
		return "", 0
	}
	var body struct {
		HeadSHA  string `json:"head_sha"`
		PRNumber int    `json:"pr_number"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return "", 0
	}
	return body.HeadSHA, body.PRNumber
}

// parseRepo splits "owner/name" into a RepoRef. Mirrors the
// helpers in other packages; duplicated here to keep auditcomplete
// import-free of higher layers.
func parseRepo(s string) (forge.RepoRef, error) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return forge.RepoRef{}, fmt.Errorf("auditcomplete: repo %q must be owner/name", s)
	}
	return forge.RepoRef{Owner: s[:i], Name: s[i+1:]}, nil
}

// shortSHA renders the leading 7 of a SHA for human-readable
// missing-item details. Leaves shorter SHAs untouched.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// onlyPendingFlavored returns true when every entry in `missing` is a
// pending-flavored row — `head_fetch_failed` (we couldn't read the live
// PR HEAD), `review_pending` (a dispatched agent review hasn't landed
// yet), or `security_findings_unverified` (we couldn't read/decode the
// code-scanning signal). Used to demote the overall state from fail to
// pending: none is an audit GAP, just "wait / we don't know." A mix with
// any hard gap (plan_missing, trace_missing, foreign_commit,
// security_findings_unresolved, …) still fails.
func onlyPendingFlavored(missing []MissingItem) bool {
	for _, m := range missing {
		switch m.Kind {
		case MissingHeadFetchFail, MissingReviewPending, MissingSecurityScanUnverified:
			// pending-flavored — keep scanning
		default:
			return false
		}
	}
	return len(missing) > 0
}

func shortID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

func hasStandardV1Plan(ctx context.Context, repo artifact.Repository, stageID uuid.UUID) (bool, error) {
	arts, err := repo.ListForStage(ctx, stageID)
	if err != nil {
		return false, err
	}
	for _, a := range arts {
		if a.Kind != artifact.KindPlan {
			continue
		}
		if a.SchemaVersion == nil || *a.SchemaVersion != "standard_v1" {
			continue
		}
		return true, nil
	}
	return false, nil
}

func hasPullRequest(ctx context.Context, repo artifact.Repository, stageID uuid.UUID) (bool, error) {
	arts, err := repo.ListForStage(ctx, stageID)
	if err != nil {
		return false, err
	}
	for _, a := range arts {
		if a.Kind == artifact.KindPullRequest {
			return true, nil
		}
	}
	return false, nil
}

// missingTraces returns one MissingItem per non-review stage that
// didn't ship both raw + redacted bundles. The runner posts both
// variants per stage (E2.4); a missing variant still implies the
// audit chain is incomplete.
func missingTraces(ctx context.Context, repo audit.Repository, runID uuid.UUID, nonReview []*run.Stage) ([]MissingItem, error) {
	if len(nonReview) == 0 {
		return nil, nil
	}
	entries, err := repo.ListForRunByCategory(ctx, runID, "trace_uploaded")
	if err != nil {
		return nil, err
	}

	// Build (stage_id → set-of-variants) from the audit log.
	type variantSet struct{ raw, redacted bool }
	got := map[uuid.UUID]*variantSet{}
	for _, e := range entries {
		if e.StageID == nil {
			continue
		}
		v, ok := got[*e.StageID]
		if !ok {
			v = &variantSet{}
			got[*e.StageID] = v
		}
		// Variant comes from the audit payload; fall through if
		// the entry is shaped wrong (older format, etc.) — the
		// chain-verify rule will catch a tampered payload.
		switch traceVariantOf(e.Payload) {
		case "raw":
			v.raw = true
		case "redacted":
			v.redacted = true
		}
	}

	var out []MissingItem
	for _, s := range nonReview {
		// Only stages that actually executed need traces. A
		// stage that was cancelled before dispatch has nothing
		// to ship.
		if s.State == run.StageStatePending || s.State == run.StageStateCancelled {
			continue
		}
		v, ok := got[s.ID]
		if !ok {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) has no trace_uploaded audit entry", shortID(s.ID), s.Type),
			})
			continue
		}
		if !v.raw {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) is missing the raw trace bundle", shortID(s.ID), s.Type),
			})
		}
		if !v.redacted {
			out = append(out, MissingItem{
				Kind:   MissingTrace,
				Detail: fmt.Sprintf("stage %s (%s) is missing the redacted trace bundle", shortID(s.ID), s.Type),
			})
		}
	}
	return out, nil
}

// traceVariantOf reads the `variant` field out of a trace_uploaded
// audit entry's payload. Returns "" on parse failure or absent
// field — the caller treats that as "neither raw nor redacted"
// which counts as a missing variant.
func traceVariantOf(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		Variant string `json:"variant"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Variant
}

// errChainInvalid signals that an entry's recomputed hash didn't
// match what's stored — the chain has been tampered with. Distinct
// from I/O errors so Compute can categorize the missing item.
var errChainInvalid = errors.New("audit chain mismatch")

func verifyChain(ctx context.Context, repo audit.Repository, runID uuid.UUID) error {
	entries, err := repo.ListForRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("list audit entries: %w", err)
	}
	var prev *string
	for _, e := range entries {
		// Recompute the hash from the entry's inputs. The
		// canonical algorithm lives in audit.ComputeEntryHash;
		// the verifier package mirrors it but is intended for
		// external callers. We use the backend's copy here so
		// we don't reach across the module boundary.
		runIDPtr := e.RunID
		got, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        runIDPtr,
			StageID:      e.StageID,
			Timestamp:    e.Timestamp,
			Category:     e.Category,
			ActorKind:    e.ActorKind,
			ActorSubject: e.ActorSubject,
			Payload:      e.Payload,
			PrevHash:     prev,
		})
		if err != nil {
			return fmt.Errorf("hash entry %s: %w", e.ID, err)
		}
		if got != e.EntryHash {
			return fmt.Errorf("%w: entry %s recomputed %q != stored %q",
				errChainInvalid, e.ID, got, e.EntryHash)
		}
		// PrevHash for the next entry is THIS entry's stored
		// hash, not the one we just recomputed — the link
		// integrity is the (prev, current) pair as stored, not
		// our recomputation.
		hash := e.EntryHash
		prev = &hash
	}
	return nil
}
