package mcpserver

import (
	"context"
	"encoding/json"
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
// serves only GET /v0/onboarding/readiness. lastRepo captures the last repo
// query so tests assert the env fallback; status drives the HTTP status
// (default 200); errBody, when set, is written verbatim for the error-path
// tests; rawBody, when set, is written verbatim as a 200 body so a test can
// serve a payload the Go struct cannot express (a response that OMITS
// merge_gate, as a pre-#3161 fishhawkd does); resp overrides the default
// echoed report.
type doctorFakeBackend struct {
	mu       sync.Mutex
	lastRepo string
	calls    int
	status   int
	errBody  string
	rawBody  string
	resp     *OnboardingReadinessReport
}

func newDoctorFakeBackend(t *testing.T) (*doctorFakeBackend, *httptest.Server) {
	fb := &doctorFakeBackend{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/onboarding/readiness", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fb.mu.Lock()
		fb.calls++
		fb.lastRepo = r.URL.Query().Get("repo")
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
					{Provider: "claudecode", Model: "claude-opus-4-8", Available: true},
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
	if !out.Report.Scopes.Adequate {
		t.Errorf("Scopes.Adequate = false, want true")
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
