package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- fishhawk_doctor (E29.6 / #1506) ---

// doctorFakeBackend is a self-contained backend stub for the doctor tool: it
// serves only GET /v0/onboarding/readiness. lastRepo captures the last repo
// query so tests assert the env fallback; status drives the HTTP status
// (default 200); errBody, when set, is written verbatim for the error-path
// tests; resp overrides the default echoed report.
type doctorFakeBackend struct {
	mu       sync.Mutex
	lastRepo string
	calls    int
	status   int
	errBody  string
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
		resp := fb.resp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
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
