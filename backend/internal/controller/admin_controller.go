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

	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// AdminController handles HTTP endpoints for Stage 9 platform administration and monitoring.
type AdminController struct {
	svc *service.AdminService
}

// NewAdminController constructs an AdminController backed by AdminService.
func NewAdminController(svc *service.AdminService) *AdminController {
	return &AdminController{svc: svc}
}

type updateAdminUserStatusRequest struct {
	Status *int8  `json:"status"`
	Reason string `json:"reason"`
}

type kickAdminUserRequest struct {
	SessionID *string `json:"session_id"`
	Reason    string  `json:"reason"`
}

type forceDisbandAdminGroupRequest struct {
	Reason string `json:"reason"`
}

// ListUsers handles GET /api/v1/admin/users.
func (h *AdminController) ListUsers(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	var statusPtr *int8
	if rawStatus := strings.TrimSpace(c.Query("status")); rawStatus != "" {
		parsed, err := strconv.ParseInt(rawStatus, 10, 8)
		if err != nil || (parsed != 0 && parsed != 1) {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("status must be 0 or 1"))
			return
		}
		v := int8(parsed)
		statusPtr = &v
	}

	page, bizErr := h.svc.ListUsers(c.Request.Context(), service.ListAdminUsersInput{
		Query:  c.Query("q"),
		Status: statusPtr,
		Role:   c.Query("role"),
		Cursor: c.Query("cursor"),
		Limit:  limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

// GetUserDetail handles GET /api/v1/admin/users/:user_id.
func (h *AdminController) GetUserDetail(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	userID, paramErr := parseAdminIDParam(c, "user_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	detail, bizErr := h.svc.GetUserDetail(c.Request.Context(), userID)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, detail)
}

// UpdateUserStatus handles PATCH /api/v1/admin/users/:user_id/status.
func (h *AdminController) UpdateUserStatus(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	adminID, authErr := extractAdminUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	targetUserID, paramErr := parseAdminIDParam(c, "user_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req updateAdminUserStatusRequest
	if bindErr := bindStrictJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}
	if req.Status == nil {
		errcode.Fail(c, errcode.ErrInvalidParams.WithMessage("status is required"))
		return
	}

	res, bizErr := h.svc.UpdateUserStatus(
		c.Request.Context(),
		adminID,
		targetUserID,
		*req.Status,
		req.Reason,
		extractAuditMeta(c),
	)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// KickUser handles POST /api/v1/admin/users/:user_id/kick.
func (h *AdminController) KickUser(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	adminID, authErr := extractAdminUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	targetUserID, paramErr := parseAdminIDParam(c, "user_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req kickAdminUserRequest
	if bindErr := bindOptionalJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}

	var sessionIDPtr *uint64
	if req.SessionID != nil && strings.TrimSpace(*req.SessionID) != "" {
		parsed, err := strconv.ParseUint(strings.TrimSpace(*req.SessionID), 10, 64)
		if err != nil || parsed == 0 {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("invalid session_id"))
			return
		}
		sessionIDPtr = &parsed
	}

	res, bizErr := h.svc.KickUser(
		c.Request.Context(),
		adminID,
		targetUserID,
		sessionIDPtr,
		req.Reason,
		extractAuditMeta(c),
	)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// ListGroups handles GET /api/v1/admin/groups.
func (h *AdminController) ListGroups(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	page, bizErr := h.svc.ListGroups(c.Request.Context(), service.ListAdminGroupsInput{
		Query:  c.Query("q"),
		Status: c.Query("status"),
		Cursor: c.Query("cursor"),
		Limit:  limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

// GetGroupDetail handles GET /api/v1/admin/groups/:group_id.
func (h *AdminController) GetGroupDetail(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseAdminIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	detail, bizErr := h.svc.GetGroupDetail(c.Request.Context(), groupID)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, detail)
}

// ForceDisbandGroup handles DELETE /api/v1/admin/groups/:group_id.
func (h *AdminController) ForceDisbandGroup(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	adminID, authErr := extractAdminUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	groupID, paramErr := parseAdminIDParam(c, "group_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var req forceDisbandAdminGroupRequest
	if bindErr := bindOptionalJSON(c, &req); bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}

	res, bizErr := h.svc.ForceDisbandGroup(
		c.Request.Context(),
		adminID,
		groupID,
		req.Reason,
		extractAuditMeta(c),
	)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// ListMessageAudit handles GET /api/v1/admin/messages/audit.
func (h *AdminController) ListMessageAudit(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit, limitErr := parseOptionalLimit(c)
	if limitErr != nil {
		errcode.Fail(c, limitErr)
		return
	}

	convID, convErr := parseOptionalAuditUint64(c, "conversation_id")
	if convErr != nil {
		errcode.Fail(c, convErr)
		return
	}
	senderID, senderErr := parseOptionalAuditUint64(c, "sender_id")
	if senderErr != nil {
		errcode.Fail(c, senderErr)
		return
	}
	from, fromErr := parseOptionalAuditTime(c, "from")
	if fromErr != nil {
		errcode.Fail(c, fromErr)
		return
	}
	to, toErr := parseOptionalAuditTime(c, "to")
	if toErr != nil {
		errcode.Fail(c, toErr)
		return
	}

	page, bizErr := h.svc.ListMessageAudit(c.Request.Context(), service.ListAdminMessageAuditInput{
		ConversationID: convID,
		SenderID:       senderID,
		Keyword:        c.Query("keyword"),
		From:           from,
		To:             to,
		Cursor:         c.Query("cursor"),
		Limit:          limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

// GetMetricsOverview handles GET /api/v1/admin/metrics/overview.
func (h *AdminController) GetMetricsOverview(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	overview, bizErr := h.svc.GetMetricsOverview(c.Request.Context())
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, overview)
}

// GetMetricsStreams handles GET /api/v1/admin/metrics/streams.
func (h *AdminController) GetMetricsStreams(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	streams, bizErr := h.svc.GetMetricsStreams(c.Request.Context())
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, streams)
}

// GetDependenciesHealth handles GET /api/v1/admin/health/dependencies.
func (h *AdminController) GetDependenciesHealth(c *gin.Context) {
	if h == nil || h.svc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	if _, authErr := extractAdminUserID(c); authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	health, bizErr := h.svc.GetDependenciesHealth(c.Request.Context())
	if bizErr != nil {
		errcode.FailWithData(c, bizErr, health)
		return
	}
	errcode.OK(c, health)
}

func extractAdminUserID(c *gin.Context) (uint64, *errcode.Error) {
	if c == nil {
		return 0, errcode.ErrAccessTokenInvalid
	}
	if authCtx, ok := middleware.GetAuthContext(c); ok && authCtx.UserID > 0 {
		if errcode.GetUserID(c) == "" {
			errcode.SetUserID(c, strconv.FormatUint(authCtx.UserID, 10))
		}
		if authCtx.Role != model.UserRoleAdmin {
			return 0, errcode.ErrAdminRequired
		}
		return authCtx.UserID, nil
	}
	userID, ok := readContextUint64(c, errcode.ContextKeyUserID)
	if !ok || userID == 0 {
		return 0, errcode.ErrAccessTokenInvalid
	}
	role := readContextString(c, "role")
	if role != model.UserRoleAdmin {
		return 0, errcode.ErrAdminRequired
	}
	return userID, nil
}

func parseAdminIDParam(c *gin.Context, paramName string) (uint64, *errcode.Error) {
	raw := strings.TrimSpace(c.Param(paramName))
	if raw == "" {
		return 0, errcode.ErrInvalidValue.WithMessage("missing " + paramName)
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, errcode.ErrInvalidValue.WithMessage("invalid " + paramName)
	}
	return id, nil
}

func bindOptionalJSON(c *gin.Context, dst any) *errcode.Error {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
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
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	return nil
}

func parseOptionalAuditUint64(c *gin.Context, key string) (*uint64, *errcode.Error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || parsed == 0 {
		return nil, errcode.ErrInvalidAuditFilter.WithMessage("invalid " + key)
	}
	return &parsed, nil
}

func parseOptionalAuditTime(c *gin.Context, key string) (*time.Time, *errcode.Error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t, err = time.Parse(time.RFC3339, raw)
	}
	if err != nil {
		return nil, errcode.ErrInvalidAuditFilter.WithMessage("invalid RFC3339 timestamp for " + key)
	}
	utc := t.UTC()
	return &utc, nil
}
