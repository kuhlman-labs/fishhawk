package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// approverMembersSpec renders a minimal workflow spec whose one approval gate
// (feature_change/gate) declares members; nil members omits the key.
func approverMembersSpec(members []string) string {
	var b strings.Builder
	b.WriteString(`version: "1.0"
workflows:
  feature_change:
    stages:
      - id: gate
        type: acceptance
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
`)
	if members != nil {
		quoted := make([]string, len(members))
		for i, m := range members {
			quoted[i] = fmt.Sprintf("%q", m)
		}
		fmt.Fprintf(&b, "              members: [%s]\n", strings.Join(quoted, ", "))
	}
	return b.String()
}

func writeApproverMembersSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workflows.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// recordingPrincipalLoader is a fake approverPrincipalLoader that records
// every call's arguments and returns fixed rows (or err).
type recordingPrincipalLoader struct {
	rows  []approverPrincipalRow
	err   error
	calls int
	repo  string
	since time.Time
}

func (l *recordingPrincipalLoader) load(_ context.Context, _, repo string, since time.Time) ([]approverPrincipalRow, error) {
	l.calls++
	l.repo, l.since = repo, since
	return l.rows, l.err
}

var approverMembersNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return approverMembersNow }

// TestApproverMembers_FakeLoaderTable pins the report and exit code over the
// issue's distributions: the 30d pre-switch shape refuses brett@local-mcp
// (exit 1, the EXPECTED pre-switch refusal), the post-switch shape exits 0,
// a delegated row is evaluated against its on_behalf_of, and agent-kind
// principals are reported but never an impact.
func TestApproverMembers_FakeLoaderTable(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	listed := []string{"github:kuhlman-labs"}
	user := string(audit.ActorUser)
	agent := string(audit.ActorAgent)
	cases := []struct {
		name     string
		members  []string
		extra    []string
		rows     []approverPrincipalRow
		wantExit int
		want     []string
		notWant  []string
	}{
		{
			name:    "pre-switch 30d distribution refuses the static subject",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "brett@local-mcp", ActorKind: user, Approvals: 285},
				{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 4},
			},
			wantExit: exitFailure,
			want: []string{
				"principal=brett@local-mcp approvals=285 verdict=refused refusing_gates=feature_change/gate\n",
				"principal=github:kuhlman-labs approvals=4 verdict=admitted refusing_gates=-\n",
				"impact: refused=1 principals=2 approvals=289 refused_approvals=285 agent_rows_now_refused=0\n",
			},
		},
		{
			name:    "post-switch distribution admits everyone",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 4},
			},
			wantExit: exitOK,
			want:     []string{"impact: refused=0 principals=1 approvals=4 refused_approvals=0 agent_rows_now_refused=0\n"},
		},
		{
			name:    "delegated row is evaluated against on_behalf_of and aggregated with direct rows",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 4},
				{ActorSubject: "operator-agent/delegated", ActorKind: agent, OnBehalfOf: "github:kuhlman-labs", Delegated: true, Approvals: 2},
			},
			wantExit: exitOK,
			want: []string{
				"principal=github:kuhlman-labs approvals=6 verdict=admitted refusing_gates=-\n",
				"impact: refused=0 principals=1 approvals=6 refused_approvals=0 agent_rows_now_refused=0\n",
			},
			notWant: []string{"principal=operator-agent/delegated"},
		},
		{
			name:    "delegated row on behalf of an unlisted operator is refused",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "operator-agent/delegated", ActorKind: agent, OnBehalfOf: "brett@local-mcp", Delegated: true, Approvals: 3},
			},
			wantExit: exitFailure,
			want:     []string{"principal=brett@local-mcp approvals=3 verdict=refused refusing_gates=feature_change/gate\n"},
		},
		{
			name:    "pre-#2381 delegated human row with no on_behalf_of is evaluated as its actor",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "github:kuhlman-labs", ActorKind: user, Delegated: true, Approvals: 1},
			},
			wantExit: exitOK,
			want:     []string{"principal=github:kuhlman-labs approvals=1 verdict=admitted refusing_gates=-\n"},
		},
		{
			// C4: a NON-delegated agent-kind submission on a members gate flips
			// 200 -> 403; the inventory counts those rows but they stay out of
			// refused= and the exit code because agents never count.
			name:    "non-delegated agent rows are counted as agent_rows_now_refused, never refused",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "operator-agent/driver", ActorKind: agent, Approvals: 3},
				{ActorSubject: "operator-agent/delegated", ActorKind: agent, Delegated: true, Approvals: 2},
				{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 1},
			},
			wantExit: exitOK,
			want: []string{
				"principal=operator-agent/driver approvals=3 verdict=agent_never_counted refusing_gates=feature_change/gate\n",
				"principal=operator-agent/delegated approvals=2 verdict=agent_never_counted refusing_gates=feature_change/gate\n",
				"impact: refused=0 principals=1 approvals=1 refused_approvals=0 agent_rows_now_refused=5\n",
			},
		},
		{
			name:    "agent-kind actor column alone marks a prefixless principal as agent",
			members: listed,
			rows: []approverPrincipalRow{
				{ActorSubject: "legacy-bot", ActorKind: agent, Approvals: 2},
			},
			wantExit: exitOK,
			want:     []string{"impact: refused=0 principals=0 approvals=0 refused_approvals=0 agent_rows_now_refused=2\n"},
		},
		{
			name:     "plain member is qualified with --forge github",
			members:  []string{"kuhlman-labs"},
			extra:    []string{"--forge", "github"},
			rows:     []approverPrincipalRow{{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 4}},
			wantExit: exitOK,
			want:     []string{"principal=github:kuhlman-labs approvals=4 verdict=admitted refusing_gates=-\n"},
		},
		{
			name:     "plain member qualified with --forge gitlab refuses a github subject",
			members:  []string{"kuhlman-labs"},
			extra:    []string{"--forge", "gitlab"},
			rows:     []approverPrincipalRow{{ActorSubject: "github:kuhlman-labs", ActorKind: user, Approvals: 4}},
			wantExit: exitFailure,
			want:     []string{"principal=github:kuhlman-labs approvals=4 verdict=refused refusing_gates=feature_change/gate\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specPath := writeApproverMembersSpec(t, approverMembersSpec(tc.members))
			loader := &recordingPrincipalLoader{rows: tc.rows}
			var out, errs strings.Builder
			args := append([]string{"--spec", specPath, "--db", "postgres://unused"}, tc.extra...)
			got := runApproverMembersWith(args, &out, &errs, loader.load, fixedNow)
			if got != tc.wantExit {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, tc.wantExit, out.String(), errs.String())
			}
			if loader.calls != 1 {
				t.Errorf("loader calls = %d, want 1", loader.calls)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout missing %q:\n%s", w, out.String())
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(out.String(), nw) {
					t.Errorf("stdout unexpectedly contains %q:\n%s", nw, out.String())
				}
			}
			for _, note := range []string{"gate feature_change/gate members=[", "resolveReviewStageOnMerge", "BYPASSRLS"} {
				if !strings.Contains(out.String(), note) {
					t.Errorf("stdout header missing %q:\n%s", note, out.String())
				}
			}
		})
	}
}

// TestApproverMembers_SinceIsVerbatimBound pins that --since reaches the
// loader as the parsed instant itself, never recomputed from now (CF9), and
// that --repo is passed through.
func TestApproverMembers_SinceIsVerbatimBound(t *testing.T) {
	specPath := writeApproverMembersSpec(t, approverMembersSpec([]string{"github:kuhlman-labs"}))
	loader := &recordingPrincipalLoader{}
	var out, errs strings.Builder
	got := runApproverMembersWith([]string{"--spec", specPath, "--db", "postgres://unused", "--since", "2026-10-08T16:56:00Z", "--repo", "kuhlman-labs/fishhawk"},
		&out, &errs, loader.load, fixedNow)
	if got != exitOK {
		t.Fatalf("exit = %d, want 0\n%s\n%s", got, out.String(), errs.String())
	}
	want := time.Date(2026, 10, 8, 16, 56, 0, 0, time.UTC)
	if !loader.since.Equal(want) {
		t.Errorf("loader since = %s, want %s (verbatim --since)", loader.since, want)
	}
	if loader.repo != "kuhlman-labs/fishhawk" {
		t.Errorf("loader repo = %q, want kuhlman-labs/fishhawk", loader.repo)
	}
	if !strings.Contains(out.String(), "since=2026-10-08T16:56:00Z (--since)") {
		t.Errorf("header does not name the --since bound:\n%s", out.String())
	}
}

// TestApproverMembers_WindowBoundIsNowRelative pins the default and an
// explicit --window: the bound is now - window.
func TestApproverMembers_WindowBoundIsNowRelative(t *testing.T) {
	specPath := writeApproverMembersSpec(t, approverMembersSpec([]string{"github:kuhlman-labs"}))
	for _, tc := range []struct {
		args []string
		want time.Time
	}{
		{nil, approverMembersNow.Add(-30 * 24 * time.Hour)},
		{[]string{"--window", "7d"}, approverMembersNow.Add(-7 * 24 * time.Hour)},
	} {
		loader := &recordingPrincipalLoader{}
		var out, errs strings.Builder
		args := append([]string{"--spec", specPath, "--db", "postgres://unused"}, tc.args...)
		if got := runApproverMembersWith(args, &out, &errs, loader.load, fixedNow); got != exitOK {
			t.Fatalf("%v: exit = %d, want 0\n%s", tc.args, got, errs.String())
		}
		if !loader.since.Equal(tc.want) {
			t.Errorf("%v: loader since = %s, want %s", tc.args, loader.since, tc.want)
		}
		if loader.repo != "" {
			t.Errorf("%v: loader repo = %q, want all repositories", tc.args, loader.repo)
		}
	}
}

// TestApproverMembers_UsageErrors: each malformed invocation exits 2 naming
// the problem on stderr, and the loader is NEVER called (CF8 for the
// --window/--since row).
func TestApproverMembers_UsageErrors(t *testing.T) {
	t.Setenv("FISHHAWKD_DATABASE_URL", "")
	good := writeApproverMembersSpec(t, approverMembersSpec([]string{"github:kuhlman-labs"}))
	invalid := writeApproverMembersSpec(t, "version: \"1.0\"\nworkflows: {feature_change: {stages: [{id: x}]}}\n")
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	db := []string{"--db", "postgres://unused"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing --spec", db, "--spec is required"},
		{"unreadable spec", append([]string{"--spec", missing}, db...), "read --spec"},
		{"invalid spec", append([]string{"--spec", invalid}, db...), "parse --spec"},
		{"bad --window", append([]string{"--spec", good, "--window", "0d"}, db...), "--window"},
		{"missing --db", []string{"--spec", good}, "--db or FISHHAWKD_DATABASE_URL is required"},
		{"both --window and --since", append([]string{"--spec", good, "--window", "30d", "--since", "2026-10-08T16:56:00Z"}, db...), "mutually exclusive"},
		{"unparseable --since", append([]string{"--spec", good, "--since", "yesterday"}, db...), `--since "yesterday" is not an RFC3339 instant`},
		{"bad --forge", append([]string{"--spec", good, "--forge", "bitbucket"}, db...), `--forge "bitbucket"`},
		{"unexpected argument", append([]string{"--spec", good, "extra"}, db...), `unexpected argument "extra"`},
		{"unknown flag", []string{"--no-such-flag"}, "no-such-flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader := &recordingPrincipalLoader{}
			var out, errs strings.Builder
			if got := runApproverMembersWith(tc.args, &out, &errs, loader.load, fixedNow); got != exitUsage {
				t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, exitUsage, out.String(), errs.String())
			}
			if loader.calls != 0 {
				t.Errorf("loader called %d times on a usage error; want never", loader.calls)
			}
			if !strings.Contains(errs.String(), tc.want) {
				t.Errorf("stderr missing %q:\n%s", tc.want, errs.String())
			}
		})
	}
}

// TestApproverMembers_NoMembersGates: a spec with no members-declaring gate
// restricts nobody, so the command says so and exits 0 without reading rows.
func TestApproverMembers_NoMembersGates(t *testing.T) {
	for _, members := range [][]string{nil, {}} {
		specPath := writeApproverMembersSpec(t, approverMembersSpec(members))
		loader := &recordingPrincipalLoader{}
		var out, errs strings.Builder
		if got := runApproverMembersWith([]string{"--spec", specPath, "--db", "postgres://unused"}, &out, &errs, loader.load, fixedNow); got != exitOK {
			t.Fatalf("members=%v: exit = %d, want 0\n%s", members, got, errs.String())
		}
		if loader.calls != 0 {
			t.Errorf("members=%v: loader called %d times, want never", members, loader.calls)
		}
		if !strings.Contains(out.String(), "no approval gate declares approvals.members") {
			t.Errorf("members=%v: stdout missing the no-gates line:\n%s", members, out.String())
		}
	}
}

// TestApproverMembers_LoaderFailure: a database fault exits 1 (failure, not
// usage) and prints no impact line.
func TestApproverMembers_LoaderFailure(t *testing.T) {
	specPath := writeApproverMembersSpec(t, approverMembersSpec([]string{"github:kuhlman-labs"}))
	loader := &recordingPrincipalLoader{err: errors.New("boom")}
	var out, errs strings.Builder
	if got := runApproverMembersWith([]string{"--spec", specPath, "--db", "postgres://unused"}, &out, &errs, loader.load, fixedNow); got != exitFailure {
		t.Fatalf("exit = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(errs.String(), "boom") || strings.Contains(out.String(), "impact:") {
		t.Errorf("want the loader error on stderr and no impact line\nstdout:\n%s\nstderr:\n%s", out.String(), errs.String())
	}
}

// TestRun_ApproverMembersDispatch: the subcommand is reachable through run().
func TestRun_ApproverMembersDispatch(t *testing.T) {
	var errs strings.Builder
	if got := run([]string{"approver-members"}, &errs); got != exitUsage {
		t.Errorf("run(approver-members) = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(errs.String(), "--spec is required") {
		t.Errorf("stderr missing the --spec refusal: %s", errs.String())
	}
}

// TestApproverMembers_RepoSpecAtHEAD is the done-means check over the SHIPPED
// spec (read-only): every members gate admits the forge-verified operator
// identity and refuses the retired static MCP-token subject.
func TestApproverMembers_RepoSpecAtHEAD(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".fishhawk", "workflows.yaml"))
	if err != nil {
		t.Fatalf("read repo spec: %v", err)
	}
	parsed, err := spec.ParseBytes(raw)
	if err != nil {
		t.Fatalf("parse repo spec: %v", err)
	}
	gates := collectMembersGates(parsed)
	if len(gates) == 0 {
		t.Fatal("the shipped .fishhawk/workflows.yaml declares no approvals.members gate")
	}
	for _, g := range gates {
		if !identity.SubjectListed(g.Members, "github:kuhlman-labs", identity.ProviderGitHub) {
			t.Errorf("gate %s members %v does not admit github:kuhlman-labs", g.name(), g.Members)
		}
		if identity.SubjectListed(g.Members, "brett@local-mcp", identity.ProviderGitHub) {
			t.Errorf("gate %s members %v admits the static subject brett@local-mcp", g.name(), g.Members)
		}
	}
}

// TestApproverMembers_PostgresLoader crosses DB -> loader -> evaluation ->
// report: approval_submitted rows seeded through the real audit repository
// are filtered by the since bound (one second before excluded, exactly at
// included), the approve decision, the category and the repository, and the
// delegated on_behalf_of is extracted.
func TestApproverMembers_PostgresLoader(t *testing.T) {
	dbURL := pgtest.NewURL(t)
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	seedRun := func(repo string) (uuid.UUID, uuid.UUID) {
		runID, stageID := uuid.New(), uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
			VALUES ($1, $2, 'feature_change', 'sha-1', 'cli', 'pending', 'local')`, runID, repo); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
			VALUES ($1, $2, 0, 'plan', 'agent', 'claude-code', 'pending')`, stageID, runID); err != nil {
			t.Fatalf("seed stage: %v", err)
		}
		return runID, stageID
	}
	repo := audit.NewPostgresRepository(pool)
	appendEntry := func(runID, stageID uuid.UUID, ts time.Time, category, subject string, kind audit.ActorKind, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Timestamp: ts, Category: category,
			ActorKind: &kind, ActorSubject: &subject, Payload: raw,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	since := time.Date(2026, 10, 8, 16, 56, 0, 0, time.UTC)
	approve := map[string]any{"decision": "approve"}
	runA, stageA := seedRun("acme/widgets")
	runB, stageB := seedRun("other/repo")
	appendEntry(runA, stageA, since.Add(-time.Second), "approval_submitted", "brett@local-mcp", audit.ActorUser, approve)
	appendEntry(runA, stageA, since, "approval_submitted", "github:kuhlman-labs", audit.ActorUser, approve)
	appendEntry(runA, stageA, since.Add(time.Minute), "approval_submitted", "github:kuhlman-labs", audit.ActorUser, approve)
	appendEntry(runA, stageA, since.Add(time.Minute), "approval_submitted", "github:kuhlman-labs", audit.ActorUser, map[string]any{"decision": "reject"})
	appendEntry(runA, stageA, since.Add(time.Minute), "approval_predicate_rejected", "github:mallory", audit.ActorUser, approve)
	appendEntry(runA, stageA, since.Add(2*time.Minute), "approval_submitted", "operator-agent/delegated", audit.ActorAgent,
		map[string]any{"decision": "approve", "delegated": "may_approve", "on_behalf_of": "github:kuhlman-labs"})
	appendEntry(runB, stageB, since.Add(time.Minute), "approval_submitted", "brett@local-mcp", audit.ActorUser, approve)

	rows, err := loadApproverPrincipals(ctx, dbURL, "acme/widgets", since)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []approverPrincipalRow{
		{ActorSubject: "github:kuhlman-labs", ActorKind: "user", Approvals: 2},
		{ActorSubject: "operator-agent/delegated", ActorKind: "agent", OnBehalfOf: "github:kuhlman-labs", Delegated: true, Approvals: 1},
	}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Errorf("repo-scoped rows = %+v\nwant %+v", rows, want)
	}

	all, err := loadApproverPrincipals(ctx, dbURL, "", since)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if fmt.Sprint(all) != fmt.Sprint(append([]approverPrincipalRow{{ActorSubject: "brett@local-mcp", ActorKind: "user", Approvals: 1}}, want...)) {
		t.Errorf("all-repo rows = %+v", all)
	}

	// End to end through the command with the REAL loader.
	specPath := writeApproverMembersSpec(t, approverMembersSpec([]string{"github:kuhlman-labs"}))
	var out, errs strings.Builder
	got := runApproverMembersWith([]string{"--spec", specPath, "--db", dbURL, "--since", "2026-10-08T16:56:00Z", "--repo", "acme/widgets"},
		&out, &errs, loadApproverPrincipals, fixedNow)
	if got != exitOK {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", got, out.String(), errs.String())
	}
	if !strings.Contains(out.String(), "principal=github:kuhlman-labs approvals=3 verdict=admitted") ||
		!strings.Contains(out.String(), "impact: refused=0 principals=1 approvals=3 refused_approvals=0 agent_rows_now_refused=0") {
		t.Errorf("end-to-end report:\n%s", out.String())
	}
	out.Reset()
	if got := runApproverMembersWith([]string{"--spec", specPath, "--db", dbURL, "--since", "2026-10-08T16:56:00Z"},
		&out, &errs, loadApproverPrincipals, fixedNow); got != exitFailure {
		t.Errorf("all-repo exit = %d, want %d (brett@local-mcp refused)\n%s", got, exitFailure, out.String())
	}
}

// TestApproverMembers_PostgresLoaderConnectFailure: an unreachable database
// surfaces as a loader error.
func TestApproverMembers_PostgresLoaderConnectFailure(t *testing.T) {
	if _, err := loadApproverPrincipals(context.Background(), "postgres://x:y@127.0.0.1:1/db?connect_timeout=2", "", time.Now()); err == nil {
		t.Fatal("want a connect error")
	}
}
