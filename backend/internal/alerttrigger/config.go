package alerttrigger

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file loads the alert sources file (FISHHAWKD_ALERT_SOURCES_FILE):
// which senders may call the ingress, the secret each signs with, and where
// and how each one's incidents are filed. It is parsed strictly — an unknown
// key, a wrong type or a missing secret fails fishhawkd startup rather than
// silently serving a half-configured ingress. Secrets never live in the file:
// each source names an environment variable (secret_env) that carries it.

// SourcesFileVersion is the only accepted `version:` of the sources file.
const SourcesFileVersion = 1

// MinSecretBytes is the shortest accepted shared secret. 32 bytes matches
// the HMAC-SHA256 output size, so the key is never the weaker half.
const MinSecretBytes = 32

// Defaults a source inherits when it omits the key.
const (
	DefaultWorkItemType = "bug"
	DefaultWorkflowID   = "hotfix_change"
)

var (
	sourceIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repoRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	itemTypeRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	workflowIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	parentEpicRE = regexp.MustCompile(`^#?[1-9][0-9]{0,9}$`)
)

// maxLabelLen bounds one configured label (GitHub's own label-name limit).
const maxLabelLen = 50

// Source is one configured alert sender.
type Source struct {
	// ID is the sender's HeaderSource value and the dedup namespace.
	ID string
	// SecretEnv names the environment variable the secret was read from.
	SecretEnv string
	// Repo is the owner/name repository incident issues are filed in.
	Repo string
	// WorkItemType is the conventions type the incident is filed as
	// (default "bug").
	WorkItemType string
	// ParentEpic is the issue reference (`#N` or `N`) the incident rolls up
	// to. Optional here; a conventions title_format that needs {epic} fails
	// the filing closed when it is empty.
	ParentEpic string
	// Labels are merged onto the type's default labels at filing.
	Labels []string
	// AutoStart, when true, starts a WorkflowID run on a newly filed
	// incident. It is FALSE unless the file sets `auto_start: true`
	// explicitly: auto-starting a hotfix from an external signal is an
	// operator configuration decision (ADR-053 fork 2), never a default.
	AutoStart bool
	// WorkflowID is the workflow an auto-start runs, and the one the
	// incident's next-step line names (default "hotfix_change").
	WorkflowID string
	// RunnerKind is the auto-started run's runner_kind; empty leaves the
	// server default.
	RunnerKind string

	secret []byte
}

// Format renders a Source for every fmt verb with the secret redacted, so
// logging a Source can never leak it. (A String method on an unexported
// field is not enough: fmt cannot call methods on unexported fields.)
func (s Source) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "alerttrigger.Source{ID:%q SecretEnv:%q Repo:%q WorkItemType:%q ParentEpic:%q Labels:%q AutoStart:%t WorkflowID:%q RunnerKind:%q secret:[redacted]}",
		s.ID, s.SecretEnv, s.Repo, s.WorkItemType, s.ParentEpic, s.Labels, s.AutoStart, s.WorkflowID, s.RunnerKind)
}

// Sources is the parsed sources file. The zero value has no sources, which
// is the ingress-off state.
type Sources struct {
	byID  map[string]Source
	order []string
}

// Format renders Sources for every fmt verb (%v, %+v, %#v, %s, ...) as the
// configured ids only, e.g. `alerttrigger.Sources{ids:[a b]}`. Source's own
// Format does not protect a Sources value: fmt does not invoke formatting
// methods on unexported fields, so without this method %+v would reflect into
// the unexported id->Source map and print each raw secret byte slice.
func (s Sources) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "alerttrigger.Sources{ids:%v}", s.order)
}

// Lookup returns the source with id.
func (s Sources) Lookup(id string) (Source, bool) {
	src, ok := s.byID[id]
	return src, ok
}

// Len is the number of configured sources.
func (s Sources) Len() int { return len(s.order) }

// IDs lists the configured source ids in file order.
func (s Sources) IDs() []string { return append([]string(nil), s.order...) }

// AutoStartIDs lists, in file order, the ids of sources with auto_start true
// — what fishhawkd's startup WARN names.
func (s Sources) AutoStartIDs() []string {
	var ids []string
	for _, id := range s.order {
		if s.byID[id].AutoStart {
			ids = append(ids, id)
		}
	}
	return ids
}

type sourcesFile struct {
	Version *int          `yaml:"version"`
	Sources []sourceEntry `yaml:"sources"`
}

type sourceEntry struct {
	ID           string     `yaml:"id"`
	SecretEnv    string     `yaml:"secret_env"`
	Repo         string     `yaml:"repo"`
	WorkItemType string     `yaml:"work_item_type"`
	ParentEpic   string     `yaml:"parent_epic"`
	Labels       []string   `yaml:"labels"`
	AutoStart    strictBool `yaml:"auto_start"`
	WorkflowID   string     `yaml:"workflow_id"`
	RunnerKind   string     `yaml:"runner_kind"`
}

// strictBool decodes ONLY a YAML 1.2 boolean (true/false). yaml.v3 otherwise
// accepts YAML 1.1 spellings such as yes/on/y — even quoted — into a bool
// field, and a typo'd `auto_start: "yes"` must not arm auto-start.
type strictBool bool

func (b *strictBool) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
		return fmt.Errorf("line %d: want a boolean (true or false), got %q", n.Line, n.Value)
	}
	var v bool
	if err := n.Decode(&v); err != nil {
		return err
	}
	*b = strictBool(v)
	return nil
}

// LoadSources reads and parses the sources file at path.
func LoadSources(path string, getenv func(string) string) (Sources, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sources{}, fmt.Errorf("alert sources file: %w", err)
	}
	srcs, err := ParseSources(data, getenv)
	if err != nil {
		return Sources{}, fmt.Errorf("%s: %w", path, err)
	}
	return srcs, nil
}

// ParseSources parses a sources document, resolving each secret_env through
// getenv. Every refusal names the offending source and key; none echoes a
// secret value.
func ParseSources(data []byte, getenv func(string) string) (Sources, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f sourcesFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return Sources{}, errors.New("alert sources: empty document (want version: 1 and a sources list)")
		}
		return Sources{}, fmt.Errorf("alert sources: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Sources{}, errors.New("alert sources: more than one YAML document (want exactly one)")
	}
	if f.Version == nil || *f.Version != SourcesFileVersion {
		return Sources{}, fmt.Errorf("alert sources: version must be %d", SourcesFileVersion)
	}
	if len(f.Sources) == 0 {
		return Sources{}, errors.New("alert sources: sources is empty (an unconfigured ingress is an unset FISHHAWKD_ALERT_SOURCES_FILE, not an empty file)")
	}
	out := Sources{byID: make(map[string]Source, len(f.Sources))}
	for i, e := range f.Sources {
		src, err := e.resolve(getenv)
		if err != nil {
			return Sources{}, fmt.Errorf("alert sources: sources[%d]: %w", i, err)
		}
		if _, dup := out.byID[src.ID]; dup {
			return Sources{}, fmt.Errorf("alert sources: sources[%d]: duplicate id %q", i, src.ID)
		}
		out.byID[src.ID] = src
		out.order = append(out.order, src.ID)
	}
	return out, nil
}

func (e sourceEntry) resolve(getenv func(string) string) (Source, error) {
	if !sourceIDRE.MatchString(e.ID) {
		return Source{}, fmt.Errorf("id %q must match %s", e.ID, sourceIDRE)
	}
	src := Source{
		ID:           e.ID,
		SecretEnv:    e.SecretEnv,
		Repo:         e.Repo,
		WorkItemType: e.WorkItemType,
		ParentEpic:   e.ParentEpic,
		AutoStart:    bool(e.AutoStart),
		WorkflowID:   e.WorkflowID,
		RunnerKind:   e.RunnerKind,
	}
	if src.SecretEnv == "" {
		return Source{}, fmt.Errorf("source %q: secret_env is required (the secret is read from that environment variable, never from this file)", e.ID)
	}
	if !envNameRE.MatchString(src.SecretEnv) {
		return Source{}, fmt.Errorf("source %q: secret_env %q is not an environment variable name", e.ID, src.SecretEnv)
	}
	secret := getenv(src.SecretEnv)
	if secret == "" {
		return Source{}, fmt.Errorf("source %q: environment variable %s is unset or empty", e.ID, src.SecretEnv)
	}
	if len(secret) < MinSecretBytes {
		return Source{}, fmt.Errorf("source %q: secret in %s is %d bytes; at least %d are required", e.ID, src.SecretEnv, len(secret), MinSecretBytes)
	}
	src.secret = []byte(secret)
	if !repoRE.MatchString(src.Repo) {
		return Source{}, fmt.Errorf("source %q: repo %q must be owner/name", e.ID, src.Repo)
	}
	if src.WorkItemType == "" {
		src.WorkItemType = DefaultWorkItemType
	}
	if !itemTypeRE.MatchString(src.WorkItemType) {
		return Source{}, fmt.Errorf("source %q: work_item_type %q must match %s", e.ID, src.WorkItemType, itemTypeRE)
	}
	if src.ParentEpic != "" && !parentEpicRE.MatchString(src.ParentEpic) {
		return Source{}, fmt.Errorf("source %q: parent_epic %q must be an issue reference (#N or N)", e.ID, src.ParentEpic)
	}
	if src.WorkflowID == "" {
		src.WorkflowID = DefaultWorkflowID
	}
	if !workflowIDRE.MatchString(src.WorkflowID) {
		return Source{}, fmt.Errorf("source %q: workflow_id %q must match %s", e.ID, src.WorkflowID, workflowIDRE)
	}
	if src.RunnerKind != "" {
		if _, ok := run.ValidRunnerKinds[src.RunnerKind]; !ok {
			return Source{}, fmt.Errorf("source %q: runner_kind %q is not one of %s", e.ID, src.RunnerKind, runnerKindList())
		}
	}
	for _, l := range e.Labels {
		if l == "" || strings.TrimSpace(l) != l || len(l) > maxLabelLen {
			return Source{}, fmt.Errorf("source %q: label %q must be 1-%d characters with no surrounding whitespace", e.ID, l, maxLabelLen)
		}
		src.Labels = append(src.Labels, l)
	}
	return src, nil
}

func runnerKindList() string {
	return strings.Join([]string{run.RunnerKindGitHubActions, run.RunnerKindGitLabCI, run.RunnerKindLocal}, ", ")
}
