package controller

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

type updateFriendRemarkRequest struct {
	Remark *string `json:"remark"`
}

// FriendController handles HTTP endpoints for friend list, remark, and removal.
type FriendController struct {
	friendSvc *service.FriendService
}

// NewFriendController creates a FriendController backed by FriendService.
func NewFriendController(friendSvc *service.FriendService) *FriendController {
	return &FriendController{friendSvc: friendSvc}
}

// List handles GET /api/v1/friends.
func (h *FriendController) List(c *gin.Context) {
	userID, authErr := extractAuthenticatedUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit := service.DefaultFriendListLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxFriendListLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	cursor := strings.TrimSpace(c.Query("cursor"))
	res, svcErr := h.friendSvc.ListFriends(c.Request.Context(), userID, cursor, limit)
	if svcErr != nil {
		errcode.Fail(c, svcErr)
		return
	}
	errcode.OK(c, res)
}

// UpdateRemark handles PATCH /api/v1/friends/:friend_id.
func (h *FriendController) UpdateRemark(c *gin.Context) {
	userID, authErr := extractAuthenticatedUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	friendID, paramErr := parsePositiveUint64Param(c.Param("friend_id"), "friend_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	if c.Request == nil || c.Request.Body == nil {
		errcode.Fail(c, errcode.ErrInvalidParams)
		return
	}
	var req updateFriendRemarkRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		errcode.Fail(c, errcode.ErrInvalidParams)
		return
	}
	if req.Remark == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("remark is required"))
		return
	}

	res, svcErr := h.friendSvc.UpdateRemark(c.Request.Context(), userID, friendID, *req.Remark)
	if svcErr != nil {
		errcode.Fail(c, svcErr)
		return
	}
	errcode.OK(c, res)
}

// Delete handles DELETE /api/v1/friends/:friend_id.
func (h *FriendController) Delete(c *gin.Context) {
	userID, authErr := extractAuthenticatedUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	friendID, paramErr := parsePositiveUint64Param(c.Param("friend_id"), "friend_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	res, svcErr := h.friendSvc.DeleteFriend(c.Request.Context(), userID, friendID)
	if svcErr != nil {
		errcode.Fail(c, svcErr)
		return
	}
	errcode.OK(c, res)
}

func extractAuthenticatedUserID(c *gin.Context) (uint64, *errcode.Error) {
	if s := strings.TrimSpace(errcode.GetUserID(c)); s != "" {
		uid, err := strconv.ParseUint(s, 10, 64)
		if err != nil || uid == 0 {
			return 0, errcode.ErrAccessTokenInvalid
		}
		return uid, nil
	}
	if c != nil {
		if raw, exists := c.Get(errcode.ContextKeyUserID); exists {
			switch v := raw.(type) {
			case uint64:
				if v > 0 {
					return v, nil
				}
			case int64:
				if v > 0 {
					return uint64(v), nil
				}
			case int:
				if v > 0 {
					return uint64(v), nil
				}
			}
		}
	}
	return 0, errcode.ErrAccessTokenInvalid
}

func parsePositiveUint64Param(raw, name string) (uint64, *errcode.Error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errcode.ErrInvalidParams.WithMessage(name + " is required")
	}
	val, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || val == 0 {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid " + name)
	}
	return val, nil
}
