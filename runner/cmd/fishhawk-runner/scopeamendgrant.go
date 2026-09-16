package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Unused approved mid-stage scope grant (E64 / #3390) — the sibling of the
// #3320 settle wait in scopeamendwait.go.
//
// The incident (run faf4cf76): the agent filed a mid-stage amendment, the
// operator approved it 25 seconds later, refreshScopeAmendments folded the
// path into cfg.scopeFiles BEFORE the verify-fix loop ran — and the stage
// still died in that loop with the granted file never edited, because the
// fresh fix agent's prompt named neither the scope nor the grant. Two runner
// side mechanisms close that: verifyFixPrompt now renders the effective scope
// and the GRANTED MID-STAGE block (main.go), and the helpers here make an
// unused grant LOUD — one JSONL line + policy_event as evidence on every path,
// and a one-line failure-reason attribution on the failing path.

// Grant dispositions (#3434). Each unused grant carries exactly one.
const (
	// grantNotModified: the granted path is absent from the dirty set — the
	// agent never touched it (today's #3390 meaning).
	grantNotModified = "not_modified"
	// grantNotStaged: the granted path IS dirty but ABSENT from the staged
	// set — modified in the working tree yet excluded from the verified
	// commit's index. This is the ` M` post-mortem shape of run 49f642b8: a
	// fix agent edited and `git add`ed the file, and the loop's scope-only
	// re-stage unstaged it because the path was not in the scope set the
	// commit was cut from. Reported only when the caller can vouch that the
	// index IS the verified commit's staged set (stagedKnown).
	grantNotStaged = "not_staged"
)

// unusedScopeAmendmentGrant is one granted path the verified commit did not
// carry, with the Disposition saying why.
type unusedScopeAmendmentGrant struct {
	AmendmentID string
	Path        string
	Operation   string
	Disposition string
}

// unusedScopeAmendmentGrants returns, in row/path order, every path of an
// approved row belonging to stageID that the verified commit did not carry:
// a path absent from the dirty working-tree set (Disposition not_modified)
// and, when stagedKnown, a path present in the dirty set but absent from the
// staged set (Disposition not_staged). With stagedKnown false the second
// branch is disabled and a dirty grant counts as used, exactly the pre-#3434
// behaviour — the caller withholds the refinement whenever the index is
// agent-left state rather than a committed-tree gate's staged set.
//
// It mirrors gitops.MissingScopeFiles's skip rules: an empty path, a
// trailing-slash directory prefix (#824), and a non-repo-relative path (`/`
// prefix or `..` traversal, #1155) are skipped because none can be required
// to match a per-file dirty entry without a guaranteed false-trip.
//
// An empty stageID returns nil, mirroring undecidedScopeAmendmentsForStage: a
// row whose StageID is also empty would otherwise match by accident and
// mis-attribute another stage's grant to this one.
//
// The dirty set is `git status --porcelain -uall` (gitops.DirtyPaths), which
// lists modified, deleted, and untracked paths per file — so a granted create,
// modify, or delete that the agent performed all count as USED. A granted path
// a pre-existing operator edit had already dirtied counts as used too: that is
// a false NEGATIVE only, never a false failure. The staged set is the
// index-column subset of the same listing (gitops.StagedPaths).
func unusedScopeAmendmentGrants(approved []upload.ScopeAmendment, stageID string, dirty, staged []string, stagedKnown bool) []unusedScopeAmendmentGrant {
	if stageID == "" {
		return nil
	}
	dirtySet := make(map[string]struct{}, len(dirty))
	for _, d := range dirty {
		dirtySet[d] = struct{}{}
	}
	stagedSet := make(map[string]struct{}, len(staged))
	for _, st := range staged {
		stagedSet[st] = struct{}{}
	}
	var out []unusedScopeAmendmentGrant
	for _, a := range approved {
		if a.Status != "approved" || a.StageID != stageID {
			continue
		}
		for _, p := range a.Paths {
			if p.Path == "" || strings.HasSuffix(p.Path, "/") {
				continue
			}
			if strings.HasPrefix(p.Path, "/") || strings.Contains(p.Path, "..") {
				continue
			}
			disposition := grantNotModified
			if _, ok := dirtySet[p.Path]; ok {
				if !stagedKnown {
					continue
				}
				if _, ok := stagedSet[p.Path]; ok {
					continue
				}
				disposition = grantNotStaged
			}
			out = append(out, unusedScopeAmendmentGrant{AmendmentID: a.ID, Path: p.Path, Operation: p.Operation, Disposition: disposition})
		}
	}
	return out
}

// stagedPathsFn is the gitops.StagedPaths seam, swappable in tests so the
// StagedPaths-error degrade branch of detectUnusedScopeAmendmentGrants is
// reachable while DirtyPaths (the same `git status` invocation) still
// succeeds.
var stagedPathsFn = gitops.StagedPaths

// unusedGrantStagedKnown is the run()-level predicate deciding whether the
// index can be read as "the verified commit's staged set" for the not_staged
// disposition (#3434). True exactly when a committed-tree gate — the verify-
// fix loop or the #802 single-shot — ran StageScoped last: both cut a
// scope-only throwaway commit and undo it with `git reset --soft`, which
// leaves the index intact, so index-column X of `git status --porcelain` is
// precisely "staged into the verified commit". On the verifyCmd=="" path no
// gate ran (the index is agent-left state), and on the appliedFixup path the
// deterministic apply's gate ran BEFORE the amendment fold, so its staged set
// was cut from the pre-fold scope and cannot vouch for a folded path; both
// withhold the refinement (a dirty grant counts as used).
func unusedGrantStagedKnown(appliedFixup bool, verifyCmd string) bool {
	return !appliedFixup && verifyCmd != ""
}

// detectUnusedScopeAmendmentGrants reads the working tree's dirty set and
// reports every granted path of THIS stage's approved amendments
// (cfg.approvedAmendments, recorded by refreshScopeAmendments) that the agent
// never touched. Returns the unused grants plus a single
// scope_amendment_grant_unused policy_event for the trace bundle, and writes
// one JSONL line to logSink so the runner log carries the signal.
//
// No-op (nil, nil — no git call) when cfg.stageID is empty or when no approved
// row belongs to this stage. A `git status` failure logs
// scope_amendment_grant_check_failed and returns nil, nil: fail-open, the
// stage is never failed because the check could not run. When stagedKnown
// (unusedGrantStagedKnown) the staged set is read too and a dirty-but-unstaged
// grant is reported with Disposition not_staged (#3434); a StagedPaths failure
// logs the same check_failed line and DEGRADES to stagedKnown=false — the
// not_modified rows are still reported, never a false not_staged line. The
// check NEVER sets res.OK, a failure category, or a failure reason itself —
// the caller in run() owns the failure-only annotation.
func detectUnusedScopeAmendmentGrants(ctx context.Context, cfg config, stagedKnown bool, logSink io.Writer) ([]unusedScopeAmendmentGrant, []agent.Event) {
	if cfg.stageID == "" || !hasApprovedAmendmentForStage(cfg.approvedAmendments, cfg.stageID) {
		return nil, nil
	}
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	dirty, err := gitops.DirtyPaths(ctx, repoDir)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"scope_amendment_grant_check_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			cfg.runID, cfg.stageID, err.Error())
		return nil, nil
	}
	var staged []string
	if stagedKnown {
		staged, err = stagedPathsFn(ctx, repoDir)
		if err != nil {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"scope_amendment_grant_check_failed","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
				cfg.runID, cfg.stageID, "staged set unavailable; reporting not_modified grants only: "+err.Error())
			stagedKnown = false
		}
	}
	unused := unusedScopeAmendmentGrants(cfg.approvedAmendments, cfg.stageID, dirty, staged, stagedKnown)
	if len(unused) == 0 {
		return nil, nil
	}
	// The literal field set {event, run_id, stage_id, grants:[{amendment_id,
	// path, operation, disposition}]} is pinned by
	// TestEmitScopeAmendmentGrantUnused_SeamContract. No backend or frontend
	// consumer parses these rows — they are an opaque pass-through into the
	// bundle and the runner log — so adding `disposition` is not a wire-
	// contract change.
	rows := make([]map[string]any, 0, len(unused))
	for _, g := range unused {
		rows = append(rows, map[string]any{
			"amendment_id": g.AmendmentID,
			"path":         g.Path,
			"operation":    g.Operation,
			"disposition":  g.Disposition,
		})
	}
	rowsJSON, _ := json.Marshal(rows)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"scope_amendment_grant_unused","run_id":%q,"stage_id":%q,"grants":%s}`+"\n",
		cfg.runID, cfg.stageID, rowsJSON)
	// Mirror the scope_drift / scope_amendments_folded /
	// scope_amendment_settle_wait policy_event shape so the record rides into
	// BOTH bundle variants (PackBytes / redactEvents).
	return unused, []agent.Event{{
		Kind: "policy_event",
		Payload: agent.MakePayload(map[string]any{
			"check":  "scope_amendment_grant_unused",
			"grants": rows,
		}),
	}}
}

// hasApprovedAmendmentForStage reports whether any approved row belongs to
// stageID — the run()-gate / inner-filter seam: run() gates on
// len(cfg.approvedAmendments) > 0 (any stage), this filter decides whether a
// git call is warranted for THIS stage.
func hasApprovedAmendmentForStage(approved []upload.ScopeAmendment, stageID string) bool {
	for _, a := range approved {
		if a.Status == "approved" && a.StageID == stageID {
			return true
		}
	}
	return false
}

// annotateUnusedAmendmentFailure appends the unused-grant attribution to a
// failure reason (#3390, dispositions #3434): one line per grant, EXACTLY
// `amendment <id> granted <path> (<op>); file not modified` for a
// not_modified grant (byte-identical to the #3390 line; the parenthetical
// omitted when the operation is empty) and
// `amendment <id> granted <path> (<op>); file modified but not staged into the
// verified commit` for a not_staged one, then a disposition-specific
// one-sentence explanation and the recovery verb. This is the line that
// replaces the log archaeology the issues describe — the amendment was
// approved and in force, yet the verified commit did not carry the granted
// file, so the verify failure is very likely the one the amendment was
// requested to fix.
//
// Pure and reason-preserving: the captured verify output stays the prefix, so
// nothing an operator or reviewer reads today is lost. An empty list returns
// reason unchanged.
func annotateUnusedAmendmentFailure(reason string, unused []unusedScopeAmendmentGrant) string {
	if len(unused) == 0 {
		return reason
	}
	var b strings.Builder
	b.WriteString(reason)
	b.WriteString("\n\n")
	anyNotStaged, anyNotModified := false, false
	for _, g := range unused {
		suffix := "file not modified"
		if g.Disposition == grantNotStaged {
			suffix = "file modified but not staged into the verified commit"
			anyNotStaged = true
		} else {
			anyNotModified = true
		}
		if g.Operation != "" {
			fmt.Fprintf(&b, "amendment %s granted %s (%s); %s\n", g.AmendmentID, g.Path, g.Operation, suffix)
		} else {
			fmt.Fprintf(&b, "amendment %s granted %s; %s\n", g.AmendmentID, g.Path, suffix)
		}
	}
	if anyNotModified {
		b.WriteString("The scope amendment above was APPROVED and in force for this stage, but the granted file was " +
			"never edited, so the verify failure is very likely the one the amendment was requested to fix. " +
			"Recovery: fishhawk_retry_stage — the amendment persists on the stage and folds into the retried scope, " +
			"and the retried agent's fix prompt names it as granted.")
	}
	if anyNotStaged {
		if anyNotModified {
			b.WriteString("\n")
		}
		b.WriteString("The scope amendment above was APPROVED and the granted file WAS edited, but the edit was not in " +
			"the scope-only tree the verify gate committed (the grant landed after that commit's scope was cut, so the " +
			"re-stage unstaged it) — the verify failure very likely ran without the fix it was requested for. " +
			"Recovery: fishhawk_retry_stage — the amendment persists on the stage and folds into the retried scope " +
			"before the first commit, so the retried gate verifies the granted edit.")
	}
	return b.String()
}
