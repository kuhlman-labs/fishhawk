import { useCallback, useEffect, useState } from 'react';
import type { PaginatedList } from './types';
import type { AsyncState } from './use-async';

/*
 * Hook for cursor-paginated lists. Owns the current cursor and the
 * history stack (the v0 cursor format is opaque + non-reversible —
 * `next_cursor` lets you walk forward, but there's no `prev_cursor`,
 * so we remember the cursors we've passed through to step back).
 *
 *   prev_cursors:  [null, A, B]   ← stack of cursors leading up to here
 *   cursor:        C              ← current page request
 *   data:          {items, next_cursor: D}
 *
 * Going Next pushes `cursor` onto the stack and sets cursor=D.
 * Going Prev pops the stack and sets cursor=B.
 *
 * `deps` lets callers force the pagination state to reset (e.g.,
 * when a filter changes); a new `deps` array clears history and
 * sends the cursor back to null. The fetcher itself receives the
 * current cursor on each fetch.
 */
export interface PaginatedHandle<T> {
  state: AsyncState<PaginatedList<T>>;
  pageIndex: number;
  hasNext: boolean;
  hasPrev: boolean;
  next: () => void;
  prev: () => void;
}

export function usePaginated<T>(
  fetcher: (cursor: string | null, signal: AbortSignal) => Promise<PaginatedList<T>>,
  deps: ReadonlyArray<unknown> = [],
): PaginatedHandle<T> {
  const [cursor, setCursor] = useState<string | null>(null);
  const [history, setHistory] = useState<Array<string | null>>([]);
  const [state, setState] = useState<AsyncState<PaginatedList<T>>>({ status: 'loading' });

  // Reset paging state when the caller's deps change (filter shifts,
  // route remounts, etc.). The dependency array on the inner effect
  // already covers `cursor`; this effect is just for the explicit
  // outer change.
  useEffect(() => {
    setCursor(null);
    setHistory([]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    let cancelled = false;
    const ctrl = new AbortController();
    setState({ status: 'loading' });
    fetcher(cursor, ctrl.signal)
      .then((data) => {
        if (!cancelled) setState({ status: 'ok', data });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const error = err instanceof Error ? err : new Error(String(err));
        setState({ status: 'error', error });
      });
    return () => {
      cancelled = true;
      ctrl.abort();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cursor, ...deps]);

  const next = useCallback(() => {
    if (state.status !== 'ok' || !state.data.next_cursor) return;
    setHistory((h) => [...h, cursor]);
    setCursor(state.data.next_cursor);
  }, [state, cursor]);

  const prev = useCallback(() => {
    setHistory((h) => {
      if (h.length === 0) return h;
      const previous = h[h.length - 1];
      setCursor(previous);
      return h.slice(0, -1);
    });
  }, []);

  const hasNext = state.status === 'ok' && state.data.next_cursor !== null;
  const hasPrev = history.length > 0;
  const pageIndex = history.length;

  return { state, pageIndex, hasNext, hasPrev, next, prev };
}
