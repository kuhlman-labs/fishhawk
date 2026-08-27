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

// runInstallation dispatches the `installation` subcommand — the operator write
// path for the ADR-057 tenancy `installations` rows that gate GitLab run
// creation (E45.33 / #2923). Direct DB, no running server.
func runInstallation(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "register":
		return runInstallationRegister(rest, logSink)
	case "list":
		return runInstallationList(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd installation: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd installation register --provider <p> --account-key <k> --installation-ref <ref> [--forge-base-url <url>] [--oauth-base-url <url>]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd installation list [--provider <p>]")
		return exitUsage
	}
}

func runInstallationRegister(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd installation register", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	provider := fs.String("provider", "", "forge discriminator (github|gitlab) — required")
	accountKey := fs.String("account-key", "", "account_key of the owning account (must already exist) — required")
	installationRef := fs.String("installation-ref", "", "credential-scope ref (gitlab:<project-id> | bare github installation id) — required")
	forgeBaseURL := fs.String("forge-base-url", "", "per-installation forge base URL override (optional; https only)")
	oauthBaseURL := fs.String("oauth-base-url", "", "per-installation OAuth base URL override (optional; https only)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd installation register: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	if *provider == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd installation register: --provider required (one of github, gitlab)")
		return exitUsage
	}
	if *accountKey == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd installation register: --account-key required")
		return exitUsage
	}
	if *installationRef == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd installation register: --installation-ref required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd installation register: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	inst, err := account.RegisterInstallation(context.Background(), accountdb.New(pool), account.RegisterInstallationRequest{
		Provider:        *provider,
		AccountKey:      *accountKey,
		InstallationRef: *installationRef,
		ForgeBaseURL:    *forgeBaseURL,
		OAuthBaseURL:    *oauthBaseURL,
	})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd installation register: %v\n", err)
		// A mis-typed flag (ErrValidation) is a usage error; an unknown account
		// (ErrAccountNotFound) and any DB fault are failures — distinguishable
		// by exit code alone.
		if errors.Is(err, account.ErrValidation) {
			return exitUsage
		}
		return exitFailure
	}

	_, _ = fmt.Printf("registered installation id=%s provider=%s installation_ref=%s account_key=%s\n",
		inst.ID, inst.Provider, inst.InstallationRef, strings.TrimSpace(*accountKey))
	return exitOK
}

func runInstallationList(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd installation list", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	provider := fs.String("provider", "", "filter by forge discriminator (github|gitlab); empty lists all")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd installation list: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	pool, err := postgres.Connect(context.Background(), *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd installation list: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	installs, err := accountdb.New(pool).ListInstallations(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd installation list: %v\n", err)
		return exitFailure
	}
	filter := strings.TrimSpace(*provider)
	rows := installs[:0:0]
	for _, i := range installs {
		if filter == "" || i.Provider == filter {
			rows = append(rows, i)
		}
	}
	if len(rows) == 0 {
		_, _ = fmt.Println("no installations registered")
		return exitOK
	}

	_, _ = fmt.Printf("%-8s  %-24s  %-24s  %-32s  %s\n", "PROVIDER", "INSTALLATION_REF", "ACCOUNT_KEY", "FORGE_BASE_URL", "ID")
	for _, i := range rows {
		forge := ""
		if i.ForgeBaseUrl != nil {
			forge = *i.ForgeBaseUrl
		}
		_, _ = fmt.Printf("%-8s  %-24s  %-24s  %-32s  %s\n", i.Provider, i.InstallationRef, i.AccountKey, forge, i.ID)
	}
	return exitOK
}
