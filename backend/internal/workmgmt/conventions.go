package workmgmt

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schemas defaults
var embedded embed.FS

const (
	schemaPath      = "schemas/work-management-v0.schema.json"
	defaultSpecPath = "defaults/work-management-default.yaml"
)

// Compiled once at package init; a malformed embedded artifact crashes
// the process at start, not on the first call.
var (
	configSchema       = mustCompileSchema(schemaPath)
	embeddedSchemaHash = computeSchemaHash()
	defaultConventions = mustParseDefault()
)

// mandatoryFields is the closed required-field trio (#1005, operator
// discussion): every conventions config must require Summary, Done-means,
// and complexity. The map key is the normalized form (lowercased,
// hyphens and spaces stripped) so "Done-means" and "Done means" both
// match. The value is the canonical label used in error messages.
var mandatoryFields = map[string]string{
	"summary":    "Summary",
	"donemeans":  "Done-means",
	"complexity": "complexity",
}

// Conventions is the typed form of a work-management conventions config.
type Conventions struct {
	SpecVersion      string              `json:"spec_version"`
	Provider         string              `json:"provider"`
	Project          *Project            `json:"project,omitempty"`
	Jira             *JiraConnection     `json:"jira,omitempty"`
	GitLab           *GitLabConnection   `json:"gitlab,omitempty"`
	ComplexityLevels map[string]string   `json:"complexity_levels,omitempty"`
	RequiredFields   []string            `json:"required_fields"`
	FieldHints       map[string]string   `json:"field_hints,omitempty"`
	Types            map[string]ItemType `json:"types"`
	ProductFeedback  *ProductFeedback    `json:"product_feedback,omitempty"`
	States           map[string]string   `json:"states,omitempty"`
	Transitions      map[string]string   `json:"transitions,omitempty"`
	Charter          *Charter            `json:"charter,omitempty"`
	Grooming         *Grooming           `json:"grooming,omitempty"`
	Selection        *Selection          `json:"selection,omitempty"`
}

// Charter declares where the repo's checked-in charter document lives
// (E54.1 / #2233) — the prioritization anchor a backlog-grooming run reads
// so every ranking it proposes cites a rubric line by id. The document is
// human-authored (agents read it, agents never write it) and is resolved
// from the run's BASE ref, so a change cannot rewrite the charter
// constraining it.
//
// The field is a POINTER so absent (nil) stays distinguishable from
// present-but-empty: a value type would render a config that omits the
// block as a zero-valued present one, and a consumer could not tell "no
// charter declared" from "charter declared with an empty path".
//
// Schema-optional, feature-mandatory: the block's absence is not a
// fail-open. backlog_grooming fails closed when no charter resolves, and
// that enforcement lives in #2234/#2236, not here. This declaration ships
// no reading, injection, or rendering — the injection mechanism is
// backend/internal/repodoc (#2796), whose validatePath owns declared-path
// safety fail-closed at the single point where the path is used to fetch
// the document. That is why validateSemantics carries no path-shape rule:
// a second copy of it here would be a second owner of the same invariant,
// free to drift from the one that actually gates the read.
type Charter struct {
	// Path is the repo-relative path of the charter document, relative to
	// the filing repo's root (e.g. .fishhawk/charter.md, the conventional
	// location the shipped default declares). The schema requires it and
	// enforces minLength 1, so a parsed Charter always carries a non-empty
	// path.
	Path string `json:"path"`
}

// Grooming declares the operator's controls on what a backlog-grooming run
// PROPOSES (E54.8 / #2240) — the churn guard ADR-065 names as the mitigation
// for its own sharpest adoption risk.
//
// POINTER for the same reason Charter is: absent (nil) must stay
// distinguishable from present-but-empty, so a consumer can tell "no grooming
// controls declared" (resolve the package defaults) from "declared, with
// nothing set".
type Grooming struct {
	Thresholds *GroomingThresholdSpec `json:"thresholds,omitempty"`
}

// GroomingThresholdSpec is the DECLARED form of the significance bar: every
// scalar is a POINTER so "declared" stays distinguishable from "zero". A
// value-typed MinRankMovement could not tell an omitted field from a declared
// 0, and 0 is precisely the value that would disable the guard.
type GroomingThresholdSpec struct {
	MinRankMovement           *int     `json:"min_rank_movement,omitempty"`
	MinScoreDelta             *float64 `json:"min_score_delta,omitempty"`
	SignificantHygieneDefects []string `json:"significant_hygiene_defects,omitempty"`
}

// Selection declares WHICH board view feeds work selection for this tenant
// (E45.24 / #2231) — the per-tenant answer to "where does the backlog this
// repo works from actually come from?".
//
// POINTER for the same reason Charter and Grooming are: absent (nil) must stay
// distinguishable from present-but-empty, so a consumer can tell "no selection
// source declared" from "declared, with nothing set".
//
// DECLARATION ONLY — there is no consumer. Nothing in production reads
// Conventions.Selection; the board-view-as-selection-source feature is
// deferred post-alpha by ADR-064, and this slice reserves the declaration so
// a tenant's intent has one recorded home. There is deliberately NO resolver
// (no ResolveSelection): an unused resolver would have to invent a default
// ordering, which is a policy #2231 does not decide.
//
// It lives in the work-management conventions rather than as a `board:` block
// in the workflow spec (ADR-064 fork 1) because the provider, project ref,
// states and transitions the selection source is read against already live
// here, and the workflow spec stays a pure governance surface.
//
// NO *SemanticError rule is added. Structural validation — required
// source_view, minLength 1, the closed order_by enum, additionalProperties
// false — is its whole declaration-time contract, the same posture Charter
// takes. A cross-check tying SourceView to a States value would be a second
// owner of a rule no consumer enforces yet, and it would reject a legitimate
// view that is not a status column at all (a saved GitHub Projects view, a
// GitLab board list). Per-forge readability constraints on a declared source
// are documented in docs/board-capability-matrix.md.
type Selection struct {
	// SourceView is the PROVIDER-side board view or column name whose
	// membership feeds selection (e.g. "Up Next" — the provider option, not
	// the canonical state up_next). The schema requires it and enforces
	// minLength 1, so a parsed Selection always carries a non-empty view.
	SourceView string `json:"source_view"`
	// OrderBy is one of priority, rank, created_at — enforced by the schema's
	// closed enum. EMPTY means UNSET: no ordering policy is declared, and
	// nothing populates it. The schema deliberately carries no `default`
	// annotation, because a JSON Schema default populates nothing and would
	// advertise behaviour no code implements. The future consumer of this
	// block is what will have to decide what unset resolves to.
	OrderBy string `json:"order_by,omitempty"`
}

// The conservative shipped defaults, declared as named constants so
// docs/spec/work-management-default.yaml and the Go fallback have ONE citable
// source. TestShippedDefaultMatchesCodeDefaults asserts the two cannot drift.
const (
	// DefaultGroomingMinRankMovement suppresses a one-position swap — #2240's
	// own first churn example — and proposes anything larger.
	DefaultGroomingMinRankMovement = 2
	// DefaultGroomingMinScoreDelta suppresses a 0.02 rescoring move — #2240's
	// second churn example — and proposes anything larger.
	DefaultGroomingMinScoreDelta = 0.05
)

// DefaultSignificantHygieneDefects is the default significance set: ALL six of
// grooming-report-v1's hygiene defect classes. Absent declaration means every
// class is significant, which is the conservative direction for a suppression
// control — a repo silences a class by naming the ones it keeps.
//
// Returned as a fresh copy so a caller cannot mutate the package default.
func DefaultSignificantHygieneDefects() []string {
	return []string{
		"absent_done_means",
		"missing_estimate",
		"missing_label_namespace",
		"missing_parent_epic_link",
		"unboarded",
		"unlinked_parent_epic",
	}
}

// GroomingThresholds is the fully RESOLVED significance bar — every field
// populated, no pointers, safe to pass by value into the pure guard.
//
// Clamped names any declared field this resolve REPLACED with the package
// default because the declared value would have disabled the guard. It is a
// recorded outcome rather than a silent substitution: a control that can be
// declared into a no-op is not a control, and an operator who tried needs to
// see that the attempt did not take.
type GroomingThresholds struct {
	MinRankMovement           int      `json:"min_rank_movement"`
	MinScoreDelta             float64  `json:"min_score_delta"`
	SignificantHygieneDefects []string `json:"significant_hygiene_defects"`
	Clamped                   []string `json:"clamped,omitempty"`
}

// IsSignificantHygieneDefect reports whether a hygiene defect class is one the
// operator considers worth proposing. Resolution always populates the set, so
// an empty set here can only come from a hand-built zero value; it is treated
// as "nothing is significant" rather than silently meaning "everything", since
// a resolved value is the only supported input.
func (t GroomingThresholds) IsSignificantHygieneDefect(defect string) bool {
	for _, d := range t.SignificantHygieneDefects {
		if d == defect {
			return true
		}
	}
	return false
}

// ResolveGroomingThresholds returns the fully-resolved significance bar,
// substituting the package defaults for every absent field.
//
// It resolves FAIL-CLOSED on a declared value that would disable the guard —
// a non-positive rank movement, a non-positive score delta, an
// empty-after-trim defect name, an all-blank defect set. Each such field is
// clamped to its default and named in Clamped. The JSON Schema already rejects
// these at parse time, so this is the SECOND line, for a Conventions value
// constructed in Go rather than parsed from YAML (which every server-side
// caller building a config in code does).
func (c Conventions) ResolveGroomingThresholds() GroomingThresholds {
	out := GroomingThresholds{
		MinRankMovement:           DefaultGroomingMinRankMovement,
		MinScoreDelta:             DefaultGroomingMinScoreDelta,
		SignificantHygieneDefects: DefaultSignificantHygieneDefects(),
	}
	if c.Grooming == nil || c.Grooming.Thresholds == nil {
		return out
	}
	spec := c.Grooming.Thresholds

	if spec.MinRankMovement != nil {
		if *spec.MinRankMovement >= 1 {
			out.MinRankMovement = *spec.MinRankMovement
		} else {
			out.Clamped = append(out.Clamped, "min_rank_movement")
		}
	}
	if spec.MinScoreDelta != nil {
		if *spec.MinScoreDelta > 0 {
			out.MinScoreDelta = *spec.MinScoreDelta
		} else {
			out.Clamped = append(out.Clamped, "min_score_delta")
		}
	}
	if spec.SignificantHygieneDefects != nil {
		declared := make([]string, 0, len(spec.SignificantHygieneDefects))
		for _, d := range spec.SignificantHygieneDefects {
			if trimmed := strings.TrimSpace(d); trimmed != "" {
				declared = append(declared, trimmed)
			}
		}
		if len(declared) > 0 {
			sort.Strings(declared)
			out.SignificantHygieneDefects = declared
		} else {
			out.Clamped = append(out.Clamped, "significant_hygiene_defects")
		}
	}
	return out
}

// ProductFeedback configures the upstream product-feedback egress path
// (#1006). It is a per-repo kill-switch: when present with
// enabled=false the product-report endpoint returns 403
// product_feedback_disabled and files nothing. Absent (nil) means
// enabled — egress is on by default, so an existing config keeps working
// without edits.
type ProductFeedback struct {
	Enabled bool `json:"enabled"`
}

// ProductFeedbackEnabled reports whether upstream product-feedback egress
// is allowed for this repo. Egress is on by default; only an explicit
// product_feedback.enabled=false kill-switch disables it.
func (c Conventions) ProductFeedbackEnabled() bool {
	return c.ProductFeedback == nil || c.ProductFeedback.Enabled
}

// Project is the GitHub Projects connection. Owner is the project owner
// login; OwnerType selects the GraphQL owner query (user vs organization
// — the Project #7 trap); Number is the integer project number.
type Project struct {
	Owner     string `json:"owner"`
	OwnerType string `json:"owner_type,omitempty"`
	Number    int    `json:"number"`
}

// JiraConnection is the Jira connection. ProjectKey selects the target
// Jira project (e.g. FISH) that filed issues are created under; IssueTypes
// optionally maps a canonical work-item type to the Jira issue-type name,
// with absent entries defaulting to a title-cased fallback in the provider.
// ParentField selects the field used to link a created issue to its parent
// epic: the default `parent` reference for team-managed (next-gen) projects,
// or a classic epic-link custom field id (e.g. customfield_10014) for
// company-managed projects; empty means the team-managed `parent` default.
// The Jira instance base URL and credentials are server-side env
// (FISHHAWKD_JIRA_*), never in this checked-in config, so this block
// carries no secrets and no base URL.
type JiraConnection struct {
	ProjectKey  string            `json:"project_key"`
	IssueTypes  map[string]string `json:"issue_types,omitempty"`
	ParentField string            `json:"parent_field,omitempty"`
}

// GitLabConnection is the GitLab connection (ADR-058 Phase 2). Project
// optionally overrides the target GitLab project — a namespaced project path
// (e.g. group/subgroup/project) that filed issues are created under; absent
// means the provider resolves the filing repo's owner/name path instead. The
// GitLab instance base URL and token are server-side env
// (FISHHAWKD_GITLAB_BASE_URL / FISHHAWKD_GITLAB_TOKEN), never in this
// checked-in config, so this block carries no secrets and no base URL —
// matching the jira precedent. GitLab boards are label-driven, so the
// canonical-state map's values are GitLab label names and the provider treats
// applying the state label at create time as board placement; the parent-epic
// link uses the Free-tier issue-links API rather than Premium group epics.
type GitLabConnection struct {
	Project string `json:"project,omitempty"`
}

// ItemType is the conventions for one work-item type.
type ItemType struct {
	TitleFormat  string   `json:"title_format,omitempty"`
	BodySkeleton []string `json:"body_skeleton"`
	// OptionalSections names body_skeleton headings that render only when the
	// filing supplies content for them (an absent Sections key omits the
	// heading entirely; a present key — even empty — renders it in position).
	// Every entry must appear in BodySkeleton (validateSemantics enforces,
	// fail-closed). Sections not listed here render unconditionally.
	OptionalSections []string `json:"optional_sections,omitempty"`
	DefaultLabels    []string `json:"default_labels,omitempty"`
	// LabelDefaults maps a label namespace (e.g. "autonomy") to the full
	// default label string applied at filing time when the merged label set
	// carries nothing in that namespace (#1616). validateSemantics enforces
	// that every value begins with "<key>:", fail-closed.
	LabelDefaults map[string]string `json:"label_defaults,omitempty"`
	// RequiredLabelNamespaces names the label namespaces a filed item should
	// carry after merge, derivation, and defaulting (#1616). A still-absent
	// namespace is reported loudly in Classification.MissingLabelNamespaces —
	// never a rejection (fail-open).
	RequiredLabelNamespaces []string      `json:"required_label_namespaces,omitempty"`
	DefaultFields           DefaultFields `json:"default_fields,omitempty"`
	Numbering               *Numbering    `json:"numbering,omitempty"`
	EpicLink                string        `json:"epic_link,omitempty"`
}

// DefaultFields holds a type's default board placement and complexity.
type DefaultFields struct {
	Status      string `json:"status,omitempty"`
	BoardColumn string `json:"board_column,omitempty"`
	Complexity  string `json:"complexity,omitempty"`
}

// Numbering is a type's numbering rule. Scheme is the allocation scheme
// (sequential in v0); Prefix is rendered into the title (e.g. ADR-); Pad is
// the zero-pad minimum width for the {number} substitution (e.g. 3 -> 041),
// 0/absent meaning no padding.
type Numbering struct {
	Scheme string `json:"scheme"`
	Prefix string `json:"prefix,omitempty"`
	Pad    int    `json:"pad,omitempty"`
}

// Default returns the shipped default work-management conventions, parsed
// from the embedded copy and validated against the embedded schema at
// package init. Callers must treat the returned value as read-only: the
// slices and maps are shared.
func Default() Conventions { return defaultConventions }

// EmbeddedSchemaHash returns the hex-encoded SHA-256 of the canonical
// JSON bytes of the embedded work-management-v0 schema. Callers use this
// to detect schema drift between components at startup, matching the
// spec, plan, and operatorrole packages' convention.
func EmbeddedSchemaHash() string { return embeddedSchemaHash }

// YAMLError is returned when the config input is not parseable as YAML or
// is empty.
type YAMLError struct {
	Msg   string
	Cause error
}

func (e *YAMLError) Error() string {
	if e.Msg != "" {
		return "workmgmt: yaml: " + e.Msg
	}
	if e.Cause != nil {
		return "workmgmt: yaml: " + e.Cause.Error()
	}
	return "workmgmt: yaml: unknown error"
}

// Unwrap exposes the underlying error so callers can errors.As against
// yaml package error types when they need line/column information.
func (e *YAMLError) Unwrap() error { return e.Cause }

// SchemaError is returned when the config parses but doesn't satisfy the
// schema. Path is a JSON Pointer (RFC 6901) pointing at the offending
// instance location; Message is the schema's reported reason.
type SchemaError struct {
	Path    string
	Message string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("workmgmt: schema: %s: %s", e.Path, e.Message)
}

// SemanticError is returned when the config satisfies the schema but
// violates a cross-field rule the schema can't express (the mandatory
// required-field trio, the github_projects connection requirement, ADR
// numbering, complexity cross-reference). It is also the apply-time error
// for a filing that violates its type's conventions (an unresolved title
// placeholder, an off-skeleton sections key).
//
// Details optionally carries machine-readable structure for the caller's
// 422 work_item_invalid response (e.g. missing_placeholders,
// unknown_sections, expected_sections). It defaults nil so Error() and
// every existing errors.As(&sem) call site are unchanged — the field is
// purely additive.
type SemanticError struct {
	Msg     string
	Details map[string]any
}

func (e *SemanticError) Error() string {
	return "workmgmt: semantic: " + e.Msg
}

// Parse reads a work-management conventions document from r and returns
// the typed config. It validates in two stages — JSON Schema, then the
// semantic checks the schema can't express — so a structural failure
// returns a *SchemaError, a cross-field failure a *SemanticError, and
// unparseable input a *YAMLError. Use errors.As to distinguish them. The
// document must be a single YAML document; a multi-document stream is
// rejected with a *YAMLError, since trailing documents would otherwise
// bypass validation entirely.
func Parse(r io.Reader) (Conventions, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Conventions{}, fmt.Errorf("read: %w", err)
	}
	return parse(data)
}

func parse(data []byte) (Conventions, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Conventions{}, &YAMLError{Msg: "empty document"}
	}

	var raw any
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false) // permissive at YAML layer; schema is the gate
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return Conventions{}, &YAMLError{Msg: "empty document"}
		}
		return Conventions{}, &YAMLError{Msg: err.Error(), Cause: err}
	}

	// Only the first document is validated, so any trailing document
	// would escape the schema and the semantic checks entirely. Reject
	// the stream outright.
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Conventions{}, &YAMLError{Msg: "multiple YAML documents in input: a work-management config must be a single document"}
	}

	if err := configSchema.Validate(raw); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return Conventions{}, schemaErrorFrom(verr)
		}
		return Conventions{}, &SchemaError{Path: "/", Message: err.Error()}
	}

	// Round-trip through JSON into the typed struct. The schema has
	// already enforced shapes/enums, so this only fails on internal bugs.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return Conventions{}, fmt.Errorf("re-marshal config: %w", err)
	}
	var c Conventions
	jdec := json.NewDecoder(bytes.NewReader(jsonBytes))
	jdec.DisallowUnknownFields()
	if err := jdec.Decode(&c); err != nil {
		return Conventions{}, fmt.Errorf("decode config: %w", err)
	}

	if err := validateSemantics(c); err != nil {
		return Conventions{}, err
	}
	return c, nil
}

// validateSemantics enforces the cross-field rules the JSON Schema can't
// express: the mandatory required-field trio, the github_projects, jira, and
// gitlab connection requirements, ADR numbering, the complexity
// cross-reference, and the transitions->states cross-reference (every
// configured transition target must name a canonical state declared in the
// states map).
func validateSemantics(c Conventions) error {
	if missing := missingMandatoryFields(c.RequiredFields); len(missing) > 0 {
		return &SemanticError{Msg: fmt.Sprintf(
			"required_fields must include the mandatory trio; missing: %s",
			strings.Join(missing, ", "))}
	}

	if c.Provider == "github_projects" && c.Project == nil {
		return &SemanticError{Msg: "provider github_projects requires a project connection block (owner + number)"}
	}

	if c.Provider == "jira" && c.Jira == nil {
		return &SemanticError{Msg: "provider jira requires a jira connection block (project_key)"}
	}

	// The gitlab block is required under provider gitlab (mirroring jira) and
	// rejected under any other provider. The off-provider rejection is
	// fail-closed: a gitlab block under github_projects/jira is almost
	// certainly a misconfiguration (a copied-in connection the active
	// provider will silently ignore), so it is surfaced rather than dropped.
	if c.Provider == "gitlab" && c.GitLab == nil {
		return &SemanticError{Msg: "provider gitlab requires a gitlab connection block (project optional)"}
	}
	if c.Provider != "gitlab" && c.GitLab != nil {
		return &SemanticError{Msg: fmt.Sprintf(
			"gitlab connection block is set but provider is %q, not gitlab", c.Provider)}
	}

	// Type keys are deterministically ordered so the first failure is
	// stable across runs. A type named "adr" must carry a numbering rule;
	// the schema can't express the conditional, so it's enforced here. Each
	// optional_sections entry must also name a section in that type's
	// body_skeleton — otherwise the render skip would key on a heading that
	// never exists, so it's rejected fail-closed.
	for _, name := range sortedTypeNames(c.Types) {
		it := c.Types[name]
		if name == "adr" && it.Numbering == nil {
			return &SemanticError{Msg: "type \"adr\" must declare a numbering rule"}
		}
		if err := validateOptionalSections(name, it); err != nil {
			return err
		}
		if err := validateLabelDefaults(name, it); err != nil {
			return err
		}
	}

	// Every lifecycle transition must target a canonical state that the
	// states map actually declares — otherwise the board-sync hook would
	// have no provider option to move the card to. The schema constrains
	// transition values to the canonical enum, but only this check ties a
	// value to a configured states key. Event keys are sorted so the first
	// failure is stable across runs.
	for _, event := range sortedKeys(c.Transitions) {
		target := c.Transitions[event]
		if _, ok := c.States[target]; !ok {
			return &SemanticError{Msg: fmt.Sprintf(
				"transition %q targets canonical state %q, which is not declared in states", event, target)}
		}
	}
	return nil
}

// validateOptionalSections enforces the cross-field rule the JSON Schema
// can't express: every optional_sections entry for a type must name a
// section present (exact match) in that type's body_skeleton. A stray entry
// would key the render skip on a heading assembleBody never emits, so it's
// rejected fail-closed with a *SemanticError naming the type, the offending
// entry, and the body_skeleton it is absent from.
func validateOptionalSections(name string, it ItemType) error {
	if len(it.OptionalSections) == 0 {
		return nil
	}
	inSkeleton := make(map[string]bool, len(it.BodySkeleton))
	for _, s := range it.BodySkeleton {
		inSkeleton[s] = true
	}
	for _, opt := range it.OptionalSections {
		if !inSkeleton[opt] {
			return &SemanticError{Msg: fmt.Sprintf(
				"type %q lists optional_section %q, which is absent from its body_skeleton [%s]",
				name, opt, strings.Join(it.BodySkeleton, ", "))}
		}
	}
	return nil
}

// validateLabelDefaults enforces the cross-field rule the JSON Schema can't
// express: every label_defaults value must begin with its key's namespace
// prefix "<key>:" (#1616). A key "autonomy" mapped to a bare "high" (missing
// the "autonomy:" prefix) would apply a namespace-less label the completeness
// pass could never suppress by a caller's own autonomy label, so it is
// rejected fail-closed with a *SemanticError naming the type, key, and value.
// Keys are iterated in sorted order so the first failure is stable across runs.
func validateLabelDefaults(name string, it ItemType) error {
	if len(it.LabelDefaults) == 0 {
		return nil
	}
	for _, key := range sortedKeys(it.LabelDefaults) {
		value := it.LabelDefaults[key]
		if !strings.HasPrefix(value, key+":") {
			return &SemanticError{Msg: fmt.Sprintf(
				"type %q label_defaults[%q] = %q must begin with the namespace prefix %q",
				name, key, value, key+":")}
		}
	}
	return nil
}

// missingMandatoryFields returns the canonical labels of the mandatory
// fields not present in required, normalizing each entry so "Done-means"
// and "Done means" both satisfy the rule.
func missingMandatoryFields(required []string) []string {
	present := make(map[string]bool, len(required))
	for _, f := range required {
		present[normalizeField(f)] = true
	}
	var missing []string
	for key, label := range mandatoryFields {
		if !present[key] {
			missing = append(missing, label)
		}
	}
	sort.Strings(missing)
	return missing
}

// normalizeField lowercases a field name and strips hyphens and spaces so
// the mandatory-field check is robust to "Done-means" vs "Done means".
func normalizeField(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

func sortedTypeNames(types map[string]ItemType) []string {
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func mustCompileSchema(path string) *jsonschema.Schema {
	data, err := embedded.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: read embedded schema %s: %v", path, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("workmgmt: parse embedded schema %s: %v", path, err))
	}
	name := strings.TrimPrefix(path, "schemas/")
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name, raw); err != nil {
		panic(fmt.Sprintf("workmgmt: register embedded schema %s: %v", path, err))
	}
	s, err := c.Compile(name)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: compile embedded schema %s: %v", path, err))
	}
	return s
}

func computeSchemaHash() string {
	data, err := embedded.ReadFile(schemaPath)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: read embedded schema for hash %s: %v", schemaPath, err))
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		panic(fmt.Sprintf("workmgmt: parse embedded schema for hash %s: %v", schemaPath, err))
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: re-marshal embedded schema for hash %s: %v", schemaPath, err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// mustParseDefault parses and validates the embedded default config. Any
// failure is a build-artifact bug, so it panics at package init.
func mustParseDefault() Conventions {
	data, err := embedded.ReadFile(defaultSpecPath)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: read embedded default %s: %v", defaultSpecPath, err))
	}
	c, err := parse(data)
	if err != nil {
		panic(fmt.Sprintf("workmgmt: embedded default %s is invalid: %v", defaultSpecPath, err))
	}
	return c
}

// schemaErrorFrom collapses a jsonschema.ValidationError tree to the most
// actionable leaf for callers, mirroring the operatorrole/spec packages.
func schemaErrorFrom(verr *jsonschema.ValidationError) *SchemaError {
	leaf := deepestLeaf(verr)
	loc := leaf.InstanceLocation
	if len(loc) == 0 {
		loc = []string{""}
	}
	return &SchemaError{
		Path:    "/" + strings.Join(loc, "/"),
		Message: kindMessage(leaf),
	}
}

// kindMessage returns a human-readable description of a single validation
// failure. The library's ErrorKind.LocalizedString takes a non-nil
// *message.Printer (passing nil panics inside x/text), so instead we lean
// on the leaf's Error() output and trim its prefix so the caller-formatted
// Path isn't repeated.
func kindMessage(v *jsonschema.ValidationError) string {
	full := v.Error()
	if idx := strings.LastIndex(full, ": "); idx >= 0 {
		return full[idx+2:]
	}
	return full
}

// deepestLeaf walks the validation error tree to the most specific
// failure; the v6 library wraps each rule violation, so the deepest node
// is closest to the offending field.
func deepestLeaf(v *jsonschema.ValidationError) *jsonschema.ValidationError {
	for _, c := range v.Causes {
		if len(c.InstanceLocation) >= len(v.InstanceLocation) {
			return deepestLeaf(c)
		}
	}
	return v
}
