package service

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression for Codex subagent history: commentary can appear between a call
// and its result, and each tool continuation still needs the original reasoning.
func TestForwardChatStoreFalsePreservesReasoningAcrossToolContinuations(t *testing.T) {
	for _, reasoning := range []string{
		`"summary":[{"type":"summary_text","text":"original thinking"}]`,
		`"summary":[],"content":[{"type":"reasoning_text","text":"original thinking"}]`,
	} {
		body := []byte(`{"model":"deepseek-v4.1-flash","store":false,"stream":false,"input":[
			{"type":"message","role":"user","content":"Check the results."},
			{"type":"reasoning",` + reasoning + `},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},
			{"type":"message","role":"assistant","content":"Checking."},
			{"type":"function_call_output","call_id":"call_a","output":"first result"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"second thinking"}]},
			{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":"second result"}
		]}`)
		account := openaiPlatformDeepSeekAccount()
		// A general reseller does not get official-DeepSeek placeholder repair.
		account.Credentials["base_url"] = "https://reseller.example"
		upstream := newOKChatCompletionsUpstream("store-false-replay", deepSeekChatFallbackOKBody)
		svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
		_, err := svc.Forward(context.Background(), newDeepSeekChatFallbackContext(t, body), account, body)
		require.NoError(t, err)
		require.Contains(t, upstream.lastReq.URL.Path, "/chat/completions")
		messages := gjson.GetBytes(upstream.lastBody, "messages").Array()
		calls, commentary := 0, 0
		for _, message := range messages {
			if message.Get("role").String() != "assistant" {
				continue
			}
			if message.Get("content").String() == "Checking." {
				require.Equal(t, "original thinking", message.Get("reasoning_content").String())
				commentary++
			}
			for _, call := range message.Get("tool_calls").Array() {
				want := "original thinking"
				if call.Get("id").String() == "call_b" {
					want = "second thinking"
				}
				require.Equal(t, want, message.Get("reasoning_content").String())
				calls++
			}
		}
		require.Equal(t, 2, calls)
		require.Equal(t, 1, commentary)
		require.NotEqual(t, gjson.True, gjson.GetBytes(upstream.lastBody, "store").Type)
	}
}

func TestStoreFalseReasoningNativeCompactAndWebSocketBoundaries(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","store":false,"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"original"}],"content":[{"type":"reasoning_text","text":"full original"}]},{"type":"message","role":"assistant","content":"answer"}]}`)
	for _, tc := range []struct {
		name    string
		chat    bool
		compact bool
	}{
		{"native", false, false}, {"native compact", false, true}, {"chat compact", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := openaiPlatformDeepSeekAccount()
			account.Credentials["base_url"] = "https://reseller.example"
			if !tc.chat {
				account.Extra = nil
			}
			c := newDeepSeekChatFallbackContext(t, body)
			if tc.compact {
				c.Request = httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(string(body)))
			}
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
			_, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			if tc.chat {
				require.Contains(t, upstream.lastReq.URL.Path, "/chat/completions")
				for _, message := range gjson.GetBytes(upstream.lastBody, "messages").Array() {
					require.Empty(t, message.Get("reasoning_content").String())
				}
			} else {
				require.Contains(t, upstream.lastReq.URL.Path, "/responses")
				for _, item := range gjson.GetBytes(upstream.lastBody, "input").Array() {
					require.NotEqual(t, "reasoning", item.Get("type").String())
				}
			}
			require.NotEqual(t, gjson.True, gjson.GetBytes(upstream.lastBody, "store").Type)
		})
	}
	// The WS HTTP bridge still speaks native Responses, even with a Chat probe hint.
	for _, chatHint := range []bool{false, true} {
		account := openaiPlatformDeepSeekAccount()
		if !chatHint {
			account.Extra = nil
		}
		got, _, err := normalizeOpenAIResponsesWebSocketCompatibilityBody(body, account, false)
		require.NoError(t, err)
		require.Equal(t, "message", gjson.GetBytes(got, "input.0.type").String())
		require.Equal(t, gjson.False, gjson.GetBytes(got, "store").Type)
	}
}

func TestForwardModelCatalogKeepsProtocolSpecificReasoning(t *testing.T) {
	account := &Account{ID: 982731, Platform: PlatformCommandCode, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test", "api_base_urls": map[string]any{
			APIProtocolChatCompletions: "https://reasoning-catalog.example/v1",
			APIProtocolResponses:       "https://reasoning-catalog.example/v1",
		}},
	}
	key := modelProtocolCatalogKey(account, "https://reasoning-catalog.example/v1/models")
	upstreamModelProtocols.store(key, map[string][]string{
		"replay-chat": {APIProtocolChatCompletions}, "replay-native": {APIProtocolResponses},
	}, nil, time.Now())
	t.Cleanup(func() {
		upstreamModelProtocols.mu.Lock()
		defer upstreamModelProtocols.mu.Unlock()
		delete(upstreamModelProtocols.entries, key)
	})
	for _, model := range []string{"replay-chat", "replay-native"} {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","store":false,"stream":false,"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"catalog thinking"}]},{"type":"function_call","call_id":"call_1","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
			_, err := svc.Forward(context.Background(), newDeepSeekChatFallbackContext(t, body), account, body)
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			if model == "replay-chat" {
				require.Contains(t, upstream.lastReq.URL.Path, "/chat/completions")
				require.Equal(t, "catalog thinking", gjson.GetBytes(upstream.lastBody, "messages.0.reasoning_content").String())
			} else {
				require.Contains(t, upstream.lastReq.URL.Path, "/responses")
				// Non-OpenAI native providers retain the upstream's existing passthrough contract.
				require.Equal(t, "catalog thinking", gjson.GetBytes(upstream.lastBody, "input.0.summary.0.text").String())
			}
		})
	}
}
