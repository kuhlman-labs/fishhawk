-- 0006: persist the gate's SLA string on the stage row (E3.11 / #123).
--
-- The dispatcher reads the gate's `sla` field from the parsed
-- workflow spec at stage-create time and stores it here. The SLA
-- ticker then scans for awaiting_approval stages whose
-- updated_at + parsed(sla) < now() and transitions them to
-- failed-D.
--
-- Storing the raw string (not a parsed duration) keeps the column
-- forward-compatible: the v0 parser treats "4_business_hours" as
-- 4 wall-clock hours; v0.x can swap the parser for true business-
-- hours math without a schema change.
--
-- Nullable: gates may have no SLA (rare; the schema doesn't
-- require it), and rows that predate this migration legitimately
-- have no value to backfill.

ALTER TABLE stages
    ADD COLUMN gate_sla TEXT;
