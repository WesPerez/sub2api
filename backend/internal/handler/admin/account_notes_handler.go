package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AccountNotesHandler exposes only the fields needed to match balance notes.
// It is protected by the same admin authentication as other admin APIs.
type AccountNotesHandler struct{ svc *service.AccountNotesService }

func NewAccountNotesHandler(svc *service.AccountNotesService) *AccountNotesHandler {
	return &AccountNotesHandler{svc: svc}
}

var accountNotesETag = regexp.MustCompile(`^"[a-f0-9]{64}"$`)

func accountNotesError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrAccountNotesConflict):
		response.Error(c, http.StatusPreconditionFailed, service.ErrAccountNotesConflict.Error())
	case errors.Is(err, service.ErrAccountNotesInvalid):
		response.BadRequest(c, service.ErrAccountNotesInvalid.Error())
	case errors.Is(err, service.ErrIntegrationBalanceInvalid):
		response.BadRequest(c, service.ErrIntegrationBalanceInvalid.Error())
	case errors.Is(err, service.ErrAccountNotFound):
		response.Error(c, http.StatusNotFound, "账号不存在或不支持备注同步")
	default:
		response.Error(c, http.StatusInternalServerError, "备注服务暂时不可用，请稍后重试")
	}
}

func (h *AccountNotesHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	rows, result, err := h.svc.List(c.Request.Context(), pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: "id", SortOrder: pagination.SortOrderAsc})
	if err != nil {
		accountNotesError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Paginated(c, rows, result.Total, page, pageSize)
}

func notesAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 1 {
		response.BadRequest(c, "账号 ID 无效")
		return 0, false
	}
	return id, true
}

func (h *AccountNotesHandler) Get(c *gin.Context) {
	id, ok := notesAccountID(c)
	if !ok {
		return
	}
	value, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		accountNotesError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("ETag", value.Revision)
	response.Success(c, value)
}

func (h *AccountNotesHandler) Update(c *gin.Context) {
	id, ok := notesAccountID(c)
	if !ok {
		return
	}
	revision := c.GetHeader("If-Match")
	if revision == "" {
		response.Error(c, http.StatusPreconditionRequired, "请先读取账号备注，并携带 If-Match 版本")
		return
	}
	if !accountNotesETag.MatchString(revision) {
		response.BadRequest(c, "备注版本格式无效")
		return
	}
	var input struct {
		Notes   *string                          `json:"notes" binding:"required"`
		Balance *service.IntegrationBalanceInput `json:"balance_snapshot"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.Notes == nil {
		response.BadRequest(c, "请提供 notes 字符串；空字符串表示清空")
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		response.BadRequest(c, "请求必须为单个 JSON 对象")
		return
	}
	value, err := h.svc.UpdateWithBalance(c.Request.Context(), id, revision, input.Notes, input.Balance)
	if err != nil {
		accountNotesError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("ETag", value.Revision)
	response.Success(c, value)
}
