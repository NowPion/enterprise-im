package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
)

// NewRouter builds the Gin engine with global middlewares and Stage 3 routes only.
func NewRouter(cfg *config.Config, log *slog.Logger, healthCtrl *controller.HealthController) *gin.Engine {
	if cfg != nil && cfg.App.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	} else {
		gin.SetMode(gin.DebugMode)
	}

	r := gin.New()
	r.HandleMethodNotAllowed = true

	var allowedOrigins []string
	var maxBodyBytes int64
	if cfg != nil {
		allowedOrigins = cfg.CORS.AllowedOrigins
		maxBodyBytes = cfg.Server.MaxBodyBytes
	}

	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLogger(log))
	r.Use(middleware.Recovery(log))
	r.Use(middleware.CORS(allowedOrigins))
	r.Use(middleware.MaxBodySize(maxBodyBytes))

	r.NoRoute(func(c *gin.Context) {
		errcode.FailWithStatus(c, http.StatusNotFound, errcode.ErrInvalidValue.WithMessage("endpoint not found"), nil)
	})
	r.NoMethod(func(c *gin.Context) {
		errcode.FailWithStatus(c, http.StatusMethodNotAllowed, errcode.ErrInvalidValue.WithMessage("method not allowed"), nil)
	})

	if healthCtrl != nil {
		r.GET("/healthz", healthCtrl.Check)
	}

	return r
}
