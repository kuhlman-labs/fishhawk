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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
	CreateIssue(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, p githubclient.CreateIssueParams) (*githubclient.CreatedIssue, error)
	IssueNodeID(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) (string, error)
	ProjectFields(ctx context.Context, scope forge.CredentialScope, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error)
	ProjectItemStatus(ctx context.Context, scope forge.CredentialScope, issueNodeID, projectID, fieldName string) (*githubclient.ProjectItemStatus, error)
	AddProjectItem(ctx context.Context, scope forge.CredentialScope, projectID, contentID string) (string, error)
	SetProjectItemSingleSelect(ctx context.Context, scope forge.CredentialScope, projectID, itemID, fieldID, optionID string) error
	AddSubIssue(ctx context.Context, scope forge.CredentialScope, parentNodeID, childNodeID string) error
	ListSubIssues(ctx context.Context, scope forge.CredentialScope, parentNodeID string) ([]githubclient.SubIssue, error)
	SearchIssuesByTitle(ctx context.Context, scope forge.CredentialScope, query string) ([]githubclient.IssueTitleResult, error)
	// GetIssue fetches a single issue's body/labels/state — the per-issue
	// resolution the no-epic campaign source (ResolveDependencies) reads each
	// named issue's depends_on marker, autonomy label, and completion state
	// from. Signature matches *githubclient.Client.GetIssue, so the production
	// client satisfies it directly.
	GetIssue(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) (*githubclient.Issue, error)
	// ListRepoIssues enumerates the repository's issues via the paginated
	// GraphQL repository.issues connection — the enumeration primitive the
	// optional workmgmt.WorkItemReader capability lists through (#2230), never a
	// ProjectV2 board item list (which caps and truncates silently). Signature
	// matches *githubclient.Client.ListRepoIssues, so the production client
	// satisfies it directly.
	//
	// The read capability also calls GetIssue (single-item read), IssueNodeID +
	// ProjectItemStatus (per-item board state) and ProjectFields (project node
	// id) — all already members above, which is why this is the only addition.
	ListRepoIssues(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, opts githubclient.ListRepoIssuesOptions) ([]githubclient.RepoIssue, error)
	// UpdateIssue edits an existing issue's body, label set, state and
	// state_reason — the single REST write primitive the optional
	// workmgmt.GroomingMutator capability dispatches its label, depends-on and
	// close mutations through (E54.5 / #2237). Its params are POINTER-typed so
	// an omitted key never reaches the wire and an unrelated body update cannot
	// strip the issue's labels. Signature matches *githubclient.Client.UpdateIssue,
	// so the production client satisfies it directly.
	//
	// The mutate capability's other paths reuse members already above:
	// GetIssue (read-before-union / read-before-marker), IssueNodeID +
	// AddSubIssue (epic link), and ProjectFields + AddProjectItem +
	// ProjectItemStatus + SetProjectItemSingleSelect (board moves and field
	// writes) — which is why this is the only addition.
	UpdateIssue(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int, p githubclient.UpdateIssueParams) (*githubclient.Issue, error)
	//
	// The grooming label mutation needs one MORE primitive than this
	// interface declares — the additive AddIssueLabels write — but it is
	// declared as the OPTIONAL labelAdder extension in grooming.go rather
	// than promoted in here, so the many API implementations that only
	// exercise the filing and board-sync paths are not forced to carry a
	// stub for a capability they never reach. *githubclient.Client
	// satisfies both, so production dispatch is unaffected.
	ProjectsTokenConfigured() bool
}

// Provider is the GitHub Projects work-management provider.
type Provider struct {
	api API
}

// Compile-time capability assertions: the GitHub provider implements the
// optional board-transition, number-discovery, epic-children, issue-set
// dependency and work-item read capability interfaces in addition to the base
// Provider.
//
// WorkItemReader (#2230 / ADR-064) reserves the board-as-input seam: it is
// implemented here (reader.go) but has NO production consumer — no HTTP route,
// MCP tool, CLI verb, env var or config field resolves a reader — and the
// ratified invariant that no board-read path is exposed through the MCP agent
// tool surface is enforced by
// backend/internal/mcpserver/board_read_guard_test.go.
//
// GroomingMutator (E54.5 / #2237) reserves the board-WRITE seam on the same
// terms: implemented here (grooming.go), resolved by nothing in production,
// and covered by the same MCP-surface guard, extended to the write symbols.
var (
	_ workmgmt.Transitioner               = (*Provider)(nil)
	_ workmgmt.NumberDiscoverer           = (*Provider)(nil)
	_ workmgmt.EpicChildrenQuerier        = (*Provider)(nil)
	_ workmgmt.IssueSetDependencyResolver = (*Provider)(nil)
	_ workmgmt.WorkItemReader             = (*Provider)(nil)
	_ workmgmt.GroomingMutator            = (*Provider)(nil)
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
	repo := forge.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	scope := req.Target.Scope
	// Fail closed when no installation scope is available (#1005 concern-2).
	// On the run-absent filing path Target.Scope stays the zero scope, so the
	// client cannot mint an installation token; proceeding would fail
	// opaquely deep inside the first REST call. GitHub Projects filing is
	// run-scoped in v0 — name the missing context and the constraint here
	// instead. A run-absent installation source is a follow-up.
	if scope.IsZero() {
		return nil, errors.New("workmgmt/github: no installation id available; GitHub Projects filing is run-scoped in v0 — file with a run_id whose run carries an installation, or use a provider that needs no installation token")
	}

	// GitHub has no native issue-to-issue depends_on relation, so a campaign
	// dependency edge is persisted as a parsed body marker line (ADR-047 /
	// #1437) — the only derivable mechanism, mirroring the existing
	// `Parent epic: #N` body convention. ensureDependsOnMarker is idempotent:
	// it appends the marker only when DependsOn is non-empty and no marker is
	// already present, so re-filing a body that already carries it is a no-op.
	body := ensureDependsOnMarker(req.Item.Body, req.Item.Relations.DependsOn)

	issue, err := p.api.CreateIssue(ctx, scope, repo, githubclient.CreateIssueParams{
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
	} else if err := p.placeOnBoard(ctx, scope, req, issue); err != nil {
		created.BoardingError = err.Error()
	} else {
		created.Boarded = true
	}

	// Epic linking is best-effort too; an empty parent epic means nothing
	// to link (leave EpicLinked false with no error).
	if epic := strings.TrimSpace(req.Item.Relations.ParentEpic); epic != "" {
		if err := p.linkEpic(ctx, scope, repo, epic, issue.NodeID); err != nil {
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
// Two of those skips — no project configured, and the issue not being on
// the board — additionally set NotApplicable (#2494): there is no card to
// act on, so the caller records no work_item_transitioned entry. The
// remaining skips (unmapped canonical state, target status absent from the
// board, unreachable user-owned board, never-fight-the-human source
// mismatch, already-at-target) are DECISIONS about a real work item and
// leave NotApplicable false, so they keep auditing.
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
		// NotApplicable (#2494): with no project configured there is no card
		// anywhere to act on, so the caller suppresses the work_item_transitioned
		// audit rather than recording a transition that touched nothing.
		return &workmgmt.TransitionResult{Skipped: true, NotApplicable: true,
			SkipReason: "no project configured"}, nil
	}
	if req.IssueNumber <= 0 {
		return nil, errors.New("workmgmt/github: transition requires a positive issue number")
	}
	scope := req.Target.Scope
	if scope.IsZero() {
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
	repo := forge.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}

	issueNodeID, err := p.api.IssueNodeID(ctx, scope, repo, req.IssueNumber)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve issue #%d: %w", req.IssueNumber, err)
	}
	meta, err := p.api.ProjectFields(ctx, scope, coord, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	optionID, ok := meta.StatusOptions[toOption]
	if !ok {
		return &workmgmt.TransitionResult{Skipped: true,
			SkipReason: fmt.Sprintf("target status %q is not a %s option on the project", toOption, statusFieldName)}, nil
	}

	item, err := p.api.ProjectItemStatus(ctx, scope, issueNodeID, meta.ProjectID, statusFieldName)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: read project item status: %w", err)
	}
	if !item.OnBoard {
		// NotApplicable (#2494): the issue carries no card, so there is no
		// work item this transition could have moved — distinct from the
		// DECISION skips below, which are real outcomes about a card that
		// exists.
		return &workmgmt.TransitionResult{Skipped: true, NotApplicable: true, To: toOption,
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
	if err := p.api.SetProjectItemSingleSelect(ctx, scope, meta.ProjectID, item.ItemID, meta.FieldID, optionID); err != nil {
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
	scope := req.Target.Scope
	if scope.IsZero() {
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
	hits, err := p.api.SearchIssuesByTitle(ctx, scope, query)
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

// boardAPI is the board-placement slice of the GitHub calls, split out so
// BOTH the work-item provider (API) and the product-feedback provider
// (FeedbackAPI) can drive the same placement routine (#1737) without one
// depending on the other's full interface.
type boardAPI interface {
	ProjectFields(ctx context.Context, scope forge.CredentialScope, coord githubclient.ProjectCoord, fieldName string) (*githubclient.ProjectMeta, error)
	AddProjectItem(ctx context.Context, scope forge.CredentialScope, projectID, contentID string) (string, error)
	SetProjectItemSingleSelect(ctx context.Context, scope forge.CredentialScope, projectID, itemID, fieldID, optionID string) error
}

// placeOnBoard adds the created issue to the configured project and sets
// its Status field. No-op when the conventions declare no project.
func (p *Provider) placeOnBoard(ctx context.Context, scope forge.CredentialScope, req workmgmt.ProviderRequest, issue *githubclient.CreatedIssue) error {
	return placeIssueOnBoard(ctx, p.api, scope, req.Target.Project, req.Item.BoardPlacement.Status, issue)
}

// placeIssueOnBoard is the shared placement routine: add the issue to the
// project and set its single-select Status. A nil project is a no-op (the
// caller reports that as "not attempted", not as a failure).
func placeIssueOnBoard(ctx context.Context, api boardAPI, scope forge.CredentialScope, proj *workmgmt.Project, desiredStatus string, issue *githubclient.CreatedIssue) error {
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
	meta, err := api.ProjectFields(ctx, scope, coord, statusFieldName)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve project fields: %w", err)
	}
	itemID, err := api.AddProjectItem(ctx, scope, meta.ProjectID, issue.NodeID)
	if err != nil {
		return fmt.Errorf("workmgmt/github: add project item: %w", err)
	}
	status := strings.TrimSpace(desiredStatus)
	if status == "" {
		return nil
	}
	optionID, ok := meta.StatusOptions[status]
	if !ok {
		return fmt.Errorf("workmgmt/github: status %q is not a %s option on the project; available: %s",
			status, statusFieldName, strings.Join(sortedKeys(meta.StatusOptions), ", "))
	}
	if err := api.SetProjectItemSingleSelect(ctx, scope, meta.ProjectID, itemID, meta.FieldID, optionID); err != nil {
		return fmt.Errorf("workmgmt/github: set status field: %w", err)
	}
	return nil
}

// linkEpic resolves the parent-epic reference (#N or N) to its node id
// and links the new issue as its sub-issue.
func (p *Provider) linkEpic(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, epicRef, childNodeID string) error {
	number, err := parseIssueRef(epicRef)
	if err != nil {
		return fmt.Errorf("workmgmt/github: parent epic %q: %w", epicRef, err)
	}
	parentNodeID, err := p.api.IssueNodeID(ctx, scope, repo, number)
	if err != nil {
		return fmt.Errorf("workmgmt/github: resolve parent epic #%d: %w", number, err)
	}
	if err := p.api.AddSubIssue(ctx, scope, parentNodeID, childNodeID); err != nil {
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
	scope := req.Target.Scope
	if scope.IsZero() {
		return nil, errors.New("workmgmt/github: no installation id available; epic-children query is run-scoped in v0 — file with a run_id whose run carries an installation")
	}
	number, err := parseIssueRef(req.Epic)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: epic %q: %w", req.Epic, err)
	}
	repo := forge.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}
	epicNodeID, err := p.api.IssueNodeID(ctx, scope, repo, number)
	if err != nil {
		return nil, fmt.Errorf("workmgmt/github: resolve epic #%d: %w", number, err)
	}
	subs, err := p.api.ListSubIssues(ctx, scope, epicNodeID)
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
	var satisfied []workmgmt.SatisfiedEdge
	// Memoize the out-of-set target state read across every edge in this one
	// query, so N depends_on references to the same closed target cost ONE
	// GetIssue (#2953).
	stateCache := map[int]targetState{}
	// Collapse duplicate edges so a repeated depends_on token
	// (`Depends on: #1639, #1639` — an untrusted body preserves every
	// occurrence) yields AT MOST ONE edge in whichever slice it classifies into
	// (#2953 condition 3). Keyed by (From, To, ToRefDigest): the digest is what
	// makes the key DISTINGUISH two DIFFERENT cross-repo tokens (both To=0) while
	// still collapsing the SAME canonical token repeated — a resolvable numeric
	// ref carries an empty digest, so its key reduces to (From, To) as before
	// (#2956).
	seenEdge := map[dependsEdgeKey]bool{}
	for _, s := range subs {
		children = append(children, workmgmt.EpicChild{
			Number:   s.Number,
			Title:    s.Title,
			Autonomy: parseAutonomyLabel(s.Labels),
			// Complete = closed-AND-completed (GitHub IssueState CLOSED with
			// IssueStateReason COMPLETED, uppercase GraphQL enums). A
			// not_planned / duplicate close is NOT complete — its work did not
			// land — so it does not satisfy a downstream dependency (#2120).
			Complete: s.State == "CLOSED" && s.StateReason == "COMPLETED",
			// Body and URL carry the forge's own values through verbatim so
			// the forge-state idempotency lookup (#2064, E50.7) can read the
			// hidden marker off a child's body and, on adoption, record the
			// SAME url the filed path would have recorded (issue.HTMLURL
			// above) instead of composing one — Client.BaseURL is configurable,
			// so a composed https://github.com/ url would be wrong on a GitHub
			// Enterprise Server host. Note EpicChildren deliberately applies NO
			// state filter, so a child CLOSED before a re-approval is still
			// returned and still adoptable.
			Body: s.Body,
			URL:  s.URL,
		})
		for _, dep := range parseDependsOnMarker(s.Body) {
			key := dependsEdgeKey{From: s.Number, To: dep.Number, Digest: dep.RawDigest}
			if seenEdge[key] {
				continue // duplicate depends_on token: classify once per identity.
			}
			seenEdge[key] = true
			if dep.Resolvable && isChild[dep.Number] {
				edges = append(edges, workmgmt.DependsEdge{From: s.Number, To: dep.Number})
				continue
			}
			// A depends_on reference that is NOT an in-set fellow child: a typo'd
			// number, a genuine cross-epic dependency, a cross-epic prerequisite
			// that is already done, or a cross-repo/unparseable ref. Classify it
			// (#2953): a closed-and-completed target is a satisfied dependency
			// (elided from the DAG), while an open/incomplete/unreadable one — and
			// any UNRESOLVABLE cross-repo ref — is dangling and fails closed. A
			// closed-and-completed FELLOW child stays an in-set Edge above and is
			// never classified here — condition 3's one-layer rule.
			cls, cerr := p.classifyOutOfSetTarget(ctx, scope, repo, dep, stateCache)
			if cerr != nil {
				return nil, cerr
			}
			if cls.satisfied {
				satisfied = append(satisfied, workmgmt.SatisfiedEdge{
					From: s.Number, To: dep.Number, State: cls.state, StateReason: cls.stateReason,
				})
			} else {
				dropped = append(dropped, workmgmt.DependsEdge{
					From: s.Number, To: dep.Number, Reason: cls.reason,
					// Carry the unresolvable token's identity onto the dropped edge so
					// the renderer names WHICH token (#2956); both empty on a numeric
					// dropped edge, keeping its shape unchanged.
					ToRef: dep.RawDisplay, ToRefDigest: dep.RawDigest,
				})
			}
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Number < children[j].Number })
	sortEdges(edges)
	sortEdges(dropped)
	sortSatisfiedEdges(satisfied)
	return &workmgmt.EpicChildrenResult{Children: children, Edges: edges, DroppedEdges: dropped, SatisfiedEdges: satisfied}, nil
}

// dependsEdgeKey is the per-item dedup key (#2956): (From, To, ToRefDigest). The
// digest component distinguishes two DISTINCT unresolvable cross-repo tokens
// (both To=0) so they no longer collapse into one identity-less edge, while a
// repeated IDENTICAL canonical token still collapses (its digest is equal) and a
// resolvable numeric ref (empty digest) keys exactly as the old (From, To) pair.
type dependsEdgeKey struct {
	From, To int
	Digest   string
}

// targetState is the memoized classification of one out-of-set depends_on
// target, cached per resolution call so repeated references to the same target
// cost a single GetIssue (#2953).
type targetState struct {
	satisfied   bool
	reason      workmgmt.DropReason
	state       string
	stateReason string
}

// classifyOutOfSetTarget reads a depends_on target that is OUTSIDE the assembled
// set and classifies it into a satisfied/dangling outcome, fail-closed by
// construction (#2953). It is shared by BOTH campaign source paths (EpicChildren
// and ResolveDependencies) so "satisfied" means exactly one thing across the
// codebase. It takes the PARSED reference (dependsOnRef), not a bare int, so the
// same-repo-numeric provenance parseDependsOnMarker established is available here
// rather than lost at the boundary. Rules:
//   - an UNRESOLVABLE ref (a cross-repo `owner/repo#N` ref, an owner-qualified
//     ref, or an unparseable token — Resolvable=false) stamps
//     DropTargetStateUnreadable WITHOUT a forge call: calling GetIssue with a
//     locally-reduced number could read an UNRELATED same-repo issue's state and
//     FALSELY satisfy the edge, the one failure mode this change must not have
//     (operator condition 2 / the routed cross-repo security concerns);
//   - a non-positive number (defensive; a resolvable ref always carries a positive
//     number) likewise stamps DropTargetStateUnreadable WITHOUT a forge call;
//   - a GetIssue error yields DropTargetStateUnreadable (never satisfied — an
//     unreadable target is not evidence of satisfaction);
//   - a nil issue (no error) yields DropTargetStateUnreadable (the nil-issue
//     guard, distinct from the error branch so each is independently deletable);
//   - closed AND completed (case-insensitive, since GetIssue's REST payload is
//     lowercase) yields satisfied — the SAME rule EpicChild.Complete encodes;
//   - a closed issue with any other state_reason (not_planned/duplicate) yields
//     DropTargetClosedIncomplete;
//   - anything else (open) keeps DropNotChild.
//
// The result is memoized in cache keyed by number, so the GetIssue call is made
// at most once per distinct target across the whole resolution. An unresolvable
// ref is NOT cached (it carries no meaningful number key and costs no forge call).
func (p *Provider) classifyOutOfSetTarget(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, ref dependsOnRef, cache map[int]targetState) (targetState, error) {
	// An unresolvable ref must NEVER reach GetIssue: reducing a cross-repo token to
	// a local number and reading that unrelated same-repo issue could FALSELY
	// satisfy the edge (operator condition 2). Fail closed WITHOUT a forge call,
	// ahead of the cache and number guards, so the provenance is honored before any
	// numeric reasoning.
	if !ref.Resolvable {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}, nil
	}
	number := ref.Number
	if cached, ok := cache[number]; ok {
		return cached, nil
	}
	// Guard the forge call: only a positive same-repo numeric target is safe to
	// GetIssue. A non-positive number is defensive here — parseDependsOnMarker only
	// marks a ref Resolvable when it captured `[1-9]\d*` — but keep it so a
	// direct/synthetic non-positive ref never reads an arbitrary issue.
	if number <= 0 {
		ts := targetState{reason: workmgmt.DropTargetStateUnreadable}
		cache[number] = ts
		return ts, nil
	}
	issue, err := p.api.GetIssue(ctx, scope, repo, number)
	ts := classifyFetchedTarget(issue, err)
	cache[number] = ts
	return ts, nil
}

// classifyFetchedTarget is the PURE classification half of
// classifyOutOfSetTarget: given an ALREADY-FETCHED out-of-set target it makes
// no forge call and consults no cache, so the same rules can be applied by the
// serial EpicChildren path (which fetches inline, above) and by the concurrent
// ResolveDependencies path (which fetches in a bounded pool and classifies
// afterwards, #3113) with ONE definition of what "satisfied" means. The
// branches are unchanged from the pre-#3113 inline switch:
//   - a fetch error yields DropTargetStateUnreadable (never satisfied — an
//     unreadable target is not evidence of satisfaction). Kept DISTINCT from
//     the nil-issue branch below so deleting either control is independently
//     observable (#2953 operator condition 4);
//   - a nil issue with no error yields DropTargetStateUnreadable;
//   - closed AND completed (case-insensitive, since GetIssue's REST payload is
//     lowercase) yields satisfied;
//   - a closed issue with any other state_reason yields DropTargetClosedIncomplete;
//   - anything else (open) keeps DropNotChild.
func classifyFetchedTarget(issue *githubclient.Issue, err error) targetState {
	if err != nil {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}
	}
	if issue == nil {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}
	}
	switch {
	case strings.EqualFold(issue.State, "closed") && strings.EqualFold(issue.StateReason, "completed"):
		// Closed AND completed: the prerequisite landed. Satisfied — the same
		// rule EpicChild.Complete encodes, case-insensitively (REST is lowercase).
		return targetState{satisfied: true, state: issue.State, stateReason: issue.StateReason}
	case strings.EqualFold(issue.State, "closed"):
		// Closed but not completed (not_planned/duplicate): work did not land, so
		// no batch-widening knob can satisfy the edge.
		return targetState{reason: workmgmt.DropTargetClosedIncomplete, state: issue.State, stateReason: issue.StateReason}
	default:
		// Open (or any non-closed state): the pre-#2953 dangling refusal.
		return targetState{reason: workmgmt.DropNotChild, state: issue.State, stateReason: issue.StateReason}
	}
}

// sortEdges deterministically orders depends_on edges by (From, To, ToRefDigest)
// so an EpicChildrenResult is stable across runs. The ToRefDigest tiebreak
// (#2956) fully determines the order of a set of UNRESOLVABLE edges that share
// (From, To=0) but carry distinct token digests — without it sort.Slice (which
// is not stable) leaves their order to the input, which it does not preserve
// above the insertion-sort threshold.
func sortEdges(es []workmgmt.DependsEdge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From != es[j].From {
			return es[i].From < es[j].From
		}
		if es[i].To != es[j].To {
			return es[i].To < es[j].To
		}
		return es[i].ToRefDigest < es[j].ToRefDigest
	})
}

// sortSatisfiedEdges deterministically orders elided (satisfied) edges by
// (From, To), matching sortEdges so the whole result is stable (#2953).
func sortSatisfiedEdges(es []workmgmt.SatisfiedEdge) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].From != es[j].From {
			return es[i].From < es[j].From
		}
		return es[i].To < es[j].To
	})
}

// ResolveDependencies resolves the depends_on edges over an ARBITRARY,
// explicitly-named set of issues that share no epic parent — the no-epic
// campaign source (E48.36 / #2051, the items-without-epic_ref variant). Where
// EpicChildren derives its sibling set from an epic's sub-issue graph, this is
// HANDED the exact set (req.Items) and resolves each named issue directly: it
// parses each ref via parseIssueRef, fetches the issue body via GetIssue, and
// runs the SAME depends_on marker / autonomy-label / completion helpers
// EpicChildren uses (no regex duplication), emitting the same
// *workmgmt.EpicChildrenResult shape campaign.Assemble consumes.
//
// The named set IS the sibling set: a parsed depends_on ref pointing at a
// member is an Edge; a ref pointing OUTSIDE the named set is CLASSIFIED by
// reading the target's issue state (#2953, via classifyOutOfSetTarget): a
// closed-AND-completed target is a SatisfiedEdge (its work landed, so the
// dependency is satisfied and the edge is elided from the wave DAG), while an
// OPEN target (DropNotChild), a closed-but-incomplete not_planned/duplicate
// target (DropTargetClosedIncomplete), or an unreadable target
// (DropTargetStateUnreadable) is a DroppedEdge the campaign fails closed on.
// This is the #2953 refinement of the older fail-closed-on-EVERY-out-of-set
// contract, so a batch whose prerequisite is already done assembles instead of
// refusing with unactionable advice. Children are returned ascending by number
// and all edge slices are deterministically sorted (by From, then To),
// mirroring EpicChildren. It validates the target repo + installation (fail
// closed with File's actionable style).
//
// # Three-phase bounded-concurrency resolution (#3113)
//
// Resolving a large named set serially costs one round-trip per named issue
// plus one per distinct out-of-set target — ~60-100 sequential REST calls for a
// sixty-item grooming order, which is what made campaign assembly from a full
// ratified order time out. The resolution therefore runs in three phases:
//
//   - PHASE 1 fetches every named issue with at most issueSetFetchConcurrency
//     concurrent GetIssue calls;
//   - PHASE 2 collects the DISTINCT out-of-set depends_on targets in
//     first-encounter order and fetches them with the same bounded pool,
//     building the state cache afterwards;
//   - PHASE 3 classifies SERIALLY, on one goroutine, in request order.
//
// NO SHARED MUTABLE STATE crosses a goroutine boundary and there is no mutex: a
// worker's only output is a value struct sent on a channel, and every map and
// slice write is the parent's, made after the pool has drained. That is what
// makes the emitted *workmgmt.EpicChildrenResult BYTE-IDENTICAL to the serial
// result regardless of completion order (pinned by the 50-repetition
// jitter test), and it is why Go's concurrent-map-write fatal error is
// unreachable here by construction rather than by locking discipline.
//
// EpicChildren is deliberately NOT routed through this pool: it reads its
// sibling set in ONE ListSubIssues call, so it was never the cost, and keeping
// it serial keeps its output byte-identical.
//
// DEADLINES. Every phase checks the context BEFORE returning any wrapped fetch
// error and returns *workmgmt.IssueSetResolutionTimeout instead, carrying the
// honest counts (see issueSetTimeout for the accounting rule). An ORDINARY
// phase-1 fetch failure with the context alive still returns the wrapped
// provider error, and deterministically the one FIRST IN REQUEST ORDER. An
// ordinary phase-2 (target) fetch failure is NOT an error at all — it still
// classifies DropTargetStateUnreadable exactly as the serial path did, so
// #2953's meaning of "unreadable" is unchanged; only a CONTEXT-TERMINATED
// target fetch is treated differently, leaving no cache entry.
func (p *Provider) ResolveDependencies(ctx context.Context, req workmgmt.IssueSetRequest) (*workmgmt.EpicChildrenResult, error) {
	if p.api == nil {
		return nil, errors.New("workmgmt/github: provider missing API client")
	}
	if req.Target.Repo.Owner == "" || req.Target.Repo.Name == "" {
		return nil, errors.New("workmgmt/github: target repo owner and name required")
	}
	scope := req.Target.Scope
	if scope.IsZero() {
		return nil, errors.New("workmgmt/github: no installation id available; issue-set dependency resolution is run-scoped in v0 — file with a run_id whose run carries an installation")
	}
	repo := forge.RepoRef{Owner: req.Target.Repo.Owner, Name: req.Target.Repo.Name}

	// Resolve each named ref to an issue number up front, so the sibling set is
	// known before edges are partitioned. Parse errors fail closed naming the ref.
	numbers := make([]int, 0, len(req.Items))
	inSet := make(map[int]bool, len(req.Items))
	for _, ref := range req.Items {
		// Items arrive in the bare-number ("101") or issue:N ("issue:101") ref
		// convention (the campaign subset ref shape); strip the optional issue:
		// prefix, then REUSE parseIssueRef (which also tolerates a leading #) so
		// the number parse stays a single helper — no regex duplication.
		n, err := parseIssueRef(strings.TrimPrefix(strings.TrimSpace(ref), "issue:"))
		if err != nil {
			return nil, fmt.Errorf("workmgmt/github: item %q: %w", ref, err)
		}
		if inSet[n] {
			continue // tolerate a duplicate ref: resolve each named issue once.
		}
		inSet[n] = true
		numbers = append(numbers, n)
	}

	// PHASE 1 — fetch every named issue with bounded concurrency. Workers RETURN
	// their results over a channel and write nothing shared; the parent places
	// each into an ordinal-indexed slice after the pool drains, so the whole
	// resolution has no shared mutable state across goroutines and no mutex
	// (#3113).
	fetches := p.fetchIssuesBounded(ctx, scope, repo, numbers)
	// Deadline precedence: check the context BEFORE returning any wrapped fetch
	// error, so a deadline that tripped a forge call surfaces as the typed
	// timeout with honest counts rather than as `get issue #N: context deadline
	// exceeded` — which would present a deadline as a provider fault and leave
	// the server on its generic failure code.
	if ctx.Err() != nil {
		return nil, issueSetTimeout(numbers, inSet, fetches, nil, "fetch_items")
	}
	// Context alive, but a fetch failed: return the error of the item FIRST IN
	// REQUEST ORDER. The parent walks the ordinal-indexed slice ascending, so
	// the returned error is deterministic no matter which worker finished first.
	for _, f := range fetches {
		if f.err != nil {
			return nil, fmt.Errorf("workmgmt/github: get issue #%d: %w", f.number, f.err)
		}
		if f.issue == nil {
			// A named item the forge returned neither an issue nor an error for.
			// Fail closed naming the item: every downstream phase reads the
			// issue's number/title/body, so continuing would either panic or
			// invent a child.
			return nil, fmt.Errorf("workmgmt/github: get issue #%d: no issue returned", f.number)
		}
	}

	// PHASE 2 — collect the DISTINCT out-of-set targets in first-encounter order
	// (a single-goroutine walk of the phase-1 results in request order), fetch
	// them with the same bounded pool, and build the state cache SERIALLY in the
	// parent after the pool drains.
	targets := outOfSetTargets(inSet, fetches)
	stateCache := map[int]targetState{}
	targetFetches := p.fetchIssuesBounded(ctx, scope, repo, targets)
	for _, tf := range targetFetches {
		if tf.aborted {
			// A context-TERMINATED fetch is not evidence of anything: it is never
			// cached and never counts its item resolved. This is deliberately
			// distinct from an ordinary 404/permission error, which still caches
			// DropTargetStateUnreadable exactly as the serial path did (#2953 is
			// not widened) — the distinction is context-termination, not
			// error-vs-success.
			continue
		}
		stateCache[tf.number] = classifyFetchedTarget(tf.issue, tf.err)
	}
	if ctx.Err() != nil {
		return nil, issueSetTimeout(numbers, inSet, fetches, stateCache, "classify_targets")
	}

	// PHASE 3 — a SERIAL classification pass on one goroutine over the phase-1
	// results in request order, reading the now-immutable stateCache. This is
	// the pre-#3113 loop body verbatim except that the classification is a cache
	// LOOKUP rather than a forge call, so the emitted result is byte-identical.
	children := make([]workmgmt.EpicChild, 0, len(numbers))
	var edges, dropped []workmgmt.DependsEdge
	var satisfied []workmgmt.SatisfiedEdge
	// Collapse duplicate edges so a repeated depends_on token
	// (`Depends on: #1639, #1639`) yields AT MOST ONE edge in whichever slice it
	// classifies into (#2953 condition 3) — the same (From, To, ToRefDigest) key
	// EpicChildren applies, so two DISTINCT cross-repo tokens stay two edges while
	// the same canonical token repeated collapses (#2956).
	seenEdge := map[dependsEdgeKey]bool{}
	for _, f := range fetches {
		issue := f.issue
		children = append(children, workmgmt.EpicChild{
			Number:   issue.Number,
			Title:    issue.Title,
			Autonomy: parseAutonomyLabel(issue.Labels),
			// Complete = closed-AND-completed. GetIssue is the REST issue payload,
			// whose state/state_reason are LOWERCASE ("closed"/"completed") — unlike
			// ListSubIssues' uppercase GraphQL enums — so match case-insensitively.
			// A not_planned / duplicate close is NOT complete (its work did not land).
			Complete: strings.EqualFold(issue.State, "closed") && strings.EqualFold(issue.StateReason, "completed"),
		})
		for _, dep := range parseDependsOnMarker(issue.Body) {
			key := dependsEdgeKey{From: issue.Number, To: dep.Number, Digest: dep.RawDigest}
			if seenEdge[key] {
				continue // duplicate depends_on token: classify once per identity.
			}
			seenEdge[key] = true
			if dep.Resolvable && inSet[dep.Number] {
				edges = append(edges, workmgmt.DependsEdge{From: issue.Number, To: dep.Number})
				continue
			}
			// A depends_on reference that is NOT an in-set named issue: a typo'd
			// number, a genuine cross-batch dependency, a prerequisite that is
			// already done, or a cross-repo/unparseable ref. Classify it (#2953): a
			// closed-and-completed target is a satisfied dependency (elided from the
			// DAG), while an open/incomplete/unreadable one — and any UNRESOLVABLE
			// cross-repo ref — is dangling and campaign assembly fails closed on it.
			cls, ok := lookupTargetState(dep, stateCache)
			if !ok {
				// Defensive: a target reached classification unclassified — its
				// phase-2 fetch was context-terminated and left no cache entry.
				// Return the typed timeout rather than GUESSING a classification;
				// guessing would either invent a satisfied dependency or fabricate
				// an unreadable one, and both are worse than an honest refusal.
				return nil, issueSetTimeout(numbers, inSet, fetches, stateCache, "build_result")
			}
			if cls.satisfied {
				satisfied = append(satisfied, workmgmt.SatisfiedEdge{
					From: issue.Number, To: dep.Number, State: cls.state, StateReason: cls.stateReason,
				})
			} else {
				dropped = append(dropped, workmgmt.DependsEdge{
					From: issue.Number, To: dep.Number, Reason: cls.reason,
					// Carry the unresolvable token's identity onto the dropped edge so
					// the renderer names WHICH token (#2956); both empty on a numeric
					// dropped edge, keeping its shape unchanged.
					ToRef: dep.RawDisplay, ToRefDigest: dep.RawDigest,
				})
			}
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Number < children[j].Number })
	sortEdges(edges)
	sortEdges(dropped)
	sortSatisfiedEdges(satisfied)
	return &workmgmt.EpicChildrenResult{Children: children, Edges: edges, DroppedEdges: dropped, SatisfiedEdges: satisfied}, nil
}

// issueSetFetchConcurrency bounds the in-flight forge reads of one issue-set
// resolution. It is well under GitHub's published no-more-than-100-concurrent-
// requests REST guidance, and it is the bound the at-most-N / at-least-N channel
// probes in provider_test.go pin structurally rather than by reading the
// constant.
const issueSetFetchConcurrency = 8

// issueFetch is ONE worker's return value: a pure value struct sent over a
// channel. A worker writes to nothing else — no map, no counter, no mutex, no
// slice element — so the concurrent phases of ResolveDependencies have no shared
// mutable state at all (#3113). The parent places each value into an
// ordinal-indexed slice after the pool drains.
type issueFetch struct {
	ordinal int
	number  int
	issue   *githubclient.Issue
	err     error
	// aborted marks a fetch the CONTEXT terminated, as distinct from one the
	// forge refused. A context-terminated target is never cached and never
	// counts its item resolved; an ordinary 404/permission error still classifies
	// DropTargetStateUnreadable exactly as the serial path did.
	aborted bool
}

// fetchIssuesBounded fetches numbers with at most issueSetFetchConcurrency
// concurrent GetIssue calls and returns one issueFetch per input, INDEXED BY
// REQUEST ORDER regardless of completion order. Every result slot is filled:
// the feeder never short-circuits on a dead context, because a GetIssue made
// with an expired context returns promptly with a context error, and filling
// every slot is what keeps the accounting (and the deterministic
// first-in-request-order error selection) total rather than dependent on how
// many results happened to arrive.
func (p *Provider) fetchIssuesBounded(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, numbers []int) []issueFetch {
	out := make([]issueFetch, len(numbers))
	if len(numbers) == 0 {
		return out
	}
	workers := issueSetFetchConcurrency
	if workers > len(numbers) {
		workers = len(numbers)
	}
	jobs := make(chan int)
	// Buffered to len(numbers) so no worker can block on the send: a worker that
	// has produced its value is done, and the parent drains at its own pace.
	results := make(chan issueFetch, len(numbers))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n := numbers[i]
				issue, err := p.api.GetIssue(ctx, scope, repo, n)
				results <- issueFetch{
					ordinal: i,
					number:  n,
					issue:   issue,
					err:     err,
					// Context-terminated iff the call FAILED and the failure is (or
					// happened under) a dead context. The err != nil guard is what
					// keeps a successful fetch made just before the deadline from
					// being mislabelled aborted.
					aborted: err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil),
				}
			}
		}()
	}
	for i := range numbers {
		jobs <- i
	}
	close(jobs)
	for range numbers {
		r := <-results
		out[r.ordinal] = r
	}
	wg.Wait()
	return out
}

// outOfSetTargets walks the phase-1 results in REQUEST ORDER on a single
// goroutine and collects the distinct out-of-set depends_on targets that need a
// forge read, in first-encounter order. It applies the SAME per-item
// (From, To, ToRefDigest) dedup phase 3 applies, so a token phase 3 will skip is
// never fetched, and it excludes exactly the refs classifyOutOfSetTarget
// classifies WITHOUT a forge call: an unresolvable cross-repo/unparseable ref
// (reducing it to a local number could read an UNRELATED issue and falsely
// satisfy the edge — #2953 operator condition 2) and a non-positive number.
func outOfSetTargets(inSet map[int]bool, fetches []issueFetch) []int {
	var targets []int
	seenTarget := map[int]bool{}
	seenEdge := map[dependsEdgeKey]bool{}
	for _, f := range fetches {
		if f.issue == nil {
			continue
		}
		for _, dep := range parseDependsOnMarker(f.issue.Body) {
			key := dependsEdgeKey{From: f.issue.Number, To: dep.Number, Digest: dep.RawDigest}
			if seenEdge[key] {
				continue
			}
			seenEdge[key] = true
			if !needsTargetFetch(dep, inSet) || seenTarget[dep.Number] {
				continue
			}
			seenTarget[dep.Number] = true
			targets = append(targets, dep.Number)
		}
	}
	return targets
}

// needsTargetFetch reports whether a parsed depends_on ref costs a forge read:
// it must be resolvable, positive, and OUTSIDE the named set. It is the single
// predicate phase 2 (what to fetch), the accounting (what must be classified for
// an item to count resolved) and lookupTargetState (what may be answered without
// a cache entry) all agree on, so the three can never drift.
func needsTargetFetch(dep dependsOnRef, inSet map[int]bool) bool {
	if !dep.Resolvable || dep.Number <= 0 {
		return false
	}
	return !inSet[dep.Number]
}

// lookupTargetState answers phase 3's classification from the immutable cache.
// The two no-forge-call refusals classifyOutOfSetTarget stamps directly — an
// UNRESOLVABLE ref and a non-positive number — are answered here WITHOUT a cache
// entry, preserving that neither ever reaches GetIssue. Anything else must have
// been fetched in phase 2; ok=false means it was not (a context-terminated
// fetch), which phase 3 turns into the typed timeout rather than a guess.
func lookupTargetState(dep dependsOnRef, cache map[int]targetState) (targetState, bool) {
	if !dep.Resolvable {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}, true
	}
	if dep.Number <= 0 {
		return targetState{reason: workmgmt.DropTargetStateUnreadable}, true
	}
	ts, ok := cache[dep.Number]
	return ts, ok
}

// issueSetTimeout builds the typed deadline error with the counts, applying ONE
// uniform accounting rule at whichever phase the deadline hit. An item is FULLY
// RESOLVED when its own fetch completed AND every out-of-set target it names is
// classified; an item naming no out-of-set target is fully resolved as soon as
// its fetch completes. Resolved is the count of such items anywhere in the
// request order; SuggestedLimit is the length of the longest fully-resolved
// PREFIX of that order — a value that provably would have fit, because the
// grooming path takes the top-N by ratified rank and this resolver preserves
// request order — and 0 (no suggestion) when that prefix is empty. A suggestion
// is NEVER derived from a non-prefix count.
func issueSetTimeout(numbers []int, inSet map[int]bool, fetches []issueFetch, cache map[int]targetState, phase string) *workmgmt.IssueSetResolutionTimeout {
	resolved := 0
	suggested := 0
	prefixIntact := true
	for _, f := range fetches {
		if itemFullyResolved(f, inSet, cache) {
			resolved++
			if prefixIntact {
				suggested++
			}
		} else {
			prefixIntact = false
		}
	}
	return &workmgmt.IssueSetResolutionTimeout{
		Resolved:       resolved,
		Total:          len(numbers),
		SuggestedLimit: suggested,
		Phase:          phase,
	}
}

// itemFullyResolved is the single accounting predicate (see issueSetTimeout).
// A nil cache — the phase-1 deadline, where no target has been classified yet —
// makes every item naming an out-of-set target unresolved, which is exactly
// true at that phase.
func itemFullyResolved(f issueFetch, inSet map[int]bool, cache map[int]targetState) bool {
	if f.err != nil || f.issue == nil {
		return false
	}
	for _, dep := range parseDependsOnMarker(f.issue.Body) {
		if !needsTargetFetch(dep, inSet) {
			continue
		}
		if _, ok := cache[dep.Number]; !ok {
			return false
		}
	}
	return true
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
// `Depends on: #X, #Y`. It is the single source of truth for the marker
// FORMAT (the `Depends on: ` prefix and the comma-separated join), paired with
// parseDependsOnMarker so write and read cannot drift. Returns "" when refs is
// empty, so an item with no depends_on carries no marker.
//
// The SHAPE of each emitted ref is decided by workmgmt.NormalizeIssueRef and
// nowhere else (#2860). This function used to inline its own
// trim/strip/`#`-prefix, which was a THIRD copy of that decision and disagreed
// with the normalizer on a non-numeric token: it emitted `#owner/repo#123`
// where the membership read observes `owner/repo#123`, so what the filing path
// WROTE and what a later read OBSERVED could never match. For every numeric
// token the emitted bytes are IDENTICAL to before (`#41`->`#41`, `42`->`#42`).
//
// The empty-after-strip guard below is NOT a competing normalizer — it decides
// whether a token is emitted AT ALL, not what shape it takes. It must stay:
// NormalizeIssueRef("#") returns the trimmed original `"#"`, which is
// non-empty, so without this guard a bare `#` token would start being emitted
// as a reference.
func renderDependsOnMarker(refs []string) string {
	var parts []string
	for _, r := range refs {
		if dependsOnRefStripped(r) == "" {
			continue
		}
		parts = append(parts, workmgmt.NormalizeIssueRef(r))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Depends on: " + strings.Join(parts, ", ")
}

// dependsOnRefStripped is the depends_on marker's VALIDITY test, spelled once
// so the render path and the amend path cannot disagree about which tokens are
// references at all. It returns the ref with surrounding whitespace and ONE
// optional leading `#` removed; an empty result means the token carries no
// reference (an empty string, whitespace, or a bare `#`) and must be skipped.
//
// It decides EMPTINESS only, never SHAPE — shape is workmgmt.NormalizeIssueRef's
// job alone (#2860).
func dependsOnRefStripped(ref string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
}

// dependsOnMarkerRefs is the AMEND path's MEMBERSHIP read: every depends_on ref
// the body already records, normalized through workmgmt.NormalizeIssueRef and
// returned in body order.
//
// It walks EVERY marker line, not just the first, mirroring the core's
// groomingMarkerObserved — the two layers must observe the SAME ref set or
// #2860 reappears as a disagreement about membership.
//
// It deliberately does NOT reuse parseDependsOnMarker: that reads only the
// FIRST marker line and collapses every non-bare-numeric token to
// dependsOnRef{Resolvable: false}, DISCARDING the token text, so a cross-repo
// ref could not be compared at all.
func dependsOnMarkerRefs(body string) []string {
	var out []string
	for _, m := range dependsOnMarkerRE.FindAllStringSubmatch(body, -1) {
		// The `(.+)` capture swallows a trailing carriage return on a CRLF
		// body (Go's `(?m)$` anchors before `\n` only, and `.` matches `\r`),
		// so trim it before splitting or the last token would be `#123` plus a
		// stray CR and would never compare equal.
		for _, tok := range strings.Split(strings.TrimSuffix(m[1], "\r"), ",") {
			if dependsOnRefStripped(tok) == "" {
				continue
			}
			out = append(out, workmgmt.NormalizeIssueRef(tok))
		}
	}
	return out
}

// ensureDependsOnMarker appends the depends_on marker line to body when refs
// is non-empty and body does not already carry a marker. Idempotent: a body
// that already has a `Depends on:` line is returned unchanged, so re-filing
// never double-stamps the marker.
//
// This is the FILING path's helper (E34.3 / #1594) and its never-double-stamp
// contract is CORRECT there: filing an item re-sends its whole declared
// depends_on set, so a body that already carries a marker already carries them.
// It is deliberately NOT widened to be additive — the grooming AMEND path,
// which adds ONE new edge to an item that may already record others, uses the
// separately-named appendDependsOnRef instead (#2860). Two named helpers, not
// one helper with a mode flag, so neither path can silently acquire the
// other's behaviour.
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

// appendDependsOnRef MERGES one depends_on ref into the body, additively. It is
// the grooming AMEND path's counterpart to the filing path's
// ensureDependsOnMarker, and is what #2860 fixes: ensureDependsOnMarker refuses
// EVERY write against a marker-bearing body, so an item recording one
// dependency could never gain a second, and that refusal was audited as an
// indistinguishable no-op.
//
// It returns the new body and whether the body actually CHANGED. changed is
// never true for a body that did not change — reporting a refusal as a success
// is the exact failure #2860 exists to kill.
//
//   - A ref that is not a reference at all (empty, whitespace, a bare `#`) is a
//     no-op returning changed=false. Both body shapes must be covered here:
//     with a marker the splice would append a meaningless `, #`, and WITHOUT one
//     the delegation below would return the body unchanged (renderDependsOnMarker
//     skips the token) while still reporting changed=true.
//   - A ref the body ALREADY records is the genuine idempotent no-op.
//   - A body with NO marker delegates to ensureDependsOnMarker rather than
//     hand-rolling the append-a-whole-new-marker case.
//   - Otherwise the ref is spliced into the FIRST marker line's captured value.
//
// TRAILING-CR HANDLING. Go's `(?m)$` anchors immediately before a `\n` only —
// it does not treat `\r\n` as one terminator — and `.` matches every byte but
// `\n`, including a carriage return. So on a CRLF body dependsOnMarkerRE's
// capture ENDS WITH the CR, and appending at the raw capture end would emit
// `#1641<CR>, #2032` and corrupt the line. The CR is therefore trimmed before
// appending and re-emitted after, preserving the line ending byte for byte.
// Nothing outside the capture group is rewritten, so surrounding body bytes,
// any other marker lines and the body's final newline are untouched.
func appendDependsOnRef(body, ref string) (string, bool) {
	if dependsOnRefStripped(ref) == "" {
		return body, false
	}
	want := workmgmt.NormalizeIssueRef(ref)
	for _, got := range dependsOnMarkerRefs(body) {
		if got == want {
			return body, false
		}
	}
	loc := dependsOnMarkerRE.FindStringSubmatchIndex(body)
	if loc == nil {
		return ensureDependsOnMarker(body, []string{ref}), true
	}
	val := body[loc[2]:loc[3]]
	core := strings.TrimSuffix(val, "\r")
	cr := val[len(core):]
	return body[:loc[2]] + core + ", " + want + cr + body[loc[3]:], true
}

// dependsOnRef is one parsed token from the depends_on marker list, carrying the
// parse/repository provenance the out-of-set classifier needs to fail closed on a
// non-same-repo target (#2953, operator condition 2). Resolvable is true ONLY for
// a bare same-repo numeric reference (`#N` or `N`), in which case Number is the
// positive issue number safe to GetIssue against the ambient repo. It is false for
// any OTHER non-empty token — a cross-repo `owner/repo#N` ref, an owner-qualified
// ref, or an otherwise unparseable token — so the classifier can stamp
// DropTargetStateUnreadable WITHOUT a forge call instead of either silently
// omitting the edge or reducing the token to a local number and reading an
// unrelated same-repo issue's state. An owner-qualified ref is treated as
// unresolvable even when its owner/name would match the ambient repo: only a BARE
// number is unambiguously same-repo, and fail-closed is the safe direction.
type dependsOnRef struct {
	Number     int
	Resolvable bool
	// RawDigest and RawDisplay give an UNRESOLVABLE token an identity in the
	// dropped edge it produces (#2956), so two distinct cross-repo tokens no
	// longer both collapse to the identity-less `issue:From->issue:0`. Both are
	// empty on a resolvable numeric ref (its edge keeps its byte-identical
	// zero-valued shape). RawDigest is dependsOnTokenDigest over the CANONICAL
	// (pre-sanitization) form, so control-rune stripping and rune truncation
	// cannot merge two distinct canonical tokens; RawDisplay is the sanitized,
	// rune-bounded human form (the raw token is untrusted issue-body content and
	// must never reach a message or log unsanitized).
	RawDigest  string
	RawDisplay string
}

// dependsOnTokenCanonical is the single definition of a depends_on token's
// IDENTITY relation (#2956): the whitespace-trimmed, PRE-sanitization text. Two
// tokens sharing a canonical form ARE one token and correctly collapse to one
// edge (the #2953 condition-3 dedup this change preserves on purpose); distinct
// canonical forms are distinguished by dependsOnTokenDigest.
func dependsOnTokenCanonical(tok string) string {
	return strings.TrimSpace(tok)
}

// dependsOnTokenDigest is the 16-hex-char (64-bit) truncated SHA-256 of a
// token's canonical form, the stable identity a dropped unresolvable edge
// carries (#2956). For k distinct canonical tokens compared within one item the
// birthday-bound probability that any two share a digest is about k^2/2^65
// (~2.7e-16 at k=100), so the identity claim is explicitly PROBABILISTIC, not
// absolute — the digest is an operator-facing DIAGNOSTIC identifier, never an
// authorization or satisfaction control. It is computed on the CANONICAL form,
// BEFORE any sanitization, so control-rune stripping or rune truncation can
// never merge two distinct canonical tokens onto one digest.
func dependsOnTokenDigest(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

// sanitizeDependsOnToken renders a canonical token into the bounded, printable
// human DISPLAY form carried on RawDisplay and, ultimately, into an operator
// message (#2956). It drops unicode control / non-printable runes (and U+FEFF),
// collapses internal whitespace runs to single spaces, and bounds the result to
// dependsOnDisplayMaxRunes runes, appending a horizontal-ellipsis rune on
// truncation. It returns "" when nothing printable survives, which the renderer
// turns into the bounded `<unprintable>` sentinel rather than an empty ref.
//
// Sanitization is a DISPLAY transform ONLY: it runs AFTER the digest is taken,
// never before, so it can never merge two distinct canonical tokens — that
// ordering is the identity guarantee (#2956, digest-before-sanitize).
func sanitizeDependsOnToken(canonical string) string {
	var runes []rune
	prevSpace := false
	for _, r := range canonical {
		// Normalize EVERY whitespace class \u2014 the ASCII space, a tab, and any
		// printable Unicode space (U+00A0 no-break, U+2003 em, \u2026) \u2014 to ONE
		// collapsed ASCII space, so display normalization is consistent across
		// classes (#2956, routed whitespace concern). This is checked BEFORE the
		// IsPrint drop so a tab (which unicode.IsPrint rejects) collapses to a
		// space rather than being silently discarded, and an exotic Unicode space
		// is collapsed rather than passed through unchanged. U+FEFF is NOT
		// White_Space, so it still falls through to the drop below.
		if unicode.IsSpace(r) {
			if prevSpace {
				continue
			}
			prevSpace = true
			runes = append(runes, ' ')
			continue
		}
		if r == '\uFEFF' || !unicode.IsPrint(r) {
			continue
		}
		prevSpace = false
		runes = append(runes, r)
	}
	s := strings.TrimSpace(string(runes))
	if utf8.RuneCountInString(s) > dependsOnDisplayMaxRunes {
		rs := []rune(s)
		s = string(rs[:dependsOnDisplayMaxRunes-1]) + "…"
	}
	return s
}

// dependsOnDisplayMaxRunes bounds the sanitized display form of a depends_on
// token so a maliciously long issue body cannot blow up an operator message.
// The renderer (workmgmt.DependsEdge.TargetRef) enforces the SAME bound on any
// directly-constructed edge, so the guarantee holds however a DependsEdge is
// built (#2956, operator condition 1).
const dependsOnDisplayMaxRunes = 64

// parseDependsOnMarker parses the depends_on body marker line into its referenced
// targets. It reads the FIRST `Depends on:` line and splits the captured list on
// commas; each NON-EMPTY token becomes a dependsOnRef — a bare `#N`/`N` token is a
// Resolvable same-repo numeric reference, and every OTHER non-empty token (a
// cross-repo `owner/repo#N` ref, an owner-qualified ref, or garbage) is carried
// through UNRESOLVABLE rather than silently dropped, so the classifier fails the
// campaign closed on it instead of letting a cross-repo dependency vanish (#2953,
// operator condition 2). Genuinely empty tokens (a trailing comma, surrounding
// whitespace) are skipped — they are not references at all. Returns nil when no
// marker is present. Paired with renderDependsOnMarker as the single source of
// truth for the marker round trip.
func parseDependsOnMarker(body string) []dependsOnRef {
	m := dependsOnMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	var refs []dependsOnRef
	for _, tok := range strings.Split(m[1], ",") {
		// The canonical form (whitespace-trimmed, pre-sanitization) is the token's
		// identity (#2956): computed ONCE, it drives both the empty-token skip and
		// the digest so control-rune stripping / truncation cannot merge two
		// distinct canonical tokens.
		canon := dependsOnTokenCanonical(tok)
		if canon == "" {
			continue // trailing comma / whitespace: not a reference at all.
		}
		rm := dependsOnRefRE.FindStringSubmatch(tok)
		if rm == nil {
			// A non-empty token that is NOT a bare same-repo numeric ref — a
			// cross-repo `owner/repo#N`, an owner-qualified ref, or garbage. Carry
			// it through as UNRESOLVABLE, now with an IDENTITY (digest over the
			// canonical form + sanitized display), so the classifier fails it closed
			// AND the dropped edge names WHICH token — two distinct cross-repo tokens
			// no longer collapse to one identity-less `issue:From->issue:0` (#2956).
			refs = append(refs, dependsOnRef{
				Resolvable: false,
				RawDigest:  dependsOnTokenDigest(canon),
				RawDisplay: sanitizeDependsOnToken(canon),
			})
			continue
		}
		n, err := strconv.Atoi(rm[1])
		if err != nil {
			refs = append(refs, dependsOnRef{
				Resolvable: false,
				RawDigest:  dependsOnTokenDigest(canon),
				RawDisplay: sanitizeDependsOnToken(canon),
			})
			continue
		}
		refs = append(refs, dependsOnRef{Number: n, Resolvable: true})
	}
	return refs
}

// parseAutonomyLabel delegates to workmgmt.ParseAutonomyLabel — the single
// source of truth for the `autonomy:` tier parse (#2355), shared with the
// reconcile-on-read autonomy refresh path. It is retained as a thin local name
// so the provider's assembly call sites (EpicChildren / ResolveDependencies) and
// the existing provider_test table read unchanged, proving the extraction is
// behavior-preserving.
func parseAutonomyLabel(labels []string) string {
	return workmgmt.ParseAutonomyLabel(labels)
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
