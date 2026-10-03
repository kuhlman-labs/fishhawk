package permdrift

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ManifestPermissionPrefix and ManifestEventPrefix are the key prefixes of a
// GitHub App manifest's grants. The Go-source manifest extractor emits the
// SAME keys, so the JSON template and the Go literal compare identically.
const (
	ManifestPermissionPrefix = "default_permissions."
	ManifestEventPrefix      = "default_events."
)

// manifestDoc is the slice of a GitHub App manifest this extractor reads.
// Every other manifest key is ignored.
type manifestDoc struct {
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// ExtractManifest extracts a GitHub App manifest's grants (JSON, as
// docs/github-app/manifest.template.json):
//
//   - `default_permissions.<name>`: a ranked Grant on AppLevels
//     (read < write < admin); `none` grants nothing;
//   - `default_events.<event>`: a presence Grant (subscribing to an event
//     adds received data).
//
// Empty content (an absent file) yields an empty set. JSON that does not
// parse, a wrongly-typed field and an unrecognized level are errors.
func ExtractManifest(content []byte) (Grants, error) {
	out := Grants{}
	if len(bytes.TrimSpace(content)) == 0 {
		return out, nil
	}
	var doc manifestDoc
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("app manifest: %w", err)
	}
	if err := AddManifestGrants(out, doc.DefaultPermissions, doc.DefaultEvents); err != nil {
		return nil, err
	}
	return out, nil
}

// AddManifestGrants records manifest permissions and events into out. It is
// the one place the manifest key shape and level ranking live, shared by the
// JSON extractor and the Go-source one. Permission and event names are
// FILE-DERIVED segments, escaped with keySegment: a permission literally
// named "*" keys as `default_permissions.%2A`, an ordinary key, never a
// wildcard that would subsume a sibling permission's widening.
func AddManifestGrants(out Grants, permissions map[string]string, events []string) error {
	for name, level := range permissions {
		r, err := AppLevels.mustRank(ManifestPermissionPrefix+name, level)
		if err != nil {
			return fmt.Errorf("app manifest: %w", err)
		}
		if r == 0 {
			continue
		}
		out.Put(Entry{Key: ManifestPermissionPrefix + keySegment(name), Value: level, Rank: r, Polarity: Grant})
	}
	for _, ev := range events {
		out.Put(Entry{Key: ManifestEventPrefix + keySegment(ev), Value: Present, Rank: PresenceRank, Polarity: Grant})
	}
	return nil
}
