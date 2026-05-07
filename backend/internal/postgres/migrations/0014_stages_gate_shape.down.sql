ALTER TABLE stages
    DROP CONSTRAINT IF EXISTS stages_gate_type_check,
    DROP COLUMN IF EXISTS gate_approvers,
    DROP COLUMN IF EXISTS gate_blocking_checks,
    DROP COLUMN IF EXISTS gate_type;
