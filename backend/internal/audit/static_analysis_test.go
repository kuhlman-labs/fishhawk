package audit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoAuditMutationsOutsideAuditPackage is the static-analysis
// guard that backs up the append-only invariant. It walks every Go
// and SQL file under /backend (the workspace's only Go module so
// far) and asserts none contains UPDATE / DELETE / TRUNCATE
// statements targeting audit_entries — except in this package,
// which has legitimate trigger tests, and except in the migrations
// directory, which legitimately runs DROP TABLE on rollback.
//
// Layered with the schema-level triggers (refusing UPDATE/DELETE
// at runtime) and the Repository interface (no Update/Delete
// method exists), this test ensures a developer can't accidentally
// add a forbidden mutation without CI catching it at compile-time
// of the test suite.
func TestNoAuditMutationsOutsideAuditPackage(t *testing.T) {
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bUPDATE\s+audit_entries\b`),
		regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+audit_entries\b`),
		regexp.MustCompile(`(?i)\bTRUNCATE\s+(?:TABLE\s+)?audit_entries\b`),
	}

	// Walk from backend/ root. CWD during `go test` is the package
	// directory (backend/internal/audit/), so two parents up.
	backendRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	skipDirNames := map[string]struct{}{
		"node_modules": {},
		".git":         {},
	}

	auditPkgRoot := filepath.Join(backendRoot, "internal", "audit")
	migrationsRoot := filepath.Join(backendRoot, "internal", "postgres", "migrations")
	// auditrehash is the canonical-hash data-migration package (#302).
	// It legitimately UPDATEs audit_entries inside a transaction that
	// temporarily disables the append-only triggers, walking every
	// chain in sequence order to rewrite entry_hash + prev_hash under
	// the new canonical form. The append-only invariant is preserved
	// at every visible boundary — only the rehash transaction
	// relaxes it, and rollback restores the triggers if anything
	// fails. Treated like the audit package itself: exempt from the
	// scan, gated by code review.
	auditRehashRoot := filepath.Join(backendRoot, "internal", "auditrehash")

	var violations []string
	walkErr := filepath.WalkDir(backendRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, skip := skipDirNames[d.Name()]; skip {
				return filepath.SkipDir
			}
			// audit package owns the legitimate trigger-blocks
			// tests (which use UPDATE / DELETE deliberately) and
			// the audit-write code paths.
			if path == auditPkgRoot {
				return filepath.SkipDir
			}
			// Migrations legitimately DROP TABLE on rollback. They
			// don't contain UPDATE / DELETE statements at the row
			// level (we checked when authoring 0002), but skip the
			// dir wholesale to keep the rule unambiguous.
			if path == migrationsRoot {
				return filepath.SkipDir
			}
			// auditrehash is the one-shot canonical-hash migration
			// (#302). See comment above the path definition.
			if path == auditRehashRoot {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".sql" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(backendRoot, path)
		for _, re := range forbidden {
			if re.Match(data) {
				violations = append(violations, rel+": matches "+re.String())
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Errorf("audit_entries is mutated outside the audit package:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
