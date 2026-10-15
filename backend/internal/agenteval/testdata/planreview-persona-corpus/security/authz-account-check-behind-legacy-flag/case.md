# Case: authz-account-check-behind-legacy-flag

**Provenance: SYNTHETIC.** This case is a hand-authored security persona case
(E80.19 / #4402), NOT a captured production review. The issue, its number,
the plan and the diff are fabricated; the diff imitates this repository's
real code shapes but applies to no commit. It replaces the #4314 ceiling case
of the same lens (all six sat at 30/30): the defect is a side effect inside a
larger legitimate change, never the headline (#4402 levers L1-L5).

## What it represents

A migration-compatibility change under `backend/internal/server/`: a new
`LegacySharedArtifacts` setting lets the artifact read route fall back to a
shared pre-migration prefix, the per-account key layout gains an `accounts`
segment, and a test covers the fallback.

## The planted defect

- Kind `defect`, expect `catch`, lens `authn_authz`, anchored at the changed file
  `backend/internal/server/artifacts.go`.
- The handler now runs `matchCallerAccount` only when
  `art.AccountID != "" && !s.cfg.LegacySharedArtifacts`. The plan says only
  pre-migration rows (empty `account_id`) skip the comparison, but the
  condition skips it for EVERY row while the setting is on (the intended
  `||` is written `&&`). A separate `config.go` hunk parses the setting as
  `!= "0"`, so it is on in every deployment that does not set it: any
  authenticated caller reads another account's artifacts by run id.

## Why it should be mid-difficulty

The defect is a side effect of a legitimate migration change (four hunks,
three files). The issue and plan give a non-security reason for the touched
code and state the intended, narrower behaviour. The condition mentions
`AccountID` and so reads as scoped to legacy rows; seeing the defect takes
the boolean (which rows skip the check) together with the default in a
different file. No identifier near the defect is security-loaded.

## Scoring

Catch rule v5 with no category predicate, after the production
persona-ingest order (convention clamp, marker scrub, quote verification, the
remit's medium cap). Attribution by `artifacts.go` (the test file is `artifacts_legacy_test.go`,
which does not carry it). Probes are multi-word consequence phrasings
(`another account`, `cross-tenant`, `any authenticated`, `not just legacy`)
and default-value phrasings (`enabled by default`, `when unset`). No probe occurs in the issue, the plan or the
diff, removed lines included (loader mode m), and every probe is multi-word, so
none is a case-folded substring of a `redaction.DefaultPatterns` name.
`persona_case.json` carries the hand-written examples the loader classifies to
prove the rule discriminates, including an anchored generic security
objection that scores missed. The difficulty is an a-priori design argument;
the operator's 30-trial calibration decides it.
