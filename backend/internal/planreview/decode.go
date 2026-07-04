package planreview

import (
	"bytes"
	"encoding/json"
)

// DecodeVerdict unmarshals a model-emitted verdict body into a ReviewVerdict,
// tolerating two common classes of model output that strict JSON rejects.
//
// As of #1324 this is the FALLBACK decode path, not the primary one. The
// first-class structured-output backends constrain the model to emit
// schema-guaranteed JSON directly — the Anthropic adapter sets
// OutputConfig.Format=json_schema and the codex adapter passes
// `--output-schema`, both built from the single VerdictSchema() source of
// truth (see schema.go). DecodeVerdict remains the documented fallback for
// every non-constrained path: claudecode (whose CLI exposes no response-schema
// flag), and any error or unconstrained response from the constrained backends
// where the model still returns free-text JSON. Its conservative fence-strip +
// escape-repair below keeps those paths working unchanged.
//
// First, it strips a surrounding markdown code fence (```json … ``` or a bare
// ``` … ```) via stripCodeFence — reviewer models routinely wrap their JSON in
// a fence, and the leading backtick fails strict decode before any escape
// repair can run. Stripping is conservative: it only acts when the trimmed
// body actually starts with a triple-backtick fence, leaving unfenced input
// byte-for-byte unchanged.
//
// Second, it tolerates the common class of invalid-JSON backslash escapes that
// models produce when they quote code in the verdict body — e.g. a regex like
// `ghs_[A-Za-z0-9_.\-]{36,}` or a path/identifier escape (`\-`, `\d`, `\w`,
// `\.`) that is valid in Go/regex but is NOT a legal JSON string escape.
//
// After de-fencing it attempts a strict json.Unmarshal, preserving today's
// behavior for well-formed output. Only on failure does it run sanitizeEscapes
// and retry. The escape repair is intentionally conservative:
//   - It never alters an already-valid escape: `\"`, `\\`, `\/`, `\b`, `\f`,
//     `\n`, `\r`, `\t`, and `\uXXXX` (four hex digits) are consumed intact, so
//     well-formed verdicts round-trip through the strict path untouched.
//   - It never masks a non-escape decode failure: if the sanitized retry still
//     fails, the ORIGINAL strict-decode error is returned unchanged so
//     genuinely-malformed output keeps its precise diagnostic.
func DecodeVerdict(raw []byte) (ReviewVerdict, error) {
	raw = stripCodeFence(raw)
	// Prose before and/or after the JSON object — a reviewer model routinely
	// narrates ("The plan looks solid. Here is my verdict:") around the object,
	// whose leading non-'{' byte fails strict decode before any escape repair
	// can run. This is the E32.9 prose-prefix failure (#1576): claudecode's CLI
	// exposes no response-schema flag, so the model is free to emit prose and
	// the strict decoder gave up with `invalid character 'T'`. firstJSONObject
	// extracts the first balanced top-level object; when it finds none raw is
	// left unchanged so a genuinely-undecodable body keeps its precise strict
	// error below.
	if obj := firstJSONObject(raw); obj != nil {
		raw = obj
	}

	var verdict ReviewVerdict
	strictErr := json.Unmarshal(raw, &verdict)
	if strictErr == nil {
		return verdict, nil
	}

	var repaired ReviewVerdict
	if err := json.Unmarshal(sanitizeEscapes(raw), &repaired); err != nil {
		// The sanitizer could not rescue this input — it is malformed for a
		// reason other than a stray backslash. Return the original strict
		// error so the caller's diagnostic points at the real fault.
		return ReviewVerdict{}, strictErr
	}
	return repaired, nil
}

// stripCodeFence removes a surrounding markdown code fence from a model-emitted
// verdict body, returning the inner content. Reviewer models commonly wrap
// their JSON in a fence (```json … ``` or a bare ``` … ```), whose leading
// backtick fails strict JSON decode before any escape repair can run.
//
// It is deliberately conservative: it only acts when the whitespace-trimmed
// input actually begins with a triple-backtick fence. Anything else — most
// importantly a well-formed JSON object whose string values merely contain
// backticks — is returned unchanged. The opening fence line (including an
// optional info string like `json`) and a trailing ``` line are dropped; the
// body between them is returned verbatim.
func stripCodeFence(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("```")) {
		return raw
	}

	// Drop the opening fence line (the ``` plus any info string such as
	// `json`, up to and including the first newline).
	rest := trimmed[3:]
	if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	} else {
		// No newline after the opener — there is no fenced body to extract.
		return raw
	}

	// Drop a trailing ``` fence, tolerating trailing whitespace after it.
	body := bytes.TrimRight(rest, " \t\r\n")
	if idx := bytes.LastIndex(body, []byte("```")); idx >= 0 {
		body = body[:idx]
	} else {
		// Opening fence with no closing fence — not a well-formed surrounding
		// fence, so leave the original input untouched.
		return raw
	}

	return body
}

// sanitizeEscapes rewrites every backslash that does NOT introduce a legal JSON
// string escape into a doubled backslash, so it survives json.Unmarshal as a
// literal backslash. It walks the whole buffer rather than tracking in/out of
// string context: well-formed JSON carries no backslash outside a string
// literal, so the transform is a no-op there and the implementation stays small
// and auditable.
//
// Crucially it CONSUMES legal escapes rather than merely detecting them: on a
// legal two-character escape (or a well-formed `\uXXXX`) it advances past the
// entire escape, so the second byte of a `\\` is never re-examined as a lone
// backslash.
func sanitizeEscapes(raw []byte) []byte {
	out := make([]byte, 0, len(raw))
	i := 0
	for i < len(raw) {
		c := raw[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}

		// c == '\\'. A trailing backslash with no following byte is lone.
		if i+1 >= len(raw) {
			out = append(out, '\\', '\\')
			i++
			continue
		}

		switch next := raw[i+1]; next {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			// Legal two-character escape — consume BOTH bytes so the second
			// backslash of a `\\` pair is not re-processed as lone.
			out = append(out, '\\', next)
			i += 2
		case 'u':
			// Legal only when followed by exactly four hex digits.
			if i+5 < len(raw) && isHex(raw[i+2]) && isHex(raw[i+3]) && isHex(raw[i+4]) && isHex(raw[i+5]) {
				out = append(out, raw[i:i+6]...)
				i += 6
			} else {
				out = append(out, '\\', '\\')
				i++
			}
		default:
			// Lone backslash before an illegal escape char — double it.
			out = append(out, '\\', '\\')
			i++
		}
	}
	return out
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// firstJSONObject returns the first balanced top-level JSON object in b — from
// the first '{' through its matching '}' — tolerating prose before and after
// it. The scan is string-literal-aware and escape-aware: braces (and quotes)
// inside a JSON string value do not affect the depth count, so an object whose
// string values contain '{' or '}' is extracted whole. Returns nil when b holds
// no '{' or the object is unbalanced (no matching close), in which case
// DecodeVerdict falls back to strict-decoding the original body so the strict
// error is preserved.
//
// This is a deliberate twin of refinement.firstJSONObject (backend/internal/
// refinement/decode.go), which cites the same E32.9 prose-prefix lesson. The
// two copies live in different packages; keep the string-literal/escape scan in
// sync if either changes. The duplication is scoped by design (see #1576): a
// small self-contained string utility, cross-referenced here to bound drift.
func firstJSONObject(b []byte) []byte {
	start := bytes.IndexByte(b, '{')
	if start < 0 {
		return nil
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : i+1]
			}
		}
	}
	return nil
}
