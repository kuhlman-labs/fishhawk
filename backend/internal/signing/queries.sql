-- Signing-key queries consumed by the postgres adapter for the
-- signing.Repository interface (E2.3 / #24). Append-only — no Update
-- or Delete queries; the schema's triggers backstop that.

-- name: IssueSigningKey :one
INSERT INTO signing_keys (run_id, public_key, issued_at, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSigningKey :one
SELECT * FROM signing_keys WHERE run_id = $1;
