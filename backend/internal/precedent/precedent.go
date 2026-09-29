// Package precedent owns the ADR-082 (#3728) decision (b) PRECEDENT QUERY's
// pure half (E75.3 / #3731): given a decision context, rank prior decisions of
// the same class from the SAME repository by a deterministic weighted score,
// and summarise the agreement of the returned set.
//
// It is PURE. It imports backend/internal/decisionindex for the row model and
// the canonical concern-category vocabulary and NOTHING else from the tree — no
// database handle, no clock, no HTTP — so a ranking is byte-reproducible and
// every component is testable in isolation. There is no model call, no
// embedding, and no write: a precedent query mints no audit entry and grants no
// authority. It reports what was decided before; the decision is still the
// caller's.
//
// Long-form contract (the weights and why each is where it is, the determinism
// argument, and the agreement summary E75.5's divergence threshold consumes):
// backend/internal/precedent/README.md.
package precedent

import (
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// Context is the decision a caller wants precedent FOR. Repo and DecisionClass
// are the hard filter (ADR-082 rule 3); the rest are ranking signals, each
// inert when empty.
type Context struct {
	Repo            string
	DecisionClass   decisionindex.DecisionClass
	StageKind       string
	TouchedPaths    []string
	ConcernCategory string
	Severity        string
	EscalationKeys  []string
}

// The score weights. They are CHOSEN, not derived — there is no corpus of
// precedent queries to tune them against yet — so they are exported named
// constants with their rationale in the package README, making a later
// calibration a value change with a test rather than a rewrite. The
// determinism guarantee below is independent of their values.
//
// They sum to 1.0, so a total score is always in [0, 1]:
//
//   - WeightTouchedPaths dominates because WHERE a decision was made is the
//     strongest available proxy for whether it is about the same thing. It is
//     prefix-aware, so a sibling file in the same package still scores.
//   - WeightEscalationKeys is next: a shared fired escalation key means the two
//     decisions were made under the same governing rule, which is a stronger
//     signal than the free-text concern category.
//   - WeightConcernCategory is the normalized reviewer-concern kind. Lower than
//     the above because the vocabulary is coarse (six canonical keys).
//   - WeightSeverity is the weakest: severity co-varies with category and a
//     bare severity match carries little on its own.
const (
	WeightTouchedPaths    = 0.45
	WeightEscalationKeys  = 0.20
	WeightConcernCategory = 0.25
	WeightSeverity        = 0.10
)

// MaxMatchedKeys bounds each reported matched-key list. A matched-path-prefix
// intersection is O(paths x depth) and a caller can supply thousands of paths,
// so the EXPLANATION is capped (with its total reported) rather than allowed to
// dominate the response. It never affects the score, only what is echoed.
const MaxMatchedKeys = 20

// MatchedKeys explains WHICH keys a row matched on — the ADR-082 requirement
// that a ranked item say why it ranked. Each list is capped at MaxMatchedKeys
// with its untruncated total reported alongside.
type MatchedKeys struct {
	TouchedPathPrefixes          []string `json:"touched_path_prefixes"`
	TouchedPathPrefixesTotal     int      `json:"touched_path_prefixes_total"`
	TouchedPathPrefixesTruncated bool     `json:"touched_path_prefixes_truncated,omitempty"`
	ConcernCategory              string   `json:"concern_category,omitempty"`
	Severity                     string   `json:"severity,omitempty"`
	EscalationKeys               []string `json:"escalation_keys"`
	EscalationKeysTotal          int      `json:"escalation_keys_total"`
	EscalationKeysTruncated      bool     `json:"escalation_keys_truncated,omitempty"`
}

// ScoreComponents reports what each signal contributed, so a score is never an
// unexplained number. Each field is the WEIGHTED contribution (the raw
// similarity times its weight); Total is their sum.
type ScoreComponents struct {
	TouchedPaths    float64 `json:"touched_paths"`
	EscalationKeys  float64 `json:"escalation_keys"`
	ConcernCategory float64 `json:"concern_category"`
	Severity        float64 `json:"severity"`
	Total           float64 `json:"total"`
}

// Item is one ranked prior decision. It cites its source entry by sequence and
// hash; ReasonExcerpt is filled in by the CALLER, read from that entry at query
// time (ADR-082 rule 1 — the prose is never copied into the index).
type Item struct {
	SourceSequence  int64           `json:"source_sequence"`
	SourceEntryHash string          `json:"source_entry_hash"`
	RunID           string          `json:"run_id"`
	StageID         string          `json:"stage_id,omitempty"`
	Repo            string          `json:"repo"`
	DecisionClass   string          `json:"decision_class"`
	StageKind       string          `json:"stage_kind,omitempty"`
	Outcome         string          `json:"outcome,omitempty"`
	RejectClass     string          `json:"reject_class,omitempty"`
	ConcernCategory string          `json:"concern_category,omitempty"`
	Severity        string          `json:"severity,omitempty"`
	Delegated       bool            `json:"delegated"`
	ActorKind       string          `json:"actor_kind,omitempty"`
	DoctrineVersion string          `json:"doctrine_version,omitempty"`
	DecidedAt       time.Time       `json:"decided_at"`
	ReasonSequence  int64           `json:"reason_sequence"`
	ReasonKey       string          `json:"reason_key,omitempty"`
	ReasonExcerpt   string          `json:"reason_excerpt,omitempty"`
	MatchedKeys     MatchedKeys     `json:"matched_keys"`
	Score           ScoreComponents `json:"score"`
	// HardFilterOnly marks a row that matched the hard filter (repo, class,
	// stage kind) and NOTHING else — ADR-082 rule 3's "reports that"
	// requirement. Its components are zero and its matched-key set empty.
	HardFilterOnly bool `json:"hard_filter_only,omitempty"`
}

// Summary is the result-set agreement summary E75.5's divergence threshold
// consumes. It describes the RETURNED items, not the whole index.
type Summary struct {
	Count     int `json:"count"`
	Human     int `json:"human"`
	Delegated int `json:"delegated"`
	// ModalOutcome is the most frequent outcome among the returned items (the
	// lexicographically smallest on a tie, so the field is deterministic), and
	// AgreementRatio is its share: 1.0 for a unanimous set, 0 for an empty one.
	// Reporting the outcome ALONGSIDE the ratio is what keeps the number
	// interpretable — any other definition (pairwise agreement, entropy) would
	// be equally defensible, so this one is stated rather than inferred.
	ModalOutcome   string  `json:"modal_outcome,omitempty"`
	AgreementRatio float64 `json:"agreement_ratio"`
	// DoctrineVersions is the sorted set of doctrine versions the returned
	// items span: a set of size > 1 means the precedent was set under more
	// than one charter revision.
	DoctrineVersions []string `json:"doctrine_versions"`
	// HardFilterOnly counts the returned items that matched nothing beyond
	// the hard filter.
	HardFilterOnly int `json:"hard_filter_only"`
}

// Rank scores rows against c, drops the ones the hard filter refuses, and
// returns the highest-scoring limit items newest-first-on-ties plus the
// agreement summary of exactly those items.
//
// THE HARD FILTER IS RE-ASSERTED HERE. The store is expected to have applied it
// already (the caller builds a ListFilter from the same Context), and this
// second check is DEFENCE IN DEPTH: it makes it structurally impossible for the
// scorer to emit a cross-repository or cross-class item even if a caller builds
// the filter wrongly. The filter the store received is the control the handler
// test observes; this is the backstop.
//
// ORDERING IS TOTAL AND DETERMINISTIC: score DESC, then decided_at DESC
// (recency), then source_sequence DESC as the final discriminator. Source
// sequence is the index's primary key, so no two rows can compare equal and the
// output never depends on the input order.
//
// limit <= 0 returns every surviving row.
func Rank(c Context, rows []decisionindex.Row, limit int) ([]Item, Summary) {
	ctxPaths := expandPathPrefixes(c.TouchedPaths)
	ctxKeys := stringSet(c.EscalationKeys)

	items := make([]Item, 0, len(rows))
	for _, row := range rows {
		if !hardFilterAdmits(c, row) {
			continue
		}
		items = append(items, scoreRow(c, row, ctxPaths, ctxKeys))
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score.Total != items[j].Score.Total {
			return items[i].Score.Total > items[j].Score.Total
		}
		if !items[i].DecidedAt.Equal(items[j].DecidedAt) {
			return items[i].DecidedAt.After(items[j].DecidedAt)
		}
		return items[i].SourceSequence > items[j].SourceSequence
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, Summarize(items)
}

// hardFilterAdmits is the defensive re-assertion of ADR-082 rule 3's hard
// filter. An EMPTY context field is match-all, exactly as ListFilter's is, so
// the two layers agree by construction.
func hardFilterAdmits(c Context, row decisionindex.Row) bool {
	if c.Repo != "" && row.Repo != c.Repo {
		return false
	}
	if c.DecisionClass != "" && row.DecisionClass != c.DecisionClass {
		return false
	}
	if c.StageKind != "" && row.StageKind != c.StageKind {
		return false
	}
	return true
}

func scoreRow(c Context, row decisionindex.Row, ctxPaths, ctxKeys map[string]struct{}) Item {
	it := Item{
		SourceSequence:  row.SourceSequence,
		SourceEntryHash: row.SourceEntryHash,
		RunID:           row.RunID.String(),
		Repo:            row.Repo,
		DecisionClass:   string(row.DecisionClass),
		StageKind:       row.StageKind,
		Outcome:         row.Outcome,
		RejectClass:     row.RejectClass,
		ConcernCategory: row.ConcernCategory,
		Severity:        row.Severity,
		Delegated:       row.Delegated,
		ActorKind:       row.ActorKind,
		DoctrineVersion: row.DoctrineVersion,
		DecidedAt:       row.DecidedAt.UTC(),
		ReasonSequence:  row.ReasonSequence,
		ReasonKey:       row.ReasonKey,
	}
	if row.StageID != nil {
		it.StageID = row.StageID.String()
	}

	rowPaths := expandPathPrefixes(row.TouchedPaths)
	pathSim, pathHits := jaccard(ctxPaths, rowPaths)
	rowKeys := stringSet(row.EscalationKeys)
	keySim, keyHits := jaccard(ctxKeys, rowKeys)

	it.Score.TouchedPaths = pathSim * WeightTouchedPaths
	it.Score.EscalationKeys = keySim * WeightEscalationKeys
	if c.ConcernCategory != "" && row.ConcernCategory == c.ConcernCategory {
		it.Score.ConcernCategory = WeightConcernCategory
		it.MatchedKeys.ConcernCategory = row.ConcernCategory
	}
	if c.Severity != "" && row.Severity == c.Severity {
		it.Score.Severity = WeightSeverity
		it.MatchedKeys.Severity = row.Severity
	}
	it.Score.Total = it.Score.TouchedPaths + it.Score.EscalationKeys +
		it.Score.ConcernCategory + it.Score.Severity

	it.MatchedKeys.TouchedPathPrefixes, it.MatchedKeys.TouchedPathPrefixesTotal,
		it.MatchedKeys.TouchedPathPrefixesTruncated = capKeys(pathHits)
	it.MatchedKeys.EscalationKeys, it.MatchedKeys.EscalationKeysTotal,
		it.MatchedKeys.EscalationKeysTruncated = capKeys(keyHits)

	if it.Score.Total == 0 {
		// Matched the hard filter and nothing else: say so, with zeroed
		// components and an empty matched-key set, rather than returning an
		// unexplained zero-score row.
		it.HardFilterOnly = true
		it.MatchedKeys = MatchedKeys{TouchedPathPrefixes: []string{}, EscalationKeys: []string{}}
	}
	return it
}

// Summarize builds the agreement summary of an already-ranked item set. Split
// out from Rank so E75.5 can summarise a set it filtered further.
func Summarize(items []Item) Summary {
	s := Summary{Count: len(items), DoctrineVersions: []string{}}
	if len(items) == 0 {
		return s
	}
	outcomes := map[string]int{}
	versions := map[string]struct{}{}
	for _, it := range items {
		if it.Delegated {
			s.Delegated++
		} else {
			s.Human++
		}
		if it.HardFilterOnly {
			s.HardFilterOnly++
		}
		outcomes[it.Outcome]++
		if it.DoctrineVersion != "" {
			versions[it.DoctrineVersion] = struct{}{}
		}
	}
	s.DoctrineVersions = sortedKeys(versions)

	// Modal outcome, lexicographically smallest on a count tie so the field is
	// deterministic under input reordering.
	best, bestN := "", -1
	for _, o := range sortedKeys(setOf(outcomes)) {
		if outcomes[o] > bestN {
			best, bestN = o, outcomes[o]
		}
	}
	s.ModalOutcome = best
	s.AgreementRatio = float64(bestN) / float64(len(items))
	return s
}

// expandPathPrefixes turns each path into itself PLUS every directory prefix of
// it, so a decision on backend/internal/server/foo.go matches one on
// backend/internal/server/bar.go with a real but smaller score than an exact
// hit. Empty and separator-only inputs contribute nothing.
func expandPathPrefixes(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths)*4)
	for _, p := range paths {
		p = strings.Trim(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		segs := strings.Split(p, "/")
		acc := ""
		for _, seg := range segs {
			if seg == "" {
				continue
			}
			if acc == "" {
				acc = seg
			} else {
				acc = acc + "/" + seg
			}
			out[acc] = struct{}{}
		}
	}
	return out
}

func stringSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// jaccard returns |A n B| / |A u B| plus the sorted intersection.
//
// AN EMPTY UNION RETURNS EXACTLY 0, NEVER NaN. A bare len(inter)/len(union)
// division on two empty sets yields NaN, and every comparison against NaN is
// false — so a NaN score slips past every threshold comparison silently
// (the same rule intakegroom/duplicate.go records).
func jaccard(a, b map[string]struct{}) (float64, []string) {
	union := len(a)
	inter := make([]string, 0, len(b))
	for k := range b {
		if _, ok := a[k]; ok {
			inter = append(inter, k)
		} else {
			union++
		}
	}
	if union == 0 {
		return 0, []string{}
	}
	sort.Strings(inter)
	return float64(len(inter)) / float64(union), inter
}

// capKeys bounds an explanation list at MaxMatchedKeys, reporting the
// untruncated total and whether the cap bit.
func capKeys(keys []string) (kept []string, total int, truncated bool) {
	total = len(keys)
	if total > MaxMatchedKeys {
		return append([]string(nil), keys[:MaxMatchedKeys]...), total, true
	}
	if keys == nil {
		return []string{}, 0, false
	}
	return keys, total, false
}

func setOf(m map[string]int) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
