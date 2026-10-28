package basedelta

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Package is one workspace Go package as the reach rule sees it.
type Package struct {
	// ImportPath is the package's import path.
	ImportPath string
	// Dir is the package directory, repo-relative and slash-separated ("."
	// for a package at the repository root).
	Dir string
	// ModuleDir is the directory of the module that owns the package,
	// repo-relative and slash-separated ("." for a root module).
	ModuleDir string
	// Imports is the union of the package's imports, test imports and
	// external-test imports, restricted to workspace packages.
	Imports []string
}

// Graph is a workspace import graph, indexed by import path and by directory.
type Graph struct {
	pkgs  map[string]*Package
	byDir map[string][]string
}

// NewGraph builds a Graph from pkgs. A package listed twice under one import
// path has its imports merged.
func NewGraph(pkgs []Package) *Graph {
	g := &Graph{pkgs: map[string]*Package{}, byDir: map[string][]string{}}
	for _, p := range pkgs {
		g.add(p)
	}
	return g
}

func (g *Graph) add(p Package) {
	cur, ok := g.pkgs[p.ImportPath]
	if !ok {
		cp := p
		cp.Imports = append([]string(nil), p.Imports...)
		g.pkgs[p.ImportPath] = &cp
	} else {
		cur.Imports = append(cur.Imports, p.Imports...)
	}
	for _, ip := range g.byDir[p.Dir] {
		if ip == p.ImportPath {
			return
		}
	}
	g.byDir[p.Dir] = append(g.byDir[p.Dir], p.ImportPath)
}

// Len reports the number of packages in g.
func (g *Graph) Len() int {
	if g == nil {
		return 0
	}
	return len(g.pkgs)
}

// Package returns the package with the given import path.
func (g *Graph) Package(importPath string) (Package, bool) {
	p, ok := g.pkgs[importPath]
	if !ok {
		return Package{}, false
	}
	return *p, true
}

// Union merges graphs into one. A nil graph contributes nothing. The union is
// what lets the reach rule see an import edge that exists only in the PR head
// (H) and not in the advanced base (B'), and a package directory that exists
// on only one side.
func Union(graphs ...*Graph) *Graph {
	u := NewGraph(nil)
	for _, g := range graphs {
		if g == nil {
			continue
		}
		for _, ip := range sortedKeys(g.pkgs) {
			u.add(*g.pkgs[ip])
		}
		for dir, ips := range g.byDir {
			for _, ip := range ips {
				if p, ok := g.pkgs[ip]; ok && p.Dir != dir {
					u.add(Package{ImportPath: ip, Dir: dir, ModuleDir: p.ModuleDir})
				}
			}
		}
	}
	return u
}

// reverseClosure returns every package that transitively imports (or
// test-imports) a seed, the seeds included: a breadth-first walk over the
// reversed edges to a fixed point.
func (g *Graph) reverseClosure(seeds []string) map[string]bool {
	rev := map[string][]string{}
	for ip, p := range g.pkgs {
		for _, imp := range p.Imports {
			if _, ok := g.pkgs[imp]; ok {
				rev[imp] = append(rev[imp], ip)
			}
		}
	}
	seen := map[string]bool{}
	queue := []string{}
	for _, s := range seeds {
		if !seen[s] {
			seen[s] = true
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, r := range rev[cur] {
			if !seen[r] {
				seen[r] = true
				queue = append(queue, r)
			}
		}
	}
	return seen
}

// packagesFor maps one repo-relative path to the workspace packages a change
// to it can affect.
//
//   - go.work (at any depth) maps to every package.
//   - a go.mod or go.sum maps to every package of the module rooted in its
//     directory; no such module is an error.
//   - a .go file in a package directory maps to that package.
//   - any other path (a non-Go file, or a .go file outside every package
//     directory) maps to EVERY ancestor directory, its own first, that is a
//     package directory in the graph. An unmatched .go file is an error; an
//     unmatched non-Go file maps to nothing, and the CALLER decides whether
//     the declaration makes it inert or it is unclassified (ADR-124 D3).
func (g *Graph) packagesFor(p string) ([]string, error) {
	base := path.Base(p)
	switch base {
	case "go.work", "go.work.sum":
		return sortedKeys(g.pkgs), nil
	case "go.mod", "go.sum":
		mod := path.Dir(p)
		var out []string
		for _, ip := range sortedKeys(g.pkgs) {
			if g.pkgs[ip].ModuleDir == mod {
				out = append(out, ip)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("unmapped_go_mod:%s", p)
		}
		return out, nil
	}
	dir := path.Dir(p)
	isGo := strings.HasSuffix(p, ".go")
	if isGo {
		if ips := g.byDir[dir]; len(ips) > 0 {
			return append([]string(nil), ips...), nil
		}
	}
	var out []string
	for d := dir; ; d = path.Dir(d) {
		out = append(out, g.byDir[d]...)
		if d == "." || d == "/" {
			break
		}
	}
	if len(out) == 0 && isGo {
		return nil, fmt.Errorf("unmapped_go_path:%s", p)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
