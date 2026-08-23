package campaigndriver

// E54.6 / #2238 AC3, enforced technically: the ratified grooming order is read
// EXACTLY ONCE, at campaign assembly, and NOTHING in the campaign driver re-reads
// a grooming report or a board afterwards.
//
// Why it is an invariant rather than a preference. The order that becomes a
// campaign's queue is a RATIFIED artifact: an operator approved that specific
// report at that specific content hash. A mid-campaign re-read would silently
// re-derive the queue from whatever the backlog looks like NOW — a second,
// unapproved source of truth for a decision the operator already made, and one
// that would change a running campaign's behaviour with no gate and no audit
// row. The order is therefore written down durably at assembly
// (campaign_items.queue_position + campaigns.grooming_source, migration 0074)
// and the driver reads only those rows.
//
// What this file establishes, precisely: no NON-TEST file in this package names
// a grooming-order or grooming-report read symbol, and no non-test file imports
// backend/internal/workmgmt (the board/tracker capability). The mid-campaign
// re-read is a code path that does not exist, and this fails the build if one
// appears.
//
// What it does NOT establish, stated rather than implied:
//
//   - It is a SOURCE-LEVEL check over THIS package only, tamper-evident within
//     the repo rather than a runtime capability boundary. A helper in another
//     package that this one calls could still read a report; the complementary
//     control is the counting-fake assertion in the server package, which proves
//     exactly one artifact read per create across a subsequent status read and a
//     full engine partition.
//   - It bans the reading VOCABULARY, not every conceivable spelling. A caller
//     that reached the same data through an interface whose method names avoid
//     all of these strings would pass. That is the accepted residual of an
//     AST-name check; the behavioural counting fake is what closes it.
//
// Today this package's non-test code names none of these symbols and imports
// workmgmt zero times, so the guard pins a real current state, not an aspiration.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// groomingReadSymbols are the identifiers whose appearance in this package's
// non-test code would mean a mid-campaign re-derivation of the ratified order.
// Matched by PREFIX, mirroring the mcpserver board-read guard's matcher.
var groomingReadSymbols = []string{
	// Parsing a grooming report inside the driver would mean re-deriving the
	// order from the artifact rather than reading the persisted queue.
	"ParseGroomingReport",
	"ValidateGroomingReport",
	"GroomingReport",
	// The artifact kind: fetching grooming_report artifacts at all.
	"KindGroomingReport",
	// The order derivation itself.
	"OrderFromReport",
	"ReorderByPriority",
	"GroomingOrder",
}

// forbiddenImportPrefix is the board/tracker capability. Any package under it
// pulls in the same read vocabulary.
const forbiddenImportPrefix = "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"

// TestNoGroomingReadInCampaignDriver walks every non-test .go file in this
// package and fails, naming file+symbol, if any names a grooming-read symbol.
func TestNoGroomingReadInCampaignDriver(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range nonTestGoFiles(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			for _, sym := range groomingReadSymbols {
				if strings.HasPrefix(id.Name, sym) {
					t.Errorf("%s:%d names %q: the campaign driver must never re-read the ratified grooming order — it is read exactly once at assembly and persisted (E54.6 / #2238 AC3)",
						name, fset.Position(id.Pos()).Line, id.Name)
					break
				}
			}
			return true
		})
	}
}

// TestCampaignDriverDoesNotImportWorkmgmt is the import-direction half: the
// driver must not reach the board/tracker capability at all, so it cannot
// acquire the read vocabulary under a different local name.
func TestCampaignDriverDoesNotImportWorkmgmt(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range nonTestGoFiles(t) {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == forbiddenImportPrefix || strings.HasPrefix(path, forbiddenImportPrefix+"/") {
				t.Errorf("%s imports %s: the campaign driver must not reach the board/tracker capability (E54.6 / #2238 AC3)", name, path)
			}
		}
	}
}

// nonTestGoFiles lists this package's non-test .go sources. It fails the test
// when the directory is unreadable or empty rather than passing vacuously — a
// guard that silently inspects nothing is worse than no guard.
func nonTestGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, e.Name())
	}
	if len(out) == 0 {
		t.Fatal("no non-test .go files found: this guard would pass vacuously")
	}
	return out
}
