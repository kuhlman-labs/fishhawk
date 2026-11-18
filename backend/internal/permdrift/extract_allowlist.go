package permdrift

import (
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// DiffSecretsAllowlistPrefix is the key prefix of every diff-secrets
// allowlist grant.
const DiffSecretsAllowlistPrefix = "diff_secrets_allowlist."

// ExtractDiffSecretsAllowlist extracts the diff-secrets allowlist
// (diffsecrets.AllowlistPath, ADR-084 A2 / #5230). It parses with
// diffsecrets.ParseAllowlist over redaction.DefaultPatterns, so a whole-file
// error is returned (Detect then reports parse_error and fails closed). Per
// ACCEPTED rule and per (glob, pattern) it emits one presence Grant keyed
//
//	diff_secrets_allowlist.<entries|markers>.<id>.<RuleFingerprint(list, glob, pattern)>
//
// so a new glob, pattern or rule is a widening and a removed one a narrowing;
// a reason-only edit is no change, and an id rename reads as one added and one
// removed (raised, conservatively). The glob itself never enters a key, a
// Value or a note: a rule is attributed by its validated id and the
// fingerprint only. A rejected rule suppresses nothing, so it emits nothing;
// a credential-shaped glob added to the file is still raised by the diff
// secrets scan of that added line, which Partition never suppresses.
func ExtractDiffSecretsAllowlist(content []byte) (Grants, error) {
	out := Grants{}
	al, _, err := diffsecrets.ParseAllowlist(content, redaction.DefaultPatterns)
	if err != nil {
		return nil, err
	}
	emit := func(list string, rules []diffsecrets.Rule) {
		for _, r := range rules {
			for _, g := range r.Paths {
				for _, p := range r.Patterns {
					k := DiffSecretsAllowlistPrefix + list + "." + keySegment(r.ID) + "." + diffsecrets.RuleFingerprint(list, g, p)
					out.Put(Entry{Key: k, Value: Present, Rank: PresenceRank, Polarity: Grant})
				}
			}
		}
	}
	emit(diffsecrets.ListEntries, al.Entries)
	emit(diffsecrets.ListMarkers, al.Markers)
	return out, nil
}
