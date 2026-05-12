-- 0020 down: drop the retry-chain depth column.

ALTER TABLE runs
    DROP COLUMN IF EXISTS retry_attempt;
