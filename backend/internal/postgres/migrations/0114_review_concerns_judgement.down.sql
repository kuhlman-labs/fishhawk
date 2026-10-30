-- Down-migration for 0114: drop the seven review_concerns judgement columns.
-- Every row and every other column survive, including the outcome column
-- (0112) and any 'fixed' outcome a system writer recorded in it. Rollback
-- order: revert the fishhawkd binary FIRST, because a binary that selects the
-- judgement columns fails every concern read once they are gone.
ALTER TABLE review_concerns
    DROP COLUMN judgement,
    DROP COLUMN judgement_actor_kind,
    DROP COLUMN judgement_delegated,
    DROP COLUMN judgement_bulk,
    DROP COLUMN judgement_backfill,
    DROP COLUMN tradeoff_cost,
    DROP COLUMN duplicate_of;
