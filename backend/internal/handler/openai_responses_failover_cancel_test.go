//go:build unit

package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// openAIResponsesFailoverCancelUpstream 固定返回 HTTP 520，可在首次上游调用时
// 触发回调（用于模拟“上游在途期间客户端断开”）。
type openAIResponsesFailoverCancelUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	onFirstDo  func()
}

func (u *openAIResponsesFailoverCancelUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirstDo != nil {
		u.onFirstDo()
	}
	return &http.Response{
		StatusCode: 520,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(bytes.NewBufferString("<html>520: unknown error</html>")),
	}, nil
}

func (u *openAIResponsesFailoverCancelUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func newOpenAIResponsesFailoverTestHandler(t *testing.T, upstream service.HTTPUpstream) *OpenAIGatewayHandler {
	return newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, upstream, nil)
}

func newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t *testing.T, upstream service.HTTPUpstream, cache service.GatewayCache) *OpenAIGatewayHandler {
	return newOpenAIResponsesFailoverTestHandlerWithRuntime(t, upstream, cache, 0, nil)
}

func newOpenAIResponsesFailoverTestHandlerWithRuntime(t *testing.T, upstream service.HTTPUpstream, cache service.GatewayCache, accountConcurrency int, concurrencyCache service.ConcurrencyCache) *OpenAIGatewayHandler {
	return newOpenAIResponsesFailoverTestHandlerForPlatform(t, upstream, cache, accountConcurrency, concurrencyCache, service.PlatformOpenAI)
}

func newOpenAIResponsesFailoverTestHandlerForPlatform(t *testing.T, upstream service.HTTPUpstream, cache service.GatewayCache, accountConcurrency int, concurrencyCache service.ConcurrencyCache, platform string) *OpenAIGatewayHandler {
	t.Helper()
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "responses-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: accountConcurrency,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
		{
			ID:          2,
			Name:        "responses-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: accountConcurrency,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	if cache != nil {
		cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
		for i := range accounts {
			accounts[i].Type = service.AccountTypeAPIKey
			accounts[i].Credentials = map[string]any{"api_key": "test-key", "base_url": "https://example.com"}
		}
	}
	if platform != service.PlatformOpenAI {
		for i := range accounts {
			accounts[i].Platform = platform
			accounts[i].Type = service.AccountTypeAPIKey
			accounts[i].Credentials = map[string]any{"api_key": "test-key", "base_url": "https://example.com", "api_protocol": service.APIProtocolAnthropic}
		}
	}
	concurrencyService := service.NewConcurrencyService(concurrencyCache)
	var gatewayConcurrency *service.ConcurrencyService
	if concurrencyCache != nil {
		gatewayConcurrency = concurrencyService
	}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cache,
		cfg,
		nil,
		gatewayConcurrency,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10
	return handler
}

type canceledFirstOutputUpstream struct {
	service.HTTPUpstream
	cancelClient context.CancelFunc
	calls        int
}

func (u *canceledFirstOutputUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	<-req.Context().Done() // confirm the local timer before disconnecting the client
	u.cancelClient()
	return nil, req.Context().Err()
}

type handlerSessionTimeoutCache struct {
	service.GatewayCache
	accountID      int64
	recorded       bool
	contextErr     error
	excluded       []int64
	group          int64
	session, model string
	failedAccount  int64
}

func (c *handlerSessionTimeoutCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return c.accountID, nil
}
func (c *handlerSessionTimeoutCache) SetSessionAccountID(_ context.Context, _ int64, _ string, id int64, _ time.Duration) error {
	c.accountID = id
	return nil
}
func (c *handlerSessionTimeoutCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}
func (c *handlerSessionTimeoutCache) DeleteSessionAccountID(context.Context, int64, string) error {
	c.accountID = 0
	return nil
}
func (c *handlerSessionTimeoutCache) GetOpenAISessionTimeoutAccounts(context.Context, int64, string, string) ([]int64, error) {
	return c.excluded, nil
}
func (c *handlerSessionTimeoutCache) RecordOpenAISessionTimeout(ctx context.Context, group int64, session string, model string, account int64, _ time.Duration) error {
	c.recorded = true
	c.contextErr = ctx.Err()
	c.group, c.session, c.model, c.failedAccount = group, session, model, account
	return c.contextErr
}

func TestResponsesRecordsConfirmedTimeoutBeforeDisconnectedReturn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := &handlerSessionTimeoutCache{}
	upstream := &canceledFirstOutputUpstream{cancelClient: cancel}
	handler := newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, upstream, cache)
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("session_id", "timeout-session")
	handler.Responses(c)
	require.True(t, cache.recorded, "confirmed timeout must survive the early client-gone return")
	require.NoError(t, cache.contextErr)
	require.Equal(t, int64(3131), cache.group)
	require.Equal(t, service.DeriveSessionHashFromSeed("timeout-session"), cache.session)
	require.Equal(t, "gpt-5.1", cache.model)
	require.Equal(t, int64(1), cache.failedAccount)
	require.Equal(t, 1, upstream.calls, "cancellation must not trigger another upstream attempt")
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Zero(t, rec.Body.Len())
}

func TestResponsesSessionTimeoutExhaustionReturnsRetryableCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache := &handlerSessionTimeoutCache{excluded: []int64{1, 2}}
	upstream := &openAIResponsesFailoverCancelUpstream{}
	handler := newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, upstream, cache)
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	c.Request.Header.Set("session_id", "timeout-session")
	handler.Responses(c)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "api_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Empty(t, upstream.calls())
}

func newOpenAIResponsesFailoverTestContext(t *testing.T, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(3131)
	body := []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

// TestOpenAIGatewayHandlerResponses_FailoverAbortsWhenClientDisconnected 复现
// #4257：客户端在上游请求在途期间断开，上游随后返回可 failover 的 520。
// 期望：不再用已取消的 context 重新选号（不触达账号 2）、不把取消误报成
// 502 账号耗尽、请求按 499 归类。
func TestOpenAIGatewayHandlerResponses_FailoverAbortsWhenClientDisconnected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &openAIResponsesFailoverCancelUpstream{onFirstDo: cancel}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)

	handler.Responses(c)

	require.Equal(t, []int64{1}, upstream.calls(), "客户端断开后不应再切换到账号 2")
	require.Equal(t, statusClientClosedRequest, c.Writer.Status(), "应按 499 归类")
	require.Zero(t, rec.Body.Len(), "不应写入 502 错误响应体")

	_, hasFinalUpstreamErr := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, hasFinalUpstreamErr, "不应记录 failover 耗尽的上游错误终态")

	// 真实发生过的 520 应保留 failover 事件（service 层在返回 failover 错误前记录）
	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, 520, events[0].UpstreamStatusCode)
}

// TestOpenAIGatewayHandlerResponses_FailoverContinuesForConnectedClient 回归
// 守卫：客户端在线时 failover 行为不变——切换到账号 2，两个账号都 520 后按
// 耗尽返回 502。
func TestOpenAIGatewayHandlerResponses_FailoverContinuesForConnectedClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &openAIResponsesFailoverCancelUpstream{}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, rec := newOpenAIResponsesFailoverTestContext(t, nil)

	handler.Responses(c)

	require.Equal(t, []int64{1, 2}, upstream.calls(), "在线客户端应正常切换账号")
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
}

type budgetMixedUpstream struct {
	service.HTTPUpstream
	calls int
}

func (u *budgetMixedUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	if u.calls == 1 {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(700 * time.Millisecond):
		}
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(`{"error":{"type":"rate_limit_error","message":"token rate limit"}}`))}, nil
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}
func TestResponsesFirstOutputBudgetIncludesSlow429AndNextAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &budgetMixedUpstream{}
	handler := newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, upstream, &handlerSessionTimeoutCache{})
	handler.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	handler.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	started := time.Now()
	handler.Responses(c)
	require.Equal(t, 2, upstream.calls)
	require.GreaterOrEqual(t, time.Since(started), 950*time.Millisecond)
	require.Less(t, time.Since(started), 1800*time.Millisecond)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "Upstream")
}

type delayedBudgetRequestBody struct {
	io.Reader
	once sync.Once
}

func (r *delayedBudgetRequestBody) Read(p []byte) (int, error) {
	r.once.Do(func() { time.Sleep(600 * time.Millisecond) })
	return r.Reader.Read(p)
}
func (*delayedBudgetRequestBody) Close() error { return nil }

func TestResponsesFirstOutputBudgetIncludesBodyPreparation(t *testing.T) {
	upstream := &budgetMixedUpstream{}
	handler := newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, upstream, &handlerSessionTimeoutCache{})
	handler.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	handler.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	c.Request.Body = &delayedBudgetRequestBody{Reader: bytes.NewReader(body)}
	c.Request.ContentLength = int64(len(body))
	started := time.Now()
	handler.Responses(c)
	require.GreaterOrEqual(t, time.Since(started), 950*time.Millisecond)
	require.Less(t, time.Since(started), 1450*time.Millisecond, "body preparation reset the retry budget")
	require.Equal(t, 1, upstream.calls, "remaining budget must cancel the first attempt without replay")
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "Upstream")
}

type budgetAdmissionCache struct{ service.ConcurrencyCache }

func (*budgetAdmissionCache) AcquireUserSlot(ctx context.Context, _ int64, _ int, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func TestResponsesFirstOutputBudgetCancelsUserAdmissionBeforeUpstream(t *testing.T) {
	upstream := &openAIResponsesFailoverCancelUpstream{}
	handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
	handler.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	handler.concurrencyHelper = NewConcurrencyHelper(service.NewConcurrencyService(&budgetAdmissionCache{}), SSEPingFormatComment, time.Second)
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 1})
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	started := time.Now()
	handler.Responses(c)
	require.Less(t, time.Since(started), 1800*time.Millisecond)
	require.Empty(t, upstream.calls())
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "first_output_timeout")
	require.NoError(t, c.Request.Context().Err(), "phase cancellation escaped into lifecycle")
}

type budgetAccountAdmissionCache struct {
	fakeConcurrencyCache
	waiting  int
	released int
}

func (*budgetAccountAdmissionCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return false, nil
}
func (c *budgetAccountAdmissionCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	c.waiting++
	return true, nil
}
func (c *budgetAccountAdmissionCache) DecrementAccountWaitCount(context.Context, int64) error {
	c.waiting--
	return nil
}
func (c *budgetAccountAdmissionCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.released++
	return nil
}

func TestResponsesFirstOutputBudgetCancelsAccountQueue(t *testing.T) {
	cache := &budgetAccountAdmissionCache{}
	h := &OpenAIGatewayHandler{
		gatewayService:    &service.OpenAIGatewayService{},
		concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatComment, time.Second),
	}
	c, rec := newOpenAIResponsesFailoverTestContext(t, service.WithOpenAIResponsesFirstOutputBudget(context.Background(), 100*time.Millisecond))
	started := false
	reservationCache := newHandlerInflightCache(10)
	cfg := &config.Config{}
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
	billing := service.NewBillingCacheService(reservationCache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	phase, cancelPhase := service.OpenAIResponsesPreOutputContext(c.Request.Context())
	reserved, releaseReservation, err := reserveInflightBalanceCtx(phase, billing, &countingEstimator{cost: 1, priced: true}, &service.APIKey{User: &service.User{ID: 1}}, nil, tokenInflightEstimate("gpt-5.1", nil))
	require.NoError(t, err)
	c.Request = c.Request.WithContext(service.WithInflightReservation(c.Request.Context(), service.InflightReservationFromContext(reserved)))
	cancelPhase()
	require.Equal(t, 1, reservationCache.count(), "canceling admission phase must not release reservation")
	release, result := h.acquireResponsesAccountSlot(c, nil, "", &service.AccountSelectionResult{
		Account:  &service.Account{ID: 1, Platform: service.PlatformOpenAI},
		WaitPlan: &service.AccountWaitPlan{AccountID: 1, MaxConcurrency: 1, MaxWaiting: 1, Timeout: time.Second},
	}, true, &started, zap.NewNop())
	require.Nil(t, release)
	require.Equal(t, openAISlotAcquireFailed, result)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "first_output_timeout")
	require.Zero(t, cache.waiting)
	require.Zero(t, cache.released, "never acquired an account lease")
	require.NoError(t, c.Request.Context().Err())
	releaseReservation()
	releaseReservation()
	require.Zero(t, reservationCache.count(), "budget failure must release its reservation without a usage task")
}

func TestResponsesFirstOutputBudgetIncludesIngressBeforeAuthentication(t *testing.T) {
	upstream := &openAIResponsesFailoverCancelUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	h.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	ctx := context.WithValue(context.Background(), ctxkey.RequestStartedAt, time.Now().Add(-2*time.Second))
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)
	c.Request.Body = io.NopCloser(bytes.NewBufferString(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	h.Responses(c)
	require.Empty(t, upstream.calls())
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "first_output_timeout")
	require.True(t, rec.Flushed, "terminal response must precede deferred cleanup")
	require.Equal(t, fmt.Sprint(rec.Body.Len()), rec.Header().Get("Content-Length"))
	require.NoError(t, c.Request.Context().Err())
}

// A slow independent cleanup must not hold the already terminal response hostage.
type budgetReleaseGateCache struct {
	fakeConcurrencyCache
	rec              *httptest.ResponseRecorder
	releaseEntered   chan struct{}
	allowRelease     chan struct{}
	releaseOnce      sync.Once
	releases         atomic.Int32
	flushedAtRelease atomic.Bool
}

func (c *budgetReleaseGateCache) ReleaseAccountSlot(context.Context, int64, string) error {
	c.flushedAtRelease.Store(c.rec.Flushed)
	c.releases.Add(1)
	c.releaseOnce.Do(func() { close(c.releaseEntered) })
	<-c.allowRelease
	return nil
}

type budgetWaitUpstream struct{ service.HTTPUpstream }

func (*budgetWaitUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type budgetAnthropicBridgeUpstream struct {
	service.HTTPUpstream
	paths []string
}

func (u *budgetAnthropicBridgeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.paths = append(u.paths, req.URL.Path)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestResponsesFirstOutputBudgetReachesAnthropicBridgeFromHandler(t *testing.T) {
	up := &budgetAnthropicBridgeUpstream{}
	h := newOpenAIResponsesFailoverTestHandlerForPlatform(t, up, &handlerSessionTimeoutCache{}, 0, nil, service.PlatformKimi)
	h.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	h.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)
	value, _ := c.Get(string(middleware2.ContextKeyAPIKey))
	value.(*service.APIKey).Group.Platform = service.PlatformKimi
	body := []byte(`{"model":"kimi-k2","stream":true,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	start := time.Now()
	h.Responses(c)
	require.Equal(t, []string{"/v1/messages"}, up.paths)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "first_output_timeout")
	require.GreaterOrEqual(t, time.Since(start), 950*time.Millisecond)
	require.Less(t, time.Since(start), 1800*time.Millisecond)
	require.NoError(t, ctx.Err(), "shared deadline, not client deadline, must cancel transport")
}

func TestResponsesFirstOutputBudgetPlatformScope(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformKimi, service.PlatformZhipu,
		service.PlatformDeepseek, service.PlatformMiniMax, service.PlatformOpenCodeGo, service.PlatformCommandCode, service.PlatformCline,
		service.PlatformGrok, service.PlatformAnthropic, service.PlatformGemini, service.PlatformAntigravity} {
		t.Run(platform, func(t *testing.T) {
			want := platform == service.PlatformOpenAI || service.IsMultiProtocolAPIKeyProvider(platform)
			key := &service.APIKey{Group: &service.Group{Platform: platform}}
			require.Equal(t, want, responsesAttemptBudgetApplies(context.Background(), key))
			key.Group.Platform = service.PlatformComposite
			ctx := service.WithResolvedTargetPlatform(context.Background(), platform)
			require.Equal(t, want, responsesAttemptBudgetApplies(ctx, key))
		})
	}
}

func TestResponsesBudgetFlushesTerminalBeforeAccountRelease(t *testing.T) {
	gate := &budgetReleaseGateCache{releaseEntered: make(chan struct{}), allowRelease: make(chan struct{})}
	var allowOnce sync.Once
	unblock := func() { allowOnce.Do(func() { close(gate.allowRelease) }) }
	defer unblock()
	h := newOpenAIResponsesFailoverTestHandlerWithRuntime(t, &budgetWaitUpstream{}, &handlerSessionTimeoutCache{}, 1, gate)
	h.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	h.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	gate.rec = rec
	body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	done := make(chan struct{})
	go func() { h.Responses(c); close(done) }()
	select {
	case <-gate.releaseEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("account release was not reached")
	}
	require.True(t, gate.flushedAtRelease.Load(), "terminal response must precede account cleanup")
	require.True(t, rec.Flushed)
	require.Equal(t, 1, bytes.Count(rec.Body.Bytes(), []byte(`"error"`)))
	select {
	case <-done:
		t.Fatal("cleanup unexpectedly completed while blocked")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after release")
	}
	require.Equal(t, int32(1), gate.releases.Load())
	require.Equal(t, 1, bytes.Count(rec.Body.Bytes(), []byte(`"error"`)), "duplicate terminal response")
}

type budgetGateLogWriter struct {
	entered chan struct{}
	resume  chan struct{}
}

func (w *budgetGateLogWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("openai.first_output_retry_budget_exhausted")) {
		close(w.entered)
		<-w.resume
	}
	return len(p), nil
}
func (*budgetGateLogWriter) Sync() error { return nil }

func TestResponsesBudgetTerminalPrecedesBlockedLogger(t *testing.T) {
	gate := &budgetGateLogWriter{entered: make(chan struct{}), resume: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.resume) }) }
	defer unblock()
	log := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), gate, zapcore.DebugLevel))
	h := newOpenAIResponsesFailoverTestHandlerWithTimeoutCache(t, &budgetWaitUpstream{}, &handlerSessionTimeoutCache{})
	h.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 1
	ctx := service.WithOpenAIResponsesFirstOutputDeadline(logger.IntoContext(context.Background(), log), time.Now().Add(-time.Second))
	c, rec := newOpenAIResponsesFailoverTestContext(t, ctx)
	done := make(chan struct{})
	go func() { h.Responses(c); close(done) }()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline logger not reached")
	}
	require.True(t, rec.Flushed, "deadline response must precede log I/O")
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Equal(t, "first_output_timeout", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, fmt.Sprint(rec.Body.Len()), rec.Header().Get("Content-Length"))
	unblock()
	<-done
}

func TestResponsesCodedErrorIsSelfDelimitedBeforeCleanup(t *testing.T) {
	c, rec := newOpenAIResponsesFailoverTestContext(t, service.WithOpenAIResponsesFirstOutputBudget(context.Background(), time.Second))
	(&OpenAIGatewayHandler{}).handleStreamingAwareErrorWithCode(c, 503, "upstream_error", "capacity", "retry", false, false)
	require.True(t, rec.Flushed)
	require.Equal(t, fmt.Sprint(rec.Body.Len()), rec.Header().Get("Content-Length"))
	require.Equal(t, "capacity", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
}

type budgetDelayedReleaseCache struct {
	fakeConcurrencyCache
	entered        chan struct{}
	resume         chan struct{}
	secondReleased chan struct{}
	calls          atomic.Int32
}

func (c *budgetDelayedReleaseCache) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.calls.Add(1)
	if id == 1 {
		close(c.entered)
		<-c.resume
	} else {
		close(c.secondReleased)
	}
	return nil
}

type budgetRetryAfterTimeoutUpstream struct {
	service.HTTPUpstream
	backup chan struct{}
}

func (u *budgetRetryAfterTimeoutUpstream) Do(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
	if id == 1 {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	close(u.backup)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewBufferString(`{"id":"resp_ok","object":"response","status":"completed","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
}

func TestResponsesTimeoutRetryDoesNotWaitForOldAccountRelease(t *testing.T) {
	gate := &budgetDelayedReleaseCache{entered: make(chan struct{}), resume: make(chan struct{}), secondReleased: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.resume) }) }
	defer unblock()
	up := &budgetRetryAfterTimeoutUpstream{backup: make(chan struct{})}
	h := newOpenAIResponsesFailoverTestHandlerWithRuntime(t, up, &handlerSessionTimeoutCache{}, 1, gate)
	h.cfg.Gateway.OpenAIResponsesFirstOutputBudgetSeconds = 5
	c, rec := newOpenAIResponsesFailoverTestContext(t, context.Background())
	body := []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	done := make(chan struct{})
	go func() { h.Responses(c); close(done) }()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first account release was not reached")
	}
	select {
	case <-up.backup:
	case <-time.After(time.Second):
		t.Fatal("next account waited for the canceled account's Redis release")
	}
	select {
	case <-gate.secondReleased:
	case <-time.After(time.Second):
		t.Fatal("successful response waited for the old account release")
	}
	require.True(t, rec.Flushed)
	require.Equal(t, "completed", gjson.GetBytes(rec.Body.Bytes(), "status").String())
	select {
	case <-done:
		t.Fatal("handler did not join the outstanding release")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not join cleanup")
	}
	require.Equal(t, int32(2), gate.calls.Load())
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, rec.Flushed)
	require.Equal(t, "completed", gjson.GetBytes(rec.Body.Bytes(), "status").String())
	require.Equal(t, fmt.Sprint(rec.Body.Len()), rec.Header().Get("Content-Length"))
}
