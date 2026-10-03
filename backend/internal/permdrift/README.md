# permdrift

The deterministic permission-drift check (ADR-084 D5 / rule 5, E80.4 / #3761). Per declared **permission surface**, a semantic extractor turns a file's bytes into a normalized grant set; `Compare` diffs base against head into widenings and narrowings. The package is pure (no I/O, no logging, no clock). The server (`backend/internal/server/permission_drift.go`) fetches base/head bytes, calls `Detect`, raises one `server_check` concern per widening, and records narrowings as an audit notice only.

## Model (`permdrift.go`)

- `Entry{Key, Value, Rank, Polarity}`; `Grants` is keyed by `Entry.Key`. `Rank` is the POWER ordinal: higher is always more power, for either polarity.
- **Polarity.** `Grant`: presence or a higher rank is more power. `Restriction`: presence LIMITS power, absence is unrestricted — a vanished Restriction is a widening.
- **Compare** (deterministic, sorted by key): in both sides → rank up widened, down narrowed, equal nothing; head only → Grant widened / Restriction narrowed; base only → Grant narrowed / Restriction widened. `Before`/`After` render `(absent)` for a missing side.
- **Wildcard subsumption** (Grants only): `<prefix>.*` covers every single-segment `<prefix>.<x>`. A head-only Grant covered by a base wildcard of rank ≥ its own is not a widening; a base-only Grant covered by a head wildcard of rank ≥ its own is not a narrowing. A wildcard is itself an ordinary key, so a new head wildcard is a widening. A remainder containing `.` is never subsumed (conservative for widenings).
- **Level vocabularies** (least → most power): actions `none<read<write`; App `none<read<write<admin`; autonomy `low<medium<high`; shell `none<restricted<unrestricted`. An out-of-vocabulary level is an extraction error (→ `parse_error`), never a guess.

## Surfaces (`surfaces.go`)

`SurfacesVersion = 1`. `DefaultSurfaces()` (severity map ratified at the E80.4 plan gate: high everywhere, medium for GitHub App events):

| id | kind | paths | severity |
|---|---|---|---|
| `gha-workflow-permissions` | `gha_workflow_permissions` | `.github/workflows/*.yml`, `*.yaml` | high |
| `github-app-permissions-template` | `github_app_permissions_json` | `docs/github-app/manifest.template.json` | high |
| `github-app-permissions-go` | `github_app_permissions_go` | `backend/internal/server/manifest.go` | high |
| `github-app-events-template` | `github_app_events_json` | `docs/github-app/manifest.template.json` | medium |
| `github-app-events-go` | `github_app_events_go` | `backend/internal/server/manifest.go` | medium |
| `mcp-tool-scopes` | `mcp_tool_scopes` | `backend/internal/server/mcpscopes.go` | high |
| `run-token-scope-grants` | `run_token_scopes` | `backend/internal/server/mcptoken.go` | high |
| `reviewer-env-allowlist` | `env_allowlist` | `backend/internal/reviewsandbox/env.go` | high |
| `fishhawk-spec-forbidden-paths` | `fishhawk_spec_forbidden_paths` | `.fishhawk/workflows.yaml` | high |
| `fishhawk-spec-escalations` | `fishhawk_spec_escalations` | `.fishhawk/workflows.yaml` | high |
| `fishhawk-spec-autonomy` | `fishhawk_spec_autonomy` | `.fishhawk/workflows.yaml` | high |
| `fishhawk-spec-stage-permissions` | `fishhawk_spec_stage_permissions` | `.fishhawk/workflows.yaml` | high |
| `permission-surface-declarations` | `surface_declarations` | `.fishhawk/permission-surfaces.yaml` | high |

Bump `SurfacesVersion` when a surface is added, removed or re-keyed. `MatchSurfaces(surfaces, path)` returns every surface (in list order) one of whose doublestar globs matches; one file can belong to several surfaces.

## Keys per kind

| kind | keys | polarity |
|---|---|---|
| actions | `jobs.<id>.<scope>` (level), `jobs.<id>.*` for read-all/write-all/DEFAULT (no block anywhere = write, conservatively); no jobs → `permissions.<scope>` | Grant, ranked |
| App JSON / Go | `default_permissions.<name>` (level), `default_events.<event>` (presence) — the JSON and Go extractors emit the SAME keys | Grant |
| spec forbidden_paths | `workflows.<wf>.stages.<stage>.forbidden_paths[<glob>]`, keyed by VALUE (reorder = no change) | Restriction |
| spec escalations | `workflows.<wf>.escalations[<canonical match>]` (+ `.max_autonomy`, `.approvals.count` negated, `.approvals.member_of[..]`, `.approvals.min_permission[..]`, `.reviewers[..]`) | Restriction |
| spec autonomy | `workflows.<wf>.autonomy`, `workflows.<wf>.actions.<class>`, per-gate `…gates[<i>].actions.<class>` where it differs from the workflow | Grant (gate tightening: Restriction) |
| spec stage permissions | `…stages.<stage>.egress[<host>]`, `…permissions.write[<glob>]`, `…permissions.shell` | Grant |
| mcp_tool_scopes | `mcp_tool.<tool>.admitted`, `…any_of.<scope>`, `…any_of.*` (empty anyOf / `mcpScopeAuthenticatedOnly`), `…run_bound_subject_ok` | Grant |
| run_token_scopes | `run_token.<scope>@<stage>` (`@any_stage` when unguarded or negated) | Grant |
| env_allowlist | `env_allow.<Var>.<NAME>` | Grant |
| surface_declarations | `surfaces[<id>]`, `…paths[<glob>]`, `…kind[<kind>]`, `…severity` (negated rank) | Restriction |

The spec extractors read the RESOLVED document from `spec.ParseBytes` (`defaults`/`extends`, `permissions.network`); none invents a rank for an absent declaration (pinned by `TestExtractSpecAutonomy`).

## Go-source extractors (`extract_go.go`)

`go/parser` + `go/ast`, **no type checking**, so they are tied to the shapes of four files. Identifier resolution, one rule for all four:

- a string literal (or a `+` of two literal-valued operands) is its value;
- an identifier naming a package-level const/var declared in the SAME FILE resolves to its value (recursively);
- an identifier that is function-local (parameter, `:=`, `var`, range variable), a predeclared name, or declared in-file as a func/type/non-string var is **unresolved**;
- an identifier declared nowhere in the file (a sibling-file const like `scopeWriteMessages`) is **kept as its name** — a stable identity, so swapping a literal for such a name still reads as one key vanishing and one appearing; a qualified `pkg.Name` likewise;
- anything else (a call, an index) is unresolved.

Per file: **manifest.go** — the values keyed `"default_permissions"` / `"default_events"` in any composite literal. **mcpscopes.go** — the package var `mcpToolScopes`; each value must be a rule composite literal (or a same-file var holding one, e.g. the sentinel) with keyed `anyOf` / `runBoundSubjectOK`. **mcptoken.go** — every function declaring `scopes := []string{...}`; each literal member and each `scopes = append(scopes, X...)` argument is keyed per `run.StageType*` selector in its enclosing `if` conditions (and `switch` case lists), resolving ONE level into a same-file predicate function's `return` expressions (`stageTypeMayMessage`); an `else`, a `default`, or a `!=`/`!` over a stage selector keys `@any_stage`. **env.go** — every package-level `[]string` var (by declared type, or by a `[]string{}` / `append` / `extend` value), expanding same-file slice identifiers and `extend(base, extra...)`; `extend` is trusted only while its body calls nothing but `make/len/cap/append/copy` and holds no literal.

### Shape guard — the fail-closed rule

`Detect` returns `Unevaluable: shape_unrecognized` (with NO changes — never a narrowing) when ANY of:

1. either side holds an **unresolved construct** inside a recognized one: an append argument, an anyOf element or a slice element that is a call or an unresolvable identifier; an anyOf / rule / permissions value built by a call; an unknown rule field; any write to `scopes` other than its literal declaration or `scopes = append(scopes, …)` (a helper call, `append` not assigned back, `&scopes`, an indexed write, an assignment inside a closure);
2. an **anchor** present at base is missing at head: `mcpToolScopes`; each manifest key (`default_permissions`, `default_events`); each string-slice var; each function declaring the `scopes` literal;
3. base yielded ≥1 entry and head yields **zero** (including a deleted file).

Each clause has an isolating arm in `TestDetect_GoSurfaceShapeLostIsUnevaluable`. `TestExtractGo_RealProductFiles` pins that the four product files currently extract with zero unresolved constructs and every anchor present — refactor one of them and update `extract_go.go` in the same change, or every later change to it raises an unevaluable concern.

The manifest Go/JSON kinds are split into a permissions part and an events part; anchors, grants and unresolved constructs are filtered by key prefix, so an unresolvable events value fails only the events surface.

## Detect

`Detect(surface, path, base, head FileSide{Content, Exists}) Result{Widened, Narrowed, Unevaluable, Detail}`. An absent side (or empty content) extracts as empty. A parse error on either side → `parse_error`; the Go shape guard → `shape_unrecognized`. `Detail` is structural (anchor name, line number, expression kind) for logs and never carries file bytes. Reason classes the server raises before `Detect` can run are also defined here (`fetch_failed`, `compare_failed`, `compare_truncated`) so `NoteUnevaluable` describes each.

## Repository extension (`.fishhawk/permission-surfaces.yaml`)

Read by the server at the run's **BASE** commit only (so a change cannot remove its own surface), and itself the `permission-surface-declarations` surface:

```yaml
version: 1
surfaces:
  - id: infra-app-permissions          # ^[a-z0-9][a-z0-9-]{0,62}$
    kind: github_app_permissions_json  # any kind above
    paths: ["infra/app.json"]          # doublestar globs, non-empty
    severity: high                     # low | medium | high; omitted = high
    description: infra app manifest
```

`ParseRepoSurfaces` decodes STRICTLY: an unknown key anywhere, a missing/unsupported `version`, or unparseable YAML is a whole-file error. An entry is REJECTED — named in `[]Rejection{Index, ID, Reason}`, never fatal — for `unknown_kind`, `bad_id`, `duplicate_id`, `product_id_collision` (an extension may only ADD), `empty_paths`, `bad_path`, `bad_severity`. `MergeSurfaces(product, accepted)` appends accepted surfaces whose id is not already present. Only accepted entries count toward the `surface_declarations` extraction, so an entry made invalid at head vanishes (a widening).

## Keys and notes

- `CheckKey(surface, path, key, after)` = `permission_drift|<surface>|<path>|<key>|<after>`, each field escaped `%`→`%25` then `|`→`%7C`, so it is injective (`TestCheckKey_Injective`). `after` is in the key so a later move to a different value raises again.
- `UnevaluableKey(surface, path)` = `permission_drift|<surface>|<path>|unevaluable` — four fields, never equal to a `CheckKey`; the reason is not in it (one unevaluable file is one concern).
- `Note` names surface, path, key, before and after, says the deterministic check (not a model reviewer) raised it and only a human can waive it with a reason. `NoteUnevaluable` names the reason class only.

## Residuals

- **GitHub Actions job rename** (identical permissions block, new job id): keys are per job id, so it reads as a narrowing of the old id plus a widening of the new — noise for a human to clear.
- **Absent permissions block = write on every scope** (the default token may be read/write per repo/org setting this file cannot see): removing a block always widens, adding one always narrows.
- **Non-stage conditions on token grants** (e.g. `agent_self_retry`) key `@any_stage`, so making such a grant unconditional is NOT detected. Likewise a stage compared by string literal rather than a `run.StageType*` selector, and an early-return guard clause (`if st != X { return }`) before an append, key `@any_stage` (over-approximation: a change still shows, as `@any_stage` noise).
- **Cross-file identifiers are kept by name**, so a change to a sibling-file constant's VALUE (e.g. `scopeWriteMessages` in `crewmessage.go`) is not seen by these surfaces; the constant's own file is not a declared surface.
- **A Go-source refactor that keeps the grants but changes their shape** raises `shape_unrecognized` (or, where the shape still resolves, spurious widenings) for a human to clear. The guard fails closed on zero entries, on a missing anchor, and on an unresolved construct — it does NOT catch a grant moved into an unrecognized shape that leaves every anchor in place and resolves cleanly (e.g. a token grant added in a different function that never declares the `scopes` literal).
- **`extend` purity** is a syntactic check (only pure builtins, no literals); it does not prove the helper concatenates in order.
