-- 0052 (down): drop the tenancy tables (children first — installations holds
-- the composite FK into accounts). The tenancy foundation (ADR-057 / ADR-058,
-- #1823/#1851/#1854) is purely additive — it creates two new tables with no
-- readers or writers and makes no change to any existing table — so the
-- rollback is a clean two-table drop with no data migration. The shared
-- fishhawk_set_updated_at() function is left in place (it predates this
-- migration, defined in 0001).

DROP TABLE installations;
DROP TABLE accounts;
