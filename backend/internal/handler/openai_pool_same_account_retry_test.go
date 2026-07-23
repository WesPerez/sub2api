package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestOpenAIPoolSameAccountRetrySelectionIsRequestScoped(t *testing.T) {
	svc := &service.OpenAIGatewayService{}
	account := &service.Account{
		ID:          901,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Credentials: map[string]any{"pool_mode": true},
	}

	pending := svc.NewOpenAISameAccountRetrySelection(account)
	require.NotNil(t, pending)
	require.Same(t, account, pending.Account)
	require.False(t, pending.Acquired)
	require.True(t, pending.SkipStickyBinding)
	require.NotNil(t, pending.WaitPlan)
	require.Equal(t, account.ID, pending.WaitPlan.AccountID)
	require.Equal(t, account.Concurrency, pending.WaitPlan.MaxConcurrency)

	selection, decision, ok := consumeOpenAISameAccountRetrySelection(&pending)
	require.True(t, ok)
	require.Same(t, account, selection.Account)
	require.Equal(t, openAIAccountScheduleLayerSameAccountRetry, decision.Layer)
	require.Equal(t, account.ID, decision.SelectedAccountID)
	require.Nil(t, pending)

	selection, decision, ok = consumeOpenAISameAccountRetrySelection(&pending)
	require.False(t, ok)
	require.Nil(t, selection)
	require.Empty(t, decision.Layer)
}

func TestOpenAISameAccountRetrySelectionSupportsOrdinaryImageAccount(t *testing.T) {
	svc := &service.OpenAIGatewayService{}
	account := &service.Account{ID: 902, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	pending := svc.NewOpenAISameAccountRetrySelection(account)
	require.NotNil(t, pending)
	require.Same(t, account, pending.Account)
	require.True(t, pending.SkipStickyBinding)
	require.Nil(t, svc.NewOpenAISameAccountRetrySelection(nil))
}

func TestOpenAIPoolSameAccountRetrySlotFailureDoesNotCommitResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

	h := &OpenAIGatewayHandler{
		concurrencyHelper: NewConcurrencyHelper(
			service.NewConcurrencyService(&concurrencyCacheMock{}),
			SSEPingFormatNone,
			time.Millisecond,
		),
	}
	selection := &service.AccountSelectionResult{
		Account: &service.Account{ID: 903, Concurrency: 1},
		WaitPlan: &service.AccountWaitPlan{
			AccountID:      903,
			MaxConcurrency: 1,
			Timeout:        time.Millisecond,
			MaxWaiting:     1,
		},
		SkipStickyBinding: true,
	}
	streamStarted := false

	release, acquired := h.acquireResponsesAccountSlot(c, nil, "session", selection, false, &streamStarted, zap.NewNop())
	require.False(t, acquired)
	require.Nil(t, release)
	require.False(t, c.Writer.Written())
	require.Empty(t, recorder.Body.String())
}

func TestOpenAIPoolSameAccountRetrySlotSuccessSkipsStickyBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	cache := &concurrencyCacheMock{
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		concurrencyHelper: NewConcurrencyHelper(
			service.NewConcurrencyService(cache),
			SSEPingFormatNone,
			time.Millisecond,
		),
	}
	selection := &service.AccountSelectionResult{
		Account: &service.Account{ID: 904, Concurrency: 1},
		WaitPlan: &service.AccountWaitPlan{
			AccountID:      904,
			MaxConcurrency: 1,
			Timeout:        time.Millisecond,
			MaxWaiting:     1,
		},
		SkipStickyBinding: true,
	}
	streamStarted := false

	release, acquired := h.acquireResponsesAccountSlot(c, nil, "session", selection, false, &streamStarted, zap.NewNop())
	require.True(t, acquired)
	require.NotNil(t, release)
	require.False(t, c.Writer.Written())
	release()
	require.Equal(t, int32(1), cache.releaseAccountCalled)
}

func TestOpenAIStickyEscapeWaitPlanPreservesExistingBinding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	cache := &concurrencyCacheMock{
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		concurrencyHelper: NewConcurrencyHelper(
			service.NewConcurrencyService(cache),
			SSEPingFormatNone,
			time.Millisecond,
		),
	}
	selection := &service.AccountSelectionResult{
		Account: &service.Account{ID: 905, Concurrency: 1},
		WaitPlan: &service.AccountWaitPlan{
			AccountID:      905,
			MaxConcurrency: 1,
			Timeout:        time.Millisecond,
			MaxWaiting:     1,
		},
		PreserveStickyBinding: true,
	}
	streamStarted := false

	release, acquired := h.acquireResponsesAccountSlot(c, nil, "session", selection, false, &streamStarted, zap.NewNop())
	require.True(t, acquired)
	require.NotNil(t, release)
	require.False(t, c.Writer.Written())
	release()
	require.Equal(t, int32(1), cache.releaseAccountCalled)
}
