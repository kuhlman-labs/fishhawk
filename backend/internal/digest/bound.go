package digest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultByteBudget is the ONE serialized-size bound every digest surface
// applies (REST at this value; the MCP tool at its resolved response budget):
// the ADR-077 number backend/internal/mcpserver/bound.go already justifies.
const DefaultByteBudget = 32768

// MaxFieldBytes caps the JSON-encoded length of every variable-length string
// field of an item, gap or degradation (headline, category text, detail …),
// so ANY single item fits a budget that fits the floor plus one item
// (#3734 approval condition 1a). A cut field ends in "…" and sets the item's
// FieldsTruncated.
const MaxFieldBytes = 256

// ErrBudgetTooSmall is returned when even the constant-size floor (every
// collection empty but its markers and cursors, plus the one item a section
// call must return) does not fit the budget.
var ErrBudgetTooSmall = errors.New("digest: byte budget below the digest's constant-size floor")

// Bound returns d trimmed so json.Marshal of the WHOLE result is at most
// budget bytes. Size is always MEASURED with the in-progress truncation
// markers attached, never estimated.
//
// Algorithm:
//
//  1. Cap every variable-length field (MaxFieldBytes), marking the cut.
//  2. Keep a FLOOR that is never trimmed: every section's markers and cursors,
//     the sequence-less gaps and degradations (bounded at Build), and — for a
//     single-section or gaps call — that collection's FIRST element, so every
//     cursor-following call returns at least one element or reports the
//     collection complete (condition 1c). A floor over budget is
//     ErrBudgetTooSmall.
//  3. Grow a prefix from the floor in section order (merges,
//     waivers_and_deferrals, pages, open_decisions, then the gaps stream —
//     sequenced gaps and degradations merged by sequence) until the next
//     element would not fit.
//  4. Every collection cut short carries Truncated, its OmittedCount and a
//     cursor whose FromSequence is the FIRST OMITTED element's sequence
//     (condition 1b), whose Call names the exact next request.
//
// Termination comes from the constant-size floor plus the one-element
// guarantee, not from monotonicity of the trim.
func Bound(d Digest, budget int) (Digest, error) {
	d = capDigest(d)
	b := newBounder(d)
	floor := b.floorKeep()
	out, size, err := b.render(floor)
	if err != nil {
		return Digest{}, err
	}
	if size > budget {
		return Digest{}, fmt.Errorf("%w: floor is %d bytes, budget %d", ErrBudgetTooSmall, size, budget)
	}
	// Fast path: everything fits.
	all := b.fullKeep()
	if full, fsize, err := b.render(all); err != nil {
		return Digest{}, err
	} else if fsize <= budget {
		return full, nil
	}
	keep := floor
	for c := 0; c < len(keep); c++ {
		for keep[c] < b.total(c) {
			keep[c]++
			cand, csize, err := b.render(keep)
			if err != nil {
				return Digest{}, err
			}
			if csize > budget {
				return out, nil
			}
			out = cand
		}
	}
	return out, nil
}

// streamEntry is one element of the gaps stream.
type streamEntry struct {
	seq    int64
	gap    *Gap
	degrad *Degradation
}

type bounder struct {
	d Digest
	// fixedGaps/fixedDegs have no sequence: they ride the floor.
	fixedGaps []Gap
	fixedDegs []Degradation
	stream    []streamEntry
}

func newBounder(d Digest) *bounder {
	b := &bounder{d: d}
	for i := range d.Gaps {
		if d.Gaps[i].Sequence == 0 {
			b.fixedGaps = append(b.fixedGaps, d.Gaps[i])
		} else {
			g := d.Gaps[i]
			b.stream = append(b.stream, streamEntry{seq: g.Sequence, gap: &g})
		}
	}
	for i := range d.Degradations {
		if d.Degradations[i].Sequence == 0 {
			b.fixedDegs = append(b.fixedDegs, d.Degradations[i])
		} else {
			dg := d.Degradations[i]
			b.stream = append(b.stream, streamEntry{seq: dg.Sequence, degrad: &dg})
		}
	}
	// Stable merge by sequence: gaps precede degradations at an equal
	// sequence (both were already sorted by Build).
	sortStream(b.stream)
	return b
}

func sortStream(s []streamEntry) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].seq < s[j-1].seq; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Collections are indexed 0..len(Sections)-1 for the sections, and
// len(Sections) for the gaps stream.
func (b *bounder) total(c int) int {
	if c < len(b.d.Sections) {
		return len(b.d.Sections[c].Items)
	}
	return len(b.stream)
}

func (b *bounder) fullKeep() []int {
	k := make([]int, len(b.d.Sections)+1)
	for c := range k {
		k[c] = b.total(c)
	}
	return k
}

func (b *bounder) floorKeep() []int {
	k := make([]int, len(b.d.Sections)+1)
	switch {
	case b.d.Section == SectionGaps:
		k[len(b.d.Sections)] = min(1, len(b.stream))
	case b.d.Section != "":
		for c, s := range b.d.Sections {
			if s.Kind == b.d.Section {
				k[c] = min(1, len(s.Items))
			}
		}
	}
	return k
}

// render materializes the digest keeping keep[c] elements of each collection,
// with every truncation marker attached, and returns its marshalled size.
func (b *bounder) render(keep []int) (Digest, int, error) {
	out := b.d
	out.Sections = make([]Section, len(b.d.Sections))
	for c, s := range b.d.Sections {
		ns := s
		ns.Items = s.Items[:keep[c]]
		if cut := len(s.Items) - keep[c]; cut > 0 {
			ns.Truncated, ns.Complete = true, false
			ns.OmittedCount = s.OmittedCount + cut
			ns.Next = NewCursor(b.d.Repo, s.Kind, s.Items[keep[c]].SourceSequence, b.d.ToSequence)
		}
		out.Sections[c] = ns
	}
	g := keep[len(b.d.Sections)]
	out.Gaps = append([]Gap{}, b.fixedGaps...)
	out.Degradations = append([]Degradation{}, b.fixedDegs...)
	for _, e := range b.stream[:g] {
		if e.gap != nil {
			out.Gaps = append(out.Gaps, *e.gap)
		} else {
			out.Degradations = append(out.Degradations, *e.degrad)
		}
	}
	if cut := len(b.stream) - g; cut > 0 {
		out.GapsTruncated = true
		out.GapsOmittedCount = b.d.GapsOmittedCount + cut
		out.GapsNext = NewCursor(b.d.Repo, SectionGaps, b.stream[g].seq, b.d.ToSequence)
	}
	out.Truncated, out.Next = false, nil
	for _, s := range out.Sections {
		if s.Truncated {
			out.Truncated = true
			if out.Next == nil {
				out.Next = s.Next
			}
		}
	}
	if out.GapsTruncated {
		out.Truncated = true
		if out.Next == nil {
			out.Next = out.GapsNext
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return Digest{}, 0, fmt.Errorf("digest: bound: marshal: %w", err)
	}
	return out, len(raw), nil
}

// capDigest caps every variable-length field, copying every slice it edits so
// the caller's value is never mutated.
func capDigest(d Digest) Digest {
	secs := make([]Section, len(d.Sections))
	for i, s := range d.Sections {
		items := make([]Item, len(s.Items))
		for j, it := range s.Items {
			cut := false
			for _, f := range []*string{&it.Category, &it.SourceEntryHash, &it.StageKind, &it.StageState,
				&it.Outcome, &it.Headline, &it.ConcernCategory, &it.Severity, &it.ReasonKey, &it.AmendmentID} {
				var c bool
				*f, c = capString(*f)
				cut = cut || c
			}
			if cut {
				it.FieldsTruncated = true
			}
			items[j] = it
		}
		s.Items = items
		secs[i] = s
	}
	d.Sections = secs
	gaps := make([]Gap, len(d.Gaps))
	for i, g := range d.Gaps {
		g.Category, _ = capString(g.Category)
		g.Detail, _ = capString(g.Detail)
		gaps[i] = g
	}
	d.Gaps = gaps
	degs := make([]Degradation, len(d.Degradations))
	for i, dg := range d.Degradations {
		dg.Detail, _ = capString(dg.Detail)
		degs[i] = dg
	}
	d.Degradations = degs
	return d
}

// capString cuts s so its JSON encoding is at most MaxFieldBytes, ending the
// cut value in "…". It reports whether it cut.
func capString(s string) (string, bool) {
	if jsonLen(s) <= MaxFieldBytes {
		return s, false
	}
	// Start at a rune boundary no longer than the cap, then shrink until the
	// ENCODED form (escapes included) fits.
	cut := strings.ToValidUTF8(s, "\uFFFD")
	if len(cut) > MaxFieldBytes {
		cut = cut[:MaxFieldBytes]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1] // drop a split trailing rune
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
