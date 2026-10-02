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
// shown in full, and the matches left out by the cap.
type Selection struct {
	// Index is the index document, always included (truncated loudly when it
	// alone exceeds the cap).
	Index repodoc.Document
	// TotalRecords is how many records the index lists.
	TotalRecords int
	// Matched is how many records' applies_to matched the change.
	Matched int
	// Included are the matches shown in full, in rank order — a strict prefix
	// of the ranking.
	Included []Included
	// Dropped are the matches left out because they did not fit, in rank
	// order. Never fetched past the first.
	Dropped []Match
	// CapBytes is the budget: the resolver's effective cap. It counts the
	// index's rendered bytes plus every included record's rendered bytes.
	CapBytes int
	// IncludedBytes is that sum; always <= CapBytes.
	IncludedBytes int
}

// Assemble fetches the index at req.Commit, parses it strictly, selects and
// ranks the matching records, and fits them into r's effective cap:
//
//   - the index is ALWAYS included; when its rendered size exceeds the cap it
//     is reshaped under a smaller cap until it fits (bounded, and never under
//     a negative cap), shown with repodoc's loud truncation marker;
//   - each match, in rank order, is fetched at the SAME commit and shaped
//     under a cap at least its raw size (so its untruncated rendered size is
//     what is measured), and is included only when that size fits the
//     remaining budget;
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

	budget := r.CapBytes()
	idxDoc, err := fitIndex(idxF, budget)
	if err != nil {
		return nil, err
	}
	sel := &Selection{
		Index:         idxDoc,
		TotalRecords:  len(idx.Records),
		Matched:       len(matches),
		CapBytes:      budget,
		IncludedBytes: idxDoc.RenderedBytes,
	}
	remaining := budget - idxDoc.RenderedBytes
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
		doc := f.Document(max(budget, len(f.Content)))
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
// dropped record with its rank. A caller injecting other documents in the same
// prompt merges this into ONE repodoc.AttributeSet call.
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
		out = append(out, repodoc.ToPromptDocument(inc.Document, recordFraming(inc.Record)))
	}
	return out
}

// indexFraming is the system-authored framing of the index document.
func (s *Selection) indexFraming(grounded bool) repodoc.Framing {
	var b strings.Builder
	fmt.Fprintf(&b, "This is the repository's decision record index (%s), read at the run's admission commit. "+
		"%d of its %d records declare an applies_to path glob matching a path this change touches; ",
		repodoc.SanitizeMetadata(s.Index.Path), s.Matched, s.TotalRecords)
	if len(s.Included) == 0 {
		b.WriteString("none of them is shown in full in this prompt. ")
	} else {
		ids := make([]string, 0, len(s.Included))
		for _, inc := range s.Included {
			ids = append(ids, repodoc.SanitizeMetadata(inc.Record.ID))
		}
		fmt.Fprintf(&b, "%d of them are shown in full below, in rank order (accepted records first, then by how many changed paths each matches, then newest first): %s. ",
			len(s.Included), strings.Join(ids, ", "))
	}
	b.WriteString("Every other record is NOT shown in this prompt")
	if grounded {
		b.WriteString(": you may read one from the review tree by its path, but the review tree is the change's HEAD, not the admission commit, so a record this change edits reads there AS EDITED.")
	} else {
		b.WriteString(", and you cannot read it in this review.")
	}
	if len(s.Dropped) > 0 {
		fmt.Fprintf(&b, "\n\nNOT INCLUDED because of the injection cap (%d bytes, counting this index plus the records shown in full): "+
			"these %d matching records were left out, highest rank first. Do not judge the change as if they were absent:\n",
			s.CapBytes, len(s.Dropped))
		for _, m := range s.Dropped {
			fmt.Fprintf(&b, "- %s (%s, status: %s)\n",
				repodoc.SanitizeMetadata(m.Record.ID), repodoc.SanitizeMetadata(m.Record.Path), statusLabel(m.Record.Status))
		}
	}
	return repodoc.Framing{
		Heading:   indexHeading,
		Preamble:  strings.TrimRight(b.String(), "\n"),
		TrustNote: indexTrustNote,
	}
}

// recordFraming frames one included record with its status and what that
// status means.
func recordFraming(r Record) repodoc.Framing {
	return repodoc.Framing{
		Heading:   fmt.Sprintf(recordHeadFmt, repodoc.SanitizeMetadata(r.ID), statusLabel(r.Status)),
		Preamble:  statusMeaning(r),
		TrustNote: indexTrustNote,
	}
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
