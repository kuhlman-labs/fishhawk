import { useState } from 'react';
import { AlertTriangle, ChevronDown, ChevronRight, Info, ShieldCheck } from 'lucide-react';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { Section } from '@/plan/sections';

/*
 * Policy section (#233). Surfaces the most-recent
 * `policy_evaluated` audit entry for an implement stage so the
 * reviewer doesn't have to dig into raw audit JSON to see what
 * policy ran and what (if anything) violated.
 *
 * Four states:
 *   - pending: no entry yet (the audit fetch is still in flight).
 *     A brief loading state; settles to one of the three below
 *     as soon as the entry lands.
 *   - pass: green header + foldable applied constraints + diff
 *     summary.
 *   - fail: violations grouped by rule are the headline, applied
 *     constraints fold away by default but stay accessible.
 *   - skipped: the backend wrote a policy_evaluated entry with a
 *     structured skip_reason (#283) — e.g., the run row had no
 *     cached spec, the bundle's diff event was malformed, the
 *     stage type wasn't in the workflow. Renders as informational
 *     (gray) with the reason inline, distinct from pass / fail.
 *
 * The audit category is constant (`policy_evaluated`); the per-
 * file, per-violation, and skip detail lives in the payload,
 * mirroring `policy.EvaluationPayload` in the backend. Field names
 * track the wire shape — snake_case throughout.
 */

interface PolicyPayload {
  stage_type?: string;
  diff?: PolicyDiffEntry[];
  applied_constraints?: AppliedConstraints;
  violations?: PolicyViolation[];
  passed?: boolean;
  // skip_reason / skip_detail populated when the backend couldn't
  // carry out a meaningful evaluation (#283). When present, the
  // section renders the skipped arm regardless of `passed`.
  skip_reason?: SkipReason;
  skip_detail?: string;
  // deferred_outcomes names required_outcomes the backend declined
  // to assert on because no signal was available at evaluation
  // time (#297). At trace-upload time the only entry is "ci_green":
  // CI hasn't started against the just-opened PR yet, so branch
  // protection is the actual gate at merge time. Surfaced inline
  // with the pass state as a neutral info note.
  deferred_outcomes?: string[];
}

type SkipReason =
  | 'spec_unavailable'
  | 'spec_unparseable'
  | 'workflow_not_in_spec'
  | 'stage_not_in_spec'
  | 'no_diff_in_bundle';

interface PolicyDiffEntry {
  path?: string;
  status?: string; // 'A' | 'M' | 'D' | 'R' | 'C' | 'T'
}

interface AppliedConstraints {
  forbidden_paths?: string[];
  allowed_paths?: string[];
  max_files_changed?: number;
  required_outcomes?: string[];
  ci_green?: boolean | null;
}

interface PolicyViolation {
  constraint?: string;
  detail?: string;
  files?: string[];
}

interface Props {
  runId: string;
  stageId: string;
}

/*
 * PolicySection wraps the audit fetch + state branching. Returns
 * a Section element (so the parent's spacing rhythm stays
 * consistent) regardless of state — empty state included.
 */
export function PolicySection({ runId, stageId }: Props) {
  const result = useAsync(
    () => api.listRunAudit(runId, { stageId, category: 'policy_evaluated', limit: 1 }),
    [runId, stageId],
  );

  if (result.status === 'loading') {
    return (
      <Section id="policy" title="Policy" collapsible>
        <p className="text-sm text-neutral-500">Loading policy evaluation…</p>
      </Section>
    );
  }
  if (result.status === 'error') {
    return (
      <Section id="policy" title="Policy" collapsible>
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          Couldn&apos;t load policy evaluation: {result.error.message}
        </div>
      </Section>
    );
  }

  const entry = result.data.items[0];
  if (!entry) {
    return (
      <Section id="policy" title="Policy" collapsible>
        <p className="text-sm text-neutral-500">Policy evaluation pending.</p>
      </Section>
    );
  }

  const payload = (entry.payload as PolicyPayload | null) ?? {};
  return (
    <Section id="policy" title="Policy" collapsible>
      <PolicyBody payload={payload} />
    </Section>
  );
}

function PolicyBody({ payload }: { payload: PolicyPayload }) {
  // Skipped state takes priority over pass/fail — the backend
  // emits skip_reason when it couldn't run a meaningful evaluation
  // (#283). The payload's `passed` is true in those cases (so
  // downstream gates don't trip), but the SPA wording must reflect
  // that no actual check happened.
  if (payload.skip_reason) {
    return <SkippedBody reason={payload.skip_reason} detail={payload.skip_detail} />;
  }
  const passed = payload.passed === true;
  return (
    <div className="space-y-3">
      <PolicyHeader passed={passed} violationCount={payload.violations?.length ?? 0} />
      <DeferredOutcomesNote deferred={payload.deferred_outcomes} />
      <DiffSummary diff={payload.diff ?? []} />
      {!passed && payload.violations && payload.violations.length > 0 && (
        <ViolationsList violations={payload.violations} />
      )}
      <AppliedConstraintsBlock
        applied={payload.applied_constraints}
        defaultOpen={passed} // pass-state: open by default ("what was checked"); fail-state: collapse so violations are the headline.
      />
    </div>
  );
}

// DeferredOutcomesNote renders the small info row that names any
// required_outcomes the backend declined to assert on (#297). For
// v0 the only entry is `ci_green`; the wording calls out branch
// protection so the reviewer understands where the merge-time gate
// actually lives.
function DeferredOutcomesNote({ deferred }: { deferred?: string[] }) {
  if (!deferred || deferred.length === 0) return null;
  return (
    <p
      className="flex items-start gap-1.5 text-xs text-neutral-600 dark:text-neutral-400"
      data-testid="policy-deferred-outcomes"
    >
      <Info className="mt-0.5 size-3.5 shrink-0" aria-hidden />
      <span>
        Deferred to branch protection: <span className="font-mono">{deferred.join(', ')}</span>
      </span>
    </p>
  );
}

// SkippedBody renders the "evaluation didn't run" arm. The wording
// in `reason` is human-readable but the structured value is what
// the test asserts on — keep them aligned with the backend's
// SkipReason enum (#283).
const skipReasonLabel: Record<SkipReason, string> = {
  spec_unavailable: 'no cached workflow spec on this run',
  spec_unparseable: 'the cached workflow spec failed to parse',
  workflow_not_in_spec: "the run's workflow_id is not in the cached spec",
  stage_not_in_spec: 'this stage type is not declared in the workflow',
  no_diff_in_bundle: 'the trace bundle did not include a git_diff event',
};

function SkippedBody({ reason, detail }: { reason: SkipReason; detail?: string }) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 text-sm font-medium text-neutral-700 dark:text-neutral-300">
        <Info className="size-4" aria-hidden />
        Policy evaluation skipped
      </div>
      <p className="text-xs text-neutral-600 dark:text-neutral-400">
        Reason: {skipReasonLabel[reason] ?? reason}
      </p>
      {detail && (
        <pre className="overflow-x-auto rounded-md border border-neutral-200 bg-neutral-50 p-3 font-mono text-xs whitespace-pre-wrap text-neutral-700 dark:border-neutral-800 dark:bg-neutral-900 dark:text-neutral-300">
          {detail}
        </pre>
      )}
    </div>
  );
}

function PolicyHeader({ passed, violationCount }: { passed: boolean; violationCount: number }) {
  if (passed) {
    return (
      <div className="flex items-center gap-2 text-sm font-medium text-emerald-700 dark:text-emerald-400">
        <ShieldCheck className="size-4" aria-hidden />
        Policy passed
      </div>
    );
  }
  return (
    <div className="flex items-center gap-2 text-sm font-medium text-rose-800 dark:text-rose-300">
      <AlertTriangle className="size-4" aria-hidden />
      Policy violations ({violationCount})
    </div>
  );
}

function DiffSummary({ diff }: { diff: PolicyDiffEntry[] }) {
  if (diff.length === 0) {
    return <p className="text-xs text-neutral-500">Evaluated against an empty diff.</p>;
  }
  let added = 0;
  let modified = 0;
  let deleted = 0;
  let other = 0;
  for (const f of diff) {
    switch (f.status) {
      case 'A':
        added++;
        break;
      case 'M':
        modified++;
        break;
      case 'D':
        deleted++;
        break;
      default:
        other++;
        break;
    }
  }
  const parts: string[] = [];
  if (added) parts.push(`${added} added`);
  if (modified) parts.push(`${modified} modified`);
  if (deleted) parts.push(`${deleted} deleted`);
  if (other) parts.push(`${other} other`);
  const fileSummary = diff.length === 1 ? '1 file' : `${diff.length} files`;
  return (
    <p className="text-xs text-neutral-500">
      Evaluated against {fileSummary}: {parts.join(' · ') || 'no recognized status'}
    </p>
  );
}

function ViolationsList({ violations }: { violations: PolicyViolation[] }) {
  // Group by constraint so a reviewer sees "forbidden_paths: 3
  // files" rather than three loose rows. Constraint name doubles
  // as the heading.
  const groups = new Map<string, PolicyViolation[]>();
  for (const v of violations) {
    const key = v.constraint ?? '(unknown)';
    const existing = groups.get(key) ?? [];
    existing.push(v);
    groups.set(key, existing);
  }
  const sortedKeys = [...groups.keys()].sort();
  return (
    <ul className="overflow-hidden rounded-md border border-rose-200 bg-rose-50/30 dark:border-rose-900/60 dark:bg-rose-950/20">
      {sortedKeys.map((key) => (
        <li
          key={key}
          className="space-y-1 border-b border-rose-200 px-3 py-2 last:border-b-0 dark:border-rose-900/60"
        >
          <p className="font-mono text-xs font-medium text-rose-900 dark:text-rose-200">{key}</p>
          {groups.get(key)?.map((v, idx) => (
            <ViolationRow key={`${key}-${idx}`} violation={v} />
          ))}
        </li>
      ))}
    </ul>
  );
}

function ViolationRow({ violation }: { violation: PolicyViolation }) {
  return (
    <div className="space-y-0.5 text-xs">
      {violation.detail && (
        <p className="text-neutral-700 dark:text-neutral-300">{violation.detail}</p>
      )}
      {violation.files && violation.files.length > 0 && (
        <ul className="space-y-0.5 pl-3">
          {violation.files.map((f) => (
            <li key={f} className="font-mono text-neutral-600 dark:text-neutral-400">
              {f}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function AppliedConstraintsBlock({
  applied,
  defaultOpen,
}: {
  applied: AppliedConstraints | undefined;
  defaultOpen: boolean;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const items = formatConstraints(applied);
  if (items.length === 0) {
    return <p className="text-xs text-neutral-500">No constraints configured for this stage.</p>;
  }
  return (
    <div className="space-y-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1 text-xs text-neutral-500 hover:text-neutral-900 dark:hover:text-neutral-100"
        aria-expanded={open}
        aria-controls="policy-applied-constraints"
      >
        {open ? (
          <ChevronDown className="size-3.5" aria-hidden />
        ) : (
          <ChevronRight className="size-3.5" aria-hidden />
        )}
        {open ? 'Hide' : 'Show'} applied constraints ({items.length})
      </button>
      {open && (
        <ul
          id="policy-applied-constraints"
          className="space-y-1 rounded-md border border-neutral-200 bg-neutral-50 p-3 text-xs dark:border-neutral-800 dark:bg-neutral-900"
        >
          {items.map(({ name, value }) => (
            <li key={name} className="flex gap-2 font-mono">
              <span className="shrink-0 text-neutral-500">{name}:</span>
              <span className="text-neutral-800 dark:text-neutral-200">{value}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

interface ConstraintRow {
  name: string;
  value: string;
}

// formatConstraints flattens the AppliedConstraints object into a
// stable, render-ready list. Empty / zero values are omitted —
// "constraint is not configured" is the same as "absent" per
// `policy.Constraints` zero semantics.
function formatConstraints(applied: AppliedConstraints | undefined): ConstraintRow[] {
  if (!applied) return [];
  const out: ConstraintRow[] = [];
  if (applied.forbidden_paths && applied.forbidden_paths.length > 0) {
    out.push({ name: 'forbidden_paths', value: applied.forbidden_paths.join(', ') });
  }
  if (applied.allowed_paths && applied.allowed_paths.length > 0) {
    out.push({ name: 'allowed_paths', value: applied.allowed_paths.join(', ') });
  }
  if (typeof applied.max_files_changed === 'number' && applied.max_files_changed > 0) {
    out.push({ name: 'max_files_changed', value: String(applied.max_files_changed) });
  }
  if (applied.required_outcomes && applied.required_outcomes.length > 0) {
    out.push({ name: 'required_outcomes', value: applied.required_outcomes.join(', ') });
  }
  if (applied.ci_green !== null && applied.ci_green !== undefined) {
    out.push({ name: 'ci_green', value: applied.ci_green ? 'true' : 'false' });
  }
  return out;
}
