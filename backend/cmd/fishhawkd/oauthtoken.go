package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// runOAuthToken dispatches `oauth token revoke` — the operator revocation
// surface for AS-issued credentials (E66.5 / #2393, ADR-076 slice 4). It
// deliberately ships as a fishhawkd verb rather than an RFC 7009 endpoint: this
// slice adds no public unauthenticated AS route. Direct DB via
// --db/FISHHAWKD_DATABASE_URL, side-stepping the running server (the `oauth
// client` precedent).
func runOAuthToken(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "revoke":
		return runOAuthTokenRevoke(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth token: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd oauth token revoke --subject <s> [--client-id <c>]")
		return exitUsage
	}
}

// oauthTokenRevocation is the raw, parsed revoke input. Validation is a pure,
// DB-free function (resolveOAuthTokenRevocation) so each refusal is
// unit-testable without Postgres.
type oauthTokenRevocation struct {
	Subject  string
	ClientID string
}

// resolveOAuthTokenRevocation validates the revoke inputs and returns them
// UNCHANGED, or a usage error naming the offending flag. It is pure — no
// database.
//
// Whitespace is REFUSED, not trimmed (mirroring `oauth client register`'s
// --client-id rule): a subject or client_id carrying surrounding whitespace is
// almost certainly a copy-paste artifact, and silently trimming it would revoke
// a DIFFERENT subject from the one the operator typed. The store's own
// ErrSubjectRequired guard is the last line for an empty subject; the refusal
// here is what names the flag.
func resolveOAuthTokenRevocation(in oauthTokenRevocation) (oauthTokenRevocation, error) {
	var zero oauthTokenRevocation
	if strings.TrimSpace(in.Subject) == "" {
		return zero, errors.New("--subject required")
	}
	if in.Subject != strings.TrimSpace(in.Subject) {
		return zero, fmt.Errorf("--subject %q must not have surrounding whitespace", in.Subject)
	}
	if in.ClientID != strings.TrimSpace(in.ClientID) {
		return zero, fmt.Errorf("--client-id %q must not have surrounding whitespace", in.ClientID)
	}
	return in, nil
}

func runOAuthTokenRevoke(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd oauth token revoke", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	subject := fs.String("subject", "", "the subject whose AS-issued access AND refresh tokens are revoked (e.g. github:octocat) — required")
	clientID := fs.String("client-id", "", "narrow the revocation to tokens issued to this client_id (optional; default every client)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd oauth token revoke: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	in, err := resolveOAuthTokenRevocation(oauthTokenRevocation{Subject: *subject, ClientID: *clientID})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth token revoke: %v\n", err)
		return exitUsage
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth token revoke: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	access, refresh, err := oauthstore.NewPostgresRepository(pool).RevokeGrantsForSubject(ctx, in.Subject, in.ClientID)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth token revoke: %v\n", err)
		return exitFailure
	}
	// Zero counts are a SUCCESSFUL no-op (the subject holds no live token),
	// printed as such: unlike `oauth client remove`, there is no row the operator
	// believed existed, and an already-revoked subject is the desired end state.
	_, _ = fmt.Printf("revoked oauth tokens subject=%s client_id=%s access_revoked=%d refresh_revoked=%d\n",
		in.Subject, renderOAuthClientFilter(in.ClientID), access, refresh)
	return exitOK
}

// renderOAuthClientFilter renders the resolved client filter for operator
// output: an empty filter means every client and shows as `*` rather than a
// blank cell.
func renderOAuthClientFilter(clientID string) string {
	if clientID == "" {
		return "*"
	}
	return clientID
}
