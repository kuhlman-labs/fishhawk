package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tokenmigrate"
)

// operatorDefaultScopes is the canonical scope set applied to every
// non-MCP operator token at issuance time (#526). Shared between
// runTokenIssue and runTokenMigrate so there is one source of truth.
var operatorDefaultScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
	// write:deploy gates the deploy stage's pre-execution approval and the
	// deploy dispatch/rollback operator bearer paths (ADR-038 / #1390). New
	// operator tokens and `token migrate --apply` promotions carry it; the
	// ed25519 runner-signature path and the mcp:run self-rollback subject-
	// binding path are NOT scope-gated and stay unaffected.
	"write:deploy",
	// write:campaigns gates POST/GET /v0/campaigns and POST
	// /v0/campaigns/{id}/resume (E25.4 / #1443;
	// requireWriteScope("write:campaigns") in
	// backend/internal/server/campaigns.go), so operator tokens need it to
	// drive the campaign primitive. New tokens and `token migrate --apply`
	// promotions carry it.
	"write:campaigns",
	// read:audit-export gates the bulk compliance-export surfaces —
	// GET /v0/audit/export, /v0/audit/export.csv, and
	// /v0/reports/agent-changes(.md) (E9.5 / #1608, ADR-054). Bulk
	// evidence export is deliberately a distinct scope from per-run
	// reads so export-capable tokens are enumerable and revocable
	// independently; it is included in the operator default because the
	// operator IS the compliance principal in the single-operator v0
	// posture. New tokens and `token migrate --apply` promotions carry
	// it; a token that must NOT export can be issued with an explicit
	// --scopes list omitting it.
	"read:audit-export",
}

// mcpCapabilityScopes lists the optional scopes that can be granted
// to mcp tokens beyond the baseline "mcp:read". These are NOT in
// the operator default set and are NOT issued via `fishhawkd token issue`.
// They are granted by the backend at mcptoken issuance time (POST
// /v0/runs/{id}/mcp-token) based on the workflow spec's executor
// config — specifically, write:retries is included only when
// executor.agent_self_retry: true is set on the executing stage.
var mcpCapabilityScopes = []string{
	"write:retries",
}

// runToken dispatches the `token` subcommand. v0 has one operation
// — issue — for bootstrapping the first API token before OAuth
// (E4.2) is wired. The CLI talks to the database directly rather
// than the HTTP layer, side-stepping the chicken-and-egg of "you
// need a token to mint a token."
func runToken(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "issue":
		return runTokenIssue(rest, logSink)
	case "migrate":
		return runTokenMigrate(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd token: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd token issue --subject <s> [--scopes a,b]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd token migrate [--db <url>] [--apply]")
		return exitUsage
	}
}

func runTokenIssue(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd token issue", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""),
		"postgres URL")
	subject := fs.String("subject", "",
		"identity the token is bound to (e.g. \"github:42\", \"bootstrap\", \"operator-agent/<role-spec-version>\" for an operator-agent role instance, or \"host-dispatcher/<host>\" for an ADR-104 host dispatcher)")
	scopesCSV := fs.String("scopes", "",
		"comma-separated scope list (optional; a host-dispatcher/<host> subject defaults to, and is limited to, read:runs,read:audit,write:runs,write:stages)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd token issue: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	if *subject == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd token issue: --subject required")
		return exitUsage
	}
	// ADR-040 D4 (#1027): an operator-agent subject must name a
	// recognized role-spec version so audit attribution can tie the
	// token's actions to a concrete role contract.
	if err := operatorrole.ValidateTokenSubject(*subject); err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token issue: %v\n", err)
		return exitUsage
	}

	scopes, note, err := resolveIssueScopes(*subject, splitCSV(*scopesCSV))
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token issue: %v\n", err)
		return exitUsage
	}
	if note != "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd token issue: "+note)
	}

	pool, err := pgxpool.New(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token issue: pool: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	repo := apitoken.NewPostgresRepository(pool)
	// Untenanted by design (#4506): this is the operator bootstrap
	// credential, and its holder already has database access. A tenanted
	// token is minted through POST /v0/tokens, which inherits the caller's
	// account.
	tok, err := repo.Issue(context.Background(), *subject, scopes)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token issue: %v\n", err)
		return exitFailure
	}

	// stdout, not the log sink: scripts that pipe `... | head -n1`
	// expect just the bearer string, not "issued token X" prose.
	_, _ = fmt.Println(tok.PlainText)
	_, _ = fmt.Fprintf(logSink,
		"issued token id=%s subject=%s scopes=%v\n",
		tok.ID, tok.Subject, tok.Scopes)
	return exitOK
}

// resolveIssueScopes decides the scope set `fishhawkd token issue` mints for
// subject. A host-dispatcher/<host> subject (ADR-104 Q4, E88.2 / #4526) must
// carry a valid label, defaults to hostdispatcher.Scopes() when --scopes is
// omitted, and may not request a scope outside that profile (the error names
// it). Every other subject keeps the prior rule: an empty request defaults to
// operatorDefaultScopes, except an mcp: subject. note is the log line to print
// when a default was applied.
func resolveIssueScopes(subject string, requested []string) (scopes []string, note string, err error) {
	if hostdispatcher.IsSubject(subject) {
		if verr := hostdispatcher.ValidateSubject(subject); verr != nil {
			return nil, "", verr
		}
		if len(requested) == 0 {
			return hostdispatcher.Scopes(), "applying the host-dispatcher scope profile", nil
		}
		if outside := hostdispatcher.OutsideProfile(requested); len(outside) > 0 {
			return nil, "", fmt.Errorf("host-dispatcher subject %q may not hold scope(s) %s; the profile is %s",
				subject, strings.Join(outside, ","), strings.Join(hostdispatcher.Scopes(), ","))
		}
		return requested, "", nil
	}
	if len(requested) == 0 && !strings.HasPrefix(subject, "mcp:") {
		return operatorDefaultScopes, "applying default operator scope set", nil
	}
	return requested, "", nil
}

func runTokenMigrate(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd token migrate", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	apply := fs.Bool("apply", false, "write changes (default is dry-run)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd token migrate: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	dryRun := !*apply
	if dryRun {
		_, _ = fmt.Fprintln(logSink, "fishhawkd token migrate: dry-run (pass --apply to write)")
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token migrate: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	summary, err := tokenmigrate.MigrateScopes(context.Background(), pool, operatorDefaultScopes, dryRun, logSink)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token migrate: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Fprintf(logSink,
		"fishhawkd token migrate: done dry_run=%v scanned=%d migrated=%d skipped=%d\n",
		dryRun, summary.Scanned, summary.Migrated, summary.Skipped)
	return exitOK
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
