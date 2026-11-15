package server

import (
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
)

// human_only_docs_test.go pins the operator-facing statement of ADR-143 D1
// (#5228): which credential an agent session uses, and that no bearer clears a
// server-check concern. The strings come from code where code holds them (the
// refusal code errCodeConcernRequiresSession and operatorrole's subject
// prefix), so renaming either fails here. A failure names the prose sites to
// update together:
//
//   - docs/OPERATING_MODEL.md § 6 "Credentials for agent sessions and
//     human-only clears";
//   - backend/internal/concern/README.md § "Server-check provenance slice";
//   - backend/internal/server/docs/review.md (the diff-secrets and
//     permission-drift human-only clearing bullets, the control_unpinned
//     "What it holds" bullet, and the resolve-with-evidence auth);
//   - backend/internal/server/docs/auth.md § "Credential-kind tiers".
//
// COUNTERFACTUAL (run by hand with -count=1, #5228 slice 3, both observed RED
// and restored): (1) deleting the "Which credential an agent session uses"
// table row naming operator-agent/<role-spec-version> from OPERATING_MODEL.md
// § 6 → RED "docs/OPERATING_MODEL.md no longer states
// \"operator-agent/<role-spec-version>\""; (2) renaming every
// concern_requires_session in review.md → RED "backend/internal/server/docs/
// review.md no longer states \"concern_requires_session\"".

// humanOnlyDocSites lists each prose site and the strings it must carry.
var humanOnlyDocSites = []struct {
	path  string
	wants []string
}{
	{path: "docs/OPERATING_MODEL.md", wants: []string{
		operatorrole.TokenSubjectPrefix + "<role-spec-version>",
		errCodeConcernRequiresSession,
		"Sign in to the Fishhawk Web UI through the forge",
	}},
	{path: "backend/internal/concern/README.md", wants: []string{errCodeConcernRequiresSession}},
	{path: "backend/internal/server/docs/review.md", wants: []string{errCodeConcernRequiresSession}},
	{path: "backend/internal/server/docs/auth.md", wants: []string{errCodeConcernRequiresSession}},
}

// TestHumanOnlyDocs_ServerCheckClearStatement fails when a prose site above no
// longer states the ADR-143 D1 credential rule for server-check clears.
func TestHumanOnlyDocs_ServerCheckClearStatement(t *testing.T) {
	for _, site := range humanOnlyDocSites {
		md := readRepoDoc(t, site.path)
		for _, want := range site.wants {
			if !strings.Contains(md, want) {
				t.Errorf("%s no longer states %q (ADR-143 D1, #5228). Update it together with the other sites: "+
					"docs/OPERATING_MODEL.md § 6, backend/internal/concern/README.md § \"Server-check provenance slice\", "+
					"backend/internal/server/docs/review.md and backend/internal/server/docs/auth.md § \"Credential-kind tiers\"",
					site.path, want)
			}
		}
	}
}
