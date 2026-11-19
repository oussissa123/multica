-- A runtime whose agent CLI account used up a subscription usage window is
-- throttled until the window resets: claims and deferred promotion skip it so
-- queued work waits instead of failing on the same limit one task at a time.
-- throttled_at orders the throttle against rate-limit snapshots, so only a
-- snapshot observed after it can lift the throttle early.
ALTER TABLE agent_runtime
    ADD COLUMN IF NOT EXISTS throttled_until TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS throttle_reason TEXT,
    ADD COLUMN IF NOT EXISTS throttled_at TIMESTAMPTZ;
