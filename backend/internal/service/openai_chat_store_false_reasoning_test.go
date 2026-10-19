package service

import (
	"context"
	"testing"

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

func TestStoreFalseReasoningProtocolBoundaryIncludesWebSocket(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","store":false,"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"original"}],"content":[{"type":"reasoning_text","text":"full original"}]},{"type":"message","role":"assistant","content":"answer"}]}`)
	chat := openaiPlatformDeepSeekAccount()
	native := *chat
	native.Extra = nil
	for _, tc := range []struct {
		name    string
		account *Account
		keep    bool
	}{
		{"chat", chat, true}, {"native", &native, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, websocket := range []bool{false, true} {
				var got []byte
				var err error
				if websocket {
					got, _, err = normalizeOpenAIResponsesWebSocketCompatibilityBody(body, tc.account, false)
				} else {
					got, _, err = normalizeOpenAIAccountStoreFalseReasoningReplay(body, tc.account, false)
				}
				require.NoError(t, err)
				require.Equal(t, tc.keep, gjson.GetBytes(got, "input.0.type").String() == "reasoning")
				if tc.keep {
					require.Equal(t, "full original", gjson.GetBytes(got, "input.0.content.0.text").String())
				}
				require.Equal(t, gjson.False, gjson.GetBytes(got, "store").Type)
			}
		})
	}
	got, _, err := normalizeOpenAIAccountStoreFalseReasoningReplay(body, chat, true)
	require.NoError(t, err)
	require.Equal(t, "message", gjson.GetBytes(got, "input.0.type").String(), "compact retains native filtering")
}
