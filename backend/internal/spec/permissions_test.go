package spec_test

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// permissionsCanonicalSchemaPath is the SHIPPED canonical v2 schema the honesty
// test reads (operator condition 1 / acceptance criteria 5-6): the assertion
// binds to the artifact the product embeds, not a paraphrase.
const permissionsCanonicalSchemaPath = "../../../docs/spec/workflow-v2.schema.json"

// permissionsDisclaimer is the EXACT non-enforcement sentence every permissions
// description must carry (operator condition 1). The honesty test asserts it is
// PRESENT, then runs the banned-overclaim scan on the description WITH THIS
// SENTENCE REMOVED — a negated mention is the goal, an affirmative claim is the
// defect, so the required wording never fails its own test.
const permissionsDisclaimer = "This declaration is validated, audited and surfaced but is NOT enforced until E51 (#2133); do not rely on it as containment."

// TestNormalizeStagePermissions_NetworkIntoEgress is the NORMALIZATION IDENTITY
// (verification 2): after ParseBytes, Stage.Egress is non-nil and equal to the
// declared permissions.network on every stage type that accepts it — the
// invariant every downstream egress consumer depends on.
func TestNormalizeStagePermissions_NetworkIntoEgress(t *testing.T) {
	for _, stageType := range []string{"implement", "acceptance"} {
		t.Run(stageType, func(t *testing.T) {
			doc := `version: "2"
workflows:
  wf:
    stages:
      - id: s0
        type: ` + stageType + `
        executor:
          agent: claude-code
        permissions:
          network:
            target_hosts: ["staging.example.com:8443"]
`
			parsed, err := spec.ParseBytes([]byte(doc))
			if err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
			st := parsed.Workflows["wf"].Stages[0]
			if st.Egress == nil {
				t.Fatal("Stage.Egress is nil; permissions.network was not normalized into Egress")
			}
			if len(st.Egress.TargetHosts) != 1 || st.Egress.TargetHosts[0] != "staging.example.com:8443" {
				t.Errorf("Egress.TargetHosts = %v, want the declared permissions.network hosts", st.Egress.TargetHosts)
			}
			if st.Permissions == nil || st.Permissions.Network == nil {
				t.Fatal("Stage.Permissions.Network is nil; normalization should COPY, not move")
			}
			if st.Egress.TargetHosts[0] != st.Permissions.Network.TargetHosts[0] {
				t.Errorf("Egress and Permissions.Network disagree: %v vs %v", st.Egress.TargetHosts, st.Permissions.Network.TargetHosts)
			}
		})
	}
}

// TestParse_Permissions_Rejections is the PER-FAILURE-MODE table (verification
// 3): one behavioral case per enumerated rejection reachable through ParseBytes,
// each asserting the reported JSON pointer AND the message substring. Each is
// paired with a positive control below.
func TestParse_Permissions_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
		wantMsg  string
	}{
		{
			name: "both_egress_and_permissions_network",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: accept
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts: ["a.example.com"]
        permissions:
          network:
            target_hosts: ["b.example.com"]
`,
			wantPath: "/workflows/wf/stages/0/permissions/network",
			wantMsg:  "declares both `egress` and `permissions.network`",
		},
		{
			name: "permissions_on_human_executor",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: gate
        type: review
        executor:
          human: true
        permissions:
          shell: restricted
`,
			wantPath: "/workflows/wf/stages/0/permissions",
			wantMsg:  "valid only on a stage with an agent executor, not a human executor",
		},
		{
			name: "permissions_on_delegate_executor",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: release
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
        permissions:
          shell: restricted
`,
			wantPath: "/workflows/wf/stages/0/permissions",
			wantMsg:  "not a delegate executor",
		},
		{
			name: "malformed_write_glob",
			doc: `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor:
          agent: claude-code
        permissions:
          write: ["["]
`,
			wantPath: "/workflows/wf/stages/0/permissions/write",
			wantMsg:  "malformed path glob",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(tc.doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if ve.Path != tc.wantPath {
				t.Errorf("ValidationError.Path = %q, want %q", ve.Path, tc.wantPath)
			}
			if !strings.Contains(ve.Message, tc.wantMsg) {
				t.Errorf("ValidationError.Message = %q, want it to contain %q", ve.Message, tc.wantMsg)
			}
		})
	}
}

// TestParse_Permissions_ExecutorBindingCarriesDisclaimer pins operator condition
// 4: the executor-binding rejection and the egress/permissions.network conflict
// message MUST carry the non-enforcement disclaimer, because both invite the
// inference that declaring a permission does something.
func TestParse_Permissions_ExecutorBindingCarriesDisclaimer(t *testing.T) {
	docs := map[string]string{
		"executor_binding": `version: "2"
workflows:
  wf:
    stages:
      - id: gate
        type: review
        executor:
          human: true
        permissions:
          shell: restricted
`,
		"conflict": `version: "2"
workflows:
  wf:
    stages:
      - id: accept
        type: acceptance
        executor:
          agent: claude-code
        egress:
          target_hosts: ["a.example.com"]
        permissions:
          network:
            target_hosts: ["b.example.com"]
`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(doc))
			var ve *spec.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Message, "#2133") || !strings.Contains(ve.Message, "enforced until E51") {
				t.Errorf("message = %q, want it to carry the non-enforcement disclaimer (#2133, enforced until E51)", ve.Message)
			}
		})
	}
}

// TestValidate_Permissions_UnknownShell exercises the Go-side shell-posture
// rejection (verification 3). The schema enum catches an unknown posture through
// ParseBytes, so this drives spec.Validate on a hand-built Spec to reach
// validateStagePermissions' shell branch, asserting it names the valid set and —
// per operator condition 4 — does NOT bolt on the disclaimer (a syntax
// complaint).
func TestValidate_Permissions_UnknownShell(t *testing.T) {
	s := &spec.Spec{
		Version: "2",
		Workflows: map[string]spec.Workflow{
			"wf": {Stages: []spec.Stage{{
				ID:          "apply",
				Type:        spec.StageTypeImplement,
				Executor:    spec.Executor{Agent: "claude-code"},
				Permissions: &spec.StagePermissions{Shell: spec.ShellPosture("dangerous")},
			}}},
		},
	}
	err := spec.Validate(s)
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if ve.Path != "/workflows/wf/stages/0/permissions/shell" {
		t.Errorf("path = %q, want the shell pointer", ve.Path)
	}
	if !strings.Contains(ve.Message, "unknown shell posture") || !strings.Contains(ve.Message, "none, restricted, unrestricted") {
		t.Errorf("message = %q, want it to name the valid posture set", ve.Message)
	}
	if strings.Contains(ve.Message, "#2133") {
		t.Errorf("message = %q, a pure syntax error should NOT carry the E51 disclaimer (condition 4)", ve.Message)
	}
}

// TestValidate_BothEgressAndPermissionsNetwork_ParseOnlyContract documents that
// the `egress` + `permissions.network` both-declared CONFLICT is a ParseBytes-
// seam guarantee (normalizeStagePermissions), deliberately NOT re-checked by the
// exported Validate (fix-up low/untested-path). By the time Validate runs in the
// normal flow, normalizeStagePermissions has already folded permissions.network
// INTO Stage.Egress — a pointer COPY, not a move (TestNormalizeStagePermissions_
// NetworkIntoEgress pins the two must then ALIAS) — so Validate structurally
// cannot distinguish "author declared both" from "network was normalized into
// egress" and must not re-check. A hand-constructed Spec passed DIRECTLY to
// Validate with both fields set therefore validates clean. That is safe because
// every real YAML document flows through ParseBytes (and the CLI mirrors the
// check), where the conflict IS rejected — pinned in full by
// TestParse_Permissions_Rejections' both_egress_and_permissions_network case —
// so the conflict is unreachable via input. This test pins the parse-only
// posture so it stays intentional rather than a silent gap.
func TestValidate_BothEgressAndPermissionsNetwork_ParseOnlyContract(t *testing.T) {
	s := &spec.Spec{
		Version: "2",
		Workflows: map[string]spec.Workflow{
			"wf": {Stages: []spec.Stage{{
				ID:       "apply",
				Type:     spec.StageTypeImplement,
				Executor: spec.Executor{Agent: "claude-code"},
				Egress:   &spec.StageEgress{TargetHosts: []string{"a.example.com"}},
				Permissions: &spec.StagePermissions{
					Network: &spec.StageEgress{TargetHosts: []string{"b.example.com"}},
				},
			}}},
		},
	}
	if err := spec.Validate(s); err != nil {
		t.Fatalf("Validate on a both-set hand-constructed Spec = %v, want nil "+
			"(the conflict is a ParseBytes-seam guarantee via normalizeStagePermissions, not re-checked in Validate)", err)
	}
}

// TestParse_Permissions_SchemaRejections covers the rejections the JSON Schema
// owns (verification 3): the empty block, an empty network host list, a URL-form
// network entry (the ADR-050 hosts-not-URLs preservation), an unknown shell
// posture (enum), and an empty write entry (minLength). Each is a *SchemaError.
func TestParse_Permissions_SchemaRejections(t *testing.T) {
	docs := map[string]string{
		"empty_permissions_block": `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions: {}
`,
		"network_empty_hosts": `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions:
          network:
            target_hosts: []
`,
		"network_url_form": `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions:
          network:
            target_hosts: ["https://staging.example.com/deploy"]
`,
		"shell_unknown_enum": `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions:
          shell: dangerous
`,
		"write_empty_entry": `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions:
          write: [""]
`,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(doc))
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *SchemaError", err)
			}
		})
	}
}

// TestParse_Permissions_ValidForms is the positive control set (verification 3):
// each valid shape is ACCEPTED, so the rejection tests pin "rejects exactly
// this" rather than "rejects something".
func TestParse_Permissions_ValidForms(t *testing.T) {
	for _, shell := range []string{"none", "restricted", "unrestricted"} {
		t.Run("shell_"+shell, func(t *testing.T) {
			doc := `version: "2"
workflows:
  wf:
    stages:
      - id: apply
        type: implement
        executor: {agent: claude-code}
        permissions:
          network: {target_hosts: ["staging.example.com"]}
          write: ["src/**/*.go", "docs/**"]
          shell: ` + shell + `
`
			parsed, err := spec.ParseBytes([]byte(doc))
			if err != nil {
				t.Fatalf("valid permissions block should parse, got %v", err)
			}
			st := parsed.Workflows["wf"].Stages[0]
			if st.Permissions == nil || string(st.Permissions.Shell) != shell {
				t.Errorf("Permissions.Shell = %+v, want %q", st.Permissions, shell)
			}
			if st.Egress == nil {
				t.Error("network should normalize into Egress even alongside write/shell")
			}
		})
	}
}

// TestWritePredicate_DialectParity is the SHARED-PREDICATE DIALECT PARITY table
// (verification 5): identical glob/path pairs fed to
// StagePermissions.WritePredicate().Match and to a bare spec.Predicate{Paths}
// must yield identical verdicts — including `**` crossing `/` and a byte-unsafe
// path — so the write dialect cannot drift from applies_to / escalations.
func TestWritePredicate_DialectParity(t *testing.T) {
	globs := []string{"src/**/*.go"}
	paths := []string{
		"src/a/b/c.go",         // ** crosses /
		"src/c.go",             // ** matches zero segments
		"other/c.go",           // no match
		"src/\x00weird/c.go",   // byte-unsafe path
		"src/deep/deeper/x.go", // deep match
	}
	wp := spec.StagePermissions{Write: globs}.WritePredicate()
	bare := spec.Predicate{Paths: globs}
	for _, p := range paths {
		gotW, errW := wp.Match(spec.Change{Paths: []string{p}})
		gotB, errB := bare.Match(spec.Change{Paths: []string{p}})
		if gotW != gotB || (errW == nil) != (errB == nil) {
			t.Errorf("path %q: WritePredicate=(%v,%v) bare=(%v,%v); the write dialect drifted from the shared predicate", p, gotW, errW, gotB, errB)
		}
	}
}

// TestPermissionsSchemaHonesty is the HONESTY DONE-MEANS TEST (verification 7),
// run against the SHIPPED canonical schema: (a) the stage_permissions block
// description and EACH of its three field descriptions carry the exact
// non-enforcement disclaimer and the literal "#2133"; (b) no permissions-block
// description makes an affirmative overclaim, scanned AFTER the disclaimer
// sentence is removed (operator condition 1 — a negated mention is the goal).
func TestPermissionsSchemaHonesty(t *testing.T) {
	raw, err := os.ReadFile(permissionsCanonicalSchemaPath)
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("decode canonical schema: %v", err)
	}
	defs, _ := root["$defs"].(map[string]any)
	perms, ok := defs["stage_permissions"].(map[string]any)
	if !ok {
		t.Fatal("$defs/stage_permissions missing from the canonical schema")
	}
	descs := map[string]string{"stage_permissions": stringField(t, perms, "description")}
	props, _ := perms["properties"].(map[string]any)
	for _, field := range []string{"network", "write", "shell"} {
		fp, ok := props[field].(map[string]any)
		if !ok {
			t.Fatalf("$defs/stage_permissions.properties.%s missing", field)
		}
		descs[field] = stringField(t, fp, "description")
	}

	for name, desc := range descs {
		if !strings.Contains(desc, permissionsDisclaimer) {
			t.Errorf("%s description is missing the exact non-enforcement disclaimer sentence:\n  want substring: %q\n  got: %q", name, permissionsDisclaimer, desc)
		}
		if !strings.Contains(desc, "#2133") {
			t.Errorf("%s description does not name the enforcement tracker #2133", name)
		}
		scrubbed := strings.ReplaceAll(desc, permissionsDisclaimer, "")
		if hits := bannedOverclaimShapes(scrubbed); len(hits) > 0 {
			t.Errorf("%s description makes an affirmative overclaim (banned shapes %v) outside the non-enforcement disclaimer:\n  %q", name, hits, desc)
		}
	}
}

var affirmativePreventBlockRE = regexp.MustCompile(`\b(prevents?|blocks?)\b`)

// bannedOverclaimShapes returns the affirmative overclaim shapes present in
// text. Bare nouns (containment / contained / sandbox / isolation / security
// control) are inherently overclaiming and are banned outright — their only
// legitimate occurrence is inside the disclaimer, which the caller removes
// first. For prevent/block the check bans the AFFIRMATIVE shape only (operator
// condition 1): a negated mention ("does not prevent") is allowed, so a match is
// flagged only when not immediately preceded by a negation.
func bannedOverclaimShapes(text string) []string {
	lower := strings.ToLower(text)
	var hits []string
	for _, noun := range []string{"containment", "contained", "sandbox", "isolation", "security control"} {
		if strings.Contains(lower, noun) {
			hits = append(hits, noun)
		}
	}
	for _, m := range affirmativePreventBlockRE.FindAllStringIndex(lower, -1) {
		start := m[0]
		lo := start - 12
		if lo < 0 {
			lo = 0
		}
		prefix := lower[lo:start]
		if strings.Contains(prefix, "not ") || strings.Contains(prefix, "never ") {
			continue
		}
		hits = append(hits, lower[m[0]:m[1]])
	}
	return hits
}

func stringField(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q is not a string in %v", key, m)
	}
	return s
}
