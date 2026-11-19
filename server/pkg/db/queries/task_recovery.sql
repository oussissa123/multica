-- name: RescheduleDeferredTask :one
-- Moves a deferred task's fire time. Only a still-deferred row moves: once
-- promoted it belongs to the claim path.
UPDATE agent_task_queue
SET fire_at = @fire_at::timestamptz
WHERE id = @id AND status = 'deferred'
RETURNING *;

-- name: LiftRuntimeThrottle :execrows
-- A person lifting a runtime's usage-window throttle by hand.
UPDATE agent_runtime
SET throttled_until = NULL,
    throttle_reason = NULL,
    throttled_at = NULL
WHERE id = @id
  AND throttled_until > now();
