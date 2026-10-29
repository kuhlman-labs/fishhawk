package server

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// escape_docs_test.go pins the documentation of POST /v0/runs/{run_id}/escapes
// (E94.3 / #4694) to the handler. A refusal code added to escape.go without
// its docs fails here, naming the sites to update:
//
//   - docs/api/v0.openapi.yaml: the route, its RECORD / CLOSE schemas, every
//     refusal code in the operation's responses, NO security_finding_id in the
//     RECORD schema (ADR-126 D7 deferred, R2), and the implement_review_started
//     reviewed_head paragraph (ADR-117 A1.7);
//   - docs/api/v0.md: the route line and an error-table row per escape code.

const escapeOpenAPIPath = "../../../docs/api/v0.openapi.yaml"

// escapeDocumentedCodes is every code handleRecordEscape answers. The shared
// ladder codes (authentication_required, insufficient_scope,
// account_forbidden, run_not_found, validation_failed, internal_error) are
// included because the operation must name them too.
var escapeDocumentedCodes = []string{
	"authentication_required", "account_forbidden", "run_token_forbidden", "insufficient_scope",
	"escape_unconfigured", "validation_failed", "run_not_found", "internal_error",
	"escape_attestation_requires_human", "escape_attestation_requires_forge_credential",
	"escape_already_recorded", "escape_already_closed",
	"escape_security_free_text_refused", "escape_subject_unresolved", "escape_concern_not_found",
	"escape_target_not_found", "escape_target_not_pending",
}

// escapeOpenAPIDoc parses the OpenAPI document into a generic map.
func escapeOpenAPIDoc(t *testing.T) (map[string]any, string) {
	t.Helper()
	raw, err := os.ReadFile(escapeOpenAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", escapeOpenAPIPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", escapeOpenAPIPath, err)
	}
	return doc, string(raw)
}

func escapeMapAt(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("docs/api/v0.openapi.yaml: no mapping at %s", strings.Join(path, " > "))
		}
		cur = next
	}
	return cur
}

// TestOpenAPI_EscapeRouteDocumented pins the route, its two request schemas,
// every refusal code, the absence of security_finding_id and the
// reviewed_head paragraph.
func TestOpenAPI_EscapeRouteDocumented(t *testing.T) {
	doc, raw := escapeOpenAPIDoc(t)
	op := escapeMapAt(t, doc, "paths", "/v0/runs/{run_id}/escapes", "post")
	if op["operationId"] != "recordEscape" {
		t.Errorf("docs/api/v0.openapi.yaml: escapes operationId = %v, want recordEscape", op["operationId"])
	}
	opYAML, err := yaml.Marshal(op)
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	for _, code := range escapeDocumentedCodes {
		if !strings.Contains(string(opYAML), code) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/runs/{run_id}/escapes does not name refusal code %q", code)
		}
	}
	for _, key := range []string{"reviewed_head", "concern:<concern_uuid>", "pending_judgement", "supersedes_sequence"} {
		if !strings.Contains(string(opYAML), key) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/runs/{run_id}/escapes does not state %q", key)
		}
	}

	schemas := escapeMapAt(t, doc, "components", "schemas")
	record := escapeMapAt(t, schemas, "EscapeRecordRequest", "properties")
	if _, ok := record["security_finding_id"]; ok {
		t.Error("docs/api/v0.openapi.yaml EscapeRecordRequest declares security_finding_id; the handler refuses it (ADR-126 D7 deferred to E96 / #4774)")
	}
	for _, field := range []string{"source", "severity", "category", "description", "files", "head_sha",
		"introducing_commits", "concern_id", "idempotency_key", "security_class", "advisory_id", "delegated"} {
		if _, ok := record[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml EscapeRecordRequest is missing %q (escapeRecordRequest)", field)
		}
	}
	if escapeMapAt(t, schemas, "EscapeRecordRequest")["additionalProperties"] != false {
		t.Error("docs/api/v0.openapi.yaml EscapeRecordRequest must set additionalProperties: false (strict decode)")
	}
	closeProps := escapeMapAt(t, schemas, "EscapeCloseRequest", "properties")
	for _, field := range []string{"supersedes_sequence", "attestation", "delegated"} {
		if _, ok := closeProps[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml EscapeCloseRequest is missing %q (escapeCloseRequest)", field)
		}
	}
	if len(closeProps) != 3 {
		t.Errorf("docs/api/v0.openapi.yaml EscapeCloseRequest has %d properties, want exactly 3", len(closeProps))
	}

	// The A1.7 reviewed_head paragraph beside the #4077 round_origin prose.
	if !strings.Contains(raw, "**`reviewed_head` on `implement_review_started` (ADR-117 A1.7,") {
		t.Error("docs/api/v0.openapi.yaml lacks the implement_review_started reviewed_head paragraph (rerunRunReview description)")
	}
	for _, key := range []string{"partial_files", "omitted_files", "head_unknown", "compare_head"} {
		if !strings.Contains(raw, key) {
			t.Errorf("docs/api/v0.openapi.yaml does not name reviewed_head key %q", key)
		}
	}
}

// TestV0Doc_EscapeRouteDocumented pins docs/api/v0.md: the route line and an
// error-table row per escape-specific code.
func TestV0Doc_EscapeRouteDocumented(t *testing.T) {
	md := readRepoDoc(t, "docs/api/v0.md")
	if !strings.Contains(md, "POST   /v0/runs/{run_id}/escapes") {
		t.Error("docs/api/v0.md lacks the POST /v0/runs/{run_id}/escapes route line")
	}
	for _, code := range escapeDocumentedCodes {
		if !strings.HasPrefix(code, "escape_") {
			continue
		}
		if !strings.Contains(md, "`"+code+"`") {
			t.Errorf("docs/api/v0.md error table does not name `%s`", code)
		}
	}
}
