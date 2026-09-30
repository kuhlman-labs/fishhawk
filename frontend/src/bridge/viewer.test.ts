import { describe, expect, it } from 'vitest';
import { ApiClientError } from '@/api/client';
import type { ApiError } from '@/api/types';
import { captainRefusalMessage, viewerMatchesSubject } from './viewer';

function refusal(status: number, body: ApiError): ApiClientError {
  return new ApiClientError(status, body, body.message ?? body.error);
}

describe('viewerMatchesSubject', () => {
  it.each([
    ['provider-qualified match', 'octo', 'github:octo', true],
    ['provider-qualified match on gitlab', 'octo', 'gitlab:octo', true],
    ['provider-qualified mismatch', 'octo', 'gitlab:someone', false],
    ['unqualified subject matches whole string', 'octo', 'octo', true],
    ['unqualified subject mismatch', 'octo', 'someone', false],
    ['login portion containing a further colon', 'a:b', 'github:a:b', true],
    ['a further colon is not a second split', 'b', 'github:a:b', false],
    ['empty login never matches', '', 'github:', false],
    ['null login never matches', null, 'github:octo', false],
  ] as const)('%s', (_name, login, subject, want) => {
    expect(viewerMatchesSubject(login, subject)).toBe(want);
  });
});

describe('captainRefusalMessage', () => {
  it.each([
    [403, 'captain_not_captain', 'the server says you are not it'],
    [403, 'captain_not_offerer', 'Only the captain who made the pending offer'],
    [403, 'captain_offer_successor_mismatch', 'only its successor can accept it'],
    [409, 'captain_self_handover', 'cannot be offered to the captain already holding it'],
    [409, 'captain_no_offer', 'No handover offer is pending'],
    [409, 'captain_no_captain', 'The seat is vacant'],
    [409, 'captain_exists', 'The seat is held'],
    [403, 'captain_agent_identity_refused', 'refuse agent and delegated identities'],
    [403, 'captain_predicate_rejected', 'do not satisfy'],
    [422, 'captain_predicate_undeterminable', 'could not be determined'],
    [501, 'captain_unconfigured', 'No captain record on this instance.'],
    [403, 'delegation_not_captain', 'Only the captain in force'],
    [409, 'delegation_no_captain', 'delegation cannot be confirmed or lowered'],
    [400, 'delegation_raise_refused', 'can only lower delegation'],
    [400, 'delegation_nothing_proposed', 'Nothing was proposed'],
    [403, 'delegation_agent_identity_refused', 'refuses agent and delegated identities'],
    [501, 'delegation_confirm_unconfigured', 'No delegation-confirmation store'],
    [422, 'workflow_spec_invalid', 'does not validate'],
    [404, 'workflow_not_found', 'declares no such workflow'],
    [502, 'work_item_filing_failed', 'could not be filed'],
    [403, 'repo_forbidden', 'cannot read this repository'],
    [403, 'insufficient_scope', 'lacks the scope'],
  ])('%i %s renders its sentence AND the raw server code', (status, code, sentence) => {
    const msg = captainRefusalMessage(refusal(status, { error: code, message: 'srv' }));
    expect(msg).toContain(sentence);
    expect(msg).toContain(`(${status} · ${code})`);
  });

  it('renders details.current_content_hash for delegation_hash_stale', () => {
    const msg = captainRefusalMessage(
      refusal(409, {
        error: 'delegation_hash_stale',
        message: 'stale',
        details: { current_content_hash: 'f'.repeat(64) },
      }),
    );
    expect(msg).toContain('409 · delegation_hash_stale');
    expect(msg).toContain(`Current content hash: ${'f'.repeat(64)}.`);
  });

  it('states the item WAS filed when a 500 carries details.filed_ref', () => {
    const msg = captainRefusalMessage(
      refusal(500, {
        error: 'internal_error',
        message: 'append failed',
        details: { filed_ref: 'https://github.com/acme/app/issues/9' },
      }),
    );
    expect(msg).toContain('500 · internal_error');
    expect(msg).toContain('WAS filed (https://github.com/acme/app/issues/9)');
  });

  it('falls back to `{status} · {error}` for an unmapped code, never swallowing it', () => {
    const msg = captainRefusalMessage(refusal(418, { error: 'captain_new_refusal', message: 'x' }));
    expect(msg).toBe('418 · captain_new_refusal');
  });

  it('falls back to the message when the error body is not JSON', () => {
    const msg = captainRefusalMessage(new ApiClientError(502, null, 'request failed: 502'));
    expect(msg).toBe('502 · request failed: 502');
  });

  it('passes a plain Error through verbatim', () => {
    expect(captainRefusalMessage(new Error('network down'))).toBe('network down');
  });
});
