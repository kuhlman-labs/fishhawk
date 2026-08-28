package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// The CLI conventions read for the static charter check (E54.11 / #2801).
//
// This is a NARROW, charter-only read, not a second implementation of the
// work-management schema. It resolves the conventions file as a sibling of the
// validated spec, validates it against the canonical schema the CLI now mirrors
// (spec.ValidateConventionsDocument), and decodes ONLY charter.path. Every OTHER
// conventions rule stays backend-owned; the CLI never becomes a second author of
// the work-management rules.

// conventionsFileName is the work-management conventions file, resolved as a
// sibling of the validated spec — the conventional `.fishhawk/` layout puts
// `work-management.yaml` beside `workflows.yaml`.
const conventionsFileName = "work-management.yaml"

// defaultCharterPath is the charter path the SHIPPED DEFAULT conventions declare
// (docs/spec/work-management-default.yaml). A repo with no committed
// `work-management.yaml` inherits that default, so an absent file means "charter
// declared at this path". TestDefaultCharterPathMatchesShippedDefault pins the
// coupling, so this can never silently drift into a fail-open.
const defaultCharterPath = ".fishhawk/charter.md"

// loadCharterDeclaration resolves the repo's work-management conventions as a
// sibling of specPath and reports whether a charter is declared and its path. It
// mirrors the server loader's four outcomes:
//
//	(i)   file ABSENT (fs.ErrNotExist) -> the shipped default applies, which
//	      DECLARES charter.path = defaultCharterPath, so declared=true with that
//	      path. The analogue of the server loader's forge.ErrNotFound ->
//	      workmgmt.Default() fallback; it is what keeps THIS repo (which ships no
//	      work-management.yaml) validating green.
//	(ii)  file present but UNREADABLE (any other read error) -> err (fail closed).
//	(iii) file present but not a schema-valid single YAML document -> err (fail
//	      closed), mirroring the loader's "a committed-but-invalid file is an
//	      operator error to surface" posture.
//	(iv)  file present and valid -> decode ONLY charter.path and report declared
//	      = (Charter != nil) with the path VERBATIM (NOT trimmed — trimming is
//	      CharterAdmissionReason's job, so the empty-path branch stays distinct).
//
// A non-nil err is always the CONVENTIONS-UNAVAILABLE outcome for
// spec.CharterAdmissionReason; the absent-file admit returns a nil err.
func loadCharterDeclaration(specPath string) (declared bool, charterPath string, err error) {
	conventionsPath := filepath.Join(filepath.Dir(specPath), conventionsFileName)
	data, readErr := os.ReadFile(conventionsPath) //nolint:gosec // sibling of the user-supplied spec path is the point
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			// (i) No committed conventions file: the shipped default applies.
			return true, defaultCharterPath, nil
		}
		// (ii) present but unreadable -> fail closed.
		return false, "", readErr
	}

	// (iii) Validate against the canonical work-management-v0 schema (schema +
	// single-document) BEFORE the narrow read, so a conventions file run
	// admission would refuse with conventions_unavailable fails closed here too.
	// The backend's cross-field semantic checks are NOT reproduced — see
	// spec.ValidateConventionsDocument for the deliberate, documented residual.
	if verr := spec.ValidateConventionsDocument(data); verr != nil {
		return false, "", verr
	}

	// (iv) Decode ONLY charter.path. The schema has already accepted the shape,
	// so this only fails on an internal decode bug; treat any failure as fail
	// closed rather than a silent admit.
	var doc struct {
		Charter *struct {
			Path string `yaml:"path"`
		} `yaml:"charter"`
	}
	if derr := yaml.Unmarshal(data, &doc); derr != nil {
		return false, "", derr
	}
	if doc.Charter == nil {
		return false, "", nil
	}
	return true, doc.Charter.Path, nil
}
