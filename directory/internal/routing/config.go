// Package routing resolves an account to its home region's cell and
// redirects the browser there (ADR-062, E44.7 / #1831).
//
// The directory never proxies: it emits an HTTP 302 so the request body
// (and the OAuth/App-install credentials it carries) never transits the
// global plane. See router.go for the redirect contract and config.go for
// the region → cell_base_url configuration, which is the SINGLE source of
// truth for cell endpoints — the store maps (provider, account_key) →
// home_region only.
package routing

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Environment variables consumed by LoadConfig. The comma-split shape
// follows the FISHHAWKD_IMPLEMENT_ALLOWED_MODELS precedent.
const (
	EnvSupportedRegions = "FISHHAWK_DIRECTORY_SUPPORTED_REGIONS"
	EnvCellBaseURLs     = "FISHHAWK_DIRECTORY_CELL_BASE_URLS"
	EnvHandoffSecret    = "FISHHAWK_DIRECTORY_HANDOFF_SECRET"
	EnvHandoffTTL       = "FISHHAWK_DIRECTORY_HANDOFF_TTL"
)

// DefaultHandoffTTL is the short lifetime of a signed region pin. Short
// because the pin only has to survive one browser redirect hop.
const DefaultHandoffTTL = 2 * time.Minute

// ErrNoCellForRegion is returned by Resolve when a region has no
// configured cell. It is the fail-closed signal: the directory reports an
// explicit routing error rather than falling through to some other
// region's cell.
var ErrNoCellForRegion = errors.New("routing: no cell configured for region")

// Config is the directory's routing configuration: the supported region
// list plus each region's cell base URL, and the handoff signing secret.
type Config struct {
	// SupportedRegions is the set of regions an account may be assigned
	// to, in configured order.
	SupportedRegions []string
	// CellBaseURLs maps region → that region's cell base URL. Every
	// supported region has an entry (enforced at load).
	CellBaseURLs map[string]string
	// HandoffSecret is the HMAC secret shared with every cell.
	HandoffSecret string
	// HandoffTTL bounds the lifetime of a signed region pin.
	HandoffTTL time.Duration
}

// LoadConfig parses the routing configuration from the environment,
// reading through getenv so tests never mutate process state.
//
// It fails closed on every incomplete configuration rather than starting
// a directory that could route somewhere wrong:
//   - no supported regions,
//   - no cell base URLs,
//   - a malformed region=url pair,
//   - a base URL for a region not in the supported list,
//   - a supported region with no base URL,
//   - a base URL that is not an absolute http(s) URL,
//   - no handoff secret,
//   - an unparseable or non-positive handoff TTL.
func LoadConfig(getenv func(string) string) (Config, error) {
	regions := splitList(getenv(EnvSupportedRegions))
	if len(regions) == 0 {
		return Config{}, fmt.Errorf("routing: %s is required (comma-separated, e.g. \"us,eu,au\")", EnvSupportedRegions)
	}
	supported := make(map[string]bool, len(regions))
	for _, r := range regions {
		supported[r] = true
	}

	pairs := splitList(getenv(EnvCellBaseURLs))
	if len(pairs) == 0 {
		return Config{}, fmt.Errorf("routing: %s is required (comma-separated region=url pairs)", EnvCellBaseURLs)
	}
	cells := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		region, raw, ok := strings.Cut(pair, "=")
		region = strings.ToLower(strings.TrimSpace(region))
		raw = strings.TrimSpace(raw)
		if !ok || region == "" || raw == "" {
			return Config{}, fmt.Errorf("routing: %s entry %q is not a region=url pair", EnvCellBaseURLs, pair)
		}
		if !supported[region] {
			return Config{}, fmt.Errorf("routing: %s names region %q which is not in %s", EnvCellBaseURLs, region, EnvSupportedRegions)
		}
		if err := validateBaseURL(raw); err != nil {
			return Config{}, fmt.Errorf("routing: %s entry for region %q: %w", EnvCellBaseURLs, region, err)
		}
		cells[region] = strings.TrimRight(raw, "/")
	}
	var missing []string
	for _, r := range regions {
		if _, ok := cells[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("routing: supported region(s) %s have no cell base URL in %s", strings.Join(missing, ","), EnvCellBaseURLs)
	}

	secret := strings.TrimSpace(getenv(EnvHandoffSecret))
	if secret == "" {
		return Config{}, fmt.Errorf("routing: %s is required to sign region handoffs", EnvHandoffSecret)
	}

	ttl := DefaultHandoffTTL
	if raw := strings.TrimSpace(getenv(EnvHandoffTTL)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("routing: %s %q: %w", EnvHandoffTTL, raw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("routing: %s must be positive, got %s", EnvHandoffTTL, d)
		}
		ttl = d
	}

	return Config{
		SupportedRegions: regions,
		CellBaseURLs:     cells,
		HandoffSecret:    secret,
		HandoffTTL:       ttl,
	}, nil
}

// Supports reports whether region is in the configured supported list.
func (c Config) Supports(region string) bool {
	for _, r := range c.SupportedRegions {
		if r == region {
			return true
		}
	}
	return false
}

// Resolve returns the cell base URL for region, or ErrNoCellForRegion.
//
// LoadConfig guarantees every SUPPORTED region has a cell, so this fails
// only for a region that is recorded in the store but has since been
// dropped from the configuration — exactly the case that must error
// loudly instead of falling through to a wrong-region cell.
func (c Config) Resolve(region string) (string, error) {
	base, ok := c.CellBaseURLs[normalizeRegion(region)]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoCellForRegion, region)
	}
	return base, nil
}

// splitList splits a comma-separated env value, trimming and lowercasing
// nothing but whitespace-only entries away. Region tokens are lowercased;
// url pairs keep their case after the "=" (handled by the caller).
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "=") {
			part = strings.ToLower(part)
		}
		out = append(out, part)
	}
	return out
}

// normalizeRegion canonicalizes a region token for lookup.
func normalizeRegion(region string) string {
	return strings.ToLower(strings.TrimSpace(region))
}

// validateBaseURL rejects anything that is not an absolute http(s) URL —
// a relative or scheme-less "base URL" would produce a redirect Location
// that silently stays on the directory.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}
