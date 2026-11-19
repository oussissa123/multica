import type { Workspace } from "../types";

/**
 * A workspace's retry policy for runs stopped by a provider limit (rate limit,
 * provider server error, used-up subscription window). Stored under
 * `settings.task_recovery`; the server reads the same fields with the same
 * defaults and bounds (server/internal/service/task_recovery_policy.go).
 */
export interface TaskRecoverySettings {
  enabled: boolean;
  /** Hold a runtime's queue while its usage window is used up. */
  pause_runtime: boolean;
  usage_window_retries: number;
  rate_limit_retries: number;
  server_error_retries: number;
  /** A retry the provider would make wait longer is not made. */
  max_wait_hours: number;
}

export const TASK_RECOVERY_SETTING_KEY = "task_recovery";
export const MAX_RECOVERY_RETRIES = 10;
export const MAX_RECOVERY_WAIT_HOURS = 7 * 24;

export const DEFAULT_TASK_RECOVERY: TaskRecoverySettings = {
  enabled: true,
  pause_runtime: true,
  usage_window_retries: 3,
  rate_limit_retries: 5,
  server_error_retries: 2,
  max_wait_hours: MAX_RECOVERY_WAIT_HOURS,
};

function wholeInRange(value: unknown, min: number, max: number): value is number {
  return typeof value === "number" && Number.isInteger(value) && value >= min && value <= max;
}

/** Missing or invalid fields fall back to their defaults, as on the server. */
export function deriveTaskRecoverySettings(
  workspace: Pick<Workspace, "settings"> | null | undefined,
): TaskRecoverySettings {
  const settings = (workspace?.settings ?? {}) as Record<string, unknown>;
  const raw = settings[TASK_RECOVERY_SETTING_KEY];
  const stored = raw && typeof raw === "object" && !Array.isArray(raw) ? (raw as Record<string, unknown>) : {};
  const flag = (key: "enabled" | "pause_runtime") =>
    typeof stored[key] === "boolean" ? (stored[key] as boolean) : DEFAULT_TASK_RECOVERY[key];
  const count = (key: "usage_window_retries" | "rate_limit_retries" | "server_error_retries") =>
    wholeInRange(stored[key], 0, MAX_RECOVERY_RETRIES) ? stored[key] : DEFAULT_TASK_RECOVERY[key];
  return {
    enabled: flag("enabled"),
    pause_runtime: flag("pause_runtime"),
    usage_window_retries: count("usage_window_retries"),
    rate_limit_retries: count("rate_limit_retries"),
    server_error_retries: count("server_error_retries"),
    max_wait_hours: wholeInRange(stored.max_wait_hours, 1, MAX_RECOVERY_WAIT_HOURS)
      ? stored.max_wait_hours
      : DEFAULT_TASK_RECOVERY.max_wait_hours,
  };
}
