package mcpserver

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- fishhawk_doctor (E29.6 / #1506) ---

// doctorFakeBackend is a self-contained backend stub for the doctor tool: it
// serves only GET /v0/onboarding/readiness. lastRepo / lastForge capture the
// last repo and forge query values so tests assert the env fallbacks and the
// forge passthrough (E45.43 / #3348); status drives the HTTP status
// (default 200); errBody, when set, is written verbatim for the error-path
// tests; rawBody, when set, is written verbatim as a 200 body so a test can
// serve a payload the Go struct cannot express (a response that OMITS
// merge_gate, as a pre-#3161 fishhawkd does); resp overrides the default
// echoed report.
type doctorFakeBackend struct {
	mu        sync.Mutex
	lastRepo  string
	lastForge string
	calls     int
	status    int
	errBody   string
	rawBody   string
	resp      *OnboardingReadinessReport
}

func newDoctorFakeBackend(t *testing.T) (*doctorFakeBackend, *httptest.Server) {
	fb := &doctorFakeBackend{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/onboarding/readiness", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		fb.calls++
		fb.lastRepo = r.URL.Query().Get("repo")
		fb.lastForge = r.URL.Query().Get("forge")
		status := fb.status
		errBody := fb.errBody
		rawBody := fb.rawBody
		resp := fb.resp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if rawBody != "" {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		if resp == nil {
			resp = &OnboardingReadinessReport{
				Repo: r.URL.Query().Get("repo"),
				App:  OnboardingApp{Installed: true, InstallationID: 4242},
				Spec: OnboardingSpec{Source: "fetched", Valid: true},
				Reviewers: []OnboardingReviewer{
					{Provider: "claudecode", Model: "claude-opus-4-8", Available: true, ModelStatus: "verified", Priced: boolPtr(true)},
				},
				Scopes: OnboardingScopes{
					Adequate: true,
					Required: []string{"read:runs", "write:runs"},
					Missing:  []string{},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestDoctor_HappyPath_MapsReport(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.calls != 1 {
		t.Errorf("backend called %d times, want 1", fb.calls)
	}
	if fb.lastRepo != "kuhlman-labs/fishhawk" {
		t.Errorf("query repo = %q, want kuhlman-labs/fishhawk", fb.lastRepo)
	}
	if !out.Report.App.Installed || out.Report.App.InstallationID != 4242 {
		t.Errorf("App = %+v, want installed with id 4242", out.Report.App)
	}
	if out.Report.Spec.Source != "fetched" || !out.Report.Spec.Valid {
		t.Errorf("Spec = %+v, want fetched+valid", out.Report.Spec)
	}
	if len(out.Report.Reviewers) != 1 || out.Report.Reviewers[0].Provider != "claudecode" {
		t.Errorf("Reviewers = %+v, want one claudecode reviewer", out.Report.Reviewers)
	}
	// The #3578 model-validity fields round-trip: model_status and a non-nil
	// priced pointer.
	if rv := out.Report.Reviewers[0]; rv.ModelStatus != "verified" || rv.Priced == nil || !*rv.Priced {
		t.Errorf("reviewer model fields = %+v, want model_status=verified priced=&true", rv)
	}
	if !out.Report.Scopes.Adequate {
		t.Errorf("Scopes.Adequate = false, want true")
	}
}

// TestDoctor_ReviewerModelFields_RoundTrip pins the #3578 mirror: a payload
// carrying priced:false maps to a non-nil false pointer, and a payload OMITTING
// priced maps to nil — the three-state distinction the CLI rendering keys on.
func TestDoctor_ReviewerModelFields_RoundTrip(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = `{
	  "repo": "kuhlman-labs/fishhawk",
	  "app": {"installed": true, "installation_id": 4242},
	  "spec": {"source": "fetched", "valid": true},
	  "reviewers": [
	    {"provider": "codex", "model": "gpt-mystery", "available": true, "model_status": "verified", "model_hint": "unpriced: usage under this model is recorded at $0 (estimated)", "priced": false},
	    {"provider": "anthropic", "available": true}
	  ],
	  "scopes": {"adequate": true, "required": [], "missing": []}
	}`
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if len(out.Report.Reviewers) != 2 {
		t.Fatalf("Reviewers = %d, want 2", len(out.Report.Reviewers))
	}
	unpriced := out.Report.Reviewers[0]
	if unpriced.Priced == nil || *unpriced.Priced {
		t.Errorf("codex Priced = %v, want a non-nil false pointer", unpriced.Priced)
	}
	if unpriced.ModelStatus != "verified" || !strings.Contains(unpriced.ModelHint, "$0") {
		t.Errorf("codex model fields = %+v, want verified + $0 hint", unpriced)
	}
	if omitted := out.Report.Reviewers[1]; omitted.Priced != nil {
		t.Errorf("anthropic Priced = %v, want nil (priced omitted from the payload)", omitted.Priced)
	}
}

func TestDoctor_RepoFromEnv(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "kuhlman-labs/fishhawk"})

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.lastRepo != "kuhlman-labs/fishhawk" {
		t.Errorf("query repo = %q, want env fallback", fb.lastRepo)
	}
	if fb.lastForge != "" {
		t.Errorf("query forge = %q, want empty (GITHUB_REPOSITORY implies no forge default)", fb.lastForge)
	}
}

// TestDoctor_ForgePassthrough: an explicit forge input and a nested GitLab
// path reach the backend query untouched (E45.43 / #3348).
func TestDoctor_ForgePassthrough(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, nil)

	const nested = "gitlab-com/customer-success/solutions-architecture/coe/gitlab-migrator"
	_, _, err := r.doctor(context.Background(), nil, DoctorInput{Repo: nested, Forge: "gitlab"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.lastRepo != nested {
		t.Errorf("query repo = %q, want the nested path untouched", fb.lastRepo)
	}
	if fb.lastForge != "gitlab" {
		t.Errorf("query forge = %q, want gitlab", fb.lastForge)
	}
}

// TestDoctor_RepoFromCIProjectPath: with only GitLab CI's CI_PROJECT_PATH set,
// the repo comes from it AND forge defaults to gitlab.
func TestDoctor_RepoFromCIProjectPath(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, map[string]string{"CI_PROJECT_PATH": "acme/platform/widgets"})

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.lastRepo != "acme/platform/widgets" {
		t.Errorf("query repo = %q, want CI_PROJECT_PATH fallback", fb.lastRepo)
	}
	if fb.lastForge != "gitlab" {
		t.Errorf("query forge = %q, want gitlab (CI_PROJECT_PATH supplied the repo)", fb.lastForge)
	}
}

// TestDoctor_GitHubRepositoryOutranksCIProjectPath: both env vars set →
// GITHUB_REPOSITORY wins and no forge default is applied.
func TestDoctor_GitHubRepositoryOutranksCIProjectPath(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"GITHUB_REPOSITORY": "kuhlman-labs/fishhawk",
		"CI_PROJECT_PATH":   "acme/platform/widgets",
	})

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.lastRepo != "kuhlman-labs/fishhawk" {
		t.Errorf("query repo = %q, want GITHUB_REPOSITORY to outrank CI_PROJECT_PATH", fb.lastRepo)
	}
	if fb.lastForge != "" {
		t.Errorf("query forge = %q, want empty", fb.lastForge)
	}
}

// TestDoctor_ForgeInputOverridesEnvDefault: CI_PROJECT_PATH supplies the repo
// but an explicit forge:github input outranks the gitlab env default.
func TestDoctor_ForgeInputOverridesEnvDefault(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, map[string]string{"CI_PROJECT_PATH": "acme/widgets"})

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{Forge: "github"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if fb.lastRepo != "acme/widgets" {
		t.Errorf("query repo = %q, want CI_PROJECT_PATH fallback", fb.lastRepo)
	}
	if fb.lastForge != "github" {
		t.Errorf("query forge = %q, want github (explicit input outranks the env default)", fb.lastForge)
	}
}

func TestDoctor_MissingRepoNoEnv_FailsLocally(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{})
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err = %v, want repo-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0 (fast local fail)", fb.calls)
	}
}

func TestDoctor_AuthRequired_MapsToolError(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.status = http.StatusUnauthorized
	fb.errBody = `{"error":{"code":"authentication_required","message":"an authenticated token or session is required"}}`
	r := newResolver(srv, nil)

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "o/n"})
	if err == nil {
		t.Fatal("want a tool error on a 401")
	}
	if !strings.Contains(err.Error(), "authentication_required") || !strings.Contains(err.Error(), "FISHHAWK_API_TOKEN") {
		t.Errorf("err = %v, want authentication_required + token hint", err)
	}
}

func TestDoctor_ValidationFailed_MapsToolError(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.status = http.StatusBadRequest
	fb.errBody = `{"error":{"code":"validation_failed","message":"repo must be in owner/name format"}}`
	r := newResolver(srv, nil)

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "not-a-repo"})
	if err == nil {
		t.Fatal("want a tool error on a 400")
	}
	if !strings.Contains(err.Error(), "validation_failed") || !strings.Contains(err.Error(), "owner/name") {
		t.Errorf("err = %v, want validation_failed + owner/name hint", err)
	}
}

func TestDoctor_UnmappedError_Propagates(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.status = http.StatusInternalServerError
	fb.errBody = `{"error":{"code":"internal","message":"boom"}}`
	r := newResolver(srv, nil)

	_, _, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "o/n"})
	if err == nil {
		t.Fatal("want a tool error on a 500")
	}
	// The default branch surfaces the wrapped apiError without an added hint.
	if !strings.Contains(err.Error(), "onboarding readiness") || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v, want the generic wrapped error", err)
	}
}

// --- fishhawk_init (E29.6 / #1506) ---

// TestInit_EachPreset_ReturnsValidSpec is the behavioral done-means test: the
// SHIPPED scaffold for every tier must parse AND validate as a workflow spec,
// not merely be non-empty. It crosses the tool handler -> spec.PresetBytes
// in-process path with no HTTP hop (init generates locally).
func TestInit_EachPreset_ReturnsValidSpec(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	for _, preset := range []string{"low", "medium", "high"} {
		_, out, err := r.init(context.Background(), nil, InitInput{Preset: preset})
		if err != nil {
			t.Fatalf("init(%q): %v", preset, err)
		}
		if out.Preset != preset {
			t.Errorf("init(%q) Preset = %q", preset, out.Preset)
		}
		if strings.TrimSpace(out.WorkflowYAML) == "" {
			t.Fatalf("init(%q) returned empty WorkflowYAML", preset)
		}
		if out.TargetPath != ".fishhawk/workflows.yaml" {
			t.Errorf("init(%q) TargetPath = %q, want .fishhawk/workflows.yaml", preset, out.TargetPath)
		}
		sp, perr := spec.ParseBytes([]byte(out.WorkflowYAML))
		if perr != nil {
			t.Fatalf("init(%q) scaffold does not parse: %v", preset, perr)
		}
		if verr := spec.Validate(sp); verr != nil {
			t.Fatalf("init(%q) scaffold does not validate: %v", preset, verr)
		}
	}
}

// TestInit_NextStepNamesValidate (E45.65 / #3579): fishhawk_init's output
// must point the agent at fishhawk_validate as the pre-commit check and say
// why fishhawk_doctor cannot stand in (it reads the default branch). RED when
// the NextStep population is deleted.
func TestInit_NextStepNamesValidate(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	for _, preset := range []string{"", "low", "medium", "high"} {
		_, out, err := r.init(context.Background(), nil, InitInput{Preset: preset})
		if err != nil {
			t.Fatalf("init(%q): %v", preset, err)
		}
		for _, want := range []string{"fishhawk_validate", "BEFORE committing", "fishhawk_doctor", "DEFAULT BRANCH"} {
			if !strings.Contains(out.NextStep, want) {
				t.Errorf("init(%q) NextStep missing %q: %q", preset, want, out.NextStep)
			}
		}
	}
}

func TestInit_DefaultPresetIsMedium(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	_, out, err := r.init(context.Background(), nil, InitInput{})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if out.Preset != "medium" {
		t.Errorf("default Preset = %q, want medium", out.Preset)
	}
	// The default must also be a valid spec, not just labeled medium.
	sp, perr := spec.ParseBytes([]byte(out.WorkflowYAML))
	if perr != nil {
		t.Fatalf("default scaffold does not parse: %v", perr)
	}
	if verr := spec.Validate(sp); verr != nil {
		t.Fatalf("default scaffold does not validate: %v", verr)
	}
}

func TestInit_UnknownPreset_FailsCleanly(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	_, _, err := r.init(context.Background(), nil, InitInput{Preset: "extreme"})
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("err = %v, want unknown-preset error", err)
	}
	if !strings.Contains(err.Error(), "low, medium, high") {
		t.Errorf("err = %v, want the valid-tiers hint", err)
	}
}

// --- merge gate mirror, check (5) (#3161) ---

// mergeGateServerBody is the LITERAL merge_gate JSON the backend serves — the
// same shape backend/internal/server/onboarding_test.go's end-to-end test
// asserts on the wire. Decoding this body through OnboardingReadinessReport is
// the mirror-drift control: a json-tag typo on the MCP side zero-values the
// field and the assertions below go red.
//
// It lives here rather than in client_test.go because that file carries no
// readiness references at all, while this one already constructs
// OnboardingReadinessReport (operator condition 4b).
const mergeGateServerBody = `{
  "repo": "kuhlman-labs/fishhawk",
  "app": {"installed": true, "installation_id": 12345},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []},
  "merge_gate": {
    "status": "required",
    "check": "fishhawk_audit_complete",
    "branch": "main",
    "sources": [
      {"identity": "branch_protection", "classic": true, "bypass_entries": 0,
       "enforce_admins": true, "bypassable": false},
      {"identity": "ruleset:42", "bypass_entries": 2, "bypassable": true}
    ],
    "bypassable": false,
    "authoritative": true,
    "required_contexts": ["fishhawk_audit_complete", "ci"]
  }
}`

// TestOnboardingReadinessReport_MergeGateMirrorsBackendTags decodes the
// literal backend body and asserts every merge_gate field lands. A tag typo
// silently zero-values one — which is exactly the drift this asserts against.
func TestOnboardingReadinessReport_MergeGateMirrorsBackendTags(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(mergeGateServerBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.MergeGate == nil {
		t.Fatalf("MergeGate = nil, want the decoded object")
	}
	mg := got.MergeGate
	if mg.Status != "required" {
		t.Errorf("Status = %q, want required", mg.Status)
	}
	if mg.Check != "fishhawk_audit_complete" {
		t.Errorf("Check = %q, want fishhawk_audit_complete", mg.Check)
	}
	if mg.Branch != "main" {
		t.Errorf("Branch = %q, want main", mg.Branch)
	}
	if !mg.Authoritative {
		t.Errorf("Authoritative = false, want true")
	}
	if mg.Bypassable {
		t.Errorf("Bypassable = true, want false (one requiring source has no bypass path)")
	}
	if len(mg.RequiredContexts) != 2 {
		t.Errorf("RequiredContexts = %v, want 2 entries", mg.RequiredContexts)
	}
	if len(mg.Sources) != 2 {
		t.Fatalf("len(Sources) = %d, want 2: %+v", len(mg.Sources), mg.Sources)
	}
	classic, ruleset := mg.Sources[0], mg.Sources[1]
	if classic.Identity != "branch_protection" || !classic.Classic || !classic.EnforceAdmins {
		t.Errorf("Sources[0] = %+v, want the classic source with enforce_admins on", classic)
	}
	if classic.Bypassable {
		t.Errorf("classic Bypassable = true, want false")
	}
	if ruleset.Identity != "ruleset:42" || ruleset.BypassEntries != 2 || !ruleset.Bypassable {
		t.Errorf("Sources[1] = %+v, want ruleset:42 with 2 bypass entries, bypassable", ruleset)
	}
}

// TestDoctor_MergeGate_ReachesToolOutput proves the field survives the whole
// tool path — backend response → apiClient decode → DoctorOutput — rather than
// only the struct decode above.
func TestDoctor_MergeGate_ReachesToolOutput(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.resp = &OnboardingReadinessReport{
		Repo: "kuhlman-labs/fishhawk",
		App:  OnboardingApp{Installed: true, InstallationID: 4242},
		Spec: OnboardingSpec{Source: "fetched", Valid: true},
		Scopes: OnboardingScopes{
			Adequate: true, Required: []string{"read:runs"}, Missing: []string{},
		},
		MergeGate: &OnboardingMergeGate{
			Status: "unknown",
			Check:  "fishhawk_audit_complete",
			Reason: "administration_read_missing",
			Detail: "reading branch protection was refused (403)",
		},
	}
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Report.MergeGate == nil {
		t.Fatalf("MergeGate = nil, want the served object")
	}
	if out.Report.MergeGate.Status != "unknown" {
		t.Errorf("MergeGate.Status = %q, want unknown", out.Report.MergeGate.Status)
	}
	if out.Report.MergeGate.Reason != "administration_read_missing" {
		t.Errorf("MergeGate.Reason = %q, want administration_read_missing", out.Report.MergeGate.Reason)
	}
	if out.Report.MergeGate.Check != "fishhawk_audit_complete" {
		t.Errorf("MergeGate.Check = %q, want fishhawk_audit_complete", out.Report.MergeGate.Check)
	}
}

// preMergeGateReadinessBody is the LITERAL 200 body a PRE-#3161 fishhawkd
// serves: every field this build knows EXCEPT merge_gate, which that backend
// has no code to emit. The explicitly supported older-backend compatibility
// path — the one the CLI mirror pins with
// TestDoctorOnboarding_MergeGateAbsent_EmitsNoRung and its explicit-null
// sibling, and the one the MCP mirror had no coverage for at all, which is
// exactly why the value-vs-pointer asymmetry survived review.
//
// It is a raw string rather than an OnboardingReadinessReport because the
// omission is the whole point: the Go struct cannot express "no key".
const preMergeGateReadinessBody = `{
  "repo": "kuhlman-labs/fishhawk",
  "app": {"installed": true, "installation_id": 4242},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []}
}`

// nullMergeGateReadinessBody is the sibling shape: the key is PRESENT but
// JSON null. Absent, null and empty are three distinct states; the first two
// must both resolve to the same "this backend made no claim" outcome, and
// neither may become the third by way of an empty-status verdict.
const nullMergeGateReadinessBody = `{
  "repo": "kuhlman-labs/fishhawk",
  "app": {"installed": true, "installation_id": 4242},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []},
  "merge_gate": null
}`

// TestDoctor_MergeGateAbsent_PreservesAbsence walks the whole MCP tool path
// against a backend response that carries NO merge_gate verdict, and pins the
// invariant the routed regression names: the fishhawk_doctor report NEVER
// carries a merge_gate whose status is the empty string.
//
// The control under test is the POINTER on
// OnboardingReadinessReport.MergeGate. With a value field the decode produces
// a zero-valued struct and DoctorOutput re-emits
// `"merge_gate":{"status":"","check":"",...}` — a verdict outside the
// documented required|not_required|unknown enum that no forge read ever
// established. So the assertion is made on the RE-EMITTED WIRE BYTES, not only
// on the in-memory field: a value field cannot satisfy it, while the in-memory
// check alone would not even compile under the mutation.
func TestDoctor_MergeGateAbsent_PreservesAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"key_omitted", preMergeGateReadinessBody},
		{"key_explicit_null", nullMergeGateReadinessBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newDoctorFakeBackend(t)
			fb.rawBody = tc.body
			r := newResolver(srv, nil)

			_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			// Prove the body decoded — otherwise an absent merge_gate would be
			// indistinguishable from a wholesale decode failure and the
			// assertion below would pass for the wrong reason.
			if out.Report.Repo != "kuhlman-labs/fishhawk" || !out.Report.App.Installed {
				t.Fatalf("report did not decode: %+v", out.Report)
			}
			if out.Report.MergeGate != nil {
				t.Errorf("MergeGate = %+v, want nil: the backend served no verdict, and nil is not the same claim as status unknown", *out.Report.MergeGate)
			}

			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal DoctorOutput: %v", err)
			}
			if strings.Contains(string(encoded), "merge_gate") {
				t.Errorf("DoctorOutput re-emits a merge_gate the backend never served:\n%s", encoded)
			}
			if strings.Contains(string(encoded), `"status":""`) {
				t.Errorf("DoctorOutput carries an empty-string status - a verdict outside required|not_required|unknown:\n%s", encoded)
			}
		})
	}
}

// registeredToolDescription returns a tool's WIRE-VISIBLE description, read
// back over an in-memory MCP session through the same registration path
// tools_test.go walks. Reading the shipped string (rather than the source
// literal) is what makes the assertion a done-means test: a comment-only touch
// of onboard.go cannot satisfy it (#1169).
func registeredToolDescription(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("%s: not registered/visible over ListTools", name)
	return ""
}

// TestDoctorToolDescription_DescribesMergeGate pins the SHIPPED tool
// description: it must name the fifth check and state the fail-closed reading
// of `unknown`. A prose-only surface no compiler enforces, so a comment-only
// touch of onboard.go must not satisfy it (#1169).
func TestDoctorToolDescription_DescribesMergeGate(t *testing.T) {
	// Collapse the description's hard wrapping: the shipped string breaks
	// lines at ~80 columns, so a claim is routinely split across two lines.
	// Asserting on short load-bearing fragments of the collapsed text keeps
	// the test from being deleted by the next copy-edit's rewrap.
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"five server-side-only checks",
		"merge_gate",
		"required | not_required | unknown",
		"NOT evidence the check is unrequired",
		// The corrected backward-compat claim: the key is omitted, not
		// zero-valued, against a pre-#3161 fishhawkd.
		"OMITTED ENTIRELY against an older fishhawkd",
		"never emitted with an empty status",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description missing %q:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "returns four server-side-only checks") {
		t.Errorf("fishhawk_doctor description still says four checks")
	}
}

// TestOnboardingToolDescriptions_NameTheSkillResource pins the #1516
// discoverability sentence on both onboarding tool descriptions: a
// connecting agent reading either tool's description learns the
// fishhawk://onboarding-skill resource exists without operator memory.
func TestOnboardingToolDescriptions_NameTheSkillResource(t *testing.T) {
	doctorDesc := registeredToolDescription(t, "fishhawk_doctor")
	if !strings.Contains(doctorDesc, "fishhawk://onboarding-skill") {
		t.Errorf("fishhawk_doctor description missing fishhawk://onboarding-skill:\n%s", doctorDesc)
	}
	initDesc := registeredToolDescription(t, "fishhawk_init")
	if !strings.Contains(initDesc, "fishhawk://onboarding-skill") {
		t.Errorf("fishhawk_init description missing fishhawk://onboarding-skill:\n%s", initDesc)
	}
	// #3579: init's description must hand off to the pre-commit check, and
	// the check's own description must point back at the skill.
	if !strings.Contains(initDesc, "fishhawk_validate") {
		t.Errorf("fishhawk_init description missing the fishhawk_validate hand-off:\n%s", initDesc)
	}
	validateDesc := registeredToolDescription(t, "fishhawk_validate")
	if !strings.Contains(validateDesc, "fishhawk://onboarding-skill") {
		t.Errorf("fishhawk_validate description missing fishhawk://onboarding-skill:\n%s", validateDesc)
	}
}

// gitlabReadinessBody is a literal gitlab-family backend body (E45.43 /
// #3348): it carries forge, app.note, NO installation_id and NO merge_gate —
// and, since E45.66 / #3580, the GitLab-shaped gitlab_merge_gate sibling, and
// since E45.68 / #3582 app.resolvable plus the gitlab_registration rung.
const gitlabReadinessBody = `{
  "repo": "gitlab-com/customer-success/solutions-architecture/coe/gitlab-migrator",
  "forge": "gitlab",
  "app": {"installed": true, "resolvable": true, "note": "gitlab: installed means a gitlab installation is registered for exactly this project path (fishhawkd installation register — the authorization POST /v0/runs checks) AND the project resolves with the deployment credential (resolvable); no App installation applies on GitLab — see gitlab_registration"},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [{"provider": "anthropic", "model": "claude-opus-4-8", "available": true}],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []},
  "gitlab_merge_gate": {"status": "unknown", "reason": "forbidden", "authoritative": false, "note": "gitlab: ..."},
  "gitlab_registration": {"status": "registered", "project_path": "gitlab-com/customer-success/solutions-architecture/coe/gitlab-migrator", "installation_ref": "gitlab:5", "resolved_ref": "gitlab:5", "ref_matches": true, "note": "gitlab: this rung reports ..."}
}`

// TestOnboardingReadinessReport_GitLabBodyMirrorsBackendTags decodes the
// literal gitlab-family body and asserts the two new fields land (a tag typo
// on either side zero-values one) and MergeGate stays nil.
func TestOnboardingReadinessReport_GitLabBodyMirrorsBackendTags(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(gitlabReadinessBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.Forge != "gitlab" {
		t.Errorf("Forge = %q, want gitlab", got.Forge)
	}
	if !got.App.Installed || got.App.InstallationID != 0 {
		t.Errorf("App = %+v, want installed with no installation id", got.App)
	}
	if !strings.HasPrefix(got.App.Note, "gitlab: installed means a gitlab installation is registered") {
		t.Errorf("App.Note = %q, want the gitlab note", got.App.Note)
	}
	if got.App.Resolvable == nil || !*got.App.Resolvable {
		t.Errorf("App.Resolvable = %v, want &true", got.App.Resolvable)
	}
	if got.MergeGate != nil {
		t.Errorf("MergeGate = %+v, want nil on a gitlab-family body", *got.MergeGate)
	}
}

// TestDoctor_GitLabReport_ReemitsForgeAndNoteAndNoMergeGate walks the whole
// tool path against the gitlab-family body and asserts the RE-EMITTED WIRE
// BYTES carry "forge":"gitlab" and "note", and NO merge_gate key — the
// deliberate omission on GitLab must survive the mirror exactly as the
// pre-#3161 absence does (TestDoctor_MergeGateAbsent_PreservesAbsence).
func TestDoctor_GitLabReport_ReemitsForgeAndNoteAndNoMergeGate(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = gitlabReadinessBody
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{
		Repo: "gitlab-com/customer-success/solutions-architecture/coe/gitlab-migrator", Forge: "gitlab"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !out.Report.App.Installed {
		t.Fatalf("report did not decode: %+v", out.Report)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"forge":"gitlab"`) {
		t.Errorf("DoctorOutput lacks \"forge\":\"gitlab\":\n%s", body)
	}
	if !strings.Contains(body, `"note":"gitlab: installed means a gitlab installation is registered`) {
		t.Errorf("DoctorOutput lacks the app note:\n%s", body)
	}
	// The QUOTED key: `"merge_gate":` cannot match the sibling
	// `"gitlab_merge_gate":` key the body now carries (E45.66 / #3580), so
	// the omission assertion stays exact rather than tripping on the sibling.
	if strings.Contains(body, `"merge_gate":`) {
		t.Errorf("DoctorOutput re-emits a merge_gate on a gitlab-family report:\n%s", body)
	}
	if strings.Contains(body, "installation_id") {
		t.Errorf("DoctorOutput re-emits an installation_id on a gitlab-family report:\n%s", body)
	}
}

// TestDoctorToolDescription_DescribesForgeFamily pins the SHIPPED description's
// GitLab claims (E45.43 / #3348): the forge input, the CI_PROJECT_PATH
// fallback, and the deliberate merge_gate omission on GitLab.
func TestDoctorToolDescription_DescribesForgeFamily(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"ALSO OMITTED on a GitLab-family report",
		"CI_PROJECT_PATH",
		"pass forge=gitlab",
		"nested groups",
		"gitlab_merge_gate",
		"pipeline_gated | not_pipeline_gated | unknown",
		"no per-context required status check",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description missing %q:\n%s", want, desc)
		}
	}
}

// --- gitlab_merge_gate mirror (E45.66 / #3580) ---

// gitlabMergeGateServerBody is the LITERAL gitlab_merge_gate JSON the backend
// serves on an authoritative read — the shape
// backend/internal/server/onboarding_test.go's
// TestOnboardingReadiness_GitLabMergeGate_EndToEnd asserts on the wire.
// Decoding it through OnboardingReadinessReport is the mirror-drift control:
// a json-tag typo on the MCP side zero-values the field and the assertions
// below go red.
const gitlabMergeGateServerBody = `{
  "repo": "acme/widgets",
  "forge": "gitlab",
  "app": {"installed": true, "resolvable": true, "note": "gitlab: installed means a gitlab installation is registered for exactly this project path (fishhawkd installation register — the authorization POST /v0/runs checks) AND the project resolves with the deployment credential (resolvable); no App installation applies on GitLab — see gitlab_registration"},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []},
  "gitlab_merge_gate": {
    "status": "pipeline_gated",
    "branch": "main",
    "protected": true,
    "matched_rules": ["main", "m*"],
    "allow_force_push": true,
    "push_access_levels": [{"level": 0, "description": "No one"}],
    "merge_access_levels": [{"level": 30, "description": "Developers + Maintainers"}, {"level": 40, "description": "Maintainers"}],
    "pipeline_must_succeed": true,
    "allow_skipped_pipeline": false,
    "discussions_must_be_resolved": true,
    "authoritative": true,
    "note": "gitlab: this rung reports whether the project's real default branch is protected ..."
  }
}`

// TestOnboardingReadinessReport_GitLabMergeGateMirrorsBackendTags decodes the
// literal backend body and asserts every gitlab_merge_gate field lands, the
// sibling of TestOnboardingReadinessReport_MergeGateMirrorsBackendTags.
func TestOnboardingReadinessReport_GitLabMergeGateMirrorsBackendTags(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(gitlabMergeGateServerBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.MergeGate != nil {
		t.Errorf("MergeGate = %+v, want nil on a gitlab-family body", *got.MergeGate)
	}
	if got.GitLabMergeGate == nil {
		t.Fatalf("GitLabMergeGate = nil, want the decoded object")
	}
	g := got.GitLabMergeGate
	if g.Status != "pipeline_gated" {
		t.Errorf("Status = %q, want pipeline_gated", g.Status)
	}
	if g.Branch != "main" {
		t.Errorf("Branch = %q, want main", g.Branch)
	}
	if g.Protected == nil || !*g.Protected {
		t.Errorf("Protected = %v, want &true", g.Protected)
	}
	if len(g.MatchedRules) != 2 || g.MatchedRules[0] != "main" || g.MatchedRules[1] != "m*" {
		t.Errorf("MatchedRules = %v, want [main m*]", g.MatchedRules)
	}
	if g.AllowForcePush == nil || !*g.AllowForcePush {
		t.Errorf("AllowForcePush = %v, want &true", g.AllowForcePush)
	}
	if len(g.PushAccessLevels) != 1 || g.PushAccessLevels[0].Level != 0 || g.PushAccessLevels[0].Description != "No one" {
		t.Errorf("PushAccessLevels = %+v, want [{0 No one}]", g.PushAccessLevels)
	}
	if len(g.MergeAccessLevels) != 2 || g.MergeAccessLevels[0].Level != 30 || g.MergeAccessLevels[1].Level != 40 {
		t.Errorf("MergeAccessLevels = %+v, want ascending [30 40]", g.MergeAccessLevels)
	}
	if g.PipelineMustSucceed == nil || !*g.PipelineMustSucceed {
		t.Errorf("PipelineMustSucceed = %v, want &true", g.PipelineMustSucceed)
	}
	if g.AllowSkippedPipeline == nil || *g.AllowSkippedPipeline {
		t.Errorf("AllowSkippedPipeline = %v, want &false (read, not absent)", g.AllowSkippedPipeline)
	}
	if g.DiscussionsMustBeResolved == nil || !*g.DiscussionsMustBeResolved {
		t.Errorf("DiscussionsMustBeResolved = %v, want &true", g.DiscussionsMustBeResolved)
	}
	if !g.Authoritative {
		t.Errorf("Authoritative = false, want true")
	}
	if !strings.HasPrefix(g.Note, "gitlab: this rung reports") {
		t.Errorf("Note = %q, want the backend note", g.Note)
	}
	if g.Reason != "" || g.Detail != "" || g.Remediation != "" {
		t.Errorf("reason/detail/remediation = %q/%q/%q, want empty on an authoritative verdict", g.Reason, g.Detail, g.Remediation)
	}
}

// TestOnboardingReadinessReport_GitLabMergeGateUnknownKeepsPointersNil pins
// the absence half of the mirror: an `unknown` body that omits every signal
// decodes with every pointer bool nil — the mirror never invents a false.
func TestOnboardingReadinessReport_GitLabMergeGateUnknownKeepsPointersNil(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(gitlabReadinessBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	g := got.GitLabMergeGate
	if g == nil {
		t.Fatalf("GitLabMergeGate = nil, want the unknown object")
	}
	if g.Status != "unknown" || g.Reason != "forbidden" || g.Authoritative {
		t.Errorf("got status=%q reason=%q authoritative=%v, want unknown/forbidden/false", g.Status, g.Reason, g.Authoritative)
	}
	if g.Protected != nil || g.AllowForcePush != nil || g.PipelineMustSucceed != nil ||
		g.AllowSkippedPipeline != nil || g.DiscussionsMustBeResolved != nil {
		t.Errorf("an unread signal decoded non-nil: %+v", *g)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"protected"`, `"allow_force_push"`, `"pipeline_must_succeed"`, `"allow_skipped_pipeline"`, `"discussions_must_be_resolved"`, `"branch"`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("re-emitted unknown carries %s (an unread signal rendered as a value):\n%s", key, encoded)
		}
	}
}

// TestDoctor_GitLabReport_ReemitsGitLabMergeGate walks the whole tool path
// against the authoritative gitlab body and asserts the RE-EMITTED WIRE BYTES
// carry `"gitlab_merge_gate":{` with its status and NO `"merge_gate":` key.
func TestDoctor_GitLabReport_ReemitsGitLabMergeGate(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = gitlabMergeGateServerBody
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "acme/widgets", Forge: "gitlab"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !out.Report.App.Installed {
		t.Fatalf("report did not decode: %+v", out.Report)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"gitlab_merge_gate":{`) {
		t.Errorf("DoctorOutput lacks the gitlab_merge_gate object:\n%s", body)
	}
	if !strings.Contains(body, `"status":"pipeline_gated"`) {
		t.Errorf("DoctorOutput lacks the pipeline_gated status:\n%s", body)
	}
	if !strings.Contains(body, `"matched_rules":["main","m*"]`) {
		t.Errorf("DoctorOutput lacks matched_rules:\n%s", body)
	}
	if strings.Contains(body, `"merge_gate":`) {
		t.Errorf("DoctorOutput re-emits a merge_gate on a gitlab-family report:\n%s", body)
	}
}

// TestDoctor_GitHubReport_NoGitLabMergeGateKey pins the pointer on the mirror
// side: a github-family body (mergeGateServerBody) walked through the tool
// path re-emits NO `gitlab_merge_gate` substring at all. Counterfactual:
// making OnboardingReadinessReport.GitLabMergeGate a VALUE type turns this red
// — the re-emitted bytes then carry `"gitlab_merge_gate":{"status":"",...}`,
// a verdict outside the documented enum that no forge read established.
func TestDoctor_GitHubReport_NoGitLabMergeGateKey(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = mergeGateServerBody
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Report.MergeGate == nil || out.Report.MergeGate.Status != "required" {
		t.Fatalf("report did not decode the github merge_gate: %+v", out.Report)
	}
	if out.Report.GitLabMergeGate != nil {
		t.Errorf("GitLabMergeGate = %+v, want nil on a github-family report", *out.Report.GitLabMergeGate)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	if strings.Contains(string(encoded), "gitlab_merge_gate") {
		t.Errorf("DoctorOutput re-emits a gitlab_merge_gate on a github-family report:\n%s", encoded)
	}
}

// --- gitlab_registration rung (E45.68 / #3582) ---

// gitlabRegistrationServerBody is a literal gitlab-family backend body in which
// EVERY gitlab_registration field is NON-EMPTY — a ref-mismatch report carries
// them all — so a tag typo on either side zero-values one and the mirror test
// below cannot pass on zero values (the #3580 review's empty-fixture gap).
const gitlabRegistrationServerBody = `{
  "repo": "acme/platform/api",
  "forge": "gitlab",
  "app": {"installed": true, "resolvable": true, "note": "gitlab: installed means a gitlab installation is registered for exactly this project path (fishhawkd installation register — the authorization POST /v0/runs checks) AND the project resolves with the deployment credential (resolvable); no App installation applies on GitLab — see gitlab_registration"},
  "spec": {"source": "fetched", "valid": true},
  "reviewers": [],
  "scopes": {"adequate": true, "required": ["read:runs"], "missing": []},
  "gitlab_merge_gate": {"status": "unknown", "reason": "forbidden", "authoritative": false, "note": "gitlab: ..."},
  "gitlab_registration": {
    "status": "registered",
    "project_path": "acme/platform/api",
    "installation_ref": "gitlab:99",
    "resolved_ref": "gitlab:5",
    "ref_matches": false,
    "reason": "registry_lookup_failed",
    "detail": "the registered installation_ref gitlab:99 names a different project than acme/platform/api resolves to (gitlab:5); a run would act on the registered project",
    "remediation": "Re-register with the resolved ref: fishhawkd installation register --provider gitlab --account-key acme --installation-ref gitlab:5 --project-path acme/platform/api",
    "note": "gitlab: this rung reports whether an installations row is registered for EXACTLY this project_path ..."
  }
}`

// TestOnboardingReadinessReport_GitLabRegistrationMirrorsBackendTags decodes
// the all-fields-populated literal body and asserts EVERY gitlab_registration
// field lands with its non-zero value, plus app.resolvable — condition 3.
func TestOnboardingReadinessReport_GitLabRegistrationMirrorsBackendTags(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(gitlabRegistrationServerBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.App.Resolvable == nil || !*got.App.Resolvable {
		t.Errorf("App.Resolvable = %v, want &true", got.App.Resolvable)
	}
	if got.GitLabRegistration == nil {
		t.Fatalf("GitLabRegistration = nil, want the decoded object")
	}
	g := got.GitLabRegistration
	for _, f := range []struct{ name, got, want string }{
		{"Status", g.Status, "registered"},
		{"ProjectPath", g.ProjectPath, "acme/platform/api"},
		{"InstallationRef", g.InstallationRef, "gitlab:99"},
		{"ResolvedRef", g.ResolvedRef, "gitlab:5"},
		{"Reason", g.Reason, "registry_lookup_failed"},
		{"Detail", g.Detail, "the registered installation_ref gitlab:99 names a different project than acme/platform/api resolves to (gitlab:5); a run would act on the registered project"},
		{"Remediation", g.Remediation, "Re-register with the resolved ref: fishhawkd installation register --provider gitlab --account-key acme --installation-ref gitlab:5 --project-path acme/platform/api"},
		{"Note", g.Note, "gitlab: this rung reports whether an installations row is registered for EXACTLY this project_path ..."},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q, want %q", f.name, f.got, f.want)
		}
	}
	if g.RefMatches == nil || *g.RefMatches {
		t.Errorf("RefMatches = %v, want &false (read, not absent)", g.RefMatches)
	}
	// Re-emitted, the pointer bool survives as a literal false — never
	// dropped by omitempty as a zero value would be.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"ref_matches":false`) {
		t.Errorf("re-emitted body lost ref_matches:false:\n%s", encoded)
	}
}

// TestOnboardingReadinessReport_GitLabRegistrationAbsentKeepsNil pins the
// absence half: a github-family body (no gitlab_registration key, no
// app.resolvable) decodes with BOTH pointers nil — the mirror never invents a
// zero-valued rung or a false resolvable.
func TestOnboardingReadinessReport_GitLabRegistrationAbsentKeepsNil(t *testing.T) {
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(mergeGateServerBody), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.GitLabRegistration != nil {
		t.Errorf("GitLabRegistration = %+v, want nil on a github-family body", *got.GitLabRegistration)
	}
	if got.App.Resolvable != nil {
		t.Errorf("App.Resolvable = %v, want nil on a github-family body", *got.App.Resolvable)
	}
}

// TestOnboardingReadinessReport_ResolvableFalseSurvivesReemit pins WHY
// app.resolvable is a pointer: a not-visible project's `"resolvable": false`
// is a READ fact and must survive the mirror's re-emit as a literal false.
// Counterfactual: a value bool under omitempty drops it — the agent would then
// see neither a true nor a false and could not tell "not visible" from "never
// read" (forge unconfigured).
func TestOnboardingReadinessReport_ResolvableFalseSurvivesReemit(t *testing.T) {
	const body = `{"repo": "acme/widgets", "forge": "gitlab",
	  "app": {"installed": false, "resolvable": false, "reason": "project is not visible to the deployment GitLab credential (FISHHAWKD_GITLAB_TOKEN); confirm the token can read the project path"},
	  "spec": {"source": "unavailable"}, "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []},
	  "gitlab_registration": {"status": "registered", "project_path": "acme/widgets", "installation_ref": "gitlab:7", "note": "gitlab: ..."}}`
	var got OnboardingReadinessReport
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode backend body: %v", err)
	}
	if got.App.Resolvable == nil || *got.App.Resolvable {
		t.Fatalf("App.Resolvable = %v, want &false (read, not absent)", got.App.Resolvable)
	}
	if g := got.GitLabRegistration; g == nil || g.RefMatches != nil || g.ResolvedRef != "" {
		t.Errorf("GitLabRegistration = %+v, want registered with ref_matches and resolved_ref ABSENT (never compared)", g)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"resolvable":false`) {
		t.Errorf("re-emitted body lost resolvable:false (a read fact rendered as absence):\n%s", encoded)
	}
	if strings.Contains(string(encoded), `"ref_matches"`) {
		t.Errorf("re-emitted body invented a ref_matches on an uncompared pair:\n%s", encoded)
	}
}

// TestDoctor_GitLabReport_ReemitsGitLabRegistration walks the whole tool path
// against the gitlab body and asserts the RE-EMITTED WIRE BYTES carry the
// gitlab_registration object with its status and refs, and app.resolvable.
func TestDoctor_GitLabReport_ReemitsGitLabRegistration(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = gitlabRegistrationServerBody
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "acme/platform/api", Forge: "gitlab"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Report.GitLabRegistration == nil || out.Report.GitLabRegistration.Status != "registered" {
		t.Fatalf("report did not decode the registration rung: %+v", out.Report)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	body := string(encoded)
	for _, want := range []string{
		`"gitlab_registration":{`,
		`"status":"registered"`,
		`"installation_ref":"gitlab:99"`,
		`"resolved_ref":"gitlab:5"`,
		`"ref_matches":false`,
		`"resolvable":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DoctorOutput lacks %s:\n%s", want, body)
		}
	}
}

// TestDoctor_GitHubReport_NoGitLabRegistrationKey pins the pointers on the
// mirror side: a github-family body walked through the tool path re-emits
// NEITHER `gitlab_registration` NOR `resolvable`. Counterfactual: making
// either a VALUE type turns this red — the bytes then carry a zero-valued
// rung (status "") or `"resolvable":false`, claims no read established.
func TestDoctor_GitHubReport_NoGitLabRegistrationKey(t *testing.T) {
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = mergeGateServerBody
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.Report.MergeGate == nil || out.Report.MergeGate.Status != "required" {
		t.Fatalf("report did not decode the github merge_gate: %+v", out.Report)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	for _, key := range []string{"gitlab_registration", `"resolvable"`} {
		if strings.Contains(string(encoded), key) {
			t.Errorf("DoctorOutput re-emits %s on a github-family report:\n%s", key, encoded)
		}
	}
}

// TestDoctorToolDescription_DescribesGitLabRegistration pins the SHIPPED
// description's #3582 claims: the six-on-GitLab framing, the rung, its
// fail-closed reason codes, and the redefined installed.
func TestDoctorToolDescription_DescribesGitLabRegistration(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"five server-side-only checks the first feature_change run needs on GitHub and six on GitLab",
		"gitlab_registration — GitLab only",
		"registry_unwired",
		"registry_lookup_failed",
		"422 gitlab_project_not_registered",
		"ref_matches",
		"registered for EXACTLY this project path",
		"account_key binding",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description missing %q:\n%s", want, desc)
		}
	}
}

// --- fishhawk_init shape axis (E45.67) ---

// implementStageOfYAML parses workflow YAML and returns its implement stage.
func implementStageOfYAML(t *testing.T, workflowYAML string) spec.Stage {
	t.Helper()
	sp, err := spec.ParseBytes([]byte(workflowYAML))
	if err != nil {
		t.Fatalf("scaffold does not parse: %v", err)
	}
	if err := spec.Validate(sp); err != nil {
		t.Fatalf("scaffold does not validate: %v", err)
	}
	wf, ok := sp.Workflows["feature_change"]
	if !ok {
		t.Fatal("no feature_change workflow")
	}
	for _, st := range wf.Stages {
		if st.ID == "implement" {
			return st
		}
	}
	t.Fatal("no implement stage")
	return spec.Stage{}
}

// TestInit_ConfigOnlyShape_ReturnsValidSpecWithoutVerify crosses the tool
// handler -> spec.PresetShapeBytes -> ParseBytes+Validate path for every tier
// and asserts the config-only structural delta on the parsed implement stage.
func TestInit_ConfigOnlyShape_ReturnsValidSpecWithoutVerify(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	for _, preset := range []string{"low", "medium", "high"} {
		_, out, err := r.init(context.Background(), nil, InitInput{Preset: preset, Shape: "config-only"})
		if err != nil {
			t.Fatalf("init(%q, config-only): %v", preset, err)
		}
		if out.Shape != "config-only" {
			t.Errorf("init(%q) Shape = %q, want config-only", preset, out.Shape)
		}
		impl := implementStageOfYAML(t, out.WorkflowYAML)
		if impl.Executor.Verify != nil {
			t.Errorf("init(%q, config-only) implement executor still carries Verify: %+v", preset, impl.Executor.Verify)
		}
		if len(impl.Constraints) == 0 {
			t.Fatalf("init(%q, config-only) implement stage has no constraints", preset)
		}
		c := impl.Constraints[0]
		if len(c.RequiredOutcomes) != 1 || c.RequiredOutcomes[0] != "ci_green" {
			t.Errorf("init(%q, config-only) required_outcomes = %v, want [ci_green]", preset, c.RequiredOutcomes)
		}
		if c.MaxFilesChanged != 150 {
			t.Errorf("init(%q, config-only) max_files_changed = %d, want 150", preset, c.MaxFilesChanged)
		}
	}
}

// TestInit_DefaultShapeIsApp: an omitted shape echoes "app" and returns bytes
// identical to the app-shape PresetBytes, whose implement stage still carries
// the app defaults (make test / [tests_added_or_updated, ci_green] / 45).
func TestInit_DefaultShapeIsApp(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	_, out, err := r.init(context.Background(), nil, InitInput{Preset: "medium"})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if out.Shape != "app" {
		t.Errorf("default Shape = %q, want app", out.Shape)
	}
	want, err := spec.PresetBytes(spec.PresetMedium)
	if err != nil {
		t.Fatalf("PresetBytes(medium): %v", err)
	}
	if out.WorkflowYAML != string(want) {
		t.Errorf("default-shape WorkflowYAML differs from spec.PresetBytes(medium)")
	}
	impl := implementStageOfYAML(t, out.WorkflowYAML)
	if impl.Executor.Verify == nil || impl.Executor.Verify.Command != "make test" {
		t.Errorf("app implement verify = %+v, want command 'make test'", impl.Executor.Verify)
	}
	if len(impl.Constraints) == 0 {
		t.Fatalf("app implement stage has no constraints")
	}
	c := impl.Constraints[0]
	if len(c.RequiredOutcomes) != 2 || c.RequiredOutcomes[0] != "tests_added_or_updated" || c.RequiredOutcomes[1] != "ci_green" {
		t.Errorf("app required_outcomes = %v, want [tests_added_or_updated ci_green]", c.RequiredOutcomes)
	}
	if c.MaxFilesChanged != 45 {
		t.Errorf("app max_files_changed = %d, want 45", c.MaxFilesChanged)
	}
}

// TestInit_UnknownShape_FailsCleanly covers the unknown-shape error branch.
func TestInit_UnknownShape_FailsCleanly(t *testing.T) {
	r := &runResolver{getenv: envFuncFromMap(nil)}
	_, _, err := r.init(context.Background(), nil, InitInput{Preset: "medium", Shape: "container"})
	if err == nil || !strings.Contains(err.Error(), "unknown shape") {
		t.Fatalf("err = %v, want unknown-shape error", err)
	}
	if !strings.Contains(err.Error(), "app, config-only") {
		t.Errorf("err = %v, want the valid-shapes hint", err)
	}
}

// TestInitToolDescription_DescribesShape pins the SHIPPED tool description's
// shape claims: it names the config-only shape and states shape is chosen, not
// inferred. A prose surface no compiler enforces, so a comment-only touch must
// not satisfy it (#1169).
func TestInitToolDescription_DescribesShape(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_init")), " ")
	for _, want := range []string{
		"SHAPE axis",
		"config-only",
		"chosen EXPLICITLY — never inferred",
		"raises max_files_changed",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_init description missing %q:\n%s", want, desc)
		}
	}
}

// TestDoctor_TraceStore_ReemitsWireBytes walks the tool path and pins the
// RE-EMITTED bytes (E45.75 / #3600): a body carrying trace_store re-emits the
// object with its kind and configured verdict; a body WITHOUT it (an older
// fishhawkd — mergeGateServerBody predates the rung) re-emits NO quoted
// "trace_store" key. Counterfactual: a value-typed mirror field re-emits a
// zero-valued `"trace_store":{"configured":false,"kind":""}` on the second
// body — an out-of-enum verdict no read established.
func TestDoctor_TraceStore_ReemitsWireBytes(t *testing.T) {
	const populated = `{"repo": "x/y", "forge": "github",
	  "app": {"installed": false, "reason": "not installed"}, "spec": {"source": "unavailable", "note": "n"},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []},
	  "trace_store": {"configured": false, "kind": "none", "note": "no trace store", "remediation": "set FISHHAWKD_S3_BUCKET"}}`
	for _, tc := range []struct {
		name    string
		body    string
		present bool
	}{
		{"populated", populated, true},
		{"absent", mergeGateServerBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newDoctorFakeBackend(t)
			fb.rawBody = tc.body
			r := newResolver(srv, nil)
			_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "x/y"})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal DoctorOutput: %v", err)
			}
			body := string(encoded)
			if !tc.present {
				if strings.Contains(body, `"trace_store"`) {
					t.Errorf("DoctorOutput re-emits trace_store on a body that omitted it:\n%s", body)
				}
				return
			}
			for _, want := range []string{
				`"trace_store":{`,
				`"configured":false`,
				`"kind":"none"`,
				`"remediation":"set FISHHAWKD_S3_BUCKET"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("DoctorOutput lacks %s:\n%s", want, body)
				}
			}
		})
	}
}

// TestDoctorToolDescription_DescribesTraceStore pins the SHIPPED description's
// #3600 claims: the rung, its four kinds, the no-cascade property and the
// absence-is-not-configured:false distinction.
func TestDoctorToolDescription_DescribesTraceStore(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"trace_store",
		"DEPLOYMENT-scoped",
		"kind is s3",
		"memory (the --dev-fixtures in-memory store",
		"none (configured:false",
		"other (a non-S3",
		"NEVER cascades",
		"NOT the same claim as configured:false",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description lacks %q", want)
		}
	}
}

// TestDoctor_ReviewGrounding_ReemitsWireBytes walks the tool path and pins the
// RE-EMITTED bytes (E45.90 / #3625): a body carrying review_grounding re-emits
// the object with its enabled verdict and adapter rows; a body WITHOUT it (an
// older fishhawkd — mergeGateServerBody predates the rung) re-emits NO quoted
// "review_grounding" key. Counterfactual: a value-typed mirror field re-emits
// a zero-valued `"review_grounding":{"enabled":false}` on the second body — a
// posture verdict no read established.
func TestDoctor_ReviewGrounding_ReemitsWireBytes(t *testing.T) {
	const populated = `{"repo": "x/y", "forge": "github",
	  "app": {"installed": false, "reason": "not installed"}, "spec": {"source": "unavailable", "note": "n"},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []},
	  "review_grounding": {"enabled": false,
	    "adapters": [{"adapter": "codex", "bound": "confined"}, {"adapter": "claude", "bound": "blocklist"}],
	    "note": "reviews are DIFF-ONLY", "remediation": "set FISHHAWKD_REVIEW_GROUNDING=true"}}`
	for _, tc := range []struct {
		name    string
		body    string
		present bool
	}{
		{"populated", populated, true},
		{"absent", mergeGateServerBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newDoctorFakeBackend(t)
			fb.rawBody = tc.body
			r := newResolver(srv, nil)
			_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "x/y"})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal DoctorOutput: %v", err)
			}
			body := string(encoded)
			if !tc.present {
				if strings.Contains(body, `"review_grounding"`) {
					t.Errorf("DoctorOutput re-emits review_grounding on a body that omitted it:\n%s", body)
				}
				return
			}
			for _, want := range []string{
				`"review_grounding":{`,
				`"enabled":false`,
				`"adapter":"codex"`,
				`"bound":"confined"`,
				`"adapter":"claude"`,
				`"bound":"blocklist"`,
				`"remediation":"set FISHHAWKD_REVIEW_GROUNDING=true"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("DoctorOutput lacks %s:\n%s", want, body)
				}
			}
		})
	}
}

// TestDoctorToolDescription_DescribesReviewGrounding pins the SHIPPED
// description's #3625 claims: the rung, that enabled:false is the SUPPORTED
// DEFAULT rather than a misconfiguration, the two-value bound vocabulary with
// its asymmetry stated honestly, the no-cascade property, the static-table
// caveat, and the absence-is-not-enabled:false distinction.
func TestDoctorToolDescription_DescribesReviewGrounding(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"review_grounding",
		"DEPLOYMENT-scoped",
		"enabled:false is the SUPPORTED DEFAULT",
		"FISHHAWKD_REVIEW_GROUNDING",
		"UNTRACED / UNESTABLISHED",
		"bound=confined",
		"bound=blocklist",
		"defence-in-depth, NOT confinement",
		"STATIC restatement of the shipped posture, never a live probe",
		"NEVER cascades",
		"NOT the same claim as enabled:false",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description lacks %q", want)
		}
	}
}

// --- work_item_provider rung (E45.94 / #3646) ---

// TestDoctor_WorkItemProvider_ReemitsWireBytes walks the tool path and pins the
// RE-EMITTED bytes (E45.94 / #3646) in BOTH directions: a body carrying
// work_item_provider re-emits the object with its status, provider, registered
// array and hint; a body WITHOUT it (an older fishhawkd — mergeGateServerBody
// predates the rung) re-emits NO quoted "work_item_provider" key.
// Counterfactual: a value-typed mirror field re-emits a zero-valued
// `"work_item_provider":{"status":"","registered":null}` on the second body —
// an out-of-enum verdict no read established.
func TestDoctor_WorkItemProvider_ReemitsWireBytes(t *testing.T) {
	const populated = `{"repo": "x/y", "forge": "github",
	  "app": {"installed": false, "reason": "not installed"}, "spec": {"source": "unavailable", "note": "n"},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []},
	  "work_item_provider": {"status": "unregistered", "provider": "jira",
	    "registered": ["github_projects"],
	    "campaign_sources": [],
	    "note": "501 provider_unimplemented",
	    "missing_hint": "set FISHHAWKD_JIRA_BASE_URL"}}`
	for _, tc := range []struct {
		name    string
		body    string
		present bool
	}{
		{"populated", populated, true},
		{"absent", mergeGateServerBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newDoctorFakeBackend(t)
			fb.rawBody = tc.body
			r := newResolver(srv, nil)
			_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "x/y"})
			if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("marshal DoctorOutput: %v", err)
			}
			body := string(encoded)
			if !tc.present {
				if strings.Contains(body, `"work_item_provider"`) {
					t.Errorf("DoctorOutput re-emits work_item_provider on a body that omitted it:\n%s", body)
				}
				return
			}
			for _, want := range []string{
				`"work_item_provider":{`,
				`"status":"unregistered"`,
				`"provider":"jira"`,
				`"registered":["github_projects"]`,
				`"campaign_sources":[]`,
				`"note":"501 provider_unimplemented"`,
				`"missing_hint":"set FISHHAWKD_JIRA_BASE_URL"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("DoctorOutput lacks %s:\n%s", want, body)
				}
			}
		})
	}
}

// TestDoctorToolDescription_DescribesWorkItemProvider pins the SHIPPED
// description's #3646 claims, read off the REGISTERED tool over a real
// in-memory MCP session (not the source constant): the key, the hybrid scope,
// the closed status vocabulary, the 501 consequence naming the three foreclosed
// surfaces, the fail-closed unknown, the empty-registry sub-case, the restart
// requirement, the no-cascade property, and the absence-is-not-unregistered
// distinction.
func TestDoctorToolDescription_DescribesWorkItemProvider(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"work_item_provider",
		"HYBRID-scoped",
		"registered | unregistered | unknown",
		"status=unregistered is a FAILURE, not data",
		"fishhawk_start_campaign, fishhawk_file_issue and the whole backlog-grooming loop respond 501 provider_unimplemented",
		"Read status=unknown FAIL-CLOSED",
		"it is NOT evidence the provider is unregistered",
		"fabricates no env var",
		"a credential set after boot changes nothing until fishhawkd restarts",
		"set OUTSIDE every repo-scoped cascade",
		"NOT the same claim as unregistered",
		"campaign_sources[] (#3658)",
		"ALWAYS an array",
		"[] on a registered provider means fishhawk_start_campaign refuses 501 in either mode",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description lacks %q", want)
		}
	}
}

// --- runner_credentials rung (E45.82 / #3617) ---

// runnerCredsSentinel is a distinctive value seeded as the env token so the
// redaction assertions cannot pass by accident: it appears NOWHERE else in the
// package, so any occurrence in the marshalled rung is a real echo.
const runnerCredsSentinel = "glpat-SENTINEL-NEVER-ECHOED"

// TestRunnerCredentials_BranchPerStatus pins one case per named branch of the
// resolver. The seeding is BY CONSTRUCTION (a forge literal and a map with or
// without the key), never by calling the control inside the setup, so a RED
// under the control's deletion lands on the status assertion.
func TestRunnerCredentials_BranchPerStatus(t *testing.T) {
	cases := []struct {
		name            string
		forge           string
		env             map[string]string
		nilGetenv       bool
		wantStatus      string
		wantRemediation bool
	}{
		{
			name: "gitlab_token_present", forge: "gitlab",
			env:        map[string]string{gitLabRunnerPushCredentialEnv: runnerCredsSentinel},
			wantStatus: runnerCredentialPresent,
		},
		{
			name: "gitlab_token_absent", forge: "gitlab",
			env:        map[string]string{},
			wantStatus: runnerCredentialMissing, wantRemediation: true,
		},
		{
			// Whitespace-only is not a credential — the same tightening the
			// runner's resolveGitLabPushToken applies.
			name: "gitlab_token_whitespace_only", forge: "gitlab",
			env:        map[string]string{gitLabRunnerPushCredentialEnv: "   \t "},
			wantStatus: runnerCredentialMissing, wantRemediation: true,
		},
		{
			name: "github_not_applicable", forge: "github",
			env:        map[string]string{gitLabRunnerPushCredentialEnv: runnerCredsSentinel},
			wantStatus: runnerCredentialNotApplicable,
		},
		{
			// An older fishhawkd serves no `forge` key: applicability could not
			// be decided, which is NOT a finding that the credential is missing.
			name: "empty_forge_unknown", forge: "",
			env:        map[string]string{},
			wantStatus: runnerCredentialUnknown,
		},
		{
			name: "unrecognised_forge_unknown", forge: "bitbucket",
			env:        map[string]string{},
			wantStatus: runnerCredentialUnknown,
		},
		{
			// A nil getenv must not panic; it reads as unset.
			name: "gitlab_nil_getenv_missing", forge: "gitlab", nilGetenv: true,
			wantStatus: runnerCredentialMissing, wantRemediation: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := envFuncFromMap(tc.env)
			if tc.nilGetenv {
				getenv = nil
			}
			got := runnerCredentials(tc.forge, getenv)
			if got == nil {
				t.Fatal("runnerCredentials returned nil — the rung is rendered on every branch")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if (got.Remediation != "") != tc.wantRemediation {
				t.Errorf("remediation = %q, want present=%v", got.Remediation, tc.wantRemediation)
			}
			if got.Note == "" {
				t.Error("every branch must carry a human note")
			}
			// BINDING CONDITION 2: the scope sentence bounds the claim on EVERY
			// branch — it is the MCP server's spawning environment (local
			// runner) and says nothing about gitlab_ci.
			if got.Scope != runnerCredentialsScope {
				t.Errorf("scope = %q, want the fixed sentence", got.Scope)
			}
			for _, want := range []string{"local runner", "gitlab_ci", "PRESENCE only"} {
				if !strings.Contains(got.Scope, want) {
					t.Errorf("scope sentence lacks %q: %s", want, got.Scope)
				}
			}
			// REDACTION: the rung must never echo the token value, on any
			// branch. Marshalled, not field-by-field, so a future field is
			// covered automatically.
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal rung: %v", err)
			}
			if strings.Contains(string(encoded), runnerCredsSentinel) {
				t.Errorf("the rung echoed the token value:\n%s", encoded)
			}
		})
	}
}

// TestRunnerCredentials_VariableIsTheRunnerLiteral pins the duplicated env-var
// name. The runner and backend are separate modules with no import edge, so
// this literal is the cross-module contract: a rename on the runner side must
// redden here rather than silently decouple the rung from the runner.
func TestRunnerCredentials_VariableIsTheRunnerLiteral(t *testing.T) {
	for _, forge := range []string{"gitlab", "github", ""} {
		got := runnerCredentials(forge, envFuncFromMap(nil))
		if got.Variable != "FISHHAWK_GITLAB_TOKEN" {
			t.Errorf("forge %q: variable = %q, want the literal FISHHAWK_GITLAB_TOKEN", forge, got.Variable)
		}
		// The near-identical DAEMON variable must never be named as THIS
		// rung's variable (the #3617 near-miss).
		if got.Variable == "FISHHAWKD_GITLAB_TOKEN" {
			t.Errorf("forge %q: the rung names the DAEMON credential", forge)
		}
	}
}

// TestDoctor_RunnerCredentials_PopulatedAndReportUnchanged is the end-to-end
// check through the real handler: a gitlab-family report yields a populated
// runner_credentials SIBLING key, and `report` stays the byte-mirror of what
// the backend served — the cross-boundary proof that the new key did not
// perturb the backend-body mirror.
func TestDoctor_RunnerCredentials_PopulatedAndReportUnchanged(t *testing.T) {
	const served = `{"repo": "group/project", "forge": "gitlab",
	  "app": {"installed": true, "resolvable": true}, "spec": {"source": "fetched", "valid": true},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []}}`
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = served
	r := newResolver(srv, map[string]string{gitLabRunnerPushCredentialEnv: runnerCredsSentinel})

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "group/project", Forge: "gitlab"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.RunnerCredentials == nil {
		t.Fatal("runner_credentials is absent on a gitlab-family report")
	}
	if out.RunnerCredentials.Status != runnerCredentialPresent {
		t.Errorf("status = %q, want present", out.RunnerCredentials.Status)
	}
	if out.RunnerCredentials.Forge != "gitlab" {
		t.Errorf("forge = %q, want it echoed from the report", out.RunnerCredentials.Forge)
	}

	// The report half must decode byte-identically to what was served: the new
	// key is a SIBLING, so it cannot appear inside `report`.
	var wantReport OnboardingReadinessReport
	if err := json.Unmarshal([]byte(served), &wantReport); err != nil {
		t.Fatalf("decode the served body: %v", err)
	}
	wantBytes, err := json.Marshal(wantReport)
	if err != nil {
		t.Fatalf("marshal the expected report: %v", err)
	}
	gotBytes, err := json.Marshal(out.Report)
	if err != nil {
		t.Fatalf("marshal the returned report: %v", err)
	}
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("report changed:\n got: %s\nwant: %s", gotBytes, wantBytes)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	body := string(encoded)
	if !strings.Contains(body, `"runner_credentials":{`) {
		t.Errorf("DoctorOutput lacks the runner_credentials sibling key:\n%s", body)
	}
	if strings.Contains(string(gotBytes), "runner_credentials") {
		t.Errorf("runner_credentials leaked INSIDE report:\n%s", gotBytes)
	}
	if strings.Contains(body, runnerCredsSentinel) {
		t.Errorf("the token value reached the wire:\n%s", body)
	}
}

// TestDoctor_RunnerCredentials_GitHubStillRendered pins that a github-family
// report still CARRIES the rung (as not_applicable) rather than omitting it —
// the operator must see the question was asked and answered, not silently
// dropped.
func TestDoctor_RunnerCredentials_GitHubStillRendered(t *testing.T) {
	const served = `{"repo": "x/y", "forge": "github",
	  "app": {"installed": true, "installation_id": 4242}, "spec": {"source": "fetched", "valid": true},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []}}`
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = served
	// The runner variable IS set here: a github report must still answer
	// not_applicable, so the verdict keys on the FORGE and not on the env.
	r := newResolver(srv, map[string]string{gitLabRunnerPushCredentialEnv: runnerCredsSentinel})

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "x/y"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.RunnerCredentials == nil {
		t.Fatal("runner_credentials is absent on a github-family report — it must be RENDERED as not_applicable")
	}
	if out.RunnerCredentials.Status != runnerCredentialNotApplicable {
		t.Errorf("status = %q, want not_applicable", out.RunnerCredentials.Status)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal DoctorOutput: %v", err)
	}
	if !strings.Contains(string(encoded), `"status":"not_applicable"`) {
		t.Errorf("the not_applicable verdict is not on the wire:\n%s", encoded)
	}
}

// TestDoctor_RunnerCredentials_UnknownForge pins the end-to-end unknown branch:
// an older fishhawkd serves no `forge` key, so applicability cannot be decided
// and the rung must say so rather than report missing.
func TestDoctor_RunnerCredentials_UnknownForge(t *testing.T) {
	const served = `{"repo": "x/y",
	  "app": {"installed": true}, "spec": {"source": "fetched", "valid": true},
	  "reviewers": [], "scopes": {"adequate": true, "required": [], "missing": []}}`
	fb, srv := newDoctorFakeBackend(t)
	fb.rawBody = served
	r := newResolver(srv, nil)

	_, out, err := r.doctor(context.Background(), nil, DoctorInput{Repo: "x/y"})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if out.RunnerCredentials == nil {
		t.Fatal("runner_credentials is absent on a forge-less report")
	}
	if out.RunnerCredentials.Status != runnerCredentialUnknown {
		t.Errorf("status = %q, want unknown (NOT missing) when the report named no forge", out.RunnerCredentials.Status)
	}
	if out.RunnerCredentials.Remediation != "" {
		t.Errorf("unknown must carry no remediation, got %q", out.RunnerCredentials.Remediation)
	}
}

// TestNewServer_WiresNonNilGetenv is BINDING CONDITION 3: the production
// constructor must wire a real getenv, so runnerCredentials' nil fallback is a
// defensive branch that never fires in production. There is no runtime seam to
// read the constructed resolver back through (NewServer returns *mcp.Server),
// so the assertion is STRUCTURAL: parse server.go and require the runResolver
// composite literal inside NewServer to carry a non-nil getenv key.
func TestNewServer_WiresNonNilGetenv(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "NewServer" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatal("NewServer not found in server.go — the production constructor moved; repoint this assertion")
	}
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := lit.Type.(*ast.Ident)
		if !ok || ident.Name != "runResolver" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "getenv" {
				continue
			}
			if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "nil" {
				t.Error("NewServer wires getenv: nil — the production path must never take runnerCredentials' nil fallback")
			}
			found = true
		}
		return true
	})
	if !found {
		t.Error("NewServer's runResolver literal sets no getenv key — the zero value is nil, so runnerCredentials would read every credential as absent in production")
	}
}

// TestDoctorToolDescription_DescribesRunnerCredentials pins the SHIPPED
// description's #3617 claims, mirroring the TestDoctorToolDescription_
// DescribesTraceStore convention: the rung, that it is computed LOCALLY, the
// four statuses, the presence-only limit and the gitlab_ci exclusion.
func TestDoctorToolDescription_DescribesRunnerCredentials(t *testing.T) {
	desc := strings.Join(strings.Fields(registeredToolDescription(t, "fishhawk_doctor")), " ")
	for _, want := range []string{
		"runner_credentials",
		"SIBLING key, never a field inside it",
		"computed LOCALLY by this MCP server",
		"NOT served by fishhawkd",
		"TWO credentials",
		"FISHHAWKD_GITLAB_TOKEN",
		"FISHHAWK_GITLAB_TOKEN",
		"status is present | missing | not_applicable",
		"unknown (the report named no forge family",
		"PRESENCE only",
		"says NOTHING about a gitlab_ci runner",
		"environment that LAUNCHES fishhawk-mcp",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_doctor description lacks %q", want)
		}
	}
}
