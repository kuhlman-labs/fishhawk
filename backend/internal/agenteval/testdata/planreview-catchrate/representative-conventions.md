# Review conventions

These are the repository-specific review rules for `widgets-service`. They
supplement the standard review criteria; they do not replace them. Each rule
names what to check and why the rule exists, so a reviewer can tell a real
violation from a change that merely looks similar.

## 1. Audit categories are registered in the same change

Every audit event category string written through `audit.Append` (or any
helper that wraps it) must be added to the registry in
`internal/audit/categories.go` in the SAME change that first emits it. The
registry test `TestKnownCategoriesCoversEmittedCategories` fails the build on
an unregistered category, so a plan that adds an emitter without scoping the
registry file will fail its own verify gate.

- A plan that introduces a new audit category must list
  `internal/audit/categories.go` in its scope.
- A category that operators should see on the activity feed must also be added
  to the feed allow-list in `internal/activity/feed.go`.
- A log event name (written only to the process log, never to the audit table)
  is NOT an audit category and does not need registering.

## 2. Pin every tool fetched at CI run time

Any tool a CI workflow downloads and executes during a `run:` step — an install
script piped to a shell, an `npx` package, a `go install` of a binary — must be
pinned to an immutable version or tag. Never `latest`, `main`, `master` or a
floating major. A third-party project can change its install script or publish
a new release and break every pipeline, including the default branch, with no
change on our side.

- GitHub Action references (`actions/checkout@v4`) are the exception: they stay
  on floating major tags because Dependabot bumps them deliberately.
- When a pin is bumped, every workflow that carries the same tool must be
  bumped in the same change.

## 3. `t.Parallel()` never shares a test with `t.Setenv` or `t.Chdir`

Go panics when a test that calls `t.Setenv` or `t.Chdir` also calls
`t.Parallel()`. The packages under `internal/store` and `internal/api` run every
top-level test in parallel, so a new test that needs an environment variable or
a working-directory change belongs in a separate, non-parallel test file.

- Prefer passing configuration explicitly over reading the environment in the
  code under test; it removes the need for `t.Setenv` entirely.

## 4. Cross-module wire structs are registered with the parity check

The API server and the worker are separate Go modules that cannot import each
other. A JSON struct that one module marshals and the other decodes agrees only
by convention, so every such pair must:

- carry a `WIRE CONTRACT` marker comment on both structs, and
- be listed in `internal/wirecheck/manifest.go`, which asserts the json tags of
  both sides agree.

A struct carrying the marker but missing from the manifest fails the build. A
renamed json tag on one side only is exactly the defect this check exists to
catch.

## 5. Documentation ships in the same change

- A new HTTP route, response field or status code updates `api/openapi.yaml`
  (the source of truth) and `docs/api.md` (the human companion) in the same
  change.
- A new environment variable or command-line flag is documented in the owning
  component's `README.md`.
- A behaviour change updates every document that described the old behaviour.
  Search the repository for the old claim; one fact is often repeated in a code
  comment, a README and a design note.

## 6. Database migrations are reversible and additive by default

- Every migration under `internal/store/migrations/` ships with a matching down
  migration.
- A new column is nullable or carries a default, so a rolling deploy never
  writes a row the previous binary cannot read.
- A migration that rewrites or drops data states the rollback consequence in
  the plan's rollback section.

## 7. Error messages say what failed and how to fix it

An error returned to an operator names the input that failed, the rule it
broke and the remedy. No generic "something went wrong", and no apologies. An
internal error is wrapped with `%w` so callers can still match it with
`errors.Is`.

## 8. Tests seed bad state by construction

A test for a guard builds the bad input directly rather than calling the guard
inside its own setup, so a failure lands on the behavioural assertion rather
than on fixture setup. A test for a fail-closed branch asserts the specific
error, not merely that some error occurred.
