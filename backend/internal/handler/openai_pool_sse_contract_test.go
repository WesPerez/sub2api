package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSetOpenAIPoolExhaustedFailureMetadataHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "rate_limit_error",
			"code":    "rate_limit_exceeded",
			"message": "limited",
		},
	})
	fe := &service.UpstreamFailoverError{
		StatusCode:      http.StatusTooManyRequests,
		ResponseBody:    body,
		ResponseHeaders: http.Header{"X-Request-Id": []string{"req-abc-1"}},
	}
	setOpenAIPoolExhaustedFailureMetadata(c, fe, false)

	require.Equal(t, "pool", rec.Header().Get("X-Sub2-Failure-Scope"))
	require.Equal(t, "false", rec.Header().Get("X-Sub2-Retry-Safe"))
	require.Equal(t, "false", rec.Header().Get("X-Sub2-Semantic-Started"))
	require.Equal(t, "429", rec.Header().Get("X-Sub2-Original-Status"))
	require.Equal(t, "pool_exhausted", rec.Header().Get("X-Sub2-Error-Code"))
	require.Equal(t, "req-abc-1", rec.Header().Get("X-Sub2-Request-ID"))
}

func TestSetOpenAIPoolExhaustedFailureMetadataSkipsAfterSemanticStart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// Simulate committed/written stream
	_, _ = rec.WriteString("data: already\n\n")
	c.Writer.WriteHeaderNow()

	setOpenAIPoolExhaustedFailureMetadata(c, &service.UpstreamFailoverError{StatusCode: 502}, true)
	require.Empty(t, rec.Header().Get("X-Sub2-Failure-Scope"))
}

func TestOpenAIPoolAttemptAuditDoesNotLeakCredentials(t *testing.T) {
	audit := &openAIPoolAttemptAudit{}
	account7 := &service.Account{ID: 7, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}}
	account8 := &service.Account{ID: 8, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true}}
	observeOpenAIPoolAccount(audit, 7, 2)
	recordOpenAIPoolForwardAttempt(audit, account7, 1, &service.UpstreamFailoverError{
		StatusCode:             429,
		RetryableOnSameAccount: true,
		ResponseBody:           []byte(`{"error":{"code":"rate_limit_exceeded","message":"secret token sk-abc"}}`),
	})
	recordOpenAIPoolForwardAttempt(audit, account7, 2, &service.UpstreamFailoverError{StatusCode: 429})
	recordOpenAIPoolAccountSwitch(audit, 7)
	observeOpenAIPoolAccount(audit, 8, 2)
	recordOpenAIPoolForwardAttempt(audit, account8, 1, &service.UpstreamFailoverError{StatusCode: 502})
	require.Equal(t, []int64{7, 8}, audit.SelectedAccountIDs)
	require.Equal(t, 2, audit.EligibleAccountCount)
	require.Len(t, audit.Attempts, 3)
	// only sanitized code field is stored, never raw body/credentials
	require.Equal(t, "rate_limit_exceeded", audit.Attempts[0].ErrorCode)
	raw, err := json.Marshal(audit)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sk-abc")
	require.NotContains(t, string(raw), "secret token")
	finalizeOpenAIPoolAttemptAudit(audit, &service.UpstreamFailoverError{StatusCode: 502}, true)
	require.Equal(t, "scheduler_no_eligible_account", audit.ExhaustionReason)
}

func TestSanitizeOpenAIFailureCode(t *testing.T) {
	require.Equal(t, "rate_limit_exceeded", sanitizeOpenAIFailureCode("rate_limit_exceeded"))
	require.Equal(t, "upstream_error", sanitizeOpenAIFailureCode("!!!"))
	require.Equal(t, "a_b-1", sanitizeOpenAIFailureCode("a_b-1;drop"))
}
