package oauthas

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"
)

// TestSupportedScopes_MatchesOperatorDefault is a REAL drift detector (#2427
// condition 5, option a): operatorDefaultScopes lives in the main package
// backend/cmd/fishhawkd and cannot be imported, so this AST-parses token.go and
// compares the parsed literal against SupportedScopes by value AND order. A
// hardcoded duplicate would be a false assertion; parsing the actual source is not.
func TestSupportedScopes_MatchesOperatorDefault(t *testing.T) {
	t.Parallel()
	const path = "../../cmd/fishhawkd/token.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	got := extractStringSliceVar(t, f, "operatorDefaultScopes")
	if len(got) == 0 {
		t.Fatalf("could not extract operatorDefaultScopes from %s", path)
	}
	if !reflect.DeepEqual(got, SupportedScopes) {
		t.Fatalf("scope drift:\n token.go operatorDefaultScopes = %v\n oauthas.SupportedScopes       = %v", got, SupportedScopes)
	}
}

// extractStringSliceVar returns the string literals of a top-level
// `var name = []string{...}` declaration.
func extractStringSliceVar(t *testing.T, f *ast.File, name string) []string {
	t.Helper()
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, id := range vs.Names {
				if id.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.CompositeLit)
				if !ok {
					continue
				}
				out := make([]string, 0, len(lit.Elts))
				for _, elt := range lit.Elts {
					bl, ok := elt.(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(bl.Value)
					if err != nil {
						t.Fatalf("unquote %q: %v", bl.Value, err)
					}
					out = append(out, s)
				}
				return out
			}
		}
	}
	return nil
}

func TestIsSupportedScope(t *testing.T) {
	t.Parallel()
	if !IsSupportedScope("read:runs") {
		t.Fatalf("read:runs should be supported")
	}
	if IsSupportedScope("mcp:read") {
		t.Fatalf("mcp:read should not be in the operator vocabulary")
	}
	if IsSupportedScope("") {
		t.Fatalf("empty scope should not be supported")
	}
}

func TestParseScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "single", in: "read:runs", want: []string{"read:runs"}},
		{name: "multi", in: "read:runs write:runs", want: []string{"read:runs", "write:runs"}},
		{name: "dedup preserves first-seen order", in: "write:runs read:runs write:runs", want: []string{"write:runs", "read:runs"}},
		{name: "multi-space tolerated", in: "read:runs   write:runs", want: []string{"read:runs", "write:runs"}},
		{name: "unknown refused", in: "read:runs bogus:scope", wantErr: true},
		{name: "empty refused", in: "", wantErr: true},
		{name: "all-space refused", in: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseScope(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseScope(%q) = %v, want error", tc.in, got)
				}
				assertCode(t, err, ErrCodeInvalidScope)
				return
			}
			if err != nil {
				t.Fatalf("ParseScope(%q) unexpected error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseScope(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// COUNTERFACTUAL LOG (#2477) — each control below was DELETED, the guarding
// tests RUN, the RED observed, and the file restored byte-identically. Recorded
// here rather than asserted in prose so the next reader can re-run them.
//
//  1. THE REQUEST-TIME BOUND. Removed the isPreRegistrationOnlyScope loop from
//     ResolveRequestedScope's PRESENT path. RED, both altitudes:
//     TestResolveRequestedScope/{explicit_write:deploy_with_no_registration_refused,
//     explicit_write:deploy_with_a_narrower_registration_refused,
//     mixed_request_naming_write:deploy_refused_whole_not_stripped} each
//     `= [write:deploy], want error`; and in package server
//     TestAuthorize_WriteDeployIsPreRegistrationOnly/{all_eight_scopes_with_no_registration_refused,
//     mixed_request_refused_whole_not_stripped} plus
//     TestOAuthFlow_MaximalScopeRequestRefusedAndMintsNothing, all
//     `error = "", want invalid_scope` — the all-eight request was GRANTED.
//
//  2. THE NARROWED DEFAULT. Reverted the no-registration branch to copy
//     SupportedScopes and both ScopesSupported sites to oauthas.SupportedScopes.
//     RED across all five advertised/defaulted surfaces:
//     TestResolveRequestedScope/no_token_and_no_registration_defaults_to_the_advertised_posture,
//     TestResolveRequestedScope_DefaultDoesNotAliasDefaultScopes,
//     TestAuthorize_AbsentScopeDefaults/{registered_scope_absent,
//     present_but_empty_scope_takes_default},
//     TestOAuthFlow_ScopeOmittedClientOnboardsEndToEnd,
//     TestOAuthASMetadata_ScopesSupportedMirrorsAdvertisedPosture and
//     TestOAuthPRM_ServedAndSelfConsistentWithASMetadata — each reporting the
//     eight-member list where the seven-member posture was wanted.
//
//  3. THE STRICT-SUBSET INVARIANT. Added "bogus:scope" to DefaultScopes. RED:
//     TestDefaultScopes_IsStrictSubsetOfSupportedScopes,
//     `DefaultScopes member "bogus:scope" is absent from SupportedScopes`. (The
//     first attempt at this mutation failed to apply and the test passed GREEN
//     — a no-op-mutation false negative; the mutation was verified present in
//     the file before the RED above was accepted.)

// TestResolveRequestedScope covers one case per branch of the #2466 defaulting
// helper. The PRESENT cases are the unchanged-behaviour controls (they delegate
// to ParseScope verbatim); the NO-SCOPE-TOKEN cases are the new behaviour.
func TestResolveRequestedScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		requested  string
		registered []string
		want       []string
		wantErr    bool
	}{
		// PRESENT path — delegated to ParseScope, unchanged.
		{name: "present valid passes through", requested: "read:runs write:runs", want: []string{"read:runs", "write:runs"}},
		{name: "present unknown refused", requested: "bogus:scope", wantErr: true},
		// CONDITION A boundary: whitespace-only is PRESENT, so it stays refused —
		// this is what proves the change did not swallow the present-but-invalid
		// path along with the genuinely-tokenless one.
		{name: "present whitespace-only refused not defaulted", requested: "   ", wantErr: true},
		{name: "present whitespace-only refused even with a registration", requested: "   ", registered: []string{"read:runs"}, wantErr: true},

		// NO SCOPE TOKEN — defaults. An absent key and a present-but-empty
		// `scope=` both arrive here as "" and are deliberately equivalent
		// (CONDITION A); the handler-level pin for the `scope=` wire form is
		// TestAuthorize_AbsentScopeDefaults/present_but_empty_scope_takes_default.
		{name: "no token and no registration defaults to the advertised posture", requested: "", registered: nil, want: DefaultScopes},
		{name: "no token and a registration pinning write:deploy grants it", requested: "", registered: []string{"read:runs", "write:deploy"}, want: []string{"read:runs", "write:deploy"}},

		// #2477 — the write:deploy request-time bound. write:deploy stays in the
		// VOCABULARY (ParseScope accepts it, `fishhawkd token issue` mints it),
		// but an authorization request reaches it only via a registration pin.
		{name: "explicit write:deploy with no registration refused", requested: "write:deploy", registered: nil, wantErr: true},
		{name: "explicit write:deploy with a pinning registration granted", requested: "write:deploy", registered: []string{"read:runs", "write:deploy"}, want: []string{"write:deploy"}},
		{name: "explicit write:deploy with a narrower registration refused", requested: "write:deploy", registered: []string{"read:runs", "write:runs"}, wantErr: true},
		// REFUSED WHOLE, never stripped: a mixed request must not mint a grant
		// narrower than the one the client asked for and consent displayed.
		{name: "mixed request naming write:deploy refused whole not stripped", requested: "read:runs write:runs write:deploy", registered: nil, wantErr: true},
		// The bound is NARROW: every other scope is requestable exactly as before.
		{name: "explicit write:approvals with no registration still granted", requested: "write:approvals", registered: nil, want: []string{"write:approvals"}},
		{name: "explicit full DefaultScopes with no registration granted", requested: ScopeString(DefaultScopes), registered: nil, want: DefaultScopes},
		{name: "no token defaults to a fully supported registration", requested: "", registered: []string{"read:runs", "write:runs"}, want: []string{"read:runs", "write:runs"}},
		{name: "no token drops registered scopes outside the vocabulary", requested: "", registered: []string{"openid", "read:runs"}, want: []string{"read:runs"}},
		{name: "no token and a wholly unsupported registration fails closed", requested: "", registered: []string{"openid", "profile"}, wantErr: true},
		{name: "no token dedups a duplicate-bearing registration", requested: "", registered: []string{"write:runs", "read:runs", "write:runs"}, want: []string{"write:runs", "read:runs"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveRequestedScope(tc.requested, tc.registered)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveRequestedScope(%q, %v) = %v, want error", tc.requested, tc.registered, got)
				}
				assertCode(t, err, ErrCodeInvalidScope)
				return
			}
			if err != nil {
				t.Fatalf("ResolveRequestedScope(%q, %v) unexpected error: %v", tc.requested, tc.registered, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ResolveRequestedScope(%q, %v) = %v, want %v", tc.requested, tc.registered, got, tc.want)
			}
		})
	}
}

// TestResolveRequestedScope_DefaultDoesNotAliasDefaultScopes pins the defensive
// copy: the caller stores the returned slice on the authorization-code row, so
// handing out DefaultScopes' backing array would let one request corrupt the
// advertised posture for every later one. It follows the list actually handed
// out (#2477 moved the default from SupportedScopes to DefaultScopes) and
// additionally asserts SupportedScopes is untouched.
func TestResolveRequestedScope_DefaultDoesNotAliasDefaultScopes(t *testing.T) {
	// NOT parallel: it mutates its own copy and then re-reads the package vars.
	got, err := ResolveRequestedScope("", nil)
	if err != nil {
		t.Fatalf("ResolveRequestedScope: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("default scope set is empty")
	}
	firstDefault, firstSupported := DefaultScopes[0], SupportedScopes[0]
	got[0] = "corrupted:scope"
	if DefaultScopes[0] != firstDefault {
		t.Fatalf("mutating the returned default corrupted DefaultScopes[0] = %q, want %q", DefaultScopes[0], firstDefault)
	}
	if SupportedScopes[0] != firstSupported {
		t.Fatalf("mutating the returned default corrupted SupportedScopes[0] = %q, want %q", SupportedScopes[0], firstSupported)
	}
	again, err := ResolveRequestedScope("", nil)
	if err != nil {
		t.Fatalf("second ResolveRequestedScope: %v", err)
	}
	if !reflect.DeepEqual(again, DefaultScopes) {
		t.Fatalf("second default = %v, want %v", again, DefaultScopes)
	}
}

// TestDefaultScopes_IsStrictSubsetOfSupportedScopes pins the #2477 invariant
// that keeps the posture list and the vocabulary list from diverging in the
// UNSAFE direction: every advertised/defaulted scope must be mintable, and the
// sole difference must be write:deploy.
//
// COUNTERFACTUAL (run, observed RED): adding "bogus:scope" to DefaultScopes
// fails this test with `DefaultScopes member "bogus:scope" is absent from
// SupportedScopes`.
func TestDefaultScopes_IsStrictSubsetOfSupportedScopes(t *testing.T) {
	t.Parallel()
	for _, s := range DefaultScopes {
		if !IsSupportedScope(s) {
			t.Errorf("DefaultScopes member %q is absent from SupportedScopes — it would be advertised and defaulted while ParseScope refuses it", s)
		}
	}
	if len(DefaultScopes) >= len(SupportedScopes) {
		t.Fatalf("DefaultScopes must be a STRICT subset: len(DefaultScopes)=%d, len(SupportedScopes)=%d", len(DefaultScopes), len(SupportedScopes))
	}
	var excluded []string
	for _, s := range SupportedScopes {
		if !containsScope(DefaultScopes, s) {
			excluded = append(excluded, s)
		}
	}
	if !reflect.DeepEqual(excluded, []string{"write:deploy"}) {
		t.Fatalf("SupportedScopes minus DefaultScopes = %v, want exactly [write:deploy]", excluded)
	}
}

// TestPreRegistrationOnlyScopes_AreSupportedButNotDefaulted pins that the
// request-time bound names only scopes that remain MINTABLE. A member that fell
// out of SupportedScopes would make the bound dead code (ParseScope would refuse
// the scope first), and a member present in DefaultScopes would be advertised
// and defaulted while the bound refuses it explicitly — an unresolvable
// contradiction for a client that requests scopes_supported verbatim.
func TestPreRegistrationOnlyScopes_AreSupportedButNotDefaulted(t *testing.T) {
	t.Parallel()
	if len(PreRegistrationOnlyScopes) == 0 {
		t.Fatal("PreRegistrationOnlyScopes is empty; the write:deploy bound is gone")
	}
	for _, s := range PreRegistrationOnlyScopes {
		if !IsSupportedScope(s) {
			t.Errorf("PreRegistrationOnlyScopes member %q is not in SupportedScopes — the bound would be dead code", s)
		}
		if containsScope(DefaultScopes, s) {
			t.Errorf("PreRegistrationOnlyScopes member %q is also advertised in DefaultScopes — advertised yet unrequestable", s)
		}
		if !isPreRegistrationOnlyScope(s) {
			t.Errorf("isPreRegistrationOnlyScope(%q) = false, want true", s)
		}
	}
	if isPreRegistrationOnlyScope("read:runs") {
		t.Error("isPreRegistrationOnlyScope(read:runs) = true; the bound must be narrow")
	}
}

func TestScopeString(t *testing.T) {
	t.Parallel()
	in := []string{"read:runs", "write:runs"}
	if got := ScopeString(in); got != "read:runs write:runs" {
		t.Fatalf("ScopeString = %q", got)
	}
	// Round-trip through ParseScope.
	back, err := ParseScope(ScopeString(in))
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round-trip = %v, want %v", back, in)
	}
}
