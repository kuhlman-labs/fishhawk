/*
 * Wire shape for the acceptance stage's `acceptance` artifact. Mirrors
 * acceptanceBody / acceptanceCriterionResult in
 * backend/internal/server/acceptance.go — keep the two in sync on every
 * backend change to the acceptance upload handler.
 *
 * v0 carries no schema_version on this artifact (the field shape isn't
 * schema-stable yet, mirroring the pull_request / deployment artifacts), so
 * the SPA gates on a structural decode instead.
 *
 * ONE decoding contract (#1715, approval condition 2): `criteria` is
 * normalized from EITHER wire shape — the schema-required flat array, or the
 * historical object keyed by criterion id that the backend's
 * coerceAcceptanceCriteria tolerates (the #1574 class) — BEFORE the shape
 * guard decides anything. The guard therefore rejects only a value that
 * neither shape decodes; an object-keyed artifact is readable, not rejected.
 */

export type AcceptanceCriterionOutcome = 'passed' | 'failed' | 'skipped' | 'undecidable';

const CRITERION_OUTCOMES: ReadonlySet<string> = new Set([
  'passed',
  'failed',
  'skipped',
  'undecidable',
]);

export interface AcceptanceCriterionResult {
  id: string;
  result: AcceptanceCriterionOutcome;
  observed?: string;
  expected?: string;
  steps_taken?: string;
  expectation_basis?: string;
  repro_handle?: string;
  undecidable_reason?: string;
}

/**
 * The decoded artifact body. `criteria` is ALWAYS the normalized flat array
 * (empty when the wire omitted it) — consumers never see the object-keyed
 * variant.
 */
export interface AcceptanceArtifactBody {
  /** The settled disposition: `passed` | `failed`. */
  verdict: string;
  failure_mode?: string;
  criteria: AcceptanceCriterionResult[];
  target_url?: string;
  evidence_hashes?: string[];
  notes?: string;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function optionalString(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

/**
 * Decode one criterion element. `idOverride` is the object-keyed variant's
 * key, which the backend folds into the element id (a conflicting non-empty
 * element id is refused there too). Returns null on a shape it cannot read.
 */
function decodeCriterion(raw: unknown, idOverride?: string): AcceptanceCriterionResult | null {
  if (!isRecord(raw)) return null;
  const elementId = raw.id;
  if (elementId !== undefined && typeof elementId !== 'string') return null;
  let id: string;
  if (idOverride !== undefined) {
    if (typeof elementId === 'string' && elementId !== '' && elementId !== idOverride) return null;
    id = idOverride;
  } else {
    if (typeof elementId !== 'string' || elementId === '') return null;
    id = elementId;
  }
  const result = raw.result;
  if (typeof result !== 'string' || !CRITERION_OUTCOMES.has(result)) return null;
  const out: AcceptanceCriterionResult = { id, result: result as AcceptanceCriterionOutcome };
  const observed = optionalString(raw.observed);
  if (observed !== undefined) out.observed = observed;
  const expected = optionalString(raw.expected);
  if (expected !== undefined) out.expected = expected;
  const steps = optionalString(raw.steps_taken);
  if (steps !== undefined) out.steps_taken = steps;
  const basis = optionalString(raw.expectation_basis);
  if (basis !== undefined) out.expectation_basis = basis;
  const repro = optionalString(raw.repro_handle);
  if (repro !== undefined) out.repro_handle = repro;
  const reason = optionalString(raw.undecidable_reason);
  if (reason !== undefined) out.undecidable_reason = reason;
  return out;
}

/**
 * Normalize the wire `criteria` value to the flat array. Accepts:
 *   - absent / null           → [] (a verdict may settle before criteria are itemized)
 *   - a flat array            → each element decoded as-is
 *   - an object keyed by id   → each value decoded with its key folded into `id`,
 *                               sorted by id (the backend's coercion order)
 * Returns null when the value is neither shape, or any element is unreadable:
 * a partial table would silently drop a criterion.
 */
export function normalizeAcceptanceCriteria(raw: unknown): AcceptanceCriterionResult[] | null {
  if (raw === undefined || raw === null) return [];
  if (Array.isArray(raw)) {
    const out: AcceptanceCriterionResult[] = [];
    for (const el of raw) {
      const c = decodeCriterion(el);
      if (c === null) return null;
      out.push(c);
    }
    return out;
  }
  if (isRecord(raw)) {
    const out: AcceptanceCriterionResult[] = [];
    for (const [key, value] of Object.entries(raw)) {
      const c = decodeCriterion(value, key);
      if (c === null) return null;
      out.push(c);
    }
    out.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return out;
  }
  return null;
}

/**
 * Normalize an `evidence_hashes` value: the flat string array, or the
 * historical string-valued object map (its values, sorted — the backend's
 * coerceEvidenceHashes). Anything else is dropped (undefined) rather than
 * failing the whole artifact: the hashes are provenance, not a rendered row.
 */
function normalizeEvidenceHashes(raw: unknown): string[] | undefined {
  if (Array.isArray(raw)) {
    return raw.every((h) => typeof h === 'string') ? (raw as string[]) : undefined;
  }
  if (isRecord(raw)) {
    const values = Object.values(raw);
    return values.every((h) => typeof h === 'string') ? (values as string[]).sort() : undefined;
  }
  return undefined;
}

/**
 * Decode an artifact's `content` as an acceptance body, normalizing
 * `criteria` FIRST. Returns null only when the content has no string verdict
 * or its criteria decode under neither wire shape.
 */
export function decodeAcceptanceArtifact(content: unknown): AcceptanceArtifactBody | null {
  if (!isAcceptanceArtifact(content)) return null;
  const c = content as unknown as Record<string, unknown>;
  const criteria = normalizeAcceptanceCriteria(c.criteria) ?? [];
  const body: AcceptanceArtifactBody = { verdict: c.verdict as string, criteria };
  const failureMode = optionalString(c.failure_mode);
  if (failureMode !== undefined) body.failure_mode = failureMode;
  const targetURL = optionalString(c.target_url);
  if (targetURL !== undefined) body.target_url = targetURL;
  const hashes = normalizeEvidenceHashes(c.evidence_hashes);
  if (hashes !== undefined) body.evidence_hashes = hashes;
  const notes = optionalString(c.notes);
  if (notes !== undefined) body.notes = notes;
  return body;
}

/**
 * Narrow structural check that an artifact's `content` is readable as an
 * acceptance body: a string `verdict`, and `criteria` that normalizes under
 * one of the two wire shapes (normalization runs inside the guard, so the
 * object-keyed variant passes). Note the type predicate describes the
 * NORMALIZED shape — read the body through decodeAcceptanceArtifact, which
 * returns the flat criteria array, rather than trusting `content.criteria`.
 */
export function isAcceptanceArtifact(content: unknown): content is AcceptanceArtifactBody {
  if (!isRecord(content)) return false;
  if (typeof content.verdict !== 'string') return false;
  return normalizeAcceptanceCriteria(content.criteria) !== null;
}
