package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

func TestGrokStreamingWallClockEmitsSingleIncompleteGatewayMaxTime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			GrokStreamMaxWallSeconds:  30,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{"x-request-id": []string{"rid_wall"}},
	}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wall\"}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial answer\"}\n\n"))
		time.Sleep(2 * time.Second)
	}()

	account := &Account{
		ID:       42,
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	start := time.Now().Add(-29 * time.Second)
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, start, "grok-4.5", "grok-4.5")
	_ = pr.Close()
	if !errors.Is(err, ErrGrokStreamMaxWallExceeded) {
		t.Fatalf("expected gateway_max_time error, got %v", err)
	}
	if result == nil {
		t.Fatal("expected partial result")
	}
	body := rec.Body.String()
	if strings.Count(body, `"type":"response.incomplete"`) != 1 {
		t.Fatalf("expected exactly one response.incomplete, got %q", body)
	}
	if !strings.Contains(body, `"reason":"gateway_max_time"`) {
		t.Fatalf("expected gateway_max_time reason, got %q", body)
	}
	if !strings.Contains(body, `"text":"partial answer"`) {
		t.Fatalf("expected accumulated output in incomplete terminal, got %q", body)
	}
	if strings.Contains(body, `"type":"response.failed"`) || strings.Contains(body, "stream_timeout") {
		t.Fatalf("must not emit failed/stream_timeout, got %q", body)
	}
	if streamErr, ok := GetOpsStreamError(c); !ok || streamErr.ErrType != "incomplete" || streamErr.CountTowardsSLA {
		t.Fatalf("expected non-SLA ops incomplete mark, got ok=%v %#v", ok, streamErr)
	}
}

func TestGrokStreamingWallClockPreservesCompletedTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			GrokStreamMaxWallSeconds:  30,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_done\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
		time.Sleep(2 * time.Second)
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 43, Platform: PlatformGrok}, time.Now().Add(-29*time.Second), "grok-4.5", "grok-4.5")
	_ = pr.Close()
	if err != nil {
		t.Fatalf("completed terminal must win over wall timer, got %v", err)
	}
	if result == nil || result.usage == nil || result.usage.OutputTokens != 2 {
		t.Fatalf("unexpected completed result %#v", result)
	}
	if strings.Contains(rec.Body.String(), "gateway_max_time") {
		t.Fatalf("must not append an incomplete terminal after completed, body=%q", rec.Body.String())
	}
}

func TestGrokStreamingWallClockSupersedesBufferedTrailingFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamDataIntervalTimeout: 0,
		StreamKeepaliveInterval:   0,
		GrokStreamMaxWallSeconds:  1,
		MaxLineSize:               defaultMaxLineSize,
	}}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("event: response.failed\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_pending\",\"status\":\"failed\",\"error\":null}}\n\n"))
		time.Sleep(500 * time.Millisecond)
	}()

	account := &Account{
		ID:       44,
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	result, err := svc.handleStreamingResponse(
		c.Request.Context(), resp, c, account, time.Now().Add(-900*time.Millisecond), "grok-4.5", "grok-4.5",
	)
	_ = pr.Close()

	if !errors.Is(err, ErrGrokStreamMaxWallExceeded) {
		t.Fatalf("expected wall cutoff while trailing failure is buffered, got result=%#v err=%v", result, err)
	}
	body := rec.Body.String()
	if strings.Count(body, `"type":"response.incomplete"`) != 1 {
		t.Fatalf("expected one synthesized incomplete terminal, got %q", body)
	}
	if strings.Contains(body, `"type":"response.failed"`) {
		t.Fatalf("buffered failed frame must not be written after wall cutoff, got %q", body)
	}
}

func TestOpenAIStreamingIgnoresGrokWallClockConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			GrokStreamMaxWallSeconds:  30,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	}()

	start := time.Now().Add(-31 * time.Second)
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, start, "gpt-5.5", "gpt-5.5")
	_ = pr.Close()
	if err != nil {
		t.Fatalf("expected nil error for OpenAI, got %v", err)
	}
	if result == nil || result.usage == nil || result.usage.OutputTokens != 2 {
		t.Fatalf("unexpected result %#v", result)
	}
	if strings.Contains(rec.Body.String(), "gateway_max_time") {
		t.Fatalf("OpenAI path must ignore Grok wall config, body=%q", rec.Body.String())
	}
}

func TestGrokStreamingClientDisconnectGraceCancelsWithoutTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout:              0,
			StreamKeepaliveInterval:                0,
			GrokStreamClientDisconnectGraceSeconds: 1,
			MaxLineSize:                            defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}
	reqCtx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n"))
		time.Sleep(1500 * time.Millisecond)
	}()

	cancel()
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 7, Platform: PlatformGrok}, time.Now(), "grok-4.5", "grok-4.5")
	_ = pr.Close()
	if !errors.Is(err, ErrGrokStreamDisconnectGrace) {
		t.Fatalf("expected disconnect grace error, got %v", err)
	}
	if result == nil || !result.clientDisconnect {
		t.Fatalf("expected clientDisconnect partial result, got %#v", result)
	}
	if strings.Contains(rec.Body.String(), "response.incomplete") || strings.Contains(rec.Body.String(), "response.failed") {
		t.Fatalf("client already gone: must not write terminal, body=%q", rec.Body.String())
	}
}

func TestGrokStreamingDisabledDisconnectGraceRetainsLegacyDrain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout:              0,
			StreamKeepaliveInterval:                0,
			GrokStreamMaxWallSeconds:               0,
			GrokStreamClientDisconnectGraceSeconds: 0,
			MaxLineSize:                            defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqCtx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)
	cancel()

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_drained\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 8, Platform: PlatformGrok}, time.Now(), "grok-4.5", "grok-4.5")
	_ = pr.Close()
	if err != nil {
		t.Fatalf("disabled grace must retain the legacy drain, got %v", err)
	}
	if result == nil || result.usage == nil || result.usage.OutputTokens != 3 {
		t.Fatalf("expected completed legacy drain, got %#v", result)
	}
}

func TestGrokStreamingIdleTimeoutRemainsIndependentFromWallClock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 1,
			StreamKeepaliveInterval:   0,
			GrokStreamMaxWallSeconds:  30,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr, Header: http.Header{}}
	go func() {
		defer func() { _ = pw.Close() }()
		time.Sleep(1500 * time.Millisecond)
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 10, Platform: PlatformGrok}, time.Now(), "grok-4.5", "grok-4.5")
	_ = pr.Close()
	if err == nil || !strings.Contains(err.Error(), "stream data interval timeout") {
		t.Fatalf("expected the independent idle timeout, got result=%#v err=%v", result, err)
	}
	if IsGrokStreamControlledCutoff(err) {
		t.Fatalf("idle timeout must not be classified as a controlled Grok cutoff: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "stream_timeout") {
		t.Fatalf("expected stream_timeout SSE error, got %q", body)
	}
	if strings.Contains(body, "gateway_max_time") || strings.Contains(body, "response.incomplete") {
		t.Fatalf("idle timeout must not be rewritten as gateway_max_time, got %q", body)
	}
}

func TestIsGrokStreamPartialUsageError(t *testing.T) {
	if !isGrokStreamPartialUsageError(ErrGrokStreamMaxWallExceeded) {
		t.Fatal("gateway_max_time should preserve partial usage")
	}
	if !isGrokStreamPartialUsageError(fmt.Errorf("wrapped: %w", ErrGrokStreamDisconnectGrace)) {
		t.Fatal("disconnect grace should preserve partial usage")
	}
	if isGrokStreamPartialUsageError(errors.New("stream data interval timeout")) {
		t.Fatal("interval timeout must retain its existing failure path")
	}
}
