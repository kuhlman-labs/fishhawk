package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// ghCfg configures the per-case httptest GitHub used by the capture tests.
// Each field maps to one installation-scoped endpoint the capture walks; the
// zero value serves a healthy main-default repo with no protection anywhere.
type ghCfg struct {
	defaultBranch    string
	repoStatus       int   // GET /repos/{owner}/{repo}; default 200
	installID        int64 // GET /repos/{owner}/{repo}/installation body id; default 42
	installStatus    int   // default 200
	protectionStatus int   // GET .../branches/{branch}/protection; default 404
	protectionBody   string
	rulesetsStatus   int // GET .../rulesets; default 200
	rulesetsList     string
	rulesetBodies    map[int64]string
	blockRepo        bool // block the repo lookup to force the ctx deadline
}

// newRequiredChecksGitHub stands up a real *githubclient.Client against an
// httptest mux serving the required-checks endpoints, so the capture exercises
// real JSON decoding, status classification, and ruleset condition matching.
func newRequiredChecksGitHub(t *testing.T, cfg ghCfg) *githubclient.Client {
	t.Helper()
	if cfg.defaultBranch == "" {
		cfg.defaultBranch = "main"
	}
	if cfg.repoStatus == 0 {
		cfg.repoStatus = http.StatusOK
	}
	if cfg.protectionStatus == 0 {
		cfg.protectionStatus = http.StatusNotFound
	}
	if cfg.rulesetsStatus == 0 {
		cfg.rulesetsStatus = http.StatusOK
	}
	if cfg.rulesetsList == "" {
		cfg.rulesetsList = "[]"
	}
	if cfg.installStatus == 0 {
		cfg.installStatus = http.StatusOK
	}
	if cfg.installID == 0 {
		cfg.installID = 42
	}

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(cfg.installStatus)
		if cfg.installStatus == http.StatusOK {
			fmt.Fprintf(w, `{"id":%d}`, cfg.installID)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if cfg.blockRepo {
			select {
			case <-block:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(cfg.repoStatus)
		if cfg.repoStatus == http.StatusOK {
			fmt.Fprintf(w, `{"default_branch":%q}`, cfg.defaultBranch)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{branch}/protection", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(cfg.protectionStatus)
		if cfg.protectionStatus == http.StatusOK && cfg.protectionBody != "" {
			fmt.Fprint(w, cfg.protectionBody)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(cfg.rulesetsStatus)
		if cfg.rulesetsStatus == http.StatusOK {
			fmt.Fprint(w, cfg.rulesetsList)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscanf(r.PathValue("id"), "%d", &id)
		body, ok := cfg.rulesetBodies[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{},
		AppJWT:  func() (string, error) { return "jwt", nil },
	}
}

// TestCaptureRequiredChecks_Degrades pins that EVERY degrade mode returns a NIL
// snapshot — asserted by pointer-nilness, never by len(Contexts), so a nil and
// a present-but-empty snapshot cannot alias (the whole #2506 fail-safe rests on
// that distinction).
func TestCaptureRequiredChecks_Degrades(t *testing.T) {
	inst := forge.FromGitHubInstallationID(99)
	cases := []struct {
		name  string
		gh    *githubclient.Client // nil => M1
		scope forge.CredentialScope
		repo  string
		setup func(t *testing.T)
	}{
		{name: "M1 nil github client", gh: nil, scope: inst, repo: "o/r"},
		{name: "M2 no installation (zero scope)", gh: newRequiredChecksGitHub(t, ghCfg{}), scope: forge.CredentialScope{}, repo: "o/r"},
		{name: "M3 repo without slash", gh: newRequiredChecksGitHub(t, ghCfg{}), scope: inst, repo: "no-slash"},
		{name: "M4 GetRepository 500", gh: newRequiredChecksGitHub(t, ghCfg{repoStatus: http.StatusInternalServerError}), scope: inst, repo: "o/r"},
		{name: "M5 classic protection 403", gh: newRequiredChecksGitHub(t, ghCfg{protectionStatus: http.StatusForbidden}), scope: inst, repo: "o/r"},
		{name: "M6 classic protection 500", gh: newRequiredChecksGitHub(t, ghCfg{protectionStatus: http.StatusInternalServerError}), scope: inst, repo: "o/r"},
		{name: "M7 rulesets list 404", gh: newRequiredChecksGitHub(t, ghCfg{rulesetsStatus: http.StatusNotFound}), scope: inst, repo: "o/r"},
		{name: "M8 rulesets list 500", gh: newRequiredChecksGitHub(t, ghCfg{rulesetsStatus: http.StatusInternalServerError}), scope: inst, repo: "o/r"},
		{
			name:  "M9 blocks past capture timeout",
			gh:    newRequiredChecksGitHub(t, ghCfg{blockRepo: true}),
			scope: inst, repo: "o/r",
			setup: func(t *testing.T) {
				orig := requiredChecksCaptureTimeout
				requiredChecksCaptureTimeout = 50 * time.Millisecond
				t.Cleanup(func() { requiredChecksCaptureTimeout = orig })
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			s := New(Config{GitHub: tc.gh})
			got := s.captureRequiredChecks(context.Background(), tc.repo, tc.scope)
			if got != nil {
				t.Fatalf("snapshot = %+v, want nil (pointer) on degrade", got)
			}
		})
	}
}

// TestCaptureRequiredChecks_RulesetCapture is the exact shape of THIS
// repository (#2506): classic 404 + one active branch ruleset whose ref_name
// include is ~DEFAULT_BRANCH and whose required_status_checks names CI Pass.
func TestCaptureRequiredChecks_RulesetCapture(t *testing.T) {
	gh := newRequiredChecksGitHub(t, ghCfg{
		defaultBranch:  "main",
		rulesetsList:   `[{"id":7,"target":"branch","enforcement":"active"}]`,
		rulesetBodies:  map[int64]string{7: rulesetBodyDefaultBranch("CI Pass")},
		protectionBody: "",
	})
	s := New(Config{GitHub: gh})
	snap := s.captureRequiredChecks(context.Background(), "o/r", forge.FromGitHubInstallationID(5))
	if snap == nil {
		t.Fatal("snapshot nil; want non-nil with CI Pass")
	}
	if len(snap.Contexts) != 1 || snap.Contexts[0] != "CI Pass" {
		t.Errorf("Contexts = %v, want [CI Pass]", snap.Contexts)
	}
	if len(snap.Sources) != 1 || snap.Sources[0] != "ruleset:7" {
		t.Errorf("Sources = %v, want [ruleset:7]", snap.Sources)
	}
}

// TestCaptureRequiredChecks_MasterDefaultRulesetCapture is CONDITION 1(a): a
// repo whose default branch is `master` with a ~DEFAULT_BRANCH ruleset MUST
// capture its required checks. This is RED without threading the real default
// branch into the matcher (counterfactual C3).
func TestCaptureRequiredChecks_MasterDefaultRulesetCapture(t *testing.T) {
	gh := newRequiredChecksGitHub(t, ghCfg{
		defaultBranch: "master",
		rulesetsList:  `[{"id":8,"target":"branch","enforcement":"active"}]`,
		rulesetBodies: map[int64]string{8: rulesetBodyDefaultBranch("CI Pass")},
	})
	s := New(Config{GitHub: gh})
	snap := s.captureRequiredChecks(context.Background(), "o/r", forge.FromGitHubInstallationID(5))
	if snap == nil {
		t.Fatal("snapshot nil; a master-default ~DEFAULT_BRANCH ruleset must be captured (CONDITION 1a)")
	}
	if len(snap.Contexts) != 1 || snap.Contexts[0] != "CI Pass" {
		t.Errorf("Contexts = %v, want [CI Pass] on master-default repo", snap.Contexts)
	}
}

// TestCaptureRequiredChecks_UnevaluatableTokenIsNil is CONDITION 1(b): an
// include token the matcher cannot evaluate must leave the snapshot NIL, never
// a present-but-empty one. RED without the authoritative guard (counterfactual
// for 1(b)).
func TestCaptureRequiredChecks_UnevaluatableTokenIsNil(t *testing.T) {
	gh := newRequiredChecksGitHub(t, ghCfg{
		defaultBranch: "main",
		rulesetsList:  `[{"id":9,"target":"branch","enforcement":"active"}]`,
		rulesetBodies: map[int64]string{
			9: `{"conditions":{"ref_name":{"include":["~SOMETHING_ELSE"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI Pass"}]}}]}`,
		},
	})
	s := New(Config{GitHub: gh})
	snap := s.captureRequiredChecks(context.Background(), "o/r", forge.FromGitHubInstallationID(5))
	if snap != nil {
		t.Fatalf("snapshot = %+v, want nil on an unevaluatable ruleset token (CONDITION 1b)", snap)
	}
}

// TestCaptureRequiredChecks_ClassicProtectionCapture: classic 200 with contexts
// -> Sources [branch_protection].
func TestCaptureRequiredChecks_ClassicProtectionCapture(t *testing.T) {
	gh := newRequiredChecksGitHub(t, ghCfg{
		protectionStatus: http.StatusOK,
		protectionBody:   `{"required_status_checks":{"contexts":["ci/build"]}}`,
	})
	s := New(Config{GitHub: gh})
	snap := s.captureRequiredChecks(context.Background(), "o/r", forge.FromGitHubInstallationID(5))
	if snap == nil {
		t.Fatal("snapshot nil; want classic protection captured")
	}
	if len(snap.Contexts) != 1 || snap.Contexts[0] != "ci/build" {
		t.Errorf("Contexts = %v, want [ci/build]", snap.Contexts)
	}
	if len(snap.Sources) != 1 || snap.Sources[0] != "branch_protection" {
		t.Errorf("Sources = %v, want [branch_protection]", snap.Sources)
	}
}

// TestCaptureRequiredChecks_AuthoritativeEmpty: classic 404 + an empty rulesets
// list -> a NON-NIL snapshot with zero Contexts (the #2497 "repo genuinely
// requires nothing" green arm). Asserted by pointer-nilness so it cannot alias
// the degrade cases.
func TestCaptureRequiredChecks_AuthoritativeEmpty(t *testing.T) {
	gh := newRequiredChecksGitHub(t, ghCfg{rulesetsList: "[]"})
	s := New(Config{GitHub: gh})
	snap := s.captureRequiredChecks(context.Background(), "o/r", forge.FromGitHubInstallationID(5))
	if snap == nil {
		t.Fatal("snapshot nil; an authoritative no-checks repo must be a non-nil empty snapshot")
	}
	if len(snap.Contexts) != 0 {
		t.Errorf("Contexts = %v, want empty", snap.Contexts)
	}
}

// rulesetBodyDefaultBranch builds a ruleset JSON whose ref_name include is
// ~DEFAULT_BRANCH and whose required_status_checks names ctx.
func rulesetBodyDefaultBranch(ctx string) string {
	return fmt.Sprintf(`{"conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"]}},"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":%q}]}}]}`, ctx)
}
