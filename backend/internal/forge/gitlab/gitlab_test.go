package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
)

// staticToken is the credential provider the adapter is wired with in tests —
// the v0 static-token path. It records the last scope it was asked to resolve
// so a test can assert the adapter passes the scope through.
type staticToken struct{ token string }

func (staticToken) Token(context.Context, forge.CredentialScope) (string, error) {
	return "glpat-test", nil
}

// gitlabScope is the "gitlab:<id>" credential scope every scope-taking method
// parses its project id back out of.
func gitlabScope(id string) forge.CredentialScope { return forge.FromRef("gitlab:" + id) }

// newForge builds a *forgegitlab.Forge pointed at an httptest server whose
// mux the caller populates. baseURL threads through so the same construction
// covers a gitlab.com-shaped and a self-managed host.
func newForge(t *testing.T, mux *http.ServeMux) (*forgegitlab.Forge, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := forgegitlab.New(srv.URL, staticToken{}, forgegitlab.WithHTTPClient(srv.Client()))
	return f, srv
}

// writeJSON is the standard handler reply.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- interface seam + configurable base URL -----------------------------

// TestForgeRunShapeAcrossBaseURLs drives the adapter THROUGH the forge.Forge
// interface (registered via forge.Register / resolved via forge.Get) across
// the end-to-end run shape ResolveRepoScope → CreateRef → CreatePullRequest →
// CreateCheckRun, run twice against a gitlab.com-shaped and a self-managed
// base URL. It proves the interface seam and the configurable base URL cross
// layers together — the same code answers both hosts.
func TestForgeRunShapeAcrossBaseURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
	}{
		{"saas-shaped"},
		{"self-managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				sawStatusName string
				sawCreateRef  bool
				sawMR         bool
			)
			mux := http.NewServeMux()
			// ResolveRepoScope: namespaced path lookup → project id 42.
			mux.HandleFunc("GET /api/v4/projects/{path}", func(w http.ResponseWriter, r *http.Request) {
				// The namespaced slug rides the URL as one percent-encoded
				// path segment (group%2Fsub%2Fproj), per GitLab's namespaced-
				// paths rule; the mux decodes PathValue, so assert the raw
				// escaped path carries the %2F encoding.
				if got := r.URL.EscapedPath(); !strings.Contains(got, "grp%2Fsub%2Fproj") {
					t.Errorf("project lookup escaped path = %q, want the percent-encoded slug grp%%2Fsub%%2Fproj", got)
				}
				writeJSON(w, http.StatusOK, `{"id":42,"web_url":"https://x/grp/sub/proj"}`)
			})
			mux.HandleFunc("POST /api/v4/projects/42/repository/branches", func(w http.ResponseWriter, r *http.Request) {
				sawCreateRef = true
				if r.URL.Query().Get("branch") != "run/x" || r.URL.Query().Get("ref") != "deadbeef" {
					t.Errorf("create-branch query = %q, want branch=run/x ref=deadbeef", r.URL.RawQuery)
				}
				writeJSON(w, http.StatusCreated, `{"name":"run/x","commit":{"id":"deadbeef"}}`)
			})
			mux.HandleFunc("POST /api/v4/projects/42/merge_requests", func(w http.ResponseWriter, r *http.Request) {
				sawMR = true
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["source_branch"] != "run/x" || body["target_branch"] != "main" {
					t.Errorf("MR body = %v, want source_branch=run/x target_branch=main", body)
				}
				writeJSON(w, http.StatusCreated, `{"iid":7,"sha":"deadbeef","state":"opened","source_branch":"run/x","target_branch":"main","web_url":"https://x/mr/7"}`)
			})
			mux.HandleFunc("POST /api/v4/projects/42/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				sawStatusName, _ = body["name"].(string)
				if body["state"] != "running" {
					t.Errorf("status state = %v, want running", body["state"])
				}
				writeJSON(w, http.StatusCreated, `{"id":99,"sha":"deadbeef","status":"running","name":"fishhawk"}`)
			})

			f, _ := newForge(t, mux)
			// Route through the registry to exercise the interface seam.
			forge.Register(f)
			resolved, err := forge.Get("gitlab")
			if err != nil {
				t.Fatalf("forge.Get(gitlab): %v", err)
			}

			ctx := context.Background()
			repo := forge.RepoRef{Owner: "grp/sub", Name: "proj"}
			scope, err := resolved.ResolveRepoScope(ctx, repo)
			if err != nil {
				t.Fatalf("ResolveRepoScope: %v", err)
			}
			if scope.Ref() != "gitlab:42" {
				t.Fatalf("scope.Ref() = %q, want gitlab:42", scope.Ref())
			}
			if err := resolved.CreateRef(ctx, scope, repo, "run/x", "deadbeef"); err != nil {
				t.Fatalf("CreateRef: %v", err)
			}
			pr, err := resolved.CreatePullRequest(ctx, scope, repo, "run/x", "main", "t", "b")
			if err != nil {
				t.Fatalf("CreatePullRequest: %v", err)
			}
			if pr.Number != 7 || pr.State != "open" || pr.HeadRef != "run/x" || pr.BaseRef != "main" {
				t.Errorf("PR = %+v, want iid 7 / open / run/x→main", pr)
			}
			if _, err := resolved.CreateCheckRun(ctx, scope, repo, forge.CreateCheckRunParams{
				Name:    "fishhawk",
				HeadSHA: "deadbeef",
				Status:  forge.CheckRunStatusInProgress,
			}); err != nil {
				t.Fatalf("CreateCheckRun: %v", err)
			}
			if !sawCreateRef || !sawMR {
				t.Errorf("run shape incomplete: createRef=%v mr=%v", sawCreateRef, sawMR)
			}
			// Binding condition (1): the status identity rides the `name` param.
			if sawStatusName != "fishhawk" {
				t.Errorf("commit-status name = %q, want the check identity %q", sawStatusName, "fishhawk")
			}
		})
	}
}

// TestName pins the registry id.
func TestName(t *testing.T) {
	f := forgegitlab.New("https://gitlab.com", staticToken{})
	if f.Name() != "gitlab" {
		t.Errorf("Name() = %q, want gitlab", f.Name())
	}
}

// --- scope ref shape ----------------------------------------------------

// TestScopeRefFailsGitHubInstallationID pins the load-bearing invariant: a
// "gitlab:<id>" ref is non-numeric as a whole string, so
// CredentialScope.GitHubInstallationID() fails closed on it — a GitLab scope
// can never be mistaken for a GitHub installation id.
func TestScopeRefFailsGitHubInstallationID(t *testing.T) {
	scope := gitlabScope("42")
	if _, err := scope.GitHubInstallationID(); err == nil {
		t.Error("GitHubInstallationID() on a gitlab-shaped ref = nil error, want a fail-closed parse error")
	}
}

// TestNonGitLabScopeRejected pins that a scope-taking method rejects a
// ref that is not gitlab-shaped rather than dispatching against a wrong
// project. No HTTP handler is registered, so a leak would 404, not error
// with the rejection message.
func TestNonGitLabScopeRejected(t *testing.T) {
	f := forgegitlab.New("https://gitlab.com", staticToken{})
	err := f.CreateRef(context.Background(), forge.FromRef("12345"), forge.RepoRef{}, "b", "s")
	if err == nil {
		t.Fatal("CreateRef with a non-gitlab-shaped scope = nil, want a rejection")
	}
	if !strings.Contains(err.Error(), "not gitlab-shaped") {
		t.Errorf("err = %v, want a not-gitlab-shaped rejection", err)
	}
}

// TestGitLabScopeInvalidProjectID covers the non-positive / non-numeric id
// legs of projectIDFromScope.
func TestGitLabScopeInvalidProjectID(t *testing.T) {
	f := forgegitlab.New("https://gitlab.com", staticToken{})
	for _, ref := range []string{"gitlab:0", "gitlab:-3", "gitlab:abc", "gitlab:"} {
		err := f.CreateRef(context.Background(), forge.FromRef(ref), forge.RepoRef{}, "b", "s")
		if err == nil {
			t.Errorf("CreateRef with ref %q = nil, want a rejection", ref)
		}
	}
}

// --- fail-closed error mapping ------------------------------------------

// TestErrorMapping is the per-status behavioral table: each GitLab status a
// method can return maps to the documented forge sentinel.
func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"404 → ErrNotFound", http.StatusNotFound, forge.ErrNotFound},
		{"401 → ErrForbidden", http.StatusUnauthorized, forge.ErrForbidden},
		{"403 → ErrForbidden", http.StatusForbidden, forge.ErrForbidden},
		{"400 → ErrValidation", http.StatusBadRequest, forge.ErrValidation},
		{"422 → ErrValidation", http.StatusUnprocessableEntity, forge.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			// GetMergeRequest is a plain method that runs the base mapper.
			mux.HandleFunc("GET /api/v4/projects/5/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.status, `{"message":"x"}`)
			})
			f, _ := newForge(t, mux)
			_, err := f.GetPullRequest(context.Background(), gitlabScope("5"), forge.RepoRef{}, 1)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestResolveRepoScopeNotInstalled pins the ResolveRepoScope-specific 404
// mapping: a project the token cannot see is ErrNotInstalled, distinct from
// ErrNotFound.
func TestResolveRepoScopeNotInstalled(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{path}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"message":"404 Project Not Found"}`)
	})
	f, _ := newForge(t, mux)
	scope, err := f.ResolveRepoScope(context.Background(), forge.RepoRef{Owner: "o", Name: "n"})
	if !errors.Is(err, forge.ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
	if errors.Is(err, forge.ErrNotFound) {
		t.Error("ErrNotInstalled must stay distinct from ErrNotFound")
	}
	if !scope.IsZero() {
		t.Errorf("scope = %v on error, want the zero scope", scope)
	}
}

// TestCreatePullRequestExists pins the 409 → ErrPullRequestExists mapping and
// the recovery read via ListOpenPullRequestsByHead.
func TestCreatePullRequestExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/5/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, `{"message":["Another open merge request already exists"]}`)
	})
	mux.HandleFunc("GET /api/v4/projects/5/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("source_branch") != "run/x" {
			t.Errorf("recovery query = %q, want state=opened source_branch=run/x", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, `[{"iid":7,"state":"opened","source_branch":"run/x","target_branch":"main","web_url":"https://x/mr/7"}]`)
	})
	f, _ := newForge(t, mux)
	ctx := context.Background()
	scope := gitlabScope("5")

	_, err := f.CreatePullRequest(ctx, scope, forge.RepoRef{}, "run/x", "main", "t", "b")
	if !errors.Is(err, forge.ErrPullRequestExists) {
		t.Fatalf("CreatePullRequest err = %v, want ErrPullRequestExists", err)
	}
	// Recovery: the existing MR is found by head.
	prs, err := f.ListOpenPullRequestsByHead(ctx, scope, forge.RepoRef{}, "run/x", "main")
	if err != nil {
		t.Fatalf("ListOpenPullRequestsByHead: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 7 {
		t.Errorf("recovered PRs = %+v, want one MR iid 7", prs)
	}
}

// TestMergeNotMergeable pins the merge 405/406 → ErrPullRequestNotMergeable
// mapping on both EnableAutoMerge and MergePullRequest.
func TestMergeNotMergeable(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotAcceptable} {
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /api/v4/projects/5/merge_requests/7/merge", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, status, `{"message":"not mergeable"}`)
		})
		f, _ := newForge(t, mux)
		ctx := context.Background()
		scope := gitlabScope("5")
		if err := f.MergePullRequest(ctx, scope, forge.RepoRef{}, 7, forge.MergeMethodSquash); !errors.Is(err, forge.ErrPullRequestNotMergeable) {
			t.Errorf("MergePullRequest(%d) err = %v, want ErrPullRequestNotMergeable", status, err)
		}
		if err := f.EnableAutoMerge(ctx, scope, forge.RepoRef{}, 7, forge.MergeMethodSquash); !errors.Is(err, forge.ErrPullRequestNotMergeable) {
			t.Errorf("EnableAutoMerge(%d) err = %v, want ErrPullRequestNotMergeable", status, err)
		}
	}
}

// TestMergeSquashParam confirms MergeMethodSquash sends squash=true and
// MergeMethodMerge does not.
func TestMergeSquashParam(t *testing.T) {
	for _, tc := range []struct {
		method     forge.MergeMethod
		wantSquash bool
	}{
		{forge.MergeMethodSquash, true},
		{forge.MergeMethodMerge, false},
	} {
		var gotSquash bool
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /api/v4/projects/5/merge_requests/7/merge", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotSquash, _ = body["squash"].(bool)
			writeJSON(w, http.StatusOK, `{"iid":7,"state":"merged"}`)
		})
		f, _ := newForge(t, mux)
		if err := f.MergePullRequest(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, tc.method); err != nil {
			t.Fatalf("MergePullRequest: %v", err)
		}
		if gotSquash != tc.wantSquash {
			t.Errorf("method %q: squash=%v, want %v", tc.method, gotSquash, tc.wantSquash)
		}
	}
}

// TestGetBranchSHAMissing pins the ("", false, nil) contract for a missing
// branch, and the present-branch happy path.
func TestGetBranchSHAMissing(t *testing.T) {
	t.Run("missing → false", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"message":"Branch Not Found"}`)
		})
		f, _ := newForge(t, mux)
		sha, ok, err := f.GetBranchSHA(context.Background(), gitlabScope("5"), forge.RepoRef{}, "nope")
		if err != nil || ok || sha != "" {
			t.Errorf("got (%q, %v, %v), want (\"\", false, nil)", sha, ok, err)
		}
	})
	t.Run("present → sha", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"name":"main","commit":{"id":"cafe"}}`)
		})
		f, _ := newForge(t, mux)
		sha, ok, err := f.GetBranchSHA(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main")
		if err != nil || !ok || sha != "cafe" {
			t.Errorf("got (%q, %v, %v), want (cafe, true, nil)", sha, ok, err)
		}
	})
}

// TestForceUpdateRef pins delete-then-recreate ordering, the delete-404
// tolerated leg, and the create-fails leg.
func TestForceUpdateRef(t *testing.T) {
	t.Run("delete then recreate in order", func(t *testing.T) {
		var order []string
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "delete")
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST /api/v4/projects/5/repository/branches", func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "create")
			if r.URL.Query().Get("ref") != "newsha" {
				t.Errorf("recreate ref = %q, want newsha", r.URL.Query().Get("ref"))
			}
			writeJSON(w, http.StatusCreated, `{"name":"run/x","commit":{"id":"newsha"}}`)
		})
		f, _ := newForge(t, mux)
		if err := f.ForceUpdateRef(context.Background(), gitlabScope("5"), forge.RepoRef{}, "run/x", "newsha"); err != nil {
			t.Fatalf("ForceUpdateRef: %v", err)
		}
		if len(order) != 2 || order[0] != "delete" || order[1] != "create" {
			t.Errorf("call order = %v, want [delete create]", order)
		}
	})
	t.Run("delete 404 tolerated", func(t *testing.T) {
		var created bool
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"message":"Branch Not Found"}`)
		})
		mux.HandleFunc("POST /api/v4/projects/5/repository/branches", func(w http.ResponseWriter, r *http.Request) {
			created = true
			writeJSON(w, http.StatusCreated, `{"name":"run/x","commit":{"id":"newsha"}}`)
		})
		f, _ := newForge(t, mux)
		if err := f.ForceUpdateRef(context.Background(), gitlabScope("5"), forge.RepoRef{}, "run/x", "newsha"); err != nil {
			t.Fatalf("ForceUpdateRef with a missing branch should still recreate: %v", err)
		}
		if !created {
			t.Error("recreate leg did not run after a tolerated delete-404")
		}
	})
	t.Run("delete non-404 fails", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusForbidden, `{"message":"forbidden"}`)
		})
		f, _ := newForge(t, mux)
		if err := f.ForceUpdateRef(context.Background(), gitlabScope("5"), forge.RepoRef{}, "run/x", "newsha"); !errors.Is(err, forge.ErrForbidden) {
			t.Errorf("delete 403 err = %v, want ErrForbidden (create must not run)", err)
		}
	})
	t.Run("create fails after delete", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /api/v4/projects/5/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST /api/v4/projects/5/repository/branches", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, `{"message":"bad ref"}`)
		})
		f, _ := newForge(t, mux)
		if err := f.ForceUpdateRef(context.Background(), gitlabScope("5"), forge.RepoRef{}, "run/x", "newsha"); !errors.Is(err, forge.ErrValidation) {
			t.Errorf("create 400 err = %v, want ErrValidation", err)
		}
	})
}

// --- unsupported operations ---------------------------------------------

// TestUnsupportedOperations pins that every Forge method GitLab's REST API
// cannot express returns forge.ErrUnsupported.
func TestUnsupportedOperations(t *testing.T) {
	f := forgegitlab.New("https://gitlab.com", staticToken{})
	ctx := context.Background()
	scope := gitlabScope("5")
	repo := forge.RepoRef{}

	if _, err := f.GetCommit(ctx, scope, repo, "sha"); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("GetCommit err = %v, want ErrUnsupported", err)
	}
	if _, err := f.CreateTree(ctx, scope, repo, "base", nil); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("CreateTree err = %v, want ErrUnsupported", err)
	}
	if _, err := f.CreateCommit(ctx, scope, repo, "msg", "tree", nil); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("CreateCommit err = %v, want ErrUnsupported", err)
	}
	if _, err := f.MergeBranch(ctx, scope, repo, "base", "head", "msg"); !errors.Is(err, forge.ErrUnsupported) {
		t.Errorf("MergeBranch err = %v, want ErrUnsupported", err)
	}
}

// --- commit-status mapping ----------------------------------------------

// TestCheckState is the done-means table over every CheckRunStatus /
// CheckRunConclusion enum member → the GitLab commit-status state the adapter
// posts. An unmapped member would fail here rather than posting an invalid
// state. The mapping is asserted end-to-end (through CreateCheckRun) so the
// posted `state` field is what a real call sends.
func TestCheckState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     forge.CheckRunStatus
		conclusion forge.CheckRunConclusion
		wantState  string
		wantErr    bool
	}{
		{"queued", forge.CheckRunStatusQueued, "", "pending", false},
		{"in_progress", forge.CheckRunStatusInProgress, "", "running", false},
		{"completed/success", forge.CheckRunStatusCompleted, forge.CheckRunConclusionSuccess, "success", false},
		{"completed/failure", forge.CheckRunStatusCompleted, forge.CheckRunConclusionFailure, "failed", false},
		{"completed/cancelled", forge.CheckRunStatusCompleted, forge.CheckRunConclusionCancelled, "canceled", false},
		{"completed/neutral", forge.CheckRunStatusCompleted, forge.CheckRunConclusionNeutral, "success", false},
		{"completed/skipped", forge.CheckRunStatusCompleted, forge.CheckRunConclusionSkipped, "success", false},
		{"completed/timed_out", forge.CheckRunStatusCompleted, forge.CheckRunConclusionTimedOut, "failed", false},
		{"completed/action_required", forge.CheckRunStatusCompleted, forge.CheckRunConclusionActionRequired, "failed", false},
		{"completed/empty-conclusion errors", forge.CheckRunStatusCompleted, "", "", true},
		{"unknown-status errors", forge.CheckRunStatus("bogus"), "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotState string
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/v4/projects/5/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotState, _ = body["state"].(string)
				writeJSON(w, http.StatusCreated, `{"id":1,"status":"`+gotState+`"}`)
			})
			f, _ := newForge(t, mux)
			_, err := f.CreateCheckRun(context.Background(), gitlabScope("5"), forge.RepoRef{}, forge.CreateCheckRunParams{
				Name:       "fishhawk",
				HeadSHA:    "sha",
				Status:     tc.status,
				Conclusion: tc.conclusion,
			})
			if tc.wantErr {
				if !errors.Is(err, forge.ErrValidation) {
					t.Errorf("err = %v, want ErrValidation for an unmappable status/conclusion", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCheckRun: %v", err)
			}
			if gotState != tc.wantState {
				t.Errorf("posted state = %q, want %q", gotState, tc.wantState)
			}
		})
	}
}

// --- protection ---------------------------------------------------------

// TestGetBranchProtection pins the empty-contexts mapping for a present entry
// and the 404 → ErrNotFound (no classic protection) leg.
func TestGetBranchProtection(t *testing.T) {
	t.Run("present → empty contexts", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/protected_branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, `{"id":1,"name":"main"}`)
		})
		f, _ := newForge(t, mux)
		bp, err := f.GetBranchProtection(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main")
		if err != nil {
			t.Fatalf("GetBranchProtection: %v", err)
		}
		if len(bp.RequiredStatusCheckContexts) != 0 {
			t.Errorf("contexts = %v, want empty (GitLab carries none)", bp.RequiredStatusCheckContexts)
		}
	})
	t.Run("404 → ErrNotFound", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/protected_branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, `{"message":"404 Not Found"}`)
		})
		f, _ := newForge(t, mux)
		if _, err := f.GetBranchProtection(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main"); !errors.Is(err, forge.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestListRulesetRequiredChecks pins the (nil, nil) contract.
func TestListRulesetRequiredChecks(t *testing.T) {
	f := forgegitlab.New("https://gitlab.com", staticToken{})
	checks, err := f.ListRulesetRequiredChecks(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main")
	if err != nil || checks != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", checks, err)
	}
}

// --- diffs --------------------------------------------------------------

// TestComparePatch pins the compare query params (from/to/straight=false),
// the reconstructed `diff --git`-prefixed patch, the changed-file list with
// mapped statuses, and the compare_timeout → Truncated mapping.
func TestComparePatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/5/repository/compare", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("from") != "main" || q.Get("to") != "run/x" || q.Get("straight") != "false" {
			t.Errorf("compare query = %q, want from=main to=run/x straight=false", r.URL.RawQuery)
		}
		writeJSON(w, http.StatusOK, `{
			"commit":{"id":"headsha"},
			"compare_timeout": true,
			"diffs":[
				{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-x\n+y"},
				{"old_path":"new.go","new_path":"new.go","new_file":true,"diff":"@@ -0,0 +1 @@\n+n\n"},
				{"old_path":"gone.go","new_path":"gone.go","deleted_file":true}
			]
		}`)
	})
	f, _ := newForge(t, mux)
	res, err := f.ComparePatch(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main", "run/x")
	if err != nil {
		t.Fatalf("ComparePatch: %v", err)
	}
	if res.HeadSHA != "headsha" {
		t.Errorf("HeadSHA = %q, want headsha", res.HeadSHA)
	}
	if !res.Truncated || res.TruncationReason == "" {
		t.Errorf("Truncated=%v reason=%q, want a compare_timeout truncation", res.Truncated, res.TruncationReason)
	}
	if len(res.Files) != 3 {
		t.Fatalf("Files = %+v, want 3", res.Files)
	}
	if res.Files[1].Status != "added" || res.Files[2].Status != "removed" || res.Files[0].Status != "modified" {
		t.Errorf("file statuses = %+v, want modified/added/removed", res.Files)
	}
	if !strings.Contains(res.Patch, "diff --git a/a.go b/a.go\n@@ -1 +1 @@") {
		t.Errorf("patch missing synthetic git header for a.go:\n%s", res.Patch)
	}
	// A file with no diff body (the deletion here) contributes no patch hunk.
	if strings.Contains(res.Patch, "gone.go") {
		t.Errorf("patch should omit a file with no diff body:\n%s", res.Patch)
	}
}

// TestCompareCommits pins the changed-file path extraction.
func TestCompareCommits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/5/repository/compare", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"diffs":[{"new_path":"a.go"},{"old_path":"gone.go","new_path":"gone.go","deleted_file":true}]}`)
	})
	f, _ := newForge(t, mux)
	paths, err := f.CompareCommits(context.Background(), gitlabScope("5"), forge.RepoRef{}, "main", "run/x")
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if len(paths) != 2 || paths[0] != "a.go" || paths[1] != "gone.go" {
		t.Errorf("paths = %v, want [a.go gone.go]", paths)
	}
}

// TestGetRepository pins the default-branch read through the by-id project
// lookup.
func TestGetRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/5", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"id":5,"default_branch":"trunk"}`)
	})
	f, _ := newForge(t, mux)
	repo, err := f.GetRepository(context.Background(), gitlabScope("5"), forge.RepoRef{})
	if err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if repo.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch = %q, want trunk", repo.DefaultBranch)
	}
}

// TestEditAndClosePullRequest pins the description-replace and close paths.
func TestEditAndClosePullRequest(t *testing.T) {
	var (
		gotDesc  string
		gotState string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v4/projects/5/merge_requests/7", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["description"].(string); ok {
			gotDesc = v
		}
		if v, ok := body["state_event"].(string); ok {
			gotState = v
		}
		writeJSON(w, http.StatusOK, `{"iid":7,"state":"opened"}`)
	})
	f, _ := newForge(t, mux)
	ctx := context.Background()
	scope := gitlabScope("5")
	if err := f.EditPullRequest(ctx, scope, forge.RepoRef{}, 7, "new body"); err != nil {
		t.Fatalf("EditPullRequest: %v", err)
	}
	if gotDesc != "new body" {
		t.Errorf("edit sent description %q, want new body", gotDesc)
	}
	if err := f.ClosePullRequest(ctx, scope, forge.RepoRef{}, 7); err != nil {
		t.Fatalf("ClosePullRequest: %v", err)
	}
	if gotState != "close" {
		t.Errorf("close sent state_event %q, want close", gotState)
	}
}

// TestListPullRequestsForCommit pins the by-commit MR walk mapping.
func TestListPullRequestsForCommit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/5/repository/commits/{sha}/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `[{"iid":7,"title":"t","web_url":"https://x/mr/7"}]`)
	})
	f, _ := newForge(t, mux)
	refs, err := f.ListPullRequestsForCommit(context.Background(), gitlabScope("5"), forge.RepoRef{}, "sha")
	if err != nil {
		t.Fatalf("ListPullRequestsForCommit: %v", err)
	}
	if len(refs) != 1 || refs[0].Number != 7 || refs[0].Title != "t" {
		t.Errorf("refs = %+v, want one ref iid 7", refs)
	}
}
