-- 0001: account_regions — the directory's entire persistent state
-- (E44.7 / #1831, ADR-062).
--
-- The directory plane knows exactly one thing: which region owns an
-- account. Region -> cell_base_url is NOT stored here; it resolves from
-- the directory's env config, so a cell can be re-homed without a
-- migration and a stale row can never point traffic at a dead cell.
--
-- (provider, account_key) is the primary key, which is what makes the
-- assignment atomic: the first INSERT wins and every concurrent caller
-- reads the winner back via ON CONFLICT ... RETURNING rather than
-- overwriting it. First-write-wins is enforced by the constraint, not by
-- a check-then-act in Go.

CREATE TABLE account_regions (
    provider     TEXT         NOT NULL,
    account_key  TEXT         NOT NULL,
    home_region  TEXT         NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, account_key)
);
