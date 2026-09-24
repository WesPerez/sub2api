package admin

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type AgentRouterRecoveryHandler struct {
	svc *service.AgentRouterRecoveryService
}

func NewAgentRouterRecoveryHandler(svc *service.AgentRouterRecoveryService) *AgentRouterRecoveryHandler {
	return &AgentRouterRecoveryHandler{svc}
}

func recoveryError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrRecoveryConflict) || errors.Is(err, service.ErrRecoveryBusy) {
		response.Error(c, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, service.ErrRecoveryInvalid) {
		response.BadRequest(c, err.Error())
		return
	}
	// Repository errors may contain storage details. Keep them off the web UI.
	response.Error(c, http.StatusInternalServerError, "账号恢复服务暂时不可用，请稍后重试")
}
func (h *AgentRouterRecoveryHandler) Config(c *gin.Context) {
	value, err := h.svc.Config(c.Request.Context())
	if err != nil {
		recoveryError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AgentRouterRecoveryHandler) Preview(c *gin.Context) {
	var p service.RecoveryPolicy
	if err := c.ShouldBindJSON(&p); err != nil {
		response.BadRequest(c, "配置格式无效")
		return
	}
	value, err := h.svc.Preview(c.Request.Context(), p)
	if err != nil {
		recoveryError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AgentRouterRecoveryHandler) Save(c *gin.Context) {
	var input struct {
		Policy          service.RecoveryPolicy `json:"policy"`
		Revision        string                 `json:"revision"`
		PreviewRevision string                 `json:"preview_revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "配置格式无效")
		return
	}
	value, err := h.svc.Save(c.Request.Context(), input.Policy, input.Revision, input.PreviewRevision)
	if err != nil {
		recoveryError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AgentRouterRecoveryHandler) Run(c *gin.Context) {
	var input struct {
		RequestID       string `json:"request_id"`
		PreviewRevision string `json:"preview_revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "执行请求格式无效")
		return
	}
	value, err := h.svc.Run(c.Request.Context(), nil, input.RequestID, input.PreviewRevision)
	if err != nil && value == nil {
		recoveryError(c, err)
		return
	}
	response.Success(c, value)
}
func (h *AgentRouterRecoveryHandler) History(c *gin.Context) {
	value, err := h.svc.History(c.Request.Context())
	if err != nil {
		recoveryError(c, err)
		return
	}
	response.Success(c, value)
}
