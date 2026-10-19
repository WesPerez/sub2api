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

const rejectedSessionBindingBody = `{"error":{"code":"session_binding_invalidated","type":"invalid_request_error","param":"","message":"session binding invalidated: synthetic [trace_id=redacted code=30013]"}}`

func TestSessionBindingInvalidationRequiresExactStructuredConflict(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{409, rejectedSessionBindingBody, true},
		{409, `{"error":{"code":"session_binding_invalidated","type":"other_error"}}`, false},
		{409, `{"error":{"code":"other_error","type":"invalid_request_error","message":"session binding invalidated: [code=30013]"}}`, false},
		{409, `{"input":{"error":{"code":"session_binding_invalidated","type":"invalid_request_error"}}}`, false},
		{409, `{"code":"30013","message":"session binding invalidated"}`, false},
		{409, `{"response":{"error":{"code":"30013"}}}`, false},
		{409, `{"error":{"code":30014,"message":"session binding invalidated"}}`, false},
		{409, `{"error":{"message":"session binding invalidated"},"input":{"code":30013}}`, false},
		{409, `{"error":{"code":"30013-other"}}`, false},
		{409, `session binding invalidated code=30013`, false},
		{400, rejectedSessionBindingBody, false},
		{500, rejectedSessionBindingBody, false},
	} {
		require.Equal(t, tc.want, isOpenAISessionBindingInvalidated(tc.status, []byte(tc.body)), tc.body)
	}
}

func TestSessionBindingConflictFailsOverBeforeOutputWithoutAccountPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusConflict,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(rejectedSessionBindingBody)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{ID: 161, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "https://api.example.test",
					"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(409)}},
				Extra: map[string]any{"openai_passthrough": passthrough, "openai_responses_supported": true},
			}
			result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.2","stream":false,"input":"hello"}`))
			require.Nil(t, result, "a rejected request has no usage result to charge twice")
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, openAISessionBindingInvalidatedReason, failure.Reason)
			require.True(t, failure.ShouldRetryNextAccount())
			require.False(t, failure.RetryableOnSameAccount)
			require.False(t, failure.ShouldReportAccountScheduleFailure())
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
			require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
		})
	}
}

func TestSessionBindingInvalidationExcludesOnlyRejectedSessionAndKeepsNewSticky(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			group := int64(11)
			accounts := []Account{
				{ID: 2290, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
				{ID: 2265, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}, Priority: 10},
			}
			cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session": 2290}}}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
			}
			failure := svc.newOpenAIAccountFailoverError(&accounts[0], 409, nil, []byte(rejectedSessionBindingBody), "", false, true)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			svc.RecordOpenAISessionBindingInvalidation(ctx, &group, "session", "gpt-6-astra", &accounts[0], failure)
			require.NotNil(t, cache.recordCtx)
			require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &group, "other-session", "gpt-6-astra", nil))
			require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &group, "session", "other-model", nil))
			otherGroup := int64(12)
			require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &otherGroup, "session", "gpt-6-astra", nil))
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &group, "", "session", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, int64(2265), selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			require.Equal(t, int64(2265), cache.sessionBindings["openai:session"])
			svc.RecordOpenAISessionBindingInvalidation(context.Background(), &group, "session", "gpt-6-astra", &accounts[0], failure)
			require.Equal(t, int64(2265), cache.sessionBindings["openai:session"])
			require.Empty(t, cache.deletedSessions)
		})
	}
}

func TestOtherConflictsDoNotCreateSessionExclusion(t *testing.T) {
	cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{}}
	svc := &OpenAIGatewayService{cache: cache}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	body := []byte(`{"error":{"code":30014,"message":"another conflict"}}`)
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(account, 409, "", body))
	require.False(t, shouldFailoverOpenAIPassthroughResponse(account, 409, body))
	svc.RecordOpenAISessionBindingInvalidation(context.Background(), nil, "session", "model", account, newOpenAIUpstreamFailoverError(409, nil, body, "", false))
	require.Empty(t, cache.records)
}

func TestSessionBindingClassificationRequiresOpenAIAPIKeyAccount(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(rejectedSessionBindingBody)
	require.NotEqual(t, openAISessionBindingInvalidatedReason, newOpenAIUpstreamFailoverError(409, nil, body, "", true).Reason)
	for _, account := range []*Account{nil, {Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, {Platform: PlatformOpenAI, Type: AccountTypeOAuth}} {
		failure := svc.newOpenAIAccountFailoverError(account, 409, nil, body, "", false, true)
		require.NotEqual(t, openAISessionBindingInvalidatedReason, failure.Reason)
	}
}
