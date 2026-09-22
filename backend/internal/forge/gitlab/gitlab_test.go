package gitlab_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
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

// muxDoer is a gitlabclient.Doer that asserts every request the adapter emits
// targets the CONFIGURED base-URL host/scheme, then serves it against an
// in-process mux. Because the base URL is what puts the host/scheme on the
// request, asserting them per case genuinely pins base-URL configurability
// with no network — a base-URL-specific regression (a hardcoded host, a
// dropped scheme) fails here.
type muxDoer struct {
	t          *testing.T
	mux        *http.ServeMux
	wantScheme string
	wantHost   string
}

func (d *muxDoer) Do(req *http.Request) (*http.Response, error) {
	d.t.Helper()
	if req.URL.Scheme != d.wantScheme || req.URL.Host != d.wantHost {
		d.t.Errorf("request URL = %s://%s%s, want scheme %s host %s",
			req.URL.Scheme, req.URL.Host, req.URL.Path, d.wantScheme, d.wantHost)
	}
	rec := httptest.NewRecorder()
	d.mux.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// TestForgeRunShapeAcrossBaseURLs drives the adapter THROUGH the forge.Forge
// interface (registered via forge.Register / resolved via forge.Get) across
// the end-to-end run shape ResolveRepoScope → CreateRef → CreatePullRequest →
// CreateCheckRun, run twice against a gitlab.com-shaped and a self-managed
// base URL. Each case wires the adapter to that base URL and asserts every
// emitted request carries the matching host/scheme (via muxDoer), so the two
// runs are genuinely distinct — the same code answers both hosts and a
// base-URL-specific regression is caught, not duplicated.
func TestForgeRunShapeAcrossBaseURLs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseURL    string
		wantScheme string
		wantHost   string
	}{
		{"saas-shaped", "https://gitlab.com", "https", "gitlab.com"},
		{"self-managed", "https://gitlab.example.com", "https", "gitlab.example.com"},
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

			f := forgegitlab.New(tc.baseURL, staticToken{}, forgegitlab.WithHTTPClient(&muxDoer{
				t: t, mux: mux, wantScheme: tc.wantScheme, wantHost: tc.wantHost,
			}))
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

// TestGetPullRequest_DecodesMergeEvidence pins the E64.40 / #3151 merge-evidence
// mapping through the real newForge mux: mergeRequestToPR must carry BOTH the
// MR's merged_at and merge_commit_sha onto forge.PullRequest, and a null pair
// must leave MergedAt NIL (not the zero time) and MergeCommitSHA empty. These
// two fields are the whole payload of the record-merge-observation verb, so a
// mapping that dropped one passes every presence gate and records an observation
// carrying no evidence.
func TestGetPullRequest_DecodesMergeEvidence(t *testing.T) {
	t.Run("merged: merged_at and merge_commit_sha reach the PR", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/merge_requests/1", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK,
				`{"iid":1,"state":"merged","merge_commit_sha":"mc-cafebabe","merged_at":"2026-08-30T12:34:56Z","target_branch":"main"}`)
		})
		f, _ := newForge(t, mux)
		pr, err := f.GetPullRequest(context.Background(), gitlabScope("5"), forge.RepoRef{}, 1)
		if err != nil {
			t.Fatalf("GetPullRequest: %v", err)
		}
		if !pr.Merged {
			t.Errorf("Merged = false, want true on a merged MR")
		}
		if pr.MergeCommitSHA != "mc-cafebabe" {
			t.Errorf("MergeCommitSHA = %q, want mc-cafebabe", pr.MergeCommitSHA)
		}
		if pr.MergedAt == nil {
			t.Fatalf("MergedAt = nil, want the decoded merge timestamp")
		}
		want := time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC)
		if !pr.MergedAt.Equal(want) {
			t.Errorf("MergedAt = %v, want %v", pr.MergedAt.UTC(), want)
		}
	})
	t.Run("opened: null merge evidence stays nil/empty", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/projects/5/merge_requests/1", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK,
				`{"iid":1,"state":"opened","merge_commit_sha":null,"merged_at":null,"target_branch":"main"}`)
		})
		f, _ := newForge(t, mux)
		pr, err := f.GetPullRequest(context.Background(), gitlabScope("5"), forge.RepoRef{}, 1)
		if err != nil {
			t.Fatalf("GetPullRequest: %v", err)
		}
		if pr.MergedAt != nil {
			t.Errorf("MergedAt = %v, want nil (a null merged_at must not become the zero time)", pr.MergedAt.UTC())
		}
		if pr.MergeCommitSHA != "" {
			t.Errorf("MergeCommitSHA = %q, want empty on an unmerged MR", pr.MergeCommitSHA)
		}
	})
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

// --- pipelines ----------------------------------------------------------

// TestTriggerPipeline pins the resolve → CreatePipeline wrapper the gitlab_ci
// dispatch backend (#1861) drives: the scope's project id is parsed back out,
// and the POST /pipeline carries the ref and the ordered CI/CD variables.
func TestTriggerPipeline(t *testing.T) {
	var gotRef string
	var gotVars []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v4/projects/5/pipeline", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRef, _ = body["ref"].(string)
		if raw, ok := body["variables"].([]any); ok {
			for _, v := range raw {
				if m, ok := v.(map[string]any); ok {
					gotVars = append(gotVars, m)
				}
			}
		}
		writeJSON(w, http.StatusCreated, `{"id":900,"ref":"fishhawk/run-abc12345","status":"created"}`)
	})
	f, _ := newForge(t, mux)

	err := f.TriggerPipeline(context.Background(), gitlabScope("5"), "fishhawk/run-abc12345",
		[]gitlabclient.PipelineVariable{
			{Key: "run_id", Value: "r1"},
			{Key: "stage", Value: "claude-code"},
		})
	if err != nil {
		t.Fatalf("TriggerPipeline: %v", err)
	}
	if gotRef != "fishhawk/run-abc12345" {
		t.Errorf("ref = %q, want fishhawk/run-abc12345", gotRef)
	}
	if len(gotVars) != 2 || gotVars[0]["key"] != "run_id" || gotVars[1]["key"] != "stage" {
		t.Errorf("variables = %v", gotVars)
	}
}

// A non-gitlab-shaped scope fails closed in resolve before any HTTP call.
func TestTriggerPipeline_NonGitLabScopeRejected(t *testing.T) {
	f, _ := newForge(t, http.NewServeMux())
	err := f.TriggerPipeline(context.Background(), forge.FromRef("12345"), "main", nil)
	if err == nil {
		t.Fatal("want error for a non-gitlab-shaped scope")
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

// --- file fetch ---------------------------------------------------------

// fileFetchMux is the Repository Files handler the FetchFile tests share:
// it asserts the project and file paths each ride the URL as one
// percent-encoded segment and the REQUIRED ref=HEAD rides the query, then
// serves the base64 body.
func fileFetchMux(t *testing.T, content string) *http.ServeMux {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{proj}/repository/files/{file}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); !strings.Contains(got, "grp%2Fsub%2Fproj") ||
			!strings.Contains(got, ".fishhawk%2Fwork-management.yaml") {
			t.Errorf("escaped path = %q, want percent-encoded project and file segments", got)
		}
		if got := r.URL.Query().Get("ref"); got != "HEAD" {
			t.Errorf("ref = %q, want HEAD", got)
		}
		writeJSON(w, http.StatusOK,
			`{"file_path":".fishhawk/work-management.yaml","blob_id":"blob123","encoding":"base64","content":"`+encoded+`"}`)
	})
	return mux
}

// TestFetchFileRoundTrip pins the forge.FileFetcher round-trip through
// gitlabclient.GetFile: the namespaced repo addresses the project, the
// explicit ref=HEAD rides the query, and the decoded body + blob id land
// on *forge.FileContent, dispatchable through the capability interface.
func TestFetchFileRoundTrip(t *testing.T) {
	f, _ := newForge(t, fileFetchMux(t, "provider: gitlab\n"))
	var fetcher forge.FileFetcher = f
	fc, err := fetcher.FetchFile(context.Background(), forge.CredentialScope{},
		forge.RepoRef{Owner: "grp/sub", Name: "proj"}, ".fishhawk/work-management.yaml", "HEAD")
	if err != nil {
		t.Fatalf("FetchFile: %v", err)
	}
	if fc.Path != ".fishhawk/work-management.yaml" {
		t.Errorf("Path = %q", fc.Path)
	}
	if got := string(fc.Content); got != "provider: gitlab\n" {
		t.Errorf("Content = %q, want the decoded file body", got)
	}
	if fc.SHA != "blob123" {
		t.Errorf("SHA = %q, want blob123", fc.SHA)
	}
}

// TestFetchFileDefaultsRefToHEAD pins the empty-ref default: the
// Repository Files API requires an explicit ref, so the adapter
// substitutes HEAD (asserted on the wire by fileFetchMux) rather than
// emitting a request GitLab would reject.
func TestFetchFileDefaultsRefToHEAD(t *testing.T) {
	f, _ := newForge(t, fileFetchMux(t, "provider: gitlab\n"))
	if _, err := f.FetchFile(context.Background(), forge.CredentialScope{},
		forge.RepoRef{Owner: "grp/sub", Name: "proj"}, ".fishhawk/work-management.yaml", ""); err != nil {
		t.Fatalf("FetchFile with empty ref: %v", err)
	}
}

// failingToken is a credential provider whose resolution always fails —
// the FetchFile token-error leg.
type failingToken struct{}

func (failingToken) Token(context.Context, forge.CredentialScope) (string, error) {
	return "", errors.New("token boom")
}

// TestFetchFileTokenError pins the fail-closed token-resolution leg: a
// provider failure propagates before any HTTP call. No Doer is injected,
// so a leaked request would hit the real network host, not a stub — the
// error content proves the call never got that far.
func TestFetchFileTokenError(t *testing.T) {
	f := forgegitlab.New("https://gitlab.invalid", failingToken{})
	fc, err := f.FetchFile(context.Background(), forge.CredentialScope{},
		forge.RepoRef{Owner: "grp", Name: "proj"}, "f.yaml", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "token boom") {
		t.Errorf("err = %v, want the provider's token error", err)
	}
	if fc != nil {
		t.Errorf("FileContent = %+v on error, want nil", fc)
	}
}

// TestFetchFileNotFound pins the 404 → forge.ErrNotFound mapping — the
// sentinel the conventions loader's fall-through branch switches on.
func TestFetchFileNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/{proj}/repository/files/{file}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"message":"404 File Not Found"}`)
	})
	f, _ := newForge(t, mux)
	fc, err := f.FetchFile(context.Background(), forge.CredentialScope{},
		forge.RepoRef{Owner: "grp/sub", Name: "proj"}, "missing.yaml", "HEAD")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want forge.ErrNotFound", err)
	}
	if fc != nil {
		t.Errorf("FileContent = %+v on error, want nil", fc)
	}
}

// --- per-installation endpoint routing (E44.16 / #2094) -----------------

// countingBranchServer answers GET .../projects/42/repository/branches/{branch}
// with a fixed tip, counting requests and recording the PRIVATE-TOKEN so a
// cross-boundary test can prove a resolved base routes a real forge op here and
// a fail-closed test can prove ZERO requests reach it (no token ships). TLS so
// a resolved base passes account.ValidateResolvedBaseURL's https requirement.
func countingBranchServer(t *testing.T) (*httptest.Server, *int64, *string) {
	t.Helper()
	var count int64
	var gotToken string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/42/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		writeJSON(w, http.StatusOK, `{"name":"main","commit":{"id":"deadbeef"}}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &count, &gotToken
}

// TestForge_PerInstallation_RoutesToResolvedHost is the cross-boundary happy
// path (resolver -> Forge -> client construction -> real outbound host): a
// scope-taking forge op (GetBranchSHA) reaches the RESOLVED instance host, the
// resolver is keyed on the scope's installation ref, and the token ships there
// — not to the deployment default.
func TestForge_PerInstallation_RoutesToResolvedHost(t *testing.T) {
	resolvedSrv, resolvedCount, gotToken := countingBranchServer(t)
	defaultSrv, defaultCount, _ := countingBranchServer(t)

	var gotRef string
	f := forgegitlab.New(defaultSrv.URL, staticToken{},
		forgegitlab.WithHTTPClient(resolvedSrv.Client()),
		forgegitlab.WithResolveBaseURL(func(_ context.Context, ref string) (string, error) {
			gotRef = ref
			return resolvedSrv.URL, nil
		}),
	)

	sha, ok, err := f.GetBranchSHA(context.Background(), gitlabScope("42"), forge.RepoRef{}, "main")
	if err != nil {
		t.Fatalf("GetBranchSHA: %v", err)
	}
	if !ok || sha != "deadbeef" {
		t.Fatalf("GetBranchSHA = (%q, %v), want (deadbeef, true)", sha, ok)
	}
	if gotRef != "gitlab:42" {
		t.Errorf("resolver got installation ref = %q, want %q", gotRef, "gitlab:42")
	}
	if atomic.LoadInt64(resolvedCount) != 1 {
		t.Errorf("resolved host received %d requests, want 1", atomic.LoadInt64(resolvedCount))
	}
	if atomic.LoadInt64(defaultCount) != 0 {
		t.Errorf("default host received %d requests, want 0 (routed to resolved host)", atomic.LoadInt64(defaultCount))
	}
	if *gotToken != "glpat-test" {
		t.Errorf("resolved host saw PRIVATE-TOKEN = %q, want glpat-test", *gotToken)
	}
}

// TestForge_PerInstallation_FailClosed pins the per-mode fail-closed contract
// (#2094 binding condition 1): a resolver DB fault, a disallowed host
// (allowlist miss), and a bad scheme (http / relative) each make a scope-taking
// forge op return an error, construct NO client, and issue NO request — the
// token never ships.
func TestForge_PerInstallation_FailClosed(t *testing.T) {
	sentinel := errors.New("boom: db fault")
	for _, tc := range []struct {
		name      string
		allowlist []string
		resolve   func(context.Context, string) (string, error)
		wantWrap  error
	}{
		{
			name:     "resolver db fault",
			resolve:  func(context.Context, string) (string, error) { return "", sentinel },
			wantWrap: sentinel,
		},
		{
			name:      "disallowed host (allowlist miss)",
			allowlist: []string{"allowed.example.com"},
			resolve:   func(context.Context, string) (string, error) { return "https://evil.example.com", nil },
		},
		{
			name:    "bad scheme (http)",
			resolve: func(context.Context, string) (string, error) { return "http://insecure.example.com", nil },
		},
		{
			name:    "bad scheme (relative)",
			resolve: func(context.Context, string) (string, error) { return "/api/v4", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, count, gotToken := countingBranchServer(t)
			f := forgegitlab.New(srv.URL, staticToken{},
				forgegitlab.WithHTTPClient(srv.Client()),
				forgegitlab.WithResolveBaseURL(tc.resolve),
				forgegitlab.WithAllowedInstallationHosts(tc.allowlist),
			)

			_, _, err := f.GetBranchSHA(context.Background(), gitlabScope("42"), forge.RepoRef{}, "main")
			if err == nil {
				t.Fatal("GetBranchSHA succeeded, want a fail-closed error")
			}
			if tc.wantWrap != nil && !errors.Is(err, tc.wantWrap) {
				t.Errorf("err = %v, want it to wrap the resolver error", err)
			}
			if atomic.LoadInt64(count) != 0 {
				t.Errorf("server received %d requests, want 0 (fail-closed, no request issued)", atomic.LoadInt64(count))
			}
			if *gotToken != "" {
				t.Errorf("server saw PRIVATE-TOKEN = %q, want empty (token must never ship on fail-closed)", *gotToken)
			}
		})
	}
}

// TestForge_PerInstallation_BackwardCompat_NilResolver pins the deployment-
// default posture: with NO resolver wired, a scope-taking forge op targets the
// deployment-default host byte-identical to Mode 1 (no per-installation
// routing, no resolver consulted because none exists).
func TestForge_PerInstallation_BackwardCompat_NilResolver(t *testing.T) {
	defaultSrv, defaultCount, gotToken := countingBranchServer(t)
	f := forgegitlab.New(defaultSrv.URL, staticToken{},
		forgegitlab.WithHTTPClient(defaultSrv.Client()))

	sha, ok, err := f.GetBranchSHA(context.Background(), gitlabScope("42"), forge.RepoRef{}, "main")
	if err != nil {
		t.Fatalf("GetBranchSHA: %v", err)
	}
	if !ok || sha != "deadbeef" {
		t.Fatalf("GetBranchSHA = (%q, %v), want (deadbeef, true)", sha, ok)
	}
	if atomic.LoadInt64(defaultCount) != 1 {
		t.Errorf("default host received %d requests, want 1 (nil resolver -> deployment default)", atomic.LoadInt64(defaultCount))
	}
	if *gotToken != "glpat-test" {
		t.Errorf("default host saw PRIVATE-TOKEN = %q, want glpat-test", *gotToken)
	}
}

// --- forge.IssueOperations (E50.17 / #2900) -----------------------------

// issueCall is one request the issue mux recorded: method, path, query and
// the decoded JSON body (nil for a bodiless GET).
type issueCall struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// issueForge is a *forgegitlab.Forge over an httptest GitLab whose mux the
// caller populates; every request is recorded in arrival order so a case
// asserts the exact wire shape the adapter produced.
type issueForge struct {
	f   *forgegitlab.Forge
	srv *httptest.Server
	mux *http.ServeMux
	mu  sync.Mutex
	log []issueCall
}

func newIssueForge(t *testing.T) *issueForge {
	t.Helper()
	a := &issueForge{mux: http.NewServeMux()}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := issueCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.Body)
		}
		a.mu.Lock()
		a.log = append(a.log, call)
		a.mu.Unlock()
		a.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(a.srv.Close)
	a.f = forgegitlab.New(a.srv.URL, staticToken{}, forgegitlab.WithHTTPClient(a.srv.Client()))
	return a
}

func (a *issueForge) calls() []issueCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]issueCall, len(a.log))
	copy(out, a.log)
	return out
}

// TestGitLabForge_FetchIssue_NormalizesState is the named counterfactual
// vehicle for the state normalization: GitLab's native "opened" must land
// as the forge-neutral "open", "closed" stays "closed", and an unknown
// native word passes through. Deleting normalizeIssueState reddens the
// "opened" row. It also pins the named gap: StateReason is EMPTY because
// GitLab's issue object carries no such field.
func TestGitLabForge_FetchIssue_NormalizesState(t *testing.T) {
	for _, tc := range []struct{ native, want string }{
		{"opened", "open"},
		{"closed", "closed"},
		{"locked", "locked"},
	} {
		t.Run(tc.native, func(t *testing.T) {
			a := newIssueForge(t)
			a.mux.HandleFunc("GET /api/v4/projects/5/issues/7", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{"iid":7,"title":"Parent","description":"text","state":"`+tc.native+`","labels":["type:epic"],"web_url":"https://gitlab.example/g/sub/p/-/issues/7"}`)
			})
			var ops forge.IssueOperations = a.f
			is, err := ops.FetchIssue(context.Background(), gitlabScope("5"), forge.RepoRef{Owner: "g/sub", Name: "p"}, 7)
			if err != nil {
				t.Fatalf("FetchIssue: %v", err)
			}
			if is.State != tc.want {
				t.Errorf("State = %q, want %q (native %q)", is.State, tc.want, tc.native)
			}
			if is.StateReason != "" {
				t.Errorf("StateReason = %q, want empty: GitLab carries no state_reason and the adapter must not fabricate one", is.StateReason)
			}
			if is.Number != 7 || is.Title != "Parent" || is.Body != "text" || len(is.Labels) != 1 || is.Labels[0] != "type:epic" {
				t.Errorf("Issue = %+v, want iid 7 / Parent / text / [type:epic]", *is)
			}
			// web_url is carried as the forge-neutral browse URL (E45.42 /
			// #3347) so the prompt handler never fabricates a github.com
			// link for a GitLab issue.
			if is.HTMLURL != "https://gitlab.example/g/sub/p/-/issues/7" {
				t.Errorf("HTMLURL = %q, want the payload's web_url", is.HTMLURL)
			}
			calls := a.calls()
			if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/api/v4/projects/5/issues/7" {
				t.Errorf("calls = %+v, want one GET /api/v4/projects/5/issues/7 (addressed by the scope's project id, not RepoRef)", calls)
			}
		})
	}
}

// TestGitLabForge_FetchIssueComments_MapsNotes pins the note→IssueComment
// mapping through the client's Link-header walk: both pages land in order,
// a system note is INCLUDED (not filtered), and a marker body round-trips
// byte-intact.
func TestGitLabForge_FetchIssueComments_MapsNotes(t *testing.T) {
	a := newIssueForge(t)
	a.mux.HandleFunc("GET /api/v4/projects/5/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSON(w, http.StatusOK, `[{"id":3,"body":"<!-- fishhawk:key -->\nlinked","system":false,"created_at":"2026-09-01T00:02:00Z","author":{"username":"bot"}}]`)
			return
		}
		w.Header().Set("Link", `<`+a.srv.URL+`/api/v4/projects/5/issues/7/notes?page=2&per_page=100>; rel="next"`)
		writeJSON(w, http.StatusOK, `[
			{"id":1,"body":"first","system":false,"created_at":"2026-09-01T00:00:00Z","author":{"username":"alice"}},
			{"id":2,"body":"added ~type:epic label","system":true,"created_at":"2026-09-01T00:01:00Z","author":{"username":"alice"}}
		]`)
	})

	var ops forge.IssueOperations = a.f
	comments, err := ops.FetchIssueComments(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("len(comments) = %d, want 3 (both pages, system note included)", len(comments))
	}
	if comments[0].ID != 1 || comments[0].Author != "alice" || comments[0].Body != "first" || comments[0].CreatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("comments[0] = %+v", comments[0])
	}
	if comments[1].ID != 2 || comments[1].Body != "added ~type:epic label" {
		t.Errorf("comments[1] = %+v, want the system note surfaced", comments[1])
	}
	if comments[2].ID != 3 || comments[2].Author != "bot" || comments[2].Body != "<!-- fishhawk:key -->\nlinked" {
		t.Errorf("comments[2] = %+v, want the page-2 marker note byte-intact", comments[2])
	}
	if calls := a.calls(); len(calls) != 2 || !strings.Contains(calls[0].Query, "per_page=100") {
		t.Errorf("calls = %+v, want two paged GETs with per_page=100", calls)
	}
}

// TestGitLabForge_PostIssueComment pins the delegation to CreateIssueNote:
// POST .../issues/{iid}/notes carrying the body byte-intact.
func TestGitLabForge_PostIssueComment(t *testing.T) {
	a := newIssueForge(t)
	a.mux.HandleFunc("POST /api/v4/projects/5/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, `{"id":99,"body":"x","author":{"username":"bot"}}`)
	})
	var ops forge.IssueOperations = a.f
	if err := ops.PostIssueComment(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, "<!-- k -->\nhello"); err != nil {
		t.Fatalf("PostIssueComment: %v", err)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/api/v4/projects/5/issues/7/notes" {
		t.Fatalf("calls = %+v, want one POST .../issues/7/notes", calls)
	}
	if got := calls[0].Body["body"]; got != "<!-- k -->\nhello" {
		t.Errorf("body.body = %v, want the note text byte-intact", got)
	}
}

// TestGitLabForge_SetIssueState_TranslatesStateEvent pins the
// state→state_event translation: "closed" sends state_event "close",
// "open" sends "reopen", the body never carries a `state` key (GitLab
// refuses direct state writes), and StateReason is IGNORED — no
// state_reason key — per the interface's best-effort contract.
func TestGitLabForge_SetIssueState_TranslatesStateEvent(t *testing.T) {
	for _, tc := range []struct{ state, wantEvent string }{
		{"closed", "close"},
		{"open", "reopen"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			a := newIssueForge(t)
			a.mux.HandleFunc("PUT /api/v4/projects/5/issues/7", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{"iid":7,"state":"closed"}`)
			})
			state, reason := tc.state, "completed"
			var ops forge.IssueOperations = a.f
			if err := ops.SetIssueState(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, forge.IssueStateUpdate{State: &state, StateReason: &reason}); err != nil {
				t.Fatalf("SetIssueState: %v", err)
			}
			calls := a.calls()
			if len(calls) != 1 || calls[0].Method != http.MethodPut || calls[0].Path != "/api/v4/projects/5/issues/7" {
				t.Fatalf("calls = %+v, want one PUT /api/v4/projects/5/issues/7", calls)
			}
			body := calls[0].Body
			if got := body["state_event"]; got != tc.wantEvent {
				t.Errorf("body.state_event = %v, want %q", got, tc.wantEvent)
			}
			if v, has := body["state"]; has {
				t.Errorf("body carries state = %v; GitLab changes state only through state_event", v)
			}
			if v, has := body["state_reason"]; has {
				t.Errorf("body carries state_reason = %v; GitLab has no state_reason and the adapter must drop it, not send it", v)
			}
		})
	}
}

// TestGitLabForge_SetIssueState_RefusesBeforeHTTP pins the local
// refusal: a nil State and an unknown State are both forge.ErrValidation
// with ZERO HTTP calls. Deleting the guard lets the unknown-state case reach
// the wire with an empty state_event (which the client then refuses with a
// plain error) — the errors.Is assertion is what reddens.
func TestGitLabForge_SetIssueState_RefusesBeforeHTTP(t *testing.T) {
	bogus := "resolved"
	for _, tc := range []struct {
		name string
		u    forge.IssueStateUpdate
	}{
		{"nil state", forge.IssueStateUpdate{}},
		{"unknown state", forge.IssueStateUpdate{State: &bogus}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newIssueForge(t)
			a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{"iid":7}`)
			})
			err := a.f.SetIssueState(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, tc.u)
			if !errors.Is(err, forge.ErrValidation) {
				t.Errorf("err = %v, want forge.ErrValidation", err)
			}
			if n := len(a.calls()); n != 0 {
				t.Errorf("refusal made %d HTTP calls, want 0", n)
			}
		})
	}
}

// issueOpsCalls enumerates the four capability methods so the scope and
// error-mapping cases below run each one.
func issueOpsCalls() []struct {
	name string
	call func(ops forge.IssueOperations, scope forge.CredentialScope) error
} {
	closed := "closed"
	return []struct {
		name string
		call func(ops forge.IssueOperations, scope forge.CredentialScope) error
	}{
		{"FetchIssue", func(ops forge.IssueOperations, scope forge.CredentialScope) error {
			_, err := ops.FetchIssue(context.Background(), scope, forge.RepoRef{}, 7)
			return err
		}},
		{"FetchIssueComments", func(ops forge.IssueOperations, scope forge.CredentialScope) error {
			_, err := ops.FetchIssueComments(context.Background(), scope, forge.RepoRef{}, 7)
			return err
		}},
		{"PostIssueComment", func(ops forge.IssueOperations, scope forge.CredentialScope) error {
			return ops.PostIssueComment(context.Background(), scope, forge.RepoRef{}, 7, "x")
		}},
		{"SetIssueState", func(ops forge.IssueOperations, scope forge.CredentialScope) error {
			return ops.SetIssueState(context.Background(), scope, forge.RepoRef{}, 7, forge.IssueStateUpdate{State: &closed})
		}},
	}
}

// TestGitLabForge_IssueOperations_NonGitLabScopeRejected pins that every
// capability method fails closed on a non-gitlab-shaped scope (a GitHub
// installation id) BEFORE any HTTP call.
func TestGitLabForge_IssueOperations_NonGitLabScopeRejected(t *testing.T) {
	for _, tc := range issueOpsCalls() {
		t.Run(tc.name, func(t *testing.T) {
			a := newIssueForge(t)
			a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{}`)
			})
			err := tc.call(a.f, forge.FromGitHubInstallationID(12345))
			if err == nil || !strings.Contains(err.Error(), "not gitlab-shaped") {
				t.Errorf("err = %v, want a not-gitlab-shaped rejection", err)
			}
			if n := len(a.calls()); n != 0 {
				t.Errorf("a wrong-forge scope reached the wire (%d calls), want 0", n)
			}
		})
	}
}

// TestGitLabForge_IssueOperations_ErrorMapping pins the base mapper on
// every capability method: 404 → forge.ErrNotFound, 403 → forge.ErrForbidden.
func TestGitLabForge_IssueOperations_ErrorMapping(t *testing.T) {
	for _, tc := range issueOpsCalls() {
		for _, sc := range []struct {
			status int
			want   error
		}{
			{http.StatusNotFound, forge.ErrNotFound},
			{http.StatusForbidden, forge.ErrForbidden},
		} {
			t.Run(tc.name+"/"+http.StatusText(sc.status), func(t *testing.T) {
				a := newIssueForge(t)
				a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, sc.status, `{"message":"x"}`)
				})
				if err := tc.call(a.f, gitlabScope("5")); !errors.Is(err, sc.want) {
					t.Errorf("err = %v, want errors.Is %v", err, sc.want)
				}
			})
		}
	}
}

// --- forge.IssueCommentEditor (E45.52 / #3481) --------------------------

// TestGitLabForge_PostIssueCommentWithID_ReturnsNoteID pins that the
// with-id post delegates to CreateIssueNote (POST .../issues/7/notes, body
// byte-intact) AND surfaces the note id GitLab assigned — the value the
// notifier audits as github_comment_id and later edits by.
func TestGitLabForge_PostIssueCommentWithID_ReturnsNoteID(t *testing.T) {
	a := newIssueForge(t)
	a.mux.HandleFunc("POST /api/v4/projects/5/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusCreated, `{"id":99,"body":"x","author":{"username":"bot"}}`)
	})
	var ed forge.IssueCommentEditor = a.f
	id, err := ed.PostIssueCommentWithID(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, "<!-- k -->\nhello")
	if err != nil {
		t.Fatalf("PostIssueCommentWithID: %v", err)
	}
	if id != 99 {
		t.Errorf("id = %d, want the note id 99 GitLab returned", id)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/api/v4/projects/5/issues/7/notes" {
		t.Fatalf("calls = %+v, want one POST .../issues/7/notes", calls)
	}
	if got := calls[0].Body["body"]; got != "<!-- k -->\nhello" {
		t.Errorf("body.body = %v, want the note text byte-intact", got)
	}
}

// TestGitLabForge_EditIssueComment_SendsPut pins the exact edit route the
// operator's approval condition (1) names — PUT
// /api/v4/projects/5/issues/7/notes/99, i.e. the project id from the
// SCOPE, the issue iid from the number argument and the note id in the
// path — carrying the new body byte-intact and nothing else.
func TestGitLabForge_EditIssueComment_SendsPut(t *testing.T) {
	a := newIssueForge(t)
	a.mux.HandleFunc("PUT /api/v4/projects/5/issues/7/notes/99", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"id":99,"body":"edited","author":{"username":"bot"}}`)
	})
	var ed forge.IssueCommentEditor = a.f
	if err := ed.EditIssueComment(context.Background(), gitlabScope("5"), forge.RepoRef{Owner: "group/sub", Name: "proj"}, 7, 99, "<!-- k -->\nedited"); err != nil {
		t.Fatalf("EditIssueComment: %v", err)
	}
	calls := a.calls()
	if len(calls) != 1 || calls[0].Method != http.MethodPut || calls[0].Path != "/api/v4/projects/5/issues/7/notes/99" {
		t.Fatalf("calls = %+v, want exactly one PUT /api/v4/projects/5/issues/7/notes/99", calls)
	}
	if got := calls[0].Body["body"]; got != "<!-- k -->\nedited" {
		t.Errorf("body.body = %v, want the edited text byte-intact", got)
	}
	if len(calls[0].Body) != 1 {
		t.Errorf("body = %v, want ONLY the body key", calls[0].Body)
	}
}

// TestGitLabForge_EditIssueComment_NotFoundMapsToForgeErrNotFound pins the
// deleted-note arm: a 404 from the PUT must be errors.Is forge.ErrNotFound,
// because the notifier's deleted-comment fallback keys on that sentinel to
// create a fresh anchor. Deleting mapError on the edit path reddens it (the
// raw *gitlabclient.APIError is not the forge sentinel). The original
// status stays matchable through the errors.Join.
func TestGitLabForge_EditIssueComment_NotFoundMapsToForgeErrNotFound(t *testing.T) {
	a := newIssueForge(t)
	a.mux.HandleFunc("PUT /api/v4/projects/5/issues/7/notes/99", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"message":"404 Note Not Found"}`)
	})
	var ed forge.IssueCommentEditor = a.f
	err := ed.EditIssueComment(context.Background(), gitlabScope("5"), forge.RepoRef{}, 7, 99, "x")
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is forge.ErrNotFound", err)
	}
	var apiErr *gitlabclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want the original 404 *gitlabclient.APIError still matchable", err)
	}
}

// TestGitLabForge_IssueCommentEditor_ErrorMapping pins the base mapper on
// both editor methods for the non-404 arm too (403 → ErrForbidden), and
// the 404 arm on the with-id post.
func TestGitLabForge_IssueCommentEditor_ErrorMapping(t *testing.T) {
	for _, tc := range issueCommentEditorCalls() {
		for _, sc := range []struct {
			status int
			want   error
		}{
			{http.StatusNotFound, forge.ErrNotFound},
			{http.StatusForbidden, forge.ErrForbidden},
		} {
			t.Run(tc.name+"/"+http.StatusText(sc.status), func(t *testing.T) {
				a := newIssueForge(t)
				a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, sc.status, `{"message":"x"}`)
				})
				if err := tc.call(a.f, gitlabScope("5")); !errors.Is(err, sc.want) {
					t.Errorf("err = %v, want errors.Is %v", err, sc.want)
				}
			})
		}
	}
}

// issueCommentEditorCalls enumerates the two editor methods so the scope
// and error-mapping cases run each one.
func issueCommentEditorCalls() []struct {
	name string
	call func(ed forge.IssueCommentEditor, scope forge.CredentialScope) error
} {
	return []struct {
		name string
		call func(ed forge.IssueCommentEditor, scope forge.CredentialScope) error
	}{
		{"PostIssueCommentWithID", func(ed forge.IssueCommentEditor, scope forge.CredentialScope) error {
			_, err := ed.PostIssueCommentWithID(context.Background(), scope, forge.RepoRef{}, 7, "x")
			return err
		}},
		{"EditIssueComment", func(ed forge.IssueCommentEditor, scope forge.CredentialScope) error {
			return ed.EditIssueComment(context.Background(), scope, forge.RepoRef{}, 7, 99, "x")
		}},
	}
}

// TestGitLabForge_IssueCommentEditor_NonGitLabScopeRejected pins that both
// editor methods fail closed on a non-gitlab-shaped scope (a GitHub
// installation id) BEFORE any HTTP call — a wrong forge's scope can never
// address, let alone rewrite, a GitLab note.
func TestGitLabForge_IssueCommentEditor_NonGitLabScopeRejected(t *testing.T) {
	for _, tc := range issueCommentEditorCalls() {
		t.Run(tc.name, func(t *testing.T) {
			a := newIssueForge(t)
			a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, `{"id":99}`)
			})
			err := tc.call(a.f, forge.FromGitHubInstallationID(12345))
			if err == nil || !strings.Contains(err.Error(), "not gitlab-shaped") {
				t.Errorf("err = %v, want a not-gitlab-shaped rejection", err)
			}
			if n := len(a.calls()); n != 0 {
				t.Errorf("a wrong-forge scope reached the wire (%d calls), want 0", n)
			}
		})
	}
}

// --- forge.CIRequirementReader (E45.55 / #3490) -------------------------

// TestGitLabForge_ReadCIRequirement pins the adapter's mapping of the two
// GET /projects/:id merge-requirement settings onto forge.CIRequirement, that
// the request addresses the SCOPE's project id (not anything derived from
// the RepoRef), and that a 403 maps to forge.ErrForbidden — the arm the
// dispatcher's capture step degrades to a nil snapshot on.
func TestGitLabForge_ReadCIRequirement(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    forge.CIRequirement
		wantErr error
	}{
		{
			name:   "both on",
			status: http.StatusOK,
			body:   `{"id":77,"only_allow_merge_if_pipeline_succeeds":true,"allow_merge_on_skipped_pipeline":true}`,
			want:   forge.CIRequirement{PipelineMustSucceed: true, AllowSkippedPipeline: true},
		},
		{
			name:   "succeeds on, skipped off",
			status: http.StatusOK,
			body:   `{"id":77,"only_allow_merge_if_pipeline_succeeds":true,"allow_merge_on_skipped_pipeline":false}`,
			want:   forge.CIRequirement{PipelineMustSucceed: true, AllowSkippedPipeline: false},
		},
		{
			name:   "succeeds off",
			status: http.StatusOK,
			body:   `{"id":77,"only_allow_merge_if_pipeline_succeeds":false,"allow_merge_on_skipped_pipeline":true}`,
			want:   forge.CIRequirement{PipelineMustSucceed: false, AllowSkippedPipeline: true},
		},
		{
			name:    "403 → ErrForbidden",
			status:  http.StatusForbidden,
			body:    `{"message":"403 Forbidden"}`,
			wantErr: forge.ErrForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			var gotPath string
			mux.HandleFunc("/api/v4/projects/", func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				writeJSON(w, tc.status, tc.body)
			})
			f, _ := newForge(t, mux)
			// The RepoRef deliberately names a DIFFERENT project than the
			// scope: the request must address the scope's id.
			got, err := f.ReadCIRequirement(context.Background(), gitlabScope("77"),
				forge.RepoRef{Owner: "other", Name: "project"})
			if gotPath != "/api/v4/projects/77" {
				t.Errorf("request path = %q, want /api/v4/projects/77 (the scope's project id)", gotPath)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("requirement = %+v on error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadCIRequirement: %v", err)
			}
			if *got != tc.want {
				t.Errorf("requirement = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// TestGitLabForge_ReadCIRequirement_RejectsNonGitLabScope pins the
// fail-closed scope parse: a GitHub-shaped scope never reaches the wire.
func TestGitLabForge_ReadCIRequirement_RejectsNonGitLabScope(t *testing.T) {
	mux := http.NewServeMux()
	var calls atomic.Int32
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, `{}`)
	})
	f, _ := newForge(t, mux)
	_, err := f.ReadCIRequirement(context.Background(), forge.FromGitHubInstallationID(12345), forge.RepoRef{})
	if err == nil || !strings.Contains(err.Error(), "not gitlab-shaped") {
		t.Errorf("err = %v, want a not-gitlab-shaped rejection", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a wrong-forge scope reached the wire (%d calls), want 0", n)
	}
}

// --- forge.MergeProtectionReader (E45.66 / #3580) -----------------------

// mergeProtectionStub is a v4 stub routing the two reads ReadMergeProtection
// makes on project 77 — GET /projects/77 and GET /projects/77/protected_branches
// — each with a programmable status + body, counting the list requests so a
// test can pin that the project read comes FIRST.
type mergeProtectionStub struct {
	projectStatus int
	projectBody   string
	listStatus    int
	listBody      string
	listCalls     atomic.Int32
	projectCalls  atomic.Int32
}

const (
	mpProjectBody = `{"id":77,"default_branch":"main","path_with_namespace":"g/p",` +
		`"only_allow_merge_if_pipeline_succeeds":true,"allow_merge_on_skipped_pipeline":false,` +
		`"only_allow_merge_if_all_discussions_are_resolved":true}`
	mpExactMainRule = `{"id":1,"name":"main",` +
		`"push_access_levels":[{"access_level":0,"access_level_description":"No one"}],` +
		`"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],` +
		`"allow_force_push":false}`
)

func newMergeProtectionForge(t *testing.T, st *mergeProtectionStub) *forgegitlab.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/77", func(w http.ResponseWriter, r *http.Request) {
		st.projectCalls.Add(1)
		writeJSON(w, st.projectStatus, st.projectBody)
	})
	mux.HandleFunc("GET /api/v4/projects/77/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		st.listCalls.Add(1)
		writeJSON(w, st.listStatus, st.listBody)
	})
	f, _ := newForge(t, mux)
	return f
}

func okMergeProtectionStub(listBody string) *mergeProtectionStub {
	return &mergeProtectionStub{
		projectStatus: http.StatusOK, projectBody: mpProjectBody,
		listStatus: http.StatusOK, listBody: listBody,
	}
}

func accessLevelsEqual(got, want []forge.AccessLevel) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestGitLabForge_ReadMergeProtection_ExactRule pins the full mapping of an
// exact-name rule plus the three project settings onto forge.MergeProtection.
func TestGitLabForge_ReadMergeProtection_ExactRule(t *testing.T) {
	st := okMergeProtectionStub(`[` + mpExactMainRule + `]`)
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{Owner: "other", Name: "project"}, "main")
	if err != nil {
		t.Fatalf("ReadMergeProtection: %v", err)
	}
	if got.Branch != "main" || !got.Protected {
		t.Errorf("branch/protected = %q/%v, want main/true", got.Branch, got.Protected)
	}
	if len(got.MatchedRules) != 1 || got.MatchedRules[0] != "main" {
		t.Errorf("MatchedRules = %v, want [main]", got.MatchedRules)
	}
	if got.AllowForcePush {
		t.Error("AllowForcePush = true, want false")
	}
	if !accessLevelsEqual(got.PushAccessLevels, []forge.AccessLevel{{Level: 0, Description: "No one"}}) {
		t.Errorf("PushAccessLevels = %+v, want [{0 No one}]", got.PushAccessLevels)
	}
	if !accessLevelsEqual(got.MergeAccessLevels, []forge.AccessLevel{{Level: 40, Description: "Maintainers"}}) {
		t.Errorf("MergeAccessLevels = %+v, want [{40 Maintainers}]", got.MergeAccessLevels)
	}
	if !got.PipelineMustSucceed || got.AllowSkippedPipeline || !got.DiscussionsMustBeResolved {
		t.Errorf("project settings = must_succeed %v / skipped %v / discussions %v, want true/false/true",
			got.PipelineMustSucceed, got.AllowSkippedPipeline, got.DiscussionsMustBeResolved)
	}
	if n := st.listCalls.Load(); n != 1 {
		t.Errorf("list requests = %d, want 1", n)
	}
}

// TestGitLabForge_ReadMergeProtection_WildcardRule is the counterfactual
// vehicle for the glob arm of protectedBranchRules: with only wildcard rules
// present, branch main is covered by "ma*" and by nothing else. Deleting the
// glob arm reads main as unprotected → RED.
func TestGitLabForge_ReadMergeProtection_WildcardRule(t *testing.T) {
	st := okMergeProtectionStub(`[
		{"id":1,"name":"release-*","push_access_levels":[],"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"allow_force_push":false},
		{"id":2,"name":"ma*","push_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"allow_force_push":false}
	]`)
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
	if err != nil {
		t.Fatalf("ReadMergeProtection: %v", err)
	}
	if !got.Protected {
		t.Fatal("Protected = false, want true (the ma* wildcard covers main)")
	}
	if len(got.MatchedRules) != 1 || got.MatchedRules[0] != "ma*" {
		t.Errorf("MatchedRules = %v, want [ma*] (release-* must not match)", got.MatchedRules)
	}
	if !accessLevelsEqual(got.MergeAccessLevels, []forge.AccessLevel{{Level: 30, Description: "Developers + Maintainers"}}) {
		t.Errorf("MergeAccessLevels = %+v, want the ma* rule's [{30 Developers + Maintainers}]", got.MergeAccessLevels)
	}
}

// TestGitLabForge_ReadMergeProtection_OverlappingRules_MostPermissive pins
// GitLab's most-permissive semantics (approval condition 1): when both an
// exact rule and a wildcard cover the branch, EVERY matching rule is
// reported (exact first), the access levels are the union sorted ascending
// (lowest = most permissive first) and AllowForcePush is the OR. Restoring a
// first-match-wins pick drops "m*", loses level 30 and reads force-push as
// false → RED.
func TestGitLabForge_ReadMergeProtection_OverlappingRules_MostPermissive(t *testing.T) {
	st := okMergeProtectionStub(`[
		{"id":2,"name":"m*","push_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"allow_force_push":true},
		` + mpExactMainRule + `
	]`)
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
	if err != nil {
		t.Fatalf("ReadMergeProtection: %v", err)
	}
	if !got.Protected {
		t.Fatal("Protected = false, want true")
	}
	if len(got.MatchedRules) != 2 || got.MatchedRules[0] != "main" || got.MatchedRules[1] != "m*" {
		t.Errorf("MatchedRules = %v, want [main m*] (exact first, then wildcards, regardless of API order)", got.MatchedRules)
	}
	if !accessLevelsEqual(got.MergeAccessLevels, []forge.AccessLevel{
		{Level: 30, Description: "Developers + Maintainers"}, {Level: 40, Description: "Maintainers"},
	}) {
		t.Errorf("MergeAccessLevels = %+v, want the union [30 40] ascending", got.MergeAccessLevels)
	}
	if !accessLevelsEqual(got.PushAccessLevels, []forge.AccessLevel{
		{Level: 0, Description: "No one"}, {Level: 30, Description: "Developers + Maintainers"},
	}) {
		t.Errorf("PushAccessLevels = %+v, want the union [0 30] ascending", got.PushAccessLevels)
	}
	if !got.AllowForcePush {
		t.Error("AllowForcePush = false, want true (OR across matching rules: m* allows it)")
	}

	// The OR must hold whichever matched rule carries the true: with the
	// EXACT rule allowing force-push and the LAST-matched wildcard denying
	// it, a last-write-wins fold would read false. This is what makes the
	// `||` discriminable from a plain assignment.
	t.Run("or is not last-write-wins", func(t *testing.T) {
		st := okMergeProtectionStub(`[
			{"id":1,"name":"main","push_access_levels":[],"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"allow_force_push":true},
			{"id":2,"name":"m*","push_access_levels":[],"merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"allow_force_push":false}
		]`)
		f := newMergeProtectionForge(t, st)
		got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
		if err != nil {
			t.Fatalf("ReadMergeProtection: %v", err)
		}
		if !got.AllowForcePush {
			t.Error("AllowForcePush = false, want true (the exact rule allows it; the OR must not be overwritten by the later wildcard)")
		}
	})
}

// TestGitLabForge_ReadMergeProtection_NoRule_Unprotected pins that an
// AUTHORITATIVE empty rule list means unprotected — with the project settings
// still mapped and no rule/level/force-push data fabricated.
func TestGitLabForge_ReadMergeProtection_NoRule_Unprotected(t *testing.T) {
	st := okMergeProtectionStub(`[]`)
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
	if err != nil {
		t.Fatalf("ReadMergeProtection: %v", err)
	}
	if got.Protected || len(got.MatchedRules) != 0 || got.AllowForcePush || len(got.PushAccessLevels) != 0 || len(got.MergeAccessLevels) != 0 {
		t.Errorf("unprotected result = %+v, want Protected false with no rules/levels/force-push", got)
	}
	if !got.PipelineMustSucceed || !got.DiscussionsMustBeResolved {
		t.Errorf("project settings not mapped on the unprotected path: %+v", got)
	}
}

// TestGitLabForge_ReadMergeProtection_DefaultBranchResolved pins the empty-
// branch contract: "" resolves to the project's REAL default branch and the
// result carries the resolved name; an empty default branch is ErrNotFound,
// and the rule list is never read for it.
func TestGitLabForge_ReadMergeProtection_DefaultBranchResolved(t *testing.T) {
	t.Run("resolved from project body", func(t *testing.T) {
		st := okMergeProtectionStub(`[{"id":9,"name":"trunk","push_access_levels":[],"merge_access_levels":[],"allow_force_push":false}]`)
		st.projectBody = `{"id":77,"default_branch":"trunk","only_allow_merge_if_pipeline_succeeds":true}`
		f := newMergeProtectionForge(t, st)
		got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "")
		if err != nil {
			t.Fatalf("ReadMergeProtection: %v", err)
		}
		if got.Branch != "trunk" {
			t.Errorf("Branch = %q, want trunk (resolved default branch)", got.Branch)
		}
		if !got.Protected || len(got.MatchedRules) != 1 || got.MatchedRules[0] != "trunk" {
			t.Errorf("matched = %v / %v, want the trunk rule", got.Protected, got.MatchedRules)
		}
	})
	t.Run("empty default branch is ErrNotFound", func(t *testing.T) {
		st := okMergeProtectionStub(`[]`)
		st.projectBody = `{"id":77,"default_branch":""}`
		f := newMergeProtectionForge(t, st)
		got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "")
		if !errors.Is(err, forge.ErrNotFound) {
			t.Fatalf("err = %v, want errors.Is ErrNotFound", err)
		}
		if got != nil {
			t.Errorf("result = %+v on error, want nil", got)
		}
		if n := st.listCalls.Load(); n != 0 {
			t.Errorf("list requests = %d, want 0 (no branch to match)", n)
		}
	})
}

// TestGitLabForge_ReadMergeProtection_ProjectForbidden is the counterfactual
// vehicle for the project-first read order: a 403 on the project read maps
// to ErrForbidden and the list endpoint sees ZERO requests. Swapping the two
// reads makes the stub observe a list call → RED.
func TestGitLabForge_ReadMergeProtection_ProjectForbidden(t *testing.T) {
	st := okMergeProtectionStub(`[` + mpExactMainRule + `]`)
	st.projectStatus, st.projectBody = http.StatusForbidden, `{"message":"403 Forbidden"}`
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
	if !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("err = %v, want errors.Is ErrForbidden", err)
	}
	if got != nil {
		t.Errorf("result = %+v on error, want nil", got)
	}
	if n := st.listCalls.Load(); n != 0 {
		t.Errorf("list requests = %d, want 0 (the project read must come first and fail closed)", n)
	}
}

// TestGitLabForge_ReadMergeProtection_ListForbidden pins the Maintainer-role
// residual: a credential that can read the project but not its protection
// draws ErrForbidden, never an "unprotected" verdict.
func TestGitLabForge_ReadMergeProtection_ListForbidden(t *testing.T) {
	st := okMergeProtectionStub(`{"message":"403 Forbidden"}`)
	st.listStatus = http.StatusForbidden
	f := newMergeProtectionForge(t, st)
	got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, "main")
	if !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("err = %v, want errors.Is ErrForbidden", err)
	}
	if got != nil {
		t.Errorf("result = %+v on error, want nil (a failed list read is never 'unprotected')", got)
	}
	if n := st.projectCalls.Load(); n != 1 {
		t.Errorf("project requests = %d, want 1", n)
	}
}

// TestGitLabForge_ReadMergeProtection_RejectsNonGitLabScope pins the
// fail-closed scope parse: a GitHub-shaped scope never reaches the wire.
func TestGitLabForge_ReadMergeProtection_RejectsNonGitLabScope(t *testing.T) {
	mux := http.NewServeMux()
	var calls atomic.Int32
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, `{}`)
	})
	f, _ := newForge(t, mux)
	_, err := f.ReadMergeProtection(context.Background(), forge.FromGitHubInstallationID(12345), forge.RepoRef{}, "main")
	if err == nil || !strings.Contains(err.Error(), "not gitlab-shaped") {
		t.Errorf("err = %v, want a not-gitlab-shaped rejection", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a wrong-forge scope reached the wire (%d calls), want 0", n)
	}
}

// TestProtectedBranchRule is the rule-matching table (exact, `*` leading /
// mid / trailing, `*` spanning `/`, multiple `*`, no match, empty list),
// driven through ReadMergeProtection because the matcher is unexported and
// this package is an external test. Each row lists rule names, a branch, and
// the matched-rule names expected (exact first, then wildcards in API order).
func TestProtectedBranchRule(t *testing.T) {
	cases := []struct {
		name   string
		rules  []string
		branch string
		want   []string
	}{
		{"exact", []string{"main"}, "main", []string{"main"}},
		{"exact is not a prefix match", []string{"main"}, "main2", nil},
		{"trailing star", []string{"release-*"}, "release-1.2", []string{"release-*"}},
		{"trailing star needs the prefix", []string{"release-*"}, "hotfix-1", nil},
		{"leading star", []string{"*-stable"}, "v2-stable", []string{"*-stable"}},
		{"mid star", []string{"rel*se"}, "release", []string{"rel*se"}},
		{"mid star respects the suffix", []string{"rel*se"}, "release-x", nil},
		{"star spans a slash", []string{"release/*"}, "release/2026/09", []string{"release/*"}},
		{"bare star matches everything", []string{"*"}, "anything/at/all", []string{"*"}},
		{"multiple stars in order", []string{"a*b*c"}, "aXbYc", []string{"a*b*c"}},
		{"multiple stars out of order", []string{"a*b*c"}, "acb", nil},
		{"exact first then wildcards in API order", []string{"m*", "*n", "main"}, "main", []string{"main", "m*", "*n"}},
		{"no match", []string{"develop", "release-*"}, "main", nil},
		{"empty rules", nil, "main", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			sb.WriteString("[")
			for i, name := range tc.rules {
				if i > 0 {
					sb.WriteString(",")
				}
				raw, _ := json.Marshal(name)
				sb.WriteString(`{"id":` + strconv.Itoa(i+1) + `,"name":` + string(raw) + `,"push_access_levels":[],"merge_access_levels":[],"allow_force_push":false}`)
			}
			sb.WriteString("]")
			f := newMergeProtectionForge(t, okMergeProtectionStub(sb.String()))
			got, err := f.ReadMergeProtection(context.Background(), gitlabScope("77"), forge.RepoRef{}, tc.branch)
			if err != nil {
				t.Fatalf("ReadMergeProtection: %v", err)
			}
			if got.Protected != (len(tc.want) > 0) {
				t.Errorf("Protected = %v, want %v", got.Protected, len(tc.want) > 0)
			}
			if strings.Join(got.MatchedRules, ",") != strings.Join(tc.want, ",") {
				t.Errorf("MatchedRules = %v, want %v", got.MatchedRules, tc.want)
			}
		})
	}
}
