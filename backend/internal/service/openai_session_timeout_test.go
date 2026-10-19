package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type sessionTimeoutCacheStub struct {
	*schedulerTestGatewayCache
	records   map[string][]int64
	recordCtx context.Context
	err       error
}

func (c *sessionTimeoutCacheStub) RecordOpenAISessionTimeout(ctx context.Context, group int64, session, model string, id int64, ttl time.Duration) error {
	c.recordCtx = ctx
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.records == nil {
		c.records = make(map[string][]int64)
	}
	key := fmt.Sprintf("%d/%s/%s", group, session, model)
	c.records[key] = append(c.records[key], id)
	return c.err
}

func (c *sessionTimeoutCacheStub) GetOpenAISessionTimeoutAccounts(ctx context.Context, group int64, session, model string) ([]int64, error) {
	return c.records[fmt.Sprintf("%d/%s/%s", group, session, model)], c.err
}

func TestSessionTimeoutSurvivesClientCancellationAndPreservesScope(t *testing.T) {
	cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{}}
	svc := &OpenAIGatewayService{cache: cache}
	type traceKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "trace"))
	cancel()
	account := &Account{ID: 2290, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	group := int64(11)
	svc.RecordOpenAISessionFirstOutputTimeout(ctx, &group, "original-session", "public-model", account,
		fmt.Errorf("forward: %w", &UpstreamFailoverError{StatusCode: 504, FirstOutputTimedOut: true}))
	require.NotNil(t, cache.recordCtx)
	require.Equal(t, "trace", cache.recordCtx.Value(traceKey{}))
	deadline, ok := cache.recordCtx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
	existing := map[int64]struct{}{7: {}}
	merged := svc.withOpenAISessionTimeoutExclusions(context.Background(), &group, "original-session", "public-model", existing)
	require.Contains(t, merged, int64(2290))
	require.Contains(t, merged, int64(7))
	require.NotContains(t, existing, int64(2290), "must not mutate the handler's failed-attempt map")
	require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &group, "different-session", "public-model", nil))
	require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &group, "original-session", "other-model", nil))
	otherGroup := int64(12)
	require.Empty(t, svc.withOpenAISessionTimeoutExclusions(context.Background(), &otherGroup, "original-session", "public-model", nil))
}

func TestSessionTimeoutRequiresLocallyConfirmedTimer(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded,
		&UpstreamFailoverError{StatusCode: http.StatusTooManyRequests},
		&UpstreamFailoverError{StatusCode: 504, SafeToFailoverAfterWrite: true, ResponseBody: []byte(`{"error":{"type":"first_output_timeout"}}`)}} {
		cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{}}
		svc := &OpenAIGatewayService{cache: cache}
		svc.RecordOpenAISessionFirstOutputTimeout(context.Background(), nil, "session", "model", &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, err)
		require.Empty(t, cache.records)
	}
	for _, account := range []*Account{nil, {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, {ID: 1, Platform: PlatformGrok, Type: AccountTypeAPIKey}} {
		cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{}}
		svc := &OpenAIGatewayService{cache: cache}
		svc.RecordOpenAISessionFirstOutputTimeout(context.Background(), nil, "session", "model", account, &UpstreamFailoverError{FirstOutputTimedOut: true})
		require.Empty(t, cache.records)
	}
}

func TestSessionTimeoutCacheFailureDoesNotChangeExclusions(t *testing.T) {
	cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{}, err: errors.New("redis unavailable")}
	svc := &OpenAIGatewayService{cache: cache}
	existing := map[int64]struct{}{9: {}}
	require.Equal(t, existing, svc.withOpenAISessionTimeoutExclusions(context.Background(), nil, "session", "model", existing))
}

func TestSessionTimeoutNextSelectionSkipsOldStickyInBothSchedulers(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			group := int64(11)
			accounts := []Account{
				{ID: 2290, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}},
				{ID: 2265, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{group}, Priority: 10},
			}
			cache := &sessionTimeoutCacheStub{schedulerTestGatewayCache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session": 2290}}}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cache: cache, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
			}
			svc.RecordOpenAISessionFirstOutputTimeout(context.Background(), &group, "session", "gpt-6-astra", &accounts[0], &UpstreamFailoverError{FirstOutputTimedOut: true})
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &group, "", "session", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, int64(2265), selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			// A delayed failure of the old attempt never deletes a newer binding.
			cache.sessionBindings["openai:session"] = 2265
			svc.RecordOpenAISessionFirstOutputTimeout(context.Background(), &group, "session", "gpt-6-astra", &accounts[0], &UpstreamFailoverError{FirstOutputTimedOut: true})
			require.Equal(t, int64(2265), cache.sessionBindings["openai:session"])
			// All candidates excluded returns capacity exhaustion, never the stale sticky.
			svc.RecordOpenAISessionFirstOutputTimeout(context.Background(), &group, "session", "gpt-6-astra", &accounts[1], &UpstreamFailoverError{FirstOutputTimedOut: true})
			selection, _, err = svc.SelectAccountWithScheduler(context.Background(), &group, "", "session", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.ErrorIs(t, err, ErrOpenAISessionTimeoutCapacity)
			require.Nil(t, selection)
		})
	}
}
