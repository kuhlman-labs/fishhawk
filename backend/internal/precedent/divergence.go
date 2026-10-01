package precedent

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// Divergence (E75.5 / #3733, ADR-082 #3728 decision (d) and rule 5): when the
// captain's decision at an allow-listed gate goes AGAINST clear precedent, the
// server records one precedent_divergence entry and offers one optional
// question — one-off, or a change of doctrine?
//
// This file is the PURE half: the four-condition threshold rule and the closed
// allow-list. Like the rest of the package it holds no clock (now is a
// parameter), no database handle and no HTTP. Long-form contract:
// backend/internal/precedent/README.md § "Divergence threshold".

// OutcomeReject is the plan_approval outcome the allow-list admits. It is the
// approval_submitted `decision` value a reject records (approval.DecisionReject);
// the literal is repeated here so this package keeps importing nothing beyond
// decisionindex.
const OutcomeReject = "reject"

// allowedDivergenceClasses is the CLOSED allow-list (ADR-082 rule 5). Config
// may narrow it, never widen it. The keys are decisionindex's own constants, so
// a renamed class is a compile break here rather than a silent no-fire.
var allowedDivergenceClasses = map[decisionindex.DecisionClass]struct{}{
	decisionindex.ClassConcernWaive: {},
	decisionindex.ClassConcernDefer: {},
	decisionindex.ClassPlanApproval: {},
}

// DivergenceClasses returns the closed allow-list, sorted.
func DivergenceClasses() []string {
	out := make([]string, 0, len(allowedDivergenceClasses))
	for c := range allowedDivergenceClasses {
		out = append(out, string(c))
	}
	sort.Strings(out)
	return out
}

// ComparisonClasses names the decision classes whose prior decisions form the
// precedent a decision of class is compared against. A concern waive and a
// concern defer are two answers to ONE question (what to do with an open
// concern), and each class's own outcome is constant ("waived" / "deferred"),
// so comparing a waive only against prior waives could never diverge: the
// concern classes are compared against the UNION of both. plan_approval is
// compared against itself (its outcome vocabulary is approve/reject). Any other
// class yields nil.
func ComparisonClasses(class decisionindex.DecisionClass) []decisionindex.DecisionClass {
	switch class {
	case decisionindex.ClassConcernWaive, decisionindex.ClassConcernDefer:
		return []decisionindex.DecisionClass{decisionindex.ClassConcernWaive, decisionindex.ClassConcernDefer}
	case decisionindex.ClassPlanApproval:
		return []decisionindex.DecisionClass{decisionindex.ClassPlanApproval}
	}
	return nil
}

// ErrUnknownDivergenceClass is returned by NewDivergenceConfig for a configured
// class outside the closed allow-list. Config narrows the allow-list; it can
// never widen it, so an unrecognised class is refused rather than ignored.
var ErrUnknownDivergenceClass = errors.New("precedent: divergence class is not in the closed allow-list")

// ErrInvalidDivergenceConfig is returned by NewDivergenceConfig for an
// out-of-range threshold.
var ErrInvalidDivergenceConfig = errors.New("precedent: invalid divergence threshold")

// DivergenceConfig is the threshold the rule runs under. The ZERO value is
// DISABLED (Enabled false), and so is DefaultDivergenceConfig(): the feature
// ships off until the tuning report picks values from evidence.
type DivergenceConfig struct {
	Enabled bool
	// MinDecisions is N: the fewest prior HUMAN decisions that can constitute
	// clear precedent.
	MinDecisions int
	// MinAgreement is X, in (0, 1]: the modal outcome's least share.
	MinAgreement float64
	// Window is the recency window: a prior decision older than now-Window
	// does not count.
	Window time.Duration
	// AllowedClasses narrows the closed allow-list. EMPTY means the whole
	// closed set.
	AllowedClasses []string
}

// DefaultDivergenceConfig is the shipped configuration: DISABLED, with
// conservative placeholder numbers the tuning report exists to replace.
func DefaultDivergenceConfig() DivergenceConfig {
	return DivergenceConfig{
		Enabled:      false,
		MinDecisions: 5,
		MinAgreement: 0.8,
		Window:       180 * 24 * time.Hour,
	}
}

// NewDivergenceConfig validates and builds a DivergenceConfig. It refuses, with
// a named error, a configured class outside the closed allow-list
// (ErrUnknownDivergenceClass) and an out-of-range threshold
// (ErrInvalidDivergenceConfig). The thresholds are validated even when
// enabled is false, so a malformed value fails at construction rather than at
// the moment someone flips the switch.
func NewDivergenceConfig(enabled bool, minDecisions int, minAgreement float64, window time.Duration, classes []string) (DivergenceConfig, error) {
	if minDecisions < 1 {
		return DivergenceConfig{}, fmt.Errorf("%w: min decisions must be >= 1, got %d", ErrInvalidDivergenceConfig, minDecisions)
	}
	if !(minAgreement > 0 && minAgreement <= 1) {
		return DivergenceConfig{}, fmt.Errorf("%w: min agreement must be in (0, 1], got %v", ErrInvalidDivergenceConfig, minAgreement)
	}
	if window <= 0 {
		return DivergenceConfig{}, fmt.Errorf("%w: window must be positive, got %s", ErrInvalidDivergenceConfig, window)
	}
	var kept []string
	seen := map[string]struct{}{}
	for _, c := range classes {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := allowedDivergenceClasses[decisionindex.DecisionClass(c)]; !ok {
			return DivergenceConfig{}, fmt.Errorf("%w: %q (allowed: %s)", ErrUnknownDivergenceClass, c, strings.Join(DivergenceClasses(), ", "))
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		kept = append(kept, c)
	}
	sort.Strings(kept)
	return DivergenceConfig{
		Enabled:        enabled,
		MinDecisions:   minDecisions,
		MinAgreement:   minAgreement,
		Window:         window,
		AllowedClasses: kept,
	}, nil
}

// ParseWindow parses a recency window: a Go duration ("4320h") or a whole
// number of days ("180d"). Zero, negative and malformed values are refused.
func ParseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%w: window %q is not a positive whole number of days", ErrInvalidDivergenceConfig, s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: window %q is not a positive duration (e.g. 180d or 4320h)", ErrInvalidDivergenceConfig, s)
	}
	return d, nil
}

// ClassAllowed reports whether a decision of class with outcome is one the rule
// may fire on under cfg: class is in the closed allow-list, in cfg's narrowing
// (when one is set), and — for plan_approval — the outcome is a reject. The
// reject restriction is what keeps the rule off the approve-dominated base rate
// of plan gates.
func (cfg DivergenceConfig) ClassAllowed(class decisionindex.DecisionClass, outcome string) bool {
	if _, ok := allowedDivergenceClasses[class]; !ok {
		return false
	}
	if class == decisionindex.ClassPlanApproval && outcome != OutcomeReject {
		return false
	}
	if len(cfg.AllowedClasses) == 0 {
		return true
	}
	for _, c := range cfg.AllowedClasses {
		if c == string(class) {
			return true
		}
	}
	return false
}

// VerdictKind names the rule's answer.
type VerdictKind string

// The verdict kinds Decide returns.
const (
	VerdictDisabled         VerdictKind = "disabled"
	VerdictClassNotAllowed  VerdictKind = "class_not_allowed"
	VerdictNoClearPrecedent VerdictKind = "no_clear_precedent"
	VerdictAgreed           VerdictKind = "agreed_with_precedent"
	VerdictDiverged         VerdictKind = "diverged"
)

// NoPrecedentReason names the FIRST unmet clear-precedent condition.
type NoPrecedentReason string

// The no-clear-precedent reasons, one per Decide condition.
const (
	ReasonBelowMinDecisions       NoPrecedentReason = "below_min_decisions"
	ReasonOutsideWindow           NoPrecedentReason = "outside_window"
	ReasonDoctrineVersionMismatch NoPrecedentReason = "doctrine_version_mismatch"
	ReasonBelowMinAgreement       NoPrecedentReason = "below_min_agreement"
)

// Verdict is the rule's answer for one decision. Reason is set only on
// VerdictNoClearPrecedent; Summary and Cited describe the FILTERED set the
// thresholds were evaluated over (human, in-window, same doctrine version) and
// are populated on VerdictAgreed and VerdictDiverged.
type Verdict struct {
	Kind           VerdictKind
	Reason         NoPrecedentReason
	ModalOutcome   string
	AgreementRatio float64
	Human          int
	Summary        Summary
	Cited          []Item
}

// Decide evaluates the clear-precedent rule for one decision: class and
// outcome are the captain's decision, doctrineVersion the deciding run's
// doctrine version, now the decision time, and items the ranked precedent set
// (the caller has already excluded the decision itself).
//
// Clear precedent requires ALL FOUR conditions, checked in this order (the
// first unmet one is the Reason):
//
//  1. at least MinDecisions HUMAN items — a delegated decision contributes
//     nothing to N and nothing to the agreement denominator;
//  2. at least MinDecisions of those inside the recency window [now-Window, …];
//  3. at least MinDecisions of those under the SAME doctrine version;
//  4. the modal outcome's share of exactly that set is at least MinAgreement.
//
// With clear precedent, the verdict is diverged when outcome differs from the
// modal outcome and agreed_with_precedent otherwise.
func Decide(cfg DivergenceConfig, class decisionindex.DecisionClass, outcome, doctrineVersion string, now time.Time, items []Item) Verdict {
	if !cfg.Enabled {
		return Verdict{Kind: VerdictDisabled}
	}
	if !cfg.ClassAllowed(class, outcome) {
		return Verdict{Kind: VerdictClassNotAllowed}
	}

	human := make([]Item, 0, len(items))
	for _, it := range items {
		if it.Delegated {
			continue
		}
		human = append(human, it)
	}
	if len(human) < cfg.MinDecisions {
		return noClearPrecedent(ReasonBelowMinDecisions, len(human))
	}

	cutoff := now.Add(-cfg.Window)
	inWindow := make([]Item, 0, len(human))
	for _, it := range human {
		if it.DecidedAt.Before(cutoff) {
			continue
		}
		inWindow = append(inWindow, it)
	}
	if len(inWindow) < cfg.MinDecisions {
		return noClearPrecedent(ReasonOutsideWindow, len(inWindow))
	}

	same := make([]Item, 0, len(inWindow))
	for _, it := range inWindow {
		if it.DoctrineVersion != doctrineVersion {
			continue
		}
		same = append(same, it)
	}
	if len(same) < cfg.MinDecisions {
		return noClearPrecedent(ReasonDoctrineVersionMismatch, len(same))
	}

	s := Summarize(same)
	v := Verdict{
		ModalOutcome:   s.ModalOutcome,
		AgreementRatio: s.AgreementRatio,
		Human:          s.Human,
		Summary:        s,
		Cited:          same,
	}
	if s.AgreementRatio < cfg.MinAgreement {
		v.Kind = VerdictNoClearPrecedent
		v.Reason = ReasonBelowMinAgreement
		v.Cited = nil
		return v
	}
	if outcome == s.ModalOutcome {
		v.Kind = VerdictAgreed
		return v
	}
	v.Kind = VerdictDiverged
	return v
}

func noClearPrecedent(r NoPrecedentReason, human int) Verdict {
	return Verdict{Kind: VerdictNoClearPrecedent, Reason: r, Human: human}
}
