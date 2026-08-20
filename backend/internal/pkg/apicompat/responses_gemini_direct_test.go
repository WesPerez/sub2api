package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestResponsesToGeminiRequestConvertsInputImageAndTools(t *testing.T) {
	req := &ResponsesRequest{
		Model:        "gemini-3.7-flash",
		Instructions: "You are a helpful assistant.",
		Input: json.RawMessage(`[
      {"role":"user","content":[
        {"type":"input_text","text":"Describe this image"},
        {"type":"input_image","image_url":"data:image/png;base64,AA=="}
      ]}
    ]`),
		Tools: []ResponsesTool{{
			Type: "function", Name: "get_weather", Description: "Get weather",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		Reasoning: &ResponsesReasoning{Effort: "high"},
	}
	got, err := ResponsesToGeminiRequest(req, "gemini-3.7-flash")
	require.NoError(t, err)
	require.Len(t, got.Contents, 1)
	require.Equal(t, "user", got.Contents[0].Role)
	require.Equal(t, "Describe this image", got.Contents[0].Parts[0].Text)
	require.Equal(t, "image/png", got.Contents[0].Parts[1].InlineData.MimeType)
	require.Equal(t, "AA==", got.Contents[0].Parts[1].InlineData.Data)
	require.Equal(t, "high", got.GenerationConfig.ThinkingConfig.ThinkingLevel)
	require.Len(t, got.Tools, 1)
	require.Equal(t, "get_weather", got.Tools[0].FunctionDeclarations[0].Name)
}

func TestResponsesToGeminiRequestPreservesToolCallIDsAndTieredEffort(t *testing.T) {
	req := &ResponsesRequest{
		Model: "gemini-3.7-flash",
		Input: json.RawMessage(`[
		  {"type":"reasoning","encrypted_content":"opaque-signature"},
		  {"type":"function_call","call_id":"call_37","name":"inspect","arguments":"{\"path\":\"/tmp\"}"},
		  {"type":"function_call_output","call_id":"call_37","output":"done"}
		]`),
		Reasoning: &ResponsesReasoning{Effort: "high"},
	}
	got, err := ResponsesToGeminiRequest(req, "gemini-3.7-flash-tiered")
	require.NoError(t, err)
	require.Equal(t, "high", got.GenerationConfig.ThinkingConfig.ThinkingLevel)
	require.Equal(t, "call_37", got.Contents[0].Parts[0].FunctionCall.ID)
	require.Equal(t, "opaque-signature", got.Contents[0].Parts[0].ThoughtSignature)
	require.Equal(t, "call_37", got.Contents[1].Parts[0].FunctionResponse.ID)
}

func TestResponsesToGeminiRequestNormalizesGemini37EffortAndOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		effort string
		want   string
	}{
		{effort: "low", want: "low"},
		{effort: "medium", want: "medium"},
		{effort: "high", want: "high"},
		{effort: "xhigh", want: "high"},
		{effort: "max", want: "high"},
		{effort: "ultra", want: "high"},
		{effort: "minimal", want: "medium"},
	} {
		t.Run(tc.effort, func(t *testing.T) {
			maxOutputTokens := 100000
			req := &ResponsesRequest{
				Model:           "gemini-3.7-flash",
				Input:           json.RawMessage(`"inspect the compatibility route"`),
				Reasoning:       &ResponsesReasoning{Effort: tc.effort},
				MaxOutputTokens: &maxOutputTokens,
			}
			got, err := ResponsesToGeminiRequest(req, "gemini-3.7-flash-tiered")
			require.NoError(t, err)
			require.Equal(t, tc.want, got.GenerationConfig.ThinkingConfig.ThinkingLevel)
			require.Equal(t, maxGeminiResponsesOutputTokens, got.GenerationConfig.MaxOutputTokens)
		})
	}
}

func TestGeminiEventToResponsesEventsStreamingLifecycle(t *testing.T) {
	state := NewGeminiToResponsesStreamState("gemini-3.7-flash")
	first := GeminiEventToResponsesEvents(&antigravity.GeminiResponse{
		ResponseID: "resp_1",
		Candidates: []antigravity.GeminiCandidate{
			{
				Content: &antigravity.GeminiContent{
					Parts: []antigravity.GeminiPart{{Thought: true, Text: "thinking"}},
				},
			},
		},
	}, "resp_1", "gemini-3.7-flash", state)
	require.Equal(t, "response.created", first[0].Type)
	require.Equal(t, "reasoning", first[1].Item.Type)

	second := GeminiEventToResponsesEvents(&antigravity.GeminiResponse{
		Candidates: []antigravity.GeminiCandidate{
			{
				Content: &antigravity.GeminiContent{
					Parts: []antigravity.GeminiPart{{Text: "answer"}},
				},
			},
		},
	}, "resp_1", "gemini-3.7-flash", state)
	require.Contains(t, responseEventTypes(second), "response.output_text.delta")

	terminal := GeminiEventToResponsesEvents(&antigravity.GeminiResponse{
		Candidates: []antigravity.GeminiCandidate{{FinishReason: "STOP"}},
		UsageMetadata: &antigravity.GeminiUsageMetadata{
			PromptTokenCount: 10, CandidatesTokenCount: 5, ThoughtsTokenCount: 15, TotalTokenCount: 30,
		},
	}, "resp_1", "gemini-3.7-flash", state)
	require.Equal(t, "response.completed", terminal[len(terminal)-1].Type)
	require.Equal(t, 20, terminal[len(terminal)-1].Response.Usage.OutputTokens)
}

func responseEventTypes(events []ResponsesStreamEvent) []string {
	result := make([]string, 0, len(events))
	for _, event := range events {
		result = append(result, event.Type)
	}
	return result
}

func TestGeminiResponseToResponsesResponseNonStreaming(t *testing.T) {
	got := GeminiResponseToResponsesResponse(&antigravity.GeminiResponse{
		ResponseID: "resp_2", ModelVersion: "gemini-3.7-flash",
		Candidates: []antigravity.GeminiCandidate{{
			Content:      &antigravity.GeminiContent{Parts: []antigravity.GeminiPart{{Text: "ok"}}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &antigravity.GeminiUsageMetadata{PromptTokenCount: 3, CandidatesTokenCount: 2, TotalTokenCount: 5},
	}, "resp_2", "gemini-3.7-flash")
	require.Equal(t, "completed", got.Status)
	require.Equal(t, "ok", got.Output[0].Content[0].Text)
	require.Equal(t, 5, got.Usage.TotalTokens)
}

func TestResponsesToGeminiRequestToolChoiceModes(t *testing.T) {
	tests := []struct {
		name            string
		toolChoiceJSON  string
		expectedMode    string
		expectedNames   []string
		expectNilConfig bool
	}{
		{
			name:           "required tool choice",
			toolChoiceJSON: `"required"`,
			expectedMode:   "ANY",
		},
		{
			name:           "none tool choice",
			toolChoiceJSON: `"none"`,
			expectedMode:   "NONE",
		},
		{
			name:           "named function object with name",
			toolChoiceJSON: `{"type":"function","name":"get_weather"}`,
			expectedMode:   "ANY",
			expectedNames:  []string{"get_weather"},
		},
		{
			name:           "named function object with nested function name",
			toolChoiceJSON: `{"type":"function","function":{"name":"fetch_profile"}}`,
			expectedMode:   "ANY",
			expectedNames:  []string{"fetch_profile"},
		},
		{
			name:            "auto or unhandled tool choice yields nil toolConfig",
			toolChoiceJSON:  `"auto"`,
			expectNilConfig: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &ResponsesRequest{
				Model:      "gemini-3.7-flash",
				Input:      json.RawMessage(`"hello"`),
				ToolChoice: json.RawMessage(tc.toolChoiceJSON),
			}
			got, err := ResponsesToGeminiRequest(req, "gemini-3.7-flash")
			require.NoError(t, err)
			if tc.expectNilConfig {
				require.Nil(t, got.ToolConfig)
				return
			}
			require.NotNil(t, got.ToolConfig)
			require.NotNil(t, got.ToolConfig.FunctionCallingConfig)
			require.Equal(t, tc.expectedMode, got.ToolConfig.FunctionCallingConfig.Mode)
			require.Equal(t, tc.expectedNames, got.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
		})
	}
}

func TestResponsesToGeminiRequestInvalidAndHTTPImageURL(t *testing.T) {
	tests := []struct {
		name      string
		imageURL  string
		errSubstr string
	}{
		{
			name:      "http url rejected",
			imageURL:  "https://example.com/test.png",
			errSubstr: "gemini image input must be an inline data URI",
		},
		{
			name:      "malformed data uri without base64 marker",
			imageURL:  "data:image/png;utf8,hello",
			errSubstr: "invalid Gemini image data URI",
		},
		{
			name:      "malformed data uri without data payload",
			imageURL:  "data:image/png;base64,",
			errSubstr: "invalid Gemini image data URI",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &ResponsesRequest{
				Model: "gemini-3.7-flash",
				Input: json.RawMessage(`[
					{"role":"user","content":[
						{"type":"input_image","image_url":"` + tc.imageURL + `"}
					]}
				]`),
			}
			got, err := ResponsesToGeminiRequest(req, "gemini-3.7-flash")
			require.Error(t, err)
			require.Nil(t, got)
			require.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

func TestGeminiResponseToResponsesResponseFinishReasons(t *testing.T) {
	tests := []struct {
		name                  string
		finishReason          string
		expectedStatus        string
		expectedIncompleteWhy string
		expectedErrorCode     string
	}{
		{
			name:                  "MAX_TOKENS maps to incomplete max_output_tokens",
			finishReason:          "MAX_TOKENS",
			expectedStatus:        "incomplete",
			expectedIncompleteWhy: "max_output_tokens",
		},
		{
			name:                  "LENGTH maps to incomplete max_output_tokens",
			finishReason:          "LENGTH",
			expectedStatus:        "incomplete",
			expectedIncompleteWhy: "max_output_tokens",
		},
		{
			name:                  "SAFETY maps to incomplete content_filter",
			finishReason:          "SAFETY",
			expectedStatus:        "incomplete",
			expectedIncompleteWhy: "content_filter",
		},
		{
			name:                  "RECITATION maps to incomplete content_filter",
			finishReason:          "RECITATION",
			expectedStatus:        "incomplete",
			expectedIncompleteWhy: "content_filter",
		},
		{
			name:              "MALFORMED_FUNCTION_CALL maps to failed upstream_error",
			finishReason:      "MALFORMED_FUNCTION_CALL",
			expectedStatus:    "failed",
			expectedErrorCode: "upstream_error",
		},
		{
			name:              "OTHER maps to failed upstream_error",
			finishReason:      "OTHER",
			expectedStatus:    "failed",
			expectedErrorCode: "upstream_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := &antigravity.GeminiResponse{
				ResponseID:   "resp_test",
				ModelVersion: "gemini-3.7-flash",
				Candidates: []antigravity.GeminiCandidate{{
					Content:      &antigravity.GeminiContent{Parts: []antigravity.GeminiPart{{Text: "partial"}}},
					FinishReason: tc.finishReason,
				}},
			}
			got := GeminiResponseToResponsesResponse(resp, "resp_test", "gemini-3.7-flash")
			require.NotNil(t, got)
			require.Equal(t, tc.expectedStatus, got.Status)
			if tc.expectedIncompleteWhy != "" {
				require.NotNil(t, got.IncompleteDetails)
				require.Equal(t, tc.expectedIncompleteWhy, got.IncompleteDetails.Reason)
			} else {
				require.Nil(t, got.IncompleteDetails)
			}
			if tc.expectedErrorCode != "" {
				require.NotNil(t, got.Error)
				require.Equal(t, tc.expectedErrorCode, got.Error.Code)
			} else {
				require.Nil(t, got.Error)
			}
		})
	}
}

func TestGeminiEventToResponsesEventsStreamingFinishReasons(t *testing.T) {
	tests := []struct {
		finishReason      string
		expectedEventType string
		expectedStatus    string
	}{
		{finishReason: "MAX_TOKENS", expectedEventType: "response.incomplete", expectedStatus: "incomplete"},
		{finishReason: "SAFETY", expectedEventType: "response.incomplete", expectedStatus: "incomplete"},
		{finishReason: "MALFORMED_FUNCTION_CALL", expectedEventType: "response.failed", expectedStatus: "failed"},
	}

	for _, tc := range tests {
		t.Run(tc.finishReason, func(t *testing.T) {
			state := NewGeminiToResponsesStreamState("gemini-3.7-flash")
			events := GeminiEventToResponsesEvents(&antigravity.GeminiResponse{
				ResponseID: "resp_stream_test",
				Candidates: []antigravity.GeminiCandidate{
					{
						Content:      &antigravity.GeminiContent{Parts: []antigravity.GeminiPart{{Text: "stream text"}}},
						FinishReason: tc.finishReason,
					},
				},
			}, "resp_stream_test", "gemini-3.7-flash", state)

			last := events[len(events)-1]
			require.Equal(t, tc.expectedEventType, last.Type)
			require.NotNil(t, last.Response)
			require.Equal(t, tc.expectedStatus, last.Response.Status)
		})
	}
}
