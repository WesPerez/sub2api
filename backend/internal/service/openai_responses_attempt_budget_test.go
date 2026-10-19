package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type budgetBridgeUpstream struct {
	prefix      string
	suffix      string
	wait        time.Duration
	headers     bool
	status      int
	contentType string
	readErr     error
	requests    int
}

func (u *budgetBridgeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests++
	if u.headers {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	bodyCtx, cancelBody := context.WithCancel(req.Context())
	contentType := u.contentType
	if contentType == "" {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}},
		Body: &budgetBridgeBody{ctx: bodyCtx, cancel: cancelBody, prefix: strings.NewReader(u.prefix), suffix: strings.NewReader(u.suffix), wait: u.wait, readErr: u.readErr}}, nil
}
func (u *budgetBridgeUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, n)
}

type budgetBridgeBody struct {
	ctx     context.Context
	cancel  context.CancelFunc
	prefix  *strings.Reader
	suffix  *strings.Reader
	wait    time.Duration
	once    sync.Once
	err     error
	readErr error
}

func (b *budgetBridgeBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	b.once.Do(func() {
		if b.wait == 0 {
			<-b.ctx.Done()
			b.err = b.ctx.Err()
			return
		}
		timer := time.NewTimer(b.wait)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			b.err = b.ctx.Err()
		case <-timer.C:
		}
	})
	if b.err != nil {
		return 0, b.err
	}
	if b.suffix.Len() == 0 && b.readErr != nil {
		return 0, b.readErr
	}
	return b.suffix.Read(p)
}
func (b *budgetBridgeBody) Close() error { b.cancel(); return nil }

func budgetBridgeAccount(mode string) *Account {
	a := newOpenAIRejectedFieldTestAccount()
	if mode == "anthropic" {
		a.Platform = PlatformKimi
		a.Credentials["api_protocol"] = APIProtocolAnthropic
		return a
	}
	a.Extra["openai_responses_mode"] = "force_responses"
	if mode == "passthrough" {
		a.Extra["openai_passthrough"] = true
	}
	if mode == "chat" {
		a.Extra["openai_responses_mode"] = "force_chat_completions"
	}
	return a
}

func TestOpenAIFirstOutputBudgetBridgeBlockedPaths(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "chat", "anthropic"} {
		for _, scenario := range []string{"headers", "body", "metadata", "error_body"} {
			for _, stream := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", mode, scenario, stream), func(t *testing.T) {
					up := &budgetBridgeUpstream{headers: scenario == "headers"}
					if scenario == "metadata" {
						up.prefix = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"old\"}}\n\n"
						if mode == "chat" {
							up.prefix = "data: {\"id\":\"old\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"
						}
						if mode == "anthropic" {
							up.prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"old\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"kimi-k2\",\"content\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"
						}
					}
					if scenario == "error_body" {
						up.status = http.StatusTooManyRequests
						up.prefix = "{\"error\":"
					}
					svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
					svc.httpUpstream = up
					svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
					body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"hello"}`, stream))
					c := newOpenAIRejectedFieldTestContext(body)
					ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 150*time.Millisecond)
					c.Request = c.Request.WithContext(ctx)
					start := time.Now()
					result, err := svc.Forward(ctx, c, budgetBridgeAccount(mode), body)
					var failure *UpstreamFailoverError
					require.ErrorAs(t, err, &failure)
					require.True(t, failure.FirstOutputTimedOut)
					require.False(t, failure.ShouldReportAccountScheduleFailure())
					require.Nil(t, result)
					require.Equal(t, 1, up.requests)
					require.False(t, c.Writer.Written(), "metadata or synthesized terminal must not escape expired attempt")
					require.Less(t, time.Since(start), time.Second)
				})
			}
		}
	}
}

func budgetBridgeHealthyStream(mode string, wait time.Duration) *budgetBridgeUpstream {
	up := &budgetBridgeUpstream{wait: wait,
		prefix: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		suffix: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"}
	if mode == "chat" {
		up.prefix = "data: {\"id\":\"ok\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"
		up.suffix = "data: {\"id\":\"ok\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	}
	if mode == "anthropic" {
		up.prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"ok\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"kimi-k2\",\"content\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
		up.suffix = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return up
}

func TestOpenAIFirstOutputBudgetBridgeHealthyLongStream(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "chat", "anthropic"} {
		t.Run(mode, func(t *testing.T) {
			up := budgetBridgeHealthyStream(mode, 250*time.Millisecond)
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			svc.httpUpstream = up
			svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			c := newOpenAIRejectedFieldTestContext(body)
			ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 150*time.Millisecond)
			c.Request = c.Request.WithContext(ctx)
			result, err := svc.Forward(ctx, c, budgetBridgeAccount(mode), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.GreaterOrEqual(t, result.Duration, 250*time.Millisecond)
		})
	}
}

func TestOpenAIFirstOutputBudgetWriterAdmission(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","delta":""}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"   "}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"opaque","summary":[]}}`,
	} {
		t.Run(frame, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			b := newResponsesAttemptBudget(context.Background(), time.Now().Add(100*time.Millisecond))
			defer b.close()
			w := newResponsesBudgetWriter(c.Writer, b, true)
			defer func() { _ = w.stage.Close() }()
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w, "data: "+frame+"\n\n")
			require.NoError(t, err)
			w.Flush()
			require.False(t, c.Writer.Written())
			<-b.ctx.Done()
			_, err = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.False(t, c.Writer.Written())
		})
	}
}

func TestOpenAIFirstOutputBudgetWriterFragmentAndCancel(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newResponsesAttemptBudget(client, time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	payload := []byte("event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\r\n\r\n")
	for _, ch := range payload[:len(payload)-1] {
		_, err := w.Write([]byte{ch})
		require.NoError(t, err)
	}
	require.False(t, c.Writer.Written())
	_, err := w.Write(payload[len(payload)-1:])
	require.NoError(t, err)
	require.Equal(t, payload, rec.Body.Bytes())
	cancel()
	_, err = w.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, b.ctx.Err(), "usage drain survives cancellation after admission")
}

func TestOpenAIFirstOutputBudgetWriterStagingLimit(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := w.Write(bytes.Repeat([]byte("x"), openAIFirstOutputStageMaxBytes+1))
	require.ErrorIs(t, err, errOpenAIFirstOutputStageLimit)
	require.False(t, c.Writer.Written())
}

func TestOpenAIFirstOutputBudgetWriterTerminalWaitsForDecision(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`,
		`{"type":"error","code":"rate_limit_exceeded"}`,
		`{"type":"response.completed","response":{"output":[]}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
			defer b.close()
			w := newResponsesBudgetWriter(c.Writer, b, true)
			defer func() { _ = w.stage.Close() }()
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w, "data: "+terminal+"\n\n")
			require.NoError(t, err)
			require.False(t, w.committed)
			require.False(t, c.Writer.Written())
			require.True(t, w.terminal)
		})
	}
}

func TestOpenAIFirstOutputBudgetWriterHeartbeatDoesNotSplitEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := io.WriteString(w, "event: response.output_text.delta\n")
	require.NoError(t, err)
	_, err = io.WriteString(w, ": keepalive\n\n")
	require.NoError(t, err)
	_, err = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
	require.NoError(t, err)
	require.Equal(t, ": keepalive\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n", rec.Body.String())
}

func TestOpenAIFirstOutputBudgetBridgeClientCancel(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "chat", "anthropic"} {
		t.Run(mode, func(t *testing.T) {
			up := &budgetBridgeUpstream{headers: true}
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			svc.httpUpstream = up
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			c := newOpenAIRejectedFieldTestContext(body)
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			ctx = WithOpenAIResponsesFirstOutputBudget(ctx, time.Second)
			c.Request = c.Request.WithContext(ctx)
			timer := time.AfterFunc(50*time.Millisecond, cancel)
			defer timer.Stop()
			result, err := svc.Forward(ctx, c, budgetBridgeAccount(mode), body)
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, result)
			var failure *UpstreamFailoverError
			require.False(t, errors.As(err, &failure))
			require.False(t, c.Writer.Written())
			require.Equal(t, 1, up.requests)
		})
	}
}

func TestOpenAIFirstOutputBudgetWriterTerminalSealsFollowingFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")
	require.NoError(t, err)
	require.False(t, w.committed)
	require.NoError(t, w.commit())
	require.NotContains(t, rec.Body.String(), "late")
}

func TestOpenAIFirstOutputBudgetWriterJSONErrorAfterHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := io.WriteString(w, ": keepalive\n\n")
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(400)
	_, err = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad request"}}`)
	require.NoError(t, err)
	require.NoError(t, w.commit())
	require.Contains(t, rec.Body.String(), "response.failed")
	require.NotContains(t, rec.Body.String(), "\n\n{\"error\"")
	require.Equal(t, "text/event-stream", rec.Result().Header.Get("Content-Type"))
}

// Once output is admitted, the shared pre-output deadline must no longer kill
// a healthy stream, but a hung body must still release its upstream slot.
func TestOpenAIFirstOutputBudgetBridgeIdleAfterOutput(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "chat", "anthropic"} {
		t.Run(mode, func(t *testing.T) {
			up := budgetBridgeHealthyStream(mode, 0)
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			svc.httpUpstream = up
			svc.cfg.Gateway.StreamDataIntervalTimeout = 1
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			c := newOpenAIRejectedFieldTestContext(body)
			ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 150*time.Millisecond)
			c.Request = c.Request.WithContext(ctx)
			start := time.Now()
			result, err := svc.Forward(ctx, c, budgetBridgeAccount(mode), body)
			require.Error(t, err)
			require.NotNil(t, result, "partial usage must survive idle termination")
			require.Equal(t, 1, up.requests, "never retry after semantic output")
			var failure *UpstreamFailoverError
			require.False(t, errors.As(err, &failure))
			require.GreaterOrEqual(t, time.Since(start), 900*time.Millisecond)
			require.Less(t, time.Since(start), 3*time.Second)
			require.True(t, c.Writer.Written())
			if mode != "native" {
				require.False(t, IsResponseCommitted(c), "first delta is not an error terminal; handler must finish it")
			}
		})
	}
}

func TestOpenAIFirstOutputBudgetRejectsEmptyCompleted(t *testing.T) {
	for _, mode := range []string{"native", "passthrough", "chat"} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				up := &budgetBridgeUpstream{wait: time.Millisecond,
					prefix: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"empty\",\"status\":\"completed\",\"output\":[]}}\n\n"}
				if mode == "chat" {
					up.prefix = "data: [DONE]\n\n"
				}
				if mode == "anthropic" {
					up.prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"empty\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				if !stream && mode != "anthropic" {
					up.contentType = "application/json"
					up.prefix = `{"id":"empty","status":"completed","output":[]}`
					if mode == "chat" {
						up.prefix = `{"id":"empty","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
					}
				}
				svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
				svc.httpUpstream = up
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"hello"}`, stream))
				c := newOpenAIRejectedFieldTestContext(body)
				ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), time.Second)
				c.Request = c.Request.WithContext(ctx)
				_, err := svc.Forward(ctx, c, budgetBridgeAccount(mode), body)
				require.Error(t, err)
				require.False(t, c.Writer.Written(), "empty completed must not escape as success")
			})
		}
	}
}

func TestOpenAIFirstOutputBudgetAnthropicCompletedDoesNotWaitForEOF(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			up := budgetBridgeHealthyStream("anthropic", 0)
			up.prefix += up.suffix
			up.suffix = ""
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			svc.httpUpstream = up
			svc.cfg.Gateway.StreamDataIntervalTimeout = 1
			body := []byte(fmt.Sprintf(`{"model":"kimi-k2","stream":%t,"input":"hello"}`, stream))
			c := newOpenAIRejectedFieldTestContext(body)
			ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 150*time.Millisecond)
			c.Request = c.Request.WithContext(ctx)
			start := time.Now()
			result, err := svc.Forward(ctx, c, budgetBridgeAccount("anthropic"), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.Less(t, time.Since(start), 500*time.Millisecond)
		})
	}
}

func TestOpenAIFirstOutputBudgetStructuredTerminalOutput(t *testing.T) {
	for _, item := range []string{
		`{"type":"web_search_call","status":"completed","action":{"type":"search","query":"x"}}`,
		`{"type":"future_tool_result","value":{"count":3}}`,
		`{"type":"function_call","name":"f","arguments":"{}"}`,
	} {
		require.True(t, responsesBudgetResponseHasOutput(gjson.Parse(`{"output":[`+item+`]}`)))
	}
	for _, item := range []string{
		`{"type":"message","content":[{"type":"output_text","text":"   "}]}`,
		`{"type":"reasoning","encrypted_content":"opaque","summary":[]}`,
	} {
		require.False(t, responsesBudgetResponseHasOutput(gjson.Parse(`{"output":[`+item+`]}`)))
	}
}

func TestOpenAIFirstOutputBudgetWriterBareErrorKeepsAuthoritativeTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	b := newResponsesAttemptBudget(context.Background(), time.Now().Add(time.Second))
	defer b.close()
	w := newResponsesBudgetWriter(c.Writer, b, true)
	defer func() { _ = w.stage.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := io.WriteString(w, "data: {\"type\":\"error\",\"code\":\"unknown\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n")
	require.NoError(t, err)
	require.NoError(t, w.commit())
	require.Contains(t, rec.Body.String(), "response.failed")
	require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
}

func TestOpenAIFirstOutputBudgetAnthropicReadErrorIsNotCompleted(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			up := budgetBridgeHealthyStream("anthropic", time.Millisecond)
			up.suffix, up.readErr = "", io.ErrUnexpectedEOF
			svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
			svc.httpUpstream = up
			body := []byte(fmt.Sprintf(`{"model":"kimi-k2","stream":%t,"input":"hello"}`, stream))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
			ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), time.Second)
			c.Request = c.Request.WithContext(ctx)
			result, err := svc.Forward(ctx, c, budgetBridgeAccount("anthropic"), body)
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			require.NotContains(t, rec.Body.String(), "response.completed")
			if stream {
				require.NotNil(t, result)
				require.Equal(t, 7, result.Usage.InputTokens)
			}
		})
	}
}

func TestResponsesLocalTimeoutDoesNotLoadUpstreamErrorPolicies(t *testing.T) {
	up := &budgetBridgeUpstream{headers: true}
	svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
	svc.httpUpstream = up
	svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
	c := newOpenAIRejectedFieldTestContext(body)
	// A cache miss would call the nil repository and panic. A local deadline
	// must remain observable without reading an upstream error policy.
	BindErrorPassthroughService(c, &ErrorPassthroughService{})
	ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 30*time.Millisecond)
	_, err := svc.Forward(ctx, c, budgetBridgeAccount("native"), body)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	events, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	rows, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, rows, 1)
	require.Equal(t, "first_output_timeout", rows[0].Kind)
	require.Contains(t, rows[0].Detail, "cancel_observed_ms=")
	require.False(t, rows[0].SkipMonitoring)
}

func TestResponsesAttemptTimeoutRecordsBeforeSharedDeadline(t *testing.T) {
	up := &budgetBridgeUpstream{headers: true}
	svc := newOpenAIRejectedFieldTestService(&httpUpstreamRecorder{})
	svc.httpUpstream = up
	svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
	c := newOpenAIRejectedFieldTestContext(body)
	BindErrorPassthroughService(c, &ErrorPassthroughService{})
	ctx := WithOpenAIResponsesFirstOutputBudget(c.Request.Context(), 10*time.Second)
	_, err := svc.Forward(ctx, c, budgetBridgeAccount("native"), body)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	remaining, _ := OpenAIResponsesFirstOutputBudgetRemaining(ctx)
	require.Positive(t, remaining)
	events, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	rows, ok := events.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Detail, "cancel_observed_ms=")
}
