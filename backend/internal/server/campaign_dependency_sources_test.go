package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// The cross-boundary test for #4028: the REAL workmgmt/github provider, over
// an in-test github.API fake that also implements IssueBlockedBy, registered
// as the campaign provider and driven through POST /v0/campaigns/preview and
// POST /v0/campaigns. It pins the whole chain — native read → provider edge
// building (native first, every legacy line) → campaign assembly → HTTP shape —
// rather than each layer against a hand-built EpicChildrenResult.

// nativeDepsGHAPI implements github.API plus the optional IssueBlockedBy read.
// The provider's issue fetches run on a bounded concurrent pool, so every
// piece of bookkeeping is mutex-guarded (the #3226 trap).
type nativeDepsGHAPI struct {
	mu        sync.Mutex
	issues    map[int]*githubclient.Issue
	blockedBy map[int][]githubclient.BlockingIssueRef
	failFor   map[int]error
	subIssues []githubclient.SubIssue
	nativeHit map[int]int
}

func (f *nativeDepsGHAPI) CreateIssue(context.Context, forge.CredentialScope, githubclient.RepoRef, githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error) {
	return nil, errors.New("nativeDepsGHAPI: CreateIssue not used")
}

func (f *nativeDepsGHAPI) IssueNodeID(context.Context, forge.CredentialScope, githubclient.RepoRef, int) (string, error) {
	return "EPIC_NODE", nil
}

func (f *nativeDepsGHAPI) ProjectFields(context.Context, forge.CredentialScope, githubclient.ProjectCoord, string) (*githubclient.ProjectMeta, error) {
	return nil, errors.New("nativeDepsGHAPI: ProjectFields not used")
}

func (f *nativeDepsGHAPI) ProjectItemStatus(context.Context, forge.CredentialScope, string, string, string) (*githubclient.ProjectItemStatus, error) {
	return nil, errors.New("nativeDepsGHAPI: ProjectItemStatus not used")
}

func (f *nativeDepsGHAPI) AddProjectItem(context.Context, forge.CredentialScope, string, string) (string, error) {
	return "", errors.New("nativeDepsGHAPI: AddProjectItem not used")
}

func (f *nativeDepsGHAPI) SetProjectItemSingleSelect(context.Context, forge.CredentialScope, string, string, string, string) error {
	return errors.New("nativeDepsGHAPI: SetProjectItemSingleSelect not used")
}

func (f *nativeDepsGHAPI) AddSubIssue(context.Context, forge.CredentialScope, string, string) error {
	return errors.New("nativeDepsGHAPI: AddSubIssue not used")
}

func (f *nativeDepsGHAPI) ListSubIssues(context.Context, forge.CredentialScope, string) ([]githubclient.SubIssue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]githubclient.SubIssue(nil), f.subIssues...), nil
}

func (f *nativeDepsGHAPI) SearchIssuesByTitle(context.Context, forge.CredentialScope, string) ([]githubclient.IssueTitleResult, error) {
	return nil, nil
}

func (f *nativeDepsGHAPI) GetIssue(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, number int) (*githubclient.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issues[number]
	if !ok {
		return nil, githubclient.ErrNotFound
	}
	cp := *iss
	return &cp, nil
}

func (f *nativeDepsGHAPI) ListRepoIssues(context.Context, forge.CredentialScope, githubclient.RepoRef, githubclient.ListRepoIssuesOptions) ([]githubclient.RepoIssue, error) {
	return nil, nil
}

func (f *nativeDepsGHAPI) UpdateIssue(context.Context, forge.CredentialScope, githubclient.RepoRef, int, githubclient.UpdateIssueParams) (*githubclient.Issue, error) {
	return nil, errors.New("nativeDepsGHAPI: UpdateIssue not used")
}

func (f *nativeDepsGHAPI) ProjectsTokenConfigured() bool { return false }

func (f *nativeDepsGHAPI) IssueBlockedBy(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, number int) ([]githubclient.BlockingIssueRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nativeHit == nil {
		f.nativeHit = map[int]int{}
	}
	f.nativeHit[number]++
	if err := f.failFor[number]; err != nil {
		return nil, err
	}
	return append([]githubclient.BlockingIssueRef(nil), f.blockedBy[number]...), nil
}

func (f *nativeDepsGHAPI) nativeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.nativeHit {
		n += c
	}
	return n
}

// The fixture: #1 has no dependency; #2 depends on #1 ONLY through the native
// "blocked by" relation (no text); #3's body carries TWO line-start
// `Depends on:` lines (#1, then #2) and no native relation. The second legacy
// line is the only source of 3->2, and the native relation the only source of
// 2->1.
const threeBody = "Summary\n\nDepends on: #1\n\nmore text\nDepends on: #2"

func newNativeDepsGHAPI() *nativeDepsGHAPI {
	return &nativeDepsGHAPI{
		issues: map[int]*githubclient.Issue{
			1: {Number: 1, Title: "one", Body: "no deps", State: "open"},
			2: {Number: 2, Title: "two", Body: "natively blocked", State: "open"},
			3: {Number: 3, Title: "three", Body: threeBody, State: "open"},
		},
		blockedBy: map[int][]githubclient.BlockingIssueRef{
			2: {{Number: 1, Repository: "kuhlman-labs/fishhawk"}},
		},
		subIssues: []githubclient.SubIssue{
			{Number: 1, NodeID: "N1", Title: "one", Body: "no deps", State: "OPEN"},
			{Number: 2, NodeID: "N2", Title: "two", Body: "natively blocked", State: "OPEN"},
			{Number: 3, NodeID: "N3", Title: "three", Body: threeBody, State: "OPEN"},
		},
	}
}

// registerRealGitHubProvider registers the REAL github provider over api as
// the campaign provider, with a GitHub client whose installation lookup
// resolves so the provider receives a non-zero scope.
func registerRealGitHubProvider(t *testing.T, api *nativeDepsGHAPI) *Server {
	t.Helper()
	workmgmt.Register(workmgmtgithub.New(api))
	return New(Config{
		CampaignRepo: newFakeCampaignRepo(),
		GitHub:       recordingInstallGitHubClient(t, 7788, &installRecorder{}),
	})
}

func TestCampaignDependencySources_RealProvider(t *testing.T) {
	if workmgmt.Default().Provider != workmgmtgithub.ProviderName {
		t.Fatalf("default provider = %q, want %q: this test registers the real github provider under the default id", workmgmt.Default().Provider, workmgmtgithub.ProviderName)
	}
	wantEdges := []campaignPreviewEdge{
		{From: "issue:2", To: "issue:1", Source: "native"},
		{From: "issue:3", To: "issue:1", Source: "legacy_text"},
		{From: "issue:3", To: "issue:2", Source: "legacy_text"},
	}
	wantWaves := [][]string{{"issue:1"}, {"issue:2"}, {"issue:3"}}
	assertWarnings := func(t *testing.T, got []campaignDependencyWarning) {
		t.Helper()
		if len(got) != 1 || got[0].IssueRef != "issue:3" || got[0].Code != "legacy_text_dependency" ||
			!reflect.DeepEqual(got[0].Targets, []string{"issue:1", "issue:2"}) {
			t.Errorf("warnings = %+v, want exactly issue:3 legacy_text_dependency [issue:1 issue:2]", got)
		}
	}

	sources := []struct {
		name string
		body string
	}{
		{"items", `{"repo":"kuhlman-labs/fishhawk","items":["issue:1","issue:2","issue:3"]}`},
		{"epic_ref", `{"repo":"kuhlman-labs/fishhawk","epic_ref":"issue:99"}`},
	}
	for _, src := range sources {
		t.Run(src.name+"/preview", func(t *testing.T) {
			api := newNativeDepsGHAPI()
			s := registerRealGitHubProvider(t, api)
			w := postPreview(t, s, src.body)
			rep := decodePreview(t, w)
			if !rep.Valid {
				t.Fatalf("valid = false (body=%s)", w.Body.String())
			}
			if !reflect.DeepEqual(rep.Edges, wantEdges) {
				t.Errorf("edges = %+v, want %+v", rep.Edges, wantEdges)
			}
			if !reflect.DeepEqual(rep.Waves, wantWaves) {
				t.Errorf("waves = %v, want %v", rep.Waves, wantWaves)
			}
			assertWarnings(t, rep.Warnings)
			if api.nativeCalls() == 0 {
				t.Error("the campaign path made no native IssueBlockedBy read")
			}
		})
		t.Run(src.name+"/create", func(t *testing.T) {
			api := newNativeDepsGHAPI()
			s := registerRealGitHubProvider(t, api)
			w := postCampaign(t, s, src.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
			}
			var resp campaignResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertWarnings(t, resp.Warnings)
			// The persisted items carry the same in-set edges the preview did.
			got := map[string][]string{}
			for _, it := range persistedItems(t, s) {
				deps := append([]string{}, it.DependsOn...)
				sort.Strings(deps)
				got[it.IssueRef] = deps
			}
			want := map[string][]string{"issue:1": {}, "issue:2": {"issue:1"}, "issue:3": {"issue:1", "issue:2"}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("persisted depends_on = %v, want %v", got, want)
			}
		})
	}

	// (iv) A native read failure for #2 refuses BOTH verbs on BOTH sources as
	// 502 native_dependency_read_failed naming issue:2 — while the SHARED
	// EpicChildren on the very same fake still succeeds with zero native reads,
	// so refinement filing resume, rolling-walk adoption and idempotency
	// adoption cannot be reached by it.
	for _, src := range sources {
		for _, verb := range []struct {
			name string
			post func(*testing.T, *Server, string) *httptest.ResponseRecorder
		}{{"preview", postPreview}, {"create", postCampaign}} {
			t.Run(src.name+"/"+verb.name+"/native_read_fails", func(t *testing.T) {
				api := newNativeDepsGHAPI()
				api.failFor = map[int]error{2: githubclient.ErrForbidden}
				s := registerRealGitHubProvider(t, api)
				w := verb.post(t, s, src.body)
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (body=%s)", w.Code, w.Body.String())
				}
				eb := decodeErrorBody(t, w)
				if eb.Code != "native_dependency_read_failed" || eb.Details["issue_ref"] != "issue:2" || eb.Details["reason"] != "forbidden" {
					t.Errorf("refusal = %s %v, want native_dependency_read_failed issue:2 forbidden", eb.Code, eb.Details)
				}
			})
		}
	}
	t.Run("shared_EpicChildren_unaffected", func(t *testing.T) {
		api := newNativeDepsGHAPI()
		api.failFor = map[int]error{2: githubclient.ErrForbidden}
		p := workmgmtgithub.New(api)
		res, err := p.EpicChildren(context.Background(), workmgmt.EpicChildrenRequest{
			Target: workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}, Scope: forge.FromGitHubInstallationID(7788)},
			Epic:   "issue:99",
		})
		if err != nil {
			t.Fatalf("EpicChildren on the failing fake: %v", err)
		}
		if len(res.Children) != 3 {
			t.Errorf("children = %d, want 3", len(res.Children))
		}
		if n := api.nativeCalls(); n != 0 {
			t.Errorf("EpicChildren made %d native reads, want 0", n)
		}
	})
}

// persistedItems reads back the items of the single campaign the server
// persisted.
func persistedItems(t *testing.T, s *Server) []campaignItemResponse {
	t.Helper()
	repo, ok := s.cfg.CampaignRepo.(*fakeCampaignRepo)
	if !ok {
		t.Fatalf("campaign repo is %T, want *fakeCampaignRepo", s.cfg.CampaignRepo)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.itemsByCmp) != 1 {
		t.Fatalf("persisted campaigns = %d, want 1", len(repo.itemsByCmp))
	}
	var out []campaignItemResponse
	for _, items := range repo.itemsByCmp {
		for _, it := range items {
			out = append(out, toCampaignItemResponse(it))
		}
	}
	return out
}
