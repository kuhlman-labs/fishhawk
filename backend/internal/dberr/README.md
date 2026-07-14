# backend/internal/dberr

DB-unavailable auth classification — 503 vs 401 (#764).

## IsUnavailable

`IsUnavailable(err)` classifies a database-UNAVAILABLE condition (connection refused → `*pgconn.ConnectError`, closed pool → puddle/v2 `ErrClosedPool`, `net.OpError` fallback) apart from an ordinary query outcome (no rows / `ErrNotFound`).

The auth middleware `Server.bearerAuth` (`backend/internal/server/middleware.go`) captures the error from each authenticator (apitoken / mcptoken / session) and, when `dberr.IsUnavailable` is true for the credential presented, short-circuits with `503 service_unavailable` (`database unavailable; retry shortly`) instead of masking the outage as a fall-through 401.

A genuinely bad credential on a HEALTHY DB (`ErrNotFound`) is NOT unavailable, so it still falls through to the anonymous identity and the per-handler 401 is unchanged — this is an error-CLASSIFICATION change, not an authorization tightening (no scope/role/audience check added).
