// @vitest-environment jsdom

import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { AgentRuntime } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const liftRuntimeThrottle = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({
  api: { liftRuntimeThrottle },
}));
vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));
vi.mock("../../common/use-viewing-timezone", () => ({
  useViewingTimezone: () => "UTC",
}));

import { RuntimeThrottleNotice } from "./runtime-throttle-notice";

const RUNTIME: AgentRuntime = {
  id: "r-1",
  workspace_id: "ws-1",
  daemon_id: null,
  name: "Claude Code",
  runtime_mode: "local",
  provider: "claude",
  launch_header: "",
  status: "online",
  device_info: "",
  metadata: {},
  owner_id: null,
  visibility: "private",
  last_seen_at: null,
  created_at: "2026-05-01T00:00:00Z",
  updated_at: "2026-05-01T00:00:00Z",
};

function Wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={client}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RuntimeThrottleNotice", () => {
  it("renders nothing for a runtime that takes work", () => {
    const { container } = render(<RuntimeThrottleNotice runtime={RUNTIME} canEdit />, { wrapper: Wrapper });
    expect(container).toBeEmptyDOMElement();
  });

  it("says until when the runtime is paused and lets an editor resume it", async () => {
    liftRuntimeThrottle.mockResolvedValue(RUNTIME);
    render(
      <RuntimeThrottleNotice
        runtime={{ ...RUNTIME, throttled_until: "2026-11-18T14:02:00Z", throttle_reason: "agent_error.provider_usage_window" }}
        canEdit
      />,
      { wrapper: Wrapper },
    );
    expect(screen.getByText(/^Paused until Nov 18, 0?2:02/)).toBeInTheDocument();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    });
    expect(liftRuntimeThrottle).toHaveBeenCalledWith("r-1");
  });

  it("hides the resume action from members who cannot edit the runtime", () => {
    render(
      <RuntimeThrottleNotice runtime={{ ...RUNTIME, throttled_until: "2026-11-18T14:02:00Z" }} canEdit={false} />,
      { wrapper: Wrapper },
    );
    expect(screen.getByText(/^Paused until/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resume now" })).not.toBeInTheDocument();
  });
});
