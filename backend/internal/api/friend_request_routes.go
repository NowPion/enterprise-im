package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
)

// RegisterFriendRequestRoutes registers the Stage 4 friend request endpoints under /api/v1/friend-requests
// protected by the caller-provided jwtAuth middleware.
func RegisterFriendRequestRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, ctrl *controller.FriendRequestController) {
	if r == nil || ctrl == nil {
		return
	}

	prefix := "/api/v1/friend-requests"
	if bg, ok := r.(interface{ BasePath() string }); ok {
		if strings.HasSuffix(strings.TrimRight(bg.BasePath(), "/"), "/api/v1") {
			prefix = "/friend-requests"
		}
	}

	group := r.Group(prefix)
	if jwtAuth != nil {
		group.Use(jwtAuth)
	}
	group.POST("", ctrl.Create)
	group.GET("", ctrl.List)
	group.POST("/:id/accept", ctrl.Accept)
	group.POST("/:id/reject", ctrl.Reject)
	group.POST("/:id/cancel", ctrl.Cancel)
}
