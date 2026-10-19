package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

type createProductIntakeApprovalRequest struct {
	BindingID   string `json:"binding_id"`
	MessageID   string `json:"message_id"`
	ArtifactSHA string `json:"artifact_sha"`
	Decision    string `json:"decision"`
}

// CreateProductIntakeApproval is a human owner/admin gate. The decision is
// append-only and cannot be supplied by the intake sender's Chat request.
func (h *Handler) CreateProductIntakeApproval(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents cannot approve product intake")
		return
	}
	if _, roleOK := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !roleOK {
		return
	}
	var req createProductIntakeApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	bindingUUID, ok := parseUUIDOrBadRequest(w, req.BindingID, "binding_id")
	if !ok {
		return
	}
	approval, err := h.ProductIntake.CreateApproval(r.Context(), service.ProductIntakeApprovalRequest{
		WorkspaceID: workspaceUUID, BindingID: bindingUUID, ApproverID: util.MustParseUUID(userID),
		MessageID: strings.TrimSpace(req.MessageID), ArtifactSHA: strings.TrimSpace(req.ArtifactSHA), Decision: req.Decision,
	})
	if err != nil {
		if errors.Is(err, service.ErrIntakeApproval) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to persist product intake approval")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": util.UUIDToString(approval.ID), "artifact_sha": approval.ArtifactSha,
		"decision": approval.Decision, "created_at": timestampToString(approval.CreatedAt),
	})
}
