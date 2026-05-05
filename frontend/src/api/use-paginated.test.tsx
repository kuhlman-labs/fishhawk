import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import { fireEvent } from '@testing-library/react';
import type { PaginatedList } from './types';
import { usePaginated } from './use-paginated';
import { Pagination } from '@/components/pagination';

interface Item {
  id: string;
}

/*
 * Three-page fixture. The fetcher is a plain stub keyed by the
 * cursor argument. Page 1 has next_cursor=B; page 2 has next_cursor=
 * C; page 3 has next_cursor=null (last page). Going Prev from any
 * page must return the previous page's cursor — i.e. the stack-based
 * Prev path that the v0 cursor format requires.
 */
const pages: Record<string, PaginatedList<Item>> = {
  '': { items: [{ id: 'a1' }, { id: 'a2' }], next_cursor: 'B' },
  B: { items: [{ id: 'b1' }, { id: 'b2' }], next_cursor: 'C' },
  C: { items: [{ id: 'c1' }], next_cursor: null },
};

function makeFetcher() {
  return vi.fn(async (cursor: string | null) => pages[cursor ?? '']);
}

interface ProbeProps {
  fetcher: (cursor: string | null) => Promise<PaginatedList<Item>>;
}

function Probe({ fetcher }: ProbeProps) {
  const { state, hasNext, hasPrev, next, prev, pageIndex } = usePaginated<Item>(
    (cursor) => fetcher(cursor),
    [],
  );
  return (
    <div>
      {state.status === 'ok' && (
        <ul data-testid="items">
          {state.data.items.map((it) => (
            <li key={it.id}>{it.id}</li>
          ))}
        </ul>
      )}
      <Pagination
        pageIndex={pageIndex}
        hasPrev={hasPrev}
        hasNext={hasNext}
        onPrev={prev}
        onNext={next}
        loading={state.status === 'loading'}
      />
    </div>
  );
}

async function expectItems(...ids: string[]) {
  await waitFor(() => {
    const list = screen.getByTestId('items');
    const got = Array.from(list.querySelectorAll('li')).map((li) => li.textContent);
    expect(got).toEqual(ids);
  });
}

describe('usePaginated + <Pagination>', () => {
  it('fetches the first page on mount with no cursor', async () => {
    const fetcher = makeFetcher();
    render(<Probe fetcher={fetcher} />);
    await expectItems('a1', 'a2');
    expect(fetcher).toHaveBeenCalledWith(null);
  });

  it('walks through three pages forward via Next, then walks back via Previous', async () => {
    const fetcher = makeFetcher();
    render(<Probe fetcher={fetcher} />);
    await expectItems('a1', 'a2');
    expect(screen.getByRole('button', { name: /previous page/i })).toBeDisabled();

    // → page 2
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: /next page/i }));
    });
    await expectItems('b1', 'b2');
    expect(fetcher).toHaveBeenCalledWith('B');

    // → page 3 (last)
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: /next page/i }));
    });
    await expectItems('c1');
    expect(fetcher).toHaveBeenCalledWith('C');
    expect(screen.getByRole('button', { name: /next page/i })).toBeDisabled();

    // ← page 2 (Prev)
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: /previous page/i }));
    });
    await expectItems('b1', 'b2');

    // ← page 1 (Prev again — back at the start)
    act(() => {
      fireEvent.click(screen.getByRole('button', { name: /previous page/i }));
    });
    await expectItems('a1', 'a2');
    expect(screen.getByRole('button', { name: /previous page/i })).toBeDisabled();
  });

  it('disables Next on the first page when next_cursor is null', async () => {
    const fetcher = vi.fn(
      async (): Promise<PaginatedList<Item>> => ({ items: [{ id: 'only' }], next_cursor: null }),
    );
    render(<Probe fetcher={fetcher} />);
    await expectItems('only');
    expect(screen.getByRole('button', { name: /next page/i })).toBeDisabled();
    expect(screen.getByRole('button', { name: /previous page/i })).toBeDisabled();
  });

  it('shows a 1-based page indicator that advances with Next', async () => {
    const fetcher = makeFetcher();
    render(<Probe fetcher={fetcher} />);
    await expectItems('a1', 'a2');
    expect(screen.getByText(/Page 1$/)).toBeInTheDocument();

    act(() => {
      fireEvent.click(screen.getByRole('button', { name: /next page/i }));
    });
    await expectItems('b1', 'b2');
    expect(screen.getByText(/Page 2$/)).toBeInTheDocument();
  });
});
