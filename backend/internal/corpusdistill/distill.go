// Package corpusdistill scaffolds an agent-eval corpus case
// (trace.jsonl + expected.json + case.md) from a stored trace bundle.
//
// It automates the mechanical half of the #819 corpus buildout: given a
// trace bundle (the redacted *.jsonl.gz variant served by GET
// /v0/stages/{stage_id}/trace, or an equivalent plain .jsonl), it parses
// the events, scores them with the deterministic Tier-A scorer, and writes
// the three-file case directory the agent-eval replay test
// (backend/internal/agenteval TestScore) expects. Case SELECTION and the
// distilled-signal narrative remain operator curation (#819) — the helper
// produces a replay-valid scaffold with TODO prompts, not a finished case.
//
// The package is pure and offline: Distill takes an io.Reader and touches
// only the filesystem under the supplied OutDir. The optional --stage-id
// fetch convenience lives in fetch.go so it can be exercised against an
// httptest server without a real backend.
package corpusdistill

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
)

// Options configures a Distill call.
type Options struct {
	// CaseName is the corpus case slug; it becomes the case directory
	// name under OutDir. Required.
	CaseName string
	// Issue is the originating issue/run reference recorded in case.md
	// (e.g. "#819" or a run id). Required for provenance.
	Issue string
	// OutDir is the corpus parent directory; the case is written to
	// OutDir/CaseName. Required (the command layer resolves a default).
	OutDir string
	// Force permits overwriting an existing case directory. Without it,
	// Distill fails closed when OutDir/CaseName already exists.
	Force bool
	// Fetched reports that the bundle came from FetchStageTrace (the
	// --stage-id path), which GETs the redacted-only /v0/stages/{id}/trace.
	// Only then can case.md assert the PRODUCTION + REDACTED provenance; for
	// operator-supplied bundles (--in/stdin) the origin is unverifiable, so
	// the provenance line is left as a TODO prompt for the operator.
	Fetched bool
	// Signal is the optional operator-supplied scorecard signal/classification
	// this case demonstrates (e.g. "loop_detected", "scope_drift",
	// "healthy_cross_boundary"). When empty, renderCaseMD falls back to the
	// TODO(operator) prompt for the distilled-signal section (no regression).
	Signal string
	// Narrative is the optional operator-supplied distilled-signal explanation:
	// the human description of what trajectory the trace represents and why the
	// case is worth keeping. When empty, renderCaseMD falls back to the
	// TODO(operator) prompts for the "What it represents"/"Distilled signal"
	// sections (no regression).
	Narrative string
}

// Result describes the would-be corpus-case artifacts a Distill/Preview call
// produces. Preview returns it without writing anything; Distill computes the
// same Result internally and then writes TraceJSONL/ExpectedJSON/CaseMD to
// CaseDir. It lets the --dry-run path report the resolved case dir, derived
// outcome, expected.json, and case.md content the operator would get.
type Result struct {
	// CaseDir is the resolved OutDir/CaseName path the case would be written to.
	CaseDir string
	// TraceJSONL is the plain-JSONL trace.jsonl content.
	TraceJSONL []byte
	// ExpectedJSON is the marshalled scorecard (expected.json) content.
	ExpectedJSON []byte
	// CaseMD is the rendered case.md content.
	CaseMD string
	// Card is the derived deterministic scorecard.
	Card agenteval.Scorecard
}

// Distill reads a trace bundle from r, scores it, and writes a corpus case
// directory at OutDir/CaseName containing trace.jsonl (plain JSONL),
// expected.json (the marshalled scorecard), and case.md (a provenance +
// distilled-signal template). It returns the written case directory path.
//
// The bundle may arrive gzipped (the *.jsonl.gz wire form, detected by the
// RFC 1952 gzip magic bytes 0x1f 0x8b) or as plain JSONL; Distill
// normalises to both forms because bundle.ReadEvents consumes gzipped
// bytes while trace.jsonl must be written plain.
func Distill(r io.Reader, opts Options) (string, error) {
	res, err := prepare(r, opts)
	if err != nil {
		return "", err
	}

	caseDir := res.CaseDir
	if _, statErr := os.Stat(caseDir); statErr == nil {
		if !opts.Force {
			return "", fmt.Errorf("corpusdistill: case dir %q already exists; pass --force to overwrite", caseDir)
		}
		if rmErr := os.RemoveAll(caseDir); rmErr != nil {
			return "", fmt.Errorf("corpusdistill: remove existing case dir %q: %w", caseDir, rmErr)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("corpusdistill: stat case dir %q: %w", caseDir, statErr)
	}

	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return "", fmt.Errorf("corpusdistill: create case dir %q: %w", caseDir, err)
	}

	files := []struct {
		name string
		data []byte
	}{
		{"trace.jsonl", res.TraceJSONL},
		{"expected.json", res.ExpectedJSON},
		{"case.md", []byte(res.CaseMD)},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(caseDir, f.name), f.data, 0o644); err != nil {
			return "", fmt.Errorf("corpusdistill: write %s: %w", f.name, err)
		}
	}

	return caseDir, nil
}

// Preview parses and scores the bundle from r exactly as Distill does and
// returns the would-be case artifacts as a Result, but writes NOTHING to the
// filesystem (no Stat, MkdirAll, or WriteFile). It backs the command's
// --dry-run path: the operator can evaluate the derived scorecard, case dir,
// and rendered case.md before committing the case to disk. It surfaces the
// same validation/parse errors as Distill (empty bundle, corrupt gzip, unsafe
// CaseName, missing required field) so a genuine error is reported, not masked.
func Preview(r io.Reader, opts Options) (Result, error) {
	return prepare(r, opts)
}

// prepare performs all the read/normalize/parse/validate/score/render work
// shared by Distill and Preview and computes the resolved CaseDir, but does NO
// filesystem writes and does NOT perform the overwrite stat-check. It returns
// the would-be case artifacts; Distill layers the overwrite guard + writes on
// top, while Preview returns the Result verbatim.
func prepare(r io.Reader, opts Options) (Result, error) {
	if opts.CaseName == "" {
		return Result{}, fmt.Errorf("corpusdistill: CaseName is required")
	}
	if err := validateCaseName(opts.CaseName); err != nil {
		return Result{}, err
	}
	if opts.Issue == "" {
		return Result{}, fmt.Errorf("corpusdistill: Issue is required")
	}
	if opts.OutDir == "" {
		return Result{}, fmt.Errorf("corpusdistill: OutDir is required")
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return Result{}, fmt.Errorf("corpusdistill: read bundle: %w", err)
	}
	if len(raw) == 0 {
		return Result{}, fmt.Errorf("corpusdistill: empty bundle input")
	}

	gzBytes, plainBytes, err := normalize(raw)
	if err != nil {
		return Result{}, err
	}

	// bundle.ReadEvents requires gzipped input (it calls gzip.NewReader
	// first and returns ErrBadGzip on a plain frame), so we always hand it
	// the gzipped form.
	lines, err := bundle.ReadEvents(gzBytes)
	if err != nil {
		return Result{}, fmt.Errorf("corpusdistill: parse bundle: %w", err)
	}
	if len(lines) == 0 {
		return Result{}, fmt.Errorf("corpusdistill: bundle contained no events")
	}

	card := agenteval.Score(lines)

	// expected.json: marshal the scorecard with the corpus's 2-space
	// indent + a trailing newline so a fresh re-score byte-matches.
	expected, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("corpusdistill: marshal scorecard: %w", err)
	}
	expected = append(expected, '\n')

	return Result{
		CaseDir:      filepath.Join(opts.OutDir, opts.CaseName),
		TraceJSONL:   plainBytes,
		ExpectedJSON: expected,
		CaseMD:       renderCaseMD(opts, card),
		Card:         card,
	}, nil
}

// validateCaseName constrains CaseName to a single, safe path element.
// CaseName is untrusted input that is joined onto OutDir and, with Force,
// passed to os.RemoveAll — so a value like "../outside", "sub/dir", or an
// absolute path could write or delete outside the corpus tree. Rejecting
// anything that is not one clean, separator-free, non-".."/"."  element
// keeps every filesystem effect inside OutDir.
func validateCaseName(name string) error {
	if filepath.IsAbs(name) ||
		strings.ContainsRune(name, '/') ||
		strings.ContainsRune(name, filepath.Separator) ||
		name == "." || name == ".." ||
		name != filepath.Clean(name) {
		return fmt.Errorf("corpusdistill: CaseName %q must be a single path element "+
			"(no path separators, no '.'/'..', not absolute)", name)
	}
	return nil
}

// normalize returns both a gzipped and a plain copy of raw, auto-detecting
// which form raw arrived in by the RFC 1952 gzip magic bytes (0x1f 0x8b).
func normalize(raw []byte) (gzBytes, plainBytes []byte, err error) {
	if isGzip(raw) {
		plain, derr := gunzip(raw)
		if derr != nil {
			return nil, nil, fmt.Errorf("corpusdistill: decompress bundle: %w", derr)
		}
		return raw, plain, nil
	}
	gz, gerr := gzipBytes(raw)
	if gerr != nil {
		return nil, nil, fmt.Errorf("corpusdistill: recompress bundle: %w", gerr)
	}
	return gz, raw, nil
}

// isGzip reports whether b begins with the gzip member magic
// (RFC 1952 §2.3.1: ID1=0x1f, ID2=0x8b).
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderCaseMD produces the case.md template. It references the originating
// issue/run, records the derived outcome, and — when the operator supplies the
// inline-labeling flags (opts.Signal/opts.Narrative, #1291) — pre-fills the
// "What it represents" / "Distilled signal" sections with that text. When a
// label flag is empty it falls back to the EXACT same TODO(operator) prompt the
// #1290 scaffold emits, so omitting the flags is a byte-for-byte no-regression.
//
// The provenance block is source-dependent: the tool can only assert the
// PRODUCTION + REDACTED-variant provenance for the --stage-id fetch path
// (opts.Fetched), which GETs the redacted-only /v0/stages/{id}/trace. For
// operator-supplied bundles (--in/stdin) the origin and redaction status are
// unverifiable, so the line is emitted as a TODO the operator must resolve
// before the case lands — rather than mislabelling a possibly raw or
// hand-authored bundle as PRODUCTION+REDACTED.
func renderCaseMD(opts Options, card agenteval.Scorecard) string {
	return fmt.Sprintf(`# Case: %s

%s

Scaffolded by `+"`fishhawk-distill-corpus`"+` (#1290). The scorecard below
was derived deterministically by agenteval.Score; the sections marked TODO
are operator curation and must be completed before this case lands (#819).

## What it represents

Derived outcome: `+"`%s`"+`.

%s

## Distilled signal

%s
`, opts.CaseName, provenanceBlock(opts), card.Outcome,
		representsSection(opts), distilledSignalSection(opts))
}

// representsSection renders the "What it represents" body: the operator's
// Narrative when supplied (#1291), else the EXACT #1290 TODO(operator) prompt.
func representsSection(opts Options) string {
	if opts.Narrative != "" {
		return opts.Narrative
	}
	return `TODO(operator): describe the trajectory this trace represents — what the
agent was asked to do, what it did, and why this case is worth keeping in
the corpus (which scoring branch or failure mode it exercises).`
}

// distilledSignalSection renders the "Distilled signal" body: the operator's
// Signal + Narrative when supplied (#1291), else the EXACT #1290
// TODO(operator) prompt.
func distilledSignalSection(opts Options) string {
	if opts.Signal != "" || opts.Narrative != "" {
		var b strings.Builder
		if opts.Signal != "" {
			fmt.Fprintf(&b, "Signal: `%s`.", opts.Signal)
		}
		if opts.Narrative != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(opts.Narrative)
		}
		return b.String()
	}
	return `TODO(operator): state the distilled signal — the specific scorer behaviour
this case pins (the branch in deriveOutcome / evidence_before_edit /
out_of_tree_writes / scope_drift_paths it covers) and how it contrasts with
sibling cases. Add human_labels.json alongside this file once the editorial
labels are agreed.`
}

// provenanceBlock returns the case.md provenance paragraph. For the
// --stage-id fetch path it asserts the PRODUCTION + REDACTED-variant origin
// the fetch guarantees; for operator-supplied bundles (--in/stdin) it emits a
// TODO the operator must resolve, since the tool cannot vouch for an
// arbitrary bundle's origin or redaction status.
func provenanceBlock(opts Options) string {
	if opts.Fetched {
		return fmt.Sprintf(`**Provenance: PRODUCTION.** This trace was captured from a real Fishhawk
production run (%s), not hand-authored. It was sourced from the REDACTED
trace-bundle variant (the only variant GET /v0/stages/{stage_id}/trace
serves; the raw variant is object-locked and unredacted), so it carries no
unredacted secrets.`, opts.Issue)
	}
	return fmt.Sprintf(`**Provenance: TODO(operator).** This case was scaffolded from an
operator-supplied bundle (%s) via `+"`--in`/stdin"+`, so the tool cannot
assert its origin or redaction status. If this trace is a real production
run's REDACTED bundle, replace this line with "Provenance: PRODUCTION" and
note the redacted-variant source; if it is raw or hand-authored, state that
instead. Confirm no unredacted secrets remain before this case lands.`, opts.Issue)
}
