package handoverbrief

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// DefaultByteBudget is the serialized-size bound REST applies (the MCP tool
// re-bounds at its session budget): the digest's ADR-077 number.
const DefaultByteBudget = digest.DefaultByteBudget

// MaxFieldBytes caps the JSON-encoded length of every variable-length string
// field of an element, so any single element fits a budget that fits the
// floor plus one element. A cut field ends in "…" and sets FieldsTruncated.
const MaxFieldBytes = digest.MaxFieldBytes

// ErrBudgetTooSmall is returned when the constant-size floor does not fit.
var ErrBudgetTooSmall = errors.New("handoverbrief: byte budget below the brief's constant-size floor")

// Bound returns b trimmed so json.Marshal of the WHOLE result is at most
// budget bytes, measured with every truncation marker attached. It mirrors
// digest.Bound:
//
//  1. Cap every variable-length string field (MaxFieldBytes).
//  2. Keep a FLOOR that is never trimmed: every section's and part's markers,
//     standing orders, the declared absences, the sequence-less gaps and
//     degradations, and — for a single-section brief — that section's first
//     element. A floor over budget is ErrBudgetTooSmall, never an over-budget
//     or empty non-advancing value.
//  3. Grow a prefix from the floor in section/part order, then the gaps
//     stream (sequenced gaps and degradations merged by sequence).
//  4. Every collection cut short carries Truncated, OmittedCount and a Cursor
//     whose Call names the exact underlying query (the digest section, the
//     campaigns/runs list, the delegation read) at the first omitted element.
//
// BriefHash is copied through untouched: a bounded render reports the hash
// of the canonical, unbounded brief and is NEVER re-hashed.
func Bound(b Brief, budget int) (Brief, error) {
	b = capBrief(b)
	bd := newBounder(b)
	floor := bd.floorKeep()
	out, size, err := bd.render(floor)
	if err != nil {
		return Brief{}, err
	}
	if size > budget {
		return Brief{}, fmt.Errorf("%w: floor is %d bytes, budget %d", ErrBudgetTooSmall, size, budget)
	}
	if full, fsize, err := bd.render(bd.fullKeep()); err != nil {
		return Brief{}, err
	} else if fsize <= budget {
		return full, nil
	}
	keep := floor
	for c := range keep {
		for keep[c] < bd.total(c) {
			keep[c]++
			cand, csize, err := bd.render(keep)
			if err != nil {
				return Brief{}, err
			}
			if csize > budget {
				return out, nil
			}
			out = cand
		}
	}
	return out, nil
}

// partRef addresses one part collection.
type partRef struct{ sec, part int }

type streamEntry struct {
	seq    int64
	gap    *digest.Gap
	degrad *Degradation
}

type bounder struct {
	b         Brief
	parts     []partRef
	fixedGaps []digest.Gap
	fixedDegs []Degradation
	stream    []streamEntry
}

func newBounder(b Brief) *bounder {
	bd := &bounder{b: b}
	for i, s := range b.Sections {
		for j := range s.Parts {
			bd.parts = append(bd.parts, partRef{i, j})
		}
	}
	for i := range b.Gaps {
		if g := b.Gaps[i]; g.Sequence == 0 {
			bd.fixedGaps = append(bd.fixedGaps, g)
		} else {
			bd.stream = append(bd.stream, streamEntry{seq: g.Sequence, gap: &g})
		}
	}
	for i := range b.Degradations {
		if dg := b.Degradations[i]; dg.Sequence == 0 {
			bd.fixedDegs = append(bd.fixedDegs, dg)
		} else {
			bd.stream = append(bd.stream, streamEntry{seq: dg.Sequence, degrad: &dg})
		}
	}
	// Stable insertion sort by sequence: gaps precede degradations at an
	// equal sequence.
	s := bd.stream
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].seq < s[j-1].seq; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return bd
}

func (bd *bounder) part(c int) Part {
	r := bd.parts[c]
	return bd.b.Sections[r.sec].Parts[r.part]
}

func partLen(p Part) int {
	switch p.Kind {
	case PartCampaigns, PartRuns:
		return len(p.InFlight)
	case PartWorkflows:
		return len(p.Workflows)
	}
	return len(p.Items)
}

// Collections are 0..len(parts)-1 for the parts and len(parts) for the gaps
// stream.
func (bd *bounder) total(c int) int {
	if c < len(bd.parts) {
		return partLen(bd.part(c))
	}
	return len(bd.stream)
}

func (bd *bounder) fullKeep() []int {
	k := make([]int, len(bd.parts)+1)
	for c := range k {
		k[c] = bd.total(c)
	}
	return k
}

func (bd *bounder) floorKeep() []int {
	k := make([]int, len(bd.parts)+1)
	if bd.b.Section == "" {
		return k
	}
	for c := range bd.parts {
		if n := bd.total(c); n > 0 {
			k[c] = 1
			break
		}
	}
	return k
}

// cursorAt names the underlying query for p's element at index i.
func (bd *bounder) cursorAt(p Part, i int) *Cursor {
	repo, to := bd.b.Repo, bd.b.Window.ToSequence
	switch p.Kind {
	case PartCampaigns, PartRuns:
		it := p.InFlight[i]
		off := 0
		for _, prev := range p.InFlight[:i] {
			if prev.State == it.State {
				off++
			}
		}
		return listCursor(repo, p.Kind, it.State, off)
	case PartWorkflows:
		return delegationCursor(repo, delegationSource(bd.b), i)
	}
	dsec := map[PartKind]digest.SectionKind{
		PartMerges: digest.SectionMerges, PartWaiversAndDeferrals: digest.SectionWaiversAndDeferrals,
		PartUnansweredPages: digest.SectionPages, PartOpenDecisions: digest.SectionOpenDecisions,
	}[p.Kind]
	return fromDigestCursor(p.Kind, digest.NewCursor(repo, dsec, p.Items[i].SourceSequence, to))
}

func truncatePart(p Part, n int) Part {
	switch p.Kind {
	case PartCampaigns, PartRuns:
		p.InFlight = p.InFlight[:n]
	case PartWorkflows:
		p.Workflows = p.Workflows[:n]
	default:
		p.Items = p.Items[:n]
	}
	return p
}

// render materializes the brief keeping keep[c] elements of each collection,
// with every truncation marker attached, and returns its marshalled size.
func (bd *bounder) render(keep []int) (Brief, int, error) {
	out := bd.b
	out.Sections = make([]Section, len(bd.b.Sections))
	for i, s := range bd.b.Sections {
		ns := s
		ns.Parts = make([]Part, len(s.Parts))
		copy(ns.Parts, s.Parts)
		out.Sections[i] = ns
	}
	for c, r := range bd.parts {
		p := bd.part(c)
		np := truncatePart(p, keep[c])
		if cut := partLen(p) - keep[c]; cut > 0 {
			np.Truncated, np.Complete = true, false
			np.OmittedCount = p.OmittedCount + cut
			np.Next = bd.cursorAt(p, keep[c])
		}
		out.Sections[r.sec].Parts[r.part] = np
	}
	g := keep[len(bd.parts)]
	out.Gaps = append([]digest.Gap{}, bd.fixedGaps...)
	out.Degradations = append([]Degradation{}, bd.fixedDegs...)
	for _, e := range bd.stream[:g] {
		if e.gap != nil {
			out.Gaps = append(out.Gaps, *e.gap)
		} else {
			out.Degradations = append(out.Degradations, *e.degrad)
		}
	}
	if cut := len(bd.stream) - g; cut > 0 {
		out.GapsTruncated = true
		out.GapsOmittedCount = bd.b.GapsOmittedCount + cut
		// The stream's first omitted element is continued by the digest's
		// gaps query at its sequence.
		out.GapsNext = fromDigestCursor(PartGaps, digest.NewCursor(bd.b.Repo, digest.SectionGaps, bd.stream[g].seq, bd.b.Window.ToSequence))
	}
	finalize(&out)
	raw, err := json.Marshal(out)
	if err != nil {
		return Brief{}, 0, fmt.Errorf("handoverbrief: bound: marshal: %w", err)
	}
	return out, len(raw), nil
}

// capBrief caps every variable-length string field, copying every slice it
// edits so the caller's value is never mutated.
func capBrief(b Brief) Brief {
	secs := make([]Section, len(b.Sections))
	for i, s := range b.Sections {
		parts := make([]Part, len(s.Parts))
		for j, p := range s.Parts {
			if p.Items != nil {
				items := make([]digest.Item, len(p.Items))
				for k, it := range p.Items {
					if capAll(&it.Category, &it.SourceEntryHash, &it.StageKind, &it.StageState, &it.Outcome,
						&it.Headline, &it.ConcernCategory, &it.Severity, &it.ReasonKey, &it.AmendmentID) {
						it.FieldsTruncated = true
					}
					items[k] = it
				}
				p.Items = items
			}
			if p.InFlight != nil {
				fl := make([]InFlightItem, len(p.InFlight))
				for k, it := range p.InFlight {
					if capAll(&it.Kind, &it.State, &it.Ref, &it.WorkflowID) {
						it.FieldsTruncated = true
					}
					fl[k] = it
				}
				p.InFlight = fl
			}
			if p.Workflows != nil {
				ws := make([]DelegationEntry, len(p.Workflows))
				for k, e := range p.Workflows {
					if capAll(&e.WorkflowID, &e.Autonomy) {
						e.FieldsTruncated = true
					}
					ws[k] = e
				}
				p.Workflows = ws
			}
			parts[j] = p
		}
		s.Parts = parts
		secs[i] = s
	}
	b.Sections = secs
	gaps := make([]digest.Gap, len(b.Gaps))
	for i, g := range b.Gaps {
		capAll(&g.Category, &g.Detail)
		gaps[i] = g
	}
	b.Gaps = gaps
	degs := make([]Degradation, len(b.Degradations))
	for i, dg := range b.Degradations {
		capAll(&dg.Detail)
		degs[i] = dg
	}
	b.Degradations = degs
	return b
}

func capAll(fields ...*string) bool {
	cut := false
	for _, f := range fields {
		var c bool
		*f, c = capString(*f)
		cut = cut || c
	}
	return cut
}

// capString cuts s so its JSON encoding is at most MaxFieldBytes, ending the
// cut value in "…". It reports whether it cut.
func capString(s string) (string, bool) {
	if jsonLen(s) <= MaxFieldBytes {
		return s, false
	}
	cut := strings.ToValidUTF8(s, "�")
	if len(cut) > MaxFieldBytes {
		cut = cut[:MaxFieldBytes]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
	}
	for len(cut) > 0 && jsonLen(cut+"…") > MaxFieldBytes {
		_, size := utf8.DecodeLastRuneInString(cut)
		cut = cut[:len(cut)-size]
	}
	return cut + "…", true
}

func jsonLen(s string) int {
	b, _ := json.Marshal(s)
	return len(b)
}
