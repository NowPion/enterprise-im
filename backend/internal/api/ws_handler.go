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
		if failure != nil {
			hub.LogHandshakeDenied(c.Request.Context(), reqID, identity.UserID, failure)
			if failure.HTTPStatus >= http.StatusInternalServerError {
				errcode.Fail(c, errcode.ErrWSInternalError)
			} else {
				errcode.Fail(c, errcode.ErrWSAuthFailed.WithMessage(failure.Error()))
			}
			return
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, http.Header{errcode.HeaderRequestID: []string{reqID}})
		if err != nil {
			return
		}
		errcode.SetUserID(c, strconv.FormatUint(identity.UserID, 10))
		hub.Serve(c.Request.Context(), conn, identity, reqID, nil)
	}
}
