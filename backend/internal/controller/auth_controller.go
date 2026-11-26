package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// AuthController handles Stage 4 authentication and password HTTP endpoints.
type AuthController struct {
	authSvc *service.AuthService
}

// NewAuthController creates an AuthController backed by authSvc.
func NewAuthController(authSvc *service.AuthService) *AuthController {
	return &AuthController{authSvc: authSvc}
}

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

type loginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	DeviceID   string `json:"device_id"`
	DeviceType string `json:"device_type"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	DeviceID     string `json:"device_id"`
}

type logoutRequest struct {
	Scope string `json:"scope"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// Register handles POST /api/v1/auth/register.
func (ctrl *AuthController) Register(c *gin.Context) {
	var req registerRequest
	if !bindJSONBody(c, &req, false) {
		return
	}
	out, bizErr := ctrl.authSvc.Register(c.Request.Context(), service.RegisterInput{
		Username: req.Username,
		Password: req.Password,
		Nickname: req.Nickname,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.Created(c, out)
}

// Login handles POST /api/v1/auth/login.
func (ctrl *AuthController) Login(c *gin.Context) {
	var req loginRequest
	if !bindJSONBody(c, &req, false) {
		return
	}
	out, bizErr := ctrl.authSvc.Login(c.Request.Context(), service.LoginInput{
		Username:   req.Username,
		Password:   req.Password,
		DeviceID:   req.DeviceID,
		DeviceType: req.DeviceType,
		IPAddress:  c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, out)
}

// Refresh handles POST /api/v1/auth/refresh.
func (ctrl *AuthController) Refresh(c *gin.Context) {
	var req refreshRequest
	if !bindJSONBody(c, &req, false) {
		return
	}
	out, bizErr := ctrl.authSvc.Refresh(c.Request.Context(), service.RefreshInput{
		RefreshToken: req.RefreshToken,
		DeviceID:     req.DeviceID,
		IPAddress:    c.ClientIP(),
		UserAgent:    c.Request.UserAgent(),
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, out)
}

// Logout handles POST /api/v1/auth/logout.
func (ctrl *AuthController) Logout(c *gin.Context) {
	authCtx, ok := middleware.GetAuthContext(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}
	var req logoutRequest
	if !bindJSONBody(c, &req, true) {
		return
	}
	out, bizErr := ctrl.authSvc.Logout(c.Request.Context(), service.LogoutInput{
		UserID:     authCtx.UserID,
		SessionID:  authCtx.SessionID,
		CurrentJTI: authCtx.JTI,
		Scope:      req.Scope,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, out)
}

// ChangePassword handles PUT /api/v1/me/password.
func (ctrl *AuthController) ChangePassword(c *gin.Context) {
	authCtx, ok := middleware.GetAuthContext(c)
	if !ok {
		errcode.Fail(c, errcode.ErrAccessTokenInvalid)
		return
	}
	var req changePasswordRequest
	if !bindJSONBody(c, &req, false) {
		return
	}
	out, bizErr := ctrl.authSvc.ChangePassword(c.Request.Context(), service.ChangePasswordInput{
		UserID:      authCtx.UserID,
		CurrentJTI:  authCtx.JTI,
		OldPassword: req.OldPassword,
		NewPassword: req.NewPassword,
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, out)
}

func bindJSONBody(c *gin.Context, dst any, allowEmpty bool) bool {
	if c.Request == nil || c.Request.Body == nil {
		if allowEmpty {
			return true
		}
		errcode.Fail(c, errcode.ErrInvalidParams)
		return false
	}

	if err := c.ShouldBindJSON(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			errcode.Fail(c, errcode.ErrPayloadTooLarge)
			return false
		}
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.EOF) {
			errcode.Fail(c, errcode.ErrInvalidParams)
			return false
		}
		errcode.Fail(c, errcode.ErrInvalidParams)
		return false
	}
	return true
}
