package planreview

import "encoding/json"

// DecodeVerdict unmarshals a model-emitted verdict body into a ReviewVerdict,
// tolerating the common class of invalid-JSON backslash escapes that models
// produce when they quote code in the verdict body — e.g. a regex like
// `ghs_[A-Za-z0-9_.\-]{36,}` or a path/identifier escape (`\-`, `\d`, `\w`,
// `\.`) that is valid in Go/regex but is NOT a legal JSON string escape.
//
// It first attempts a strict json.Unmarshal, preserving today's behavior for
// well-formed output. Only on failure does it run sanitizeEscapes and retry.
// The helper is intentionally conservative:
//   - It never alters an already-valid escape: `\"`, `\\`, `\/`, `\b`, `\f`,
//     `\n`, `\r`, `\t`, and `\uXXXX` (four hex digits) are consumed intact, so
//     well-formed verdicts round-trip through the strict path untouched.
//   - It never masks a non-escape decode failure: if the sanitized retry still
//     fails, the ORIGINAL strict-decode error is returned unchanged so
//     genuinely-malformed output keeps its precise diagnostic.
func DecodeVerdict(raw []byte) (ReviewVerdict, error) {
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
