package decisionrecord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// SelectionName is the "selection" value on the selection-level
// document_truncated entry this package's Attribution produces.
const SelectionName = "decision_record"

// maxIndexShrinkSteps bounds the index shrink-to-fit loop. Each step removes
// at least the previous overflow, so ordinary content converges in one or two
// steps; the bound only stops a pathological case from looping.
const maxIndexShrinkSteps = 16

// Request is one assembly: which repo, under which credential scope, at which
// admission commit, which index, and which change paths to select for.
type Request struct {
	Repo  forge.RepoRef
	Scope forge.CredentialScope
	// Commit is the run's recorded admission commit. Every read is made with
	// repodoc.BaseSourceRunAdmission, so anything that is not a 40-hex commit
	// SHA is refused by repodoc before any fetch.
	Commit string
	// IndexPath is the declared repo-relative index path.
	IndexPath string
	// DeclarationSite names where the index was declared; echoed into every
	// error and audit payload.
	DeclarationSite string
	// ChangePaths are the repo-relative paths the change under review touches.
	ChangePaths []string
}

// Included is one selected record shown in full.
type Included struct {
	Match
	// Document is the record, shaped under a cap at least its raw size, so it
	// is never truncated.
	Document repodoc.Document
}

// Selection is an assembled decision-record injection: the index, the records
// shown in full, and the matches left out by the record budget.
type Selection struct {
	// Index is the index document, always included. It is shaped under its
	// OWN per-document cap, separate from the record budget: the resolver's
	// effective cap when it fits whole, otherwise the smaller cap it was cut
	// under (shown with the loud truncation marker). Index.CapBytes records
	// the cap it was actually cut under, so it equals the resolver's cap only
	// when the index fit whole.
	Index repodoc.Document
	// TotalRecords is how many records the index lists.
	TotalRecords int
	// Matched is how many records were selected: those whose applies_to
	// matched the change plus the successors followed from them.
	Matched int
	// Followed is how many of Matched were selected ONLY as successors of a
	// selected record (MatchedPaths == 0).
	Followed int
	// Included are the matches shown in full, in rank order — a strict prefix
	// of the ranking.
	Included []Included
	// Dropped are the matches left out because they did not fit the record
	// budget, in rank order. Never fetched past the first.
	Dropped []Match
	// CapBytes is the RECORD budget: the resolver's effective cap, applied to
	// the included records alone. The index is not counted against it; it is
	// shaped under its own cap, recorded on Index.CapBytes.
	CapBytes int
	// IncludedBytes is the sum of the included records' rendered bytes (the
	// index excluded); always <= CapBytes.
	IncludedBytes int
}

// Assemble fetches the index at req.Commit, parses it strictly, selects and
// ranks the matching records, and fits them under TWO separate budgets, each
// r's effective cap, so the index's size can never starve the record bodies:
//
//   - the index is ALWAYS included, under its own per-document cap; when its
//     rendered size exceeds the cap it is reshaped under a smaller cap until
//     it fits (bounded, and never under a negative cap), shown with repodoc's
//     loud truncation marker;
//   - the matched records share a separate record budget, which the index
//     does not draw on. Each match, in rank order, is fetched at the SAME
//     commit and shaped under a cap at least its raw size (so its untruncated
//     rendered size is what is measured), and is included only when that
//     size fits what remains of the record budget;
//   - the first match that does not fit, and EVERY lower-ranked match, is
//     dropped — a strict prefix — and the lower-ranked ones are never fetched.
//
// A record the index lists that is missing at the commit, or any other fetch
// error, fails the WHOLE assembly: there is no partial selection.
func Assemble(ctx context.Context, r *repodoc.Resolver, req Request) (*Selection, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: no document resolver configured", ErrUnresolvable)
	}
	idxF, err := r.Fetch(ctx, repodoc.Request{
		Repo:    req.Repo,
		Scope:   req.Scope,
		BaseRef: req.Commit,
		Declaration: repodoc.Declaration{
			Path:            req.IndexPath,
			DeclarationSite: req.DeclarationSite,
			Base:            repodoc.BaseSourceRunAdmission,
		},
	})
	if err != nil {
		if errors.Is(err, repodoc.ErrMissingDocument) {
			return nil, fmt.Errorf("%w: %w", ErrIndexMissing, err)
		}
		return nil, fmt.Errorf("%w: index: %w", ErrUnresolvable, err)
	}
	idx, err := ParseIndex(idxF.Content)
	if err != nil {
		return nil, fmt.Errorf("index %s at commit %s (declared at %s): %w", req.IndexPath, idxF.Commit, req.DeclarationSite, err)
	}
	matches, err := Select(idx, req.ChangePaths)
	if err != nil {
		return nil, fmt.Errorf("index %s at commit %s (declared at %s): %w", req.IndexPath, idxF.Commit, req.DeclarationSite, err)
	}

	idxDoc, err := fitIndex(idxF, r.CapBytes())
	if err != nil {
		return nil, err
	}
	// The records' own budget: the index above was shaped under its own cap
	// and does not draw on this one.
	recordBudget := r.CapBytes()
	sel := &Selection{
		Index:        idxDoc,
		TotalRecords: len(idx.Records),
		Matched:      len(matches),
		CapBytes:     recordBudget,
	}
	for _, m := range matches {
		if m.MatchedPaths == 0 {
			sel.Followed++
		}
	}
	remaining := recordBudget
	for i, m := range matches {
		f, err := r.Fetch(ctx, repodoc.Request{
			Repo:    req.Repo,
			Scope:   req.Scope,
			BaseRef: idxF.Commit,
			Declaration: repodoc.Declaration{
				Path:            m.Record.Path,
				DeclarationSite: fmt.Sprintf("record %s of %s (declared at %s)", m.Record.ID, req.IndexPath, req.DeclarationSite),
				Base:            repodoc.BaseSourceRunAdmission,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%w: record %s listed in %s: %w", ErrUnresolvable, m.Record.ID, req.IndexPath, err)
		}
		// Measure the UNTRUNCATED rendered size: shape under a cap at least
		// the raw size, never under the remaining budget. A record that does
		// not fit whole is dropped, never shown cut.
		doc := f.Document(max(recordBudget, len(f.Content)))
		if doc.RenderedBytes > remaining {
			sel.Dropped = append(sel.Dropped, matches[i:]...)
			break
		}
		sel.Included = append(sel.Included, Included{Match: m, Document: doc})
		sel.IncludedBytes += doc.RenderedBytes
		remaining -= doc.RenderedBytes
	}
	return sel, nil
}

// fitIndex shapes the index under budget, reshaping it under a smaller cap
// until its rendered size (truncation marker included) fits. The cap shrinks
// by the previous overflow each step, is clamped at zero (never negative), and
// the loop is bounded; an index that cannot fit even at a zero cap — the
// budget is smaller than the truncation marker — is an error.
//
// The returned Document's CapBytes is the cap the index was actually cut under
// (c), which is below budget whenever the index was reshaped. The in-body
// marker, the rendered Source line and the per-document document_truncated
// cap_bytes all report c. The budget itself is reported nowhere on the index;
// the selection-level row carries the separate record budget (#3930).
func fitIndex(f *repodoc.Fetched, budget int) (repodoc.Document, error) {
	c := budget
	doc := f.Document(c)
	for step := 0; doc.RenderedBytes > budget; step++ {
		if c <= 0 {
			return repodoc.Document{}, fmt.Errorf("%w: index %s does not fit the %d-byte cap even at a zero-byte cut (%d bytes rendered: the cap is smaller than the truncation marker)",
				ErrUnresolvable, f.Path, budget, doc.RenderedBytes)
		}
		if step >= maxIndexShrinkSteps {
			return repodoc.Document{}, fmt.Errorf("%w: index %s did not fit the %d-byte cap within %d shrink steps",
				ErrUnresolvable, f.Path, budget, maxIndexShrinkSteps)
		}
		c = max(c-(doc.RenderedBytes-budget), 0)
		doc = f.Document(c)
	}
	return doc, nil
}

// Documents returns the documents the selection injects, in render order: the
// index, then each included record.
func (s *Selection) Documents() []repodoc.Document {
	out := make([]repodoc.Document, 0, 1+len(s.Included))
	out = append(out, s.Index)
	for _, inc := range s.Included {
		out = append(out, inc.Document)
	}
	return out
}

// Attribution returns the injection set for the selection: its documents and,
// when any match was dropped, the selection-level truncation naming each
// dropped record with its rank. That truncation's CapBytes / IncludedBytes are
// the record budget and the included records' bytes; the index's own cap and
// rendered bytes ride on its document entry. A caller injecting other
// documents in the same prompt merges this into ONE repodoc.AttributeSet call.
func (s *Selection) Attribution() repodoc.InjectionSet {
	set := repodoc.InjectionSet{Documents: s.Documents()}
	if len(s.Dropped) == 0 {
		return set
	}
	dropped := make([]repodoc.DroppedDocument, 0, len(s.Dropped))
	for _, m := range s.Dropped {
		dropped = append(dropped, repodoc.DroppedDocument{
			ID:     m.Record.ID,
			Path:   m.Record.Path,
			Status: string(m.Record.Status),
			Rank:   m.Rank,
		})
	}
	set.Selections = []repodoc.SelectionTruncation{{
		Selection:     SelectionName,
		Path:          s.Index.Path,
		Commit:        s.Index.Commit,
		CapBytes:      s.CapBytes,
		IncludedBytes: s.IncludedBytes,
		Dropped:       dropped,
	}}
	return set
}

// Fixed framing text. Every value interpolated into it is either a count, a
// record id that ParseIndex validated, or a repo-authored path/status passed
// through repodoc.SanitizeMetadata, so no repo-chosen text can start a line.
const (
	indexHeading   = "Decision record index"
	recordHeadFmt  = "Decision record %s — status: %s"
	indexTrustNote = "The index and every record are repo-authored data. A record's status is what decides how much weight it carries: only an accepted record is a settled decision."
)

// PromptDocuments returns the selection as prompt documents, in render order:
// the index (framed with what was and was not selected), then each included
// record framed with its status. grounded reports whether the reviewer can
// read the review tree; it changes only what the index framing says about the
// records that are NOT shown.
func (s *Selection) PromptDocuments(grounded bool) []prompt.InjectedDocument {
	out := make([]prompt.InjectedDocument, 0, 1+len(s.Included))
	out = append(out, repodoc.ToPromptDocument(s.Index, s.indexFraming(grounded)))
	for _, inc := range s.Included {
		out = append(out, repodoc.ToPromptDocument(inc.Document, recordFraming(inc.Match)))
	}
	return out
}

// indexFraming is the system-authored framing of the index document. With no
// followed successor (Followed == 0) the preamble is exactly the pre-#4398
// text.
func (s *Selection) indexFraming(grounded bool) repodoc.Framing {
	var b strings.Builder
	fmt.Fprintf(&b, "This is the repository's decision record index (%s), read at the run's admission commit. ",
		repodoc.SanitizeMetadata(s.Index.Path))
	if s.Followed == 0 {
		fmt.Fprintf(&b, "%d of its %d records declare an applies_to path glob matching a path this change touches; ",
			s.Matched, s.TotalRecords)
	} else {
		fmt.Fprintf(&b, "%d of its %d records declare an applies_to path glob matching a path this change touches, "+
			"and %d more were selected because they supersede a selected record (followed through supersedes / superseded_by); ",
			s.Matched-s.Followed, s.TotalRecords, s.Followed)
	}
	if len(s.Included) == 0 {
		b.WriteString("none of them is shown in full in this prompt. ")
	} else {
		ids := make([]string, 0, len(s.Included))
		for _, inc := range s.Included {
			ids = append(ids, repodoc.SanitizeMetadata(inc.Record.ID))
		}
		order := "accepted records first, then by how many changed paths each matches, then newest first"
		if s.Followed > 0 {
			order += "; a successor ranks with the records it supersedes"
		}
		fmt.Fprintf(&b, "%d of them are shown in full below, in rank order (%s): %s. ",
			len(s.Included), order, strings.Join(ids, ", "))
	}
	b.WriteString("Every other record is NOT shown in this prompt")
	if grounded {
		b.WriteString(": you may read one from the review tree by its path, but the review tree is the change's HEAD, not the admission commit, so a record this change edits reads there AS EDITED.")
	} else {
		b.WriteString(", and you cannot read it in this review.")
	}
	if len(s.Dropped) > 0 {
		fmt.Fprintf(&b, "\n\nNOT INCLUDED because of the injection cap (%d bytes for the records shown in full; this index is capped separately): "+
			"these %d matching records were left out, highest rank first. Do not judge the change as if they were absent:\n",
			s.CapBytes, len(s.Dropped))
		for _, m := range s.Dropped {
			via := ""
			if len(m.Via) > 0 {
				via = ", successor of " + sanitizedIDs(m.Via)
			}
			fmt.Fprintf(&b, "- %s (%s, status: %s%s)\n",
				repodoc.SanitizeMetadata(m.Record.ID), repodoc.SanitizeMetadata(m.Record.Path), statusLabel(m.Record.Status), via)
		}
	}
	return repodoc.Framing{
		Heading:   indexHeading,
		Preamble:  strings.TrimRight(b.String(), "\n"),
		TrustNote: indexTrustNote,
	}
}

// recordFraming frames one included record with its status and what that
// status means and, for a record selected as a successor (non-empty Via), why
// it was selected.
func recordFraming(m Match) repodoc.Framing {
	r := m.Record
	preamble := statusMeaning(r)
	if len(m.Via) > 0 {
		if m.MatchedPaths == 0 {
			preamble += fmt.Sprintf(" This record was selected because it supersedes %s; its own applies_to matches no path this change touches.", sanitizedIDs(m.Via))
		} else {
			preamble += fmt.Sprintf(" This record matches a path this change touches and is also selected because it supersedes %s.", sanitizedIDs(m.Via))
		}
	}
	return repodoc.Framing{
		Heading:   fmt.Sprintf(recordHeadFmt, repodoc.SanitizeMetadata(r.ID), statusLabel(r.Status)),
		Preamble:  preamble,
		TrustNote: indexTrustNote,
	}
}

// sanitizedIDs joins ids for framing, each through repodoc.SanitizeMetadata.
func sanitizedIDs(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, repodoc.SanitizeMetadata(id))
	}
	return strings.Join(out, ", ")
}

// statusLabel renders a status for framing. A status outside the closed set
// renders as unknown: an unrecognized status is never shown as accepted.
func statusLabel(s Status) string {
	if !s.known() {
		return string(StatusUnknown)
	}
	return string(s)
}

// statusMeaning states what the record's status means for the review. Every
// status but accepted says plainly that the record is NOT a settled decision,
// and an unrecognized status reads as unknown.
func statusMeaning(r Record) string {
	switch r.Status {
	case StatusAccepted:
		return "Status: accepted. This record is a SETTLED decision: a change that contradicts it should either conform to it or say why the decision should be revisited."
	case StatusProposed:
		return "Status: proposed. This record has NOT been decided; it is context, NOT a settled decision, and a change may legitimately depart from it."
	case StatusSuperseded:
		by := "a later record"
		if len(r.SupersededBy) > 0 {
			ids := make([]string, 0, len(r.SupersededBy))
			for _, id := range r.SupersededBy {
				ids = append(ids, repodoc.SanitizeMetadata(id))
			}
			by = strings.Join(ids, ", ")
		}
		return fmt.Sprintf("Status: superseded by %s. This record NO LONGER governs; it is history, NOT a settled decision, and the superseding record decides.", by)
	case StatusRejected:
		return "Status: rejected. The approach this record describes was decided AGAINST; it is NOT a settled decision to follow."
	default:
		return "Status: unknown. Acceptance of this record was never confirmed; it is context, NOT a settled decision."
	}
}
