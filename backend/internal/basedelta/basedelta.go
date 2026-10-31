// Package basedelta decides whether a PR head that was verified green against
// base B is still independent of the base delta B..B' after main advanced to
// B' (ADR-096 D1(b)-(d) and D3). The decision is a pure function of path
// lists, the repository's DECLARED verdict-transfer surface (ADR-124) and the
// workspaces' import graphs: Classify returns Independent only when no rule
// finds an interaction, and every error, empty input, undeclared or malformed
// surface, unsupported workspace or unclassified path returns an
// `undecidable:` reason. Fail closed means verify.
//
// The contract and rule order live in README.md.
package basedelta

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Reason values. A dependent or undecidable Reason is one of the prefixes
// followed by the offending path, directory or cause.
const (
	ReasonIndependent    = "independent"
	PrefixGlobalSurface  = "global_surface:"
	PrefixSharedArtifact = "shared_artifact:"
	PrefixReachOverlap   = "reach_overlap:"
	PrefixDocCheck       = "doc_check:"
	PrefixUndecidable    = "undecidable:"
)

// Undecidable sub-reasons the declared surface and the workspace adapter add
// (ADR-124 D2, D3).
const (
	reasonSurfaceUndeclared   = "surface_undeclared"
	reasonSurfaceIncomplete   = "surface_incomplete"
	reasonInvalidSurface      = "invalid_surface"
	reasonUnsupportedLanguage = "unsupported_language"
	reasonUnclassifiedPath    = "unclassified_path"
	reasonOpaquePath          = "opaque_path"
)

// ClassifierVersion versions the rule set and its refusal codes. It is
// recorded beside the declaration's Digest on every decision, replacing the
// built-in surface's SurfaceVersion (ADR-124 D1).
const ClassifierVersion = 2

// MaxReasonLen bounds Result.Reason in bytes.
const MaxReasonLen = 256

// DefaultDeadline bounds one Classify call, every workspace Load included,
// when Classifier.Deadline is zero.
const DefaultDeadline = 5 * time.Minute

// Result is a classification verdict.
type Result struct {
	Independent bool   `json:"independent"`
	Reason      string `json:"reason"`
}

// Input is one classification request. Paths are repo-relative,
// slash-separated and canonical (path.Clean(p) == p).
type Input struct {
	// DeltaPaths are the paths changed by the base delta B..B'.
	DeltaPaths []string
	// PRPaths are the paths changed by the PR, merge-base..H.
	PRPaths []string
	// Workspaces are checkouts whose import graphs are unioned for the reach
	// rule. Pass BOTH B' and the PR head H: B' alone cannot see an import
	// edge the PR itself adds.
	//
	// Callers pass both sides of a rename (git diff --no-renames): the
	// classifier sees only path lists.
	Workspaces []string
	// Surface is the repository's declared verdict-transfer surface,
	// resolved from the run's ADMISSION spec (ResolveSurface). nil is
	// undeclared and refused as surface_undeclared.
	Surface *Surface
}

// Classifier applies the ADR-096 independence rules.
type Classifier struct {
	// Loader loads one workspace graph. Required.
	Loader Loader
	// Deadline bounds the whole Classify call, all Loads included; zero
	// means DefaultDeadline.
	Deadline time.Duration
}

// Classify applies, in order: the surface guards (undeclared, invalid,
// incomplete), input validation, the WORKSPACE adapter check
// (unsupported_language, decided before any path rule), rule (b) verdict-wide
// G on both sides (the workflow spec always included), doc-check on both
// sides, opaque on both sides, rule (d) shared artifact (inert never exempts
// it), then the graph load and per-path classification of both sides (adapter
// graph, else declared inert, else unclassified_path), then rule (c)
// reverse-dependency reach. The first rule that fires decides; with none
// firing the verdict is independent. A panic anywhere below is recovered into
// `undecidable:panic`.
func (c *Classifier) Classify(ctx context.Context, in Input) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = undecidable("panic")
		}
	}()
	if c == nil {
		return undecidable("nil_classifier")
	}
	if c.Loader == nil {
		return undecidable("no_loader")
	}
	if in.Surface == nil {
		return undecidable(reasonSurfaceUndeclared)
	}
	if bad, ok := in.Surface.firstInvalid(); ok {
		return undecidable(reasonInvalidSurface + ":" + bad)
	}
	if in.Surface.incomplete() {
		return undecidable(reasonSurfaceIncomplete)
	}
	if len(in.DeltaPaths) == 0 {
		return undecidable("empty_delta")
	}
	if len(in.PRPaths) == 0 {
		return undecidable("empty_pr_diff")
	}
	if len(in.Workspaces) == 0 {
		return undecidable("no_workspace")
	}
	for _, w := range in.Workspaces {
		if strings.TrimSpace(w) == "" {
			return undecidable("invalid_workspace")
		}
	}
	for _, list := range [][]string{in.DeltaPaths, in.PRPaths} {
		for _, p := range list {
			if !validPath(p) {
				return undecidable("invalid_path:" + p)
			}
		}
	}
	// The workspace adapter: an unsupported workspace refuses whatever
	// changed, before any path rule.
	for _, w := range in.Workspaces {
		if err := c.Loader.Supports(w); err != nil {
			return workspaceRefusal(err)
		}
	}
	delta := sortedUnique(in.DeltaPaths)
	pr := sortedUnique(in.PRPaths)
	both := [][]string{delta, pr}

	// Rule (b): a verdict-wide path on either side.
	for _, list := range both {
		for _, p := range list {
			if in.Surface.matchVerdictWide(p) {
				return dependent(PrefixGlobalSurface + p)
			}
		}
	}
	// Doc-check inputs force a pass on either side until the D-i/D-ii
	// relaxation lands (#4317): a conservative superset (ADR-098 D5.1).
	for _, list := range both {
		for _, p := range list {
			if in.Surface.matchDocCheck(p) {
				return dependent(PrefixDocCheck + p)
			}
		}
	}
	for _, list := range both {
		for _, p := range list {
			if in.Surface.matchOpaque(p) {
				return undecidable(reasonOpaquePath + ":" + p)
			}
		}
	}

	// Rule (d): the same path on both sides, or both sides under one
	// shared-artifact directory.
	prSet := map[string]bool{}
	for _, p := range pr {
		prSet[p] = true
	}
	for _, d := range delta {
		if prSet[d] {
			return dependent(PrefixSharedArtifact + d)
		}
	}
	for _, d := range delta {
		for _, p := range pr {
			if entry, ok := in.Surface.sharedDir(d, p); ok {
				return dependent(PrefixSharedArtifact + commonDir(d, p, entry))
			}
		}
	}

	// Rule (c): reverse-dependency reach over the union of every workspace
	// graph, under ONE deadline covering every Load.
	deadline := c.Deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	graphs := make([]*Graph, 0, len(in.Workspaces))
	for _, w := range in.Workspaces {
		g, err := c.Loader.Load(ctx, w)
		if ctx.Err() != nil {
			return undecidable("deadline_exceeded")
		}
		if err != nil {
			return loadFailure(err)
		}
		if g == nil {
			return undecidable("load:nil_graph")
		}
		graphs = append(graphs, g)
	}
	g := Union(graphs...)
	deltaSeeds, res, ok := classifyPaths(g, in.Surface, delta)
	if !ok {
		return res
	}
	prSeeds, res, ok := classifyPaths(g, in.Surface, pr)
	if !ok {
		return res
	}
	reachD := g.reverseClosure(deltaSeeds)
	reachP := g.reverseClosure(prSeeds)
	var overlap []string
	for ip := range reachD {
		if reachP[ip] {
			overlap = append(overlap, g.pkgs[ip].Dir+"\x00"+ip)
		}
	}
	if len(overlap) > 0 {
		sort.Strings(overlap)
		dir, _, _ := strings.Cut(overlap[0], "\x00")
		return dependent(PrefixReachOverlap + dir)
	}
	return Result{Independent: true, Reason: ReasonIndependent}
}

// classifyPaths maps every path to its reach seeds with fixed precedence
// (ADR-124 D3): adapter graph coverage first (a non-Go path under a package
// directory included), then declared inert, else refused. An unmapped .go or
// go.mod path not declared inert keeps its specific unmapped_go_* reason;
// anything else is unclassified_path. Inert therefore never removes a path
// from graph reach.
func classifyPaths(g *Graph, s *Surface, paths []string) ([]string, Result, bool) {
	var out []string
	for _, p := range paths {
		ips, err := g.packagesFor(p)
		if err == nil && len(ips) > 0 {
			out = append(out, ips...)
			continue
		}
		if s.inert(p) {
			continue
		}
		if err != nil {
			return nil, undecidable(err.Error()), false
		}
		return nil, undecidable(reasonUnclassifiedPath + ":" + p), false
	}
	return out, Result{}, true
}

// workspaceRefusal maps a Loader.Supports error onto its reason.
func workspaceRefusal(err error) Result { return loadFailure(err) }

// loadFailure maps a Loader.Load error onto its reason; the loader's own
// unsupported-workspace refusal (defence in depth) keeps its code.
func loadFailure(err error) Result {
	if errors.Is(err, ErrUnsupportedLanguage) {
		return undecidable(reasonUnsupportedLanguage)
	}
	return undecidable("load:" + err.Error())
}

// validPath accepts a non-empty, relative, slash-separated, canonical path
// that stays inside the repository and carries no control character.
func validPath(p string) bool {
	if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return false
	}
	if path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(p)
}

// commonDir is the longest common directory prefix of a and b, never shorter
// than the shared-artifact entry they both sit under (entry ends in "/").
func commonDir(a, b, entry string) string {
	da := strings.Split(path.Dir(a), "/")
	db := strings.Split(path.Dir(b), "/")
	n := 0
	for n < len(da) && n < len(db) && da[n] == db[n] {
		n++
	}
	common := strings.Join(da[:n], "/")
	floor := strings.TrimSuffix(entry, "/")
	if len(common) < len(floor) {
		return floor
	}
	return common
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func dependent(reason string) Result {
	return Result{Independent: false, Reason: boundReason(reason)}
}

func undecidable(why string) Result {
	return Result{Independent: false, Reason: boundReason(PrefixUndecidable + why)}
}

// boundReason makes a reason single-line and at most MaxReasonLen bytes,
// cutting on a UTF-8 boundary.
func boundReason(r string) string {
	r = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return ' '
		}
		return c
	}, r)
	if len(r) <= MaxReasonLen {
		return r
	}
	cut := MaxReasonLen
	for cut > 0 && !utf8.RuneStart(r[cut]) {
		cut--
	}
	return r[:cut]
}
