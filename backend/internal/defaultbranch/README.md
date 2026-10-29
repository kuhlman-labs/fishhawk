# backend/internal/defaultbranch

One cached resolver for a repository's base branch, read from the forge `default_branch` (#4660). It is the ADR-110 rule-4 realization for the base branch: one resolver over the existing forge `GetRepository` seam, a closed vocabulary of named refusal reasons, and a TTL cache, so no server-side consumer guesses a literal branch. The package imports only `forge` and the standard library, so `githubclient`, `orchestrator` and `server` all depend on it without a cycle.

## API

- `Source` — `Resolve(ctx, scope, repo) (string, error)`. Production wires `*Resolver`; test fixtures may wire `Static`.
- `Resolve(ctx, src, scope, repo)` — the ONE nil check every consumer uses: a nil `Source` refuses with `resolver_unconfigured`.
- `Getter` — `GetRepository(ctx, scope, repo) (*forge.Repository, error)`; `*githubclient.Client` satisfies it through its type aliases.
- `New(g Getter) *Resolver` — the forge-backed, cached `Source`. `githubclient.Client.DefaultBranches()` builds one per Client and returns the same pointer every call; fishhawkd hands that instance to `server.Config.DefaultBranch` and `orchestrator.Orchestrator.DefaultBranch`, and `githubclient.ListRulesetRequiredChecks` reads through it, so all of them share one cache.
- `Static` — a fixed-branch `Source` for fixtures (the migration target of the retired `Orchestrator.DefaultRef: "main"`); an empty `Static` refuses `resolver_unconfigured`. Production never wires it.
- `UnresolvedError{Repo, Reason, Err}` — the typed refusal; `Error()` names repo, reason and cause, `Unwrap()` exposes the cause (a forge sentinel, a context error). `ReasonOf(err)` reads the reason anywhere in a wrapped chain; `Unresolved(repo, reason, cause)` builds one.

## Reasons (closed set)

| Reason | Decided | Meaning |
|---|---|---|
| `resolver_unconfigured` | locally | nil `Source`, nil `Getter`, or an empty `Static` |
| `forge_unsupported` | by the caller | the run's forge has no read on this path (server: a GitLab run) |
| `no_installation` | locally | zero credential scope |
| `repository_invalid` | locally | empty owner or name (callers also map an unparseable `owner/name` here) |
| `repository_not_visible` | forge read | `errors.Is(err, forge.ErrNotFound)` |
| `forbidden` | forge read | `errors.Is(err, forge.ErrForbidden)` |
| `default_branch_missing` | forge read | nil repository or an empty / whitespace `DefaultBranch` |
| `lookup_failed` | forge read | any other error (transport, timeout, 5xx, decode) |

Through the real `githubclient`, a response missing `default_branch` is a plain decode-side error, so it reports `lookup_failed`; `default_branch_missing` arises from a `Getter` that returns an empty value.

## Cache

- Key: `scope.Ref()` + lower-cased `owner/name`, behind one mutex.
- A success is reused for 10 minutes (a default-branch rename is observed within that window); a forge-read failure for 30 seconds (a recovered forge is observed within that window).
- Locally decided refusals never read the forge and are never cached.
- A failure observed while the CALLER's ctx is already done is never cached, so a cancelled request cannot poison the next caller. The per-lookup timeout (10 s, layered over the caller's ctx) the resolver imposes itself, with the caller still live, IS negative-cached.
- Bounded at 1024 keys: expired entries are swept first, and the map is cleared if still full.
- No singleflight (`golang.org/x/sync` is only an indirect dependency): concurrent cold misses for one key may each read the forge once.
- TTLs, timeout and clock are unexported fields with defaults; the package tests drive expiry with a fake clock.

## Consumers (#4660)

Slice A: the orchestrator's consolidated PR, fan-in base and consolidated-review trigger (`orchestrator/README.md` § "Base-branch resolution"); the server's first-round review re-run (`server/README.md` § "Default-branch resolution"); `githubclient.ListRulesetRequiredChecks`. Each refuses on its own existing failure surface with no new audit category. Not covered: the four MCP spawn verbs (#4758), the webhook dispatchers' `DefaultRef`, and `runnerbackend.GitHubActions`' `workflow_dispatch` ref, which keeps its own `main` default.

## Tests

`defaultbranch_test.go`: one test per reason and per cache rule (positive/negative hit and expiry, caller-cancel exclusion, own-timeout negative caching, key separation and case folding, the bound, concurrent `-race` resolve), `Static`, and the error type.
