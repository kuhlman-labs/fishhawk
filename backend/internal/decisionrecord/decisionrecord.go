// Package decisionrecord selects the repository's decision records that bear
// on a change and assembles them for injection into a reviewer persona's
// prompt (ADR-084 D4(b) / binding rule 4, E78.5 / #3756; ADR-084 has no
// docs/adr record yet, so it is read from the tracker).
//
// The decision record is docs/adr/ in this repository: one file per record
// plus a machine index (docs/adr/index.json, schema adr-index-v1) naming every
// record, its status, and the repository path globs it governs (applies_to).
// repodoc can fetch a named file but cannot list a directory, so the index is
// the entry point: it is fetched at the run's admission commit, parsed
// STRICTLY (ParseIndex), and the records whose applies_to matches the change's
// paths are selected and ranked (Select). Assemble then shapes the index under
// its own per-document cap and fits the FULL text of as many ranked matches as
// a separate record budget allows, so the index cannot starve the records.
//
// The package reads through repodoc and renders through repodoc's framing, so
// base-ref pinning, content hashing, delimiter neutralization and attribution
// are repodoc's — this package adds selection, ranking, budgeting and the
// status-labelled framing. See README.md.
package decisionrecord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// IndexSchemaVersion is the only index schema this package reads.
const IndexSchemaVersion = "adr-index-v1"

// Errors callers switch on with errors.Is.
var (
	// ErrInvalidIndex means the index (or a record it lists) does not satisfy
	// the strict adr-index-v1 contract. The message names the offending record.
	ErrInvalidIndex = errors.New("decisionrecord: invalid decision record index")
	// ErrIndexMissing means the declared index path does not exist at the
	// commit (repodoc.ErrMissingDocument for the index itself).
	ErrIndexMissing = errors.New("decisionrecord: decision record index not found")
	// ErrUnresolvable is every other assembly failure: a record the index
	// lists that is absent at the commit, a fetch or pinning error, or a cap
	// too small to show even the index's truncation marker.
	ErrUnresolvable = errors.New("decisionrecord: decision record could not be resolved")
)

// Status is a record's decision status, as the index states it.
type Status string

// The closed status set (docs/adr/README.md § Field semantics).
const (
	StatusProposed   Status = "proposed"
	StatusAccepted   Status = "accepted"
	StatusRejected   Status = "rejected"
	StatusSuperseded Status = "superseded"
	StatusUnknown    Status = "unknown"
)

// known reports whether s is in the closed status set.
func (s Status) known() bool {
	switch s {
	case StatusProposed, StatusAccepted, StatusRejected, StatusSuperseded, StatusUnknown:
		return true
	}
	return false
}

// idPattern is the record id grammar. {3,} rather than {3}: the record set
// may outgrow three digits, and a wider id is still unambiguous.
var idPattern = regexp.MustCompile(`^ADR-[0-9]{3,}$`)

// Record is one index entry. Amends / AmendedBy are OPTIONAL keys (E78.13 /
// #4471): an absent key decodes as empty, exactly like an empty list.
type Record struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Path         string   `json:"path"`
	Status       Status   `json:"status"`
	Supersedes   []string `json:"supersedes"`
	SupersededBy []string `json:"superseded_by"`
	Amends       []string `json:"amends,omitempty"`
	AmendedBy    []string `json:"amended_by,omitempty"`
	AppliesTo    []string `json:"applies_to"`
}

// linkField is one of a record's link lists, by its front-matter key.
type linkField struct {
	field string
	refs  []string
}

// linkFields are a record's four link lists, in the order ParseIndex and
// checkLinks check them.
func (r Record) linkFields() []linkField {
	return []linkField{{"supersedes", r.Supersedes}, {"superseded_by", r.SupersededBy}, {"amends", r.Amends}, {"amended_by", r.AmendedBy}}
}

// Index is a parsed adr-index-v1 document.
type Index struct {
	SchemaVersion string   `json:"schema_version"`
	Records       []Record `json:"records"`
}

// ParseIndex decodes raw as a STRICT adr-index-v1 index. It refuses: malformed
// JSON or trailing data; an unknown field at any level; a schema_version other
// than adr-index-v1; a record whose id does not match ^ADR-[0-9]{3,}$, whose
// path fails repodoc.ValidatePath, whose status is outside the closed set,
// whose supersedes / superseded_by / amends / amended_by carries a malformed
// id, or whose applies_to carries a glob spec.Predicate refuses; a duplicate id
// or path; a link id that names no record in the index, or that names the
// record itself (docs/adr/README.md: "Every id must name an existing record
// other than itself"); and an id one record lists in BOTH supersedes and
// amends, or in both superseded_by and amended_by (a record cannot both
// supersede and amend the same ADR). The optional amends / amended_by keys may
// be absent: absent is empty. Every refusal wraps ErrInvalidIndex and names the
// record.
//
// Strict on purpose: the index decides which governance text a reviewer is
// shown, so a field this package does not understand is refused rather than
// ignored, and a record path is validated BEFORE it is ever fetched.
func ParseIndex(raw []byte) (*Index, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var idx Index
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the index object", ErrInvalidIndex)
	}
	if idx.SchemaVersion != IndexSchemaVersion {
		return nil, fmt.Errorf("%w: schema_version %q is not %q", ErrInvalidIndex, idx.SchemaVersion, IndexSchemaVersion)
	}
	ids := make(map[string]int, len(idx.Records))
	paths := make(map[string]int, len(idx.Records))
	for i, r := range idx.Records {
		name := fmt.Sprintf("records[%d] (id %q)", i, r.ID)
		if !idPattern.MatchString(r.ID) {
			return nil, fmt.Errorf("%w: %s: id does not match %s", ErrInvalidIndex, name, idPattern)
		}
		if err := repodoc.ValidatePath(r.Path); err != nil {
			return nil, fmt.Errorf("%w: %s: path %q: %v", ErrInvalidIndex, name, r.Path, err)
		}
		if !r.Status.known() {
			return nil, fmt.Errorf("%w: %s: status %q is not one of proposed, accepted, rejected, superseded, unknown", ErrInvalidIndex, name, r.Status)
		}
		for _, link := range r.linkFields() {
			for j, ref := range link.refs {
				if !idPattern.MatchString(ref) {
					return nil, fmt.Errorf("%w: %s: %s[%d] %q does not match %s", ErrInvalidIndex, name, link.field, j, ref, idPattern)
				}
			}
		}
		if len(r.AppliesTo) > 0 {
			if err := (spec.Predicate{Paths: r.AppliesTo}).Validate(fmt.Sprintf("/records/%d/applies_to", i)); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrInvalidIndex, name, err)
			}
		}
		if prev, dup := ids[r.ID]; dup {
			return nil, fmt.Errorf("%w: %s: duplicate id (also records[%d])", ErrInvalidIndex, name, prev)
		}
		ids[r.ID] = i
		if prev, dup := paths[r.Path]; dup {
			return nil, fmt.Errorf("%w: %s: duplicate path %q (also records[%d])", ErrInvalidIndex, name, r.Path, prev)
		}
		paths[r.Path] = i
	}
	if err := checkLinks(&idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// checkLinks refuses a supersedes / superseded_by / amends / amended_by id
// that names no record in idx, or that names the record carrying it, and an id
// one record lists in both supersedes and amends (or in both superseded_by and
// amended_by): supersession and amendment are mutually exclusive per id. It
// runs after the per-record pass, so every id it looks up is well-formed and
// unique.
func checkLinks(idx *Index) error {
	ids := make(map[string]bool, len(idx.Records))
	for _, r := range idx.Records {
		ids[r.ID] = true
	}
	for i, r := range idx.Records {
		for _, link := range r.linkFields() {
			for j, ref := range link.refs {
				if ref == r.ID {
					return fmt.Errorf("%w: records[%d] (id %q): %s[%d] %q names the record itself", ErrInvalidIndex, i, r.ID, link.field, j, ref)
				}
				if !ids[ref] {
					return fmt.Errorf("%w: records[%d] (id %q): %s[%d] %q names no record in the index", ErrInvalidIndex, i, r.ID, link.field, j, ref)
				}
			}
		}
		for _, pair := range [][2]linkField{
			{{"supersedes", r.Supersedes}, {"amends", r.Amends}},
			{{"superseded_by", r.SupersededBy}, {"amended_by", r.AmendedBy}},
		} {
			for _, ref := range pair[1].refs {
				if slices.Contains(pair[0].refs, ref) {
					return fmt.Errorf("%w: records[%d] (id %q): %q is listed in both %s and %s: a record cannot both supersede and amend the same record",
						ErrInvalidIndex, i, r.ID, ref, pair[0].field, pair[1].field)
				}
			}
		}
	}
	return nil
}

// Match is one selected record and where it ranked.
type Match struct {
	Record Record
	// Rank is the 1-based position in the selection's ranking.
	Rank int
	// MatchedPaths is how many DISTINCT change paths the record's OWN
	// applies_to matched; 0 for a record selected only as a successor or an
	// amendment.
	MatchedPaths int
	// RankPaths is the ranking key: the larger of MatchedPaths and the
	// RankPaths of every selected record this one supersedes or amends, so a
	// successor or an amendment ranks with the record it replaces or amends.
	RankPaths int
	// Via are the ids of the SELECTED records this record supersedes — the
	// selected records whose supersession link pulled it in — sorted
	// ascending. Empty for a record no selected record supersedes into.
	Via []string
	// AmendsVia are the ids of the SELECTED records this record amends — the
	// selected records whose amendment link pulled it in — sorted ascending.
	// Empty for a record that amends no selected record.
	AmendsVia []string
}

// Select returns the records of idx whose applies_to matches at least one of
// changePaths, plus every record that supersedes or amends a selected one,
// ranked. A
// record with an EMPTY applies_to never matches directly — it governs no named
// path — and is skipped before spec.Predicate.Match, which refuses an empty
// predicate. Matching is spec.Predicate{Paths: applies_to} against each
// distinct change path; a Match error fails the WHOLE selection
// (ErrInvalidIndex) rather than reading as a non-match.
//
// Supersession and amendment are followed from each directly matched record
// to its successors and amendments, transitively (followLinks): a successor or
// an amendment is selected even when its own applies_to is empty, so a
// reviewer shown a record is also shown what replaced or amended it.
// Predecessors and amended records are never followed, and a link to an id
// absent from idx fails the WHOLE selection (ErrInvalidIndex).
//
// Ranking is deterministic: accepted records first; then the higher RankPaths
// (distinct change paths matched, inherited by a successor or an amendment
// from the records it supersedes or amends); then the higher record number
// (newer) first; then id ascending. On top of that order, an amendment never
// ranks ahead of a selected record it amends (placeAmendmentsAfterAmended):
// the amended record still governs, so under the strict-prefix record budget
// its body is never dropped while an amendment of it is shown.
func Select(idx *Index, changePaths []string) ([]Match, error) {
	if idx == nil {
		return nil, fmt.Errorf("%w: no index", ErrInvalidIndex)
	}
	distinct := dedupe(changePaths)
	var direct []Match
	for i, r := range idx.Records {
		if len(r.AppliesTo) == 0 {
			continue
		}
		pred := spec.Predicate{Paths: r.AppliesTo}
		n := 0
		for _, p := range distinct {
			ok, err := pred.Match(spec.Change{Paths: []string{p}})
			if err != nil {
				return nil, fmt.Errorf("%w: records[%d] (id %q): %v", ErrInvalidIndex, i, r.ID, err)
			}
			if ok {
				n++
			}
		}
		if n > 0 {
			direct = append(direct, Match{Record: r, MatchedPaths: n, RankPaths: n})
		}
	}
	out, err := followLinks(idx, direct)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return rankBefore(out[i], out[j]) })
	out = placeAmendmentsAfterAmended(out)
	for i := range out {
		out[i].Rank = i + 1
	}
	return out, nil
}

// linkEdge is one followed link: the record it reaches and whether it is an
// amendment edge (else a successor edge).
type linkEdge struct {
	to        string
	amendment bool
}

// followLinks returns direct plus every record reachable from a direct match
// through supersession or amendment, each record once. The successor relation
// is the union of r.SupersededBy and {R.ID : r.ID in R.Supersedes}, and the
// amendment relation the union of r.AmendedBy and {R.ID : r.ID in R.Amends},
// so a hand-built index whose links are not reciprocal is still followed. Each
// walk is seeded from ONE direct match with its own visited set and traverses
// both edge kinds, so a cycle terminates and a mixed chain (an amendment of a
// successor) is followed; every record it reaches has its RankPaths raised to
// the root's, and every edge it traverses adds the (selected) linking record to
// the reached record's Via (successor edge) or AmendsVia (amendment edge). A
// superseded_by / amended_by id naming no record fails the walk: a hand-built
// index can reach Select without ParseIndex.
func followLinks(idx *Index, direct []Match) ([]Match, error) {
	if len(direct) == 0 {
		return direct, nil
	}
	byID := make(map[string]int, len(idx.Records))
	for i, r := range idx.Records {
		if _, dup := byID[r.ID]; !dup {
			byID[r.ID] = i
		}
	}
	edges := make(map[string][]linkEdge)
	addEdge := func(from, to string, amendment bool) {
		e := linkEdge{to: to, amendment: amendment}
		if !slices.Contains(edges[from], e) {
			edges[from] = append(edges[from], e)
		}
	}
	for _, r := range idx.Records {
		for _, s := range r.SupersededBy {
			addEdge(r.ID, s, false)
		}
		for _, p := range r.Supersedes {
			addEdge(p, r.ID, false)
		}
		for _, a := range r.AmendedBy {
			addEdge(r.ID, a, true)
		}
		for _, p := range r.Amends {
			addEdge(p, r.ID, true)
		}
	}
	out := slices.Clone(direct)
	pos := make(map[string]int, len(out))
	for i, m := range out {
		pos[m.Record.ID] = i
	}
	for _, root := range direct {
		visited := map[string]bool{root.Record.ID: true}
		queue := []string{root.Record.ID}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range edges[cur] {
				ri, ok := byID[e.to]
				if !ok {
					field := "superseded_by"
					if e.amendment {
						field = "amended_by"
					}
					return nil, fmt.Errorf("%w: records[%d] (id %q): %s %q names no record in the index", ErrInvalidIndex, byID[cur], cur, field, e.to)
				}
				j, selected := pos[e.to]
				if !selected {
					out = append(out, Match{Record: idx.Records[ri]})
					j = len(out) - 1
					pos[e.to] = j
				}
				via := &out[j].Via
				if e.amendment {
					via = &out[j].AmendsVia
				}
				if !slices.Contains(*via, cur) {
					*via = append(*via, cur)
				}
				out[j].RankPaths = max(out[j].RankPaths, root.RankPaths)
				if visited[e.to] {
					continue
				}
				visited[e.to] = true
				queue = append(queue, e.to)
			}
		}
	}
	for i := range out {
		sort.Strings(out[i].Via)
		sort.Strings(out[i].AmendsVia)
	}
	return out, nil
}

// placeAmendmentsAfterAmended reorders ranked so that no record precedes a
// selected record it amends (its AmendsVia), otherwise keeping the ranked
// order: it repeatedly takes the highest-ranked remaining record whose
// AmendsVia records are all already placed. An amendment cycle (a hand-built
// index where two records amend each other) leaves no eligible record; the
// highest-ranked remaining one is then taken anyway, so the pass terminates
// and stays deterministic.
func placeAmendmentsAfterAmended(ranked []Match) []Match {
	placed := make(map[string]bool, len(ranked))
	remaining := slices.Clone(ranked)
	out := make([]Match, 0, len(ranked))
	for len(remaining) > 0 {
		pick := 0
		for i, m := range remaining {
			if !slices.ContainsFunc(m.AmendsVia, func(id string) bool { return !placed[id] }) {
				pick = i
				break
			}
		}
		out = append(out, remaining[pick])
		placed[remaining[pick].Record.ID] = true
		remaining = slices.Delete(remaining, pick, pick+1)
	}
	return out
}

// rankBefore is the ranking comparator Select documents.
func rankBefore(a, b Match) bool {
	if aa, ba := a.Record.Status == StatusAccepted, b.Record.Status == StatusAccepted; aa != ba {
		return aa
	}
	if a.RankPaths != b.RankPaths {
		return a.RankPaths > b.RankPaths
	}
	if c := compareIDNumber(a.Record.ID, b.Record.ID); c != 0 {
		return c > 0
	}
	return a.Record.ID < b.Record.ID
}

// compareIDNumber compares the numeric part of two ADR ids without integer
// conversion (so an arbitrarily long number cannot overflow): leading zeros
// stripped, then a longer number is larger, then lexical order. Returns -1, 0
// or 1.
func compareIDNumber(a, b string) int {
	na := strings.TrimLeft(strings.TrimPrefix(a, "ADR-"), "0")
	nb := strings.TrimLeft(strings.TrimPrefix(b, "ADR-"), "0")
	if len(na) != len(nb) {
		if len(na) < len(nb) {
			return -1
		}
		return 1
	}
	return strings.Compare(na, nb)
}

// dedupe returns the distinct values of in, in first-seen order.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
