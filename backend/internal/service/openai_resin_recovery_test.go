package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/resinrecovery"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type recordingResinBody struct {
	io.ReadCloser
	outcomes []resinrecovery.Outcome
}

func newResinTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, recorder
}

func (b *recordingResinBody) ReportResinOutcome(outcome resinrecovery.Outcome) {
	if outcome != "" {
		b.outcomes = append(b.outcomes, outcome)
	}
}

const recoveryCompletedSSE = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
const recoveryPartialSSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"

func TestResinRecoveryStreamingVerdicts(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, body string
			readErr    error
			canceled   bool
			want       resinrecovery.Outcome
		}{
			{"empty", "", nil, false, resinrecovery.EmptyStream},
			{"html", "<html>challenge</html>", nil, false, resinrecovery.EmptyStream},
			{"preamble-only", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n", nil, false, resinrecovery.EmptyStream},
			{"partial-without-terminal", recoveryPartialSSE, nil, false, resinrecovery.EmptyStream},
			{"read-error", "", io.ErrUnexpectedEOF, false, resinrecovery.InvalidStream},
			{"partial-read-error", recoveryPartialSSE, io.ErrUnexpectedEOF, false, resinrecovery.InvalidStream},
			{"completed", recoveryCompletedSSE, nil, false, resinrecovery.Reachable},
			{"completed-then-eof", recoveryCompletedSSE, io.ErrUnexpectedEOF, false, resinrecovery.Reachable},
			{"business-failure", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"input exceeds context window\"}}}\n\n", nil, false, resinrecovery.Reachable},
			{"client-canceled-eof", "", nil, true, ""},
			{"client-canceled-read", "", context.Canceled, true, ""},
		} {
			name := "normal/" + tc.name
			if passthrough {
				name = "passthrough/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				c, _ := newResinTestContext()
				if tc.canceled {
					ctx, cancel := context.WithCancel(c.Request.Context())
					cancel()
					c.Request = c.Request.WithContext(ctx)
				}
				reader := io.NopCloser(strings.NewReader(tc.body))
				if tc.readErr != nil {
					reader = &openAIStreamReadThenErrorCloser{reader: strings.NewReader(tc.body), err: tc.readErr}
				}
				body := &recordingResinBody{ReadCloser: reader}
				resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "synthetic"}
				if passthrough {
					_, _ = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				} else {
					_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
				}
				if tc.want == "" {
					require.Empty(t, body.outcomes)
				} else {
					require.Equal(t, []resinrecovery.Outcome{tc.want}, body.outcomes)
				}
			})
		}
	}
}

func TestResinRecoveryAccountConnectionTestVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		canceled   bool
		want       resinrecovery.Outcome
	}{
		{"empty", "", false, resinrecovery.EmptyStream},
		{"html", "<html>challenge</html>", false, resinrecovery.EmptyStream},
		{"partial", recoveryPartialSSE, false, resinrecovery.EmptyStream},
		{"done-without-completion", "data: [DONE]\n\n", false, resinrecovery.EmptyStream},
		{"partial-done-without-completion", recoveryPartialSSE + "data: [DONE]\n\n", false, resinrecovery.EmptyStream},
		{"complete", recoveryCompletedSSE, false, resinrecovery.Reachable},
		{"final-line-without-newline", strings.TrimRight(recoveryCompletedSSE, "\n"), false, resinrecovery.Reachable},
		{"business-error", "data: {\"type\":\"error\",\"error\":{\"message\":\"quota exhausted\"}}\n\n", false, resinrecovery.Reachable},
		{"cancel", "", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newResinTestContext()
			if tc.canceled {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			body := &recordingResinBody{ReadCloser: io.NopCloser(strings.NewReader(tc.body))}
			_ = (&AccountTestService{}).processOpenAIStream(c, body)
			if tc.want == "" {
				require.Empty(t, body.outcomes)
			} else {
				require.Equal(t, []resinrecovery.Outcome{tc.want}, body.outcomes)
			}
		})
	}
}

func TestResinRecoveryBufferedResponses(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, media, body string
			want              resinrecovery.Outcome
		}{
			{"json", "application/json", `{"id":"resp_test","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, resinrecovery.Reachable},
			{"json-with-sse-text", "application/json", `{"id":"resp_test","object":"response","output":[],"note":"data: event: text","usage":{}}`, resinrecovery.Reachable},
			{"sse", "text/event-stream", recoveryCompletedSSE, resinrecovery.Reachable},
			{"wrong-sse-content-type", "application/json", recoveryCompletedSSE, resinrecovery.Reachable},
			{"empty-sse", "text/event-stream", "", resinrecovery.EmptyStream},
			{"partial-sse", "text/event-stream", recoveryPartialSSE, resinrecovery.EmptyStream},
			{"html", "text/html", "<html>challenge</html>", resinrecovery.InvalidStream},
		} {
			name := "normal/" + tc.name
			if passthrough {
				name = "passthrough/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				c, _ := newResinTestContext()
				body := &recordingResinBody{ReadCloser: io.NopCloser(strings.NewReader(tc.body))}
				resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{"Content-Type": []string{tc.media}}}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "synthetic"}
				if passthrough {
					_, _ = svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, account, "model", "model")
				} else {
					_, _ = svc.handleNonStreamingResponse(c.Request.Context(), resp, c, account, "model", "model")
				}
				require.Equal(t, []resinrecovery.Outcome{tc.want}, body.outcomes)
			})
		}
	}
}

func TestResinRecoveryDoesNotQuarantineOtherAccountsOnSharedProxy(t *testing.T) {
	proxyID := int64(1)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &proxyID}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	svc.openaiProxyStreamCircuit = newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
		failureThreshold: 2, failureWindow: time.Minute, quarantineTTL: 10 * time.Minute, maxEntries: 16,
	})
	for i := range 4 {
		c, _ := newResinTestContext()
		body := &recordingResinBody{ReadCloser: &openAIStreamReadThenErrorCloser{reader: strings.NewReader(recoveryPartialSSE), err: io.ErrUnexpectedEOF}}
		wrapped := wrapResinTestStream(body, "combined")
		resp := &http.Response{StatusCode: http.StatusOK, Body: wrapped, Header: make(http.Header)}
		if i%2 == 0 {
			_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
		} else {
			_, _ = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
		}
		require.NoError(t, wrapped.Close())
		require.Equal(t, []resinrecovery.Outcome{resinrecovery.InvalidStream}, body.outcomes)
	}
	require.False(t, svc.isOpenAIProxyStreamQuarantined(context.Background(), account), "managed node failures must not quarantine the shared proxy")
}

func wrapResinTestStream(body io.ReadCloser, kind string) io.ReadCloser {
	if kind == "header" || kind == "combined" {
		body = resinrecovery.PreserveBodyObservation(&openAIRequestContextReadCloser{
			ReadCloser: body, cleanup: func() {},
		}, body)
	}
	if kind == "ping" || kind == "combined" {
		body = newGrokResponsesBillingPingFilterBody(body, &Account{Platform: PlatformGrok}, defaultMaxLineSize)
	}
	if kind == "tool" || kind == "combined" {
		body = newResponsesClientToolStreamBody(body, apicompat.ResponsesClientToolMapping{ToolSearch: true}, defaultMaxLineSize)
	}
	return body
}

func TestResinRecoveryWrappedStreamingVerdicts(t *testing.T) {
	for _, kind := range []string{"header", "tool", "ping", "combined"} {
		for _, passthrough := range []bool{false, true} {
			for _, tc := range []struct {
				name, payload string
				want          resinrecovery.Outcome
			}{
				{"empty", "", resinrecovery.EmptyStream},
				{"partial", recoveryPartialSSE, resinrecovery.EmptyStream},
				{"completed", recoveryCompletedSSE, resinrecovery.Reachable},
			} {
				name := kind + "/normal/" + tc.name
				if passthrough {
					name = kind + "/passthrough/" + tc.name
				}
				t.Run(name, func(t *testing.T) {
					c, _ := newResinTestContext()
					source := &recordingResinBody{ReadCloser: io.NopCloser(strings.NewReader(tc.payload))}
					body := wrapResinTestStream(source, kind)
					defer func() { _ = body.Close() }()
					require.True(t, resinrecovery.IsManaged(body))
					resp := &http.Response{StatusCode: http.StatusOK, Body: body, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
					if passthrough {
						_, _ = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					} else {
						_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
					}
					require.Equal(t, []resinrecovery.Outcome{tc.want}, source.outcomes)
				})
			}
		}
	}
	for _, kind := range []string{"header", "tool", "ping", "combined"} {
		body := wrapResinTestStream(io.NopCloser(strings.NewReader("")), kind)
		require.False(t, resinrecovery.IsManaged(body), "ordinary wrapper must stay unmanaged: %s", kind)
		require.NoError(t, body.Close())
	}
}
