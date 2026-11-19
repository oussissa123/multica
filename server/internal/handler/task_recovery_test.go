package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func resumeTaskRequest(t *testing.T, userID, taskID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/tasks/"+taskID+"/resume", body)
	req = withURLParam(req, "taskId", taskID)
	w := httptest.NewRecorder()
	testHandler.ResumeDeferredTaskByUser(w, withChatTestWorkspaceCtx(t, req))
	return w
}

func taskFireAt(t *testing.T, taskID string) time.Time {
	t.Helper()
	var fireAt time.Time
	dbfx.QueryRow(t, `SELECT fire_at FROM agent_task_queue WHERE id = $1`, taskID).Scan(&fireAt)
	return fireAt
}

func TestResumeDeferredTaskByUser(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "ResumeDeferredAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)

	t.Run("moves a scheduled retry to a later time", func(t *testing.T) {
		taskID := insertPendingChatTask(t, agentID, sessionID, "deferred")
		dbfx.Exec(t, `UPDATE agent_task_queue SET fire_at = now() + interval '3 hours' WHERE id = $1`, taskID)
		want := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

		w := resumeTaskRequest(t, testUserID, taskID, map[string]any{"fire_at": want.Format(time.RFC3339)})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp AgentTaskResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "deferred" || resp.FireAt == nil {
			t.Fatalf("response = status %q fire_at %v, want deferred with fire_at", resp.Status, resp.FireAt)
		}
		if got := taskFireAt(t, taskID); !got.Equal(want) {
			t.Fatalf("fire_at = %s, want %s", got, want)
		}
	})

	t.Run("without a time it becomes due now", func(t *testing.T) {
		taskID := insertPendingChatTask(t, agentID, sessionID, "deferred")
		dbfx.Exec(t, `UPDATE agent_task_queue SET fire_at = now() + interval '3 hours' WHERE id = $1`, taskID)

		w := resumeTaskRequest(t, testUserID, taskID, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := taskFireAt(t, taskID); got.After(time.Now()) {
			t.Fatalf("fire_at = %s, want due now", got)
		}
	})

	t.Run("a time beyond the horizon is refused", func(t *testing.T) {
		taskID := insertPendingChatTask(t, agentID, sessionID, "deferred")
		dbfx.Exec(t, `UPDATE agent_task_queue SET fire_at = now() + interval '3 hours' WHERE id = $1`, taskID)
		before := taskFireAt(t, taskID)

		w := resumeTaskRequest(t, testUserID, taskID, map[string]any{"fire_at": time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		if got := taskFireAt(t, taskID); !got.Equal(before) {
			t.Fatalf("refused request moved fire_at to %s", got)
		}
	})

	t.Run("a task that is not waiting is a conflict", func(t *testing.T) {
		taskID := insertPendingChatTask(t, agentID, sessionID, "running")
		w := resumeTaskRequest(t, testUserID, taskID, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("another member's chat task is not theirs to move", func(t *testing.T) {
		taskID := insertPendingChatTask(t, agentID, sessionID, "deferred")
		dbfx.Exec(t, `UPDATE agent_task_queue SET fire_at = now() + interval '3 hours' WHERE id = $1`, taskID)
		before := taskFireAt(t, taskID)
		otherID := createWorkspaceMemberUser(t, "Resume Outsider", "resume-outsider@multica.test")

		w := resumeTaskRequest(t, otherID, taskID, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
		}
		if got := taskFireAt(t, taskID); !got.Equal(before) {
			t.Fatalf("forbidden request moved fire_at to %s", got)
		}
	})
}

func liftThrottleRequest(actorID, runtimeID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := newRequestAs(actorID, http.MethodPost, "/api/runtimes/"+runtimeID+"/throttle/lift", nil)
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.LiftRuntimeThrottle(w, req)
	return w
}

func TestLiftRuntimeThrottle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	runtimeID, runtimeOwnerID, plainMemberID := runtimeVisibilityFixture(t)
	dbfx.Exec(t, `
		UPDATE agent_runtime
		SET throttled_until = now() + interval '2 hours',
		    throttle_reason = 'agent_error.provider_usage_window',
		    throttled_at = now()
		WHERE id = $1
	`, runtimeID)

	w := liftThrottleRequest(plainMemberID, runtimeID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("plain member: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var stillThrottled bool
	dbfx.QueryRow(t, `SELECT throttled_until IS NOT NULL FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&stillThrottled)
	if !stillThrottled {
		t.Fatal("a forbidden request lifted the throttle")
	}

	w = liftThrottleRequest(runtimeOwnerID, runtimeID)
	if w.Code != http.StatusOK {
		t.Fatalf("owner: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp AgentRuntimeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ThrottledUntil != nil || resp.ThrottleReason != nil {
		t.Fatalf("response still throttled: until=%v reason=%v", resp.ThrottledUntil, resp.ThrottleReason)
	}
}
