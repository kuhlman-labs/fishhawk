package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Plan-stage issue-context refresh (#5219).
//
// runs.issue_context is a snapshot taken at run creation. Before #5219 the
// prompt handlers preferred it unconditionally (#415 cache-wins), so a comment
// or body edit made after the run was minted — a late steer — never reached the
// planner. On a PLAN stage of a run that holds a forge credential, the prompt
// handlers and the plan review now re-read the issue's live title, body and
// comments and render an IN-MEMORY copy of the run row. Any failure falls back
// to the cached snapshot. The persisted runs.issue_context is NEVER rewritten:
// the labels snapshot applies_to reads stays immutable (#2226). Non-plan stages
// keep cache-wins.
//
// Three audit rows make the refresh observable. They are best-effort
// observability, never authorization, and carry ids, booleans, counts, closed
// slugs and sha256 digests only — never issue text.
//
//   - issue_context_refreshed, surface plan_prompt: a signed /prompt fetch found
//     the live prompt-visible content differs from the cached snapshot. Deduped
//     per (stage, snapshot digest), so a re-fetch of the same live content
//     appends nothing while a second, different steer appends a second row.
//   - issue_context_rendered: every signed plan-stage /prompt fetch records the
//     digest of the snapshot it rendered (refreshed or cached). Not drift-gated
//     and never deduped against OLDER digests, so the newest row is what the
//     planner last rendered even across an edit-and-revert (A→B→A) or a
//     drift-then-revert-to-cache; only a repeat of the stage's newest digest is
//     skipped, which bounds the chain without changing what the newest row says.
//   - issue_context_refreshed, surface plan_review: the plan review rendered a
//     different snapshot than the planner's newest issue_context_rendered row —
//     either the issue changed between the two fetches (refreshed=true) or the
//     review's own live fetch fell back to the cache (refreshed=false).
//     Detected, not prevented: exact parity needs a stored per-stage snapshot
//     (deferred).
//
// The live read is bounded by issueContextRefreshTimeout across both forge
// calls, so a slow forge falls back to the cache rather than stalling a prompt
// fetch or a plan upload.
const (
	issueContextRefreshedCategory = "issue_context_refreshed"
	issueContextRenderedCategory  = "issue_context_rendered"
	refreshSurfacePlanPrompt      = "plan_prompt"
	refreshSurfacePlanReview      = "plan_review"
)

// liveIssueSnapshot is one live read of the issue. CommentsOK is false when the
// comment fetch failed, in which case the cached comments are kept.
type liveIssueSnapshot struct {
	Title      string
	Body       string
	Comments   []run.IssueComment
	CommentsOK bool
}

// promptVisibleComment reports whether c survives the prompt renderer's bot
// filter. Keep it in sync with prompt.writeIssueComments' case-sensitive
// "[bot]" suffix filter (backend/internal/prompt/prompt.go, renderIssueComments).
// The predicate is mirrored rather than shared because the prompt package is
// outside #5219's scope; TestRefreshPlanIssueContext_LateBotCommentAppendsNoRow
// pins it.
func promptVisibleComment(c run.IssueComment) bool {
	return !strings.HasSuffix(c.Author, "[bot]")
}

// normalizeCommentCreatedAt maps an RFC 3339 timestamp onto one canonical UTC
// spelling, so `gh`'s createdAt, GitHub REST's created_at and GitLab's
// millisecond ISO 8601 compare equal for the same instant. An unparseable value
// is kept raw.
func normalizeCommentCreatedAt(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// commentKey is a comment's identity across two snapshots: author + NUL +
// normalized created_at. A repeated key gets an occurrence suffix so equal keys
// form a multiset rather than collapsing.
func commentKey(c run.IssueComment, seen map[string]int) string {
	key := c.Author + "\x00" + normalizeCommentCreatedAt(c.CreatedAt)
	n := seen[key]
	seen[key] = n + 1
	if n > 0 {
		key += "\x00" + strconv.Itoa(n)
	}
	return key
}

// issueContextDrift is the prompt-visible difference between the cached and
// the live snapshot.
type issueContextDrift struct {
	TitleChanged    bool
	BodyChanged     bool
	CommentsCached  int
	CommentsLive    int
	CommentsAdded   int
	CommentsRemoved int
	CommentsEdited  int
}

func (d issueContextDrift) any() bool {
	return d.TitleChanged || d.BodyChanged || d.CommentsAdded != 0 || d.CommentsRemoved != 0 || d.CommentsEdited != 0
}

// diffIssueSnapshots compares ONLY prompt-visible content: title, body and the
// comments promptVisibleComment keeps. Added, removed and body-edited comments
// are counted separately.
func diffIssueSnapshots(cached, live *run.IssueContext) issueContextDrift {
	d := issueContextDrift{
		TitleChanged: cached.Title != live.Title,
		BodyChanged:  cached.Body != live.Body,
	}
	cachedBodies := map[string]string{}
	cachedSeen := map[string]int{}
	for _, c := range cached.Comments {
		if !promptVisibleComment(c) {
			continue
		}
		d.CommentsCached++
		cachedBodies[commentKey(c, cachedSeen)] = c.Body
	}
	matched := map[string]bool{}
	liveSeen := map[string]int{}
	for _, c := range live.Comments {
		if !promptVisibleComment(c) {
			continue
		}
		d.CommentsLive++
		key := commentKey(c, liveSeen)
		if prevBody, ok := cachedBodies[key]; ok {
			matched[key] = true
			if prevBody != c.Body {
				d.CommentsEdited++
			}
			continue
		}
		d.CommentsAdded++
	}
	for key := range cachedBodies {
		if !matched[key] {
			d.CommentsRemoved++
		}
	}
	return d
}

// issueSnapshotDigest is "sha256:" + the hex sha256 of the prompt-visible
// content: title, body and the visible comments (author, normalized created_at,
// body) in forge order. Labels, URL and Number are excluded because the
// refresh never changes them.
func issueSnapshotDigest(ic *run.IssueContext) string {
	type digestComment struct {
		Author    string `json:"author"`
		CreatedAt string `json:"created_at"`
		Body      string `json:"body"`
	}
	v := struct {
		Title    string          `json:"title"`
		Body     string          `json:"body"`
		Comments []digestComment `json:"comments"`
	}{Title: ic.Title, Body: ic.Body, Comments: []digestComment{}}
	for _, c := range ic.Comments {
		if !promptVisibleComment(c) {
			continue
		}
		v.Comments = append(v.Comments, digestComment{
			Author: c.Author, CreatedAt: normalizeCommentCreatedAt(c.CreatedAt), Body: c.Body,
		})
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// logRefreshSkipped records why the live read fell back to the cached
// snapshot. The no_credential rung is a normal configuration (a local run with
// no App installation), so it logs at DEBUG; every other rung is a fault and
// logs at WARN.
func (s *Server) logRefreshSkipped(ctx context.Context, runRow *run.Run, issueNumber int, family, reason string) {
	level := slog.LevelWarn
	if reason == issueContextReasonNoCredential {
		level = slog.LevelDebug
	}
	s.cfg.Logger.LogAttrs(ctx, level, "prompt: plan issue-context refresh skipped; using the run's cached snapshot",
		slog.String("run_id", runRow.ID.String()),
		slog.Int("issue", issueNumber),
		slog.String("forge", family),
		slog.String("reason", reason),
	)
}

// issueContextRefreshTimeout bounds the whole live read (issue + comments) so
// a slow or hung forge delays a plan-stage prompt fetch or a plan upload by at
// most this long before the cached snapshot is used — well under the forge
// clients' own 30s per-call timeout and the runner's 120s upload/prompt client
// timeout. A var so a test can shorten it.
var issueContextRefreshTimeout = 10 * time.Second

// fetchLiveIssueSnapshot reads the issue's live title, body and comments
// through fillIssueContext's credential and forge ladder, writing nothing to a
// trigger. ok=false means the cached snapshot must be used: no credential,
// forge unresolved (nil github or nil issue ops), repo unparseable, fetch error
// (including the issueContextRefreshTimeout deadline), or an empty issue.
// Comments are fetched only after the issue resolves; a comment-fetch error
// sets CommentsOK=false and WARNs, and does not fail the read.
func (s *Server) fetchLiveIssueSnapshot(ctx context.Context, github issueGetter, runRow *run.Run, issueNumber int) (liveIssueSnapshot, bool) {
	ctx, cancel := context.WithTimeout(ctx, issueContextRefreshTimeout)
	defer cancel()
	family := runForge(runRow)
	if family == forgeNameGitHub {
		if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
			s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonNoCredential)
			return liveIssueSnapshot{}, false
		}
		if github == nil {
			s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonForgeUnresolved)
			return liveIssueSnapshot{}, false
		}
		repo, err := parseRepoOwnerName(runRow.Repo)
		if err != nil {
			s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonRepoUnparseable)
			return liveIssueSnapshot{}, false
		}
		scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
		issue, err := github.GetIssue(ctx, scope, repo, issueNumber)
		if err != nil {
			s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonFetchFailed)
			return liveIssueSnapshot{}, false
		}
		if issue == nil || (issue.Title == "" && issue.Body == "") {
			s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonFetchedContextEmpty)
			return liveIssueSnapshot{}, false
		}
		live := liveIssueSnapshot{Title: issue.Title, Body: issue.Body}
		comments, err := github.ListIssueComments(ctx, scope, repo, issueNumber)
		if err != nil {
			s.warnRefreshCommentsFailed(ctx, runRow, issueNumber, family, err)
			return live, true
		}
		live.CommentsOK = true
		for _, c := range comments {
			live.Comments = append(live.Comments, run.IssueComment{Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt})
		}
		return live, true
	}

	if runRow.InstallationRef == nil || *runRow.InstallationRef == "" {
		s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonNoCredential)
		return liveIssueSnapshot{}, false
	}
	ops := s.issueOpsFor(family)
	if ops == nil {
		s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonForgeUnresolved)
		return liveIssueSnapshot{}, false
	}
	repo, ok := splitParentRepoRef(runRow.Repo)
	if !ok {
		s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonRepoUnparseable)
		return liveIssueSnapshot{}, false
	}
	scope := forge.FromRef(*runRow.InstallationRef)
	issue, err := ops.FetchIssue(ctx, scope, repo, issueNumber)
	if err != nil {
		s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonFetchFailed)
		return liveIssueSnapshot{}, false
	}
	if issue == nil || (issue.Title == "" && issue.Body == "") {
		s.logRefreshSkipped(ctx, runRow, issueNumber, family, issueContextReasonFetchedContextEmpty)
		return liveIssueSnapshot{}, false
	}
	live := liveIssueSnapshot{Title: issue.Title, Body: issue.Body}
	comments, err := ops.FetchIssueComments(ctx, scope, repo, issueNumber)
	if err != nil {
		s.warnRefreshCommentsFailed(ctx, runRow, issueNumber, family, err)
		return live, true
	}
	live.CommentsOK = true
	for _, c := range comments {
		live.Comments = append(live.Comments, run.IssueComment{Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt})
	}
	return live, true
}

func (s *Server) warnRefreshCommentsFailed(ctx context.Context, runRow *run.Run, issueNumber int, family string, err error) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: plan issue-context refresh: list issue comments failed; keeping the cached comments",
		slog.String("run_id", runRow.ID.String()),
		slog.Int("issue", issueNumber),
		slog.String("forge", family),
		slog.String("error", err.Error()),
	)
}

// issueContextRefresh is refreshPlanIssueContext's result. run is the row to
// render from: the persisted row itself, or a shallow copy carrying the
// refreshed IssueContext. digest is the prompt-visible digest of what run
// renders; it is empty only when the run has no cached IssueContext.
type issueContextRefresh struct {
	run       *run.Run
	planStage bool
	refreshed bool
	family    string
	drift     issueContextDrift
	digest    string
}

// refreshPlanIssueContext returns the row a stage's prompt should render. On a
// PLAN stage of a run with a cached IssueContext and a usable forge credential
// it re-reads the live issue and returns a shallow copy of runRow whose
// IssueContext takes the live title, body and (when fetched) comments, keeping
// URL, Number and Labels. runRow and its IssueContext are never mutated, so
// runs.issue_context is never rewritten. Every other case returns runRow.
func (s *Server) refreshPlanIssueContext(ctx context.Context, github issueGetter, runRow *run.Run, stageType run.StageType, issueNumber int) issueContextRefresh {
	if runRow == nil || runRow.IssueContext == nil {
		return issueContextRefresh{run: runRow}
	}
	res := issueContextRefresh{
		run:    runRow,
		family: runForge(runRow),
		digest: issueSnapshotDigest(runRow.IssueContext),
	}
	if stageType != run.StageTypePlan {
		return res
	}
	res.planStage = true
	live, ok := s.fetchLiveIssueSnapshot(ctx, github, runRow, issueNumber)
	if !ok {
		// The fallback renders the cache, so its drift against the cache is
		// zero but its comment counts are the cache's — a plan_review row
		// written for a review-side fallback reports them truthfully.
		res.drift = diffIssueSnapshots(runRow.IssueContext, runRow.IssueContext)
		return res
	}
	refreshed := *runRow.IssueContext
	refreshed.Title = live.Title
	refreshed.Body = live.Body
	if live.CommentsOK {
		refreshed.Comments = live.Comments
	}
	cp := *runRow
	cp.IssueContext = &refreshed
	res.run = &cp
	res.refreshed = true
	res.drift = diffIssueSnapshots(runRow.IssueContext, &refreshed)
	res.digest = issueSnapshotDigest(&refreshed)
	return res
}

// recordPlanPromptRefresh writes the signed /prompt fetch's refresh evidence:
// the issue_context_rendered row on every plan-stage fetch whose digest differs
// from the stage's newest row (the planner's rendered digest, C1 of #5219's
// approval), and an issue_context_refreshed
// plan_prompt row when the live content drifted from the cache. /prompt-render
// never calls it, so a preview writes no row.
func (s *Server) recordPlanPromptRefresh(ctx context.Context, runRow *run.Run, stageID uuid.UUID, issueNumber int, res issueContextRefresh) {
	if !res.planStage {
		return
	}
	s.appendIssueContextRendered(ctx, runRow, stageID, issueNumber, res)
	if !res.drift.any() {
		return
	}
	s.appendIssueContextRefreshed(ctx, runRow, stageID, issueNumber, res, refreshSurfacePlanPrompt, "")
}

// appendIssueContextRendered appends the per-fetch rendered-digest row. It is
// never deduped against OLDER digests — the newest row is the planner's last
// rendering, so A→B→A appends three rows — but a fetch whose digest equals the
// stage's NEWEST row appends nothing, since it would not change what the newest
// row says; that bounds the chain under repeated identical fetches. The check
// is a non-atomic list-then-append: a race appends a harmless identical row,
// and a list error appends without the check. Best-effort.
func (s *Server) appendIssueContextRendered(ctx context.Context, runRow *run.Run, stageID uuid.UUID, issueNumber int, res issueContextRefresh) {
	if s.cfg.AuditRepo == nil {
		return
	}
	newest, ok, err := s.newestRenderedDigest(ctx, runRow.ID, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: list issue_context_rendered entries failed; appending without the unchanged-digest check",
			slog.String("run_id", runRow.ID.String()), slog.String("stage_id", stageID.String()), slog.String("error", err.Error()))
	}
	if err == nil && ok && newest == res.digest {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "prompt: issue_context_rendered unchanged since the stage's newest row; not appended",
			slog.String("run_id", runRow.ID.String()), slog.String("stage_id", stageID.String()))
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id":          runRow.ID.String(),
		"stage_id":        stageID.String(),
		"issue":           issueNumber,
		"forge":           res.family,
		"refreshed":       res.refreshed,
		"snapshot_digest": res.digest,
		"hint":            "the prompt-visible issue snapshot this signed plan-stage prompt fetch rendered; the plan review compares its own snapshot against the newest row",
	})
	systemKind := audit.ActorKind("system")
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  issueContextRenderedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: append issue_context_rendered failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// appendIssueContextRefreshed builds and appends one issue_context_refreshed
// row for surface (plan_prompt or plan_review). plannerDigest is set for
// plan_review only. The payload carries ids, booleans, counts, closed slugs and
// digests, never issue text. Best-effort: an append error WARNs and the prompt
// is still served.
func (s *Server) appendIssueContextRefreshed(ctx context.Context, runRow *run.Run, stageID uuid.UUID, issueNumber int, res issueContextRefresh, surface, plannerDigest string) {
	if s.cfg.AuditRepo == nil {
		return
	}
	dedupeKey := surface + ":" + res.digest
	hint := "the live issue's prompt-visible title, body or comments differ from the run's cached snapshot; the plan prompt rendered the live content (the persisted snapshot is unchanged)"
	fields := map[string]any{
		"run_id":           runRow.ID.String(),
		"stage_id":         stageID.String(),
		"issue":            issueNumber,
		"forge":            res.family,
		"surface":          surface,
		"title_changed":    res.drift.TitleChanged,
		"body_changed":     res.drift.BodyChanged,
		"comments_cached":  res.drift.CommentsCached,
		"comments_live":    res.drift.CommentsLive,
		"comments_added":   res.drift.CommentsAdded,
		"comments_removed": res.drift.CommentsRemoved,
		"comments_edited":  res.drift.CommentsEdited,
		"snapshot_digest":  res.digest,
	}
	if surface == refreshSurfacePlanReview {
		dedupeKey += ":" + plannerDigest
		fields["planner_snapshot_digest"] = plannerDigest
		fields["refreshed"] = res.refreshed
		if res.refreshed {
			hint = "the plan review rendered a different issue snapshot than the planner's last signed prompt fetch; the issue changed between the two fetches, so re-read the issue before approving"
		} else {
			hint = "the plan review's own live issue fetch fell back to the run's cached snapshot, so the review judged the plan against the cached issue while the planner's last signed prompt fetch rendered a different snapshot; re-read the issue before approving"
		}
	}
	fields["dedupe_key"] = dedupeKey
	fields["hint"] = hint
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorKind("system")
	p := audit.ChainAppendParams{
		RunID:     runRow.ID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  issueContextRefreshedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}
	if err := s.appendIssueContextRefreshedOnce(ctx, runRow.ID, stageID, dedupeKey, p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: append issue_context_refreshed failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("surface", surface),
			slog.String("error", err.Error()),
		)
	}
}

// appendIssueContextRefreshedOnce follows appendRetirementDropOnce's posture
// (acceptance_retirement_drop.go): the atomic DedupedChainAppender path under
// the run-row lock when the repo implements it, otherwise a non-atomic
// list-then-append whose list error WARNs and proceeds without the guard. A
// duplicate (same stage, same dedupe_key) logs DEBUG and writes nothing.
func (s *Server) appendIssueContextRefreshedOnce(ctx context.Context, runID, stageID uuid.UUID, dedupeKey string, p audit.ChainAppendParams) error {
	spec := audit.DedupeSpec{StageID: &stageID, PayloadKey: "dedupe_key", PayloadValue: dedupeKey}
	var err error
	if d, ok := s.cfg.AuditRepo.(audit.DedupedChainAppender); ok {
		_, err = d.AppendChainedDeduped(ctx, p, spec)
	} else {
		entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, issueContextRefreshedCategory)
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"prompt: list issue_context_refreshed entries failed; proceeding without idempotency guard",
				slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()), slog.String("error", lerr.Error()))
		} else {
			err = audit.CheckDedupeSpec(entries, &p, spec)
		}
		if err == nil {
			_, err = s.cfg.AuditRepo.AppendChained(ctx, p)
		}
	}
	var dup *audit.DedupedDuplicateError
	if errors.As(err, &dup) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug, "prompt: issue_context_refreshed already recorded for this stage and snapshot",
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()), slog.String("dedupe_key", dedupeKey))
		return nil
	}
	return err
}

// plannerSnapshotDigest returns the snapshot_digest of the newest (highest
// Sequence) issue_context_rendered row for stageID: what the planner's last
// signed prompt fetch rendered. ok=false when there is no audit repo, the list
// fails, or no row exists (a failed append, or a stage fetched before #5219);
// the caller then skips the parity check rather than guess.
func (s *Server) plannerSnapshotDigest(ctx context.Context, runID, stageID uuid.UUID) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	digest, ok, err := s.newestRenderedDigest(ctx, runID, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "plan review: list issue_context_rendered entries failed; skipping the planner snapshot parity check",
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()), slog.String("error", err.Error()))
		return "", false
	}
	return digest, ok
}

// newestRenderedDigest reads the snapshot_digest of the newest (highest
// Sequence) issue_context_rendered row for stageID. ok=false when no row exists
// or the newest row's payload is undecodable or carries no digest; err is the
// list error.
func (s *Server) newestRenderedDigest(ctx context.Context, runID, stageID uuid.UUID) (string, bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, issueContextRenderedCategory)
	if err != nil {
		return "", false, err
	}
	var newest *audit.Entry
	for _, e := range entries {
		if e == nil || e.StageID == nil || *e.StageID != stageID {
			continue
		}
		if newest == nil || e.Sequence >= newest.Sequence {
			newest = e
		}
	}
	if newest == nil {
		return "", false, nil
	}
	var payload struct {
		SnapshotDigest string `json:"snapshot_digest"`
	}
	if err := json.Unmarshal(newest.Payload, &payload); err != nil || payload.SnapshotDigest == "" {
		return "", false, nil
	}
	return payload.SnapshotDigest, true, nil
}

// planReviewIssueContext returns the row the plan review renders its issue
// context from: the same plan-stage refresh the planner's signed fetch ran. It
// compares the review's snapshot digest with the planner's newest
// issue_context_rendered row and, when they differ, appends an
// issue_context_refreshed row with surface plan_review whose `refreshed` flag
// and hint distinguish a mid-window issue edit (the review's fetch succeeded)
// from a review-side fallback to the cache (it failed). The review always
// renders whatever the refresh returned.
func (s *Server) planReviewIssueContext(ctx context.Context, runRow *run.Run, stageID uuid.UUID, issueNumber int) *run.Run {
	res := s.refreshPlanIssueContext(ctx, s.issueGetter(), runRow, run.StageTypePlan, issueNumber)
	if res.digest == "" {
		return res.run
	}
	plannerDigest, ok := s.plannerSnapshotDigest(ctx, runRow.ID, stageID)
	if !ok {
		return res.run
	}
	if res.digest == plannerDigest {
		return res.run
	}
	s.appendIssueContextRefreshed(ctx, runRow, stageID, issueNumber, res, refreshSurfacePlanReview, plannerDigest)
	return res.run
}
