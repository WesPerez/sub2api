package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type responsesAttemptBudgetKey struct{}

// One account attempt owns one cancellable transport lifetime, including all
// compatibility retries and protocol bridges. Its deadline never moves.
type responsesAttemptBudget struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	client         context.Context
	deadline       time.Time
	timer          *time.Timer
	stopClient     func() bool
	admitted       bool
	expired        bool
	expireObserved time.Time
	cancelReturned time.Time
}

func newResponsesAttemptBudget(ctx context.Context, deadline time.Time) *responsesAttemptBudget {
	transport, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b := &responsesAttemptBudget{ctx: transport, cancel: cancel, client: ctx, deadline: deadline}
	b.stopClient = context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.admitted {
			b.cancel()
		}
	})
	b.timer = time.AfterFunc(time.Until(deadline), func() {
		observed := time.Now()
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.admitted {
			if b.expireObserved.IsZero() {
				b.expireObserved = observed
			}
			b.expired = true
			b.cancel()
			b.cancelReturned = time.Now()
		}
	})
	return b
}

func responsesAttemptFromContext(ctx context.Context) *responsesAttemptBudget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(responsesAttemptBudgetKey{}).(*responsesAttemptBudget)
	return b
}

func responsesAttemptError(ctx context.Context) error {
	if b := responsesAttemptFromContext(ctx); b != nil {
		return b.err()
	}
	return nil
}

func (b *responsesAttemptBudget) errLocked() error {
	if b.admitted {
		return nil
	}
	if err := b.client.Err(); err != nil {
		return err
	}
	if b.expired || !time.Now().Before(b.deadline) {
		if b.expireObserved.IsZero() {
			b.expireObserved = time.Now()
		}
		b.expired = true
		b.cancel()
		if b.cancelReturned.IsZero() {
			b.cancelReturned = time.Now()
		}
		return context.DeadlineExceeded
	}
	return nil
}

func (b *responsesAttemptBudget) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.errLocked()
}

// Admission is after bounded staging and immediately before the first write.
// A failed downstream write cannot reopen retry eligibility.
func (b *responsesAttemptBudget) admit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.errLocked(); err != nil {
		return err
	}
	b.admitted = true
	b.timer.Stop()
	b.stopClient()
	return nil
}

func (b *responsesAttemptBudget) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timer.Stop()
	b.stopClient()
	b.cancel()
}

// Captured in memory at the cancellation boundary; logging here would itself
// perturb the deadline. The Ops event is persisted after response delivery.
func (b *responsesAttemptBudget) timeoutDetail(start time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("deadline_ms=%d cancel_observed_ms=%d cancel_returned_ms=%d forward_return_ms=%d",
		b.deadline.Sub(start).Milliseconds(), b.expireObserved.Sub(start).Milliseconds(),
		b.cancelReturned.Sub(start).Milliseconds(), time.Since(start).Milliseconds())
}

type responsesBudgetTransportContext struct {
	context.Context
	values context.Context
}

func (c responsesBudgetTransportContext) Value(key any) any { return c.values.Value(key) }

// This wrapper is only installed for a request carrying the shared Responses
// budget. Existing unbudgeted transports, compact and WebSocket stay unchanged.
func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if _, enabled := OpenAIResponsesFirstOutputBudgetRemaining(ctx); !enabled || c == nil || c.Writer == nil {
		return s.forwardResponsesAttempt(ctx, c, account, body)
	}
	start := time.Now()
	effort := gjson.GetBytes(body, "reasoning.effort").String()
	timeout := s.openAIFirstOutputAttemptTimeout(ctx, start, effort)
	b := newResponsesAttemptBudget(ctx, start.Add(timeout))
	defer b.close()
	ctx = context.WithValue(ctx, responsesAttemptBudgetKey{}, b)
	request := c.Request
	if request != nil {
		c.Request = request.WithContext(ctx)
		defer func() { c.Request = request }()
	}
	original := c.Writer
	w := newResponsesBudgetWriter(original, b, gjson.GetBytes(body, "stream").Bool())
	if !w.stream {
		limit := config.DefaultUpstreamResponseReadMaxBytes
		if s.cfg != nil && s.cfg.Gateway.UpstreamResponseReadMaxBytes > 0 {
			limit = s.cfg.Gateway.UpstreamResponseReadMaxBytes
		}
		w.stage.limit = limit
	}
	c.Writer = w
	defer func() { c.Writer = original; _ = w.stage.Close() }()
	result, err := s.forwardResponsesAttempt(ctx, c, account, body)
	var retry *UpstreamFailoverError
	hasUsage := result != nil && (result.Usage.InputTokens > 0 || result.Usage.OutputTokens > 0 ||
		result.Usage.CacheReadInputTokens > 0 || result.Usage.CacheCreationInputTokens > 0)
	if !w.committed && w.terminalSuccess && (err != nil || (!w.terminalOutput && !hasUsage)) {
		// Converters can synthesize an empty completed at EOF. A completed
		// frame is not evidence of success without output, and cannot hide an
		// accompanying read error. Nothing from this attempt has been sent.
		w.terminal = false
		c.Set(ResponseCommittedKey, false)
		if err == nil {
			err = &UpstreamFailoverError{StatusCode: http.StatusBadGateway,
				ResponseBody:             []byte(`{"error":{"type":"empty_response","message":"Upstream completed without usable output"}}`),
				SafeToFailoverAfterWrite: true, RequestScopedTransient: true}
			result = nil
		}
	}
	if !errors.As(err, &retry) && !w.committed && w.terminal && w.err == nil {
		w.mu.Lock()
		w.err = w.commit()
		w.mu.Unlock()
		if w.err == nil {
			// This marker means a complete response was delivered, not merely
			// that the first delta was admitted. Partial streams still need the
			// handler's error terminal if the upstream stalls or disconnects.
			MarkResponseCommitted(c)
			original.Flush()
		}
	}
	if budgetErr := b.err(); budgetErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A shared deadline is a request limit, not an account health verdict.
		appendOpsUpstreamErrorWithPolicy(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			Kind: "first_output_timeout", Message: "Responses attempt exceeded its pre-output budget",
			UpstreamStatusCode: http.StatusGatewayTimeout,
			Detail:             b.timeoutDetail(start),
		}, false)
		return nil, errors.Join(budgetErr, &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout,
			ResponseBody:        []byte(`{"error":{"type":"first_output_timeout","message":"Upstream produced no output before the deadline"}}`),
			FirstOutputTimedOut: true, SafeToFailoverAfterWrite: true, RequestScopedTransient: true})
	}
	if w.err != nil {
		if w.committed {
			return result, errors.Join(w.err, err)
		}
		return nil, errors.Join(w.err, err)
	}
	if err == nil && !w.committed && (w.stage.Buffered() > 0 || len(w.pending) > 0) {
		return nil, errors.New("responses upstream ended without a complete output event")
	}
	return result, err
}

// Keep protocol converters from committing lifecycle-only frames. Heartbeats
// remain real downstream bytes and retain the existing keepalive accounting.
type responsesBudgetWriter struct {
	gin.ResponseWriter
	mu              sync.Mutex
	budget          *responsesAttemptBudget
	headers         http.Header
	status          int
	stream          bool
	committed       bool
	terminal        bool
	terminalSuccess bool
	terminalOutput  bool
	sealed          bool
	bareError       bool
	stage           *openAIFirstOutputStage
	pending         []byte
	err             error
}

func newResponsesBudgetWriter(w gin.ResponseWriter, b *responsesAttemptBudget, stream bool) *responsesBudgetWriter {
	return &responsesBudgetWriter{ResponseWriter: w, budget: b, headers: w.Header().Clone(), status: http.StatusOK, stream: stream, stage: newDefaultOpenAIFirstOutputStage()}
}

func (w *responsesBudgetWriter) Header() http.Header { return w.headers }
func (w *responsesBudgetWriter) Status() int         { return w.status }
func (w *responsesBudgetWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Size()
}
func (w *responsesBudgetWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ResponseWriter.Written()
}
func (w *responsesBudgetWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.committed {
		w.status = status
	} else {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responsesBudgetWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		w.ResponseWriter.WriteHeaderNow()
	}
}
func (w *responsesBudgetWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed || w.ResponseWriter.Written() {
		w.ResponseWriter.Flush()
	}
}
func (w *responsesBudgetWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *responsesBudgetWriter) commit() error {
	if err := w.budget.admit(); err != nil {
		return err
	}
	w.committed = true
	if !w.stream && !w.ResponseWriter.Written() {
		// A complete buffered response must not wait for handler cleanup/EOF.
		w.headers.Set("Content-Length", strconv.FormatInt(w.stage.Buffered(), 10))
	}
	for key, values := range w.headers {
		w.ResponseWriter.Header()[key] = append([]string(nil), values...)
	}
	w.ResponseWriter.WriteHeader(w.status)
	return w.stage.CommitTo(w.ResponseWriter)
}

func (w *responsesBudgetWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.committed {
		return w.ResponseWriter.Write(p)
	}
	if err := w.budget.err(); err != nil {
		w.err = err
		return 0, err
	}
	if bytes.HasPrefix(p, []byte(":")) && bytes.HasSuffix(p, []byte("\n\n")) && !bytes.Contains(p, []byte("\ndata:")) {
		for _, key := range []string{"Content-Type", "Cache-Control", "Connection", "X-Accel-Buffering"} {
			if value := w.headers.Get(key); value != "" {
				w.ResponseWriter.Header().Set(key, value)
			}
		}
		return w.ResponseWriter.Write(p)
	}
	if int64(len(w.pending)+len(p))+w.stage.Buffered() > w.stage.limit {
		w.err = errOpenAIFirstOutputStageLimit
		return 0, w.err
	}
	w.pending = append(w.pending, p...)
	if !w.stream || !strings.Contains(w.headers.Get("Content-Type"), "text/event-stream") {
		if !gjson.ValidBytes(w.pending) {
			return len(p), nil
		}
		w.terminalSuccess = w.status < 400 && !gjson.GetBytes(w.pending, "error").Exists()
		w.terminalOutput = responsesBudgetResponseHasOutput(gjson.ParseBytes(w.pending))
		if w.stream && w.ResponseWriter.Written() {
			// A keepalive already committed SSE headers; JSON errors must retain
			// one SSE terminal rather than being appended as bare JSON.
			payload := append([]byte(nil), w.pending...)
			if err := w.stage.Close(); err != nil {
				w.err = err
				return 0, err
			}
			w.stage = newDefaultOpenAIFirstOutputStage()
			w.headers.Set("Content-Type", "text/event-stream")
			terminal := buildOpenAIResponseFailedSSE("", "", 0, payload, "Upstream request failed")
			if gjson.GetBytes(payload, "output").Exists() && !gjson.GetBytes(payload, "error").Exists() {
				encoded, err := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(payload)})
				if err != nil {
					w.err = err
					return 0, err
				}
				terminal = "data: " + string(encoded) + "\n\n"
			}
			w.pending = []byte(terminal)
		}
		if _, err := w.stage.Write(w.pending); err != nil {
			w.err = err
			return 0, err
		}
		w.pending = nil
		w.terminal = true
		return len(p), nil
	}
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		delim := 2
		if cr := bytes.Index(w.pending, []byte("\r\n\r\n")); cr >= 0 && (end < 0 || cr < end) {
			end, delim = cr, 4
		}
		if end < 0 {
			return len(p), nil
		}
		frame := w.pending[:end+delim]
		_, frameKind := responsesBudgetFrameData(frame)
		if w.bareError && frameKind != "response.failed" && frameKind != "response.completed" && frameKind != "response.done" {
			w.pending = w.pending[end+delim:]
			continue
		}
		if w.sealed {
			payload, _ := responsesBudgetFrameData(frame)
			w.pending = w.pending[end+delim:]
			if strings.TrimSpace(payload) == "[DONE]" {
				if _, err := w.stage.Write(frame); err != nil {
					w.err = err
					return 0, err
				}
			}
			continue
		}
		if _, err := w.stage.Write(frame); err != nil {
			w.err = err
			return 0, err
		}
		admit := responsesBudgetFrameHasOutput(frame)
		w.terminal = w.terminal || responsesBudgetFrameIsTerminal(frame)
		payload, kind := responsesBudgetFrameData(frame)
		if kind == "response.completed" || kind == "response.done" {
			w.terminalSuccess = true
			w.terminalOutput = responsesBudgetResponseHasOutput(gjson.Get(payload, "response"))
		}
		w.bareError = kind == "error"
		w.sealed = kind != "error" && openAIStreamEventTypeIsTerminal(kind)
		w.pending = w.pending[end+delim:]
		if !admit {
			continue
		}
		if err := w.commit(); err != nil {
			w.err = err
			return 0, err
		}
		if len(w.pending) > 0 {
			_, w.err = w.ResponseWriter.Write(w.pending)
			w.pending = nil
		}
		return len(p), w.err
	}
}

func responsesBudgetResponseHasOutput(response gjson.Result) bool {
	// Usage-only completed is an existing valid response contract. It closes
	// the response; it does not admit an ongoing metadata-only stream.
	if response.Get("usage").Exists() {
		return true
	}
	for _, item := range response.Get("output").Array() {
		kind := item.Get("type").String()
		if kind != "" && kind != "message" && kind != "reasoning" && kind != "function_call" && kind != "custom_tool_call" && kind != "image_generation_call" {
			// Completed structured tool outputs carry meaning in fields such as
			// action/results. Do not require unknown tool types to contain text.
			return true
		}
		for _, key := range []string{"arguments", "input", "result", "audio.data", "image_url"} {
			if strings.TrimSpace(item.Get(key).String()) != "" {
				return true
			}
		}
		for _, key := range []string{"content", "summary"} {
			for _, part := range item.Get(key).Array() {
				for _, field := range []string{"text", "transcript", "refusal", "audio", "image_url"} {
					if strings.TrimSpace(part.Get(field).String()) != "" {
						return true
					}
				}
			}
		}
	}
	return false
}

func responsesBudgetFrameData(frame []byte) (string, string) {
	var data []string
	eventType := ""
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if value, ok := extractOpenAISSEDataLine(strings.TrimSuffix(line, "\r")); ok {
			data = append(data, value)
		}
	}
	payload := strings.Join(data, "\n")
	kind := gjson.Get(payload, "type").String()
	if kind == "" {
		kind = eventType
	}
	return payload, kind
}

func responsesBudgetFrameIsTerminal(frame []byte) bool {
	payload, kind := responsesBudgetFrameData(frame)
	return gjson.Valid(payload) && (openAIStreamEventTypeIsTerminal(kind) || kind == "error")
}

func responsesBudgetFrameHasOutput(frame []byte) bool {
	payload, kind := responsesBudgetFrameData(frame)
	if !gjson.Valid(payload) || openAIStreamEventTypeIsTerminal(kind) || kind == "error" {
		return false
	}
	if !openAIStreamDataStartsVisibleOutput(payload, kind) {
		return false
	}
	// Whitespace-only text/reasoning is not useful first output.
	for _, key := range []string{"delta", "text", "part.text"} {
		if value := gjson.Get(payload, key); value.Type == gjson.String {
			return strings.TrimSpace(value.String()) != ""
		}
	}
	return true
}

var _ io.StringWriter = (*responsesBudgetWriter)(nil)
