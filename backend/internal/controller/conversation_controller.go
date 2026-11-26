package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// ConversationController handles HTTP endpoints for conversations, settings, and conversation members.
type ConversationController struct {
	convSvc *service.ConversationService
}

// NewConversationController constructs a ConversationController.
func NewConversationController(convSvc *service.ConversationService) *ConversationController {
	return &ConversationController{convSvc: convSvc}
}

type optionalBool struct {
	Set   bool
	Valid bool
	Value bool
}

func (o *optionalBool) UnmarshalJSON(data []byte) error {
	o.Set = true
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		o.Valid = false
		return nil
	}
	var v bool
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	o.Valid = true
	o.Value = v
	return nil
}

type patchConversationSettingsRequest struct {
	IsPinned  optionalBool `json:"is_pinned"`
	IsMuted   optionalBool `json:"is_muted"`
	IsVisible optionalBool `json:"is_visible"`
}

// List handles GET /api/v1/conversations.
func (h *ConversationController) List(c *gin.Context) {
	if h == nil || h.convSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit := service.DefaultConversationListLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxConversationListLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	includeHidden := false
	if rawHidden, hasHidden := c.GetQuery("include_hidden"); hasHidden {
		parsedHidden, err := strconv.ParseBool(strings.TrimSpace(rawHidden))
		if err != nil {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("include_hidden must be a boolean"))
			return
		}
		includeHidden = parsedHidden
	}

	cursor := strings.TrimSpace(c.Query("cursor"))
	res, bizErr := h.convSvc.ListConversations(c.Request.Context(), userID, includeHidden, cursor, limit)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// GetDetail handles GET /api/v1/conversations/:conversation_id.
func (h *ConversationController) GetDetail(c *gin.Context) {
	if h == nil || h.convSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	res, bizErr := h.convSvc.GetConversationDetail(c.Request.Context(), userID, convID)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// UpdateSettings handles PATCH /api/v1/conversations/:conversation_id/settings.
func (h *ConversationController) UpdateSettings(c *gin.Context) {
	if h == nil || h.convSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	req, bindErr := parsePatchConversationSettings(c)
	if bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}

	var input service.UpdateConversationSettingsInput
	if req.IsPinned.Set {
		v := req.IsPinned.Value
		input.IsPinned = &v
	}
	if req.IsMuted.Set {
		v := req.IsMuted.Value
		input.IsMuted = &v
	}
	if req.IsVisible.Set {
		v := req.IsVisible.Value
		input.IsVisible = &v
	}

	res, bizErr := h.convSvc.UpdateSettings(c.Request.Context(), userID, convID, input)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// ListMembers handles GET /api/v1/conversations/:conversation_id/members.
func (h *ConversationController) ListMembers(c *gin.Context) {
	if h == nil || h.convSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	limit := service.DefaultConversationListLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxConversationListLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	cursor := strings.TrimSpace(c.Query("cursor"))
	res, bizErr := h.convSvc.ListMembers(c.Request.Context(), userID, convID, cursor, limit)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

func parsePatchConversationSettings(c *gin.Context) (patchConversationSettingsRequest, *errcode.Error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return patchConversationSettingsRequest{}, errcode.ErrPayloadTooLarge
		}
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams.WithMessage("request body must not be empty")
	}

	var req patchConversationSettingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	if !req.IsPinned.Set && !req.IsMuted.Set && !req.IsVisible.Set {
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams.WithMessage("at least one of is_pinned, is_muted, or is_visible must be provided")
	}
	if (req.IsPinned.Set && !req.IsPinned.Valid) ||
		(req.IsMuted.Set && !req.IsMuted.Valid) ||
		(req.IsVisible.Set && !req.IsVisible.Valid) {
		return patchConversationSettingsRequest{}, errcode.ErrInvalidParams.WithMessage("settings fields must be booleans")
	}
	return req, nil
}

func extractChatUserID(c *gin.Context) (uint64, *errcode.Error) {
	if c == nil {
		return 0, errcode.ErrAccessTokenInvalid
	}
	if authCtx, ok := middleware.GetAuthContext(c); ok && authCtx.UserID > 0 {
		if errcode.GetUserID(c) == "" {
			errcode.SetUserID(c, strconv.FormatUint(authCtx.UserID, 10))
		}
		return authCtx.UserID, nil
	}
	return extractFriendAuthUserID(c)
}
