import { ChevronLeft, ChevronRight } from 'lucide-react';
import { Button } from '@/components/ui/button';

interface Props {
  pageIndex: number;
  hasPrev: boolean;
  hasNext: boolean;
  onPrev: () => void;
  onNext: () => void;
  loading?: boolean;
  /** Optional label for the page indicator (e.g., "Runs"). */
  label?: string;
}

/*
 * Prev / Next pair. The page indicator shows 1-based index because
 * "Page 0" reads weird to humans; internally we still track from 0.
 * Disabled-and-styled buttons rather than hiding them so the button
 * geometry doesn't shift between page transitions.
 */
export function Pagination({ pageIndex, hasPrev, hasNext, onPrev, onNext, loading, label }: Props) {
  const pageNumber = pageIndex + 1;
  return (
    <div className="flex items-center justify-end gap-3">
      <span className="text-xs text-neutral-500">
        {label ? `${label} · ` : ''}Page {pageNumber}
      </span>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={onPrev}
          disabled={!hasPrev || loading}
          aria-label="Previous page"
        >
          <ChevronLeft className="size-4" aria-hidden />
          <span>Previous</span>
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={onNext}
          disabled={!hasNext || loading}
          aria-label="Next page"
        >
          <span>Next</span>
          <ChevronRight className="size-4" aria-hidden />
        </Button>
      </div>
    </div>
  );
}
