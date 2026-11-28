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
	"os"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// SchemaV1 is the only supported bundle schema. v2+ stays additive
// within v1; breaking changes bump the major.
const SchemaV1 = "v1"

// MaxBundleBytes caps the gzipped output size. v0 trace volumes are
// already bounded by per-stage token budgets, so a hard ceiling
// here just protects backend storage from a runaway agent.
const MaxBundleBytes = 64 * 1024 * 1024 // 64 MiB

// MaxLineBytes bounds one decompressed JSONL line, measured as the
// json.Marshal output (HTML escaping included) without its "\n"
// terminator (#5535). KEEP IN SYNC with backend/internal/bundle.MaxLineBytes:
// the backend's bounded ReadEvents refuses a bundle with a longer line, and
// every Extract* reader then fails on it. Pack never writes such a line
// (it elides or trims it, or fails closed) and Open refuses one
// (ErrLineTooLong). agent.MaxTraceLineBytes equals this value.
const MaxLineBytes = 16 * 1024 * 1024 // 16 MiB

// MaxDecompressedBytes bounds the total decompressed bundle, every line and
// its "\n" included (#5535). KEEP IN SYNC with
// backend/internal/bundle.MaxDecompressedBytes. Pack brings an over-bound
// bundle under it by eliding ordinary events and then trimming verify_run
// outputs, and fails closed (ErrDecompressedTooLarge) when that is not
// enough; Open refuses an over-bound stream.
const MaxDecompressedBytes = 256 * 1024 * 1024 // 256 MiB

// KindEventElided is the kind of the placeholder line Pack writes in place
// of an ordinary event it elided (#5535). The placeholder keeps the
// original seq and ts; its data is ElidedEventData.
const KindEventElided = "event_elided"

// Event kinds the backend reads from a bundle. Pack never elides a line of
// these kinds: an over-bound manifest, git_diff, gate_evidence or
// policy_event line fails Pack with ErrLineTooLarge, and an over-bound
// verify_run has only its output trimmed.
const (
	// kindManifest is read by backend ExtractManifest (and PeekManifest).
	kindManifest = "manifest"
	// kindTrailer is the integrity line Open checks.
	kindTrailer = "trailer"
	// kindGitDiff is read by backend ExtractDiff. KEEP IN SYNC.
	kindGitDiff = "git_diff"
	// kindPolicyEvent is read by backend ExtractScopeDrift and
	// ExtractScopeAmendmentsFolded. KEEP IN SYNC.
	kindPolicyEvent = "policy_event"
	// kindGateEvidence is read by backend ExtractGateEvidence. KEEP IN SYNC.
	kindGateEvidence = "gate_evidence"
	// kindVerifyRun is read by backend ExtractHeadSHA, ExtractVerifyHead and
	// ExtractVerifyIdentity (head_sha, tree_sha, change_id). KEEP IN SYNC.
	kindVerifyRun = "verify_run"
)

// ElidedEventData is the data payload of a KindEventElided placeholder.
// SHA256 is the hex sha256 of the original marshalled line (without its
// "\n"), and ExcerptHead / ExcerptTail are bounded slices of that line's
// start and end. Pack takes them from the bytes it is given, so in the
// redacted variant they come from the already-redacted event.
type ElidedEventData struct {
	ElidedKind           string `json:"elided_kind"`
	OriginalBytes        int    `json:"original_bytes"`
	SHA256               string `json:"sha256"`
	Bound                string `json:"bound"` // "line" or "total"
	MaxLineBytes         int    `json:"max_line_bytes,omitempty"`
	MaxDecompressedBytes int64  `json:"max_decompressed_bytes,omitempty"`
	ExcerptHead          string `json:"excerpt_head"`
	ExcerptTail          string `json:"excerpt_tail"`
}

// verifyOutputElidedMarker separates the head and tail of a trimmed
// verify_run output.
const verifyOutputElidedMarker = "\n...[fishhawk: %d bytes of verify output elided to fit the bundle bound]...\n"

// packLimits carries Pack's bounds so tests can inject small values;
// Pack uses defaultPackLimits.
type packLimits struct {
	maxLine  int
	maxTotal int64
	// excerptBytes is the per-side size of an elided placeholder's excerpt.
	excerptBytes int
	// verifyOutputKeepBytes is the per-side size kept from a trimmed
	// verify_run output.
	verifyOutputKeepBytes int
}

var defaultPackLimits = packLimits{
	maxLine:               MaxLineBytes,
	maxTotal:              MaxDecompressedBytes,
	excerptBytes:          1024,
	verifyOutputKeepBytes: 32 * 1024,
}

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

	// CacheReadInputTokens and CacheWriteInputTokens carry the prompt-cache
	// split of the input side (ADR-044 / #1349): the backend prices cache
	// reads at the family discount and cache writes at the premium via
	// cost.FromManifestWithCache, rather than the flat input rate. InputTokens
	// is the FRESH (cache-exclusive) input; these two are the cache-served read
	// and cache-creation write portions. omitempty keeps older bundles (without
	// the fields) decoding to 0 — priced as no cache, fully back-compatible.
	// Keep in lockstep with backend/internal/bundle.Manifest.
	CacheReadInputTokens  int `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens,omitempty"`

	AgentFailed        bool   `json:"agent_failed,omitempty"`
	AgentFailureReason string `json:"agent_failure_reason,omitempty"`

	// RunnerKind is the runner's self-observed execution channel
	// (`github_actions` | `local`, #1346 / ADR-045): the runner detects
	// it from its own environment (detectRunnerKind) rather than trusting
	// the operator's creation-time declaration. The backend's trace
	// handler reads it and LOCKS the run's runner_kind to it (correcting
	// the creation hint), closing the #1344 local-loop wedge where an
	// omitted runner_kind:local defaulted to github_actions and the drive
	// waited on a phantom GitHub-Actions runner.
	//
	// Carrying it INSIDE the signed manifest is what makes it attestable:
	// migration 0024's note that "the runner never self-declares" guarded
	// against a FALSIFIABLE claim, but a claim committed under the per-run
	// Ed25519 signature is tamper-evident, not falsifiable. omitempty keeps
	// older bundles (without the field) parsing as "" — treated as no
	// report, skipping reconciliation. Keep in lockstep with
	// backend/internal/bundle.Manifest.
	RunnerKind string `json:"runner_kind,omitempty"`

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

	// VerifyPinHeadSHA is the commit the runner pinned at
	// refs/fishhawk/checkpoints/<run>/<stage>-verify before the trace upload,
	// after a passing committed verify on a push-gated implement stage
	// (#5535). The backend's trace-time failure arms name it in the stage's
	// failure reason, so a verified tree the backend refuses is not lost.
	// omitempty: empty when nothing was pinned, so the manifest is
	// byte-identical to the pre-field shape.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag MUST stay byte-identical to
	// backend/internal/bundle.Manifest.VerifyPinHeadSHA. Pinned by the shared
	// golden testdata/wire/bundle_manifest_verify_pin.json, which the runner
	// bundle test writes and the backend bundle test decodes.
	VerifyPinHeadSHA string `json:"verify_pin_head_sha,omitempty"`
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

	// CacheReadInputTokens / CacheWriteInputTokens are the prompt-cache split
	// of the input side (ADR-044 / #1349), carried through to the manifest so
	// the backend prices cache reads at the discount and cache writes at the
	// premium. Zero omits the field (older / cache-less bundles) — priced as
	// no cache, fully back-compatible (same note as InputTokens).
	CacheReadInputTokens  int
	CacheWriteInputTokens int

	// AgentFailed flags a category-A failure originating in the
	// agent invocation. The runner sets this when agent.Result.OK
	// is false and the FailureCategory is "A". Backend's trace
	// handler routes the stage to FailStage(FailureA, …) when
	// this is true.
	AgentFailed        bool
	AgentFailureReason string

	// RunnerKind is the runner's self-observed execution channel
	// (`github_actions` | `local`, #1346 / ADR-045), set from
	// detectRunnerKind(os.Getenv) at pack time and carried into the signed
	// manifest. Empty omits the field (older / channel-less bundles), which
	// the backend treats as no self-report.
	RunnerKind string

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

	// VerifyPinHeadSHA is the commit pinned at the stage's -verify checkpoint
	// ref before the trace upload (#5535). Empty omits the field.
	VerifyPinHeadSHA string

	// Log receives one JSON line per elided event or trimmed verify_run
	// output (kind, seq, original_bytes, sha256). nil writes to os.Stderr,
	// the runner's log stream.
	Log io.Writer

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

	// ErrLineTooLarge means Pack could not bring a line under MaxLineBytes:
	// a manifest, git_diff, gate_evidence or policy_event line over the
	// bound, or a verify_run line still over it after its output was
	// trimmed. Pack fails closed rather than drop data the backend reads.
	ErrLineTooLarge = errors.New("bundle: load-bearing line exceeds MaxLineBytes")

	// ErrLineTooLong means Open read a line over MaxLineBytes.
	ErrLineTooLong = errors.New("bundle: line exceeds MaxLineBytes")

	// ErrDecompressedTooLarge means the decompressed bundle exceeds
	// MaxDecompressedBytes: Open read past the bound, or Pack could not
	// bring the bundle under it.
	ErrDecompressedTooLarge = errors.New("bundle: decompressed size exceeds MaxDecompressedBytes")
)

// Pack writes a complete trace bundle to w: manifest, events,
// trailer, all gzipped. Returns the number of bytes written
// (gzipped) so callers can reason about size before uploading.
//
// Events appear in the order received; their Kind / Timestamp /
// Payload feed straight into the wire envelope. agent.Event's
// Kind="" defaults to "raw" so we don't emit empty-kind lines.
//
// Pack bounds the bundle before hashing it (#5535), so the trailer always
// describes what ships and the backend's bounded reader can read it:
//
//   - Per line (MaxLineBytes): an over-bound ordinary event becomes a
//     KindEventElided placeholder with the same seq and ts. An over-bound
//     verify_run keeps every field but output, which becomes a head+tail
//     excerpt, and gains output_elided_bytes. An over-bound manifest,
//     git_diff, gate_evidence or policy_event line, or a verify_run still
//     over the bound after trimming, fails with ErrLineTooLarge.
//   - Total (MaxDecompressedBytes): ordinary events are elided largest
//     first (ties: higher seq first) until the total fits, then verify_run
//     outputs are trimmed largest first; if it still does not fit, Pack
//     fails with ErrDecompressedTooLarge.
//
// The trailer's event_count stays len(events): a placeholder counts as an
// event.
func Pack(w io.Writer, in PackInputs, events []agent.Event) (int, error) {
	return pack(w, in, events, defaultPackLimits)
}

// packedLine is one marshalled line plus what the bounding passes need.
type packedLine struct {
	seq     int
	ts      time.Time
	kind    string
	payload json.RawMessage
	body    []byte // json.Marshal(Line), no terminator
	elided  bool   // replaced by a KindEventElided placeholder
	trimmed bool   // verify_run output already trimmed
}

func pack(w io.Writer, in PackInputs, events []agent.Event, lim packLimits) (int, error) {
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
	logw := in.Log
	if logw == nil {
		logw = os.Stderr
	}

	// Marshal every line to bytes first, then apply the bounds, then hash
	// the final lines and write the trailer.
	manifestPayload, err := json.Marshal(ManifestData{
		BundleSchema:          SchemaV1,
		RunID:                 in.RunID,
		StageID:               in.StageID,
		Agent:                 in.Agent,
		Model:                 in.Model,
		InputTokens:           in.InputTokens,
		OutputTokens:          in.OutputTokens,
		CacheReadInputTokens:  in.CacheReadInputTokens,
		CacheWriteInputTokens: in.CacheWriteInputTokens,
		GeneratedAt:           now(),
		AgentFailed:           in.AgentFailed,
		AgentFailureReason:    in.AgentFailureReason,
		RunnerKind:            in.RunnerKind,
		PushAndOpenPR:         in.PushAndOpenPR,
		PushToSharedBranch:    in.PushToSharedBranch,
		PushFixup:             in.PushFixup,
		VerifyPinHeadSHA:      in.VerifyPinHeadSHA,
	})
	if err != nil {
		return 0, fmt.Errorf("bundle: marshal manifest: %w", err)
	}
	manifest, err := newPackedLine(1, now(), kindManifest, manifestPayload)
	if err != nil {
		return 0, err
	}
	if len(manifest.body) > lim.maxLine {
		return 0, fmt.Errorf("%w: manifest line is %d bytes > %d", ErrLineTooLarge, len(manifest.body), lim.maxLine)
	}

	lines := make([]*packedLine, 0, len(events))
	for i, ev := range events {
		kind := ev.Kind
		if kind == "" {
			kind = "raw"
		}
		ts := ev.Timestamp
		if ts.IsZero() {
			ts = now()
		}
		pl, err := newPackedLine(i+2, ts, kind, ev.Payload)
		if err != nil {
			return 0, err
		}
		if len(pl.body) > lim.maxLine {
			if err := boundLine(pl, lim, in, logw); err != nil {
				return 0, err
			}
		}
		lines = append(lines, pl)
	}

	// The trailer's length is fixed once its ts is: the hash is always 64
	// hex characters and event_count is len(events). A zero hash of the
	// same length sizes it for the total-bound pass.
	trailerTS := now()
	sizer, err := trailerLine(len(events), trailerTS, make([]byte, sha256.Size))
	if err != nil {
		return 0, err
	}
	total := int64(len(manifest.body)+1) + int64(len(sizer)+1)
	for _, pl := range lines {
		total += int64(len(pl.body) + 1)
	}
	if total > lim.maxTotal {
		if _, err := boundTotal(lines, total, lim, in, logw); err != nil {
			return 0, err
		}
	}

	// Hash the final lines: the trailer's content hash covers every
	// preceding line verbatim, terminators included.
	h := sha256.New()
	writeHashed := func(b []byte) {
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{'\n'})
	}
	writeHashed(manifest.body)
	for _, pl := range lines {
		writeHashed(pl.body)
	}
	trailer, err := trailerLine(len(events), trailerTS, h.Sum(nil))
	if err != nil {
		return 0, err
	}

	// Gzip in-memory so we can enforce the size cap before handing
	// bytes to the caller's writer (which may be a network stream).
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.DefaultCompression)
	if err != nil {
		return 0, fmt.Errorf("bundle: gzip writer: %w", err)
	}
	gzWriteLine := func(b []byte) error {
		if _, err := zw.Write(b); err != nil {
			return fmt.Errorf("bundle: gzip write: %w", err)
		}
		if _, err := zw.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("bundle: gzip write: %w", err)
		}
		return nil
	}
	if err := gzWriteLine(manifest.body); err != nil {
		return 0, err
	}
	for _, pl := range lines {
		if err := gzWriteLine(pl.body); err != nil {
			return 0, err
		}
	}
	if err := gzWriteLine(trailer); err != nil {
		return 0, err
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

// newPackedLine marshals one envelope. The body is json.Marshal's output,
// HTML escaping included, which is what the bounds measure.
func newPackedLine(seq int, ts time.Time, kind string, payload json.RawMessage) (*packedLine, error) {
	body, err := json.Marshal(Line{Seq: seq, Timestamp: ts, Kind: kind, Data: payload})
	if err != nil {
		return nil, fmt.Errorf("bundle: marshal line: %w", err)
	}
	return &packedLine{seq: seq, ts: ts, kind: kind, payload: payload, body: body}, nil
}

func trailerLine(eventCount int, ts time.Time, sum []byte) ([]byte, error) {
	payload, err := json.Marshal(TrailerData{
		EventCount:  eventCount,
		ContentHash: hex.EncodeToString(sum),
	})
	if err != nil {
		return nil, fmt.Errorf("bundle: marshal trailer: %w", err)
	}
	body, err := json.Marshal(Line{Seq: eventCount + 2, Timestamp: ts, Kind: kindTrailer, Data: payload})
	if err != nil {
		return nil, fmt.Errorf("bundle: marshal line: %w", err)
	}
	return body, nil
}

// loadBearing reports whether the backend reads lines of this kind, so Pack
// must never elide one wholesale.
func loadBearing(kind string) bool {
	switch kind {
	case kindManifest, kindGitDiff, kindPolicyEvent, kindGateEvidence, kindVerifyRun:
		return true
	}
	return false
}

// boundLine brings one over-bound event line under lim.maxLine or fails.
func boundLine(pl *packedLine, lim packLimits, in PackInputs, logw io.Writer) error {
	switch {
	case pl.kind == kindVerifyRun:
		if err := trimVerifyRun(pl, lim, "line", in, logw); err != nil {
			return err
		}
		if len(pl.body) > lim.maxLine {
			return fmt.Errorf("%w: verify_run line (seq %d) is %d bytes > %d after trimming its output",
				ErrLineTooLarge, pl.seq, len(pl.body), lim.maxLine)
		}
		return nil
	case loadBearing(pl.kind):
		return fmt.Errorf("%w: %s line (seq %d) is %d bytes > %d",
			ErrLineTooLarge, pl.kind, pl.seq, len(pl.body), lim.maxLine)
	default:
		return elide(pl, lim, "line", in, logw)
	}
}

// boundTotal brings the bundle's total under lim.maxTotal: ordinary events
// largest first, then verify_run outputs largest first, then fail closed.
// It returns the new total.
func boundTotal(lines []*packedLine, total int64, lim packLimits, in PackInputs, logw io.Writer) (int64, error) {
	largestFirst := func(keep func(*packedLine) bool) []*packedLine {
		var c []*packedLine
		for _, pl := range lines {
			if keep(pl) {
				c = append(c, pl)
			}
		}
		sort.SliceStable(c, func(i, j int) bool {
			if len(c[i].body) != len(c[j].body) {
				return len(c[i].body) > len(c[j].body)
			}
			return c[i].seq > c[j].seq
		})
		return c
	}

	for _, pl := range largestFirst(func(pl *packedLine) bool { return !pl.elided && !loadBearing(pl.kind) }) {
		if total <= lim.maxTotal {
			return total, nil
		}
		before := len(pl.body)
		saved := *pl
		if err := elide(pl, lim, "total", in, io.Discard); err != nil {
			return total, err
		}
		if len(pl.body) >= before {
			// A placeholder no smaller than the line frees nothing.
			*pl = saved
			continue
		}
		logElision(logw, in, saved.kind, saved.seq, saved.body, "total")
		total += int64(len(pl.body) - before)
	}

	for _, pl := range largestFirst(func(pl *packedLine) bool { return pl.kind == kindVerifyRun && !pl.trimmed }) {
		if total <= lim.maxTotal {
			return total, nil
		}
		before := len(pl.body)
		saved := *pl
		if err := trimVerifyRun(pl, lim, "total", in, logw); err != nil {
			if errors.Is(err, errNoOutputToTrim) {
				*pl = saved
				continue
			}
			return total, err
		}
		total += int64(len(pl.body) - before)
	}

	if total > lim.maxTotal {
		return total, fmt.Errorf("%w: %d bytes > %d after eliding events and trimming verify output",
			ErrDecompressedTooLarge, total, lim.maxTotal)
	}
	return total, nil
}

// elide replaces pl with a KindEventElided placeholder keeping its seq and
// ts. It logs the elision to logw.
func elide(pl *packedLine, lim packLimits, bound string, in PackInputs, logw io.Writer) error {
	sum := sha256.Sum256(pl.body)
	head, tail := excerpt(pl.body, lim.excerptBytes)
	data := ElidedEventData{
		ElidedKind:    pl.kind,
		OriginalBytes: len(pl.body),
		SHA256:        hex.EncodeToString(sum[:]),
		Bound:         bound,
		ExcerptHead:   string(head),
		ExcerptTail:   string(tail),
	}
	if bound == "line" {
		data.MaxLineBytes = lim.maxLine
	} else {
		data.MaxDecompressedBytes = lim.maxTotal
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("bundle: marshal elided placeholder: %w", err)
	}
	np, err := newPackedLine(pl.seq, pl.ts, KindEventElided, payload)
	if err != nil {
		return err
	}
	logElision(logw, in, pl.kind, pl.seq, pl.body, bound)
	np.elided = true
	*pl = *np
	return nil
}

func logElision(logw io.Writer, in PackInputs, kind string, seq int, original []byte, bound string) {
	sum := sha256.Sum256(original)
	_, _ = fmt.Fprintf(logw,
		`{"event":"bundle_event_elided","run_id":%q,"stage_id":%q,"kind":%q,"seq":%d,"original_bytes":%d,"sha256":%q,"bound":%q}`+"\n",
		in.RunID, in.StageID, kind, seq, len(original), hex.EncodeToString(sum[:]), bound)
}

var errNoOutputToTrim = errors.New("bundle: verify_run has no string output to trim")

// trimVerifyRun replaces a verify_run payload's output with a head+tail
// excerpt and adds output_elided_bytes. Every other field keeps its value
// byte-for-byte (the backend reads head_sha, tree_sha and change_id).
func trimVerifyRun(pl *packedLine, lim packLimits, bound string, in PackInputs, logw io.Writer) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pl.payload, &fields); err != nil {
		return fmt.Errorf("%w: verify_run line (seq %d) payload is not a JSON object: %v", ErrLineTooLarge, pl.seq, err)
	}
	var output string
	raw, ok := fields["output"]
	if !ok || json.Unmarshal(raw, &output) != nil {
		if bound == "line" {
			return fmt.Errorf("%w: verify_run line (seq %d) is %d bytes > %d and has no string output to trim",
				ErrLineTooLarge, pl.seq, len(pl.body), lim.maxLine)
		}
		return errNoOutputToTrim
	}
	head, tail := excerpt([]byte(output), lim.verifyOutputKeepBytes)
	elided := len(output) - len(head) - len(tail)
	if elided <= 0 {
		if bound == "line" {
			return fmt.Errorf("%w: verify_run line (seq %d) is %d bytes > %d with its output already within the excerpt",
				ErrLineTooLarge, pl.seq, len(pl.body), lim.maxLine)
		}
		return errNoOutputToTrim
	}
	trimmedOut, err := json.Marshal(string(head) + fmt.Sprintf(verifyOutputElidedMarker, elided) + string(tail))
	if err != nil {
		return fmt.Errorf("bundle: marshal trimmed verify output: %w", err)
	}
	elidedRaw, _ := json.Marshal(elided)
	fields["output"] = trimmedOut
	fields["output_elided_bytes"] = elidedRaw
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("bundle: marshal trimmed verify_run: %w", err)
	}
	np, err := newPackedLine(pl.seq, pl.ts, pl.kind, payload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pl.body)
	_, _ = fmt.Fprintf(logw,
		`{"event":"bundle_verify_output_trimmed","run_id":%q,"stage_id":%q,"kind":%q,"seq":%d,"original_bytes":%d,"output_elided_bytes":%d,"sha256":%q,"bound":%q}`+"\n",
		in.RunID, in.StageID, pl.kind, pl.seq, len(pl.body), elided, hex.EncodeToString(sum[:]), bound)
	np.trimmed = true
	*pl = *np
	return nil
}

// excerpt returns up to n bytes from each end of b, cut on UTF-8 rune
// boundaries. When b fits in 2n bytes, head is b and tail is empty.
func excerpt(b []byte, n int) (head, tail []byte) {
	if len(b) <= 2*n {
		return b, nil
	}
	h := n
	for h > 0 && !utf8.RuneStart(b[h]) {
		h--
	}
	t := len(b) - n
	for t < len(b) && !utf8.RuneStart(b[t]) {
		t++
	}
	return b[:h], b[t:]
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
//
// Open is bounded like the backend's ReadEvents (#5535): a stream that
// decompresses past MaxDecompressedBytes fails with
// ErrDecompressedTooLarge, and a line over MaxLineBytes with
// ErrLineTooLong.
func Open(r io.Reader) (manifest ManifestData, events []Line, trailer TrailerData, err error) {
	return open(r, MaxLineBytes, MaxDecompressedBytes)
}

func open(r io.Reader, maxLine int, maxTotal int64) (manifest ManifestData, events []Line, trailer TrailerData, err error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: gzip reader: %w", err)
	}
	defer func() { _ = zr.Close() }()

	// One byte past the bound tells "exactly at the bound" from "over it".
	raw, err := io.ReadAll(&io.LimitedReader{R: zr, N: maxTotal + 1})
	if err != nil {
		return manifest, nil, trailer, fmt.Errorf("bundle: read: %w", err)
	}
	if int64(len(raw)) > maxTotal {
		return manifest, nil, trailer, fmt.Errorf("bundle: read: %w", ErrDecompressedTooLarge)
	}

	// Split on '\n' but keep terminators — the content hash covers
	// each line's '\n' as written.
	var lines []Line
	var lineBytes [][]byte
	start := 0
	for i, b := range raw {
		if b != '\n' {
			if i-start >= maxLine {
				return manifest, nil, trailer,
					fmt.Errorf("bundle: line %d: %w", len(lines)+1, ErrLineTooLong)
			}
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

// writeLine emits one JSON line followed by '\n', the envelope encoding
// Pack uses. The tests build hand-crafted bundles with it.
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
