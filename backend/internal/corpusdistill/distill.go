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
	if opts.CaseName == "" {
		return "", fmt.Errorf("corpusdistill: CaseName is required")
	}
	if opts.Issue == "" {
		return "", fmt.Errorf("corpusdistill: Issue is required")
	}
	if opts.OutDir == "" {
		return "", fmt.Errorf("corpusdistill: OutDir is required")
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("corpusdistill: read bundle: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("corpusdistill: empty bundle input")
	}

	gzBytes, plainBytes, err := normalize(raw)
	if err != nil {
		return "", err
	}

	// bundle.ReadEvents requires gzipped input (it calls gzip.NewReader
	// first and returns ErrBadGzip on a plain frame), so we always hand it
	// the gzipped form.
	lines, err := bundle.ReadEvents(gzBytes)
	if err != nil {
		return "", fmt.Errorf("corpusdistill: parse bundle: %w", err)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("corpusdistill: bundle contained no events")
	}

	card := agenteval.Score(lines)

	caseDir := filepath.Join(opts.OutDir, opts.CaseName)
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

	// expected.json: marshal the scorecard with the corpus's 2-space
	// indent + a trailing newline so a fresh re-score byte-matches.
	expected, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return "", fmt.Errorf("corpusdistill: marshal scorecard: %w", err)
	}
	expected = append(expected, '\n')

	files := []struct {
		name string
		data []byte
	}{
		{"trace.jsonl", plainBytes},
		{"expected.json", expected},
		{"case.md", []byte(renderCaseMD(opts, card))},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(caseDir, f.name), f.data, 0o644); err != nil {
			return "", fmt.Errorf("corpusdistill: write %s: %w", f.name, err)
		}
	}

	return caseDir, nil
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

// renderCaseMD produces the case.md template. It carries a
// "Provenance: PRODUCTION" marker, states the bundle was sourced from the
// REDACTED variant, references the originating issue/run, records the
// derived outcome, and leaves TODO prompts for the operator's curated
// distilled-signal narrative (#819).
func renderCaseMD(opts Options, card agenteval.Scorecard) string {
	return fmt.Sprintf(`# Case: %s

**Provenance: PRODUCTION.** This trace was captured from a real Fishhawk
production run (%s), not hand-authored. It was sourced from the REDACTED
trace-bundle variant (the only variant GET /v0/stages/{stage_id}/trace
serves; the raw variant is object-locked and unredacted), so it carries no
unredacted secrets.

Scaffolded by `+"`fishhawk-distill-corpus`"+` (#1290). The scorecard below
was derived deterministically by agenteval.Score; the sections marked TODO
are operator curation and must be completed before this case lands (#819).

## What it represents

Derived outcome: `+"`%s`"+`.

TODO(operator): describe the trajectory this trace represents — what the
agent was asked to do, what it did, and why this case is worth keeping in
the corpus (which scoring branch or failure mode it exercises).

## Distilled signal

TODO(operator): state the distilled signal — the specific scorer behaviour
this case pins (the branch in deriveOutcome / evidence_before_edit /
out_of_tree_writes / scope_drift_paths it covers) and how it contrasts with
sibling cases. Add human_labels.json alongside this file once the editorial
labels are agreed.
`, opts.CaseName, opts.Issue, card.Outcome)
}
