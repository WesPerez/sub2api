package service

import (
	"errors"
	"fmt"
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

func newOpenAIPoolSSETestService(t *testing.T) *OpenAIGatewayService {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return &OpenAIGatewayService{
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				StreamDataIntervalTimeout: 0,
				StreamKeepaliveInterval:   0,
				MaxLineSize:               defaultMaxLineSize,
			},
		},
	}
}

func newPoolModeAccount(id int64, retryCount int) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Name:     "pool-acc",
		Credentials: map[string]any{
			"pool_mode":             true,
			"pool_mode_retry_count": float64(retryCount),
		},
	}
}

func runStreamingFixture(t *testing.T, svc *OpenAIGatewayService, account *Account, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"X-Request-Id": []string{"rid-pool-sse"}},
	}
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4")
	return rec, err
}

func runPassthroughStreamingFixture(t *testing.T, svc *OpenAIGatewayService, account *Account, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"X-Request-Id": []string{"rid-pool-sse-passthrough"}},
	}
	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4")
	return rec, err
}

func TestOpenAIStreamingFailedNullPlusTrailingRateLimitReturnsRetryableFailover(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(11, 10)
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"too_many_requests","code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`,
		"",
	}, "\n")

	rec, err := runStreamingFixture(t, svc, account, body)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount, "rate_limit after failed(null) must use same-account pool retry")
	require.Contains(t, string(failoverErr.ResponseBody), "rate_limit_exceeded")
	require.False(t, rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), "response.created"),
		"preamble must remain buffered so the request can be replayed")
	require.False(t, rec.Body.Len() > 0 && strings.Contains(rec.Body.String(), "response.failed"))
}

func TestOpenAIPassthroughStreamingFailedNullPlusTrailingRateLimitReturnsRetryableFailover(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(21, 10)
	account.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusTooManyRequests)}
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"too_many_requests","code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`,
		"",
	}, "\n")

	rec, err := runPassthroughStreamingFixture(t, svc, account, body)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, "rid-pool-sse-passthrough", failoverErr.ResponseHeaders.Get("X-Request-Id"))
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingNonRetryableTrailingErrorWritesOneValidFailedFrame(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(22, 10)
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"invalid_request_error","code":"invalid_prompt","message":"bad request"}}`,
		"",
	}, "\n")

	rec, err := runStreamingFixture(t, svc, account, body)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
	require.NotContains(t, rec.Body.String(), "event: error")
	require.Contains(t, rec.Body.String(), `"code":"invalid_prompt"`)
}

func TestOpenAIStreamingFailedNullTrailingWaitIsTimeBounded(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(23, 10)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		_, _ = io.WriteString(writer, strings.Join([]string{
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
			"",
		}, "\n")+"\n")
	}()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: reader, Header: http.Header{}}
	started := time.Now()
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, started, "gpt-5.4", "gpt-5.4")
	require.Error(t, err)
	require.Less(t, time.Since(started), time.Second)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingKeepaliveDoesNotPinRetryableFailure(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	// 1s ticker; wait for the first keepalive comment via body observation (no fixed sleep race).
	svc.cfg.Gateway.StreamKeepaliveInterval = 1
	account := newPoolModeAccount(24, 10)
	reader, writer := io.Pipe()
	keepaliveSeen := make(chan struct{})
	go func() {
		defer func() { _ = writer.Close() }()
		_, _ = io.WriteString(writer, strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
			"",
		}, "\n")+"\n")
		select {
		case <-keepaliveSeen:
		case <-time.After(3 * time.Second):
			// Still close so the test fails deterministically if keepalive never arrives.
		}
		_, _ = io.WriteString(writer, strings.Join([]string{
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
			"",
			"event: error",
			`data: {"type":"error","error":{"code":"rate_limit_exceeded","message":"limited"}}`,
			"",
		}, "\n"))
	}()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Body: reader, Header: http.Header{}}

	done := make(chan error, 1)
	go func() {
		_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.4", "gpt-5.4")
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.Body.String(), ":\n\n") || time.Now().After(deadline) {
			close(keepaliveSeen)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	err := <-done
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, rec.Body.String(), ":\n\n")
	require.NotContains(t, rec.Body.String(), "response.created")
}

func TestOpenAIStreamingFailedNullWithoutTrailingErrorStillFailovers(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(12, 10)
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":null}}`,
		"",
	}, "\n")
	rec, err := runStreamingFixture(t, svc, account, body)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.True(t, failoverErr.RetryableOnSameAccount || failoverErr.StatusCode >= 500 || failoverErr.StatusCode == http.StatusBadGateway)
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingFailedAfterSemanticOutputDoesNotFailover(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(13, 10)
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","error":{"code":"rate_limit_exceeded","message":"limited"}}}`,
		"",
	}, "\n")
	rec, err := runStreamingFixture(t, svc, account, body)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errorsAsUpstreamFailover(err, &failoverErr), "semantic output must pin the stream; got %v", err)
	require.Contains(t, rec.Body.String(), "response.output_text.delta")
	require.Contains(t, rec.Body.String(), "response.failed")
}

func TestOpenAIStreamingDeterministic4xxDoesNotRetryOnSameAccount(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(14, 10)
	body := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_1","error":{"type":"invalid_request_error","code":"invalid_prompt","message":"bad request"}}}`,
		"",
	}, "\n")
	rec, err := runStreamingFixture(t, svc, account, body)
	require.Error(t, err, "deterministic invalid_request must surface an error")
	require.False(t, openAIStreamFailedEventShouldFailover(
		[]byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"invalid_prompt","message":"bad request"}}}`),
		"bad request",
	))
	var failoverErr *UpstreamFailoverError
	if errorsAsUpstreamFailover(err, &failoverErr) {
		require.False(t, failoverErr.RetryableOnSameAccount, "deterministic 4xx must not burn pool same-account retries")
	} else {
		// Non-failover path writes a client-visible failed frame instead of UpstreamFailoverError.
		require.Contains(t, rec.Body.String(), "invalid_request_error")
		require.Contains(t, rec.Body.String(), "response.failed")
	}
}

func TestOpenAIStreamFailoverRetryableClassifier(t *testing.T) {
	pool := newPoolModeAccount(1, 10)
	payload429 := []byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"limited"}}`)
	capacityPayload := []byte(`{"error":{"type":"invalid_request_error","message":"Selected model is at capacity. Please try a different model."}}`)
	require.True(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusTooManyRequests, "limited", payload429))
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusBadGateway, "upstream", []byte(`{"error":{"message":"temporary"}}`)))
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusBadGateway, "Selected model is at capacity", capacityPayload))
	pool.Credentials["pool_mode_retry_status_codes"] = []any{float64(http.StatusBadGateway)}
	require.True(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusBadGateway, "upstream", []byte(`{"error":{"message":"temporary"}}`)))
	require.True(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusBadGateway, "Selected model is at capacity", capacityPayload),
		"transient capacity must win over invalid_request_error after 502 is explicitly enabled")
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusBadRequest, "bad", []byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`)))
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(pool, http.StatusUnauthorized, "auth", []byte(`{"error":{"type":"authentication_error"}}`)))
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(nil, http.StatusTooManyRequests, "limited", payload429))
	nonPool := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(nonPool, http.StatusTooManyRequests, "limited", payload429))
	emptyConfigured := newPoolModeAccount(3, 10)
	emptyConfigured.Credentials["pool_mode_retry_status_codes"] = []any{}
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(emptyConfigured, http.StatusTooManyRequests, "limited", payload429))
	require.False(t, openAIStreamFailoverRetryableOnSameAccount(
		pool,
		http.StatusBadGateway,
		"billing hard limit reached",
		[]byte(`{"error":{"code":"insufficient_quota","message":"billing hard limit reached"}}`),
	))
}

func TestOpenAIHTTPPoolRetryClassifierSkipsDeterministicFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		body    string
	}{
		{
			name:   "model not found wrapped as 503",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"code":"model_not_found","message":"model not found"}}`,
		},
		{
			name:   "invalid credentials",
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":"invalid_api_key","message":"invalid credentials"}}`,
		},
		{
			name:   "permission denied",
			status: http.StatusForbidden,
			body:   `{"error":{"code":"permission_denied","message":"forbidden"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failoverErr := newOpenAIUpstreamFailoverError(
				tt.status,
				http.Header{},
				[]byte(tt.body),
				tt.message,
				true,
			)
			require.False(t, failoverErr.RetryableOnSameAccount)
		})
	}

	transient := newOpenAIUpstreamFailoverError(
		http.StatusBadGateway,
		http.Header{},
		[]byte(`{"error":{"code":"upstream_unavailable","message":"temporary"}}`),
		"temporary",
		true,
	)
	require.True(t, transient.RetryableOnSameAccount,
		"an explicitly enabled transient 502 must retain the configured same-account budget")
}

func TestOpenAIHTTPPolicyFailureDoesNotFailoverAcrossAccounts(t *testing.T) {
	svc := &OpenAIGatewayService{}
	policyBody := []byte(`{"error":{"code":"sensitive_words_detected","message":"request rejected"}}`)
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(
		http.StatusInternalServerError,
		"request rejected",
		policyBody,
	))

	overloadBody := []byte(`{"error":{"code":"server_is_overloaded","message":"try again"}}`)
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(
		http.StatusServiceUnavailable,
		"try again",
		overloadBody,
	))

	providerRecoveryBody := []byte(`{"error":{"code":"upstream_unavailable","message":"request cancelled while provider recovery was running"}}`)
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(
		http.StatusServiceUnavailable,
		"request cancelled while provider recovery was running",
		providerRecoveryBody,
	), "an incidental cancellation word must not turn a temporary 503 into a request-level rejection")

	requestCancelledBody := []byte(`{"error":{"code":"request_cancelled","message":"request cancelled"}}`)
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(
		http.StatusServiceUnavailable,
		"request cancelled",
		requestCancelledBody,
	), "a structured request cancellation must not be replayed across accounts")

	require.True(t, openAIStreamFailedEventShouldFailover(
		[]byte(`{"error":{"code":"upstream_unavailable","message":"policy backend temporarily unavailable"}}`),
		"policy backend temporarily unavailable",
	), "a generic word such as policy must not suppress pre-semantic SSE failover")
	require.False(t, openAIStreamFailedEventShouldFailover(
		[]byte(`{"response":{"error":{"type":"invalid_request_error","message":"Content policy violation"}}}`),
		"Content policy violation",
	), "a natural-language content policy rejection must not be replayed")
}

func TestMergeOpenAIStreamFailedWithTrailingError(t *testing.T) {
	failed := []byte(`{"type":"response.failed","response":{"id":"resp_1","error":null}}`)
	trailing := []byte(`{"type":"error","error":{"type":"too_many_requests","code":"rate_limit_exceeded","message":"Rate limit exceeded"}}`)
	payload, message, status := mergeOpenAIStreamFailedWithTrailingError(failed, trailing, "")
	require.Equal(t, "Rate limit exceeded", message)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.Contains(t, string(payload), "rate_limit_exceeded")
}

func errorsAsUpstreamFailover(err error, target **UpstreamFailoverError) bool {
	if err == nil {
		return false
	}
	var fe *UpstreamFailoverError
	if errors.As(err, &fe) {
		*target = fe
		return true
	}
	return false
}

func TestOpenAIWireSSEFrameCollectorEnforcesHardLimits(t *testing.T) {
	c := &openAIWireSSEFrameCollector{}
	for i := 0; i < openAIWireSSEFrameMaxLines; i++ {
		_, complete, err := c.AddLine("data: {\"n\":" + itoaMin(i) + "}")
		require.NoError(t, err)
		require.False(t, complete)
	}
	_, _, err := c.AddLine("data: overflow")
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")

	c = &openAIWireSSEFrameCollector{}
	// Many tiny lines without blank terminator must hit the byte budget.
	payload := strings.Repeat("x", 1024)
	for {
		_, _, err := c.AddLine("data: " + payload)
		if err != nil {
			require.Contains(t, err.Error(), "exceeds limit")
			break
		}
	}
}

func TestOpenAIHTTPPoolRetryClassifierHonorsConfiguredStatusForChannelFailure(t *testing.T) {
	account := newPoolModeAccount(17, 10)
	account.Credentials["pool_mode_retry_status_codes"] = []any{http.StatusInternalServerError}
	body := []byte(`{"error":{"code":"get_channel_failed","message":"channel unavailable"}}`)

	failoverErr := newOpenAIUpstreamFailoverError(
		http.StatusInternalServerError,
		http.Header{},
		body,
		"",
		account.IsPoolModeRetryableStatus(http.StatusInternalServerError),
	)

	require.True(t, failoverErr.RetryableOnSameAccount,
		"configured HTTP 500 failures must consume the pool retry budget")

	account.Credentials["pool_mode_retry_status_codes"] = []any{http.StatusBadGateway}
	failoverErr = newOpenAIUpstreamFailoverError(
		http.StatusInternalServerError,
		http.Header{},
		body,
		"",
		account.IsPoolModeRetryableStatus(http.StatusInternalServerError),
	)
	require.False(t, failoverErr.RetryableOnSameAccount,
		"get_channel_failed must not bypass the account's configured status list")
}

func TestOpenAIWireSSEFrameCollectorPostSemanticUsesRaisedByteCap(t *testing.T) {
	c := &openAIWireSSEFrameCollector{}
	// Pre-semantic default rejects a single line above 256KiB.
	large := "data: " + strings.Repeat("y", openAIWireSSEFrameMaxBytes)
	_, _, err := c.AddLine(large)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")

	// After semantic output, MaxLineSize-sized frames must be accepted.
	c = &openAIWireSSEFrameCollector{}
	c.SetMaxBytes(defaultMaxLineSize)
	_, complete, err := c.AddLine(large)
	require.NoError(t, err)
	require.False(t, complete)
	frame, complete, err := c.AddLine("")
	require.NoError(t, err)
	require.True(t, complete)
	require.True(t, frame.HasData)
	require.True(t, c.Pending() == false)
}

func TestOpenAIPassthroughFirstSemanticEventCanExceedDefaultFrameCap(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(32, 10)
	largeDelta := strings.Repeat("p", openAIWireSSEFrameMaxBytes+32*1024)
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"` + largeDelta + `"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_passthrough_large_first","usage":{"input_tokens":2,"output_tokens":1}}}`,
		"",
	}, "\n")

	rec, err := runPassthroughStreamingFixture(t, svc, account, body)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), strings.Repeat("p", 1024))
	require.Contains(t, rec.Body.String(), `"type":"response.completed"`)
}

func itoaMin(v int) string {
	return fmt.Sprintf("%d", v)
}

func TestOpenAIWireSSEFrameCollectorEventOnlyAndMultiDataTerminal(t *testing.T) {
	// multi-data: event line supplies type; multiple data lines are joined for protocol payload.
	c := &openAIWireSSEFrameCollector{}
	_, complete, err := c.AddLine("event: response.failed")
	require.NoError(t, err)
	require.False(t, complete)
	_, complete, err = c.AddLine(`data: {"response":{"id":"r1","status":"failed","error":null}}`)
	require.NoError(t, err)
	require.False(t, complete)
	// Second data line is preserved on the wire and joined into the protocol payload buffer.
	_, complete, err = c.AddLine(`data: {"note":"multi"}`)
	require.NoError(t, err)
	require.False(t, complete)
	frame, complete, err := c.AddLine("")
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, "response.failed", frame.EventType, "normalized type must come from event line when data omits type")
	require.True(t, frame.HasData)
	require.Contains(t, string(frame.Payload), `"error":null`)
	require.Contains(t, string(frame.Payload), `"type":"response.failed"`)
	require.Equal(t, "event: response.failed", frame.Lines[0], "wire lines must stay original")
	require.Equal(t, 4, len(frame.Lines))
	require.Contains(t, strings.Join(frame.Lines, "\n"), "note")

	// event-only error frame (no data lines)
	c = &openAIWireSSEFrameCollector{}
	_, _, err = c.AddLine("event: error")
	require.NoError(t, err)
	frame, complete, err = c.AddLine("")
	require.NoError(t, err)
	require.True(t, complete)
	require.Equal(t, "error", frame.EventType)
	require.False(t, frame.HasData)

	// gate: failed(null) + trailing event:error(data) merges; event-only error still terminates wait by type
	g := &openAITrailingFailureGate{}
	failedPayload := []byte(`{"type":"response.failed","response":{"id":"r1","status":"failed","error":null}}`)
	wait := g.Accept(openAIWireSSEFrame{
		EventType: "response.failed", Payload: failedPayload, HasData: true,
		Lines: []string{"event: response.failed", "data: " + string(failedPayload), ""},
	}, true)
	require.True(t, wait.WaitStarted)
	// event-only trailing error (protocol type only)
	eventOnly := g.Accept(openAIWireSSEFrame{EventType: "error", Lines: []string{"event: error", ""}}, true)
	// Without payload, merge still finishes wait using pending failed as failure path or empty error merge
	require.True(t, eventOnly.WaitFinished || len(eventOnly.Frames) > 0 || len(eventOnly.FailurePayload) > 0)

	g = &openAITrailingFailureGate{}
	wait = g.Accept(openAIWireSSEFrame{
		EventType: "response.failed", Payload: failedPayload, HasData: true,
		Lines: []string{"event: response.failed", "data: " + string(failedPayload), ""},
	}, true)
	require.True(t, wait.WaitStarted)
	merged := g.Accept(openAIWireSSEFrame{
		EventType: "error",
		Payload:   []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"limited"}}`),
		HasData:   true,
		Lines:     []string{"event: error", `data: {"type":"error","error":{"code":"rate_limit_exceeded","message":"limited"}}`, ""},
	}, true)
	require.True(t, merged.WaitFinished)
	// Pre-semantic trailing rate_limit merges must failover (same-account retryable)
	// rather than emit a synthetic failed frame downstream.
	require.Empty(t, merged.Frames)
	require.Contains(t, string(merged.FailurePayload), "rate_limit_exceeded")
	require.Equal(t, "limited", merged.FailureMessage)

	// With waiting disabled, response.failed is forwarded immediately; no trailing
	// merge is attempted because semantic output has already committed the stream.
	g = &openAITrailingFailureGate{}
	wait = g.Accept(openAIWireSSEFrame{
		EventType: "response.failed", Payload: failedPayload, HasData: true,
		Lines: []string{"event: response.failed", "data: " + string(failedPayload), ""},
	}, false)
	require.False(t, wait.WaitStarted)
	require.Len(t, wait.Frames, 1)
}

func TestOpenAIPassthroughPreSemanticBufferIsBounded(t *testing.T) {
	svc := newOpenAIPoolSSETestService(t)
	account := newPoolModeAccount(31, 10)
	frame := strings.Join([]string{
		"event: response.in_progress",
		`data: {"type":"response.in_progress","response":{"id":"resp_buffered","status":"in_progress"},"padding":"` + strings.Repeat("x", 1024) + `"}`,
		"",
	}, "\n") + "\n"
	var body strings.Builder
	for body.Len() <= openAIPassthroughPreSemanticMaxBytes+len(frame) {
		_, _ = body.WriteString(frame)
	}

	rec, err := runPassthroughStreamingFixture(t, svc, account, body.String())
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, openAIFirstOutputStageFailureReason, failoverErr.Reason)
	require.False(t, failoverErr.RetryableOnSameAccount,
		"a malformed preamble must not consume the same account retry budget")
	require.Empty(t, rec.Body.String(), "buffered lifecycle events must not leak before failover")
}
