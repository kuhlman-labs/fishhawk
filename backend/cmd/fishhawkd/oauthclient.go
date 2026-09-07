package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthstore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
)

// runOAuth dispatches the `oauth` subcommand group. Today it carries only the
// `client` verb group (ADR-076 slice 3, E66.21 / #2438) — the operator write
// path for OAuth client pre-registration.
func runOAuth(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "client":
		return runOAuthClient(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd oauth client register --client-id <id> --redirect-uri <uri> [--redirect-uri <uri>...] [flags]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd oauth client list")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd oauth client remove --client-id <id>")
		return exitUsage
	}
}

// runOAuthClient dispatches `oauth client register|list|remove` — the write path
// for `oauth_clients` rows that #2436's resolveOAuthClient prefers over a CIMD
// fetch. Direct DB via --db/FISHHAWKD_DATABASE_URL, side-stepping the running
// server (the `account`/`installation`/`member` precedent).
func runOAuthClient(args []string, logSink io.Writer) int {
	cmd, rest := splitCommand(args)
	switch cmd {
	case "register":
		return runOAuthClientRegister(rest, logSink)
	case "list":
		return runOAuthClientList(rest, logSink)
	case "remove":
		return runOAuthClientRemove(rest, logSink)
	default:
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client: unknown subcommand %q\n", cmd)
		_, _ = fmt.Fprintln(logSink, "Usage: fishhawkd oauth client register --client-id <id> --redirect-uri <uri> [--redirect-uri <uri>...] [--token-endpoint-auth-method none] [--grant-type <g>...] [--response-type <r>...] [--client-name <n>] [--client-uri <u>] [--logo-uri <u>] [--scope <s>] [--provider <p> --account-key <k>]")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd oauth client list")
		_, _ = fmt.Fprintln(logSink, "       fishhawkd oauth client remove --client-id <id>")
		return exitUsage
	}
}

// stringSlice is a repeatable string flag: each --flag occurrence APPENDS. An
// unset repeatable flag is an empty slice, which the register path replaces with
// the documented default (see resolveOAuthClientRegistration).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// oauthClientRegistration is the raw, parsed register input. Kept separate from
// oauthas.ClientMetadata so validation (and its refusals) is a pure, DB-free
// function each failure mode can unit-test.
type oauthClientRegistration struct {
	ClientID                string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	GrantTypes              []string
	ResponseTypes           []string
	ClientName              string
	ClientURI               string
	LogoURI                 string
	Scope                   string
	Provider                string
	AccountKey              string
}

// oauthClientTenant names the OWNING account. Present is false for an untenanted
// registration (the column is nullable). Provider/AccountKey are trimmed.
type oauthClientTenant struct {
	Provider   string
	AccountKey string
	Present    bool
}

// resolveOAuthClientRegistration validates every field the AS will later enforce
// AT REGISTRATION TIME and returns the persisted metadata plus the resolved
// tenant, or a usage error naming the offending input. It is pure — no database
// — so each refusal is testable without Postgres.
//
// --client-id is DELIBERATELY not run through oauthas.ValidateClientIDURL:
// pre-registration exists precisely for a client that hosts no CIMD document
// (Codex, #2394), and resolveOAuthClient validates the id as a URL only on the
// store-MISS fall-through. Only non-emptiness and absence of surrounding
// whitespace are enforced here.
func resolveOAuthClientRegistration(in oauthClientRegistration) (oauthas.ClientMetadata, oauthClientTenant, error) {
	var zeroMeta oauthas.ClientMetadata
	var zeroTenant oauthClientTenant

	if strings.TrimSpace(in.ClientID) == "" {
		return zeroMeta, zeroTenant, errors.New("--client-id required")
	}
	if in.ClientID != strings.TrimSpace(in.ClientID) {
		return zeroMeta, zeroTenant, fmt.Errorf("--client-id %q must not have surrounding whitespace", in.ClientID)
	}

	if len(in.RedirectURIs) == 0 {
		return zeroMeta, zeroTenant, errors.New("at least one --redirect-uri required")
	}
	// Each URI is matched against ITSELF: MatchRedirectURI validates both
	// arguments through the registered-side component rules (no userinfo, no
	// query, no fragment, absolute with a host) BEFORE the equality branch, and
	// with identical arguments that branch trivially succeeds — so a nil return
	// means exactly "this URI passes the rules the live authorize path applies",
	// and the self-pairing cannot be defeated by the byte-equality short-circuit.
	for _, uri := range in.RedirectURIs {
		if err := oauthas.MatchRedirectURI(uri, uri); err != nil {
			return zeroMeta, zeroTenant, fmt.Errorf("invalid --redirect-uri %q: %v", uri, err)
		}
	}

	// The only method the AS supports: validateClientMetadata requires it and the
	// token endpoint rejects any Authorization header as invalid_client.
	if in.TokenEndpointAuthMethod != "none" {
		return zeroMeta, zeroTenant, fmt.Errorf(
			"--token-endpoint-auth-method %q unsupported; only 'none' is supported (the AS is a public-client-only token endpoint)",
			in.TokenEndpointAuthMethod)
	}

	// Persist EXPLICIT defaults rather than empty sets so `list` renders the truth
	// and a refresh actually works: registeredGrantTypes defaults an ABSENT set to
	// ["authorization_code"] ONLY, so an empty-set registration could never use
	// grant_type=refresh_token at the token endpoint.
	grantTypes := in.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	responseTypes := in.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}

	// Mirrors the CIMD rule: authorization_code is the only flow the AS mints.
	if !containsString(grantTypes, "authorization_code") {
		return zeroMeta, zeroTenant, fmt.Errorf("--grant-type set %v must include authorization_code", grantTypes)
	}

	provider := strings.TrimSpace(in.Provider)
	accountKey := strings.TrimSpace(in.AccountKey)
	if (provider == "") != (accountKey == "") {
		return zeroMeta, zeroTenant, errors.New("--provider and --account-key must be supplied together or not at all")
	}

	meta := oauthas.ClientMetadata{
		ClientID:                in.ClientID,
		RedirectURIs:            in.RedirectURIs,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		TokenEndpointAuthMethod: in.TokenEndpointAuthMethod,
		ClientName:              in.ClientName,
		ClientURI:               in.ClientURI,
		LogoURI:                 in.LogoURI,
		Scope:                   in.Scope,
	}
	tenant := oauthClientTenant{Provider: provider, AccountKey: accountKey, Present: provider != ""}
	return meta, tenant, nil
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func runOAuthClientRegister(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd oauth client register", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	clientID := fs.String("client-id", "", "the client's identifier (a CIMD document URL, or any stable id for a client that hosts none) — required")
	var redirectURIs stringSlice
	fs.Var(&redirectURIs, "redirect-uri", "an allowed redirect URI (repeatable; at least one required)")
	authMethod := fs.String("token-endpoint-auth-method", "none", "token endpoint auth method; only 'none' is supported")
	var grantTypes stringSlice
	fs.Var(&grantTypes, "grant-type", "an OAuth grant type (repeatable; defaults to authorization_code + refresh_token)")
	var responseTypes stringSlice
	fs.Var(&responseTypes, "response-type", "an OAuth response type (repeatable; defaults to code)")
	clientName := fs.String("client-name", "", "cosmetic client name (optional)")
	clientURI := fs.String("client-uri", "", "client homepage URI (optional)")
	logoURI := fs.String("logo-uri", "", "client logo URI (optional)")
	scope := fs.String("scope", "", "space-delimited default scope (optional)")
	// --provider/--account-key name the OWNING TENANT, not a client-registration
	// forge: oauth_clients has no provider column since 0064 (#2437).
	provider := fs.String("provider", "", "owning tenant's forge discriminator (github|gitlab); pair with --account-key")
	accountKey := fs.String("account-key", "", "owning account's key; pair with --provider")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd oauth client register: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	meta, tenant, err := resolveOAuthClientRegistration(oauthClientRegistration{
		ClientID:                *clientID,
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: *authMethod,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		ClientName:              *clientName,
		ClientURI:               *clientURI,
		LogoURI:                 *logoURI,
		Scope:                   *scope,
		Provider:                *provider,
		AccountKey:              *accountKey,
	})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client register: %v\n", err)
		return exitUsage
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client register: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	accountID := ""
	if tenant.Present {
		// FAIL CLOSED: an unknown owning account is refused, never materialized —
		// mirroring `installation register`'s load-bearing control.
		acct, err := accountdb.New(pool).GetAccountByKey(ctx, accountdb.GetAccountByKeyParams{
			Provider:   tenant.Provider,
			AccountKey: tenant.AccountKey,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				_, _ = fmt.Fprintf(logSink,
					"fishhawkd oauth client register: account %s/%s not found; create it first with `fishhawkd account create --provider %s --account-key %s`\n",
					tenant.Provider, tenant.AccountKey, tenant.Provider, tenant.AccountKey)
			} else {
				_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client register: look up account %s/%s: %v\n", tenant.Provider, tenant.AccountKey, err)
			}
			return exitFailure
		}
		accountID = acct.ID.String()
	}

	repo := oauthstore.NewPostgresRepository(pool)
	client, err := repo.UpsertClient(ctx, oauthstore.NewClient{Metadata: meta, AccountID: accountID})
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client register: %v\n", err)
		return exitFailure
	}

	// Created vs refreshed is read from the RETURNED row: an INSERT stamps
	// first_seen_at and updated_at with the same transaction now(), while the DO
	// UPDATE moves only updated_at (UpsertClient preserves first_seen_at). So an
	// equal pair is a fresh registration; a moved updated_at is a refresh.
	verb := "created"
	if !client.FirstSeenAt.Equal(client.UpdatedAt) {
		verb = "refreshed"
	}
	_, _ = fmt.Printf("%s oauth client id=%s client_id=%s account_id=%s auth_method=%s grant_types=%s\n",
		verb, client.ID, client.ClientID, renderOAuthAccountID(client.AccountID),
		client.TokenEndpointAuthMethod, strings.Join(client.GrantTypes, ","))
	return exitOK
}

// sourcePreRegistered is the constant SOURCE value `oauth client list` renders
// for EVERY row, and it is the truth, not a stub. resolveOAuthClient
// deliberately performs no UpsertClient on the authorize hot path (persisting a
// CIMD-fetched document there would make the store-first branch shadow every
// later refresh and convert the fetcher's bounded TTL into a permanent pin), so
// this CLI is the table's ONLY writer and every row is pre-registered by
// construction. A `source` COLUMN is deliberately NOT added: it could hold only
// this one value — exactly the defect 0064 called out when it dropped `provider`
// rather than keeping a column a reader would believe. REVISIT if any future
// code path outside this CLI ever calls UpsertClient (a CIMD persister).
const sourcePreRegistered = "pre-registered"

func runOAuthClientList(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd oauth client list", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd oauth client list: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client list: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	clients, err := oauthstore.NewPostgresRepository(pool).ListClients(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client list: %v\n", err)
		return exitFailure
	}
	if len(clients) == 0 {
		_, _ = fmt.Println("no oauth client registrations")
		return exitOK
	}

	_, _ = fmt.Printf("%-40s  %-14s  %-36s  %-12s  %-20s  %-20s  %s\n",
		"CLIENT_ID", "SOURCE", "ACCOUNT_ID", "AUTH_METHOD", "FIRST_SEEN_AT", "UPDATED_AT", "REDIRECT_URIS")
	for _, c := range clients {
		_, _ = fmt.Printf("%-40s  %-14s  %-36s  %-12s  %-20s  %-20s  %s\n",
			c.ClientID, sourcePreRegistered, renderOAuthAccountID(c.AccountID), c.TokenEndpointAuthMethod,
			c.FirstSeenAt.UTC().Format(time.RFC3339), c.UpdatedAt.UTC().Format(time.RFC3339),
			strings.Join(c.RedirectURIs, ","))
	}
	return exitOK
}

func runOAuthClientRemove(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd oauth client remove", flag.ContinueOnError)
	fs.SetOutput(logSink)
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""), "postgres URL")
	clientID := fs.String("client-id", "", "client_id of the registration to remove — required")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dbURL == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd oauth client remove: --db (or FISHHAWKD_DATABASE_URL) required")
		return exitUsage
	}
	if strings.TrimSpace(*clientID) == "" {
		_, _ = fmt.Fprintln(logSink, "fishhawkd oauth client remove: --client-id required")
		return exitUsage
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, *dbURL)
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client remove: connect: %v\n", err)
		return exitFailure
	}
	defer pool.Close()

	err = oauthstore.NewPostgresRepository(pool).DeleteClient(ctx, *clientID)
	if errors.Is(err, oauthstore.ErrNotFound) {
		// A no-op delete reported as success would tell an operator they had
		// revoked a client's access when they had not.
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client remove: no oauth client registration with client_id %q\n", *clientID)
		return exitFailure
	}
	if err != nil {
		_, _ = fmt.Fprintf(logSink, "fishhawkd oauth client remove: %v\n", err)
		return exitFailure
	}
	_, _ = fmt.Printf("removed oauth client client_id=%s\n", *clientID)
	return exitOK
}

// renderOAuthAccountID renders a nullable account_id for operator output: an
// untenanted registration (empty string) shows a dash rather than a blank cell.
func renderOAuthAccountID(accountID string) string {
	if accountID == "" {
		return "-"
	}
	return accountID
}
