package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Provider-limit recovery. A rate-limited or usage-window failure is not a
// problem with the work: the provider says the account will accept requests
// again, often when. These reasons are retried after that moment instead of
// ending the task, and a used-up usage window also throttles the runtime so
// the rest of its queue waits rather than failing on the same limit.

// Default attempt budgets (first run included); a workspace can change them
// through its recovery policy (task_recovery_policy.go).
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
)

// rateLimitBackoff, serverErrorBackoff and awaitingFixBackoff are indexed by
// the failed attempt number minus one; the last entry repeats.
var (
	rateLimitBackoff   = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}
	serverErrorBackoff = []time.Duration{30 * time.Second, 2 * time.Minute}
	// A person has to act first, so rechecks start late and spread out: a
	// recheck before the fix only fails fast at startup again.
	awaitingFixBackoff = []time.Duration{30 * time.Minute, 2 * time.Hour, 8 * time.Hour, 24 * time.Hour}
)

// awaitingFixReasons are failures a person fixes on the runtime's machine or
// account (sign in again, set the missing key, install or upgrade the CLI,
// top up the balance). The run is rechecked on a long backoff, and pulled
// forward as soon as another run on the same runtime succeeds.
var awaitingFixReasons = map[string]bool{
	string(taskfailure.ReasonAgentProviderAuthOrAccess):      true,
	string(taskfailure.ReasonAgentMissingConfig):             true,
	string(taskfailure.ReasonAgentRuntimeMissingExecutable):  true,
	string(taskfailure.ReasonAgentRuntimeVersionUnsupported): true,
	string(taskfailure.ReasonAgentProviderQuotaLimit):        true,
}

// policyAttemptCeiling is the hard cap retryEligible applies to the reasons
// the workspace recovery policy governs; the policy's own budget, at or below
// it, is enforced by planRetry.
const policyAttemptCeiling = MaxRecoveryRetries + 1

var providerLimitReasons = map[string]bool{
	string(taskfailure.ReasonAgentProviderUsageWindow):         true,
	string(taskfailure.ReasonAgentProviderCapacityOrRateLimit): true,
	string(taskfailure.ReasonAgentProviderServerError):         true,
}

// providerLimitRecoveryEnabled is the operator kill switch
// (MULTICA_RECOVERY_ENABLED=false). Turning it off restores the earlier
// posture: provider limits end the task.
func providerLimitRecoveryEnabled() bool {
	v, err := strconv.ParseBool(os.Getenv("MULTICA_RECOVERY_ENABLED"))
	return err != nil || v
}

func isProviderLimitReason(reason string) bool {
	return providerLimitReasons[reason]
}

// isPolicyReason reports whether the workspace recovery policy governs a
// failure reason's retries.
func isPolicyReason(reason string) bool {
	return providerLimitReasons[reason] || awaitingFixReasons[reason]
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
	// maxAttempts is the policy's attempt budget for a provider-limit
	// reason, persisted on the retry so the chain reads "attempt 2 of 4";
	// zero for other reasons.
	maxAttempts int32
}

// attemptCeiling is the max_attempts a retry child records.
func (p retryPlan) attemptCeiling(reason string, taskMaxAttempts int32) int32 {
	if p.maxAttempts > 0 {
		return p.maxAttempts
	}
	return retryAttemptCeiling(reason, taskMaxAttempts)
}

// retryPlanFor plans the retry of a failed attempt, under the workspace
// recovery policy for provider-limit reasons.
func (s *TaskService) retryPlanFor(ctx context.Context, parent db.AgentTaskQueue, reason, errMsg string, now time.Time) (retryPlan, bool) {
	policy := DefaultRecoveryPolicy()
	if isPolicyReason(reason) {
		policy = s.recoveryPolicyFor(ctx, parent)
	}
	return s.planRetry(ctx, parent, reason, errMsg, now, policy)
}

// planRetry schedules the retry of a failed attempt. retry is false when the
// policy turns provider-limit retries off, the reason's budget is spent, or
// the provider's reset lies beyond the policy's longest wait. A used-up usage
// window still sets throttleUntil then: the account is out either way, and
// the rest of the runtime's queue should wait rather than fail.
func (s *TaskService) planRetry(ctx context.Context, parent db.AgentTaskQueue, reason, errMsg string, now time.Time, policy RecoveryPolicy) (plan retryPlan, retry bool) {
	if !isPolicyReason(reason) {
		if delay := retryDelayForAttempt(reason, parent.Attempt); delay > 0 {
			return retryPlan{fireAt: now.Add(delay)}, true
		}
		return retryPlan{}, true
	}
	if !policy.Enabled {
		return retryPlan{}, false
	}
	budget := policy.retries(reason)
	plan.maxAttempts = budget + 1
	retry = parent.Attempt <= budget

	switch reason {
	case string(taskfailure.ReasonAgentProviderUsageWindow):
		reset, source := usageWindowReset(errMsg, s.runtimeRateLimitSnapshot(ctx, parent.RuntimeID), now)
		throttle := reset.Add(usageWindowResetMargin)
		if policy.PauseRuntime && reset.Sub(now) <= time.Duration(MaxRecoveryWaitHours)*time.Hour {
			plan.throttleUntil = throttle
		}
		plan.fireAt = throttle.Add(rand.N(usageWindowResumeJitter))
		plan.hintSource = source
		return plan, retry && reset.Sub(now) <= policy.maxWait()
	case string(taskfailure.ReasonAgentProviderCapacityOrRateLimit):
		wait := backoffFor(rateLimitBackoff, parent.Attempt)
		plan.hintSource = "backoff"
		if hint, ok := taskfailure.ParseRetryHint(errMsg, now, nil); ok && hint.At.Sub(now) > wait {
			wait, plan.hintSource = hint.At.Sub(now), hint.Source
		}
		plan.fireAt = now.Add(jitter(wait, 0.2))
		return plan, retry && wait <= policy.maxWait()
	case string(taskfailure.ReasonAgentProviderServerError):
		plan.fireAt = now.Add(jitter(backoffFor(serverErrorBackoff, parent.Attempt), 0.2))
		plan.hintSource = "backoff"
		return plan, retry
	default: // awaiting a person's fix
		wait := backoffFor(awaitingFixBackoff, parent.Attempt)
		plan.fireAt = now.Add(jitter(wait, 0.2))
		plan.hintSource = "awaiting_fix"
		return plan, retry && wait <= policy.maxWait()
	}
}

// pullForwardRetriesAwaitingFix runs a runtime's waiting rechecks now that a
// run there has succeeded. Best-effort: a missed pull-forward only means the
// recheck runs at its scheduled time.
func (s *TaskService) pullForwardRetriesAwaitingFix(ctx context.Context, runtimeID pgtype.UUID) {
	if !runtimeID.Valid || !providerLimitRecoveryEnabled() {
		return
	}
	reasons := make([]string, 0, len(awaitingFixReasons))
	for reason := range awaitingFixReasons {
		reasons = append(reasons, reason)
	}
	pulled, err := s.Queries.PullForwardRetriesAwaitingFix(ctx, db.PullForwardRetriesAwaitingFixParams{
		RuntimeID: runtimeID,
		Reasons:   reasons,
	})
	if err != nil {
		slog.Warn("pull forward retries awaiting a fix failed", "runtime_id", util.UUIDToString(runtimeID), "error", err)
		return
	}
	if len(pulled) == 0 {
		return
	}
	slog.Info("retries awaiting a fix pulled forward: a run on their runtime succeeded",
		"runtime_id", util.UUIDToString(runtimeID), "count", len(pulled))
	if err := s.PromoteDueDeferredTasksForRuntime(ctx, runtimeID); err != nil {
		slog.Warn("promote pulled-forward retries failed", "runtime_id", util.UUIDToString(runtimeID), "error", err)
	}
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

// ErrTaskNotDeferred: the task already left 'deferred' (promoted, claimed or
// cancelled), so there is no fire time left to move.
var ErrTaskNotDeferred = errors.New("task is not waiting for a scheduled retry")

// RescheduleDeferredTask moves a deferred task's fire time on a person's
// request. A time that is already due is promoted straight away, so "resume
// now" does not wait for the daemon's next deferred-work check.
func (s *TaskService) RescheduleDeferredTask(ctx context.Context, taskID pgtype.UUID, fireAt time.Time) (db.AgentTaskQueue, error) {
	task, err := s.Queries.RescheduleDeferredTask(ctx, db.RescheduleDeferredTaskParams{
		ID:     taskID,
		FireAt: pgtype.Timestamptz{Time: fireAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.AgentTaskQueue{}, ErrTaskNotDeferred
	}
	if err != nil {
		return db.AgentTaskQueue{}, fmt.Errorf("reschedule deferred task: %w", err)
	}
	s.broadcastTaskEvent(ctx, protocol.EventTaskRescheduled, task)
	if !fireAt.After(time.Now()) {
		if err := s.PromoteDueDeferredTasksForRuntime(ctx, task.RuntimeID); err != nil {
			slog.Warn("promote rescheduled task failed", "task_id", util.UUIDToString(task.ID), "error", err)
		}
	}
	return task, nil
}

// LiftRuntimeThrottle ends a usage-window throttle on a person's request and
// releases the deferred work that was only waiting for it. It reports false
// when the runtime was not throttled.
func (s *TaskService) LiftRuntimeThrottle(ctx context.Context, runtimeID pgtype.UUID) (bool, error) {
	lifted, err := s.Queries.LiftRuntimeThrottle(ctx, runtimeID)
	if err != nil {
		return false, fmt.Errorf("lift runtime throttle: %w", err)
	}
	if lifted == 0 {
		return false, nil
	}
	if err := s.PromoteDueDeferredTasksForRuntime(ctx, runtimeID); err != nil {
		slog.Warn("promote after lifting runtime throttle failed", "runtime_id", util.UUIDToString(runtimeID), "error", err)
	}
	return true, nil
}
