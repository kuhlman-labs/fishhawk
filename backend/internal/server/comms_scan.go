package server

// The comms scan GATHER (E81.5 / #3775, phase 4 #4014): what a plan stage
// declaring `produces: comms_report` is shown when its prompt is served. It
// reads the charter (rubric + non-goal ids), runs userreport.Scan through a
// DEFERRING cursor store and a capturing Recorder (serving never moves
// user_report_cursors), keeps only external reports, suppresses the ones a
// prior comms apply or a trusted draft marker already accounted for, caps
// and orders what is rendered so the cursor can always make forward progress,
// suggests clusters, and builds both the prompt's CommsScanContext and the
// comms_scan_gathered payload facts (comms_record.go records them). Contract:
// README.md § "Comms scan gather".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Gather seams. Package vars so a NON-PARALLEL test can shrink them, drive
// the cache clock or substitute the forge reader.
var (
	// commsScanBudget bounds one gather (charter read, scan and the audit
	// reads), detached from the serving request.
	commsScanBudget = 20 * time.Second
	// commsScanCacheTTL is how long a completed gather is served from the
	// cache, so /prompt and /prompt-render agree within it. A refusal or an
	// aborted gather is never cached.
	commsScanCacheTTL = 60 * time.Second
	// commsScanCacheMaxEntries hard-caps the cache map. When it is full after
	// the expired-entry sweep, a new gather runs uncached.
	commsScanCacheMaxEntries = 256
	// commsScanNow is the cache's clock.
	commsScanNow = time.Now
	// commsScanJoinHook, when non-nil, is called each time a caller JOINS an
	// in-flight gather instead of starting its own. A test-only barrier.
	commsScanJoinHook func()
	// commsScanMaxReports caps the external, unsuppressed reports one gather
	// hands the prompt: the OLDEST are kept, the rest are omitted and the
	// pending cursor is held back to them.
	commsScanMaxReports = 100
	// commsScanMaxRecordedIDs caps the omitted and suppressed id lists the
	// comms_scan_gathered row carries (the counts stay exact).
	commsScanMaxRecordedIDs = 500
	// commsUserReportReaderFor resolves the provider's user-report reader.
	commsUserReportReaderFor = workmgmt.UserReportReaderFor
)

// commsCharterDeclarationSite names the comms gather in a charter log line.
const commsCharterDeclarationSite = "comms scan charter (#4014)"

// The CLOSED charter-refusal reason set: details.reason of the 422
// comms_charter_refused both prompt endpoints write. A comms scan has no
// unanchored mode, so every way the charter can fail to anchor it refuses.
const (
	commsRefusalConventionsUnavailable  = "conventions_unavailable"
	commsRefusalRepoMalformed           = "repo_malformed"
	commsRefusalCharterUndeclared       = string(intakegroom.DegradeReasonCharterUndeclared)
	commsRefusalSeamUnwired             = string(intakegroom.DegradeReasonSeamUnwired)
	commsRefusalCharterUnresolved       = string(intakegroom.DegradeReasonCharterUnresolved)
	commsRefusalBudgetExceeded          = string(intakegroom.DegradeReasonBudgetExceeded)
	commsRefusalCharterRubricUnparsed   = string(intakegroom.DegradeReasonCharterRubricUnparsed)
	commsRefusalCharterRubricUnconforms = "charter_rubric_unconforming"
)

// commsCharterRefusalReasons returns the closed refusal reason set in a
// stable order (the OpenAPI document lists every one).
func commsCharterRefusalReasons() []string {
	return []string{
		commsRefusalConventionsUnavailable,
		commsRefusalRepoMalformed,
		commsRefusalCharterUndeclared,
		commsRefusalSeamUnwired,
		commsRefusalCharterUnresolved,
		commsRefusalBudgetExceeded,
		commsRefusalCharterRubricUnparsed,
		commsRefusalCharterRubricUnconforms,
	}
}

// commsCharterRefusal is the gather's fail-closed refusal: the charter could
// not anchor the scan. CharterPath is set when the declaration was read;
// Detail is for the log only and never reaches a response.
type commsCharterRefusal struct {
	Reason      string
	CharterPath string
	Detail      string
}

func (e *commsCharterRefusal) Error() string {
	return fmt.Sprintf("comms scan charter refused: %s: %s", e.Reason, e.Detail)
}

// The scan-level degradations (source user_reports). Each leaves the gather
// with nothing shown and no pending cursor.
const (
	commsDegradeSourceUserReports  = "user_reports"
	commsDegradeSourcePage         = "user_reports_page"
	commsDegradeSourceClassify     = "user_reports_classification"
	commsDegradeCursorStoreUnwired = "cursor_store_unwired"
	commsDegradeReaderUnavailable  = "reader_unavailable"
	commsDegradeScopeUnavailable   = "scope_unavailable"
	commsDegradeAccountUnparseable = "account_unparseable"
	commsDegradeScanFailed         = "scan_failed"
	commsDegradeBudgetExceeded     = "budget_exceeded"
)

// The id shapes the prompt's charter tables accept (prompt/comms.go): a
// rubric id is an uppercase letter then digits and NOT a non-goal id; a
// non-goal id is N then digits. The server applies the same filter so it can
// refuse an unconforming rubric before Build and record exactly the ids the
// prompt renders (TestCommsScan_RecordedCharterIDsMatchRender pins parity).
var (
	commsRubricIDConforms  = regexp.MustCompile(`^[A-Z][0-9]+$`)
	commsNonGoalIDConforms = regexp.MustCompile(`^N[0-9]+$`)
)

// commsGather is one completed gather: the prompt input and the
// comms_scan_gathered payload facts. Payload carries no stage identity;
// payloadFor stamps it at serve time. Shared through the cache — read only.
type commsGather struct {
	Context *prompt.CommsScanContext
	Payload commsScanGatheredPayload
}

// payloadFor returns the payload stamped with stage's id and attempt token,
// ready for recordCommsScanGathered.
func (g *commsGather) payloadFor(stage *run.Stage) commsScanGatheredPayload {
	p := g.Payload
	p.StageID = stage.ID
	p.StageAttempt = run.StageAttemptToken(stage.DispatchedAt)
	return p
}

// resolveCommsScanContext returns the comms scan input for a plan stage that
// declares `produces: comms_report`, and (nil, nil, nil) for every other
// stage — a non-plan stage with no read at all, a plan stage after one GetRun
// and spec parse. The error is the binding's transport error verbatim, or a
// *commsCharterRefusal (the caller writes 422 comms_charter_refused).
//
// The gather is served through commsScans, so /prompt and /prompt-render share
// one gather per stage within commsScanCacheTTL and concurrent serves gather
// once. An Undecidable binding gets nil — the ordinary plan prompt — the same
// documented residual as the upkeep fork.
func (s *Server) resolveCommsScanContext(ctx context.Context, runRow *run.Run, stage *run.Stage) (*prompt.CommsScanContext, *commsGather, error) {
	if stage.Type != run.StageTypePlan {
		return nil, nil, nil
	}
	b, err := s.resolveStageArtifactBinding(ctx, runRow.ID, stage, spec.ArtifactCommsReport)
	if err != nil {
		return nil, nil, err
	}
	if !b.StageDeclares {
		return nil, nil, nil
	}
	key := commsScanKey{srv: s, runID: runRow.ID, stageID: stage.ID}
	g, err := commsScans.do(ctx, key, func(gctx context.Context) (*commsGather, error) {
		return s.gatherCommsScan(gctx, runRow)
	})
	if err != nil {
		return nil, nil, err
	}
	return g.Context, g, nil
}

// commsGatherState accumulates one gather's degradations, logging each.
type commsGatherState struct {
	s     *Server
	runID uuid.UUID
	deg   []commsGatherDegradation
}

func (st *commsGatherState) degrade(ctx context.Context, d commsGatherDegradation, detail string) {
	st.deg = append(st.deg, d)
	st.s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "comms scan gather: partial scan",
		slog.String("run_id", st.runID.String()),
		slog.String("source", d.Source),
		slog.String("reason", d.Reason),
		slog.Int("count", d.Count),
		slog.String("detail", detail))
}

// refuse WARN-logs and returns the charter refusal.
func (s *Server) commsRefuse(ctx context.Context, runID uuid.UUID, reason, path, detail string) error {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "comms scan gather: charter refused",
		slog.String("run_id", runID.String()),
		slog.String("reason", reason),
		slog.String("charter_path", path),
		slog.String("declaration_site", commsCharterDeclarationSite),
		slog.String("detail", detail))
	return &commsCharterRefusal{Reason: reason, CharterPath: path, Detail: detail}
}

// commsCharterReason maps the shared charter read's degrade reason onto the
// refusal set. A reason outside the expected set is charter_unresolved.
func commsCharterReason(r intakegroom.DegradeReason) string {
	switch reason := string(r); reason {
	case commsRefusalCharterUndeclared, commsRefusalSeamUnwired, commsRefusalCharterUnresolved,
		commsRefusalBudgetExceeded, commsRefusalCharterRubricUnparsed:
		return reason
	}
	return commsRefusalCharterUnresolved
}

// commsCharterLines returns every parsed charter line (the prompt discloses
// the ones it withholds) and the ids the prompt RENDERS: those passing
// accept, capped at max.
func commsCharterLines(ids []string, quote func(string) string, accept func(string) bool, max int) ([]prompt.CommsCharterLine, []string) {
	lines := make([]prompt.CommsCharterLine, 0, len(ids))
	var rendered []string
	for _, id := range ids {
		lines = append(lines, prompt.CommsCharterLine{ID: id, Text: quote(id)})
		if accept(id) && len(rendered) < max {
			rendered = append(rendered, id)
		}
	}
	return lines, rendered
}

func commsRubricConforms(id string) bool {
	return commsRubricIDConforms.MatchString(id) && !commsNonGoalIDConforms.MatchString(id)
}

// gatherCommsScan runs one gather under ctx (already bounded by the cache's
// detached budget). Order:
//
//  1. Conventions, the repo coordinates and the run-scoped target.
//  2. The charter (resolveCharterDocument); any failure, or no rubric id the
//     prompt would render, REFUSES.
//  3. Scan preconditions (cursor store, reader, scope, account); any miss
//     degrades and Scan is not called.
//  4. userreport.Scan through commsDeferringCursors and a capturing Recorder.
//  5. Class partition: only external reports are kept.
//  6. Suppression: prior comms_apply_completed memory, then trusted markers.
//  7. The gather cap keeps the OLDEST commsScanMaxReports.
//  8. Newest-first render order; the render cap drops the NEWEST.
//  9. The pending cursor held back to the earliest omitted report.
//  10. Advisory clusters.
func (s *Server) gatherCommsScan(ctx context.Context, runRow *run.Run) (*commsGather, error) {
	st := &commsGatherState{s: s, runID: runRow.ID}
	repo := runRow.Repo

	conv, err := conventionsLoader(ctx, repo)
	if err != nil {
		return nil, s.commsRefuse(ctx, runRow.ID, commsRefusalConventionsUnavailable, "", err.Error())
	}
	owner, name, ok := splitRepoFullName(repo)
	if !ok {
		return nil, s.commsRefuse(ctx, runRow.ID, commsRefusalRepoMalformed, "", fmt.Sprintf("run repo %q is not owner/name", repo))
	}
	target, scopeErr := s.runScopedWorkTarget(ctx, runRow, owner, name, conv)

	charter, reason, detail := s.resolveCharterDocument(ctx, conv, target)
	if reason != "" {
		return nil, s.commsRefuse(ctx, runRow.ID, commsCharterReason(reason), charter.Path, detail)
	}
	rubric, rubricIDs := commsCharterLines(charter.RubricIDs.IDs(), charter.RubricIDs.Quote, commsRubricConforms, prompt.CommsMaxRubricLines)
	if len(rubricIDs) == 0 {
		return nil, s.commsRefuse(ctx, runRow.ID, commsRefusalCharterRubricUnconforms, charter.Path,
			"no rubric id of the rubric shape (an uppercase letter then digits, never N<digits>) in "+charter.Path)
	}
	nonGoals, nonGoalIDs := commsCharterLines(charter.NonGoals.IDs(), charter.NonGoals.Quote, commsNonGoalIDConforms.MatchString, prompt.CommsMaxNonGoalLines)

	payload := commsScanGatheredPayload{
		Repo: repo,
		Charter: commsCharterRecord{
			Path: charter.Path, ContentHash: charter.ContentHash,
			RubricIDs: rubricIDs, NonGoalIDs: nonGoalIDs,
		},
	}
	cc := &prompt.CommsScanContext{Repo: repo, Rubric: rubric, NonGoals: nonGoals}

	report, accountID := s.commsScanReports(ctx, st, runRow, conv, target, scopeErr)
	if report != nil {
		s.commsBuildReports(ctx, st, report, accountID, cc, &payload)
	}

	payload.Degradations = st.deg
	for _, d := range st.deg {
		cc.Degradations = append(cc.Degradations, prompt.CommsScanDegrade{Source: d.Source, Reason: d.Reason, Count: d.Count})
	}
	return &commsGather{Context: cc, Payload: payload}, nil
}

// commsScanReports checks the scan preconditions and runs userreport.Scan,
// returning the captured report (nil on any degrade) and the run account.
func (s *Server) commsScanReports(ctx context.Context, st *commsGatherState, runRow *run.Run, conv workmgmt.Conventions, target workmgmt.Target, scopeErr error) (*userreport.Report, *uuid.UUID) {
	scanDeg := func(reason, detail string) {
		st.degrade(ctx, commsGatherDegradation{Source: commsDegradeSourceUserReports, Reason: reason, Count: 1}, detail)
	}
	blocked := false
	if s.cfg.UserReportCursors == nil {
		scanDeg(commsDegradeCursorStoreUnwired, "no user-report cursor store is configured on this deployment")
		blocked = true
	}
	reader, err := commsUserReportReaderFor(conv.Provider)
	if err != nil {
		scanDeg(commsDegradeReaderUnavailable, err.Error())
		blocked = true
	}
	if scopeErr != nil {
		scanDeg(commsDegradeScopeUnavailable, scopeErr.Error())
		blocked = true
	}
	var accountID *uuid.UUID
	if runRow.AccountID != "" {
		id, perr := uuid.Parse(runRow.AccountID)
		if perr != nil {
			scanDeg(commsDegradeAccountUnparseable, perr.Error())
			blocked = true
		} else {
			accountID = &id
		}
	}
	if blocked {
		return nil, accountID
	}

	var captured *userreport.Report
	params := userreport.ScanParams{
		AccountID: accountID,
		Repo:      runRow.Repo,
		Source:    userreport.SourceIssues,
		Target:    target,
		Reader:    reader,
		Cursors:   commsDeferringCursors{inner: s.cfg.UserReportCursors},
		Record: userreport.RecorderFunc(func(_ context.Context, r userreport.Report) error {
			captured = &r
			return nil
		}),
		Logger: s.cfg.Logger,
	}
	if s.cfg.CaptainStore != nil {
		// Set only when non-nil: a nil *captain.Store in the interface would
		// be a typed nil Scan dereferences instead of naming
		// captain_unavailable.
		params.Captain = s.cfg.CaptainStore
	}
	if _, err := userreport.Scan(ctx, params); err != nil {
		reason := commsDegradeScanFailed
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = commsDegradeBudgetExceeded
		}
		scanDeg(reason, err.Error())
		return nil, accountID
	}
	return captured, accountID
}

// commsKept is one external, unsuppressed report with its derived identity.
type commsKept struct {
	item userreport.ReportItem
	id   string
	hash string
}

// commsBuildReports applies the class partition, suppression, both caps,
// the pending cursor and the clusters to a captured report.
func (s *Server) commsBuildReports(ctx context.Context, st *commsGatherState, report *userreport.Report, accountID *uuid.UUID, cc *prompt.CommsScanContext, payload *commsScanGatheredPayload) {
	for _, d := range report.Degradations {
		src := commsDegradeSourcePage
		if d.Source == userreport.DegradationSourceReport {
			src = commsDegradeSourceClassify
		}
		n := d.Count
		if n < 1 {
			n = 1
		}
		st.degrade(ctx, commsGatherDegradation{Source: src, Reason: d.Code, Count: n}, d.Detail)
	}

	draftFiled, err := s.loadCommsDraftFiled(ctx, accountID, payload.Repo)
	if err != nil {
		st.degradeMemory(ctx, err, commsDegradeDraftFiledUnavailable)
	}
	memory, err := s.loadCommsSuppressionMemory(ctx, accountID, payload.Repo)
	if err != nil {
		st.degradeMemory(ctx, err, commsDegradeSuppressionMemoryUnavailable)
	}
	// Markers are parsed over EVERY scanned item: the evidence lives on the
	// fishhawk_filed items the class partition below removes.
	trusted, malformed := trustedCommsMarkers(report.Items, draftFiled)
	payload.MalformedMarkers = malformed

	var suppressed []commsSuppression
	byID := map[string]int{}
	var kept []commsKept
	for _, it := range report.Items {
		switch it.Classification {
		case userreport.ClassExternal:
		case userreport.ClassFishhawkFiled:
			payload.ClassExcluded.FishhawkFiled++
			continue
		case userreport.ClassBot:
			payload.ClassExcluded.Bot++
			continue
		default:
			payload.ClassExcluded.Internal++
			continue
		}
		k := commsKept{
			item: it,
			id:   prompt.UserReportID(string(it.Kind), it.IssueNumber, it.CommentID),
			hash: userreport.ContentHash(it.Kind, it.Title, it.Body),
		}
		// A prior apply's basis wins over a marker for the same (id, hash).
		if sup, ok := memory.lookup(k.id, k.hash); ok {
			suppressed = append(suppressed, sup)
			continue
		}
		if sup, ok := trusted.lookup(k.id, k.hash); ok {
			suppressed = append(suppressed, sup)
			continue
		}
		// One report per id: a repeat keeps the later update.
		if i, dup := byID[k.id]; dup {
			if k.item.UpdatedAt.After(kept[i].item.UpdatedAt) {
				kept[i] = k
			}
			continue
		}
		byID[k.id] = len(kept)
		kept = append(kept, k)
	}
	payload.SuppressedCount = len(suppressed)
	payload.Suppressed = commsCapSuppressions(suppressed)
	cc.SuppressedCount = len(suppressed)

	// Scan's order is ascending updated_at; sort explicitly so the cap's
	// "oldest kept" property does not rest on the provider.
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].item.UpdatedAt.Equal(kept[j].item.UpdatedAt) {
			return kept[i].item.UpdatedAt.Before(kept[j].item.UpdatedAt)
		}
		return kept[i].id < kept[j].id
	})
	var omitted []commsKept
	if len(kept) > commsScanMaxReports {
		omitted = append(omitted, kept[commsScanMaxReports:]...)
		kept = kept[:commsScanMaxReports]
	}
	cc.OmittedCount = len(omitted)

	// Newest-first: the render cap drops the EARLIEST-listed reports, so the
	// newest are omitted and the oldest always render — the cursor can always
	// advance past at least the oldest report.
	newestFirst := make([]commsKept, len(kept))
	for i := range kept {
		newestFirst[len(kept)-1-i] = kept[i]
	}
	reports := make([]prompt.UserReport, len(newestFirst))
	for i, k := range newestFirst {
		reports[i] = commsPromptReport(k.item)
	}
	// RenderUserReports is deterministic, so buildCommsScan omits exactly
	// these ids when it renders the same slice.
	_, renderOmitted := prompt.RenderUserReports(reports)
	isRenderOmitted := make(map[string]bool, len(renderOmitted))
	for _, id := range renderOmitted {
		isRenderOmitted[id] = true
	}
	var shown []commsKept
	for _, k := range newestFirst {
		if isRenderOmitted[k.id] {
			omitted = append(omitted, k)
			continue
		}
		shown = append(shown, k)
		payload.Shown = append(payload.Shown, commsShownReport{
			ID: k.id, Kind: string(k.item.Kind), IssueNumber: k.item.IssueNumber, CommentID: k.item.CommentID,
			ContentHash: k.hash, UpdatedAt: k.item.UpdatedAt,
		})
	}
	cc.UserReports = reports
	payload.OmittedCount = len(omitted)
	for _, k := range omitted {
		if len(payload.Omitted) >= commsScanMaxRecordedIDs {
			break
		}
		payload.Omitted = append(payload.Omitted, k.id)
	}
	payload.PendingCursor = commsPendingCursorFor(report, omitted)
	cc.SuggestedClusters = commsSuggestClusters(shown)
}

// degradeMemory records a suppression or draft-filed read degrade: the
// error's own named degradation, else fallback.
func (st *commsGatherState) degradeMemory(ctx context.Context, err error, fallback string) {
	var mu *commsMemoryUnavailableError
	if errors.As(err, &mu) {
		st.degrade(ctx, mu.Degradation(), err.Error())
		return
	}
	st.degrade(ctx, commsGatherDegradation{Source: commsDegradeSourceAudit, Reason: fallback, Count: 1}, err.Error())
}

// commsCapSuppressions caps the recorded suppressions at
// commsScanMaxRecordedIDs.
func commsCapSuppressions(sups []commsSuppression) []commsSuppression {
	if len(sups) > commsScanMaxRecordedIDs {
		return sups[:commsScanMaxRecordedIDs]
	}
	return sups
}

// commsPendingCursorFor returns the bounds phase 7 may advance to: the scan's
// next cursors held back to the earliest omitted report's updated_at, with
// note <= cursor, never behind the read bounds. The note bound is derived
// from the held cursor BEFORE it is clamped to Since (as commsCursorHoldBack
// does): a GitLab note listed under a NoteSince earlier than Since may be
// older than Since, and clamping first would lift the note floor past it.
func commsPendingCursorFor(report *userreport.Report, omitted []commsKept) *commsPendingCursor {
	pc := &commsPendingCursor{
		Since: report.Since, NoteSince: report.NoteSince,
		Cursor: report.NextCursor, NoteCursor: report.NextNoteCursor,
	}
	for _, k := range omitted {
		if k.item.UpdatedAt.Before(pc.Cursor) {
			pc.Cursor = k.item.UpdatedAt
		}
	}
	if pc.Cursor.Before(pc.NoteCursor) {
		pc.NoteCursor = pc.Cursor
	}
	if pc.Cursor.Before(pc.Since) {
		pc.Cursor = pc.Since
	}
	if pc.NoteCursor.Before(pc.NoteSince) {
		pc.NoteCursor = pc.NoteSince
	}
	if pc.NoteCursor.After(pc.Cursor) {
		pc.NoteCursor = pc.Cursor
	}
	return pc
}

// commsPromptReport maps one classified item onto the prompt's plain-data
// report.
func commsPromptReport(it userreport.ReportItem) prompt.UserReport {
	return prompt.UserReport{
		Kind:                string(it.Kind),
		IssueNumber:         it.IssueNumber,
		CommentID:           it.CommentID,
		ReportTitle:         it.Title,
		ReportBody:          it.Body,
		AuthorLogin:         it.Author.Login,
		Association:         it.Author.Association,
		AssociationResolved: it.Author.AssociationResolved,
		Classification:      string(it.Classification),
		ClassificationBasis: it.Basis,
		MarkerFromExternal:  it.MarkerFromExternal,
		System:              it.System,
		CreatedAt:           it.CreatedAt,
		UpdatedAt:           it.UpdatedAt,
		Reactions: prompt.UserReportReactions{
			Total: it.Reactions.Total, PlusOne: it.Reactions.PlusOne, MinusOne: it.Reactions.MinusOne,
			Resolved: it.Reactions.Resolved,
		},
	}
}

// commsClusterTitle is a report's clustering title: the issue title, or a
// comment's first non-empty body line.
func commsClusterTitle(it userreport.ReportItem) string {
	if it.Kind == workmgmt.UserReportKindIssue {
		return it.Title
	}
	for _, line := range strings.Split(it.Body, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			return l
		}
	}
	return ""
}

// commsSuggestClusters joins reports whose titles intakegroom.Duplicates
// scores at medium confidence or higher (union-find over pairwise edges).
// Each cluster carries its ids sorted and its max edge score; clusters are
// ordered by score, then first id. Lexical and ADVISORY.
func commsSuggestClusters(reports []commsKept) []prompt.CommsCluster {
	n := len(reports)
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	type edge struct {
		a     int
		score float64
	}
	var edges []edge
	for i := 0; i < n; i++ {
		ti := commsClusterTitle(reports[i].item)
		if ti == "" {
			continue
		}
		for j := i + 1; j < n; j++ {
			tj := commsClusterTitle(reports[j].item)
			if tj == "" {
				continue
			}
			dups := intakegroom.Duplicates(intakegroom.Filing{Title: ti}, []intakegroom.Candidate{{Number: j, Title: tj}})
			if len(dups) == 0 || !dups[0].Confidence.AtLeastMedium() {
				continue
			}
			if ri, rj := find(i), find(j); ri != rj {
				parent[rj] = ri
			}
			edges = append(edges, edge{a: i, score: dups[0].Score})
		}
	}
	best := map[int]float64{}
	for _, e := range edges {
		r := find(e.a)
		if e.score > best[r] {
			best[r] = e.score
		}
	}
	members := map[int][]string{}
	for i := 0; i < n; i++ {
		r := find(i)
		members[r] = append(members[r], reports[i].id)
	}
	var out []prompt.CommsCluster
	for r, ids := range members {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		out = append(out, prompt.CommsCluster{ReportIDs: ids, Score: best[r]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ReportIDs[0] < out[j].ReportIDs[0]
	})
	return out
}

// commsDeferringCursors wraps the cursor store so a SERVE never advances it:
// Get and Init pass through (Init is an idempotent insert-if-absent, never an
// advance), Advance records nothing and reports the target as committed but
// not moved. The comms apply (phase 7) advances the REAL store to the
// recorded pending cursor once every shown report is accounted for.
type commsDeferringCursors struct {
	inner userreport.CursorStore
}

func (c commsDeferringCursors) Get(ctx context.Context, key userreport.Key) (time.Time, bool, error) {
	return c.inner.Get(ctx, key)
}

func (c commsDeferringCursors) Init(ctx context.Context, key userreport.Key, initial time.Time) (time.Time, error) {
	return c.inner.Init(ctx, key, initial)
}

func (commsDeferringCursors) Advance(_ context.Context, _ userreport.Key, to time.Time) (time.Time, bool, error) {
	return to, false, nil
}

// commsScanKey identifies one cached gather. The server is part of the key
// so two Servers in one process (tests) never share a gather.
type commsScanKey struct {
	srv     *Server
	runID   uuid.UUID
	stageID uuid.UUID
}

// commsScanEntry is one gather: in flight until done is closed. ok=false
// after done means the gather aborted (it panicked); err non-nil means it
// refused. Only ok && err == nil is served after completion, until expires.
type commsScanEntry struct {
	done    chan struct{}
	result  *commsGather
	err     error
	ok      bool
	expires time.Time
}

// commsScanCache is the single-flight + short-TTL gather cache, mirroring
// upkeepScanCache, plus the error leg: a refusal is handed to the callers
// already waiting on that gather and never cached.
type commsScanCache struct {
	mu      sync.Mutex
	entries map[commsScanKey]*commsScanEntry
}

// commsScans is the process's comms gather cache, shared by /prompt and
// /prompt-render.
var commsScans = &commsScanCache{}

// do returns the cached gather for key, or runs gather. The LEADER runs it
// under a context DETACHED from its request and bounded by commsScanBudget,
// so a cancelled leader never hands its waiters a truncated result. A gather
// that panics is removed and its waiters retry; one that refuses hands the
// refusal to its waiters and is removed.
func (c *commsScanCache) do(ctx context.Context, key commsScanKey, gather func(context.Context) (*commsGather, error)) (*commsGather, error) {
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = map[commsScanKey]*commsScanEntry{}
		}
		now := commsScanNow()
		if e, hit := c.entries[key]; hit {
			select {
			case <-e.done:
				if e.ok && e.err == nil && now.Before(e.expires) {
					c.mu.Unlock()
					return e.result, nil
				}
				delete(c.entries, key)
			default:
				c.mu.Unlock()
				if hook := commsScanJoinHook; hook != nil {
					hook()
				}
				<-e.done
				if e.ok {
					return e.result, e.err
				}
				continue // the leader aborted: retry, possibly as the leader
			}
		}
		c.sweepLocked(now)
		e := &commsScanEntry{done: make(chan struct{})}
		if len(c.entries) < commsScanCacheMaxEntries {
			c.entries[key] = e
		}
		c.mu.Unlock()
		return c.lead(ctx, key, e, gather)
	}
}

// lead runs gather for e under a detached, budgeted context and publishes the
// outcome. The deferred publish runs on a panic too, so waiters are never
// stranded.
func (c *commsScanCache) lead(ctx context.Context, key commsScanKey, e *commsScanEntry, gather func(context.Context) (*commsGather, error)) (*commsGather, error) {
	defer func() {
		c.mu.Lock()
		if e.ok && e.err == nil {
			e.expires = commsScanNow().Add(commsScanCacheTTL)
		} else if c.entries[key] == e {
			delete(c.entries, key)
		}
		c.mu.Unlock()
		close(e.done)
	}()
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commsScanBudget)
	defer cancel()
	out, err := gather(gctx)
	c.mu.Lock()
	e.result, e.err, e.ok = out, err, true
	c.mu.Unlock()
	return out, err
}

// sweepLocked drops every completed entry that has expired, refused or
// aborted. c.mu must be held.
func (c *commsScanCache) sweepLocked(now time.Time) {
	for k, e := range c.entries {
		select {
		case <-e.done:
			if !e.ok || e.err != nil || !now.Before(e.expires) {
				delete(c.entries, k)
			}
		default:
		}
	}
}

// size reports the number of map entries (tests).
func (c *commsScanCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
