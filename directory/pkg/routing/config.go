// Package routing is the directory plane's HTTP surface (ADR-062, #1831):
// the operator-gated region-assignment endpoint and the 302 router that
// hands a caller off to the cell owning their account.
//
// The package is PUBLIC rather than internal on purpose. The cell's
// cross-boundary test drives this router directly, and Go's
// internal-package rule would stop that import at the module boundary.
// The dependency stays one-way: directory never imports backend.
package routing

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Environment variables the directory reads.
const (
	EnvRegions       = "FISHHAWK_DIRECTORY_REGIONS"
	EnvRoutedPaths   = "FISHHAWK_DIRECTORY_ROUTED_PATHS"
	EnvHandoffSecret = "FISHHAWK_DIRECTORY_HANDOFF_SECRET"
	EnvHandoffTTL    = "FISHHAWK_DIRECTORY_HANDOFF_TTL"
	EnvAdminToken    = "FISHHAWK_DIRECTORY_ADMIN_TOKEN"
)

// DefaultHandoffTTL bounds how long an issued handoff stays valid. It only
// has to cover one redirect hop, so it is deliberately short.
const DefaultHandoffTTL = 5 * time.Minute

// DefaultRoutedPath is the ONLY path routed by default.
//
// A routed surface must carry explicit account identity in its query, since
// that is the only thing the directory can resolve a region from. The OAuth
// login/callback pair does NOT qualify: a callback arrives from the forge
// carrying code+state and no account parameter, and the cell mints that
// state only AFTER the redirect, so the directory cannot pre-register a
// correlation either. Routing those surfaces needs a correlation design
// that does not exist yet; until it does, they are deliberately unrouted
// rather than guessed at.
const DefaultRoutedPath = "/v0/onboarding/start"

// ErrInvalidConfig is the typed startup failure. Every validation branch
// wraps it, so a caller can match the whole class with errors.Is.
var ErrInvalidConfig = errors.New("routing: invalid directory configuration")

// Config is the directory's whole runtime configuration.
//
// Region -> cell base URL lives HERE, in env, and not in the database: a
// cell can be re-homed without a migration, and a stale row can never point
// traffic at a decommissioned cell.
type Config struct {
	// Regions maps a region name to that region's cell base URL. Must be
	// non-empty; every URL must be absolute.
	Regions map[string]string
	// RoutedPaths are the cell paths the directory redirects for. Each is
	// preserved verbatim in the redirect target.
	RoutedPaths []string
	// HandoffSecret is the HMAC key shared with every cell. Required.
	HandoffSecret string
	// HandoffTTL is how long an issued handoff stays valid.
	HandoffTTL time.Duration
	// AdminToken is the operator credential gating BOTH directory
	// surfaces. An EMPTY value is not a startup error — it is a runtime
	// refusal: every request to both surfaces is answered 503. Failing
	// startup instead would tempt an operator to unset it to "disable
	// auth"; refusing at request time makes unset mean closed, never open.
	AdminToken string
}

// LoadConfig builds a Config from the given lookup function and validates
// it. getenv is injected so tests need not mutate process environment.
func LoadConfig(getenv func(string) string) (Config, error) {
	regions, err := parseRegions(getenv(EnvRegions))
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Regions:       regions,
		RoutedPaths:   parseRoutedPaths(getenv(EnvRoutedPaths)),
		HandoffSecret: getenv(EnvHandoffSecret),
		HandoffTTL:    DefaultHandoffTTL,
		AdminToken:    getenv(EnvAdminToken),
	}

	if raw := getenv(EnvHandoffTTL); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%w: %s=%q is not a duration: %v", ErrInvalidConfig, EnvHandoffTTL, raw, err)
		}
		cfg.HandoffTTL = d
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadConfigFromEnv is LoadConfig over the process environment.
func LoadConfigFromEnv() (Config, error) { return LoadConfig(os.Getenv) }

// Validate fails closed: every defect is a startup error, never a warning
// that leaves the process serving a half-configured router.
func (c Config) Validate() error {
	if len(c.Regions) == 0 {
		return fmt.Errorf("%w: %s is required (format: region=https://cell.example,…)", ErrInvalidConfig, EnvRegions)
	}
	for _, region := range sortedKeys(c.Regions) {
		if err := validateCellURL(region, c.Regions[region]); err != nil {
			return err
		}
	}
	if c.HandoffSecret == "" {
		return fmt.Errorf("%w: %s is required (there is no unsigned handoff mode)", ErrInvalidConfig, EnvHandoffSecret)
	}
	if c.HandoffTTL <= 0 {
		return fmt.Errorf("%w: %s must be positive, got %s", ErrInvalidConfig, EnvHandoffTTL, c.HandoffTTL)
	}
	if len(c.RoutedPaths) == 0 {
		return fmt.Errorf("%w: %s must name at least one path", ErrInvalidConfig, EnvRoutedPaths)
	}
	seen := make(map[string]bool, len(c.RoutedPaths))
	for _, p := range c.RoutedPaths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("%w: routed path %q must be absolute (start with /)", ErrInvalidConfig, p)
		}
		if seen[p] {
			return fmt.Errorf("%w: routed path %q is listed twice", ErrInvalidConfig, p)
		}
		seen[p] = true
	}
	return nil
}

// CellURL returns the base URL of the cell owning region, and whether the
// region is configured at all. An unknown region is a config fault the
// router reports as such; there is deliberately no default cell to fall
// back to, because falling back would route an account's traffic into a
// region that does not own it.
func (c Config) CellURL(region string) (string, bool) {
	u, ok := c.Regions[region]
	return u, ok
}

// parseRegions reads `region=url,region=url`. Whitespace around entries is
// tolerated; anything else is a startup error.
func parseRegions(raw string) (map[string]string, error) {
	regions := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, cellURL, ok := strings.Cut(entry, "=")
		name, cellURL = strings.TrimSpace(name), strings.TrimSpace(cellURL)
		if !ok || name == "" || cellURL == "" {
			return nil, fmt.Errorf("%w: %s entry %q is not region=url", ErrInvalidConfig, EnvRegions, entry)
		}
		if _, dup := regions[name]; dup {
			return nil, fmt.Errorf("%w: %s names region %q twice", ErrInvalidConfig, EnvRegions, name)
		}
		regions[name] = cellURL
	}
	return regions, nil
}

// parseRoutedPaths reads a comma-separated list, falling back to the single
// default surface when unset.
func parseRoutedPaths(raw string) []string {
	var paths []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return []string{DefaultRoutedPath}
	}
	return paths
}

// validateCellURL requires an absolute http(s) URL with a host and no query
// or fragment — the router appends the caller's own path and query to it,
// and either would be silently dropped.
func validateCellURL(region, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: region %q cell URL %q is unparsable: %v", ErrInvalidConfig, region, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: region %q cell URL %q must be absolute http(s)", ErrInvalidConfig, region, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: region %q cell URL %q has no host", ErrInvalidConfig, region, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: region %q cell URL %q must not carry a query or fragment", ErrInvalidConfig, region, raw)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
