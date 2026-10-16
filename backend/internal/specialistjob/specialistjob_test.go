package specialistjob

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	canonicalSchemaPath = "../../../docs/spec/specialist-job-v1.schema.json"
	planSchemaPath      = "../../../docs/spec/plan-standard-v1.schema.json"
	sha1                = "1111111111111111111111111111111111111111"
	sha2                = "2222222222222222222222222222222222222222"
	runA                = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c01"
	runB                = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c02"
)

// validInputs is one schema- and semantics-valid inputs object per kind.
func validInputs(k Kind) map[string]any {
	switch k {
	case KindCounterfactualBattery:
		return map[string]any{
			"mutations": []any{map[string]any{
				"control_path": "backend/internal/x/x.go",
				"mutation":     map[string]any{"find": "return err", "replace": "return nil"},
				"tests":        map[string]any{"run": "^TestX$", "package": "./backend/internal/x"},
				"expect":       "red",
			}},
			"probes": []any{validProbe("p1")},
		}
	case KindPlanCrosscheck:
		return map[string]any{
			"questions":    []any{validProbe("q1")},
			"focus_files":  []any{map[string]any{"path": "backend/x.go", "expect": "present"}},
			"compare_runs": []any{map[string]any{"run_id": runA, "head_sha": sha1, "base_sha": sha2}},
		}
	case KindOverlapScan:
		return map[string]any{
			"runs": []any{
				map[string]any{"run_id": runA, "head_sha": sha1, "base_sha": sha2},
				map[string]any{"run_id": runB, "head_sha": sha2, "base_sha": sha1},
			},
			"watch_paths": []any{"backend/internal/*/store.go"},
		}
	case KindFaninBuild:
		return map[string]any{
			"slices":   []any{map[string]any{"name": "slice-1", "head_sha": sha1}},
			"packages": []any{"./backend/internal/x/..."},
		}
	case KindInventory:
		return map[string]any{"command": "git_log", "args": map[string]any{"paths": []any{"docs"}, "max_count": 20}}
	case KindDraft:
		return map[string]any{
			"document_kind": "adr",
			"title":         "Specialist jobs",
			"brief":         "Why.",
			"outline":       []any{map[string]any{"heading": "Context", "body": "ctx"}},
		}
	}
	panic("unknown kind")
}

func validProbe(id string) map[string]any {
	return map[string]any{
		"id":       id,
		"question": "does the store exist?",
		"probe":    map[string]any{"path_exists": map[string]any{"path": "backend/internal/x/store.go"}},
		"expect":   "pass",
	}
}

func requestDoc(k Kind, inputs map[string]any) map[string]any {
	subject := map[string]any{"run_id": runA, "head_sha": sha1}
	return map[string]any{"schema_version": SchemaVersion, "kind": string(k), "subject": subject, "inputs": inputs}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantSchemaError(t *testing.T, err error, path string) *SchemaError {
	t.Helper()
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want *SchemaError at %s", err, err, path)
	}
	if path != "" && se.Path != path {
		t.Fatalf("SchemaError.Path = %q, want %q (%s)", se.Path, path, se.Message)
	}
	return se
}

// TestEmbeddedSchemaMatchesCanonical is the in-loop mirror-drift belt.
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile(canonicalSchemaPath)
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	if got := EmbeddedSchema(); !reflect.DeepEqual(got, canonical) {
		t.Errorf("embedded %s drifted from %s: run scripts/sync-schemas", embeddedSchemaPath, canonicalSchemaPath)
	}
}

func TestParseRequest_AcceptsEveryKind(t *testing.T) {
	for _, k := range AllKinds {
		t.Run(string(k), func(t *testing.T) {
			req, err := ParseRequest(mustJSON(t, requestDoc(k, validInputs(k))))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if req.Kind != k {
				t.Fatalf("Kind = %q", req.Kind)
			}
			typed := map[Kind]bool{
				KindCounterfactualBattery: req.Battery != nil, KindPlanCrosscheck: req.Crosscheck != nil,
				KindOverlapScan: req.Overlap != nil, KindFaninBuild: req.Fanin != nil,
				KindInventory: req.Inventory != nil, KindDraft: req.Draft != nil,
			}
			for kk, set := range typed {
				if set != (kk == k) {
					t.Errorf("typed inputs for %q set=%v, want %v", kk, set, kk == k)
				}
			}
			var gotInputs, wantInputs any
			_ = json.Unmarshal(req.Inputs, &gotInputs)
			_ = json.Unmarshal(mustJSON(t, validInputs(k)), &wantInputs)
			if !reflect.DeepEqual(gotInputs, wantInputs) {
				t.Errorf("raw Inputs = %s", req.Inputs)
			}
		})
	}
}

func TestParseRequest_RejectsUnknownInputMember(t *testing.T) {
	for _, k := range AllKinds {
		t.Run(string(k), func(t *testing.T) {
			in := validInputs(k)
			in["bogus"] = true
			_, err := ParseRequest(mustJSON(t, requestDoc(k, in)))
			wantSchemaError(t, err, "/inputs/bogus")
		})
	}
}

func TestParseRequest_RejectsOversizedInputs(t *testing.T) {
	cases := map[string]func() map[string]any{
		"17 mutations": func() map[string]any {
			in := validInputs(KindCounterfactualBattery)
			m := in["mutations"].([]any)[0]
			var ms []any
			for i := 0; i < 17; i++ {
				ms = append(ms, m)
			}
			in["mutations"] = ms
			return requestDoc(KindCounterfactualBattery, in)
		},
		"1001-char question": func() map[string]any {
			in := validInputs(KindPlanCrosscheck)
			q := validProbe("q1")
			q["question"] = strings.Repeat("q", 1001)
			in["questions"] = []any{q}
			return requestDoc(KindPlanCrosscheck, in)
		},
		"8193-byte brief": func() map[string]any {
			in := validInputs(KindDraft)
			in["brief"] = strings.Repeat("b", 8193)
			return requestDoc(KindDraft, in)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseRequest(mustJSON(t, mk()))
			wantSchemaError(t, err, "")
		})
	}
}

func TestParseRequest_RejectsRequesterMember(t *testing.T) {
	doc := requestDoc(KindDraft, validInputs(KindDraft))
	doc["requester"] = map[string]any{"seat": "operator", "actor_kind": "user", "subject": "mallory"}
	_, err := ParseRequest(mustJSON(t, doc))
	wantSchemaError(t, err, "/requester")
}

func TestParseRequest_RequiresHeadSHAForRunnerKinds(t *testing.T) {
	for _, k := range AllKinds {
		t.Run(string(k), func(t *testing.T) {
			doc := requestDoc(k, validInputs(k))
			doc["subject"] = map[string]any{"run_id": runA}
			_, err := ParseRequest(mustJSON(t, doc))
			if k == KindDraft {
				if err != nil {
					t.Fatalf("draft without head_sha: %v", err)
				}
				return
			}
			wantSchemaError(t, err, "/subject")
		})
	}
}

func TestParseRequest_BatteryControlPathRules(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		form    map[string]any
		wantErr error
	}{
		{"absolute", "/etc/x.go", nil, ErrInvalidControlPath},
		{"dotdot", "backend/../x.go", nil, ErrInvalidControlPath},
		{"backslash", `backend\x.go`, nil, ErrInvalidControlPath},
		{"test file", "backend/x_test.go", nil, ErrInvalidControlPath},
		{"blank", "   ", nil, ErrInvalidControlPath},
		{"both forms", "backend/x.go", map[string]any{"find": "a", "replace": "b", "patch": "p"}, ErrInvalidMutationForm},
		{"neither form", "backend/x.go", map[string]any{}, ErrInvalidMutationForm},
		{"find without replace", "backend/x.go", map[string]any{"find": "a"}, ErrInvalidMutationForm},
		{"replace without find", "backend/x.go", map[string]any{"replace": "b"}, ErrInvalidMutationForm},
		{"replace equals find", "backend/x.go", map[string]any{"find": "a", "replace": "a"}, ErrInvalidMutationForm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInputs(KindCounterfactualBattery)
			m := in["mutations"].([]any)[0].(map[string]any)
			m["control_path"] = tc.path
			if tc.form != nil {
				m["mutation"] = tc.form
			}
			_, err := ParseRequest(mustJSON(t, requestDoc(KindCounterfactualBattery, in)))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("patch form accepted", func(t *testing.T) {
		in := validInputs(KindCounterfactualBattery)
		in["mutations"].([]any)[0].(map[string]any)["mutation"] = map[string]any{"patch": "--- a\n+++ b\n"}
		if _, err := ParseRequest(mustJSON(t, requestDoc(KindCounterfactualBattery, in))); err != nil {
			t.Fatalf("patch form: %v", err)
		}
	})
	t.Run("empty replace deletes", func(t *testing.T) {
		in := validInputs(KindCounterfactualBattery)
		in["mutations"].([]any)[0].(map[string]any)["mutation"] = map[string]any{"find": "a", "replace": ""}
		if _, err := ParseRequest(mustJSON(t, requestDoc(KindCounterfactualBattery, in))); err != nil {
			t.Fatalf("empty replace: %v", err)
		}
	})
}

// TestParseRequest_PathAndPackageRules pins the repo-relative rules on every
// path and package a job executes or reads: an absolute path would turn
// file_contains into a host-file oracle and a leading `-` would inject a
// flag into go test / go list / git.
func TestParseRequest_PathAndPackageRules(t *testing.T) {
	cases := []struct {
		name    string
		kind    Kind
		mutate  func(in map[string]any)
		wantErr error
	}{
		{"battery tests.package absolute", KindCounterfactualBattery, func(in map[string]any) {
			in["mutations"].([]any)[0].(map[string]any)["tests"] = map[string]any{"run": "x", "package": "/abs"}
		}, ErrInvalidPackagePath},
		{"battery tests.package dotdot", KindCounterfactualBattery, func(in map[string]any) {
			in["mutations"].([]any)[0].(map[string]any)["tests"] = map[string]any{"run": "x", "package": "./../x"}
		}, ErrInvalidPackagePath},
		{"battery tests.package flag", KindCounterfactualBattery, func(in map[string]any) {
			in["mutations"].([]any)[0].(map[string]any)["tests"] = map[string]any{"run": "x", "package": "-exec=/bin/sh"}
		}, ErrInvalidPackagePath},
		{"probe path absolute", KindCounterfactualBattery, func(in map[string]any) {
			p := validProbe("p1")
			p["probe"] = map[string]any{"file_contains": map[string]any{"path": "/Users/x/.ssh/id_rsa", "literal": "KEY"}}
			in["probes"] = []any{p}
		}, ErrInvalidRepoPath},
		{"probe path_exists dotdot", KindPlanCrosscheck, func(in map[string]any) {
			p := validProbe("q1")
			p["probe"] = map[string]any{"path_exists": map[string]any{"path": "../outside"}}
			in["questions"] = []any{p}
		}, ErrInvalidRepoPath},
		{"probe go_test flag", KindPlanCrosscheck, func(in map[string]any) {
			p := validProbe("q1")
			p["probe"] = map[string]any{"go_test": map[string]any{"tests": map[string]any{"run": "x", "package": "-toolexec=x"}}}
			in["questions"] = []any{p}
		}, ErrInvalidPackagePath},
		{"focus file backslash", KindPlanCrosscheck, func(in map[string]any) {
			in["focus_files"] = []any{map[string]any{"path": `a\b`, "expect": "present"}}
		}, ErrInvalidRepoPath},
		{"watch path absolute", KindOverlapScan, func(in map[string]any) {
			in["watch_paths"] = []any{"/etc/*"}
		}, ErrInvalidRepoPath},
		{"fanin package flag", KindFaninBuild, func(in map[string]any) {
			in["packages"] = []any{"-exec=x"}
		}, ErrInvalidPackagePath},
		{"inventory git_log path flag", KindInventory, func(in map[string]any) {
			in["args"] = map[string]any{"paths": []any{"--output=/tmp/x"}, "max_count": 5}
		}, ErrInvalidRepoPath},
		{"inventory ls_files pathspec absolute", KindInventory, func(in map[string]any) {
			in["command"] = "ls_files"
			in["args"] = map[string]any{"pathspec": []any{"/etc"}, "max_entries": 5}
		}, ErrInvalidRepoPath},
		{"inventory go_list package dotdot", KindInventory, func(in map[string]any) {
			in["command"] = "go_list"
			in["args"] = map[string]any{"packages": []any{"../x"}}
		}, ErrInvalidPackagePath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInputs(tc.kind)
			tc.mutate(in)
			_, err := ParseRequest(mustJSON(t, requestDoc(tc.kind, in)))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseRequest_InventoryCommandEnum(t *testing.T) {
	in := validInputs(KindInventory)
	in["command"] = "rm_rf"
	_, err := ParseRequest(mustJSON(t, requestDoc(KindInventory, in)))
	wantSchemaError(t, err, "/inputs/command")
}

// TestParseRequest_HostInventoryCommands (#4330): each host.* id parses with
// the empty args object; a host id carrying any args member, and every id
// outside the closed set (a shell, git, a host.* lookalike), is refused.
func TestParseRequest_HostInventoryCommands(t *testing.T) {
	for _, cmd := range []string{InventoryHostMemory, InventoryHostTopMemory, InventoryHostLoad, InventoryHostContainers} {
		req, err := ParseRequest(mustJSON(t, requestDoc(KindInventory, map[string]any{"command": cmd, "args": map[string]any{}})))
		if err != nil {
			t.Fatalf("%s with {}: %v", cmd, err)
		}
		if req.Inventory == nil || req.Inventory.Command != cmd || !reflect.DeepEqual(req.Inventory.Args, InventoryArgs{}) {
			t.Fatalf("%s typed inputs = %+v", cmd, req.Inventory)
		}
		for _, args := range []map[string]any{
			{"max_count": 1},
			{"paths": []any{"docs"}},
			{"pid": 1},
		} {
			_, err := ParseRequest(mustJSON(t, requestDoc(KindInventory, map[string]any{"command": cmd, "args": args})))
			wantSchemaError(t, err, "")
		}
	}
	for _, cmd := range []string{"host.shell", "sh", "git", "host.memory ", "HOST.MEMORY", "host.*"} {
		_, err := ParseRequest(mustJSON(t, requestDoc(KindInventory, map[string]any{"command": cmd, "args": map[string]any{}})))
		wantSchemaError(t, err, "/inputs/command")
	}
}

// TestParseCompletion_HostInventoryAndNotApplicable (#4330 condition 3): a
// serialized host.memory completion with typed evidence, and an inconclusive
// not_applicable completion without evidence, both pass ParseCompletion; the
// evidence command enum refuses an id outside the closed set.
func TestParseCompletion_HostInventoryAndNotApplicable(t *testing.T) {
	ev := map[string]any{"command": InventoryHostMemory, "lines": []any{"# vm_stat", "Pages free: 1000."}, "truncated": false, "exit_code": 0}
	c, err := ParseCompletion(mustJSON(t, completionDoc(KindInventory, "pass", ev)), KindInventory)
	if err != nil {
		t.Fatalf("host.memory completion: %v", err)
	}
	typed, err := DecodeEvidence(KindInventory, c.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if got := typed.(*InventoryEvidence); got.Command != InventoryHostMemory || len(got.Lines) != 2 {
		t.Fatalf("typed evidence = %+v", got)
	}
	na := completionDoc(KindInventory, "inconclusive", nil)
	na["reason"] = "not_applicable"
	c, err = ParseCompletion(mustJSON(t, na), KindInventory)
	if err != nil {
		t.Fatalf("not_applicable completion: %v", err)
	}
	if c.Reason != "not_applicable" || len(c.Evidence) != 0 {
		t.Fatalf("not_applicable completion = %+v", c)
	}
	ev["command"] = "host.shell"
	_, err = ParseCompletion(mustJSON(t, completionDoc(KindInventory, "pass", ev)), KindInventory)
	wantSchemaError(t, err, "/evidence/command")
}

// TestInventoryCommands_MatchSchemaEnums pins both schema command enums
// (inputs-inventory and evidence-inventory) to InventoryCommands, and both
// reason enums (completion and result) to the same set carrying not_applicable.
func TestInventoryCommands_MatchSchemaEnums(t *testing.T) {
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, def := range []string{"inputs-inventory", "evidence-inventory"} {
		if got := doc.Defs[def].Properties["command"].Enum; !reflect.DeepEqual(got, InventoryCommands) {
			t.Errorf("$defs.%s.command enum = %v, want InventoryCommands %v", def, got, InventoryCommands)
		}
	}
	completion := doc.Defs["completion"].Properties["reason"].Enum
	if result := doc.Defs["result"].Properties["reason"].Enum; !reflect.DeepEqual(completion, result) {
		t.Errorf("completion reason enum %v != result reason enum %v", completion, result)
	}
	found := false
	for _, r := range completion {
		found = found || r == "not_applicable"
	}
	if !found {
		t.Errorf("completion reason enum %v lacks not_applicable", completion)
	}
}

func TestParseRequest_InventoryArgsSelectedByCommand(t *testing.T) {
	in := validInputs(KindInventory)
	in["command"] = "go_list" // args still carry git_log's members
	_, err := ParseRequest(mustJSON(t, requestDoc(KindInventory, in)))
	wantSchemaError(t, err, "")
}

func TestParseRequest_RejectsDuplicateMember(t *testing.T) {
	// The inputs member is repeated: the schema layer (map, last-wins) would
	// see the valid second copy while the typed decode would merge both.
	body := `{"schema_version":"specialist-job-v1","kind":"draft","subject":{"run_id":"` + runA + `"},` +
		`"inputs":{"document_kind":"adr","title":"t","bogus_extra":1},` +
		`"inputs":{"document_kind":"adr","title":"t"}}`
	_, err := ParseRequest([]byte(body))
	if !errors.Is(err, ErrDuplicateMember) {
		t.Fatalf("err = %v, want ErrDuplicateMember", err)
	}
}

func TestParseRequest_MalformedAndEmpty(t *testing.T) {
	for _, body := range []string{"", "   ", "{", `{"kind":`} {
		var pe *ParseError
		_, err := ParseRequest([]byte(body))
		if !errors.As(err, &pe) {
			t.Errorf("body %q: err = %v, want *ParseError", body, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "specialist job parse error: ") {
			t.Errorf("ParseError text = %q", err.Error())
		}
		if body == "{" && errors.Unwrap(err) == nil {
			t.Error("ParseError does not unwrap its cause")
		}
	}
	_, err := ParseRequest(mustJSON(t, map[string]any{"kind": "draft"}))
	if se := wantSchemaError(t, err, ""); !strings.HasPrefix(se.Error(), "specialist job schema error at ") {
		t.Errorf("SchemaError text = %q", se.Error())
	}
}

// validEvidence is one schema-valid evidence object per kind.
func validEvidence(k Kind) map[string]any {
	switch k {
	case KindCounterfactualBattery:
		return map[string]any{
			"mutations": []any{map[string]any{"control_path": "backend/x.go", "observed": "red", "baseline": "green", "mutation_sha256": strings.Repeat("a", 64)}},
			"probes":    []any{map[string]any{"id": "p1", "observed": "pass", "expected": "pass", "matched": true}},
		}
	case KindOverlapScan:
		return map[string]any{
			"runs":       []any{map[string]any{"run_id": runA, "resolved": true, "changed_files": []any{"a.go"}, "truncated": false}},
			"overlaps":   []any{map[string]any{"run_a": runA, "run_b": runB, "paths": []any{"a.go"}, "truncated": false}},
			"watch_hits": []any{},
		}
	case KindFaninBuild:
		return map[string]any{
			"slices":   []any{map[string]any{"name": "s1", "head_sha": sha1, "outcome": "merged", "conflict_paths": []any{}}},
			"packages": []any{map[string]any{"package": "./x", "outcome": "pass"}},
		}
	case KindPlanCrosscheck:
		return map[string]any{
			"questions":    []any{},
			"focus_files":  []any{map[string]any{"path": "x.go", "expect": "present", "observed": "present", "matched": true, "touched_by": []any{}}},
			"compare_runs": []any{map[string]any{"run_id": runA, "resolved": true, "overlap_paths": []any{}}},
		}
	case KindInventory:
		return map[string]any{"command": "go_list", "lines": []any{"x"}, "truncated": false, "exit_code": 0}
	case KindDraft:
		return map[string]any{"text": "# t\n"}
	}
	panic("unknown kind")
}

func completionDoc(k Kind, verdict string, evidence map[string]any) map[string]any {
	d := map[string]any{"schema_version": SchemaVersion, "execution_nonce": "n-1", "kind": string(k), "verdict": verdict, "duration_ms": 12}
	if evidence != nil {
		d["evidence"] = evidence
	}
	return d
}

func TestParseCompletion_AcceptsEveryKind(t *testing.T) {
	for _, k := range AllKinds {
		t.Run(string(k), func(t *testing.T) {
			c, err := ParseCompletion(mustJSON(t, completionDoc(k, "pass", validEvidence(k))), k)
			if err != nil {
				t.Fatalf("ParseCompletion: %v", err)
			}
			if _, err := DecodeEvidence(k, c.Evidence); err != nil {
				t.Fatalf("DecodeEvidence: %v", err)
			}
		})
	}
}

func TestParseCompletion_KindMismatch(t *testing.T) {
	body := mustJSON(t, completionDoc(KindOverlapScan, "pass", validEvidence(KindOverlapScan)))
	if _, err := ParseCompletion(body, KindCounterfactualBattery); !errors.Is(err, ErrKindMismatch) {
		t.Fatalf("err = %v, want ErrKindMismatch", err)
	}
}

func TestParseCompletion_EvidenceMustMatchItsKind(t *testing.T) {
	body := mustJSON(t, completionDoc(KindCounterfactualBattery, "pass", validEvidence(KindOverlapScan)))
	_, err := ParseCompletion(body, KindCounterfactualBattery)
	wantSchemaError(t, err, "")
}

func TestParseCompletion_InconclusiveMayOmitEvidenceOnlyWithReason(t *testing.T) {
	ok := completionDoc(KindFaninBuild, "inconclusive", nil)
	ok["reason"] = "kind_not_executable"
	if _, err := ParseCompletion(mustJSON(t, ok), KindFaninBuild); err != nil {
		t.Fatalf("inconclusive with reason, no evidence: %v", err)
	}
	noReason := completionDoc(KindFaninBuild, "inconclusive", nil)
	_, err := ParseCompletion(mustJSON(t, noReason), KindFaninBuild)
	wantSchemaError(t, err, "")
	passNoEvidence := completionDoc(KindFaninBuild, "pass", nil)
	passNoEvidence["reason"] = "execution_error"
	_, err = ParseCompletion(mustJSON(t, passNoEvidence), KindFaninBuild)
	wantSchemaError(t, err, "/verdict")
	badReason := completionDoc(KindFaninBuild, "inconclusive", nil)
	badReason["reason"] = "because"
	_, err = ParseCompletion(mustJSON(t, badReason), KindFaninBuild)
	wantSchemaError(t, err, "/reason")
}

func TestParseCompletion_RejectsDuplicateMember(t *testing.T) {
	body := `{"schema_version":"specialist-job-v1","execution_nonce":"n","execution_nonce":"m","kind":"draft","verdict":"pass","duration_ms":1,"evidence":{"text":"x"}}`
	if _, err := ParseCompletion([]byte(body), KindDraft); !errors.Is(err, ErrDuplicateMember) {
		t.Fatalf("err = %v, want ErrDuplicateMember", err)
	}
}

func TestDecodeEvidence_UnknownKind(t *testing.T) {
	var pe *ParseError
	if _, err := DecodeEvidence(Kind("nope"), json.RawMessage(`{}`)); !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

func TestValidateResult(t *testing.T) {
	res := Result{
		JobID: runA, Kind: KindDraft,
		Requester: Requester{Seat: SeatOperator, ActorKind: "user", Subject: "alice"},
		Subject:   Subject{RunID: runA}, Verdict: VerdictPass,
		Evidence:  json.RawMessage(`{"text":"# t\n"}`),
		ChargedTo: ChargedTo{Seat: SeatOperator, Subject: "alice"}, DurationMS: 1,
	}
	if err := ValidateResult(mustJSON(t, res)); err != nil {
		t.Fatalf("valid result: %v", err)
	}
	res.Requester.Seat = "captain"
	if err := ValidateResult(mustJSON(t, res)); err == nil {
		t.Fatal("unknown seat accepted")
	}
}

// TestBatteryMutationShapeMatchesPlanCounterfactual pins the battery mutation
// $def to plan-standard-v1 $defs.counterfactual: same property keys, required
// set and expect enum, recursively for the mutation and tests defs.
func TestBatteryMutationShapeMatchesPlanCounterfactual(t *testing.T) {
	load := func(p string) map[string]any {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc["$defs"].(map[string]any)
	}
	ours, plan := load(canonicalSchemaPath), load(planSchemaPath)
	keys := func(def map[string]any, member string) []string {
		var out []string
		switch v := def[member].(type) {
		case map[string]any:
			for k := range v {
				out = append(out, k)
			}
		case []any:
			for _, k := range v {
				out = append(out, k.(string))
			}
		}
		sort.Strings(out)
		return out
	}
	pairs := [][2]string{
		{"battery-mutation", "counterfactual"},
		{"battery-mutation-form", "counterfactual-mutation"},
		{"battery-tests", "counterfactual-tests"},
	}
	for _, p := range pairs {
		o, pl := ours[p[0]].(map[string]any), plan[p[1]].(map[string]any)
		for _, member := range []string{"properties", "required"} {
			if got, want := keys(o, member), keys(pl, member); !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s = %v, plan-standard-v1 %s.%s = %v", p[0], member, got, p[1], member, want)
			}
		}
		if o["additionalProperties"] != false {
			t.Errorf("%s is not closed", p[0])
		}
	}
	enum := func(def map[string]any) any {
		return def["properties"].(map[string]any)["expect"].(map[string]any)["enum"]
	}
	if got, want := enum(ours["battery-mutation"].(map[string]any)), enum(plan["counterfactual"].(map[string]any)); !reflect.DeepEqual(got, want) {
		t.Errorf("expect enum = %v, plan = %v", got, want)
	}
}

// TestSchema_ClosedAtEveryLevel: every object-typed subschema closes with
// additionalProperties:false, except the two kind-selected members (inputs,
// evidence, args) whose closure comes from the if/then-selected $def.
func TestSchema_ClosedAtEveryLevel(t *testing.T) {
	var doc any
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{"inputs": true, "evidence": true, "args": true}
	var walk func(v any, where string)
	walk = func(v any, where string) {
		switch n := v.(type) {
		case map[string]any:
			if n["type"] == "object" && n["additionalProperties"] != false {
				last := where[strings.LastIndex(where, "/")+1:]
				if !selected[last] {
					t.Errorf("object at %s is not closed", where)
				}
			}
			for k, c := range n {
				walk(c, where+"/"+k)
			}
		case []any:
			for i, c := range n {
				walk(c, where+"/"+string(rune('0'+i%10)))
			}
		}
	}
	walk(doc, "")
}

func TestRenderDraft_ADRAndIssue(t *testing.T) {
	adr := DraftInputs{DocumentKind: "adr", Title: " Jobs ", Brief: "Why.", Outline: []OutlineSection{
		{Heading: "context", Body: "ctx"}, {Heading: "Extra"},
	}}
	got, err := RenderDraft(adr)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Jobs\n\nWhy.\n\n## context\n\nctx\n\n## Extra\n" +
		"\n## Options\n\n_TBD_\n\n## Recommendation\n\n_TBD_\n\n## Decision\n\n_TBD_\n\n## Consequences\n\n_TBD_\n"
	if got != want {
		t.Errorf("adr render =\n%q\nwant\n%q", got, want)
	}
	again, _ := RenderDraft(adr)
	if again != got {
		t.Error("render is not deterministic")
	}

	issue, err := RenderDraft(DraftInputs{DocumentKind: "issue", Title: "Fix"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"## Summary\n\n_TBD_", "## Scope\n\n_TBD_", "## Done means\n\n_TBD_"} {
		if !strings.Contains(issue, h) {
			t.Errorf("issue render missing %q:\n%s", h, issue)
		}
	}

	var big []OutlineSection
	for i := 0; i < 5; i++ {
		big = append(big, OutlineSection{Heading: "S", Body: strings.Repeat("x", 8192)})
	}
	if _, err := RenderDraft(DraftInputs{DocumentKind: "adr", Title: "t", Outline: big}); !errors.Is(err, ErrDraftTooLarge) {
		t.Errorf("oversized render err = %v, want ErrDraftTooLarge", err)
	}
	if _, err := RenderDraft(DraftInputs{DocumentKind: "memo", Title: "t"}); err == nil {
		t.Error("unknown document_kind accepted")
	}
}

func TestKind_RunnerExecuted(t *testing.T) {
	for _, k := range AllKinds {
		if k.RunnerExecuted() == (k == KindDraft) {
			t.Errorf("%q RunnerExecuted = %v", k, k.RunnerExecuted())
		}
	}
}
