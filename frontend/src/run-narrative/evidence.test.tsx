import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { AuditEntry } from '@/api/types';

vi.mock('@/api/client', () => ({ api: { listRunAudit: vi.fn() } }));

import { api } from '@/api/client';
import { DegradeNotes, EvidenceLink, EvidencePanel, TransitionMarker } from './evidence';
import type { EvidenceRef } from './narrative';

const m = vi.mocked(api);
const RUN = 'run-1';

const auditRef: EvidenceRef = {
  kind: 'audit',
  runId: RUN,
  sequence: 42,
  entryHash: 'abcdef0123456789abcdef',
  category: 'approval_submitted',
};
const artifactRef: EvidenceRef = {
  kind: 'artifact',
  runId: RUN,
  stageId: 'st-1',
  artifactId: 'art-1',
  artifactKind: 'plan',
  contentHash: 'sha256:0011223344556677',
};

function wrap(ui: React.ReactNode) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

beforeEach(() => vi.resetAllMocks());

describe('<EvidenceLink>', () => {
  it('links an audit claim to its entry by sequence, never a bare #audit', () => {
    wrap(<EvidenceLink refTo={auditRef} />);
    const link = screen.getByTestId('evidence-link');
    expect(link).toHaveAttribute('href', `/runs/${RUN}?entry=42#entry-42`);
    expect(link).toHaveAttribute('data-evidence-kind', 'audit');
    // Truncated hash as text, the full hash in the title.
    expect(link).toHaveTextContent('abcdef012345…');
    expect(link.getAttribute('title')).toContain('abcdef0123456789abcdef');
  });

  it('links an artifact claim to the stage page that renders it', () => {
    wrap(<EvidenceLink refTo={artifactRef} />);
    const link = screen.getByTestId('evidence-link');
    expect(link).toHaveAttribute('href', `/runs/${RUN}/stages/st-1`);
    expect(link).toHaveTextContent('sha256:00112…');
  });
});

describe('<TransitionMarker>', () => {
  it('carries the treatment as data and labels a gate distinctly from an auto-advance', () => {
    wrap(
      <>
        <TransitionMarker kind="gate" />
        <TransitionMarker kind="auto_advance" />
      </>,
    );
    const [gate, auto] = screen.getAllByTestId('transition-marker');
    expect(gate).toHaveAttribute('data-treatment', 'gate');
    expect(gate).toHaveTextContent('Gate');
    expect(auto).toHaveAttribute('data-treatment', 'auto_advance');
    expect(auto).toHaveTextContent('Auto-advance');
    expect(gate.className).not.toBe(auto.className);
  });
});

describe('<DegradeNotes>', () => {
  it('renders nothing when empty and one note per entry otherwise', () => {
    const { container } = wrap(<DegradeNotes notes={[]} />);
    expect(container.querySelector('[data-testid="degrade-notes"]')).toBeNull();
    wrap(
      <DegradeNotes
        notes={[{ section: 'plan', kind: 'absent', read: 'plan artifact', message: 'no plan' }]}
      />,
    );
    expect(screen.getByRole('note')).toHaveTextContent('no plan');
    expect(screen.getByRole('note')).toHaveAttribute('data-read', 'plan artifact');
  });
});

describe('<EvidencePanel>', () => {
  const found: AuditEntry = {
    id: 'e42',
    sequence: 42,
    run_id: RUN,
    stage_id: null,
    ts: 'T',
    category: 'approval_submitted',
    actor_kind: 'user',
    actor_subject: 'github:octo',
    payload: { decision: 'approve' },
    prev_hash: null,
    entry_hash: 'fullhash-42',
  };

  it('resolves the entry by since_sequence=N-1, limit=1 and shows its hash and payload', async () => {
    m.listRunAudit.mockResolvedValue({ items: [found], next_cursor: null });
    wrap(<EvidencePanel runId={RUN} sequence={42} />);
    expect(await screen.findByTestId('evidence-entry-hash')).toHaveTextContent('fullhash-42');
    expect(screen.getByText(/"decision": "approve"/)).toBeInTheDocument();
    expect(screen.getByTestId('evidence-panel')).toHaveAttribute('id', 'entry-42');
    expect(m.listRunAudit).toHaveBeenCalledWith(RUN, { sinceSequence: 41, limit: 1 });
  });

  it('renders "not found" when the next entry after N-1 is not N', async () => {
    m.listRunAudit.mockResolvedValue({ items: [{ ...found, sequence: 43 }], next_cursor: null });
    wrap(<EvidencePanel runId={RUN} sequence={42} />);
    expect(await screen.findByTestId('evidence-entry-not-found')).toBeInTheDocument();
  });

  it('renders the read error', async () => {
    m.listRunAudit.mockRejectedValue(new Error('boom'));
    wrap(<EvidencePanel runId={RUN} sequence={42} />);
    expect(await screen.findByRole('alert')).toHaveTextContent('boom');
  });
});
