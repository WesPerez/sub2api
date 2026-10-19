package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type cancelOnReadPassthroughBody struct {
	io.Reader
	cancel context.CancelFunc
	closed bool
}

func (b *cancelOnReadPassthroughBody) Read(p []byte) (int, error) {
	b.cancel()
	return b.Reader.Read(p)
}

func (b *cancelOnReadPassthroughBody) Close() error {
	b.closed = true
	return nil
}

func TestOpenAIHTTPCanceledClientDoesNotStartAttempt(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			upstream := &httpUpstreamRecorder{}

			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, body)

			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, result)
			require.Empty(t, upstream.requests)
			require.False(t, c.Writer.Written(), "cancellation must not write a generic upstream error")
			// Cancellation now exits before routing or passthrough preparation;
			// only an attempt that actually starts needs the internal mode flag.
		})
	}
}

func TestOpenAIHTTPCanceledClientDoesNotRetryRejectedField(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			body := []byte(`{"model":"gpt-5.5","stream":false,"max_output_tokens":4096,"input":"hello"}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			rejectedBody := &cancelOnReadPassthroughBody{
				Reader: strings.NewReader(`{"error":{"code":"unsupported_parameter","message":"Unsupported parameter: max_output_tokens","param":"max_output_tokens"}}`),
				cancel: cancel,
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: rejectedBody},
				newOpenAIRejectedFieldTestResponse(http.StatusOK, `{"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
			}}

			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, body)

			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, result)
			require.Len(t, upstream.requests, 1, "the rejected request must not be replayed after cancellation")
			require.True(t, rejectedBody.closed)
			require.False(t, c.Writer.Written(), "cancellation must not write a generic upstream error")
			if passthrough {
				flag, exists := c.Get("openai_passthrough")
				require.True(t, exists)
				require.Equal(t, true, flag)
			}
		})
	}
}

func TestOpenAIHTTPCanceledClientStillDrainsStartedResponse(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request = c.Request.WithContext(ctx)
			responseBody := &cancelOnReadPassthroughBody{
				Reader: strings.NewReader("data: " + `{"type":"response.completed","response":{"id":"resp_drain","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}` + "\n\n"),
				cancel: cancel,
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       responseBody,
			}}

			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(ctx, c, account, body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 1)
			require.True(t, responseBody.closed)
			require.Equal(t, 11, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			if passthrough {
				flag, exists := c.Get("openai_passthrough")
				require.True(t, exists)
				require.Equal(t, true, flag)
			}
		})
	}
}
