# runner/internal/diffcov

New-line coverage measurement for the workflow-v1.6 `diff_coverage`
constraint (ADR-059 / #1888).

The package is pure and injectable: the git invocation is a function field
(`GitRunner`), so every parser and the measurement itself test without a
real repository. Execution of the customer coverage command and emission of
the trace event live in `runner/cmd/fishhawk-runner` (`runDiffCoverageGate`);
the VERDICT lives in `backend/internal/policy`. This package only computes
the number.

## Division of labour

| Layer | Owns |
|---|---|
| `backend/internal/spec` | Parsing + validating the declaration |
| `backend/internal/server/prompt.go` | Serving the config to the runner |
| `runner/cmd/fishhawk-runner` | Running the command, emitting `diff_coverage` evidence |
| **this package** | Diff parsing, LCOV parsing, path normalization, the intersection |
| `backend/internal/policy` | The threshold verdict (authoritative) |

The runner never fails the stage on a coverage shortfall. It measures and
reports; the backend re-evaluates from the uploaded bundle and owns the
category-B verdict. This mirrors `verification_reported` (#1886).

## The added-line set (`ChangedLines`)

The diff is taken **merge-base → work tree**, the same framing
`scripts/check-coverage.py` uses:

- The **merge base** is resolved explicitly rather than via the `A...B`
  three-dot shorthand, so commits that landed on the base branch *after*
  the run branched stay out of the denominator. (`git diff A...B` is
  defined as `git diff $(git merge-base A B) B` — git-diff(1).)
- The **work tree**, not `HEAD`, because the coverage report describes the
  tree the coverage command just executed against. Diffing `HEAD` would put
  coverage and lines on two different snapshots and the measurement would be
  meaningless. The runner points this at the **throwaway checkout** the
  coverage command runs in — a clean detached checkout of the committed
  tree — so there the work tree *is* the committed tree and the
  one-snapshot property holds by construction.
- `-U0` makes every hunk header describe exactly the changed lines.

### Pinning the merge base (`MergeBase`)

`MergeBase` is exported separately so the caller can resolve the fork point
to a SHA **before** anything moves `HEAD`. The runner needs that: it
materializes the committed tree with a throwaway commit, which advances
whatever branch `HEAD` is on, so a `base_ref` naming that same branch (the
ordinary local case — `HEAD` on `main`, `base_ref: main`) would merge-base
to the throwaway commit *itself* and the measurement would see zero added
lines — a silent false vacuous pass, the exact failure mode this constraint
must never produce. Re-resolving the pinned SHA against the later `HEAD` is
a no-op, because it is an ancestor of it, so `ChangedLines` accepts either
a ref name or an already-pinned SHA.

An **empty base ref returns `ErrEmptyBaseRef` without invoking git**. The
caller resolves an omitted `base_ref` to the run's base branch first
(`resolveDiffCoverageBaseRef`, the same resolver the implement push uses);
a resolution that yielded nothing is reported as a named failure rather
than shelling out with an empty argument.

### Untracked files

`git diff` sees only **tracked** files, so a never-`git add`ed new file
would contribute zero added lines and bypass the gate outright. After the
tracked diff, `ChangedLines` enumerates untracked files
(`git ls-files --others --exclude-standard -z`, NUL-split because git
C-quotes exotic paths and a newline in a name would split one path into
two) and folds every line of each in via
`git diff --no-index -- /dev/null <path>`.

In the runner's own use the sweep finds nothing — it diffs a clean
throwaway checkout — and does not need to: a new file inside the declared
scope is part of the committed tree, and one outside it is scope drift,
excluded from the commit and correctly not attributed to the stage. The
sweep stays because the package's contract is "added lines relative to a
base in this work tree", and a dirty-tree caller must not silently lose
untracked files.

`ExecGit` therefore treats `git diff` exit status **1** as success — git
uses it to mean "differences were found", which is the normal outcome
under `--no-index`. Only status ≥ 2 is a real failure.

### Hunk-header shapes

| Shape | Meaning |
|---|---|
| `@@ -0,0 +7 @@` | The `,d` count is **omitted when it equals 1** — one added line at 7 |
| `@@ -10,0 +11,3 @@` | Three added lines, 11–13 |
| `@@ -5,3 +4,0 @@` | Pure deletion: **zero** added lines |
| `+++ /dev/null` | The file was deleted: nothing attributed |
| `--- /dev/null` | New file: its lines are added normally |

Paths containing spaces survive (the `b/` prefix is stripped positionally);
C-quoted paths are decoded, so a non-ASCII-named file is attributed to its
real name rather than to a literal key that could never match a coverage
report entry.

**The decode runs BEFORE the `b/` strip**, because git quotes the *whole*
token — the header reads `+++ "b/caf\303\251.go"`, with the opening quote
*outside* the prefix. Stripping first finds no `b/` to remove and yields the
key `b/café.go`, which matches no report path; the file then falls out of
the **denominator** (`Measure` excludes files the report never mentions), so
a fully covered file measures zero new lines — a silent vacuous PASS rather
than a visible failure. `TestChangedLinesDecodesRealGitQuotingAndIntersects`
pins the header shape against real git (tracked *and* untracked, which reach
the parser by different routes) rather than a hand-written fixture.

## LCOV parsing (`ParseLCOV`)

The parsed subset is the stable per-line grammar from the lcov/geninfo
tracefile reference:

```
SF:<path>          begins a record for one source file
DA:<line>,<hits>   one line's execution count (a trailing checksum is ignored)
end_of_record      closes the record
```

Every other record type (`TN`, `FN`/`FNDA`/`FNF`/`FNH`, `BRDA`/`BRF`/`BRH`,
`LF`/`LH`) is **ignored**, not rejected — real producers emit them and none
carry per-line data. Repeated `DA` lines for one line **sum**, matching
lcov's semantics for a file measured across several test binaries.

Malformed input returns an error wrapping `ErrParse`, never a partially
filled map. A truncated record, a non-numeric line number or hit count, a
`DA` outside any `SF`, and a report with zero `SF` records are each a report
the producer did not finish writing. A record is truncated whether it runs
off the end of the file **or** is cut short by a nested `SF:` that opens a
new record without an intervening `end_of_record` — the mid-file case fails
closed for the same reason as the end-of-file one, rather than keeping the
first record's partial line set as if it were complete. Treating one as "nothing covered" would
fail an opted-in run with a false RED — the worst failure mode for an
opt-in gate.

LCOV is the interchange format because coverage.py (`coverage lcov`),
Istanbul/nyc, cargo-llvm-cov, JaCoCo (via converter) and Go (via
gcov2lcov) all emit it. `format` is an enum so a later additive minor can
add others.

## Path normalization (`NormalizePath`) — the load-bearing part

Coverage producers spell `SF:` paths inconsistently — absolute
(`/build/repo/src/app.go`), `./`-prefixed, or carrying redundant separators
(`src//app.go`) — while the diff parser yields clean repo-relative paths.
**Intersecting the two key sets verbatim would classify covered new lines as
uncovered and fail every opted-in customer run with a false RED.** Both sides
are therefore normalized to clean, slash-separated, repo-relative form
before intersection.

An absolute path is made relative to the repo root; when that fails, the
**symlink-resolved** root is retried, because a tool running inside the
checkout reports the resolved path (on macOS `/var/...` resolves to
`/private/var/...`).

A path that cannot be placed inside the repository — absolute and outside
the root, or escaping via `..` — is returned in
`Result.UnnormalizablePaths` and **named explicitly in the evidence
reason**, never silently counted as uncovered.

## The denominator (`Measure`)

`NewLines` counts added lines the report could speak to at all. Two
exclusions, both deliberate:

- **A file the report never mentions** (a README, a generated file, a
  language the tool does not cover). Counting these would make every
  docs-touching stage fail an opted-in gate.
- **A line the report measured no statement on** (a blank line, a comment,
  a brace) — not a coverable statement.

`Percent` is `covered*100/total`, or 0 when `NewLines` is 0. The backend
compares with `>=`, so a measurement exactly **at** the threshold passes.

`NewLines == 0` is the documented **vacuous pass**: a diff that added no
coverable lines cannot be under-covered. The runner emits this as an
explicit measured-with-zero signal rather than emitting nothing — an
explicit zero is auditable, whereas absence is indistinguishable from a
runner that failed to run, which the backend treats as a violation.

### Zero that is NOT a vacuous pass (`ResolvedFiles`)

`NewLines == 0` is only a legitimate vacuous pass when the report was
*usable* — it placed into the repo and simply measured none of the stage's
added lines. It is a measurement **failure** when the exclusions consumed
the entire denominator: nothing in the report resolves into the checkout at
all. That is the field case where a coverage tool ran under a
container/build root whose absolute `SF:` paths all resolve *outside*
`repoDir`, or an instrumentation config excluded the changed package — the
zero says nothing about the stage's coverage, and reporting it as
measured-zero would hand the backend its vacuous PASS on **every** run for
an affected repo, with the only explanation buried in a `Reason` nobody
reads on a green result. That is the silent-neuter shape this constraint
exists to eliminate.

`Result.ResolvedFiles` counts the distinct report files placed inside the
repo, so the runner can tell the two zeros apart: when `NewLines == 0` **and**
(`UnnormalizablePaths` is non-empty **or** `ResolvedFiles == 0`), the runner
emits outcome `failed` naming what ran, its exit code, and that zero of the
stage's added lines could be measured — routing it through the backend's
not-measured violation, which surfaces the reason. A usable report that
happens to measure none of the stage's files (`ResolvedFiles > 0`,
no unnormalizable paths) stays the vacuous pass.

## Known limitation

A coverage percentage credits **execution, not assertion**: a vacuous test
still earns diff coverage. This mirrors ADR-059's stated limitation for the
repo-local gate and is deliberately not addressed here.
