"use client";

import { toast } from "sonner";
import { CirclePause } from "@multica/ui/icons";
import { Button } from "@multica/ui/components/ui/button";
import {
  Alert,
  AlertAction,
  AlertDescription,
  AlertTitle,
} from "@multica/ui/components/ui/alert";
import type { AgentRuntime } from "@multica/core/types";
import { useLiftRuntimeThrottle } from "@multica/core/runtimes/mutations";
import { useWorkspaceId } from "@multica/core/hooks";
import { useT } from "../../i18n";
import { useViewingTimezone } from "../../common/use-viewing-timezone";
import { formatInTimeZone } from "../../common/format-in-time-zone";

// Shown while the runtime takes no work because its agent CLI account used up
// a usage window: its queued runs wait for the reset instead of failing.
export function RuntimeThrottleNotice({
  runtime,
  canEdit,
}: {
  runtime: AgentRuntime;
  canEdit: boolean;
}) {
  const { t, i18n } = useT("runtimes");
  const tz = useViewingTimezone();
  const wsId = useWorkspaceId();
  const lift = useLiftRuntimeThrottle(wsId);

  if (!runtime.throttled_until) return null;
  const until = formatInTimeZone(
    runtime.throttled_until,
    tz,
    i18n.resolvedLanguage ?? i18n.language,
  );

  const handleLift = () => {
    lift.mutateAsync(runtime.id).then(undefined, () => {
      toast.error(t(($) => $.throttle.lift_failed));
    });
  };

  return (
    <Alert>
      <CirclePause className="text-warning" />
      <AlertTitle>{t(($) => $.throttle.title, { time: until })}</AlertTitle>
      <AlertDescription>{t(($) => $.throttle.description)}</AlertDescription>
      {canEdit && (
        <AlertAction>
          <Button size="sm" variant="outline" onClick={handleLift} disabled={lift.isPending}>
            {t(($) => $.throttle.lift)}
          </Button>
        </AlertAction>
      )}
    </Alert>
  );
}
