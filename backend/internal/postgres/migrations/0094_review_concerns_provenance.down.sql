-- Down-migration for 0094: drop the two review_concerns provenance columns.
-- The table and every other column are untouched. A rollback LOSES the
-- server_check marker on rows a server check raised; the check's own origin
-- audit entry keeps the authoritative record of what was detected and where.
ALTER TABLE review_concerns
    DROP COLUMN provenance,
    DROP COLUMN check_key;
