// Package github implements the work-management Provider (#1005) against
// GitHub Projects: it creates the issue (labels applied at creation),
// adds it to the configured project board, sets the single-select Status
// field, and links the parent epic as a sub-issue. The GraphQL/REST calls
// live in backend/internal/githubclient (projects.go); this package is
// the orchestration that turns a resolved workmgmt.ProviderRequest into
// those calls.
package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ProviderName is the conventions `provider` id this provider registers
// under and echoes into CreatedItem.Provider.
const ProviderName = "github_projects"

// statusFieldName is the conventional single-select board field the
// provider sets from BoardPlacement.Status.
const statusFieldName = "Status"

// API is the slice of githubclient.Client the provider needs, declared as
// a consumer-side interface so the provider can be unit-tested against a
// fake. *githubclient.Client satisfies it.
type API interface {
	CreateIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error)
	IssueNodeID(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (string, error)
	ProjectFields(ctx context.Context, installationID int64, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error)
	ProjectItemStatus(ctx context.Context, installationID int64, issueNodeID, projectID, fieldName string) (*githubclient.ProjectItemStatus, error)
	AddProjectItem(ctx context.Context, installationID int64, projectID, contentID string) (string, error)
	SetProjectItemSingleSelect(ctx context.Context, installationID int64, projectID, itemID, fieldID, optionID string) error
	AddSubIssue(ctx context.Context, installationID int64, parentNodeID, childNodeID string) error
	ListSubIssues(ctx context.Context, installationID int64, parentNodeID string) ([]githubclient.SubIssue, error)
	SearchIssuesByTitle(ctx context.Context, installationID int64, query string) ([]githubclient.IssueTitleResult, error)
	ProjectsTokenConfigured() bool
}

// Provider is the GitHub Projects work-management provider.
type Provider struct {
	api API
}

// Compile-time capability assertions: the GitHub provider implements the
// optional board-transition, number-discovery, and epic-children capability
// interfaces in addition to the base Provider.
var (
	_ workmgmt.Transitioner        = (*Provider)(nil)
	_ workmgmt.NumberDiscoverer    = (*Provider)(nil)
	_ workmgmt.EpicChildrenQuerier = (*Provider)(nil)
)

// New returns a Provider backed by api (in production *githubclient.Client).
func New(api API) *Provider { return &Provider{api: api} }

// Name implements workmgmt.Provider.
func (*Provider) Name() string { return ProviderName }

// File creates the issue and applies the conventions-resolved placement
// and relations. The issue is created first — it is the durable result and
// the only fatal step: a CreateIssue failure (or a failed pre-create
// guard) returns a nil item and an error, because no issue exists. Board
// placement and epic linking are best-effort (#1107): once the issue
// exists File always returns it with a nil error, recording whether the
// enrichment landed in CreatedItem.Boarded / EpicLinked and the cause in
// BoardingError / EpicLinkError when it did not. The server logs those
// causes and echoes them in the response so a real misconfiguration stays
// diagnosable while a placement failure no longer orphans a created issue.
func (p *Provider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	inst := req.Target.InstallationID
	// Fail closed when no installation id is available (#1005 concern-2).
	// On the run-absent filing path Target.InstallationID stays 0, so the
	// client cannot mint an installation token; proceeding would fail
	// opaquely deep inside the first REST call. GitHub Projects filing is
	// run-scoped in v0 — name the missing context and the constraint here
	// instead. A run-absent installation source is a follow-up.
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; GitHub Projects filing is run-scoped in v0 — file with a run_id whose run carries an installation, or use a provider that needs no installation token")
	}

	// GitHub has no native issue-to-issue depends_on relation, so a campaign
	// dependency edge is persisted as a parsed body marker line (ADR-047 /
	// #1437) — the only derivable mechanism, mirroring the existing
	// `Parent epic: #N` body convention. ensureDependsOnMarker is idempotent:
	// it appends the marker only when DependsOn is non-empty and no marker is
	// already present, so re-filing a body that already carries it is a no-op.
	body := ensureDependsOnMarker(req.Item.Body, req.Item.Relations.DependsOn)

	issue, err := p.api.CreateIssue(ctx, inst, repo, githubclient.CreateIssueParams{
		Title:  req.Item.Title,
		Body:   body,
		Labels: req.Item.Classification.Labels,
	})
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: create issue: %w", err)
	}

	created := &workmgmt.CreatedItem{
		Provider:      ProviderName,
		Number:        issue.Number,
		URL:           issue.HTMLURL,
		AppliedLabels: req.Item.Classification.Labels,
		Status:        req.Item.BoardPlacement.Status,
		BoardColumn:   req.Item.BoardPlacement.BoardColumn,
	}

	// Board placement is best-effort (#1107): the issue is the durable
	// result, so a placement failure records the cause and leaves Boarded
	// false rather than discarding the created issue. No project configured
	// means nothing to board — leave Boarded false with no error.
	if req.Target.Project == nil {
		created.Boarded = false
	} else if err := p.placeOnBoard(ctx, inst, req, issue); err != nil {
		created.BoardingError = err.Error()
	} else {
		created.Boarded = true
	}

	// Epic linking is best-effort too; an empty parent epic means nothing
	// to link (leave EpicLinked false with no error).
	if epic := strings.TrimSpace(req.Item.Relations.ParentEpic); epic != "" {
		if err := p.linkEpic(ctx, inst, repo, epic, issue.NodeID); err != nil {
			created.EpicLinkError = err.Error()
		} else {
			created.EpicLinked = true
		}
	}

	return created, nil
}

// Transition moves an already-filed issue's board Status along a
// run-lifecycle edge (#1012). It resolves the issue node id, the project's
// Status field + options, and the issue's current project item, then:
//   - SKIPS (no mutation) when no project is configured, the issue is not on
//     the board, the target canonical state has no configured/board option,
//     or — the never-fight-the-human guard — the card's current status is
//     not in the request's expected source set. An unset status counts as
//     Backlog so a fresh card still advances on run_started.
//   - otherwise sets the Status single-select to the target option and
//     reports Moved with from->to.
//
// Genuine provider failures (issue/field resolution, the status read, the
// set mutation) return an error; the lifecycle hook logs it best-effort and
// never unwinds the run. Only the Status column is touched — never labels,
// fields, or epic links (the #1005 scope split).
func (p *Provider) Transition(ctx context.Context, req workmgmt.TransitionRequest) (*workmgmt.TransitionResult, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	proj := req.Target.Project
	if proj == nil {
		return &workmgmt.TransitionResult{Skipped: true, SkipReason: "no project configured"}, nil
	}
	if req.IssueNumber <= 0 {
		return nil, errors.New("workmgmt/github: transition requires a positive issue number")
	}
	inst := req.Target.InstallationID
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; board transitions are run-scoped in v0")
	}

	// Resolve the target board option from the canonical state via the
	// conventions' states map. An unmapped canonical state is a no-op skip,
	// not an error — the config simply doesn't bind that state to a column.
	toOption := strings.TrimSpace(req.States[req.CanonicalState])
	if toOption == "" {
		return &workmgmt.TransitionResult{Skipped: true,
			SkipReason: fmt.Sprintf("canonical state %q has no configured provider option", req.CanonicalState)}, nil
	}

	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	// User-owned Projects v2 (the Project #7 case) cannot be reached with the
	// App installation token (#1114). With no projects token configured the
	// installation-token fallback would error on every board GraphQL call, and
	// that error would drop the mandated work_item_transitioned audit — so
	// degrade to a best-effort SKIP (the #1107/#1114 posture: never an error)
	// before dispatching anything. With a projects token configured, opt the
	// board GraphQL calls into it.
	if proj.OwnerType == "user" {
		if !p.api.ProjectsTokenConfigured() {
			return &workmgmt.TransitionResult{Skipped: true, To: toOption,
				SkipReason: "user-owned project board unreachable: no projects token configured"}, nil
		}
		ctx = githubclient.WithProjectsToken(ctx)
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}

	issueNodeID, err := p.api.IssueNodeID(ctx, inst, repo, req.IssueNumber)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", req.IssueNumber, err)
	}
	meta, err := p.api.ProjectFields(ctx, inst, coord, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	optionID, ok := meta.StatusOptions[toOption]
	if !ok {
		return &workmgmt.TransitionResult{Skipped: true,
			SkipReason: fmt.Sprintf("target status %q is not a %s option on the project", toOption, statusFieldName)}, nil
	}

	item, err := p.api.ProjectItemStatus(ctx, inst, issueNodeID, meta.ProjectID, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read project item status: %w", err)
	}
	if !item.OnBoard {
		return &workmgmt.TransitionResult{Skipped: true, To: toOption,
			SkipReason: "issue is not on the project board"}, nil
	}
	current := item.Status
	// never-fight-the-human: only advance from an expected source status. A
	// card a human parked elsewhere (e.g. Blocked) is left untouched.
	if !sourceAllows(current, req) {
		return &workmgmt.TransitionResult{Skipped: true, From: current, To: toOption,
			SkipReason: fmt.Sprintf("current status %q is not in the expected source set", labelOrUnset(current))}, nil
	}
	if current == toOption {
		return &workmgmt.TransitionResult{Skipped: true, From: current, To: toOption,
			SkipReason: "card already at target status"}, nil
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, inst, meta.ProjectID, item.ItemID, meta.FieldID, optionID); err != nil {
		return nil, fmt.Errorf("workmgmt/github: set status field: %w", err)
	}
	return &workmgmt.TransitionResult{Moved: true, From: current, To: toOption}, nil
}

// sourceAllows reports whether the card's current board status is an
// expected source for the move. The expected source canonical states are
// resolved to board options through the request's states map; an unset
// current status (a fresh/un-triaged card) counts as Backlog so it still
// advances when backlog is an expected source (run_started's unset/Backlog).
func sourceAllows(current string, req workmgmt.TransitionRequest) bool {
	for _, s := range req.ExpectedSourceStates {
		if current == "" && s == workmgmt.CanonicalStateBacklog {
			return true
		}
		if opt := strings.TrimSpace(req.States[s]); opt != "" && current == opt {
			return true
		}
	}
	return false
}

// labelOrUnset renders an empty status as "(unset)" for skip-reason text.
func labelOrUnset(status string) string {
	if status == "" {
		return "(unset)"
	}
	return status
}

// DiscoverNumbers enumerates the sequential numbers already in use for a
// numbered type (#1269) by searching issue TITLES — open AND closed, since
// decided ADRs are closed — and parsing the number out of each matched title.
// It is the optional workmgmt.NumberDiscoverer capability the filing handler
// calls before Apply when a numbered filing omits existing_numbers.
//
// It validates the target repo + installation (fail closed with the same
// actionable style File uses), then composes the search query by branch on
// req.DefaultLabels. When the type carries a default label, it queries by
// `label:"<first default label>"` ALONE — omitting the in:title term entirely
// (#1522/#1523): against the live search API `in:title "[E"` matches nothing
// (`[` is not indexed, `E` is too short for the fuzzy term), and AND-ed with
// `label:epic` it collapses an otherwise-complete label result to zero, so
// discovery mis-picks a colliding low number. A type WITHOUT a default label
// keeps the selective in:title-only query derived from req.TitleFormat (the
// substring before {number}, e.g. "[ADR-"). Either branch composes a query
// with NO is:open qualifier and re-parses every returned title with a regex
// built from req.TitleFormat —
// GitHub's in:title search is fuzzy, so a search hit is not proof the title
// carries the [PREFIX-N] token. Non-matching/malformed titles are skipped.
// Returns the collected numbers (possibly empty, no error). It never invents
// a number: allocation stays in Apply, which seeds the +1 (or the seed-zero
// first item) from this result.
func (p *Provider) DiscoverNumbers(ctx context.Context, req workmgmt.DiscoverNumbersRequest) ([]int, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	inst := req.Target.InstallationID
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; number discovery is run-scoped in v0 — file with a run_id whose run carries an installation, or pass existing_numbers explicitly")
	}
	re, err := titleNumberRegexp(req.TitleFormat)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: build title number regexp: %w", err)
	}

	repoQ := req.Target.Repo.Owner + "/" + req.Target.Repo.Name
	// Compose the discovery query by branch on the type's default label
	// (#1522/#1523). When the type carries a default label, query by
	// `label:"<label>"` ALONE — dropping the in:title term entirely. #1523
	// added the label qualifier but kept the co-present in:title term, so the
	// composed query was `repo:X label:"epic" in:title "[E"`; against the live
	// search API the `in:title "[E"` term matches nothing (`[` is not indexed,
	// `E` is too short for the fuzzy term) and, AND-ed with `label:epic`, it
	// collapses the otherwise-complete label result to zero — the #1522 root
	// cause #1523 left in place, so the fail-closed allocate mis-picks a
	// colliding low number. `label:"epic"` returns exactly the epics (children
	// carry type:feature/etc., never epic) — a small, complete set no recency
	// window truncates — while the anchored titleNumberRegexp re-parse stays the
	// sole value extractor. A type WITHOUT a default label keeps the selective
	// in:title-only query (its literal prefix, e.g. `[ADR-`, still narrows the
	// fuzzy search).
	var query string
	if len(req.DefaultLabels) > 0 {
		query = fmt.Sprintf(`repo:%s label:%s`, repoQ, labelSearchQualifier(req.DefaultLabels[0]))
	} else {
		query = fmt.Sprintf(`repo:%s in:title "%s"`, repoQ, titleNumberSearchPrefix(req.TitleFormat))
	}
	hits, err := p.api.SearchIssuesByTitle(ctx, inst, query)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: search issues by title: %w", err)
	}

	numbers := make([]int, 0, len(hits))
	for _, h := range hits {
		m := re.FindStringSubmatch(h.Title)
		if m == nil {
			continue
		}
		// strconv.Atoi parses leading zeros (pad:3 titles like 041) cleanly.
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			continue
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

// titlePlaceholderRE matches a `{name}` placeholder in a title_format, so the
// number-discovery helpers can split the literal segments from the {number}
// (and any other) placeholder.
var titlePlaceholderRE = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

// titleNumberSearchPrefix returns the literal title segment before {number}
// (e.g. "[ADR-" for "[ADR-{number}] {summary}") — the in:title search term
// that narrows the fuzzy search to candidate numbered titles. An empty result
// (no {number} or it leads the format) yields "" and the search degrades to
// repo-wide title matching, which the regex re-parse still filters.
func titleNumberSearchPrefix(format string) string {
	idx := strings.Index(format, "{number}")
	if idx <= 0 {
		return ""
	}
	// Drop characters that could break out of the quoted in:title qualifier in
	// the composed search query: a double quote ends the quoted term and a
	// backslash could escape the closing quote. A legitimate title prefix
	// carries none of these, and the regex re-parse still validates exact
	// matches, so stripping them only tightens the fuzzy search term.
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' {
			return -1
		}
		return r
	}, format[:idx])
}

// labelSearchQualifier renders a label as the quoted value of a `label:`
// search qualifier (e.g. `epic` -> `"epic"`). It strips the characters that
// could break out of the quoted qualifier (double quote, backslash) — the same
// hardening titleNumberSearchPrefix applies to the in:title term — and always
// encloses the value in double quotes so a label carrying a colon or space
// (e.g. `type:feature`) stays a single qualifier rather than splitting into a
// second search term.
func labelSearchQualifier(label string) string {
	stripped := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' {
			return -1
		}
		return r
	}, label)
	return `"` + stripped + `"`
}

// titleNumberRegexp builds an anchored regexp from a numbered type's
// title_format that captures the integer substituted for {number}. The literal
// segments are QuoteMeta-escaped, {number} becomes a (\d+) capture group, and
// any other {placeholder} (e.g. {summary}) becomes .*? so the whole title
// shape is matched. It anchors at ^ so a stray leading token cannot smuggle a
// false number. An error is returned only if the assembled pattern fails to
// compile (it should not for any well-formed format).
func titleNumberRegexp(format string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range titlePlaceholderRE.FindAllStringSubmatchIndex(format, -1) {
		// Literal text before this placeholder.
		b.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		name := format[loc[2]:loc[3]]
		if name == "number" {
			b.WriteString(`(\d+)`)
		} else {
			b.WriteString(`.*?`)
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(format[last:]))
	return regexp.Compile(b.String())
}

// placeOnBoard adds the created issue to the configured project and sets
// its Status field. No-op when the conventions declare no project.
func (p *Provider) placeOnBoard(ctx context.Context, inst int64, req workmgmt.ProviderRequest, issue *githubclient.CreatedIssue) error {
	proj := req.Target.Project
	if proj == nil {
		return nil
	}
	coord := githubclient.ProjectCoord{Owner: proj.Owner, OwnerType: proj.OwnerType, Number: proj.Number}
	// User-owned Projects v2 boards (the Project #7 case) cannot be written
	// with the App installation token — there is no user-projects permission
	// for GitHub Apps (#1114). Opt the three board-placement GraphQL calls
	// into the static projects token via the request-scoped flag; the client
	// honors it only when a projects token is configured, so this stays the
	// #1107 best-effort boarded:false path when it is not. Org-owned projects
	// and the repo-scoped epic link (AddSubIssue) stay on the installation
	// token.
	if proj.OwnerType == "user" {
		ctx = githubclient.WithProjectsToken(ctx)
	}
	meta, err := p.api.ProjectFields(ctx, inst, coord, statusFieldName)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	itemID, err := p.api.AddProjectItem(ctx, inst, meta.ProjectID, issue.NodeID)
	if err != nil {
		return fmt.Errorf("workmgmt/github: add project item: %w", err)
	}
	status := strings.TrimSpace(req.Item.BoardPlacement.Status)
	if status == "" {
		return nil
	}
	optionID, ok := meta.StatusOptions[status]
	if !ok {
		return fmt.Errorf("workmgmt/github: status %q is not a %s option on the project; available: %s",
			status, statusFieldName, strings.Join(sortedKeys(meta.StatusOptions), ", "))
	}
	if err := p.api.SetProjectItemSingleSelect(ctx, inst, meta.ProjectID, itemID, meta.FieldID, optionID); err != nil {
		return fmt.Errorf("workmgmt/github: set status field: %w", err)
	}
	return nil
}

// linkEpic resolves the parent-epic reference (#N or N) to its node id
// and links the new issue as its sub-issue.
func (p *Provider) linkEpic(ctx context.Context, inst int64, repo githubclient.RepoRef, epicRef, childNodeID string) error {
	number, err := parseIssueRef(epicRef)
	if err != nil {
		return fmt.Errorf("workmgmt/github: parent epic %q: %w", epicRef, err)
	}
	parentNodeID, err := p.api.IssueNodeID(ctx, inst, repo, number)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve parent epic #%d: %w", number, err)
	}
	if err := p.api.AddSubIssue(ctx, inst, parentNodeID, childNodeID); err != nil {
		return fmt.Errorf("workmgmt/github: link parent epic #%d: %w", number, err)
	}
	return nil
}

// EpicChildren lists an epic's child issues and returns the depends_on edges
// among them (ADR-047 / #1437, the campaign DAG source). It resolves the epic
// reference to a node id, reads the sub-issues connection, parses each child
// body for the depends_on marker, and builds a DependsEdge for every
// referenced number that is itself in the children set. A reference to a
// non-child is kept OUT of Edges (the campaign wave DAG, plan.Waves, is over
// the epic's own children) but surfaced in DroppedEdges rather than silently
// discarded, so campaign assembly can fail closed on a dangling/mis-targeted
// dependency. Children are returned ascending by number and both edge slices
// are deterministically sorted (by From, then To) so the result is stable.
//
// It validates the target repo + installation (fail closed with File's
// actionable style). It is the optional workmgmt.EpicChildrenQuerier
// capability E25.3 calls during campaign assembly.
func (p *Provider) EpicChildren(ctx context.Context, req workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	inst := req.Target.InstallationID
	if inst == 0 {
		return nil, errors.New("workmgmt/github: no installation id available; epic-children query is run-scoped in v0 — file with a run_id whose run carries an installation")
	}
	number, err := parseIssueRef(req.Epic)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: epic %q: %w", req.Epic, err)
	}
	repo := githubclient.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	epicNodeID, err := p.api.IssueNodeID(ctx, inst, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve epic #%d: %w", number, err)
	}
	subs, err := p.api.ListSubIssues(ctx, inst, epicNodeID)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: list epic #%d children: %w", number, err)
	}

	// The sibling set: a depends_on reference is an edge only when it points
	// at another child of this epic.
	isChild := make(map[int]bool, len(subs))
	for _, s := range subs {
		isChild[s.Number] = true
	}

	children := make([]workmgmt.EpicChild, 0, len(subs))
	var edges, dropped []workmgmt.DependsEdge
	for _, s := range subs {
		children = append(children, workmgmt.EpicChild{Number: s.Number, Title: s.Title, Autonomy: parseAutonomyLabel(s.Labels)})
		for _, dep := range parseDependsOnMarker(s.Body) {
			if isChild[dep] {
				edges = append(edges, workmgmt.DependsEdge{From: s.Number, To: dep})
			} else {
				// A depends_on reference to a non-child: a typo'd number or a
				// real cross-epic dependency. Surface it as a dropped edge
				// (campaign assembly fails closed on it) rather than silently
				// discarding it.
				dropped = append(dropped, workmgmt.DependsEdge{From: s.Number, To: dep})
			}
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Number < children[j].Number })
	sortEdges := func(es []workmgmt.DependsEdge) {
		sort.Slice(es, func(i, j int) bool {
			if es[i].From != es[j].From {
				return es[i].From < es[j].From
			}
			return es[i].To < es[j].To
		})
	}
	sortEdges(edges)
	sortEdges(dropped)
	return &workmgmt.EpicChildrenResult{Children: children, Edges: edges, DroppedEdges: dropped}, nil
}

// dependsOnMarkerRE matches the depends_on body marker line and captures the
// comma-separated reference list. It is the single source of truth for the
// marker shape paired with renderDependsOnMarker, so a write and a read can
// never drift. The `(?im)` flags make it case-insensitive and line-anchored
// so the marker is found wherever it sits in the body.
var dependsOnMarkerRE = regexp.MustCompile(`(?im)^Depends on:\s*(.+)$`)

// dependsOnRefRE extracts a positive integer issue number from one
// comma-separated marker token (`#12` or `12`), tolerating surrounding
// whitespace. Tokens that are not a positive-integer reference are skipped.
var dependsOnRefRE = regexp.MustCompile(`^\s*#?([1-9]\d*)\s*$`)

// renderDependsOnMarker renders the depends_on body marker line for refs as
// `Depends on: #X, #Y`. Each ref is normalized to `#N` (a bare `N` gains the
// `#`). It is the single source of truth for the marker format, paired with
// parseDependsOnMarker so write and read cannot drift. Returns "" when refs is
// empty, so an item with no depends_on carries no marker.
func renderDependsOnMarker(refs []string) string {
	var parts []string
	for _, r := range refs {
		s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r), "#"))
		if s == "" {
			continue
		}
		parts = append(parts, "#"+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Depends on: " + strings.Join(parts, ", ")
}

// ensureDependsOnMarker appends the depends_on marker line to body when refs
// is non-empty and body does not already carry a marker. Idempotent: a body
// that already has a `Depends on:` line is returned unchanged, so re-filing
// never double-stamps the marker.
func ensureDependsOnMarker(body string, refs []string) string {
	marker := renderDependsOnMarker(refs)
	if marker == "" {
		return body
	}
	if dependsOnMarkerRE.MatchString(body) {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return marker
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker
}

// parseDependsOnMarker parses the depends_on body marker line into the
// referenced issue numbers. It reads the FIRST `Depends on:` line, splits the
// captured list on commas, and parses each token as a positive-integer issue
// reference; non-matching tokens are skipped. Returns nil when no marker is
// present. Paired with renderDependsOnMarker as the single source of truth for
// the marker round trip.
func parseDependsOnMarker(body string) []int {
	m := dependsOnMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var nums []int
	for _, tok := range strings.Split(m[1], ",") {
		rm := dependsOnRefRE.FindStringSubmatch(tok)
		if rm == nil {
			continue
		}
		n, err := strconv.Atoi(rm[1])
		if err != nil {
			continue
		}
		nums = append(nums, n)
	}
	return nums
}

// autonomyLabelPrefix is the label namespace the campaign autonomy tier is
// read from. It mirrors the conventions' `autonomy:` namespace (LabelDefaults
// keys on "autonomy"; the default value is "autonomy:medium") so the tier a
// filing stamps at creation time is the tier the campaign source reads back.
const autonomyLabelPrefix = "autonomy:"

// parseAutonomyLabel extracts the autonomy tier from a child issue's labels:
// the suffix of the first `autonomy:<tier>` label (e.g. "autonomy:low" ->
// "low"). It returns "" when no autonomy label is present (unknown/default,
// treated downstream as non-human-led) so the whole namespace lives in one
// helper — a future tier is one edit. Only the recognized tiers ("", "low",
// "medium", "high") pass through; an unrecognized tier (a typo like
// "autonomy:critical") normalizes to "" = non-human-led. This keeps the parse
// boundary in lockstep with the fail-closed campaign_items.autonomy CHECK
// (migration 0049): a mislabeled child degrades to the autonomous default
// rather than aborting the entire epic campaign with a CHECK violation when
// Persist writes the row.
func parseAutonomyLabel(labels []string) string {
	for _, l := range labels {
		if tier := strings.TrimPrefix(l, autonomyLabelPrefix); tier != l {
			switch tier {
			case "low", "medium", "high":
				return tier
			default:
				return ""
			}
		}
	}
	return ""
}

// parseIssueRef parses "#123" or "123" into the issue number.
func parseIssueRef(ref string) (int, error) {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a numeric issue reference")
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue number must be > 0")
	}
	return n, nil
}

// sortedKeys returns the sorted keys of a string-keyed map, for stable
// error messages.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
