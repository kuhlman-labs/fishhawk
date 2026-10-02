-- 0092: review_concerns persona attribution + ingest markers (E55.10 / #3755,
-- ADR-084 D3 / rule 3).
--
-- Three columns, each recording what the server did to a review concern at
-- verdict ingest. The plan_reviewed / implement_reviewed audit payload stays
-- the authoritative record; these columns let the operator surfaces that read
-- the concern store (gate view, run status, fix-up routing) show the same
-- facts without re-parsing the payload.
--
--   reviewer_role          WHICH reviewer raised the concern: a reviewer
--                          persona's name, or 'standard' for the stage's
--                          standard reviewer. '' = UNATTRIBUTED — every row
--                          minted before this migration, and any concern
--                          written by a path that names no role.
--   quote_unverified       true when the reviewer quoted a document passage
--                          (quoted_passage + document_ref) that the server
--                          could NOT find in the text it injected into that
--                          invocation; the concern was demoted to low.
--   severity_clamped_from  the reviewer's original severity when ingest
--                          lowered it (persona severity_cap clamp or an
--                          unverified quote); '' when untouched.
--
-- NOT NULL DEFAULT '' / false, the additive shape 0033 and 0069 used, so every
-- existing row reads back the empty/false value.
--
-- NO BACKFILL of reviewer_role. The role of a historical row is unknowable
-- after the fact: a persona row minted since E55.8 must not be mislabelled
-- 'standard', so legacy rows stay '' (unattributed). Identity comparisons
-- normalize '' to 'standard' in Go (concern.NormalizedReviewerRole), never in
-- the stored value.
--
-- Plain single-apply DDL (no IF NOT EXISTS), matching 0089-0091: the
-- migration runner applies each version exactly once. Postgres APPENDS added
-- columns, so the sqlc column order is ... settled_ref, reviewer_role,
-- quote_unverified, severity_clamped_from.
ALTER TABLE review_concerns
    ADD COLUMN reviewer_role TEXT NOT NULL DEFAULT '',
    ADD COLUMN quote_unverified BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN severity_clamped_from TEXT NOT NULL DEFAULT '';
