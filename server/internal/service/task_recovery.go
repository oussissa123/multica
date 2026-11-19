package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Provider-limit recovery. A rate-limited or usage-window failure is not a
// problem with the work: the provider says the account will accept requests
// again, often when. These reasons are retried after that moment instead of
// ending the task, and a used-up usage window also throttles the runtime so
// the rest of its queue waits rather than failing on the same limit.

const (
	// usageWindowMaxAttempts: first run plus three reports, enough to ride
	// out a session window, a weekly window behind it, and one misread reset.
	usageWindowMaxAttempts = 4
	rateLimitMaxAttempts   = 6
	serverErrorMaxAttempts = 3

	// usageWindowResetMargin keeps a retry from landing on the reset second,
	// when the provider may not have rolled the window over yet.
	usageWindowResetMargin = 2 * time.Minute
	// usageWindowResumeJitter spreads the retries of tasks that hit the same
	// window so they do not all wake at once.
	usageWindowResumeJitter = time.Minute
	// usageWindowDefaultWait applies when neither the error nor the latest
	// rate-limit snapshot names a reset.
	usageWindowDefaultWait = time.Hour
	// recoveryMaxHorizon caps how far ahead a retry may be scheduled. A
	// weekly window reset is within it; anything later goes to a person.
	recoveryMaxHorizon = 7 * 24 * time.Hour
)

// rateLimitBackoff and serverErrorBackoff are indexed by the failed attempt
// number minus one; the last entry repeats.
var (
	rateLimitBackoff   = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
	serverErrorBackoff = []time.Duration{30 * time.Second, 2 * time.Minute}
)

var providerLimitAttemptCeilings = map[string]int32{
	string(taskfailure.ReasonAgentProviderUsageWindow):         usageWindowMaxAttempts,
	string(taskfailure.ReasonAgentProviderCapacityOrRateLimit): rateLimitMaxAttempts,
	string(taskfailure.ReasonAgentProviderServerError):         serverErrorMaxAttempts,
}

// providerLimitRecoveryEnabled is the operator kill switch
// (MULTICA_RECOVERY_ENABLED=false). Turning it off restores the earlier
// posture: provider limits end the task.
func providerLimitRecoveryEnabled() bool {
	v, err := strconv.ParseBool(os.Getenv("MULTICA_RECOVERY_ENABLED"))
	return err != nil || v
}

func isProviderLimitReason(reason string) bool {
	_, ok := providerLimitAttemptCeilings[reason]
	return ok
}

// retryPlan is when the next attempt of a failed task should run.
type retryPlan struct {
	// fireAt is zero for an immediate retry.
	fireAt time.Time
	// throttleUntil is non-zero when the runtime should take no work until
	// then (a used-up usage window is the account's, not the task's).
	throttleUntil time.Time
	// hintSource says where fireAt came from, for the log.
	hintSource string
}

// planRetry schedules the retry of a failed attempt. ok is false when the
// provider's reset lies beyond recoveryMaxHorizon: such a task is not retried.
func (s *TaskService) planRetry(ctx context.Context, parent db.AgentTaskQueue, reason, errMsg string, now time.Time) (retryPlan, bool) {
	switch reason {
	case string(taskfailure.ReasonAgentProviderUsageWindow):
		reset, source := usageWindowReset(errMsg, s.runtimeRateLimitSnapshot(ctx, parent.RuntimeID), now)
		if reset.Sub(now) > recoveryMaxHorizon {
			return retryPlan{}, false
		}
		throttle := reset.Add(usageWindowResetMargin)
		return retryPlan{
			fireAt:        throttle.Add(rand.N(usageWindowResumeJitter)),
			throttleUntil: throttle,
			hintSource:    source,
		}, true
	case string(taskfailure.ReasonAgentProviderCapacityOrRateLimit):
		wait := backoffFor(rateLimitBackoff, parent.Attempt)
		source := "backoff"
		if hint, ok := taskfailure.ParseRetryHint(errMsg, now, nil); ok && hint.At.Sub(now) > wait {
			wait, source = hint.At.Sub(now), hint.Source
		}
		if wait > recoveryMaxHorizon {
			return retryPlan{}, false
		}
		return retryPlan{fireAt: now.Add(jitter(wait, 0.2)), hintSource: source}, true
	case string(taskfailure.ReasonAgentProviderServerError):
		return retryPlan{fireAt: now.Add(jitter(backoffFor(serverErrorBackoff, parent.Attempt), 0.2)), hintSource: "backoff"}, true
	}
	if delay := retryDelayForAttempt(reason, parent.Attempt); delay > 0 {
		return retryPlan{fireAt: now.Add(delay)}, true
	}
	return retryPlan{}, true
}

// applyRetryThrottle throttles the parent's runtime for a usage-window retry.
// It runs after the failure commits: the runtime row is locked by claim paths
// in the opposite order to the task transaction, and the throttle is advisory
// — a missed one only costs the queue one more failure on the same limit.
func (s *TaskService) applyRetryThrottle(ctx context.Context, parent db.AgentTaskQueue, reason string, plan retryPlan) {
	if plan.throttleUntil.IsZero() || !parent.RuntimeID.Valid {
		return
	}
	if err := s.Queries.ThrottleRuntime(ctx, db.ThrottleRuntimeParams{
		ID:             parent.RuntimeID,
		ThrottledUntil: pgtype.Timestamptz{Time: plan.throttleUntil, Valid: true},
		ThrottleReason: reason,
	}); err != nil {
		slog.Warn("throttle runtime for usage window failed",
			"runtime_id", util.UUIDToString(parent.RuntimeID), "error", err)
		return
	}
	slog.Info("runtime throttled until usage window resets",
		"runtime_id", util.UUIDToString(parent.RuntimeID),
		"task_id", util.UUIDToString(parent.ID),
		"throttled_until", plan.throttleUntil,
		"hint_source", plan.hintSource,
	)
}

// LiftRuntimeThrottleForSnapshot lifts a runtime's throttle when a snapshot
// observed after the throttle shows every window with room left.
func (s *TaskService) LiftRuntimeThrottleForSnapshot(ctx context.Context, runtimeID pgtype.UUID, snapshot []byte, observedAt time.Time) {
	if _, exhausted := exhaustedWindowReset(snapshot, observedAt); exhausted {
		return
	}
	lifted, err := s.Queries.LiftRuntimeThrottleObservedAfter(ctx, db.LiftRuntimeThrottleObservedAfterParams{
		ID:         runtimeID,
		ObservedAt: pgtype.Timestamptz{Time: observedAt, Valid: true},
	})
	if err != nil {
		slog.Warn("lift runtime throttle failed", "runtime_id", util.UUIDToString(runtimeID), "error", err)
		return
	}
	if lifted > 0 {
		slog.Info("runtime throttle lifted: usage window has room again", "runtime_id", util.UUIDToString(runtimeID))
	}
}

func (s *TaskService) runtimeRateLimitSnapshot(ctx context.Context, runtimeID pgtype.UUID) []byte {
	if !runtimeID.Valid {
		return nil
	}
	row, err := s.Queries.GetRuntimeRateLimit(ctx, runtimeID)
	if err != nil {
		return nil
	}
	return row.Snapshot
}

// usageWindowReset picks when a used-up window resets: the provider's own
// words first, then the latest snapshot the CLI reported, then a default.
func usageWindowReset(errMsg string, snapshot []byte, now time.Time) (time.Time, string) {
	if hint, ok := taskfailure.ParseRetryHint(errMsg, now, nil); ok {
		return hint.At, hint.Source
	}
	if reset, ok := exhaustedWindowReset(snapshot, now); ok && reset.After(now) {
		return reset, "rate_limit_snapshot"
	}
	return now.Add(usageWindowDefaultWait), "default"
}

// storedRateLimitSnapshot is the part of runtime_rate_limit.snapshot (written
// by handler.ReportTaskRateLimits) that recovery reads.
type storedRateLimitSnapshot struct {
	Status  string `json:"status"`
	Windows []struct {
		UsedPercent *float64   `json:"used_percent"`
		ResetsAt    *time.Time `json:"resets_at"`
		Status      string     `json:"status"`
	} `json:"windows"`
}

// exhaustedWindowReset reports whether the snapshot shows a used-up window
// and, if so, the latest reset among the used-up windows still ahead of now:
// work resumes only when every exhausted window has reset.
func exhaustedWindowReset(snapshot []byte, now time.Time) (time.Time, bool) {
	if len(snapshot) == 0 {
		return time.Time{}, false
	}
	var snap storedRateLimitSnapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return time.Time{}, false
	}
	exhausted := snap.Status == "rejected"
	var latest time.Time
	for _, w := range snap.Windows {
		if w.Status != "rejected" && (w.UsedPercent == nil || *w.UsedPercent < 100) {
			continue
		}
		exhausted = true
		if w.ResetsAt != nil && w.ResetsAt.After(now) && w.ResetsAt.After(latest) {
			latest = *w.ResetsAt
		}
	}
	return latest, exhausted
}

func backoffFor(schedule []time.Duration, failedAttempt int32) time.Duration {
	i := int(failedAttempt) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(schedule) {
		i = len(schedule) - 1
	}
	return schedule[i]
}

// jitter spreads d by ±fraction so tasks throttled together do not retry in
// lockstep.
func jitter(d time.Duration, fraction float64) time.Duration {
	return time.Duration(float64(d) * (1 + fraction*(2*rand.Float64()-1)))
}
