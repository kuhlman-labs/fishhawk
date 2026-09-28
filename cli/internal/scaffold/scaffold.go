// Package scaffold renders and writes the three governance documents
// `fishhawk init` adds to a repository alongside its workflow spec: the
// human-authored charter SKELETON (.fishhawk/charter.md), the thin operator
// overlay (.fishhawk/operator.yaml) and the work-management config
// (.fishhawk/work-management.yaml) the grooming gate requires (E74.3 /
// #3718).
//
// The templates are embedded package assets. They are the bytes the CLI
// writes into a CALLER's repository at runtime; nothing here writes into the
// Fishhawk repository's own .fishhawk/ tree.
//
// The package is mirrored into backend/internal/scaffold so the MCP
// fishhawk_init tool can return the identical file set from the backend
// module: the backend and CLI are separate Go modules that cannot import
// one another (the module wall cli/internal/bridge documents). Template
// drift across the two copies is guarded by a byte-parity test in the
// backend copy, not by convention.
//
// Two halves:
//
//   - Files (pure, no I/O) renders the three documents for an Options and
//     self-validates them, failing closed: the work-management bytes must
//     pass the work-management-v0 schema and the charter bytes must carry
//     every rubric group under RubricRowPattern.
//   - EnsureFiles writes each rendered document under a root ONLY when the
//     path does not already exist, reporting created / skipped-existing per
//     path. It has no force parameter at all, so no caller flag can make it
//     overwrite a charter a human has already written.
package scaffold

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// Repo-relative target paths of the three scaffolded documents.
const (
	CharterPath        = ".fishhawk/charter.md"
	OperatorPath       = ".fishhawk/operator.yaml"
	WorkManagementPath = ".fishhawk/work-management.yaml"
)

// Work-management providers init can target.
const (
	ProviderGitHubProjects = "github_projects"
	ProviderGitLab         = "gitlab"
)

// RubricRowPattern is the charter rubric-row regexp source. It is the SAME
// string as backend/internal/intakegroom/score.go's rubricRow — the grooming
// reader's parser — held equal across the module wall by
// TestRubricRowParityAcrossModules, which reads that file and requires this
// value verbatim. A row is a leading table cell holding a bolded id, then a
// non-empty line cell: `| **V1** | ... |`.
const RubricRowPattern = `^\s*\|\s*\*\*([A-Za-z]+[0-9]+)\*\*\s*\|\s*(.+?)\s*\|\s*$`

var rubricRow = regexp.MustCompile(RubricRowPattern)

// rubricGroups are the id prefixes the charter content contract requires
// (docs/spec/work-management-v0.md): value, risk, dependency-unblocking and
// staleness.
var rubricGroups = []string{"V", "R", "U", "S"}

// Template seams. Package-level so a test can substitute a template the
// shipped assets never exercise (an invalid work-management document) and
// observe that Files fails closed.
var (
	//go:embed templates/charter.md
	charterTemplate []byte
	//go:embed templates/operator.yaml
	operatorTemplate []byte
	//go:embed templates/work-management.yaml
	workManagementTemplate []byte
)

// Options selects how the templates render.
type Options struct {
	// Provider is ProviderGitHubProjects or ProviderGitLab.
	Provider string
	// Autonomy is the operator overlay's autonomy knob preset: low,
	// medium or high.
	Autonomy string
	// ProjectOwner is the GitHub Projects owner login. Empty renders a
	// fill-me-in marker and leaves the project block commented.
	ProjectOwner string
	// ProjectOwnerType is user or organization; empty means user.
	ProjectOwnerType string
	// ProjectNumber is the GitHub Projects number. Zero leaves the project
	// block commented with a required-fill marker: a plausible-looking
	// number would point at an unrelated board, which fails silently.
	ProjectNumber int
	// GitLabProject is the optional namespaced GitLab project path. Empty
	// means the filing repo's own owner/name path.
	GitLabProject string
}

// Missing names the work-management fields the rendered config leaves for
// the operator to fill, e.g. "project.number". Empty when the provider
// connection is complete.
func (o Options) Missing() []string {
	if o.Provider != ProviderGitHubProjects {
		return nil
	}
	var out []string
	if o.ProjectOwner == "" {
		out = append(out, "project.owner")
	}
	if o.ProjectNumber < 1 {
		out = append(out, "project.number")
	}
	return out
}

// Paths returns the scaffolded paths in write/report order.
func Paths() []string {
	return []string{CharterPath, OperatorPath, WorkManagementPath}
}

// RubricIDs returns the ordered rubric ids the charter markdown carries
// under RubricRowPattern — the row shape the grooming reader parses.
func RubricIDs(markdown string) []string {
	var ids []string
	for _, line := range strings.Split(markdown, "\n") {
		if m := rubricRow.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// Files renders the three documents keyed by repo-relative path. It
// performs no I/O and fails closed on an unknown option value or on a
// rendered document that does not validate.
func Files(opts Options) (map[string][]byte, error) {
	switch opts.Autonomy {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("scaffold: unknown autonomy %q (want low, medium or high)", opts.Autonomy)
	}
	connection, err := renderConnection(opts)
	if err != nil {
		return nil, err
	}

	operator, err := render(OperatorPath, operatorTemplate, map[string]string{
		"{{autonomy}}": opts.Autonomy,
	})
	if err != nil {
		return nil, err
	}
	workMgmt, err := render(WorkManagementPath, workManagementTemplate, map[string]string{
		"{{provider}}":   opts.Provider,
		"{{connection}}": connection,
	})
	if err != nil {
		return nil, err
	}
	if err := spec.ValidateConventionsDocument(workMgmt); err != nil {
		return nil, fmt.Errorf("scaffold: rendered %s is invalid: %w", WorkManagementPath, err)
	}
	charter := append([]byte(nil), charterTemplate...)
	if err := checkCharterRubric(charter); err != nil {
		return nil, err
	}
	return map[string][]byte{
		CharterPath:        charter,
		OperatorPath:       operator,
		WorkManagementPath: workMgmt,
	}, nil
}

// checkCharterRubric requires at least one rubric row in every group the
// content contract names.
func checkCharterRubric(charter []byte) error {
	seen := map[string]bool{}
	for _, id := range RubricIDs(string(charter)) {
		seen[strings.TrimRight(id, "0123456789")] = true
	}
	for _, g := range rubricGroups {
		if !seen[g] {
			return fmt.Errorf("scaffold: %s carries no %s* rubric row in the `| **%s1** | ... |` shape", CharterPath, g, g)
		}
	}
	return nil
}

// renderConnection renders the provider connection block.
func renderConnection(opts Options) (string, error) {
	switch opts.Provider {
	case ProviderGitLab:
		if opts.GitLabProject == "" {
			return "# GitLab connection. No project override: filed issues land in this\n" +
				"# repository's own owner/name project path.\n" +
				"gitlab: {}", nil
		}
		return "# GitLab connection: the namespaced project filed issues land in.\n" +
			"gitlab:\n  project: " + yamlQuote(opts.GitLabProject), nil
	case ProviderGitHubProjects:
		ownerType := opts.ProjectOwnerType
		if ownerType == "" {
			ownerType = "user"
		}
		if ownerType != "user" && ownerType != "organization" {
			return "", fmt.Errorf("scaffold: unknown project owner type %q (want user or organization)", ownerType)
		}
		owner := "FILL ME IN: the Project owner login"
		if opts.ProjectOwner != "" {
			owner = opts.ProjectOwner
		}
		if missing := opts.Missing(); len(missing) > 0 {
			number := "FILL ME IN: the integer in the Project URL"
			if opts.ProjectNumber >= 1 {
				number = fmt.Sprint(opts.ProjectNumber)
			}
			return "# REQUIRED — FILL ME IN (" + strings.Join(missing, ", ") + "): provider github_projects\n" +
				"# needs a project connection block. Grooming and issue filing fail closed\n" +
				"# until it is uncommented and filled.\n" +
				"# project:\n" +
				"#   owner: " + owner + "\n" +
				"#   owner_type: " + ownerType + "  # user | organization\n" +
				"#   number: " + number, nil
		}
		return "# GitHub Projects connection. owner_type is user or organization.\n" +
			"project:\n" +
			"  owner: " + yamlQuote(owner) + "\n" +
			"  owner_type: " + ownerType + "\n" +
			"  number: " + fmt.Sprint(opts.ProjectNumber), nil
	default:
		return "", fmt.Errorf("scaffold: unknown provider %q (want %s or %s)", opts.Provider, ProviderGitHubProjects, ProviderGitLab)
	}
}

// yamlQuote renders s as a double-quoted YAML scalar.
func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// render substitutes every placeholder and fails closed when any `{{`
// placeholder survives.
func render(path string, tmpl []byte, vars map[string]string) ([]byte, error) {
	out := string(tmpl)
	for k, v := range vars {
		out = strings.ReplaceAll(out, k, v)
	}
	if i := strings.Index(out, "{{"); i >= 0 {
		end := strings.Index(out[i:], "}}")
		if end < 0 {
			end = len(out) - i
		} else {
			end += 2
		}
		return nil, fmt.Errorf("scaffold: %s: unrendered placeholder %q", path, out[i:i+end])
	}
	return []byte(out), nil
}

// Status is the per-path outcome of EnsureFiles.
type Status string

// Per-path statuses.
const (
	StatusCreated         Status = "created"
	StatusSkippedExisting Status = "skipped-existing"
)

// FileResult is one path's outcome.
type FileResult struct {
	Path   string
	Status Status
}

// Result lists EnsureFiles outcomes in Paths() order.
type Result struct {
	Files []FileResult
}

// EnsureFiles renders the documents and writes each under root only when
// the path does not already exist. It NEVER rewrites an existing path, and
// it has no force parameter, so a caller flag cannot reach an existing
// charter.
func EnsureFiles(root string, opts Options) (Result, error) {
	files, err := Files(opts)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, rel := range Paths() {
		st, err := ensureOne(filepath.Join(root, filepath.FromSlash(rel)), files[rel])
		if err != nil {
			return res, fmt.Errorf("scaffold: %s: %w", rel, err)
		}
		res.Files = append(res.Files, FileResult{Path: rel, Status: st})
	}
	return res, nil
}

// ensureOne creates path with data unless something already exists there.
// The existence check and the create are ONE operation (O_EXCL), so there is
// no check-then-write window in which a concurrently written file is
// clobbered; anything already at the path — a file, a directory, a symlink
// — is reported skipped-existing and left untouched.
func ensureOne(path string, data []byte) (Status, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // 0644 is the intended mode for checked-in config
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return StatusSkippedExisting, nil
		}
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return StatusCreated, nil
}
