// Package bundle packages a runner's captured trace events into the
// signed *.jsonl.gz wire format defined by ADR-007 (#71). The
// resulting artifact is what the runner uploads to the backend's
// `POST /v0/runs/{id}/trace` endpoint per docs/api/v0.openapi.yaml.
//
// Bundle layout (per ADR-007):
//
//   - One JSON object per line, UTF-8.
//   - Each line: {seq, ts, kind, data}. seq is monotonic from 1.
//   - First line: kind="manifest", data carries bundle_schema,
//     run_id, stage_id, agent identity.
//   - Middle lines: one per captured agent.Event.
//   - Last line: kind="trailer", data carries event_count and a
//     sha256 of all preceding lines (truncation detector).
//   - The whole stream is gzipped at the default level (6).
//
// Storage / signing identity is the sha256 of the gzipped bytes
// (ARCHITECTURE.md §5.2 layout `{run_id}/<variant>/<sha256>.jsonl.gz`).
// That hash is computed by the caller from Pack's output.
//
// The trailer's content hash covers everything BEFORE the trailer
// line. It's an integrity check that runs purely on the JSONL
// payload and stays meaningful even if a downstream tool re-gzips
// or strips compression.
package bundle

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// SchemaV1 is the only supported bundle schema. v2+ stays additive
// within v1; breaking changes bump the major.
const SchemaV1 = "v1"

// MaxBundleBytes caps the gzipped output size. v0 trace volumes are
// already bounded by per-stage token budgets, so a hard ceiling
// here just protects backend storage from a runaway agent.
const MaxBundleBytes = 64 * 1024 * 1024 // 64 MiB

// ManifestData is the payload of the first line of every bundle.
// Fields align with ADR-007's manifest record.
//
// AgentFailed is set by the runner when the agent invocation
// returned a category-A failure (process crash, non-zero exit
// without producing a plan, etc.). The backend's trace handler
// reads this field and routes to FailStage(stageID, FailureA, …)
// instead of the policy-evaluation path. omitempty keeps older
// bundles (without the field) parsing as AgentFailed=false.
// (E8.5 #163)
type ManifestData struct {
	BundleSchema string    `json:"bundle_schema"`
	RunID        string    `json:"run_id"`
	StageID      string    `json:"stage_id"`
	Agent        string    `json:"agent"`
	Model        string    `json:"model,omitempty"`
	GeneratedAt  time.Time `json:"generated_at"`

	// InputTokens and OutputTokens are the agent-reported token split
	// for this stage. They are the authoritative cost input: the
	// backend prices them via pricing.Cost from the SIGNED bundle
	// rather than trusting a runner-emitted span, so a tampered or
	// dropped span cannot corrupt the cost ledger. omitempty keeps
	// older bundles (without the fields) parsing as 0/0.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	AgentFailed        bool   `json:"agent_failed,omitempty"`
	AgentFailureReason string `json:"agent_failure_reason,omitempty"`

	// PushAndOpenPR signals that this implement stage will commit, push,
	// and open a PR AFTER the trace upload. The backend's trace handler
	// reads it to forward-gate the implement stage's terminal transition:
	// it leaves the stage in `running` and lets the /pull-request upload
	// drive the terminal transition, so a commit/push/PR-open failure
	// lands the stage `failed` instead of stranding the run at the review
	// gate with a null PR (the #742 zombie). omitempty keeps older bundles
	// (and every non-PR-opening stage: plan/review, --no-pr, decomposed
	// children) parsing as false — the prior trace-driven transition. Keep
	// in lockstep with backend/internal/bundle.Manifest. (#742)
	PushAndOpenPR bool `json:"push_and_open_pr,omitempty"`

	// PushToSharedBranch signals that this implement stage is a decomposed
	// child that will commit + push onto the shared parent branch AFTER the
	// trace upload (but never open a PR — the parent run opens one
	// consolidated PR after all children settle, per ADR-032). The backend's
	// trace handler reads it to forward-gate the child stage's terminal
	// transition exactly like PushAndOpenPR does for standalone children: it
	// leaves the stage in `running` and lets the /pull-request upload drive
	// the terminal transition, so a commit/push failure lands the stage
	// `failed` instead of reaching the terminal succeeded state with no code
	// on the shared branch (the #771 zombie — the decomposition-child
	// analogue of #742). Mutually exclusive with PushAndOpenPR (a decomposed
	// child never opens its own PR). omitempty keeps older bundles (and every
	// non-child stage) parsing as false. Keep in lockstep with
	// backend/internal/bundle.Manifest. (#771)
	PushToSharedBranch bool `json:"push_to_shared_branch,omitempty"`

	// PushFixup signals that this implement stage is a fix-up re-dispatch
	// (#762) that will commit onto the EXISTING PR branch AFTER the trace
	// upload (updating the open PR, not opening a new one). The backend's
	// trace handler reads it to forward-gate the fix-up stage's terminal
	// transition exactly like PushAndOpenPR/PushToSharedBranch do for the
	// PR-opening and decomposed-child cases: it leaves the stage in `running`
	// and lets the /pull-request upload drive the terminal transition, so a
	// commit/push/compile-gate failure lands the stage `failed` (firing #788
	// fix-up recovery) instead of reaching terminal succeeded with the
	// implement re-review approving an unlanded diff (the #794 swallow).
	// Mutually exclusive with PushAndOpenPR and PushToSharedBranch (a fix-up
	// neither opens a PR nor pushes to a shared parent branch). omitempty keeps
	// older bundles (and every non-fix-up stage) parsing as false. Keep in
	// lockstep with backend/internal/bundle.Manifest. (#794)
	PushFixup bool `json:"push_fixup,omitempty"`
}

// TrailerData is the payload of the last line of every bundle. The
// ContentHash covers all preceding lines' raw bytes (with
// terminators) and is the floor on integrity inside the gzip layer.
type TrailerData struct {
	EventCount  int    `json:"event_count"`
	ContentHash string `json:"content_hash"`
}

// Line is the on-the-wire envelope. Every JSONL line in a bundle
// decodes to one of these regardless of kind.
type Line struct {
	Seq       int             `json:"seq"`
	Timestamp time.Time       `json:"ts"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// PackInputs collects the few non-event fields Pack needs. Keeping
// this in a struct (rather than positional args) makes call sites
// readable and lets us add fields without churning callers.
type PackInputs struct {
	RunID   string
	StageID string
	Agent   string // e.g. "claude-code"
	Model   string // optional; "" omits the field

	// InputTokens / OutputTokens are the agent-reported token split
	// carried through to the manifest for backend-side cost pricing.
	// Zero omits the field (older / token-less bundles).
	InputTokens  int
	OutputTokens int

	// AgentFailed flags a category-A failure originating in the
	// agent invocation. The runner sets this when agent.Result.OK
	// is false and the FailureCategory is "A". Backend's trace
	// handler routes the stage to FailStage(FailureA, …) when
	// this is true.
	AgentFailed        bool
	AgentFailureReason string

	// PushAndOpenPR flags an implement stage that will open a PR after the
	// trace upload. The runner sets it for standalone implement stages
	// (not --no-pr, not a decomposed child) so the backend forward-gates
	// the terminal transition onto the /pull-request upload. (#742)
	PushAndOpenPR bool

	// PushToSharedBranch flags a decomposed-child implement stage that will
	// commit + push onto the shared parent branch after the trace upload
	// (without opening a PR). The runner sets it for decomposed children so
	// the backend forward-gates the child stage's terminal transition onto
	// the /pull-request upload. Mutually exclusive with PushAndOpenPR. (#771)
	PushToSharedBranch bool

	// PushFixup flags a fix-up re-dispatch implement stage that will commit
	// onto the EXISTING PR branch after the trace upload (updating the open PR).
	// The runner sets it for fix-up passes so the backend forward-gates the
	// fix-up stage's terminal transition onto the /pull-request upload. Mutually
	// exclusive with PushAndOpenPR and PushToSharedBranch. (#794)
	PushFixup bool

	// Now returns the manifest's GeneratedAt timestamp. Default
	// time.Now; overridable for deterministic tests.
	Now func() time.Time
}

// Errors callers may want to switch on.
var (
	ErrEmptyRunID    = errors.New("bundle: RunID required")
	ErrEmptyStageID  = errors.New("bundle: StageID required")
	ErrEmptyAgent    = errors.New("bundle: Agent required")
	ErrTooLarge      = errors.New("bundle: exceeds MaxBundleBytes")
	ErrBadTrailer    = errors.New("bundle: trailer missing or malformed")
	ErrHashMismatch  = errors.New("bundle: content hash does not match trailer")
	ErrSchemaUnknown = errors.New("bundle: unsupported bundle schema")
)

// Pack writes a complete trace bundle to w: manifest, events,
// trailer, all gzipped. Returns the number of bytes written
// (gzipped) so callers can reason about size before uploading.
//
// Events appear in the order received; their Kind / Timestamp /
// Payload feed straight into the wire envelope. agent.Event's
// Kind="" defaults to "raw" so we don't emit empty-kind lines.
func Pack(w io.Writer, in PackInputs, events []agent.Event) (int, error) {
	if in.RunID == "" {
		return 0, ErrEmptyRunID
	}
	if in.StageID == "" {
		return 0, ErrEmptyStageID
	}
	if in.Agent == "" {
		return 0, ErrEmptyAgent
	}
	now := in.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// Build the raw JSONL stream first, then gzip into a counting
	// writer. Hashing the pre-gzip bytes is required for the
	// trailer; building in memory keeps the streaming code simple
	// and the size cap enforceable.
	var raw bytes.Buffer
	manifestPayload, err := json.Marshal(ManifestData{
		BundleSchema:       SchemaV1,
		RunID:              in.RunID,
		StageID:            in.StageID,
		Agent:              in.Agent,
		Model:              in.Model,
		InputTokens:        in.InputTokens,
		OutputTokens:       in.OutputTokens,
		GeneratedAt:        now(),
		AgentFailed:        in.AgentFailed,
		AgentFailureReason: in.AgentFailureReason,
		PushAndOpenPR:      in.PushAndOpenPR,
		PushToSharedBranch: in.PushToSharedBranch,
		PushFixup:          in.PushFixup,
	})
	if err != nil {
		return 0, fmt.Errorf("bundle: marshal manifest: %w", err)
	}
	if err := writeLine(&raw, Line{
		Seq: 1, Timestamp: now(), Kind: "manifest",
		Data: manifestPayload,
	}); err != nil {
		return 0, err
	}

	for i, ev := range events {
		kind := ev.Kind
		if kind == "" {
			kind = "raw"
		}
		ts := ev.Timestamp
		if ts.IsZero() {
			ts = now()
		}
		if err := writeLine(&raw, Line{
			Seq: i + 2, Timestamp: ts, Kind: kind, Data: ev.Payload,
		}); err != nil {
			return 0, err
		}
	}

	// Hash everything written so far — the trailer's content hash
	// covers all preceding lines verbatim.
	contentHash := sha256.Sum256(raw.Bytes())

	trailerPayload, err := json.Marshal(TrailerData{
		EventCount:  len(events),
		ContentHash: hex.EncodeToString(contentHash[:]),
	})
	if err != nil {
		return 0, fmt.Errorf("bundle: marshal trailer: %w", err)
	}
	if err := writeLine(&raw, Line{
		Seq: len(events) + 2, Timestamp: now(), Kind: "trailer",
		Data: trailerPayload,
	}); err != nil {
		return 0, err
	}

	// Gzip in-memory so we can enforce the size cap before handing
	// bytes to the caller's writer (which may be a network stream).
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.DefaultCompression)
	if err != nil {
		return 0, fmt.Errorf("bundle: gzip writer: %w", err)
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return 0, fmt.Errorf("bundle: gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("bundle: gzip close: %w", err)
	}
	if gz.Len() > MaxBundleBytes {
		return 0, fmt.Errorf("%w: gzipped %d bytes > %d", ErrTooLarge, gz.Len(), MaxBundleBytes)
	}
	n, err := w.Write(gz.Bytes())
	if err != nil {
		return n, fmt.Errorf("bundle: write: %w", err)
	}
	return n, nil
}

// PackBytes is the convenience form: returns the gzipped bundle as
// a byte slice plus the storage hash (sha256 of those bytes). The
// storage hash is the upload key per ARCHITECTURE.md §5.2 and the
// signing input the runner feeds to backend/internal/signing.
func PackBytes(in PackInputs, events []agent.Event) (data []byte, storageHash string, err error) {
	var buf bytes.Buffer
	if _, err := Pack(&buf, in, events); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// Open decompresses r and returns the parsed bundle: manifest,
// trace lines (events between manifest and trailer), and the
// trailer. It also re-computes the content hash and returns
// ErrHashMismatch if the trailer's value disagrees, so callers
// can rely on the bundle's integrity once Open returns nil error.
//
// Decoding stops on EOF or io.ErrUnexpectedEOF; partial bundles
// produce ErrBadTrailer rather than silent success.
func Open(r io.Reader) (manifest ManifestData, events []Line, trailer TrailerData, err error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: gzip reader: %w", err)
	}
	defer func() { _ = zr.Close() }()

	raw, err := io.ReadAll(zr)
	if err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: read: %w", err)
	}

	// Split on '\n' but keep terminators — the content hash covers
	// each line's '\n' as written.
	var lines []Line
	var lineBytes [][]byte
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		lineBytes = append(lineBytes, raw[start:i+1])
		var ln Line
		if err := json.Unmarshal(raw[start:i], &ln); err != nil {
			return manifest, nil, trailer,
				fmt.Errorf("bundle: parse line %d: %w", len(lines)+1, err)
		}
		lines = append(lines, ln)
		start = i + 1
	}

	if len(lines) < 2 {
		return manifest, nil, trailer, ErrBadTrailer
	}

	first := lines[0]
	if first.Kind != "manifest" {
		return manifest, nil, trailer,
			fmt.Errorf("%w: first line kind=%q, want manifest", ErrBadTrailer, first.Kind)
	}
	if err := json.Unmarshal(first.Data, &manifest); err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: parse manifest: %w", err)
	}
	if manifest.BundleSchema != SchemaV1 {
		return manifest, nil, trailer,
			fmt.Errorf("%w: %q", ErrSchemaUnknown, manifest.BundleSchema)
	}

	last := lines[len(lines)-1]
	if last.Kind != "trailer" {
		return manifest, nil, trailer,
			fmt.Errorf("%w: last line kind=%q, want trailer", ErrBadTrailer, last.Kind)
	}
	if err := json.Unmarshal(last.Data, &trailer); err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: parse trailer: %w", err)
	}

	// Content hash covers everything except the trailer line's bytes.
	preceding := concat(lineBytes[:len(lineBytes)-1])
	got := sha256.Sum256(preceding)
	if hex.EncodeToString(got[:]) != trailer.ContentHash {
		return manifest, nil, trailer, ErrHashMismatch
	}
	if trailer.EventCount != len(lines)-2 {
		return manifest, nil, trailer,
			fmt.Errorf("%w: trailer event_count=%d, observed=%d",
				ErrBadTrailer, trailer.EventCount, len(lines)-2)
	}

	events = lines[1 : len(lines)-1]
	return manifest, events, trailer, nil
}

// writeLine emits one JSON line followed by '\n'. Centralized so
// every line uses the same encoder configuration and we never
// accidentally end up with a missing terminator.
func writeLine(w io.Writer, ln Line) error {
	body, err := json.Marshal(ln)
	if err != nil {
		return fmt.Errorf("bundle: marshal line: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("bundle: write line: %w", err)
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("bundle: write newline: %w", err)
	}
	return nil
}

func concat(bs [][]byte) []byte {
	n := 0
	for _, b := range bs {
		n += len(b)
	}
	out := make([]byte, 0, n)
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}
