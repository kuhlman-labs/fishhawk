import type { StageState } from '@/api/types';
import { cn } from '@/lib/cn';

/*
 * Stage-state badge used by both the run-detail stage list and the
 * stage-detail page header (#205). One component so the visual
 * language for run lifecycle stays consistent across the UI; if a
 * state's color changes, both surfaces pick it up automatically.
 */
const stageStateStyles: Record<StageState, string> = {
  pending: 'text-neutral-500',
  dispatched: 'text-blue-700 dark:text-blue-300',
  running: 'text-blue-700 dark:text-blue-300',
  awaiting_approval: 'text-amber-700 dark:text-amber-300',
  succeeded: 'text-emerald-700 dark:text-emerald-300',
  failed: 'text-rose-700 dark:text-rose-300',
  cancelled: 'text-neutral-500',
};

export function StageStateBadge({ state, className }: { state: StageState; className?: string }) {
  return (
    <span className={cn('font-mono text-xs', stageStateStyles[state], className)}>{state}</span>
  );
}
