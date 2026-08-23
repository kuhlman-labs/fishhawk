package campaign

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// GroomingOrder is the rank-ordered issue set derived from ONE approved
// grooming run's grooming_report artifact — the third campaign source
// (E54.6 / #2238). It is produced by OrderFromReport (pure) and consumed by
// the create handler, which feeds Numbers into the existing #2051 no-epic
// item path and then permutes the resolved children with ReorderByPriority so
// the ratified rank order becomes the ORDER campaign items are created in.
//
// The order is read EXACTLY ONCE, at assembly. Nothing in the campaign engine
// or the auto-driver re-reads a report or a board afterwards; the durable
// campaign_items.queue_position column carries the order from then on.
type GroomingOrder struct {
	// RunID / StageID / ArtifactID identify the ratified source: the grooming
	// run, its plan stage carrying the report, and the report artifact row.
	RunID      uuid.UUID
	StageID    uuid.UUID
	ArtifactID uuid.UUID
	// ContentHash is the report artifact's content hash — the value that makes
	// the provenance reproducible (a later reader can prove WHICH report text
	// the campaign was built from, not merely which run).
	ContentHash string
	// Numbers are the target-repo issue numbers in ASCENDING RANK order (rank 1
	// first). This is the campaign's queue order.
	Numbers []int
	// Refs mirror Numbers in the campaign `issue:N` ref convention, so the
	// caller can hand them straight to the no-epic item-set resolver.
	Refs []string
	// Excluded records every ordering entry that did NOT become an issue
	// number, each with a NAMED reason. Never silently dropped: a
	// silently-truncated batch is exactly the failure this surface exists to
	// prevent.
	Excluded []GroomingOrderExclusion
	// Limit is the applied cap (0 = uncapped), and OmittedByLimit is the count
	// of CONVERTIBLE entries the cap dropped. The two are carried separately
	// from Excluded precisely so the omitted-by-limit count is derivable: a
	// capped convertible entry is NOT an exclusion (it was convertible), and
	// conflating the two would make this number uncomputable (K4).
	Limit          int
	OmittedByLimit int
	// SupersededBy is set ONLY when the caller explicitly acknowledged a NAMED
	// newer approved grooming run via allow_superseded. Nil means the order was
	// not superseded (or supersession was refused before this point).
	//
	// There is no companion "undetermined" field: a supersession scan that could
	// not PROVE absence refuses the create unconditionally, so no resolved order
	// can carry that state.
	SupersededBy *uuid.UUID
}

// GroomingOrderExclusion is one ordering entry that could not be converted
// into a target-repo issue number, with the reason it was excluded.
type GroomingOrderExclusion struct {
	// Ref is the report's item_ref id (its provider-native coordinates).
	Ref string `json:"ref"`
	// Type is the item_ref type discriminator (github_issue, gitlab_issue, …).
	Type string `json:"type"`
	// Rank is the entry's rank in the report.
	Rank int `json:"rank"`
	// Reason is one of the named exclusion reasons below.
	Reason string `json:"reason"`
}

// Named exclusion reasons. Every excluded ordering entry carries exactly one,
// so an operator reading the create response can tell an out-of-repo item from
// a malformed id from a non-GitHub forge item.
const (
	// ExclusionNotGitHubIssue marks an entry whose item_ref.type is not
	// github_issue — a gitlab_issue or jira_issue cannot become a GitHub
	// campaign item.
	ExclusionNotGitHubIssue = "not_github_issue"
	// ExclusionOtherRepo marks a github_issue whose owner/repo is not the
	// campaign's target repo.
	ExclusionOtherRepo = "other_repo"
	// ExclusionUnparseableID marks a github_issue whose id does not parse as
	// `<owner>/<repo>#<number>`.
	ExclusionUnparseableID = "unparseable_id"
)

// GroomingOrderError is the typed fail-closed error OrderFromReport and
// ReorderByPriority return. Code is a stable machine token the create handler
// maps to a documented API error code without string-parsing the message.
type GroomingOrderError struct {
	Code    string
	Message string
}

func (e *GroomingOrderError) Error() string { return e.Message }

// GroomingOrderError codes.
const (
	// GroomingOrderErrEmpty means no ordering entry converted into a
	// target-repo issue number, so there is nothing to build a campaign from.
	GroomingOrderErrEmpty = "empty"
	// GroomingOrderErrDuplicate means two ordering entries resolved to the SAME
	// issue number, which would seed two campaign items for one issue.
	GroomingOrderErrDuplicate = "duplicate"
	// GroomingOrderErrSetMismatch means the resolved children and the requested
	// order are not exactly the same set, so the permutation would silently
	// drop or invent an item.
	GroomingOrderErrSetMismatch = "set_mismatch"
)

// ErrGroomingOrder is the umbrella sentinel every *GroomingOrderError wraps,
// so a caller can errors.Is against it without depending on the code.
var ErrGroomingOrder = errors.New("campaign: grooming order")

// Unwrap makes errors.Is(err, ErrGroomingOrder) hold.
func (*GroomingOrderError) Unwrap() error { return ErrGroomingOrder }

// OrderFromReport maps an approved grooming report's Ordering onto the
// rank-ordered set of target-repo issue numbers a campaign can be built from.
// It is PURE: no I/O, no provider, no clock — the same two-layer split
// grooming_apply.go uses — so the report→order mapping is unit-testable on its
// own.
//
// Contract:
//   - entries are sorted ASCENDING by Rank (the grooming-report-v1 schema
//     already enforces ranks are exactly the permutation 1..N, so the sort is
//     total and deterministic); a tie is broken by item_ref id so the result
//     is deterministic even for a hand-written report the schema never saw;
//   - an entry is CONVERTIBLE only when item_ref.type == "github_issue" AND its
//     id parses as `<owner>/<repo>#<number>` with owner/repo matching the
//     target CASE-INSENSITIVELY (GitHub owner/repo names are case-insensitive);
//   - every non-convertible entry is EXCLUDED with a named reason, never
//     silently dropped;
//   - two entries resolving to the same issue number fail closed
//     (GroomingOrderErrDuplicate);
//   - limit > 0 keeps the first `limit` CONVERTIBLE entries by rank and records
//     the rest in OmittedByLimit; limit <= 0 means no cap;
//   - an empty convertible set fails closed (GroomingOrderErrEmpty) rather than
//     persisting an item-less campaign.
func OrderFromReport(report *plan.GroomingReport, owner, name string, limit int) (*GroomingOrder, error) {
	if report == nil {
		return nil, &GroomingOrderError{Code: GroomingOrderErrEmpty, Message: "campaign: nil grooming report"}
	}

	entries := make([]plan.OrderingEntry, len(report.Ordering))
	copy(entries, report.Ordering)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Rank != entries[j].Rank {
			return entries[i].Rank < entries[j].Rank
		}
		return entries[i].ItemRef.ID < entries[j].ItemRef.ID
	})

	order := &GroomingOrder{Limit: limit}
	seen := make(map[int]string, len(entries))
	for _, e := range entries {
		number, reason := convertOrderingRef(e.ItemRef, owner, name)
		if reason != "" {
			order.Excluded = append(order.Excluded, GroomingOrderExclusion{
				Ref:    e.ItemRef.ID,
				Type:   e.ItemRef.Type,
				Rank:   e.Rank,
				Reason: reason,
			})
			continue
		}
		if prior, dup := seen[number]; dup {
			return nil, &GroomingOrderError{
				Code: GroomingOrderErrDuplicate,
				Message: fmt.Sprintf("campaign: grooming order names issue #%d twice (%s and %s); one issue cannot seed two campaign items",
					number, prior, e.ItemRef.ID),
			}
		}
		seen[number] = e.ItemRef.ID
		// The cap is applied AFTER convertibility so `limit` means "the top N
		// items I can actually campaign", not "the top N report rows". The
		// dropped remainder is counted, not excluded: it was convertible, and
		// K4's omitted-by-limit count must be derivable from the result alone.
		if limit > 0 && len(order.Numbers) >= limit {
			order.OmittedByLimit++
			continue
		}
		order.Numbers = append(order.Numbers, number)
		order.Refs = append(order.Refs, issueRef(number))
	}

	if len(order.Numbers) == 0 {
		return nil, &GroomingOrderError{
			Code: GroomingOrderErrEmpty,
			Message: fmt.Sprintf("campaign: the grooming order names no issue in %s/%s (%d ordering entries, all excluded)",
				owner, name, len(entries)),
		}
	}
	return order, nil
}

// convertOrderingRef maps one ordering entry's item_ref onto a target-repo
// issue number, or returns a named exclusion reason. It never returns both.
func convertOrderingRef(ref plan.ItemRef, owner, name string) (int, string) {
	if ref.Type != "github_issue" {
		return 0, ExclusionNotGitHubIssue
	}
	// `<owner>/<repo>#<number>` — normatively stated by
	// grooming-report-v1.schema.json's item-ref.id description for
	// github_issue, whose character class admits '/' and '#'.
	slash := strings.IndexByte(ref.ID, '/')
	hash := strings.IndexByte(ref.ID, '#')
	if slash <= 0 || hash <= slash+1 || hash == len(ref.ID)-1 {
		return 0, ExclusionUnparseableID
	}
	number, err := strconv.Atoi(ref.ID[hash+1:])
	if err != nil || number <= 0 {
		return 0, ExclusionUnparseableID
	}
	if !strings.EqualFold(ref.ID[:slash], owner) || !strings.EqualFold(ref.ID[slash+1:hash], name) {
		return 0, ExclusionOtherRepo
	}
	return number, ""
}

// ReorderByPriority returns a COPY of res whose Children are permuted into
// `numbers` order. It is what makes the ratified rank order the order campaign
// items are CREATED in — and therefore, via the durable
// campaign_items.queue_position column, the order campaign.ListItems returns them and
// the engine's Eligible slice works.
//
// It fails closed with GroomingOrderErrSetMismatch when the two sets are not
// EXACTLY equal in either direction (an ordered number the provider did not
// resolve, or a resolved child the order does not name): a mismatch would
// silently drop or duplicate an item.
//
// Edges and DroppedEdges are carried through UNTOUCHED. Assemble builds its
// child-index map from the actual Children order (indexOf is populated by
// iteration, not by assuming ascending numbers) and translates wave indices
// back through that same map, so permuting Children changes only the item
// CREATION order, never the DAG.
func ReorderByPriority(res *workmgmt.EpicChildrenResult, numbers []int) (*workmgmt.EpicChildrenResult, error) {
	if res == nil {
		return nil, &GroomingOrderError{Code: GroomingOrderErrSetMismatch, Message: "campaign: nil epic-children result to reorder"}
	}
	byNumber := make(map[int]workmgmt.EpicChild, len(res.Children))
	for _, c := range res.Children {
		byNumber[c.Number] = c
	}
	if len(byNumber) != len(res.Children) {
		return nil, &GroomingOrderError{
			Code:    GroomingOrderErrSetMismatch,
			Message: "campaign: the resolved children contain a duplicate issue number",
		}
	}

	children := make([]workmgmt.EpicChild, 0, len(numbers))
	claimed := make(map[int]bool, len(numbers))
	var missing []string
	for _, n := range numbers {
		c, ok := byNumber[n]
		if !ok {
			missing = append(missing, issueRef(n))
			continue
		}
		if claimed[n] {
			return nil, &GroomingOrderError{
				Code:    GroomingOrderErrSetMismatch,
				Message: fmt.Sprintf("campaign: the grooming order names %s twice", issueRef(n)),
			}
		}
		claimed[n] = true
		children = append(children, c)
	}
	var unordered []string
	for _, c := range res.Children {
		if !claimed[c.Number] {
			unordered = append(unordered, issueRef(c.Number))
		}
	}
	if len(missing) > 0 || len(unordered) > 0 {
		return nil, &GroomingOrderError{
			Code: GroomingOrderErrSetMismatch,
			Message: fmt.Sprintf("campaign: the grooming order and the resolved issue set differ (ordered-but-unresolved: %v; resolved-but-unordered: %v)",
				missing, unordered),
		}
	}

	out := *res
	out.Children = children
	return &out, nil
}
