# credstore

Shared credential-store module (#2389 / ADR-076) — persists minted Fishhawk bearer tokens on the local filesystem so both the `fishhawk` CLI and `fishhawk-mcp` can authenticate without re-passing a secret on every call. Promoted out of `cli/internal/credstore` so the backend-module `fishhawk-mcp` binary can import it without inverting the module hierarchy (a backend → cli dependency).

## File location

A single JSON file under the XDG config directory: `$XDG_CONFIG_HOME/fishhawk/credentials`, falling back to `~/.config/fishhawk/credentials`. `Path()` returns the resolved absolute path.

## Permission posture

The file holds live bearer secrets, so it is written `0600` (owner read/write only) and its containing directory `0700`. A wider mode would let a co-tenant read a live token.

## Key normalization

Each credential is keyed by its backend URL, canonicalized by `normalizeURL` (trailing-slash and surrounding-whitespace trimmed) so `http://localhost:8080` and `http://localhost:8080/` address the same record. An operator can hold tokens for several backends at once; a consumer picks the one matching its resolved `--backend-url` / `FISHHAWK_BACKEND_URL`.

## Atomic write

`Store` writes to a temp file in the same directory and `rename`s it into place, so a crash mid-write never truncates an existing credential set. `Store` merges into any existing store — re-storing one backend leaves the others untouched.

## API

- `Load(backendURL) (Credential, error)` — the credential for a backend, or `ErrNotFound` if none is stored. A present-but-corrupt store surfaces the wrapped parse error (distinct from `ErrNotFound`) so a corrupt store is loud rather than silently read as "no credential".
- `List() (map[string]Credential, error)` — every stored credential keyed by normalized backend URL. A missing file is an empty map, not an error.
- `Store(backendURL, Credential) error` — persist a credential (atomic, merging).
- `Path() (string, error)` — the credentials file path.

`Credential.ExpiresAt` is `*time.Time`; a nil value means non-expiring (v0 tokens do not expire).

## Consumers

- **CLI** — `cli/cmd/fishhawk/token.go` (`token login` writes, `token list` reads) and `cli/cmd/fishhawk/run.go` (the `newClient` fallback when no `--token`/env is set).
- **fishhawk-mcp** — `backend/cmd/fishhawk-mcp/main.go`'s startup token-resolution ladder: `FISHHAWK_API_TOKEN` wins when set; when empty, the credential keyed by the resolved backend URL is loaded; a not-usable credential fails startup rather than degrading to an empty bearer.
