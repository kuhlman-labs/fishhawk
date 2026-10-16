package opsmonitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

type fakeIssueClient struct {
	mu        sync.Mutex
	instID    int64
	instErr   error
	postErr   error
	posted    []string
	scopes    []forge.CredentialScope
	repos     []forge.RepoRef
	issueNums []int
}

func (f *fakeIssueClient) GetRepoInstallation(context.Context, forge.RepoRef) (int64, error) {
	return f.instID, f.instErr
}

func (f *fakeIssueClient) CreateIssueComment(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, n int, body string) (*githubclient.IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes = append(f.scopes, scope)
	f.repos = append(f.repos, repo)
	f.issueNums = append(f.issueNums, n)
	f.posted = append(f.posted, body)
	if f.postErr != nil {
		return nil, f.postErr
	}
	return &githubclient.IssueComment{}, nil
}

// Compile-time check: the production client satisfies the narrow pair.
var _ IssueCommentClient = (*githubclient.Client)(nil)

func TestParseIssueRef(t *testing.T) {
	repo, n, err := ParseIssueRef("kuhlman-labs/fishhawk#4142")
	if err != nil || repo.Owner != "kuhlman-labs" || repo.Name != "fishhawk" || n != 4142 {
		t.Fatalf("got %+v %d %v", repo, n, err)
	}
	for _, bad := range []string{
		"kuhlman-labs/fishhawk",     // missing '#'
		"kuhlman-labs/fishhawk#abc", // non-numeric
		"kuhlman-labs/fishhawk#0",   // zero
		"kuhlman-labs/fishhawk#-3",  // negative
		"kuhlman-labs/fish/hawk#1",  // extra '/'
		"kuhlman-labs/fishhawk#1#2", // extra '#'
		"/fishhawk#1",               // empty owner
		"kuhlman-labs/#1",           // empty name
		" kuhlman-labs/fishhawk#1",  // padded
		"bogus",                     // no shape at all
	} {
		if _, _, err := ParseIssueRef(bad); err == nil || !strings.Contains(err.Error(), "owner/name#N") {
			t.Errorf("ParseIssueRef(%q) err = %v, want a shape error", bad, err)
		}
	}
}

func TestRenderComment_CarriesFactsNotDetail(t *testing.T) {
	r := Result{
		Probe: "disk", Condition: ConditionDiskFreeLow, Status: StatusFailing,
		Observed: "5.0 GiB free of 100.0 GiB", Threshold: ">= 20.0 GiB free", Remedy: remedies[ConditionDiskFreeLow],
		Detail: map[string]any{"path": "/Users/secret-user/fishhawk-data"},
	}
	body := RenderComment(r, DeploymentLocal, time.Hour)
	for _, want := range []string{
		"<!-- fishhawk-ops-monitor:v1 condition=disk_free_low -->",
		"Fishhawk ops monitor: disk_free_low",
		"5.0 GiB free of 100.0 GiB", ">= 20.0 GiB free", remedies[ConditionDiskFreeLow], "local", "1h0m0s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-user") {
		t.Errorf("comment leaked a Detail path:\n%s", body)
	}
	if strings.Contains(body, "Subject") {
		t.Errorf("empty subject rendered:\n%s", body)
	}
	if !strings.Contains(RenderComment(Result{Condition: ConditionOrphanedRunner, Subject: "1/a/b"}, DeploymentLocal, time.Hour), "**Subject:** 1/a/b") {
		t.Error("non-empty subject not rendered")
	}
}

func TestIssueCommentNotifier_ScopeResolution(t *testing.T) {
	repo := forge.RepoRef{Owner: "o", Name: "n"}
	r := Result{Condition: ConditionDiskFreeLow}
	t.Run("installed", func(t *testing.T) {
		c := &fakeIssueClient{instID: 77}
		if err := (IssueCommentNotifier{Client: c, Repo: repo, Issue: 5}).Notify(context.Background(), r, DeploymentLocal); err != nil {
			t.Fatal(err)
		}
		if len(c.posted) != 1 || c.scopes[0] != forge.FromGitHubInstallationID(77) || c.repos[0] != repo || c.issueNums[0] != 5 {
			t.Errorf("posted %d, scope %+v, repo %+v, issue %v", len(c.posted), c.scopes, c.repos, c.issueNums)
		}
		if !strings.Contains(c.posted[0], "suppressed for 1h0m0s") {
			t.Errorf("default quiet window not rendered: %s", c.posted[0])
		}
	})
	t.Run("not installed posts with the zero scope", func(t *testing.T) {
		c := &fakeIssueClient{instErr: forge.ErrNotInstalled}
		if err := (IssueCommentNotifier{Client: c, Repo: repo, Issue: 5, QuietWindow: 2 * time.Hour}).Notify(context.Background(), r, DeploymentLocal); err != nil {
			t.Fatal(err)
		}
		if len(c.posted) != 1 || c.scopes[0] != (forge.CredentialScope{}) {
			t.Errorf("posted %d, scope %+v; want zero scope", len(c.posted), c.scopes)
		}
	})
	t.Run("other error returned without posting", func(t *testing.T) {
		c := &fakeIssueClient{instErr: errors.New("github 502")}
		err := (IssueCommentNotifier{Client: c, Repo: repo, Issue: 5}).Notify(context.Background(), r, DeploymentLocal)
		if err == nil || !strings.Contains(err.Error(), "github 502") || len(c.posted) != 0 {
			t.Errorf("err %v, posted %d; want the error and no post", err, len(c.posted))
		}
	})
	t.Run("post error returned", func(t *testing.T) {
		c := &fakeIssueClient{instID: 1, postErr: errors.New("403 forbidden")}
		err := (IssueCommentNotifier{Client: c, Repo: repo, Issue: 5}).Notify(context.Background(), r, DeploymentLocal)
		if err == nil || !strings.Contains(err.Error(), "403 forbidden") {
			t.Errorf("err = %v, want the post error", err)
		}
	})
	t.Run("nil client", func(t *testing.T) {
		if err := (IssueCommentNotifier{Repo: repo, Issue: 5}).Notify(context.Background(), r, DeploymentLocal); err == nil {
			t.Error("nil client returned nil error")
		}
	})
}

// TestNoRawErrorTextReachesCommentOrAudit is approval condition (7): a DSN
// host and a socket path planted in the probe errors reach neither the
// rendered comment nor the audit payload's observed/reason.
func TestNoRawErrorTextReachesCommentOrAudit(t *testing.T) {
	const dsnHost = "db.secret-host.internal"
	const socketPath = "/Users/secret-user/.docker/run/docker.sock"
	probes := []Probe{
		PostgresProbe{Ping: func(context.Context) error {
			return errors.New("failed to connect to `host=" + dsnHost + " user=fishhawk database=fishhawk`: dial error")
		}},
		PostgresProbe{Ping: func(context.Context) error {
			return errors.New("postgres://fishhawk:pw@" + dsnHost + ":5432/fishhawk: read: input/output error")
		}},
		DockerProbe{Exec: shExec("echo 'Cannot connect to the Docker daemon at unix://" + socketPath + ". Is the docker daemon running?' >&2; exit 1")},
		DockerProbe{Exec: shExec("echo 'open " + socketPath + ": input/output error' >&2; exit 1")},
	}
	client := &fakeIssueClient{instID: 1}
	au := &fakeAudit{}
	h, clk := &captureHandler{}, newClock()
	tk := newTicker(probes, au, IssueCommentNotifier{Client: client, Repo: forge.RepoRef{Owner: "o", Name: "n"}, Issue: 1}, clk, h)
	res := tk.Tick(context.Background())

	conds := map[string]bool{}
	for _, r := range res {
		if r.Status != StatusFailing {
			t.Fatalf("result = %+v, want failing", r)
		}
		conds[r.Condition] = true
	}
	for _, c := range []string{ConditionPostgresUnreachable, ConditionDockerUnreachable, ConditionStorageEIO} {
		if !conds[c] {
			t.Errorf("condition %s not exercised (got %v)", c, conds)
		}
	}
	if len(client.posted) == 0 || au.appendCount() == 0 {
		t.Fatalf("posted %d, appends %d; want both paths exercised", len(client.posted), au.appendCount())
	}
	for _, body := range client.posted {
		if strings.Contains(body, dsnHost) || strings.Contains(body, socketPath) {
			t.Errorf("comment leaked raw error text:\n%s", body)
		}
	}
	for i := 0; i < au.appendCount(); i++ {
		p := au.payload(t, i)
		for _, field := range []string{"observed", "reason"} {
			s, _ := p[field].(string)
			if strings.Contains(s, dsnHost) || strings.Contains(s, socketPath) {
				t.Errorf("audit payload %s leaked raw error text: %q", field, s)
			}
		}
	}
}
