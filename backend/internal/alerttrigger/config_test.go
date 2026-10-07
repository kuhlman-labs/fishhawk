package alerttrigger

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

var testEnv = envMap(map[string]string{
	"PAGER_SECRET": testSecret,
	"OTHER_SECRET": strings.Repeat("s", MinSecretBytes),
	"SHORT_SECRET": strings.Repeat("s", MinSecretBytes-1),
})

// sourceDoc renders a one-source document; extra is appended to the source
// mapping verbatim (each line indented four spaces).
func sourceDoc(extra ...string) string {
	var b strings.Builder
	b.WriteString("version: 1\nsources:\n  - id: pager\n    secret_env: PAGER_SECRET\n    repo: acme/shop\n")
	for _, l := range extra {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

func TestParseSources_ValidWithDefaults(t *testing.T) {
	srcs, err := ParseSources([]byte(sourceDoc()), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	src, ok := srcs.Lookup("pager")
	if !ok {
		t.Fatal("pager not found")
	}
	if src.WorkItemType != DefaultWorkItemType || src.WorkflowID != DefaultWorkflowID {
		t.Fatalf("defaults: work_item_type=%q workflow_id=%q, want %q / %q", src.WorkItemType, src.WorkflowID, DefaultWorkItemType, DefaultWorkflowID)
	}
	if src.RunnerKind != "" || src.ParentEpic != "" || src.Labels != nil {
		t.Fatalf("unexpected optional values: %+v", src)
	}
	if string(src.secret) != testSecret || src.SecretEnv != "PAGER_SECRET" {
		t.Fatalf("secret not resolved from PAGER_SECRET")
	}
}

func TestParseSources_AllFields(t *testing.T) {
	doc := `version: 1
sources:
  - id: grafana-prod
    secret_env: PAGER_SECRET
    repo: acme/shop.api
    work_item_type: incident
    parent_epic: "#35"
    labels: [area:backend, "phase:beta"]
    auto_start: true
    workflow_id: hotfix_change
    runner_kind: local
  - id: sentry_2
    secret_env: OTHER_SECRET
    repo: Acme-Org/web
    parent_epic: 7
`
	srcs, err := ParseSources([]byte(doc), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if srcs.Len() != 2 || !reflect.DeepEqual(srcs.IDs(), []string{"grafana-prod", "sentry_2"}) {
		t.Fatalf("ids = %v", srcs.IDs())
	}
	g, _ := srcs.Lookup("grafana-prod")
	want := Source{ID: "grafana-prod", SecretEnv: "PAGER_SECRET", Repo: "acme/shop.api", WorkItemType: "incident",
		ParentEpic: "#35", Labels: []string{"area:backend", "phase:beta"}, AutoStart: true, WorkflowID: "hotfix_change",
		RunnerKind: "local", secret: []byte(testSecret)}
	if !reflect.DeepEqual(g, want) {
		t.Fatalf("grafana-prod = %v\nwant %v (or the secret differs)", g, want)
	}
	s, _ := srcs.Lookup("sentry_2")
	if s.ParentEpic != "7" || s.AutoStart {
		t.Fatalf("sentry_2 = %+v", s)
	}
	if got := srcs.AutoStartIDs(); !reflect.DeepEqual(got, []string{"grafana-prod"}) {
		t.Fatalf("AutoStartIDs = %v", got)
	}
	if _, ok := srcs.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) found a source")
	}
}

// The shipped-default done-means: a source that omits auto_start does NOT
// auto-start. The explicit-true case keeps the default assertion from being
// vacuous, and non-boolean spellings are refused rather than coerced.
func TestParseSources_AutoStartDefaultsOff(t *testing.T) {
	srcs, err := ParseSources([]byte(sourceDoc()), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if src, _ := srcs.Lookup("pager"); src.AutoStart {
		t.Fatal("omitted auto_start parsed to true; the shipped default must be OFF")
	}
	if ids := srcs.AutoStartIDs(); len(ids) != 0 {
		t.Fatalf("AutoStartIDs = %v, want none", ids)
	}
	for _, v := range []string{"false", "null", ""} {
		srcs, err := ParseSources([]byte(sourceDoc("auto_start: "+v)), testEnv)
		if err != nil {
			t.Fatalf("auto_start: %s: %v", v, err)
		}
		if src, _ := srcs.Lookup("pager"); src.AutoStart {
			t.Fatalf("auto_start: %s parsed to true", v)
		}
	}
	srcs, err = ParseSources([]byte(sourceDoc("auto_start: true")), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if src, _ := srcs.Lookup("pager"); !src.AutoStart {
		t.Fatal("explicit auto_start: true parsed to false")
	}
	for _, v := range []string{`"yes"`, "yes", "on", "y", `"true"`, "1", "[true]"} {
		if _, err := ParseSources([]byte(sourceDoc("auto_start: "+v)), testEnv); err == nil || !strings.Contains(err.Error(), "boolean") {
			t.Fatalf("auto_start: %s: err = %v, want a boolean refusal", v, err)
		}
	}
}

func TestParseSources_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"empty document", "", "empty document"},
		{"comment only", "# nothing\n", "empty document"},
		{"not yaml", "version: [1\n", "alert sources"},
		{"unknown top-level key", sourceDoc() + "extra: 1\n", "field extra not found"},
		{"unknown source key", sourceDoc("secret: inline"), "field secret not found"},
		{"version missing", "sources:\n  - id: pager\n    secret_env: PAGER_SECRET\n    repo: acme/shop\n", "version must be 1"},
		{"version 2", strings.Replace(sourceDoc(), "version: 1", "version: 2", 1), "version must be 1"},
		{"two documents", sourceDoc() + "---\n" + sourceDoc(), "more than one YAML document"},
		{"sources empty", "version: 1\nsources: []\n", "sources is empty"},
		{"sources absent", "version: 1\n", "sources is empty"},
		{"id uppercase", strings.Replace(sourceDoc(), "id: pager", "id: Pager", 1), `id "Pager" must match`},
		{"id empty", strings.Replace(sourceDoc(), "id: pager", "id: \"\"", 1), `id "" must match`},
		{"id leading dash", strings.Replace(sourceDoc(), "id: pager", "id: -pager", 1), "must match"},
		{"id 65 chars", strings.Replace(sourceDoc(), "id: pager", "id: "+strings.Repeat("a", 65), 1), "must match"},
		{"duplicate id", sourceDoc() + "  - id: pager\n    secret_env: OTHER_SECRET\n    repo: acme/other\n", `sources[1]: duplicate id "pager"`},
		{"secret_env missing", "version: 1\nsources:\n  - id: pager\n    repo: acme/shop\n", "secret_env is required"},
		{"secret_env not a name", strings.Replace(sourceDoc(), "PAGER_SECRET", "PAGER-SECRET", 1), "not an environment variable name"},
		{"secret env unset", strings.Replace(sourceDoc(), "PAGER_SECRET", "UNSET_SECRET", 1), "UNSET_SECRET is unset or empty"},
		{"secret 31 bytes", strings.Replace(sourceDoc(), "PAGER_SECRET", "SHORT_SECRET", 1), "is 31 bytes; at least 32"},
		{"repo missing", "version: 1\nsources:\n  - id: pager\n    secret_env: PAGER_SECRET\n", `repo "" must be owner/name`},
		{"repo without owner", strings.Replace(sourceDoc(), "acme/shop", "shop", 1), "must be owner/name"},
		{"repo with three parts", strings.Replace(sourceDoc(), "acme/shop", "acme/shop/x", 1), "must be owner/name"},
		{"repo empty name", strings.Replace(sourceDoc(), "acme/shop", "acme/", 1), "must be owner/name"},
		{"bad work_item_type", sourceDoc("work_item_type: Bug Report"), "work_item_type"},
		{"bad workflow_id", sourceDoc("workflow_id: hotfix-change"), "workflow_id"},
		{"bad parent_epic", sourceDoc("parent_epic: epic-35"), "parent_epic"},
		{"zero parent_epic", sourceDoc("parent_epic: \"#0\""), "parent_epic"},
		{"unknown runner_kind", sourceDoc("runner_kind: kubernetes"), `runner_kind "kubernetes" is not one of`},
		{"empty label", sourceDoc(`labels: [""]`), "label"},
		{"padded label", sourceDoc(`labels: [" area:x"]`), "label"},
		{"long label", sourceDoc("labels: [" + strings.Repeat("l", maxLabelLen+1) + "]"), "label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcs, err := ParseSources([]byte(tc.doc), testEnv)
			if err == nil {
				t.Fatalf("accepted; want a refusal containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if srcs.Len() != 0 {
				t.Fatalf("refusal returned %d sources", srcs.Len())
			}
		})
	}
}

func TestParseSources_ThirtyTwoByteSecretAccepted(t *testing.T) {
	if _, err := ParseSources([]byte(strings.Replace(sourceDoc(), "PAGER_SECRET", "OTHER_SECRET", 1)), testEnv); err != nil {
		t.Fatalf("32-byte secret refused: %v", err)
	}
}

func TestParseSources_ValidRunnerKinds(t *testing.T) {
	for _, k := range []string{"local", "github_actions", "gitlab_ci"} {
		srcs, err := ParseSources([]byte(sourceDoc("runner_kind: "+k)), testEnv)
		if err != nil {
			t.Fatalf("runner_kind %s: %v", k, err)
		}
		if src, _ := srcs.Lookup("pager"); src.RunnerKind != k {
			t.Fatalf("runner_kind = %q, want %q", src.RunnerKind, k)
		}
	}
}

// A refusal and every fmt rendering of a Source keep the secret out.
func TestSecretNeverRendered(t *testing.T) {
	short := "tooShortSecretValue-xyz"
	_, err := ParseSources([]byte(sourceDoc()), envMap(map[string]string{"PAGER_SECRET": short}))
	if err == nil || strings.Contains(err.Error(), short) {
		t.Fatalf("short-secret refusal = %v; must refuse without echoing the value", err)
	}
	srcs, err := ParseSources([]byte(sourceDoc()), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := srcs.Lookup("pager")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q", "%d"} {
		if out := fmt.Sprintf(verb, src); strings.Contains(out, testSecret) || strings.Contains(out, fmt.Sprintf("%x", testSecret)) {
			t.Fatalf("%s rendering leaks the secret: %s", verb, out)
		}
		if !strings.Contains(fmt.Sprintf(verb, src), `ID:"pager"`) {
			t.Fatalf("%s rendering lost the non-secret fields", verb)
		}
	}
}

func TestLoadSources(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "sources.yaml")
	if err := os.WriteFile(good, []byte(sourceDoc()), 0o600); err != nil {
		t.Fatal(err)
	}
	srcs, err := LoadSources(good, testEnv)
	if err != nil || srcs.Len() != 1 {
		t.Fatalf("LoadSources = %d sources, %v", srcs.Len(), err)
	}
	if _, err := LoadSources(filepath.Join(dir, "absent.yaml"), testEnv); err == nil || !strings.Contains(err.Error(), "alert sources file") {
		t.Fatalf("missing file: err = %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 2\nsources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSources(bad, testEnv); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("bad file: err = %v, want it to name %s", err, bad)
	}
}

func TestZeroSourcesIsIngressOff(t *testing.T) {
	var s Sources
	if s.Len() != 0 || len(s.IDs()) != 0 || len(s.AutoStartIDs()) != 0 {
		t.Fatal("zero Sources is not empty")
	}
	if _, ok := s.Lookup("pager"); ok {
		t.Fatal("zero Sources found a source")
	}
}
