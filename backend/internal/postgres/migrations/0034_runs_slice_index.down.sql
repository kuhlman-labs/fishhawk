-- Roll back 0034: drop the additive nullable slice_index column. No
-- backfill or data migration is required — reverting restores the
-- shared-branch routing exactly (the prompt field is omitempty so
-- older runners ignore it).
ALTER TABLE runs
    DROP COLUMN slice_index;
