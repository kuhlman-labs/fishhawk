package gitlab

// This file implements the two campaign-source capabilities on the gitlab
// provider (#3658): workmgmt.IssueSetDependencyResolver (items / grooming-order
// mode) and workmgmt.EpicChildrenQuerier (epic mode). Both read issues through
// the Free-tier issue-links API and share ONE edge-derivation / out-of-set
// classification pipeline, so the two modes cannot drift. The three GitLab
// mapping decisions (relates_to + Parent epic marker as the child source,
// is_blocked_by as the depends_on source, closed-means-complete) and their
// residuals are documented in README.md.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Compile-time capability assertions: a signature drift is a build failure,
// not a silent 501 from a campaign handler's type assertion.
var (
	_ workmgmt.EpicChildrenQuerier        = (*Provider)(nil)
	_ workmgmt.IssueSetDependencyResolver = (*Provider)(nil)
)

// issueSetFetchConcurrency bounds the in-flight forge reads of one resolution
// (each worker job is a GetIssue, plus a ListIssueLinks for a named item). It
// mirrors the github sibling's bound and is pinned structurally by the
// peak-in-flight probe in campaign_test.go rather than by reading the constant.
const issueSetFetchConcurrency = 8

// parentEpicMarker is the body-marker prefix naming an item's parent epic —
// the same `Parent epic: #N` convention the filing paths write.
const parentEpicMarker = "Parent epic:"

// issueFetch is ONE worker's return value: a pure value struct sent over a
// channel. A worker writes to nothing shared, so the concurrent phases have no
// shared mutable state and the emitted result is independent of completion
// order.
type issueFetch struct {
	ordinal int
	number  int
	issue   *gitlabclient.Issue
	links   []gitlabclient.IssueLink
	err     error
	// aborted marks a fetch the CONTEXT terminated, as distinct from one the
	// forge refused (see fetchIssuesBounded).
	aborted bool
}

// targetState is the classification of one out-of-set is_blocked_by target.
type targetState struct {
	satisfied bool
	reason    workmgmt.DropReason
	state     string
}

// blockedByRef is one is_blocked_by link read from an item, reduced to what
// edge derivation needs. Local is false for a CROSS-PROJECT link: its Number is
// scoped to another project and must never be read as a local issue.
type blockedByRef struct {
	Number  int
	Local   bool
	Display string
	Digest  string
}

// resolveProjectID resolves the target project exactly as File does (the
// conventions gitlab.project override, else the repo owner/name path) and reads
// its numeric id, failing closed with File's actionable wording.
func (p *Provider) resolveProjectID(ctx context.Context, target workmgmt.Target) (int, error) {
	if p.api == nil {
		return 0, errors.New("workmgmt/gitlab: provider missing API client")
	}
	conn := target.GitLab
	if conn == nil {
		return 0, errors.New("workmgmt/gitlab: target gitlab connection required; the conventions must declare a gitlab block")
	}
	projectPath := resolveProjectPath(conn, target.Repo)
	if projectPath == "" {
		return 0, errors.New("workmgmt/gitlab: no target project; set the gitlab.project override or supply a filing repo")
	}
	project, err := p.api.GetProject(ctx, projectPath)
	if err != nil {
		return 0, fmt.Errorf("workmgmt/gitlab: resolve project %q: %w", projectPath, err)
	}
	if project == nil || project.ID <= 0 {
		return 0, fmt.Errorf("workmgmt/gitlab: resolve project %q: no project id returned", projectPath)
	}
	return project.ID, nil
}

// ResolveDependencies implements workmgmt.IssueSetDependencyResolver: resolve
// the depends_on edges over an explicitly-named set of issues (the items /
// grooming-order campaign source).
//
// Each ref is parsed by the SHARED workmgmt.ParseIssueRef (so N / #N / issue:N
// behave exactly as on the github path); a parse failure wraps
// workmgmt.ErrInvalidItemRef so the handler answers 422 rather than 502.
// Repeated refs resolve once.
//
// depends_on edges come from each item's OWN is_blocked_by links (a GitLab
// Premium link type — a Free-tier project yields an edgeless result). The
// resolution runs the github sibling's three-phase shape (#3113): PHASE 1
// fetches every named issue and its links with a bounded pool; PHASE 2 fetches
// the DISTINCT out-of-set targets in first-encounter order with the same pool;
// PHASE 3 classifies SERIALLY in request order. Every phase checks the context
// BEFORE returning a wrapped fetch error and returns
// *workmgmt.IssueSetResolutionTimeout instead.
func (p *Provider) ResolveDependencies(ctx context.Context, req workmgmt.IssueSetRequest) (*workmgmt.EpicChildrenResult, error) {
	projectID, err := p.resolveProjectID(ctx, req.Target)
	if err != nil {
		return nil, err
	}

	numbers := make([]int, 0, len(req.Items))
	inSet := make(map[int]bool, len(req.Items))
	for _, ref := range req.Items {
		n, err := workmgmt.ParseIssueRef(ref)
		if err != nil {
			// Multi-%w: the classification sentinel AND the parse cause both
			// stay reachable (the handler errors.Is the sentinel → 422
			// campaign_item_ref_invalid).
			return nil, fmt.Errorf("workmgmt/gitlab: item %q: %w: %w", ref, workmgmt.ErrInvalidItemRef, err)
		}
		if inSet[n] {
			continue // a duplicate ref resolves once.
		}
		inSet[n] = true
		numbers = append(numbers, n)
	}

	// PHASE 1 — every named issue plus its links, bounded.
	fetches := p.fetchIssuesBounded(ctx, projectID, numbers, true)
	if err := phaseOneError(ctx, projectID, numbers, inSet, fetches); err != nil {
		return nil, err
	}
	return p.buildResult(ctx, projectID, numbers, inSet, fetches)
}

// EpicChildren implements workmgmt.EpicChildrenQuerier over the Free tier: the
// epic is an ordinary issue, and its relates_to links are the CANDIDATE child
// set — the reciprocal of the relates_to link File writes from a child to its
// parent_epic. A GitLab Premium group-epic ref never reaches here: the server
// refuses it (campaign_epic_ref_group_unsupported) before any provider call.
//
// Candidate decisions, in order:
//   - a CROSS-PROJECT relates_to link is excluded WITHOUT any fetch — its iid is
//     scoped to another project and is never reduced to a local number — and is
//     recorded in ExcludedCandidates{Reason: cross_project};
//   - each remaining candidate is fetched (issue + links, bounded pool); one
//     whose body carries a `Parent epic:` marker that does NOT name this epic is
//     excluded (ExcludedCandidates{Reason: foreign_parent_marker});
//   - a candidate with NO marker, or a marker naming THIS epic, is a child. A
//     marker-less hand-added relates_to link is therefore admitted — the stated
//     over-inclusion residual (README.md), backstopped by the campaign
//     admission screen and the operator gate.
//
// Edges over the resolved child set are derived by the SAME pipeline
// ResolveDependencies uses.
func (p *Provider) EpicChildren(ctx context.Context, req workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	projectID, err := p.resolveProjectID(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	epic, err := workmgmt.ParseIssueRef(req.Epic)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: epic %q: %w", req.Epic, err)
	}
	epicLinks, err := p.api.ListIssueLinks(ctx, projectID, epic)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: list links of epic #%d: %w", epic, err)
	}

	var candidates []int
	seen := map[int]bool{}
	var excluded []workmgmt.ExcludedCandidate
	for _, l := range epicLinks {
		if l.LinkType != gitlabclient.LinkTypeRelatesTo {
			continue // blocks / is_blocked_by on the epic are not membership.
		}
		if l.ProjectID != projectID {
			// Cross-project: never reduced to a local iid, never fetched.
			excluded = append(excluded, workmgmt.ExcludedCandidate{
				Number: l.IID, Ref: crossProjectDisplay(l.ProjectID, l.IID), Reason: workmgmt.ExcludeCrossProject,
			})
			continue
		}
		if l.IID <= 0 || l.IID == epic || seen[l.IID] {
			continue
		}
		seen[l.IID] = true
		candidates = append(candidates, l.IID)
	}

	fetches := p.fetchIssuesBounded(ctx, projectID, candidates, true)
	if err := phaseOneError(ctx, projectID, candidates, seen, fetches); err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: epic #%d children: %w", epic, err)
	}

	var numbers []int
	var childFetches []issueFetch
	inSet := map[int]bool{}
	for _, f := range fetches {
		if !parentMarkerAdmits(f.issue.Description, epic) {
			excluded = append(excluded, workmgmt.ExcludedCandidate{
				Number: f.number, Ref: "#" + strconv.Itoa(f.number), Reason: workmgmt.ExcludeForeignParentMarker,
			})
			continue
		}
		f.ordinal = len(childFetches)
		inSet[f.number] = true
		numbers = append(numbers, f.number)
		childFetches = append(childFetches, f)
	}

	res, err := p.buildResult(ctx, projectID, numbers, inSet, childFetches)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/gitlab: epic #%d children: %w", epic, err)
	}
	sort.Slice(excluded, func(i, j int) bool {
		if excluded[i].Number != excluded[j].Number {
			return excluded[i].Number < excluded[j].Number
		}
		return excluded[i].Ref < excluded[j].Ref
	})
	res.ExcludedCandidates = excluded
	return res, nil
}

// parentMarkerAdmits applies the explicit parent-marker decision: a body with
// no `Parent epic:` marker admits the candidate; a body with one or more
// markers admits it only when at least one names THIS epic (N / #N / issue:N,
// tolerating the trailing period some filing paths write). A marker that does
// not parse is, by definition, not a reference to this epic.
func parentMarkerAdmits(body string, epic int) bool {
	sawMarker := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < len(parentEpicMarker) || !strings.EqualFold(trimmed[:len(parentEpicMarker)], parentEpicMarker) {
			continue
		}
		sawMarker = true
		val := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(trimmed[len(parentEpicMarker):]), "."))
		if n, err := workmgmt.ParseIssueRef(val); err == nil && n == epic {
			return true
		}
	}
	return !sawMarker
}

// phaseOneError applies the phase-1 outcome rules: an expired context wins
// (typed timeout, fetch_items), else the first failed fetch IN REQUEST ORDER is
// returned wrapped, and a nil issue with no error fails closed naming the item.
func phaseOneError(ctx context.Context, projectID int, numbers []int, inSet map[int]bool, fetches []issueFetch) error {
	if ctx.Err() != nil {
		return issueSetTimeout(projectID, numbers, inSet, fetches, nil, "fetch_items")
	}
	for _, f := range fetches {
		if f.err != nil {
			return fmt.Errorf("workmgmt/gitlab: get issue #%d: %w", f.number, f.err)
		}
		if f.issue == nil {
			return fmt.Errorf("workmgmt/gitlab: get issue #%d: no issue returned", f.number)
		}
	}
	return nil
}

// buildResult runs phases 2 and 3 over phase-1 fetches that all succeeded.
func (p *Provider) buildResult(ctx context.Context, projectID int, numbers []int, inSet map[int]bool, fetches []issueFetch) (*workmgmt.EpicChildrenResult, error) {
	// PHASE 2 — distinct out-of-set LOCAL targets, first-encounter order,
	// bounded; the cache is built in the parent after the pool drains.
	targets := outOfSetTargets(projectID, inSet, fetches)
	stateCache := map[int]targetState{}
	for _, tf := range p.fetchIssuesBounded(ctx, projectID, targets, false) {
		if tf.aborted {
			// A context-terminated fetch is evidence of nothing: no cache entry.
			continue
		}
		stateCache[tf.number] = classifyFetchedTarget(tf.issue, tf.err)
	}
	if ctx.Err() != nil {
		return nil, issueSetTimeout(projectID, numbers, inSet, fetches, stateCache, "classify_targets")
	}

	// PHASE 3 — serial classification in request order.
	children := make([]workmgmt.EpicChild, 0, len(fetches))
	var edges, dropped []workmgmt.DependsEdge
	var satisfied []workmgmt.SatisfiedEdge
	for _, f := range fetches {
		issue := f.issue
		children = append(children, workmgmt.EpicChild{
			Number:   f.number,
			Title:    issue.Title,
			Autonomy: workmgmt.ParseAutonomyLabel(issue.Labels),
			// GitLab issues carry NO state_reason (the REST payload exposes only
			// state: opened|closed), so closed IS complete here — there is no
			// not_planned close to distinguish. A GitLab-shape fact, not an
			// oversight; see README.md.
			Complete:    strings.EqualFold(issue.State, "closed"),
			State:       normalizeState(issue.State),
			Body:        issue.Description,
			URL:         issue.WebURL,
			NotRunnable: workmgmt.ParseRunnableLabel(issue.Labels),
		})
		for _, dep := range blockedByRefs(projectID, f) {
			if dep.Local && inSet[dep.Number] {
				edges = append(edges, workmgmt.DependsEdge{From: f.number, To: dep.Number})
				continue
			}
			cls, ok := lookupTargetState(dep, stateCache)
			if !ok {
				// The target's phase-2 fetch was context-terminated: return the
				// typed timeout rather than guess a classification.
				return nil, issueSetTimeout(projectID, numbers, inSet, fetches, stateCache, "build_result")
			}
			if cls.satisfied {
				// StateReason is empty: GitLab has no state_reason to carry.
				satisfied = append(satisfied, workmgmt.SatisfiedEdge{From: f.number, To: dep.Number, State: cls.state})
				continue
			}
			e := workmgmt.DependsEdge{From: f.number, To: dep.Number, Reason: cls.reason}
			if !dep.Local {
				// A cross-project target has no local number: render it by its
				// own identity (TargetRef), never as issue:<foreign iid>.
				e.To, e.ToRef, e.ToRefDigest = 0, dep.Display, dep.Digest
			}
			dropped = append(dropped, e)
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Number < children[j].Number })
	sortEdges(edges)
	sortEdges(dropped)
	sort.Slice(satisfied, func(i, j int) bool {
		if satisfied[i].From != satisfied[j].From {
			return satisfied[i].From < satisfied[j].From
		}
		return satisfied[i].To < satisfied[j].To
	})
	return &workmgmt.EpicChildrenResult{Children: children, Edges: edges, DroppedEdges: dropped, SatisfiedEdges: satisfied}, nil
}

// normalizeState maps GitLab's native state onto EpicChild.State's uppercase
// OPEN/CLOSED spelling. An unrecognized state is "" (UNKNOWN), never guessed.
func normalizeState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "opened", "open":
		return "OPEN"
	case "closed":
		return "CLOSED"
	default:
		return ""
	}
}

// blockedByRefs is the SINGLE reader of an item's depends_on edges, shared by
// phase 2 (what to fetch), the timeout accounting and phase 3 so the three
// cannot drift. Only is_blocked_by links are edges (relates_to and blocks are
// not). A link whose project differs from the queried one is CROSS-PROJECT
// (Local=false). Duplicate (From,To) links collapse to one; a self-link is
// dropped.
func blockedByRefs(projectID int, f issueFetch) []blockedByRef {
	var out []blockedByRef
	seen := map[blockedByRef]bool{}
	for _, l := range f.links {
		if l.LinkType != gitlabclient.LinkTypeIsBlockedBy {
			continue
		}
		ref := blockedByRef{Number: l.IID, Local: l.ProjectID == projectID}
		if ref.Local && ref.Number == f.number {
			continue // self-link
		}
		if !ref.Local {
			ref.Display = crossProjectDisplay(l.ProjectID, l.IID)
			ref.Digest = refDigest(ref.Display)
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// crossProjectDisplay renders a foreign-project issue unambiguously.
func crossProjectDisplay(projectID, iid int) string {
	return "project:" + strconv.Itoa(projectID) + "#" + strconv.Itoa(iid)
}

// refDigest is the 16-lowercase-hex identity DependsEdge.TargetRef requires.
func refDigest(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// needsTargetFetch reports whether a ref costs a forge read: LOCAL, positive
// and outside the set. A cross-project ref is never fetched.
func needsTargetFetch(dep blockedByRef, inSet map[int]bool) bool {
	return dep.Local && dep.Number > 0 && !inSet[dep.Number]
}

// outOfSetTargets collects the distinct targets needing a forge read, in
// first-encounter request order.
func outOfSetTargets(projectID int, inSet map[int]bool, fetches []issueFetch) []int {
	var targets []int
	seen := map[int]bool{}
	for _, f := range fetches {
		if f.issue == nil {
			continue
		}
		for _, dep := range blockedByRefs(projectID, f) {
			if !needsTargetFetch(dep, inSet) || seen[dep.Number] {
				continue
			}
			seen[dep.Number] = true
			targets = append(targets, dep.Number)
		}
	}
	return targets
}

// lookupTargetState answers phase 3 from the immutable cache. A cross-project
// or non-positive ref is answered DropTargetStateUnreadable without a cache
// entry (it was never fetched); ok=false means a fetchable target has no entry
// (its fetch was context-terminated).
func lookupTargetState(dep blockedByRef, cache map[int]targetState) (targetState, bool) {
	if !dep.Local || dep.Number <= 0 {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}, true
	}
	ts, ok := cache[dep.Number]
	return ts, ok
}

// classifyFetchedTarget classifies an already-fetched out-of-set target,
// mirroring the github classifier's rules with GitLab's shape: an error or a
// nil issue is DropTargetStateUnreadable (never evidence of satisfaction); a
// closed issue is SATISFIED (no state_reason exists to say otherwise); anything
// else keeps DropNotChild.
func classifyFetchedTarget(issue *gitlabclient.Issue, err error) targetState {
	if err != nil {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}
	}
	if issue == nil {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}
	}
	if strings.EqualFold(issue.State, "closed") {
		return targetState{satisfied: true, state: issue.State}
	}
	return targetState{reason: workmgmt.DropNotChild, state: issue.State}
}

// issueSetTimeout builds the typed deadline error with the github sibling's
// accounting: an item is FULLY RESOLVED when its own fetch completed and every
// out-of-set target it names is classified; SuggestedLimit is the longest
// fully-resolved PREFIX of the request order (0 = no suggestion).
func issueSetTimeout(projectID int, numbers []int, inSet map[int]bool, fetches []issueFetch, cache map[int]targetState, phase string) *workmgmt.IssueSetResolutionTimeout {
	resolved, suggested := 0, 0
	prefixIntact := true
	for _, f := range fetches {
		if itemFullyResolved(projectID, f, inSet, cache) {
			resolved++
			if prefixIntact {
				suggested++
			}
		} else {
			prefixIntact = false
		}
	}
	return &workmgmt.IssueSetResolutionTimeout{Resolved: resolved, Total: len(numbers), SuggestedLimit: suggested, Phase: phase}
}

// itemFullyResolved is the single accounting predicate (see issueSetTimeout).
func itemFullyResolved(projectID int, f issueFetch, inSet map[int]bool, cache map[int]targetState) bool {
	if f.err != nil || f.issue == nil {
		return false
	}
	for _, dep := range blockedByRefs(projectID, f) {
		if !needsTargetFetch(dep, inSet) {
			continue
		}
		if _, ok := cache[dep.Number]; !ok {
			return false
		}
	}
	return true
}

// fetchIssuesBounded fetches numbers with at most issueSetFetchConcurrency
// concurrent workers and returns one issueFetch per input INDEXED BY REQUEST
// ORDER. withLinks adds a ListIssueLinks per issue (named items); out-of-set
// targets are read for state only. Every slot is filled — a call made under an
// expired context returns promptly with a context error — so the accounting
// and the first-in-request-order error selection stay total.
func (p *Provider) fetchIssuesBounded(ctx context.Context, projectID int, numbers []int, withLinks bool) []issueFetch {
	out := make([]issueFetch, len(numbers))
	if len(numbers) == 0 {
		return out
	}
	workers := issueSetFetchConcurrency
	if workers > len(numbers) {
		workers = len(numbers)
	}
	jobs := make(chan int)
	results := make(chan issueFetch, len(numbers))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n := numbers[i]
				issue, err := p.api.GetIssue(ctx, projectID, n)
				var links []gitlabclient.IssueLink
				if err == nil && issue != nil && withLinks {
					links, err = p.api.ListIssueLinks(ctx, projectID, n)
					if err != nil {
						err = fmt.Errorf("list issue links: %w", err)
					}
				}
				results <- issueFetch{
					ordinal: i,
					number:  n,
					issue:   issue,
					links:   links,
					err:     err,
					// Context-terminated iff the call FAILED under a dead context;
					// a success just before the deadline is not aborted.
					aborted: err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil),
				}
			}
		}()
	}
	for i := range numbers {
		jobs <- i
	}
	close(jobs)
	for range numbers {
		r := <-results
		out[r.ordinal] = r
	}
	wg.Wait()
	return out
}

// sortEdges orders depends_on edges by (From, To, ToRefDigest) so a result is
// stable across runs.
func sortEdges(es []workmgmt.DependsEdge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From != es[j].From {
			return es[i].From < es[j].From
		}
		if es[i].To != es[j].To {
			return es[i].To < es[j].To
		}
		return es[i].ToRefDigest < es[j].ToRefDigest
	})
}
