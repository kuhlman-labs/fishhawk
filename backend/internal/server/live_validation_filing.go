package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// live-validation walk audit categories (#2045, E48.35). Two distinct
// categories implement the intent-marker-before-file idempotency the operator's
// replan directives require:
//
//   - liveValidationWalkIntentKind records the "walk attempt started" marker
//     BEFORE the forge call. Once ANY intent marker exists a prior approval
//     already attempted (or completed) the filing, so a re-approval is a no-op —
//     it never files a second walk. This is the durable idempotency anchor.
//   - liveValidationWalkLinkedKind records the "walk filed" (or "filing
//     failed") outcome AFTER the forge call. The surface reads the newest linked
//     marker to render the pending count + walk ref (or the file-manually
//     variant).
//
// The two-category split (rather than one category with a phase discriminator)
// is deliberate: it lets the linked-marker write fail in isolation from the
// intent-marker write, so the partial-failure re-approval path — issue filed,
// linked-marker write lost — is exercisable and its idempotency provable.
//
// These are INTERNAL markers read exclusively via AuditRepo.ListForRunByCategory
// (a direct repo query) and distilled into the run-status live_validation
// surface — they are never operator-awaited through the registry-gated
// GET /audit / fishhawk_await_audit path. They are therefore deliberately NOT
// entered in audit.KnownCategories, and named `...Kind` rather than `...Category`
// to mark that distinction.
const (
	liveValidationWalkIntentKind = "live_validation_walk_intent"
	liveValidationWalkLinkedKind = "live_validation_walk_linked"
)

// liveValidationWalkMarker is the audit payload written for BOTH the intent and
// linked markers (the phase field names which). The pending count + criterion
// ids are carried on both so the surface can render from whichever marker is
// newest — a linked marker, or a stranded intent marker with no linked marker
// following it.
type liveValidationWalkMarker struct {
	// Phase is "intent" (written before the forge call) or "linked" (written
	// after). Redundant with the category but self-describing on the payload.
	Phase string `json:"phase"`
	// PendingCriteriaCount is the number of requires_live_validation criteria on
	// the approved plan awaiting an operator live check.
	PendingCriteriaCount int `json:"pending_criteria_count"`
	// CriterionIDs are the slug join keys of those criteria.
	CriterionIDs []string `json:"criterion_ids,omitempty"`
	// WalkRef is the filed walk work-item ref ("#N"); empty on an intent marker
	// and on a filing-failure linked marker.
	WalkRef string `json:"walk_ref,omitempty"`
	// FilingFailed is true on a linked marker written when the forge filing
	// failed (walk_ref empty). Always false on an intent marker.
	FilingFailed bool `json:"filing_failed"`
}

// runLiveValidationPayload is the run-status / gate-view wire surface for a
// run's pending operator live-validation walk (#2045). It is a distinct type
// from the audit marker so a payload-shape change in the audit trail cannot
// silently leak through the API surface. Populated ONLY by the single-run reads
// (handleGetRun, buildGateView), same best-effort single-read posture as the
// other distilled surfaces (Concerns / SecurityFindings); omitted (nil) when
// the run carries no live_validation marker.
//
// FilingFailed is the "file the walk by hand" decision bit: it is true for BOTH
// a linked-marker filing failure AND a stranded intent-only marker (the
// crash-window case), so a consumer that reads only FilingFailed never renders
// the healthy "walk: #X" variant for a run whose walk is not durably filed
// (binding condition A(1) — treat filing_failed and intent-only identically for
// rendering). FilingIncomplete additionally flags the stranded-intent sub-case
// so a consumer can word it "walk filing incomplete" vs "walk filing failed".
type runLiveValidationPayload struct {
	PendingCriteriaCount int    `json:"pending_criteria_count"`
	WalkRef              string `json:"walk_ref,omitempty"`
	FilingFailed         bool   `json:"filing_failed"`
	FilingIncomplete     bool   `json:"filing_incomplete,omitempty"`
}

// liveValidationWalkArea is the area:* label supplied on the filed chore walk so
// the chore type's required 'area' label namespace is satisfied (autonomy is
// covered by the type's label_default). A missing namespace is fail-open (a WARN
// in applyAndFileWorkItem, still files), so the value is cosmetic — it names the
// subsystem the walk-filing machinery lives in.
const liveValidationWalkArea = "area:backend"

// liveValWalkLocks serializes the intent-check → intent-append → file critical
// section of fileOrLinkLiveValidationWalk PER RUN. The intent-marker idempotency
// guard is a non-atomic list-then-append: without serialization two concurrent
// approvals of the SAME run could both observe no intent marker, both append
// one, and both file a walk (the implement-review concurrency fix). The per-run
// in-process mutex makes them serialize so the second observes the first's
// intent marker and no-ops. This is sufficient for the single-daemon v0
// deployment; a hosted MULTI-INSTANCE deployment (multiple fishhawkd processes)
// would need a Postgres advisory lock instead — the in-process map is invisible
// across processes, mirroring the childNumberLocks note in workitems.go. The map
// is never pruned (one small mutex per run for the process lifetime), bounded by
// the number of runs whose approved plan carried a live-validation criterion.
//
// NOT THE PRIMARY GUARD ANY MORE (E50.16 / #2657 — an OPERATOR decision, not a
// re-derivation). This mutex PREDATES the approve compare-and-swap and is
// retained as belt-and-braces BEHIND it, not as the control that prevents
// production double-filing. Since E50.15 / #2656 the approve advance is a CAS
// anchored on the observed state (advanceStage → casTransitionFromObserved), so
// a raced second approval is refused at the advance and NEVER reaches this hook.
// Both hooks are invoked from exactly one production call site each, in
// finishApprovalAdvance (approvals.go — fileSplitProposalChildren, then
// fileOrLinkLiveValidationWalk), and the production RunRepo is the postgres one,
// which implements run.StageCASTransitioner; the non-CAS degradation in
// casTransitionFromObserved is reachable only by in-memory test fakes. So the
// CAS covers every path a deployed daemon can take, and this mutex is load-
// bearing only where the CAS is bypassed: a direct caller such as
// TestFileOrLinkLiveValidationWalk_ConcurrentApprovals, which is why deleting
// the acquisition below still reddens that test (measured: 19 of 20 -race
// iterations, provider File called 2 times, want 1).
//
// Its sibling fileSplitProposalChildren (split_filing.go) deliberately holds NO
// equivalent lock, and that asymmetry is NOT a principled difference between the
// two hooks — at this layer there is none. It is retained here only because
// removing a working guard buys nothing, and not added there only because it
// would defend a path that cannot occur outside tests.
//
// THE RESIDUAL, stated plainly for whoever changes this next: if the CAS is ever
// removed, or the run.StageCASTransitioner capability assert in
// casTransitionFromObserved is dropped, BOTH hooks lose their protection — and
// the split hook loses it FIRST and SILENTLY, because it has nothing underneath.
// The #2656 evidence is the measurement: with the CAS deleted, split-child
// filings went 3 → 6 (two split_children_filed markers) while the walk count
// stayed at 1, held up by this mutex alone.
var (
	liveValWalkLocksMu sync.Mutex
	liveValWalkLocks   = map[uuid.UUID]*sync.Mutex{}
)

// lockLiveValWalk acquires (creating on first use) the per-run mutex and returns
// its unlock func. The caller holds it across the list-intent → append-intent →
// file → append-linked window so the whole idempotency-and-file section is
// serialized against a concurrent approval of the same run.
func lockLiveValWalk(runID uuid.UUID) func() {
	liveValWalkLocksMu.Lock()
	m := liveValWalkLocks[runID]
	if m == nil {
		m = &sync.Mutex{}
		liveValWalkLocks[runID] = m
	}
	liveValWalkLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// fileOrLinkLiveValidationWalk is the best-effort on-approval hook (#2045,
// E48.35): when an operator approves a plan carrying any requires_live_validation
// acceptance criterion, it auto-files (or, on a re-approval, no-ops on) a
// `chore`-type operator-validation walk work item and records a durable audit
// marker so the pending live check is tracked rather than shipped silently
// unvalidated. It is invoked from finishApprovalAdvance with the same
// best-effort posture as fileSplitProposalChildren — every forge / work-item /
// audit error logs and returns, NEVER unwinding the approval the gate already
// recorded. A plan with no marked criterion no-ops with zero side effects.
//
// Idempotency (binding condition A / replan directive 2) — INTENT-MARKER-BEFORE-
// FILE: a durable intent marker is appended BEFORE the forge call. On entry, if
// ANY intent marker is already present a prior approval already attempted the
// filing, so this is a no-op — it NEVER files a second walk. This deliberately
// does NOT re-file on a bare intent marker: a stranded intent marker (the
// process died between the intent append and the forge call, condition A) is
// indistinguishable from "issue filed, linked-marker write failed" without a
// forge query this hook deliberately avoids. Re-filing either would reopen the
// double-file window the intent marker closes.
//
// MARKER ORDERING vs the sibling hook (E50.16 / #2657): this hook writes its
// intent marker BEFORE the forge call; fileSplitProposalChildren
// (split_filing.go) writes each per-phase work_item_filed marker AFTER each
// child is filed. The difference follows from resume semantics, not from a
// difference in how the two protect themselves. The walk is ONE all-or-nothing
// filing with nothing to resume, so it can afford to burn the marker first and
// accept the stranded-intent residual below (which degrades to the documented
// operator-files-it-by-hand path) in exchange for never double-filing. Split
// files N children and must know WHICH ordinals landed, so its marker records a
// completed fact and it accepts the mirrored at-least-once residual instead — a
// child filed whose marker never persisted re-files once on a re-approval.
//
// MANUAL RECOVERY (binding condition A(2)): a stranded intent-only marker — an
// intent marker with no linked marker following it — degrades to the pre-#2045
// status quo: the surface renders the file-manually guidance (see
// liveValidationForRun) and the OPERATOR files the walk by hand. This hook will
// not re-file it. This is the accepted residual of the forge-query-free
// idempotency design; it is rare (requires a crash inside the narrow
// intent-append → forge-call window).
func (s *Server) fileOrLinkLiveValidationWalk(ctx context.Context, stage *run.Stage) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	runID := stage.RunID

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "load approved plan failed", err.Error())
		return
	}
	if approvedPlan == nil {
		return
	}
	crits := plan.LiveValidationCriteria(approvedPlan.Verification)
	if len(crits) == 0 {
		return // no marked criterion → no forge call, no marker
	}

	// Serialize the intent-check → intent-append → file → linked-append section
	// per run so two CONCURRENT approvals of the same run cannot both pass the
	// (non-atomic) list-then-append idempotency guard below and file duplicate
	// walks. The second holder observes the first's intent marker and no-ops.
	unlockWalk := lockLiveValWalk(runID)
	defer unlockWalk()

	// Idempotency anchor: the intent marker is the durable "walk attempt started"
	// record. Its presence — from ANY prior approval — means this hook already
	// ran, so no-op rather than file a second walk.
	priorIntent, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkIntentKind)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "list intent markers failed", err.Error())
		return
	}
	if len(priorIntent) > 0 {
		return // already attempted → idempotent no-op
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.logLiveValidationWarn(ctx, runID, "get run failed", err.Error())
		return
	}
	owner, name, ok := splitRepoFullName(runRow.Repo)
	if !ok {
		s.logLiveValidationWarn(ctx, runID, "malformed run repo", runRow.Repo)
		// The walk cannot be filed (no owner/name), but marked criteria exist —
		// record a filing-failure marker so those pending criteria surface as
		// file-manually rather than advancing silently unvalidated (implement-
		// review high/correctness). crits is guaranteed non-empty here (the
		// no-marked-criterion case returned above), so this never marks a
		// no-op approval as failed.
		s.writeLiveValidationFilingFailedMarker(ctx, runRow, crits)
		return
	}
	parentIssue := 0
	if runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			parentIssue = n
		}
	}
	if parentIssue == 0 {
		// No originating issue to companion-link / parent against; the walk
		// cannot be filed. Marked criteria exist, so record a filing-failure
		// marker rather than advancing the run with pending live-validation
		// criteria silently accepted (implement-review high/correctness) — the
		// same failure-marker path a post-File error takes.
		s.writeLiveValidationFilingFailedMarker(ctx, runRow, crits)
		return
	}

	ids := make([]string, 0, len(crits))
	for _, c := range crits {
		ids = append(ids, c.ID)
	}

	// (b) Durable INTENT marker BEFORE the forge call. On an append failure we
	// have filed nothing — a re-approval finds no intent marker and retries
	// cleanly (no orphan walk, no double file).
	if err := s.appendLiveValidationMarker(ctx, runRow, liveValidationWalkIntentKind, liveValidationWalkMarker{
		Phase:                "intent",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
	}); err != nil {
		s.logLiveValidationWarn(ctx, runID, "append intent marker failed; filed nothing", err.Error())
		return
	}

	// (c) File the chore walk (a SINGLE companion-linked filing — see
	// fileLiveValidationChore). (d) Append the LINKED marker in BOTH outcomes —
	// on success with the walk ref, on ANY filing failure with filing_failed=true
	// and an empty ref — so approval never advances leaving pending
	// live-validation criteria with zero surfaced indication (replan directive 1).
	walkRef, filed := s.fileLiveValidationChore(ctx, runRow, owner, name, parentIssue, crits)
	linked := liveValidationWalkMarker{
		Phase:                "linked",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
	}
	if filed {
		linked.WalkRef = walkRef
	} else {
		linked.FilingFailed = true
	}
	if err := s.appendLiveValidationMarker(ctx, runRow, liveValidationWalkLinkedKind, linked); err != nil {
		// The linked-marker write failed. The walk MAY already be filed. A
		// re-approval reads the intent marker and no-ops (never a second walk);
		// the surface renders the file-manually variant from the stranded intent
		// marker (liveValidationForRun). Best-effort: warn, never unwind.
		s.logLiveValidationWarn(ctx, runID, "append linked marker failed", err.Error())
	}
}

// writeLiveValidationFilingFailedMarker records a filing-failure LINKED marker
// (filing_failed=true, empty walk_ref, the pending count + criterion ids) for a
// walk that CANNOT be filed because a structural prerequisite is invalid before
// the forge call is ever reached — a malformed run repo or a non-issue trigger —
// even though the approved plan carries live-validation criteria. It is the same
// failure-marker the post-File error path writes, so run-status / gate_view /
// next_actions render "N criteria pending operator live-validation (walk filing
// failed — file manually)" and the run never advances with pending criteria
// silently accepted (implement-review high/correctness, #2045). Callers must
// invoke it ONLY when len(crits) > 0. Best-effort: a write failure WARNs and
// does not unwind the approval the gate already recorded.
func (s *Server) writeLiveValidationFilingFailedMarker(ctx context.Context, runRow *run.Run, crits []plan.AcceptanceCriterion) {
	ids := make([]string, 0, len(crits))
	for _, c := range crits {
		ids = append(ids, c.ID)
	}
	if err := s.appendLiveValidationMarker(ctx, runRow, liveValidationWalkLinkedKind, liveValidationWalkMarker{
		Phase:                "linked",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
		FilingFailed:         true,
	}); err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "append filing-failed marker (unfileable walk) failed", err.Error())
	}
}

// githubSubIssueParentCap is GitHub's per-parent sub-issue limit: a parent
// issue can hold at most 100 sub-issues, so the addSubIssue mutation REJECTS the
// 101st with a GraphQL error (surfaced through doGraphQL as ErrValidation).
// resolveWalkParentEpic treats an epic already at this cap as unattachable and
// degrades to the companion arm, so a full epic files a safe self-consistent
// companion walk rather than an [E<epic>.<n>]-titled walk that could never
// attach (binding condition 4 — E22 #389, E48 #1940, E67 #2561 are each at the
// cap today). See backend/internal/server/README.md.
const githubSubIssueParentCap = 100

// walkEpicTitleRE matches ONLY the bracket-CLOSED [E<n>] epic title form. It is
// deliberately DISTINCT from workitems.go's epicTitleRE (`^\s*\[E(\d+)`), which
// stops at the first non-digit and so ACCEPTS a CHILD title like [E48.35] —
// exactly the mis-parenting #2179 exists to prevent. The `\]` anchor after the
// digit run rejects a child title, so only a true epic parents the walk.
var walkEpicTitleRE = regexp.MustCompile(`^\s*\[E(\d+)\]`)

// resolveWalkParentEpic decides whether the live-validation walk should be
// parented under the triggering child's TRUE epic (#2179) instead of companion-
// linked to the child. It returns ("#<epic issue>", "<epic digits>",
// "<child number>", unlock, true) only when EVERY precondition holds — where
// unlock is a HELD per-epic allocation lock the caller must release AFTER its
// File (see the TOCTOU note below); otherwise ok=false, unlock is nil, and the
// caller files the UNCHANGED companion walk. Each fallback mode is an explicit
// early return:
//
//	(1) no GitHub client wired;
//	(2) a zero credential scope;
//	(3) IssueParent errors;
//	(4) IssueParent returns no parent (the triggering issue has no sub-issue parent);
//	(5) the parent's title is not the bracket-closed [E<n>] epic form (e.g. it is
//	    another child, [E48.35]) — walkEpicTitleRE, NOT the child-accepting epicTitleRE;
//	(6) the resolved provider is unregistered (workmgmt.Get errors) or does not
//	    implement EpicChildrenQuerier, so {n} could never be discovered and the
//	    epic-arm filing would 422 at renderTitle — both degrade the same safe way;
//	(7) the epic is resolvable but UNATTACHABLE — it already holds the per-parent
//	    sub-issue cap, its child count could not be read, or its children carry no
//	    numbered [E<epic>.<n>] form so {n} cannot be allocated (#2101); degrade to
//	    companion the same way an unresolvable epic does (binding condition 4).
//
// CAP DECISION UNDER THE ALLOCATION LOCK (high/concurrency TOCTOU, #2179 fix-up).
// The capacity decision and the {n} allocation are BOTH made under the SAME
// per-epic childNumberLock applyAndFileWorkItem's deriveChildNumberTitleVar
// takes, and the lock stays HELD (returned to the caller as unlock) across the
// caller's File. The unlocked pre-count is only a fast reject of an obviously-
// full epic; the AUTHORITATIVE check is the second read UNDER the lock. Without
// this a concurrent filer could add the cap-th child between an unlocked count
// and the File, after which this walk would take the epic arm and fail
// attachment instead of using the mandatory companion fallback. Because the arm
// pre-computes {n} here, the caller supplies it explicitly so
// deriveChildNumberTitleVar short-circuits and does NOT re-take the lock (no
// deadlock, no second discovery read).
func (s *Server) resolveWalkParentEpic(ctx context.Context, scope forge.CredentialScope, owner, name string, childIssue int, conv workmgmt.Conventions) (string, string, string, func(), bool) {
	if s.cfg.GitHub == nil {
		return "", "", "", nil, false // (1)
	}
	if scope.IsZero() {
		return "", "", "", nil, false // (2)
	}
	parent, err := s.cfg.GitHub.IssueParent(ctx, scope, forge.RepoRef{Owner: owner, Name: name}, childIssue)
	if err != nil {
		return "", "", "", nil, false // (3)
	}
	if parent == nil {
		return "", "", "", nil, false // (4)
	}
	m := walkEpicTitleRE.FindStringSubmatch(parent.Title)
	if m == nil {
		return "", "", "", nil, false // (5) not the bracket-closed epic form
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		return "", "", "", nil, false // (6) unregistered provider → no querier capability reachable, safe companion degrade
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return "", "", "", nil, false // (6) no capability to discover {n}
	}
	choreType, ok := conv.Types["chore"]
	if !ok || !strings.Contains(choreType.TitleFormat, "{n}") {
		return "", "", "", nil, false // (6) the chore type carries no {n} placeholder to allocate → companion
	}
	epicRef := "#" + strconv.Itoa(parent.Number)
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		Scope:   scope,
	}

	// Fast reject: an obviously-full (or unreadable) epic degrades to companion
	// without taking the allocation lock. This unlocked pre-count is ONLY an
	// optimization — the AUTHORITATIVE capacity decision is the locked read below.
	if res, perr := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: target, Epic: epicRef}); perr != nil {
		return "", "", "", nil, false // (7) child count unreadable → companion
	} else if len(res.Children) >= githubSubIssueParentCap {
		return "", "", "", nil, false // (7) full epic → companion (binding condition 4)
	}

	// AUTHORITATIVE decision under the per-epic allocation lock, HELD across the
	// caller's File so the capacity decision is effective under the same
	// synchronization deriveChildNumberTitleVar uses (high/concurrency TOCTOU).
	unlock := lockChildNumberKey(childNumberLockKey(target, epicRef))
	res, err := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: target, Epic: epicRef})
	if err != nil {
		unlock()
		return "", "", "", nil, false // (7) child count unreadable under the lock → companion
	}
	if len(res.Children) >= githubSubIssueParentCap {
		// Raced to the cap between the pre-count and here: degrade to companion
		// rather than take the epic arm to a doomed over-cap attachment.
		unlock()
		return "", "", "", nil, false // (7) full epic (binding condition 4)
	}
	n, ok := workmgmt.NextChildNumber(choreType.TitleFormat, m[1], res.Children)
	if !ok {
		// Children exist but none carry the numbered [E<epic>.<n>] form, so {n}
		// cannot be allocated (#2101). Degrade to companion — a filed companion is
		// a better outcome than a filing failure (binding condition 4 spirit).
		unlock()
		return "", "", "", nil, false // (7)
	}
	return epicRef, m[1], strconv.Itoa(n), unlock, true
}

// fileLiveValidationChore files the `chore`-type operator-validation walk work
// item and returns ("#N", true) on a successful filing and ("", false) on any
// failure (the caller then writes a filing-failure linked marker).
//
// TWO ARMS, ONE FILING (#2179). The originating issue (#2045) asks for the walk
// filed "under the epic"; this hook now resolves the triggering child's TRUE
// epic via resolveWalkParentEpic (the sub-issue-PARENT query Client.IssueParent
// added). When that resolves — the parent's title is the bracket-closed [E<n>]
// epic form, the provider can discover a child number, and the epic is not full
// — the walk is filed with Relations.ParentEpic = "#<epic>" and TitleVars{epic,n}
// resolved under a HELD per-epic lock resolveWalkParentEpic returns. The lock is
// held across applyAndFileWorkItem and released here (defer unlockEpic), so the
// capacity decision AND the File are serialized against a concurrent filer
// (high/concurrency TOCTOU, #2179 fix-up). The explicit {n} means
// deriveChildNumberTitleVar short-circuits and does NOT re-take the lock. On
// EVERY resolution failure — the fallback modes enumerated on
// resolveWalkParentEpic (no client, zero scope, IssueParent error, null parent,
// non-epic parent title, unregistered/no-EpicChildrenQuerier provider, or a
// resolvable-but-UNATTACHABLE epic: full, unreadable, or non-numbered children)
// — it falls back to today's exact companion-link filing byte-for-byte: an
// explicit {epic}=<issue>/{n}=1 title that renders [E<issue>.1] and CompanionTo
// the triggering issue, which neither mis-parents nor collides with the real
// epic's child numbering.
//
// The arm is decided ONCE, BEFORE the single applyAndFileWorkItem call, so the
// single-filing / no-double-file invariant (#2045) is untouched: any error (a
// pre-File 422 or a post-File 502 alike) routes to the filing_failure linked
// marker, never to a second differently-shaped walk.
//
// RESIDUAL: on the epic arm the File itself can still fail (a provider 502),
// which routes to the filing_failure marker (the operator files by hand), NOT to
// a companion retry — a second attempt within one approval would reopen the
// same-approval double-file window. The cap decision no longer fails at file
// time: {n} is allocated under the held lock BEFORE File, so the resolvable-but-
// unattachable cases (full, unreadable, non-numbered children) are resolved to
// companion in resolveWalkParentEpic rather than surfacing as an at-file 422.
func (s *Server) fileLiveValidationChore(ctx context.Context, runRow *run.Run, owner, name string, parentIssue int, crits []plan.AcceptanceCriterion) (string, bool) {
	conv, err := conventionsLoader(ctx, runRow.Repo)
	if err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "load work-management conventions failed", err.Error())
		return "", false
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	if runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil {
		if scope, rerr := s.resolveRepoScope(ctx, owner, name); rerr == nil {
			target.Scope = scope
		}
	}

	parentRef := "#" + strconv.Itoa(parentIssue)
	summary := "Operator live-validation walk for " + parentRef

	// Decide the arm ONCE, before the single File call below. On the epic arm
	// resolveWalkParentEpic returns the pre-computed child number AND a HELD
	// per-epic allocation lock (unlockEpic); hold it across applyAndFileWorkItem
	// so the capacity decision and the File are serialized against a concurrent
	// filer, then release it (high/concurrency TOCTOU, #2179 fix-up).
	epicRef, epicVar, epicN, unlockEpic, epicArm := s.resolveWalkParentEpic(ctx, target.Scope, owner, name, parentIssue, conv)
	if epicArm {
		defer unlockEpic()
	}

	var req workmgmt.FilingRequest
	if epicArm {
		// Epic arm: parent under the TRUE epic with the {n} allocated under the
		// held lock. Passing {n} EXPLICITLY makes deriveChildNumberTitleVar
		// short-circuit so it does not re-take the per-epic lock (no deadlock).
		req = workmgmt.FilingRequest{
			Type:      "chore",
			Summary:   summary,
			Body:      liveValidationWalkBody(parentRef, epicRef, crits, false),
			Labels:    []string{liveValidationWalkArea},
			TitleVars: map[string]string{"epic": epicVar, "n": epicN},
			Relations: workmgmt.Relations{
				ParentEpic:   epicRef,
				EvidenceRuns: []string{runRow.ID.String()},
			},
		}
	} else {
		// Companion arm: the pre-#2179 filing, unchanged byte-for-byte.
		req = workmgmt.FilingRequest{
			Type:      "chore",
			Summary:   summary,
			Body:      liveValidationWalkBody(parentRef, "", crits, true),
			Labels:    []string{liveValidationWalkArea},
			TitleVars: map[string]string{"epic": strconv.Itoa(parentIssue), "n": "1"},
			Relations: workmgmt.Relations{
				CompanionTo:  []string{parentRef},
				EvidenceRuns: []string{runRow.ID.String()},
			},
		}
	}
	if _, created, werr := s.applyAndFileWorkItem(ctx, req, conv, target, owner, name); werr == nil {
		return fmt.Sprintf("#%d", created.Number), true
	} else {
		s.logLiveValidationWarn(ctx, runRow.ID, "live-validation walk filing failed", werr.msg)
		return "", false
	}
}

// liveValidationWalkBody assembles the walk body: what the walk is, the criteria
// awaiting an operator live check, and either (companion=true) a companion-link
// to the triggering issue or (companion=false, the epic arm) a Parent-epic
// reference plus a Filed-for line. The companion=true output is byte-IDENTICAL
// to the pre-#2179 body (pinned by TestLiveValidationWalkBody_CompanionByteIdentity),
// so the fallback arm — the one that fires today for every walk — is unchanged.
func liveValidationWalkBody(triggerRef, epicRef string, crits []plan.AcceptanceCriterion, companion bool) string {
	body := "## Summary\n\nThis run's approved plan carries acceptance criteria whose true verification " +
		"needs a live forge/deploy/external target the default-deny acceptance sandbox cannot reach " +
		"(`requires_live_validation`). The acceptance stage short-circuits them; this walk tracks the " +
		"operator live check so nothing ships silently unvalidated (#2045).\n\n"
	if companion {
		body += "Companion to " + triggerRef + ".\n\n"
	} else {
		body += "Parent epic: " + epicRef + ".\n\n"
		body += "Filed for " + triggerRef + ".\n\n"
	}
	body += "## Done-means\n\nEach criterion below has been live-validated by the operator against the real target:\n\n"
	for _, c := range crits {
		stmt := c.Statement
		if stmt == "" {
			stmt = c.ID
		}
		body += fmt.Sprintf("- [ ] `%s` — %s\n", c.ID, stmt)
	}
	return body
}

// appendLiveValidationMarker appends one intent-or-linked live-validation walk
// audit marker under the given category. It returns the append error (also
// WARN-logged by the caller) so the intent-marker append can be treated as the
// hard idempotency prerequisite.
func (s *Server) appendLiveValidationMarker(ctx context.Context, runRow *run.Run, category string, marker liveValidationWalkMarker) error {
	payload, _ := json.Marshal(marker)
	systemKind := audit.ActorSystem
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	return err
}

// liveValidationForRun distills the run's pending operator live-validation walk
// (#2045) from the newest live_validation walk marker. The single-run reads
// (handleGetRun, buildGateView) call it with the same best-effort posture as the
// other distilled surfaces: a nil AuditRepo or a read failure degrades to an
// omitted field (WARN, never a failed read), and a run with no marker returns
// nil.
//
// Marker precedence (binding condition A(1)):
//   - A linked marker (the forge outcome) wins over any earlier intent marker.
//     A healthy linked marker (walk_ref set) renders "walk: #N"; a filing-failure
//     linked marker (filing_failed) renders the file-manually variant.
//   - A stranded intent marker (an intent marker with NO linked marker following
//     it — the crash-window case) renders the file-manually variant too
//     (filing_failed=true), additionally flagged filing_incomplete so a consumer
//     can word it "walk filing incomplete" vs "walk filing failed". It is NEVER
//     rendered as the healthy "walk: #N" variant and never as a malformed
//     empty-ref string.
func (s *Server) liveValidationForRun(ctx context.Context, runID uuid.UUID) *runLiveValidationPayload {
	if s.cfg.AuditRepo == nil {
		return nil
	}
	linked, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkLinkedKind)
	if err != nil {
		s.cfg.Logger.Warn("list live-validation linked markers failed; omitting live_validation block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(linked) > 0 {
		// Newest wins: ListForRunByCategory is sequence-ascending.
		newest := linked[len(linked)-1]
		var m liveValidationWalkMarker
		if uerr := json.Unmarshal(newest.Payload, &m); uerr != nil {
			s.cfg.Logger.Warn("decode live-validation linked marker failed; omitting live_validation block",
				"run_id", runID.String(), "error", uerr.Error())
			return nil
		}
		return &runLiveValidationPayload{
			PendingCriteriaCount: m.PendingCriteriaCount,
			WalkRef:              m.WalkRef,
			FilingFailed:         m.FilingFailed,
		}
	}

	// No linked marker: a stranded intent marker (the crash-window case) still
	// surfaces as file-manually so the pending criteria are never silently
	// accepted.
	intent, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, liveValidationWalkIntentKind)
	if err != nil {
		s.cfg.Logger.Warn("list live-validation intent markers failed; omitting live_validation block",
			"run_id", runID.String(), "error", err.Error())
		return nil
	}
	if len(intent) == 0 {
		return nil
	}
	newest := intent[len(intent)-1]
	var m liveValidationWalkMarker
	if uerr := json.Unmarshal(newest.Payload, &m); uerr != nil {
		s.cfg.Logger.Warn("decode live-validation intent marker failed; omitting live_validation block",
			"run_id", runID.String(), "error", uerr.Error())
		return nil
	}
	return &runLiveValidationPayload{
		PendingCriteriaCount: m.PendingCriteriaCount,
		FilingFailed:         true,
		FilingIncomplete:     true,
	}
}

// logLiveValidationWarn is the shared WARN logger for the best-effort hook, so
// no branch fails the approval silently.
func (s *Server) logLiveValidationWarn(ctx context.Context, runID uuid.UUID, msg, detail string) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "live-validation filing: "+msg,
		slog.String("run_id", runID.String()),
		slog.String("detail", detail),
	)
}
