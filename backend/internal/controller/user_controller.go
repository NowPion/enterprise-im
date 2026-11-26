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

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

const (
	ContextKeySessionID = "session_id"
	ContextKeyDeviceID  = "device_id"
)

// UserController handles authenticated current-user and user-search HTTP endpoints.
type UserController struct {
	userSvc *service.UserService
}

// NewUserController creates a UserController backed by UserService.
func NewUserController(userSvc *service.UserService) *UserController {
	return &UserController{userSvc: userSvc}
}

type optionalString struct {
	Set   bool
	Valid bool
	Value string
}

func (o *optionalString) UnmarshalJSON(data []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		o.Valid = false
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	o.Valid = true
	o.Value = s
	return nil
}

type patchMeRequest struct {
	Nickname  optionalString `json:"nickname"`
	Signature optionalString `json:"signature"`
}

// GetMe handles GET /api/v1/me.
func (h *UserController) GetMe(c *gin.Context) {
	if h == nil || h.userSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, sessionID, deviceID, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	res, bizErr := h.userSvc.GetMe(c.Request.Context(), userID, sessionID, deviceID)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// PatchMe handles PATCH /api/v1/me.
func (h *UserController) PatchMe(c *gin.Context) {
	if h == nil || h.userSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	req, bindErr := parsePatchMeRequest(c)
	if bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}

	input := service.UpdateMeInput{UserID: userID}
	if req.Nickname.Set {
		v := req.Nickname.Value
		input.Nickname = &v
	}
	if req.Signature.Set {
		v := req.Signature.Value
		input.Signature = &v
	}

	res, bizErr := h.userSvc.UpdateMe(c.Request.Context(), input)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// SearchUsers handles GET /api/v1/users/search.
func (h *UserController) SearchUsers(c *gin.Context) {
	if h == nil || h.userSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	limit := 0
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		trimmed := strings.TrimSpace(rawLimit)
		if trimmed != "" {
			parsed, err := strconv.Atoi(trimmed)
			if err != nil || parsed < 1 || parsed > service.MaxSearchLimit {
				errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
				return
			}
			limit = parsed
		}
	}

	page, bizErr := h.userSvc.SearchUsers(c.Request.Context(), service.SearchUsersInput{
		ViewerUserID: userID,
		Query:        c.Query("q"),
		Field:        c.Query("field"),
		Match:        c.Query("match"),
		Cursor:       c.Query("cursor"),
		Limit:        limit,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, page)
}

func parsePatchMeRequest(c *gin.Context) (patchMeRequest, *errcode.Error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return patchMeRequest{}, errcode.ErrInvalidParams
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return patchMeRequest{}, errcode.ErrPayloadTooLarge
		}
		return patchMeRequest{}, errcode.ErrInvalidParams
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return patchMeRequest{}, errcode.ErrInvalidParams.WithMessage("request body must not be empty")
	}

	var req patchMeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return patchMeRequest{}, errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	if !req.Nickname.Set && !req.Signature.Set {
		return patchMeRequest{}, errcode.ErrInvalidParams.WithMessage("at least one of nickname or signature must be provided")
	}
	if req.Nickname.Set && !req.Nickname.Valid {
		return patchMeRequest{}, errcode.ErrInvalidParams.WithMessage("nickname must be a string")
	}
	if req.Signature.Set && !req.Signature.Valid {
		return patchMeRequest{}, errcode.ErrInvalidParams.WithMessage("signature must be a string")
	}
	return req, nil
}

func extractAuthIdentity(c *gin.Context) (uint64, string, string, *errcode.Error) {
	if c == nil {
		return 0, "", "", errcode.ErrAccessTokenInvalid
	}
	userID, ok := readContextUint64(c, errcode.ContextKeyUserID)
	if !ok || userID == 0 {
		return 0, "", "", errcode.ErrAccessTokenInvalid
	}
	if errcode.GetUserID(c) == "" {
		errcode.SetUserID(c, strconv.FormatUint(userID, 10))
	}

	sessionID := readContextIDString(c, ContextKeySessionID)
	deviceID := readContextString(c, ContextKeyDeviceID)
	return userID, sessionID, deviceID, nil
}

func readContextUint64(c *gin.Context, key string) (uint64, bool) {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case uint64:
		return val, true
	case int64:
		if val <= 0 {
			return 0, false
		}
		return uint64(val), true
	case uint:
		return uint64(val), true
	case int:
		if val <= 0 {
			return 0, false
		}
		return uint64(val), true
	default:
		return 0, false
	}
}

func readContextIDString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case uint64:
		return strconv.FormatUint(val, 10)
	case int64:
		if val < 0 {
			return ""
		}
		return strconv.FormatInt(val, 10)
	case uint:
		return strconv.FormatUint(uint64(val), 10)
	case int:
		if val < 0 {
			return ""
		}
		return strconv.Itoa(val)
	default:
		return ""
	}
}

func readContextString(c *gin.Context, key string) string {
	v, exists := c.Get(key)
	if !exists || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
