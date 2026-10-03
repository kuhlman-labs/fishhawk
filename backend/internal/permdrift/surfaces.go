package permdrift

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

// SurfacesVersion is the version of the product's permission-surface list
// (DefaultSurfaces). Bump it whenever a surface is added, removed or re-keyed,
// so an audit entry recording it says which list a check ran against.
const SurfacesVersion = 1

// Kind selects a surface's extractor.
type Kind string

// The surface kinds. Each names one extractor and, for a file carrying two
// independently-severe parts (a GitHub App manifest's permissions and its
// events), one part of it.
const (
	KindActionsWorkflow      Kind = "gha_workflow_permissions"
	KindAppPermissionsJSON   Kind = "github_app_permissions_json"
	KindAppEventsJSON        Kind = "github_app_events_json"
	KindAppPermissionsGo     Kind = "github_app_permissions_go"
	KindAppEventsGo          Kind = "github_app_events_go"
	KindMCPToolScopes        Kind = "mcp_tool_scopes"
	KindRunTokenScopes       Kind = "run_token_scopes"
	KindEnvAllowList         Kind = "env_allowlist"
	KindSpecForbiddenPaths   Kind = "fishhawk_spec_forbidden_paths"
	KindSpecEscalations      Kind = "fishhawk_spec_escalations"
	KindSpecAutonomy         Kind = "fishhawk_spec_autonomy"
	KindSpecStagePermissions Kind = "fishhawk_spec_stage_permissions"
	KindSurfaceDeclarations  Kind = "surface_declarations"
)

// surfaceDeclarationsPrefix is the key prefix of a surface declaration.
const surfaceDeclarationsPrefix = "surfaces["

// Severity is the severity a widening on a surface is raised at. The values
// are the concern severity vocabulary.
type Severity string

// The severities.
const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

// severityRank orders severities least to most severe.
var severityRank = map[Severity]int{SeverityLow: 1, SeverityMedium: 2, SeverityHigh: 3}

// Surface is one declared permission surface.
type Surface struct {
	// ID is the stable surface identity (part of every check key).
	ID string
	// Kind selects the extractor.
	Kind Kind
	// Paths are doublestar globs over repo-relative paths.
	Paths []string
	// Severity is the concern severity a widening is raised at.
	Severity Severity
	// Description says what the surface governs.
	Description string
}

// The product surface ids the server and tests name.
const (
	SurfaceIDForbiddenPaths      = "fishhawk-spec-forbidden-paths"
	SurfaceIDSurfaceDeclarations = "permission-surface-declarations"
)

// RepoSurfacesPath is where a repository extends the surface list. The server
// reads it at the run's BASE commit only, so a change cannot remove its own
// surface; it is itself the permission-surface-declarations surface.
const RepoSurfacesPath = ".fishhawk/permission-surfaces.yaml"

// DefaultSurfaces returns the product's versioned surface list
// (SurfacesVersion). The severity map was ratified at the E80.4 plan gate:
// high everywhere, medium for GitHub App events (subscribing to an event adds
// received data, not acting power). A fresh slice per call, so a caller may
// append to it.
func DefaultSurfaces() []Surface {
	const spec = ".fishhawk/workflows.yaml"
	const manifestJSON = "docs/github-app/manifest.template.json"
	const manifestGo = "backend/internal/server/manifest.go"
	return []Surface{
		{ID: "gha-workflow-permissions", Kind: KindActionsWorkflow, Severity: SeverityHigh,
			Paths:       []string{".github/workflows/*.yml", ".github/workflows/*.yaml"},
			Description: "GitHub Actions workflow GITHUB_TOKEN permissions, effective per job"},
		{ID: "github-app-permissions-template", Kind: KindAppPermissionsJSON, Severity: SeverityHigh,
			Paths:       []string{manifestJSON},
			Description: "GitHub App manifest template default_permissions"},
		{ID: "github-app-permissions-go", Kind: KindAppPermissionsGo, Severity: SeverityHigh,
			Paths:       []string{manifestGo},
			Description: "GitHub App manifest default_permissions built in Go"},
		{ID: "github-app-events-template", Kind: KindAppEventsJSON, Severity: SeverityMedium,
			Paths:       []string{manifestJSON},
			Description: "GitHub App manifest template default_events"},
		{ID: "github-app-events-go", Kind: KindAppEventsGo, Severity: SeverityMedium,
			Paths:       []string{manifestGo},
			Description: "GitHub App manifest default_events built in Go"},
		{ID: "mcp-tool-scopes", Kind: KindMCPToolScopes, Severity: SeverityHigh,
			Paths:       []string{"backend/internal/server/mcpscopes.go"},
			Description: "the /mcp tool -> required-scope table"},
		{ID: "run-token-scope-grants", Kind: KindRunTokenScopes, Severity: SeverityHigh,
			Paths:       []string{"backend/internal/server/mcptoken.go"},
			Description: "run-bound MCP token scope grants per stage type"},
		{ID: "reviewer-env-allowlist", Kind: KindEnvAllowList, Severity: SeverityHigh,
			Paths:       []string{"backend/internal/reviewsandbox/env.go"},
			Description: "reviewer subprocess environment allow-lists"},
		{ID: SurfaceIDForbiddenPaths, Kind: KindSpecForbiddenPaths, Severity: SeverityHigh,
			Paths:       []string{spec},
			Description: "workflow-spec stage forbidden_paths"},
		{ID: "fishhawk-spec-escalations", Kind: KindSpecEscalations, Severity: SeverityHigh,
			Paths:       []string{spec},
			Description: "workflow-spec escalations and their autonomy ceilings"},
		{ID: "fishhawk-spec-autonomy", Kind: KindSpecAutonomy, Severity: SeverityHigh,
			Paths:       []string{spec},
			Description: "workflow-spec autonomy tiers (workflow and gate)"},
		{ID: "fishhawk-spec-stage-permissions", Kind: KindSpecStagePermissions, Severity: SeverityHigh,
			Paths:       []string{spec},
			Description: "workflow-spec stage permissions and egress"},
		{ID: SurfaceIDSurfaceDeclarations, Kind: KindSurfaceDeclarations, Severity: SeverityHigh,
			Paths:       []string{RepoSurfacesPath},
			Description: "the repository's own permission-surface declarations"},
	}
}

// knownKinds is every Kind an extractor exists for.
var knownKinds = map[Kind]bool{
	KindActionsWorkflow: true, KindAppPermissionsJSON: true, KindAppEventsJSON: true,
	KindAppPermissionsGo: true, KindAppEventsGo: true, KindMCPToolScopes: true,
	KindRunTokenScopes: true, KindEnvAllowList: true, KindSpecForbiddenPaths: true,
	KindSpecEscalations: true, KindSpecAutonomy: true, KindSpecStagePermissions: true,
	KindSurfaceDeclarations: true,
}

// MatchSurfaces returns the surfaces (in list order) one of whose globs
// matches path.
func MatchSurfaces(surfaces []Surface, path string) []Surface {
	var out []Surface
	for _, s := range surfaces {
		for _, p := range s.Paths {
			if ok, err := doublestar.Match(p, path); err == nil && ok {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// FileSide is one side (base or head) of a surface file.
type FileSide struct {
	Content []byte
	// Exists is false when the file is absent at that ref.
	Exists bool
}

// The unevaluable reason classes. Detect returns the first two; the server
// raises the rest for failures before Detect can run.
const (
	ReasonParseError        = "parse_error"
	ReasonShapeUnrecognized = "shape_unrecognized"
	ReasonFetchFailed       = "fetch_failed"
	ReasonCompareFailed     = "compare_failed"
	ReasonCompareTruncated  = "compare_truncated"
)

// Result is one surface file's evaluation.
type Result struct {
	Widened  []Change
	Narrowed []Change
	// Unevaluable is the reason class when the file could not be compared
	// ("" when it could). An unevaluable result carries no changes: it fails
	// CLOSED to one concern, never to a narrowing.
	Unevaluable string
	// Detail is a STRUCTURAL description of an unevaluable result (an anchor
	// name, a line number, an expression kind) for logs; it never carries
	// file bytes.
	Detail string
}

// Detect evaluates one surface file: it extracts base and head with the
// surface's extractor and compares them. A parse failure on either side is
// Unevaluable parse_error; for a Go-source surface the shape guard
// (shapeProblem) is Unevaluable shape_unrecognized. An absent side extracts
// as empty.
func Detect(s Surface, base, head FileSide) Result {
	b, h, reason, detail := extractPair(s, base, head)
	if reason != "" {
		return Result{Unevaluable: reason, Detail: detail}
	}
	var r Result
	for _, c := range Compare(b, h) {
		if c.Direction == Widened {
			r.Widened = append(r.Widened, c)
		} else {
			r.Narrowed = append(r.Narrowed, c)
		}
	}
	return r
}

// sideContent is a side's bytes, nil when absent.
func sideContent(f FileSide) []byte {
	if !f.Exists {
		return nil
	}
	return f.Content
}

// extractPair runs s's extractor over both sides.
func extractPair(s Surface, base, head FileSide) (Grants, Grants, string, string) {
	bc, hc := sideContent(base), sideContent(head)
	if goEx, prefix, ok := goExtractorFor(s.Kind); ok {
		bx, err := goEx(bc)
		if err != nil {
			return nil, nil, ReasonParseError, "base: go source does not parse"
		}
		hx, err := goEx(hc)
		if err != nil {
			return nil, nil, ReasonParseError, "head: go source does not parse"
		}
		bx, hx = bx.withPrefix(prefix), hx.withPrefix(prefix)
		if p := shapeProblem(bx, hx); p != "" {
			return nil, nil, ReasonShapeUnrecognized, p
		}
		return bx.Grants, hx.Grants, "", ""
	}
	ex, prefix, ok := extractorFor(s.Kind)
	if !ok {
		return nil, nil, ReasonParseError, "unknown surface kind"
	}
	bg, err := ex(bc)
	if err != nil {
		return nil, nil, ReasonParseError, "base does not parse"
	}
	hg, err := ex(hc)
	if err != nil {
		return nil, nil, ReasonParseError, "head does not parse"
	}
	return filterPrefix(bg, prefix), filterPrefix(hg, prefix), "", ""
}

// filterPrefix keeps g's entries whose key starts with prefix ("" keeps all).
func filterPrefix(g Grants, prefix string) Grants {
	if prefix == "" {
		return g
	}
	out := Grants{}
	for k, e := range g {
		if strings.HasPrefix(k, prefix) {
			out[k] = e
		}
	}
	return out
}

// extractorFor returns a structured kind's extractor and key prefix.
func extractorFor(k Kind) (func([]byte) (Grants, error), string, bool) {
	switch k {
	case KindActionsWorkflow:
		return ExtractActions, "", true
	case KindAppPermissionsJSON:
		return ExtractManifest, ManifestPermissionPrefix, true
	case KindAppEventsJSON:
		return ExtractManifest, ManifestEventPrefix, true
	case KindSpecForbiddenPaths:
		return ExtractSpecForbiddenPaths, "", true
	case KindSpecEscalations:
		return ExtractSpecEscalations, "", true
	case KindSpecAutonomy:
		return ExtractSpecAutonomy, "", true
	case KindSpecStagePermissions:
		return ExtractSpecStagePermissions, "", true
	case KindSurfaceDeclarations:
		return ExtractSurfaceDeclarations, "", true
	}
	return nil, "", false
}

// goExtractorFor returns a Go-source kind's extractor and key prefix.
func goExtractorFor(k Kind) (func([]byte) (GoExtraction, error), string, bool) {
	switch k {
	case KindAppPermissionsGo:
		return ExtractGoManifest, ManifestPermissionPrefix, true
	case KindAppEventsGo:
		return ExtractGoManifest, ManifestEventPrefix, true
	case KindMCPToolScopes:
		return ExtractGoMCPScopes, MCPToolPrefix, true
	case KindRunTokenScopes:
		return ExtractGoRunTokenScopes, RunTokenPrefix, true
	case KindEnvAllowList:
		return ExtractGoEnvAllow, EnvAllowPrefix, true
	}
	return nil, "", false
}

// ExtractSurfaceDeclarations extracts a repository surface-declarations file
// (RepoSurfacesPath). Only ACCEPTED entries count (an entry made invalid at
// head therefore vanishes, which is a widening). Per accepted surface:
//
//   - `surfaces[<id>]`: presence Restriction (removing a surface widens);
//   - `surfaces[<id>].paths[<glob>]`: presence Restriction;
//   - `surfaces[<id>].kind[<kind>]`: presence Restriction, keyed by value so
//     a kind change reads as one removed and one added;
//   - `surfaces[<id>].severity`: ranked Restriction whose rank is the
//     NEGATED severity, so lowering a severity is a widening.
func ExtractSurfaceDeclarations(content []byte) (Grants, error) {
	out := Grants{}
	accepted, _, err := ParseRepoSurfaces(content)
	if err != nil {
		return nil, err
	}
	for _, s := range accepted {
		k := surfaceDeclarationsPrefix + s.ID + "]"
		out.Put(Entry{Key: k, Value: Present, Rank: PresenceRank, Polarity: Restriction})
		for _, p := range s.Paths {
			out.Put(Entry{Key: k + ".paths[" + p + "]", Value: Present, Rank: PresenceRank, Polarity: Restriction})
		}
		out.Put(Entry{Key: k + ".kind[" + string(s.Kind) + "]", Value: Present, Rank: PresenceRank, Polarity: Restriction})
		out.Put(Entry{Key: k + ".severity", Value: string(s.Severity), Rank: -severityRank[s.Severity], Polarity: Restriction})
	}
	return out, nil
}

// The extension-entry rejection reasons.
const (
	RejectUnknownKind        = "unknown_kind"
	RejectBadID              = "bad_id"
	RejectDuplicateID        = "duplicate_id"
	RejectProductIDCollision = "product_id_collision"
	RejectEmptyPaths         = "empty_paths"
	RejectBadPath            = "bad_path"
	RejectBadSeverity        = "bad_severity"
)

// Rejection names one extension entry that was not accepted.
type Rejection struct {
	// Index is the entry's position in the surfaces list.
	Index int
	// ID is the entry's id as written (possibly malformed or empty).
	ID string
	// Reason is one of the Reject* constants.
	Reason string
}

// repoSurfacesFile is the version-1 extension format.
type repoSurfacesFile struct {
	Version  *int               `yaml:"version"`
	Surfaces []repoSurfaceEntry `yaml:"surfaces"`
}

type repoSurfaceEntry struct {
	ID          string   `yaml:"id"`
	Kind        string   `yaml:"kind"`
	Paths       []string `yaml:"paths"`
	Severity    string   `yaml:"severity"`
	Description string   `yaml:"description"`
}

// surfaceIDPattern is a well-formed surface id.
var surfaceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseRepoSurfaces parses a repository's RepoSurfacesPath (version 1):
//
//	version: 1
//	surfaces:
//	  - id: infra-app-permissions
//	    kind: github_app_permissions_json
//	    paths: ["infra/app.json"]
//	    severity: high        # low | medium | high; omitted = high
//	    description: "..."
//
// Decoding is STRICT: an unknown key anywhere, a missing or unsupported
// version, or YAML that does not parse is an error (the whole file). An
// individual entry is REJECTED — named in rejected, never fatal — for an
// unknown kind, a malformed or duplicate id, an id colliding with a product
// surface (an extension may only ADD surfaces), empty paths or an invalid
// glob, or a bad severity. Empty content (no file) is no surfaces.
func ParseRepoSurfaces(content []byte) (accepted []Surface, rejected []Rejection, err error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	var f repoSurfacesFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) { // a comment-only document
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("permission surfaces: %w", err)
	}
	if f.Version == nil {
		return nil, nil, errors.New("permission surfaces: version is required (want 1)")
	}
	if *f.Version != 1 {
		return nil, nil, fmt.Errorf("permission surfaces: unsupported version %d (want 1)", *f.Version)
	}
	product := map[string]bool{}
	for _, s := range DefaultSurfaces() {
		product[s.ID] = true
	}
	seen := map[string]bool{}
	for i, e := range f.Surfaces {
		reject := func(reason string) { rejected = append(rejected, Rejection{Index: i, ID: e.ID, Reason: reason}) }
		switch {
		case !surfaceIDPattern.MatchString(e.ID):
			reject(RejectBadID)
			continue
		case product[e.ID]:
			reject(RejectProductIDCollision)
			continue
		case seen[e.ID]:
			reject(RejectDuplicateID)
			continue
		}
		seen[e.ID] = true
		if !knownKinds[Kind(e.Kind)] {
			reject(RejectUnknownKind)
			continue
		}
		if len(e.Paths) == 0 {
			reject(RejectEmptyPaths)
			continue
		}
		badPath := false
		for _, p := range e.Paths {
			if p == "" || !doublestar.ValidatePattern(p) {
				badPath = true
			}
		}
		if badPath {
			reject(RejectBadPath)
			continue
		}
		sev := Severity(e.Severity)
		if sev == "" {
			sev = SeverityHigh
		}
		if _, ok := severityRank[sev]; !ok {
			reject(RejectBadSeverity)
			continue
		}
		accepted = append(accepted, Surface{
			ID: e.ID, Kind: Kind(e.Kind), Paths: append([]string(nil), e.Paths...),
			Severity: sev, Description: e.Description,
		})
	}
	return accepted, rejected, nil
}

// MergeSurfaces returns product followed by every accepted extension surface
// whose id does not collide with one already in the list (ParseRepoSurfaces
// already rejects collisions; this is the belt to its braces).
func MergeSurfaces(product, accepted []Surface) []Surface {
	out := append([]Surface(nil), product...)
	ids := map[string]bool{}
	for _, s := range out {
		ids[s.ID] = true
	}
	for _, s := range accepted {
		if ids[s.ID] {
			continue
		}
		ids[s.ID] = true
		out = append(out, s)
	}
	return out
}

// CheckName is the check-key namespace of every permission-drift concern.
const CheckName = "permission_drift"

// escapeKeyField makes a check-key field '|'-free: '%' -> "%25" first, then
// '|' -> "%7C", so the encoding is injective and the field separator never
// appears inside a field.
func escapeKeyField(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "%", "%25"), "|", "%7C")
}

// CheckKey is the de-duplication key the server stores on a widening's
// concern row: `permission_drift|<surface>|<path>|<key>|<after>`, every
// field escaped (escapeKeyField) so the key is injective. The after value is
// part of the key so a LATER move of the same key to a different value
// raises again, while a repeat of the same widening does not.
func CheckKey(surfaceID, path, key, after string) string {
	return strings.Join([]string{CheckName, escapeKeyField(surfaceID), escapeKeyField(path),
		escapeKeyField(key), escapeKeyField(after)}, "|")
}

// UnevaluableKey is the de-duplication key of an unevaluable concern:
// `permission_drift|<surface>|<path>|unevaluable` — four fields, so it never
// equals a five-field CheckKey. The reason is deliberately not part of it:
// one surface file that cannot be evaluated is one concern, whichever way it
// failed.
func UnevaluableKey(surfaceID, path string) string {
	return strings.Join([]string{CheckName, escapeKeyField(surfaceID), escapeKeyField(path), "unevaluable"}, "|")
}

// noteHumanOnly closes every permission-drift note.
const noteHumanOnly = "This concern was raised by the server's deterministic permission-drift check, not by a model reviewer. " +
	"Only a human can waive it, with a reason; agents and delegated actions cannot waive or defer it."

// Note renders a widening's concern note: the surface, the path, the key and
// its before/after values.
func Note(s Surface, path string, c Change) string {
	return fmt.Sprintf("Permission-drift check: this change widens the %s surface (%s) in %s: %s moved from %s to %s. ",
		s.ID, s.Description, path, c.Key, c.Before, c.After) +
		"Revert the widening, or have a human confirm it is intended. " + noteHumanOnly
}

// unevaluableWhy explains each reason class.
var unevaluableWhy = map[string]string{
	ReasonParseError:        "the file does not parse at base or head",
	ReasonShapeUnrecognized: "the file no longer has the shape the check's extractor recognizes, so its grants cannot be compared",
	ReasonFetchFailed:       "the file could not be read from the forge",
	ReasonCompareFailed:     "the forge compare listing the changed files failed",
	ReasonCompareTruncated:  "the forge compare listing the changed files was truncated",
}

// NoteUnevaluable renders an unevaluable concern's note. It names the reason
// CLASS only — never file bytes — and says the check failed closed.
func NoteUnevaluable(s Surface, path, reason string) string {
	why, ok := unevaluableWhy[reason]
	if !ok {
		why = "the check could not evaluate it (" + reason + ")"
	}
	where := s.ID
	if path != "" {
		where += " in " + path
	}
	return "Permission-drift check: the " + where + " surface could not be evaluated: " + why +
		". The check fails closed, so a human must confirm this change does not widen a permission. " + noteHumanOnly
}
