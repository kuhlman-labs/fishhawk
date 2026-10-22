package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-096 D1/D2/D5 under ADR-098 D3 (E87.2 / #4293): at a CLEAN base advance
// Fishhawk performed, decide whether the prior head's PASSED merge-candidate
// verdict may transfer to the new merge commit instead of running a pass.
//
//   - decideMergeCandidateTransfer runs the precondition ladder, resolves the
//     transfer chain's ROOT head, reads the inputs (basedelta.GitDelta plus
//     two basedelta.MaterializeTreeLinks workspaces, B' and H', with symlink
//     aliases expanded) and calls the classifier on a bounded SUB-budget;
//   - mergeCandidateTransferEnforce writes the decision in enforce mode:
//     merge_candidate_verdict_transferred (the caller skips the pass) or
//     merge_candidate_verdict_transfer_refused (the caller runs the pass);
//   - scheduleMergeCandidateTransferShadow runs the same decision ASYNC in
//     shadow mode and appends the INTERNAL shadow row; it never touches the
//     pass path;
//   - detectMergeCandidateFalseTransfers appends the ADVISORY
//     merge_candidate_verdict_false_transfer row when a shadow would_transfer
//     head's pass went red, in either arrival order.
//
// The classifier's Reason is UNTRUSTED (it names repository paths): it is
// stored split into a closed reason_code and a bounded, control-stripped
// reason_detail flagged reason_untrusted, neutralized at every render
// (issuecomment.NeutralizeUntrustedInline) and never rendered into a prompt.

// Transfer audit categories. transferred and transfer_refused are
// operator-visible (anchor / status timeline); false_transfer is an
// operator-visible ADVISORY that never blocks a gate; shadow is INTERNAL.
const (
	CategoryMergeCandidateVerdictTransferred     = "merge_candidate_verdict_transferred"
	CategoryMergeCandidateVerdictTransferRefused = "merge_candidate_verdict_transfer_refused"
	CategoryMergeCandidateVerdictTransferShadow  = "merge_candidate_verdict_transfer_shadow"
	CategoryMergeCandidateVerdictFalseTransfer   = "merge_candidate_verdict_false_transfer"
)

// MergeCandidateTransferMode selects how verdict transfer runs. The Config
// ZERO VALUE ("") is off, so an in-process test server is unchanged; fishhawkd
// defaults to shadow (ADR-098 D3).
type MergeCandidateTransferMode string

// Transfer modes.
const (
	MergeCandidateTransferOff     MergeCandidateTransferMode = "off"
	MergeCandidateTransferShadow  MergeCandidateTransferMode = "shadow"
	MergeCandidateTransferEnforce MergeCandidateTransferMode = "enforce"
)

// ParseMergeCandidateTransferMode validates a configured mode. Any value
// outside off|shadow|enforce is an error naming the value and the allowed set,
// so fishhawkd refuses to start on a typo rather than guessing.
func ParseMergeCandidateTransferMode(v string) (MergeCandidateTransferMode, error) {
	switch m := MergeCandidateTransferMode(v); m {
	case MergeCandidateTransferOff, MergeCandidateTransferShadow, MergeCandidateTransferEnforce:
		return m, nil
	}
	return "", fmt.Errorf("invalid merge-candidate transfer mode %q: must be one of off, shadow, enforce", v)
}

// MergeCandidateClassifier decides one base delta (basedelta.Classifier).
type MergeCandidateClassifier interface {
	Classify(ctx context.Context, in basedelta.Input) basedelta.Result
}

// MergeCandidateTransferConfig is Config.MergeCandidateTransfer.
type MergeCandidateTransferConfig struct {
	// Mode is off, shadow or enforce; "" is off.
	Mode MergeCandidateTransferMode
	// Classifier decides the delta; nil makes every decision undecidable.
	Classifier MergeCandidateClassifier
	// Deadline is the classification SUB-budget (inputs + classify); zero is
	// mergeCandidateTransferDefaultDeadline.
	Deadline time.Duration
	// LookPath probes git and go at startup; nil is exec.LookPath.
	LookPath func(string) (string, error)
}

// mergeCandidateTransferDefaultDeadline is the default classification
// sub-budget. It is deliberately far below basedelta.DefaultDeadline: the
// decision sits on a hand-off path (approval condition S2).
const mergeCandidateTransferDefaultDeadline = 30 * time.Second

// mergeCandidateTransferHandOffReserve is what an admission-path caller must
// leave of its hand-off budget for the re-anchor, the verdict reads, the pass
// start and Unadmit (approval condition S2).
const mergeCandidateTransferHandOffReserve = 60 * time.Second

// mergeCandidateTransferShadowWait bounds how long a shadow decision waits for
// the one classification slot before it records undecidable:busy. A var only
// so a test can shorten it.
var mergeCandidateTransferShadowWait = 10 * time.Minute

// mergeCandidateTransferDetailMax bounds the stored reason_detail in bytes.
const mergeCandidateTransferDetailMax = basedelta.MaxReasonLen

// Closed reason codes. A classifier code (independent, global_surface,
// shared_artifact, reach_overlap, undecidable) carries the untrusted
// remainder as reason_detail; every other code is Fishhawk's own.
const (
	transferReasonIndependent        = "independent"
	transferReasonGlobalSurface      = "global_surface"
	transferReasonSharedArtifact     = "shared_artifact"
	transferReasonReachOverlap       = "reach_overlap"
	transferReasonUndecidable        = "undecidable"
	transferReasonDeadlineExceeded   = "undecidable:deadline_exceeded"
	transferReasonIncompleteExport   = "undecidable:incomplete_export"
	transferReasonNoClassifier       = "undecidable:no_classifier"
	transferReasonBusy               = "undecidable:busy"
	transferReasonConflictResolution = "conflict_resolution"
	transferReasonNotCleanAdvance    = "not_clean_advance"
	transferReasonSourceNotPassed    = "source_not_passed:"
	transferReasonSourceUnreadable   = "source_unreadable"
	transferReasonNoWorkingDir       = "no_working_dir"
	transferReasonInputs             = "inputs:"
	transferReasonUnknown            = "unknown"
)

// errTransferWorkspace marks a workspace that could not be created.
var errTransferWorkspace = errors.New("merge-candidate transfer: workspace")

// mergeCandidateTransferRequest is one clean-advance decision request.
type mergeCandidateTransferRequest struct {
	Run       *run.Run
	PriorHead string
	NewHead   string
	// MergePerformed is the FULL clean-advance predicate, not merely "a merge
	// happened": the caller sets it true only when THIS advance performed a
	// merge, the new head IS the decoded merge commit, AND that merge
	// commit's parents were verified as exactly [prior head, base tip]
	// (ADR-112 R1(a)(ii), rebaseAdvanceOutcome.CleanVerified). Both callers —
	// the rebase verb (rebaseMergeCandidateVerify) and the queue-admission
	// auto-advance (admitMergeCandidateEntry) — apply the same rule, so
	// false here is decided not_clean_advance.
	MergePerformed bool
	Cause          string
	// Reserve is the part of ctx's remaining time the classification must
	// leave untouched: mergeCandidateTransferHandOffReserve on the admission
	// path, zero on a caller that runs on its own detached context.
	Reserve time.Duration
}

// mergeCandidateTransferDecision is decideMergeCandidateTransfer's answer.
// Computed is false when the ladder stopped before anything was decided (no
// verify command, mode off): no row is written.
type mergeCandidateTransferDecision struct {
	Computed        bool
	Transfer        bool
	ReasonCode      string
	ReasonDetail    string
	FromHead        string
	ToHead          string
	FromBase        string
	ToBase          string
	RootHead        string
	Cause           string
	DeltaPathsCount int
	PRPathsCount    int
	Duration        time.Duration
}

// mergeCandidateTransferPayload is the shared payload of the transferred,
// transfer_refused and shadow rows. reason_detail is UNTRUSTED.
type mergeCandidateTransferPayload struct {
	FromHead        string `json:"from_head"`
	ToHead          string `json:"to_head"`
	FromBase        string `json:"from_base,omitempty"`
	ToBase          string `json:"to_base,omitempty"`
	RootHead        string `json:"root_head,omitempty"`
	Cause           string `json:"cause,omitempty"`
	DeltaPathsCount int    `json:"delta_paths_count"`
	PRPathsCount    int    `json:"pr_paths_count"`
	Reason          string `json:"reason"`
	ReasonCode      string `json:"reason_code"`
	ReasonDetail    string `json:"reason_detail,omitempty"`
	ReasonUntrusted bool   `json:"reason_untrusted"`
	WouldTransfer   *bool  `json:"would_transfer,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	SurfaceVersion  int    `json:"surface_version"`
	Mode            string `json:"mode"`
}

// mergeCandidateFalseTransferPayload is the ADVISORY false-transfer row.
type mergeCandidateFalseTransferPayload struct {
	ToHead           string `json:"to_head"`
	FromHead         string `json:"from_head"`
	ReasonCode       string `json:"reason_code"`
	ShadowSequence   int64  `json:"shadow_sequence"`
	VerifiedSequence int64  `json:"verified_sequence"`
	Mode             string `json:"mode"`
}

// mergeCandidateTransferInputs are the classifier inputs plus a cleanup the
// caller must run.
type mergeCandidateTransferInputs struct {
	Delta      basedelta.DeltaInputs
	Workspaces []string
	Cleanup    func()
}

// mergeCandidateTransferInputsFunc resolves the inputs; the Server's
// transferInputs seam overrides the default in tests.
type mergeCandidateTransferInputsFunc func(ctx context.Context, workingDir, candidate, priorHead, rootHead string) (*mergeCandidateTransferInputs, error)

// mergeCandidateTransferMode reports the configured mode; "" and any unknown
// value read as off.
func (s *Server) mergeCandidateTransferMode() MergeCandidateTransferMode {
	switch m := s.cfg.MergeCandidateTransfer.Mode; m {
	case MergeCandidateTransferShadow, MergeCandidateTransferEnforce:
		return m
	}
	return MergeCandidateTransferOff
}

// logMergeCandidateTransferStartup is the boot attribution (approval condition
// S2/C3): a WARN naming the mode whenever an EXPLICITLY configured mode is not
// shadow, and — when the mode is not off — a distinctive WARN per missing git
// or go binary, without which every decision is undecidable. The zero value
// (an in-process server that never set the mode) logs nothing.
func (s *Server) logMergeCandidateTransferStartup() {
	cfg := s.cfg.MergeCandidateTransfer
	if cfg.Mode == "" {
		return
	}
	mode := s.mergeCandidateTransferMode()
	if mode != MergeCandidateTransferShadow {
		s.cfg.Logger.Warn("merge-candidate verdict transfer: mode is not the shadow default (ADR-098 D3)",
			slog.String("mode", string(mode)))
	}
	if mode == MergeCandidateTransferOff {
		return
	}
	lookPath := cfg.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, bin := range []string{"git", "go"} {
		if _, err := lookPath(bin); err != nil {
			s.cfg.Logger.Warn("merge-candidate verdict transfer: binary missing on PATH; every transfer decision will be undecidable (no shadow data, no transfer)",
				slog.String("mode", string(mode)),
				slog.String("missing_binary", bin))
		}
	}
}

// mergeCandidateTransferSubBudget carves the classification budget from ctx:
// min(Deadline, remaining - reserve). ok is false when nothing is left.
func (s *Server) mergeCandidateTransferSubBudget(ctx context.Context, reserve time.Duration) (time.Duration, bool) {
	budget := s.cfg.MergeCandidateTransfer.Deadline
	if budget <= 0 {
		budget = mergeCandidateTransferDefaultDeadline
	}
	if dl, has := ctx.Deadline(); has {
		left := time.Until(dl) - reserve
		if left <= 0 {
			return 0, false
		}
		budget = min(budget, left)
	}
	return budget, true
}

// decideMergeCandidateTransfer runs the ladder; the first failure wins:
//
//  1. no verify command declared, or mode off → nothing computed;
//  2. cause conflict_resolution → conflict_resolution; not a performed
//     merge or cause != base_advance → not_clean_advance;
//  3. the prior head's state read fails → source_unreadable; is not passed →
//     source_not_passed:<state>;
//  4. no working dir → no_working_dir;
//  5. inputs: deadline → undecidable:deadline_exceeded; an incomplete tree
//     or a symlink resolving outside the repository →
//     undecidable:incomplete_export; any other → inputs:<sentinel code>;
//  6. the classifier's own code (independent, global_surface,
//     shared_artifact, reach_overlap, undecidable), or
//     undecidable:deadline_exceeded once the sub-budget is spent.
//
// The classifier is reached only past every precondition.
func (s *Server) decideMergeCandidateTransfer(ctx context.Context, req mergeCandidateTransferRequest) mergeCandidateTransferDecision {
	start := time.Now()
	d := mergeCandidateTransferDecision{FromHead: req.PriorHead, ToHead: req.NewHead, Cause: req.Cause}
	finish := func(code, detail string) mergeCandidateTransferDecision {
		d.Computed = true
		d.ReasonCode = code
		d.ReasonDetail = boundTransferDetail(detail)
		d.Transfer = code == transferReasonIndependent
		d.Duration = time.Since(start)
		return d
	}
	if req.Run == nil {
		return d
	}
	if command, _, _ := s.resolveVerifyConfig(ctx, req.Run, run.StageTypeImplement); command == "" {
		return d
	}
	if s.mergeCandidateTransferMode() == MergeCandidateTransferOff {
		return d
	}
	if req.Cause == mergeCandidateCauseConflictResolution {
		return finish(transferReasonConflictResolution, "")
	}
	if !req.MergePerformed || req.Cause != mergeCandidateCauseBaseAdvance {
		return finish(transferReasonNotCleanAdvance, "")
	}
	st, err := s.mergeCandidateVerifyState(ctx, req.Run, req.PriorHead)
	if err != nil {
		return finish(transferReasonSourceUnreadable, err.Error())
	}
	if st.State != mergeCandidateStatePassed {
		return finish(transferReasonSourceNotPassed+closedMergeCandidateState(st.State), "")
	}
	d.RootHead = req.PriorHead
	if st.Source == mergeCandidateSourceTransferred && st.RootHead != "" {
		d.RootHead = st.RootHead
	}
	if req.Run.WorkingDir == "" {
		return finish(transferReasonNoWorkingDir, "")
	}

	budget, ok := s.mergeCandidateTransferSubBudget(ctx, req.Reserve)
	if !ok {
		return finish(transferReasonDeadlineExceeded, "")
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resolve := s.transferInputs
	if resolve == nil {
		resolve = defaultMergeCandidateTransferInputs
	}
	in, err := resolve(cctx, req.Run.WorkingDir, req.NewHead, req.PriorHead, d.RootHead)
	if err != nil {
		switch {
		case cctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
			return finish(transferReasonDeadlineExceeded, "")
		case errors.Is(err, basedelta.ErrIncompleteTree), errors.Is(err, basedelta.ErrUnsafeTreePath),
			errors.Is(err, basedelta.ErrSymlinkEscape):
			return finish(transferReasonIncompleteExport, err.Error())
		case errors.Is(err, errTransferWorkspace):
			return finish(transferReasonInputs+"workspace", err.Error())
		}
		return finish(transferReasonInputs+basedelta.InputsErrorCode(err), err.Error())
	}
	if in.Cleanup != nil {
		defer in.Cleanup()
	}
	d.FromBase, d.ToBase = in.Delta.FromBase, in.Delta.ToBase
	d.DeltaPathsCount, d.PRPathsCount = len(in.Delta.DeltaPaths), len(in.Delta.PRPaths)

	classifier := s.cfg.MergeCandidateTransfer.Classifier
	if classifier == nil {
		return finish(transferReasonNoClassifier, "")
	}
	res := classifier.Classify(cctx, basedelta.Input{
		DeltaPaths: in.Delta.DeltaPaths,
		PRPaths:    in.Delta.PRPaths,
		Workspaces: in.Workspaces,
	})
	if cctx.Err() != nil {
		return finish(transferReasonDeadlineExceeded, "")
	}
	code, detail := splitTransferReason(res)
	return finish(code, detail)
}

// closedMergeCandidateState maps a verify state onto its closed set.
func closedMergeCandidateState(state string) string {
	switch state {
	case mergeCandidateStateNotRequired, mergeCandidateStateUnverified, mergeCandidateStateInFlight,
		mergeCandidateStateFailed:
		return state
	}
	return transferReasonUnknown
}

// splitTransferReason splits a classifier Result into a closed reason code
// and the untrusted remainder. An independent code requires Independent; an
// unknown prefix is `unknown` with the whole reason as detail.
func splitTransferReason(res basedelta.Result) (code, detail string) {
	r := res.Reason
	if res.Independent {
		if r == basedelta.ReasonIndependent {
			return transferReasonIndependent, ""
		}
		return transferReasonUnknown, r
	}
	for _, p := range []struct{ prefix, code string }{
		{basedelta.PrefixGlobalSurface, transferReasonGlobalSurface},
		{basedelta.PrefixSharedArtifact, transferReasonSharedArtifact},
		{basedelta.PrefixReachOverlap, transferReasonReachOverlap},
		{basedelta.PrefixUndecidable, transferReasonUndecidable},
	} {
		if rest, ok := strings.CutPrefix(r, p.prefix); ok {
			return p.code, rest
		}
	}
	return transferReasonUnknown, r
}

// boundTransferDetail drops control characters (newlines included) and caps
// the detail at mergeCandidateTransferDetailMax bytes on a rune boundary. It
// is a STORAGE bound, not a render neutralizer.
func boundTransferDetail(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > mergeCandidateTransferDetailMax {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// defaultMergeCandidateTransferInputs reads the delta from the run's LOCAL
// checkout, then materializes B' and H' with basedelta.MaterializeTree —
// never `git archive`, so export-ignore cannot hide a package and symlinks are
// kept (approval condition S0/S2). Every symlink of either tree whose resolved
// target is, or contains, a delta or PR path then joins that list
// (basedelta.ExpandSymlinkAliases), so an edit to a link's target is seen at
// the link's path too. A first GitDelta failure that is a plain
// git error gets one fetch-on-miss of the candidate (reviewsandbox.ExportTree,
// whose export is discarded unread) and one retry: a forge-made merge commit
// is not in the checkout until fetched.
func defaultMergeCandidateTransferInputs(ctx context.Context, workingDir, candidate, priorHead, rootHead string) (*mergeCandidateTransferInputs, error) {
	delta, err := basedelta.GitDelta(ctx, workingDir, candidate, priorHead, rootHead)
	if err != nil && basedelta.InputsErrorCode(err) == "git" {
		if _, _, _, cleanup, ferr := reviewsandbox.ExportTree(ctx, workingDir, candidate, reviewsandbox.DefaultLimits()); ferr == nil {
			cleanup()
		}
		delta, err = basedelta.GitDelta(ctx, workingDir, candidate, priorHead, rootHead)
	}
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "fishhawk-transfer-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errTransferWorkspace, err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	var spaces []string
	var links []map[string]string
	for _, c := range []struct{ name, commit string }{{"base", delta.ToBase}, {"candidate", candidate}} {
		dir := filepath.Join(root, c.name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			cleanup()
			return nil, fmt.Errorf("%w: %w", errTransferWorkspace, err)
		}
		l, err := basedelta.MaterializeTreeLinks(ctx, workingDir, c.commit, dir)
		if err != nil {
			cleanup()
			return nil, err
		}
		spaces = append(spaces, dir)
		links = append(links, l)
	}
	if delta, err = basedelta.ExpandSymlinkAliases(delta, links...); err != nil {
		cleanup()
		return nil, err
	}
	return &mergeCandidateTransferInputs{Delta: delta, Workspaces: spaces, Cleanup: cleanup}, nil
}

// transferPayload renders a decision as the shared row payload.
func (d mergeCandidateTransferDecision) transferPayload(mode MergeCandidateTransferMode) mergeCandidateTransferPayload {
	return mergeCandidateTransferPayload{
		FromHead: d.FromHead, ToHead: d.ToHead, FromBase: d.FromBase, ToBase: d.ToBase,
		RootHead: d.RootHead, Cause: d.Cause,
		DeltaPathsCount: d.DeltaPathsCount, PRPathsCount: d.PRPathsCount,
		Reason: d.ReasonCode, ReasonCode: d.ReasonCode, ReasonDetail: d.ReasonDetail,
		ReasonUntrusted: true,
		DurationMS:      d.Duration.Milliseconds(),
		SurfaceVersion:  basedelta.SurfaceVersion,
		Mode:            string(mode),
	}
}

// appendMergeCandidateTransferRow appends one run-level system-actor row.
func (s *Server) appendMergeCandidateTransferRow(ctx context.Context, runID uuid.UUID, category string, payload any) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("no audit repository is wired")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	kind := audit.ActorSystem
	subject := mergeCandidateSystemSubject
	_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      raw,
	})
	return err
}

// appendMergeCandidateTransferred records a transfer (enforce only).
func (s *Server) appendMergeCandidateTransferred(ctx context.Context, runID uuid.UUID, d mergeCandidateTransferDecision) error {
	if err := s.appendMergeCandidateTransferRow(ctx, runID, CategoryMergeCandidateVerdictTransferred,
		d.transferPayload(s.mergeCandidateTransferMode())); err != nil {
		return err
	}
	s.notifyOperatorVisible(ctx, runID, CategoryMergeCandidateVerdictTransferred)
	return nil
}

// appendMergeCandidateTransferRefused records a refused transfer (enforce).
func (s *Server) appendMergeCandidateTransferRefused(ctx context.Context, runID uuid.UUID, d mergeCandidateTransferDecision) error {
	if err := s.appendMergeCandidateTransferRow(ctx, runID, CategoryMergeCandidateVerdictTransferRefused,
		d.transferPayload(s.mergeCandidateTransferMode())); err != nil {
		return err
	}
	s.notifyOperatorVisible(ctx, runID, CategoryMergeCandidateVerdictTransferRefused)
	return nil
}

// appendMergeCandidateTransferShadow records the would-be decision (INTERNAL),
// then checks whether the head's pass already went red.
func (s *Server) appendMergeCandidateTransferShadow(ctx context.Context, runID uuid.UUID, d mergeCandidateTransferDecision) error {
	p := d.transferPayload(MergeCandidateTransferShadow)
	would := d.Transfer
	p.WouldTransfer = &would
	if err := s.appendMergeCandidateTransferRow(ctx, runID, CategoryMergeCandidateVerdictTransferShadow, p); err != nil {
		return err
	}
	if would {
		s.detectMergeCandidateFalseTransfers(ctx, runID, d.ToHead)
	}
	return nil
}

// mergeCandidateTransferEnforce decides and writes in ENFORCE mode. It returns
// true only when the transferred row is DURABLE, so the caller may skip the
// pass; on a refusal (or any append failure) it returns false and the caller
// runs the pass. Outside enforce it writes nothing and returns false.
func (s *Server) mergeCandidateTransferEnforce(ctx context.Context, req mergeCandidateTransferRequest) (bool, mergeCandidateTransferDecision) {
	if s.mergeCandidateTransferMode() != MergeCandidateTransferEnforce || req.Run == nil {
		return false, mergeCandidateTransferDecision{}
	}
	d := s.decideMergeCandidateTransfer(ctx, req)
	if !d.Computed {
		return false, d
	}
	log := func(msg string, err error) {
		attrs := []slog.Attr{
			slog.String("run_id", req.Run.ID.String()),
			slog.String("from_head", d.FromHead),
			slog.String("to_head", d.ToHead),
			slog.String("reason_code", d.ReasonCode),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, msg, attrs...)
	}
	if d.Transfer {
		if err := s.appendMergeCandidateTransferred(ctx, req.Run.ID, d); err != nil {
			log("merge-candidate verdict transfer: the transferred row did not append; running the pass", err)
			d.Transfer = false
			return false, d
		}
		return true, d
	}
	if err := s.appendMergeCandidateTransferRefused(ctx, req.Run.ID, d); err != nil {
		log("merge-candidate verdict transfer: the refused row did not append", err)
	}
	return false, d
}

// scheduleMergeCandidateTransferShadow runs the decision ASYNC in shadow mode
// on a detached context bounded by the sub-budget, behind the one-slot
// classification semaphore, and appends the shadow row. It never touches the
// pass path; outside shadow it does nothing.
func (s *Server) scheduleMergeCandidateTransferShadow(req mergeCandidateTransferRequest) {
	if s.mergeCandidateTransferMode() != MergeCandidateTransferShadow || req.Run == nil {
		return
	}
	req.Reserve = 0
	s.bgTransferShadows.Add(1)
	go func() {
		defer s.bgTransferShadows.Done()
		ctx := context.Background()
		wait := time.NewTimer(mergeCandidateTransferShadowWait)
		defer wait.Stop()
		var d mergeCandidateTransferDecision
		select {
		case s.transferSem <- struct{}{}:
			budget, _ := s.mergeCandidateTransferSubBudget(ctx, 0)
			dctx, cancel := context.WithTimeout(ctx, budget+mergeCandidateTransferDefaultDeadline)
			d = s.decideMergeCandidateTransfer(dctx, req)
			cancel()
			<-s.transferSem
		case <-wait.C:
			d = mergeCandidateTransferDecision{Computed: true, ReasonCode: transferReasonBusy,
				FromHead: req.PriorHead, ToHead: req.NewHead, Cause: req.Cause}
		}
		if !d.Computed {
			return
		}
		actx, cancel := context.WithTimeout(ctx, rebasePostMergeTailBudget)
		defer cancel()
		if err := s.appendMergeCandidateTransferShadow(actx, req.Run.ID, d); err != nil {
			s.cfg.Logger.LogAttrs(actx, slog.LevelWarn, "merge-candidate verdict transfer: the shadow row did not append",
				slog.String("run_id", req.Run.ID.String()),
				slog.String("to_head", req.NewHead),
				slog.String("error", err.Error()))
		}
	}()
}

// waitMergeCandidateTransferShadows blocks until every scheduled shadow
// decision has appended (tests; shutdown deliberately does not wait — a lost
// shadow row is a measurement gap, never a skipped pass).
func (s *Server) waitMergeCandidateTransferShadows() { s.bgTransferShadows.Wait() }

// detectMergeCandidateFalseTransfers appends one ADVISORY false-transfer row
// per shadow would_transfer row for headSHA whose head carries a RECORDED
// merge_candidate_verified{failed}. Both writers call it after their own
// append (the shadow writer and recordMergeCandidateVerifiedEvidence), so
// either arrival order is caught. It is idempotent per shadow row, never
// blocks a gate, and a read or append failure only logs.
func (s *Server) detectMergeCandidateFalseTransfers(ctx context.Context, runID uuid.UUID, headSHA string) {
	if s.cfg.AuditRepo == nil || headSHA == "" {
		return
	}
	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, msg,
			slog.String("run_id", runID.String()),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
	}
	verified, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerified)
	if err != nil {
		warn("merge-candidate verdict transfer: false-transfer check could not read verdicts", err)
		return
	}
	var failedSeq int64 = -1
	for _, e := range verified {
		var p mergeCandidateVerifiedPayload
		if json.Unmarshal(e.Payload, &p) == nil && p.HeadSHA == headSHA && p.Result == mergeCandidateResultFailed {
			failedSeq = e.Sequence
			break
		}
	}
	if failedSeq < 0 {
		return
	}
	shadows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerdictTransferShadow)
	if err != nil {
		warn("merge-candidate verdict transfer: false-transfer check could not read shadow rows", err)
		return
	}
	recorded, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeCandidateVerdictFalseTransfer)
	if err != nil {
		warn("merge-candidate verdict transfer: false-transfer check could not read prior findings", err)
		return
	}
	done := map[int64]bool{}
	for _, e := range recorded {
		var p mergeCandidateFalseTransferPayload
		if json.Unmarshal(e.Payload, &p) == nil && p.ToHead == headSHA {
			done[p.ShadowSequence] = true
		}
	}
	for _, e := range shadows {
		var p mergeCandidateTransferPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.ToHead != headSHA || p.WouldTransfer == nil || !*p.WouldTransfer || done[e.Sequence] {
			continue
		}
		row := mergeCandidateFalseTransferPayload{
			ToHead: p.ToHead, FromHead: p.FromHead, ReasonCode: p.ReasonCode,
			ShadowSequence: e.Sequence, VerifiedSequence: failedSeq, Mode: p.Mode,
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"merge-candidate verdict transfer: FALSE TRANSFER — shadow would have transferred a verdict whose pass failed",
			slog.String("run_id", runID.String()),
			slog.String("to_head", row.ToHead),
			slog.String("from_head", row.FromHead),
			slog.Int64("shadow_sequence", row.ShadowSequence),
			slog.Int64("verified_sequence", row.VerifiedSequence))
		if err := s.appendMergeCandidateTransferRow(ctx, runID, CategoryMergeCandidateVerdictFalseTransfer, row); err != nil {
			warn("merge-candidate verdict transfer: the false-transfer row did not append", err)
			continue
		}
		done[e.Sequence] = true
		s.notifyOperatorVisible(ctx, runID, CategoryMergeCandidateVerdictFalseTransfer)
	}
}
