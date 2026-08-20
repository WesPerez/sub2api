package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

type antigravityDirectResponsesStreamSession struct {
	state          *apicompat.GeminiToResponsesStreamState
	writer         *antigravityClientWriter
	pending        []apicompat.ResponsesStreamEvent
	usage          *ClaudeUsage
	firstTokenMs   *int
	startTime      time.Time
	meaningfulData bool
}

func newAntigravityDirectResponsesStreamSession(
	model string,
	startTime time.Time,
	writer *antigravityClientWriter,
	mapping apicompat.ResponsesClientToolMapping,
) *antigravityDirectResponsesStreamSession {
	return &antigravityDirectResponsesStreamSession{
		state:     apicompat.NewGeminiToResponsesStreamStateWithMapping(model, mapping),
		writer:    writer,
		usage:     &ClaudeUsage{},
		startTime: startTime,
	}
}

func (s *antigravityDirectResponsesStreamSession) consume(line string) error {
	payload, ok := antigravitySSEData(line)
	if !ok {
		return nil
	}
	response, responseID, modelVersion, err := apicompat.DecodeGeminiEventPayload([]byte(payload))
	if err != nil {
		return err
	}
	events := apicompat.GeminiEventToResponsesEvents(response, responseID, modelVersion, s.state)
	if len(events) == 0 {
		return nil
	}
	s.pending = append(s.pending, events...)
	if !s.meaningfulData && geminiResponseHasMeaningfulData(response) {
		s.meaningfulData = true
		ms := int(time.Since(s.startTime).Milliseconds())
		s.firstTokenMs = &ms
	}
	if s.meaningfulData {
		return s.flushPending()
	}
	return nil
}

func (s *antigravityDirectResponsesStreamSession) finish(c *gin.Context) (*antigravityStreamResult, error) {
	if !s.meaningfulData {
		return nil, antigravityCompatEmptyStreamError()
	}
	if !s.state.CompletedSent {
		s.collectUsage()
		result := &antigravityStreamResult{
			usage:            s.usage,
			firstTokenMs:     s.firstTokenMs,
			clientDisconnect: s.writer.Disconnected(),
		}
		if s.writer.Disconnected() {
			return result, nil
		}
		writeDirectResponsesStreamError(c, s.writer, "stream_read_error", "Gemini stream ended without finishReason")
		return result, errors.New("gemini stream ended without finishReason")
	}
	s.pending = append(s.pending, apicompat.FinalizeGeminiResponsesStream(s.state)...)
	if err := s.flushPending(); err != nil {
		return nil, err
	}
	s.collectUsage()
	return &antigravityStreamResult{
		usage:            s.usage,
		firstTokenMs:     s.firstTokenMs,
		clientDisconnect: s.writer.Disconnected(),
	}, nil
}

func (s *antigravityDirectResponsesStreamSession) flushPending() error {
	for _, event := range s.pending {
		encoded, err := apicompat.ResponsesEventToSSE(event)
		if err != nil {
			return fmt.Errorf("encode direct Responses event: %w", err)
		}
		s.writer.Write([]byte(encoded))
	}
	s.pending = nil
	return nil
}

func (s *antigravityDirectResponsesStreamSession) collectUsage() {
	if s.state == nil {
		return
	}
	inputTokens := s.state.PromptTokens - s.state.CachedContentTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	s.usage.InputTokens = inputTokens
	s.usage.CacheReadInputTokens = s.state.CachedContentTokens
	s.usage.OutputTokens = s.state.CandidatesTokens + s.state.ThoughtsTokens
	s.usage.ImageOutputTokens = s.state.ImageOutputTokens
}

func (s *AntigravityGatewayService) handleResponsesStreamingDirectFromAntigravity(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	mapping apicompat.ResponsesClientToolMapping,
) (*antigravityStreamResult, error) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	writer := newAntigravityClientWriter(c.Writer, flusher, "antigravity direct responses stream")
	writer.beforeFirstWrite = func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
	}
	session := newAntigravityDirectResponsesStreamSession(originalModel, startTime, writer, mapping)
	events, stopScanner, maxLineSize := s.startAntigravityCompatScanner(resp.Body)
	defer stopScanner()

	timeout := s.antigravityCompatStreamTimeout()
	timeoutTimer, timeoutCh := newAntigravityCompatTimer(timeout)
	if timeoutTimer != nil {
		defer timeoutTimer.Stop()
	}
	keepaliveTicker, keepaliveCh := s.newAntigravityCompatKeepaliveTicker()
	if keepaliveTicker != nil {
		defer keepaliveTicker.Stop()
	}

	for {
		select {
		case event, open := <-events:
			if !open {
				return session.finish(c)
			}
			if event.err != nil {
				return s.handleDirectResponsesReadError(c, session, event.err, maxLineSize)
			}
			resetAntigravityCompatTimer(timeoutTimer, timeout)
			s.observeAntigravityGeminiSSELine(c, event.line)
			if err := session.consume(event.line); err != nil {
				if !session.meaningfulData && !writer.Disconnected() {
					return nil, antigravityCompatEmptyStreamError()
				}
				writeDirectResponsesStreamError(c, writer, "upstream_error", "Failed to parse Gemini stream")
				return nil, fmt.Errorf("parse Gemini stream: %w", err)
			}

		case <-timeoutCh:
			if writer.Disconnected() {
				session.collectUsage()
				return &antigravityStreamResult{usage: session.usage, firstTokenMs: session.firstTokenMs, clientDisconnect: true}, nil
			}
			if !session.meaningfulData {
				return nil, antigravityCompatEmptyStreamError()
			}
			writeDirectResponsesStreamError(c, writer, "upstream_error", "stream_timeout")
			session.collectUsage()
			return &antigravityStreamResult{usage: session.usage, firstTokenMs: session.firstTokenMs}, fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if session.meaningfulData && !writer.Disconnected() {
				writer.Write([]byte(": ping\n\n"))
			}
		}
	}
}

func (s *AntigravityGatewayService) handleDirectResponsesReadError(
	c *gin.Context,
	session *antigravityDirectResponsesStreamSession,
	err error,
	maxLineSize int,
) (*antigravityStreamResult, error) {
	if !session.meaningfulData && !session.writer.Disconnected() {
		return nil, antigravityCompatEmptyStreamError()
	}
	if disconnect, handled := handleStreamReadError(err, session.writer.Disconnected(), "antigravity direct responses"); handled {
		session.collectUsage()
		return &antigravityStreamResult{usage: session.usage, firstTokenMs: session.firstTokenMs, clientDisconnect: disconnect}, nil
	}
	if errors.Is(err, bufio.ErrTooLong) {
		logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (responses direct): max_size=%d error=%v", maxLineSize, err)
		writeDirectResponsesStreamError(c, session.writer, "response_too_large", "Upstream response line too long")
		session.collectUsage()
		return &antigravityStreamResult{usage: session.usage, firstTokenMs: session.firstTokenMs}, err
	}
	writeDirectResponsesStreamError(c, session.writer, "stream_read_error", "Upstream stream read failed")
	return nil, fmt.Errorf("stream read error: %w", err)
}

func writeDirectResponsesStreamError(c *gin.Context, writer *antigravityClientWriter, code, message string) {
	payload, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": code, "message": message},
	})
	writer.Write([]byte("event: error\ndata: " + string(payload) + "\n\n"))
	MarkResponseCommitted(c)
}

func (s *AntigravityGatewayService) handleResponsesNonStreamingDirectFromAntigravity(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	mapping apicompat.ResponsesClientToolMapping,
) (*antigravityStreamResult, error) {
	state := apicompat.NewGeminiToResponsesStreamStateWithMapping(originalModel, mapping)
	events, stopScanner, maxLineSize := s.startAntigravityCompatScanner(resp.Body)
	defer stopScanner()

	timeout := s.antigravityCompatStreamTimeout()
	timeoutTimer, timeoutCh := newAntigravityCompatTimer(timeout)
	if timeoutTimer != nil {
		defer timeoutTimer.Stop()
	}
	meaningful := false
	var firstTokenMs *int
	var terminal *apicompat.ResponsesResponse

	for {
		select {
		case event, open := <-events:
			if !open {
				if !meaningful {
					return nil, antigravityCompatEmptyStreamError()
				}
				if !state.CompletedSent {
					return nil, antigravityCompatIncompleteStreamError()
				}
				for _, finalEvent := range apicompat.FinalizeGeminiResponsesStream(state) {
					if finalEvent.Response != nil {
						terminal = finalEvent.Response
					}
				}
				return writeDirectResponsesNonStreaming(c, state, terminal, firstTokenMs)
			}
			if event.err != nil {
				if errors.Is(event.err, bufio.ErrTooLong) {
					logger.LegacyPrintf("service.antigravity_gateway", "SSE line too long (responses direct buffered): max_size=%d error=%v", maxLineSize, event.err)
				}
				return nil, s.mapAntigravityCompatCollectionError(c, event.err)
			}
			resetAntigravityCompatTimer(timeoutTimer, timeout)
			payload, ok := antigravitySSEData(event.line)
			if !ok {
				continue
			}
			response, responseID, modelVersion, err := apicompat.DecodeGeminiEventPayload([]byte(payload))
			if err != nil {
				continue
			}
			s.observeAntigravityGeminiSSELine(c, event.line)
			if geminiResponseHasMeaningfulData(response) && !meaningful {
				meaningful = true
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			for _, responseEvent := range apicompat.GeminiEventToResponsesEvents(response, responseID, modelVersion, state) {
				if responseEvent.Response != nil && isTerminalResponsesEvent(responseEvent.Type) {
					terminal = responseEvent.Response
				}
			}

		case <-timeoutCh:
			return nil, s.mapAntigravityCompatCollectionError(c, fmt.Errorf("stream data interval timeout"))
		}
	}
}

func writeDirectResponsesNonStreaming(
	c *gin.Context,
	state *apicompat.GeminiToResponsesStreamState,
	terminal *apicompat.ResponsesResponse,
	firstTokenMs *int,
) (*antigravityStreamResult, error) {
	if terminal == nil {
		return nil, errors.New("gemini stream ended without a Responses terminal event")
	}
	payload, err := json.Marshal(terminal)
	if err != nil {
		return nil, fmt.Errorf("marshal direct Responses result: %w", err)
	}
	c.Data(http.StatusOK, "application/json", payload)

	inputTokens := state.PromptTokens - state.CachedContentTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	return &antigravityStreamResult{
		usage: &ClaudeUsage{
			InputTokens:          inputTokens,
			OutputTokens:         state.CandidatesTokens + state.ThoughtsTokens,
			CacheReadInputTokens: state.CachedContentTokens,
			ImageOutputTokens:    state.ImageOutputTokens,
		},
		firstTokenMs: firstTokenMs,
	}, nil
}

func antigravityCompatIncompleteStreamError() error {
	logger.LegacyPrintf("service.antigravity_gateway", "Incomplete Antigravity compatibility stream, triggering failover")
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           []byte(`{"error":"upstream stream ended without finishReason"}`),
		RetryableOnSameAccount: true,
	}
}

func antigravitySSEData(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return "", false
	}
	return payload, true
}

func geminiResponseHasMeaningfulData(response *antigravity.GeminiResponse) bool {
	if response == nil || len(response.Candidates) == 0 {
		return false
	}
	candidate := response.Candidates[0]
	if strings.TrimSpace(candidate.FinishReason) != "" {
		return true
	}
	if candidate.Content == nil {
		return false
	}
	for _, part := range candidate.Content.Parts {
		if part.Text != "" || part.Thought || part.FunctionCall != nil || (part.InlineData != nil && part.InlineData.Data != "") {
			return true
		}
	}
	return false
}

func isTerminalResponsesEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}
