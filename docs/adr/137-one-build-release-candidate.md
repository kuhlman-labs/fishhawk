---
id: ADR-137
title: "One build produces the release candidate (amends ADR-073)"
status: accepted
date: 2026-11-04
issue: https://github.com/kuhlman-labs/fishhawk/issues/4926
supersedes: []
superseded_by: []
amends: ["ADR-073"]
applies_to: [".github/workflows/backend-build.yml", ".github/workflows/backend-release.yml", "backend/internal/version/version.go"]
---

# ADR-137: One build produces the release candidate (amends ADR-073)

**Status: accepted (ratified 2026-11-04).**

## Context

Prepared at the Captain's request on 2026-11-04.

This record amends ADR-073 Decision §4 on one question: which build produces the release candidate. Bare workflow names are under `.github/workflows/`.

No build today yields a digest that is both qualified and later published:

- `backend-build.yml:32` builds main heads only, stamps `VERSION=sha-<commit>` (`:78`), pushes `:sha-<full>` (`:59`) and signs by digest (`:88-95`). It publishes no Release.
- `backend-release.yml:229` rebuilds at a `backend/v*` tag (`:35-38`) with the tag's version (`:237`). One run builds, signs and creates the GitHub Release (`:285-286`), so nothing can qualify in between, and a published Release is immutable (`:24-27`).
- ADR-073 §4 freezes both: "They stay exactly as they are." (`docs/adr/073-kubernetes-helm-single-deployment-substrate.md:97`).

Two unratified proposals need one build: #4897 D4 ("Build and sign a candidate once") and #4898 D1 ("A rebuild is a different candidate") with D8. Neither makes this amendment.

## Options

**A. The main-head image is the candidate; the tag-driven Release publishes that digest without rebuilding.** Qualifying changes no workflow: `:sha-<full>` already exists, signed. The Release's build step (`backend-release.yml:227-242`) becomes a tag-by-digest step, as `backend-build.yml:110-112` does for `:main`. Costs:
- The published image reports version `sha-<commit>` on `/healthz` (`backend/internal/server/handlers.go:426`) and loses the version label (`backend-release.yml:224`).
- The runner floor would switch off. A `sha-` stamp is not release-shaped (`backend/internal/version/version.go:47-49`), so it "stays `"dev"`, meaning no enforcement" (`backend/internal/version/README.md:37`; `runner/cmd/fishhawk-runner/main.go:743`). This needs a product change (D2).
- A tag needs an image at its exact commit. `publish-backend` runs only on main pushes that change image inputs (`ci.yml:476-486`), so a docs-only head or a backport commit has none.
- `:sha-<full>` is mutable; re-running the main build pushes to it again (`backend-build.yml:59`, `:74-75`), so the Release does not read it (D1).

**B. Split the tag workflow: build, stop for qualification, then publish.** Meets the build-once rule and keeps the stamp, label and floor with no product change. Costs:
- The `release` job (`backend-release.yml:175-303`) splits, a new gate holds the run, and binaries handed across the wait expire at `retention-days: 7` (`:173`).
- The version is in the bytes (`:237`), so the build under test must already be final, not `-rc.N`. A failed candidate leaves a `backend/vX.Y.Z` tag, also a Go module tag (`:30-31`), to delete or burn.
- No main-head environment would have run the candidate's digest.

**C. Keep §4 as it stands.** No cost today: no production deployment exists (`deploy.yml:11-14`, `.fishhawk/workflows.yaml:1281-1282`). §4 does not stop anyone qualifying a main-head image; it stops the published `:vX.Y.Z` carrying those bytes, so ADR-073 §2's "deploys from a `backend/v*` tag" (its `:79`) would deploy a rebuild nobody qualified. Under #4897 and #4898 as proposed, this repository qualifies no candidate.

## Recommendation

**Option A.** It changes least and meets the build-once rule as #4897 and #4898 propose it: `backend-build.yml` needs no edit, and `backend-release.yml` changes only in the ways the ADR-073 §4 paragraph names (D1's replacement build step and the moved `Install cosign` step). It preserves candidate bytes without rebuilding: an RC and final tag carry one digest only when both explicitly select the same candidate digest. A main-build rerun may produce a different candidate at the same commit; that candidate cannot inherit the earlier candidate's qualification. B gets the version string right but needs a job split, a new gate and a tag ahead of qualification. A's costs are that string, the D2 product change and bump-procedure change, and an image at the tagged commit; `git_sha` and the digest still identify the image.

**D1. The candidate is a `backend-build.yml` image, identified by digest; the Release publishes it and builds no image.** The build step of `backend-release.yml` becomes: take the digest the operator named for this tag; refuse unless that digest exists in the registry, carries a cosign signature whose certificate identity is exactly this repository's `.github/workflows/backend-build.yml@refs/heads/main` with issuer `https://token.actions.githubusercontent.com` (not the documented `backend-(build|release)\.yml@.*` regexp at `.github/release-notes/backend.md:24`, which anchors neither repository nor ref), and whose certificate commit claim equals the tagged commit (the `org.opencontainers.image.revision` label, `backend-build.yml:65`, may be checked in addition, never instead); apply the `:vX.Y.Z` tag to it, and `:latest` only when the tag is a final release (the existing `backend-release.yml:218` prerelease guard stands). `:sha-<full>` is not consulted, so a later re-run of the main build cannot strand a qualified digest; the digest stays pullable only while the registry keeps untagged versions, which this record assumes and no workflow in this repository removes.

The tag is pushed after qualification and before any production deployment. On a fresh attempt, a missing image, absent name, missing signature or commit mismatch creates neither a Release nor a `:vX.Y.Z` tag. On retry, existing publication state is inspected and preserved: an existing `:vX.Y.Z` tag must resolve to the bound candidate digest, conflicts refuse, and a published immutable Release is not rewritten. Failures after retagging remain explicit partial publication and require reconciliation.

A tag push carries no inputs (`backend-release.yml:35-38`). The channel for the name is deferred to the operator-authored workflow change, with one constraint: the workflow attributes the name to the event actor that pushed the tag and never reads it from the tag object's tagger field, and it reads the name at the tagged ref, not back from the registry; an annotated tag message is a candidate channel. Who may push a `backend/v*` tag is the forge's tag-protection rule, outside this record. Tag text is parsed only as data; a retry consumes the same binding.

**D2. A `sha-<commit>` stamp carries the runner floor.** `backend/internal/version/version.go` resolves the committed floor for a `Version` of exactly `sha-` followed by 40 lowercase hex digits (what `backend-build.yml:78` stamps) as for a release-shaped one, by widening `releaseVersionRE` (`version.go:49`), the one predicate that `resolveMinRunnerVersion` (`:64`) and `TestMinRunnerVersion_MatchesBuildStamp` (`version_test.go:89`) both consult; the comments at `version.go:47-49` and `:51`, and `backend/internal/version/README.md:35` and `:37`, change with it; `backend/internal/version/version_test.go:54`'s `main image` case (`sha-0123abc`, a 7-hex stamp) keeps `dev` as the control for any other `sha-` shape, and a new case stamping `sha-` plus 40 hex digits expects the floor; `ci-<sha>`, any other `sha-` shape and `dev` are unchanged. This lands before D1's workflow change, or published images lose enforcement. A release-shaped stamp at `backend-build.yml:78` instead would edit the frozen workflow and report a version that does not exist.

The bump procedure therefore changes (`backend/internal/version/README.md:43` changes with D2): `min-runner-version` may name only a `runner/v*` tag that already exists, and the runner release is cut before the backend PR that bumps the floor merges. No test enforces the rule; it is procedural, and the floor committed when D2 lands is checked against it before D2 merges, because the next `:main` image advertises it.

**ADR-073 §4.** The amendment changes one sentence, "They stay exactly as they are." (`:97` of that record), and its repeat, "stay as they are" (`:111`). After it, `backend-build.yml` stays exactly as it is. In `backend-release.yml`, move the existing `Install cosign` step (`:244-245`) before D1's replacement build step; that replacement keeps `id: build` and emits the validated digest, so the sign and SBOM steps (`:247-260`) consume it unchanged. These are the permitted workflow changes, plus the header comment of `backend-release.yml` (`:1-31`), which describes the replacement step; `backend-build.yml`'s header (`:7-12`) is left as is, since the file stays exactly as it is. The rest of that record's `:97` stands, and its lead 'Unchanged: the image supply chain' (and the lead of `:111`) is read as unchanged by the substrate decision, not by this record: both still "publish cosign-signed images to ghcr and are substrate-independent". ADR-073 §2 stands as written, except that this amendment adds one reading to it: the `backend/v*` tag it names a qualified digest, and is pushed after qualification. A later amendment of §2, such as the one #4897 proposes, leaves that ordering in force. Production still "deploys from a `backend/v*` tag" (that record's `:79`); under this record that tag names the qualified digest, so the tag is pushed after qualification and before any production deployment. That ordering governs publication, not dispatch, and stands even if a later record retires §2's tag trigger. ADR-073's body is not edited; the record writer adds `amended_by` to its front matter only (ADR-113 D3), and its status does not change.

**Signing stays** keyless and by digest ("the cosign signing posture", ADR-073 `:111`). The Release's sign step (`backend-release.yml:247-252`) stays and adds a signature to the same digest, which does not change it (`backend-build.yml:93-94`); the provenance check runs first: before tagging, D1 requires `cosign verify` of the digest against the certificate identity and issuer D1 pins to pass, so the Release never signs or tags bytes that build did not sign. The documented check accepts either identity (`.github/release-notes/backend.md:24`, `:30`) and is not this check; Validation 3 verifies against D1's identity alone. The Release's signature is additive and changes no bytes, so it satisfies a build-once rule as #4897 proposes it; this record decides that the step stays.

**Other components are out of scope.** §4 does not name `runner-release.yml:35`, `mcp-release.yml:37` or `cli-release.yml:32`. They publish binaries at a tag with no main-head build to reuse, so Option A does not transfer. A candidate under this record is the `fishhawkd` image alone, not the binaries the Release builds at the tag (`backend-release.yml:125-152`) or the chart, which no workflow packages. #4898 D1 proposes "exact component artifacts" and a "deployment package/chart digest". If a candidate needs any of these, this record does not supply them; the Captain must decide it, in #4898's component list, with a further record for those workflows.

**Authorship and authority.** The workflow change is operator-authored and human-led: implement stages cannot write `.github/workflows/**` (`.fishhawk/workflows.yaml:746-747`, `AGENTS.md:10-12`). D2 is ordinary product work. This record enables no production deployment, creates no target and qualifies no digest.

## Decision

**Accepted 2026-11-04** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver and hashes in the ratification comment). **Option A**, i.e. D1 and D2 as stated in the Recommendation, decided now. It amends ADR-073 §4 as its §4 paragraph states; ADR-073's body is not edited. This record enables no production deployment, creates no target and qualifies no digest.

## Consequences

- **Subject.** `.github/workflows/backend-build.yml` stays the image build; `.github/workflows/backend-release.yml` publishes its digest; `backend/internal/version/version.go` gives it the floor.
- **Costs.** Main-head images enforce the committed floor from the commit that lands D2, and each later floor from the commit that bumps it (`backend/internal/version/README.md:43`). That covers the `:main` image ADR-073 §2 auto-deploys to `dev` and every install on the rolling `main` tag or a `sha-` tag, including the Helm chart default (`deploy/helm/fishhawk/Chart.yaml:19-21`), where a runner older than the floor exits 3. That is why D2 changes the bump procedure. One Release carries a tag-stamped binary (`backend-release.yml:136`) and a `sha-` stamped image (`backend-build.yml:78`).
- **Residuals.** A tag push runs the workflow file at the tagged commit, so a tag on an older commit still rebuilds. D1 checks the named digest's signature and commit and cannot see qualification; the operator's record says which digest qualified. A refusal comes after the tag push: the pushed `backend/vX.Y.Z` tag, also a Go module tag (`backend-release.yml:30-31`), remains and is deleted or burned, as under Option B. A release tag therefore points only at a main commit whose push ran `publish-backend` (`ci.yml:476-481`); a docs-only or backport head has no image at its exact commit and is refused. `scripts/ci-image-current`'s tree-equality rule for `:main` promotion is not adopted here: D1 binds the certificate commit claim to the tagged commit exactly, which is what makes Validation 1's `git_sha` check hold. An `-rc.N` tag publishes an immutable Release and locks its tag (`backend-release.yml:24-27`, `:58-59`), so one naming a digest that later fails qualification is burned the same way; D1's ordering avoids this only where qualification precedes the push. `fishhawk doctor` also compares the CLI version against this runner floor and may begin warning for older CLIs (`backend/internal/version/README.md:47`); that existing coupling does not establish a separate CLI compatibility requirement. The committed floor is `v0.1.0` and no `runner/v*` release exists yet, so the first published runner release must be at least `v0.1.0`; until one exists, `fishhawk doctor` on a `:main` install may warn on an older CLI.
- **Record metadata.** amends: ["ADR-073"]; supersedes: []; applies_to: [".github/workflows/backend-build.yml", ".github/workflows/backend-release.yml", "backend/internal/version/version.go"]
- **Related:** ADR-073 (amended), ADR-051 (its 2026-10-16 amendment describes the tag-driven release workflows and the immutable Release; unchanged by this record), ADR-113. #4897 and #4898 are unratified proposals. This record depends on neither: until a stage target exists, the named digest is the one the operator chooses.

### Validation

1. A `backend/v*` tag at a main head with an image publishes `:vX.Y.Z` with the named digest and builds no image, and `/healthz` `git_sha` on the published image equals the tagged commit (`backend/internal/server/handlers.go:427`).
2. On a fresh attempt, a missing image, absent name, unsigned image, image from another builder or image for another commit leaves no Release and no `:vX.Y.Z`. A re-run of the main build that moves `:sha-<full>` does not stop a named digest publishing. On a retry, an existing `:vX.Y.Z` on another digest refuses and a published Release is not rewritten.
3. `cosign verify` against this repository's `.github/workflows/backend-build.yml@refs/heads/main` certificate identity alone, with issuer `https://token.actions.githubusercontent.com`, passes on the published tag.
4. A build stamped with `sha-` followed by exactly 40 lowercase hex digits advertises the committed floor on `/healthz`, and a below-floor runner exits 3 (`backend/internal/version/README.md:45`). Short, overlong, uppercase and non-hex `sha-` stamps, plus `ci-<sha>` and `dev`, still advertise `dev`; retain the existing short-SHA negative fixture alongside the full-SHA positive case. `scripts/test-release-ldflags` r9 (`:296-297`) gains a third arm stamping `sha-` plus 40 hex digits and expecting the floor; `TestMinRunnerVersion_MatchesBuildStamp` (`backend/internal/version/version_test.go:87-96`), the one test r9 runs, derives its expectation from the same predicate D2 widens, so it expects the floor for that stamp too.
5. A final tag moves `:latest` to the named digest; an `-rc.N` tag leaves `:latest` where it is.
6. One rehearsal ahead of the first publishing tag runs D1's checks, the same commands, against a real `backend-build.yml` digest outside `backend-release.yml` (by hand or in a separate non-publishing workflow file) and publishes nothing: no `backend/v*` tag, `:vX.Y.Z` tag, signature or Release. `backend-release.yml` gains no rehearsal trigger; the permitted changes to it stay those the ADR-073 §4 paragraph names.
