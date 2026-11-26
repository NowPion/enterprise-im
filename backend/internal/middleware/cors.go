package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
)

// CORS applies origin allowlist headers and handles preflight OPTIONS requests.
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowAll := false
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAll = true
			continue
		}
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		c.Header("Vary", "Origin")

		if origin != "" {
			if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
				setCommonCORSHeaders(c)
			} else if allowAll {
				c.Header("Access-Control-Allow-Origin", "*")
				setCommonCORSHeaders(c)
			}
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func setCommonCORSHeaders(c *gin.Context) {
	c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
	c.Header("Access-Control-Expose-Headers", errcode.HeaderRequestID)
	c.Header("Access-Control-Max-Age", "86400")
}

// MaxBodySize rejects requests exceeding maxBytes with frozen code 10002 (HTTP 413).
func MaxBodySize(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 {
			if c.Request.ContentLength > maxBytes {
				errcode.Fail(c, errcode.ErrPayloadTooLarge)
				c.Abort()
				return
			}
			if c.Request.Body != nil {
				c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
			}
		}
		c.Next()
	}
}
