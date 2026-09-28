package scaffold

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

func githubOpts() Options {
	return Options{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "acme", ProjectNumber: 4}
}

func mustFiles(t *testing.T, opts Options) map[string][]byte {
	t.Helper()
	files, err := Files(opts)
	if err != nil {
		t.Fatalf("Files(%+v): %v", opts, err)
	}
	return files
}

// wantRubricIDs is the ordered id set the shipped charter skeleton carries.
var wantRubricIDs = []string{
	"V1", "V2", "V3", "V4", "V5",
	"R1", "R2", "R3", "R4", "R5",
	"U1", "U2", "U3", "U4",
	"S1", "S2", "S3", "S4", "S5",
}

// TestScaffoldTemplateParityAcrossModules (C5) holds every template
// byte-identical to its cli/internal/scaffold/templates/ sibling. The two
// modules cannot import each other, so this reads the CLI copy over the repo
// root. It also requires the two directories to carry the SAME file set, so a
// template added to one copy only fails here too.
func TestScaffoldTemplateParityAcrossModules(t *testing.T) {
	cliDir := filepath.Join("..", "..", "..", "cli", "internal", "scaffold", "templates")
	names := func(dir string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	backendNames, cliNames := names("templates"), names(cliDir)
	if !reflect.DeepEqual(backendNames, cliNames) {
		t.Fatalf("template sets differ: backend/internal/scaffold/templates=%v cli/internal/scaffold/templates=%v", backendNames, cliNames)
	}
	for _, name := range backendNames {
		ours, err := os.ReadFile(filepath.Join("templates", name))
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := os.ReadFile(filepath.Join(cliDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ours, theirs) {
			t.Errorf("backend/internal/scaffold/templates/%s differs from cli/internal/scaffold/templates/%s: edit both copies together", name, name)
		}
	}
	// The embedded seams must be exactly the on-disk template files, so the
	// parity above is parity of what Files actually renders from.
	for name, embedded := range map[string][]byte{
		"charter.md":           charterTemplate,
		"operator.yaml":        operatorTemplate,
		"work-management.yaml": workManagementTemplate,
	} {
		disk, err := os.ReadFile(filepath.Join("templates", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(disk, embedded) {
			t.Errorf("embedded %s differs from templates/%s", name, name)
		}
	}
}

// TestRubricRowParityAcrossModules holds this copy's RubricRowPattern equal
// to the CLI copy's const, read over the repo root. The CLI copy is in turn
// held to the grooming reader's source; together the three agree.
func TestRubricRowParityAcrossModules(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "cli", "internal", "scaffold", "scaffold.go"))
	if err != nil {
		t.Fatalf("read CLI scaffold: %v", err)
	}
	want := "const RubricRowPattern = `" + RubricRowPattern + "`"
	if !strings.Contains(string(src), want) {
		t.Fatalf("cli/internal/scaffold/scaffold.go no longer declares %s: keep the two copies equal", want)
	}
}

// TestCharterTemplateParsesWithGroomingReader runs the REAL grooming reader
// (intakegroom.ParseRubricIDs) over the shipped charter bytes and requires the
// exact ordered id set, and that this package's own parser agrees with it.
func TestCharterTemplateParsesWithGroomingReader(t *testing.T) {
	charter := string(mustFiles(t, githubOpts())[CharterPath])
	got := intakegroom.ParseRubricIDs(charter).IDs()
	if !reflect.DeepEqual(got, wantRubricIDs) {
		t.Fatalf("grooming reader parses charter ids %v, want %v", got, wantRubricIDs)
	}
	if ours := RubricIDs(charter); !reflect.DeepEqual(ours, got) {
		t.Fatalf("scaffold.RubricIDs = %v, grooming reader = %v", ours, got)
	}
}

// TestCharterTemplateCarriesNoDirectionText asserts every rubric cell is a
// fill-me-in marker and the header states the document is human-authored.
func TestCharterTemplateCarriesNoDirectionText(t *testing.T) {
	charter := string(mustFiles(t, githubOpts())[CharterPath])
	if !strings.Contains(charter, "**This document is human-authored.**") {
		t.Error("charter does not state it is human-authored")
	}
	for _, id := range wantRubricIDs {
		if q := intakegroom.ParseRubricIDs(charter).Quote(id); !strings.HasPrefix(q, "<!-- fill me in") {
			t.Errorf("rubric %s carries direction text, want a fill-me-in marker: %q", id, q)
		}
	}
}

// TestWorkManagementTemplateParsesWithBackendSemantics renders every complete
// provider branch and runs workmgmt.Parse — schema AND the semantic rules the
// CLI's schema-only validator does not reproduce.
func TestWorkManagementTemplateParsesWithBackendSemantics(t *testing.T) {
	for name, opts := range map[string]Options{
		"github complete": githubOpts(),
		"github org":      {Provider: ProviderGitHubProjects, Autonomy: "high", ProjectOwner: "acme", ProjectOwnerType: "organization", ProjectNumber: 2},
		"gitlab default":  {Provider: ProviderGitLab, Autonomy: "medium"},
		"gitlab project":  {Provider: ProviderGitLab, Autonomy: "low", GitLabProject: "group/sub/proj"},
	} {
		t.Run(name, func(t *testing.T) {
			data := mustFiles(t, opts)[WorkManagementPath]
			conv, err := workmgmt.Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("workmgmt.Parse rejects the rendered config: %v\n%s", err, data)
			}
			if conv.Provider != opts.Provider {
				t.Errorf("provider = %q, want %q", conv.Provider, opts.Provider)
			}
			for _, typ := range []string{"feature", "bug", "chore", "adr"} {
				if _, ok := conv.Types[typ]; !ok {
					t.Errorf("rendered config lacks type %q", typ)
				}
			}
			if conv.Charter == nil || conv.Charter.Path != CharterPath {
				t.Errorf("charter block = %+v, want path %s", conv.Charter, CharterPath)
			}
		})
	}
}

// TestWorkManagementIncompleteConnectionFailsClosedNamingProject pins the
// deliberately-incomplete arm: without a project number the config is
// returned, but the backend parser refuses it naming the connection block, so
// grooming and filing fail closed until the operator fills it.
func TestWorkManagementIncompleteConnectionFailsClosedNamingProject(t *testing.T) {
	data := mustFiles(t, Options{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "acme"})[WorkManagementPath]
	_, err := workmgmt.Parse(bytes.NewReader(data))
	var sem *workmgmt.SemanticError
	if !errors.As(err, &sem) || !strings.Contains(sem.Msg, "project connection block") {
		t.Fatalf("workmgmt.Parse err = %v, want the github_projects connection SemanticError", err)
	}
}

// TestOperatorTemplatePassesThinnessRule runs the backend's overlay validator
// (schema + ADR-040 D1 thinness rule) over every autonomy rendering, and
// proves the validator would catch a procedure field on the same bytes.
func TestOperatorTemplatePassesThinnessRule(t *testing.T) {
	for _, a := range []string{"low", "medium", "high"} {
		opts := githubOpts()
		opts.Autonomy = a
		data := mustFiles(t, opts)[OperatorPath]
		if err := operatorrole.ValidateOverlay(bytes.NewReader(data)); err != nil {
			t.Errorf("autonomy %s: ValidateOverlay: %v\n%s", a, err, data)
		}
		withProcedure := append(append([]byte(nil), data...), []byte("mission: run everything\n")...)
		var thin *operatorrole.ThinnessError
		if err := operatorrole.ValidateOverlay(bytes.NewReader(withProcedure)); !errors.As(err, &thin) {
			t.Errorf("autonomy %s: a procedure field was not rejected by the thinness rule: %v", a, err)
		}
	}
}

func TestFiles_RejectsUnknownOptions(t *testing.T) {
	for name, c := range map[string]struct {
		opts Options
		want string
	}{
		"autonomy":   {Options{Provider: ProviderGitLab, Autonomy: "max"}, `unknown autonomy "max"`},
		"provider":   {Options{Provider: "jira", Autonomy: "low"}, `unknown provider "jira"`},
		"owner type": {Options{Provider: ProviderGitHubProjects, Autonomy: "low", ProjectOwnerType: "team"}, `unknown project owner type "team"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Files(c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Files err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestOptionsMissing(t *testing.T) {
	cases := []struct {
		opts Options
		want []string
	}{
		{githubOpts(), nil},
		{Options{Provider: ProviderGitHubProjects, ProjectOwner: "a"}, []string{"project.number"}},
		{Options{Provider: ProviderGitHubProjects, ProjectNumber: 1}, []string{"project.owner"}},
		{Options{Provider: ProviderGitHubProjects}, []string{"project.owner", "project.number"}},
		{Options{Provider: ProviderGitLab}, nil},
	}
	for _, c := range cases {
		if got := c.opts.Missing(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Missing(%+v) = %v, want %v", c.opts, got, c.want)
		}
	}
}

// swapTemplate replaces a template seam for one test.
func swapTemplate(t *testing.T, target *[]byte, data []byte) {
	t.Helper()
	orig := *target
	*target = data
	t.Cleanup(func() { *target = orig })
}

// TestWorkManagementIncompleteBranchesFailOnlyOnTheConnection covers every
// incomplete github_projects rendering: workmgmt.Parse must refuse it with the
// provider-connection *SemanticError ONLY (a *SemanticError is reachable only
// after the schema passed), and the same options with the connection filled
// in must parse clean — so an incomplete config cannot hide a broken
// types/states/transitions set behind its expected connection error.
func TestWorkManagementIncompleteBranchesFailOnlyOnTheConnection(t *testing.T) {
	for name, opts := range map[string]Options{
		"no owner":       {Provider: ProviderGitHubProjects, Autonomy: "low", ProjectNumber: 3},
		"no number":      {Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "acme"},
		"nothing":        {Provider: ProviderGitHubProjects, Autonomy: "high"},
		"org, no number": {Provider: ProviderGitHubProjects, Autonomy: "high", ProjectOwner: "acme", ProjectOwnerType: "organization"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := workmgmt.Parse(bytes.NewReader(mustFiles(t, opts)[WorkManagementPath]))
			var sem *workmgmt.SemanticError
			if !errors.As(err, &sem) || !strings.Contains(sem.Msg, "project connection block") {
				t.Fatalf("workmgmt.Parse err = %v, want only the github_projects connection SemanticError", err)
			}
			filled := opts
			filled.ProjectOwner, filled.ProjectNumber = "acme", 9
			if _, err := workmgmt.Parse(bytes.NewReader(mustFiles(t, filled)[WorkManagementPath])); err != nil {
				t.Fatalf("with the connection filled in, workmgmt.Parse still rejects: %v", err)
			}
		})
	}
}

// TestBackendSemanticProofDiscriminates proves the workmgmt.Parse proof above
// is a real vehicle, not a vacuous pass: a template that is SCHEMA-valid but
// breaks a semantic rule the CLI's schema-only validator cannot see (the adr
// type loses its numbering rule) renders through Files yet is rejected by the
// backend parser.
func TestBackendSemanticProofDiscriminates(t *testing.T) {
	numbering := []byte("    numbering:\n      scheme: sequential\n      prefix: ADR-\n      pad: 3\n")
	bad := bytes.Replace(workManagementTemplate, numbering, nil, 1)
	if bytes.Equal(bad, workManagementTemplate) {
		t.Fatal("fixture did not remove the adr numbering rule")
	}
	swapTemplate(t, &workManagementTemplate, bad)
	data := mustFiles(t, githubOpts())[WorkManagementPath]
	_, err := workmgmt.Parse(bytes.NewReader(data))
	var sem *workmgmt.SemanticError
	if !errors.As(err, &sem) || !strings.Contains(sem.Msg, `type "adr" must declare a numbering rule`) {
		t.Fatalf("workmgmt.Parse err = %v, want the adr numbering SemanticError", err)
	}
}

func TestFiles_FailsClosedOnRubriclessCharter(t *testing.T) {
	swapTemplate(t, &charterTemplate, []byte("# Charter\n\n## 4. Prioritization rubric\n\n| **V1** | x |\n| **R1** | x |\n| **U1** | x |\n"))
	if _, err := Files(githubOpts()); err == nil || !strings.Contains(err.Error(), "no S* rubric row") {
		t.Fatalf("Files err = %v, want a missing S* rubric group", err)
	}
}

func TestFiles_FailsClosedOnUnrenderedPlaceholder(t *testing.T) {
	swapTemplate(t, &operatorTemplate, append(append([]byte(nil), operatorTemplate...), []byte("# {{unknown}}\n")...))
	if _, err := Files(githubOpts()); err == nil || !strings.Contains(err.Error(), `"{{unknown}}"`) {
		t.Fatalf("Files err = %v, want the unrendered placeholder named", err)
	}
}

func TestFiles_ProviderConnectionBranches(t *testing.T) {
	type wmDoc struct {
		Project map[string]any `yaml:"project"`
		GitLab  map[string]any `yaml:"gitlab"`
	}
	decode := func(data []byte) wmDoc {
		var d wmDoc
		if err := yaml.Unmarshal(data, &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return d
	}
	if d := decode(mustFiles(t, githubOpts())[WorkManagementPath]); d.Project["number"] != 4 || d.GitLab != nil {
		t.Errorf("github complete: %+v", d)
	}
	incomplete := mustFiles(t, Options{Provider: ProviderGitHubProjects, Autonomy: "medium"})[WorkManagementPath]
	if d := decode(incomplete); d.Project != nil {
		t.Errorf("incomplete: live project block %v", d.Project)
	}
	if !strings.Contains(string(incomplete), "FILL ME IN (project.owner, project.number)") {
		t.Errorf("incomplete: no required-fill marker:\n%s", incomplete)
	}
	if d := decode(mustFiles(t, Options{Provider: ProviderGitLab, Autonomy: "medium", GitLabProject: "grp.x/sub_1/p-2"})[WorkManagementPath]); d.GitLab["project"] != "grp.x/sub_1/p-2" {
		t.Errorf("gitlab quoted project: %+v", d)
	}
}

func TestEnsureFiles_CreatesAllThenSkips(t *testing.T) {
	root := t.TempDir()
	res, err := EnsureFiles(root, githubOpts())
	if err != nil {
		t.Fatal(err)
	}
	want := mustFiles(t, githubOpts())
	for i, p := range Paths() {
		if res.Files[i].Path != p || res.Files[i].Status != StatusCreated {
			t.Errorf("first run result[%d] = %+v, want %s created", i, res.Files[i], p)
		}
		got, err := os.ReadFile(filepath.Join(root, p))
		if err != nil || !bytes.Equal(got, want[p]) {
			t.Errorf("%s bytes differ from Files output (err=%v)", p, err)
		}
	}
	res, err = EnsureFiles(root, Options{Provider: ProviderGitLab, Autonomy: "high"})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range Paths() {
		if res.Files[i].Status != StatusSkippedExisting {
			t.Errorf("second run %s = %s, want skipped-existing", p, res.Files[i].Status)
		}
		got, _ := os.ReadFile(filepath.Join(root, p))
		if !bytes.Equal(got, want[p]) {
			t.Errorf("second run rewrote %s", p)
		}
	}
}

// TestEnsureFiles_NeverOverwritesExistingCharter (C1): a charter a human
// already wrote survives byte-identically. The sentinel is authored
// independently of the template, so the byte sequences differ by construction.
func TestEnsureFiles_NeverOverwritesExistingCharter(t *testing.T) {
	root := t.TempDir()
	sentinel := []byte("# Our charter\n\nsentinel-3718-human-authored-text\n")
	if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, CharterPath), sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureFiles(root, githubOpts())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, CharterPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("existing charter was rewritten:\n%s", got)
	}
	if res.Files[0].Path != CharterPath || res.Files[0].Status != StatusSkippedExisting {
		t.Errorf("charter result = %+v, want skipped-existing", res.Files[0])
	}
}

func TestEnsureFiles_PropagatesRenderAndWriteErrors(t *testing.T) {
	if _, err := EnsureFiles(t.TempDir(), Options{Provider: "jira", Autonomy: "low"}); err == nil {
		t.Error("EnsureFiles accepted an unknown provider")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".fishhawk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFiles(root, githubOpts()); err == nil || !strings.Contains(err.Error(), CharterPath) {
		t.Errorf("EnsureFiles err = %v, want a write failure naming %s", err, CharterPath)
	}
}

// TestFiles_RejectsConnectionInjection is the configuration-injection
// control (E74.3 fix-up). project_owner and gitlab_project are exposed
// operator and MCP-tool inputs, and the INCOMPLETE GitHub branch renders the
// owner into a COMMENTED block — a place no quoting pass can defend, because
// a newline simply ends the comment. renderConnection therefore constrains
// both scalars to forge-identifier characters and fails closed.
//
// Control absent, the first payload below renders
//
//	#   owner: acme
//	project:
//	  owner: attacker
//	  owner_type: user
//	  number: 7
//
// i.e. an ACTIVE project connection out of a configuration the header still
// calls commented and incomplete. The assertions are therefore made on the
// DECODED document as well as on the refusal: a rendered document that
// carries a live `project` mapping while Options.Missing() is non-empty is
// the defect, whatever the error value says.
func TestFiles_RejectsConnectionInjection(t *testing.T) {
	owners := map[string]string{
		"newline activates a connection": "acme\nproject:\n  owner: attacker\n  owner_type: user\n  number: 7",
		"inline mapping":                 `acme": {x: 1}, "y`,
		"comment escape":                 "acme # owner: attacker",
		"colon":                          "acme: attacker",
		"carriage return":                "acme\rproject: x",
		"leading dash":                   "-acme",
		"too long":                       strings.Repeat("a", 40),
	}
	for name, owner := range owners {
		t.Run("owner/"+name, func(t *testing.T) {
			// Both arms: the incomplete (commented) render AND the
			// complete one, so the guard is proven on the branch the
			// payload targets and on the live branch alike.
			for _, opts := range []Options{
				{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: owner},
				{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: owner, ProjectNumber: 7},
			} {
				files, err := Files(opts)
				if err == nil {
					var d struct {
						Project map[string]any `yaml:"project"`
					}
					_ = yaml.Unmarshal(files[WorkManagementPath], &d)
					t.Fatalf("Files(owner=%q, number=%d) rendered instead of refusing; project = %v\n%s",
						owner, opts.ProjectNumber, d.Project, files[WorkManagementPath])
				}
				if !strings.Contains(err.Error(), "invalid project owner") {
					t.Fatalf("Files(owner=%q, number=%d) error = %v, want an invalid-project-owner refusal", owner, opts.ProjectNumber, err)
				}
			}
		})
	}

	projects := map[string]string{
		"newline activates a connection": "grp/p\ngitlab:\n  project: attacker/p",
		"quote":                          `g/"p`,
		"no namespace":                   "justaproject",
		"empty segment":                  "grp//p",
		"comment escape":                 "grp/p # project: attacker/p",
	}
	for name, project := range projects {
		t.Run("gitlab/"+name, func(t *testing.T) {
			_, err := Files(Options{Provider: ProviderGitLab, Autonomy: "medium", GitLabProject: project})
			if err == nil || !strings.Contains(err.Error(), "invalid gitlab project") {
				t.Fatalf("Files(gitlab_project=%q) error = %v, want an invalid-gitlab-project refusal", project, err)
			}
		})
	}

	// Discrimination: the guard must not refuse the identifiers real
	// forges issue, or it would be a blanket refusal rather than a
	// control.
	for _, ok := range []Options{
		{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "kuhlman-labs", ProjectNumber: 7},
		{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: strings.Repeat("a", 39)},
		{Provider: ProviderGitLab, Autonomy: "medium", GitLabProject: "grp.x/sub_1/p-2"},
	} {
		if _, err := Files(ok); err != nil {
			t.Errorf("Files(%+v) refused a legitimate identifier: %v", ok, err)
		}
	}
}
