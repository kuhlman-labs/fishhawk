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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
	// ChecklistAnchor is this run's section anchor ("run-<run_id>") inside the
	// ROLLING per-epic walk (#3323). Set on a healthy linked marker whose walk
	// carries a per-run section; empty on an intent marker, a filing-failure
	// marker, and a companion-arm walk (which carries no per-run section).
	ChecklistAnchor string `json:"checklist_anchor,omitempty"`
	// WalkAppended is true on a healthy linked marker when the run APPENDED its
	// section to an existing rolling walk rather than filing a new one (#3323).
	// Diagnostic; empty on the file-new path and on every non-healthy marker.
	WalkAppended bool `json:"walk_appended,omitempty"`
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
	// ChecklistAnchor is this run's section anchor ("run-<run_id>") inside the
	// rolling per-epic walk (#3323), copied off the newest healthy linked marker.
	// Empty when the walk was not durably filed (a stranded intent marker or a
	// filing failure has no section) and on the companion arm.
	ChecklistAnchor string `json:"checklist_anchor,omitempty"`
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
	walkRef, anchor, appended, filed := s.fileLiveValidationChore(ctx, runRow, owner, name, parentIssue, crits)
	linked := liveValidationWalkMarker{
		Phase:                "linked",
		PendingCriteriaCount: len(crits),
		CriterionIDs:         ids,
	}
	if filed {
		linked.WalkRef = walkRef
		// ChecklistAnchor is set only on the ROLLING epic arm (#3323); the
		// companion arm returns an empty anchor.
		linked.ChecklistAnchor = anchor
		linked.WalkAppended = appended
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

// liveValidationRollingKey is the stable per-(repo, epic) identity of the
// ROLLING live-validation walk (#3323). It is stamped into the rolling walk's
// body as a hidden idempotency marker (via FilingRequest.IdempotencyKey), so a
// later run under the SAME epic re-derives the byte-identical key and adopts the
// existing walk by a WHOLE-LINE body-marker match (workmgmt.BodyHasIdempotencyKey)
// rather than a fragile title match — an operator retitling the walk cannot
// orphan it. Marker-keyed discovery is the ratified deviation from the issue's
// title-match proposal; do not weaken it to a title match.
func liveValidationRollingKey(repoFullName, epicRef string) string {
	return workmgmt.MintIdempotencyKey("live_validation_walk_rolling", repoFullName, epicRef)
}

// liveValidationSectionKey is the per-run identity of one appended checklist
// section (#3323), stamped as a hidden whole-line marker inside the section so a
// re-entry (a re-approval that reaches the append) can recognize its own section
// already present and write NOTHING rather than duplicating it.
func liveValidationSectionKey(runID uuid.UUID) string {
	return workmgmt.MintIdempotencyKey("live_validation_walk_section", runID.String())
}

// walkEpicResolution is resolveWalkParentEpic's outcome, carrying the adoption /
// allocation decision honestly (binding condition — separate ADOPTION from
// ALLOCATION). EpicArm is false for every companion-degrade fallback. On the
// epic arm exactly one of two paths holds:
//
//   - AppendTo > 0: an OPEN rolling walk already exists under this epic; APPEND a
//     section to it. This path is taken regardless of the sub-issue cap and
//     regardless of whether {n} is allocatable — appending needs no new child,
//     no addSubIssue and no {n}. ChildN is a BEST-EFFORT allocation carried only
//     so a rare append DEGRADE (the candidate unreadable/closed at the fresh
//     read) can still file a new rolling walk; it is legitimately "" here.
//   - AppendTo == 0: no candidate; FILE a new rolling walk with the allocated
//     ChildN. Only THIS path is gated by the cap and the NextChildNumber
//     allocation (binding condition — allocation gates only the file-new path).
//
// Unlock is the HELD per-epic allocation lock the caller releases after its
// File/append (nil when EpicArm is false).
type walkEpicResolution struct {
	EpicArm  bool
	EpicRef  string // "#<epic issue>"
	EpicVar  string // "<epic digits>"
	ChildN   string // allocated {n} for a file-new; "" is legal on the append path
	AppendTo int    // >0 → append to this open rolling walk's issue number
	Unlock   func()
}

// findHighestOpenRollingCandidate returns the HIGHEST-numbered child that carries
// the rolling key in its body AND is positively OPEN (State == "OPEN"), or nil.
// The OPEN requirement is strict: an empty State (a provider that does not
// populate it) is UNKNOWN and NEVER adopts, so the fail direction is the
// pre-#3323 file-a-new-walk status quo. Highest-numbered is deterministic when a
// prior degrade left two open candidates.
func findHighestOpenRollingCandidate(children []workmgmt.EpicChild, rollingKey string) *workmgmt.EpicChild {
	var best *workmgmt.EpicChild
	for i := range children {
		c := &children[i]
		if c.State != "OPEN" {
			continue // empty/CLOSED → not adoptable
		}
		if !workmgmt.BodyHasIdempotencyKey(c.Body, rollingKey) {
			continue
		}
		if best == nil || c.Number > best.Number {
			best = c
		}
	}
	return best
}

// resolveWalkParentEpic decides whether the live-validation walk should be
// parented under the triggering child's TRUE epic (#2179, ROLLING per epic
// #3323) instead of companion-linked to the child. It returns EpicArm=true only
// when EVERY precondition holds; otherwise EpicArm is false and the caller files
// the UNCHANGED companion walk. Each companion-degrade fallback is an explicit
// early return:
//
//	(1) no GitHub client wired;
//	(2) a zero credential scope;
//	(3) IssueParent errors;
//	(4) IssueParent returns no parent (the triggering issue has no sub-issue parent);
//	(5) the parent's title is not the bracket-closed [E<n>] epic form (e.g. it is
//	    another child, [E48.35]) — walkEpicTitleRE, NOT the child-accepting epicTitleRE;
//	(6) the resolved provider is unregistered (workmgmt.Get errors) or does not
//	    implement EpicChildrenQuerier — both degrade the same safe way;
//	(7) the epic is resolvable but the AUTHORITATIVE child read fails, or — WHEN
//	    THERE IS NO OPEN ROLLING CANDIDATE — the epic is at the sub-issue cap or
//	    its children carry no numbered [E<epic>.<n>] form so {n} cannot be
//	    allocated (#2101); degrade to companion (binding condition 4).
//
// ADOPTION BEFORE ALLOCATION (binding condition). Under the HELD per-epic lock,
// using the AUTHORITATIVE EpicChildren result, an OPEN rolling candidate is
// looked for FIRST. If one exists the append path is taken REGARDLESS of the cap
// and REGARDLESS of whether {n} is allocatable — appending needs no new child.
// Only when there is NO candidate do the cap and NextChildNumber failures degrade
// to companion. The cap/allocation gating therefore CANNOT route to companion an
// epic that still holds a perfectly good open rolling walk — the exact inertness
// (E22 #389, E48 #1940, E67 #2561 are at the cap today) this issue exists to fix.
//
// CAP/ALLOCATION UNDER THE ALLOCATION LOCK (high/concurrency TOCTOU, #2179
// fix-up). The file-new capacity decision and the {n} allocation are BOTH made
// under the SAME per-epic childNumberLock deriveChildNumberTitleVar takes, and
// the lock stays HELD (returned as Unlock) across the caller's File. The
// unlocked pre-count still fast-rejects an obviously-full epic WITHOUT the lock —
// but ONLY when the pre-count shows no candidate, so a capped epic with a
// candidate is never fast-rejected before the authoritative candidate scan.
func (s *Server) resolveWalkParentEpic(ctx context.Context, scope forge.CredentialScope, owner, name string, childIssue int, conv workmgmt.Conventions, repoFullName string) walkEpicResolution {
	none := walkEpicResolution{}
	if s.cfg.GitHub == nil {
		return none // (1)
	}
	if scope.IsZero() {
		return none // (2)
	}
	parent, err := s.cfg.GitHub.IssueParent(ctx, scope, forge.RepoRef{Owner: owner, Name: name}, childIssue)
	if err != nil {
		return none // (3)
	}
	if parent == nil {
		return none // (4)
	}
	m := walkEpicTitleRE.FindStringSubmatch(parent.Title)
	if m == nil {
		return none // (5) not the bracket-closed epic form
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		return none // (6) unregistered provider → no querier capability reachable, safe companion degrade
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return none // (6) no capability to query children
	}
	choreType, ok := conv.Types["chore"]
	if !ok || !strings.Contains(choreType.TitleFormat, "{n}") {
		return none // (6) the chore type carries no {n} placeholder → companion
	}
	epicRef := "#" + strconv.Itoa(parent.Number)
	epicVar := m[1]
	rollingKey := liveValidationRollingKey(repoFullName, epicRef)
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		Scope:   scope,
	}

	// Fast reject: an obviously-full (or unreadable) epic THAT HOLDS NO OPEN
	// ROLLING CANDIDATE degrades to companion without taking the allocation lock.
	// The candidate scan is gated FIRST so a capped epic with an adoptable walk is
	// never fast-rejected here (binding condition) — the AUTHORITATIVE decision is
	// still the locked read below.
	if res, perr := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: target, Epic: epicRef}); perr != nil {
		return none // (7) child count unreadable → companion
	} else if findHighestOpenRollingCandidate(res.Children, rollingKey) == nil && len(res.Children) >= githubSubIssueParentCap {
		return none // (7) full epic, no candidate to adopt → companion (binding condition 4)
	}

	// AUTHORITATIVE decision under the per-epic allocation lock, HELD across the
	// caller's File/append.
	unlock := lockChildNumberKey(childNumberLockKey(target, epicRef))
	res, err := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: target, Epic: epicRef})
	if err != nil {
		unlock()
		return none // (7) child count unreadable under the lock → companion
	}

	// ADOPTION FIRST: an open rolling walk is appendable regardless of the cap and
	// regardless of {n} allocatability. ChildN is allocated best-effort only for a
	// possible append DEGRADE; its absence never blocks the append path.
	if cand := findHighestOpenRollingCandidate(res.Children, rollingKey); cand != nil {
		childN := ""
		if n, ok := workmgmt.NextChildNumber(choreType.TitleFormat, epicVar, res.Children); ok {
			childN = strconv.Itoa(n)
		}
		return walkEpicResolution{EpicArm: true, EpicRef: epicRef, EpicVar: epicVar, ChildN: childN, AppendTo: cand.Number, Unlock: unlock}
	}

	// ALLOCATION (file-new path only): the cap and NextChildNumber gate here,
	// AFTER the candidate scan.
	if len(res.Children) >= githubSubIssueParentCap {
		unlock()
		return none // (7) full epic (binding condition 4)
	}
	n, ok := workmgmt.NextChildNumber(choreType.TitleFormat, epicVar, res.Children)
	if !ok {
		// Children exist but none carry the numbered [E<epic>.<n>] form, so {n}
		// cannot be allocated (#2101). Degrade to companion (binding condition 4).
		unlock()
		return none // (7)
	}
	return walkEpicResolution{EpicArm: true, EpicRef: epicRef, EpicVar: epicVar, ChildN: strconv.Itoa(n), Unlock: unlock}
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
// RESIDUAL — two distinct cases, stated precisely (high/concurrency, #2179
// fix-up). The earlier comment here overclaimed; both are corrected:
//
//  1. A provider CreateIssue failure (a 502) is the ONLY fatal File step; it
//     routes to the filing_failure marker (the operator files by hand), NOT to a
//     companion retry — a second attempt within one approval would reopen the
//     same-approval double-file window.
//  2. The CROSS-PROCESS cap window is NARROWED, not closed. Allocating {n} under
//     the held lock BEFORE File resolves the resolvable-but-unattachable cases
//     (full, unreadable, non-numbered children) to companion WITHIN THIS DAEMON —
//     so the cap decision no longer fails at file time for filers THIS process
//     serializes. It does NOT close the window against writers this process
//     cannot see (another fishhawkd instance, a human adding a sub-issue in the
//     GitHub UI, the grooming apply hook) reaching the cap between our locked read
//     and our File. In that window applyAndFileWorkItem's File still CREATES the
//     [E<epic>.<n>] issue, and the epic attach (Provider.File's linkEpic /
//     AddSubIssue) is NON-fatal: GitHub rejects the 101st addSubIssue as
//     ErrValidation, captured as created.EpicLinkError (a WARN, not a File error).
//     The outcome is a filed-but-UNPARENTED walk — NOT a filing_failure and NOT a
//     companion, so binding condition 4's companion-degrade is not honored in this
//     residual window. Closing it needs create-then-attach as SEPARATE
//     workmgmt.Provider steps (so a cap refusal degrades to a re-LINK, not a
//     re-FILE that would violate the #2045 single-filing invariant); that is a
//     provider-seam change out of #2179's scope, and is DECLINED here rather than
//     silently substituting the operator-pre-rejected lock-tighter-only design.
//     A filed unparented walk is still a better outcome than a hook that errors,
//     and the residual is rare and visible via the EpicLinkError WARN. See
//     backend/internal/server/README.md.
func (s *Server) fileLiveValidationChore(ctx context.Context, runRow *run.Run, owner, name string, parentIssue int, crits []plan.AcceptanceCriterion) (walkRef, anchor string, appended, filed bool) {
	conv, err := conventionsLoader(ctx, runRow.Repo)
	if err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "load work-management conventions failed", err.Error())
		return "", "", false, false
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
	// resolveWalkParentEpic returns a HELD per-epic allocation lock; hold it
	// across the append/File so the adoption/allocation decision is serialized
	// against a concurrent filer, then release it (high/concurrency TOCTOU).
	res := s.resolveWalkParentEpic(ctx, target.Scope, owner, name, parentIssue, conv, runRow.Repo)
	if res.EpicArm {
		defer res.Unlock()
	}

	if res.EpicArm {
		rollingKey := liveValidationRollingKey(runRow.Repo, res.EpicRef)
		if res.AppendTo > 0 {
			// ADOPTION: append this run's section to the existing open rolling walk
			// (#3323). No File, no addSubIssue, no {n}.
			a, appended, retryable := s.appendRollingWalkSection(ctx, target.Scope, runRow, owner, name, res.AppendTo, parentRef, crits)
			if appended {
				return fmt.Sprintf("#%d", res.AppendTo), a, true, true
			}
			if !retryable {
				// UpdateIssue FAILED on an existing walk: route to filing_failed and
				// NEVER file a second walk — a re-file would reopen the same-approval
				// double-file window (#2045). The walk exists; the operator's surface
				// renders file-manually and a re-approval no-ops on the intent marker.
				return "", "", false, false
			}
			// Retryable degrade (the candidate could not be READ, or was CLOSED at the
			// fresh read): file a NEW rolling walk when {n} is allocatable (never a
			// lost walk); else fall through to the companion arm.
			if res.ChildN == "" {
				s.logLiveValidationWarn(ctx, runRow.ID, "rolling walk append degraded and no allocatable child number; filing companion", res.EpicRef)
			} else {
				ref, a, ok := s.fileNewRollingWalk(ctx, runRow, conv, target, owner, name, res, parentRef, rollingKey, crits)
				return ref, a, false, ok
			}
		} else {
			// No candidate: FIRST rolling filing under this epic.
			ref, a, ok := s.fileNewRollingWalk(ctx, runRow, conv, target, owner, name, res, parentRef, rollingKey, crits)
			return ref, a, false, ok
		}
	}

	// Companion arm: the pre-#2179 filing, unchanged byte-for-byte. It carries no
	// per-run rolling section, so its anchor is empty.
	req := workmgmt.FilingRequest{
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
	if _, created, werr := s.applyAndFileWorkItem(ctx, req, conv, target, owner, name); werr == nil {
		return fmt.Sprintf("#%d", created.Number), "", false, true
	} else {
		s.logLiveValidationWarn(ctx, runRow.ID, "live-validation walk filing failed", werr.msg)
		return "", "", false, false
	}
}

// fileNewRollingWalk files the FIRST rolling walk under an epic (#3323): summary
// "Operator live-validation walk (rolling)", the rolling body carrying this run's
// first section, and the rolling idempotency key stamped into the body so the
// NEXT run under this epic adopts it. The explicit {n} (allocated under the held
// per-epic lock) makes deriveChildNumberTitleVar short-circuit so it does not
// re-take the lock (no deadlock). Returns ("#N", "run-<id>", true) on success and
// ("", "", false) on a File failure (the caller routes that to filing_failed).
func (s *Server) fileNewRollingWalk(ctx context.Context, runRow *run.Run, conv workmgmt.Conventions, target workmgmt.Target, owner, name string, res walkEpicResolution, parentRef, rollingKey string, crits []plan.AcceptanceCriterion) (string, string, bool) {
	body, anchor := liveValidationRollingWalkBody(runRow.ID, res.EpicRef, parentRef, crits)
	req := workmgmt.FilingRequest{
		Type:           "chore",
		Summary:        "Operator live-validation walk (rolling)",
		Body:           body,
		Labels:         []string{liveValidationWalkArea},
		TitleVars:      map[string]string{"epic": res.EpicVar, "n": res.ChildN},
		IdempotencyKey: rollingKey,
		Relations: workmgmt.Relations{
			ParentEpic:   res.EpicRef,
			EvidenceRuns: []string{runRow.ID.String()},
		},
	}
	_, created, werr := s.applyAndFileWorkItem(ctx, req, conv, target, owner, name)
	if werr != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "live-validation rolling walk filing failed", werr.msg)
		return "", "", false
	}
	return fmt.Sprintf("#%d", created.Number), anchor, true
}

// appendRollingWalkSection appends this run's checklist section to the existing
// open rolling walk (#3323), running INSIDE the already-held per-epic lock. Order
// (a lost-update read-modify-write narrowed by that lock, not closed across
// processes — see backend/internal/server/README.md):
//
//	(a) GetIssue for a FRESH authoritative body+state (the EpicChildren snapshot
//	    is stale by construction);
//	(b) if the fresh state is not open, treat as no candidate — return ok=false so
//	    the caller files a new rolling walk;
//	(c) if the fresh body already carries this run's section key, the section is
//	    already present (idempotent re-entry): return the anchor, write NOTHING;
//	(d) otherwise UpdateIssue with body + the rendered section.
//
// Return contract, three cases the caller acts on distinctly:
//
//	appended=true                    → the section is written (or already present).
//	appended=false, retryable=true   → the candidate is unusable (GetIssue error or
//	                                   CLOSED at the fresh read): file a NEW rolling
//	                                   walk (never a lost walk).
//	appended=false, retryable=false  → UpdateIssue FAILED on an existing walk: route
//	                                   to filing_failed and NEVER file a second walk
//	                                   (the #2045 double-file window).
func (s *Server) appendRollingWalkSection(ctx context.Context, scope forge.CredentialScope, runRow *run.Run, owner, name string, walkNumber int, triggerRef string, crits []plan.AcceptanceCriterion) (anchor string, appended, retryable bool) {
	if s.cfg.GitHub == nil {
		return "", false, true
	}
	repo := forge.RepoRef{Owner: owner, Name: name}
	issue, err := s.cfg.GitHub.GetIssue(ctx, scope, repo, walkNumber)
	if err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "rolling walk fresh read failed; filing new walk", fmt.Sprintf("#%d: %v", walkNumber, err))
		return "", false, true // retryable → file a new walk
	}
	// (b) The snapshot said OPEN, but the fresh read is authoritative — a walk
	// closed between the snapshot and here is no longer a candidate. REST lowercases
	// state, so compare case-insensitively.
	if !strings.EqualFold(issue.State, "open") {
		s.logLiveValidationWarn(ctx, runRow.ID, "rolling walk closed at fresh read; filing new walk", fmt.Sprintf("#%d state=%q", walkNumber, issue.State))
		return "", false, true // retryable → file a new walk
	}
	section, a := liveValidationRunSection(runRow.ID, triggerRef, crits)
	// (c) Idempotent re-entry: this run's section is already present.
	if workmgmt.BodyHasIdempotencyKey(issue.Body, liveValidationSectionKey(runRow.ID)) {
		return a, true, false
	}
	// (d) Append the section.
	newBody := strings.TrimRight(issue.Body, "\n") + "\n\n" + section
	if _, err := s.cfg.GitHub.UpdateIssue(ctx, scope, repo, walkNumber, githubclient.UpdateIssueParams{Body: &newBody}); err != nil {
		s.logLiveValidationWarn(ctx, runRow.ID, "rolling walk section append (UpdateIssue) failed", fmt.Sprintf("#%d: %v", walkNumber, err))
		return "", false, false // NOT retryable → filing_failed, never re-file
	}
	return a, true, false
}

// liveValidationRunSection renders one per-run checklist section for the rolling
// walk (#3323) and returns (section, anchor). The `### Run <run-id>` heading's
// GitHub slug IS the anchor `run-<run-id>`; the hidden section marker line keys
// the per-run idempotent re-entry; a `Filed for <triggerRef>.` line names the
// triggering issue; and each criterion is a checkbox bullet with an indented
// `Verify:` continuation when it carries a verify_hint.
func liveValidationRunSection(runID uuid.UUID, triggerRef string, crits []plan.AcceptanceCriterion) (string, string) {
	anchor := "run-" + runID.String()
	var b strings.Builder
	fmt.Fprintf(&b, "### Run %s\n\n", runID.String())
	b.WriteString(workmgmt.StampIdempotencyKey("", liveValidationSectionKey(runID)))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Filed for %s.\n\n", triggerRef)
	for _, c := range crits {
		stmt := c.Statement
		if stmt == "" {
			stmt = c.ID
		}
		fmt.Fprintf(&b, "- [ ] `%s` — %s\n", c.ID, stmt)
		if c.VerifyHint != "" {
			fmt.Fprintf(&b, "  Verify: %s\n", c.VerifyHint)
		}
	}
	return b.String(), anchor
}

// liveValidationRollingWalkBody assembles the body of a NEW rolling walk's first
// filing (#3323): the walk summary, a Parent-epic reference, a line stating that
// each run appends its own section and the walk closes only when every section is
// ticked, then this run's first section. It returns (body, anchor).
func liveValidationRollingWalkBody(runID uuid.UUID, epicRef, triggerRef string, crits []plan.AcceptanceCriterion) (string, string) {
	body := "## Summary\n\nThis run's approved plan carries acceptance criteria whose true verification " +
		"needs a live forge/deploy/external target the default-deny acceptance sandbox cannot reach " +
		"(`requires_live_validation`). The acceptance stage short-circuits them; this walk tracks the " +
		"operator live check so nothing ships silently unvalidated (#2045).\n\n"
	body += "Parent epic: " + epicRef + ".\n\n"
	body += "This is a ROLLING walk (#3323): each run under this epic appends its own section below, " +
		"and the walk closes only when EVERY section's criteria are ticked.\n\n"
	section, anchor := liveValidationRunSection(runID, triggerRef, crits)
	body += section
	return body, anchor
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
			// The rolling per-epic section anchor (#3323); empty on a filing-failure
			// marker and on the companion arm (neither carries a per-run section).
			ChecklistAnchor: m.ChecklistAnchor,
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
