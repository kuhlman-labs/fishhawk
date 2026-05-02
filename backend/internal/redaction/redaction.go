// Package redaction scans byte payloads for known secret patterns
// and replaces them with stable, non-sensitive markers (E2.4 / #25).
//
// Per MVP_SPEC §4.4, prompts and tool outputs surfaced through agent
// runs WILL contain secrets — API keys customers pass in, tokens
// agents stumble across, occasional Authorization headers in pasted
// HTTP traces. The audit log persists both the unredacted bytes
// (under stricter access control) and the redacted view (default-
// readable). This library produces the redacted view.
//
// The pattern set is intentionally conservative: each pattern below
// targets a specific, well-known credential format with low false-
// positive risk. Callers that need stricter rules pass their own
// patterns via Redact; the default set via RedactDefault.
//
// Hits returned alongside the redacted output drive telemetry —
// "the runner redacted N GitHub tokens this run" without ever
// surfacing the tokens themselves.
package redaction

import (
	"regexp"
	"sort"
)

// Pattern is one redaction rule.
type Pattern struct {
	// Name identifies the pattern in Hit telemetry. Stable across
	// versions of this package; rename only with a heads-up.
	Name string
	// Regex matches the secret bytes. The whole match (group 0) is
	// what gets replaced.
	Regex *regexp.Regexp
	// Replace is the replacement string. Defaults to
	// "[REDACTED:<name>]" when empty.
	Replace string
}

// Hit summarizes how often a pattern matched in a given Redact call.
// Surfaced for telemetry and for tests; the secrets themselves are
// gone by the time Hit is observed.
type Hit struct {
	Pattern string
	Count   int
}

// DefaultPatterns is the v0 set. Each pattern aims for a low false-
// positive rate against typical agent traces; the trade is that
// some secret formats outside this list will pass through. New
// formats land here as we encounter them.
//
// References: GitHub Token Formats (PAT, fine-grained), OpenAI API
// Keys, Anthropic API Keys (sk-ant-…), AWS Access Key IDs (AKIA…),
// Authorization Bearer headers, generic password/token/secret
// values inside JSON.
var DefaultPatterns = []Pattern{
	{
		Name:  "github-pat-classic",
		Regex: regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	},
	{
		Name:  "github-pat-fine-grained",
		Regex: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`),
	},
	{
		Name:  "github-app-token",
		Regex: regexp.MustCompile(`ghs_[A-Za-z0-9]{36}`),
	},
	{
		Name:  "openai-api-key",
		Regex: regexp.MustCompile(`sk-[A-Za-z0-9]{48}`),
	},
	{
		Name:  "openai-project-key",
		Regex: regexp.MustCompile(`sk-proj-[A-Za-z0-9_\-]{40,}`),
	},
	{
		Name:  "anthropic-api-key",
		Regex: regexp.MustCompile(`sk-ant-api03-[A-Za-z0-9_\-]{40,}`),
	},
	{
		Name:  "aws-access-key-id",
		Regex: regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	},
	{
		Name: "authorization-bearer",
		// Header name is case-insensitive in HTTP but case-sensitive
		// in JSON map keys; (?i) covers both. Captures the token
		// portion as the whole match, including the "Authorization:
		// Bearer " prefix, so the entire credential line goes away.
		Regex: regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9_\-\.~+/=]+`),
	},
	{
		Name: "json-password-field",
		// Matches "password": "anything-not-quote", "secret": "...",
		// "token": "...", "api_key": "...", "apikey": "...",
		// "access_token": "...", "refresh_token": "...". Group 0 is
		// the entire match, so the field key + value disappear into
		// the replacement marker.
		Regex: regexp.MustCompile(`(?i)"(password|secret|token|api[_\-]?key|access_token|refresh_token)"\s*:\s*"[^"]+"`),
	},
}

// Redact applies patterns to input in the order given, replacing
// every match with the pattern's Replace template (or the default
// "[REDACTED:<name>]" marker). Returns the redacted bytes plus a
// hits slice describing how many times each pattern fired —
// suitable for emitting as an audit-log event without leaking the
// secrets themselves.
//
// Patterns are applied in order and may overlap; later patterns see
// the output of earlier ones. Most use cases are insensitive to
// order because the default set's regexes don't share prefixes.
func Redact(input []byte, patterns []Pattern) ([]byte, []Hit) {
	if len(input) == 0 || len(patterns) == 0 {
		return input, nil
	}

	out := input
	hitMap := make(map[string]int, len(patterns))
	for _, p := range patterns {
		repl := p.Replace
		if repl == "" {
			repl = "[REDACTED:" + p.Name + "]"
		}
		matches := p.Regex.FindAll(out, -1)
		if len(matches) == 0 {
			continue
		}
		hitMap[p.Name] += len(matches)
		out = p.Regex.ReplaceAll(out, []byte(repl))
	}

	// Sort hits by pattern name for deterministic output. Counts
	// are accumulated above; sorting here doesn't affect totals.
	hits := make([]Hit, 0, len(hitMap))
	for name, n := range hitMap {
		hits = append(hits, Hit{Pattern: name, Count: n})
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Pattern < hits[j].Pattern
	})
	return out, hits
}

// RedactDefault is shorthand for Redact(input, DefaultPatterns). The
// runner uses this on the gzip-decompressed trace bytes before
// re-compressing the redacted variant for upload to S3.
func RedactDefault(input []byte) ([]byte, []Hit) {
	return Redact(input, DefaultPatterns)
}
