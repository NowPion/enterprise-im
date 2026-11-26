package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
)

// RegisterGroupRoutes mounts all Stage 6 group management endpoints under /api/v1/groups
// protected by jwtAuth. If jwtAuth is nil, routes fail closed with 11001.
func RegisterGroupRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, groupCtrl *controller.GroupController) {
	if r == nil || groupCtrl == nil {
		return
	}
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	prefix := "/api/v1/groups"
	if bg, ok := r.(interface{ BasePath() string }); ok {
		if strings.HasSuffix(strings.TrimRight(bg.BasePath(), "/"), "/api/v1") {
			prefix = "/groups"
		}
	}

	groups := r.Group(prefix)
	groups.Use(jwtAuth)

	groups.POST("", groupCtrl.Create)
	groups.GET("", groupCtrl.List)
	groups.GET("/:group_id", groupCtrl.Get)
	groups.PATCH("/:group_id", groupCtrl.Update)
	groups.DELETE("/:group_id", groupCtrl.Disband)

	groups.GET("/:group_id/members", groupCtrl.ListMembers)
	groups.POST("/:group_id/members", groupCtrl.AddMembers)
	groups.DELETE("/:group_id/members/:user_id", groupCtrl.RemoveMember)
	// PATCH /:group_id/members/:user_id dispatches /members/me internally to avoid Gin
	// radix-tree wildcard conflicts with /:group_id/members/:user_id/role.
	groups.PATCH("/:group_id/members/:user_id", groupCtrl.PatchMember)
	groups.PATCH("/:group_id/members/:user_id/role", groupCtrl.UpdateMemberRole)

	groups.POST("/:group_id/leave", groupCtrl.Leave)
	groups.POST("/:group_id/transfer", groupCtrl.Transfer)
	groups.GET("/:group_id/audit", groupCtrl.ListAudit)
}
