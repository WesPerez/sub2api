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
)

const unavailableServiceEvent = `{"code":"bad_response","message":"上游服务暂时不可用，unavailable service","param":null,"type":"error"}`

func TestOpenAIUnavailableServiceErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          bool
	}{
		{"observed service outage", unavailableServiceEvent, true},
		{"English service outage", `{"type":"error","code":"bad_response","message":"unavailable service"}`, true},
		{"normalized outage", `{"type":"error","code":" BAD_RESPONSE ","message":" Unavailable Service "}`, true},
		{"unrelated bad response", `{"type":"error","code":"bad_response","message":"Malformed response"}`, false},
		{"message alone", `{"type":"error","code":"invalid_request","message":"unavailable service"}`, false},
		{"nested rejection wins", `{"type":"error","code":"bad_response","message":"unavailable service","error":{"code":"invalid_request","message":"Invalid input"}}`, false},
		{"nested rejection type wins", `{"type":"error","code":"bad_response","message":"unavailable service","error":{"type":"invalid_request_error"}}`, false},
		{"explicit permission denial wins", `{"type":"error","code":"bad_response","message":"unavailable service","status_code":403}`, false},
		{"context rejection wins", `{"type":"error","code":"bad_response","message":"unavailable service","response":{"error":{"code":"context_length_exceeded","message":"Input exceeds context window"}}}`, false},
		{"non-error frame", `{"type":"response.created","code":"bad_response","message":"unavailable service"}`, false},
		{"policy denial", `{"type":"error","code":"bad_response","message":"Request blocked by policy: unavailable service"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			require.Equal(t, tc.want, isOpenAIStreamUnavailableServiceError(payload))
			require.Equal(t, tc.want, openAIStreamErrorEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
		})
	}
}

func TestOpenAIUnavailableServiceRespectsOutputBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough"} {
		for _, tc := range []struct {
			name, output string
			heartbeat    bool
			wantFailover bool
		}{
			{name: "before output", wantFailover: true},
			{name: "after keepalive", heartbeat: true, wantFailover: true},
			{name: "after text", output: `{"type":"response.output_text.delta","delta":"partial"}`},
			{name: "after tool", output: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}}`},
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
				stream := "data: " + `{"type":"response.created","response":{"id":"resp_outage","status":"in_progress","output":[]}}` + "\n\n"
				if tc.output != "" {
					stream += "data: " + tc.output + "\n\n"
				}
				stream += "event: error\ndata: " + unavailableServiceEvent + "\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"rid-outage"}}, Body: io.NopCloser(strings.NewReader(stream))}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
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
					require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
					require.False(t, failoverErr.RetryableOnSameAccount)
					require.False(t, failoverErr.IsCredentialFailure())
					require.NotContains(t, rec.Body.String(), "response.created")
					require.NotContains(t, rec.Body.String(), "bad_response")
					ops, exists := c.Get(OpsUpstreamErrorsKey)
					require.True(t, exists)
					events, ok := ops.([]*OpsUpstreamErrorEvent)
					require.True(t, ok)
					require.Len(t, events, 1)
					require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
					require.Equal(t, "rid-outage", events[0].UpstreamRequestID)
				} else {
					require.Contains(t, rec.Body.String(), tc.output)
					require.Contains(t, rec.Body.String(), "bad_response")
				}
			})
		}
	}
}
