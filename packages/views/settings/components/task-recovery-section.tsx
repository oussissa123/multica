"use client";

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useCurrentWorkspace } from "@multica/core/paths";
import {
  deriveTaskRecoverySettings,
  MAX_RECOVERY_RETRIES,
  TASK_RECOVERY_SETTING_KEY,
  type TaskRecoverySettings,
} from "@multica/core/runtimes/task-recovery";
import type { Workspace } from "@multica/core/types";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { Switch } from "@multica/ui/components/ui/switch";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection } from "./settings-layout";

type RetryField = "usage_window_retries" | "rate_limit_retries" | "server_error_retries";

const RETRY_COUNTS = Array.from({ length: MAX_RECOVERY_RETRIES + 1 }, (_, n) => n);
const WAIT_HOURS = [1, 6, 24, 72, 168];

/**
 * How runs stopped by a provider limit are retried in this workspace. Members
 * see the policy; owners and admins change it. Each change saves at once.
 */
export function TaskRecoverySection({ canManage }: { canManage: boolean }) {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const qc = useQueryClient();
  const [saving, setSaving] = useState(false);
  const policy = deriveTaskRecoverySettings(workspace);

  async function persist(patch: Partial<TaskRecoverySettings>) {
    if (!workspace || saving) return;
    setSaving(true);
    try {
      const settings = (workspace.settings as Record<string, unknown>) ?? {};
      const updated = await api.updateWorkspace(workspace.id, {
        settings: { ...settings, [TASK_RECOVERY_SETTING_KEY]: { ...policy, ...patch } },
      });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.auto_save.failed));
    } finally {
      setSaving(false);
    }
  }

  const retryLabel = (n: number) =>
    n === 0 ? t(($) => $.task_recovery.retries_none) : t(($) => $.task_recovery.retries_count, { count: n });
  const waitLabel = (hours: number) =>
    hours % 24 === 0
      ? t(($) => $.task_recovery.wait_days, { count: hours / 24 })
      : t(($) => $.task_recovery.wait_hours, { count: hours });
  const waitOptions = WAIT_HOURS.includes(policy.max_wait_hours)
    ? WAIT_HOURS
    : [...WAIT_HOURS, policy.max_wait_hours].sort((a, b) => a - b);
  const locked = !canManage || saving;
  const detailLocked = locked || !policy.enabled;

  const retryRow = (field: RetryField, label: string) => (
    <SettingsRow anchor={`task-recovery-${field}`} label={label} size="select">
      <Select
        items={RETRY_COUNTS.map((n) => ({ value: String(n), label: retryLabel(n) }))}
        value={String(policy[field])}
        onValueChange={(next) => next !== null && void persist({ [field]: Number(next) })}
        disabled={detailLocked}
      >
        <SelectTrigger size="sm" className="w-full" aria-label={label}>
          <SelectValue>{() => retryLabel(policy[field])}</SelectValue>
        </SelectTrigger>
        <SelectContent align="end">
          {RETRY_COUNTS.map((n) => (
            <SelectItem key={n} value={String(n)}>
              {retryLabel(n)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </SettingsRow>
  );

  return (
    <SettingsSection anchor="task-recovery" title={t(($) => $.task_recovery.title)}>
      <SettingsCard>
        <SettingsRow
          anchor="task-recovery-enabled"
          label={t(($) => $.task_recovery.enabled_label)}
          description={t(($) => $.task_recovery.enabled_description)}
        >
          <Switch
            checked={policy.enabled}
            disabled={locked}
            onCheckedChange={(v) => void persist({ enabled: v })}
            aria-label={t(($) => $.task_recovery.enabled_label)}
          />
        </SettingsRow>
        <SettingsRow
          anchor="task-recovery-pause-runtime"
          label={t(($) => $.task_recovery.pause_runtime_label)}
          description={t(($) => $.task_recovery.pause_runtime_description)}
        >
          <Switch
            checked={policy.pause_runtime}
            disabled={detailLocked}
            onCheckedChange={(v) => void persist({ pause_runtime: v })}
            aria-label={t(($) => $.task_recovery.pause_runtime_label)}
          />
        </SettingsRow>
        {retryRow("usage_window_retries", t(($) => $.task_recovery.usage_window_label))}
        {retryRow("rate_limit_retries", t(($) => $.task_recovery.rate_limit_label))}
        {retryRow("server_error_retries", t(($) => $.task_recovery.server_error_label))}
        <SettingsRow
          anchor="task-recovery-max-wait"
          label={t(($) => $.task_recovery.max_wait_label)}
          description={t(($) => $.task_recovery.max_wait_description)}
          size="select"
        >
          <Select
            items={waitOptions.map((h) => ({ value: String(h), label: waitLabel(h) }))}
            value={String(policy.max_wait_hours)}
            onValueChange={(next) => next !== null && void persist({ max_wait_hours: Number(next) })}
            disabled={detailLocked}
          >
            <SelectTrigger size="sm" className="w-full" aria-label={t(($) => $.task_recovery.max_wait_label)}>
              <SelectValue>{() => waitLabel(policy.max_wait_hours)}</SelectValue>
            </SelectTrigger>
            <SelectContent align="end">
              {waitOptions.map((h) => (
                <SelectItem key={h} value={String(h)}>
                  {waitLabel(h)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </SettingsRow>
      </SettingsCard>
    </SettingsSection>
  );
}
