package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// WorkspaceSettingTaskRecovery is the workspace.settings key holding how a
// workspace retries runs stopped by a provider limit. Every field is optional;
// an absent or invalid field keeps its default.
const WorkspaceSettingTaskRecovery = "task_recovery"

const (
	// MaxRecoveryRetries bounds every retry budget a workspace may set.
	MaxRecoveryRetries = 10
	// MaxRecoveryWaitHours bounds how far ahead a retry may be scheduled: a
	// weekly window reset, and no further.
	MaxRecoveryWaitHours = 7 * 24
)

// RecoveryPolicy is a workspace's provider-limit retry policy.
type RecoveryPolicy struct {
	// Enabled turns provider-limit retries on; off, such runs end as failed.
	Enabled bool `json:"enabled"`
	// PauseRuntime holds a runtime's queue while its usage window is used up.
	PauseRuntime       bool  `json:"pause_runtime"`
	UsageWindowRetries int32 `json:"usage_window_retries"`
	RateLimitRetries   int32 `json:"rate_limit_retries"`
	ServerErrorRetries int32 `json:"server_error_retries"`
	// MaxWaitHours: a retry the provider would make wait longer is not made.
	MaxWaitHours int32 `json:"max_wait_hours"`
}

// DefaultRecoveryPolicy is what a workspace that set nothing gets.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{
		Enabled:            true,
		PauseRuntime:       true,
		UsageWindowRetries: usageWindowMaxAttempts - 1,
		RateLimitRetries:   rateLimitMaxAttempts - 1,
		ServerErrorRetries: serverErrorMaxAttempts - 1,
		MaxWaitHours:       MaxRecoveryWaitHours,
	}
}

// RecoveryPolicyFromSettings reads the policy from workspace.settings,
// falling back to the default for anything missing or out of range, so a
// hand-edited row can never disable retries by accident.
func RecoveryPolicyFromSettings(settings []byte) RecoveryPolicy {
	p := DefaultRecoveryPolicy()
	var values map[string]json.RawMessage
	if json.Unmarshal(settings, &values) != nil {
		return p
	}
	raw, ok := values[WorkspaceSettingTaskRecovery]
	if !ok {
		return p
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return p
	}
	readBool := func(key string, dst *bool) {
		var v bool
		if json.Unmarshal(fields[key], &v) == nil {
			*dst = v
		}
	}
	readInt := func(key string, lo, hi int32, dst *int32) {
		var v int32
		if json.Unmarshal(fields[key], &v) == nil && v >= lo && v <= hi {
			*dst = v
		}
	}
	readBool("enabled", &p.Enabled)
	readBool("pause_runtime", &p.PauseRuntime)
	readInt("usage_window_retries", 0, MaxRecoveryRetries, &p.UsageWindowRetries)
	readInt("rate_limit_retries", 0, MaxRecoveryRetries, &p.RateLimitRetries)
	readInt("server_error_retries", 0, MaxRecoveryRetries, &p.ServerErrorRetries)
	readInt("max_wait_hours", 1, MaxRecoveryWaitHours, &p.MaxWaitHours)
	return p
}

// ValidateRecoverySetting checks a task_recovery value sent by a client. Only
// known fields with in-range values are accepted, so a settings write cannot
// store something RecoveryPolicyFromSettings would silently ignore.
func ValidateRecoverySetting(value any) error {
	fields, ok := value.(map[string]any)
	if !ok {
		return errors.New("task_recovery must be an object")
	}
	for key, v := range fields {
		switch key {
		case "enabled", "pause_runtime":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("task_recovery.%s must be true or false", key)
			}
		case "usage_window_retries", "rate_limit_retries", "server_error_retries":
			if !intInRange(v, 0, MaxRecoveryRetries) {
				return fmt.Errorf("task_recovery.%s must be a whole number from 0 to %d", key, MaxRecoveryRetries)
			}
		case "max_wait_hours":
			if !intInRange(v, 1, MaxRecoveryWaitHours) {
				return fmt.Errorf("task_recovery.max_wait_hours must be a whole number from 1 to %d", MaxRecoveryWaitHours)
			}
		default:
			return fmt.Errorf("task_recovery.%s is not a known setting", key)
		}
	}
	return nil
}

func intInRange(v any, lo, hi float64) bool {
	n, ok := v.(float64)
	return ok && n == float64(int64(n)) && n >= lo && n <= hi
}

// retries is the retry budget the policy gives a provider-limit reason.
func (p RecoveryPolicy) retries(reason string) int32 {
	switch reason {
	case string(taskfailure.ReasonAgentProviderUsageWindow):
		return p.UsageWindowRetries
	case string(taskfailure.ReasonAgentProviderCapacityOrRateLimit):
		return p.RateLimitRetries
	case string(taskfailure.ReasonAgentProviderServerError):
		return p.ServerErrorRetries
	}
	return 0
}

func (p RecoveryPolicy) maxWait() time.Duration {
	return time.Duration(p.MaxWaitHours) * time.Hour
}

// recoveryPolicyFor resolves the policy of the workspace a task runs in. A
// lookup failure keeps the default: losing a retry over a read error would be
// worse than retrying under defaults.
func (s *TaskService) recoveryPolicyFor(ctx context.Context, task db.AgentTaskQueue) RecoveryPolicy {
	agent, err := s.Queries.GetAgent(ctx, task.AgentID)
	if err != nil {
		return DefaultRecoveryPolicy()
	}
	ws, err := s.Queries.GetWorkspace(ctx, agent.WorkspaceID)
	if err != nil {
		return DefaultRecoveryPolicy()
	}
	return RecoveryPolicyFromSettings(ws.Settings)
}
