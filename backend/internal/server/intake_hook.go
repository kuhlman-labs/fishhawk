package server

// This file carries the intake-groom HOOK (#2239 / E54.7): the bounded,
// panic-guarded, always-degrading step that derives advisory intake signals
// for a work item at the moment it is filed.
//
// WHY THIS DEGRADES WHILE THE REST OF E54 FAILS CLOSED — the decision, not an
// oversight (binding approval condition L4).
//
// Everywhere else in E54 a missing or unresolvable charter is a REFUSAL. A
// backlog-grooming run's whole output is a charter-anchored ranking, so an
// unanchored ranking is worse than no ranking and charter_gate.go refuses the
// stage. Intake grooming is the deliberate exception, and the asymmetry is the
// point of this slice rather than a hole in it.
//
// The reason is what this hook rides on. Work-item FILING is a load-bearing
// write path: operator follow-up filing (#1005), product-issue reporting
// (#1006), deferred review concerns (#2000-era defer_concern), refinement
// filing and split filing all funnel through applyAndFileWorkItem. Its job is
// to record work RELIABLY. Making a filing's success depend on the health of
// an advisory enhancement would convert that enhancement into a NEW FAILURE
// MODE on the one path whose reliability everything else assumes — an agent
// that cannot file a follow-up issue because a charter fetch 502'd has lost
// something real in exchange for something advisory.
//
// So every failure inside this hook is swallowed into a typed
// intakegroom.DegradeReason, the item is filed regardless, the reason is
// WARN-logged and reported on the 201, and nothing about the filing changes.
// A degraded hook is NORMAL, not an incident.
//
// TWO CONCURRENT READS UNDER ONE BUDGET (#2827). runIntakeGroom needs two
// INDEPENDENT forge reads — the duplicate-candidate scan and the charter read —
// and it runs them CONCURRENTLY on one derived context, joining both before it
// reads either result.
//
// It used to spend that one budget SEQUENTIALLY, scan first. On a real backlog
// the scan alone costs most of the 3s, so the charter read inherited whatever
// was left and the outcome flipped on ordinary latency variance. Worse, when it
// lost, the reported gap said "no rubric lines could be parsed from the
// charter" — blaming a demonstrably healthy parser for a document that had
// never been fetched. Concurrency makes the hook's wall clock max(scan,
// charter) instead of scan+charter, so neither read can starve the other, and
// the budget becomes a PER-READ time cap rather than a shared pool. That is why
// DefaultDeadline is NOT raised: with the reads concurrent the constant already
// bounds each read on its own.
//
// It does NOT rescue a scan that alone exceeds the whole budget. That filing
// still degrades; what changes is that the reason names the read that actually
// failed.
//
// PER-GOROUTINE RECOVER. A deferred recover() in the frame that STARTED a
// goroutine cannot catch that goroutine's panic — the runtime terminates the
// program (Go spec, "Handling panics": https://go.dev/ref/spec#Handling_panics).
// So each read goroutine carries its OWN deferred recover mapping a panic onto
// DegradeReasonHookPanic. The outer recover below remains for the hook's own
// frame. (An earlier version of this comment claimed the outer guard was total
// "because the hook starts no goroutine of its own" — that is no longer true,
// and the per-goroutine recovers are what keeps the claim it stood for.)
//
// JOIN, NOT ABANDON. The hook WAITS for both goroutines before returning, so
// neither outlives this frame, no abandoned result is held, and no goroutine's
// later panic escapes an already-returned recover. That is precisely the
// failure mode option (b) of #2239 (start the read on a goroutine and SELECT on
// the deadline, abandoning the loser) was rejected for, and joining is what
// keeps that objection from applying to this shape.
//
// THE LATENCY BOUND, STATED HONESTLY — AND WHAT CONCURRENCY CHANGED. Because
// the hook joins, a reader that never consults its context still blocks the
// filing, exactly as it did under the sequential shape. A context deadline does
// NOT preempt a callee that ignores the context, so this mechanism bounds a
// CANCELLATION-COOPERATIVE reader and is not claimed to be a hard bound against
// an arbitrary blocking one.
//
// That bound is PRESERVED for the candidate read and EXTENDED to the charter
// read, and the extension is a NEW exposure path rather than a preserved one:
// under the sequential shape a scan that exhausted the budget meant the charter
// read was never dialed at all, so a non-cooperative charter reader could not
// block a filing it was never asked to serve. Now it is always dialed, so it
// can. That is an accepted consequence of running the reads concurrently — the
// same conditional exposure the candidate read has always carried, now applying
// to both.
//
// Both production paths ARE cancellation-cooperative, which is what makes the
// conditional bound the real one: the GitHub work-item reader and the charter
// document read both reach the forge through githubclient, which builds every
// request with http.NewRequestWithContext, and Go's HTTP client returns at a
// context deadline. TestIntakeHook_ProductionReadPathCancelsInFlightAtDeadline
// pins that BEHAVIOURALLY for BOTH seams — it drives the real reader and the
// real repodoc charter resolver through the real client against a hanging HTTP
// server and asserts the server's own in-flight request is cancelled at the
// caller's deadline, so a future edit that stopped threading the context
// anywhere in either chain reddens it. (An earlier version grepped githubclient
// for the http.NewRequestWithContext constructor, which stayed green as long as
// any file in the package mentioned it and so could not see that regression at
// all.) The wedged-reader tests use a ctx-respecting fake — which honestly
// tests this plumbing rather than pretending to test preemption.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// intakeCharterDeclarationSite is the provenance string the charter read is
// declared under. repodoc echoes it into every error, so a failure names the
// knob that produced it.
const intakeCharterDeclarationSite = "charter.path in .fishhawk/work-management.yaml (intake groom, #2239)"

// runIntakeGroom derives the advisory intake signals for one filing.
//
// It NEVER returns an error and never panics out: every failure mode becomes a
// degraded Signals carrying a typed reason from intakegroom's closed set. The
// caller files the work item either way — see the file comment for why that
// asymmetry is deliberate.
//
// It is called AFTER workmgmt.Apply (so the filing it evaluates is the rendered
// title/labels/body a reader will actually see) and BEFORE the provider File
// (so the rendered advisory section lands on the created issue itself).
func (s *Server) runIntakeGroom(ctx context.Context, conv workmgmt.Conventions, target workmgmt.Target, filing intakegroom.Filing) (sig intakegroom.Signals) {
	start := time.Now()
	// The panic guard for the hook's OWN frame: a bug in the sequential half
	// below — including in the pure derivation package — degrades the hook
	// rather than taking down the filing that was about to succeed. It does
	// NOT cover the two read goroutines; each carries its own (see the file
	// comment's per-goroutine-recover note).
	defer func() {
		if rec := recover(); rec != nil {
			s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonHookPanic,
				fmt.Sprintf("panic recovered in intake groom: %v", rec))
			sig = intakegroom.Degrade(intakegroom.DegradeReasonHookPanic)
		}
		sig.DurationMS = time.Since(start).Milliseconds()
	}()

	// The hook's own budget. This is a CHILD context: cancelling it does not
	// affect the caller's request context, which is what lets a hook timeout
	// degrade while the filing proceeds on the parent. Both reads share it, and
	// because they run CONCURRENTLY it bounds each of them rather than being a
	// pool the first read can drain. A context is safe for simultaneous use by
	// multiple goroutines.
	hctx, cancel := context.WithTimeout(ctx, s.intakeGroomDeadline())
	defer cancel()

	var (
		candidates      []intakegroom.Candidate
		truncated       bool
		candidateReason intakegroom.DegradeReason

		charter       intakegroom.Charter
		charterReason intakegroom.DegradeReason
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer s.recoverIntakeRead(ctx, target, func(reason intakegroom.DegradeReason) {
			candidates, truncated, candidateReason = nil, false, reason
		})
		candidates, truncated, candidateReason = s.intakeCandidates(hctx, conv, target)
	}()
	go func() {
		defer wg.Done()
		defer s.recoverIntakeRead(ctx, target, func(reason intakegroom.DegradeReason) {
			charter, charterReason = intakegroom.Charter{}, reason
		})
		charter, charterReason = s.intakeCharter(hctx, conv, target)
	}()
	// JOIN, not abandon: every write above happens-before this returns, so the
	// reads below need no further synchronisation, and no goroutine outlives
	// this frame.
	wg.Wait()

	if candidateReason != "" {
		// A failed scan degrades the whole hook and reports NO findings, so the
		// filed body stays byte-identical. A charter resolved alongside a
		// failed scan is therefore read but not used: scoring on a partial
		// window is deliberately out of scope.
		return intakegroom.Degrade(candidateReason)
	}

	sig = intakegroom.Evaluate(filing, candidates, charter)
	sig.WindowTruncated = truncated
	if charterReason != "" {
		// Evaluate can only see whether the Charter it was handed was resolved.
		// Override it with the more specific cause observed upstream — an
		// undeclared charter, an unwired seam and an exhausted budget are
		// different operator problems and must not be reported as the same one.
		sig.Degraded = true
		sig.DegradeReason = charterReason
	}
	return sig
}

// intakeGroomDeadline resolves the hook's budget at the READ site: the Config
// value when set, intakegroom.DefaultDeadline otherwise.
//
// The zero value meaning "use the default" is what keeps every existing Server
// construction site — none of which sets the field — on today's behaviour, and
// resolving it here rather than at construction keeps exactly one place knowing
// the default. Config.IntakeGroomDeadline is a test seam; production sets it
// nowhere.
func (s *Server) intakeGroomDeadline() time.Duration {
	if s.cfg.IntakeGroomDeadline > 0 {
		return s.cfg.IntakeGroomDeadline
	}
	return intakegroom.DefaultDeadline
}

// recoverIntakeRead is the deferred panic guard ONE read goroutine installs.
//
// It exists per-goroutine because a recover() in the frame that started a
// goroutine cannot catch that goroutine's panic — the runtime terminates the
// program instead (Go spec, "Handling panics"). Omitting it would turn what is
// today a recovered panic on a load-bearing write path into a process crash.
//
// It logs through the same funnel every other degradation uses and hands the
// typed reason back to the caller's own result variables via assign, so the
// join below sees a degraded read rather than a half-written one.
func (s *Server) recoverIntakeRead(ctx context.Context, target workmgmt.Target, assign func(intakegroom.DegradeReason)) {
	rec := recover()
	if rec == nil {
		return
	}
	s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonHookPanic,
		fmt.Sprintf("panic recovered in intake groom: %v", rec))
	assign(intakegroom.DegradeReasonHookPanic)
}

// intakeCandidates enumerates the bounded, newest-first window of existing
// items the duplicate and epic derivations compare against.
//
// Every failure returns a typed reason and NO candidates. It never returns a
// partial window silently: an error from the reader is a degradation, because a
// truncated-by-failure window would produce "no duplicate found" for a filing
// that has one.
func (s *Server) intakeCandidates(ctx context.Context, conv workmgmt.Conventions, target workmgmt.Target) ([]intakegroom.Candidate, bool, intakegroom.DegradeReason) {
	reader, err := workmgmt.ReaderFor(conv.Provider)
	if err != nil {
		// A File-only provider (gitlab, jira in v0) or an unregistered id.
		// Reported, never fatal.
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonReaderUnavailable, err.Error())
		return nil, false, intakegroom.DegradeReasonReaderUnavailable
	}

	page, err := reader.ListWorkItems(ctx, workmgmt.ListWorkItemsRequest{
		Target:        target,
		IncludeClosed: true,
		// A closed duplicate is often the resolution, so the window includes
		// closed items.
		Newest:     true,
		MaxScanned: intakegroom.DefaultMaxScanned,
		// ResolveBoardState stays OFF: intake signals need title/labels only,
		// and asking for board state would drag in the user-owned-board
		// projects-token precondition (#1114) and fail an otherwise-healthy
		// read closed.
		ResolveBoardState: false,
	})
	if err != nil {
		reason := intakegroom.DegradeReasonReaderError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = intakegroom.DegradeReasonBudgetExceeded
		}
		s.logIntakeDegrade(ctx, target, reason, err.Error())
		return nil, false, reason
	}
	if page == nil {
		// The reader contract says a degradation is a typed error with a nil
		// page, never a nil page with a nil error. Treat the contract violation
		// as a reader error rather than dereferencing it.
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonReaderError,
			"reader returned a nil page and a nil error")
		return nil, false, intakegroom.DegradeReasonReaderError
	}

	candidates := make([]intakegroom.Candidate, 0, len(page.Items))
	for _, rec := range page.Items {
		candidates = append(candidates, intakegroom.Candidate{
			Number: rec.Number,
			Title:  rec.Title,
			Body:   rec.Body,
			Labels: rec.Labels,
			URL:    rec.URL,
			Closed: rec.Complete || strings.EqualFold(rec.State, "closed"),
		})
	}
	return candidates, page.Truncated, ""
}

// intakeCharter resolves the repo's declared charter and parses its rubric ids.
//
// It returns the Charter it managed to build alongside the reason, so a caller
// that degrades on an unparsable rubric still knows which document it read.
func (s *Server) intakeCharter(ctx context.Context, conv workmgmt.Conventions, target workmgmt.Target) (intakegroom.Charter, intakegroom.DegradeReason) {
	if conv.Charter == nil || strings.TrimSpace(conv.Charter.Path) == "" {
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonCharterUndeclared,
			"the repo's work-management conventions declare no charter path")
		return intakegroom.Charter{}, intakegroom.DegradeReasonCharterUndeclared
	}
	path := strings.TrimSpace(conv.Charter.Path)

	if s.cfg.DocumentResolver == nil || s.cfg.DocumentBaseRef == nil {
		// A deployment that never wired the forge-backed document seam. This is
		// also the documented no-revert kill switch: unwire the seam and every
		// filing takes this branch and files exactly as it did before #2239.
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonSeamUnwired,
			"no document resolver / base-ref resolver is configured on this deployment")
		return intakegroom.Charter{Path: path}, intakegroom.DegradeReasonSeamUnwired
	}

	repo := forge.RepoRef{Owner: target.Repo.Owner, Name: target.Repo.Name}
	baseRef, err := s.cfg.DocumentBaseRef(ctx, repo)
	if err != nil || strings.TrimSpace(baseRef) == "" {
		detail := "the document base-ref resolver returned an empty ref"
		if err != nil {
			detail = err.Error()
		}
		reason := intakeCharterFailureReason(ctx, err)
		s.logIntakeDegrade(ctx, target, reason, detail)
		return intakegroom.Charter{Path: path}, reason
	}

	// The document read acts under the same credential scope the filing does,
	// falling back to the target's own scope when no resolver is wired.
	scope := target.Scope
	if s.cfg.DocumentScope != nil {
		resolved, serr := s.cfg.DocumentScope(ctx, repo)
		if serr != nil {
			reason := intakeCharterFailureReason(ctx, serr)
			s.logIntakeDegrade(ctx, target, reason, serr.Error())
			return intakegroom.Charter{Path: path}, reason
		}
		scope = resolved
	}

	doc, err := s.cfg.DocumentResolver.Resolve(ctx, repodoc.Request{
		Repo:    repo,
		Scope:   scope,
		BaseRef: baseRef,
		Declaration: repodoc.Declaration{
			Path:            path,
			DeclarationSite: intakeCharterDeclarationSite,
		},
	})
	if err != nil || doc == nil {
		detail := "the document resolver returned no document"
		if err != nil {
			detail = err.Error()
		}
		reason := intakeCharterFailureReason(ctx, err)
		s.logIntakeDegrade(ctx, target, reason, detail)
		return intakegroom.Charter{Path: path}, reason
	}

	// Resolved is set HERE and nowhere else: this is the one path on which a
	// charter document was actually fetched and its content parsed. Every early
	// return above leaves it false, which is what lets the pure package tell a
	// never-read charter from a read-but-rubric-less one.
	charter := intakegroom.Charter{
		Path:        doc.Path,
		ContentHash: doc.ContentHash,
		RubricIDs:   intakegroom.ParseRubricIDs(doc.Content),
		Resolved:    true,
	}
	if charter.RubricIDs.Len() == 0 {
		// The charter resolved but carries no parsable rubric ids. Scoring
		// records the gap as a finding; it never invents an id to cite.
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonCharterRubricUnparsed,
			"no rubric line ids could be parsed from "+doc.Path)
		return charter, intakegroom.DegradeReasonCharterRubricUnparsed
	}
	return charter, ""
}

// intakeCharterFailureReason classifies ONE charter-seam failure into the
// degrade reason that names its actual cause.
//
// Deadline expiry is BUDGET EXHAUSTION, not an unresolvable charter, and it
// must classify the same way at EVERY seam the budget can expire in. The
// charter read spends the hook's budget across three seams in sequence
// (base-ref resolution, credential-scope resolution, the document read), so any
// of them can be the one that observes the deadline, not just the document
// read. Classifying two of them as charter_unresolved sends an operator hunting
// a charter-path misconfiguration for what is a slow forge. (This mattered even
// more before #2827, when the candidate scan ran FIRST on the same budget and
// routinely left the charter read almost none of it; the classification is
// still required now that the reads are concurrent, because a slow forge
// expires the budget at whichever seam the charter read happens to be in.)
//
// It reads BOTH the returned error and ctx.Err(): a seam may wrap the
// cancellation into an opaque error of its own, in which case the context is
// the only honest witness that the budget is what ran out.
func intakeCharterFailureReason(ctx context.Context, err error) intakegroom.DegradeReason {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return intakegroom.DegradeReasonBudgetExceeded
	}
	return intakegroom.DegradeReasonCharterUnresolved
}

// logIntakeDegrade WARN-logs one degradation. It is a single funnel so every
// swallowed failure is VISIBLE with the same shape — the #1107
// enrichment-incomplete precedent: best-effort does not mean silent.
func (s *Server) logIntakeDegrade(ctx context.Context, target workmgmt.Target, reason intakegroom.DegradeReason, detail string) {
	if s.cfg.Logger == nil {
		return
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "intake groom degraded; work item filed without signals",
		slog.String("repo", target.Repo.Owner+"/"+target.Repo.Name),
		slog.String("degrade_reason", string(reason)),
		slog.String("detail", detail),
	)
}

// intakeFilingFor adapts the applied work item into intakegroom's own input
// vocabulary. The adaptation lives HERE, at the call site, which is what keeps
// the derivation package free of any workmgmt import.
func intakeFilingFor(filing workmgmt.FilingRequest, item workmgmt.WorkItem) intakegroom.Filing {
	f := intakegroom.Filing{
		Title:                  item.Title,
		Summary:                filing.Summary,
		Body:                   item.Body,
		Labels:                 item.Classification.Labels,
		Type:                   item.Type,
		MissingLabelNamespaces: item.Classification.MissingLabelNamespaces,
	}
	f.ParentEpicRef = strings.TrimSpace(filing.Relations.ParentEpic)
	f.DependsOn = filing.Relations.DependsOn
	return f
}

// intakeAuditSummary renders the compact intake summary folded into the
// existing work_item_filed audit payload.
//
// It is a SUMMARY, not the whole Signals value: the audit chain records what a
// later reader needs to answer "what did intake see when this was filed?" —
// how many duplicate candidates, the top one, the score and the rubric ids it
// cited, and whether the hook degraded and why — without carrying every basis
// string into a hash-chained payload that grows unboundedly with the window.
func intakeAuditSummary(s intakegroom.Signals) map[string]any {
	out := map[string]any{
		"duplicate_count": len(s.Duplicates),
		"score":           s.Score.Value,
		"unscored":        s.Score.Unscored,
		"degraded":        s.Degraded,
		"scanned_items":   s.ScannedItems,
		"duration_ms":     s.DurationMS,
	}
	if s.Degraded {
		out["degrade_reason"] = string(s.DegradeReason)
	}
	if len(s.Duplicates) > 0 {
		out["top_duplicate_number"] = s.Duplicates[0].Number
		out["top_duplicate_confidence"] = string(s.Duplicates[0].Confidence)
	}
	if s.EpicSuggestion != nil {
		out["epic_suggestion_number"] = s.EpicSuggestion.Number
	}
	if len(s.Score.Citations) > 0 {
		ids := make([]string, 0, len(s.Score.Citations))
		for _, c := range s.Score.Citations {
			ids = append(ids, c.RubricID)
		}
		out["cited_rubric_ids"] = ids
	}
	return out
}
