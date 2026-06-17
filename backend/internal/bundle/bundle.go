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

// EventKindPolicyEvent is the kind the runner stamps on side-channel
// policy observations that aren't the git_diff itself — notably the
// scope_drift event read by ExtractScopeDrift.
const EventKindPolicyEvent = "policy_event"

// EventKindVerifyRun is the kind the runner stamps on a committed-tree
// verify run (#651). Its payload carries the throwaway committed-tree
// WIP-commit head_sha, read by ExtractHeadSHA as the #797 implement-review
// idempotency key.
const EventKindVerifyRun = "verify_run"

// EventKindGateEvidence is the kind the runner stamps on the single
// synthesized gate-evidence event (#963): a bounded, pre-redacted
// digest of the stage's deterministic gate results (verify runs,
// verify summary, infra-flake retries, scope-enforcement facts,
// constraint violations) composed by the runner's composeGateEvidence
// immediately before packing, so both bundle variants carry it. Read
// by ExtractGateEvidence for the implement-review prompt.
const EventKindGateEvidence = "gate_evidence"

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
	// PushAndOpenPR signals an implement stage that will commit + push +
	// open a PR after the trace upload. The trace handler reads it to
	// forward-gate the implement stage's terminal transition onto the
	// /pull-request upload (#742), closing the zombie-run hole where a
	// commit/push failure stranded the run at review with a null PR. Older
	// bundles (and every non-PR-opening stage) omit it and decode to false,
	// preserving the prior trace-driven transition. Keep in lockstep with
	// runner/internal/bundle.ManifestData.
	PushAndOpenPR bool `json:"push_and_open_pr,omitempty"`
	// PushToSharedBranch signals a decomposed-child implement stage that will
	// commit + push onto the shared parent branch after the trace upload
	// (without opening its own PR). The trace handler reads it to forward-gate
	// the child stage's terminal transition onto the /pull-request upload
	// (#771), closing the decomposition-child analogue of the #742 zombie:
	// a child reaching terminal succeeded with no code on the shared branch
	// after a commit/push failure, which the childcompletion sweeper would
	// then consolidate into a PR silently missing that child's work. Older
	// bundles (and every non-child stage) omit it and decode to false. Keep
	// in lockstep with runner/internal/bundle.ManifestData.
	PushToSharedBranch bool `json:"push_to_shared_branch,omitempty"`
	// PushFixup signals a fix-up re-dispatch implement stage that will commit
	// onto the EXISTING PR branch after the trace upload (updating the open PR
	// rather than opening a new one). The trace handler reads it to forward-gate
	// the fix-up stage's terminal transition onto the /pull-request upload
	// (#794), closing the fix-up analogue of the #742/#771 zombies: a fix-up
	// re-dispatch reaching terminal succeeded after a commit/push/compile-gate
	// failure, so the implement re-review approves an unlanded diff and the #788
	// recovery never fires. Older bundles (and every non-fix-up stage) omit it
	// and decode to false. Keep in lockstep with
	// runner/internal/bundle.ManifestData.
	PushFixup bool `json:"push_fixup,omitempty"`
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

	// ErrNoHeadSHA means the bundle parsed cleanly but carried no
	// verify_run event with a non-empty head_sha (the common no-verify
	// case, or a bundle whose only verify_run events were gate-skipped /
	// infra-failed and so carry an empty head_sha). Returned by
	// ExtractHeadSHA so the caller can fail open — an absent head_sha must
	// never suppress the implement-review dispatch (#797). Mirrors
	// ErrNoDiffEvent.
	ErrNoHeadSHA = errors.New("bundle: no verify_run event with a head_sha found")

	// ErrNoGateEvidence means the bundle parsed cleanly but carried no
	// gate_evidence event — the ordinary case for older bundles (runners
	// predating the emitter) and for stages where no gate ran (the runner
	// composes nil when there is no verify_run/verify_summary/policy_event
	// to digest). Returned by ExtractGateEvidence so the caller can fail
	// open: an absent evidence event must never block or alter the
	// implement-review dispatch, only omit the prompt's Gate evidence
	// section. Mirrors ErrNoHeadSHA.
	ErrNoGateEvidence = errors.New("bundle: no gate_evidence event found")
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
// decode to an empty string (#585). Insertions/Deletions are likewise
// additive and `omitempty` (#1137): they carry the staged-diff numstat
// totals so the MCP server reports diff stats from the event rather than
// re-deriving them from the operator checkout's HEAD, which per-run
// worktree isolation no longer carries the run's commit.
type gitDiffPayload struct {
	Kind           string         `json:"kind"`
	BaseRef        string         `json:"base_ref"`
	Files          []gitDiffEntry `json:"files"`
	NumFiles       int            `json:"num_files"`
	Patch          string         `json:"patch,omitempty"`
	PatchTruncated bool           `json:"patch_truncated,omitempty"`
	Insertions     int            `json:"insertions,omitempty"`
	Deletions      int            `json:"deletions,omitempty"`
}

type gitDiffEntry struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// scopeDriftPayload mirrors the runner's scope_drift policy_event
// payload exactly. The emitter is computeAndEmitDiff
// (runner/cmd/fishhawk-runner/main.go), which builds it via
// agent.MakePayload(map[string]any{"check":"scope_drift",
// "outcome":"excluded","undeclared":[...]}). Like gitDiffPayload this
// is a lockstep runner↔backend wire contract, not a JSON Schema — the
// `check` / `undeclared` json tags MUST stay identical to the emitter
// or ExtractScopeDrift silently reads nothing.
type scopeDriftPayload struct {
	Check      string   `json:"check"`
	Outcome    string   `json:"outcome"`
	Undeclared []string `json:"undeclared"`
}

// verifyRunPayload mirrors the runner's verify_run event payload
// (verifyRunEvent in runner/cmd/fishhawk-runner/main.go). Only head_sha
// is read here; like the other payloads this is a lockstep runner↔backend
// wire contract, not a JSON Schema, so the `head_sha` json tag MUST stay
// identical to the emitter.
type verifyRunPayload struct {
	HeadSHA string `json:"head_sha"`
}

// GateEvidence mirrors the runner's gate_evidence event payload
// (composeGateEvidence in runner/cmd/fishhawk-runner/gateevidence.go).
// Like gitDiffPayload this is a lockstep runner↔backend wire contract,
// not a JSON Schema — every json tag here MUST stay identical to the
// composer or ExtractGateEvidence silently reads zero values. All
// free-text fields (verify output tails, summary/violation details)
// are pre-redacted by the runner via redaction.RedactDefault before
// the event is packed, so the evidence is safe to render into the
// review prompt even from the RAW bundle variant (the raw variant is
// what dispatches the implement review, before the redacted variant
// exists).
type GateEvidence struct {
	// VerifyRuns digests each committed-tree verify_run attempt:
	// command, exit, outcome classification, and a bounded output tail.
	VerifyRuns []VerifyRunEvidence `json:"verify_runs,omitempty"`
	// VerifySummary digests the once-per-stage verify_summary event.
	// Nil when the stage emitted none.
	VerifySummary *VerifySummaryEvidence `json:"verify_summary,omitempty"`
	// FlakeRetries counts verify_infra_flake_retry absorbs (#972).
	FlakeRetries int `json:"flake_retries,omitempty"`
	// ScopeFacts carries declared-vs-staged scope counts and the
	// drift-excluded path list. Nil when the runner had no scope data.
	ScopeFacts *ScopeFactsEvidence `json:"scope_facts,omitempty"`
	// PolicyViolations digests constraint-violation policy_events
	// (check + detail), excluding scope_drift which ScopeFacts carries.
	PolicyViolations []PolicyViolationEvidence `json:"policy_violations,omitempty"`
	// BindingAssertions digests the operator-declared binding-assertion
	// checks (#1171) the runner evaluated against the committed scope-only
	// tree, each with its satisfied verdict, so the implement review sees
	// which binding conditions were machine-verified. Nil when no
	// assertions were declared.
	BindingAssertions []BindingAssertionEvidence `json:"binding_assertions,omitempty"`
}

// BindingAssertionEvidence is one digested binding-assertion check (#1171):
// the operator-declared type/path/literal plus whether the committed tree
// satisfied it. Mirrors the runner's bindingAssertionEvidence — the json tags
// MUST stay identical to the composer, same lockstep wire contract as the
// parent payload.
type BindingAssertionEvidence struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Literal   string `json:"literal"`
	Satisfied bool   `json:"satisfied"`
}

// VerifyRunEvidence is one digested verify_run attempt. Field names
// follow the raw verify_run event payload (verifyRunEvent in
// runner/cmd/fishhawk-runner/main.go); OutputTail is the composer's
// bounded (last 30 lines, ~4KB cap), pre-redacted tail of the raw
// event's output — the raw verify_run keeps the full unredacted
// output for compliance, only this derived digest is prompt-bound.
type VerifyRunEvidence struct {
	Command  string `json:"command"`
	HeadSHA  string `json:"head_sha,omitempty"`
	TreeSHA  string `json:"tree_sha,omitempty"`
	ExitCode int    `json:"exit_code"`
	// Outcome is passed | failed | skipped, as classified by the gate.
	// On the skipped paths the tail carries the skip reason.
	Outcome    string `json:"outcome"`
	OutputTail string `json:"output_tail,omitempty"`
	// TailTruncated reports the composer cut the tail to its line/byte
	// bounds — the prompt flags it so a reviewer knows the output is
	// partial.
	TailTruncated bool `json:"tail_truncated,omitempty"`
	// Superseded is true when a LATER verify_run iteration superseded this
	// one in the verify-fix loop (#1205): an earlier absorbed iteration that
	// ran on a stale tree and was followed by a passing terminal run, so it
	// is NOT the committed-tree result. Only the LAST verify_run is the
	// terminal/authoritative attempt and is never marked. Mirrors the
	// runner's verifyRunEvidence — the json tag MUST stay identical to the
	// composer, same lockstep wire contract as the parent payload. Additive/
	// omitempty: older bundles decode to false (not superseded).
	Superseded bool `json:"superseded,omitempty"`
}

// VerifySummaryEvidence digests the verify_summary event (#804).
type VerifySummaryEvidence struct {
	Outcome       string `json:"outcome"`
	Iterations    int    `json:"iterations"`
	MaxIterations int    `json:"max_iterations"`
	Detail        string `json:"detail,omitempty"`
}

// ScopeFactsEvidence carries the scope-enforcement facts: how many
// files the approved plan declared, how many the runner staged into
// the commit (a pointer so "no git_diff event ran" stays
// distinguishable from a real zero-file diff), and the scope_drift
// undeclared list (paths the agent dirtied that were EXCLUDED from
// the commit). UndeclaredCategorized is the per-path A/B
// categorization of the same list (#991); older runners don't emit it,
// so it decodes nil and UndeclaredPaths stays the authoritative list.
type ScopeFactsEvidence struct {
	DeclaredFiles         int                 `json:"declared_files"`
	StagedFiles           *int                `json:"staged_files,omitempty"`
	UndeclaredPaths       []string            `json:"undeclared_paths,omitempty"`
	UndeclaredCategorized []DriftPathEvidence `json:"undeclared_categorized,omitempty"`
}

// DriftPathEvidence is one categorized scope-drift path (#991):
// category "A" is an agent edit to a tracked file excluded from the
// commit (the pushed head may be missing a required change); "B" is a
// file created out of scope. Disposition is what enforcement did with
// the path ("excluded_from_commit" | "would_fail_loud"). Mirrors the
// runner's driftPathEvidence — the json tags MUST stay identical to
// the composer, same lockstep wire contract as the parent payload.
type DriftPathEvidence struct {
	Path        string `json:"path"`
	Category    string `json:"category"`
	Disposition string `json:"disposition,omitempty"`
}

// PolicyViolationEvidence is one digested constraint-violation
// policy_event (check/constraint identifiers, pre-redacted detail,
// and the violating files when the event named them).
type PolicyViolationEvidence struct {
	Check      string   `json:"check"`
	Constraint string   `json:"constraint,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Files      []string `json:"files,omitempty"`
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
//
// Invariant: the authoritative git_diff is the LAST one emitted. The
// runner re-emits a fresh scope-only git_diff after a verify-fix loop
// reinvokes the agent (#870), so the later event reflects the reconciled
// committed scope-only tree the PR ships — the diff both the implement
// review and policy re-eval must see. Single-event bundles (every bundle
// without a reconciling reinvoke) are unaffected: last == first.
func ExtractDiff(bundleBytes []byte) (policy.Diff, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return policy.Diff{}, err
	}
	var (
		out   policy.Diff
		found bool
	)
	for _, line := range lines {
		if line.Kind != EventKindGitDiff {
			continue
		}
		var payload gitDiffPayload
		if err := json.Unmarshal(line.Data, &payload); err != nil {
			return policy.Diff{}, fmt.Errorf("bundle: parse git_diff payload: %w", err)
		}
		d := policy.Diff{
			ChangedFiles: make([]policy.ChangedFile, 0, len(payload.Files)),
			// Additive content for the implement-review prompt only;
			// the policy engine never reads Patch. Empty when the
			// bundle predates the field or the runner couldn't compute
			// the patch (#585).
			Patch: payload.Patch,
		}
		for _, f := range payload.Files {
			d.ChangedFiles = append(d.ChangedFiles, policy.ChangedFile{
				Path:   f.Path,
				Status: policy.Status(f.Status),
			})
		}
		out = d
		found = true
	}
	if !found {
		return policy.Diff{}, ErrNoDiffEvent
	}
	return out, nil
}

// ExtractScopeDrift returns the `undeclared` path list carried in the
// bundle's scope_drift policy_event, or (nil, nil) when no such event
// is present. Drift is the exception, not the norm — the runner's
// computeAndEmitDiff (runner/cmd/fishhawk-runner/main.go) emits the
// event only when StageScoped finds dirty-but-undeclared paths
// (len(drift) > 0), so an absent event is the ordinary no-drift case
// and is NOT an error. Only a corrupt gzip frame / malformed line
// propagates as an error.
//
// These paths were created or modified by the implement stage but
// excluded from the scope-bounded git_diff event, so the
// implement-review path threads them into the reviewer prompt flagged
// "operator may stage" — a required test/doc landing here is expected
// to ship even though it is absent from the scoped diff (#695). Like
// ExtractDiff this is a lockstep runner↔backend wire contract; the
// scopeDriftPayload tags move with the emitter.
func ExtractScopeDrift(bundleBytes []byte) ([]string, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		if line.Kind != EventKindPolicyEvent {
			continue
		}
		var payload scopeDriftPayload
		if err := json.Unmarshal(line.Data, &payload); err != nil {
			return nil, fmt.Errorf("bundle: parse policy_event payload: %w", err)
		}
		if payload.Check != "scope_drift" {
			continue
		}
		return payload.Undeclared, nil
	}
	return nil, nil
}

// ExtractHeadSHA returns the head_sha carried in the bundle's verify_run
// event (#651 committed-tree verify), or ErrNoHeadSHA if the bundle parsed
// cleanly but carried no verify_run event with a non-empty head_sha. It is
// the #797 implement-review idempotency key: the raw and redacted variants
// of one pack carry the IDENTICAL verify_run head_sha (the event is emitted
// once per stage execution and copied verbatim into both variants —
// redaction strips secrets, not a git SHA), while a FixupStage re-pack runs
// a new committed-tree verify on a new diff → new WIP commit → a NEW
// head_sha. That is exactly the discrimination the (stage_id, head_sha) key
// needs to dedup a retried raw upload while still re-reviewing a fix-up.
//
// The returned SHA is the throwaway committed-tree WIP-commit SHA (the
// runner reset --soft's it away immediately), NOT the final pushed PR head
// SHA — fine for dedup since it is deterministic across raw+redacted of one
// pack and distinct per re-pack.
//
// The first NON-EMPTY head_sha wins: the gate-skipped / worktree-add-failure
// / infra-failure verify_run paths emit a verify_run with an empty head_sha,
// and a multi-iteration fix loop or a skipped first gate can place such an
// empty-head_sha event ahead of a real one. Returning the empty value would
// needlessly disable the dedup, so we skip empties and return the first real
// SHA (fail-open intent: only a genuinely head_sha-less bundle degrades to
// the variant gate). Only a corrupt gzip frame / malformed line propagates
// as an error.
func ExtractHeadSHA(bundleBytes []byte) (string, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return "", err
	}
	for _, line := range lines {
		if line.Kind != EventKindVerifyRun {
			continue
		}
		var payload verifyRunPayload
		if err := json.Unmarshal(line.Data, &payload); err != nil {
			return "", fmt.Errorf("bundle: parse verify_run payload: %w", err)
		}
		if payload.HeadSHA != "" {
			return payload.HeadSHA, nil
		}
	}
	return "", ErrNoHeadSHA
}

// ExtractGateEvidence returns the digested gate results carried in the
// bundle's gate_evidence event (#963), or the zero value plus
// ErrNoGateEvidence when the bundle parsed cleanly but carried none —
// the ordinary case for older bundles and no-gate stages, NOT a hard
// error (mirrors ErrNoHeadSHA's fail-open contract: absent evidence
// only omits the review prompt's Gate evidence section, never blocks
// the dispatch). Only a corrupt gzip frame / malformed line propagates
// as a different error.
//
// The runner composes exactly one gate_evidence event per pack
// (appended immediately before PackBytes, so raw and redacted variants
// carry the identical event); the LAST one wins here for symmetry with
// ExtractDiff's authoritative-is-last rule should that ever change.
func ExtractGateEvidence(bundleBytes []byte) (GateEvidence, error) {
	lines, err := ReadEvents(bundleBytes)
	if err != nil {
		return GateEvidence{}, err
	}
	var (
		out   GateEvidence
		found bool
	)
	for _, line := range lines {
		if line.Kind != EventKindGateEvidence {
			continue
		}
		var payload GateEvidence
		if err := json.Unmarshal(line.Data, &payload); err != nil {
			return GateEvidence{}, fmt.Errorf("bundle: parse gate_evidence payload: %w", err)
		}
		out = payload
		found = true
	}
	if !found {
		return GateEvidence{}, ErrNoGateEvidence
	}
	return out, nil
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
