package server

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The campaign preview's advisory screens (#5221).
//
// POST /v0/campaigns/preview reports two hazards beside the assembled DAG,
// before any campaign exists:
//
//   - admission_screen: the SAME #3649 screen create runs (runnable:no and
//     forbidden-path candidates), via screenCampaignAdmission. The preview
//     writes NO campaign_admission_screened audit row; only create does.
//   - path_overlap: pairs of items whose title or body name the same file path
//     and that no in-set depends_on chain orders. Their plans may conflict, so
//     the remedy is a native blocked-by link or a confirmation they touch
//     unrelated parts of the file.
//
// Both blocks are ADVISORY: they never change valid, waves or items, and every
// degrade is fail-open (the forbidden-path class is skipped exactly as on
// create). attachPreviewScreens is the ONLY place either field is set, and the
// handler calls it on the valid, dangling and cycle report paths alike.
//
// path_overlap is LEXICAL over issue prose. It reuses extractCriterionPathTokens
// (the #3649 screen's token grammar) and then drops prose tokens through
// previewOverlapPathToken. Known false positives: a path named only as an
// example, a bare file name such as `client.go` shared by different
// directories, three-segment slash prose (read/write/delete), and a backticked
// dotted identifier whose last member is a non-ambiguous allowlisted extension
// (`cfg.YAML`, `resp.JSON`). Known false negatives: a path the plan will touch
// that the issue never names, directory-prefix overlap, a two-segment
// directory (docs/adr), an extensionless file (Makefile), a root file with an
// ambiguous extension written without a directory (go.mod, yarn.lock), and a
// path whose first segment carries a non-leading dot (it reads as a Go import
// host such as github.com).
const (
	// previewOverlapMinDirSlashes is the minimum number of '/' an
	// extensionless token needs to count as a directory path: three segments,
	// so one-slash prose (and/or, owner/repo) never qualifies.
	previewOverlapMinDirSlashes = 2

	// previewPathOverlapMaxPairs caps the reported pairs; total_pairs carries
	// the uncapped count.
	previewPathOverlapMaxPairs = 50

	// previewPathOverlapMaxPathsPerPair caps the paths listed in one pair, so
	// two issues that paste the same file listing cannot inflate every pair of
	// the block. The class, rank and overlap decision use the full shared set;
	// only the listed paths are capped, and paths_truncated says so.
	previewPathOverlapMaxPathsPerPair = 10

	previewOverlapClassCode     = "code"
	previewOverlapClassDocsOnly = "docs_only"

	previewOverlapRemedyCode = "both items name this code path and no depends_on orders them, so their plans may conflict: " +
		"add a native blocked-by link so the later item plans after the earlier one's PR opens, " +
		"or confirm they touch unrelated parts of the file; re-preview after the tracker edit"
	previewOverlapRemedyDocsOnly = "only documentation paths are shared: parallel plans are fine and the merges serialize " +
		"through the merge overlap guard. docs/api/v0.openapi.yaml, docs/spec, docs/adr, docs/review, " +
		"docs/compliance and instruction files count as code"
)

// previewOverlapFileExtensions is the repo-agnostic allowlist of file
// extensions (lowercased, without the dot) that make a token a file path. The
// empty string is never a member, so an extensionless token falls through to
// the directory rule.
var previewOverlapFileExtensions = map[string]bool{
	"go": true, "md": true, "mdx": true, "markdown": true, "rst": true, "adoc": true, "txt": true,
	"yaml": true, "yml": true, "json": true, "toml": true, "ini": true, "cfg": true, "conf": true,
	"env": true, "xml": true, "html": true, "css": true, "scss": true, "ts": true, "tsx": true,
	"js": true, "jsx": true, "mjs": true, "cjs": true, "vue": true, "svelte": true, "py": true,
	"rb": true, "rs": true, "java": true, "kt": true, "swift": true, "c": true, "h": true,
	"cc": true, "cpp": true, "hpp": true, "cs": true, "php": true, "sh": true, "bash": true,
	"zsh": true, "sql": true, "proto": true, "graphql": true, "gql": true, "tf": true,
	"hcl": true, "mod": true, "sum": true, "lock": true, "gradle": true, "csv": true,
}

// previewOverlapSlashOnlyExtensions are the allowlisted extensions that are
// also common dotted-identifier members (s.cfg, process.env, x.h, app.conf). A
// token with one of them counts only when it carries a '/'.
var previewOverlapSlashOnlyExtensions = map[string]bool{
	"cfg": true, "env": true, "conf": true, "h": true, "c": true,
	"cs": true, "sh": true, "lock": true, "sum": true, "mod": true,
}

// previewOverlapPathToken reports whether an extractCriterionPathTokens token
// names a repository file or directory rather than prose. It rejects globs,
// absolute and parent-escaping paths, Go import paths (a first segment with a
// non-leading dot, such as github.com), and ambiguous short extensions
// without a '/'. A token with an allowlisted extension is a file; any other
// token is a directory only with previewOverlapMinDirSlashes or more slashes.
func previewOverlapPathToken(tok string) bool {
	if strings.Contains(tok, "*") {
		return false
	}
	p := path.Clean(toSlashPath(tok))
	if path.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	if i := strings.IndexByte(p, '/'); i > 0 && strings.IndexByte(p[:i], '.') > 0 {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	if previewOverlapSlashOnlyExtensions[ext] && !strings.Contains(p, "/") {
		return false
	}
	if previewOverlapFileExtensions[ext] {
		return true
	}
	return strings.Count(p, "/") >= previewOverlapMinDirSlashes
}

// campaignPreviewPathOverlap is the preview's advisory path_overlap block.
// Mirrors docs/api/v0.openapi.yaml's CampaignPreviewPathOverlap.
type campaignPreviewPathOverlap struct {
	// Advisory is always true: the block never changes valid or the waves.
	Advisory bool                             `json:"advisory"`
	Pairs    []campaignPreviewPathOverlapPair `json:"pairs"`
	// TotalPairs is the uncapped pair count.
	TotalPairs int `json:"total_pairs"`
	// Truncated is true when more than previewPathOverlapMaxPairs pairs were
	// found and the list was capped.
	Truncated bool `json:"truncated,omitempty"`
}

// campaignPreviewPathOverlapPair is one unordered item pair sharing a path.
// Mirrors docs/api/v0.openapi.yaml's CampaignPreviewPathOverlapPair.
type campaignPreviewPathOverlapPair struct {
	// Items are the two issue refs, lower number first.
	Items []string `json:"items"`
	// Paths are the shared, filtered, cleaned path tokens, sorted, capped at
	// previewPathOverlapMaxPathsPerPair.
	Paths []string `json:"paths"`
	// PathsTruncated is true when the pair shared more than
	// previewPathOverlapMaxPathsPerPair paths and Paths lists only the first
	// sorted ones.
	PathsTruncated bool `json:"paths_truncated,omitempty"`
	// Class is docs_only when every shared path is documentation
	// (isDocumentationPath), otherwise code.
	Class  string `json:"class"`
	Remedy string `json:"remedy"`
}

// evaluatePreviewPathOverlap is the PURE path_overlap evaluator. It returns
// nil when no unordered pair shares a path.
//
// Per child it takes the filtered path tokens of title ∪ body. A pair (a, b)
// with a < b is reported when it shares at least one path and no in-set
// depends_on chain orders it in either direction. Pairs are ranked by the
// fanout of their rarest shared path (the number of children naming it), so a
// hub path named by many items sorts last and cannot crowd specific overlaps
// out of the previewPathOverlapMaxPairs cap; ties fall to code before
// docs_only, then (a, b).
func evaluatePreviewPathOverlap(children []workmgmt.EpicChild, edges []workmgmt.DependsEdge) *campaignPreviewPathOverlap {
	paths := make(map[int]map[string]bool, len(children))
	fanout := make(map[string]int)
	inSet := make(map[int]bool, len(children))
	nums := make([]int, 0, len(children))
	for _, c := range children {
		if inSet[c.Number] {
			continue // a repeated child never pairs with itself
		}
		inSet[c.Number] = true
		nums = append(nums, c.Number)
		kept := make(map[string]bool)
		for _, text := range []string{c.Title, c.Body} {
			for _, tok := range extractCriterionPathTokens(text) {
				if !previewOverlapPathToken(tok) {
					continue
				}
				kept[path.Clean(toSlashPath(tok))] = true
			}
		}
		paths[c.Number] = kept
		for p := range kept {
			fanout[p]++
		}
	}
	sort.Ints(nums)

	deps := make(map[int][]int)
	for _, e := range edges {
		if inSet[e.From] && inSet[e.To] {
			deps[e.From] = append(deps[e.From], e.To)
		}
	}

	type rankedPair struct {
		pair   campaignPreviewPathOverlapPair
		fanout int
		a, b   int
	}
	var ranked []rankedPair
	for i, a := range nums {
		for _, b := range nums[i+1:] {
			var shared []string
			for p := range paths[a] {
				if paths[b][p] {
					shared = append(shared, p)
				}
			}
			if len(shared) == 0 {
				continue
			}
			if previewReaches(deps, a, b) || previewReaches(deps, b, a) {
				continue
			}
			sort.Strings(shared)
			class, remedy := previewOverlapClassCode, previewOverlapRemedyCode
			if previewPathsDocsOnly(shared) {
				class, remedy = previewOverlapClassDocsOnly, previewOverlapRemedyDocsOnly
			}
			minFanout := fanout[shared[0]]
			for _, p := range shared[1:] {
				if fanout[p] < minFanout {
					minFanout = fanout[p]
				}
			}
			listed, pathsTruncated := shared, false
			if len(listed) > previewPathOverlapMaxPathsPerPair {
				listed, pathsTruncated = listed[:previewPathOverlapMaxPathsPerPair], true
			}
			ranked = append(ranked, rankedPair{
				pair: campaignPreviewPathOverlapPair{
					Items:          []string{"issue:" + strconv.Itoa(a), "issue:" + strconv.Itoa(b)},
					Paths:          listed,
					PathsTruncated: pathsTruncated,
					Class:          class,
					Remedy:         remedy,
				},
				fanout: minFanout,
				a:      a,
				b:      b,
			})
		}
	}
	if len(ranked) == 0 {
		return nil
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].fanout != ranked[j].fanout {
			return ranked[i].fanout < ranked[j].fanout
		}
		if ranked[i].pair.Class != ranked[j].pair.Class {
			return ranked[i].pair.Class == previewOverlapClassCode
		}
		if ranked[i].a != ranked[j].a {
			return ranked[i].a < ranked[j].a
		}
		return ranked[i].b < ranked[j].b
	})
	out := &campaignPreviewPathOverlap{Advisory: true, TotalPairs: len(ranked)}
	if len(ranked) > previewPathOverlapMaxPairs {
		ranked = ranked[:previewPathOverlapMaxPairs]
		out.Truncated = true
	}
	out.Pairs = make([]campaignPreviewPathOverlapPair, 0, len(ranked))
	for _, r := range ranked {
		out.Pairs = append(out.Pairs, r.pair)
	}
	return out
}

// previewReaches reports whether to is reachable from from along deps (an
// edge From->To means From depends on To). A visited set makes it terminate
// on a cycle.
func previewReaches(deps map[int][]int, from, to int) bool {
	visited := map[int]bool{from: true}
	queue := []int{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range deps[cur] {
			if next == to {
				return true
			}
			if !visited[next] {
				visited[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// previewPathsDocsOnly reports whether every path is documentation under the
// merge guard's conservative isDocumentationPath (no declared review paths).
func previewPathsDocsOnly(paths []string) bool {
	for _, p := range paths {
		if !isDocumentationPath(p, nil) {
			return false
		}
	}
	return true
}

// attachPreviewScreens sets the preview report's two advisory blocks. It is
// the ONLY place either field is set; each is nil (and omitted) when its
// screen finds nothing. It writes no audit row.
func (s *Server) attachPreviewScreens(ctx context.Context, resp *campaignPreviewResponse, source *campaignSourceResolution, owner, name string) {
	resp.AdmissionScreen = s.screenCampaignAdmission(ctx, source.Scope, forge.RepoRef{Owner: owner, Name: name}, source.Result.Children)
	resp.PathOverlap = evaluatePreviewPathOverlap(source.Result.Children, source.Result.Edges)
}
