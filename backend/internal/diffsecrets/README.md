# backend/internal/diffsecrets

The deterministic half of the diff secrets check (ADR-084 D5 / rule 5, E80.3 / [#3760](https://github.com/kuhlman-labs/fishhawk/issues/3760)). A pure package — no I/O, no logging, no clock — that scans the ADDED lines of a unified diff for credential-shaped strings and reports WHERE each hit is, never WHAT matched. The server turns the result into server-synthesized review concerns (`backend/internal/server/diff_secrets.go`, contract in `backend/internal/server/README.md` § "Diff secrets check").

## API

| Symbol | Contract |
|---|---|
| `Scan(patch, patterns) Result` | Walks the patch; one `Hit{Path, Line, Pattern}` per (added line, pattern). `Result.AddedLines` counts the `+` lines scanned. Production passes `redaction.DefaultPatterns` — the same set the runner applies to trace bytes, so a format added there is detected here too. |
| `GroupHits(hits) []Group` | Folds by (path, pattern); lines sorted and de-duplicated; groups ordered by path then pattern. |
| `CheckKey(pattern, path)` / `Group.Key()` | `diff_secrets\|<pattern>\|<path>` — the server's de-duplication key. |
| `Note(group)` | The concern note: pattern class + `path:line` list (capped at 20 locations, remainder counted; an `UnknownLine` renders `path:(line unknown)`), the provenance sentence, and the clearing instruction (remove/rotate, or a human waive with a reason). |

## Parse rules

A header/hunk state machine, one line at a time:

- `diff --git` opens a section, resets to HEADER state and seeds the path from the header's b-side. GitHub compare patches are rebuilt as `diff --git a/<p> b/<p>` plus hunks with NO `---`/`+++` lines (`githubclient.ComparePatch`), so for the fix-up re-review delta and the decomposed parent's consolidated review this is the only path source. An unquoted header with a space in the name is resolved by the equal-halves rule (a non-rename header is exactly `a/<p> b/<p>`), then by the last ` b/`.
- `---` / `+++` are headers ONLY in header state — between `diff --git` and the first `@@`. Inside a hunk, an added line whose content begins `++ ` renders `+++ …` and is CONTENT: it is scanned and does not rename the section.
- `+++ <path>` overrides the section path (git C-quoting decoded, `b/` stripped, a plain `diff -u` timestamp cut at the tab); `+++ /dev/null` keeps the git-header path.
- `@@ -a,b +c,d @@` seeds the new-side counter at `c` and enters hunk state. A MALFORMED `@@` header (its `+c` cannot be parsed) ALSO enters hunk state — or keeps it, mid-hunk — with the new-side line UNKNOWN until the next well-formed header: the `+` lines under it are still scanned and each hit records `UnknownLine` (0), which `Note` renders `<path>:(line unknown)`. Staying in header state instead would discard every added line under the header, and a missed secret is silent (carried from #3760 into E80.4 / [#3761](https://github.com/kuhlman-labs/fishhawk/issues/3761)). Pinned by `TestScan_MalformedHunkHeaderStillScansAddedLines`.
- In a hunk: `+` is scanned and advances the counter; ` ` (and an empty line — a context line whose trailing space was stripped) advances it; `-` and `\ No newline at end of file` do neither. **Removed and context lines are never scanned**: deleting a credential is the fix, not the defect.

## What never carries bytes

`Hit` and `Group` have no field that can hold matched bytes, and the path itself is passed through the same pattern set (`displayPath`), so a file NAMED like a credential is recorded redacted rather than carrying the value onto a note, an audit payload or a log line. `Note` takes only a `Group`. Pinned by `TestHit_CarriesNoBytes` and `TestScan_CredentialShapedFileNameIsRedacted`.

## Grouping and the de-duplication trade

The key is (pattern, path) — line numbers are deliberately NOT part of it, because they shift between fix-up passes and would re-raise every concern a human already waived. The cost, stated plainly: once a human waives a pattern class in a file (a known test fixture), a later DIFFERENT credential of the SAME class in the SAME file is not re-raised. `CheckKey` is injective because no pattern `Name` contains `|`; `TestCheckKey_Injective` pins that fact over `DefaultPatterns`, so a future pattern named with a `|` fails the test rather than silently colliding keys.

## Residuals

- **Detection is the pattern set.** A credential format outside `redaction.DefaultPatterns` passes. `json-password-field` matches any JSON-shaped `password`/`secret`/`token`/`api_key` key with a string value, including Go map literals in test code — each hit is a high concern only a human can clear. Tuning is a follow-up decision, not a change here.
- **Per-line matching.** A credential split across two added lines is not matched (the `authorization-bearer` regex's `\s*` cannot span the newline here).
- **A truncated patch is scanned only up to the cut.** The server records `patch_truncated` on its audit entry.
- **Test fixtures build synthetic keys at runtime** (`"ghp_" + strings.Repeat(…)`), never as literals: a literal would be flagged by this very check on this repository's own runs and could trip forge push protection.
