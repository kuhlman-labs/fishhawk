# backend/internal/scaffold

Backend mirror of [`cli/internal/scaffold`](../../../cli/internal/scaffold/README.md)
(E74.3 / #3718). It renders the three governance documents `fishhawk init`
writes beside the workflow spec — `.fishhawk/charter.md` (human-authored
SKELETON), `.fishhawk/operator.yaml` (thin overlay) and
`.fishhawk/work-management.yaml` (complete config with the `charter:` block) —
so the MCP `fishhawk_init` tool (`backend/internal/mcpserver/onboard.go`)
returns the identical file set from the backend module. The backend and CLI are
separate Go modules that cannot import one another (the module wall
`cli/internal/bridge` documents), hence the copy.

The template contract, the `Files` / `EnsureFiles` / `Options.Missing` /
`RubricIDs` API and the provider-connection rules are the CLI copy's — read its
README. This file records only what differs and what guards the mirror.

## What is mirrored, and how drift is caught

| Surface | Guard |
|---|---|
| `templates/*` (byte-identical, same file SET) | `TestScaffoldTemplateParityAcrossModules` reads `cli/internal/scaffold/templates/` over the repo root; it also requires the embedded seams to equal the on-disk files. |
| `RubricRowPattern` | `TestRubricRowParityAcrossModules` requires the CLI const verbatim (the CLI copy is itself held to `intakegroom`'s `rubricRow` source), and `TestCharterTemplateParsesWithGroomingReader` runs the real `intakegroom.ParseRubricIDs` over the shipped charter. |
| Assembler / writer | Same code. `EnsureFiles` is retained so the copies stay the same package; the backend has no caller today — `fishhawk_init` returns bytes and writes no file. |

## The one deliberate difference: no runtime work-management validation

The CLI copy's `Files` self-validates the rendered work-management bytes with
the schema-only `spec.ValidateConventionsDocument`. The backend's only
work-management validator is `workmgmt.Parse`, and this package is imported by
`backend/internal/mcpserver`, whose non-test dependency closure must never
reach `backend/internal/workmgmt` (ADR-064; `TestNoBoardReadOnMCPToolSurface`
fails naming the import chain). So `Files` here performs no work-management
validation, and the backend proof lives in TESTS, where it is stricter than the
CLI's runtime check:

- `TestWorkManagementTemplateParsesWithBackendSemantics` runs `workmgmt.Parse`
  (schema AND semantics: the mandatory trio, the provider connection, adr
  numbering, transitions->states) over every complete provider branch.
- `TestWorkManagementIncompleteBranchesFailOnlyOnTheConnection` requires every
  incomplete `github_projects` rendering to fail ONLY on the connection
  `*SemanticError`, and the same options with the connection filled to parse
  clean — so an incomplete config cannot hide another semantic defect.
- `TestBackendSemanticProofDiscriminates` proves the vehicle is not vacuous:
  a schema-valid template missing the adr numbering rule is rejected.
- `TestOperatorTemplatePassesThinnessRule` runs `operatorrole.ValidateOverlay`
  (schema + ADR-040 D1 thinness rule) per autonomy, and shows the same bytes
  plus a procedure field are refused.

Every value `Files` substitutes is an enumerated branch (provider, owner type,
owner/number presence, gitlab path), so these tests cover the rendered space.

The identifier guard on `ProjectOwner` / `GitLabProject` — untrusted input that
must not become YAML structure in either the live or the COMMENTED connection
rendering — is the same code in both copies and documented once, in
`cli/internal/scaffold/README.md` § "Identifier guard (configuration
injection)". `TestFiles_RejectsConnectionInjection` runs in both.

A third consumer should promote the templates to a canonical `docs/spec/`
document mirrored by `scripts/sync-schemas` instead of adding a third copy.
