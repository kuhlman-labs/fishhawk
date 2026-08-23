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
// THE LATENCY BOUND, STATED HONESTLY (binding approval condition L1, option
// (a)). runIntakeGroom derives its own context with intakegroom.DefaultDeadline
// and hands that context to the reader. A context deadline does NOT preempt a
// callee that never consults the context, so this mechanism bounds the read for
// a CANCELLATION-COOPERATIVE reader — it is not, and is not claimed to be, a
// hard bound against an arbitrary blocking reader.
//
// The production path IS cancellation-cooperative, which is what makes the
// conditional bound the real one: the GitHub work-item reader reaches the forge
// through githubclient, which builds every request with
// http.NewRequestWithContext, and Go's HTTP client returns at a context
// deadline. TestIntakeHook_ProductionReadPathIsCancellationCooperative pins
// that property at the source level, and the wedged-reader tests use a
// ctx-respecting fake — which honestly tests this plumbing rather than
// pretending to test preemption. Option (b) (running the read on a goroutine
// and selecting on the deadline) was the alternative; it was not taken because
// it buys a hard bound only by leaking a goroutine of unbounded lifetime
// holding an abandoned result, whose later panic would be unrecoverable by this
// frame's recover() — a worse failure mode than the one it removes, on a path
// whose entire promise is that it cannot make filing worse.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// The panic guard is the outermost control: a bug anywhere below —
	// including in the pure derivation package — degrades the hook rather than
	// taking down the filing that was about to succeed. It is total for this
	// hook's own code because the hook starts no goroutine of its own.
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
	// degrade while the filing proceeds on the parent.
	hctx, cancel := context.WithTimeout(ctx, intakegroom.DefaultDeadline)
	defer cancel()

	candidates, truncated, reason := s.intakeCandidates(hctx, conv, target)
	if reason != "" {
		return intakegroom.Degrade(reason)
	}

	charter, charterReason := s.intakeCharter(hctx, conv, target)

	sig = intakegroom.Evaluate(filing, candidates, charter)
	sig.WindowTruncated = truncated
	if charterReason != "" {
		// Evaluate reports the generic charter_rubric_unparsed for any empty
		// rubric. Override it with the more specific cause observed upstream —
		// an undeclared charter and an unresolvable one are different operator
		// problems and must not be reported as the same one.
		sig.Degraded = true
		sig.DegradeReason = charterReason
	}
	return sig
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
		s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonCharterUnresolved, detail)
		return intakegroom.Charter{Path: path}, intakegroom.DegradeReasonCharterUnresolved
	}

	// The document read acts under the same credential scope the filing does,
	// falling back to the target's own scope when no resolver is wired.
	scope := target.Scope
	if s.cfg.DocumentScope != nil {
		resolved, serr := s.cfg.DocumentScope(ctx, repo)
		if serr != nil {
			s.logIntakeDegrade(ctx, target, intakegroom.DegradeReasonCharterUnresolved, serr.Error())
			return intakegroom.Charter{Path: path}, intakegroom.DegradeReasonCharterUnresolved
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
		reason := intakegroom.DegradeReasonCharterUnresolved
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = intakegroom.DegradeReasonBudgetExceeded
		}
		s.logIntakeDegrade(ctx, target, reason, detail)
		return intakegroom.Charter{Path: path}, reason
	}

	charter := intakegroom.Charter{
		Path:        doc.Path,
		ContentHash: doc.ContentHash,
		RubricIDs:   intakegroom.ParseRubricIDs(doc.Content),
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
