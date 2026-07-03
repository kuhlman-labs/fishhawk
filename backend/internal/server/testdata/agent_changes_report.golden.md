# Agent changes report

Generated at: 2026-07-02T14:45:00Z

Filters: repo=acme/app, from=2026-06-01T00:00:00Z, to=2026-06-30T00:00:00Z

Totals: 4 runs in range, 1 agent changes, 1 human-led changes, 2 runs without change

## Agent-authored changes

### 11111111-1111-1111-1111-111111111111

- Repo: acme/app
- Workflow: feature_change
- Trigger: issue:1606
- PR: https://github.com/acme/app/pull/42 (#42) branch=fh/run-merged head=deadbeef
- Merged: yes by bob@acme at 2026-06-10T12:06:00Z base=cafef00d
- Approvals:
  - alice@acme decision=approve surface=review at 2026-06-10T12:04:00Z
- Reviews:
  - plan opus-4-8 (agent) verdict=approve at 2026-06-10T12:02:00Z
  - implement gpt-5.5 (agent) verdict=approve_with_concerns at 2026-06-10T12:03:00Z
- Acceptance: verdict=passed evidence=ev-hash-a,ev-hash-b content_hash=acc-hash-1
- Audit chain: sequences 1–7 (7 entries), first=hash-first last=hash-last
- Evidence:
  - run: /v0/runs/11111111-1111-1111-1111-111111111111
  - audit: /v0/runs/11111111-1111-1111-1111-111111111111/audit
  - export: /v0/audit/export?run_id=11111111-1111-1111-1111-111111111111
  - artifacts: /v0/stages/22222222-2222-2222-2222-222222222222/artifacts

## Human-led changes (reduced evidence)

### 33333333-3333-3333-3333-333333333333

- Repo: acme/app
- Workflow: human_led_change
- Trigger: issue:1590
- PR: https://github.com/acme/app/pull/40 (#40) branch=human/surface head=0ddba11
- Merged: yes by carol@acme at 2026-06-08T12:04:00Z base=feedface
- Approvals:
  - carol@acme decision=approve surface=review at 2026-06-08T12:01:00Z
- Audit chain: sequences 1–5 (5 entries), first=hl-first last=hl-last
- Evidence:
  - run: /v0/runs/33333333-3333-3333-3333-333333333333
  - audit: /v0/runs/33333333-3333-3333-3333-333333333333/audit
  - export: /v0/audit/export?run_id=33333333-3333-3333-3333-333333333333

