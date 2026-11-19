ALTER TABLE agent_runtime
    DROP COLUMN IF EXISTS throttled_at,
    DROP COLUMN IF EXISTS throttle_reason,
    DROP COLUMN IF EXISTS throttled_until;
