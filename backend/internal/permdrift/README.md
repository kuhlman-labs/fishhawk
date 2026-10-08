# permdrift

The deterministic permission-drift check (ADR-084 D5 / rule 5, E80.4 / #3761). Per declared **permission surface**, a semantic extractor turns a file's bytes into a normalized grant set; `Compare` diffs base against head into widenings and narrowings. The package is pure (no I/O, no logging, no clock). The server (`backend/internal/server/permission_drift.go`) fetches base/head bytes, calls `Detect`, raises one `server_check` concern per widening, and records narrowings as an audit notice only.

## Model (`permdrift.go`)

- `Entry{Key, Value, Rank, Polarity, Except}`; `Grants` is keyed by `Entry.Key`. `Rank` is the POWER ordinal: higher is always more power, for either polarity. `Except` (a plain string, so `Entry` stays comparable) names the ONE single-segment sibling a wildcard entry does NOT cover (`""` = covers every sibling); only the Actions default token sets it (`id-token`, #3939).
- **Polarity.** `Grant`: presence or a higher rank is more power. `Restriction`: presence LIMITS power, absence is unrestricted — a vanished Restriction is a widening.
- **Compare** (deterministic, sorted by key): in both sides with a **polarity change** → one Change whatever the ranks (Restriction→Grant widened, Grant→Restriction narrowed — so a gate override flipping from "not auto" to "auto" at one key is caught); in both, same polarity → rank up widened, down narrowed, equal nothing; head only → Grant widened / Restriction narrowed; base only → Grant narrowed / Restriction widened. `Before`/`After` render `(absent)` for a missing side.
- **`keySegment`** escapes every FILE-DERIVED dotted key segment (job id, scope, manifest permission/event name, /mcp tool and scope, run-token scope, env-allow var and NAME): `%`→`%25` first, then `*`, `.` and every control/format rune (Cc/Cf/Zl/Zp, invalid UTF-8) percent-encoded byte by byte. A file therefore cannot forge a segment boundary (job `a.b` + scope `c` vs job `a` + scope `b.c`) or mint its own wildcard (a manifest permission named `*`). The extractors' own wildcard keys keep the literal `.*`; bracketed values (globs, hosts, canonical matches) end in `]` and are not escaped.
- **Wildcard subsumption** (Grants only): `<prefix>.*` covers every single-segment `<prefix>.<x>` except the one its `Except` names. A head-only Grant covered by a base wildcard of rank ≥ its own is not a widening; a base-only Grant covered by a head wildcard of rank ≥ its own is not a narrowing. A wildcard is itself an ordinary key, so a new head wildcard is a widening. A remainder containing `.` is never subsumed (conservative for widenings). The carve-out is pinned by `TestCompare`'s `except carve-out` rows.
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
| actions | `jobs.<id>.<scope>` (level); `jobs.<id>.*` for read-all (read) and write-all (write, PLUS an explicit `jobs.<id>.id-token` = write); DEFAULT (no block anywhere) = `jobs.<id>.*` write with `Except: id-token` — the default `GITHUB_TOKEN` never carries id-token under either repo/org setting, so adding `id-token: write` or `write-all` to a block-less job widens `jobs.<id>.id-token` and write-all → block-less narrows it (`TestExtractActions_DefaultTokenExcludesIDToken`); no jobs → `permissions.<scope>` (`permissions.*` + `permissions.id-token` for write-all) | Grant, ranked |
| App JSON / Go | `default_permissions.<name>` (level), `default_events.<event>` (presence) — the JSON and Go extractors emit the SAME keys | Grant |
| spec forbidden_paths | `workflows.<wf>.stages.<stage>.forbidden_paths[<glob>]`, keyed by VALUE (reorder = no change) | Restriction |
| spec escalations | `workflows.<wf>.escalations[<canonical match>]` (+ `.max_autonomy`, `.approvals.count` negated, `.approvals.member_of[..]`, `.approvals.min_permission[..]`, `.reviewers[..]`) | Restriction |
| spec autonomy | `workflows.<wf>.autonomy`, `workflows.<wf>.actions.<class>`, per-gate `…gates[<i>].actions.<class>` where it differs from the workflow | Grant (gate tightening: Restriction) |
| spec stage permissions | `…stages.<stage>.egress[<host>]`, `…permissions.write[<glob>]` (Grant per value); `…egress` on a NON-acceptance stage, `…permissions.write` when a list is declared, `…permissions.shell` when declared below `unrestricted` (ranked) | Restriction for the list/posture keys (absence model below) |
| mcp_tool_scopes | `mcp_tool.<tool>.admitted`, `…any_of.<scope>`, `…any_of.*` (empty anyOf / `mcpScopeAuthenticatedOnly`), `…run_bound_subject_ok` | Grant |
| run_token_scopes | `run_token.<scope>@<stage>` (`@any_stage` when unguarded or negated) | Grant |
| env_allowlist | `env_allow.<Var>.<NAME>` | Grant |
| surface_declarations | `surfaces[<id>]`, `…paths[<glob>]`, `…kind[<kind>]`, `…severity` (negated rank) | Restriction |

The spec extractors read the RESOLVED document from `spec.ParseBytes` (`defaults`/`extends`, `permissions.network`); none invents a rank for an absent declaration (pinned by `TestExtractSpecAutonomy`).

**Stage-permission absence model.** `permissions` is declaration-only with no default resolver (`backend/internal/spec`), so an absent shell posture, write list or non-acceptance egress list is NO LIMIT: each declaration is a Restriction and removing it widens (`shell: none` removed, `none`→`restricted`, a write list or non-acceptance egress list removed, the whole `permissions` block removed). Declaring `shell: unrestricted` emits nothing (same as absence). An ACCEPTANCE stage's egress is enforced by the runner's default-deny proxy (`runner/internal/egressproxy` `BuildAllowlist`: absent = built-ins only), so removing its hosts stays a narrowing. A declared-but-EMPTY `write: []` / `target_hosts: []` is rejected by the workflow-v2 schema (`minItems: 1`; `permissions: {}` by `minProperties: 1`), so `spec.ParseBytes` fails and the surface is an unevaluable `parse_error` — never read as "no limit".

## Go-source extractors (`extract_go.go`)

`go/parser` + `go/ast`, **no type checking**, so they are tied to the shapes of four files. Identifier resolution, one rule for all four:

- a string literal (or a `+` of two literal-valued operands) is its value;
- an identifier naming a package-level const/var declared in the SAME FILE resolves to its value (recursively);
- an identifier that is function-local (parameter, `:=`, `var`, range variable), a predeclared name, or declared in-file as a func/type/non-string var is **unresolved**;
- an identifier declared nowhere in the file (a sibling-file const like `scopeWriteMessages`) is **kept as its name** — a stable identity, so swapping a literal for such a name still reads as one key vanishing and one appearing; a qualified `pkg.Name` likewise;
- anything else (a call, an index) is unresolved.

Per file: **manifest.go** — the values keyed `"default_permissions"` / `"default_events"` in any composite literal. **mcpscopes.go** — the package var `mcpToolScopes`; each value must be a rule composite literal (or a same-file var holding one, e.g. the sentinel) with keyed `anyOf` / `runBoundSubjectOK`. **mcptoken.go** — every function declaring `scopes := []string{...}`; each literal member and each `scopes = append(scopes, X...)` argument is keyed per `run.StageType*` selector in its enclosing `if` conditions (and `switch` case lists), resolving ONE level into a same-file predicate function's `return` expressions (`stageTypeMayMessage`); an `else`, a `default`, or a `!=`/`!` over a stage selector keys `@any_stage`; an `||` with an operand carrying NO stage selector (`|| true`, `|| other.Pred(st)`, a two-level same-file predicate) keys `@any_stage`; a case ending in `fallthrough` carries its guard into the next case (the last NON-EMPTY statement, a label unwrapped: `fallthrough;;` and `L: fallthrough` carry too). **env.go** — every package-level `[]string` var (by declared type, or by a `[]string{}` / `append` / `extend` value), expanding same-file slice identifiers and `extend(base, extra...)`; `extend` is trusted only while its body calls nothing but `make/len/cap/append/copy` and holds no literal.

### Shape guard — the fail-closed rule

`Detect` returns `Unevaluable: shape_unrecognized` (with NO changes — never a narrowing) when ANY of:

1. either side holds an **unresolved construct** inside a recognized one: an append argument, an anyOf element or a slice element that is a call or an unresolvable identifier; an anyOf / rule / permissions value built by a call; an unknown rule field; any write to `scopes` other than its literal declaration or `scopes = append(scopes, …)` (a helper call, `append` not assigned back, any `append` to a reslice such as `append(scopes[:0], …)` (it writes the backing array in place), `&scopes` or the address of an element or reslice, bare or as a call argument (`mutate(&scopes)`, `p := &scopes[0]`), an indexed write, `copy(scopes, …)`, an alias `alias := scopes` / `var alias = scopes[:]`, an assignment inside a closure); OR any **write to a package-level var of the file outside its own declaration** (the package-var write sweep below);
2. an **anchor** present at base is missing at head: `mcpToolScopes`; each manifest key (`default_permissions`, `default_events`); each string-slice var; each function declaring the `scopes` literal;
3. base yielded ≥1 entry and head yields **zero** (including a deleted file).

**Package-var write sweep** (`sweepPackageVarWrites`, #3939 F6). Every Go extractor ends with a file-wide sweep: the extractors read grants from a var's DECLARATION literal only, so an `init()`, a helper or a closure mutating that var would leave base and head extracting identical grants. The tracked set is EVERY package-level var the file declares (less `_`). Inside every function, method and `init` body — and every function literal anywhere, including one inside a package var initializer — it records one Unresolved (`line N: write to package var X outside its declaration`, `… alias of package var X`, …) for: an assignment (any token but `:=`) whose left side is rooted at a tracked name through paren/index/selector/deref/slice (`X = …`, `X[k] = …`, `X.f = …`, `X += …`); an increment/decrement rooted at one; a `for … = range` key or value rooted at one; `&` of a rooted expression; builtin `copy` (destination), `delete`/`clear` (first argument) or `append` (first argument) rooted at one; and an ALIAS — a tracked name or a slice expression of it bound to a DIFFERENT name by `:=`, `=` or `var` inside a body. Package-level initializer expressions get the call and address rules too (`var _ = copy(X, …)`, `var p = &X`), except that `append` to a BARE tracked name stays allowed there (`var Y = append(X, …)`, a recognized env-allow shape that cannot overwrite X's visible elements). For the split manifest file the construct is recorded under BOTH `default_permissions.` and `default_events.`, so each manifest surface fails on its own part (`TestDetect_ManifestPackageVarWriteFailsBothParts`). Reads stay resolved: index/selector reads, `range X` with `:=`, `len`/`cap`, a method call on X, and passing X by value. One row per shape plus read-only controls: `TestExtractGo_PackageVarWriteSweep`; on the real `env.go` / `mcpscopes.go`: `TestDetect_PackageVarMutatedOutsideDeclaration`.

Each clause has an isolating arm in `TestDetect_GoSurfaceShapeLostIsUnevaluable`. `TestExtractGo_RealProductFiles` pins that the four product files currently extract with zero unresolved constructs and every anchor present — refactor one of them and update `extract_go.go` in the same change, or every later change to it raises an unevaluable concern.

The manifest Go/JSON kinds are split into a permissions part and an events part; anchors, grants and unresolved constructs are filtered by key prefix, so an unresolvable events value fails only the events surface.

## Detect

`Detect(surface, path, base, head FileSide{Content, Exists}) Result{Widened, Narrowed, Unevaluable, Detail}`. An absent side (or empty content) extracts as empty. A parse error on either side → `parse_error`; the Go shape guard → `shape_unrecognized`. `Detail` is structural (anchor name, line number, expression kind) for logs and never carries file bytes. Reason classes the server raises before `Detect` can run are also defined here (`fetch_failed`, `compare_failed`, `compare_truncated`, `commit_missing`, `rename_source_unknown`, `extension_parse_error`, `surfaces_ref_unresolved` — the run's recorded base commit could not be resolved, so the extension was not read on that pass and every surface it declares went unchecked) so `NoteUnevaluable` describes each; what raises each is the server README's fail-closed table.

## Repository extension (`.fishhawk/permission-surfaces.yaml`)

Read by the server at the RUN's recorded base commit on EVERY pass — PR-open, fix-up and conflict-resolution alike, never at an agent-authored pass base (#3939 F3) — so a change cannot remove its own surface; it is itself the `permission-surface-declarations` surface. The per-trigger and per-run-kind ref rule, the `surfaces_ref` payload field and the `surfaces_ref_unresolved` fail-closed row are the server README's (`backend/internal/server/README.md`):

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

- `CheckKey(surface, path, key, after)` = `permission_drift|<surface>|<path>|<key>|<after>`. Each field is escaped (`escapeKeyField`): `%`→`%25` first, then `|` and every control/format rune (Cc/Cf/Zl/Zp: newline, bidi override, zero-width, U+2028/U+2029) and invalid UTF-8 byte percent-encoded — so it is injective and no control rune reaches a stored key (`TestCheckKey_Injective`, `TestCheckKey_ControlFreeAndBounded`). An escaped field over 256 bytes is replaced by `%H` + hex SHA-256 of the raw field (an escaped field never holds `%H`, so a digest never equals one). `.` is not escaped at this level (it has no meaning in a check key); the file-derived `.` is escaped earlier by `keySegment`. `after` is in the key so a later move to a different value raises again.
- `UnevaluableKey(surface, path, head)` = `permission_drift|<surface>|<path>|unevaluable@<head>` — four fields, never equal to a `CheckKey`. `head` is the resolved commit SHA, so a waived/deferred unevaluable row suppresses only a repeat at the same commit; the server passes `""` when it cannot resolve one, and then only an OPEN row suppresses. The reason is not in it (one unevaluable file at one head is one concern). Changing the format re-keys pre-existing rows once (a still-unevaluable file raises one duplicate on its next check).
- `Display(s)` renders a file-derived value for a note or payload: Cc/Cf/Zl/Zp runes and invalid UTF-8 → U+FFFD, cut at 200 bytes on a rune boundary with `…[truncated]` (`TestDisplay`).
- `Note` names surface, path, key, before and after (each through `Display`), says the deterministic check (not a model reviewer) raised it and only a human can waive it with a reason. `NoteUnevaluable` names the reason class and the `Display`ed path only.

## Residuals

- **GitHub Actions job rename** (identical permissions block, new job id): keys are per job id, so it reads as a narrowing of the old id plus a widening of the new — noise for a human to clear.
- **Absent permissions block = write on every scope EXCEPT `id-token`** (the default token may be read/write per repo/org setting this file cannot see, and never carries id-token under either): removing a block always widens, adding one always narrows. A scope GitHub later adds whose PERMISSIVE default is `none` would be over-approximated as default-granted, so explicitly granting it on a block-less job would be subsumed (missed) until it is added to the carve-out; the carve-out is a single scope by design (`Entry.Except`).
- **Non-stage conditions on token grants** (e.g. `agent_self_retry`) key `@any_stage`, so making such a grant unconditional is NOT detected. Likewise a stage compared by string literal rather than a `run.StageType*` selector, and an early-return guard clause (`if st != X { return }`) before an append, key `@any_stage` (over-approximation: a change still shows, as `@any_stage` noise).
- **Cross-file identifiers are kept by name**, so a change to a sibling-file constant's VALUE (e.g. `scopeWriteMessages` in `crewmessage.go`) is not seen by these surfaces; the constant's own file is not a declared surface.
- **A Go-source refactor that keeps the grants but changes their shape** raises `shape_unrecognized` (or, where the shape still resolves, spurious widenings) for a human to clear. The guard fails closed on zero entries, on a missing anchor, and on an unresolved construct — it does NOT catch a grant moved into an unrecognized shape that leaves every anchor in place and resolves cleanly (e.g. a token grant added in a different function that never declares the `scopes` literal).
- **`extend` purity** is a syntactic check (only pure builtins, no literals); it does not prove the helper concatenates in order.
- **`scopes` passed BY VALUE** (`s.issue(scopes)`, `Scopes: scopes`) stays allowed because the real handler does it; a callee or struct field that then WRITES the shared elements is not followed. `&scopes`, `mutate(&scopes)`, `&scopes[0]`, `&scopes[i:j]`, an `append` to a reslice (`append(scopes[:0], …)`), an alias and `copy(scopes, …)` are unresolved (fail closed); the `copy` arm is pinned on the REAL `mcptoken.go` by `TestDetect_RunTokenCopyIntoScopesRealFile` (#3939 F5).
- **Package-var writes from a SIBLING file** are not seen: the sweep reads one file, so `BaseAllow = append(BaseAllow, …)` in another file of `reviewsandbox` (or `mcpToolScopes[k] = …` elsewhere in `server`) is a miss. Likewise a package var passed BY VALUE (a call argument, a return value, a composite field) to a callee that writes the shared map or backing array, a method call on a package var (an implicit `&X` for a pointer-receiver method), and a STRUCT copied out of a map and written back through a sibling-file helper are not followed.
- **A function-local that SHADOWS a tracked package var** (`BaseAllow := …` inside a function, then `BaseAllow[0] = …`) is flagged as a package-var write — noise, never a miss. So is any package var of a surface file reassigned for an unrelated reason (the sweep tracks every var, not only the ones a grant is read from); `TestExtractGo_RealProductFiles` catches that in-loop.
- **Run-base fallback** (owned by the server; see its README). A non-child run with no readable `pull_request_opened` entry (e.g. a decomposed PARENT) reads the extension at `run.DocumentBaseCommit`, the default-branch head pinned at admission. That is not the run's own base when the run was based on a non-default branch or the default branch moved before checkout, so a surface declared only at the real base can be missed (#3902). A decomposed CHILD reads at its slice cut point (the `child_pushed` `base_sha`), which for a later wave includes producer slices' integrated changes; the consolidated trigger's cumulative base → head check is the backstop for a declaration a producer slice removed. When no ref resolves the pass records `surfaces_ref_unresolved` and reads no extension.
- **`Display` neutralises control and format runes only.** Single-line inline content survives it: a file-derived key, glob or path can still put up to 200 bytes of markdown (link or image syntax) or instruction-like text into a concern note, which the operator, a fix-up agent and later review rounds read. The note marks the values as file-derived and the check as deterministic; it does not quote or code-span them.
- **The `||` rule over-approximates**: a stage-independent operand that is in fact false still keys `@any_stage` — noise, never a miss.
- **First declaring a write list or non-acceptance egress list** reads as one narrowing (the new restriction) plus one widening per glob/host; a rename inside one Restriction-polarity glob surface reads as a widening at the source.
