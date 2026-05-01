DROP TRIGGER IF EXISTS audit_entries_no_delete ON audit_entries;
DROP TRIGGER IF EXISTS audit_entries_no_update ON audit_entries;
DROP FUNCTION IF EXISTS fishhawk_audit_no_mutation();
DROP TABLE   IF EXISTS audit_entries;
DROP TABLE   IF EXISTS artifacts;
