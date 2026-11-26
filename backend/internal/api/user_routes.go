package api

import (
	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
)

// RegisterUserRoutes registers Stage 4 current-user and user-search REST endpoints on r
// using the caller-provided JWT authentication middleware jwtAuth.
func RegisterUserRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, userCtrl *controller.UserController) {
	if r == nil || userCtrl == nil {
		return
	}

	v1 := r.Group("/api/v1")
	if jwtAuth != nil {
		v1.Use(jwtAuth)
	}

	v1.GET("/me", userCtrl.GetMe)
	v1.PATCH("/me", userCtrl.PatchMe)
	v1.GET("/users/search", userCtrl.SearchUsers)
}
