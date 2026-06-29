import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { Campaigns } from './campaigns';
import { api } from '@/api/client';
import type { Campaign, PaginatedList } from '@/api/types';

function makeCampaign(over: Partial<Campaign> = {}): Campaign {
  return {
    id: 'cccccccc-1111-1111-1111-111111111111',
    repo: 'kuhlman-labs/fishhawk',
    epic_ref: 'issue:1439',
    state: 'running',
    pause_policy: 'pause_campaign',
    created_at: '2026-06-29T20:00:00Z',
    updated_at: '2026-06-29T20:05:00Z',
    ...over,
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <Campaigns />
    </MemoryRouter>,
  );
}

describe('<Campaigns>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('renders the heading + eyebrow', async () => {
    vi.spyOn(api, 'listCampaigns').mockResolvedValue({
      items: [],
      next_cursor: null,
    } satisfies PaginatedList<Campaign>);
    renderPage();
    expect(screen.getByRole('heading', { name: /campaigns/i })).toBeInTheDocument();
    expect(screen.getByText(/epic-driven, multi-issue runs/i)).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/no campaigns yet/i)).toBeInTheDocument();
    });
  });

  it('renders the campaigns returned by listCampaigns with their state', async () => {
    vi.spyOn(api, 'listCampaigns').mockResolvedValue({
      items: [
        makeCampaign({ id: 'c1', epic_ref: 'issue:1439', state: 'running' }),
        makeCampaign({ id: 'c2', epic_ref: 'issue:1500', state: 'paused' }),
      ],
      next_cursor: null,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText('issue:1439')).toBeInTheDocument();
      expect(screen.getByText('issue:1500')).toBeInTheDocument();
      expect(screen.getByText('running')).toBeInTheDocument();
      expect(screen.getByText('paused')).toBeInTheDocument();
    });
    // The repo cell deep-links to /campaigns/<id>.
    const link = screen.getAllByRole('link', { name: /kuhlman-labs\/fishhawk/i })[0];
    expect(link).toHaveAttribute('href', '/campaigns/c1');
  });

  it('shows the empty-state message when no campaigns exist', async () => {
    vi.spyOn(api, 'listCampaigns').mockResolvedValue({ items: [], next_cursor: null });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/no campaigns yet/i)).toBeInTheDocument();
    });
  });

  it('surfaces the API error message in the error box', async () => {
    vi.spyOn(api, 'listCampaigns').mockRejectedValue(new Error('boom'));
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/boom/i);
    });
  });
});
