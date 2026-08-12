package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func sharedChatTestAccount(baseURL string) *Account {
	return &Account{
		ID:          2155,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": baseURL},
		Extra:       map[string]any{"openai_passthrough": true},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func TestBuildOpenAIResponsesURL_SharedChatCodexBase(t *testing.T) {
	require.Equal(t,
		"https://new.sharedchat.cc/codex/responses?tenant=a",
		buildOpenAIResponsesURL("https://new.sharedchat.cc/codex?tenant=a#stale"),
	)
	require.Equal(t,
		"https://compat.example/codex/v1/responses",
		buildOpenAIResponsesURL("https://compat.example/codex"),
	)
}

func TestSharedChatCodexPassthroughDetection(t *testing.T) {
	require.True(t, isSharedChatCodexPassthrough(sharedChatTestAccount("https://new.sharedchat.cc/codex")))
	require.True(t, isSharedChatCodexPassthrough(sharedChatTestAccount("https://NEW.SHAREDCHAT.CC/codex/responses/")))
	require.False(t, isSharedChatCodexPassthrough(sharedChatTestAccount("https://other.example/codex")))

	account := sharedChatTestAccount("https://new.sharedchat.cc/codex")
	account.Extra["openai_passthrough"] = false
	require.False(t, isSharedChatCodexPassthrough(account))
}

func TestSharedChatPassthroughBridgesNonStreamingWithoutHeaderOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "third-party/1.0")
	c.Request.Header.Set("Originator", "third-party")
	c.Request.Header.Set("Accept", "application/json")

	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"summarize two release notes"}`)
	upstreamSSE := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_sharedchat","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Summary"}]}],"usage":{"input_tokens":8,"output_tokens":2}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamSSE)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, sharedChatTestAccount("https://new.sharedchat.cc/codex"), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.Equal(t, "https://new.sharedchat.cc/codex/responses", upstream.lastReq.URL.String())
	require.Equal(t, codexCanonicalUserAgent(), upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "responses=experimental", upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.Equal(t, "completed", gjson.Get(recorder.Body.String(), "status").String())
	require.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
}

func TestSharedChatPassthroughIgnoresLegacyIdentityHeaderOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	account := sharedChatTestAccount("https://new.sharedchat.cc/codex")
	account.Credentials["header_override_enabled"] = true
	account.Credentials["header_overrides"] = map[string]any{
		"User-Agent": "legacy-egress-client/0.1",
		"Originator": "legacy-egress",
		"Version":    "0.1",
		"Accept":     "application/json",
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\ndata: [DONE]\n\n")),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: upstream,
	}

	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"compare configs"}`))
	require.NoError(t, err)
	require.Equal(t, codexCanonicalUserAgent(), upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.NotEqual(t, "legacy-egress", upstream.lastReq.Header.Get("Originator"))
	require.NotEqual(t, "0.1", upstream.lastReq.Header.Get("Version"))
}

func TestSharedChatPassthroughPreservesStreamingAndCompactSemantics(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       []byte
		upstream   *http.Response
		wantStream bool
		wantURL    string
		wantAccept string
	}{
		{
			name: "streaming",
			path: "/v1/responses",
			body: []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"compare two configs"}`),
			upstream: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\ndata: [DONE]\n\n")),
			},
			wantStream: true,
			wantURL:    "https://new.sharedchat.cc/codex/responses",
			wantAccept: "text/event-stream",
		},
		{
			name: "compact",
			path: "/v1/responses/compact",
			body: []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"compact this context"}`),
			upstream: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"resp_compact","status":"completed","output":[],"usage":{}}`)),
			},
			wantStream: false,
			wantURL:    "https://new.sharedchat.cc/codex/responses/compact",
			wantAccept: "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader(tt.body))
			upstream := &httpUpstreamRecorder{resp: tt.upstream}
			svc := &OpenAIGatewayService{
				cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
				httpUpstream: upstream,
			}

			result, err := svc.Forward(context.Background(), c, sharedChatTestAccount("https://new.sharedchat.cc/codex"), tt.body)
			require.NoError(t, err)
			require.Equal(t, tt.wantStream, result.Stream)
			require.Equal(t, tt.wantStream, gjson.GetBytes(upstream.lastBody, "stream").Bool())
			require.Equal(t, tt.wantURL, upstream.lastReq.URL.String())
			require.Equal(t, tt.wantAccept, upstream.lastReq.Header.Get("Accept"))
		})
	}
}

func TestNormalizeSharedChatQuotaResponse(t *testing.T) {
	now := time.Date(2026, time.August, 13, 10, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	resp := &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header)}
	body := []byte(`{"error":{"code":"global_fixed_window_quota_exhausted","message":"quota exhausted"}}`)

	normalizeSharedChatQuotaResponse(resp, body, now)

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "429 Too Many Requests", resp.Status)
	require.Equal(t, "5400", resp.Header.Get("Retry-After"))
	require.Equal(t, "100", resp.Header.Get("X-Codex-Primary-Used-Percent"))
	require.Equal(t, "180", resp.Header.Get("X-Codex-Primary-Window-Minutes"))
	_, err := strconv.Atoi(resp.Header.Get("X-Codex-Primary-Reset-After-Seconds"))
	require.NoError(t, err)

	other := &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header)}
	normalizeSharedChatQuotaResponse(other, []byte(`{"error":{"code":"codex_access_restricted"}}`), now)
	require.Equal(t, http.StatusForbidden, other.StatusCode)
}
