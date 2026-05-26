package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tokenmigrate"
)

// operatorDefaultScopes is the canonical scope set applied to every
// non-MCP operator token at issuance time (#526). Shared between
// runTokenIssue and runTokenMigrate so there is one source of truth.
var operatorDefaultScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
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
		"identity the token is bound to (e.g. \"github:42\" or \"bootstrap\")")
	scopesCSV := fs.String("scopes", "",
		"comma-separated scope list (optional)")
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

	scopes := splitCSV(*scopesCSV)
	if len(scopes) == 0 && !strings.HasPrefix(*subject, "mcp:") {
		scopes = operatorDefaultScopes
		_, _ = fmt.Fprintln(logSink, "fishhawkd token issue: applying default operator scope set")
	}

	pool, err := pgxpool.New(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd token issue: pool: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	repo := apitoken.NewPostgresRepository(pool)
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
