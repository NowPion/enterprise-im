package api

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/middleware"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/ws"
)

func wsHandler(cfg *config.Config, hub *ws.Hub) gin.HandlerFunc {
	upgrader := websocket.Upgrader{HandshakeTimeout: cfg.WS.WriteTimeout, CheckOrigin: func(req *http.Request) bool {
		origin := req.Header.Get("Origin")
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Host == "" {
			return false
		}
		return strings.EqualFold(parsed.Host, req.Host) || slices.Contains(cfg.CORS.AllowedOrigins, origin) || slices.Contains(cfg.CORS.AllowedOrigins, "*")
	}}
	return func(c *gin.Context) {
		if !websocket.IsWebSocketUpgrade(c.Request) {
			errcode.FailWithStatus(c, http.StatusBadRequest, errcode.ErrWSInvalidEnvelope.WithMessage("websocket upgrade required"), nil)
			return
		}
		token := c.Query("access_token")
		if header := c.GetHeader("Authorization"); header != "" {
			token, _ = middleware.BearerToken(header)
		}
		identity, failure := hub.Authenticate(c.Request.Context(), token)
		reqID := errcode.GetRequestID(c)
		conn, err := upgrader.Upgrade(c.Writer, c.Request, http.Header{errcode.HeaderRequestID: []string{reqID}})
		if err != nil {
			return
		}
		if failure == nil {
			errcode.SetUserID(c, strconv.FormatUint(identity.UserID, 10))
		} else if failure.HTTPStatus >= http.StatusInternalServerError {
			c.Set(errcode.ContextKeyBizCode, errcode.ErrWSInternalError.Code)
		} else {
			c.Set(errcode.ContextKeyBizCode, errcode.ErrWSAuthFailed.Code)
		}
		hub.Serve(c.Request.Context(), conn, identity, reqID, failure)
	}
}
