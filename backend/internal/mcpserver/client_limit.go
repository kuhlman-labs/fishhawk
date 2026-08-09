package mcpserver

import (
	"encoding/json"
	"log"
	"math"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// Client tool-result-limit discovery (E48.76 / #2509)
// ---------------------------------------------------------------------------
//
// Every bounded MCP surface (bound.go / bound_surfaces.go) resolves its byte
// budget through a THREE-RUNG discovery ladder rather than assuming the measured
// 32 KiB default. The issue's precedence — ADVERTISED > CONFIGURED > DEFAULT
// (BINDING CONDITION 1, #2509) — is: a limit the client advertised on the
// initialize handshake wins, then an operator env override, then the ADR-077
// measured constant. When a client advertises NOTHING (the common case today)
// the resolved budget is byte-identical to what #2508/#2510 ship.

// budgetSource names which rung of the discovery ladder decided the effective
// response byte budget. It travels WITH the byte count in a responseBudget so no
// call site can emit a budget while reporting an unrelated source.
type budgetSource string

const (
	// sourceAdvertised: the client advertised a tool-result limit on the
	// initialize handshake and it was honoured as-is.
	sourceAdvertised budgetSource = "advertised"
	// sourceConfigured: an operator env override decided the budget.
	sourceConfigured budgetSource = "configured"
	// sourceDefault: the ADR-077 measured 32 KiB constant.
	sourceDefault budgetSource = "default"
	// sourceAdvertisedBelowFloor marks a VALID advertised limit that fell BELOW
	// the convergence floor: convergence cannot promise less than the floor, so
	// the floor is emitted, but the source SAYS SO on the wire rather than
	// presenting the floor as honouring the smaller advertisement (BINDING
	// CONDITION 2 / #2509). Under-delivering is acceptable; misreporting is not.
	sourceAdvertisedBelowFloor budgetSource = "advertised_below_floor"
)

// validBudgetSource reports whether s is one of the four sources a wire DTO may
// name. validateWireElisions rejects any Elisions carrying anything else — the
// DTO must not ship a source it cannot name.
func validBudgetSource(s budgetSource) bool {
	switch s {
	case sourceAdvertised, sourceConfigured, sourceDefault, sourceAdvertisedBelowFloor:
		return true
	default:
		return false
	}
}

// responseBudget pairs the effective byte budget with the rung that decided it,
// kept as ONE value so a call site can never pass a byte count while reporting an
// unrelated source.
type responseBudget struct {
	bytes  int
	source budgetSource
}

// advertisedLimitKey is the vendor-prefixed capability key Fishhawk reads for a
// client's advertised tool-result byte limit, in the SDK's documented
// {vendor-prefix}/{extension-name} Extensions format.
const advertisedLimitKey = "fishhawk/tool-result-limit"

// advertisedBytesSettingsKey is the key inside a settings-object-shaped
// advertisement that carries the byte count. The SDK's AddExtension normalises
// an extension value to a settings map, so a bare number is not the only shape a
// well-behaved client produces.
const advertisedBytesSettingsKey = "bytes"

// maxPlausibleAdvertisedBytes is the plausibility ceiling: 32x the measured
// default. That is generous headroom for a materially different client, while a
// value beyond it is a bug or a lie and must NOT be trusted — the failure mode
// of an over-large budget is exactly the SILENT client-side rejection this epic
// exists to prevent, so an implausible advertisement is treated as ABSENT and
// the resolution falls back to the conservative default.
const maxPlausibleAdvertisedBytes = 1 << 20 // 1 MiB

// advertisedResultLimit reads a client's advertised tool-result byte limit off
// the negotiated capabilities. It returns (0, false) for nil caps or any absent
// / implausible advertisement.
//
// It looks the key up in caps.Extensions FIRST, then caps.Experimental — a
// fixed, documented precedence so a client that populates BOTH gets a
// deterministic answer rather than Go map-iteration chance. A bucket whose value
// is present but implausible is treated as ABSENT and the lookup falls through to
// the next bucket.
func advertisedResultLimit(caps *mcp.ClientCapabilities) (int, bool) {
	if caps == nil {
		return 0, false
	}
	for _, bucket := range []map[string]any{caps.Extensions, caps.Experimental} {
		if bucket == nil {
			continue
		}
		raw, present := bucket[advertisedLimitKey]
		if !present {
			continue
		}
		if n, ok := plausibleAdvertisedBytes(raw); ok {
			return n, true
		}
	}
	return 0, false
}

// plausibleAdvertisedBytes extracts a plausible positive byte count from an
// advertised capability value, treating every implausible or wrong-typed value
// as ABSENT (0, false) rather than coercing it. It accepts either a bare number
// or a settings object carrying the count under the "bytes" key.
//
// Each rejection logs one line naming the key, the offending value and the
// reason, mirroring resolveByteBudget's logged-reason discipline, so an operator
// whose client advertised garbage can see why it was ignored.
func plausibleAdvertisedBytes(raw any) (int, bool) {
	if settings, ok := raw.(map[string]any); ok {
		inner, present := settings[advertisedBytesSettingsKey]
		if !present {
			log.Printf("mcp: advertised %s settings object carries no %q key; ignoring", advertisedLimitKey, advertisedBytesSettingsKey)
			return 0, false
		}
		raw = inner
	}
	f, ok := advertisedFloat(raw)
	if !ok {
		log.Printf("mcp: advertised %s value %v (%T) is not a number; ignoring", advertisedLimitKey, raw, raw)
		return 0, false
	}
	// Reject a NaN or infinite float BEFORE any conversion: converting either to
	// int in Go is implementation-defined and would yield a garbage budget.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		log.Printf("mcp: advertised %s value %v is NaN or infinite; ignoring", advertisedLimitKey, f)
		return 0, false
	}
	if f != math.Trunc(f) {
		log.Printf("mcp: advertised %s value %v is not integral; ignoring", advertisedLimitKey, f)
		return 0, false
	}
	if f <= 0 {
		log.Printf("mcp: advertised %s value %v is not positive; ignoring", advertisedLimitKey, f)
		return 0, false
	}
	if f > float64(maxPlausibleAdvertisedBytes) {
		log.Printf("mcp: advertised %s value %v exceeds the %d-byte plausibility ceiling; ignoring", advertisedLimitKey, f, maxPlausibleAdvertisedBytes)
		return 0, false
	}
	return int(f), true
}

// advertisedFloat reports whether raw is a JSON/Go numeric type, returning its
// float64 value. It does NOT validate — plausibleAdvertisedBytes does that so
// each rejection reason logs distinctly. encoding/json decodes every JSON number
// into any as float64, so that is the common case; json.Number and the Go
// integer kinds are accepted too. Every integer a plausible advertisement could
// carry is below 2^53 and so is exactly representable as a float64 (a huge int
// loses precision but is rejected by the ceiling regardless). Any other type —
// string, bool, nil, slice, a map without the count key — is not a number.
func advertisedFloat(raw any) (float64, bool) {
	switch n := raw.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// resolveResponseBudget resolves the effective response byte budget through the
// three-rung discovery ladder, following the issue's precedence ADVERTISED >
// CONFIGURED > DEFAULT (BINDING CONDITION 1):
//
//	rung 1  a client-advertised, plausible tool-result limit -> source advertised
//	        (a valid value BELOW the convergence floor emits the floor but reports
//	        source advertised_below_floor, never claiming the smaller
//	        advertisement was honoured — BINDING CONDITION 2)
//	rung 2  an operator env override present                  -> source configured
//	rung 3  the ADR-077 measured default                     -> source default
func resolveResponseBudget(caps *mcp.ClientCapabilities, getenv func(string) string) responseBudget {
	if n, ok := advertisedResultLimit(caps); ok {
		if n < mcpConvergenceFloorBytes {
			return responseBudget{bytes: mcpConvergenceFloorBytes, source: sourceAdvertisedBelowFloor}
		}
		return responseBudget{bytes: n, source: sourceAdvertised}
	}
	if n, ok := configuredByteBudget(getenv); ok {
		return responseBudget{bytes: n, source: sourceConfigured}
	}
	return responseBudget{bytes: mcpResponseByteBudgetDefault, source: sourceDefault}
}

// responseBudget is the nil-safe accessor from a tool handler's request to the
// resolved budget. It walks req -> req.Session -> req.Session.InitializeParams()
// -> params.Capabilities, returning nil capabilities (hence the env-or-default
// answer) at the FIRST nil hop.
//
// This is load-bearing, not merely defensive: ~90 existing handler tests pass a
// literal nil request, and they must keep resolving to the default without
// edits.
func (r *runResolver) responseBudget(req *mcp.CallToolRequest) responseBudget {
	return resolveResponseBudget(clientCapabilities(req), r.getenv)
}

// clientCapabilities returns the negotiated client capabilities carried on a
// tool request's session, or nil at the first nil hop.
func clientCapabilities(req *mcp.CallToolRequest) *mcp.ClientCapabilities {
	if req == nil || req.Session == nil {
		return nil
	}
	params := req.Session.InitializeParams()
	if params == nil {
		return nil
	}
	return params.Capabilities
}
