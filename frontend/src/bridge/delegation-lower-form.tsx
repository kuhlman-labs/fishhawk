import { useState } from 'react';
import { api } from '@/api/client';
import type {
  AutonomyTier,
  DelegationLowerRequest,
  DelegationVerbResponse,
  RepoDelegationWorkflow,
} from '@/api/captain';
import { Button } from '@/components/ui/button';
import {
  DecisionError,
  ReasonField,
  TextField,
  useDecisionSubmit,
} from '@/attention/decision-form';
import { captainRefusalMessage } from './viewer';

/*
 * The "propose lower" form on the delegation screen (E76.6 / #3769) over
 * POST /v0/repos/{owner}/{name}/delegation/lower (E76.5 / #3768).
 *
 * A proposal can only LOWER. The tier options are STRICTLY below the
 * workflow's current resolved tier (medium ⇒ {low}; high ⇒ {medium, low};
 * low ⇒ none), and a workflow that declares NO tier offers nothing at all:
 * the API reads an undeclared current tier fail-closed, so the form renders
 * the named refusal and keeps submit disabled rather than guessing a baseline.
 * The optional escalation ceiling's `max_autonomy` is restricted to
 * at-or-below the EFFECTIVE proposed tier (the proposed one, else the current
 * one). The server re-checks every one of these (`delegation_raise_refused`);
 * the filter only keeps the form from offering a raise it would refuse.
 *
 * A resolved lower files ONE `autonomy:low` work item and changes nothing in
 * force: the delegation stays as shown until a human lands the edit.
 */

const TIER_RANK: Record<AutonomyTier, number> = { low: 0, medium: 1, high: 2 };
const TIERS_DESC: AutonomyTier[] = ['high', 'medium', 'low'];

/** Tiers strictly below `current`; none when the current tier is undeclared. */
function tiersBelow(current: AutonomyTier | undefined): AutonomyTier[] {
  if (current === undefined) return [];
  return TIERS_DESC.filter((t) => TIER_RANK[t] < TIER_RANK[current]);
}

/** Tiers at or below `ceiling`. */
function tiersAtOrBelow(ceiling: AutonomyTier): AutonomyTier[] {
  return TIERS_DESC.filter((t) => TIER_RANK[t] <= TIER_RANK[ceiling]);
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s !== '');
}

/** `key=value` pairs, comma- or newline-separated; a pair without `=` is dropped. */
function parseTitleVars(raw: string): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const pair of splitList(raw)) {
    const eq = pair.indexOf('=');
    if (eq <= 0) continue;
    out[pair.slice(0, eq).trim()] = pair.slice(eq + 1).trim();
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

export interface DelegationLowerFormProps {
  owner: string;
  name: string;
  workflow: RepoDelegationWorkflow;
  /** A named reason the verb is unavailable on this instance; disables submit. */
  disabledReason?: string;
  /** Fired after a resolved lower, so the panel re-reads the verdicts. */
  onFiled?: () => void;
}

export function DelegationLowerForm({
  owner,
  name,
  workflow,
  disabledReason,
  onFiled,
}: DelegationLowerFormProps) {
  const [filed, setFiled] = useState<DelegationVerbResponse | null>(null);
  const { submitting, error, submit } = useDecisionSubmit(() => onFiled?.());
  const [tier, setTier] = useState<AutonomyTier | ''>('');
  const [withEscalation, setWithEscalation] = useState(false);
  const [paths, setPaths] = useState('');
  const [maxAutonomy, setMaxAutonomy] = useState<AutonomyTier | ''>('');
  const [reason, setReason] = useState('');
  const [parentEpic, setParentEpic] = useState('');
  const [labels, setLabels] = useState('');
  const [titleVars, setTitleVars] = useState('');

  const current = workflow.autonomy;
  const tierOptions = tiersBelow(current);
  const effective: AutonomyTier | undefined = tier === '' ? current : tier;
  const ceilingOptions = effective === undefined ? [] : tiersAtOrBelow(effective);
  const pathList = splitList(paths);
  // A max_autonomy picked before the proposed tier moved lower no longer
  // qualifies; it is treated as unset rather than silently sent.
  const ceiling = maxAutonomy !== '' && ceilingOptions.includes(maxAutonomy) ? maxAutonomy : '';
  const escalationReady = withEscalation && pathList.length > 0 && ceiling !== '';
  const proposesSomething = tier !== '' || escalationReady;
  const escalationIncomplete = withEscalation && !escalationReady;
  const canSubmit =
    current !== undefined &&
    disabledReason === undefined &&
    reason.trim() !== '' &&
    proposesSomething &&
    !escalationIncomplete &&
    !submitting;

  function onSubmit() {
    const body: DelegationLowerRequest = { workflow: workflow.id, reason: reason.trim() };
    if (tier !== '') body.proposed_tier = tier;
    if (withEscalation && pathList.length > 0 && ceiling !== '') {
      body.proposed_escalation = { paths: pathList, max_autonomy: ceiling };
    }
    if (parentEpic.trim() !== '') body.parent_epic = parentEpic.trim();
    const labelList = splitList(labels);
    if (labelList.length > 0) body.labels = labelList;
    const vars = parseTitleVars(titleVars);
    if (vars) body.title_vars = vars;
    void submit(async () => {
      try {
        setFiled(await api.lowerRepoDelegation(owner, name, body));
      } catch (err) {
        throw new Error(captainRefusalMessage(err), { cause: err });
      }
    });
  }

  if (filed?.filed) {
    return (
      <div data-testid={`lower-filed-${workflow.id}`} role="status" className="space-y-1 text-sm">
        <p>
          Filed{' '}
          <a href={filed.filed.url} className="underline" target="_blank" rel="noreferrer">
            #{filed.filed.number}
          </a>{' '}
          — {filed.filed.title}
        </p>
        {filed.filed.applied_labels && filed.filed.applied_labels.length > 0 && (
          <p className="text-xs text-neutral-500">
            Labels: {filed.filed.applied_labels.join(', ')}
          </p>
        )}
        <p className="text-xs text-neutral-600 dark:text-neutral-400">
          Nothing in force changed: the delegation stays as shown until a human lands the edit this
          autonomy:low work item describes.
        </p>
      </div>
    );
  }

  return (
    <div className="space-y-3 text-sm" data-testid={`lower-form-${workflow.id}`}>
      {current === undefined ? (
        <p role="note" className="text-neutral-600 dark:text-neutral-400">
          An undeclared current tier admits no proposal: this workflow declares no autonomy tier, so
          there is nothing to lower from.
        </p>
      ) : (
        <>
          <label className="block">
            <span className="text-xs tracking-wide text-neutral-500 uppercase">Proposed tier</span>
            <select
              aria-label="Proposed tier"
              value={tier}
              onChange={(e) => setTier(e.target.value as AutonomyTier | '')}
              disabled={submitting}
              className="mt-1 block rounded-md border border-neutral-300 bg-white px-2 py-1 text-xs dark:border-neutral-700 dark:bg-neutral-950"
            >
              <option value="">keep {current}</option>
              {tierOptions.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </label>
          {tierOptions.length === 0 && (
            <p className="text-xs text-neutral-500">
              {current} is the lowest tier; only an escalation ceiling can be proposed.
            </p>
          )}
          <label className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={withEscalation}
              onChange={(e) => setWithEscalation(e.target.checked)}
              disabled={submitting}
              aria-label="Propose an escalation ceiling"
            />
            <span>Propose an escalation ceiling</span>
          </label>
          {withEscalation && (
            <div className="space-y-2 border-l border-neutral-200 pl-3 dark:border-neutral-800">
              <TextField
                label="Escalation paths (comma-separated)"
                value={paths}
                onChange={setPaths}
                disabled={submitting}
              />
              <label className="block">
                <span className="text-xs tracking-wide text-neutral-500 uppercase">
                  Ceiling max autonomy
                </span>
                <select
                  aria-label="Ceiling max autonomy"
                  value={ceiling}
                  onChange={(e) => setMaxAutonomy(e.target.value as AutonomyTier | '')}
                  disabled={submitting}
                  className="mt-1 block rounded-md border border-neutral-300 bg-white px-2 py-1 text-xs dark:border-neutral-700 dark:bg-neutral-950"
                >
                  <option value="">choose…</option>
                  {ceilingOptions.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
              </label>
              <p className="text-xs text-neutral-500">
                At or below the effective proposed tier ({effective}); at least one path.
              </p>
            </div>
          )}
          <ReasonField label="Reason" value={reason} onChange={setReason} disabled={submitting} />
          <TextField
            label="Parent epic (optional)"
            value={parentEpic}
            onChange={setParentEpic}
            disabled={submitting}
          />
          <TextField
            label="Labels (optional, comma-separated)"
            value={labels}
            onChange={setLabels}
            disabled={submitting}
          />
          <TextField
            label="Title vars (optional, key=value, comma-separated)"
            value={titleVars}
            onChange={setTitleVars}
            disabled={submitting}
          />
        </>
      )}
      {disabledReason !== undefined && (
        <p role="note" className="text-xs text-neutral-600 dark:text-neutral-400">
          Lowering is unavailable: {disabledReason}
        </p>
      )}
      <Button
        size="sm"
        variant="outline"
        data-delegation-verb="lower"
        disabled={!canSubmit}
        onClick={onSubmit}
      >
        File the lower proposal
      </Button>
      {error && <DecisionError message={error} />}
    </div>
  );
}
