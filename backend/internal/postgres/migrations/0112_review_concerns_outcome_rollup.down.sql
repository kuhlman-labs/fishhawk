-- Down-migration for 0112: drop the rollup idempotence index and the two
-- review_concerns columns. Every row and every other column survive. Parent
-- copies already imported stay behind as ordinary raised parent concerns
-- without their rolled_up_from link; the concern_rolled_up audit rows keep the
-- child -> parent mapping. Rollback order: revert the fishhawkd binary FIRST,
-- because a binary that selects outcome / rolled_up_from fails every concern
-- read once they are gone.
DROP INDEX review_concerns_rolled_up_once_idx;

ALTER TABLE review_concerns
    DROP COLUMN outcome,
    DROP COLUMN rolled_up_from;
