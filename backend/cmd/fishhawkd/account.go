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

// runAccount dispatches the `account` subcommand — the operator write path for
// the ADR-057 tenancy `accounts` rows that GitLab run creation is gated on
// (E45.33 / #2923). It talks to the database directly (mirroring `token`),
// side-stepping the running server.
func runAccount(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "create":
		return runAccountCreate(rest, logSink)
	case "list":
		return runAccountList(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd account: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd account create --provider <p> --account-key <k> [--display-name <n>] [--granularity <g>]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd account list [--provider <p>]")
		return exitUsage
	}
}

func runAccountCreate(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd account create", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	// --provider is REQUIRED with no default: a GitLab namespace silently
	// created as a github account would pass every local check and then be
	// invisible to the GitLab authorization gate.
	provider := fs.String("provider", "", "forge discriminator (github|gitlab) — required")
	accountKey := fs.String("account-key", "", "forge-neutral natural key (enterprise slug / org login / GitLab group path) — required")
	displayName := fs.String("display-name", "", "cosmetic display name (optional)")
	granularity := fs.String("granularity", "", "enterprise|organization|group (optional; defaults per provider)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd account create: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	if *provider == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd account create: --provider required (one of github, gitlab)")
		return exitUsage
	}
	if *accountKey == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd account create: --account-key required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd account create: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	acct, err := account.CreateAccount(context.Background(), accountdb.New(pool), account.CreateAccountRequest{
		Provider:    *provider,
		AccountKey:  *accountKey,
		DisplayName: *displayName,
		Granularity: *granularity,
	})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd account create: %v\n", err)
		if errors.Is(err, account.ErrValidation) {
			return exitUsage
		}
		return exitFailure
	}

	_, _ = fmt.Printf("created account id=%s provider=%s account_key=%s granularity=%s\n",
		acct.ID, acct.Provider, acct.AccountKey, acct.Granularity)
	return exitOK
}

func runAccountList(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd account list", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	provider := fs.String("provider", "", "filter by forge discriminator (github|gitlab); empty lists all")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd account list: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd account list: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	accounts, err := accountdb.New(pool).ListAccounts(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd account list: %v\n", err)
		return exitFailure
	}
	filter := strings.TrimSpace(*provider)
	rows := accounts[:0:0]
	for _, a := range accounts {
		if filter == "" || a.Provider == filter {
			rows = append(rows, a)
		}
	}
	if len(rows) == 0 {
		_, _ = fmt.Println("no accounts registered")
		return exitOK
	}

	_, _ = fmt.Printf("%-8s  %-24s  %-13s  %-24s  %s\n", "PROVIDER", "ACCOUNT_KEY", "GRANULARITY", "DISPLAY_NAME", "ID")
	for _, a := range rows {
		display := ""
		if a.DisplayName != nil {
			display = *a.DisplayName
		}
		_, _ = fmt.Printf("%-8s  %-24s  %-13s  %-24s  %s\n", a.Provider, a.AccountKey, a.Granularity, display, a.ID)
	}
	return exitOK
}
