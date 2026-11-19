package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestExhaustedWindowReset(t *testing.T) {
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		snapshot      string
		wantExhausted bool
		wantReset     time.Time
	}{
		{"no snapshot", ``, false, time.Time{}},
		{"room left", `{"status":"allowed","windows":[{"kind":"session","used_percent":40,"resets_at":"2026-11-18T14:00:00Z"}]}`, false, time.Time{}},
		{"session used up", `{"windows":[{"kind":"session","used_percent":100,"resets_at":"2026-11-18T14:00:00Z"}]}`, true, time.Date(2026, time.November, 18, 14, 0, 0, 0, time.UTC)},
		{
			"latest of the used-up windows",
			`{"windows":[{"kind":"session","status":"rejected","resets_at":"2026-11-18T14:00:00Z"},{"kind":"weekly","used_percent":101,"resets_at":"2026-11-20T09:00:00Z"},{"kind":"weekly_opus","used_percent":20,"resets_at":"2026-11-25T09:00:00Z"}]}`,
			true, time.Date(2026, time.November, 20, 9, 0, 0, 0, time.UTC),
		},
		{"rejected without a reset", `{"status":"rejected","windows":[]}`, true, time.Time{}},
		{"malformed", `{"windows":`, false, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset, exhausted := exhaustedWindowReset([]byte(tc.snapshot), now)
			if exhausted != tc.wantExhausted || !reset.Equal(tc.wantReset) {
				t.Fatalf("exhaustedWindowReset = (%s, %v), want (%s, %v)", reset, exhausted, tc.wantReset, tc.wantExhausted)
			}
		})
	}
}

func TestUsageWindowResetSources(t *testing.T) {
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	snapshot := []byte(`{"windows":[{"kind":"session","used_percent":100,"resets_at":"2026-11-18T16:00:00Z"}]}`)

	reset, source := usageWindowReset("You've hit your session limit · resets 2pm (UTC)", snapshot, now)
	if source != "error_text" || !reset.Equal(time.Date(2026, time.November, 18, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("error text first: got (%s, %s)", reset, source)
	}
	reset, source = usageWindowReset("You've hit your session limit", snapshot, now)
	if source != "rate_limit_snapshot" || !reset.Equal(time.Date(2026, time.November, 18, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("snapshot second: got (%s, %s)", reset, source)
	}
	reset, source = usageWindowReset("You've hit your session limit", nil, now)
	if source != "default" || !reset.Equal(now.Add(usageWindowDefaultWait)) {
		t.Fatalf("default last: got (%s, %s)", reset, source)
	}
}

func TestPlanRetryRateLimit(t *testing.T) {
	now := time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC)
	svc := &TaskService{}
	reason := "agent_error.provider_capacity_or_rate_limit"

	plan, ok := svc.planRetry(context.Background(), db.AgentTaskQueue{Attempt: 1}, reason, "API Error: 429 Too Many Requests", now)
	if !ok || plan.fireAt.Before(now.Add(48*time.Second)) || plan.fireAt.After(now.Add(72*time.Second)) {
		t.Fatalf("first backoff = %s (ok=%v), want 1m ±20%%", plan.fireAt.Sub(now), ok)
	}
	if !plan.throttleUntil.IsZero() {
		t.Fatalf("a rate limit must not throttle the runtime")
	}

	plan, ok = svc.planRetry(context.Background(), db.AgentTaskQueue{Attempt: 1}, reason, "API Error: 429 (retry-after: 600)", now)
	if !ok || plan.hintSource != "error_text" || plan.fireAt.Before(now.Add(8*time.Minute)) || plan.fireAt.After(now.Add(12*time.Minute)) {
		t.Fatalf("retry-after longer than the backoff wins: got %s from %q", plan.fireAt.Sub(now), plan.hintSource)
	}

	plan, _ = svc.planRetry(context.Background(), db.AgentTaskQueue{Attempt: 9}, reason, "429", now)
	if plan.fireAt.Before(now.Add(48*time.Minute)) || plan.fireAt.After(now.Add(72*time.Minute)) {
		t.Fatalf("backoff past the schedule repeats the last step: got %s", plan.fireAt.Sub(now))
	}

	if got := retryAttemptCeiling(reason, 2); got != rateLimitMaxAttempts {
		t.Fatalf("retryAttemptCeiling = %d, want %d", got, rateLimitMaxAttempts)
	}
	if got := retryAttemptCeiling(reason, 1); got != 1 {
		t.Fatalf("max_attempts=1 disables retry, got ceiling %d", got)
	}
}

func TestProviderLimitRecoveryKillSwitch(t *testing.T) {
	t.Setenv("MULTICA_RECOVERY_ENABLED", "false")
	if isRetryableReason("agent_error.provider_usage_window") {
		t.Fatal("usage window retried with MULTICA_RECOVERY_ENABLED=false")
	}
	if !isRetryableReason("agent_error.provider_network") {
		t.Fatal("the kill switch must leave the infrastructure allowlist alone")
	}
}

// TestUsageWindowFailureThrottlesRuntimeUntilReset covers the whole boundary:
// a used-up session window defers the retry to the reset, throttles the
// runtime so neither due deferred work nor queued work is handed out, and a
// later snapshot with room left releases the runtime early.
func TestUsageWindowFailureThrottlesRuntimeUntilReset(t *testing.T) {
	sharedPool := newResolveOriginatorPool(t)
	// The throttle is on a runtime row other packages' sweeps may read.
	pool := testutil.IsolatedDB(t, sharedPool.Config().ConnString())
	ctx := context.Background()
	q := db.New(pool)
	_, _, agentID, issueID := seedAttributionFixture(t, pool)

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET status='online', last_seen_at=now() WHERE id=$1`, runtimeID); err != nil {
		t.Fatalf("mark runtime online: %v", err)
	}
	var parentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts, session_id, work_dir)
		VALUES ($1, $2, $3, 'running', 0, 1, 2, 'src-session', '/tmp/src-workdir')
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&parentID); err != nil {
		t.Fatalf("insert parent task: %v", err)
	}

	svc := NewTaskService(q, pool, nil, events.New())
	before := time.Now()
	failed, err := svc.FailTask(ctx, parentID, "You've hit your session limit · resets 2pm (UTC)", "src-session", "/tmp/src-workdir", "", "", false, "", "")
	if err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	if failed.FailureReason.String != "agent_error.provider_usage_window" {
		t.Fatalf("failure_reason = %q, want agent_error.provider_usage_window", failed.FailureReason.String)
	}

	reset := time.Date(before.UTC().Year(), before.UTC().Month(), before.UTC().Day(), 14, 0, 0, 0, time.UTC)
	if !reset.After(before) {
		reset = reset.AddDate(0, 0, 1)
	}
	var (
		childID     pgtype.UUID
		childStatus string
		fireAt      pgtype.Timestamptz
		maxAttempts int32
		sessionID   pgtype.Text
	)
	if err := pool.QueryRow(ctx, `
		SELECT id, status, fire_at, max_attempts, session_id FROM agent_task_queue WHERE parent_task_id = $1
	`, parentID).Scan(&childID, &childStatus, &fireAt, &maxAttempts, &sessionID); err != nil {
		t.Fatalf("read retry child: %v", err)
	}
	wantEarliest := reset.Add(usageWindowResetMargin)
	if childStatus != "deferred" || fireAt.Time.Before(wantEarliest) || fireAt.Time.After(wantEarliest.Add(usageWindowResumeJitter)) {
		t.Fatalf("child = %s fire_at %s, want deferred within a minute after %s", childStatus, fireAt.Time, wantEarliest)
	}
	if maxAttempts != usageWindowMaxAttempts {
		t.Fatalf("child max_attempts = %d, want %d", maxAttempts, usageWindowMaxAttempts)
	}
	if sessionID.String != "src-session" {
		t.Fatalf("child session = %q, want the parent's session resumed", sessionID.String)
	}

	var throttledUntil pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT throttled_until FROM agent_runtime WHERE id=$1`, runtimeID).Scan(&throttledUntil); err != nil {
		t.Fatalf("read runtime throttle: %v", err)
	}
	if !throttledUntil.Valid || !throttledUntil.Time.Equal(wantEarliest) {
		t.Fatalf("runtime throttled_until = %+v, want %s", throttledUntil, wantEarliest)
	}

	// Due deferred work stays put while the runtime is throttled.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET fire_at=now() - interval '1 second' WHERE id=$1`, childID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	claimed, err := svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID))
	if err != nil || claimed != nil {
		t.Fatalf("throttled runtime claim = (%v, %v), want nothing", claimed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, childID).Scan(&childStatus); err != nil || childStatus != "deferred" {
		t.Fatalf("throttled claim promoted the child to %q (err %v)", childStatus, err)
	}

	// So does queued work.
	if _, err := pool.Exec(ctx, `UPDATE agent_task_queue SET status='queued' WHERE id=$1`, childID); err != nil {
		t.Fatalf("queue child: %v", err)
	}
	claimed, err = svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID))
	if err != nil || claimed != nil {
		t.Fatalf("throttled runtime claimed queued work = (%v, %v), want nothing", claimed, err)
	}

	// A snapshot from before the throttle says nothing about the account now.
	room := []byte(`{"status":"allowed","windows":[{"kind":"session","used_percent":5}]}`)
	svc.LiftRuntimeThrottleForSnapshot(ctx, util.MustParseUUID(runtimeID), room, before.Add(-time.Minute))
	if claimed, _ = svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID)); claimed != nil {
		t.Fatalf("a stale snapshot lifted the throttle")
	}

	svc.LiftRuntimeThrottleForSnapshot(ctx, util.MustParseUUID(runtimeID), room, time.Now().Add(time.Second))
	claimed, err = svc.ClaimTaskForRuntime(ctx, util.MustParseUUID(runtimeID))
	if err != nil {
		t.Fatalf("claim after the window has room: %v", err)
	}
	if claimed == nil || claimed.ID != childID {
		t.Fatalf("claimed = %v, want the retry child %s", claimed, util.UUIDToString(childID))
	}
}
