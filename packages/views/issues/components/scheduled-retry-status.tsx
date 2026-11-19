import type { AgentTask } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { formatInTimeZone } from "../../common/format-in-time-zone";

// Status of an automatic retry waiting for its scheduled time. A backend that
// does not report fire_at still reads as scheduled, just without the time.
export function ScheduledRetryStatus({ task, className }: { task: AgentTask; className?: string }) {
  const { t, i18n } = useT("issues");
  const tz = useViewingTimezone();
  const text = task.fire_at
    ? t(($) => $.execution_log.retry_at, {
        time: formatInTimeZone(task.fire_at, tz, i18n.resolvedLanguage ?? i18n.language),
      })
    : t(($) => $.execution_log.status_deferred);
  return <span className={cn("min-w-0 truncate", className)}>{text}</span>;
}
