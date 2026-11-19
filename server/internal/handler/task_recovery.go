package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// rescheduleMaxAhead matches the longest wait a recovery policy may allow
// (service.MaxRecoveryWaitHours).
const rescheduleMaxAhead = service.MaxRecoveryWaitHours * time.Hour

// ResumeDeferredTaskByUser moves a waiting automatic retry: with no fire_at it
// runs as soon as its runtime can take it, otherwise at the given time.
// Callers who may cancel the task may move it.
func (h *Handler) ResumeDeferredTaskByUser(w http.ResponseWriter, r *http.Request) {
	task, ok := h.requireTaskControl(w, r)
	if !ok {
		return
	}
	var req struct {
		FireAt *time.Time `json:"fire_at"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	now := time.Now()
	fireAt := now
	if req.FireAt != nil {
		if req.FireAt.Before(now.Add(-time.Minute)) || req.FireAt.After(now.Add(rescheduleMaxAhead)) {
			writeError(w, http.StatusBadRequest, "fire_at must be between now and 7 days ahead")
			return
		}
		fireAt = *req.FireAt
	}
	updated, err := h.TaskService.RescheduleDeferredTask(r.Context(), task.ID, fireAt)
	if errors.Is(err, service.ErrTaskNotDeferred) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reschedule task")
		return
	}
	resp := taskToResponse(updated, ctxWorkspaceID(r.Context()))
	h.hydrateTaskAttributions(r.Context(), []*TaskAttribution{resp.Attribution})
	writeJSON(w, http.StatusOK, resp)
}

// LiftRuntimeThrottle lets whoever may edit a runtime end its usage-window
// throttle early, for instance after switching the CLI to another account.
func (h *Handler) LiftRuntimeThrottle(w http.ResponseWriter, r *http.Request) {
	runtimeUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "runtimeId"), "runtime_id")
	if !ok {
		return
	}
	rt, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime not found")
		return
	}
	member, ok := h.requireWorkspaceMember(w, r, uuidToString(rt.WorkspaceID), "runtime not found")
	if !ok {
		return
	}
	if !canEditRuntime(member, rt) {
		writeError(w, http.StatusForbidden, "you can only edit your own runtimes")
		return
	}
	if _, err := h.TaskService.LiftRuntimeThrottle(r.Context(), rt.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lift runtime throttle")
		return
	}
	updated, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reload runtime")
		return
	}
	writeJSON(w, http.StatusOK, runtimeToResponse(updated))
}

// requireTaskControl loads a task the caller may act on, with the same gates
// as CancelTaskByUser: a chat task belongs to the member who opened the chat;
// any other task must be visible and its agent accessible to the caller.
func (h *Handler) requireTaskControl(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	workspaceID := ctxWorkspaceID(r.Context())
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	taskUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "taskId"), "task id")
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	task, err := h.Queries.GetAgentTaskInWorkspace(r.Context(), db.GetAgentTaskInWorkspaceParams{ID: taskUUID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}
	if task.ChatSessionID.Valid {
		cs, err := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{ID: task.ChatSessionID, WorkspaceID: wsUUID})
		if err != nil {
			writeError(w, http.StatusNotFound, "task not found")
			return db.AgentTaskQueue{}, false
		}
		if uuidToString(cs.CreatorID) != userID {
			writeError(w, http.StatusForbidden, "not your task")
			return db.AgentTaskQueue{}, false
		}
		return task, true
	}
	if !h.requireTaskVisible(w, r, workspaceID, task, http.StatusNotFound, "task not found") {
		return db.AgentTaskQueue{}, false
	}
	agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: task.AgentID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return db.AgentTaskQueue{}, false
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if !h.canAccessPrivateAgent(r.Context(), agent, actorType, actorID, workspaceID) {
		writeError(w, http.StatusForbidden, "you do not have access to this agent")
		return db.AgentTaskQueue{}, false
	}
	return task, true
}

func deferredFireAt(t db.AgentTaskQueue) *string {
	if t.Status != "deferred" {
		return nil
	}
	return timestampToPtr(t.FireAt)
}

func activeThrottleUntil(rt db.AgentRuntime) *string {
	if !rt.ThrottledUntil.Valid || !rt.ThrottledUntil.Time.After(time.Now()) {
		return nil
	}
	return timestampToPtr(rt.ThrottledUntil)
}

func activeThrottleReason(rt db.AgentRuntime) *string {
	if activeThrottleUntil(rt) == nil {
		return nil
	}
	return textToPtr(rt.ThrottleReason)
}
