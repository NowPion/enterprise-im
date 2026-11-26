package api

import (
	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
)

// RegisterFriendRoutes mounts the Stage 4 friend endpoints (/api/v1/friends*) onto r.
// The JWT middleware is supplied by the caller; if nil, endpoints fail closed with 11001.
func RegisterFriendRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, friendCtrl *controller.FriendController) {
	if r == nil || friendCtrl == nil {
		return
	}
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	friends := r.Group("/api/v1/friends")
	friends.Use(jwtAuth)
	friends.GET("", friendCtrl.List)
	friends.PATCH("/:friend_id", friendCtrl.UpdateRemark)
	friends.DELETE("/:friend_id", friendCtrl.Delete)
}
