package server

// In-flight advisory findings (E80.6 / #3763).
//
// After the captain APPROVES an upkeep scan's gate, every advisory finding the
// captain approved in that window is matched against the dependency changes
// of the repository's other RUNNING runs. A run whose own diff introduces an
// affected version receives ONE crew-message-v1 `finding` (sender security,
// recipient reviewer, anchored on that run, system actor), which E77.7's
// deferred delivery hands to its next review render and its gate view. It is a
// crew_messages row, never a concern: the merge gate is untouched.
//
// TRUST BOUNDARY (approval condition 1): the pass is started ONLY from
// applyApprovedUpkeep, after the disposition window settles `approved`, and
// only for advisory findings carrying an approved disposition. Unratified
// scanner output never reaches another run; the upkeep_report ingest starts
// nothing.
//
// THE PASS, detached and bounded by upkeepInflightBudget (the clock starts at
// the CALL SITE, so a pass that never gets the slot degrades `pass_busy`):
//
//	slot      a one-slot semaphore acquired via select on the pass context
//	report    the recorded upkeep_report_recorded row (its sequence is the
//	          finding's audit_entry evidence) and the scan run row
//	base ref  cfg.DocumentBaseRef(repo)
//	runs      RunRepo.ListRuns{Repo, AccountID, State: running}, paged, at most
//	          upkeepInflightMaxRuns examined (run.State has no gate-parked
//	          value: a run parked at a plan/review/merge gate is `running`)
//	per run   head (latestRunHeadSHA) -> forge compare default...head (merge-
//	          base anchored) -> every changed go.mod / pnpm-lock.yaml in a
//	          directory an approved advisory of that ecosystem cites -> its
//	          patch section + its content at the head COMMIT ->
//	          upkeep.DependencyChanges -> upkeep.MatchInFlight -> chain dedupe
//	          -> crewmessage Send
//
// Every per-run failure is a counted skip that never stops the pass; the
// budget expiring stops it (`budget_exceeded`, sent work kept). Each pass
// appends ONE upkeep_inflight_pass_completed row on the scan run, on a FRESH
// short-timeout context so an expired pass context cannot lose it. Long-form
// contract: backend/internal/server/README.md § "In-flight advisory findings".

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// CategoryUpkeepInflightPassCompleted is the pass's ONE summary row on the
// scan run (registered in backend/internal/audit/categories.go). INTERNAL:
// not an issue-comment activity category.
const CategoryUpkeepInflightPassCompleted = "upkeep_inflight_pass_completed"

// upkeepInflightActorSubject is the system actor subject on every send.
const upkeepInflightActorSubject = "upkeep:advisory-watch"

// The closed set of pass degrade reasons (payload VALUES).
const (
	upkeepInflightBusy               = "pass_busy"
	upkeepInflightReportUnreadable   = "report_unreadable"
	upkeepInflightScanRunUnreadable  = "scan_run_unreadable"
	upkeepInflightBaseRefUnavailable = "base_ref_unavailable"
	upkeepInflightRunListFailed      = "run_list_failed"
	upkeepInflightBudgetExceeded     = "budget_exceeded"
	upkeepInflightPassPanic          = "pass_panic"
)

// The closed set of per-run skip reasons (skipped_runs keys).
const (
	upkeepInflightSkipDecompositionChild = "decomposition_child"
	upkeepInflightSkipOwnershipRefused   = "ownership_refused"
	upkeepInflightSkipHeadReadFailed     = "head_read_failed"
	upkeepInflightSkipNoHead             = "no_head"
	upkeepInflightSkipForgeUnavailable   = "forge_unavailable"
	upkeepInflightSkipCompareFailed      = "compare_failed"
	upkeepInflightSkipPatchUnavailable   = "patch_unavailable"
	upkeepInflightSkipFetchFailed        = "manifest_fetch_failed"
	upkeepInflightSkipTooLarge           = "manifest_too_large"
	upkeepInflightSkipUnparseable        = "manifest_unparseable"
	upkeepInflightSkipDedupeReadFailed   = "dedupe_read_failed"
	upkeepInflightSkipSendFailed         = "send_failed"
)

// Bounds. Vars ONLY so a test can shrink them.
var (
	upkeepInflightBudget           = 60 * time.Second
	upkeepInflightRowBudget        = 10 * time.Second
	upkeepInflightMaxRuns          = 50
	upkeepInflightMaxManifestBytes = 4 << 20
)

// upkeepInflightSlot serializes passes in one process: a one-slot semaphore
// acquired via select on the pass context, so a queued pass spends its OWN
// budget waiting and then degrades `pass_busy` instead of blocking. A var
// ONLY so a test can isolate it.
var upkeepInflightSlot = make(chan struct{}, 1)

// upkeepInflightSender is the mailbox seam: *crewmessage.Mailbox satisfies it.
type upkeepInflightSender interface {
	Send(ctx context.Context, p crewmessage.SendParams) (*crewmessage.Row, error)
}

// upkeepInflightMailbox resolves the sender; nil means the pass never starts.
// A var ONLY so a unit test can substitute a fake (no pgtest mailbox).
var upkeepInflightMailbox = upkeepInflightMailboxFromConfig

// upkeepInflightMailboxFromConfig is the production resolver. The explicit nil
// check runs BEFORE the interface assignment: a nil *Mailbox inside the
// interface would be a non-nil sender.
func upkeepInflightMailboxFromConfig(s *Server) upkeepInflightSender {
	if s.cfg.CrewMailbox == nil {
		return nil
	}
	return s.cfg.CrewMailbox
}

// upkeepInflightForge resolves a run's diff source and file reader (the
// shared per-family ladders). reason non-empty means unavailable. A var ONLY
// so a test can serve compares and manifests.
var upkeepInflightForge = func(s *Server, rn *run.Run) (patchComparer, forge.FileFetcher, forge.CredentialScope, forge.RepoRef, string) {
	c, scope, repo, reason := s.forgeCompareFor(rn)
	if reason != "" {
		return nil, nil, forge.CredentialScope{}, forge.RepoRef{}, reason
	}
	f, _, _, freason := s.fileFetcherFor(rn)
	if freason != "" {
		return nil, nil, forge.CredentialScope{}, forge.RepoRef{}, freason
	}
	return c, f, scope, repo, ""
}

// upkeepInflightJob is the pass's input, resolved on the apply's synchronous
// half.
type upkeepInflightJob struct {
	scanRunID, stageID uuid.UUID
	artifactID         string
	advisories         []upkeep.InFlightAdvisory
	sender             upkeepInflightSender
}

type upkeepInflightSent struct {
	RunID        string `json:"run_id"`
	FindingID    string `json:"finding_id"`
	AdvisoryID   string `json:"advisory_id"`
	SentSequence int64  `json:"sent_sequence"`
}

type upkeepInflightAlready struct {
	RunID      string `json:"run_id"`
	FindingID  string `json:"finding_id"`
	AdvisoryID string `json:"advisory_id"`
}

// upkeepInflightPassPayload is the ONE upkeep_inflight_pass_completed row.
// Arrays and the skip map are always non-nil.
type upkeepInflightPassPayload struct {
	ArtifactID          string                  `json:"artifact_id"`
	AdvisoryFindings    int                     `json:"advisory_findings"`
	BaseRef             string                  `json:"base_ref,omitempty"`
	RunsListed          int                     `json:"runs_listed"`
	RunsExamined        int                     `json:"runs_examined"`
	RunsWindowTruncated bool                    `json:"runs_window_truncated"`
	Sent                []upkeepInflightSent    `json:"sent"`
	AlreadySent         []upkeepInflightAlready `json:"already_sent"`
	SkippedRuns         map[string]int          `json:"skipped_runs"`
	Degraded            bool                    `json:"degraded"`
	DegradeReason       string                  `json:"degrade_reason,omitempty"`
}

// upkeepInflightAdvisories reduces the report's advisory findings the captain
// APPROVED to the matcher's input, in report order.
func upkeepInflightAdvisories(r *plan.UpkeepReport, consumed map[string]upkeepConsumedDisposition) []upkeep.InFlightAdvisory {
	out := []upkeep.InFlightAdvisory{}
	if r == nil {
		return out
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		if f.Source != plan.UpkeepSourceAdvisory || f.Advisory == nil {
			continue
		}
		if d, ok := consumed[f.ID]; !ok || d.Verdict != upkeepVerdictApproved {
			continue
		}
		a := f.Advisory
		fixed := ""
		if a.FixedVersion != nil {
			fixed = *a.FixedVersion
		}
		frames := make([]upkeep.DisclosureFrame, 0, len(a.CallPath))
		for _, fr := range a.CallPath {
			df := upkeep.DisclosureFrame{Package: fr.Package, Function: fr.Function, Receiver: fr.Receiver}
			if fr.Position != nil {
				df.Filename = fr.Position.Filename
			}
			frames = append(frames, df)
		}
		out = append(out, upkeep.InFlightAdvisory{
			FindingID: f.ID, IDs: a.AdvisoryIDs, Ecosystem: a.Ecosystem, Package: a.Package,
			Version: a.Version, FixedVersion: fixed, Directories: plan.UpkeepAdvisoryManifestDirs(f),
			Reachability: a.Reachability, Severity: a.Severity, CallPath: frames,
		})
	}
	return out
}

// startUpkeepInflightPass starts the detached pass for the captain-approved
// advisory findings of one settled window. It is a NO-OP (no goroutine, no
// row) when a repository the pass reads or the mailbox is unwired, or when no
// approved advisory finding exists. It never blocks its caller.
func (s *Server) startUpkeepInflightPass(ctx context.Context, scanRunID, stageID uuid.UUID, artifactID string,
	report *plan.UpkeepReport, consumed map[string]upkeepConsumedDisposition) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	sender := upkeepInflightMailbox(s)
	if sender == nil {
		return
	}
	advs := upkeepInflightAdvisories(report, consumed)
	if len(advs) == 0 {
		return
	}
	job := &upkeepInflightJob{scanRunID: scanRunID, stageID: stageID, artifactID: artifactID, advisories: advs, sender: sender}
	// The budget clock starts HERE, so time spent queued for the slot counts.
	passCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upkeepInflightBudget)
	s.bgUpkeepInflight.Add(1)
	go func() {
		defer s.bgUpkeepInflight.Done()
		defer cancel()
		s.runUpkeepInflightPass(passCtx, job)
	}()
}

// runUpkeepInflightPass runs one pass and ALWAYS appends its summary row,
// including after a panic.
func (s *Server) runUpkeepInflightPass(passCtx context.Context, job *upkeepInflightJob) {
	sum := &upkeepInflightPassPayload{
		ArtifactID: job.artifactID, AdvisoryFindings: len(job.advisories),
		Sent: []upkeepInflightSent{}, AlreadySent: []upkeepInflightAlready{}, SkippedRuns: map[string]int{},
	}
	defer func() {
		if r := recover(); r != nil {
			s.upkeepInflightDegrade(passCtx, job, sum, upkeepInflightPassPanic, fmt.Sprint(r))
		}
		s.appendUpkeepInflightSummary(passCtx, job, sum)
	}()

	select {
	case upkeepInflightSlot <- struct{}{}:
		defer func() { <-upkeepInflightSlot }()
	case <-passCtx.Done():
		s.upkeepInflightDegrade(passCtx, job, sum, upkeepInflightBusy, "another in-flight pass held the slot for this pass's whole budget")
		return
	}
	s.upkeepInflightBody(passCtx, job, sum)
}

// upkeepInflightDegrade marks the summary degraded and WARN-logs it.
func (s *Server) upkeepInflightDegrade(ctx context.Context, job *upkeepInflightJob, sum *upkeepInflightPassPayload, reason, detail string) {
	sum.Degraded = true
	sum.DegradeReason = reason
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep in-flight pass degraded",
		slog.String("run_id", job.scanRunID.String()),
		slog.String("reason", reason),
		slog.String("detail", detail))
}

// appendUpkeepInflightSummary appends the summary row on a FRESH bounded
// context detached from the (possibly expired) pass context.
func (s *Server) appendUpkeepInflightSummary(passCtx context.Context, job *upkeepInflightJob, sum *upkeepInflightPassPayload) {
	body, err := json.Marshal(sum)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(passCtx), upkeepInflightRowBudget)
	defer cancel()
	systemKind := audit.ActorSystem
	stageID := job.stageID
	if _, aerr := s.cfg.AuditRepo.AppendChained(wctx, audit.ChainAppendParams{
		RunID: job.scanRunID, StageID: &stageID, Timestamp: time.Now().UTC(),
		Category: CategoryUpkeepInflightPassCompleted, ActorKind: &systemKind, Payload: body,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(wctx, slog.LevelWarn, "upkeep in-flight pass: summary row append failed",
			slog.String("run_id", job.scanRunID.String()),
			slog.String("error", aerr.Error()))
	}
}

// upkeepInflightBody is the pass under the slot.
func (s *Server) upkeepInflightBody(ctx context.Context, job *upkeepInflightJob, sum *upkeepInflightPassPayload) {
	recorded, rerr := s.upkeepRecordedRow(ctx, job.scanRunID, job.artifactID)
	if rerr != nil {
		s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightReportUnreadable, rerr.Error())
		return
	}
	scan, gerr := s.cfg.RunRepo.GetRun(ctx, job.scanRunID)
	if gerr != nil || scan == nil {
		s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightScanRunUnreadable, fmt.Sprint(gerr))
		return
	}
	baseRef, berr := s.upkeepInflightBaseRef(ctx, scan)
	if berr != nil {
		s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightBaseRefUnavailable, berr.Error())
		return
	}
	sum.BaseRef = baseRef

	const page = 100
	for offset := 0; ; offset += page {
		runs, lerr := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			Repo: scan.Repo, AccountID: scan.AccountID, State: string(run.StateRunning), Limit: page, Offset: offset,
		})
		if lerr != nil {
			if ctx.Err() != nil {
				s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightBudgetExceeded, ctx.Err().Error())
				return
			}
			s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightRunListFailed, lerr.Error())
			return
		}
		for _, rn := range runs {
			if rn == nil || rn.ID == job.scanRunID {
				continue
			}
			if sum.RunsListed >= upkeepInflightMaxRuns {
				sum.RunsWindowTruncated = true
				return
			}
			sum.RunsListed++
			if reason := s.upkeepInflightExamine(ctx, job, scan, rn, baseRef, recorded.Sequence, sum); reason != "" {
				sum.SkippedRuns[reason]++
				s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep in-flight pass: run skipped",
					slog.String("run_id", job.scanRunID.String()),
					slog.String("target_run_id", rn.ID.String()),
					slog.String("reason", reason))
			}
			if ctx.Err() != nil {
				s.upkeepInflightDegrade(ctx, job, sum, upkeepInflightBudgetExceeded, ctx.Err().Error())
				return
			}
		}
		if len(runs) < page {
			return
		}
	}
}

// upkeepInflightBaseRef resolves the default-branch ref the run diffs are
// compared from, refusing rather than defaulting.
func (s *Server) upkeepInflightBaseRef(ctx context.Context, scan *run.Run) (string, error) {
	if s.cfg.DocumentBaseRef == nil {
		return "", fmt.Errorf("no document base-ref resolver is configured")
	}
	repo, err := parseRepoRef(scan.Repo)
	if err != nil {
		return "", err
	}
	ref, err := s.cfg.DocumentBaseRef(ctx, repo)
	if err != nil {
		return "", err
	}
	if ref == "" {
		return "", fmt.Errorf("the base-ref resolver returned an empty ref for %s", scan.Repo)
	}
	return ref, nil
}

// upkeepInflightCited reports whether an advisory of ecosystem cites dir.
func upkeepInflightCited(advs []upkeep.InFlightAdvisory, ecosystem, dir string) bool {
	for _, a := range advs {
		if a.Ecosystem != ecosystem {
			continue
		}
		for _, d := range a.Directories {
			if d == dir {
				return true
			}
		}
	}
	return false
}

// upkeepInflightExamine examines one listed run, recording its sends and
// already-sent matches on sum. It returns the run's skip reason, or "" when
// it was examined.
func (s *Server) upkeepInflightExamine(ctx context.Context, job *upkeepInflightJob, scan, rn *run.Run,
	baseRef string, recordedSeq int64, sum *upkeepInflightPassPayload) string {
	if rn.DecomposedFrom != nil {
		return upkeepInflightSkipDecompositionChild
	}
	if upkeepRunOwnershipRefusal(rn, scan) != "" {
		return upkeepInflightSkipOwnershipRefused
	}
	head, ok, herr := s.latestRunHeadSHA(ctx, rn.ID)
	if herr != nil {
		return upkeepInflightSkipHeadReadFailed
	}
	if !ok || head == "" {
		return upkeepInflightSkipNoHead
	}
	cmp, ff, scope, repo, reason := upkeepInflightForge(s, rn)
	if reason != "" {
		return upkeepInflightSkipForgeUnavailable
	}
	res, cerr := cmp.ComparePatch(ctx, scope, repo, baseRef, head)
	if cerr != nil || res == nil {
		return upkeepInflightSkipCompareFailed
	}
	sum.RunsExamined++

	sections := upkeep.SplitComparePatch(res.Patch)
	var changes []upkeep.DependencyChange
	for _, file := range res.Files {
		if file.Status == "removed" {
			continue
		}
		eco, dir, known := upkeep.ManifestEcosystem(file.Path)
		if !known || !upkeepInflightCited(job.advisories, eco, dir) {
			continue
		}
		section, has := sections[file.Path]
		if !has {
			return upkeepInflightSkipPatchUnavailable
		}
		fc, ferr := ff.FetchFile(ctx, scope, repo, file.Path, head)
		if ferr != nil || fc == nil {
			return upkeepInflightSkipFetchFailed
		}
		if len(fc.Content) > upkeepInflightMaxManifestBytes {
			return upkeepInflightSkipTooLarge
		}
		dc, derr := upkeep.DependencyChanges(file.Path, section, string(fc.Content))
		if derr != nil {
			return upkeepInflightSkipUnparseable
		}
		changes = append(changes, dc...)
	}

	matches := upkeep.MatchInFlight(job.advisories, changes)
	if len(matches) == 0 {
		return ""
	}
	existing, eerr := s.upkeepInflightSentSummaries(ctx, rn.ID)
	if eerr != nil {
		return upkeepInflightSkipDedupeReadFailed
	}
	skip := ""
	for _, m := range matches {
		summary, detail, severity := upkeep.RenderInFlightFinding(m)
		primary := ""
		if len(m.Advisory.IDs) > 0 {
			primary = m.Advisory.IDs[0]
		}
		if existing[summary] {
			sum.AlreadySent = append(sum.AlreadySent, upkeepInflightAlready{
				RunID: rn.ID.String(), FindingID: m.Advisory.FindingID, AdvisoryID: primary,
			})
			continue
		}
		seq, serr := upkeepInflightSend(ctx, job, rn.ID, recordedSeq, summary, detail, severity)
		if serr != nil {
			skip = upkeepInflightSkipSendFailed
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep in-flight pass: send failed",
				slog.String("run_id", job.scanRunID.String()),
				slog.String("target_run_id", rn.ID.String()),
				slog.String("error", serr.Error()))
			continue
		}
		existing[summary] = true
		sum.Sent = append(sum.Sent, upkeepInflightSent{
			RunID: rn.ID.String(), FindingID: m.Advisory.FindingID, AdvisoryID: primary, SentSequence: seq,
		})
	}
	return skip
}

// upkeepInflightSentSummaries reads the TARGET run's CHAIN (never the derived
// table, so a truncated index cannot reset the dedupe) for the summaries of
// SECURITY-sent findings. Only fishhawkd sends as security
// (crewSenderRoleForStage allow-lists planner and reviewer for run tokens), so
// an agent cannot plant a summary that suppresses a finding. A row that does
// not decode is a read failure: fail toward NOT sending.
func (s *Server) upkeepInflightSentSummaries(ctx context.Context, target uuid.UUID) (map[string]bool, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, target, crewmessage.CategorySent)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range rows {
		if e == nil {
			continue
		}
		var p struct {
			Message json.RawMessage `json:"message"`
		}
		if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
			return nil, fmt.Errorf("decode crew_message_sent row %d: %w", e.Sequence, jerr)
		}
		var m crewmessage.Message
		if jerr := json.Unmarshal(p.Message, &m); jerr != nil {
			return nil, fmt.Errorf("decode crew message on row %d: %w", e.Sequence, jerr)
		}
		if m.SenderRole == crewmessage.RoleSecurity && m.Type == crewmessage.TypeFinding {
			out[m.Payload.Summary] = true
		}
	}
	return out, nil
}

// upkeepInflightSend sends one finding to target. A *crewmessage.ProjectionError
// counts as SENT: the chain entry committed and must never be re-sent.
func upkeepInflightSend(ctx context.Context, job *upkeepInflightJob, target uuid.UUID, recordedSeq int64,
	summary, detail, severity string) (int64, error) {
	msg := crewmessage.Message{
		SchemaVersion: crewmessage.SchemaVersion,
		Type:          crewmessage.TypeFinding,
		SenderRole:    crewmessage.RoleSecurity,
		RecipientRole: crewmessage.RoleReviewer,
		Anchor:        crewmessage.Anchor{RunID: target.String()},
		Payload:       crewmessage.Payload{Summary: summary, Detail: detail, Severity: severity},
		Evidence: []crewmessage.EvidenceReference{
			{Kind: crewmessage.EvidenceRun, Ref: job.scanRunID.String()},
			{Kind: crewmessage.EvidenceAuditEntry, Ref: fmt.Sprint(recordedSeq)},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	row, serr := job.sender.Send(ctx, crewmessage.SendParams{
		RawMessage: raw,
		Actor:      crewmessage.Actor{Kind: audit.ActorSystem, Subject: upkeepInflightActorSubject},
	})
	if pe, ok := serr.(*crewmessage.ProjectionError); ok {
		return pe.Sequence, nil
	}
	if serr != nil {
		return 0, serr
	}
	if row == nil {
		return 0, nil
	}
	return row.SentSequence, nil
}
