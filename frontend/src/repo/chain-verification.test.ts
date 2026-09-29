import { describe, expect, it } from 'vitest';
import { verifyExportChain, type ChainIssueKind } from './chain-verification';

/*
 * Unit tests for the pure Export v1 fold (E40.6 / #1718). Fixtures are
 * hand-authored bodies matching backend/internal/server/audit_export.go's
 * wire shape. Each fail-closed and each issue kind gets its own case, and
 * each fixture is built so the control under test is the ONLY thing that
 * can produce the asserted issue — a masking sibling check would make the
 * counterfactual deletion unobservable.
 */

const RUN_A = '11111111-1111-4111-8111-111111111111';
const RUN_B = '22222222-2222-4222-8222-222222222222';

const H1 = 'a'.repeat(64);
const H2 = 'b'.repeat(64);
const H3 = 'c'.repeat(64);
/** Unrelated by CONSTRUCTION — never derived from any fixture's entry_hash. */
const UNRELATED = 'd'.repeat(64);

const EXPORTED_AT = '2026-09-20T12:00:00Z';

function entry(sequence: number, prevHash: string | null, entryHash: string) {
  return {
    id: `00000000-0000-4000-8000-${String(sequence).padStart(12, '0')}`,
    sequence,
    run_id: RUN_A,
    stage_id: null,
    ts: '2026-09-19T00:00:00Z',
    category: 'run_created',
    actor_kind: 'operator',
    actor_subject: 'octo',
    payload: {},
    prev_hash: prevHash,
    entry_hash: entryHash,
  };
}

function key(issuedAt = '2026-09-01T00:00:00Z', expiresAt = '2026-12-01T00:00:00Z') {
  return { public_key: 'AAAA', issued_at: issuedAt, expires_at: expiresAt };
}

/** A clean two-run export: every check this module runs passes. */
function cleanExport() {
  return {
    schema: 'v1',
    exported_at: EXPORTED_AT,
    runs: {
      [RUN_A]: {
        signing_key: key(),
        audit_entries: [entry(1, null, H1), entry(2, H1, H2), entry(3, H2, H3)],
      },
      [RUN_B]: { signing_key: key(), audit_entries: [entry(7, null, H1)] },
    },
  };
}

function kinds(body: unknown, opts?: { complete?: boolean }): ChainIssueKind[] {
  return verifyExportChain(body, opts).issues.map((i) => i.kind);
}

describe('verifyExportChain — happy path', () => {
  it('passes a well-formed export and counts runs and entries', () => {
    const result = verifyExportChain(cleanExport(), { complete: true });
    expect(result.status).toBe('pass');
    expect(result.issues).toEqual([]);
    expect(result.runsVerified).toBe(2);
    expect(result.entriesChecked).toBe(4);
    expect(result.complete).toBe(true);
  });

  it('carries complete: false through when the caller saw a partial page', () => {
    expect(verifyExportChain(cleanExport(), { complete: false }).complete).toBe(false);
    expect(verifyExportChain(cleanExport()).complete).toBe(false);
  });

  it('reports a run with a signing key but no entries without a coverage issue', () => {
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: { [RUN_A]: { audit_entries: [] } },
    };
    expect(verifyExportChain(body).status).toBe('pass');
  });
});

describe('verifyExportChain — shape guard fails closed', () => {
  /*
   * Each case asserts 'unverifiable' AND explicitly not 'pass': a shape
   * failure must never be folded into a verdict the badge would render
   * as green.
   */
  const cases: Array<[string, unknown]> = [
    ['a non-object body', 'not an export'],
    ['a wrong schema string', { ...cleanExport(), schema: 'v2' }],
    ['runs that is not an object', { schema: 'v1', exported_at: EXPORTED_AT, runs: [] as unknown }],
    [
      'a run whose value is not an object',
      { schema: 'v1', exported_at: EXPORTED_AT, runs: { [RUN_A]: 'not a run' } },
    ],
    [
      // The key is ABSENT, not merely empty: an empty array would also
      // yield 'pass' with the guard intact, so it would prove nothing.
      'a run whose audit_entries key is absent',
      { schema: 'v1', exported_at: EXPORTED_AT, runs: { [RUN_A]: { signing_key: key() } } },
    ],
    [
      'a run whose audit_entries is a non-array value',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: { [RUN_A]: { signing_key: key(), audit_entries: {} } },
      },
    ],
    [
      'an entry with a non-string entry_hash',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: {
            signing_key: key(),
            audit_entries: [{ ...entry(1, null, H1), entry_hash: 7 }],
          },
        },
      },
    ],
    [
      'an entry with a non-numeric sequence',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: {
            signing_key: key(),
            audit_entries: [{ ...entry(1, null, H1), sequence: '1' }],
          },
        },
      },
    ],
    [
      'an entry with a non-string, non-null prev_hash',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: { signing_key: key(), audit_entries: [{ ...entry(1, null, H1), prev_hash: 5 }] },
        },
      },
    ],
    // Operator condition 1: the timestamp and signing-key shape checks
    // fail closed exactly like the chain-shape ones.
    ['a malformed exported_at', { ...cleanExport(), exported_at: 'not-a-timestamp' }],
    [
      'a signing_key with an unparseable issued_at',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: {
            signing_key: { ...key(), issued_at: 'whenever' },
            audit_entries: [entry(1, null, H1)],
          },
        },
      },
    ],
    [
      'a signing_key with an unparseable expires_at',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: {
            signing_key: { ...key(), expires_at: 'never' },
            audit_entries: [entry(1, null, H1)],
          },
        },
      },
    ],
    [
      'a signing_key with a non-string public_key',
      {
        schema: 'v1',
        exported_at: EXPORTED_AT,
        runs: {
          [RUN_A]: {
            signing_key: { ...key(), public_key: 42 },
            audit_entries: [entry(1, null, H1)],
          },
        },
      },
    ],
  ];

  for (const [label, body] of cases) {
    it(`rejects ${label} as unverifiable`, () => {
      const result = verifyExportChain(body);
      expect(result.status).toBe('unverifiable');
      expect(result.status).not.toBe('pass');
      expect(result.issues.map((i) => i.kind)).toEqual(['malformed_export']);
      expect(result.entriesChecked).toBe(0);
    });
  }
});

describe('verifyExportChain — chain structure', () => {
  it('reports chain_broken on a tampered prev_hash', () => {
    // Entry 2's prev_hash is unrelated BY CONSTRUCTION; genesis and
    // sequence both hold, so neither sibling check can mask it.
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key(),
          audit_entries: [entry(1, null, H1), entry(2, UNRELATED, H2)],
        },
      },
    };
    const result = verifyExportChain(body);
    expect(result.status).toBe('fail');
    expect(result.issues.map((i) => i.kind)).toEqual(['chain_broken']);
    expect(result.issues[0].runId).toBe(RUN_A);
    expect(result.issues[0].sequence).toBe(2);
  });

  it('reports first_entry_has_prev_hash', () => {
    // Entry 2 links correctly to entry 1, so the linkage check passes
    // and only the genesis check can produce an issue here.
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key(),
          audit_entries: [entry(1, UNRELATED, H1), entry(2, H1, H2)],
        },
      },
    };
    expect(kinds(body)).toEqual(['first_entry_has_prev_hash']);
  });

  it('reports sequence_not_monotonic', () => {
    // Sequences [1, 3, 2] with linkage kept internally consistent, so
    // the chain-link check cannot mask the monotonicity failure.
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key(),
          audit_entries: [entry(1, null, H1), entry(3, H1, H2), entry(2, H2, H3)],
        },
      },
    };
    const result = verifyExportChain(body);
    expect(result.issues.map((i) => i.kind)).toEqual(['sequence_not_monotonic']);
    expect(result.issues[0].sequence).toBe(2);
  });
});

describe('verifyExportChain — signing-key coverage', () => {
  it('reports missing_signing_key for a run with entries and no key', () => {
    // Structurally perfect chain: coverage is the only check left.
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: { [RUN_A]: { audit_entries: [entry(1, null, H1), entry(2, H1, H2)] } },
    };
    const result = verifyExportChain(body);
    expect(result.status).toBe('fail');
    expect(result.issues.map((i) => i.kind)).toEqual(['missing_signing_key']);
  });

  it('reports signing_key_expired when expires_at precedes exported_at', () => {
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key('2026-01-01T00:00:00Z', '2026-09-19T00:00:00Z'),
          audit_entries: [entry(1, null, H1)],
        },
      },
    };
    expect(kinds(body)).toEqual(['signing_key_expired']);
  });

  it('reports signing_key_not_yet_valid when the key was issued after exported_at', () => {
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key('2026-09-21T00:00:00Z', '2026-12-01T00:00:00Z'),
          audit_entries: [entry(1, null, H1)],
        },
      },
    };
    const result = verifyExportChain(body);
    expect(result.issues.map((i) => i.kind)).toEqual(['signing_key_not_yet_valid']);
    expect(result.status).not.toBe('pass');
  });

  it('treats the window as inclusive at both ends', () => {
    const body = {
      schema: 'v1',
      exported_at: EXPORTED_AT,
      runs: {
        [RUN_A]: {
          signing_key: key(EXPORTED_AT, EXPORTED_AT),
          audit_entries: [entry(1, null, H1)],
        },
      },
    };
    expect(verifyExportChain(body).status).toBe('pass');
  });
});

describe('verifyExportChain — scope of the check', () => {
  it('never reports a hash or signature issue kind', () => {
    // The fold cannot check either (see the module header), so no
    // fixture may produce a kind that claims it did.
    const bodies = [cleanExport(), { schema: 'nope' }];
    for (const body of bodies) {
      for (const kind of kinds(body)) {
        expect(kind).not.toMatch(/hash_mismatch|signature/);
      }
    }
  });
});
