-- 0110 (down): drop refinement_drafts.repo (ADR-114 / #4642, E89.25 / #4626).
--
-- Draft rows survive; only the open-time repo pins are lost, so those
-- sessions preview with the shipped default until filed. Rollback order:
-- revert the fishhawkd binary together with this migration — a binary that
-- selects or writes the column fails every refinement draft read and write
-- once it is gone, and a pre-0110 binary's SELECT * / RETURNING * scan fails
-- while it is still present.
ALTER TABLE refinement_drafts DROP COLUMN repo;
