package concurrency

import (
	"errors"
	"testing"
)

// acceptedGroupKeys is every canonical spelling ParseGroupKey accepts, with
// the identity it parses to (ADR-094 D3, Amendment A1). TestGroupKey_RoundTrip
// walks the same table.
var acceptedGroupKeys = []struct {
	key  string
	want ScopedGroup
}{
	{"local-implement:h1", ScopedGroup{ScopeHost, "h1", "implement"}},
	{"local-verify:h1", ScopedGroup{ScopeHost, "h1", "verify"}},
	{"local-implement:unknown", ScopedGroup{ScopeHost, "unknown", "implement"}},
	{"spec:o/r:deploy", ScopedGroup{ScopeRepository, "o/r", "deploy"}},
	{"spec:o/r:verify:main", ScopedGroup{ScopeRepository, "o/r", "verify:main"}},
	{"host:h1:preview-target", ScopedGroup{ScopeHost, "h1", "preview-target"}},
	{"host:h1:implement:x", ScopedGroup{ScopeHost, "h1", "implement:x"}},
	{"account:codex/acct:review", ScopedGroup{ScopeAccount, "codex/acct", "review"}},
}

func TestParseGroupKey(t *testing.T) {
	for _, tc := range acceptedGroupKeys {
		t.Run("accepts_"+tc.key, func(t *testing.T) {
			got, ok := ParseGroupKey(tc.key)
			if !ok || got != tc.want {
				t.Fatalf("ParseGroupKey(%q) = %+v, %v; want %+v, true", tc.key, got, ok, tc.want)
			}
		})
	}
	// One rejection per alias and malformed form (R5): each key has exactly
	// one spelling.
	rejected := []struct {
		name string
		key  string
	}{
		{"repository_alias", "repository:o/r:deploy"},
		{"host_implement_alias", "host:h1:implement"},
		{"host_verify_alias", "host:h1:verify"},
		{"legacy_implement_empty_host", "local-implement:"},
		{"legacy_implement_colon_host", "local-implement:a:b"},
		{"legacy_verify_empty_host", "local-verify:"},
		{"legacy_verify_colon_host", "local-verify:a:b"},
		{"spec_empty_name", "spec:o/r:"},
		{"spec_no_name_separator", "spec:o/r"},
		{"spec_empty_scope_id", "spec::deploy"},
		{"host_empty_scope_id", "host::x"},
		{"account_empty_name", "account:a:"},
		{"unknown_scope", "unknown:x:y"},
		{"no_separator", "deploy"},
		{"empty", ""},
	}
	for _, tc := range rejected {
		t.Run("rejects_"+tc.name, func(t *testing.T) {
			if got, ok := ParseGroupKey(tc.key); ok {
				t.Fatalf("ParseGroupKey(%q) = %+v, true; want rejected", tc.key, got)
			}
		})
	}
}

// TestGroupKey_RoundTrip pins R5: for every accepted key,
// ScopedGroupKey(ParseGroupKey(k)) == k.
func TestGroupKey_RoundTrip(t *testing.T) {
	for _, tc := range acceptedGroupKeys {
		g, ok := ParseGroupKey(tc.key)
		if !ok {
			t.Fatalf("ParseGroupKey(%q) rejected an accepted key", tc.key)
		}
		got, err := ScopedGroupKey(g.Scope, g.ScopeID, g.Name)
		if err != nil || got != tc.key {
			t.Errorf("ScopedGroupKey(ParseGroupKey(%q)) = %q, %v; want %q", tc.key, got, err, tc.key)
		}
	}
}

// TestScopedGroupKey_CanonicalisesLegacyIdentities pins ADR-094 A1: the
// legacy key strings are the canonical keys of the host implement/verify and
// repository identities, so no persisted key or advisory-lock input changes.
func TestScopedGroupKey_CanonicalisesLegacyIdentities(t *testing.T) {
	cases := []struct {
		scope         Scope
		scopeID, name string
		want          string
	}{
		{ScopeHost, "h1", "implement", DefaultGroupKey("h1")},
		{ScopeHost, "h1", "implement", "local-implement:h1"},
		{ScopeHost, "h1", "verify", VerifyGroupKey("h1")},
		{ScopeHost, "h1", "verify", "local-verify:h1"},
		{ScopeHost, "", "implement", "local-implement:unknown"},
		{ScopeHost, "", "preview-target", "host:unknown:preview-target"},
		{ScopeRepository, "o/r", "x", NamedGroupKey("o/r", "x")},
		{ScopeRepository, "o/r", "verify:main", "spec:o/r:verify:main"},
		{ScopeHost, "h1", "preview-target", "host:h1:preview-target"},
		{ScopeAccount, "codex/acct", "review", "account:codex/acct:review"},
	}
	for _, tc := range cases {
		got, err := ScopedGroupKey(tc.scope, tc.scopeID, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("ScopedGroupKey(%s, %q, %q) = %q, %v; want %q", tc.scope, tc.scopeID, tc.name, got, err, tc.want)
		}
	}
}

func TestScopedGroupKey_Refusals(t *testing.T) {
	cases := []struct {
		name           string
		scope          Scope
		scopeID, gname string
	}{
		{"host_colon_scope_id", ScopeHost, "a:b", "x"},
		{"host_colon_scope_id_implement", ScopeHost, "a:b", "implement"},
		{"repository_colon_scope_id", ScopeRepository, "a:b", "x"},
		{"account_colon_scope_id", ScopeAccount, "a:b", "x"},
		{"unknown_scope", Scope("org"), "o", "x"},
		{"empty_name", ScopeHost, "h1", ""},
		{"empty_repository_scope_id", ScopeRepository, "", "x"},
		{"empty_account_scope_id", ScopeAccount, "", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScopedGroupKey(tc.scope, tc.scopeID, tc.gname)
			if !errors.Is(err, ErrInvalidGroupKey) {
				t.Errorf("err = %v, want ErrInvalidGroupKey", err)
			}
			if got != "" {
				t.Errorf("key = %q, want empty on refusal", got)
			}
		})
	}
}

// TestMaxGroupsAboveSchemaMaxItems pins R7: the store's group-set bound sits
// above the workflow-v2 schema's concurrency.groups maxItems (8, pinned by
// the spec package's rejection table), so ADR-094 slice 3 (#4241) can add the
// host capacity group to a full spec-declared set without a store change.
func TestMaxGroupsAboveSchemaMaxItems(t *testing.T) {
	const schemaMaxItems = 8
	if MaxGroups <= schemaMaxItems {
		t.Fatalf("MaxGroups = %d, want > the schema's concurrency.groups maxItems %d", MaxGroups, schemaMaxItems)
	}
}
