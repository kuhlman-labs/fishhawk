package server

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The campaign admission screen (#3649).
//
// Campaign admission (POST /v0/campaigns) otherwise screens item DEPENDENCIES
// only — campaign.Assemble fails closed on a dangling depends_on edge — and
// nothing asks whether a candidate is runnable at all, so the batch commitment
// discovers an unrunnable item one item-run at a time. This screen reports two
// such classes on the CREATE response:
//
//   - not_runnable_declared (class 1): the candidate carries an explicit
//     `runnable:no` label (workmgmt.EpicChild.NotRunnable) — a DECLARATION that
//     it produces no diff, never an inference from issue content.
//   - forbidden_path (class 2): the candidate's title or body names a path the
//     repo's implement-stage forbidden_paths forbid — the same collision the
//     #3620 plan-gate forbidden-criterion rule catches, but at assembly rather
//     than at the plan gate of the one item that reached plan. It reuses that
//     rule's token extractor (extractCriterionPathTokens) and matcher
//     (forbiddenGlobForPaths → policy.Evaluate, the post-implement gate's
//     matcher), applied to issue prose instead of acceptance-criteria prose.
//
// ADVISORY in every direction: the screen never refuses, never excludes an
// item, never changes item state or the assembled DAG, and every degrade
// (unwired forge client, unreadable/unparseable spec, no implement stage, no
// forbidden paths) is FAIL-OPEN with a logged reason — class 2 is silently
// skipped and the create proceeds. Issue prose is far noisier than
// acceptance-criterion prose, so false positives are expected; each finding
// carries the exact token and its location so an operator can dismiss it in
// one read.
const (
	// campaignScreenKindNotRunnable is the class-1 finding kind.
	campaignScreenKindNotRunnable = "not_runnable_declared"
	// campaignScreenKindForbiddenPath is the class-2 finding kind.
	campaignScreenKindForbiddenPath = "forbidden_path"
	// campaignAdmissionScreenMaxFindings caps the findings per create response
	// so a prose-heavy batch cannot flood the block.
	campaignAdmissionScreenMaxFindings = 50
	// categoryCampaignAdmissionScreened is the best-effort audit row the create
	// handler appends when the screen produced at least one finding.
	categoryCampaignAdmissionScreened = "campaign_admission_screened"
)

// campaignScreenFinding is one admission-screen finding. Mirrors
// docs/api/v0.openapi.yaml's CampaignAdmissionFinding schema. Path, Location
// and ForbiddenPattern are set only on a forbidden_path finding.
type campaignScreenFinding struct {
	Issue int    `json:"issue"`
	Kind  string `json:"kind"`
	// Path is the path-shaped token named in the issue prose.
	Path string `json:"path,omitempty"`
	// Location is "title" or "body" — where the token was named.
	Location string `json:"location,omitempty"`
	// ForbiddenPattern is the first declared forbidden_paths glob that
	// matches Path.
	ForbiddenPattern string `json:"forbidden_pattern,omitempty"`
}

// campaignAdmissionScreenPayload is the create-response-only admission_screen
// block. Like satisfied_dependencies (#2953) it is never persisted, so a later
// GET omits it. Mirrors docs/api/v0.openapi.yaml's CampaignAdmissionScreen.
type campaignAdmissionScreenPayload struct {
	// Advisory is always true: the screen reports and never blocks.
	Advisory bool                    `json:"advisory"`
	Findings []campaignScreenFinding `json:"findings"`
	// ForbiddenPathsScreened reports whether class 2 ran: false when the
	// repo's implement-stage forbidden_paths could not be resolved (every
	// fail-open degrade), so a class-1-only block is not mistaken for a
	// clean class-2 result.
	ForbiddenPathsScreened bool `json:"forbidden_paths_screened"`
	// Truncated is true when more than campaignAdmissionScreenMaxFindings
	// findings were produced and the list was capped.
	Truncated bool `json:"truncated,omitempty"`
}

// evaluateCampaignAdmissionScreen is the PURE evaluator (no receiver, no I/O),
// mirroring evaluateForbiddenCriterionRule. children are the assembled
// candidates; forbidden is the union of the repo's implement-stage
// forbidden_paths (nil when unresolved).
//
// Contract (operator condition 1): a NotRunnable candidate ALWAYS yields one
// not_runnable_declared finding regardless of forbidden; an empty/nil
// forbidden list only suppresses forbidden_path findings. Per candidate, one
// forbidden_path finding per distinct forbidden token, title scanned before
// body so a token named in both is attributed to the title; the same token in
// two candidates yields one finding each. Findings are sorted by (issue, kind,
// path) and capped at campaignAdmissionScreenMaxFindings (truncated=true when
// capped). Returns nil, false when there are no findings.
func evaluateCampaignAdmissionScreen(children []workmgmt.EpicChild, forbidden []string) ([]campaignScreenFinding, bool) {
	if len(children) == 0 {
		return nil, false
	}

	type mention struct {
		issue          int
		path, location string
	}
	var mentions []mention
	var tokens []string
	seenTok := make(map[string]bool)
	if len(forbidden) > 0 {
		for _, c := range children {
			seen := make(map[string]bool)
			for _, field := range []struct{ location, text string }{
				{"title", c.Title},
				{"body", c.Body},
			} {
				for _, tok := range extractCriterionPathTokens(field.text) {
					if seen[tok] {
						continue
					}
					seen[tok] = true
					mentions = append(mentions, mention{issue: c.Number, path: tok, location: field.location})
					if !seenTok[tok] {
						seenTok[tok] = true
						tokens = append(tokens, tok)
					}
				}
			}
		}
	}
	matched := forbiddenGlobForPaths(tokens, forbidden)

	var out []campaignScreenFinding
	for _, c := range children {
		if c.NotRunnable {
			out = append(out, campaignScreenFinding{Issue: c.Number, Kind: campaignScreenKindNotRunnable})
		}
	}
	for _, m := range mentions {
		glob, ok := matched[m.path]
		if !ok {
			continue
		}
		out = append(out, campaignScreenFinding{
			Issue:            m.issue,
			Kind:             campaignScreenKindForbiddenPath,
			Path:             toSlashPath(m.path),
			Location:         m.location,
			ForbiddenPattern: glob,
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Issue != out[b].Issue {
			return out[a].Issue < out[b].Issue
		}
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Path < out[b].Path
	})
	if len(out) > campaignAdmissionScreenMaxFindings {
		return out[:campaignAdmissionScreenMaxFindings], true
	}
	return out, false
}

// forbiddenPathsUnion returns the deduplicated, sorted union of forbidden_paths
// across EVERY implement stage of EVERY workflow in parsed. A campaign carries
// no workflow_id (it is supplied per item run), so no single workflow is
// resolvable at admission; the union is the right direction for an advisory
// report — the campaign will run SOME workflow from this spec.
func forbiddenPathsUnion(parsed *spec.Spec) []string {
	if parsed == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, wf := range parsed.Workflows {
		for _, st := range wf.Stages {
			if st.Type != spec.StageTypeImplement {
				continue
			}
			for _, p := range flattenPathConstraints(st.Constraints).ForbiddenPaths {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolveCampaignForbiddenPaths fetches the repo's workflow spec at its DEFAULT
// branch (empty ref, the form onboarding.go uses) and returns the
// forbiddenPathsUnion. FAIL-OPEN with a logged one-line reason and ok=false on
// every degrade: s.cfg.GitHub nil; a fetch error (not found, transport, or an
// unresolved/zero credential scope — a non-GitHub work-item provider leaves
// scope zero and the client refuses before any network call); a parse error;
// and a spec declaring no implement-stage forbidden_paths.
func (s *Server) resolveCampaignForbiddenPaths(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) ([]string, bool) {
	if s.cfg.GitHub == nil {
		s.logAdmissionScreenSkip(ctx, repo, "github client not configured")
		return nil, false
	}
	fc, err := s.cfg.GitHub.GetWorkflowSpec(ctx, scope, repo, "")
	if err != nil {
		reason := "fetch workflow spec failed: " + err.Error()
		if errors.Is(err, forge.ErrNotFound) {
			reason = "no workflow spec on the default branch"
		}
		s.logAdmissionScreenSkip(ctx, repo, reason)
		return nil, false
	}
	parsed, err := spec.ParseBytes(fc.Content)
	if err != nil {
		s.logAdmissionScreenSkip(ctx, repo, "parse workflow spec failed: "+err.Error())
		return nil, false
	}
	forbidden := forbiddenPathsUnion(parsed)
	if len(forbidden) == 0 {
		s.logAdmissionScreenSkip(ctx, repo, "no implement-stage forbidden_paths declared")
		return nil, false
	}
	return forbidden, true
}

func (s *Server) logAdmissionScreenSkip(ctx context.Context, repo forge.RepoRef, reason string) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "campaign admission screen: forbidden-path class skipped",
		slog.String("repo", repo.Owner+"/"+repo.Name),
		slog.String("reason", reason),
	)
}

// screenCampaignAdmission runs the advisory screen over the candidates. The
// caller passes the resolved result's Children, which ARE the assembled set:
// the optional items subset is applied by campaign.FilterToSubset before
// campaign.Assemble, and Assemble admits every child it is handed. Returns nil
// when there is nothing to report — the caller then omits the admission_screen
// key and the audit.
func (s *Server) screenCampaignAdmission(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, children []workmgmt.EpicChild) *campaignAdmissionScreenPayload {
	forbidden, screened := s.resolveCampaignForbiddenPaths(ctx, scope, repo)
	findings, truncated := evaluateCampaignAdmissionScreen(children, forbidden)
	if len(findings) == 0 {
		return nil
	}
	return &campaignAdmissionScreenPayload{
		Advisory:               true,
		Findings:               findings,
		ForbiddenPathsScreened: screened,
		Truncated:              truncated,
	}
}
