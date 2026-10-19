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

const noCapacityMessage = "The system is currently experiencing high demand and cannot process your request. Your request exceeds the maximum usage size allowed during peak load. For improved capacity reliability, consider switching to Provisioned Throughput."

const noCapacityErrorEvent = `{"type":"error","error":{"type":"too_many_requests","code":"no_capacity","headers":{"x-ms-fe-error":"true"},"message":"` + noCapacityMessage + `","param":null},"sequence_number":1}`

func TestOpenAINoCapacityClassification(t *testing.T) {
	for _, payload := range []string{
		noCapacityErrorEvent,
		`{"type":"error","code":"no_capacity","message":"capacity unavailable"}`,
		`{"type":"response.failed","response":{"error":{"code":"no_capacity","message":"capacity unavailable"}}}`,
	} {
		message := extractOpenAISSEErrorMessage([]byte(payload))
		require.True(t, isOpenAIUpstreamCapacityShedEvent([]byte(payload)))
		require.Equal(t, http.StatusServiceUnavailable, openAIStreamFailedEventSemanticStatus([]byte(payload), message))
		require.True(t, openAIStreamErrorEventShouldFailover([]byte(payload), message))
		require.True(t, openAIStreamFailedEventShouldFailover([]byte(payload), message))
		// Codex already retries this code. Preserve it when a committed stream
		// cannot be replayed, rather than disguising the upstream diagnosis.
		out, changed := sanitizeOpenAICapacityShedErrorCodeForClient([]byte(payload))
		require.False(t, changed)
		require.Equal(t, payload, string(out))
	}
	// Echoing a machine code in arbitrary text is not a capacity verdict.
	negative := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"invalid no_capacity field"}}`)
	require.False(t, isOpenAIUpstreamCapacityShedEvent(negative))
	require.False(t, openAIStreamErrorEventShouldFailover(negative, extractOpenAISSEErrorMessage(negative)))
}

func TestOpenAINoCapacityStreamReplayBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, semanticStarted := range []bool{false, true} {
			name := "native"
			if passthrough {
				name = "passthrough"
			}
			if semanticStarted {
				name += "/after_output"
			} else {
				name += "/before_output"
			}
			t.Run(name, func(t *testing.T) {
				stream := "data: " + `{"type":"response.created","response":{"id":"resp_capacity","metadata":{"padding":"` + strings.Repeat("x", 40*1024) + `"}}}` + "\n\n"
				if semanticStarted {
					stream += "data: " + `{"type":"response.output_text.delta","delta":"already visible"}` + "\n\n"
				}
				stream += "data: " + noCapacityErrorEvent + "\n\n"
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
				var err error
				if passthrough {
					_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
				} else {
					_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
				}
				require.Error(t, err)
				var failoverErr *UpstreamFailoverError
				if semanticStarted {
					require.False(t, errors.As(err, &failoverErr))
					require.Contains(t, rec.Body.String(), "already visible")
					require.Contains(t, rec.Body.String(), "no_capacity")
					require.Contains(t, rec.Body.String(), noCapacityMessage)
					return
				}
				require.ErrorAs(t, err, &failoverErr)
				require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
				require.Equal(t, http.StatusServiceUnavailable, failoverErr.ClientStatusCode)
				require.Equal(t, noCapacityMessage, failoverErr.ClientMessage)
				require.True(t, failoverErr.RetryableOnSameAccount)
				require.True(t, failoverErr.RequestScopedTransient)
				require.True(t, failoverErr.IsOpenAICapacityShed())
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String())
				repo := &capacityShedAccountRepoStub{}
				(&GatewayService{accountRepo: repo}).TempUnscheduleRetryableError(c.Request.Context(), account.ID, failoverErr)
				require.Zero(t, repo.tempUnschedCalls)
			})
		}
	}
}
