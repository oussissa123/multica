package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestRecoveryPolicyFromSettings(t *testing.T) {
	def := DefaultRecoveryPolicy()
	cases := []struct {
		name     string
		settings string
		want     RecoveryPolicy
	}{
		{"no settings", ``, def},
		{"no recovery key", `{"github_enabled":false}`, def},
		{"overrides", `{"task_recovery":{"enabled":true,"pause_runtime":false,"usage_window_retries":1,"rate_limit_retries":0,"server_error_retries":10,"max_wait_hours":24}}`,
			RecoveryPolicy{Enabled: true, PauseRuntime: false, UsageWindowRetries: 1, RateLimitRetries: 0, ServerErrorRetries: 10, MaxWaitHours: 24}},
		{"turned off", `{"task_recovery":{"enabled":false}}`, func() RecoveryPolicy { p := def; p.Enabled = false; return p }()},
		{"out of range and mistyped values keep defaults", `{"task_recovery":{"enabled":"no","usage_window_retries":99,"rate_limit_retries":-1,"max_wait_hours":0}}`, def},
		{"malformed object", `{"task_recovery":[1,2]}`, def},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecoveryPolicyFromSettings([]byte(tc.settings)); got != tc.want {
				t.Fatalf("RecoveryPolicyFromSettings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestValidateRecoverySetting(t *testing.T) {
	valid := map[string]any{"enabled": true, "pause_runtime": false, "usage_window_retries": float64(3), "max_wait_hours": float64(168)}
	if err := ValidateRecoverySetting(valid); err != nil {
		t.Fatalf("valid setting rejected: %v", err)
	}
	for name, value := range map[string]any{
		"not an object":    "on",
		"unknown field":    map[string]any{"retries": float64(3)},
		"mistyped switch":  map[string]any{"enabled": "yes"},
		"too many retries": map[string]any{"rate_limit_retries": float64(MaxRecoveryRetries + 1)},
		"fractional":       map[string]any{"rate_limit_retries": 1.5},
		"zero wait":        map[string]any{"max_wait_hours": float64(0)},
		"wait over a week": map[string]any{"max_wait_hours": float64(MaxRecoveryWaitHours + 1)},
	} {
		if err := ValidateRecoverySetting(value); err == nil {
			t.Errorf("%s: accepted %v", name, value)
		}
	}
}

func TestPlanRetryFollowsPolicy(t *testing.T) {
	now := time.Date(2026, time.November, 18, 11, 58, 0, 0, time.UTC)
	svc := &TaskService{}
	usage := "agent_error.provider_usage_window"
	notice := "You've hit your session limit · resets 2pm (UTC)"
	first := db.AgentTaskQueue{Attempt: 1}

	plan, retry := svc.planRetry(context.Background(), first, usage, notice, now, DefaultRecoveryPolicy())
	if !retry || plan.throttleUntil.IsZero() || plan.maxAttempts != usageWindowMaxAttempts {
		t.Fatalf("default policy: retry=%v throttle=%s max=%d", retry, plan.throttleUntil, plan.maxAttempts)
	}

	off := DefaultRecoveryPolicy()
	off.Enabled = false
	if plan, retry := svc.planRetry(context.Background(), first, usage, notice, now, off); retry || !plan.throttleUntil.IsZero() {
		t.Fatalf("disabled policy still acts: retry=%v throttle=%s", retry, plan.throttleUntil)
	}

	noRetries := DefaultRecoveryPolicy()
	noRetries.UsageWindowRetries = 0
	plan, retry = svc.planRetry(context.Background(), first, usage, notice, now, noRetries)
	if retry || plan.throttleUntil.IsZero() {
		t.Fatalf("spent budget: retry=%v, want no retry but the runtime still throttled (throttle=%s)", retry, plan.throttleUntil)
	}

	shortWait := DefaultRecoveryPolicy()
	shortWait.MaxWaitHours = 1
	if _, retry := svc.planRetry(context.Background(), first, usage, notice, now, shortWait); retry {
		t.Fatal("a reset two hours away was retried under a one-hour maximum wait")
	}

	noPause := DefaultRecoveryPolicy()
	noPause.PauseRuntime = false
	plan, retry = svc.planRetry(context.Background(), first, usage, notice, now, noPause)
	if !retry || !plan.throttleUntil.IsZero() {
		t.Fatalf("pause_runtime off: retry=%v throttle=%s, want a retry and no throttle", retry, plan.throttleUntil)
	}

	moreRetries := DefaultRecoveryPolicy()
	moreRetries.ServerErrorRetries = 6
	plan, retry = svc.planRetry(context.Background(), db.AgentTaskQueue{Attempt: 5}, "agent_error.provider_server_error", "API Error: 503", now, moreRetries)
	if !retry || plan.attemptCeiling("agent_error.provider_server_error", 2) != 7 {
		t.Fatalf("raised budget: retry=%v ceiling=%d", retry, plan.attemptCeiling("agent_error.provider_server_error", 2))
	}
	if !retryEligible("agent_error.provider_server_error", db.AgentTaskQueue{Attempt: 5, MaxAttempts: 2, IssueID: pgtype.UUID{Valid: true}}) {
		t.Fatal("retryEligible's hard cap blocks a budget the policy allows")
	}
}

// TestWorkspacePolicyStopsUsageWindowRetries: a workspace that allows no
// usage-window retries gets no retry child, but its runtime is still paused
// so the rest of the queue does not fail on the same limit.
func TestWorkspacePolicyStopsUsageWindowRetries(t *testing.T) {
	sharedPool := newResolveOriginatorPool(t)
	pool := testutil.IsolatedDB(t, sharedPool.Config().ConnString())
	ctx := context.Background()
	q := db.New(pool)
	workspaceID, _, agentID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE workspace SET settings = settings || '{"task_recovery":{"usage_window_retries":0}}'::jsonb WHERE id = $1`, workspaceID); err != nil {
		t.Fatalf("set workspace policy: %v", err)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("read agent runtime: %v", err)
	}
	var parentID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts)
		VALUES ($1, $2, $3, 'running', 0, 1, 2)
		RETURNING id
	`, agentID, runtimeID, issueID).Scan(&parentID); err != nil {
		t.Fatalf("insert parent task: %v", err)
	}

	svc := NewTaskService(q, pool, nil, events.New())
	if _, err := svc.FailTask(ctx, parentID, "You've hit your session limit · resets 2pm (UTC)", "", "", "", "", false, "", ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	var children int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, parentID).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if children != 0 {
		t.Fatalf("created %d retries, want none under usage_window_retries=0", children)
	}
	var throttled bool
	if err := pool.QueryRow(ctx, `SELECT throttled_until > now() FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&throttled); err != nil {
		t.Fatalf("read throttle: %v", err)
	}
	if !throttled {
		t.Fatal("runtime not throttled although its account is out")
	}
}
