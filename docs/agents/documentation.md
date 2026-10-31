# Documentation contracts

Read when changing documentation or a source that generates it.

## Ownership and layout

- Root README and `site/` are human-facing narrative; `docs/` is structured,
  agent-consumed material. Site content never imports/globs/symlinks docs;
  GitHub prose links are allowed. Package READMEs hold behavioral contracts.
- Architecture §10 is an index: path + at most one sentence + one issue/ADR
  anchor per row, at most 1,000 characters per line. Large package READMEs become
  indexes into package `docs/*.md`; add a topic and index row, not a new monolith.
- A behavior change includes a repository-wide sweep for claims about the old
  behavior, including rationale, examples, commands and code comments. Correct
  stale claims and leave correctly conditional claims alone. Run documented
  commands only within existing authorization and egress controls.
- New lessons belong in the owning contract, skill or executable guard. Keep
  history in issues/package documentation, and leave a short actionable rule
  plus a reference where it is needed. See [context maintenance](README.md).

## Public site

[site/README.md](../../site/README.md) owns the build loop and dependency pins.
The site is unversioned and tracks main. Workflow majors are content: one
`reference/workflow-spec.md` covers v0/v1/v2 inline, with the support table on
`reference/index.md`. Frozen old majors do not get competing authoring pages.
Revisit only on the recorded triggers: numbered releases or differences too
large for inline explanation (`reference/versioning.md`, #2261).

Four pages have byte-exact generated regions: workflow-spec, plan-schema, cli,
api. Edit orientation only outside `BEGIN/END GENERATED` markers. Change the
canonical schema/OpenAPI or CLI command inventory and run
`scripts/gen-site-reference`; the CLI inventory must agree with executable
flags in both directions. [cli/README.md](../../cli/README.md) owns docgen.

The site voice and IA gates run in verify. The voice matcher uses §5 brand terms
(case-insensitive word-start/prefix matching); `trust` remains human-reviewed.
The IA checker text-matches literal sidebar slug entries, not arbitrary JS:
comments, variables, spread or autogenerate would invalidate that assumption.
Keep every non-root page reachable and every sidebar slug backed by a page.
The operator installs `.github/workflows/docs.yml` from the fenced workflow in
site/README.md and selects GitHub Actions as the Pages source; implement stages
cannot write `.github/workflows/**`.

## Schemas, REST and ADRs

- `docs/spec/` is canonical; synchronize every embedded schema copy before
  committing, then regenerate site references. Use `spec-change` and
  [change checklists](change-checklists.md).
- Workflow-v2 reuse (`defaults`/`extends`) requires `fishhawk validate`; bare
  check-jsonschema cannot resolve it. The optional secondary check is
  `fishhawk validate --emit-resolved <yaml> | check-jsonschema --schemafile docs/spec/workflow-v2.schema.json --default-filetype yaml -`.
  Emit-resolved resolves without performing all product validation; the plain
  validator remains authoritative.
- `docs/api/v0.openapi.yaml` is canonical REST; update `v0.md` alongside it and
  use the pinned Redocly command in [API guidance](../api/v0.md).
- [ADR README](../adr/README.md) owns frontmatter, record format and upkeep.
  Bodies transcribe the tracker verbatim. Update status table, index and both
  ends of supersession/amendment links; `scripts/check-adr --write-index`
  regenerates the machine index. Read unrecorded ADRs from the tracker.

## Voice and naming

[Brand foundations](../BRAND_FOUNDATIONS.md) is canonical. Product prose uses
Fishhawk; CLI/module paths use `fishhawk`. Workflow primitives are lowercase
nouns, never branded. Error messages identify the failure and remedy without
generic apologies. Avoid revolutionary, game-changing, next-generation,
industry-leading, world-class; AI-powered as a sole differentiator;
frictionless, seamless, effortless; empower; and trust as a marketing claim.
