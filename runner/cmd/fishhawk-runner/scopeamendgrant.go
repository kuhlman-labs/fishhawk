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

// unusedScopeAmendmentGrant is one granted path the agent never touched.
type unusedScopeAmendmentGrant struct {
	AmendmentID string
	Path        string
	Operation   string
}

// unusedScopeAmendmentGrants returns, in row/path order, every path of an
// approved row belonging to stageID that is absent from the dirty working-tree
// set. It mirrors gitops.MissingScopeFiles's skip rules: an empty path, a
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
// a false NEGATIVE only, never a false failure.
func unusedScopeAmendmentGrants(approved []upload.ScopeAmendment, stageID string, dirty []string) []unusedScopeAmendmentGrant {
	if stageID == "" {
		return nil
	}
	dirtySet := make(map[string]struct{}, len(dirty))
	for _, d := range dirty {
		dirtySet[d] = struct{}{}
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
			if _, ok := dirtySet[p.Path]; ok {
				continue
			}
			out = append(out, unusedScopeAmendmentGrant{AmendmentID: a.ID, Path: p.Path, Operation: p.Operation})
		}
	}
	return out
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
// stage is never failed because the check could not run. The check NEVER sets
// res.OK, a failure category, or a failure reason itself — the caller in run()
// owns the failure-only annotation.
func detectUnusedScopeAmendmentGrants(ctx context.Context, cfg config, logSink io.Writer) ([]unusedScopeAmendmentGrant, []agent.Event) {
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
	unused := unusedScopeAmendmentGrants(cfg.approvedAmendments, cfg.stageID, dirty)
	if len(unused) == 0 {
		return nil, nil
	}
	// The literal field set {event, run_id, stage_id, grants:[{amendment_id,
	// path, operation}]} is pinned by
	// TestEmitScopeAmendmentGrantUnused_SeamContract.
	rows := make([]map[string]any, 0, len(unused))
	for _, g := range unused {
		rows = append(rows, map[string]any{
			"amendment_id": g.AmendmentID,
			"path":         g.Path,
			"operation":    g.Operation,
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
// failure reason (#3390): one line per grant, EXACTLY
// `amendment <id> granted <path> (<op>); file not modified` (the parenthetical
// omitted when the operation is empty), then a one-sentence explanation and
// the recovery verb. This is the line that replaces the log archaeology the
// issue describes — the amendment was approved and in force, yet the granted
// file was never edited, so the verify failure is very likely the one the
// amendment was requested to fix.
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
	for _, g := range unused {
		if g.Operation != "" {
			fmt.Fprintf(&b, "amendment %s granted %s (%s); file not modified\n", g.AmendmentID, g.Path, g.Operation)
		} else {
			fmt.Fprintf(&b, "amendment %s granted %s; file not modified\n", g.AmendmentID, g.Path)
		}
	}
	b.WriteString("The scope amendment above was APPROVED and in force for this stage, but the granted file was " +
		"never edited, so the verify failure is very likely the one the amendment was requested to fix. " +
		"Recovery: fishhawk_retry_stage — the amendment persists on the stage and folds into the retried scope, " +
		"and the retried agent's fix prompt names it as granted.")
	return b.String()
}
