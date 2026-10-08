package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

const approverMembersUsage = "Usage: fishhawkd approver-members --spec <path> [--db <url>] [--window 30d | --since <RFC3339>] [--repo owner/name] [--forge github|gitlab]"

// approverPrincipalRow is one aggregate group of approve-decision
// approval_submitted audit rows: the rows sharing an actor subject/kind, a
// recorded on_behalf_of and a delegated flag, with how many there were.
type approverPrincipalRow struct {
	ActorSubject string
	ActorKind    string
	OnBehalfOf   string
	Delegated    bool
	Approvals    int
}

// principal is the identity the approvals.members gate evaluates for the
// group's rows (#4116): the recorded on_behalf_of for a delegated row (the
// real operator after the #2381 remap, or the seated captain for an
// agent-kind delegation), the actor subject otherwise. A delegated row that
// recorded no on_behalf_of falls back to its actor subject, so a pre-#2381
// human delegation is still evaluated as that human.
func (r approverPrincipalRow) principal() string {
	if r.Delegated && r.OnBehalfOf != "" {
		return r.OnBehalfOf
	}
	return r.ActorSubject
}

// agentKind reports whether the group's principal is an agent (or absent):
// the operator-agent token family, or an empty principal. Such a principal
// never counts toward a quorum (the unconditional #1709 agent floor), so it
// is reported but is never an impact.
func (r approverPrincipalRow) agentKind() bool {
	p := r.principal()
	if p == "" || operatorrole.IsTokenSubject(p) {
		return true
	}
	return p == r.ActorSubject && r.ActorKind == string(audit.ActorAgent)
}

// approverPrincipalLoader reads the aggregate approve-decision
// approval_submitted groups recorded at or after since (optionally for one
// repository). It is a seam so the command's flags, evaluation and report are
// testable without a database; production wires loadApproverPrincipals.
type approverPrincipalLoader func(ctx context.Context, dbURL, repo string, since time.Time) ([]approverPrincipalRow, error)

// membersGate is one approval gate of the spec that declares a non-empty
// approvals.members list.
type membersGate struct {
	Workflow string
	Stage    string
	Members  []string
}

func (g membersGate) name() string { return g.Workflow + "/" + g.Stage }

// runApproverMembers is the no-wedge dry-run and auth-checklist impact
// inventory for approvals.members enforcement (#4116):
//
//	fishhawkd approver-members --spec <path> [--db <url>] [--window 30d | --since <RFC3339>]
//	    [--repo owner/name] [--forge github|gitlab]
//
// It evaluates every principal that recorded an approve in the window against
// every members-declaring approval gate of the spec, through the same
// identity.SubjectListed the gate runs, and exits 1 when any human principal
// would be refused (0 when none would, 2 on a usage error).
func runApproverMembers(args []string, logSink io.Writer) int {
	return runApproverMembersWith(args, os.Stdout, logSink, loadApproverPrincipals, time.Now)
}

func runApproverMembersWith(args []string, out, logSink io.Writer, load approverPrincipalLoader, now func() time.Time) int {
	fs := flag.NewFlagSet("fishhawkd approver-members", flag.ContinueOnError)
	fs.SetOutput(logSink)
	specPath := fs.String("spec", "", "workflow spec file whose members-declaring approval gates to evaluate (required)")
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	window := fs.String("window", "30d", "look-back window (30d or a Go duration); mutually exclusive with --since")
	since := fs.String("since", "", "RFC3339 lower bound used verbatim (e.g. 2026-10-08T16:56:00Z); mutually exclusive with --window")
	repo := fs.String("repo", "", "restrict to one repository (owner/name); default all repositories")
	forge := fs.String("forge", identity.ProviderGitHub, "forge family a plain (unqualified) members entry is qualified with: github|gitlab")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	usageErr := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(logSink, "fishhawkd approver-members: "+format+"\n", a...)
		_, _ = fmt.Fprintln(logSink, approverMembersUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(*specPath) == "" {
		return usageErr("--spec is required (the gates to evaluate come from a workflow spec file)")
	}
	// Exclusivity is decided on the flags the operator SET (Visit visits only
	// those), so the --window default alone never conflicts with --since.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["window"] && set["since"] {
		return usageErr("--window and --since are mutually exclusive; pass one")
	}
	var lower time.Time
	bound := ""
	if set["since"] {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return usageErr("--since %q is not an RFC3339 instant (e.g. 2026-10-08T16:56:00Z): %v", *since, err)
		}
		lower, bound = t, "--since"
	} else {
		w, err := precedent.ParseWindow(*window)
		if err != nil {
			return usageErr("--window: %v", err)
		}
		lower, bound = now().UTC().Add(-w), "--window "+*window
	}
	if *forge != identity.ProviderGitHub && *forge != identity.ProviderGitLab {
		return usageErr("--forge %q is not one of github|gitlab", *forge)
	}
	if *dbURL == "" {
		return usageErr("--db or FISHHAWKD_DATABASE_URL is required")
	}
	raw, err := os.ReadFile(*specPath)
	if err != nil {
		return usageErr("read --spec: %v", err)
	}
	parsed, err := spec.ParseBytes(raw)
	if err != nil {
		return usageErr("parse --spec %s: %v", *specPath, err)
	}
	gates := collectMembersGates(parsed)
	if len(gates) == 0 {
		_, _ = fmt.Fprintf(out, "approver-members spec=%s: no approval gate declares approvals.members; nothing is restricted\n", *specPath)
		return exitOK
	}

	repoLabel := *repo
	if repoLabel == "" {
		repoLabel = "(all)"
	}
	_, _ = fmt.Fprintf(out, "approver-members spec=%s since=%s (%s) repo=%s forge=%s members_gates=%d\n",
		*specPath, lower.UTC().Format(time.RFC3339), bound, repoLabel, *forge, len(gates))
	for _, g := range gates {
		_, _ = fmt.Fprintf(out, "gate %s members=[%s]\n", g.name(), strings.Join(g.Members, ","))
	}
	_, _ = fmt.Fprintln(out, "note: merge-settled review gates (resolveReviewStageOnMerge) record no approval row and are not members-checked; they are not part of this inventory")
	_, _ = fmt.Fprintln(out, "note: each principal is evaluated against EVERY members gate above, whichever gate its approvals were recorded at")
	_, _ = fmt.Fprintln(out, "note: audit_entries is row-level secured; a role without BYPASSRLS (and not a superuser) sees only account-unscoped rows, so a short inventory may be incomplete")

	rows, err := load(context.Background(), *dbURL, *repo, lower)
	if err != nil {
		newLogger(logSink).Error("approver-members: read approval_submitted rows failed", slog.String("error", err.Error()))
		return exitFailure
	}
	if writeApproverMembersReport(out, rows, gates, *forge) > 0 {
		return exitFailure
	}
	return exitOK
}

// collectMembersGates returns every approval gate of s that declares a
// non-empty approvals.members list, workflows in name order and stages in
// declaration order. An empty or absent list is no restriction, so it is not
// a gate the inventory evaluates.
func collectMembersGates(s *spec.Spec) []membersGate {
	names := make([]string, 0, len(s.Workflows))
	for n := range s.Workflows {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []membersGate
	for _, wf := range names {
		for _, st := range s.Workflows[wf].Stages {
			for _, g := range st.Gates {
				if g.Type != spec.GateTypeApproval || g.Approvals == nil || len(g.Approvals.Members) == 0 {
					continue
				}
				out = append(out, membersGate{Workflow: wf, Stage: st.ID, Members: append([]string(nil), g.Approvals.Members...)})
			}
		}
	}
	return out
}

// principalTally is one principal's aggregate across loader groups.
type principalTally struct {
	subject   string
	approvals int
	agent     bool
}

// writeApproverMembersReport prints one line per principal and the summary
// line, and returns the number of HUMAN principals refused by at least one
// members gate (the exit-code driver). Agent-kind principals are reported
// agent_never_counted: they are refused at a members gate too (a non-delegated
// agent submission's 200 became a 403), so their rows are summed into
// agent_rows_now_refused, but they never count toward a quorum and so stay
// out of refused= and the exit code.
func writeApproverMembersReport(out io.Writer, rows []approverPrincipalRow, gates []membersGate, forge string) int {
	byKey := map[string]*principalTally{}
	for _, r := range rows {
		agent := r.agentKind()
		p := r.principal()
		key := fmt.Sprintf("%t|%s", agent, p)
		t, ok := byKey[key]
		if !ok {
			t = &principalTally{subject: p, agent: agent}
			byKey[key] = t
		}
		t.approvals += r.Approvals
	}
	tallies := make([]*principalTally, 0, len(byKey))
	for _, t := range byKey {
		tallies = append(tallies, t)
	}
	sort.Slice(tallies, func(i, j int) bool {
		if tallies[i].agent != tallies[j].agent {
			return !tallies[i].agent
		}
		return tallies[i].subject < tallies[j].subject
	})

	var refused, principals, approvals, refusedApprovals, agentRefused int
	for _, t := range tallies {
		var refusing []string
		seen := map[string]bool{}
		for _, g := range gates {
			if !identity.SubjectListed(g.Members, t.subject, forge) && !seen[g.name()] {
				seen[g.name()] = true
				refusing = append(refusing, g.name())
			}
		}
		subject := t.subject
		if subject == "" {
			subject = `""`
		}
		if t.agent {
			if len(refusing) > 0 {
				agentRefused += t.approvals
			}
			_, _ = fmt.Fprintf(out, "principal=%s approvals=%d verdict=agent_never_counted refusing_gates=%s\n",
				subject, t.approvals, joinOrDash(refusing))
			continue
		}
		principals++
		approvals += t.approvals
		verdict := "admitted"
		if len(refusing) > 0 {
			verdict = "refused"
			refused++
			refusedApprovals += t.approvals
		}
		_, _ = fmt.Fprintf(out, "principal=%s approvals=%d verdict=%s refusing_gates=%s\n",
			subject, t.approvals, verdict, joinOrDash(refusing))
	}
	_, _ = fmt.Fprintf(out, "impact: refused=%d principals=%d approvals=%d refused_approvals=%d agent_rows_now_refused=%d\n",
		refused, principals, approvals, refusedApprovals, agentRefused)
	return refused
}

func joinOrDash(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}

// approverPrincipalsSQL aggregates the approve-decision approval_submitted
// rows at or after $1 with decision $3, optionally for one repository (an
// empty $2 is all repositories).
const approverPrincipalsSQL = `
SELECT COALESCE(a.actor_subject, ''),
       COALESCE(a.actor_kind, ''),
       COALESCE(a.payload->>'on_behalf_of', ''),
       COALESCE(a.payload->>'delegated', '') <> '',
       count(*)
FROM audit_entries a
JOIN runs r ON r.id = a.run_id
WHERE a.category = 'approval_submitted'
  AND a.payload->>'decision' = $3
  AND a.ts >= $1
  AND ($2 = '' OR r.repo = $2)
GROUP BY 1, 2, 3, 4
ORDER BY 1, 2, 3, 4`

// loadApproverPrincipals is the production loader: one aggregate SELECT over
// audit_entries joined to runs. Like `decision-index backfill` it sets no
// app.account_id, so under a non-BYPASSRLS role only account-unscoped rows
// are visible (the report prints that caveat).
func loadApproverPrincipals(ctx context.Context, dbURL, repo string, since time.Time) ([]approverPrincipalRow, error) {
	pool, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	rs, err := pool.Query(ctx, approverPrincipalsSQL, since.UTC(), repo, string(approval.DecisionApprove))
	if err != nil {
		return nil, fmt.Errorf("query approval_submitted: %w", err)
	}
	defer rs.Close()
	var out []approverPrincipalRow
	for rs.Next() {
		var r approverPrincipalRow
		var n int64
		if err := rs.Scan(&r.ActorSubject, &r.ActorKind, &r.OnBehalfOf, &r.Delegated, &n); err != nil {
			return nil, fmt.Errorf("scan approval_submitted aggregate: %w", err)
		}
		r.Approvals = int(n)
		out = append(out, r)
	}
	return out, rs.Err()
}
