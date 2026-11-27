package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

// RequirePlatformAdmin enforces role == admin on authenticated requests.
func RequirePlatformAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if authCtx, ok := middleware.GetAuthContext(c); ok && authCtx.UserID > 0 {
			if authCtx.Role != model.UserRoleAdmin {
				errcode.Fail(c, errcode.ErrAdminRequired)
				c.Abort()
				return
			}
			c.Next()
			return
		}
		if role, exists := c.Get("role"); exists {
			if s, ok := role.(string); ok && s == model.UserRoleAdmin {
				c.Next()
				return
			}
		}
		errcode.Fail(c, errcode.ErrAdminRequired)
		c.Abort()
	}
}

// RegisterAdminRoutes mounts all platform admin endpoints under /api/v1/admin
// protected by jwtAuth and RequirePlatformAdmin. If jwtAuth is nil, routes fail closed with 11001.
func RegisterAdminRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, adminCtrl *controller.AdminController) {
	if r == nil || adminCtrl == nil {
		return
	}
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	prefix := "/api/v1/admin"
	if bg, ok := r.(interface{ BasePath() string }); ok {
		if strings.HasSuffix(strings.TrimRight(bg.BasePath(), "/"), "/api/v1") {
			prefix = "/admin"
		}
	}

	admin := r.Group(prefix)
	admin.Use(jwtAuth, RequirePlatformAdmin())

	admin.GET("/users", adminCtrl.ListUsers)
	admin.GET("/users/:user_id", adminCtrl.GetUserDetail)
	admin.PATCH("/users/:user_id/status", adminCtrl.UpdateUserStatus)
	admin.POST("/users/:user_id/kick", adminCtrl.KickUser)

	admin.GET("/groups", adminCtrl.ListGroups)
	admin.GET("/groups/:group_id", adminCtrl.GetGroupDetail)
	admin.DELETE("/groups/:group_id", adminCtrl.ForceDisbandGroup)

	admin.GET("/messages/audit", adminCtrl.ListMessageAudit)
	admin.GET("/metrics/overview", adminCtrl.GetMetricsOverview)
	admin.GET("/metrics/streams", adminCtrl.GetMetricsStreams)
	admin.GET("/health/dependencies", adminCtrl.GetDependenciesHealth)
}
