// Package corpusdistill scaffolds an agent-eval corpus case
// (trace.jsonl + expected.json + case.md) from a stored trace bundle,
// automating the mechanical half of #819. The seed corpus committed
// under backend/internal/agenteval/testdata/corpus is replayed by
// agenteval.TestScore; this helper turns a captured production bundle
// into a ready-to-curate case directory in that shape.
//
// The core is PURE and OFFLINE: Distill takes bundle bytes (gzipped or
// plain — it auto-detects) and writes three files. No network code
// lives here; the standalone command
// (backend/cmd/fishhawk-distill-corpus) adds the convenience path that
// fetches a bundle over the GET /v0/stages/{stage_id}/trace endpoint
// and feeds the bytes to this same core, so the offline path stays
// fully unit-testable.
//
// Case SELECTION and the distilled-signal narrative remain operator
// curation (#819): the generated case.md is a fill-in template carrying
// a Provenance: PRODUCTION (redacted) marker and the originating-issue
// reference, with TODO prompts for the human's analysis.
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

// Options configures a single Distill call.
type Options struct {
	// CaseName is the corpus case directory name (the leaf under
	// OutDir). Required.
	CaseName string
	// Issue is the originating GitHub issue reference (e.g. "#1290" or
	// a URL) recorded in case.md's provenance block. Optional but
	// recommended; an empty value renders a TODO marker instead.
	Issue string
	// OutDir is the parent directory the case directory is created
	// under. Required (the command resolves the default and fails loud
	// when the corpus parent is unreachable, so the core never guesses
	// a cwd-relative path — condition 2).
	OutDir string
	// Force overwrites an existing case directory's three files in
	// place. Without it, an existing case directory is an error (the
	// overwrite guard).
	Force bool
}

// gzipMagic is the two-byte prefix every gzip member begins with
// (RFC 1952 §2.3.1). Distill inspects these bytes to decide whether
// the input arrived gzipped or as plain JSONL.
var gzipMagic = [2]byte{0x1f, 0x8b}

// Distill scaffolds a corpus case from one trace bundle's bytes.
//
// input may be gzipped (the wire/at-rest form the trace endpoint and
// tracestore use) OR plain JSONL — it is auto-detected by the gzip
// magic bytes. The bundle is parsed via bundle.ReadEvents (which
// consumes GZIPPED bytes) and scored via agenteval.Score; the three
// case files are written into filepath.Join(opts.OutDir, opts.CaseName):
//
//   - trace.jsonl   the PLAIN JSONL trajectory (replay-readable)
//   - expected.json the serialized agenteval.Scorecard
//   - case.md       a provenance-stamped fill-in template
//
// It returns an error (and leaves any existing case directory
// untouched) when the case directory already exists and opts.Force is
// not set.
func Distill(input []byte, opts Options) error {
	if opts.CaseName == "" {
		return fmt.Errorf("corpusdistill: case name is required")
	}
	if opts.OutDir == "" {
		return fmt.Errorf("corpusdistill: out dir is required")
	}

	plainBytes, gzBytes, err := normalize(input)
	if err != nil {
		return err
	}

	lines, err := bundle.ReadEvents(gzBytes)
	if err != nil {
		return fmt.Errorf("corpusdistill: parse trace bundle: %w", err)
	}
	card := agenteval.Score(lines)

	caseDir := filepath.Join(opts.OutDir, opts.CaseName)
	// Overwrite guard (fail-closed): an existing case directory is only
	// overwritten under Force. Check BEFORE MkdirAll so the non-force
	// path leaves the directory untouched.
	if _, statErr := os.Stat(caseDir); statErr == nil {
		if !opts.Force {
			return fmt.Errorf("corpusdistill: case directory %q already exists; pass force to overwrite", caseDir)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("corpusdistill: stat case directory %q: %w", caseDir, statErr)
	}

	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return fmt.Errorf("corpusdistill: create case directory %q: %w", caseDir, err)
	}

	expected, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return fmt.Errorf("corpusdistill: marshal scorecard: %w", err)
	}
	expected = append(expected, '\n')

	files := map[string][]byte{
		"trace.jsonl":   plainBytes,
		"expected.json": expected,
		"case.md":       []byte(renderCaseMarkdown(opts, card)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(caseDir, name), content, 0o644); err != nil {
			return fmt.Errorf("corpusdistill: write %s: %w", name, err)
		}
	}
	return nil
}

// DistillReader is the io.Reader-accepting wrapper around Distill: it
// reads r fully into memory (a trace bundle is bounded by the runner's
// token budget) and delegates. Used by the command's stdin and
// HTTP-body paths.
func DistillReader(r io.Reader, opts Options) error {
	input, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("corpusdistill: read input: %w", err)
	}
	return Distill(input, opts)
}

// normalize returns the input in BOTH forms the rest of the pipeline
// needs: plainBytes (plain JSONL, written to trace.jsonl) and gzBytes
// (gzipped, consumed by bundle.ReadEvents). It auto-detects the input
// encoding from the gzip magic bytes:
//
//   - gzipped input  → gzBytes = input as-is; plainBytes = gunzip(input)
//   - plain input    → plainBytes = input as-is; gzBytes = gzip(input)
//
// A too-short input is treated as plain (it can't be a valid gzip
// member; ReadEvents will surface the real parse error downstream).
func normalize(input []byte) (plainBytes, gzBytes []byte, err error) {
	if isGzip(input) {
		plain, derr := gunzip(input)
		if derr != nil {
			return nil, nil, fmt.Errorf("corpusdistill: gunzip input: %w", derr)
		}
		return plain, input, nil
	}
	gz, gerr := gzipBytes(input)
	if gerr != nil {
		return nil, nil, fmt.Errorf("corpusdistill: gzip input: %w", gerr)
	}
	return input, gz, nil
}

// isGzip reports whether b begins with the gzip magic bytes.
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == gzipMagic[0] && b[1] == gzipMagic[1]
}

// gunzip decompresses a gzip member into plain bytes.
func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// gzipBytes compresses plain bytes into a gzip member.
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

// renderCaseMarkdown builds the fill-in case.md template. It pre-fills
// the derived outcome and stamps the Provenance: PRODUCTION (redacted)
// marker plus the originating-issue reference, leaving the
// distilled-signal narrative as TODO prompts for the operator (#819).
func renderCaseMarkdown(opts Options, card agenteval.Scorecard) string {
	issue := opts.Issue
	if issue == "" {
		issue = "TODO: originating issue reference (e.g. #1290)"
	}
	return fmt.Sprintf(`# Case: %[1]s

**Provenance: PRODUCTION (redacted bundle).** This trace was distilled
from a captured production run's REDACTED trace bundle (the
GET /v0/stages/{stage_id}/trace endpoint serves only the redacted
variant). Originating reference: %[2]s.

## What it represents

TODO: describe the trajectory this case captures — what the agent was
asked to do, what it did, and why this run is worth a corpus slot.

Derived outcome: `+"`%[3]s`"+` (from `+"`agenteval.Score`"+`).

## Distilled signal

TODO: state the specific Tier-A signal this case proves — the
scorecard field(s) that discriminate it from the healthy control, and
the branch of the scorer it exercises. See the sibling cases under
backend/internal/agenteval/testdata/corpus and
docs/architecture/agent-eval.md (#652, #819).
`, opts.CaseName, issue, card.Outcome)
}
