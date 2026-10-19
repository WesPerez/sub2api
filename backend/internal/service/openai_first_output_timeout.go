package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const (
	openAIFirstOutputStageMemoryLimit        = 64 * 1024
	openAIFirstOutputStageMaxBytes           = 8 * 1024 * 1024
	openAIFirstOutputScannerFramingAllowance = 64
	openAIFirstOutputGuardQueueSize          = 1
	openAIDefaultStreamQueueSize             = 16
)

var (
	errOpenAIFirstOutputStageLimit   = errors.New("openai first-output staging limit exceeded")
	errOpenAIFirstOutputScannerLimit = errors.New("openai pre-output scanner token limit exceeded")
)

type openAIFirstOutputStage struct {
	limit      int64
	size       int64
	memory     bytes.Buffer
	tempFile   *os.File
	tempPath   string
	createTemp func() (*os.File, error)
	removeFile func(string) error
	memoryOnly bool
	cleanupErr error
	closed     bool
}

func newOpenAIFirstOutputStage(limit int64) *openAIFirstOutputStage {
	if limit < 1 {
		limit = 1
	}
	return &openAIFirstOutputStage{
		limit:      limit,
		createTemp: func() (*os.File, error) { return os.CreateTemp("", "sub2api-openai-first-output-*") },
		removeFile: os.Remove,
		memoryOnly: runtime.GOOS == "windows",
	}
}

func newDefaultOpenAIFirstOutputStage() *openAIFirstOutputStage {
	return newOpenAIFirstOutputStage(openAIFirstOutputStageMaxBytes)
}

func openAIFirstOutputEventQueueSize(guardFirstOutput bool) int {
	if guardFirstOutput {
		return openAIFirstOutputGuardQueueSize
	}
	return openAIDefaultStreamQueueSize
}

func openAIFirstOutputDynamicScanLines(guardActive *atomic.Bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		if err != nil || guardActive == nil || !guardActive.Load() {
			return advance, token, err
		}
		limit := openAIFirstOutputStageMaxBytes + openAIFirstOutputScannerFramingAllowance
		if token != nil {
			if len(token) > limit {
				return 0, nil, errOpenAIFirstOutputScannerLimit
			}
			return advance, token, nil
		}
		// At the limit with no delimiter, another byte would necessarily exceed
		// the guarded token budget. Fail before Scanner grows toward MaxLineSize.
		if len(data) >= limit {
			return 0, nil, errOpenAIFirstOutputScannerLimit
		}
		return advance, token, nil
	}
}

func (s *openAIFirstOutputStage) Buffered() int64 {
	if s == nil {
		return 0
	}
	return s.size
}

func (s *openAIFirstOutputStage) WriteString(value string) (int, error) {
	if err := s.prepareWrite(len(value)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.WriteString(value)
	} else {
		n, err = io.WriteString(s.tempFile, value)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) Write(p []byte) (int, error) {
	if err := s.prepareWrite(len(p)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.Write(p)
	} else {
		n, err = s.tempFile.Write(p)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) prepareWrite(incoming int) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if int64(incoming) > s.limit-s.size {
		return fmt.Errorf("%w: buffered=%d incoming=%d limit=%d", errOpenAIFirstOutputStageLimit, s.size, incoming, s.limit)
	}
	if s.tempFile != nil || s.memoryOnly || s.size+int64(incoming) <= openAIFirstOutputStageMemoryLimit {
		return nil
	}
	file, err := s.createTemp()
	if err != nil {
		return fmt.Errorf("create first-output spool: %w", err)
	}
	path := file.Name()
	// Unlink before writing any request data. Unix keeps the file descriptor
	// readable, while crashes and SIGKILL cannot leave a named plaintext spool.
	if unlinkErr := s.removeFile(path); unlinkErr != nil {
		closeErr := file.Close()
		removeErr := s.removeFile(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		s.memoryOnly = true
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = path
		}
		s.cleanupErr = errors.Join(
			s.cleanupErr,
			fmt.Errorf("unlink first-output spool before use: %w", unlinkErr),
			closeErr,
			removeErr,
		)
		return nil
	}
	if _, err := file.Write(s.memory.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("initialize first-output spool: %w", err)
	}
	s.tempFile = file
	s.tempPath = path
	s.memory.Reset()
	return nil
}

func (s *openAIFirstOutputStage) CommitTo(dst io.Writer) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if s.tempFile == nil {
		if _, err := io.Copy(dst, bytes.NewReader(s.memory.Bytes())); err != nil {
			return err
		}
	} else {
		if _, err := s.tempFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek first-output spool: %w", err)
		}
		if _, err := io.CopyN(dst, s.tempFile, s.size); err != nil {
			return err
		}
	}
	if err := s.Close(); err != nil {
		// Delivery succeeded. Preserve cleanup failures for the handler's deferred
		// cleanup/logging pass instead of turning committed bytes into a stream error.
		s.cleanupErr = errors.Join(s.cleanupErr, err)
	}
	return nil
}

func (s *openAIFirstOutputStage) Close() error {
	if s == nil {
		return nil
	}
	if s.closed && s.tempFile == nil && s.tempPath == "" && s.cleanupErr == nil {
		return nil
	}
	s.closed = true
	s.size = 0
	s.memory.Reset()
	closeErr := s.cleanupErr
	s.cleanupErr = nil
	if s.tempFile != nil {
		closeErr = errors.Join(closeErr, s.tempFile.Close())
		s.tempFile = nil
	}
	if s.tempPath != "" {
		removeErr := s.removeFile(s.tempPath)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = ""
		} else {
			closeErr = errors.Join(closeErr, removeErr)
		}
	}
	return closeErr
}

func (s *OpenAIGatewayService) openAIFirstOutputTimeout(reasoningEffort string) time.Duration {
	if s == nil || s.cfg == nil || s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds <= 0 {
		return 0
	}
	seconds := s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds
	switch strings.ToLower(strings.TrimSpace(reasoningEffort)) {
	case "high", "xhigh", "max":
		if override := s.cfg.Gateway.OpenAIHighEffortFirstOutputTimeoutSeconds; override > 0 {
			seconds = override
		}
	}
	return time.Duration(seconds) * time.Second
}

type openAIResponsesFirstOutputBudgetKey struct{}

// Carry an absolute pre-output budget without imposing a deadline on a stream
// that has already started. All account and compatibility attempts share it.
func WithOpenAIResponsesFirstOutputBudget(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return WithOpenAIResponsesFirstOutputDeadline(ctx, time.Now().Add(timeout))
}

// Keep an expired deadline enabled: late admission must not reset the budget.
func WithOpenAIResponsesFirstOutputDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, openAIResponsesFirstOutputBudgetKey{}, deadline)
}

// Use only for bounded pre-output operations, never for a stream or slot lease.
func OpenAIResponsesPreOutputContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if budget := responsesAttemptFromContext(ctx); budget != nil {
		return context.WithDeadline(ctx, budget.deadline)
	}
	if deadline, ok := ctx.Value(openAIResponsesFirstOutputBudgetKey{}).(time.Time); ok {
		return context.WithDeadline(ctx, deadline)
	}
	return ctx, func() {}
}

func OpenAIResponsesFirstOutputBudgetRemaining(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Value(openAIResponsesFirstOutputBudgetKey{}).(time.Time)
	return time.Until(deadline), ok
}

// Failure bookkeeping must survive a client disconnect, but must not start a
// fresh multi-second wait after the shared first-output budget has expired.
func openAIResponsesMaintenanceContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if remaining, enabled := OpenAIResponsesFirstOutputBudgetRemaining(ctx); enabled && remaining < timeout {
		timeout = remaining
		if timeout < 200*time.Millisecond {
			timeout = 200 * time.Millisecond
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (s *OpenAIGatewayService) openAIFirstOutputAttemptTimeout(ctx context.Context, start time.Time, effort string) time.Duration {
	timeout := s.openAIFirstOutputTimeout(effort)
	if remaining, ok := OpenAIResponsesFirstOutputBudgetRemaining(ctx); ok {
		// Return an elapsed-inclusive duration: callers use start+timeout.
		budget := time.Since(start) + remaining
		if budget < time.Nanosecond {
			budget = time.Nanosecond
		}
		if timeout <= 0 || budget < timeout {
			timeout = budget
		}
	}
	return timeout
}

// newOpenAIFirstOutputTimeoutError records the timeout as an upstream attempt
// and returns the failover error. proxyID/proxyName are supplied by the caller
// because the same deadline is enforced over HTTP and WebSocket transports,
// whose direct-route semantics differ (see opsUpstreamWSProxyAttribution).
func (s *OpenAIGatewayService) newOpenAIFirstOutputTimeoutError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	proxyID *int64,
	proxyName string,
	startTime time.Time,
	originalModel string,
	reasoningEffort string,
	timeout time.Duration,
	phase string,
	responseHeaders http.Header,
) *UpstreamFailoverError {
	if responsesAttemptFromContext(ctx) != nil {
		// The shared attempt owner records this local timeout once. Logging or
		// matching upstream error policies here can delay cancellation handoff.
		return &UpstreamFailoverError{
			StatusCode:      http.StatusGatewayTimeout,
			ResponseBody:    []byte(`{"error":{"type":"first_output_timeout","message":"Upstream produced no output before the deadline"}}`),
			ResponseHeaders: responseHeaders.Clone(), SafeToFailoverAfterWrite: true,
			FirstOutputTimedOut: true, RequestScopedTransient: true,
		}
	}
	elapsed := time.Since(startTime)
	logger.LegacyPrintf(
		"service.openai_gateway",
		"OpenAI first output timeout: account=%d model=%s effort=%s phase=%s elapsed=%s limit=%s",
		account.ID, originalModel, reasoningEffort, phase, elapsed, timeout,
	)
	requestID := strings.TrimSpace(responseHeaders.Get("x-request-id"))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   proxyID,
		ProxyName: proxyName,
		Platform:  account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: http.StatusGatewayTimeout, UpstreamRequestID: requestID,
		Kind: "first_output_timeout", Message: "OpenAI upstream produced no semantic output before the deadline",
		Detail: fmt.Sprintf("phase=%s elapsed_ms=%d timeout_ms=%d", phase, elapsed.Milliseconds(), timeout.Milliseconds()),
	})
	if s.rateLimitService != nil && responsesAttemptFromContext(ctx) == nil {
		maintenanceCtx, cancel := openAIResponsesMaintenanceContext(ctx, time.Second)
		s.rateLimitService.HandleStreamTimeout(maintenanceCtx, account, originalModel)
		cancel()
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusGatewayTimeout,
		ResponseBody:    []byte(`{"error":{"type":"first_output_timeout","message":"Upstream produced no output before the deadline"}}`),
		ResponseHeaders: responseHeaders.Clone(), SafeToFailoverAfterWrite: true,
		FirstOutputTimedOut: true, RequestScopedTransient: responsesAttemptFromContext(ctx) != nil,
	}
}

type openAIFirstOutputHeaderGuard struct {
	cancel     context.CancelFunc
	release    context.CancelFunc
	timer      *time.Timer
	mu         sync.Mutex
	stopped    bool
	timedOut   bool
	once       sync.Once
	stopClient func() bool
}

func newOpenAIFirstOutputHeaderGuard(
	ctx context.Context,
	release context.CancelFunc,
	deadline time.Time,
) (context.Context, *openAIFirstOutputHeaderGuard) {
	guardedCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	guard := &openAIFirstOutputHeaderGuard{cancel: cancel, release: release}
	guard.stopClient = context.AfterFunc(ctx, cancel)
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	guard.timer = time.AfterFunc(remaining, func() {
		guard.mu.Lock()
		defer guard.mu.Unlock()
		if !guard.stopped {
			guard.timedOut = true
			cancel()
		}
	})
	return guardedCtx, guard
}

func (g *openAIFirstOutputHeaderGuard) stopHeaderWait() bool {
	g.stopClient()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.timer.Stop()
	return g.timedOut
}

func (g *openAIFirstOutputHeaderGuard) close() {
	g.once.Do(func() {
		g.stopHeaderWait()
		g.cancel()
		g.release()
	})
}

type openAIRequestContextReadCloser struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
	err     error
}

func (r *openAIRequestContextReadCloser) Close() error {
	r.once.Do(func() {
		r.cleanup()
		r.err = r.ReadCloser.Close()
	})
	return r.err
}
