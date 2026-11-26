package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// GroupController handles HTTP endpoints for Stage 6 group chat management.
type GroupController struct {
	svc *service.GroupService
}

// NewGroupController constructs a GroupController backed by GroupService.
func NewGroupController(svc *service.GroupService) *GroupController {
	return &GroupController{svc: svc}
}

type createGroupRequest struct {
	Name          *string  `json:"name"`
	MemberIDs     []string `json:"member_ids"`
	AvatarAssetID *string  `json:"avatar_asset_id"`
	Announcement  *string  `json:"announcement"`
}

type patchGroupRequest struct {
	Name          optionalString `json:"name"`
	AvatarAssetID optionalString `json:"avatar_asset_id"`
	Announcement  optionalString `json:"announcement"`
}

type addGroupMembersRequest struct {
	UserIDs []string `json:"user_ids"`
}

type transferGroupRequest struct {
	TargetUserID *string `json:"target_user_id"`
}

type updateMemberRoleRequest struct {
	Role *string `json:"role"`
}

type updateMyGroupMemberRequest struct {
	NicknameInGroup *string `json:"nickname_in_group"`
}

// Create handles POST /api/v1/groups.
func (h *GroupController) Create(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	var req createGroupRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.Name == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("name is required"))
		return
	}
	if req.MemberIDs == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("member_ids is required"))
		return
	}

	memberIDs, parseErr := parseDecimalUint64Slice(req.MemberIDs, "member_ids")
	if parseErr != nil {
		errcode.Fail(c, parseErr)
		return
	}

	res, bizErr := h.svc.Create(c.Request.Context(), userID, service.CreateGroupInput{
		Name:          *req.Name,
		MemberIDs:     memberIDs,
		AvatarAssetID: req.AvatarAssetID,
		Announcement:  req.Announcement,
		Meta:          extractAuditMeta(c),
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.Created(c, res)
}

// List handles GET /api/v1/groups.
func (h *GroupController) List(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	page, bizErr := h.svc.List(
		c.Request.Context(),
		userID,
		c.Query("status"),
		c.Query("cursor"),
		limit,
	)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

// Get handles GET /api/v1/groups/:group_id.
func (h *GroupController) Get(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	res, bizErr := h.svc.Get(c.Request.Context(), userID, groupID)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Update handles PATCH /api/v1/groups/:group_id.
func (h *GroupController) Update(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req patchGroupRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if !req.Name.Set && !req.AvatarAssetID.Set && !req.Announcement.Set {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("at least one of name, avatar_asset_id, or announcement must be provided"))
		return
	}
	if req.Name.Set && !req.Name.Valid {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("name must be a string"))
		return
	}

	input := service.UpdateGroupInput{
		Meta: extractAuditMeta(c),
	}
	if req.Name.Set {
		v := req.Name.Value
		input.Name = &v
	}
	if req.AvatarAssetID.Set {
		input.HasAvatarAsset = true
		if req.AvatarAssetID.Valid {
			v := req.AvatarAssetID.Value
			input.AvatarAssetID = &v
		}
	}
	if req.Announcement.Set {
		input.HasAnnouncement = true
		if req.Announcement.Valid {
			v := req.Announcement.Value
			input.Announcement = &v
		}
	}

	res, bizErr := h.svc.Update(c.Request.Context(), userID, groupID, input)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Disband handles DELETE /api/v1/groups/:group_id.
func (h *GroupController) Disband(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	res, bizErr := h.svc.Disband(c.Request.Context(), userID, groupID, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// ListMembers handles GET /api/v1/groups/:group_id/members.
func (h *GroupController) ListMembers(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	onlineOnly := false
	if rawOnline, hasOnline := c.GetQuery("online_only"); hasOnline {
		trimmed := strings.TrimSpace(rawOnline)
		if trimmed != "" {
			parsed, err := strconv.ParseBool(trimmed)
			if err != nil {
				errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("online_only must be a boolean"))
				return
			}
			onlineOnly = parsed
		}
	}

	page, bizErr := h.svc.ListMembers(c.Request.Context(), userID, groupID, service.ListGroupMembersInput{
		Role:       c.Query("role"),
		OnlineOnly: onlineOnly,
		Cursor:     c.Query("cursor"),
		Limit:      limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

// AddMembers handles POST /api/v1/groups/:group_id/members.
func (h *GroupController) AddMembers(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req addGroupMembersRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.UserIDs == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("user_ids is required"))
		return
	}

	targetIDs, parseErr := parseDecimalUint64Slice(req.UserIDs, "user_ids")
	if parseErr != nil {
		errcode.Fail(c, parseErr)
		return
	}

	res, bizErr := h.svc.AddMembers(c.Request.Context(), userID, groupID, targetIDs, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// RemoveMember handles DELETE /api/v1/groups/:group_id/members/:user_id.
func (h *GroupController) RemoveMember(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if c.Param("user_id") == "me" {
		errcode.FailWithStatus(c, http.StatusMethodNotAllowed, errcode.ErrInvalidValue.WithMessage("method not allowed"), nil)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}
	targetUserID, targetErr := parseGroupIDParam(c, "user_id")
	if targetErr != nil {
		errcode.Fail(c, targetErr)
		return
	}

	res, bizErr := h.svc.RemoveMember(c.Request.Context(), userID, groupID, targetUserID, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Leave handles POST /api/v1/groups/:group_id/leave.
func (h *GroupController) Leave(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	res, bizErr := h.svc.Leave(c.Request.Context(), userID, groupID, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Transfer handles POST /api/v1/groups/:group_id/transfer.
func (h *GroupController) Transfer(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req transferGroupRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.TargetUserID == nil || strings.TrimSpace(*req.TargetUserID) == "" {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("target_user_id is required"))
		return
	}
	targetUserID, ok := parsePositiveDecimalUint64(strings.TrimSpace(*req.TargetUserID))
	if !ok {
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid target_user_id"))
		return
	}

	res, bizErr := h.svc.Transfer(c.Request.Context(), userID, groupID, targetUserID, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// UpdateMemberRole handles PATCH /api/v1/groups/:group_id/members/:user_id/role.
func (h *GroupController) UpdateMemberRole(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}
	targetUserID, targetErr := parseGroupIDParam(c, "user_id")
	if targetErr != nil {
		errcode.Fail(c, targetErr)
		return
	}

	var req updateMemberRoleRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.Role == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("role is required"))
		return
	}

	res, bizErr := h.svc.UpdateMemberRole(c.Request.Context(), userID, groupID, targetUserID, *req.Role, extractAuditMeta(c))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// PatchMember dispatches PATCH /api/v1/groups/:group_id/members/:user_id,
// handling `/members/me` without Gin radix-tree wildcard conflicts against `/members/:user_id/role`.
func (h *GroupController) PatchMember(c *gin.Context) {
	if c.Param("user_id") == "me" {
		h.UpdateMyMember(c)
		return
	}
	errcode.FailWithStatus(c, http.StatusNotFound, errcode.ErrInvalidValue.WithMessage("endpoint not found"), nil)
}

// UpdateMyMember handles PATCH /api/v1/groups/:group_id/members/me.
func (h *GroupController) UpdateMyMember(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req updateMyGroupMemberRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.NicknameInGroup == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("nickname_in_group is required"))
		return
	}

	res, bizErr := h.svc.UpdateMyMember(c.Request.Context(), userID, groupID, *req.NicknameInGroup)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// ListAudit handles GET /api/v1/groups/:group_id/audit.
func (h *GroupController) ListAudit(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseGroupIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	fromTime, fromErr := parseOptionalRFC3339Query(c, "from")
	if fromErr != nil {
		errcode.Fail(c, fromErr)
		return
	}
	toTime, toErr := parseOptionalRFC3339Query(c, "to")
	if toErr != nil {
		errcode.Fail(c, toErr)
		return
	}

	page, bizErr := h.svc.ListAudit(c.Request.Context(), userID, groupID, service.ListGroupAuditInput{
		From:   fromTime,
		To:     toTime,
		Cursor: c.Query("cursor"),
		Limit:  limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

func bindStrictJSON(c *gin.Context, dst any) *errcode.Error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return errcode.ErrInvalidParams
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return errcode.ErrPayloadTooLarge
		}
		return errcode.ErrInvalidParams
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errcode.ErrInvalidParams.WithMessage("request body must not be empty")
	}
	if trimmed[0] != '{' {
		return errcode.ErrInvalidParams.WithMessage("request body must be a JSON object")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		return errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	return nil
}

func parseGroupIDParam(c *gin.Context, paramName string) (uint64, *errcode.Error) {
	raw := strings.TrimSpace(c.Param(paramName))
	if raw == "" {
		return 0, errcode.ErrInvalidParams.WithMessage(paramName + " is required")
	}
	val, ok := parsePositiveDecimalUint64(raw)
	if !ok {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid " + paramName)
	}
	return val, nil
}

func parseDecimalUint64Slice(items []string, fieldName string) ([]uint64, *errcode.Error) {
	out := make([]uint64, 0, len(items))
	for _, raw := range items {
		val, ok := parsePositiveDecimalUint64(strings.TrimSpace(raw))
		if !ok {
			return nil, errcode.ErrInvalidValue.WithMessage("invalid user id in " + fieldName)
		}
		out = append(out, val)
	}
	return out, nil
}

func parseOptionalLimit(c *gin.Context) (int, *errcode.Error) {
	rawLimit, hasLimit := c.GetQuery("limit")
	if !hasLimit {
		return 0, nil
	}
	trimmed := strings.TrimSpace(rawLimit)
	if trimmed == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(trimmed)
	if err != nil || parsed < 1 || parsed > service.MaxGroupListLimit {
		return 0, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100")
	}
	return parsed, nil
}

func parseOptionalRFC3339Query(c *gin.Context, key string) (*time.Time, *errcode.Error) {
	raw, exists := c.GetQuery(key)
	if !exists {
		return nil, nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return nil, errcode.ErrInvalidValue.WithMessage("invalid " + key + " timestamp, expected RFC 3339")
	}
	utc := parsed.UTC()
	return &utc, nil
}

func extractAuditMeta(c *gin.Context) service.GroupAuditMeta {
	if c == nil || c.Request == nil {
		return service.GroupAuditMeta{}
	}
	return service.GroupAuditMeta{
		RequestID: errcode.GetRequestID(c),
		IPAddress: c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
	}
}
