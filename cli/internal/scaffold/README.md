# cli/internal/scaffold

Renders and writes the three governance documents `fishhawk init` adds beside
the workflow spec (E74.3 / #3718). The templates are embedded package assets
under `templates/`; they are the bytes the CLI writes into a CALLER's
repository at runtime. Nothing here writes into the Fishhawk repository's own
`.fishhawk/` tree.

| Path written | Template | Contract |
|---|---|---|
| `.fishhawk/charter.md` | `templates/charter.md` | SKELETON only: the content-contract sections (`docs/spec/work-management-v0.md`) and rubric ids `V1`–`V5`, `R1`–`R5`, `U1`–`U4`, `S1`–`S5`, every body a `<!-- fill me in -->` marker. Human-authored — no direction text is ever written. |
| `.fishhawk/operator.yaml` | `templates/operator.yaml` | Thin overlay: `spec_version`, `knob_presets.autonomy`, fill-me-in `conventions`, `work_management` pointer. No procedure field (ADR-040 D1 thinness rule). |
| `.fishhawk/work-management.yaml` | `templates/work-management.yaml` | Complete, self-sufficient config (a repo-level file REPLACES the shipped default): provider connection, `required_fields` trio, states, transitions, feature/bug/chore/adr types, `charter: {path: .fishhawk/charter.md}`. |

## API

- `Files(Options) (map[string][]byte, error)` — pure. Renders the three
  documents and fails closed on an unknown autonomy/provider/owner type, an
  unrendered `{{…}}` placeholder, work-management bytes the
  `work-management-v0` schema rejects (`spec.ValidateConventionsDocument`), or
  a charter missing any of the four rubric groups.
- `EnsureFiles(root, Options) (Result, error)` — writes each document only
  when the path is absent, one `created` / `skipped-existing` result per path
  in `Paths()` order. The existence check and the create are one
  `O_CREATE|O_EXCL` open, so there is no check-then-write window; anything
  already at the path is left untouched. **There is no force parameter**: no
  caller flag (including `fishhawk init --force`) can overwrite an existing
  charter.
- `Options.Missing()` — the work-management fields left for the operator
  (`project.owner`, `project.number`).
- `RubricIDs(markdown)` / `RubricRowPattern` — the rubric-row parser used by
  `Files`' self-check and by the `fishhawk doctor` charter rung.

## Provider connection

- `gitlab` — `gitlab: {project: <path>}` when `GitLabProject` is set, else
  `gitlab: {}` (the filing repo's own path).
- `github_projects` — a live `project: {owner, owner_type, number}` block only
  when BOTH owner and number are known. Otherwise the block is written
  COMMENTED under a `# REQUIRED — FILL ME IN (<fields>)` marker: the config is
  schema-valid but semantically incomplete, so grooming and filing fail closed
  naming the missing field — preferred over a plausible-looking number that
  would point at an unrelated board. `owner_type` defaults to `user`.

## Cross-module invariants

- **Rubric-row pattern.** `RubricRowPattern` is the same regexp source as
  `backend/internal/intakegroom/score.go`'s `rubricRow` (the grooming reader).
  `TestRubricRowParityAcrossModules` reads that file over the repo root and
  requires the backtick-quoted value verbatim, so `fishhawk doctor` and the
  grooming reader cannot disagree about what a rubric row is.
- **Backend mirror.** `backend/internal/scaffold/` carries byte-identical
  templates so the MCP `fishhawk_init` tool returns the same file set from the
  backend module (the module wall `cli/internal/bridge` documents); the
  backend copy owns the template byte-parity test and the backend-only
  semantic proofs (`workmgmt.Parse`, `operatorrole.ValidateOverlay`) the CLI's
  schema-only validator cannot make. Edit a template here and in the mirror
  together. A third consumer should promote the templates to a canonical
  `docs/spec/` document mirrored by `scripts/sync-schemas` instead of adding a
  third copy.

## Tests

`scaffold_test.go` asserts the SHIPPED content, not presence: the exact
ordered rubric id set, no direction text in any section or rubric cell, schema
validity per provider branch, the `charter.path` declaration, the overlay's
thinness, the fail-closed render paths (via the package-level template seams),
and the never-overwrite write contract over `t.TempDir()`.
