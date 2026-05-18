-- 0024: tag each run with the execution backend that runs it
-- (ADR-022 / #388).
--
-- Today's runner is a published GitHub Action (fishhawk/runner@v1).
-- The roadmap calls for pluggable backends — operator-local runners
-- (Phase C of E22 / #389) and eventually K8s. Without this column
-- the audit log can't distinguish runs across backends; compliance
-- consumers reading the chain would see "local" and "github_actions"
-- traces interleaved with no way to tell them apart.
--
-- Authority model (ADR-022 §"Authority"): the backend assigns
-- runner_kind at run-create time based on the dispatch path. The
-- runner never self-declares — a falsifiable claim from the runner
-- defeats the audit-integrity story. The dispatcher (which handles
-- GHA workflow_dispatch today) stamps 'github_actions'; the local-
-- runner CLI (Phase C) stamps 'local'.
--
-- Default 'github_actions' covers all legacy rows: every run created
-- before this migration came from the GHA dispatch path. The CHECK
-- constraint enumerates the closed v0 set; future kinds extend the
-- enum via a follow-up migration.
--
-- Verifier impact (verifier/internal/audit/): provenance is metadata
-- atop the tamper-evidence chain, not part of the hash inputs. The
-- verifier doesn't read runner_kind in v0 — adding the column
-- doesn't change the rehash invariant.
--
-- No index: read by PK (id) on the same row already fetched by every
-- run-detail surface.

ALTER TABLE runs
    ADD COLUMN runner_kind TEXT NOT NULL DEFAULT 'github_actions'
    CHECK (runner_kind IN ('github_actions', 'local'));
