---
name: new-go-module
description: Add a new Go module to the Fishhawk multi-module workspace — go.mod, go.work, the backend Dockerfile COPY lines, Dependabot, and the docs pointer — so builds, CI and the container image all see it. Use when creating a new top-level Go module (not a new package inside an existing module).
---

# Add a Go module

There is no root `go.mod`: each top-level component is its own module listed in `go.work`. A new **package** inside `backend/`, `runner/` etc. needs none of this. Confirm the user really wants a separate module, which is warranted only when it must be versioned/tagged or depended on independently (ADR-014).

## Steps

1. **Create the module**
   ```sh
   mkdir <name> && cd <name> && go mod init github.com/kuhlman-labs/fishhawk/<name>
   ```
2. **Register it in the workspace.** Add `./<name>` to the `use ( … )` block in `/go.work`, keeping alphabetical order. `go.work` is committed; `go.work.sum` is not.
3. **Wire it into `backend/Dockerfile`.** This is the step most often missed. The image runs `go mod download` across the whole workspace, so every `go.work` module's `go.mod` must be copied first. Without it, the image build fails with `cannot load module ../<name>`.
   - First COPY phase: `COPY <name>/go.mod <name>/go.sum ./<name>/` (omit `go.sum` if the module has none yet)
   - Source phase: `COPY <name> ./<name>`
4. **Dependabot.** Add a `package-ecosystem: gomod` entry with `directory: /<name>` to `.github/dependabot.yml`, matching the existing entries. `.github/**` is human-authored and forbidden to Fishhawk implement stages, so if this runs inside a Fishhawk run, hand this step to the operator.
5. **Docs**
   - add a `<name>/README.md` holding the module's contract
   - add a pointer row in `docs/ARCHITECTURE.md` §10 "Where to look": path + ≤1 sentence + one anchor issue/ADR ref, line ≤1000 chars
6. **Verify**
   ```sh
   (cd <name> && go build ./... && go test ./... && golangci-lint run ./...)
   docker build -f backend/Dockerfile . >/dev/null && echo image-ok
   ```
   Then run the `verify-gate` skill.

## Already automatic

- `scripts/test`, `scripts/test lint`, `scripts/test coverage` and `scripts/dev` all enumerate modules from `go.work`.
- CI's `scripts/ci-test-leg` puts any new non-backend/non-runner module in the `rest` leg.
- The 80% aggregate coverage gate includes the new module from day one, so ship it with tests.
