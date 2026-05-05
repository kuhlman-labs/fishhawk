import { AlertTriangle } from 'lucide-react';
import type { Stage } from '@/api/types';
import { describeFailure } from '@/api/types';

/*
 * Inline banner shown above a stage when its state is failed.
 * Surfaces the category letter (A/B/C/D), the canonical description
 * (mirrors run.FailureCategory.Description() in Go), and the
 * stage's failure_reason verbatim. MVP_SPEC §6 requires that
 * failed runs are equally visible to successful ones — this banner
 * is part of how the UI honours that.
 */
export function FailureBanner({ stage }: { stage: Stage }) {
  if (stage.state !== 'failed' || !stage.failure_category) {
    return null;
  }
  const description = describeFailure(stage.failure_category);
  return (
    <div
      role="alert"
      className="flex gap-3 rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <div className="space-y-1">
        <div className="font-medium">
          Failed · category {stage.failure_category}
          {description && <span className="font-normal"> — {description}</span>}
        </div>
        {stage.failure_reason && (
          <div className="font-mono text-xs leading-relaxed">{stage.failure_reason}</div>
        )}
      </div>
    </div>
  );
}
