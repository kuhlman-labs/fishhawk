package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
)

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
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd token: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd token issue --subject <s> [--scopes a,b]")
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
		scopes = []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
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
