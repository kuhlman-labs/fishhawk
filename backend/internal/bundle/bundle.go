// Package bundle reads the *.jsonl.gz trace bundle wire format
// produced by runner/internal/bundle. The runner's package owns
// pack semantics; this package owns the read side, kept narrow to
// what backend handlers actually need.
//
// We deliberately don't import the runner package: backend and
// runner are separate Go modules, and the read-side surface is
// small enough (~100 lines) that the duplication is cheaper than
// promoting bundle to a shared module.
//
// The reader doesn't verify integrity — the trace upload handler
// already verified the Ed25519 signature over the raw bundle bytes
// before this package sees them. That signature commits to every
// byte we're about to parse; a tamper between sig-verify and
// extract is implausible without something like a library bug.
package bundle

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
)

// EventKindGitDiff is the event the runner emits ahead of policy
// evaluation. The backend's policy re-evaluation path reads from
// this event rather than re-running git on the customer's filesystem
// (which the backend doesn't have access to).
const EventKindGitDiff = "git_diff"

// EventKindManifest is the bundle's first line. Carries
// ManifestData; the trace handler reads it for the agent_failed
// signal and the bundle schema version.
const EventKindManifest = "manifest"

// Manifest mirrors runner/internal/bundle.ManifestData. Backend
// owns the read side; agreeing field-by-field with the runner is
// the contract — there's no schema-sync CI for this format
// (it's the wire format, not a JSON Schema), so add fields on
// both sides in lockstep. (E8.5 #163 added AgentFailed +
// AgentFailureReason.)
type Manifest struct {
	BundleSchema string `json:"bundle_schema"`
	RunID        string `json:"run_id"`
	StageID      string `json:"stage_id"`
	Agent        string `json:"agent"`
	Model        string `json:"model,omitempty"`
	// InputTokens / OutputTokens are the agent-reported token split,
	// added on the runner side alongside Model (#649). The backend's
	// cost rollup (backend/internal/cost) reads them from the signed
	// manifest to compute the authoritative per-run cost — control-
	// plane-side, not trusted from a runner span. Older bundles omit
	// the fields and decode to 0 (recorded as zero-cost). Keep in
	// lockstep with runner/internal/bundle.ManifestData.
	InputTokens        int    `json:"input_tokens,omitempty"`
	OutputTokens       int    `json:"output_tokens,omitempty"`
	GeneratedAt        string `json:"generated_at"`
	AgentFailed        bool   `json:"agent_failed,omitempty"`
	AgentFailureReason string `json:"agent_failure_reason,omitempty"`
}

// Errors callers may want to switch on.
var (
	// ErrBadGzip means the bundle's gzip frame couldn't be opened
	// (truncated, wrong magic). Distinct from a parse error so the
	// upload handler can surface "your trace bundle is corrupt"
	// rather than "your event stream is malformed."
	ErrBadGzip = errors.New("bundle: gzip frame invalid")

	// ErrNoDiffEvent means the bundle parsed cleanly but contained
	// no git_diff event. Returned by ExtractDiff so the caller can
	// distinguish "bundle had no diff" (skip policy re-eval) from
	// "bundle was malformed."
	ErrNoDiffEvent = errors.New("bundle: no git_diff event found")

	// ErrNoManifest means the bundle's first line wasn't a manifest
	// record, or the bundle parsed but had zero lines. The trace
	// handler treats this as a malformed upload and rejects the
	// request rather than guessing at the missing context.
	ErrNoManifest = errors.New("bundle: manifest line missing or first line had wrong kind")
)

// Line is the on-the-wire envelope for one JSONL line. Mirrors
// the runner's bundle.Line shape; we redefine here to avoid the
// cross-module import.
type Line struct {
	Seq       int             `json:"seq"`
	Timestamp time.Time       `json:"ts"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// gitDiffPayload mirrors the runner's payload struct exactly.
// Adding fields requires both sides to agree. The `patch` /
// `patch_truncated` json tags MUST stay identical to the runner's
// gitDiffPayload (runner/cmd/fishhawk-runner/main.go) — this is the
// runner↔backend wire contract, not a JSON Schema, so the two sides
// move in lockstep. Patch is additive: older bundles omit it and
// decode to an empty string (#585).
type gitDiffPayload struct {
	Kind           string         `json:"kind"`
	BaseRef        string         `json:"base_ref"`
	Files          []gitDiffEntry `json:"files"`
	NumFiles       int            `json:"num_files"`
	Patch          string         `json:"patch,omitempty"`
	PatchTruncated bool           `json:"patch_truncated,omitempty"`
}

type gitDiffEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// ReadEvents decompresses bundleBytes and returns every JSONL line
// (manifest, events, trailer) as a Line. Use ExtractDiff for the
// policy re-eval path; this function is for tests / future readers.
func ReadEvents(bundleBytes []byte) ([]Line, error) {
	zr, err := gzip.NewReader(byteReader(bundleBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadGzip, err)
	}
	defer func() { _ = zr.Close() }()

	var out []Line
	scanner := bufio.NewScanner(zr)
	// Bundle lines can carry diffs / model output bigger than the
	// default 64 KiB scanner buffer. Cap at 4 MiB per line, which
	// is well over the practical event size and well under the
	// bundle's overall MaxBundleBytes.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var line Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("bundle: parse line: %w", err)
		}
		out = append(out, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("bundle: read: %w", err)
	}
	return out, nil
}

// ExtractManifest returns the parsed first-line manifest. Used by
// the trace handler to read the agent_failed flag (E8.5) before
// deciding whether to enter the policy-evaluation path or route
// to FailStage(FailureA, …).
//
// Returns ErrBadGzip on a corrupt frame and ErrNoManifest when the
// bundle is empty or the first line isn't a manifest record.
func ExtractManifest(bundleBytes []byte) (Manifest, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return Manifest{}, err
	}
	if len(lines) == 0 || lines[0].Kind != EventKindManifest {
		return Manifest{}, ErrNoManifest
	}
	var m Manifest
	if err := json.Unmarshal(lines[0].Data, &m); err != nil {
		return Manifest{}, fmt.Errorf("bundle: parse manifest: %w", err)
	}
	return m, nil
}

// ExtractTiming returns the Timestamp of the first non-manifest,
// non-trailer event (agent start proxy) and the Timestamp of the last
// non-trailer event (agent end proxy). Both timestamps come from the
// runner host's clock so they are accurate relative to each other.
//
// Returns ok=false when the bundle contains fewer than two such
// intermediate events — e.g. a manifest-plus-trailer-only bundle —
// so callers can skip calibration emission without emitting a warning.
func ExtractTiming(bundleBytes []byte) (startedAt, endedAt time.Time, ok bool) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	var intermediate []Line
	for _, l := range lines {
		if l.Kind == EventKindManifest || l.Kind == "trailer" {
			continue
		}
		intermediate = append(intermediate, l)
	}
	if len(intermediate) < 2 {
		return time.Time{}, time.Time{}, false
	}
	return intermediate[0].Timestamp, intermediate[len(intermediate)-1].Timestamp, true
}

// ExtractDiff returns the policy.Diff carried in the bundle's
// git_diff event, or ErrNoDiffEvent if the bundle didn't include
// one. A bundle without a git_diff event isn't an error — the
// backend's policy re-eval path treats that as "no diff to
// evaluate; skip re-eval, proceed to awaiting_approval."
func ExtractDiff(bundleBytes []byte) (policy.Diff, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return policy.Diff{}, err
	}
	for _, line := range lines {
		if line.Kind != EventKindGitDiff {
			continue
		}
		var payload gitDiffPayload
		if err := json.Unmarshal(line.Data, &payload); err != nil {
			return policy.Diff{}, fmt.Errorf("bundle: parse git_diff payload: %w", err)
		}
		out := policy.Diff{
			ChangedFiles: make([]policy.ChangedFile, 0, len(payload.Files)),
			// Additive content for the implement-review prompt only;
			// the policy engine never reads Patch. Empty when the
			// bundle predates the field or the runner couldn't compute
			// the patch (#585).
			Patch: payload.Patch,
		}
		for _, f := range payload.Files {
			out.ChangedFiles = append(out.ChangedFiles, policy.ChangedFile{
				Path:   f.Path,
				Status: policy.Status(f.Status),
			})
		}
		return out, nil
	}
	return policy.Diff{}, ErrNoDiffEvent
}

// byteReader is the smallest io.Reader over a []byte that
// gzip.NewReader needs. Avoids pulling in bytes.Reader for one
// line of glue.
func byteReader(b []byte) io.Reader {
	return &readerOverBytes{b: b}
}

type readerOverBytes struct {
	b   []byte
	off int
}

func (r *readerOverBytes) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
