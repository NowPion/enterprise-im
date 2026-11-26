package api

import (
	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
)

// RegisterAuthRoutes mounts Stage 4 authentication routes under /api/v1 onto a root router
// (such as *gin.Engine) without modifying Stage 3's router.go.
//
// Public routes:
//   - POST /api/v1/auth/register
//   - POST /api/v1/auth/login
//   - POST /api/v1/auth/refresh
//
// Protected routes (guarded by jwtAuth):
//   - POST /api/v1/auth/logout
//   - PUT  /api/v1/me/password
func RegisterAuthRoutes(r gin.IRouter, authCtrl *controller.AuthController, jwtAuth gin.HandlerFunc) {
	if r == nil || authCtrl == nil {
		return
	}
	v1 := r.Group("/api/v1")
	RegisterAuthV1Routes(v1, authCtrl, jwtAuth)
}

// RegisterAuthV1Routes mounts Stage 4 authentication routes directly onto an existing /api/v1 RouterGroup.
func RegisterAuthV1Routes(v1 gin.IRouter, authCtrl *controller.AuthController, jwtAuth gin.HandlerFunc) {
	if v1 == nil || authCtrl == nil {
		return
	}
	if jwtAuth == nil {
		panic("RegisterAuthRoutes requires a non-nil jwtAuth middleware")
	}

	auth := v1.Group("/auth")
	{
		auth.POST("/register", authCtrl.Register)
		auth.POST("/login", authCtrl.Login)
		auth.POST("/refresh", authCtrl.Refresh)
		auth.POST("/logout", jwtAuth, authCtrl.Logout)
	}
	v1.PUT("/me/password", jwtAuth, authCtrl.ChangePassword)
}
