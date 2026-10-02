---
id: ADR-005
title: "API auth/session model"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/69
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-005: API auth/session model

## Context

Users sign in via GitHub OAuth (MVP_SPEC §5.4). The Web UI talks to the Go backend. CLI uses scoped API tokens. Decision: how does the browser maintain a session? HTTP-only cookie or bearer JWT in localStorage?

## Options

- **HTTP-only secure cookie session** — set on a sign-in callback, sent automatically with same-origin requests. Resistant to XSS-driven token theft. Requires CSRF protection on state-changing endpoints. Works well with same-origin SPA + Go backend.
- **Bearer JWT in localStorage** — flexible, works across origins, easier debugging. Vulnerable to XSS-driven theft. CSP can mitigate but not eliminate.
- **Hybrid** — short-lived JWT in memory + refresh token in HTTP-only cookie. More moving parts.

## Recommendation

HTTP-only secure cookie. Same-origin app, audit-grade product, XSS resistance matters.

## Decision

**Recorded 2026-04-30: HTTP-only secure cookie session for browser; bearer tokens for the CLI.**

**Browser (Web UI):**

- After OAuth callback, the backend sets `fishhawk_session` cookie: `HttpOnly; Secure; SameSite=Lax; Path=/`.
- Session value is an opaque random ID (not a JWT) keyed into a server-side `sessions` table. Server-side state means revocation is immediate.
- Session lifetime: 24 hours sliding (refreshed on each authenticated request); absolute lifetime: 7 days.
- **CSRF**: state-changing endpoints (POST, PUT, PATCH, DELETE) require an `X-CSRF-Token` header that matches a server-issued token tied to the session. The token is delivered via a `__Host-csrf` cookie (readable from JS, distinct from the session cookie) on the first authenticated request.

**CLI:**

- Scoped API tokens issued via `POST /v0/tokens` (under E4.5 / #51), passed via `Authorization: Bearer <token>` on every request.
- Tokens are server-side records with explicit scopes, revocable, and audit-logged on issue/use/revoke.
- Tokens are NOT JWTs — they are opaque IDs that index a row. Keeps revocation immediate.

## Consequences

**Easier**
- XSS theft of session credentials is precluded (HttpOnly).
- Revocation is immediate (server-side state) — important for audit-grade systems where a leaked credential must be invalidated within minutes.
- The CLI auth model is symmetric with the API model: a token corresponds to a row, scopes are explicit, audit is automatic.

**Harder**
- CSRF protection adds a small layer of plumbing (token issuance + verification middleware). Standard, but not free.
- Same-origin requirement: the Web UI and the API must share a domain/subdomain (e.g., `app.fishhawk.[tld]` and `api.fishhawk.[tld]` with cookies scoped to `.fishhawk.[tld]`). Tighter coupling than a JWT-everywhere model.
- Mobile / future SDK clients won't share the cookie model — they'll use the bearer-token path. Acceptable; that's already the CLI shape.

**Other decisions this constrains**
- E4.2 (#49) implements the OAuth → cookie flow.
- E4.5 (#51) implements scoped API tokens for the bearer path.
- E7.2 (#38) wires the SPA's auth state to the cookie model (no client-side token handling).

## Spec reference

`docs/MVP_SPEC.md` §5.4

## Target deadline

Day 5 — **met**.

---
Parent epic: #15
