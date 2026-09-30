import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { DelegationVerbResponse, RepoDelegationWorkflow } from '@/api/captain';
import { DelegationLowerForm } from './delegation-lower-form';

/*
 * The "propose lower" form driven through the REAL api client over a stubbed
 * fetch. Fixtures are literal `RepoDelegationWorkflow` / `DelegationVerbResponse`
 * wire bodies. Counterfactual (4): the strictly-below filter (a `medium`
 * workflow offers EXACTLY {low}) and the undeclared-tier refusal (no
 * `autonomy` key offers nothing and keeps submit disabled).
 */

function workflow(over: Partial<RepoDelegationWorkflow> = {}): RepoDelegationWorkflow {
  return {
    id: 'feature_change',
    autonomy: 'medium',
    matrix: [{ action: 'merge', mode: 'gated', source: 'tier' }],
    content_hash: 'c'.repeat(64),
    ...over,
  };
}

const FILED: DelegationVerbResponse = {
  repo: 'acme/app',
  workflow: { workflow: 'feature_change', status: 'confirmed' },
  event: {
    sequence: 12,
    entry_hash: 'e'.repeat(64),
    category: 'delegation_lower_proposed',
    at: '2026-09-29T12:00:00Z',
    payload: {},
  },
  filed: {
    number: 4242,
    url: 'https://github.com/acme/app/issues/4242',
    title: '[E9.1] Lower feature_change to low',
    applied_labels: ['autonomy:low', 'type:chore'],
  },
};

function stub(status = 200, body: unknown = FILED) {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
  );
  vi.stubGlobal('fetch', fetchMock);
  return {
    posts: () =>
      fetchMock.mock.calls.map((call) => {
        const [input, init] = call as unknown as [RequestInfo, RequestInit];
        return {
          path: new URL(String(input), 'http://localhost').pathname,
          method: init.method,
          body: JSON.parse(String(init.body)) as unknown,
        };
      }),
  };
}

function tierOptions(): string[] {
  const select = screen.queryByLabelText('Proposed tier') as HTMLSelectElement | null;
  if (!select) return [];
  return Array.from(select.options)
    .map((o) => o.value)
    .filter((v) => v !== '');
}

function ceilingOptions(): string[] {
  const select = screen.getByLabelText('Ceiling max autonomy') as HTMLSelectElement;
  return Array.from(select.options)
    .map((o) => o.value)
    .filter((v) => v !== '');
}

function submitButton(): HTMLButtonElement {
  return document.querySelector('[data-delegation-verb="lower"]') as HTMLButtonElement;
}

function fillReason(text = 'the loop keeps shipping unreviewed migrations') {
  fireEvent.change(screen.getByLabelText('Reason'), { target: { value: text } });
}

afterEach(() => vi.unstubAllGlobals());

describe('<DelegationLowerForm> tier options (counterfactual 4)', () => {
  it('a medium workflow offers EXACTLY {low}', () => {
    render(<DelegationLowerForm owner="acme" name="app" workflow={workflow()} />);
    expect(tierOptions()).toEqual(['low']);
  });

  it('a high workflow offers {medium, low}', () => {
    render(
      <DelegationLowerForm owner="acme" name="app" workflow={workflow({ autonomy: 'high' })} />,
    );
    expect(tierOptions()).toEqual(['medium', 'low']);
  });

  it('a low workflow offers no tier, only an escalation ceiling', () => {
    render(
      <DelegationLowerForm owner="acme" name="app" workflow={workflow({ autonomy: 'low' })} />,
    );
    expect(tierOptions()).toEqual([]);
    expect(screen.getByText(/low is the lowest tier/)).toBeInTheDocument();
    expect(screen.getByLabelText('Propose an escalation ceiling')).toBeInTheDocument();
  });

  it('an undeclared tier offers nothing, names the refusal and keeps submit disabled', () => {
    const wf = workflow();
    delete wf.autonomy;
    render(<DelegationLowerForm owner="acme" name="app" workflow={wf} />);
    expect(tierOptions()).toEqual([]);
    expect(screen.queryByLabelText('Propose an escalation ceiling')).toBeNull();
    expect(screen.getByRole('note')).toHaveTextContent(
      'An undeclared current tier admits no proposal',
    );
    expect(submitButton()).toBeDisabled();
  });
});

describe('<DelegationLowerForm> escalation ceiling', () => {
  it('restricts max_autonomy to at-or-below the EFFECTIVE proposed tier', () => {
    render(
      <DelegationLowerForm owner="acme" name="app" workflow={workflow({ autonomy: 'high' })} />,
    );
    fireEvent.click(screen.getByLabelText('Propose an escalation ceiling'));
    expect(ceilingOptions()).toEqual(['high', 'medium', 'low']);
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'medium' } });
    expect(ceilingOptions()).toEqual(['medium', 'low']);
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
    expect(ceilingOptions()).toEqual(['low']);
  });

  it('an incomplete escalation (no path) keeps submit disabled even with a tier chosen', () => {
    render(<DelegationLowerForm owner="acme" name="app" workflow={workflow()} />);
    fillReason();
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
    expect(submitButton()).toBeEnabled();
    fireEvent.click(screen.getByLabelText('Propose an escalation ceiling'));
    expect(submitButton()).toBeDisabled();
  });

  it('files an escalation-only proposal with the exact body', async () => {
    const s = stub();
    render(
      <DelegationLowerForm owner="acme" name="app" workflow={workflow({ autonomy: 'low' })} />,
    );
    fillReason('migrations need a human');
    fireEvent.click(screen.getByLabelText('Propose an escalation ceiling'));
    fireEvent.change(screen.getByLabelText('Escalation paths (comma-separated)'), {
      target: { value: 'db/migrations/**, deploy/**' },
    });
    fireEvent.change(screen.getByLabelText('Ceiling max autonomy'), { target: { value: 'low' } });
    fireEvent.click(submitButton());
    await screen.findByTestId('lower-filed-feature_change');
    expect(s.posts()).toEqual([
      {
        path: '/v0/repos/acme/app/delegation/lower',
        method: 'POST',
        body: {
          workflow: 'feature_change',
          reason: 'migrations need a human',
          proposed_escalation: { paths: ['db/migrations/**', 'deploy/**'], max_autonomy: 'low' },
        },
      },
    ]);
  });
});

describe('<DelegationLowerForm> submit', () => {
  it('blank reason or nothing proposed keeps submit disabled', () => {
    render(<DelegationLowerForm owner="acme" name="app" workflow={workflow()} />);
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
    expect(submitButton()).toBeDisabled();
    fillReason('   ');
    expect(submitButton()).toBeDisabled();
    fillReason();
    expect(submitButton()).toBeEnabled();
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: '' } });
    expect(submitButton()).toBeDisabled();
  });

  it('a named disabledReason disables submit and is rendered', () => {
    render(
      <DelegationLowerForm
        owner="acme"
        name="app"
        workflow={workflow()}
        disabledReason="no delegation-confirmation store on this instance"
      />,
    );
    fillReason();
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
    expect(submitButton()).toBeDisabled();
    expect(
      screen.getByText(/Lowering is unavailable: no delegation-confirmation store/),
    ).toBeInTheDocument();
  });

  it('POSTs the exact body, renders the ONE filed item, states nothing changed, fires onFiled', async () => {
    const s = stub();
    const onFiled = vi.fn();
    render(<DelegationLowerForm owner="acme" name="app" workflow={workflow()} onFiled={onFiled} />);
    fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
    fillReason(' too much autonomy ');
    fireEvent.change(screen.getByLabelText('Parent epic (optional)'), {
      target: { value: '#9' },
    });
    fireEvent.change(screen.getByLabelText('Labels (optional, comma-separated)'), {
      target: { value: 'area:backend, phase:beta' },
    });
    fireEvent.change(screen.getByLabelText('Title vars (optional, key=value, comma-separated)'), {
      target: { value: 'area=backend, junk' },
    });
    fireEvent.click(submitButton());
    const filed = await screen.findByTestId('lower-filed-feature_change');
    expect(s.posts()).toEqual([
      {
        path: '/v0/repos/acme/app/delegation/lower',
        method: 'POST',
        body: {
          workflow: 'feature_change',
          reason: 'too much autonomy',
          proposed_tier: 'low',
          parent_epic: '#9',
          labels: ['area:backend', 'phase:beta'],
          title_vars: { area: 'backend' },
        },
      },
    ]);
    expect(filed).toHaveTextContent('#4242');
    expect(filed).toHaveTextContent('[E9.1] Lower feature_change to low');
    expect(filed).toHaveTextContent('autonomy:low, type:chore');
    expect(filed).toHaveTextContent('Nothing in force changed');
    expect(screen.getByRole('link', { name: '#4242' })).toHaveAttribute(
      'href',
      'https://github.com/acme/app/issues/4242',
    );
    await waitFor(() => expect(onFiled).toHaveBeenCalledTimes(1));
  });
});

describe('<DelegationLowerForm> refusals', () => {
  const cases: Array<[string, number, Record<string, unknown>, RegExp]> = [
    [
      'delegation_raise_refused',
      400,
      { error: 'delegation_raise_refused', message: 'not strictly lower' },
      /can only lower delegation.*\(400 · delegation_raise_refused\)/,
    ],
    [
      'delegation_nothing_proposed',
      400,
      { error: 'delegation_nothing_proposed', message: 'nothing' },
      /Nothing was proposed.*\(400 · delegation_nothing_proposed\)/,
    ],
    [
      'work_item_invalid',
      422,
      { error: 'work_item_invalid', message: 'bad title' },
      /work-item conventions.*\(422 · work_item_invalid\)/,
    ],
    [
      'work_item_filing_failed',
      502,
      { error: 'work_item_filing_failed', message: 'forge down' },
      /could not be filed at the forge.*\(502 · work_item_filing_failed\)/,
    ],
    [
      'a 500 carrying details.filed_ref',
      500,
      { error: 'internal', message: 'append failed', details: { filed_ref: 'acme/app#4243' } },
      /500 · internal The work item WAS filed \(acme\/app#4243\) but recording it on the chain failed/,
    ],
    [
      'an unmapped code (fallback)',
      418,
      { error: 'brand_new_refusal', message: 'new' },
      /^418 · brand_new_refusal$/,
    ],
  ];

  for (const [label, status, body, want] of cases) {
    it(`renders ${label} with the raw server code`, async () => {
      stub(status, body);
      render(<DelegationLowerForm owner="acme" name="app" workflow={workflow()} />);
      fireEvent.change(screen.getByLabelText('Proposed tier'), { target: { value: 'low' } });
      fillReason();
      fireEvent.click(submitButton());
      expect(await screen.findByRole('alert')).toHaveTextContent(want);
      expect(screen.queryByTestId('lower-filed-feature_change')).toBeNull();
    });
  }
});
