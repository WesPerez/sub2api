package service

import (
	"context"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/resinrecovery"
	"github.com/tidwall/gjson"
)

// Reuse the Responses parser for buffered replies, including stream=false
// requests whose upstream still returns SSE. This never changes the response.
func reportOpenAIResinBuffered(ctx context.Context, resp *http.Response, body []byte) {
	if ctx.Err() != nil || !resinrecovery.IsManaged(resp.Body) {
		return
	}
	outcome := resinrecovery.InvalidStream
	if isEventStreamResponse(resp.Header) || bodyHasSSEFraming(body) {
		_, payload, terminal := extractOpenAISSETerminalEvent(string(body))
		if terminal && gjson.ValidBytes(payload) {
			outcome = resinrecovery.Reachable
		} else {
			outcome = resinrecovery.EmptyStream
		}
	} else if gjson.ValidBytes(body) && gjson.ParseBytes(body).IsObject() {
		if gjson.GetBytes(body, "object").String() == "response" ||
			(gjson.GetBytes(body, "id").String() != "" && gjson.GetBytes(body, "output").IsArray()) ||
			gjson.GetBytes(body, "error.message").String() != "" {
			outcome = resinrecovery.Reachable
		}
	}
	resinrecovery.Report(ctx, resp.Body, outcome)
}
