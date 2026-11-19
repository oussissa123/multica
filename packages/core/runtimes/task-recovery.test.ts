// @vitest-environment node
import { describe, expect, it } from "vitest";
import { DEFAULT_TASK_RECOVERY, deriveTaskRecoverySettings } from "./task-recovery";

describe("deriveTaskRecoverySettings", () => {
  it("defaults a workspace that set nothing", () => {
    expect(deriveTaskRecoverySettings({ settings: {} })).toEqual(DEFAULT_TASK_RECOVERY);
    expect(deriveTaskRecoverySettings(null)).toEqual(DEFAULT_TASK_RECOVERY);
  });

  it("reads stored values", () => {
    expect(
      deriveTaskRecoverySettings({
        settings: {
          task_recovery: {
            enabled: false,
            pause_runtime: false,
            usage_window_retries: 1,
            rate_limit_retries: 0,
            server_error_retries: 10,
            max_wait_hours: 24,
          },
        },
      }),
    ).toEqual({
      enabled: false,
      pause_runtime: false,
      usage_window_retries: 1,
      rate_limit_retries: 0,
      server_error_retries: 10,
      max_wait_hours: 24,
    });
  });

  it("keeps defaults for values the server would ignore", () => {
    expect(
      deriveTaskRecoverySettings({
        settings: { task_recovery: { enabled: "no", rate_limit_retries: 11, server_error_retries: 1.5, max_wait_hours: 0 } },
      }),
    ).toEqual(DEFAULT_TASK_RECOVERY);
    expect(deriveTaskRecoverySettings({ settings: { task_recovery: [1, 2] } })).toEqual(DEFAULT_TASK_RECOVERY);
  });
});
