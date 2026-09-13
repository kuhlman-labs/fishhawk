package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Acceptance transcript capture (E72.5 / #3329).
//
// The acceptance agent writes ONE JSON object — the per-criterion transcript
// (seed applied, each request as method/path/body, each response as
// status/body, the assertion, its outcome, wall time) — to the run/stage-keyed
// sidecar acceptanceTranscriptPath. After the verdict validates, the runner:
//
//   - reads the sidecar through readSidecarBounded (E64.12 / #3106) with the
//     #3142 CHECKED-removal shape on an over-ceiling file;
//   - strictly decodes it (DisallowUnknownFields, single object);
//   - truncates request/response bodies to acceptanceTranscriptMaxBodyBytes
//     with a trailing marker BEFORE validation (bodies are stored-only, never
//     rendered, so bounding them is safe; ids and paths are NEVER truncated —
//     they are REJECTED, matching the backend);
//   - validates it with validateAcceptanceTranscript, the twin of the backend
//     validator (backend/internal/server/acceptance_transcript.go) plus the
//     SECOND-LAYER served-criterion-id membership;
//   - redacts the bytes through redaction.RedactDefault (the verdict's posture),
//     re-validates the redacted bytes, and bounds them at
//     acceptanceTranscriptShipMaxBytes;
//   - ships them (ShipAcceptanceTranscript) BEFORE the verdict and injects the
//     backend-minted {artifact_id, content_hash} ref into the redacted verdict
//     (upload.InjectTranscript).
//
// NONE of these is a stage failure: the VERDICT is the contract and is
// authoritative; the transcript is descriptive evidence. Every branch that
// drops the transcript emits a named non-fatal runner-log event
// (acceptance_transcript_missing / _oversize / _unremovable / _invalid /
// _redacted / _captured / _upload_failed / _shipped) and the verdict ships
// exactly as it would have without the transcript.

// acceptanceTranscriptShipMaxBytes bounds the POST-redaction transcript bytes
// the runner ships: 256 KiB, the backend endpoint's request cap. A plain const,
// never test-injectable (the #3106 rule); pinned by
// TestAcceptanceTranscriptShipMaxBytesValue.
const acceptanceTranscriptShipMaxBytes = 256 * 1024

// Per-field bounds, byte-identical to the backend twin's consts.
const (
	acceptanceTranscriptMaxCriteria       = 100
	acceptanceTranscriptMaxRequests       = 200
	acceptanceTranscriptMaxIDBytes        = 128
	acceptanceTranscriptMaxSeedBytes      = 200
	acceptanceTranscriptMaxPathBytes      = 512
	acceptanceTranscriptMaxBodyBytes      = 4096
	acceptanceTranscriptMaxAssertionBytes = 2000
)

var (
	// acceptanceTranscriptIDRe mirrors the backend: the plan-schema
	// criterion-id pattern, optionally under the replayed-scenario prefix.
	acceptanceTranscriptIDRe = regexp.MustCompile(`^(scenario:issue-[0-9]+/)?[a-z0-9][a-z0-9-]*$`)
	// acceptanceTranscriptSeedRe mirrors the backend catalog-name charset.
	acceptanceTranscriptSeedRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// acceptanceTranscriptPathRe mirrors the backend: RFC 3986 pchar + '/' +
	// '?' — no whitespace, control byte, backtick or '|'.
	acceptanceTranscriptPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)
)

var acceptanceTranscriptMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {}, "OPTIONS": {},
}

var acceptanceTranscriptOutcomes = map[string]struct{}{
	"passed": {}, "failed": {}, "skipped": {}, "undecidable": {},
}

// acceptanceTranscriptPath is the run/stage-keyed sidecar the acceptance
// prompt names for the transcript. KEYED ONLY — there is no legacy fixed path
// (the transcript is new, so no older prompt ever named one). MUST stay
// byte-identical to prompt.AcceptanceTranscriptPath
// (backend/internal/prompt/prompt.go), pinned from both sides. The
// `fishhawk-acceptance-*-*.json` glob `scripts/dev sweep` already prunes
// matches it, so no scripts/dev change is needed.
func acceptanceTranscriptPath(runID, stageID string) string {
	return filepath.Join(acceptanceVerdictDir, fmt.Sprintf("fishhawk-acceptance-transcript-%s-%s.json", runID, stageID))
}

// truncateAcceptanceTranscriptBodies bounds every request_body /
// response_body to acceptanceTranscriptMaxBodyBytes, appending a
// `...[truncated N bytes]` marker naming the bytes dropped. Applied BEFORE
// validation so an honest oversize body is bounded rather than dropped. The
// truncation point is pulled back to a rune boundary so a multi-byte UTF-8
// sequence is never split. Returns how many bodies were truncated.
func truncateAcceptanceTranscriptBodies(tr *upload.AcceptanceTranscript) int {
	n := 0
	clip := func(s string) string {
		if len(s) <= acceptanceTranscriptMaxBodyBytes {
			return s
		}
		// Reserve room for the marker inside the cap.
		const markerMax = len("...[truncated 2147483647 bytes]")
		cut := acceptanceTranscriptMaxBodyBytes - markerMax
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		n++
		return s[:cut] + fmt.Sprintf("...[truncated %d bytes]", len(s)-cut)
	}
	for i := range tr.Criteria {
		for j := range tr.Criteria[i].Requests {
			r := &tr.Criteria[i].Requests[j]
			r.RequestBody = clip(r.RequestBody)
			r.ResponseBody = clip(r.ResponseBody)
		}
	}
	return n
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// validateAcceptanceTranscript enforces the CLOSED transcript shape, MIRRORING
// the backend twin's every bound (same id grammar, seed charset, method enum,
// path charset + 512, status range, body caps, 200 requests, 100 criteria,
// assertion cap) so a runner-accepted transcript is backend-acceptable, PLUS
// the SECOND-LAYER served-id membership: when servedIDs is non-empty every
// criteria[].id must be a member. The backend grammar is what establishes the
// render-safety property for the independently callable endpoint; the
// membership check is the runner's own layer against a transcript pinning
// evidence to a criterion the approved plan never declared. Every rule is
// fail-closed — REJECT, never truncate or sanitise — and names its field.
func validateAcceptanceTranscript(tr *upload.AcceptanceTranscript, servedIDs []string) error {
	if len(tr.Criteria) == 0 {
		return errors.New("criteria: required, at least one entry")
	}
	if len(tr.Criteria) > acceptanceTranscriptMaxCriteria {
		return fmt.Errorf("criteria: %d entries exceeds the %d cap", len(tr.Criteria), acceptanceTranscriptMaxCriteria)
	}
	if tr.TargetURL != "" && !strings.HasPrefix(tr.TargetURL, "http://") && !strings.HasPrefix(tr.TargetURL, "https://") {
		return errors.New("target_url: must be an http(s) URL")
	}
	served := make(map[string]struct{}, len(servedIDs))
	for _, id := range servedIDs {
		served[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tr.Criteria))
	for i, c := range tr.Criteria {
		at := fmt.Sprintf("criteria[%d]", i)
		if c.ID == "" {
			return fmt.Errorf("%s.id: required", at)
		}
		if len(c.ID) > acceptanceTranscriptMaxIDBytes {
			return fmt.Errorf("%s.id: %d bytes exceeds the %d cap", at, len(c.ID), acceptanceTranscriptMaxIDBytes)
		}
		if !acceptanceTranscriptIDRe.MatchString(c.ID) {
			return fmt.Errorf("%s.id: must match %s", at, acceptanceTranscriptIDRe.String())
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("%s.id: duplicate id %q", at, c.ID)
		}
		seen[c.ID] = struct{}{}
		if len(served) > 0 {
			if _, ok := served[c.ID]; !ok {
				return fmt.Errorf("%s.id: %q is not a served criterion id", at, c.ID)
			}
		}
		if c.Seed != "" {
			if len(c.Seed) > acceptanceTranscriptMaxSeedBytes {
				return fmt.Errorf("%s.seed: %d bytes exceeds the %d cap", at, len(c.Seed), acceptanceTranscriptMaxSeedBytes)
			}
			if !acceptanceTranscriptSeedRe.MatchString(c.Seed) {
				return fmt.Errorf("%s.seed: must match %s", at, acceptanceTranscriptSeedRe.String())
			}
		}
		if len(c.Requests) > acceptanceTranscriptMaxRequests {
			return fmt.Errorf("%s.requests: %d entries exceeds the %d cap", at, len(c.Requests), acceptanceTranscriptMaxRequests)
		}
		for j, r := range c.Requests {
			rat := fmt.Sprintf("%s.requests[%d]", at, j)
			if _, ok := acceptanceTranscriptMethods[r.Method]; !ok {
				return fmt.Errorf("%s.method: %q is not one of GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS", rat, r.Method)
			}
			if len(r.Path) > acceptanceTranscriptMaxPathBytes {
				return fmt.Errorf("%s.path: %d bytes exceeds the %d cap", rat, len(r.Path), acceptanceTranscriptMaxPathBytes)
			}
			if !acceptanceTranscriptPathRe.MatchString(r.Path) {
				return fmt.Errorf("%s.path: must match %s", rat, acceptanceTranscriptPathRe.String())
			}
			if len(r.RequestBody) > acceptanceTranscriptMaxBodyBytes {
				return fmt.Errorf("%s.request_body: %d bytes exceeds the %d cap", rat, len(r.RequestBody), acceptanceTranscriptMaxBodyBytes)
			}
			if r.Status < 100 || r.Status > 599 {
				return fmt.Errorf("%s.status: %d is outside 100..599", rat, r.Status)
			}
			if len(r.ResponseBody) > acceptanceTranscriptMaxBodyBytes {
				return fmt.Errorf("%s.response_body: %d bytes exceeds the %d cap", rat, len(r.ResponseBody), acceptanceTranscriptMaxBodyBytes)
			}
			if r.ElapsedMs < 0 {
				return fmt.Errorf("%s.elapsed_ms: must be >= 0", rat)
			}
		}
		if len(c.Assertion) > acceptanceTranscriptMaxAssertionBytes {
			return fmt.Errorf("%s.assertion: %d bytes exceeds the %d cap", at, len(c.Assertion), acceptanceTranscriptMaxAssertionBytes)
		}
		if _, ok := acceptanceTranscriptOutcomes[c.Outcome]; !ok {
			return fmt.Errorf("%s.outcome: %q is not one of passed|failed|skipped|undecidable", at, c.Outcome)
		}
		if c.WallMs < 0 {
			return fmt.Errorf("%s.wall_ms: must be >= 0", at)
		}
	}
	return nil
}

// decodeAcceptanceTranscript strictly decodes ONE transcript object
// (DisallowUnknownFields, no trailing data).
func decodeAcceptanceTranscript(raw []byte) (*upload.AcceptanceTranscript, error) {
	var tr upload.AcceptanceTranscript
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("body must contain a single JSON object")
	}
	return &tr, nil
}

// captureAcceptanceTranscript reads, bounds, validates and redacts the
// transcript sidecar at keyedPath. It returns the redacted bytes to ship, or
// nil when there is nothing shippable — and NEVER an error: every drop is
// reported through warn (nil-tolerant) as a named event and the caller ships
// the verdict unchanged. The branches, each with its own event:
//
//   - not-exist                          → nil, acceptance_transcript_missing
//   - over the readSidecarBounded ceiling → nil, acceptance_transcript_oversize,
//     the file REMOVED with the #3142 checked shape (a failed removal emits
//     acceptance_transcript_unremovable BESIDE the oversize event, never
//     instead of it)
//   - other read error / empty file      → nil, acceptance_transcript_missing
//   - decode or validation failure       → nil, acceptance_transcript_invalid
//   - redaction hits                     → acceptance_transcript_redacted
//   - post-redaction re-validation fails → nil, acceptance_transcript_invalid
//     (a redaction placeholder can lengthen a body past its cap)
//   - post-redaction > 256 KiB           → nil, acceptance_transcript_oversize
//   - success                            → bytes, acceptance_transcript_captured
func captureAcceptanceTranscript(keyedPath string, servedIDs []string, warn func(event, detail string)) []byte {
	emit := func(event, detail string) {
		if warn != nil {
			warn(event, detail)
		}
	}
	raw, err := readSidecarBounded(keyedPath)
	if err != nil {
		if errors.Is(err, errSidecarTooLarge) {
			// Removal is CHECKED (#3142): a readable-but-unremovable oversize
			// transcript would otherwise survive silently.
			rmErr := os.Remove(keyedPath)
			emit("acceptance_transcript_oversize", "transcript exceeds size ceiling: "+keyedPath)
			if rmErr != nil {
				emit("acceptance_transcript_unremovable", "transcript removal failed: "+keyedPath+": "+rmErr.Error())
			}
			return nil
		}
		if os.IsNotExist(err) {
			emit("acceptance_transcript_missing", "no transcript sidecar at "+keyedPath)
			return nil
		}
		emit("acceptance_transcript_missing", "transcript read failed: "+keyedPath+": "+err.Error())
		return nil
	}
	if len(raw) == 0 {
		emit("acceptance_transcript_missing", "empty transcript sidecar at "+keyedPath)
		return nil
	}
	tr, err := decodeAcceptanceTranscript(raw)
	if err != nil {
		emit("acceptance_transcript_invalid", err.Error())
		return nil
	}
	if n := truncateAcceptanceTranscriptBodies(tr); n > 0 {
		emit("acceptance_transcript_bodies_truncated", fmt.Sprintf("%d bodies truncated to %d bytes", n, acceptanceTranscriptMaxBodyBytes))
	}
	if err := validateAcceptanceTranscript(tr, servedIDs); err != nil {
		emit("acceptance_transcript_invalid", err.Error())
		return nil
	}
	canonical, err := json.Marshal(tr)
	if err != nil {
		emit("acceptance_transcript_invalid", "marshal: "+err.Error())
		return nil
	}
	redacted, hits := redaction.RedactDefault(canonical)
	if len(hits) > 0 {
		hitsJSON, _ := json.Marshal(hits)
		emit("acceptance_transcript_redacted", string(hitsJSON))
	}
	// Re-validate the REDACTED bytes: a placeholder can be longer than the
	// secret it replaced and push a body past the cap the backend enforces.
	if rtr, rerr := decodeAcceptanceTranscript(redacted); rerr != nil {
		emit("acceptance_transcript_invalid", "post-redaction: "+rerr.Error())
		return nil
	} else if verr := validateAcceptanceTranscript(rtr, servedIDs); verr != nil {
		emit("acceptance_transcript_invalid", "post-redaction: "+verr.Error())
		return nil
	}
	if len(redacted) > acceptanceTranscriptShipMaxBytes {
		emit("acceptance_transcript_oversize", fmt.Sprintf("post-redaction transcript is %d bytes, over the %d ship bound", len(redacted), acceptanceTranscriptShipMaxBytes))
		return nil
	}
	emit("acceptance_transcript_captured", fmt.Sprintf("bytes=%d criteria=%d", len(redacted), len(tr.Criteria)))
	return redacted
}
