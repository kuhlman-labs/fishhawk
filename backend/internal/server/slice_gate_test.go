package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// sliceGateSpecBase is a minimal NON-Fishhawk v2 admission spec; each case
// appends its own verdict_transfer block (or none).
const sliceGateSpecBase = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// sliceGateGoldenSpec is the declaration whose served config the shared
// golden's `config` block pins. Makefile is declared twice (G and doc-check)
// so the union's de-duplication is exercised, and shared_artifact_dirs is
// declared so its deliberate omission (S7) is observable.
const sliceGateGoldenSpec = sliceGateSpecBase + `verdict_transfer:
  global_surface: ["build/**", "Makefile"]
  doc_check_inputs: ["notes/**", "Makefile"]
  opaque_paths: ["vendor-blobs/**"]
  inert_paths: ["docs/**"]
  shared_artifact_dirs: ["shared/"]
  completeness_check: "example check"
`

// sliceGateIncompleteSpec declares no G entry: surface_incomplete, the
// golden's `unavailable` block.
const sliceGateIncompleteSpec = sliceGateSpecBase + `verdict_transfer:
  inert_paths: ["docs/**"]
`

// sliceGateGoldenFile is the shared golden's shape.
type sliceGateGoldenFile struct {
	Config      json.RawMessage `json:"config"`
	Unavailable json.RawMessage `json:"unavailable"`
	Cases       []struct {
		Path string `json:"path"`
		Full bool   `json:"full"`
	} `json:"cases"`
}

// sliceGateGolden reads the CROSS-MODULE GOLDEN testdata/wire/
// slice_gate_prompt.json (#5498), the SAME file the runner's upload decode
// test and slice-gate cases test read.
func sliceGateGolden(t *testing.T) sliceGateGoldenFile {
	t.Helper()
	path := filepath.Join(repoRoot(t), "testdata", "wire", "slice_gate_prompt.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared slice_gate golden %s: %v", path, err)
	}
	var g sliceGateGoldenFile
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("decode shared slice_gate golden: %v", err)
	}
	return g
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

func marshalSliceGate(t *testing.T, c *sliceGateConfig) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestResolveSliceGateConfig pins when the config is served and what it
// carries, over a NON-Fishhawk declaration.
func TestResolveSliceGateConfig(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := newPromptServer(t)
	parent := uuid.New()
	childRun := func(specYAML string) *run.Run {
		return &run.Run{ID: uuid.New(), WorkflowID: "feature_change", WorkflowSpec: []byte(specYAML), DecomposedFrom: &parent}
	}
	impl := &run.Stage{ID: uuid.New(), Type: run.StageTypeImplement}
	planStage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan}
	usable := sliceGateSpecBase + `verdict_transfer:
  global_surface: ["Makefile", "build/**"]
  doc_check_inputs: ["notes/**"]
  opaque_paths: ["vendor-blobs/**"]
  inert_paths: ["docs/**"]
`
	standalone := childRun(usable)
	standalone.DecomposedFrom = nil
	longBad := "/" + strings.Repeat("x", 400)

	for _, tc := range []struct {
		name           string
		runRow         *run.Run
		stage          *run.Stage
		fixup          bool
		mergeCandidate *mergeCandidateVerifyTrigger
		want           *sliceGateConfig
	}{
		{"standalone implement", standalone, impl, false, nil, nil},
		{"child fix-up", childRun(usable), impl, true, nil, nil},
		{"child merge-candidate verify", childRun(usable), impl, false, &mergeCandidateVerifyTrigger{}, nil},
		{"child plan stage", childRun(usable), planStage, false, nil, nil},
		{"nil run", nil, impl, false, nil, nil},
		{"nil stage", childRun(usable), nil, false, nil, nil},
		{"child implement", childRun(usable), impl, false, nil, &sliceGateConfig{
			FullGatePaths: []string{basedelta.WorkflowSpecPath, "Makefile", "build/**", "notes/**", "vendor-blobs/**"},
			InertPaths:    []string{"docs/**"},
		}},
		{"no inert declared", childRun(sliceGateSpecBase + "verdict_transfer:\n  global_surface: [\"Makefile\"]\n"), impl, false, nil,
			&sliceGateConfig{FullGatePaths: []string{basedelta.WorkflowSpecPath, "Makefile"}}},
		{"no verdict_transfer", childRun(sliceGateSpecBase), impl, false, nil, &sliceGateConfig{UnavailableReason: "surface_undeclared"}},
		{"no cached spec", &run.Run{ID: uuid.New(), DecomposedFrom: &parent}, impl, false, nil, &sliceGateConfig{UnavailableReason: "surface_undeclared"}},
		{"bad entry", childRun(sliceGateSpecBase + "verdict_transfer:\n  global_surface: [\"Makefile\"]\n  inert_paths: [\"/abs/**\"]\n"), impl, false, nil,
			&sliceGateConfig{UnavailableReason: "invalid_surface:/abs/**"}},
		{"no G", childRun(sliceGateIncompleteSpec), impl, false, nil, &sliceGateConfig{UnavailableReason: "surface_incomplete"}},
		{"unparseable spec", childRun("version: \"2\"\nworkflows: [::"), impl, false, nil, &sliceGateConfig{UnavailableReason: "surface_unreadable"}},
		{"over-long bad entry is bounded", childRun(sliceGateSpecBase + "verdict_transfer:\n  global_surface: [\"" + longBad + "\"]\n"), impl, false, nil,
			&sliceGateConfig{UnavailableReason: ("invalid_surface:" + longBad)[:basedelta.MaxReasonLen]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.resolveSliceGateConfig(ctx, tc.runRow, tc.stage, tc.fixup, tc.mergeCandidate)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveSliceGateConfig = %s, want %s", marshalSliceGate(t, got), marshalSliceGate(t, tc.want))
			}
			if got != nil && len(got.UnavailableReason) > basedelta.MaxReasonLen {
				t.Errorf("unavailable_reason is %d bytes, want <= %d", len(got.UnavailableReason), basedelta.MaxReasonLen)
			}
		})
	}
}

// TestSliceGateConfigMatchesSharedGolden pins the EMIT side of the
// cross-module contract at the resolver: the bytes served for the golden
// declaration equal the golden's `config`, the incomplete declaration's
// equal `unavailable`, and every `cases` row matches (or not) the served
// full_gate_paths under the same doublestar module the runner's matcher
// uses, so encoding and matcher semantics are pinned from both modules.
func TestSliceGateConfigMatchesSharedGolden(t *testing.T) {
	golden := sliceGateGolden(t)
	s, _, _, _ := newPromptServer(t)
	parent := uuid.New()
	impl := &run.Stage{ID: uuid.New(), Type: run.StageTypeImplement}
	serve := func(specYAML string) *sliceGateConfig {
		return s.resolveSliceGateConfig(context.Background(),
			&run.Run{ID: uuid.New(), WorkflowSpec: []byte(specYAML), DecomposedFrom: &parent}, impl, false, nil)
	}
	cfg := serve(sliceGateGoldenSpec)
	if got, want := marshalSliceGate(t, cfg), compactJSON(t, golden.Config); got != want {
		t.Errorf("served config = %s, golden config = %s", got, want)
	}
	if got, want := marshalSliceGate(t, serve(sliceGateIncompleteSpec)), compactJSON(t, golden.Unavailable); got != want {
		t.Errorf("served unavailable = %s, golden unavailable = %s", got, want)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("golden carries no cases")
	}
	for _, c := range golden.Cases {
		full := false
		for _, pat := range cfg.FullGatePaths {
			if ok, err := doublestar.Match(pat, c.Path); err == nil && ok {
				full = true
				break
			}
		}
		if full != c.Full {
			t.Errorf("case %q: matches full_gate_paths = %v, golden says %v", c.Path, full, c.Full)
		}
	}
}

// sliceGateFromBody returns the raw slice_gate member of a prompt response,
// and whether the key is present at all.
func sliceGateFromBody(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	raw, ok := members["slice_gate"]
	if !ok {
		return "", false
	}
	return compactJSON(t, raw), true
}

// TestPromptServesSliceGate_GoldenBytes drives BOTH real handlers, the
// signed /prompt fetch and the unsigned /prompt-render, for a seeded
// decomposition child's implement stage: each serves slice_gate bytes equal
// to the golden `config`, and a standalone run's responses carry no
// slice_gate key. Each endpoint populates the field independently.
func TestPromptServesSliceGate_GoldenBytes(t *testing.T) {
	golden := sliceGateGolden(t)
	want := compactJSON(t, golden.Config)
	for _, child := range []bool{true, false} {
		s, rr, _, rows, fetch := gateContainerFixture(t, sliceGateGoldenSpec, "feature_change", run.StageTypeImplement)
		parent := uuid.New()
		if child {
			rr.runRow.DecomposedFrom = &parent
		}
		bodies := map[string][]byte{"/prompt": fetch(rows[0].ID).Body.Bytes()}
		req := httptest.NewRequest(http.MethodGet, "/v0/stages/"+rows[0].ID.String()+"/prompt-render", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("render status = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		bodies["/prompt-render"] = w.Body.Bytes()
		for endpoint, body := range bodies {
			got, present := sliceGateFromBody(t, body)
			switch {
			case child && (!present || got != want):
				t.Errorf("child %s: slice_gate = %s (present %v), want golden %s", endpoint, got, present, want)
			case !child && present:
				t.Errorf("standalone %s: slice_gate key present (%s), want absent", endpoint, got)
			}
		}
	}
}
