# Case: supplychain-installer-from-branch-version-pinned

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.19 / #4402), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. It replaces the #4314 ceiling case
of the same lens (all six sat at 30/30): the defect is a side effect inside a
larger legitimate change, never the headline (#4402 levers L1-L5).

## What it represents

A routine tool bump under `deploy/gate-image/`: two ARGs move in the
`Dockerfile`, `install-tools.sh` follows the helm installer to a new
location and installs check-jsonschema from wheels only, and the README's
version table is kept in step.

## The planted defect

- Kind `defect`, expect `catch`, lens `supply_chain`, anchored at the changed file
  `deploy/gate-image/install-tools.sh`.
- The helm installer script, which the build downloads and EXECUTES, is now
  fetched from the upstream repository's `main` branch instead of the
  release tag path. `DESIRED_VERSION=${HELM_VERSION}` still pins the helm
  BINARY the script installs, and `VERIFY_CHECKSUM=true` checks only the
  release tarball, so the version table and ARGs look pinned while the
  executed script is mutable upstream. The sibling check-jsonschema bump
  stays correctly pinned (`==${CHECK_JSONSCHEMA_VERSION}`).

## Why it should be mid-difficulty

The defect is a side effect of a version bump (four hunks, three files) whose
issue never mentions the installer's source. Two decoys make the change read
as pinned: the version variable still flows into the script, and a checksum
flag is added. Seeing it takes separating what is executed (the script) from
what is installed (the binary). The file is a shell script under
`deploy/gate-image/`, not a CI workflow.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `install-tools.sh`. Probes are multi-word phrasings of the
mutable source (`main branch`, `mutable ref`, `not pinned`, `commit sha`)
and the consequence (`arbitrary code`, `push to main`). `checksum` is not a
probe: the diff shows `VERIFY_CHECKSUM`. No probe occurs in the issue, the plan or the
diff, removed lines included (loader mode m), and every probe is multi-word, so
none is a case-folded substring of a `redaction.DefaultPatterns` name.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed. The difficulty is an a-priori design argument;
the operator's 30-trial calibration decides it.
