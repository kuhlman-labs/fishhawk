/*
 * Client-side audit-chain verification over one page of the compliance
 * export (Export v1, ADR-054 / #1604), for the repo Record tab
 * (E40.6 / #1718). PURE: no React, no `api` import, no fetch — it folds
 * an already-parsed body, so its tests are plain unit tests.
 *
 * WHAT IT CHECKS, and why the set stops where it does:
 *
 *  - Per-run chain STRUCTURE: genesis (first entry's prev_hash is null),
 *    linkage (entry[i].prev_hash equals entry[i-1].entry_hash), and
 *    strict sequence monotonicity. These mirror the external verifier's
 *    chain_broken / first_entry_has_prev_hash / sequence_not_monotonic
 *    issue kinds (verifier/internal/audit/verify.go).
 *  - SIGNING-KEY COVERAGE: a run with entries must carry a signing_key
 *    whose validity window covers the export's exported_at. The verifier
 *    reports the absent case as missing_signing_key; the window checks
 *    are this module's own (signing_key_expired /
 *    signing_key_not_yet_valid).
 *
 * WHAT IT DELIBERATELY DOES NOT CHECK:
 *
 *  - ENTRY-HASH RECOMPUTE (the verifier's `hash_mismatch`). Reproducing
 *    it needs byte-exact reproduction of Go's encoding/json output over
 *    the canonical HashInputs — sorted map keys, HTML escaping of <, >
 *    and &, json.Number-verbatim numbers, RFC3339 truncated to
 *    microseconds (verifier/internal/audit/chain.go ComputeEntryHash and
 *    canonicalizeJSON). A THIRD implementation of that algorithm with no
 *    shared (input, expected-hash) fixture — the mechanism
 *    backend/internal/audit/canonical_fixture_test.go and its verifier
 *    twin use to hold the two existing implementations together — would
 *    drift silently. It is also outside ADR-008's trust model, whose
 *    whole point is that verification does not run code the backend
 *    served: a recompute in browser JS shipped BY the backend proves
 *    nothing an adversarial backend could not also fake.
 *  - ED25519 BUNDLE SIGNATURES (the verifier's `signature_invalid`).
 *    Structurally impossible from this input: the export is
 *    POINTER-ONLY — trace bundle bytes are never inlined (ADR-054, and
 *    the /v0/audit/export description in docs/api/v0.openapi.yaml) —
 *    and VerifyBundleSignature takes the bundle bytes as an argument.
 *    So there is nothing here to verify a signature OVER.
 *
 * The badge that renders this result must therefore say "signing-key
 * coverage", never "signatures verified", and must point at the offline
 * `fishhawk-verify` CLI as the cryptographic-proof path.
 *
 * FAIL-CLOSED: a body whose shape does not match Export v1 returns
 * status 'unverifiable' — never 'pass'. A pass is only ever reported
 * over material this module actually validated.
 */

/**
 * Verification failure modes. `hash_mismatch` and `signature_invalid`
 * are deliberately ABSENT — see the header comment: neither is
 * checkable from this input, and offering the kind would invite a
 * caller to believe it was checked.
 */
export type ChainIssueKind =
  | 'chain_broken'
  | 'first_entry_has_prev_hash'
  | 'sequence_not_monotonic'
  | 'missing_signing_key'
  | 'signing_key_expired'
  | 'signing_key_not_yet_valid'
  | 'malformed_export';

export interface ChainIssue {
  kind: ChainIssueKind;
  /** The export's runs-map key, or '' for a whole-body shape failure. */
  runId: string;
  /** The offending entry's sequence, or 0 when not entry-scoped. */
  sequence: number;
  detail: string;
}

export interface ChainVerification {
  /**
   * 'pass' — every check this module runs passed over the page it saw.
   * 'fail' — the body was well-shaped and a check failed.
   * 'unverifiable' — the body's shape could not be trusted, so NOTHING
   * was concluded. Never conflate it with 'pass'.
   */
  status: 'pass' | 'fail' | 'unverifiable';
  runsVerified: number;
  entriesChecked: number;
  issues: ChainIssue[];
  /** Whether the caller saw a complete export (the response header). */
  complete: boolean;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** A timestamp is usable only if it parses to a finite epoch. */
function parseTimestamp(v: unknown): number | null {
  if (typeof v !== 'string') return null;
  const ms = Date.parse(v);
  return Number.isFinite(ms) ? ms : null;
}

function malformed(detail: string, complete: boolean): ChainVerification {
  return {
    status: 'unverifiable',
    runsVerified: 0,
    entriesChecked: 0,
    issues: [{ kind: 'malformed_export', runId: '', sequence: 0, detail }],
    complete,
  };
}

/** A shape-validated run, carrying the timestamps already parsed. */
interface ShapedRun {
  runId: string;
  entries: Array<{ sequence: number; prevHash: string | null; entryHash: string }>;
  key: { issuedAt: number; expiresAt: number } | null;
}

/**
 * SHAPE GUARD. Runs FIRST and fails closed: any deviation from Export v1
 * yields 'unverifiable', so a renamed or restructured field can never be
 * folded into a vacuous 'pass'. Returns either the validated projection
 * or the reason it was rejected.
 */
function shapeExport(
  body: unknown,
): { ok: true; exportedAt: number; runs: ShapedRun[] } | { ok: false; detail: string } {
  if (!isRecord(body)) return { ok: false, detail: 'export body is not an object' };
  if (body.schema !== 'v1') {
    return { ok: false, detail: `unrecognized export schema: ${JSON.stringify(body.schema)}` };
  }
  const exportedAt = parseTimestamp(body.exported_at);
  if (exportedAt === null) {
    return { ok: false, detail: 'exported_at is not a parseable timestamp' };
  }
  if (!isRecord(body.runs)) return { ok: false, detail: 'runs is not an object' };

  const runs: ShapedRun[] = [];
  for (const runId of Object.keys(body.runs).sort()) {
    const raw = body.runs[runId];
    if (!isRecord(raw)) return { ok: false, detail: `run ${runId}: run data is not an object` };
    if (!Array.isArray(raw.audit_entries)) {
      return { ok: false, detail: `run ${runId}: audit_entries is not an array` };
    }

    const entries: ShapedRun['entries'] = [];
    for (const [i, e] of raw.audit_entries.entries()) {
      if (!isRecord(e)) return { ok: false, detail: `run ${runId}: entry ${i} is not an object` };
      if (typeof e.entry_hash !== 'string') {
        return { ok: false, detail: `run ${runId}: entry ${i} has a non-string entry_hash` };
      }
      if (typeof e.sequence !== 'number' || !Number.isFinite(e.sequence)) {
        return { ok: false, detail: `run ${runId}: entry ${i} has a non-numeric sequence` };
      }
      if (e.prev_hash !== null && typeof e.prev_hash !== 'string') {
        return {
          ok: false,
          detail: `run ${runId}: entry ${i} has a non-string, non-null prev_hash`,
        };
      }
      entries.push({ sequence: e.sequence, prevHash: e.prev_hash, entryHash: e.entry_hash });
    }

    // A PRESENT signing_key must be well-shaped too (operator condition
    // 1): a key we cannot read is not a key we can clear a run on.
    let key: ShapedRun['key'] = null;
    if (raw.signing_key !== undefined) {
      const sk = raw.signing_key;
      if (!isRecord(sk)) return { ok: false, detail: `run ${runId}: signing_key is not an object` };
      if (typeof sk.public_key !== 'string') {
        return { ok: false, detail: `run ${runId}: signing_key.public_key is not a string` };
      }
      const issuedAt = parseTimestamp(sk.issued_at);
      if (issuedAt === null) {
        return {
          ok: false,
          detail: `run ${runId}: signing_key.issued_at is not a parseable timestamp`,
        };
      }
      const expiresAt = parseTimestamp(sk.expires_at);
      if (expiresAt === null) {
        return {
          ok: false,
          detail: `run ${runId}: signing_key.expires_at is not a parseable timestamp`,
        };
      }
      key = { issuedAt, expiresAt };
    }

    runs.push({ runId, entries, key });
  }
  return { ok: true, exportedAt, runs };
}

/**
 * Folds one already-parsed Export v1 body into a verification verdict.
 * `opts.complete` is the caller's reading of the
 * X-Fishhawk-Export-Complete response header; it is carried through so
 * the badge can say the verdict covers only the page it saw.
 */
export function verifyExportChain(body: unknown, opts?: { complete?: boolean }): ChainVerification {
  const complete = opts?.complete === true;

  const shaped = shapeExport(body);
  if (!shaped.ok) return malformed(shaped.detail, complete);

  const issues: ChainIssue[] = [];
  let entriesChecked = 0;

  for (const run of shaped.runs) {
    for (const [i, entry] of run.entries.entries()) {
      entriesChecked += 1;
      if (i === 0) {
        if (entry.prevHash !== null) {
          issues.push({
            kind: 'first_entry_has_prev_hash',
            runId: run.runId,
            sequence: entry.sequence,
            detail: 'the first entry in the run carries a non-null prev_hash',
          });
        }
        continue;
      }
      const prior = run.entries[i - 1];
      if (entry.sequence <= prior.sequence) {
        issues.push({
          kind: 'sequence_not_monotonic',
          runId: run.runId,
          sequence: entry.sequence,
          detail: `sequence ${entry.sequence} does not strictly follow ${prior.sequence}`,
        });
      }
      if (entry.prevHash !== prior.entryHash) {
        issues.push({
          kind: 'chain_broken',
          runId: run.runId,
          sequence: entry.sequence,
          detail: "prev_hash does not match the prior entry's entry_hash",
        });
      }
    }

    // Signing-key COVERAGE (never a signature check): a run with entries
    // must have a key whose window covers exported_at.
    if (run.entries.length === 0) continue;
    if (run.key === null) {
      issues.push({
        kind: 'missing_signing_key',
        runId: run.runId,
        sequence: 0,
        detail: 'the run has audit entries but no signing key in the export',
      });
      continue;
    }
    if (run.key.expiresAt < shaped.exportedAt) {
      issues.push({
        kind: 'signing_key_expired',
        runId: run.runId,
        sequence: 0,
        detail: "the run's signing key expired before the export was taken",
      });
    }
    if (run.key.issuedAt > shaped.exportedAt) {
      issues.push({
        kind: 'signing_key_not_yet_valid',
        runId: run.runId,
        sequence: 0,
        detail: "the run's signing key was issued after the export was taken",
      });
    }
  }

  return {
    status: issues.length === 0 ? 'pass' : 'fail',
    runsVerified: shaped.runs.length,
    entriesChecked,
    issues,
    complete,
  };
}
