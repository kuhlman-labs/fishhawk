package crewmessage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// canonicalSchemaPath is the canonical copy scripts/sync-schemas mirrors into
// this package's schemas/ dir.
const canonicalSchemaPath = "../../../docs/spec/crew-message-v1.schema.json"

// TestEmbeddedSchemaMatchesCanonical is the in-loop mirror-drift belt: an edit
// to the canonical schema that skips scripts/sync-schemas fails HERE, before
// CI's schema-sync gate and before the plan-gate sweep registries catch it on
// the NEXT plan.
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile(canonicalSchemaPath)
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	if got := EmbeddedSchema(); !reflect.DeepEqual(got, canonical) {
		t.Errorf("embedded %s drifted from %s: run scripts/sync-schemas (embedded %d bytes, canonical %d bytes)",
			embeddedSchemaPath, canonicalSchemaPath, len(got), len(canonical))
	}
}

// TestSchema_ClosedAtEveryLevel is the shipped-behaviour assertion for the
// half of ADR-081 rule 2 that says no field can express scope, constraints,
// delegation or a gate outcome: every object-typed subschema in the SHIPPED
// document must close with additionalProperties:false, so an unknown property
// is refused wherever it is added. A comment-only touch of the schema cannot
// satisfy this.
func TestSchema_ClosedAtEveryLevel(t *testing.T) {
	var doc any
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	var open []string
	walkSchema(doc, "#", func(loc string, obj map[string]any) {
		if obj["type"] != "object" {
			return
		}
		ap, ok := obj["additionalProperties"]
		if !ok || ap != false {
			open = append(open, loc)
		}
	})
	if len(open) > 0 {
		sort.Strings(open)
		t.Errorf("object subschemas missing additionalProperties:false (ADR-081 rule 2): %v", open)
	}
}

// authorityVocabulary is the set of property names that would let a crew
// message express authority — scope paths, constraints, delegation, autonomy,
// approval or a gate outcome. ADR-081 rule 2 makes a message ADVICE, NEVER AN
// ORDER, and that is encoded as the schema's closed shape (the test above)
// PLUS the absence of this vocabulary, never as prose.
var authorityVocabulary = []string{
	"scope", "files", "constraints", "forbidden_paths", "max_files_changed",
	"delegation", "delegated", "autonomy", "approval", "approved",
	"gate", "verdict", "outcome",
}

// TestSchema_DeclaresNoAuthorityVocabulary walks every property NAME declared
// anywhere in the shipped document and fails on any member of the authority
// vocabulary. There is deliberately no exemption list: a legitimate need for
// one of these names is a decision to make explicitly, not a carve-out that
// quietly weakens the test pinning rule 2.
func TestSchema_DeclaresNoAuthorityVocabulary(t *testing.T) {
	var doc any
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	banned := make(map[string]bool, len(authorityVocabulary))
	for _, b := range authorityVocabulary {
		banned[b] = true
	}
	var found []string
	walkSchema(doc, "#", func(loc string, obj map[string]any) {
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			return
		}
		for name := range props {
			if banned[name] {
				found = append(found, loc+"/properties/"+name)
			}
		}
	})
	if len(found) > 0 {
		sort.Strings(found)
		t.Errorf("schema declares authority-vocabulary properties (ADR-081 rule 2 forbids a field that can express scope, constraints, delegation or a gate outcome): %v", found)
	}
}

// walkSchema visits every JSON object in the document, reporting its JSON
// Pointer location. It walks the raw document rather than a compiled schema so
// the assertion is about the SHIPPED bytes.
func walkSchema(node any, loc string, visit func(loc string, obj map[string]any)) {
	switch n := node.(type) {
	case map[string]any:
		visit(loc, n)
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkSchema(n[k], loc+"/"+escapePointer(k), visit)
		}
	case []any:
		for i, v := range n {
			walkSchema(v, loc+"/"+strconv.Itoa(i), visit)
		}
	}
}

func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}

// TestSchemaRoleEnumMatchesAllRoles pins the Go vocabulary against the shipped
// schema's crew-role enum in BOTH directions, so a role added on one side only
// fails here instead of surfacing as a silently unaddressable role.
func TestSchemaRoleEnumMatchesAllRoles(t *testing.T) {
	var doc struct {
		Defs struct {
			CrewRole struct {
				Enum []string `json:"enum"`
			} `json:"crew-role"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	want := make([]string, 0, len(AllRoles))
	for _, r := range AllRoles {
		want = append(want, string(r))
	}
	if !reflect.DeepEqual(doc.Defs.CrewRole.Enum, want) {
		t.Errorf("schema crew-role enum = %v, AllRoles = %v", doc.Defs.CrewRole.Enum, want)
	}
}

// TestCanReceive_ExcludesImplementStageRoles pins the addressability predicate
// and, load-bearing for the design, pins that AllRoles CONTAINS implementer
// while CanReceive refuses it. That asymmetry is deliberate: the schema enum is
// the role VOCABULARY (an implementer is a legal sender), and this predicate is
// the single source of truth for who may be ADDRESSED (invariant #8). Narrowing
// the enum instead would make the Go rule dead code its own test could not
// redden.
func TestCanReceive_ExcludesImplementStageRoles(t *testing.T) {
	addressable := map[Role]bool{
		RoleCaptain:   true,
		RolePlanner:   true,
		RoleArchitect: true,
		RoleHistorian: true,
		RoleSecurity:  true,
		RoleReviewer:  true,
		// RoleImplementer is deliberately absent.
	}
	for _, r := range AllRoles {
		if got, want := CanReceive(r), addressable[r]; got != want {
			t.Errorf("CanReceive(%q) = %v, want %v", r, got, want)
		}
	}

	var sawImplementer bool
	for _, r := range AllRoles {
		if r == RoleImplementer {
			sawImplementer = true
		}
	}
	if !sawImplementer {
		t.Error("AllRoles must CONTAIN implementer: it is a real crew role and a legal sender_role, and keeping it in the enum is what leaves the Go no-implement-recipient rule the sole gate in its fixture's path")
	}
	if CanReceive(RoleImplementer) {
		t.Error("CanReceive(implementer) = true, want false (ARCHITECTURE §6 invariant #8)")
	}

	// Fail-closed for a value outside the vocabulary: the schema rejects it
	// upstream, and a caller reaching the predicate with an unvalidated value
	// must not get an addressable verdict.
	if CanReceive(Role("claude-opus-5")) {
		t.Error("CanReceive on a model identifier = true, want false")
	}
	if CanReceive(Role("")) {
		t.Error("CanReceive on the empty role = true, want false")
	}
}

// TestValidFixturesExistForEveryType is the issue's first acceptance criterion
// restated as a test: the closed five-member type set has one valid fixture
// each, so the corpus later E77 children reuse can never silently lose a type.
func TestValidFixturesExistForEveryType(t *testing.T) {
	for _, mt := range []MessageType{TypeConsult, TypeFinding, TypeWorkRequest, TypeNotice, TypeEscalation} {
		path := filepath.Join("testdata", "valid", string(mt)+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing valid fixture for type %q: %v", mt, err)
		}
	}
}
