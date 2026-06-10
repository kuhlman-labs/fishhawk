-- 0029 (down): drop the scope_amendments table. Mid-stage scope
-- amendment requests (E22.X / #961) are unavailable after this
-- migration; in-flight runs stop folding amendments at the next
-- prompt fetch and the original scope.files remains authoritative.

DROP TABLE scope_amendments;
