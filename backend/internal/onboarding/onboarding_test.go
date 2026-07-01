package onboarding

import (
	"context"
	"errors"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- ScaffoldFiles content tests ---

func TestScaffoldFiles_Content(t *testing.T) {
	files, err := ScaffoldFiles(spec.PresetMedium)
	if err != nil {
		t.Fatalf("ScaffoldFiles: %v", err)
	}

	// All four paths present.
	for _, p := range []string{specPath, agentsPath, claudePath, workflowPath} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing scaffold file %q", p)
		}
	}

	// workflows.yaml is a schema-valid workflow spec.
	if _, err := spec.ParseBytes(files[specPath]); err != nil {
		t.Errorf("scaffolded %s does not validate: %v", specPath, err)
	}

	// AGENTS.md carries the Fishhawk managed block markers.
	agents := string(files[agentsPath])
	if !strings.Contains(agents, bridge.BeginMarker) || !strings.Contains(agents, bridge.EndMarker) {
		t.Errorf("AGENTS.md missing managed-block markers")
	}

	// CLAUDE.md imports AGENTS.md.
	claude := string(files[claudePath])
	if !strings.Contains(claude, bridge.ImportLine) {
		t.Errorf("CLAUDE.md missing import line %q", bridge.ImportLine)
	}

	// fishhawk.yml is valid YAML and references the published runner action.
	wf := files[workflowPath]
	var parsed map[string]any
	if err := yaml.Unmarshal(wf, &parsed); err != nil {
		t.Errorf("fishhawk.yml does not parse as YAML: %v", err)
	}
	if !strings.Contains(string(wf), "kuhlman-labs/fishhawk/runner@") {
		t.Errorf("fishhawk.yml does not reference the published runner action")
	}
	// It must reference the PUBLISHED action, not the local ./runner path.
	if strings.Contains(string(wf), "uses: ./runner") {
		t.Errorf("fishhawk.yml references the local ./runner path, not the published action")
	}
}

func TestScaffoldFiles_UnknownPreset(t *testing.T) {
	_, err := ScaffoldFiles(spec.Preset("nonsense"))
	if err == nil {
		t.Fatal("want error for unknown preset, got nil")
	}
}

// --- fakeClient: records the call sequence for Scaffolder tests ---

type fakeClient struct {
	calls []string

	getFileResult *githubclient.FileContent
	getFileErr    error

	repoDefaultBranch string
	getRepoErr        error

	baseBranchSHA    string
	baseBranchExists bool
	baseBranchErr    error

	// onboardingExists controls the SECOND GetBranchSHA (the onboarding
	// branch probe). getBranchCall counts invocations so the two probes
	// return different answers.
	onboardingExists bool
	getBranchCall    int

	commitTreeSHA string
	getCommitErr  error

	createTreeSHA string
	createTreeErr error
	baseTreeSeen  string
	entriesSeen   []githubclient.TreeEntry

	createCommitSHA string
	createCommitErr error
	commitParents   []string
	commitTree      string

	createRefErr      error
	createRefBranch   string
	createRefSHA      string
	forceUpdateErr    error
	forceUpdateBranch string
	forceUpdateSHA    string

	createPRErr  error
	createPRHead string
	createPRBase string
	createPRURL  string
}

func (f *fakeClient) GetFile(_ context.Context, _ int64, _ githubclient.RepoRef, _, _ string) (*githubclient.FileContent, error) {
	f.calls = append(f.calls, "GetFile")
	return f.getFileResult, f.getFileErr
}

func (f *fakeClient) GetRepository(_ context.Context, _ int64, _ githubclient.RepoRef) (*githubclient.Repository, error) {
	f.calls = append(f.calls, "GetRepository")
	if f.getRepoErr != nil {
		return nil, f.getRepoErr
	}
	return &githubclient.Repository{DefaultBranch: f.repoDefaultBranch}, nil
}

func (f *fakeClient) GetBranchSHA(_ context.Context, _ int64, _ githubclient.RepoRef, branch string) (string, bool, error) {
	f.calls = append(f.calls, "GetBranchSHA:"+branch)
	f.getBranchCall++
	if branch == OnboardingBranch {
		return "", f.onboardingExists, nil
	}
	// Base (default) branch probe.
	return f.baseBranchSHA, f.baseBranchExists, f.baseBranchErr
}

func (f *fakeClient) GetCommit(_ context.Context, _ int64, _ githubclient.RepoRef, _ string) (*githubclient.GitCommit, error) {
	f.calls = append(f.calls, "GetCommit")
	if f.getCommitErr != nil {
		return nil, f.getCommitErr
	}
	return &githubclient.GitCommit{SHA: f.baseBranchSHA, TreeSHA: f.commitTreeSHA}, nil
}

func (f *fakeClient) CreateTree(_ context.Context, _ int64, _ githubclient.RepoRef, baseTree string, entries []githubclient.TreeEntry) (string, error) {
	f.calls = append(f.calls, "CreateTree")
	f.baseTreeSeen = baseTree
	f.entriesSeen = entries
	return f.createTreeSHA, f.createTreeErr
}

func (f *fakeClient) CreateCommit(_ context.Context, _ int64, _ githubclient.RepoRef, _, treeSHA string, parents []string) (string, error) {
	f.calls = append(f.calls, "CreateCommit")
	f.commitTree = treeSHA
	f.commitParents = parents
	return f.createCommitSHA, f.createCommitErr
}

func (f *fakeClient) CreateRef(_ context.Context, _ int64, _ githubclient.RepoRef, branch, sha string) error {
	f.calls = append(f.calls, "CreateRef")
	f.createRefBranch = branch
	f.createRefSHA = sha
	return f.createRefErr
}

func (f *fakeClient) ForceUpdateRef(_ context.Context, _ int64, _ githubclient.RepoRef, branch, newSHA string) error {
	f.calls = append(f.calls, "ForceUpdateRef")
	f.forceUpdateBranch = branch
	f.forceUpdateSHA = newSHA
	return f.forceUpdateErr
}

func (f *fakeClient) CreatePullRequest(_ context.Context, _ int64, _ githubclient.RepoRef, head, base, _, _ string) (*githubclient.PullRequest, error) {
	f.calls = append(f.calls, "CreatePullRequest")
	f.createPRHead = head
	f.createPRBase = base
	if f.createPRErr != nil {
		return nil, f.createPRErr
	}
	return &githubclient.PullRequest{HTMLURL: f.createPRURL}, nil
}

// happyClient returns a fake wired for the full create path (spec absent,
// base branch present, onboarding branch absent, PR opens).
func happyClient() *fakeClient {
	return &fakeClient{
		getFileErr:        githubclient.ErrNotFound, // not onboarded yet
		repoDefaultBranch: "main",
		baseBranchSHA:     "basecommit",
		baseBranchExists:  true,
		onboardingExists:  false,
		commitTreeSHA:     "basetree",
		createTreeSHA:     "newtree",
		createCommitSHA:   "newcommit",
		createPRURL:       "https://github.com/x/y/pull/1",
	}
}

func TestOpenScaffoldPR_HappyPath(t *testing.T) {
	f := happyClient()
	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("OpenScaffoldPR: %v", err)
	}
	if res.Skipped {
		t.Fatalf("unexpected skip: %+v", res)
	}
	if res.PullRequestURL != "https://github.com/x/y/pull/1" {
		t.Errorf("PullRequestURL = %q", res.PullRequestURL)
	}
	if res.RefForceUpdated {
		t.Errorf("RefForceUpdated = true, want false on the create path")
	}

	// Call sequence: probe → repo → base branch → base commit → tree →
	// commit → onboarding-branch probe → create ref → open PR.
	want := []string{
		"GetFile", "GetRepository", "GetBranchSHA:main", "GetCommit",
		"CreateTree", "CreateCommit", "GetBranchSHA:" + OnboardingBranch,
		"CreateRef", "CreatePullRequest",
	}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Errorf("call sequence =\n  %v\nwant\n  %v", f.calls, want)
	}

	// The tree is built on the base tree and carries all four scaffold paths.
	if f.baseTreeSeen != "basetree" {
		t.Errorf("base_tree = %q, want basetree (a tree sha, not a commit sha)", f.baseTreeSeen)
	}
	gotPaths := map[string]bool{}
	for _, e := range f.entriesSeen {
		gotPaths[e.Path] = true
	}
	for _, p := range []string{specPath, agentsPath, claudePath, workflowPath} {
		if !gotPaths[p] {
			t.Errorf("tree entry missing %q", p)
		}
	}
	// The commit parents onto the base commit; ref/PR use the new commit.
	if len(f.commitParents) != 1 || f.commitParents[0] != "basecommit" {
		t.Errorf("commit parents = %v, want [basecommit]", f.commitParents)
	}
	if f.commitTree != "newtree" {
		t.Errorf("commit tree = %q, want newtree", f.commitTree)
	}
	if f.createRefSHA != "newcommit" {
		t.Errorf("ref sha = %q, want newcommit", f.createRefSHA)
	}
	if f.createPRHead != OnboardingBranch || f.createPRBase != "main" {
		t.Errorf("PR head/base = %q/%q, want %q/main", f.createPRHead, f.createPRBase, OnboardingBranch)
	}
}

func TestOpenScaffoldPR_AlreadyOnboardedSkips(t *testing.T) {
	f := happyClient()
	f.getFileErr = nil
	f.getFileResult = &githubclient.FileContent{Path: specPath, Content: []byte("version: 1.0\n")}

	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("OpenScaffoldPR: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped = false, want true when already onboarded")
	}
	// Only the probe ran — no writes.
	if strings.Join(f.calls, ",") != "GetFile" {
		t.Errorf("calls = %v, want only GetFile", f.calls)
	}
}

func TestOpenScaffoldPR_PRAlreadyExistsIsIdempotent(t *testing.T) {
	f := happyClient()
	f.createPRErr = githubclient.ErrPullRequestExists

	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("OpenScaffoldPR: %v", err)
	}
	if !res.PRAlreadyExisted {
		t.Errorf("PRAlreadyExisted = false, want true")
	}
	if res.Skipped {
		t.Errorf("Skipped = true, want false (the create path ran)")
	}
}

func TestOpenScaffoldPR_RefAlreadyExistsForceUpdates(t *testing.T) {
	f := happyClient()
	f.onboardingExists = true // a prior attempt left the branch behind

	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("OpenScaffoldPR: %v", err)
	}
	if !res.RefForceUpdated {
		t.Errorf("RefForceUpdated = false, want true when the onboarding ref already exists")
	}
	// Force-update targets the onboarding branch at the FRESH commit — not a
	// bare no-op, so the existing PR reflects the newly-generated scaffold.
	if f.forceUpdateBranch != OnboardingBranch || f.forceUpdateSHA != "newcommit" {
		t.Errorf("force-update branch/sha = %q/%q, want %q/newcommit",
			f.forceUpdateBranch, f.forceUpdateSHA, OnboardingBranch)
	}
	// CreateRef must NOT run on the already-exists path.
	for _, c := range f.calls {
		if c == "CreateRef" {
			t.Errorf("CreateRef ran on the ref-already-exists path; want ForceUpdateRef only")
		}
	}
}

func TestOpenScaffoldPR_ForbiddenReadSurfacesNoBranch(t *testing.T) {
	f := happyClient()
	f.getRepoErr = githubclient.ErrForbidden

	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err == nil || !errors.Is(err, githubclient.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if res != nil {
		t.Errorf("res = %+v, want nil on error", res)
	}
	// No branch/PR side effects.
	for _, c := range f.calls {
		if c == "CreateRef" || c == "ForceUpdateRef" || c == "CreatePullRequest" || c == "CreateTree" {
			t.Errorf("unexpected write %q after a forbidden pre-commit read", c)
		}
	}
}

func TestOpenScaffoldPR_EmptyRepoSkips(t *testing.T) {
	f := happyClient()
	f.baseBranchExists = false // empty repo: default branch has no commit

	s := NewScaffolder(f)
	res, err := s.OpenScaffoldPR(context.Background(), 42, githubclient.RepoRef{Owner: "x", Name: "y"})
	if err != nil {
		t.Fatalf("OpenScaffoldPR: %v", err)
	}
	if !res.Skipped {
		t.Errorf("Skipped = false, want true for an empty repo")
	}
	for _, c := range f.calls {
		if c == "CreateTree" || c == "CreatePullRequest" {
			t.Errorf("unexpected write %q for an empty repo", c)
		}
	}
}
