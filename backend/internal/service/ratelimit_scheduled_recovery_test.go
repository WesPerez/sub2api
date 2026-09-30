package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type scheduledRecoveryRepoStub struct {
	AccountRepository
	changed bool
}

func (r *scheduledRecoveryRepoStub) ApplyScheduledRecovery(context.Context, *Account, bool) (bool, error) {
	return r.changed, nil
}

type scheduledRecoveryCacheStub struct {
	TempUnschedCache
	OpenAI403CounterCache
	AccountRuntimeBlocker
	deleted, reset, cleared []int64
}

func (c *scheduledRecoveryCacheStub) DeleteTempUnsched(_ context.Context, id int64) error {
	c.deleted = append(c.deleted, id)
	return nil
}
func (c *scheduledRecoveryCacheStub) ResetOpenAI403Count(_ context.Context, id int64) error {
	c.reset = append(c.reset, id)
	return nil
}
func (c *scheduledRecoveryCacheStub) ClearAccountSchedulingBlock(id int64) {
	c.cleared = append(c.cleared, id)
}

func TestScheduledRecoveryClearsRuntimeCachesWhileSchedulingStaysDisabled(t *testing.T) {
	for _, changed := range []bool{false, true} {
		cache := &scheduledRecoveryCacheStub{}
		s := &RateLimitService{accountRepo: &scheduledRecoveryRepoStub{changed: changed}, tempUnschedCache: cache,
			openAI403CounterCache: cache, runtimeBlocker: cache}
		applied, err := s.ApplyScheduledRecovery(context.Background(), &Account{ID: 42, Type: AccountTypeAPIKey}, false)
		require.NoError(t, err)
		require.Equal(t, changed, applied)
		if changed {
			require.Equal(t, []int64{42}, cache.deleted)
			require.Equal(t, []int64{42}, cache.reset)
			require.Equal(t, []int64{42}, cache.cleared)
		} else {
			require.Empty(t, cache.deleted)
			require.Empty(t, cache.reset)
			require.Empty(t, cache.cleared)
		}
	}
}
