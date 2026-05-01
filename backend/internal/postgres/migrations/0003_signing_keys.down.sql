DROP TRIGGER IF EXISTS signing_keys_no_delete ON signing_keys;
DROP TRIGGER IF EXISTS signing_keys_no_update ON signing_keys;
DROP FUNCTION IF EXISTS fishhawk_signing_keys_no_mutation();
DROP TABLE   IF EXISTS signing_keys;
