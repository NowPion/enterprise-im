package controller

import (
	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// HealthController handles unauthenticated readiness/liveness probe endpoints.
type HealthController struct {
	healthSvc *service.HealthService
}

// NewHealthController creates a HealthController backed by HealthService.
func NewHealthController(healthSvc *service.HealthService) *HealthController {
	return &HealthController{healthSvc: healthSvc}
}

// Check handles GET /healthz.
func (h *HealthController) Check(c *gin.Context) {
	report, err := h.healthSvc.Check(c.Request.Context())
	if err != nil {
		errcode.FailWithData(c, err, report)
		return
	}
	errcode.OK(c, report)
}
