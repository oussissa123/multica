import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import { DEFAULT_TASK_RECOVERY } from "@multica/core/runtimes/task-recovery";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockUpdateWorkspace = vi.hoisted(() => vi.fn());
const workspaceRef = vi.hoisted(() => ({
  current: { id: "workspace-1", settings: {} as Record<string, unknown> },
}));

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ setQueryData: vi.fn() }),
}));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => workspaceRef.current }));
vi.mock("@multica/core/api", () => ({ api: { updateWorkspace: mockUpdateWorkspace } }));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

import { TaskRecoverySection } from "./task-recovery-section";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };
const copy = enSettings.task_recovery;

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  workspaceRef.current = { id: "workspace-1", settings: { github_enabled: true } };
  mockUpdateWorkspace.mockResolvedValue(workspaceRef.current);
});

describe("TaskRecoverySection", () => {
  it("shows the default policy for a workspace that set nothing", () => {
    render(<TaskRecoverySection canManage />, { wrapper: Wrapper });
    expect(screen.getByRole("switch", { name: copy.enabled_label })).toBeChecked();
    expect(screen.getByRole("switch", { name: copy.pause_runtime_label })).toBeChecked();
    expect(screen.getByRole("combobox", { name: copy.usage_window_label })).toHaveTextContent("3 retries");
    expect(screen.getByRole("combobox", { name: copy.rate_limit_label })).toHaveTextContent("5 retries");
    expect(screen.getByRole("combobox", { name: copy.max_wait_label })).toHaveTextContent("7 days");
  });

  it("saves a change with the rest of the policy and the other workspace settings", async () => {
    const user = userEvent.setup();
    render(<TaskRecoverySection canManage />, { wrapper: Wrapper });

    await user.click(screen.getByRole("combobox", { name: copy.rate_limit_label }));
    await user.click(await screen.findByRole("option", { name: copy.retries_none }));

    await waitFor(() =>
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        settings: { github_enabled: true, task_recovery: { ...DEFAULT_TASK_RECOVERY, rate_limit_retries: 0 } },
      }),
    );
  });

  it("locks the details while retries are off", () => {
    workspaceRef.current.settings = { task_recovery: { enabled: false } };
    render(<TaskRecoverySection canManage />, { wrapper: Wrapper });
    expect(screen.getByRole("switch", { name: copy.enabled_label })).not.toBeChecked();
    expect(screen.getByRole("switch", { name: copy.enabled_label })).not.toHaveAttribute("aria-disabled", "true");
    expect(screen.getByRole("combobox", { name: copy.usage_window_label })).toBeDisabled();
  });

  it("is read-only for members who cannot manage the workspace", () => {
    render(<TaskRecoverySection canManage={false} />, { wrapper: Wrapper });
    expect(screen.getByRole("switch", { name: copy.enabled_label })).toHaveAttribute("aria-disabled", "true");
    expect(screen.getByRole("combobox", { name: copy.server_error_label })).toBeDisabled();
  });
});
