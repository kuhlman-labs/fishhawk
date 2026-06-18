-- 0033: review_concerns.suggested_patch — the reviewer-emitted unified
-- diff that mechanically resolves a concern (E22.X / #1165). NOT NULL
-- DEFAULT '' so pre-existing rows read back an empty patch and the
-- column is additive: a concern without a suggested patch (the common
-- case, and every row written before this migration) carries ''.
ALTER TABLE review_concerns
    ADD COLUMN suggested_patch TEXT NOT NULL DEFAULT '';
