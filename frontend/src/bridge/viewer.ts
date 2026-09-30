import { ApiClientError } from '@/api/client';

/*
 * Pure helpers for the Bridge tab (E76.6 / #3769). No React.
 *
 * THE UI ADDS NO AUTHORITY. GET /v0/auth/me carries no identity provider
 * (the `User` schema is id + github_login + name), while a captain subject is
 * provider-qualified (`github:octo`, `gitlab:octo` — the server's
 * sessionSubject). The SPA therefore cannot compute the viewer's real
 * subject, so NO verb is gated on a client-side identity match: every
 * state-admitted button stays enabled and the server's refusal is the only
 * authority. `viewerMatchesSubject` is a COSMETIC "(you)" hint and nothing
 * more — it must never disable or hide a control.
 */

/**
 * True when `login` equals the login portion of `subject`: the part after
 * the FIRST ':' of a provider-qualified subject, or the whole string when
 * the subject is unqualified. An empty login never matches.
 */
export function viewerMatchesSubject(login: string | null | undefined, subject: string): boolean {
  if (!login) return false;
  const colon = subject.indexOf(':');
  const subjectLogin = colon === -1 ? subject : subject.slice(colon + 1);
  return subjectLogin === login;
}

/*
 * One operator-readable sentence per refusal code the captain and
 * delegation-confirmation routes document. The sentence is rendered
 * ALONGSIDE the raw `{status} · {error}` — never instead of it — so the
 * server's own code stays visible and greppable.
 */
const REFUSALS: Record<string, string> = {
  captain_not_captain: 'Only the sitting captain can do this, and the server says you are not it.',
  captain_not_offerer: 'Only the captain who made the pending offer can withdraw it.',
  captain_offer_successor_mismatch:
    'The pending offer names someone else; only its successor can accept it.',
  captain_self_handover: 'The seat cannot be offered to the captain already holding it.',
  captain_successor_required: 'An offer needs a successor.',
  captain_no_offer: 'No handover offer is pending — it may have been withdrawn.',
  captain_no_captain: 'The seat is vacant, so there is no captain to act as.',
  captain_exists: 'The seat is held; a claim is only possible while it is vacant.',
  captain_agent_identity_refused:
    'Captain verbs refuse agent and delegated identities; only a human can act here.',
  captain_predicate_rejected:
    "You do not satisfy this repository's approval predicate, so the claim was refused.",
  captain_predicate_undeterminable:
    "The repository's approval predicate could not be determined, so the claim was refused rather than guessed.",
  captain_unconfigured: 'No captain record on this instance.',
  delegation_not_captain: 'Only the captain in force can confirm or lower delegation.',
  delegation_no_captain: 'The seat is vacant; delegation cannot be confirmed or lowered.',
  delegation_hash_stale:
    'The delegation changed since it was shown; re-read it and confirm the current hash.',
  delegation_raise_refused:
    'A proposal can only lower delegation — a tier or ceiling that is not strictly lower is refused.',
  delegation_nothing_proposed: 'Nothing was proposed: name a lower tier or an escalation ceiling.',
  delegation_agent_identity_refused:
    'Delegation confirmation refuses agent and delegated identities; only a human can act here.',
  delegation_confirm_unconfigured: 'No delegation-confirmation store on this instance.',
  workflow_spec_invalid:
    'The workflow spec at this source does not validate; nothing can be confirmed or lowered against it.',
  workflow_not_found: 'The spec at this source declares no such workflow.',
  work_item_invalid: "The work item would violate the repository's work-item conventions.",
  work_item_filing_failed: 'The work item could not be filed at the forge; nothing was recorded.',
  handover_brief_unavailable: 'The handover brief could not be established.',
  handover_brief_unconfigured: 'The handover brief is not configured on this instance.',
  repo_forbidden: 'You cannot read this repository.',
  insufficient_scope: 'Your token lacks the scope this action needs.',
};

function detailString(err: ApiClientError, key: string): string | null {
  const v = err.body?.details?.[key];
  return typeof v === 'string' && v !== '' ? v : null;
}

/**
 * Render a thrown error as one line: the mapped sentence (when the code is
 * known) followed by the raw `{status} · {error}`. An UNMAPPED code renders
 * the raw form alone — the same shape approval-panel.tsx and
 * decision-form.tsx use — so a new server refusal is surfaced, never
 * swallowed. `delegation_hash_stale` appends `details.current_content_hash`;
 * any body carrying `details.filed_ref` (a lower whose append failed AFTER
 * filing) states that the item WAS filed.
 */
export function captainRefusalMessage(err: unknown): string {
  if (!(err instanceof ApiClientError)) {
    return err instanceof Error ? err.message : 'unknown error';
  }
  const code = err.body?.error;
  const raw = `${err.status} · ${code ?? err.message}`;
  const parts: string[] = [];
  const sentence = code !== undefined ? REFUSALS[code] : undefined;
  if (sentence) parts.push(sentence);
  parts.push(sentence ? `(${raw})` : raw);
  const current = detailString(err, 'current_content_hash');
  if (code === 'delegation_hash_stale' && current) {
    parts.push(`Current content hash: ${current}.`);
  }
  const filed = detailString(err, 'filed_ref');
  if (filed) {
    parts.push(`The work item WAS filed (${filed}) but recording it on the chain failed.`);
  }
  return parts.join(' ');
}

/** The server's error code on an ApiClientError, or null for anything else. */
export function apiErrorCode(err: unknown): string | null {
  return err instanceof ApiClientError ? (err.body?.error ?? null) : null;
}

/** `abcdef012345…` — the truncated-hash form the run narrative uses. */
export function shortHash(hash: string): string {
  return hash.length > 12 ? `${hash.slice(0, 12)}…` : hash;
}
