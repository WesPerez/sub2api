package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestbudget"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Called only during authentication/body preparation, before stream routing.
func AbortResponsesIngressExpired(c *gin.Context) bool {
	if !requestbudget.Expired(c.Request.Context()) {
		return false
	}
	requestbudget.FinishRead(c.Request.Context())
	c.Header("Connection", "close")
	start, _ := c.Request.Context().Value(ctxkey.RequestStartedAt).(time.Time)
	fields := []zap.Field{zap.String("phase", "ingress")}
	if !start.IsZero() {
		fields = append(fields, zap.Int64("elapsed_ms", time.Since(start).Milliseconds()))
	}
	// Delimit and deliver the terminal before logging or middleware cleanup.
	// Flush alone still leaves a chunked response waiting for handler EOF.
	const body = `{"error":{"type":"first_output_timeout","code":"first_output_timeout","message":"Request preparation exceeded the first-output budget. Please retry."}}`
	c.Header("Content-Length", strconv.Itoa(len(body)))
	c.Data(http.StatusGatewayTimeout, "application/json; charset=utf-8", []byte(body))
	c.Writer.Flush()
	c.Abort()
	logger.FromContext(c.Request.Context()).Warn("openai.first_output_retry_budget_exhausted", fields...)
	return true
}

func abortAPIKeyAuthError(c *gin.Context, status int, code, message string) {
	if AbortResponsesIngressExpired(c) {
		return
	}
	AbortWithError(c, status, code, message)
}
