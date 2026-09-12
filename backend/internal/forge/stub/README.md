# `backend/internal/forge/stub` — in-process stub forge for the acceptance preview

**E72.3 / #3327.** The stateful GitHub and GitLab test fakes that
`backend/internal/server/split_parent_close_test.go` grew for the E50.6
parent-close watcher, promoted into a package the acceptance preview's own
`fishhawkd` mounts under `--dev-stub-forge` / `FISHHAWKD_DEV_STUB_FORGE=1`.
The sandboxed acceptance agent reaches only the spec-declared egress host
(`localhost:8090`), so the stub is an `http.Handler` served **in-process**
through an `http.RoundTripper` — no second port, no second binary. The
control API (`/v0/dev/forge*`) and the `fishhawkd` wiring live in
`backend/internal/server/devforge.go` and `backend/cmd/fishhawkd/serve.go`;
this package is the state, the two family handlers and the transport.

**Never in production.** `serve.go` refuses `--dev-stub-forge` when a GitHub
App or GitLab token is configured; the credentials below are fixed public dev
constants; the base URLs use the RFC 2606 `.invalid` TLD so a request that
escaped the in-process transport could not resolve on the network.

## State model (`stub.go`)

One mutex-guarded `*Forge` (`New()`), safe for concurrent use — the webhook
receivers and the control API reach it from different goroutines.

| Record | Key | Native state vocabulary |
|---|---|---|
| `Issue` | `github:<owner/name>#<n>` / `gitlab:<project-id>#<n>` | GitHub `open`/`closed` + `state_reason`; GitLab `opened`/`closed` |
| `PullRequest` | same | GitHub `open`/`closed` + `merged`; GitLab `opened`/`closed`/`merged` |
| project path → id | GitLab namespaced path | populated by `SeedIssue`/`SeedPullRequest` when `Repo` is set |

State is held **natively** so the real adapters' normalization is exercised
(`forgegitlab` maps `opened` → `open`), not bypassed. Two repositories may
hold the same number simultaneously.

Exported surface: `SeedIssue` / `GetIssue`, `SeedPullRequest` /
`GetPullRequest` (an empty `State` defaults to the family's native open
word; a seed failing `validateSeed` — unknown forge, non-positive number,
GitHub without `Repo`, GitLab without `ProjectID` — returns a wrapped
`ErrInvalid` and stores nothing), `Snapshot()` (JSON-shaped
`{github:{issues,pulls}, gitlab:{issues,merge_requests}, requests}`, records
sorted by key), `Reset()` (drops records, request log, faults and the
notes page-size override), `Requests()` (arrival-ordered `"<op> <key>"`
strings — the watcher's comment-before-close invariant is an ORDERING
property), `SetFault(op, on)` (the named `Op*` endpoint answers 500 until
cleared, so a consumer's error branch is exercised in isolation and then
proven transient), `SetNotesPageSize(n)` (forces GitLab notes pagination
with a short thread).

## Served subsets

`GitHubHandler()` (`github.go`):

| Route | Behaviour |
|---|---|
| `POST /app/installations/{id}/access_tokens` | `201 {token: InstallationToken, expires_at: now+1h}` — a signer-backed `githubapp.Client` mints against it |
| `GET /repos/{o}/{n}` | `{id, full_name, default_branch: "main"}` |
| `GET` / `PATCH /repos/{o}/{n}/issues/{n}` | PATCH writes `state` and `state_reason` **verbatim** (GitHub semantics); also `title`/`body` |
| `GET` / `POST /repos/{o}/{n}/issues/{n}/comments` | arrival order; single page |
| `GET /repos/{o}/{n}/pulls/{n}` | `node_id`, `state`, `merged`, `merged_at` (JSON `null` when unmerged), `merge_commit_sha`, `head.sha/ref`, `base.ref` |

`GitLabHandler()` (`gitlab.go`):

| Route | Behaviour |
|---|---|
| `GET /api/v4/projects/{url-encoded path}` | `{id, path_with_namespace, web_url}`; parsed from `EscapedPath()` so `%2F` stays one segment |
| `GET /api/v4/projects/{id}` | same shape by id (reverse lookup of a registered path) |
| `GET` / `PUT /api/v4/projects/{id}/issues/{iid}` | PUT changes state **only** via `state_event` `close`/`reopen`; a bare `state` field is not decoded and is ignored, as the real API does; an unknown `state_event` is `400` |
| `GET` / `POST .../issues/{iid}/notes` | `per_page`/`page` pagination with an RFC 8288 `rel="next"` `Link` **rooted at `GitLabBaseURL`** — the real client follows a next link only on its own scheme+host |
| `GET /api/v4/projects/{id}/merge_requests/{iid}` | `state`, `sha`, `merge_commit_sha`, `merged_at`, branches |

Every unknown path or wrong method on either family answers a **JSON 404,
never a panic** — the board-sync reconciler's conventions fetch on the same
`issues.closed` delivery hits a contents path the stub does not serve and
must simply fail its load (both adapters map it to `forge.ErrNotFound`).

## Transport (`transport.go`)

`Transport(h)` serves each request through `h` into a minimal
`ResponseWriter` capture (no `httptest` import in production code) and
returns the `*http.Response`; a cancelled request context is refused before
dispatch. `(*Forge).Handler()` routes by host — `GitHubBaseURL`'s host →
`GitHubHandler`, `GitLabBaseURL`'s host → `GitLabHandler`, anything else 404.
`(*Forge).HTTPClient()` wraps it; hand it to `githubclient.Client.HTTP` and
`forgegitlab.WithHTTPClient`.

`StaticTokens{Value}` is the `githubapp.TokenProvider` for the stub path.
The **method** name is fixed by the interface —
`Token(ctx context.Context, installationID int64) (string, error)` at
`backend/internal/githubapp/cache.go` — so the **field** is `Value` (Go
rejects a field and a method of one name on a type). Its second parameter
is blank: this package must never declare an `installationID int64`
identifier, which the forge credential-scope gate
(`backend/internal/forge/credential_scope_gate_test.go`) forbids outside
its allowlist. `var _ githubapp.TokenProvider = StaticTokens{}` pins
conformance at build time.

`SignGitHubDelivery(secret, body)` returns `"sha256=" + hex(HMAC-SHA256)`,
exactly what `webhook.VerifySignature` accepts.

Wiring, as `stub_test.go` and `serve.go` do it:

```go
st := stub.New()
gh := forgegithub.New(&githubclient.Client{
    BaseURL: stub.GitHubBaseURL,
    Tokens:  stub.StaticTokens{Value: stub.InstallationToken},
    HTTP:    st.HTTPClient(),
})
gl := forgegitlab.New(stub.GitLabBaseURL,
    forgegitlab.NewStaticCredentialProvider(stub.InstallationToken),
    forgegitlab.WithHTTPClient(st.HTTPClient()))
```

## Tests (`stub_test.go`)

Every behavioural test drives the stub **only through the real adapters**
(`forgegithub` over a genuine `*githubclient.Client`, `forgegitlab` over the
genuine `gitlabclient` factory) so the served subset is proven to be what
the product calls: issue round-trips on both forges, GitLab native-state
normalization, notes pagination to exhaustion with a page size of 2, merged
pull/merge-request fields, project-path lookup (`ResolveRepoScope`), every
`SetFault` op surfacing through the adapter and then clearing, unknown path →
`forge.ErrNotFound`, request-log ordering, `Reset`, `Snapshot` shape, seed
validation, `StaticTokens`, and `SignGitHubDelivery` against the real
receiver verifier. `TestStubGitLab_PutIgnoresBareState` and
`TestStub_RawEdgeResponses` send raw HTTP because no adapter produces the
malformed inputs they pin. Counterfactuals run at implement time: writing a
bare `state` verbatim reddens `TestStubGitLab_PutIgnoresBareState`; dropping
the `Link` emission reddens `TestStubGitLab_NotesPaginateToExhaustion`
(only page one reaches the adapter).

A branch no client can reach: `url.PathUnescape` failing on the project
segment — `URL.EscapedPath()` only returns a `RawPath` that is a valid
encoding, so the error arm is defensive and untested.
