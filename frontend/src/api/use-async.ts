import { useEffect, useState } from 'react';

/*
 * Minimal async loader. Returns one of:
 *   { status: 'loading' }
 *   { status: 'error', error: Error }
 *   { status: 'ok', data: T }
 *
 * No caching, no retries, no abort: each route mount fetches fresh,
 * which is exactly what plan review wants — reviewers refresh to see
 * state changes from approvals or re-runs. Reach for TanStack Query
 * when we need shared cache or invalidation; until then this stays
 * boring.
 *
 * The fetcher receives an AbortSignal so callers can wire request
 * cancellation if they want; today nobody does.
 */
export type AsyncState<T> =
  | { status: 'loading' }
  | { status: 'error'; error: Error }
  | { status: 'ok'; data: T };

export function useAsync<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  deps: ReadonlyArray<unknown>,
): AsyncState<T> {
  const [state, setState] = useState<AsyncState<T>>({ status: 'loading' });

  useEffect(() => {
    let cancelled = false;
    const ctrl = new AbortController();
    setState({ status: 'loading' });

    fetcher(ctrl.signal)
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
  }, deps);

  return state;
}
