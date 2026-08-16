package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Migration-number collision recovery (#2748), the DECISION shape.
//
// Two concurrent items that both add a database migration are planned against
// the same next free number. Whichever lands second must renumber its files —
// the correct engineering move — but scope.files pins the filename the stale
// plan chose, so the renamed pair is a net-new out-of-scope CREATE and the
// #818/#825 created-out-of-scope gate fails the stage category-B before the
// push.
//
// This file does NOT relax that gate and does NOT auto-substitute anything. It
// RECOGNIZES the shape and turns it into an operator-gated scope amendment,
// reusing the mid-stage amendment machinery (POST
// /v0/runs/{id}/scope-amendments, the amendment park fishhawk_await_stage
// surfaces, fishhawk_decide_scope_amendment). On approval the created paths are
// folded into cfg.scopeFiles by the existing refreshScopeAmendments and the
// stale declared paths become scope-completeness exemptions. On denial,
// timeout, budget exhaustion, or ANY error nothing changes and the gate fires
// exactly as it does today.
//
// The recognition carries NO allocation proof (whether the new number is the
// RIGHT one is the operator's call) and NO freshness invariant (it is a pattern
// match on declared-vs-on-disk paths, never a claim about what the base tree
// contains). Long-form contract, refusal branches, and residuals:
// runner/cmd/fishhawk-runner/README.md.

// migrationFileRE matches a migration BASENAME: NNNN_<slug>.{up,down}.sql.
var migrationFileRE = regexp.MustCompile(`^([0-9]+)_(.+)\.(up|down)\.sql$`)

// migrationsDirName is the REQUIRED parent-directory basename. The boundary is
// deliberate: a "migration-shaped" file under testdata/, fixtures/, or a
// `migrations-old/` sibling is a fixture, not a migration, and must never be
// offered as a substitution.
const migrationsDirName = "migrations"

// migrationRenumber is one recognized declared→created substitution: the
// declared {up,down} pair (absent from the work tree) and the on-disk
// same-slug pair that carries a different number.
type migrationRenumber struct {
	Dir            string
	Slug           string
	DeclaredNumber string
	CreatedNumber  string
	DeclaredUp     string
	DeclaredDown   string
	CreatedUp      string
	CreatedDown    string
}

// parseMigrationPath splits a repo-relative, slash-separated migration path
// into its parts. ok is false unless the parent directory's BASENAME is exactly
// `migrations` AND the basename matches NNNN_<slug>.{up,down}.sql with a
// non-empty slug.
func parseMigrationPath(p string) (dir, number, slug, direction string, ok bool) {
	if p == "" {
		return "", "", "", "", false
	}
	dir = path.Dir(p)
	if path.Base(dir) != migrationsDirName {
		return "", "", "", "", false
	}
	m := migrationFileRE.FindStringSubmatch(path.Base(p))
	if m == nil || m[2] == "" {
		return "", "", "", "", false
	}
	return dir, m[1], m[2], m[3], true
}

// migrationPair is a {up,down} pair under one (dir, slug, number) key.
type migrationPair struct {
	up   string
	down string
}

func (p migrationPair) complete() bool { return p.up != "" && p.down != "" }

// migrationKey identifies one declared or on-disk migration pair.
type migrationKey struct {
	dir    string
	slug   string
	number string
}

// recognizeMigrationRenumbers reports the declared→created migration
// substitutions visible in repoDir's work tree, given the stage's DECLARED
// scope paths.
//
// It is purely declared-driven and filesystem-only: no git, no index, no base
// tree. That is what makes it indifferent to whether the agent staged the
// created files, and what stops it from ever asserting anything about the base.
//
// A candidate is recognized only when ALL of these hold:
//   - the declared pair is complete ({up,down}) under one (dir, slug, number)
//     key and its parent directory basename is `migrations`;
//   - BOTH declared paths are ABSENT from the work tree (os.Lstat →
//     fs.ErrNotExist). A present path — including a dangling symlink, which
//     Lstat reports as present — disqualifies the candidate;
//   - the SAME directory holds EXACTLY ONE complete same-slug {up,down} pair of
//     regular files carrying a DIFFERENT number, neither of whose paths is
//     itself declared.
//
// After all candidates are processed a global strict 1:1 sweep rejects the
// WHOLE result (nil, nil) if any declared or created path appears in more than
// one substitution.
//
// A fresh result slice is allocated on every invocation and no state is cached
// across calls, so a second invocation re-derives everything from the work tree
// as it stands then.
//
// Lstat/ReadDir errors return (nil, err) — fail-closed at the predicate, which
// the caller turns into fail-open (proceed unchanged). A ReadDir on a
// NONEXISTENT directory is not an error condition: there can be no created pair
// there, so that candidate is simply disqualified.
func recognizeMigrationRenumbers(repoDir string, declared []string) ([]migrationRenumber, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	declaredSet := make(map[string]struct{}, len(declared))
	for _, p := range declared {
		declaredSet[p] = struct{}{}
	}

	// (a) group the DECLARED paths by (dir, slug, number); only complete
	// {up,down} pairs are candidates, so a declared half-pair is never one.
	declaredPairs := make(map[migrationKey]*migrationPair)
	var order []migrationKey
	for _, p := range declared {
		dir, number, slug, direction, ok := parseMigrationPath(p)
		if !ok {
			continue
		}
		k := migrationKey{dir: dir, slug: slug, number: number}
		pair, seen := declaredPairs[k]
		if !seen {
			pair = &migrationPair{}
			declaredPairs[k] = pair
			order = append(order, k)
		}
		if direction == "up" {
			pair.up = p
		} else {
			pair.down = p
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].dir != order[j].dir {
			return order[i].dir < order[j].dir
		}
		if order[i].slug != order[j].slug {
			return order[i].slug < order[j].slug
		}
		return order[i].number < order[j].number
	})

	// One ReadDir per candidate directory, memoized within this invocation
	// only (the fresh-slice discipline: nothing survives the call).
	dirCache := make(map[string][]os.DirEntry)
	readDir := func(dir string) ([]os.DirEntry, bool, error) {
		if entries, ok := dirCache[dir]; ok {
			return entries, true, nil
		}
		entries, err := os.ReadDir(filepath.Join(repoDir, filepath.FromSlash(dir)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				dirCache[dir] = nil
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("read migrations dir %s: %w", dir, err)
		}
		dirCache[dir] = entries
		return entries, true, nil
	}

	var out []migrationRenumber
	for _, k := range order {
		pair := declaredPairs[k]
		if !pair.complete() {
			continue
		}
		// (b) both declared paths must be ABSENT from the work tree.
		absent, err := bothDeclaredAbsent(repoDir, pair.up, pair.down)
		if err != nil {
			return nil, err
		}
		if !absent {
			continue
		}
		// (c) collect the on-disk same-slug pairs carrying a different number.
		entries, present, err := readDir(k.dir)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		candidates := make(map[string]*migrationPair)
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			p := path.Join(k.dir, e.Name())
			_, number, slug, direction, ok := parseMigrationPath(p)
			if !ok || slug != k.slug || number == k.number {
				continue
			}
			// (e) a path already in `declared` needs no amendment.
			if _, isDeclared := declaredSet[p]; isDeclared {
				continue
			}
			cp, seen := candidates[number]
			if !seen {
				cp = &migrationPair{}
				candidates[number] = cp
			}
			if direction == "up" {
				cp.up = p
			} else {
				cp.down = p
			}
		}
		// (d) EXACTLY ONE complete on-disk pair; zero or two-or-more disqualifies.
		var matchNumber string
		var match *migrationPair
		for number, cp := range candidates {
			if !cp.complete() {
				continue
			}
			if match != nil {
				match = nil
				break
			}
			matchNumber, match = number, cp
		}
		if match == nil {
			continue
		}
		out = append(out, migrationRenumber{
			Dir:            k.dir,
			Slug:           k.slug,
			DeclaredNumber: k.number,
			CreatedNumber:  matchNumber,
			DeclaredUp:     pair.up,
			DeclaredDown:   pair.down,
			CreatedUp:      match.up,
			CreatedDown:    match.down,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	// (f) strict global 1:1. A duplicate on EITHER side means the recognition
	// is ambiguous, and an ambiguous recognition must not be offered at all —
	// the whole result is rejected, not the offending entry.
	seen := make(map[string]bool, len(out)*4)
	for _, s := range out {
		for _, p := range []string{s.DeclaredUp, s.DeclaredDown, s.CreatedUp, s.CreatedDown} {
			if seen[p] {
				return nil, nil
			}
			seen[p] = true
		}
	}
	return out, nil
}

// bothDeclaredAbsent reports whether BOTH declared paths are absent from the
// work tree. os.Lstat (not os.Stat) is deliberate: a dangling symlink at a
// declared path returns the link's own info rather than fs.ErrNotExist, so it
// counts as PRESENT and disqualifies the candidate — the fail-closed direction.
// Any non-ErrNotExist stat error (e.g. a permission denial) is returned so the
// caller falls open rather than guessing.
func bothDeclaredAbsent(repoDir string, paths ...string) (bool, error) {
	for _, p := range paths {
		_, err := os.Lstat(filepath.Join(repoDir, filepath.FromSlash(p)))
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, fs.ErrNotExist):
			continue
		default:
			return false, fmt.Errorf("stat declared migration %s: %w", p, err)
		}
	}
	return true, nil
}

// Decision-park tuning. Vars, not consts, so tests can shrink them; production
// callers leave them alone.
var (
	// migrationRenumberDecisionBudget bounds the blocking wait for the
	// operator's decision. 900s matches the backend's
	// AmendmentPollWindowSeconds, the window the agent's own mid-stage
	// amendment poll uses.
	migrationRenumberDecisionBudget = 900 * time.Second
	// migrationRenumberWaitSeconds is the per-request `?wait=N` long-poll
	// hold the backend applies.
	migrationRenumberWaitSeconds = 30
	// migrationRenumberPollInterval is the pause between long-polls. It
	// matters only when the server returns immediately (no pending row to
	// hold on, or a fake in tests); the real path spends its time inside the
	// long-poll.
	migrationRenumberPollInterval = 2 * time.Second
)

// maybeParkForMigrationRenumber is the run() call site's single entry point:
// recognize, and if anything is recognized, park for the decision. Returns the
// substitution exemptions to honor (nil unless an amendment was APPROVED) plus
// the substitutions that were APPROVED, which the base-rebase re-invoke arm
// passes back in as `prior`.
//
// prior is empty on the first call. On the re-invoke it carries attempt 1's
// APPROVED substitutions, whose created paths the approval folded into
// cfg.scopeFiles — see dropAbsentFoldedCreations for why that provenance
// cannot be re-derived from the declared set alone.
//
// Fail-open throughout: a recognition error logs one line and returns nil, and
// every driver refusal returns nil, so the caller proceeds into today's
// unchanged created-out-of-scope path.
func maybeParkForMigrationRenumber(ctx context.Context, client uploadClient, cfg *config, mcpToken string, prior []migrationRenumber, logSink io.Writer) ([]scopeExemption, []migrationRenumber) {
	repoDir := cfg.workingDir
	if repoDir == "" {
		repoDir = "."
	}
	declared, carried, err := dropAbsentFoldedCreations(repoDir, scopePaths(cfg.scopeFiles), prior)
	if err != nil {
		emitMigrationRenumberScanSkipped(cfg, err, logSink)
		return nil, nil
	}
	subs, err := recognizeMigrationRenumbers(repoDir, declared)
	if err != nil {
		emitMigrationRenumberScanSkipped(cfg, err, logSink)
		return nil, nil
	}
	if len(subs) == 0 {
		return nil, nil
	}
	approved, exemptions := parkForMigrationRenumberAmendment(ctx, client, cfg, mcpToken, subs, logSink)
	if !approved {
		return nil, nil
	}
	return append(carried, exemptions...), subs
}

// dropAbsentFoldedCreations removes from `declared` the created paths of the
// ALREADY-APPROVED substitutions in `prior` that the re-invoked agent has since
// removed from the work tree, and returns one exemption per dropped path.
//
// WHY THE CALLER MUST SUPPLY THE PROVENANCE (#2748 fix-up). Attempt 1's approval
// folds the created pair (say 0071) into cfg.scopeFiles. If the base-rebase
// re-invoke renumbers AGAIN (0071 → 0072), the declared set now holds TWO absent
// same-slug pairs (the plan's 0070 and the folded 0071) both resolving to the
// one on-disk 0072 pair — which the recognition's strict global 1:1 sweep
// rejects wholesale, so no amendment is offered and the second ship falls into
// category-B. That state is structurally IDENTICAL to the genuinely ambiguous
// C5 shape (a plan declaring two same-slug migrations and delivering one), so no
// predicate over (declared, on-disk) alone can tell them apart. The difference
// is provenance, which only the caller holds: the 0071 pair entered scope.files
// by an approved amendment, not by the plan. Dropping exactly those paths
// preserves the strict sweep untouched for every other shape.
//
// The drop is CONDITIONAL on both created paths being ABSENT. A prior pair the
// re-invoked agent left in place stays declared, so the "created pair already
// declared" branch still disqualifies it and no second amendment is filed for a
// substitution that already landed. A half-present pair (one path absent) is
// likewise left declared — the conservative direction.
//
// The returned exemptions cover the dropped paths themselves: they remain in
// cfg.scopeFiles from the first fold but are no longer in the work tree, so
// without them the #1151 scope-completeness gate would demand a file the
// re-invoke deliberately abandoned.
//
// Lstat errors are returned so the caller falls open, exactly as a recognition
// error does.
func dropAbsentFoldedCreations(repoDir string, declared []string, prior []migrationRenumber) ([]string, []scopeExemption, error) {
	if len(prior) == 0 {
		return declared, nil, nil
	}
	drop := make(map[string]struct{}, len(prior)*2)
	var exemptions []scopeExemption
	for _, s := range prior {
		absent, err := bothDeclaredAbsent(repoDir, s.CreatedUp, s.CreatedDown)
		if err != nil {
			return nil, nil, err
		}
		if !absent {
			continue
		}
		reason := fmt.Sprintf(
			"migration %s → %s (slug %q) was folded into scope.files by an approved amendment on this stage's first attempt; the base-rebase re-invoke renumbered again, so the folded path is no longer in the work tree",
			s.DeclaredNumber, s.CreatedNumber, s.Slug)
		for _, p := range []string{s.CreatedUp, s.CreatedDown} {
			drop[p] = struct{}{}
			exemptions = append(exemptions, scopeExemption{Path: p, Reason: reason})
		}
	}
	if len(drop) == 0 {
		return declared, nil, nil
	}
	out := make([]string, 0, len(declared))
	for _, p := range declared {
		if _, skip := drop[p]; skip {
			continue
		}
		out = append(out, p)
	}
	return out, exemptions, nil
}

// parkForMigrationRenumberAmendment files ONE scope amendment naming exactly
// the recognized CREATED paths (operation `create`) and BLOCKS on the operator's
// decision via the backend's `?wait` long-poll.
//
// On `approved` it re-runs refreshScopeAmendments (reuse, not a parallel fold)
// so the created paths enter cfg.scopeFiles, and returns one scopeExemption per
// SUBSTITUTED DECLARED path — the "substitutes rather than adds" semantics that
// stops the #1151 scope-completeness gate demanding the stale declared path.
//
// EVERY other outcome returns (false, nil): a nil client, an empty token, a
// POST failure (including 422 amendment_budget_exhausted, 409
// stage_not_implement and 403), a fetch failure, ctx cancellation, an explicit
// `denied`, and an undecided amendment at budget expiry. The
// migration_renumber_amendment_decided event is emitted on EVERY exit path, so
// the recognition is never silent.
//
// The amendment names ONLY the recognized created paths. Any OTHER created file
// — a plain created.txt, or a migration whose slug matches no declared entry —
// is not in the request, so approval cannot fold it and the gate still fails
// the stage on it. That restriction is what keeps the gate un-widened.
func parkForMigrationRenumberAmendment(ctx context.Context, client uploadClient, cfg *config, mcpToken string, subs []migrationRenumber, logSink io.Writer) (bool, []scopeExemption) {
	if len(subs) == 0 {
		return false, nil
	}
	emitMigrationRenumberRecognized(cfg, subs, logSink)

	if client == nil || mcpToken == "" {
		emitMigrationRenumberDecided(cfg, "", "unavailable",
			"no run-bound amendment client: scope amendment not requested", logSink)
		return false, nil
	}

	paths := make([]upload.ScopeAmendmentPath, 0, len(subs)*2)
	for _, s := range subs {
		paths = append(paths,
			upload.ScopeAmendmentPath{Path: s.CreatedUp, Operation: "create"},
			upload.ScopeAmendmentPath{Path: s.CreatedDown, Operation: "create"})
	}
	amendment, err := client.RequestScopeAmendment(ctx, upload.RequestScopeAmendmentArgs{
		RunID:    cfg.runID,
		MCPToken: mcpToken,
		Paths:    paths,
		Reason:   migrationRenumberReason(subs),
	})
	if err != nil || amendment == nil {
		detail := "scope-amendment request returned no amendment"
		if err != nil {
			detail = err.Error()
		}
		emitMigrationRenumberDecided(cfg, "", "unavailable", detail, logSink)
		return false, nil
	}

	decision, detail := awaitMigrationRenumberDecision(ctx, client, cfg, mcpToken, amendment.ID)
	if decision != "approved" {
		emitMigrationRenumberDecided(cfg, amendment.ID, decision, detail, logSink)
		return false, nil
	}

	// Reuse the #961 fold verbatim. Its returned policy_event is dropped on
	// purpose: the trace bundle already shipped under #742 forward gating, so
	// the JSONL line it writes to logSink is the surviving record. The
	// substitution's own visibility rides the supplemental scope_files_exempted
	// audit row the caller passes to openPRAndShipArtifact.
	_ = refreshScopeAmendments(ctx, client, cfg, mcpToken, logSink)
	emitMigrationRenumberDecided(cfg, amendment.ID, "approved", detail, logSink)

	exemptions := make([]scopeExemption, 0, len(subs)*2)
	for _, s := range subs {
		reason := fmt.Sprintf(
			"migration renumbered %s → %s (slug %q): the declared path is pinned by a plan written before a sibling took number %s; the substituted file was folded into scope by an approved amendment",
			s.DeclaredNumber, s.CreatedNumber, s.Slug, s.DeclaredNumber)
		exemptions = append(exemptions,
			scopeExemption{Path: s.DeclaredUp, Reason: reason},
			scopeExemption{Path: s.DeclaredDown, Reason: reason})
	}
	return true, exemptions
}

// awaitMigrationRenumberDecision long-polls the amendment list until the filed
// amendment leaves `pending`, the decision budget elapses, the fetch fails, or
// the context is cancelled. Returns one of approved|denied|undecided|unavailable
// plus a short detail.
//
// THE BUDGET BOUNDS THE BLOCKED REQUEST, not just the gaps between requests
// (#2748 fix-up). Checking the deadline only after a request returns lets a
// long-poll issued just under the deadline hold the stage for another full
// migrationRenumberWaitSeconds — and for as long as the server likes if it does
// not honor `?wait` at all. So every iteration bounds its fetch BOTH ways: the
// per-request `?wait=N` hold is capped at the whole seconds remaining, and the
// request carries a context deadline at the budget so a server ignoring `?wait`
// cannot outlast it either.
//
// A fetch that fails because THAT derived deadline fired is an UNDECIDED
// amendment, not an unavailable backend: the budget is what ended the request,
// and classifying it as unavailable would report a healthy backend as broken.
// A cancellation of the PARENT context still classifies unavailable.
func awaitMigrationRenumberDecision(ctx context.Context, client uploadClient, cfg *config, mcpToken, amendmentID string) (string, string) {
	deadline := time.Now().Add(migrationRenumberDecisionBudget)
	undecided := func() (string, string) {
		return "undecided", fmt.Sprintf("no decision within %s", migrationRenumberDecisionBudget)
	}
	for {
		if err := ctx.Err(); err != nil {
			return "unavailable", err.Error()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return undecided()
		}
		fetchCtx, cancel := context.WithDeadline(ctx, deadline)
		items, err := client.FetchScopeAmendments(fetchCtx, upload.FetchScopeAmendmentsArgs{
			RunID:       cfg.runID,
			MCPToken:    mcpToken,
			WaitSeconds: boundedRenumberWaitSeconds(remaining),
		})
		cancel()
		if err != nil {
			// Classify by the CAUSE, not by the clock: a backend failure that
			// happens to land near the deadline is still an unavailable
			// backend. Only OUR derived deadline firing — with the parent
			// context still healthy, so a stage-level deadline is not being
			// mislabelled — means the amendment is genuinely undecided.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return undecided()
			}
			return "unavailable", err.Error()
		}
		for _, a := range items {
			if a.ID != amendmentID {
				continue
			}
			switch a.Status {
			case "approved":
				return "approved", a.DecisionReason
			case "denied":
				return "denied", a.DecisionReason
			}
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return undecided()
		}
		pause := migrationRenumberPollInterval
		if pause > remaining {
			pause = remaining
		}
		select {
		case <-ctx.Done():
			return "unavailable", ctx.Err().Error()
		case <-time.After(pause):
		}
	}
}

// boundedRenumberWaitSeconds is the `?wait=N` hold for one long-poll: the
// per-request cap, FLOORED to the whole seconds left in the decision budget so
// the server is never asked to hold past it. A sub-second remainder yields 0,
// which omits the query parameter entirely and makes the request return
// immediately — the loop then observes the expired deadline and reports
// undecided.
func boundedRenumberWaitSeconds(remaining time.Duration) int {
	if remaining <= 0 {
		return 0
	}
	secs := int(remaining / time.Second)
	if secs > migrationRenumberWaitSeconds {
		return migrationRenumberWaitSeconds
	}
	return secs
}

// migrationRenumberReason spells out each declared→created substitution so the
// operator can decide without opening the diff.
func migrationRenumberReason(subs []migrationRenumber) string {
	var b strings.Builder
	b.WriteString("Migration-number collision (#2748). This stage's declared scope.files pins migration number(s) a plan chose before a sibling item took them, so the correctly-renumbered file(s) are net-new out-of-scope creates. Substitutions: ")
	for i, s := range subs {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s -> %s, %s -> %s", s.DeclaredUp, s.CreatedUp, s.DeclaredDown, s.CreatedDown)
	}
	b.WriteString(". Approving folds ONLY the created path(s) into scope.files and exempts the stale declared path(s) from the scope-completeness gate. This request carries NO proof the new number is the right one — that is your call. Denying (or leaving it undecided) changes nothing: the stage fails category-B on the created-out-of-scope gate with origin untouched.")
	return b.String()
}

// emitMigrationRenumberScanSkipped is the one fail-open reason line: a
// filesystem error anywhere in the pre-park scan (the folded-path absence check
// or the recognition predicate itself) leaves the stage on today's path.
func emitMigrationRenumberScanSkipped(cfg *config, err error, logSink io.Writer) {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"migration_renumber_scan_skipped","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
		cfg.runID, cfg.stageID, err.Error())
}

func emitMigrationRenumberRecognized(cfg *config, subs []migrationRenumber, logSink io.Writer) {
	type wire struct {
		DeclaredUp   string `json:"declared_up"`
		DeclaredDown string `json:"declared_down"`
		CreatedUp    string `json:"created_up"`
		CreatedDown  string `json:"created_down"`
	}
	items := make([]wire, 0, len(subs))
	for _, s := range subs {
		items = append(items, wire{s.DeclaredUp, s.DeclaredDown, s.CreatedUp, s.CreatedDown})
	}
	payload, _ := json.Marshal(items)
	_, _ = fmt.Fprintf(logSink,
		`{"event":"migration_renumber_recognized","run_id":%q,"stage_id":%q,"substitutions":%s}`+"\n",
		cfg.runID, cfg.stageID, payload)
}

func emitMigrationRenumberDecided(cfg *config, amendmentID, decision, detail string, logSink io.Writer) {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"migration_renumber_amendment_decided","run_id":%q,"stage_id":%q,"amendment_id":%q,"decision":%q,"detail":%q}`+"\n",
		cfg.runID, cfg.stageID, amendmentID, decision, detail)
}
