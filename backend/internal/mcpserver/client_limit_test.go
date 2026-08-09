package mcpserver

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file pins the client tool-result-limit discovery ladder (E48.76 / #2509).
// Every case asserts BOTH the resolved bytes AND the resolved source: a budget
// that is right by accident (correct bytes, wrong source, or vice versa) is a
// silent lie about whether the client's advertised limit was honoured, which is
// exactly the failure class this epic exists to close.

// extCaps builds capabilities advertising raw under the discovery key in the
// Extensions bucket — the vendor-prefixed open-set mechanism the resolver reads
// first.
func extCaps(raw any) *mcp.ClientCapabilities {
	return &mcp.ClientCapabilities{Extensions: map[string]any{advertisedLimitKey: raw}}
}

// noEnv is the env seam a test that advertises (or advertises nothing) uses when
// it wants NO operator override to interfere with the advertised/default rung.
func noEnv(string) string { return "" }

// TestAdvertisedLimit_Absent_UsesMeasuredDefault: no advertisement and no env
// override resolves to the ADR-077 measured constant with source default.
func TestAdvertisedLimit_Absent_UsesMeasuredDefault(t *testing.T) {
	got := resolveResponseBudget(nil, noEnv)
	assertBudget(t, got, mcpResponseByteBudgetDefault, sourceDefault)
	// A non-nil caps carrying no key is equally absent.
	got = resolveResponseBudget(&mcp.ClientCapabilities{}, noEnv)
	assertBudget(t, got, mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_ValidValueHonoured: a plausible advertised value above the
// floor and below the ceiling is used verbatim with source advertised.
func TestAdvertisedLimit_ValidValueHonoured(t *testing.T) {
	got := resolveResponseBudget(extCaps(float64(16384)), noEnv)
	assertBudget(t, got, 16384, sourceAdvertised)
}

// TestAdvertisedLimit_BelowFloorClampedUp is BINDING CONDITION 2: a valid
// advertised value BELOW the convergence floor still emits the floor (convergence
// cannot promise less), but the source SAYS SO — advertised_below_floor — rather
// than presenting the floor as honouring the smaller advertisement.
func TestAdvertisedLimit_BelowFloorClampedUp(t *testing.T) {
	got := resolveResponseBudget(extCaps(float64(2048)), noEnv)
	assertBudget(t, got, mcpConvergenceFloorBytes, sourceAdvertisedBelowFloor)
	if got.source == sourceAdvertised {
		t.Fatal("a below-floor advertisement must NOT report source=advertised — that would claim the smaller limit was honoured when the floor knowingly exceeded it")
	}
}

// TestAdvertisedLimit_AboveCeilingIgnored asserts BOTH sides of the plausibility
// boundary: ceiling+1 is a lie or a bug and is ignored (falls to the default),
// while ceiling EXACTLY is still plausible and is honoured.
func TestAdvertisedLimit_AboveCeilingIgnored(t *testing.T) {
	over := resolveResponseBudget(extCaps(float64(maxPlausibleAdvertisedBytes+1)), noEnv)
	assertBudget(t, over, mcpResponseByteBudgetDefault, sourceDefault)

	atCeiling := resolveResponseBudget(extCaps(float64(maxPlausibleAdvertisedBytes)), noEnv)
	assertBudget(t, atCeiling, maxPlausibleAdvertisedBytes, sourceAdvertised)
}

// TestAdvertisedLimit_Zero: a zero advertisement is not positive and is ignored.
func TestAdvertisedLimit_Zero(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(float64(0)), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_Negative: a negative advertisement is ignored.
func TestAdvertisedLimit_Negative(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(float64(-4096)), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_NonIntegralFloat: a non-integral number cannot be a byte
// count and is ignored (converting it to int would silently truncate).
func TestAdvertisedLimit_NonIntegralFloat(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(16384.5), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_NaN: a NaN is rejected BEFORE any conversion — converting
// it to int in Go is implementation-defined.
func TestAdvertisedLimit_NaN(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(math.NaN()), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_Infinity: +Inf is rejected before conversion.
func TestAdvertisedLimit_Infinity(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(math.Inf(1)), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestAdvertisedLimit_WrongType: a value that is not a number and not a
// settings object carrying the count is treated as ABSENT, never coerced.
func TestAdvertisedLimit_WrongType(t *testing.T) {
	cases := []struct {
		name string
		raw  any
	}{
		{"string", "16384"},
		{"bool", true},
		{"nil", nil},
		{"map without the bytes key", map[string]any{"limit": float64(16384)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertBudget(t, resolveResponseBudget(extCaps(tc.raw), noEnv), mcpResponseByteBudgetDefault, sourceDefault)
		})
	}
}

// TestAdvertisedLimit_SettingsObjectShape: the SDK's AddExtension normalises an
// extension value to a settings map, so the count under a "bytes" key is a valid
// advertisement shape, not only a bare number.
func TestAdvertisedLimit_SettingsObjectShape(t *testing.T) {
	got := resolveResponseBudget(extCaps(map[string]any{advertisedBytesSettingsKey: float64(16384)}), noEnv)
	assertBudget(t, got, 16384, sourceAdvertised)
}

// TestAdvertisedLimit_ExtensionsBeatsExperimental: a client populating BOTH
// buckets gets a deterministic answer — Extensions wins — rather than Go
// map-iteration chance.
func TestAdvertisedLimit_ExtensionsBeatsExperimental(t *testing.T) {
	caps := &mcp.ClientCapabilities{
		Extensions:   map[string]any{advertisedLimitKey: float64(16384)},
		Experimental: map[string]any{advertisedLimitKey: float64(8192)},
	}
	got := resolveResponseBudget(caps, noEnv)
	assertBudget(t, got, 16384, sourceAdvertised)
}

// TestAdvertisedLimit_ExperimentalWhenNoExtensions: the looser Experimental
// bucket named in the issue is read when Extensions carries nothing.
func TestAdvertisedLimit_ExperimentalWhenNoExtensions(t *testing.T) {
	caps := &mcp.ClientCapabilities{Experimental: map[string]any{advertisedLimitKey: float64(8192)}}
	assertBudget(t, resolveResponseBudget(caps, noEnv), 8192, sourceAdvertised)
}

// TestResolveResponseBudget_AdvertisedBeatsConfigured pins BINDING CONDITION 1's
// precedence — advertised > configured > default. When a client advertises a
// valid limit AND an operator env override is set, the ADVERTISED value wins.
// (This supersedes the pre-approval plan's inverted ordering.)
func TestResolveResponseBudget_AdvertisedBeatsConfigured(t *testing.T) {
	env := envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "20000"})
	got := resolveResponseBudget(extCaps(float64(16384)), env)
	assertBudget(t, got, 16384, sourceAdvertised)
}

// TestResolveResponseBudget_ConfiguredWhenNoAdvertisement: with no advertisement
// the operator env override decides, source configured. Deleting the configured
// rung makes this fall through to the default, turning the test red.
func TestResolveResponseBudget_ConfiguredWhenNoAdvertisement(t *testing.T) {
	env := envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "20000"})
	assertBudget(t, resolveResponseBudget(nil, env), 20000, sourceConfigured)
}

// TestResponseBudget_NilRequest / _NilSession / _NilInitializeParams pin the
// nil-safe walk: each nil hop resolves to the env-or-default answer rather than
// panicking. This is load-bearing — ~90 existing handler tests pass a literal
// nil request.
func TestResponseBudget_NilRequest(t *testing.T) {
	r := &runResolver{getenv: noEnv}
	assertBudget(t, r.responseBudget(nil), mcpResponseByteBudgetDefault, sourceDefault)
}

func TestResponseBudget_NilSession(t *testing.T) {
	r := &runResolver{getenv: noEnv}
	assertBudget(t, r.responseBudget(&mcp.CallToolRequest{Session: nil}), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestResponseBudget_NilInitializeParams uses a server session connected but not
// yet handshaken by a client — its InitializeParams() is nil — to reach the
// third nil hop, which a bare nil request cannot.
func TestResponseBudget_NilInitializeParams(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	serverTransport, _ := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()
	if ss.InitializeParams() != nil {
		t.Skip("server session already has InitializeParams — cannot exercise the nil-params hop")
	}
	r := &runResolver{getenv: noEnv}
	assertBudget(t, r.responseBudget(&mcp.CallToolRequest{Session: ss}), mcpResponseByteBudgetDefault, sourceDefault)
}

// TestClientCapabilities_NilHops pins the accessor directly at each nil hop.
func TestClientCapabilities_NilHops(t *testing.T) {
	if clientCapabilities(nil) != nil {
		t.Error("nil request must yield nil capabilities")
	}
	if clientCapabilities(&mcp.CallToolRequest{Session: nil}) != nil {
		t.Error("nil session must yield nil capabilities")
	}
}

// TestAdvertisedResultLimit_NilCaps: the extractor returns (0,false) for nil
// caps rather than dereferencing them.
func TestAdvertisedResultLimit_NilCaps(t *testing.T) {
	if n, ok := advertisedResultLimit(nil); ok || n != 0 {
		t.Errorf("advertisedResultLimit(nil) = (%d,%v), want (0,false)", n, ok)
	}
}

// TestAdvertisedLimit_JSONNumberShape: a value decoded as json.Number (a client
// decoding with UseNumber) is accepted like any other integer.
func TestAdvertisedLimit_JSONNumberShape(t *testing.T) {
	assertBudget(t, resolveResponseBudget(extCaps(json.Number("16384")), noEnv), 16384, sourceAdvertised)
}

// TestAdvertisedResultLimit_NumericKinds exercises every Go numeric kind
// advertisedFloat accepts — encoding/json produces float64, but a client
// decoding with UseNumber or one that assigned a typed integer directly may
// carry any of these. Each is extracted to its exact value with ok=true.
func TestAdvertisedResultLimit_NumericKinds(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want int
	}{
		{"int", int(16384), 16384},
		{"int8", int8(100), 100},
		{"int16", int16(16384), 16384},
		{"int32", int32(16384), 16384},
		{"int64", int64(16384), 16384},
		{"uint", uint(16384), 16384},
		{"uint8", uint8(100), 100},
		{"uint16", uint16(16384), 16384},
		{"uint32", uint32(16384), 16384},
		{"uint64", uint64(16384), 16384},
		{"float32", float32(16384), 16384},
		{"float64", float64(16384), 16384},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := advertisedResultLimit(extCaps(tc.raw))
			if !ok || n != tc.want {
				t.Errorf("advertisedResultLimit(%s) = (%d,%v), want (%d,true)", tc.name, n, ok, tc.want)
			}
		})
	}
}

// TestAdvertisedResultLimit_BadJSONNumber: a json.Number that does not parse as
// a float is treated as absent, not coerced.
func TestAdvertisedResultLimit_BadJSONNumber(t *testing.T) {
	if n, ok := advertisedResultLimit(extCaps(json.Number("not-a-number"))); ok || n != 0 {
		t.Errorf("advertisedResultLimit(bad json.Number) = (%d,%v), want (0,false)", n, ok)
	}
}

func assertBudget(t *testing.T, got responseBudget, wantBytes int, wantSource budgetSource) {
	t.Helper()
	if got.bytes != wantBytes {
		t.Errorf("resolved budget = %d bytes, want %d", got.bytes, wantBytes)
	}
	if got.source != wantSource {
		t.Errorf("resolved budget source = %q, want %q", got.source, wantSource)
	}
}
