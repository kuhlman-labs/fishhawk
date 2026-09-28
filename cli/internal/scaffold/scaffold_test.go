package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
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

// TestRubricRowParityAcrossModules holds RubricRowPattern byte-identical to
// the grooming reader's rubricRow in backend/internal/intakegroom/score.go:
// the two modules cannot import each other, so this reads the backend source
// over the repo root and requires the backtick-quoted CLI value verbatim.
func TestRubricRowParityAcrossModules(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "backend", "internal", "intakegroom", "score.go"))
	if err != nil {
		t.Fatalf("read backend rubric reader: %v", err)
	}
	want := "var rubricRow = regexp.MustCompile(`" + RubricRowPattern + "`)"
	if !strings.Contains(string(src), want) {
		t.Fatalf("backend/internal/intakegroom/score.go no longer declares %s\n"+
			"update cli/internal/scaffold.RubricRowPattern (and its backend mirror) to the grooming reader's pattern", want)
	}
}

// TestCharterTemplateRubricIDsParse asserts the SHIPPED charter bytes parse
// to exactly the expected ordered id set under the grooming row shape.
func TestCharterTemplateRubricIDsParse(t *testing.T) {
	files := mustFiles(t, githubOpts())
	got := RubricIDs(string(files[CharterPath]))
	want := []string{
		"V1", "V2", "V3", "V4", "V5",
		"R1", "R2", "R3", "R4", "R5",
		"U1", "U2", "U3", "U4",
		"S1", "S2", "S3", "S4", "S5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("charter rubric ids = %v, want %v", got, want)
	}
}

// TestCharterTemplateCarriesNoDirectionText asserts the skeleton invents no
// direction: every section body line and every rubric cell is a fill-me-in
// marker, and the header says the document is human-authored.
func TestCharterTemplateCarriesNoDirectionText(t *testing.T) {
	charter := string(mustFiles(t, githubOpts())[CharterPath])
	header, body, ok := strings.Cut(charter, "\n## ")
	if !ok {
		t.Fatal("charter has no `## ` section")
	}
	if !strings.Contains(header, "**This document is human-authored.**") {
		t.Errorf("charter header does not state it is human-authored:\n%s", header)
	}
	sections := 0
	for _, line := range strings.Split("## "+body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", line == "---", line == "| id | line |", line == "|---|---|":
		case strings.HasPrefix(line, "#"):
			sections++
		case rubricRow.MatchString(line):
			cell := rubricRow.FindStringSubmatch(line)[2]
			if !strings.HasPrefix(cell, "<!-- fill me in") {
				t.Errorf("rubric cell carries direction text, want a fill-me-in marker: %q", line)
			}
		default:
			if !strings.HasPrefix(line, "<!-- fill me in") || !strings.HasSuffix(line, "-->") {
				t.Errorf("section body carries direction text, want a fill-me-in marker: %q", line)
			}
		}
	}
	// north star, phase (+2 subsections), non-goals, rubric (+4 groups).
	if sections != 10 {
		t.Errorf("charter carries %d headings, want 10 (the content contract's sections)", sections)
	}
}

// TestWorkManagementTemplateIsSchemaValid runs the CLI's schema validator
// over the rendered bytes for every provider branch.
func TestWorkManagementTemplateIsSchemaValid(t *testing.T) {
	for name, opts := range map[string]Options{
		"github complete":       githubOpts(),
		"github missing number": {Provider: ProviderGitHubProjects, Autonomy: "low", ProjectOwner: "acme"},
		"github missing owner":  {Provider: ProviderGitHubProjects, Autonomy: "high"},
		"github org":            {Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "acme", ProjectOwnerType: "organization", ProjectNumber: 2},
		"gitlab default":        {Provider: ProviderGitLab, Autonomy: "medium"},
		"gitlab project":        {Provider: ProviderGitLab, Autonomy: "medium", GitLabProject: "group/sub/proj"},
	} {
		t.Run(name, func(t *testing.T) {
			data := mustFiles(t, opts)[WorkManagementPath]
			if err := spec.ValidateConventionsDocument(data); err != nil {
				t.Fatalf("rendered work-management fails the schema: %v\n%s", err, data)
			}
		})
	}
}

type wmDoc struct {
	Provider string         `yaml:"provider"`
	Project  map[string]any `yaml:"project"`
	GitLab   map[string]any `yaml:"gitlab"`
	Charter  struct {
		Path string `yaml:"path"`
	} `yaml:"charter"`
}

func decodeWM(t *testing.T, data []byte) wmDoc {
	t.Helper()
	var d wmDoc
	if err := yaml.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode work-management: %v", err)
	}
	return d
}

func TestWorkManagementTemplateDeclaresCharterBlock(t *testing.T) {
	d := decodeWM(t, mustFiles(t, githubOpts())[WorkManagementPath])
	if d.Charter.Path != CharterPath {
		t.Fatalf("charter.path = %q, want %q", d.Charter.Path, CharterPath)
	}
}

func TestFiles_ProviderConnectionBranches(t *testing.T) {
	complete := decodeWM(t, mustFiles(t, githubOpts())[WorkManagementPath])
	if complete.Provider != ProviderGitHubProjects || complete.GitLab != nil {
		t.Errorf("github complete: provider=%q gitlab=%v", complete.Provider, complete.GitLab)
	}
	if complete.Project["owner"] != "acme" || complete.Project["number"] != 4 || complete.Project["owner_type"] != "user" {
		t.Errorf("github complete project block = %v", complete.Project)
	}

	incomplete := mustFiles(t, Options{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectOwner: "acme"})[WorkManagementPath]
	if d := decodeWM(t, incomplete); d.Project != nil {
		t.Errorf("missing number: project block is live, want commented: %v", d.Project)
	}
	if !strings.Contains(string(incomplete), "FILL ME IN (project.number)") || !strings.Contains(string(incomplete), "# project:") {
		t.Errorf("missing number: no commented required-fill project block:\n%s", incomplete)
	}

	noOwner := mustFiles(t, Options{Provider: ProviderGitHubProjects, Autonomy: "medium", ProjectNumber: 3})[WorkManagementPath]
	if d := decodeWM(t, noOwner); d.Project != nil {
		t.Errorf("missing owner: project block is live, want commented: %v", d.Project)
	}
	if !strings.Contains(string(noOwner), "#   owner: FILL ME IN") || !strings.Contains(string(noOwner), "#   number: 3") {
		t.Errorf("missing owner: commented block lacks the owner marker / supplied number:\n%s", noOwner)
	}

	gl := decodeWM(t, mustFiles(t, Options{Provider: ProviderGitLab, Autonomy: "medium", GitLabProject: "g/p"})[WorkManagementPath])
	if gl.Provider != ProviderGitLab || gl.Project != nil || gl.GitLab["project"] != "g/p" {
		t.Errorf("gitlab: %+v", gl)
	}
	glDefault := decodeWM(t, mustFiles(t, Options{Provider: ProviderGitLab, Autonomy: "medium"})[WorkManagementPath])
	if glDefault.GitLab == nil || len(glDefault.GitLab) != 0 {
		t.Errorf("gitlab default: want an empty gitlab block, got %v", glDefault.GitLab)
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

func TestOperatorTemplateRendersAutonomyAndNoProcedure(t *testing.T) {
	for _, a := range []string{"low", "medium", "high"} {
		opts := githubOpts()
		opts.Autonomy = a
		data := mustFiles(t, opts)[OperatorPath]
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("decode operator overlay: %v", err)
		}
		if got := doc["knob_presets"].(map[string]any)["autonomy"]; got != a {
			t.Errorf("autonomy = %v, want %s", got, a)
		}
		for _, procedure := range []string{"mission", "gate_procedures", "escalation", "forbidden"} {
			if _, ok := doc[procedure]; ok {
				t.Errorf("overlay defines procedure field %q (thinness rule)", procedure)
			}
		}
		if doc["work_management"] != WorkManagementPath {
			t.Errorf("work_management = %v, want %s", doc["work_management"], WorkManagementPath)
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

// swapTemplate replaces a template seam for one test.
func swapTemplate(t *testing.T, target *[]byte, data []byte) {
	t.Helper()
	orig := *target
	*target = data
	t.Cleanup(func() { *target = orig })
}

// TestFiles_FailsClosedOnInvalidWorkManagementTemplate (C7): a template with
// spec_version removed is rejected by the schema, so Files must refuse.
func TestFiles_FailsClosedOnInvalidWorkManagementTemplate(t *testing.T) {
	bad := bytes.Replace(workManagementTemplate, []byte("spec_version: work-management-v0\n"), nil, 1)
	if bytes.Equal(bad, workManagementTemplate) {
		t.Fatal("fixture did not remove spec_version")
	}
	swapTemplate(t, &workManagementTemplate, bad)
	files, err := Files(githubOpts())
	if err == nil {
		t.Fatalf("Files accepted an invalid work-management document:\n%s", files[WorkManagementPath])
	}
	if !strings.Contains(err.Error(), "spec_version") {
		t.Errorf("error does not name spec_version: %v", err)
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
// already wrote survives byte-identically and is reported skipped-existing.
// The sentinel is authored independently of the template, so the two byte
// sequences differ by construction.
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
	for _, fr := range res.Files[1:] {
		if fr.Status != StatusCreated {
			t.Errorf("%s = %s, want created", fr.Path, fr.Status)
		}
	}
}

func TestEnsureFiles_PropagatesRenderAndWriteErrors(t *testing.T) {
	if _, err := EnsureFiles(t.TempDir(), Options{Provider: "jira", Autonomy: "low"}); err == nil {
		t.Error("EnsureFiles accepted an unknown provider")
	}
	// A regular file where the .fishhawk directory must go makes the write fail.
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
