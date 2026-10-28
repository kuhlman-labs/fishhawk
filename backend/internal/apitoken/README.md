# `backend/internal/apitoken`

Scoped `fhk_` bearer tokens: issue, authenticate, list, revoke. Only the
sha256 of the plaintext is stored (`api_tokens.token_hash`); the plaintext is
returned once, on the issuing call.

## Tenancy contract (#4506)

`api_tokens.account_id` (migration 0055, nullable, FK to `accounts(id)`) binds
a token to a workspace account. `Authenticate` returns it as `Token.AccountID`
and bearerAuth threads it onto `Identity.AccountID`, which the
account-ownership middleware and the list surfaces bound requests by. NULL is
untenanted: the identity resolves accountless (the operator view).

| Mint path | Repository call | `account_id` | Why |
|---|---|---|---|
| `POST /v0/tokens`, tenanted caller | `IssueForAccount` | caller's account | the minted bearer must not leave the caller's account |
| `POST /v0/tokens`, untenanted caller | `Issue` | NULL | operator caller; unchanged |
| `POST /v0/tokens/login` | `IssueOAuth` | NULL, by design | unauthenticated at the bearer layer, no account context; authorized by the operator-repo permission gate, so operator-class |
| `fishhawkd token issue` | `Issue` | NULL, by design | operator bootstrap credential; its holder already has database access |
| first-officer seat attach | `Issue` | NULL at the row | bearerAuth binds a seat identity to its anchor's account at auth time (`resolveFirstOfficerSeat`) |

- `IssueForAccount` is a REQUIRED `Repository` method, not an optional seam, so
  no wiring gap can turn a tenanted mint into an untenanted one (the
  E44.11 / #2074 rule). It fails closed: an empty or malformed account id is
  an error from `uuid.Parse`, never a silent untenanted mint, and an id with no
  `accounts` row fails on the FK. No row is written on any failure.
- `Issue` and `IssueForAccount` share one `CreateToken` query; a nil account
  writes NULL.
- Pinned by `TestIssueForAccount_PersistsAccount` and
  `TestIssueForAccount_RefusesInvalidAccount` (`postgres_test.go`, real
  Postgres, committed rows read back by SQL); the handler side is in
  `backend/internal/server/README.md` § "`POST /v0/tokens` mint ceiling".

## sqlc asymmetry

`db/queries.sql.go` is sqlc output, hand-kept in step with `queries.sql`.
`GetTokenByHash` and `CreateToken` select `account_id`; `CreateOAuthToken`,
`GetTokenByID`, `ListTokensForSubject` and `RevokeToken` still list the
pre-0055 columns, so `rowToToken` maps their `AccountID` to `""`. Those paths
do not consume the account. A later `sqlc generate` adds the column to them
too, which is harmless.

## Inventory: OAuth-login tokens minted before the client binding (#4627)

`POST /v0/tokens/login` now binds the presented forge token to the
deployment's own device OAuth client before it mints. Rows minted before that
change were verified by token owner only. This change revokes and rewrites
nothing; for routine review, list the active OAuth-login rows (read-only):

```sql
SELECT id, subject, provider, scopes, created_at, last_used_at
  FROM api_tokens
 WHERE auth_method = 'oauth'
   AND revoked_at IS NULL
 ORDER BY created_at;
```

- Compare `created_at` with the deploy time of the binding; rows minted after
  it went through the client check.
- Owner path: `DELETE /v0/tokens/{token_id}` as the token's subject.
- Operator path, for a reviewed id only:
  `UPDATE api_tokens SET revoked_at = now() WHERE id = '<reviewed-id>' AND revoked_at IS NULL;`
- The holder re-mints with `fishhawk token login` against the deployment's
  own device client.

## Inventory: pre-existing untenanted tokens held by tenanted members

Tokens minted through `POST /v0/tokens` before #4506 are untenanted whatever
the caller's account. This change stamps newly minted rows only; it revokes
and rewrites nothing. To list the active untenanted rows whose subject is a
workspace member, for manual review (read-only):

```sql
SELECT t.id, t.subject, t.auth_method, t.scopes, t.created_at, t.last_used_at,
       m.account_id, m.role
  FROM api_tokens t
  JOIN account_members m ON t.subject = m.provider || ':' || m.member_ref
 WHERE t.account_id IS NULL
   AND t.revoked_at IS NULL
 ORDER BY t.created_at;
```

- Caveat: the join assumes `account_members.member_ref` holds the login form
  that `<provider>:<login>` session subjects use. A `member_ref` stored as a
  numeric forge id does not match and its tokens are not listed.
- A listed row is not necessarily wrong: an operator who is also a member may
  hold an intentionally untenanted token (CLI bootstrap, OAuth login).
- The query revokes nothing. Review the rows and revoke by hand
  (`DELETE /v0/tokens/{token_id}` as the owner), then re-mint through
  `POST /v0/tokens` from the member's tenanted session.
