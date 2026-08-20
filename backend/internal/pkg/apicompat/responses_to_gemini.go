package apicompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const (
	defaultGeminiResponsesMaxOutputTokens = 32768
	maxGeminiResponsesOutputTokens        = 65000
	geminiDummyThoughtSignature           = "dummy_thought_signature"
)

// ResponsesToGeminiRequest converts an OpenAI Responses request directly to
// Gemini generateContent. Codex-only tools must be lowered with
// AdaptResponsesClientTools before this function is called.
func ResponsesToGeminiRequest(req *ResponsesRequest, targetModel string) (*antigravity.GeminiRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("responses request is nil")
	}

	contents, systemInstruction, err := convertResponsesInputToGemini(req.Instructions, req.Input)
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("responses input contains no Gemini-compatible content")
	}

	tools, toolConfig := convertResponsesToolsToGemini(req.Tools)
	if explicit := convertResponsesToolChoiceToGemini(req.ToolChoice); explicit != nil {
		toolConfig = mergeGeminiToolConfig(toolConfig, explicit)
	}
	if hasMixedGeminiResponsesTools(tools) {
		if toolConfig == nil {
			toolConfig = &antigravity.GeminiToolConfig{}
		}
		enabled := true
		toolConfig.IncludeServerSideToolInvocations = &enabled
	}

	return &antigravity.GeminiRequest{
		Contents:          contents,
		SystemInstruction: systemInstruction,
		GenerationConfig:  buildGeminiGenerationConfigFromResponses(req, targetModel),
		Tools:             tools,
		ToolConfig:        toolConfig,
		SessionID:         stableGeminiResponsesSessionID(req, contents),
	}, nil
}

func convertResponsesInputToGemini(
	instructions string,
	inputRaw json.RawMessage,
) ([]antigravity.GeminiContent, *antigravity.GeminiContent, error) {
	systemTexts := make([]string, 0, 2)
	if text := strings.TrimSpace(instructions); text != "" {
		systemTexts = append(systemTexts, text)
	}

	var plain string
	if err := json.Unmarshal(inputRaw, &plain); err == nil {
		if plain == "" {
			return nil, geminiSystemInstruction(systemTexts), nil
		}
		return []antigravity.GeminiContent{{
			Role:  "user",
			Parts: []antigravity.GeminiPart{{Text: plain}},
		}}, geminiSystemInstruction(systemTexts), nil
	}

	var items []ResponsesInputItem
	if err := json.Unmarshal(inputRaw, &items); err != nil {
		return nil, nil, fmt.Errorf("parse responses input: %w", err)
	}

	callNames := make(map[string]string)
	for _, item := range items {
		if item.Type == "function_call" && item.CallID != "" && item.Name != "" {
			callNames[item.CallID] = item.Name
		}
	}

	contents := make([]antigravity.GeminiContent, 0, len(items))
	pendingThoughtSignature := ""
	for _, item := range items {
		switch {
		case item.Role == "system" || item.Role == "developer":
			if text := strings.TrimSpace(extractTextFromContent(item.Content)); text != "" {
				systemTexts = append(systemTexts, text)
			}

		case item.Type == "reasoning":
			if signature := strings.TrimSpace(item.EncryptedContent); signature != "" {
				pendingThoughtSignature = signature
			}

		case item.Type == "function_call":
			name := strings.TrimSpace(item.Name)
			if name == "" {
				return nil, nil, fmt.Errorf("function_call name is required")
			}
			arguments := any(map[string]any{})
			if raw := strings.TrimSpace(item.Arguments); raw != "" {
				if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
					arguments = map[string]any{"input": item.Arguments}
				}
			}
			signature := pendingThoughtSignature
			if signature == "" {
				signature = geminiDummyThoughtSignature
			}
			pendingThoughtSignature = ""
			appendGeminiContent(&contents, "model", []antigravity.GeminiPart{{
				ThoughtSignature: signature,
				FunctionCall: &antigravity.GeminiFunctionCall{
					Name: name,
					Args: arguments,
					ID:   item.CallID,
				},
			}})

		case item.Type == "function_call_output":
			name := strings.TrimSpace(callNames[item.CallID])
			if name == "" {
				name = strings.TrimSpace(item.Name)
			}
			if name == "" {
				name = "tool"
			}
			appendGeminiContent(&contents, "user", []antigravity.GeminiPart{{
				FunctionResponse: &antigravity.GeminiFunctionResponse{
					Name:     name,
					Response: responsesFunctionOutputToGemini(item),
					ID:       item.CallID,
				},
			}})

		case item.Role == "user" || item.Role == "assistant":
			parts, err := responsesContentToGeminiParts(item.Content)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) == 0 {
				continue
			}
			role := "user"
			if item.Role == "assistant" {
				role = "model"
			}
			appendGeminiContent(&contents, role, parts)

		default:
			if len(item.Content) == 0 {
				continue
			}
			parts, err := responsesContentToGeminiParts(item.Content)
			if err != nil {
				return nil, nil, err
			}
			appendGeminiContent(&contents, "user", parts)
		}
	}

	return contents, geminiSystemInstruction(systemTexts), nil
}

func geminiSystemInstruction(texts []string) *antigravity.GeminiContent {
	if len(texts) == 0 {
		return nil
	}
	return &antigravity.GeminiContent{
		Role:  "user",
		Parts: []antigravity.GeminiPart{{Text: strings.Join(texts, "\n\n")}},
	}
}

func appendGeminiContent(contents *[]antigravity.GeminiContent, role string, parts []antigravity.GeminiPart) {
	if len(parts) == 0 {
		return
	}
	if n := len(*contents); n > 0 && (*contents)[n-1].Role == role {
		(*contents)[n-1].Parts = append((*contents)[n-1].Parts, parts...)
		return
	}
	*contents = append(*contents, antigravity.GeminiContent{Role: role, Parts: parts})
}

func responsesContentToGeminiParts(raw json.RawMessage) ([]antigravity.GeminiPart, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []antigravity.GeminiPart{{Text: text}}, nil
	}

	var parts []ResponsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("parse responses message content: %w", err)
	}
	result := make([]antigravity.GeminiPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			if part.Text != "" {
				result = append(result, antigravity.GeminiPart{Text: part.Text})
			}
		case "input_image":
			inline, err := dataURIToGeminiInlineData(part.ImageURL)
			if err != nil {
				return nil, err
			}
			result = append(result, antigravity.GeminiPart{InlineData: inline})
		}
	}
	return result, nil
}

func dataURIToGeminiInlineData(dataURI string) (*antigravity.GeminiInlineData, error) {
	if !strings.HasPrefix(dataURI, "data:") {
		return nil, fmt.Errorf("gemini image input must be an inline data URI")
	}
	mediaAndData := strings.TrimPrefix(dataURI, "data:")
	separator := strings.Index(mediaAndData, ";base64,")
	if separator <= 0 || separator+8 >= len(mediaAndData) {
		return nil, fmt.Errorf("invalid Gemini image data URI")
	}
	return &antigravity.GeminiInlineData{
		MimeType: mediaAndData[:separator],
		Data:     mediaAndData[separator+8:],
	}, nil
}

func responsesFunctionOutputToGemini(item ResponsesInputItem) map[string]any {
	if len(item.outputRaw) > 0 {
		var object map[string]any
		if json.Unmarshal(item.outputRaw, &object) == nil && object != nil {
			return object
		}
		var parts []ResponsesContentPart
		if json.Unmarshal(item.outputRaw, &parts) == nil {
			texts := make([]string, 0, len(parts))
			for _, part := range parts {
				if part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
			if len(texts) > 0 {
				return map[string]any{"content": strings.Join(texts, "\n\n")}
			}
		}
	}

	output := item.Output
	if strings.TrimSpace(output) == "" {
		output = "(empty)"
	}
	var object map[string]any
	if json.Unmarshal([]byte(output), &object) == nil && object != nil {
		return object
	}
	return map[string]any{"content": output}
}

func buildGeminiGenerationConfigFromResponses(req *ResponsesRequest, model string) *antigravity.GeminiGenerationConfig {
	maxOutputTokens := defaultGeminiResponsesMaxOutputTokens
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		maxOutputTokens = *req.MaxOutputTokens
	}
	if maxOutputTokens > maxGeminiResponsesOutputTokens {
		maxOutputTokens = maxGeminiResponsesOutputTokens
	}
	config := &antigravity.GeminiGenerationConfig{
		MaxOutputTokens: maxOutputTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
	}

	effort := ""
	if req.Reasoning != nil {
		effort = normalizeGeminiResponsesEffort(req.Reasoning.Effort)
	}
	lowerModel := strings.ToLower(strings.TrimSpace(model))
	if isGemini37FlashModel(lowerModel) {
		if encoded := geminiModelEncodedEffort(lowerModel); encoded != "" {
			effort = encoded
		}
		if effort == "" {
			effort = "medium"
		}
		config.ThinkingConfig = &antigravity.GeminiThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   effort,
		}
		return config
	}
	if geminiModelEncodedEffort(lowerModel) != "" {
		return config
	}
	if effort == "" && !strings.Contains(lowerModel, "-thinking") {
		return config
	}
	budget := geminiResponsesThinkingBudget(effort)
	if budget <= 0 {
		budget = -1
	}
	if budget > 0 && budget >= maxOutputTokens {
		budget = maxOutputTokens - 1024
		if budget < 1024 {
			budget = 1024
		}
	}
	config.ThinkingConfig = &antigravity.GeminiThinkingConfig{
		IncludeThoughts: true,
		ThinkingBudget:  budget,
	}
	return config
}

func normalizeGeminiResponsesEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max", "ultra":
		return "high"
	default:
		return ""
	}
}

func isGemini37FlashModel(model string) bool {
	return strings.HasPrefix(model, "gemini-3.7-flash")
}

func geminiModelEncodedEffort(model string) string {
	switch {
	case strings.HasSuffix(model, "-low"):
		return "low"
	case strings.HasSuffix(model, "-medium"):
		return "medium"
	case strings.HasSuffix(model, "-high"):
		return "high"
	default:
		return ""
	}
}

func geminiResponsesThinkingBudget(effort string) int {
	switch effort {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 24576
	default:
		return -1
	}
}

func convertResponsesToolsToGemini(tools []ResponsesTool) ([]antigravity.GeminiToolDeclaration, *antigravity.GeminiToolConfig) {
	functions := make([]antigravity.GeminiFunctionDecl, 0, len(tools))
	search := false
	for _, tool := range tools {
		switch tool.Type {
		case "web_search", "web_search_preview", "google_search", "web_search_20250305":
			search = true
		case "function":
			if strings.TrimSpace(tool.Name) == "" {
				continue
			}
			functions = append(functions, antigravity.GeminiFunctionDecl{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  normalizeGeminiToolParameters(tool.Parameters),
			})
		}
	}

	declarations := make([]antigravity.GeminiToolDeclaration, 0, 2)
	if len(functions) > 0 {
		declarations = append(declarations, antigravity.GeminiToolDeclaration{FunctionDeclarations: functions})
	}
	if search {
		declarations = append(declarations, antigravity.GeminiToolDeclaration{GoogleSearch: &antigravity.GeminiGoogleSearch{}})
	}
	return declarations, nil
}

func normalizeGeminiToolParameters(schema json.RawMessage) map[string]any {
	empty := map[string]any{"type": "object", "properties": map[string]any{}}
	if len(bytes.TrimSpace(schema)) == 0 || bytes.Equal(bytes.TrimSpace(schema), []byte("null")) {
		return empty
	}
	var document map[string]any
	if json.Unmarshal(schema, &document) != nil || document == nil {
		return empty
	}
	antigravity.DeepCleanUndefined(document)
	cleaned := antigravity.CleanJSONSchema(document)
	if cleaned == nil {
		return empty
	}
	if _, ok := cleaned["type"]; !ok {
		cleaned["type"] = "object"
	}
	if _, ok := cleaned["properties"]; !ok {
		cleaned["properties"] = map[string]any{}
	}
	return cleaned
}

func convertResponsesToolChoiceToGemini(raw json.RawMessage) *antigravity.GeminiToolConfig {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "required":
			return geminiFunctionCallingConfig("ANY", "")
		case "none":
			return geminiFunctionCallingConfig("NONE", "")
		default:
			return nil
		}
	}
	var object struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &object) != nil || object.Type != "function" {
		return nil
	}
	name := strings.TrimSpace(object.Name)
	if name == "" {
		name = strings.TrimSpace(object.Function.Name)
	}
	if name == "" {
		return nil
	}
	return geminiFunctionCallingConfig("ANY", name)
}

func geminiFunctionCallingConfig(mode, name string) *antigravity.GeminiToolConfig {
	config := &antigravity.GeminiFunctionCallingConfig{Mode: mode}
	if name != "" {
		config.AllowedFunctionNames = []string{name}
	}
	return &antigravity.GeminiToolConfig{FunctionCallingConfig: config}
}

func mergeGeminiToolConfig(base, override *antigravity.GeminiToolConfig) *antigravity.GeminiToolConfig {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	if override.FunctionCallingConfig != nil {
		base.FunctionCallingConfig = override.FunctionCallingConfig
	}
	if override.IncludeServerSideToolInvocations != nil {
		base.IncludeServerSideToolInvocations = override.IncludeServerSideToolInvocations
	}
	return base
}

func hasMixedGeminiResponsesTools(tools []antigravity.GeminiToolDeclaration) bool {
	functions := false
	search := false
	for _, tool := range tools {
		functions = functions || len(tool.FunctionDeclarations) > 0
		search = search || tool.GoogleSearch != nil
	}
	return functions && search
}

func stableGeminiResponsesSessionID(req *ResponsesRequest, contents []antigravity.GeminiContent) string {
	seed := strings.TrimSpace(req.PromptCacheKey)
	if seed == "" {
		for _, content := range contents {
			if content.Role != "user" {
				continue
			}
			for _, part := range content.Parts {
				if part.Text != "" {
					seed = part.Text
					break
				}
			}
			if seed != "" {
				break
			}
		}
	}
	if seed == "" {
		encoded, _ := json.Marshal(contents)
		seed = string(encoded)
	}
	digest := sha256.Sum256([]byte(seed))
	value := binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff
	return "-" + strconv.FormatUint(value, 10)
}

func ResponsesToGeminiGenerateContentJSON(req *ResponsesRequest, targetModel string) ([]byte, error) {
	request, err := ResponsesToGeminiRequest(req, targetModel)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(request); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}
