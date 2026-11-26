package controller

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// FriendRequestController handles HTTP endpoints for friend request workflows.
type FriendRequestController struct {
	svc *service.FriendRequestService
}

// NewFriendRequestController constructs a FriendRequestController backed by FriendRequestService.
func NewFriendRequestController(svc *service.FriendRequestService) *FriendRequestController {
	return &FriendRequestController{svc: svc}
}

type createFriendRequestBody struct {
	ToUserID string  `json:"to_user_id"`
	Message  *string `json:"message"`
}

// Create handles POST /api/v1/friend-requests.
func (h *FriendRequestController) Create(c *gin.Context) {
	userID, ok := extractAuthenticatedUserID(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}

	var body createFriendRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		errcode.Fail(c, errcode.ErrInvalidParams)
		return
	}
	if body.ToUserID == "" {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("to_user_id is required"))
		return
	}
	toUserID, valid := parsePositiveDecimalUint64(body.ToUserID)
	if !valid {
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid to_user_id"))
		return
	}

	msg := ""
	if body.Message != nil {
		msg = *body.Message
	}

	dto, svcErr := h.svc.Create(c.Request.Context(), userID, service.CreateFriendRequestInput{
		ToUserID: toUserID,
		Message:  msg,
	})
	if svcErr != nil {
		writeServiceError(c, svcErr)
		return
	}
	errcode.Created(c, gin.H{"request": dto})
}

// List handles GET /api/v1/friend-requests.
func (h *FriendRequestController) List(c *gin.Context) {
	userID, ok := extractAuthenticatedUserID(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}

	limit := 0
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid limit"))
			return
		}
		limit = parsed
	}

	page, svcErr := h.svc.List(c.Request.Context(), userID, service.ListFriendRequestsInput{
		Direction: c.Query("direction"),
		Status:    c.Query("status"),
		Cursor:    c.Query("cursor"),
		Limit:     limit,
	})
	if svcErr != nil {
		writeServiceError(c, svcErr)
		return
	}
	errcode.OK(c, page)
}

// Accept handles POST /api/v1/friend-requests/:id/accept.
func (h *FriendRequestController) Accept(c *gin.Context) {
	userID, ok := extractAuthenticatedUserID(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}
	reqID, valid := parsePositiveDecimalUint64(c.Param("id"))
	if !valid {
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid friend request id"))
		return
	}

	res, svcErr := h.svc.Accept(c.Request.Context(), userID, reqID)
	if svcErr != nil {
		writeServiceError(c, svcErr)
		return
	}
	errcode.OK(c, res)
}

// Reject handles POST /api/v1/friend-requests/:id/reject.
func (h *FriendRequestController) Reject(c *gin.Context) {
	h.handleRequestAction(c, h.svc.Reject)
}

// Cancel handles POST /api/v1/friend-requests/:id/cancel.
func (h *FriendRequestController) Cancel(c *gin.Context) {
	h.handleRequestAction(c, h.svc.Cancel)
}

func (h *FriendRequestController) handleRequestAction(
	c *gin.Context,
	fn func(ctx context.Context, userID, reqID uint64) (*service.FriendRequestDTO, *service.ServiceError),
) {
	userID, ok := extractAuthenticatedUserID(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}
	reqID, valid := parsePositiveDecimalUint64(c.Param("id"))
	if !valid {
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid friend request id"))
		return
	}

	dto, svcErr := fn(c.Request.Context(), userID, reqID)
	if svcErr != nil {
		writeServiceError(c, svcErr)
		return
	}
	errcode.OK(c, gin.H{"request": dto})
}

func writeServiceError(c *gin.Context, svcErr *service.ServiceError) {
	if svcErr == nil || svcErr.BizErr == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if svcErr.HTTPStatus > 0 && svcErr.HTTPStatus != svcErr.BizErr.HTTPStatus {
		errcode.FailWithStatus(c, svcErr.HTTPStatus, svcErr.BizErr, nil)
		return
	}
	if svcErr.HTTPStatus == http.StatusForbidden && svcErr.BizErr.HTTPStatus != http.StatusForbidden {
		errcode.FailWithStatus(c, http.StatusForbidden, svcErr.BizErr, nil)
		return
	}
	errcode.Fail(c, svcErr.BizErr)
}

func extractAuthenticatedUserID(c *gin.Context) (uint64, bool) {
	if c == nil {
		return 0, false
	}
	raw, exists := c.Get(errcode.ContextKeyUserID)
	if !exists || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case string:
		return parsePositiveDecimalUint64(v)
	case uint64:
		return v, v > 0
	case int64:
		if v <= 0 {
			return 0, false
		}
		return uint64(v), true
	case uint:
		if v == 0 {
			return 0, false
		}
		return uint64(v), true
	default:
		return 0, false
	}
}

func parsePositiveDecimalUint64(raw string) (uint64, bool) {
	if raw == "" || raw[0] < '1' || raw[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}
