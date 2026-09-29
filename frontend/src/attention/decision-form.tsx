import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { ApiClientError } from '@/api/client';

/*
 * Shared submit machinery for the attention-queue decision panels
 * (E40.2 / #1717). The three simple panels (scope amendment, concern,
 * acceptance) carry no duplicated state code: they drive `useDecisionSubmit`
 * and compose the presentational primitives below.
 *
 * The remove-on-success contract is centralised here: `onResolved` is called
 * ONLY on a resolved submit — never in the catch. A failing submit surfaces
 * the error and leaves the item in the queue.
 */

type SubmitPhase = { kind: 'idle' } | { kind: 'submitting' } | { kind: 'errored'; message: string };

// formatDecisionError renders a thrown error the same way approval-panel.tsx's
// formatApprovalError does: `{status} · {body.error}` for an ApiClientError.
function formatDecisionError(err: unknown): string {
  if (err instanceof ApiClientError) {
    return `${err.status} · ${err.body?.error ?? err.message}`;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'unknown error';
}

export interface DecisionSubmit {
  submitting: boolean;
  error: string | null;
  submit: (run: () => Promise<unknown>) => Promise<void>;
}

// The shared submit hook is deliberately co-located with the form primitives
// it drives (E40.2); Fast Refresh's one-export rule does not apply to a hook.
// eslint-disable-next-line react-refresh/only-export-components
export function useDecisionSubmit(onResolved: () => void): DecisionSubmit {
  const [phase, setPhase] = useState<SubmitPhase>({ kind: 'idle' });

  async function submit(run: () => Promise<unknown>) {
    setPhase({ kind: 'submitting' });
    try {
      await run();
      // Remove-only-on-success: onResolved fires on the resolution path ONLY,
      // never in a catch or finally, so a non-2xx leaves the item in the list.
      onResolved();
      setPhase({ kind: 'idle' });
    } catch (err) {
      setPhase({ kind: 'errored', message: formatDecisionError(err) });
    }
  }

  return {
    submitting: phase.kind === 'submitting',
    error: phase.kind === 'errored' ? phase.message : null,
    submit,
  };
}

export function ReasonField({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <label className="block">
      <span className="text-xs tracking-wide text-neutral-500 uppercase">{label}</span>
      <textarea
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        rows={2}
        className="mt-1 w-full rounded-md border border-neutral-300 bg-white px-2 py-1 font-mono text-xs disabled:opacity-50 dark:border-neutral-700 dark:bg-neutral-950"
        aria-label={label}
      />
    </label>
  );
}

export function TextField({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <label className="block">
      <span className="text-xs tracking-wide text-neutral-500 uppercase">{label}</span>
      <input
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        className="mt-1 w-full rounded-md border border-neutral-300 bg-white px-2 py-1 font-mono text-xs disabled:opacity-50 dark:border-neutral-700 dark:bg-neutral-950"
        aria-label={label}
      />
    </label>
  );
}

export function AcknowledgeCheckbox({
  label,
  checked,
  onChange,
  disabled,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className="flex items-start gap-2 text-sm text-neutral-800 dark:text-neutral-200">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        disabled={disabled}
        className="mt-0.5"
        aria-label={label}
      />
      <span>{label}</span>
    </label>
  );
}

/** One submit verb: its label, its action, and whether its required field is blank. */
export interface DecisionVerbAction {
  label: string;
  onClick: () => void;
  /** True when this verb's REQUIRED field is empty; disables the button. */
  requiredBlank?: boolean;
}

/*
 * Cancel + one Button per verb. The blank-required-field guard lives HERE in
 * one place: each verb button is disabled while submitting OR while its
 * required field is blank. Every verb button carries `data-decision-verb` so
 * the queue's precise-verb-equality test can scope to submit verbs only —
 * Cancel deliberately does NOT carry it.
 */
export function DecisionActions({
  verbs,
  onCancel,
  submitting,
}: {
  verbs: DecisionVerbAction[];
  onCancel: () => void;
  submitting: boolean;
}) {
  return (
    <div className="flex flex-wrap justify-end gap-2">
      <Button variant="ghost" size="sm" onClick={onCancel} disabled={submitting}>
        Cancel
      </Button>
      {verbs.map((v) => (
        <Button
          key={v.label}
          size="sm"
          variant="outline"
          data-decision-verb=""
          onClick={v.onClick}
          disabled={submitting || !!v.requiredBlank}
        >
          {v.label}
        </Button>
      ))}
    </div>
  );
}

export function DecisionError({ message }: { message: string }) {
  return (
    <p role="alert" className="font-mono text-xs text-rose-700 dark:text-rose-300">
      {message}
    </p>
  );
}

/*
 * Fail-closed refusal: an AttentionItem whose required ids are absent (they
 * are all optional on the wire) renders this named line and NO submit button,
 * rather than building a `/runs/undefined/...` URL.
 */
export function DecisionRefusal({ message }: { message: string }) {
  return (
    <p className="text-sm text-neutral-500 dark:text-neutral-400" role="note">
      {message}
    </p>
  );
}
