package apicompat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

// GeminiToResponsesStreamState tracks a Gemini stream while emitting native
// Responses items and semantic SSE events.
type GeminiToResponsesStreamState struct {
	ResponseID     string
	Model          string
	Created        int64
	SequenceNumber int
	CreatedSent    bool
	CompletedSent  bool

	OutputIndex      int
	ContentIndex     int
	CurrentItemID    string
	CurrentItemType  string
	CurrentCallID    string
	CurrentName      string
	CurrentArgs      string
	CurrentSummary   string
	CurrentEncrypted string
	CurrentText      string
	CurrentContent   []ResponsesContentPart
	Outputs          []ResponsesOutput

	PromptTokens        int
	CandidatesTokens    int
	CachedContentTokens int
	ThoughtsTokens      int
	ImageOutputTokens   int
	TotalTokens         int
	FinishReason        string

	Restorer *ResponsesClientToolStreamRestorer
}

func NewGeminiToResponsesStreamState(model string) *GeminiToResponsesStreamState {
	return &GeminiToResponsesStreamState{Model: model, Created: time.Now().Unix()}
}

func NewGeminiToResponsesStreamStateWithMapping(model string, mapping ResponsesClientToolMapping) *GeminiToResponsesStreamState {
	state := NewGeminiToResponsesStreamState(model)
	if len(mapping.CustomTools) > 0 || mapping.ToolSearch || len(mapping.NamespaceTools) > 0 {
		state.Restorer = NewResponsesClientToolStreamRestorer(mapping)
	}
	return state
}

// DecodeGeminiEventPayload accepts direct Gemini responses plus the one- and
// two-level v1internal wrappers observed in Antigravity streams.
func DecodeGeminiEventPayload(payload []byte) (*antigravity.GeminiResponse, string, string, error) {
	if len(payload) == 0 {
		return nil, "", "", fmt.Errorf("empty Gemini event payload")
	}

	type envelope struct {
		Response     json.RawMessage `json:"response"`
		ResponseID   string          `json:"responseId"`
		ModelVersion string          `json:"modelVersion"`
	}

	candidate := json.RawMessage(payload)
	responseID := ""
	modelVersion := ""
	for depth := 0; depth < 3; depth++ {
		var wrapped envelope
		if err := json.Unmarshal(candidate, &wrapped); err != nil {
			return nil, "", "", fmt.Errorf("parse Gemini event envelope: %w", err)
		}
		if wrapped.ResponseID != "" {
			responseID = wrapped.ResponseID
		}
		if wrapped.ModelVersion != "" {
			modelVersion = wrapped.ModelVersion
		}
		if len(wrapped.Response) == 0 || string(wrapped.Response) == "null" {
			break
		}
		candidate = wrapped.Response
	}

	var response antigravity.GeminiResponse
	if err := json.Unmarshal(candidate, &response); err != nil {
		return nil, "", "", fmt.Errorf("parse Gemini response: %w", err)
	}
	if response.ResponseID != "" {
		responseID = response.ResponseID
	}
	if response.ModelVersion != "" {
		modelVersion = response.ModelVersion
	}
	if len(response.Candidates) == 0 && response.UsageMetadata == nil && responseID == "" && modelVersion == "" {
		return nil, "", "", fmt.Errorf("gemini event contains no response data")
	}
	return &response, responseID, modelVersion, nil
}

func GeminiEventToResponsesEvents(
	response *antigravity.GeminiResponse,
	responseID string,
	modelVersion string,
	state *GeminiToResponsesStreamState,
) []ResponsesStreamEvent {
	if response == nil || state == nil || state.CompletedSent {
		return nil
	}
	if state.ResponseID == "" {
		state.ResponseID = firstNonEmpty(responseID, response.ResponseID, generateResponsesID())
	}
	if state.Model == "" {
		state.Model = firstNonEmpty(modelVersion, response.ModelVersion)
	}
	updateGeminiResponsesUsage(state, response.UsageMetadata)

	events := make([]ResponsesStreamEvent, 0, 8)
	if !state.CreatedSent {
		state.CreatedSent = true
		events = append(events, makeGeminiResponsesCreatedEvent(state))
	}

	if len(response.Candidates) > 0 {
		candidate := response.Candidates[0]
		if candidate.Content != nil {
			for i := range candidate.Content.Parts {
				events = append(events, processGeminiResponsesPart(&candidate.Content.Parts[i], state)...)
			}
		}
		if candidate.FinishReason != "" {
			state.FinishReason = candidate.FinishReason
			events = append(events, finalizeGeminiResponsesStream(state, candidate.FinishReason)...)
		}
	}
	return restoreGeminiResponsesEvents(state, events)
}

func FinalizeGeminiResponsesStream(state *GeminiToResponsesStreamState) []ResponsesStreamEvent {
	if state == nil || !state.CreatedSent || state.CompletedSent {
		return nil
	}
	return restoreGeminiResponsesEvents(state, finalizeGeminiResponsesStream(state, state.FinishReason))
}

func processGeminiResponsesPart(part *antigravity.GeminiPart, state *GeminiToResponsesStreamState) []ResponsesStreamEvent {
	if part == nil {
		return nil
	}
	events := make([]ResponsesStreamEvent, 0, 8)

	if part.FunctionCall != nil {
		if part.ThoughtSignature != "" {
			events = append(events, preserveGeminiThoughtSignature(state, part.ThoughtSignature)...)
		}
		events = append(events, closeGeminiResponsesItem(state)...)
		state.CurrentItemID = generateItemID()
		state.CurrentItemType = "function_call"
		state.CurrentCallID = part.FunctionCall.ID
		if state.CurrentCallID == "" {
			state.CurrentCallID = generateGeminiResponsesCallID()
		}
		state.CurrentName = part.FunctionCall.Name
		state.CurrentArgs = "{}"
		if part.FunctionCall.Args != nil {
			if encoded, err := json.Marshal(part.FunctionCall.Args); err == nil {
				state.CurrentArgs = string(encoded)
			}
		}
		events = append(events,
			makeGeminiResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex,
				Item:        &ResponsesOutput{Type: "function_call", ID: state.CurrentItemID, CallID: state.CurrentCallID, Name: state.CurrentName, Status: "in_progress"},
			}),
			makeGeminiResponsesEvent(state, "response.function_call_arguments.delta", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, ItemID: state.CurrentItemID, CallID: state.CurrentCallID, Name: state.CurrentName, Delta: state.CurrentArgs,
			}),
			makeGeminiResponsesEvent(state, "response.function_call_arguments.done", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, ItemID: state.CurrentItemID, CallID: state.CurrentCallID, Name: state.CurrentName, Arguments: state.CurrentArgs,
			}),
		)
		events = append(events, closeGeminiResponsesItem(state)...)
		return events
	}

	if part.Thought {
		if state.CurrentItemType != "reasoning" {
			events = append(events, closeGeminiResponsesItem(state)...)
			events = append(events, openGeminiReasoningItem(state)...)
		}
		if part.ThoughtSignature != "" {
			state.CurrentEncrypted = part.ThoughtSignature
		}
		if part.Text != "" {
			state.CurrentSummary += part.Text
			events = append(events, makeGeminiResponsesEvent(state, "response.reasoning_summary_text.delta", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, SummaryIndex: 0, ItemID: state.CurrentItemID, Delta: part.Text,
			}))
		}
		return events
	}

	if part.ThoughtSignature != "" {
		events = append(events, preserveGeminiThoughtSignature(state, part.ThoughtSignature)...)
	}
	text := part.Text
	if part.InlineData != nil && part.InlineData.Data != "" {
		image := fmt.Sprintf("![image](data:%s;base64,%s)", part.InlineData.MimeType, part.InlineData.Data)
		if text != "" {
			text += "\n" + image
		} else {
			text = image
		}
	}
	if text == "" {
		return events
	}
	if state.CurrentItemType != "message" {
		events = append(events, closeGeminiResponsesItem(state)...)
		state.CurrentItemID = generateItemID()
		state.CurrentItemType = "message"
		state.ContentIndex = 0
		events = append(events,
			makeGeminiResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex,
				Item:        &ResponsesOutput{Type: "message", ID: state.CurrentItemID, Role: "assistant", Status: "in_progress"},
			}),
			makeGeminiResponsesEvent(state, "response.content_part.added", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, ContentIndex: state.ContentIndex, ItemID: state.CurrentItemID,
				Part: &ResponsesContentPart{Type: "output_text", Text: ""},
			}),
		)
	}
	state.CurrentText += text
	events = append(events, makeGeminiResponsesEvent(state, "response.output_text.delta", &ResponsesStreamEvent{
		OutputIndex: state.OutputIndex, ContentIndex: state.ContentIndex, ItemID: state.CurrentItemID, Delta: text,
	}))
	return events
}

func preserveGeminiThoughtSignature(state *GeminiToResponsesStreamState, signature string) []ResponsesStreamEvent {
	events := make([]ResponsesStreamEvent, 0, 3)
	if state.CurrentItemType != "reasoning" {
		events = append(events, closeGeminiResponsesItem(state)...)
		events = append(events, openGeminiReasoningItem(state)...)
	}
	state.CurrentEncrypted = signature
	events = append(events, closeGeminiResponsesItem(state)...)
	return events
}

func openGeminiReasoningItem(state *GeminiToResponsesStreamState) []ResponsesStreamEvent {
	state.CurrentItemID = generateItemID()
	state.CurrentItemType = "reasoning"
	return []ResponsesStreamEvent{makeGeminiResponsesEvent(state, "response.output_item.added", &ResponsesStreamEvent{
		OutputIndex: state.OutputIndex,
		Item:        &ResponsesOutput{Type: "reasoning", ID: state.CurrentItemID},
	})}
}

func closeGeminiResponsesItem(state *GeminiToResponsesStreamState) []ResponsesStreamEvent {
	if state.CurrentItemType == "" {
		return nil
	}
	events := make([]ResponsesStreamEvent, 0, 3)
	item := ResponsesOutput{Type: state.CurrentItemType, ID: state.CurrentItemID, Status: "completed"}
	switch state.CurrentItemType {
	case "message":
		item.Role = "assistant"
		item.Content = append(item.Content, ResponsesContentPart{Type: "output_text", Text: state.CurrentText})
		events = append(events,
			makeGeminiResponsesEvent(state, "response.output_text.done", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, ContentIndex: state.ContentIndex, ItemID: state.CurrentItemID, Text: state.CurrentText,
			}),
			makeGeminiResponsesEvent(state, "response.content_part.done", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, ContentIndex: state.ContentIndex, ItemID: state.CurrentItemID,
				Part: &ResponsesContentPart{Type: "output_text", Text: state.CurrentText},
			}),
		)
	case "function_call":
		item.CallID = state.CurrentCallID
		item.Name = state.CurrentName
		item.Arguments = state.CurrentArgs
	case "reasoning":
		item.EncryptedContent = state.CurrentEncrypted
		if state.CurrentSummary != "" {
			item.Summary = []ResponsesSummary{{Type: "summary_text", Text: state.CurrentSummary}}
			events = append(events, makeGeminiResponsesEvent(state, "response.reasoning_summary_text.done", &ResponsesStreamEvent{
				OutputIndex: state.OutputIndex, SummaryIndex: 0, ItemID: state.CurrentItemID, Text: state.CurrentSummary,
			}))
		}
	}
	state.Outputs = append(state.Outputs, item)
	events = append(events, makeGeminiResponsesEvent(state, "response.output_item.done", &ResponsesStreamEvent{
		OutputIndex: state.OutputIndex, Item: &item,
	}))

	state.OutputIndex++
	state.ContentIndex = 0
	state.CurrentItemID = ""
	state.CurrentItemType = ""
	state.CurrentCallID = ""
	state.CurrentName = ""
	state.CurrentArgs = ""
	state.CurrentSummary = ""
	state.CurrentEncrypted = ""
	state.CurrentText = ""
	state.CurrentContent = nil
	return events
}

func finalizeGeminiResponsesStream(state *GeminiToResponsesStreamState, finishReason string) []ResponsesStreamEvent {
	if state.CompletedSent {
		return nil
	}
	events := closeGeminiResponsesItem(state)
	status := "completed"
	eventType := "response.completed"
	var incomplete *ResponsesIncompleteDetails
	var responseError *ResponsesError
	switch strings.ToUpper(strings.TrimSpace(finishReason)) {
	case "MAX_TOKENS", "LENGTH":
		status = "incomplete"
		eventType = "response.incomplete"
		incomplete = &ResponsesIncompleteDetails{Reason: "max_output_tokens"}
	case "SAFETY", "RECITATION":
		status = "incomplete"
		eventType = "response.incomplete"
		incomplete = &ResponsesIncompleteDetails{Reason: "content_filter"}
	case "MALFORMED_FUNCTION_CALL", "OTHER":
		status = "failed"
		eventType = "response.failed"
		responseError = &ResponsesError{Code: "upstream_error", Message: "Gemini could not produce a valid response"}
	}
	response := &ResponsesResponse{
		ID:                state.ResponseID,
		Object:            "response",
		Model:             state.Model,
		Status:            status,
		Output:            append([]ResponsesOutput(nil), state.Outputs...),
		Usage:             geminiResponsesUsage(state),
		IncompleteDetails: incomplete,
		Error:             responseError,
	}
	events = append(events, makeGeminiResponsesEvent(state, eventType, &ResponsesStreamEvent{Response: response}))
	state.CompletedSent = true
	return events
}

func updateGeminiResponsesUsage(state *GeminiToResponsesStreamState, usage *antigravity.GeminiUsageMetadata) {
	if usage == nil {
		return
	}
	state.PromptTokens = usage.PromptTokenCount
	state.CandidatesTokens = usage.CandidatesTokenCount
	state.CachedContentTokens = usage.CachedContentTokenCount
	state.ThoughtsTokens = usage.ThoughtsTokenCount
	state.ImageOutputTokens = usage.ImageOutputTokens()
	state.TotalTokens = usage.TotalTokenCount
}

func geminiResponsesUsage(state *GeminiToResponsesStreamState) *ResponsesUsage {
	outputTokens := state.CandidatesTokens + state.ThoughtsTokens
	totalTokens := state.TotalTokens
	if totalTokens == 0 {
		totalTokens = state.PromptTokens + outputTokens
	}
	usage := &ResponsesUsage{
		InputTokens:  state.PromptTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}
	if state.CachedContentTokens > 0 {
		usage.InputTokensDetails = &ResponsesInputTokensDetails{CachedTokens: state.CachedContentTokens}
	}
	if state.ThoughtsTokens > 0 || state.ImageOutputTokens > 0 {
		usage.OutputTokensDetails = &ResponsesOutputTokensDetails{
			ReasoningTokens: state.ThoughtsTokens,
			ImageTokens:     state.ImageOutputTokens,
		}
	}
	return usage
}

func makeGeminiResponsesCreatedEvent(state *GeminiToResponsesStreamState) ResponsesStreamEvent {
	return makeGeminiResponsesEvent(state, "response.created", &ResponsesStreamEvent{Response: &ResponsesResponse{
		ID: state.ResponseID, Object: "response", Model: state.Model, Status: "in_progress", Output: []ResponsesOutput{},
	}})
}

func makeGeminiResponsesEvent(state *GeminiToResponsesStreamState, eventType string, template *ResponsesStreamEvent) ResponsesStreamEvent {
	event := *template
	event.Type = eventType
	event.SequenceNumber = state.SequenceNumber
	state.SequenceNumber++
	return event
}

func restoreGeminiResponsesEvents(state *GeminiToResponsesStreamState, events []ResponsesStreamEvent) []ResponsesStreamEvent {
	if state.Restorer == nil || len(events) == 0 {
		return events
	}
	restored := make([]ResponsesStreamEvent, 0, len(events))
	for _, event := range events {
		restored = append(restored, state.Restorer.Restore(event)...)
	}
	return restored
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func generateGeminiResponsesCallID() string {
	data := make([]byte, 12)
	_, _ = rand.Read(data)
	return "call_" + hex.EncodeToString(data)
}

func GeminiResponseToResponsesResponse(response *antigravity.GeminiResponse, responseID, model string) *ResponsesResponse {
	if response == nil {
		return nil
	}
	state := NewGeminiToResponsesStreamState(model)
	events := GeminiEventToResponsesEvents(response, responseID, response.ModelVersion, state)
	if !state.CompletedSent {
		events = append(events, FinalizeGeminiResponsesStream(state)...)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Response != nil && (events[i].Type == "response.completed" || events[i].Type == "response.incomplete" || events[i].Type == "response.failed") {
			return events[i].Response
		}
	}
	return nil
}
