-- Artifact queries consumed by the postgres adapter for the
-- artifact.Repository interface (E2.1 / #22).

-- name: CreateArtifact :one
INSERT INTO artifacts (id, stage_id, kind, schema_version, content, content_hash)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetArtifact :one
SELECT * FROM artifacts WHERE id = $1;

-- name: ListArtifactsForStage :many
SELECT * FROM artifacts
WHERE stage_id = $1
ORDER BY created_at ASC;

-- name: GetArtifactByHash :one
-- Look up an existing artifact by content hash within a stage. Used
-- when re-uploading the same plan during retry — the dedup avoids
-- a second copy of identical bytes in the audit log.
SELECT * FROM artifacts
WHERE stage_id = $1 AND content_hash = $2;
