package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// screenForbidden is the forbidden list the pure-evaluator units use — this
// repo's implement-stage forbidden_paths shape.
var screenForbidden = []string{".fishhawk/**", ".github/workflows/**", ".gitlab-ci.yml", "LICENSE", "NOTICE"}

// --- pure evaluator (evaluateCampaignAdmissionScreen) ---

func TestEvaluateCampaignAdmissionScreen_NotRunnableYieldsOneFinding(t *testing.T) {
	got, truncated := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
		{Number: 7, Title: "docs only", NotRunnable: true},
	}, screenForbidden)
	want := []campaignScreenFinding{{Issue: 7, Kind: campaignScreenKindNotRunnable}}
	if !reflect.DeepEqual(got, want) || truncated {
		t.Fatalf("findings = %+v (truncated=%v), want %+v", got, truncated, want)
	}
}

// Operator condition 1: a NotRunnable candidate ALWAYS yields its class-1
// finding, even when no forbidden paths resolved (nil and empty).
func TestEvaluateCampaignAdmissionScreen_NotRunnableWithNoForbiddenPaths(t *testing.T) {
	for name, forbidden := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			got, _ := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
				{Number: 7, Title: "touches `.gitlab-ci.yml`", NotRunnable: true},
			}, forbidden)
			want := []campaignScreenFinding{{Issue: 7, Kind: campaignScreenKindNotRunnable}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("findings = %+v, want exactly %+v", got, want)
			}
		})
	}
}

// A runnable candidate (NotRunnable=false — what runnable:yes, runnable:bogus
// and an absent label all parse to) yields no class-1 finding.
func TestEvaluateCampaignAdmissionScreen_RunnableYieldsNone(t *testing.T) {
	got, _ := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
		{Number: 1, Title: "a"}, {Number: 2, Title: "b", Body: "plain prose"},
	}, screenForbidden)
	if got != nil {
		t.Fatalf("findings = %+v, want nil", got)
	}
}

func TestEvaluateCampaignAdmissionScreen_EmptyInputsYieldNil(t *testing.T) {
	runnable := []workmgmt.EpicChild{{Number: 1, Title: "edit `.gitlab-ci.yml`"}}
	cases := map[string]struct {
		children  []workmgmt.EpicChild
		forbidden []string
	}{
		"nil children":                 {nil, screenForbidden},
		"empty children":               {[]workmgmt.EpicChild{}, screenForbidden},
		"runnable + empty forbidden":   {runnable, []string{}},
		"runnable + nil forbidden":     {runnable, nil},
		"runnable + malformed pattern": {runnable, []string{"[unterminated"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got, _ := evaluateCampaignAdmissionScreen(tc.children, tc.forbidden); got != nil {
				t.Fatalf("findings = %+v, want nil", got)
			}
		})
	}
}

func TestEvaluateCampaignAdmissionScreen_ForbiddenPathFinding(t *testing.T) {
	got, _ := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
		{Number: 5, Title: "ci", Body: "update `.gitlab-ci.yml` and docs/README.md"},
	}, screenForbidden)
	want := []campaignScreenFinding{{
		Issue: 5, Kind: campaignScreenKindForbiddenPath, Path: ".gitlab-ci.yml",
		Location: "body", ForbiddenPattern: ".gitlab-ci.yml",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %+v, want %+v", got, want)
	}
}

func TestEvaluateCampaignAdmissionScreen_TitleAndBodyDedupAttributedToTitle(t *testing.T) {
	got, _ := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
		{Number: 5, Title: "edit .github/workflows/ci.yml", Body: "the file .github/workflows/ci.yml needs it"},
	}, screenForbidden)
	if len(got) != 1 || got[0].Location != "title" || got[0].ForbiddenPattern != ".github/workflows/**" {
		t.Fatalf("findings = %+v, want one title-attributed .github/workflows/** finding", got)
	}
}

func TestEvaluateCampaignAdmissionScreen_SamePathTwoCandidatesOneEach(t *testing.T) {
	got, _ := evaluateCampaignAdmissionScreen([]workmgmt.EpicChild{
		{Number: 1, Title: "`.gitlab-ci.yml`"}, {Number: 2, Body: "`.gitlab-ci.yml`"},
	}, screenForbidden)
	if len(got) != 2 || got[0].Issue != 1 || got[1].Issue != 2 {
		t.Fatalf("findings = %+v, want one per candidate", got)
	}
}

func TestEvaluateCampaignAdmissionScreen_DeterministicOrder(t *testing.T) {
	a := []workmgmt.EpicChild{
		{Number: 3, Body: "`NOTICE` and `.gitlab-ci.yml`", NotRunnable: true},
		{Number: 1, Body: ".fishhawk/workflows.yaml"},
		{Number: 2, NotRunnable: true},
	}
	b := []workmgmt.EpicChild{a[2], a[0], a[1]}
	ga, _ := evaluateCampaignAdmissionScreen(a, screenForbidden)
	gb, _ := evaluateCampaignAdmissionScreen(b, screenForbidden)
	if !reflect.DeepEqual(ga, gb) {
		t.Fatalf("order differs across shuffled input:\n a=%+v\n b=%+v", ga, gb)
	}
	var keys []string
	for _, f := range ga {
		keys = append(keys, fmt.Sprintf("%d/%s/%s", f.Issue, f.Kind, f.Path))
	}
	want := []string{
		"1/forbidden_path/.fishhawk/workflows.yaml",
		"2/not_runnable_declared/",
		"3/forbidden_path/.gitlab-ci.yml",
		"3/forbidden_path/NOTICE",
		"3/not_runnable_declared/",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("order = %v, want %v", keys, want)
	}
}

func TestEvaluateCampaignAdmissionScreen_CapTruncates(t *testing.T) {
	var children []workmgmt.EpicChild
	for i := 1; i <= campaignAdmissionScreenMaxFindings+10; i++ {
		children = append(children, workmgmt.EpicChild{Number: i, NotRunnable: true})
	}
	got, truncated := evaluateCampaignAdmissionScreen(children, nil)
	if len(got) != campaignAdmissionScreenMaxFindings || !truncated {
		t.Fatalf("len = %d truncated = %v, want %d + true", len(got), truncated, campaignAdmissionScreenMaxFindings)
	}
	if got[0].Issue != 1 || got[len(got)-1].Issue != campaignAdmissionScreenMaxFindings {
		t.Errorf("cap did not keep the lowest issues: first=%d last=%d", got[0].Issue, got[len(got)-1].Issue)
	}
}

// --- spec resolution fail-open branches (resolveCampaignForbiddenPaths) ---

// screenSpecYAML is a minimal spec whose implement stage declares this repo's
// forbidden_paths shape.
const screenSpecYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - forbidden_paths: [".gitlab-ci.yml", ".github/workflows/**"]
        produces:
          - artifact: pull_request
  docs_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - forbidden_paths: [".fishhawk/**", ".gitlab-ci.yml"]
        produces:
          - artifact: pull_request
`

const screenSpecNoImplementYAML = `version: "0.3"
workflows:
  plan_only:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`

const screenSpecNoForbiddenYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

const screenSpecMalformedGlobYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - forbidden_paths: ["[unterminated"]
        produces:
          - artifact: pull_request
`

// screenGitHub builds a real *githubclient.Client against a fake forge serving
// the installation endpoint and the spec contents endpoint.
func screenGitHub(t *testing.T, fake *fakeGitHubForRuns) *githubclient.Client {
	t.Helper()
	srv := fake.server(t)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt", nil },
	}
}

var screenRepo = forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}

func TestResolveCampaignForbiddenPaths_UnionAcrossWorkflows(t *testing.T) {
	s := New(Config{GitHub: screenGitHub(t, newFakeGitHubForRuns(screenSpecYAML))})
	got, ok := s.resolveCampaignForbiddenPaths(context.Background(), forge.FromGitHubInstallationID(12345), screenRepo)
	want := []string{".fishhawk/**", ".github/workflows/**", ".gitlab-ci.yml"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v ok=%v, want %v ok=true", got, ok, want)
	}
}

// The nil client is seeded BY CONSTRUCTION (Config without GitHub), so a
// deleted guard reaches s.cfg.GitHub.GetWorkflowSpec on a nil pointer.
func TestResolveCampaignForbiddenPaths_NilForgeClient_FailsOpen(t *testing.T) {
	s := New(Config{})
	got, ok := s.resolveCampaignForbiddenPaths(context.Background(), forge.FromGitHubInstallationID(12345), screenRepo)
	if ok || got != nil {
		t.Fatalf("got %v ok=%v, want nil ok=false", got, ok)
	}
}

func TestResolveCampaignForbiddenPaths_FailOpenBranches(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeGitHubForRuns)
		reason string
	}{
		"not found": {func(f *fakeGitHubForRuns) {
			f.specStatus, f.specBody = http.StatusNotFound, `{"message":"Not Found"}`
		}, "no workflow spec on the default branch"},
		"transport error": {func(f *fakeGitHubForRuns) {
			f.specStatus, f.specBody = http.StatusInternalServerError, `{"message":"boom"}`
		}, "fetch workflow spec failed"},
		"unparseable spec": {func(f *fakeGitHubForRuns) { *f = *newFakeGitHubForRuns("version: [not: a spec") },
			"parse workflow spec failed"},
		"no implement": {func(f *fakeGitHubForRuns) { *f = *newFakeGitHubForRuns(screenSpecNoImplementYAML) },
			"no implement-stage forbidden_paths declared"},
		"no forbidden": {func(f *fakeGitHubForRuns) { *f = *newFakeGitHubForRuns(screenSpecNoForbiddenYAML) },
			"no implement-stage forbidden_paths declared"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeGitHubForRuns(screenSpecYAML)
			tc.mutate(fake)
			var logs bytes.Buffer
			s := New(Config{GitHub: screenGitHub(t, fake), Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			got, ok := s.resolveCampaignForbiddenPaths(context.Background(), forge.FromGitHubInstallationID(12345), screenRepo)
			if ok || got != nil {
				t.Fatalf("got %v ok=%v, want nil ok=false", got, ok)
			}
			if fake.specCalls != 1 {
				t.Errorf("spec fetches = %d, want 1 (the branch must be reached through the fetch)", fake.specCalls)
			}
			if !strings.Contains(logs.String(), tc.reason) {
				t.Errorf("logged skip reason missing %q: %s", tc.reason, logs.String())
			}
		})
	}
}

// A zero credential scope (a non-github work-item provider) fails open in the
// client before any network call.
func TestResolveCampaignForbiddenPaths_ZeroScope_FailsOpen(t *testing.T) {
	fake := newFakeGitHubForRuns(screenSpecYAML)
	s := New(Config{GitHub: screenGitHub(t, fake)})
	got, ok := s.resolveCampaignForbiddenPaths(context.Background(), forge.CredentialScope{}, screenRepo)
	if ok || got != nil {
		t.Fatalf("got %v ok=%v, want nil ok=false", got, ok)
	}
}

// --- handler-level fail-open: every degrade still creates at 201 ---

// screenChildren is one runnable candidate naming a forbidden path in its body.
func screenForbiddenDAG() *workmgmt.EpicChildrenResult {
	return &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 100, Title: "ci", Body: "edit `.gitlab-ci.yml`"}},
	}
}

func TestCreateCampaign_AdmissionScreen_DegradesNeverBlockCreate(t *testing.T) {
	cases := map[string]func() *githubclient.Client{
		"nil github": func() *githubclient.Client { return nil },
		"not found": func() *githubclient.Client {
			f := newFakeGitHubForRuns(screenSpecYAML)
			f.specStatus, f.specBody = http.StatusNotFound, `{"message":"Not Found"}`
			return screenGitHub(t, f)
		},
		"transport error": func() *githubclient.Client {
			f := newFakeGitHubForRuns(screenSpecYAML)
			f.specStatus, f.specBody = http.StatusInternalServerError, `{"message":"boom"}`
			return screenGitHub(t, f)
		},
		"unparseable":    func() *githubclient.Client { return screenGitHub(t, newFakeGitHubForRuns("version: [nope")) },
		"no implement":   func() *githubclient.Client { return screenGitHub(t, newFakeGitHubForRuns(screenSpecNoImplementYAML)) },
		"no forbidden":   func() *githubclient.Client { return screenGitHub(t, newFakeGitHubForRuns(screenSpecNoForbiddenYAML)) },
		"malformed glob": func() *githubclient.Client { return screenGitHub(t, newFakeGitHubForRuns(screenSpecMalformedGlobYAML)) },
	}
	for name, gh := range cases {
		t.Run(name, func(t *testing.T) {
			registerEpicProvider(t, &fakeEpicProvider{result: screenForbiddenDAG()})
			aud := &campaignAuditRecorder{}
			s := New(Config{CampaignRepo: newFakeCampaignRepo(), AuditRepo: aud, GitHub: gh()})
			w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "admission_screen") {
				t.Errorf("degraded screen still reported a block: %s", w.Body.String())
			}
			if n := aud.count(categoryCampaignAdmissionScreened); n != 0 {
				t.Errorf("%s audits = %d, want 0", categoryCampaignAdmissionScreened, n)
			}
		})
	}
}

// A degraded class 2 still reports class 1, flagged forbidden_paths_screened
// false so it is not mistaken for a clean class-2 result.
func TestCreateCampaign_AdmissionScreen_DegradedStillReportsNotRunnable(t *testing.T) {
	registerEpicProvider(t, &fakeEpicProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 100, Title: "x", NotRunnable: true}},
	}})
	s := New(Config{CampaignRepo: newFakeCampaignRepo()}) // GitHub nil: class 2 degrades
	w := postCampaign(t, s, `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	var created campaignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sc := created.AdmissionScreen
	if sc == nil || sc.ForbiddenPathsScreened || !sc.Advisory || len(sc.Findings) != 1 ||
		sc.Findings[0].Kind != campaignScreenKindNotRunnable {
		t.Fatalf("admission_screen = %+v, want one not_runnable_declared, screened=false", sc)
	}
}
