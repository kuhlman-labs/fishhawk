package failuresig

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// catalogDocPath is the published operator-facing catalog, relative to this
// package directory (backend/internal/failuresig -> repository root).
const catalogDocPath = "../../../docs/architecture/failure-signatures.md"

// hintMaxBytes bounds a marshalled Hint. It is the named cap that LICENSES the
// tierNever byte classification of next_actions.signature in
// backend/internal/mcpserver/bound.go: because a Hint echoes only
// registry-owned strings, the block cannot grow with run data, so it can ride
// the response budget without an elision tier. A future field echoing caller
// input turns this test RED.
const hintMaxBytes = 2048

// signatureIDPattern is the id contract: a lowercase slug. Underscores (not
// hyphens) match the surrounding vocabulary — next_actions state labels and
// audit categories are all snake_case.
var signatureIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)

func TestRegistryIDsUniqueAndSlugged(t *testing.T) {
	seen := map[string]struct{}{}
	for _, sig := range Registry() {
		if !signatureIDPattern.MatchString(sig.ID) {
			t.Errorf("id %q is not a lowercase slug (%s)", sig.ID, signatureIDPattern)
		}
		if _, dup := seen[sig.ID]; dup {
			t.Errorf("duplicate id %q — the id is the join key the catalog doc and the operator both use", sig.ID)
		}
		seen[sig.ID] = struct{}{}
	}
}

func TestRegistryEntriesComplete(t *testing.T) {
	for _, sig := range Registry() {
		if sig.Title == "" {
			t.Errorf("%s: empty Title", sig.ID)
		}
		if sig.Means == "" {
			t.Errorf("%s: empty Means", sig.ID)
		}
		if len(sig.Playbook) == 0 {
			t.Errorf("%s: empty Playbook — a signature with no recovery steps carries no operator value", sig.ID)
		}
		for i, step := range sig.Playbook {
			if strings.TrimSpace(step) == "" {
				t.Errorf("%s: playbook step %d is blank", sig.ID, i)
			}
		}
		if sig.match == nil {
			t.Errorf("%s: nil matcher — the entry can never fire", sig.ID)
		}
	}
}

// TestRegistryCoversAtLeastFiveSignatures is the issue's done-means floor: a
// future deletion that drops the catalog below it fails loudly rather than
// silently shipping a stub.
func TestRegistryCoversAtLeastFiveSignatures(t *testing.T) {
	const floor = 5
	if n := len(Registry()); n < floor {
		t.Fatalf("catalog carries %d signatures, want at least %d (#1703 done-means)", n, floor)
	}
}

// TestRegistryVersionStamped pins that Match STAMPS the catalog revision onto
// production output, and that the revision is non-empty.
//
// Both halves are load-bearing and neither is covered by
// TestMatch_EverySignature, which compares the hint's field against the same
// constant and so would stay green if RegistryVersion were blanked. This test
// drives real evidence through Match — never a hand-built Hint — so deleting
// the stamp in Match turns it RED.
func TestRegistryVersionStamped(t *testing.T) {
	if RegistryVersion == "" {
		t.Fatal("RegistryVersion is empty — an unstamped hint names no catalog revision")
	}
	for _, tc := range matchFixtures() {
		got := Match(tc.ev)
		if got == nil {
			t.Fatalf("%s: Match returned nil, want %s", tc.name, tc.want)
		}
		if got.RegistryVersion == "" {
			t.Fatalf("%s: Match produced a hint with no registry_version", tc.want)
		}
		if got.RegistryVersion != RegistryVersion {
			t.Fatalf("%s: Match stamped %q, want %q", tc.want, got.RegistryVersion, RegistryVersion)
		}
	}
}

// TestRegistryReturnsFreshPlaybooksPerCall is the counterfactual vehicle for
// Registry()'s per-call reconstruction — the control that actually defends
// catalog immutability today (see TestMatch_ReturnsIndependentPlaybookCopies,
// which asserts the same property one layer up but cannot discriminate it).
// Change Registry() to hand out a shared package-level catalog and this goes
// RED.
func TestRegistryReturnsFreshPlaybooksPerCall(t *testing.T) {
	first := Registry()
	if len(first) == 0 || len(first[0].Playbook) == 0 {
		t.Fatal("catalog is empty")
	}
	original := first[0].Playbook[0]
	first[0].Playbook[0] = "MUTATED"

	second := Registry()
	if second[0].Playbook[0] != original {
		t.Fatalf("a mutation of one Registry() result reached the next: %q — the catalog must be rebuilt per call", second[0].Playbook[0])
	}
}

// TestMatchBlockIsConstantSize drives EVERY catalog entry through the
// PRODUCTION path — Match, on evidence a caller could really supply — and
// asserts the marshalled hint stays under the named cap AND does not grow when
// the same evidence carries a 200 KB failure reason. This is the control that
// licenses classifying next_actions.signature tierNever in the response byte
// ladder.
//
// Driving Match rather than a hand-built Hint is what makes the counterfactual
// attainable: the named regression — adding a caller-derived field to Hint and
// populating it in Match — leaves a hand-built Hint's new field empty and the
// assertion green. Padding the reason per fixture is what turns that same
// regression RED here rather than only in
// TestMatchBlockNeverEchoesEvidence.
func TestMatchBlockIsConstantSize(t *testing.T) {
	const pad = 200_000
	for _, tc := range matchFixtures() {
		got := Match(tc.ev)
		if got == nil {
			t.Fatalf("%s: Match returned nil, want %s", tc.name, tc.want)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.want, err)
		}
		if len(b) > hintMaxBytes {
			t.Errorf("%s: marshalled hint is %d bytes, cap is %d — the block must stay constant-size to ride the response budget untiered", tc.want, len(b), hintMaxBytes)
		}

		// The same evidence, with the reason padded far past the cap. The
		// anchors are substring contracts, so appended padding cannot change
		// which signature fires.
		padded := tc.ev
		padded.FailureReason += " " + strings.Repeat("x", pad)
		gotPadded := Match(padded)
		if gotPadded == nil {
			t.Fatalf("%s: Match on a padded reason returned nil", tc.want)
		}
		if gotPadded.ID != got.ID {
			t.Fatalf("%s: padding the reason changed the match to %q", tc.want, gotPadded.ID)
		}
		bPadded, err := json.Marshal(gotPadded)
		if err != nil {
			t.Fatalf("%s: marshal padded: %v", tc.want, err)
		}
		if len(bPadded) != len(b) {
			t.Errorf("%s: hint grew with the failure reason: %d bytes vs %d — the block must echo no caller text", tc.want, len(bPadded), len(b))
		}
	}
}

// TestMatchBlockNeverEchoesEvidence is the structural companion to the cap: a
// Hint produced from evidence carrying a very long failure reason must be the
// SAME size as one produced from a short reason. A field that echoed caller
// input would make the block grow with run data.
func TestMatchBlockNeverEchoesEvidence(t *testing.T) {
	short := failedEvidence("A", "terminal external API error 529 (retries exhausted)")
	long := failedEvidence("A", "terminal external API error 529 (retries exhausted): "+strings.Repeat("x", 200_000))

	a, err := json.Marshal(Match(short))
	if err != nil {
		t.Fatalf("marshal short: %v", err)
	}
	b, err := json.Marshal(Match(long))
	if err != nil {
		t.Fatalf("marshal long: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("hint size varies with the failure reason: %d vs %d bytes", len(a), len(b))
	}
	if len(b) > hintMaxBytes {
		t.Fatalf("hint from a 200KB reason is %d bytes, cap is %d", len(b), hintMaxBytes)
	}
}

// TestCatalogDocumentsEverySignature is the done-means test for "the registry
// is documented so external operators can read the catalog": it reads the
// published catalog and fails NAMING any registry id with no section.
func TestCatalogDocumentsEverySignature(t *testing.T) {
	raw, err := os.ReadFile(catalogDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", catalogDocPath, err)
	}
	doc := string(raw)
	var missing []string
	for _, sig := range Registry() {
		if !strings.Contains(doc, "\n### "+sig.ID) {
			missing = append(missing, sig.ID)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s has no `### <id>` section for: %s — every catalog entry must be documented for an operator with no dogfood history", catalogDocPath, strings.Join(missing, ", "))
	}
}

// TestCatalogDocumentsNoUnknownSignature is the reverse pin: a section for an
// id the registry no longer carries is a stale doc, which reads to an operator
// as a signature the product will produce and never does.
func TestCatalogDocumentsNoUnknownSignature(t *testing.T) {
	raw, err := os.ReadFile(catalogDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", catalogDocPath, err)
	}
	known := map[string]struct{}{}
	for _, sig := range Registry() {
		known[sig.ID] = struct{}{}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		id := strings.TrimSpace(strings.TrimPrefix(line, "### "))
		if idx := strings.Index(id, " "); idx >= 0 {
			id = id[:idx]
		}
		if _, ok := known[id]; !ok {
			t.Errorf("%s documents %q, which is not a registry id", catalogDocPath, id)
		}
	}
}

// TestAnchorsAreNonEmpty pins that no anchor is accidentally blanked — an
// empty anchor would make strings.Contains true for EVERY reason, turning a
// fail-open registry into an always-match one.
func TestAnchorsAreNonEmpty(t *testing.T) {
	anchors := map[string]string{
		"AnchorExternalAPIError":            AnchorExternalAPIError,
		"AnchorQuotaUnavailable":            AnchorQuotaUnavailable,
		"AnchorVerifyInfraFlake":            AnchorVerifyInfraFlake,
		"AnchorSliceIntegrationConflict":    AnchorSliceIntegrationConflict,
		"AnchorLineageLock":                 AnchorLineageLock,
		"AnchorRunnerExitedBeforeReporting": AnchorRunnerExitedBeforeReporting,
		"AnchorZeroExitStrand":              AnchorZeroExitStrand,
	}
	for name, v := range anchors {
		if v == "" {
			t.Errorf("%s is empty", name)
		}
	}
}
