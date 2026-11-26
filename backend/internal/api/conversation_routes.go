package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
)

// RegisterConversationRoutes mounts Stage 6 conversation and message routes under /api/v1/conversations.
// All endpoints are protected by jwtAuth; if jwtAuth is nil, requests fail closed with 11001.
func RegisterConversationRoutes(
	r gin.IRouter,
	jwtAuth gin.HandlerFunc,
	convCtrl *controller.ConversationController,
	msgCtrl *controller.MessageController,
) {
	if r == nil || (convCtrl == nil && msgCtrl == nil) {
		return
	}
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	prefix := "/api/v1/conversations"
	if bg, ok := r.(interface{ BasePath() string }); ok {
		if strings.HasSuffix(strings.TrimRight(bg.BasePath(), "/"), "/api/v1") {
			prefix = "/conversations"
		}
	}

	group := r.Group(prefix)
	group.Use(jwtAuth)

	if convCtrl != nil {
		group.GET("", convCtrl.List)
		group.GET("/:conversation_id", convCtrl.GetDetail)
		group.PATCH("/:conversation_id/settings", convCtrl.UpdateSettings)
		group.GET("/:conversation_id/members", convCtrl.ListMembers)
	}
	if msgCtrl != nil {
		group.GET("/:conversation_id/messages", msgCtrl.List)
		group.GET("/:conversation_id/sync", msgCtrl.Sync)
		group.POST("/:conversation_id/messages", msgCtrl.Send)
		group.POST("/:conversation_id/messages/:msg_id/recall", msgCtrl.Recall)
	}
}
