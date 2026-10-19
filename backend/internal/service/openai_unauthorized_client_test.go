package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const unauthorizedClientFixture = `{"error":{"message":"unauthorized client detected, contact support for assistance"},"message":"UNAUTHENTICATED","success":false,"type":"unauthorized_client_error"}`

func TestUnauthorizedClientRequiresStructuredSignature(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"observed", 401, unauthorizedClientFixture, true},
		{"wrong_status", 403, unauthorizedClientFixture, false},
		{"missing_type", 401, `{"error":{"message":"unauthorized client detected, contact support"},"message":"UNAUTHENTICATED"}`, false},
		{"wrong_type", 401, strings.Replace(unauthorizedClientFixture, "unauthorized_client_error", "invalid_api_key", 1), false},
		{"wrong_root_message", 401, strings.Replace(unauthorizedClientFixture, "UNAUTHENTICATED", "TOKEN_REVOKED", 1), false},
		{"quoted_message", 401, strings.Replace(unauthorizedClientFixture, "unauthorized client detected,", "User quoted unauthorized client detected,", 1), false},
		{"echo_only", 401, `{"echo":` + unauthorizedClientFixture + `}`, false},
		{"plain_text", 401, `unauthorized client detected, contact support`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isOpenAIUnauthorizedClientError(tc.status, []byte(tc.body)))
		})
	}
}

func TestUnauthorizedClientForwardPreservesAccountAndStopsRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(unauthorizedClientFixture))}}
			repo := &openAIAuthPolicyAccountRepo{}
			rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, rateLimitService: rateLimits}
			rateLimits.SetAccountRuntimeBlocker(svc)
			account := &Account{ID: 161, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "https://api.example.test", "pool_mode": true, "pool_mode_retry_status_codes": []any{float64(401)}},
				Extra:       map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true}}
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`))
			require.Nil(t, result)
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, OpenAIUnauthorizedClientReason, failure.Reason)
			require.Equal(t, GatewayFailureScopeRequest, failure.Scope)
			require.False(t, failure.ShouldRetryNextAccount())
			require.False(t, failure.RetryableOnSameAccount)
			require.False(t, failure.ShouldReportAccountScheduleFailure())
			require.Equal(t, http.StatusBadRequest, failure.ClientStatusCode)
			require.False(t, c.Writer.Written())
			require.Zero(t, repo.setErrorCalls)
			require.Zero(t, repo.tempCalls)
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
		})
	}
}

func TestUnauthorizedClientDoesNotRelaxOtherAuthenticationFailures(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"invalid_api_key","message":"Invalid API key"}}`, `{"error":{"code":"token_revoked","message":"Token revoked"}}`, `{"error":{"message":"Unauthorized"}}`} {
		repo := &openAIAuthPolicyAccountRepo{}
		rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
		svc := &OpenAIGatewayService{rateLimitService: rateLimits}
		rateLimits.SetAccountRuntimeBlocker(svc)
		account := &Account{ID: 162, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
		require.True(t, svc.handleOpenAIAccountUpstreamError(context.Background(), account, 401, nil, []byte(body)))
		require.Equal(t, 1, repo.setErrorCalls)
		require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
		failure := svc.newOpenAIAccountFailoverError(account, 401, nil, []byte(body), "", true, false)
		require.NotEqual(t, OpenAIUnauthorizedClientReason, failure.Reason)
		require.True(t, failure.ShouldRetryNextAccount())
	}
	for _, account := range []*Account{nil, {Platform: PlatformGrok, Type: AccountTypeAPIKey}, {Platform: PlatformOpenAI, Type: AccountTypeOAuth}} {
		failure := (&OpenAIGatewayService{}).newOpenAIAccountFailoverError(account, 401, nil, []byte(unauthorizedClientFixture), "", false, true)
		require.NotEqual(t, OpenAIUnauthorizedClientReason, failure.Reason)
	}
}
