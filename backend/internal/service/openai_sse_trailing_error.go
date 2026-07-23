package service

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const openAIFailedTrailingErrorWait = 250 * time.Millisecond

// Hard caps for a single in-flight SSE frame. Multi-line frames without a blank
// terminator must not grow unbounded (memory DoS via many tiny non-empty lines).
//
// The zero-value byte budget is intentionally tight. Native and passthrough
// handlers set an explicit initial budget bounded by the 8MiB first-output
// limit, then switch to MaxLineSize after semantic output. Aggregate lifecycle
// buffering remains separately bounded by openAIPassthroughPreSemanticMaxBytes
// or the disk-backed native first-output stage.
const (
	openAIWireSSEFrameMaxLines = 256
	openAIWireSSEFrameMaxBytes = 256 * 1024
)

// openAIWireSSEFrame keeps the original wire lines for ordinary forwarding and
// a normalized payload/type for protocol decisions.
type openAIWireSSEFrame struct {
	Lines     []string
	EventType string
	Payload   []byte
	HasData   bool
}

type openAIWireSSEFrameCollector struct {
	lines     []string
	byteCount int
	// maxBytes is the per-frame byte budget. Zero means the tight collector
	// default (openAIWireSSEFrameMaxBytes); handlers set their explicit bounds.
	maxBytes int
}

func (c *openAIWireSSEFrameCollector) maxFrameBytes() int {
	if c != nil && c.maxBytes > 0 {
		return c.maxBytes
	}
	return openAIWireSSEFrameMaxBytes
}

// SetMaxBytes updates the per-frame byte budget. n <= 0 restores the tight
// zero-value cap. The line count hard cap is never raised.
func (c *openAIWireSSEFrameCollector) SetMaxBytes(n int) {
	if c == nil {
		return
	}
	if n <= 0 {
		c.maxBytes = 0
		return
	}
	c.maxBytes = n
}

// Pending reports whether the collector is holding an incomplete multi-line frame.
func (c *openAIWireSSEFrameCollector) Pending() bool {
	return c != nil && len(c.lines) > 0
}

func (c *openAIWireSSEFrameCollector) AddLine(line string) (openAIWireSSEFrame, bool, error) {
	if c == nil {
		return openAIWireSSEFrame{}, false, nil
	}
	// Count the line plus the newline that formed it on the wire.
	nextBytes := c.byteCount + len(line) + 1
	nextLines := len(c.lines) + 1
	maxBytes := c.maxFrameBytes()
	if nextLines > openAIWireSSEFrameMaxLines || nextBytes > maxBytes {
		c.lines = nil
		c.byteCount = 0
		return openAIWireSSEFrame{}, false, fmt.Errorf(
			"openai SSE frame exceeds limit (lines=%d/%d bytes=%d/%d)",
			nextLines, openAIWireSSEFrameMaxLines, nextBytes, maxBytes,
		)
	}
	c.lines = append(c.lines, line)
	c.byteCount = nextBytes
	if line != "" {
		return openAIWireSSEFrame{}, false, nil
	}
	return c.takeFrame(), true, nil
}

func (c *openAIWireSSEFrameCollector) Finish() (openAIWireSSEFrame, bool) {
	if c == nil || len(c.lines) == 0 {
		return openAIWireSSEFrame{}, false
	}
	return c.takeFrame(), true
}

func (c *openAIWireSSEFrameCollector) takeFrame() openAIWireSSEFrame {
	lines := append([]string(nil), c.lines...)
	c.lines = c.lines[:0]
	c.byteCount = 0

	parser := &openAICompatSSEFrameParser{}
	parsed := openAICompatSSEFrame{}
	parsedOK := false
	for _, line := range lines {
		if frame, ok := parser.AddLine(line); ok {
			parsed = frame
			parsedOK = true
			break
		}
	}
	if !parsedOK {
		parsed, parsedOK = parser.Finish()
	}
	if !parsedOK {
		// Preserve event-only frames (event: line with no data) for protocol use.
		eventType := ""
		for _, line := range lines {
			if et, ok := extractOpenAISSEEventLine(line); ok {
				eventType = strings.TrimSpace(et)
			}
		}
		return openAIWireSSEFrame{Lines: lines, EventType: eventType}
	}

	payload := []byte(openAICompatPayloadWithEventType(parsed.Data, parsed.EventType))
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		eventType = strings.TrimSpace(parsed.EventType)
	}
	return openAIWireSSEFrame{
		Lines:     lines,
		EventType: eventType,
		Payload:   payload,
		HasData:   strings.TrimSpace(parsed.Data) != "",
	}
}

func canonicalOpenAIResponseFailedFrame(payload []byte) openAIWireSSEFrame {
	lines := []string{"event: response.failed"}
	for _, dataLine := range strings.Split(string(payload), "\n") {
		lines = append(lines, "data: "+dataLine)
	}
	lines = append(lines, "")
	return openAIWireSSEFrame{
		Lines:     lines,
		EventType: "response.failed",
		Payload:   append([]byte(nil), payload...),
		HasData:   true,
	}
}

type openAITrailingFailureGateResult struct {
	Frames         []openAIWireSSEFrame
	FailurePayload []byte
	FailureMessage string
	WaitStarted    bool
	WaitFinished   bool
}

type openAITrailingFailureGate struct {
	pendingPayload []byte
	pendingMessage string
}

func (g *openAITrailingFailureGate) Waiting() bool {
	return g != nil && len(g.pendingPayload) > 0
}

func (g *openAITrailingFailureGate) Accept(frame openAIWireSSEFrame, allowWait bool) openAITrailingFailureGateResult {
	if g == nil {
		return openAITrailingFailureGateResult{Frames: []openAIWireSSEFrame{frame}}
	}
	if g.Waiting() {
		// SSE comments and empty dispatches do not break adjacency. The wall-clock
		// deadline remains authoritative, so a comment-only stream cannot wait forever.
		if !frame.HasData && frame.EventType == "" {
			return openAITrailingFailureGateResult{}
		}
		if frame.EventType == "error" {
			payload, message, _ := mergeOpenAIStreamFailedWithTrailingError(
				g.pendingPayload,
				frame.Payload,
				g.pendingMessage,
			)
			g.clear()
			// Prefer same-account/pool failover for pre-semantic trailing rate_limit
			// merges instead of writing a synthetic failed frame downstream.
			if allowWait && openAIStreamFailedEventShouldFailover(payload, message) {
				return openAITrailingFailureGateResult{
					FailurePayload: payload,
					FailureMessage: message,
					WaitFinished:   true,
				}
			}
			return openAITrailingFailureGateResult{
				Frames:       []openAIWireSSEFrame{canonicalOpenAIResponseFailedFrame(payload)},
				WaitFinished: true,
			}
		}
		payload := append([]byte(nil), g.pendingPayload...)
		message := g.pendingMessage
		g.clear()
		return openAITrailingFailureGateResult{
			FailurePayload: payload,
			FailureMessage: message,
			WaitFinished:   true,
		}
	}

	if allowWait && frame.EventType == "response.failed" {
		message := extractOpenAISSEErrorMessage(frame.Payload)
		if openAIStreamFailedNeedsTrailingError(frame.Payload, message) {
			g.pendingPayload = append([]byte(nil), frame.Payload...)
			g.pendingMessage = message
			return openAITrailingFailureGateResult{WaitStarted: true}
		}
	}
	return openAITrailingFailureGateResult{Frames: []openAIWireSSEFrame{frame}}
}

func (g *openAITrailingFailureGate) Finish() openAITrailingFailureGateResult {
	if g == nil || !g.Waiting() {
		return openAITrailingFailureGateResult{}
	}
	payload := append([]byte(nil), g.pendingPayload...)
	message := g.pendingMessage
	g.clear()
	return openAITrailingFailureGateResult{
		FailurePayload: payload,
		FailureMessage: message,
		WaitFinished:   true,
	}
}

func (g *openAITrailingFailureGate) clear() {
	g.pendingPayload = nil
	g.pendingMessage = ""
}

type openAITrailingErrorDeadline struct {
	timer *time.Timer
}

func (d *openAITrailingErrorDeadline) Start(body io.Closer) {
	if d == nil || d.timer != nil || body == nil {
		return
	}
	d.timer = time.AfterFunc(openAIFailedTrailingErrorWait, func() {
		_ = body.Close()
	})
}

func (d *openAITrailingErrorDeadline) Stop() {
	if d == nil || d.timer == nil {
		return
	}
	d.timer.Stop()
	d.timer = nil
}
