package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// runMember dispatches the `member` subcommand — the operator write path for the
// ADR-057 account_members membership grants that admit the first human on a fresh
// self-hosted install (E44.34 / #2924). It talks to the database directly
// (--db / FISHHAWKD_DATABASE_URL, mirroring `account` / `installation`),
// side-stepping the running server. `invite` writes exactly the origin='invited'
// row the shipped login-gate admission walk already honors.
func runMember(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "invite":
		return runMemberInvite(rest, logSink)
	case "list":
		return runMemberList(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd member: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd member invite --provider <p> --account-key <k> --member-ref <login> [--role <admin|member>]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd member list [--provider <p>] [--account-key <k>]")
		return exitUsage
	}
}

func runMemberInvite(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd member invite", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	// --provider is REQUIRED with no default: a GitLab member silently invited
	// under a github account would pass every local check and then be invisible
	// to the login gate, which reads (provider, member_ref).
	provider := fs.String("provider", "", "forge discriminator (github|gitlab) — required")
	accountKey := fs.String("account-key", "", "account_key of the owning account (must already exist) — required")
	// member-ref is the forge LOGIN: auth/membership.go resolves grants via
	// ListMemberGrants(ctx, provider, profile.Login), NOT a numeric user id or
	// an email.
	memberRef := fs.String("member-ref", "", "forge LOGIN of the member (not a numeric id or email) — required")
	role := fs.String("role", "", "grant role (admin|member); defaults to member")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd member invite: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	if *provider == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd member invite: --provider required (one of github, gitlab)")
		return exitUsage
	}
	if *accountKey == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd member invite: --account-key required")
		return exitUsage
	}
	if *memberRef == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd member invite: --member-ref required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd member invite: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	row, err := account.InviteMember(context.Background(), accountdb.New(pool), account.InviteMemberRequest{
		Provider:   *provider,
		AccountKey: *accountKey,
		MemberRef:  *memberRef,
		Role:       *role,
	})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd member invite: %v\n", err)
		// A mis-typed flag (ErrValidation) is a usage error; an unknown account
		// (ErrAccountNotFound) and any DB fault are failures — distinguishable
		// by exit code alone.
		if errors.Is(err, account.ErrValidation) {
			return exitUsage
		}
		return exitFailure
	}

	roleStr := ""
	if row.Role != nil {
		roleStr = *row.Role
	}
	_, _ = fmt.Printf("invited member id=%s account_key=%s provider=%s member_ref=%s role=%s origin=%s\n",
		row.ID, strings.TrimSpace(*accountKey), row.Provider, row.MemberRef, roleStr, row.Origin)
	return exitOK
}

func runMemberList(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd member list", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	provider := fs.String("provider", "", "filter by forge discriminator (github|gitlab); empty lists all")
	accountKey := fs.String("account-key", "", "filter by owning account_key; empty lists all")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd member list: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd member list: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	members, err := account.ListMembers(context.Background(), accountdb.New(pool))
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd member list: %v\n", err)
		return exitFailure
	}
	providerFilter := strings.TrimSpace(*provider)
	keyFilter := strings.TrimSpace(*accountKey)
	rows := members[:0:0]
	for _, m := range members {
		if providerFilter != "" && m.Provider != providerFilter {
			continue
		}
		if keyFilter != "" && m.AccountKey != keyFilter {
			continue
		}
		rows = append(rows, m)
	}
	if len(rows) == 0 {
		_, _ = fmt.Println("no members granted")
		return exitOK
	}

	_, _ = fmt.Printf("%-8s  %-24s  %-24s  %-8s  %-9s  %s\n", "PROVIDER", "ACCOUNT_KEY", "MEMBER_REF", "ROLE", "ORIGIN", "ID")
	for _, m := range rows {
		role := ""
		if m.Role != nil {
			role = *m.Role
		}
		_, _ = fmt.Printf("%-8s  %-24s  %-24s  %-8s  %-9s  %s\n", m.Provider, m.AccountKey, m.MemberRef, role, m.Origin, m.ID)
	}
	return exitOK
}
