package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The provider's real message contains spaces, so it cannot substitute for
// reading the structured rate_limit_exceeded code from a Responses error.
const topLevelRateLimitEvent = `{"type":"error","sequence_number":1,"code":"rate_limit_exceeded","message":"Your requests to gpt-6-astra for gpt-6-astra in eastus2 have exceeded token rate limit.","param":null}`

func TestOpenAIStreamTopLevelErrorCodeFallback(t *testing.T) {
	for _, tc := range []struct {
		name, payload, code string
		failover            bool
	}{
		{"responses error", topLevelRateLimitEvent, "rate_limit_exceeded", true},
		{"nested code wins", `{"type":"error","code":"rate_limit_exceeded","error":{"code":"invalid_request","type":"invalid_request_error","message":"Invalid input"}}`, "invalid_request", false},
		{"response error wins", `{"type":"error","code":"invalid_request","error":{"code":"invalid_request"},"response":{"error":{"code":"rate_limit_exceeded"}}}`, "rate_limit_exceeded", true},
		{"normalized error code", `{"type":"error","code":" RATE_LIMIT_EXCEEDED ","message":"token rate limit"}`, "rate_limit_exceeded", true},
		{"non-error code is not an error", `{"type":"response.created","code":"rate_limit_exceeded","response":{"output":[]}}`, "", false},
		{"top-level context rejection", `{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}`, "context_length_exceeded", false},
		{"top-level permission rejection", `{"type":"error","code":"permission_denied","message":"Permission denied"}`, "permission_denied", false},
		{"top-level policy rejection", `{"type":"error","code":"cyber_policy","message":"Request blocked by policy"}`, "cyber_policy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			require.Equal(t, tc.code, openAIStreamFailedEventErrorCode(payload))
			require.Equal(t, tc.failover, openAIStreamErrorEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
		})
	}
}

func TestOpenAIStreamTopLevelRateLimitUsesExistingPoolRetryPolicy(t *testing.T) {
	tests := []struct {
		name        string
		credentials map[string]any
		wantRetry   bool
	}{
		{
			name:        "default pool retry codes include 429",
			credentials: map[string]any{"pool_mode": true},
			wantRetry:   true,
		},
		{
			name: "explicit empty retry codes disable same-account retry",
			credentials: map[string]any{
				"pool_mode":                    true,
				"pool_mode_retry_status_codes": []any{},
			},
			wantRetry: false,
		},
		{
			name: "custom retry codes without 429 use account failover",
			credentials: map[string]any{
				"pool_mode":                    true,
				"pool_mode_retry_status_codes": []any{float64(http.StatusUnauthorized), float64(http.StatusForbidden)},
			},
			wantRetry: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: tc.credentials}
			require.Equal(t, tc.wantRetry, openAIStreamFailedEventRetryableOnSameAccount(
				account,
				[]byte(topLevelRateLimitEvent),
				extractOpenAISSEErrorMessage([]byte(topLevelRateLimitEvent)),
			))
		})
	}
}

func TestOpenAIStreamTopLevelRateLimitRespectsOutputBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough"} {
		for _, tc := range []struct {
			name, payload, output string
			wantFailover          bool
			retry429, heartbeat   bool
		}{
			{name: "before output without same-account retry", payload: topLevelRateLimitEvent, wantFailover: true},
			{name: "before output with same-account retry", payload: topLevelRateLimitEvent, wantFailover: true, retry429: true},
			{name: "after heartbeat", payload: topLevelRateLimitEvent, wantFailover: true, retry429: true, heartbeat: true},
			{name: "after text output", payload: topLevelRateLimitEvent, output: `{"type":"response.output_text.delta","delta":"partial output"}`},
			{name: "after tool output", payload: topLevelRateLimitEvent, output: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}}`},
			{name: "invalid request", payload: `{"type":"error","code":"invalid_request","message":"Invalid input","param":null}`},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/responses", nil)
				if tc.heartbeat {
					c.Writer.Header().Set("Content-Type", "text/event-stream")
					n, err := c.Writer.WriteString(":\n\n")
					require.NoError(t, err)
					recordOpenAIStreamKeepaliveBytes(c, n)
					c.Writer.Flush()
				}
				// A large metadata preamble must remain private too; the incident
				// response.created echoed approximately 37 KiB of tool definitions.
				stream := "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_limit","status":"in_progress","output":[],"metadata":{"padding":"` + strings.Repeat("x", 40*1024) + `"}}}` + "\n\n"
				if tc.output != "" {
					stream += "event: " + gjson.Get(tc.output, "type").String() + "\ndata: " + tc.output + "\n\n"
				}
				stream += "event: error\ndata: " + tc.payload + "\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": []string{"rid-top-level-limit"}}, Body: io.NopCloser(strings.NewReader(stream))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
					"pool_mode": true, "pool_mode_retry_status_codes": []any{float64(401), float64(403)},
				}}
				if tc.retry429 {
					account.Credentials["pool_mode_retry_status_codes"] = []any{float64(429)}
				}
				var err error
				if path == "native" {
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
				} else {
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
				}
				require.Error(t, err)
				var failoverErr *UpstreamFailoverError
				require.Equal(t, tc.wantFailover, errors.As(err, &failoverErr))
				if tc.wantFailover {
					require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
					require.Equal(t, tc.retry429, failoverErr.RetryableOnSameAccount)
					require.Equal(t, tc.heartbeat, c.Writer.Written())
					if tc.heartbeat {
						require.Equal(t, ":\n\n", rec.Body.String())
					} else {
						require.Empty(t, rec.Body.String())
					}
					ops, exists := c.Get(OpsUpstreamErrorsKey)
					require.True(t, exists)
					events := ops.([]*OpsUpstreamErrorEvent)
					require.NotEmpty(t, events)
					require.Equal(t, http.StatusTooManyRequests, events[len(events)-1].UpstreamStatusCode)
				} else {
					require.Contains(t, rec.Body.String(), "response.created")
					if tc.output != "" {
						require.Contains(t, rec.Body.String(), gjson.Get(tc.output, "type").String())
						require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
					} else {
						require.Contains(t, rec.Body.String(), "invalid_request")
					}
				}
			})
		}
	}
}

func TestOpenAIStreamTopLevelCapacityCodeSanitization(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		payload := []byte(`{"type":"error","code":"` + strings.ToUpper(code) + `","message":"capacity unavailable"}`)
		updated, changed := sanitizeOpenAICapacityShedErrorCodeForClient(payload)
		require.True(t, changed)
		require.Contains(t, string(updated), `"code":"server_error"`)
	}
	updated, changed := sanitizeOpenAICapacityShedErrorCodeForClient([]byte(topLevelRateLimitEvent))
	require.False(t, changed)
	require.Equal(t, topLevelRateLimitEvent, string(updated))
}
