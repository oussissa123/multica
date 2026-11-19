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

-- name: PullForwardRetriesAwaitingFix :many
-- A run that just succeeded on a runtime proves whatever a person had to fix
-- there (sign-in, configuration, the CLI install, credit) is fixed: retries
-- waiting out a long recheck for one of those reasons run now instead.
-- Served by the partial deferred index on (runtime_id, fire_at).
UPDATE agent_task_queue t
SET fire_at = now()
FROM agent_task_queue p
WHERE t.runtime_id = @runtime_id
  AND t.status = 'deferred'
  AND t.fire_at > now()
  AND p.id = t.parent_task_id
  AND p.failure_reason = ANY(@reasons::text[])
RETURNING t.id;
