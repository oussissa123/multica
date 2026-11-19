-- name: UpsertRuntimeRateLimit :exec
-- One snapshot per runtime (MUL-9427). Concurrent runs on the same runtime
-- each report their own view; the newest observation wins, so a late report
-- from an older run cannot roll the snapshot back.
INSERT INTO runtime_rate_limit (runtime_id, workspace_id, snapshot, observed_at)
VALUES (@runtime_id, @workspace_id, @snapshot, @observed_at)
ON CONFLICT (runtime_id) DO UPDATE
SET snapshot = EXCLUDED.snapshot,
    workspace_id = EXCLUDED.workspace_id,
    observed_at = EXCLUDED.observed_at,
    updated_at = now()
WHERE runtime_rate_limit.observed_at <= EXCLUDED.observed_at;

-- name: GetRuntimeRateLimit :one
SELECT runtime_id, workspace_id, snapshot, observed_at, updated_at
FROM runtime_rate_limit
WHERE runtime_id = $1;

-- name: ThrottleRuntime :exec
-- Holds back claims on a runtime until its usage window resets. Concurrent
-- failures keep the latest reset (GREATEST ignores the NULL of an unthrottled
-- runtime).
UPDATE agent_runtime
SET throttled_until = GREATEST(throttled_until, @throttled_until::timestamptz),
    throttle_reason = @throttle_reason::text,
    throttled_at = now()
WHERE id = @id;

-- name: LiftRuntimeThrottleObservedAfter :execrows
-- Lifts a throttle early when the CLI reports a snapshot with room left. The
-- snapshot must be observed after the throttle was set: an older report
-- predates the exhaustion and says nothing about the account now.
UPDATE agent_runtime
SET throttled_until = NULL,
    throttle_reason = NULL,
    throttled_at = NULL
WHERE id = @id
  AND throttled_until > now()
  AND throttled_at < @observed_at::timestamptz;
