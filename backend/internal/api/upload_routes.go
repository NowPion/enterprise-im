package api

import (
	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/pkg/errcode"
)

// RegisterUploadRoutes registers Stage 6 upload, metadata, download, deletion, and avatar
// REST endpoints under /api/v1, protected by jwtAuth (failing closed if jwtAuth is nil).
func RegisterUploadRoutes(r gin.IRouter, jwtAuth gin.HandlerFunc, uploadCtrl *controller.UploadController) {
	if r == nil || uploadCtrl == nil {
		return
	}
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	v1 := r.Group("/api/v1")
	v1.Use(jwtAuth)

	v1.POST("/uploads", uploadCtrl.Upload)
	v1.GET("/uploads/:asset_id", uploadCtrl.GetAsset)
	v1.GET("/uploads/:asset_id/download", uploadCtrl.DownloadAsset)
	v1.DELETE("/uploads/:asset_id", uploadCtrl.DeleteAsset)
	v1.POST("/me/avatar", uploadCtrl.UploadMyAvatar)
}
