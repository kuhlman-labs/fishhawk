// Package basedelta decides whether a PR head that was verified green against
// base B is still independent of the base delta B..B' after main advanced to
// B' (ADR-096 D1(b)-(d) and D3). The decision is a pure function of path
// lists plus the workspaces' Go import graphs: Classify returns Independent
// only when no rule finds an interaction, and every error, empty input or
// unmappable path returns an `undecidable:` reason. Fail closed means verify.
//
// Nothing consumes the package yet (slice 1 of E87); the contract, rule order
// and global-surface rationale live in README.md.
package basedelta

import (
	"context"
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
	PrefixUndecidable    = "undecidable:"
)

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
}

// Classifier applies the ADR-096 independence rules.
type Classifier struct {
	// Loader loads one workspace graph. Required.
	Loader Loader
	// Deadline bounds the whole Classify call, all Loads included; zero
	// means DefaultDeadline.
	Deadline time.Duration
}

// Classify applies, in order: input validation, rule (b) global surface (both
// sides), rule (d) shared artifact, rule (c) reverse-dependency reach. The
// first rule that fires decides; with none firing the verdict is independent.
// A panic anywhere below is recovered into `undecidable:panic`.
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
	delta := sortedUnique(in.DeltaPaths)
	pr := sortedUnique(in.PRPaths)

	// Rule (b): a global-surface path on either side.
	for _, list := range [][]string{delta, pr} {
		for _, p := range list {
			if _, ok := MatchGlobal(p); ok {
				return dependent(PrefixGlobalSurface + p)
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
		entry, ok := sharedDir(d)
		if !ok {
			continue
		}
		for _, p := range pr {
			if strings.HasPrefix(p, entry) {
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
			return undecidable("load:" + err.Error())
		}
		if g == nil {
			return undecidable("load:nil_graph")
		}
		graphs = append(graphs, g)
	}
	g := Union(graphs...)
	deltaSeeds, err := mapPaths(g, delta)
	if err != nil {
		return undecidable(err.Error())
	}
	prSeeds, err := mapPaths(g, pr)
	if err != nil {
		return undecidable(err.Error())
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

func mapPaths(g *Graph, paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		ips, err := g.packagesFor(p)
		if err != nil {
			return nil, err
		}
		out = append(out, ips...)
	}
	return out, nil
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
