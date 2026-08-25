// Command fishhawk-docgen regenerates the four generated Reference pages
// of the documentation site from the canonical sources (the workflow-spec
// and plan JSON Schemas, the OpenAPI document, and the cli/internal/cmdinfo
// inventory). It splices each rendering into its page between HTML marker
// comments, leaving the orientation prose outside the markers untouched.
//
//	fishhawk-docgen              # rewrite the pages in place
//	fishhawk-docgen --check      # report drift, write nothing (exit 1 on drift)
//	fishhawk-docgen --root DIR   # repo root (default: discovered from cwd)
//
// scripts/gen-site-reference wraps the write mode; the Go drift test
// (cli/internal/docgen/drift_test.go) and scripts/test-site-reference are
// the gates.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/cli/internal/docgen"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk-docgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "repository root (default: discovered from the working directory)")
	check := fs.Bool("check", false, "report drift and write nothing; exit 1 if any page is out of date")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	r := *root
	if r == "" {
		found, err := findRepoRoot()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk-docgen: %v\n", err)
			return 1
		}
		r = found
	}

	var drift []string
	for _, id := range docgen.PageIDs {
		if *check {
			rel, _ := docgen.PageFile(id)
			next, err := docgen.RegenerateFile(r, id)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "fishhawk-docgen: %s: %v\n", id, err)
				return 1
			}
			cur, err := os.ReadFile(filepath.Join(r, rel))
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "fishhawk-docgen: %s: %v\n", id, err)
				return 1
			}
			if string(cur) != next {
				drift = append(drift, rel)
			}
			continue
		}
		changed, err := docgen.WriteFile(r, id)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk-docgen: %s: %v\n", id, err)
			return 1
		}
		rel, _ := docgen.PageFile(id)
		if changed {
			_, _ = fmt.Fprintf(stdout, "regenerated %s\n", rel)
		} else {
			_, _ = fmt.Fprintf(stdout, "unchanged %s\n", rel)
		}
	}

	if *check {
		if len(drift) > 0 {
			_, _ = fmt.Fprintln(stderr, "fishhawk-docgen: the following pages are out of date; run scripts/gen-site-reference:")
			for _, d := range drift {
				_, _ = fmt.Fprintf(stderr, "  %s\n", d)
			}
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "all generated reference pages are up to date")
	}
	return 0
}

// findRepoRoot walks up from the working directory to the directory
// holding go.work (the workspace root).
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate repo root (no go.work found above the working directory)")
		}
		dir = parent
	}
}
