package refinement

import (
	"bytes"
	"encoding/json"
)

// DecodeDraft parses an agent-emitted draft body into an EpicDraft, tolerating
// the two common classes of model output that a bare strict json.Unmarshal
// rejects (the E32.9 prose-prefix lesson):
//
//  1. A surrounding markdown code fence (```json … ``` or a bare ``` … ```),
//     which stripCodeFence removes conservatively.
//  2. Prose before and/or after the JSON object — a model routinely narrates
//     ("Here is the draft:") around the object. firstJSONObject extracts the
//     first balanced top-level object via a string-literal-aware brace scan,
//     ignoring braces inside string values.
//
// After extraction it strict-decodes with DisallowUnknownFields so the agent's
// field set stays CLOSED (the #1543/#1567 lesson: an unknown or extra field is
// a contract violation, not tolerated drift). DisallowUnknownFields applies to
// every nested object the decoder walks, so an unknown field inside a
// children[] element is rejected too.
//
// DecodeDraft does shape only — the semantic checks (non-empty fields,
// dependency graph) live in EpicDraft.Validate. On any decode failure it
// returns the strict-decode error unchanged so the caller's diagnostic points
// at the real fault (mirroring planreview.DecodeVerdict's
// diagnostics-preserving pattern).
func DecodeDraft(raw []byte) (EpicDraft, error) {
	body := stripCodeFence(raw)
	if obj := firstJSONObject(body); obj != nil {
		body = obj
	}

	var draft EpicDraft
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&draft); err != nil {
		return EpicDraft{}, err
	}
	return draft, nil
}

// stripCodeFence removes a surrounding markdown code fence from a model-emitted
// body, returning the inner content. It is deliberately conservative: it only
// acts when the whitespace-trimmed input actually begins with a triple-backtick
// fence, so an unfenced body (or a JSON string value that merely contains
// backticks) is returned byte-for-byte unchanged. The opening fence line
// (including an optional info string like `json`) and a trailing ``` line are
// dropped; the body between them is returned verbatim.
func stripCodeFence(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("```")) {
		return raw
	}

	rest := trimmed[3:]
	if nl := bytes.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	} else {
		// No newline after the opener — there is no fenced body to extract.
		return raw
	}

	body := bytes.TrimRight(rest, " \t\r\n")
	if idx := bytes.LastIndex(body, []byte("```")); idx >= 0 {
		return body[:idx]
	}
	// Opening fence with no closing fence — not a well-formed surrounding
	// fence, so leave the original input untouched.
	return raw
}

// firstJSONObject returns the first balanced top-level JSON object in b —
// from the first '{' through its matching '}' — tolerating prose before and
// after it. The scan is string-literal-aware: braces inside a JSON string
// value (and an escaped quote within it) do not affect the depth count, so an
// object whose string values contain '{' or '}' is extracted whole. Returns
// nil when b holds no '{' or the object is unbalanced (no matching close), in
// which case DecodeDraft falls back to strict-decoding the original body so
// the strict error is preserved.
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
