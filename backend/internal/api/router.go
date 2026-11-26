package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/ws"
)

func RegisterWSRoute(router gin.IRouter, cfg *config.Config, hub *ws.Hub) {
	router.GET("/ws", wsHandler(cfg, hub))
}

// Stage4Dependencies holds the shared JWT authentication middleware and Stage 4 controllers.
type Stage4Dependencies struct {
	JWTAuth           gin.HandlerFunc
	AuthController    *controller.AuthController
	UserController    *controller.UserController
	FriendRequestCtrl *controller.FriendRequestController
	FriendController  *controller.FriendController
}

// Stage6ChatDependencies holds the shared JWT authentication middleware and Stage 6 chat controllers.
type Stage6ChatDependencies struct {
	JWTAuth                gin.HandlerFunc
	ConversationController *controller.ConversationController
	MessageController      *controller.MessageController
}

// NewRouter builds the Gin engine with global middlewares and /healthz route.
// Stage 4 routes can be mounted via RegisterStage4Routes, NewRouterWithStage4, or per-module Register*Routes helpers.
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

// RegisterStage4Routes mounts auth, user, friend-request, and friend routes onto r
// using a single shared JWT authentication middleware.
func RegisterStage4Routes(r gin.IRouter, deps Stage4Dependencies) {
	if r == nil {
		return
	}
	jwtAuth := deps.JWTAuth
	if jwtAuth == nil {
		jwtAuth = func(c *gin.Context) {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
		}
	}

	RegisterAuthRoutes(r, deps.AuthController, jwtAuth)
	RegisterUserRoutes(r, jwtAuth, deps.UserController)
	RegisterFriendRequestRoutes(r, jwtAuth, deps.FriendRequestCtrl)
	RegisterFriendRoutes(r, jwtAuth, deps.FriendController)
}

// NewRouterWithStage4 builds the Gin engine with global middlewares, /healthz,
// and all Stage 4 REST routes wired to the shared JWT middleware.
func NewRouterWithStage4(
	cfg *config.Config,
	log *slog.Logger,
	healthCtrl *controller.HealthController,
	deps Stage4Dependencies,
) *gin.Engine {
	r := NewRouter(cfg, log, healthCtrl)
	RegisterStage4Routes(r, deps)
	return r
}

// RegisterStage6ChatRoutes mounts Stage 6 conversation and message routes onto r
// using the shared JWT authentication middleware.
func RegisterStage6ChatRoutes(r gin.IRouter, deps Stage6ChatDependencies) {
	if r == nil {
		return
	}
	RegisterConversationRoutes(r, deps.JWTAuth, deps.ConversationController, deps.MessageController)
}
